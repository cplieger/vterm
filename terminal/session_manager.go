package terminal

// SessionManager runs N independent PTY-backed terminal sessions behind one set
// of HTTP handlers, so a browser can open several terminals (tabs) against one
// server. Each session is a *Handler (its own process, VT screen, scrollback,
// and resume state); sessions run continuously whether or not a client is
// attached. The manager owns creation, lookup, the crypto-random session id
// (which is both the routing id in /ws?session=<id> and the resume id), and an
// ownership-keyed idle reaper.
//
// Why N plain PTYs and not a terminal multiplexer: nesting under tmux would mean
// speaking tmux control mode to get per-session chrome, and a full-screen TUI
// under tmux collides on the prefix key and mouse/focus passthrough. dtach adds
// nothing here because the server process already outlives any client socket,
// which is the persistence we need.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// SessionInfo is the public description of one session, returned by List and
// carried on the status stream.
//
// Three of its fields are title-shaped; they are not interchangeable:
//   - Title is the RESOLVED display title (effectiveTitle) — what a consumer
//     renders unless it has a reason not to.
//   - PinnedTitle is the USER's name, set and cleared only by a human action.
//   - ClientTitle is a CLIENT-DERIVED automatic title: a guess a client
//     computed and asked the server to remember (today, the agent UI's latched
//     first submitted line). Client-supplied, but not user-authored.
type SessionInfo struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
	Title     string    `json:"title"` // resolved display title (effectiveTitle)
	// ClientTitle is the raw stored client-derived title, exposed alongside
	// Title so a consumer can read the pushed label directly (bypassing the
	// precedence baked into Title) — used by an input-title UI that treats a
	// program's OSC window title as unreliable.
	ClientTitle string `json:"clientTitle"`
	// PinnedTitle is the user's explicit name for this session, set through
	// PUT /api/sessions/{id}/pinned-title and cleared through DELETE on the same
	// path. It outranks every automatic source in Title; it is also exposed raw
	// so a UI can tell that a pin EXISTS (to offer the automatic name again)
	// rather than only seeing its effect.
	PinnedTitle     string `json:"pinnedTitle"`
	Status          string `json:"status"`
	ReportsActivity bool   `json:"reportsActivity"`
}

// Session status values. The manager computes working/idle/exited from process
// liveness, OSC 9;4 progress, and output activity; a consumer's classifier maps
// an OSC 9 notification to a latched needs-input or done state.
const (
	StatusWorking = "working" // agent working (OSC 9;4 progress active) or recent output
	StatusIdle    = "idle"    // at rest with no turn yet (the default / new-session state)
	StatusInput   = "input"   // blocked awaiting user action (latched from a notification)
	StatusDone    = "done"    // a turn completed (latched from a notification; cleared on next working)
	StatusExited  = "exited"  // the process has exited
)

// ManagerOption configures a SessionManager.
type ManagerOption func(*managerConfig)

type managerConfig struct {
	logger     *slog.Logger
	classifier func(string) (string, bool)
	idleWindow time.Duration
}

// WithIdleReaper enables the ownership-keyed idle reaper: when no client (WS or
// status stream) has been connected to the manager for d, all sessions are
// reaped (the operator's browser closed without deleting them). Zero (the
// default) disables reaping. This is keyed on client presence, not on a
// per-session socket, so a backgrounded tab with no live socket is not reaped
// while any client of the same browser is still connected.
func WithIdleReaper(d time.Duration) ManagerOption {
	return func(c *managerConfig) { c.idleWindow = d }
}

// WithManagerLogger sets the logger. A nil logger discards output.
func WithManagerLogger(l *slog.Logger) ManagerOption {
	return func(c *managerConfig) { c.logger = l }
}

// WithStatusClassifier maps an OSC 9 notification message to a session status
// (return ok=false to ignore a message). This keeps the engine generic: web-terminal-kiro
// maps kiro-cli's "Permission required" to input and "Response complete" to
// done, while a plain shell server sets no classifier and gets working/idle from
// output activity only. A classified input status latches (persists while the
// process waits) until the turn resumes or a done message clears it.
func WithStatusClassifier(fn func(notification string) (status string, ok bool)) ManagerOption {
	return func(c *managerConfig) { c.classifier = fn }
}

// Field order is pointer-scan optimal (govet fieldalignment): the struct ends
// with string fields, whose trailing length word is a scalar, so the GC's
// pointer-scan range stops before the tail. Ending with the *Handler pointer
// instead would extend that range by a word.
type session struct {
	createdAt   time.Time
	handler     *Handler
	id          string
	clientTitle string // client-derived automatic title, below the OSC title
	pinnedTitle string // the user's explicit name; outranks every automatic source
	// autoTitle is the server-derived fallback (foreground process, then cwd,
	// then command basename): the LAST rung, used when nothing else named the
	// session. Only the status sweep writes it — see confirmAutoTitle — so List
	// and snapshot read one confirmed value instead of each probing procfs and
	// disagreeing with the live stream. Initialised at Create to the command
	// basename so a List served before the first sweep still answers.
	autoTitle string
}

// titleSources is the four inputs effectiveTitle resolves. A struct rather than
// four positional strings: they are all the same type, three of them are
// title-shaped, and a swapped pair at a call site would silently invert the
// precedence while every unit test of effectiveTitle itself still passed.
type titleSources struct {
	pinned string // the user's explicit name
	// derived is the input-derived name (WithInputTitle): the first eligible line
	// the user submitted. Empty unless the consumer asked for it. It outranks the
	// OSC title because the only reason to ask for it is that the program's own
	// title is not worth showing.
	derived string
	osc     string // the program's OSC 0/2 window title
	client  string // a client-derived automatic title we were asked to remember
	auto    string // the server's own inference (foreground process / cwd / command)
}

// effectiveTitle combines a session's title sources in precedence order:
// explicit beats inferred. The user's pinned name wins outright; then the
// input-derived name, when the consumer asked the engine to derive one; then the
// live OSC window title, when the program set one; then a client-derived title a
// client asked us to remember; then the server's own inference (foreground
// process / cwd / command).
//
// For the two shipping consumers the last two rungs never compete — the generic
// preset pushes no client title, and the agent's program always sets some OSC
// title — so their relative order matters only to a future third consumer.
//
// Pure by design: the OSC title comes from a handler getter (h.mu) and the other
// three from manager state (m.mu), and every caller (List, snapshot,
// diffStatuses) reads them under their own locks — never one lock nested in the
// other.
func effectiveTitle(src *titleSources) string {
	if src.pinned != "" {
		return src.pinned
	}
	if src.derived != "" {
		return src.derived
	}
	if src.osc != "" {
		return src.osc
	}
	if src.client != "" {
		return src.client
	}
	return src.auto
}

// SessionManager maps session ids to PTY-backed handlers and serves the
// terminal WebSocket, the REST session API, and (see events.go) the status
// stream. Safe for concurrent use.
type SessionManager struct {
	factory       func(id string) *Handler
	logger        *slog.Logger
	sessions      map[string]*session
	trackers      map[string]*statusTracker
	subs          map[chan statusEvent]struct{}
	classifier    func(string) (string, bool)
	reaperCancel  context.CancelFunc
	sweepCancel   context.CancelFunc
	idleSince     time.Time
	mu            sync.Mutex
	subsMu        sync.Mutex
	idleWindow    time.Duration
	activeClients int
	created       uint64
	closed        uint64
	reaped        uint64
}

// NewSessionManager returns a manager that builds each session's handler with
// factory (called with the new session id, so the factory can scope the
// handler's logger and working directory). Options configure the idle reaper,
// the status classifier, and the logger; concurrent SSE subscribers are bounded
// internally to a fixed cap (maxSubscribers).
func NewSessionManager(factory func(id string) *Handler, opts ...ManagerOption) *SessionManager {
	cfg := managerConfig{logger: slog.Default()}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if cfg.logger == nil {
		cfg.logger = slog.New(slog.DiscardHandler)
	}
	m := &SessionManager{
		factory:    factory,
		logger:     cfg.logger,
		sessions:   make(map[string]*session),
		trackers:   make(map[string]*statusTracker),
		subs:       make(map[chan statusEvent]struct{}),
		classifier: cfg.classifier,
		idleSince:  time.Now(),
		idleWindow: cfg.idleWindow,
	}
	if m.idleWindow > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		m.reaperCancel = cancel
		go m.reapLoop(ctx)
	}
	// The status sweep computes per-session status and pushes changes to
	// subscribers. It runs regardless of subscribers (cheap, a near-no-op when
	// there are none) so status is current the instant a client subscribes.
	sctx, scancel := context.WithCancel(context.Background())
	m.sweepCancel = scancel
	go m.sweepLoop(sctx)
	return m
}

// Create starts a new session (eagerly spawning its process at a default size)
// and returns its id.
func (m *SessionManager) Create() (string, error) {
	m.mu.Lock()
	id, err := newSessionID()
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	h := m.factory(id)
	m.mu.Unlock()

	// Eager start outside the lock: spawning a process should not block other
	// manager operations. A duplicate id is astronomically unlikely (128-bit
	// random) so we do not re-check under the lock after start.
	if err := h.StartEager(); err != nil {
		return "", err
	}

	m.mu.Lock()
	// Refresh the idle clock so the reaper cannot reap a session created while the
	// manager is idle (activeClients == 0) before its first client attaches.
	m.idleSince = time.Now()
	// autoTitle starts at the command basename (the ladder's last rung): the
	// sweep refines it to a foreground-process or cwd name, but a List served
	// before the first sweep must still name the session.
	m.sessions[id] = &session{id: id, handler: h, createdAt: time.Now(), autoTitle: h.commandBase()}
	n := len(m.sessions)
	m.created++
	m.mu.Unlock()

	m.logger.Info("session: created", "session", LogID(id), "sessions", n)
	return id, nil
}

// LogID returns a short, correlation-safe prefix of a session id for logs:
// the first 8 bytes plus an ellipsis, or the id unchanged when it is already
// that short. Session ids are cryptographically-random hex, so a byte prefix
// never splits a rune.
//
// The full id is a WS routing + resume capability token: logging it whole
// places a session-access credential into aggregated logs (CWE-532), where
// anyone with log-read reach and network access can attach to the session.
// Every consumer that logs a session id — its own lifecycle lines, a
// per-session logger's bound attribute — must pass it through here rather
// than re-deriving the truncation, so the fleet keeps ONE definition of how
// much of a session token may be logged. Exported for exactly that reason:
// re-implemented copies drift, and the drift is silent (a wrong length leaks
// more entropy; a missing call leaks the whole token) unless a test asserts
// the logged value, which is why consumers should pin it.
func LogID(id string) string {
	if len(id) > 8 {
		return id[:8] + "\u2026"
	}
	return id
}

// List returns all sessions sorted by creation time.
//
// Two-phase like diffStatuses: manager state (session set, client titles,
// tracker state) is captured under m.mu, then the handler getters (Title /
// Exited / Progress — each takes that handler's h.mu) run after m.mu is
// released, so one wedged handler stalls only this call, never every manager
// path.
func (m *SessionManager) List() []SessionInfo {
	type listItem struct {
		handler    *Handler
		lastStatus string
		autoTitle  string
		info       SessionInfo
		latched    bool
	}
	m.mu.Lock()
	items := make([]listItem, 0, len(m.sessions))
	for _, s := range m.sessions {
		it := listItem{
			info: SessionInfo{
				ID: s.id, ClientTitle: s.clientTitle, PinnedTitle: s.pinnedTitle,
				CreatedAt: s.createdAt,
			},
			handler:   s.handler,
			autoTitle: s.autoTitle,
		}
		if tr := m.trackers[s.id]; tr != nil {
			it.lastStatus = tr.lastStatus
			it.latched = tr.latched != ""
		}
		items = append(items, it)
	}
	m.mu.Unlock()

	out := make([]SessionInfo, 0, len(items))
	for i := range items {
		it := &items[i]
		it.info.Status = refinedStatus(it.lastStatus, it.handler)
		osc, derived := it.handler.titles()
		it.info.Title = effectiveTitle(&titleSources{
			pinned: it.info.PinnedTitle, derived: derived, osc: osc,
			client: it.info.ClientTitle, auto: it.autoTitle,
		})
		// reportsActivity mirrors the status stream: sticky once any OSC 9;4
		// progress has been seen (Progress() >= 0), or a notification latched.
		it.info.ReportsActivity = it.handler.Progress() >= 0 || it.latched
		out = append(out, it.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Close terminates a session and removes it. Returns false if the id is unknown.
func (m *SessionManager) Close(id string) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
		m.closed++
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	s.handler.Shutdown()
	m.logger.Info("session: closed", "session", LogID(id))
	return true
}

// SetSessionTitle sets a per-session client-derived automatic title, shown as
// the session's reported title whenever its program emits no OSC window title
// and the user has pinned no name. Returns false if the id is unknown. No
// explicit broadcast is needed: the 250ms status sweep (diffStatuses) detects
// the changed effective title and pushes it to subscribers within a tick.
func (m *SessionManager) SetSessionTitle(id, title string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return false
	}
	s.clientTitle = sanitizeTitle(title, maxClientTitleRunes)
	return true
}

// SetSessionPinnedTitle sets the user's explicit name for a session, which
// outranks every automatic title source. Returns false if the id is unknown.
// Pass an empty title to clear the pin (ClearSessionPinnedTitle is the named
// spelling the DELETE route uses). Pushed to subscribers by the next sweep.
func (m *SessionManager) SetSessionPinnedTitle(id, title string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return false
	}
	s.pinnedTitle = sanitizeTitle(title, maxPinnedTitleRunes)
	return true
}

// ClearSessionPinnedTitle removes a session's user-set name, so its label falls
// back to the automatic sources again. Returns false if the id is unknown;
// clearing an unpinned session is a successful no-op (the operation is
// idempotent).
func (m *SessionManager) ClearSessionPinnedTitle(id string) bool {
	return m.SetSessionPinnedTitle(id, "")
}

// Shutdown stops the reaper and terminates every session.
func (m *SessionManager) Shutdown() {
	m.mu.Lock()
	if m.reaperCancel != nil {
		m.reaperCancel()
		m.reaperCancel = nil
	}
	if m.sweepCancel != nil {
		m.sweepCancel()
		m.sweepCancel = nil
	}
	victims := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		victims = append(victims, s)
	}
	m.sessions = make(map[string]*session)
	m.mu.Unlock()
	for _, s := range victims {
		s.handler.Shutdown()
	}
}

// WebSocketHandler serves the terminal stream at WSPath (/ws?session=<id>).
// An unknown or missing id is reported AFTER the upgrade via the definitive
// close code 4004 (statusUnknownSession); a non-WebSocket GET gets Accept's
// 426 whether or not the session exists, so a plain probe cannot test
// session existence (no pre-upgrade 404 — browser JS cannot read one, and
// it doubled as an existence oracle). While a client is attached the manager
// counts it as present, which suppresses the idle reaper. Mounted for you by
// MountSessionRoutes / MountAPI; exported so consumer tests can stub it.
func (m *SessionManager) WebSocketHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("session")
		m.mu.Lock()
		s, ok := m.sessions[id]
		if ok {
			m.activeClients++ // mark present atomically with the lookup
		}
		m.mu.Unlock()
		if !ok {
			// Unknown session: accept the upgrade and close with the
			// DEFINITIVE application code (4004), which the client reads and
			// routes to its ended state — a pre-upgrade 404 is unreadable
			// from browser JS (an opaque code-1006 failure) and condemned
			// clients to an endless reconnect loop against a session that
			// will never exist. nil AcceptOptions keep coder/websocket's
			// same-origin default, matching the fleet's live posture (no
			// consumer sets WithAcceptOptions). A non-WebSocket GET gets
			// Accept's own 426 — the same answer the known-session path
			// gives, so a plain probe can no longer distinguish session
			// existence (the old 404-vs-426 oracle).
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return // Accept already wrote its error response (e.g. 426)
			}
			_ = ws.Close(statusUnknownSession, "unknown session")
			return
		}
		defer m.clientDisconnected()
		s.handler.ServeHTTP(w, r) // blocks for the WS lifetime
	})
}

// RESTHandler serves the session REST API: POST SessionsPath (create),
// GET SessionsPath (list), DELETE /api/sessions/{id} (close),
// PUT /api/sessions/{id}/title (set the client-derived automatic title), and
// PUT + DELETE /api/sessions/{id}/pinned-title (set / clear the user's name).
// Its internal patterns are absolute, so it only functions on the SessionsPath +
// SessionsSubtreePath mounts — MountSessionRoutes / MountAPI perform them
// (the route-set contract lives there); exported so consumer tests can stub it.
func (m *SessionManager) RESTHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sessions", m.handleCreate)
	mux.HandleFunc("GET /api/sessions", m.handleList)
	mux.HandleFunc("DELETE /api/sessions/{id}", m.handleDelete)
	mux.HandleFunc("PUT /api/sessions/{id}/title", m.handleSetTitle)
	mux.HandleFunc("PUT /api/sessions/{id}/pinned-title", m.handleSetPinnedTitle)
	mux.HandleFunc("DELETE /api/sessions/{id}/pinned-title", m.handleClearPinnedTitle)
	return mux
}

func (m *SessionManager) handleCreate(w http.ResponseWriter, _ *http.Request) {
	id, err := m.Create()
	if err != nil {
		m.logger.Error("session: create failed", "error", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	// A freshly eager-started session is idle until it produces output; the
	// status stream corrects this within a tick if the process died instantly.
	writeJSON(w, http.StatusCreated, SessionInfo{ID: id, Status: StatusIdle, CreatedAt: time.Now()})
}

func (m *SessionManager) handleList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, m.List())
}

func (m *SessionManager) handleDelete(w http.ResponseWriter, r *http.Request) {
	if m.Close(r.PathValue("id")) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "unknown session", http.StatusNotFound)
}

// handleSetTitle sets a session's client-derived automatic title from a JSON body
// {"title": "..."}. The body is capped at 4 KiB; a body that cannot be decoded
// is 400, an unknown session id is 404, success is 204. The title is sanitized
// (bounded to maxClientTitleRunes, control/DEL stripped) before storage since it
// is shown as a tab label.
func (m *SessionManager) handleSetTitle(w http.ResponseWriter, r *http.Request) {
	title, ok := decodeTitleBody(w, r)
	if !ok {
		return
	}
	if m.SetSessionTitle(r.PathValue("id"), title) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "unknown session", http.StatusNotFound)
}

// handleSetPinnedTitle sets a session's user-pinned name from a JSON body
// {"title": "..."}. Same envelope as handleSetTitle (4 KiB cap, 400 on an
// undecodable body, 404 on an unknown id, 204 on success) with one difference:
// a title that sanitizes to empty is a 400, not a silent clear. Clearing is a
// destructive operation with its own verb (DELETE on this path), so it cannot be
// reached by an accidentally-empty body.
func (m *SessionManager) handleSetPinnedTitle(w http.ResponseWriter, r *http.Request) {
	title, ok := decodeTitleBody(w, r)
	if !ok {
		return
	}
	clean := sanitizeTitle(title, maxPinnedTitleRunes)
	if strings.TrimSpace(clean) == "" {
		http.Error(w, "empty title (use DELETE to clear)", http.StatusBadRequest)
		return
	}
	if m.SetSessionPinnedTitle(r.PathValue("id"), clean) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "unknown session", http.StatusNotFound)
}

// handleClearPinnedTitle removes a session's user-pinned name so its label falls
// back to the automatic sources. 204 on success (idempotent — clearing an
// unpinned session succeeds), 404 on an unknown id.
func (m *SessionManager) handleClearPinnedTitle(w http.ResponseWriter, r *http.Request) {
	if m.ClearSessionPinnedTitle(r.PathValue("id")) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "unknown session", http.StatusNotFound)
}

// decodeTitleBody reads the shared {"title": "..."} envelope both title routes
// use, writing the 400 itself on a body that is oversized or undecodable. The
// returned string is RAW: each caller applies its own sanitize bound.
func decodeTitleBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return "", false
	}
	return body.Title, true
}

// Title-length bounds, per source. A client-derived title can legitimately be a
// whole submitted line, so it keeps the historical 512; a hand-typed name past
// ~40 characters is never visible in a tab chip, so 128 is generous while
// keeping a pathological rename out of every log line and SSE frame. Both count
// RUNES, not grapheme clusters (UAX #29) — a defensive bound, not a display
// promise.
const (
	maxClientTitleRunes = 512
	maxPinnedTitleRunes = 128
)

// sanitizeTitle bounds and cleans a client-supplied title for use as a tab
// label: it truncates to at most maxRunes runes and drops control characters
// (rune < 0x20) and DEL (0x7f) so a title cannot inject newlines or control
// sequences into the UI or logs (CWE-117). The cap counts retained runes, so the
// returned string is always at most maxRunes runes and control-free.
func sanitizeTitle(s string, maxRunes int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= maxRunes {
			break
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// clientConnected records that a client (WS or status stream) is present, so
// the idle reaper does not fire while the operator is here.
func (m *SessionManager) clientConnected() {
	m.mu.Lock()
	m.activeClients++
	m.mu.Unlock()
}

// clientDisconnected records a client leaving; when the last one leaves, the
// idle window starts counting from now.
func (m *SessionManager) clientDisconnected() {
	m.mu.Lock()
	m.activeClients--
	if m.activeClients <= 0 {
		m.activeClients = 0
		m.idleSince = time.Now()
	}
	m.mu.Unlock()
}

func (m *SessionManager) reapLoop(ctx context.Context) {
	interval := min(m.idleWindow, 30*time.Second)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.maybeReap()
		}
	}
}

// maybeReap reaps every session when no client has been connected for the idle
// window. It reaps all sessions together (not per-session): with no client the
// owning browser is gone, so every session is orphaned. A backgrounded tab with
// no socket is safe as long as any client (another tab's WS, or the status
// stream) keeps activeClients above zero.
func (m *SessionManager) maybeReap() {
	m.mu.Lock()
	if m.activeClients > 0 || len(m.sessions) == 0 || time.Since(m.idleSince) < m.idleWindow {
		m.mu.Unlock()
		return
	}
	victims := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		victims = append(victims, s)
	}
	m.sessions = make(map[string]*session)
	m.reaped += uint64(len(victims))
	m.mu.Unlock()
	for _, s := range victims {
		s.handler.Shutdown()
		m.logger.Info("session: reaped (no client for idle window)", "session", LogID(s.id))
	}
}

// refinedStatus computes a session's status for a point-in-time read (List and
// the SSE snapshot) from the sweep's last computed status: that value carries
// the working/done/input refinement, so GET /api/sessions agrees with what the
// status stream pushes. Both sources MUST agree — a reloading client paints its
// activity dots from whichever answers last, and when List still reported the
// coarse liveness status a reload visibly downgraded a latched done/input dot
// back to idle until the next real transition. Process exit is checked live and
// always wins (it is definitive, while lastStatus can lag a sweep tick behind);
// an empty lastStatus (created within the current sweep tick) is the
// new-session default, idle.
func refinedStatus(lastStatus string, h *Handler) string {
	if h.Exited() {
		return StatusExited
	}
	if lastStatus != "" {
		return lastStatus
	}
	return StatusIdle
}

// newSessionID returns a 128-bit crypto-random hex id, used as both the routing
// id (/ws?session=<id>) and the resume id.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // #nosec G104 -- client hangup is not actionable
}
