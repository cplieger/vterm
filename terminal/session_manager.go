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
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// SessionID identifies one session for the life of this manager: the value
// Create mints (128-bit crypto-random hex), the key /ws?session=<id> routes by,
// and the resume id. A distinct type rather than a bare string because a
// session id shares a parameter list with other strings on this surface
// (SetSessionTitle's title sits right next to it), and a swapped pair
// type-checks as plain strings while storing the id as a tab label. The type
// makes the ID position sticky on the manager's own methods, so a misplaced
// argument there fails to compile.
//
// Three exported surfaces deliberately keep a plain string id, and a consumer
// converts at each: NewSessionManager's factory callback (it feeds
// handler-internal plumbing that is string-typed throughout), LogID (it also
// truncates handler and containment ids, not only session ids), and
// WithContainment. Retyping those would push SessionID into the handler and
// containment layers for no swap it prevents — neither takes a second string
// beside the id.
//
// On the wire (SessionInfo.ID, the status stream, URL path segments) an id
// stays a plain JSON string: SessionID declares no MarshalJSON, so encoding/json
// emits a defined string type exactly as its underlying string. Convert at the
// boundary.
type SessionID string

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
	ID        SessionID `json:"id"`    // marshals as a plain string; the wire shape is unchanged
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
	PinnedTitle string `json:"pinnedTitle"`
	Status      string `json:"status"`
	// Order is the session's position in the display order every viewer shares:
	// 0-based, dense, and unique across the live set. It exists because the
	// order is a property of the SESSION SET rather than of one browser, so two
	// devices showing the same server agree on the arrangement, and a reorder
	// made on one appears on the other.
	//
	// Read this FIELD to order a list, not the sequence the sessions arrived in.
	// The REST list and the status stream are both served in this order, but a
	// consumer that merges them (subscribing before its bootstrap list resolves,
	// which is the way to avoid double-adopting a session) sees neither
	// sequence intact.
	//
	// ABSENT on a status event carrying Removed: the session has left the order
	// and has no position to report. The status stream's copy of this field is a
	// pointer with omitempty for exactly that reason (see statusEvent.Order) —
	// absent and present-and-zero are different answers, and 0 is the FRONT of the
	// strip, so a consumer that read 0 there would be told a closing session had
	// just become the first tab.
	Order           int  `json:"order"`
	ReportsActivity bool `json:"reportsActivity"`
}

// Session status values. The manager derives every one of them from two inputs —
// process liveness and OSC 9;4 progress — plus the needs-input/done latch a
// consumer's classifier sets from an OSC 9 notification. Raw output activity is
// deliberately NOT an input; computeStatus (events.go) states why.
const (
	StatusWorking = "working" // OSC 9;4 progress reports active (state 1 or 3), and nothing else
	StatusIdle    = "idle"    // at rest: no progress state and no latch (also the new-session default)
	StatusInput   = "input"   // blocked awaiting user action (latched from a notification)
	StatusDone    = "done"    // a turn completed (latched from a notification; cleared by any progress state but 4)
	StatusExited  = "exited"  // the process has exited
	// StatusCrashed is a session whose process exited BADLY: a non-zero exit
	// status, or a terminating signal it was not asked for. StatusExited is
	// reserved for an ordinary end — status 0, or any exit the server itself
	// caused (Close, the idle reaper, Shutdown) — so a routine restart never
	// reads as a failure. crashedExit owns the exact boundary and states why
	// each case falls where it does.
	StatusCrashed = "crashed"
	// StatusFailed is OSC 9;4 progress state 2: the program reported an error
	// state (with a percentage, or indeterminate when pr is omitted). Like every
	// progress state it PERSISTS until the program changes it — it is a state,
	// not an event, so no timeout and no output activity clears it.
	StatusFailed = "failed"
	// StatusWarning is OSC 9;4 progress state 4. The two sources that define
	// these sequences disagree on state 4: iTerm2 calls it a warning at pr
	// percent, ConEmu calls it paused. The engine follows iTerm2, because it
	// advertises TERM_PROGRAM=iTerm.app — that identity is what makes a client
	// emit these sequences at all — and iTerm2's definition is the more
	// specified of the two. Persists like StatusFailed.
	StatusWarning = "warning"
)

// ManagerOption configures a SessionManager.
type ManagerOption func(*managerConfig)

type managerConfig struct {
	logger       *slog.Logger
	originPolicy *OriginPolicy
	classifier   func(string) (string, bool)
	idleWindow   time.Duration
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

// WithManagerOriginPolicy widens the browser origins allowed to reach the
// manager's WebSocket route beyond same-origin. Pass the same policy the
// per-session handlers get via WithOriginPolicy: the manager owns the
// unknown-session upgrade (the one that answers close code 4004), so a policy
// set on only one of the two leaves a widened origin working for a live session
// and failing for a reaped one. A nil policy (the default) means same-origin
// only.
func WithManagerOriginPolicy(p *OriginPolicy) ManagerOption {
	return func(c *managerConfig) { c.originPolicy = p }
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
	id          SessionID
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
	osc    string // the program's OSC 0/2 window title
	client string // a client-derived automatic title we were asked to remember
	auto   string // the server's own inference (foreground process / cwd / command)
}

// effectiveTitle combines a session's title sources in precedence order:
// explicit beats inferred. The user's pinned name wins outright; then the live
// OSC window title, when the program set one; then a client-derived title a
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
	factory      func(id string) *Handler
	logger       *slog.Logger
	originPolicy *OriginPolicy
	sessions     map[SessionID]*session
	trackers     map[SessionID]*statusTracker
	subs         map[chan statusEvent]struct{}
	classifier   func(string) (string, bool)
	reaperCancel context.CancelFunc
	sweepCancel  context.CancelFunc
	// loopsDone closes once reapLoop and sweepLoop have both returned, so
	// Shutdown can wait for the manager's OWN goroutines and not only for its
	// sessions'. Derived from m.wg by an observer started in NewSessionManager
	// after both loops are counted.
	loopsDone chan struct{}
	idleSince time.Time
	// order is the display order every viewer shares: session ids, and exactly
	// the keys of sessions. Maintained at the four sites that change the session
	// set (create appends, Close removes, the reaper and Shutdown clear it) so a
	// rank lookup never has to reconcile the two.
	//
	// A slice rather than a position field on session: a reorder then writes one
	// value instead of renumbering every session, and there is no gap to reclaim
	// when a session in the middle closes. It is not persisted, because it does
	// not outlive what it orders — sessions are PTY children of this process.
	//
	// Placed last among the pointer-bearing fields on purpose: its trailing len
	// and cap words are scalars, so ending the struct's pointer-scan range at its
	// data pointer keeps 16 bytes out of the GC's scan (govet fieldalignment).
	order         []SessionID
	wg            sync.WaitGroup
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
// handler's logger and working directory). Construction through
// NewSessionManager is mandatory: the zero SessionManager has nil maps and
// panics on first use. Options configure the idle reaper,
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
		factory:      factory,
		logger:       cfg.logger,
		originPolicy: cfg.originPolicy,
		sessions:     make(map[SessionID]*session),
		trackers:     make(map[SessionID]*statusTracker),
		subs:         make(map[chan statusEvent]struct{}),
		classifier:   cfg.classifier,
		idleSince:    time.Now(),
		idleWindow:   cfg.idleWindow,
	}
	if m.idleWindow > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		m.reaperCancel = cancel
		m.wg.Go(func() { m.reapLoop(ctx) })
	}
	// The status sweep computes per-session status and pushes changes to
	// subscribers. It runs regardless of subscribers (cheap, a near-no-op when
	// there are none) so status is current the instant a client subscribes.
	sctx, scancel := context.WithCancel(context.Background())
	m.sweepCancel = scancel
	m.wg.Go(func() { m.sweepLoop(sctx) })
	// Both loops are counted before this observer starts, which is what makes the
	// Wait below safe against a loop that returns immediately.
	m.loopsDone = make(chan struct{})
	go func(done chan struct{}) {
		m.wg.Wait()
		close(done)
	}(m.loopsDone)
	return m
}

// Create starts a new session (eagerly spawning its process at a default size)
// and returns its id.
func (m *SessionManager) Create() (SessionID, error) {
	info, err := m.create()
	return info.ID, err
}

// create is Create plus the SessionInfo describing what it stored, so
// handleCreate can echo the values every later enumeration reports for this
// session instead of re-deriving them.
//
// Both fields it carries beyond the id are ones a client sorts by, and both were
// wrong when re-derived. A second time.Now() gave the newest tab a createdAt
// matching neither List nor the status stream. A zero Order claimed position 0,
// the FRONT of the strip, for the session just appended to the back — so a client
// that trusted the 201 put its new tab first and then watched it jump when the
// next status event corrected it.
func (m *SessionManager) create() (SessionInfo, error) {
	m.mu.Lock()
	id, err := newSessionID()
	if err != nil {
		m.mu.Unlock()
		return SessionInfo{}, err
	}
	h := m.factory(string(id))
	m.mu.Unlock()

	// Eager start outside the lock: spawning a process should not block other
	// manager operations. A duplicate id is astronomically unlikely (128-bit
	// random) so we do not re-check under the lock after start.
	if err := h.StartEager(); err != nil {
		return SessionInfo{}, err
	}

	now := time.Now()
	m.mu.Lock()
	// Refresh the idle clock so the reaper cannot reap a session created while the
	// manager is idle (activeClients == 0) before its first client attaches. Same
	// reading as createdAt: they describe one event, and two calls to time.Now()
	// only invite a reader to wonder which is authoritative.
	m.idleSince = now
	// autoTitle starts at the command basename (the ladder's last rung): the
	// sweep refines it to a foreground-process or cwd name, but a List served
	// before the first sweep must still name the session.
	m.sessions[id] = &session{id: id, handler: h, createdAt: now, autoTitle: h.commandBase()}
	// Newest session last, which is where a new tab belongs. A client that has
	// arranged its tabs keeps that arrangement; only the new id moves.
	m.order = append(m.order, id)
	order := len(m.order) - 1
	n := len(m.sessions)
	m.created++
	m.mu.Unlock()

	m.logger.Info("session: created", "session", LogID(string(id)), "sessions", n)
	// A freshly eager-started session is idle until it produces output; the
	// status stream corrects that within a tick if the process died instantly.
	return SessionInfo{ID: id, Status: StatusIdle, CreatedAt: now, Order: order}, nil
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

// sessionOrder is one session's sort key for an enumeration: its position in the
// shared display order, plus the two keys that settle a stray (see
// compareSessionOrder).
type sessionOrder struct {
	createdAt time.Time
	id        SessionID
	rank      int
}

// compareSessionOrder is the one order the session set is served in: the shared
// display order first, then oldest createdAt, then id.
//
// Both enumerations use it — List (GET /api/sessions) and snapshot (the status
// stream's initial sync) — so the two can never report a different order for the
// same set. Until 3.10.0 they did: snapshot ranged m.sessions and returned
// unsorted, Go randomizes map iteration per range, and a client that placed tabs
// in arrival order therefore got a different strip on every connect.
//
// The createdAt and id keys are NOT redundant behind rank. rankOf gives a
// session missing from m.order a rank past the end, and these two then order
// those strays deterministically instead of letting them alias onto position 0.
// That state is an invariant violation rather than a reachable case (see the
// order field), so this is how a desync degrades: stray sessions sort last, by
// age, and the strip stays stable. The id key also makes the order total, which
// is what lets a client treat it as a fact rather than a hint.
func compareSessionOrder(a, b sessionOrder) int {
	if a.rank != b.rank {
		return cmp.Compare(a.rank, b.rank)
	}
	if c := a.createdAt.Compare(b.createdAt); c != 0 {
		return c
	}
	return cmp.Compare(a.id, b.id)
}

// rankLocked maps each ordered session id to its display position. Caller holds
// m.mu.
func (m *SessionManager) rankLocked() map[SessionID]int {
	rank := make(map[SessionID]int, len(m.order))
	for i, id := range m.order {
		rank[id] = i
	}
	return rank
}

// rankOf reads a session's display position out of a rankLocked map, giving one
// the order does not name a position past the end rather than the zero value,
// which is position 0 and belongs to a real session.
func rankOf(rank map[SessionID]int, id SessionID, ordered int) int {
	if i, ok := rank[id]; ok {
		return i
	}
	return ordered
}

// dropFromOrderLocked removes id from the display order, closing the gap so
// positions stay dense. Caller holds m.mu; a no-op for an id not present.
func (m *SessionManager) dropFromOrderLocked(id SessionID) {
	if i := slices.Index(m.order, id); i >= 0 {
		m.order = slices.Delete(m.order, i, i+1)
	}
}

// SetSessionOrder replaces the display order every viewer shares, and is the
// whole write side of tab-order sync: the caller sends the complete list of live
// session ids in the order it wants them shown, and the next status sweep pushes
// each session's new position to every other client within a tick.
//
// The list must name the live session set EXACTLY — same length, every id live,
// no duplicates — and the whole write is refused otherwise. That one check does
// three jobs. It keeps positions dense and unique, which is what makes a
// position meaningful at all. It makes the write atomic, so two viewers
// reordering at once produce an arrangement one of them actually chose rather
// than an interleaving of both. And it turns a stale view into a refusal a
// client can react to: a caller that has not yet seen a session created or
// closed elsewhere is told so, instead of silently dropping that session out of
// the order or resurrecting a dead id into it.
//
// No revision or If-Match beyond that. The set check already detects every
// disagreement about WHICH sessions exist, and for a disagreement about their
// ORDER alone the outcome is the same either way: whoever wrote last chose the
// arrangement. A revision would only let a client discover that before being
// told, at the cost of a rebase problem (re-applying "move this tab to position
// 3" onto a list that changed underneath) with no better answer at the end.
//
// One precondition the set check CANNOT enforce, and a caller has to hold to it:
// the list must be derived from a fully applied view. A reorder reaches a client
// as one status event per moved session, so a client that re-sorts mid-burst
// briefly holds two sessions at one position; a list derived from that hybrid
// still names the live set exactly, so it is accepted and becomes the arrangement
// every device sees — an arrangement no user chose, and one that does not heal
// because it is now the server's answer. Apply the whole tick, then derive
// anything you send back.
//
// Returns false when the list does not match, which the HTTP layer reports as
// 409. The ids are not otherwise validated because they do not need to be: an id
// that is not a live session fails the membership check, so no untrusted value
// reaches the order.
func (m *SessionManager) SetSessionOrder(ids []SessionID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(ids) != len(m.sessions) {
		return false
	}
	seen := make(map[SessionID]struct{}, len(ids))
	for _, id := range ids {
		if _, live := m.sessions[id]; !live {
			return false
		}
		if _, dup := seen[id]; dup {
			return false
		}
		seen[id] = struct{}{}
	}
	// Cloned: the caller's slice is a decoded request body, and keeping it would
	// let a later write through that same slice reorder the manager's state.
	m.order = slices.Clone(ids)
	return true
}

// List returns all sessions in compareSessionOrder: the shared display order,
// then oldest first, then id.
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
		rank       int
		latched    bool
	}
	m.mu.Lock()
	rank := m.rankLocked()
	ordered := len(m.order)
	items := make([]listItem, 0, len(m.sessions))
	for _, s := range m.sessions {
		it := listItem{
			info: SessionInfo{
				ID: s.id, ClientTitle: s.clientTitle, PinnedTitle: s.pinnedTitle,
				CreatedAt: s.createdAt,
			},
			// Held beside the info rather than in info.Order: the published field is
			// the DENSE index assigned below, and one field carrying two meanings
			// across a sort is how a future edit publishes raw ranks by accident.
			// snapshot keeps the same split.
			rank:      rankOf(rank, s.id, ordered),
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

	for i := range items {
		it := &items[i]
		it.info.Status = refinedStatus(it.lastStatus, it.handler)
		it.info.Title = effectiveTitle(&titleSources{
			pinned: it.info.PinnedTitle, osc: it.handler.Title(),
			client: it.info.ClientTitle, auto: it.autoTitle,
		})
		// reportsActivity mirrors the status stream: sticky once any OSC 9;4
		// progress has been seen (Progress() >= 0), or a notification latched.
		it.info.ReportsActivity = it.handler.Progress() >= 0 || it.latched
	}
	slices.SortFunc(items, func(a, b listItem) int {
		return compareSessionOrder(
			sessionOrder{createdAt: a.info.CreatedAt, id: a.info.ID, rank: a.rank},
			sessionOrder{createdAt: b.info.CreatedAt, id: b.info.ID, rank: b.rank},
		)
	})
	// The published position is the index in THIS sequence, and this is the only
	// write to the field, so what a client sorts by is 0-based, dense and unique
	// whatever the manager's internal state happens to be. Taking it from the rank
	// map instead would let two sessions the order does not name (an invariant
	// violation, hence unreachable — but a wire contract should not rest on that)
	// both claim the same position.
	out := make([]SessionInfo, 0, len(items))
	for i := range items {
		items[i].info.Order = i
		out = append(out, items[i].info)
	}
	return out
}

// Close terminates a session and removes it. Returns false if the id is unknown.
func (m *SessionManager) Close(id SessionID) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
		m.dropFromOrderLocked(id)
		m.closed++
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	s.handler.Close()
	m.logger.Info("session: closed", "session", LogID(string(id)))
	return true
}

// SetSessionTitle sets a per-session client-derived automatic title, shown as
// the session's reported title whenever its program emits no OSC window title
// and the user has pinned no name. Returns false if the id is unknown. No
// explicit broadcast is needed: the 250ms status sweep (diffStatuses) detects
// the changed effective title and pushes it to subscribers within a tick.
func (m *SessionManager) SetSessionTitle(id SessionID, title string) bool {
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
func (m *SessionManager) SetSessionPinnedTitle(id SessionID, title string) bool {
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
func (m *SessionManager) ClearSessionPinnedTitle(id SessionID) bool {
	return m.SetSessionPinnedTitle(id, "")
}

// Shutdown stops the manager's loops, ends every session, and waits for their
// teardown to finish, bounded by ctx. It returns an error wrapping ctx.Err() when
// the budget expired with teardowns outstanding, naming how many, and nil when
// everything finished.
//
// A manager is SINGLE-USE. Shutdown cancels the status sweep and the idle reaper
// and nothing restarts them, so a manager reused afterwards serves sessions with
// no status stream and no idle reaping. Build a new one instead.
//
// Sessions are signalled first and waited on second, deliberately. Each teardown
// can spend several containGrace windows, so closing and waiting one session at a
// time would make the total scale with the tab count; signalling all of them
// first overlaps those windows inside one budget.
func (m *SessionManager) Shutdown(ctx context.Context) error {
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
	m.sessions = make(map[SessionID]*session)
	m.order = nil
	loopsDone := m.loopsDone
	m.mu.Unlock()
	for _, s := range victims {
		s.handler.Close()
	}

	outstanding := 0
	for _, s := range victims {
		if err := s.handler.wait(ctx); err != nil {
			outstanding++
		}
	}
	if outstanding > 0 {
		return fmt.Errorf("terminal: %d of %d session teardowns unfinished: %w",
			outstanding, len(victims), ctx.Err())
	}
	if loopsDone != nil {
		if err := waitClosed(ctx, loopsDone); err != nil {
			return fmt.Errorf("terminal: manager loops unfinished: %w", err)
		}
	}
	return nil
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
		s, ok := m.sessions[SessionID(id)] // URL boundary: convert the raw query value once
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
			// will never exist. The upgrade goes through acceptWS so this path
			// applies the SAME origin policy as a live session's socket
			// (WithManagerOriginPolicy); it used to hardcode nil options, which
			// silently ignored a configured allowlist here and answered a
			// legitimately-embedded client 403 instead of the readable 4004.
			// A non-WebSocket GET gets Accept's own 426 — the same answer the
			// known-session path gives, so a plain probe can no longer
			// distinguish session existence (the old 404-vs-426 oracle).
			ws, err := acceptWS(w, r, m.originPolicy)
			if err != nil {
				return // acceptWS or Accept already wrote the response (403/426)
			}
			_ = ws.Close(statusUnknownSession, "unknown session")
			return
		}
		defer m.clientDisconnected()
		s.handler.ServeHTTP(w, r) // blocks for the WS lifetime
	})
}

// RESTHandler serves the session REST API: POST SessionsPath (create),
// GET SessionsPath (list), PUT /api/sessions/order (set the shared display
// order), DELETE /api/sessions/{id} (close),
// PUT /api/sessions/{id}/title (set the client-derived automatic title), and
// PUT + DELETE /api/sessions/{id}/pinned-title (set / clear the user's name).
//
// The order route is a literal segment where the others take an {id}, which
// ServeMux prefers over a wildcard, and no session id can collide with it
// (ids are hex).
// Its internal patterns are absolute, so it only functions on the SessionsPath +
// SessionsSubtreePath mounts — MountSessionRoutes / MountAPI perform them
// (the route-set contract lives there); exported so consumer tests can stub it.
//
// Every response the returned handler produces states Cache-Control: no-store
// unless the header is already set (see withNoStore). The wrapper sits upstream
// of the mux rather than in the handlers so it also covers the responses the mux
// SYNTHESIZES — a 404 for an unserved path, a 405 for a method a path does not
// serve, a path-cleaning redirect — which are exactly the responses no handler
// is invoked to write, and which land on paths carrying a session id. Wrapping
// here rather than only at the mount also means a consumer that mounts this
// handler itself, without MountSessionRoutes, still gets the policy; the create
// gate is the one thing this site cannot reach, which is why MountSessionRoutes
// wraps again outside it.
func (m *SessionManager) RESTHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sessions", m.handleCreate)
	mux.HandleFunc("GET /api/sessions", m.handleList)
	mux.HandleFunc("PUT /api/sessions/order", m.handleSetOrder)
	mux.HandleFunc("DELETE /api/sessions/{id}", m.handleDelete)
	mux.HandleFunc("PUT /api/sessions/{id}/title", m.handleSetTitle)
	mux.HandleFunc("PUT /api/sessions/{id}/pinned-title", m.handleSetPinnedTitle)
	mux.HandleFunc("DELETE /api/sessions/{id}/pinned-title", m.handleClearPinnedTitle)
	return withNoStore(mux)
}

func (m *SessionManager) handleCreate(w http.ResponseWriter, _ *http.Request) {
	info, err := m.create()
	if err != nil {
		m.logger.Error("session: create failed", "error", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (m *SessionManager) handleList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, m.List())
}

// handleSetOrder serves PUT /api/sessions/order: the caller sends every live
// session id in the order it wants them shown. 204 on success, 409 when the list
// does not name the live set (see SetSessionOrder), 400 on a body this cannot
// read.
//
// 409 rather than 404 or 400 for a set mismatch: the request is well formed and
// the resource exists, the caller's view of the session set is simply behind. A
// client answers it by re-listing and sending again, which is also what it does
// when it learns of a session it had not seen.
func (m *SessionManager) handleSetOrder(w http.ResponseWriter, r *http.Request) {
	raw, ok := decodeOrderBody(w, r)
	if !ok {
		return
	}
	ids := make([]SessionID, len(raw))
	for i, id := range raw {
		ids[i] = SessionID(id)
	}
	if m.SetSessionOrder(ids) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "order must name every live session exactly once", http.StatusConflict)
}

func (m *SessionManager) handleDelete(w http.ResponseWriter, r *http.Request) {
	if m.Close(SessionID(r.PathValue("id"))) {
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
	if m.SetSessionTitle(SessionID(r.PathValue("id")), title) {
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
	if m.SetSessionPinnedTitle(SessionID(r.PathValue("id")), clean) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "unknown session", http.StatusNotFound)
}

// handleClearPinnedTitle removes a session's user-pinned name so its label falls
// back to the automatic sources. 204 on success (idempotent — clearing an
// unpinned session succeeds), 404 on an unknown id.
func (m *SessionManager) handleClearPinnedTitle(w http.ResponseWriter, r *http.Request) {
	if m.ClearSessionPinnedTitle(SessionID(r.PathValue("id"))) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "unknown session", http.StatusNotFound)
}

// decodeTitleBody reads the shared {"title": "..."} envelope both title routes
// use, writing the 400 itself on a body that is oversized or undecodable. The
// returned string is RAW: each caller applies its own sanitize bound.
// decodeTitleBody reads a title out of a PUT body. Deliberately laxer than
// decodeOrderBody, which requires its field and refuses trailing data: tightening
// this one turns a request a client gets away with today (trailing junk after a
// valid title object) into a 400, which is a behaviour change to two shipped
// routes and belongs in its own release rather than riding along with a new
// feature. Worth doing; not here.
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

// maxOrderBodyBytes bounds the order request body. It carries one session id per
// live session (a 32-char hex id plus JSON punctuation, so ~36 bytes each), and
// SetSessionOrder refuses any list that is not exactly the live set, so this cap
// only has to stop a flood from being DECODED before that check can refuse it.
// 64 KiB leaves room for far more sessions than a machine will fork.
const maxOrderBodyBytes = 64 * 1024

// decodeOrderBody reads the id list out of a PUT /api/sessions/order body,
// answering 400 itself when it cannot.
//
// The field must be PRESENT: a pointer, so a body that omits "order" or sends
// null is a malformed request rather than an empty reorder. The difference is
// visible only when no sessions are live, where an absent list would otherwise
// match the empty live set and answer 204 — reporting success for a client bug
// that sent the wrong shape. `{"order":[]}` against no sessions is still the
// honest empty reorder and still succeeds.
//
// Trailing data is refused too. A single Decode stops at the end of the first
// JSON value, so `{"order":[...]}{"order":[...]}` and `{...}garbage` would both
// be accepted on the strength of the part that parsed, and the caller would
// never learn that half its request was ignored.
func decodeOrderBody(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxOrderBodyBytes)
	var body struct {
		Order *[]string `json:"order"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return nil, false
	}
	if body.Order == nil {
		http.Error(w, "body must carry an order array", http.StatusBadRequest)
		return nil, false
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		http.Error(w, "unexpected data after the body", http.StatusBadRequest)
		return nil, false
	}
	return *body.Order, true
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
	m.sessions = make(map[SessionID]*session)
	m.order = nil
	m.reaped += uint64(len(victims))
	m.mu.Unlock()
	for _, s := range victims {
		s.handler.Close()
		m.logger.Info("session: reaped (no client for idle window)", "session", LogID(string(s.id)))
	}
}

// refinedStatus computes a session's status for a point-in-time read (List and
// the SSE snapshot) from the sweep's last computed status: that value carries
// the working/done/input refinement, so GET /api/sessions agrees with what the
// status stream pushes. Both sources MUST agree — a reloading client paints its
// activity dots from whichever answers last, and when List still reported the
// coarse liveness status a reload visibly downgraded a latched done/input dot
// back to idle until the next real transition. Process exit is checked live and
// always wins (it is definitive, while lastStatus can lag a sweep tick behind),
// and it carries the same exited-vs-crashed split the sweep applies, so the two
// sources cannot disagree about HOW a session ended either; an empty lastStatus
// (created within the current sweep tick) is the new-session default, idle.
func refinedStatus(lastStatus string, h *Handler) string {
	if exited, crashed := h.exitOutcome(); exited {
		if crashed {
			return StatusCrashed
		}
		return StatusExited
	}
	if lastStatus != "" {
		return lastStatus
	}
	return StatusIdle
}

// newSessionID returns a 128-bit crypto-random hex id, used as both the routing
// id (/ws?session=<id>) and the resume id.
func newSessionID() (SessionID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return SessionID(hex.EncodeToString(b[:])), nil
}

// writeJSON writes a session-surface JSON response. Its only two callers are the
// responses that carry session ids in full — handleCreate (201, the new id) and
// handleList (200, every live id) — which is why no-store is set here rather than
// left to each consumer.
//
// A session id IS a capability: it is the credential /ws attaches and resumes
// with, which is why this package refuses to log one whole (see LogID). A 200/201
// JSON body with no Cache-Control is heuristically cacheable under RFC 9111, so
// without this header the same value the engine will not put in a log file can be
// written to a browser's disk cache or held by an intermediary. Consumers were
// closing that gap unevenly — web-terminal-kiro wrapped the surface in its own
// no-store middleware, web-terminal-server set no cache directive at all — and a
// credential's cache policy belongs to whoever issues the credential, so the
// default lives here. A consumer may still add a broader policy of its own (kiro
// covers its /api/tools and /api/health routes the same way); setting the same
// header twice is idempotent.
//
// This is the surface's one UNCONDITIONAL setter, and the difference from
// withNoStore is deliberate: that wrapper yields to a Cache-Control an outer
// consumer already set, because the responses it covers carry no credential and
// only their URL is sensitive. These two bodies CONTAIN the id, so their
// prohibition is not the consumer's to relax, and the value is written whatever
// an outer stack asked for.
func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set(cacheControlHeader, noStorePolicy)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // #nosec G104 -- client hangup is not actionable
}
