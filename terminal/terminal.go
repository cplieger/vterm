// Package terminal bridges a PTY to a browser WebSocket.
//
// Each WS connection spawns the configured command in its own PTY and
// pipes bytes both ways. Server-side state is kept in the VT screen;
// on reconnect the current cell snapshot is replayed to the new client.
// No external multiplexer is involved — the VT emulator IS the
// persistence layer.
//
// Wire protocol (binary WebSocket frames):
//
//	client → server: raw terminal input bytes
//	server → client: binary frames encoding screen/scroll/modes/title/
//	                 resumeAck/pong messages (see wire_binary.go) — PTY
//	                 output is rendered into the VT screen and sent as
//	                 absolute-indexed cell runs, not as raw bytes
//	client → server: JSON control messages prefixed with 0x00:
//	  {"type":"resize",...}, {"type":"resume",...}, {"type":"ping"}
//
// The 0x00 prefix byte distinguishes control messages from raw
// input; no valid terminal input starts with NUL.
package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/cplieger/web-terminal-engine/v3/vt"
	"github.com/creack/pty"
)

const (
	wsReadLimit   = 64 * 1024
	ptyReadBuf    = 4096
	defaultCols   = 120
	defaultRows   = 30
	flushInterval = 50 * time.Millisecond

	// redrawSettleQuiet / redrawSettleCap parameterize the redraw-settle hold
	// (armRedrawSettle): after a size change, flushes stay held until the
	// child's redraw output has been quiet for redrawSettleQuiet, but never
	// longer than redrawSettleCap in total. Quiet must exceed the child's
	// inter-chunk rendering gaps (kiro-cli brackets its reprint in many small
	// DEC 2026 batches, milliseconds apart); the cap bounds the freeze when a
	// resize lands mid-stream and the output never goes quiet.
	redrawSettleQuiet = 150 * time.Millisecond
	redrawSettleCap   = time.Second

	// healDebounce is how long the handler waits after a client disconnects
	// before relaxing the shared screen to the smallest size the remaining
	// clients need. It absorbs a brief reconnect (iOS wake, network blip): a
	// client that drops and re-attaches at the same size within the window
	// causes no grow-then-shrink flap, because the recompute at fire time counts
	// the re-attached socket. A genuinely departed client is gone well before it
	// elapses — a clean close is immediate, and an ungraceful drop is already
	// ping-confirmed (~20-45s) by the time Remove runs.
	healDebounce = 3 * time.Second

	// statusProcessExited (4001) is the WebSocket close code the terminal WS
	// uses when the child process exits, so a client can tell a dead session
	// (the tab should close) apart from a transient disconnect (reconnect).
	// 4001 is in the private application close-code range (4000-4999).
	statusProcessExited websocket.StatusCode = 4001

	// WireIncompatibleCloseCode is the definitive WebSocket close code for an
	// explicitly declared peer revision below the receiver's supported floor.
	// The server uses it when rejecting a stale client; clients may use the
	// same code when an explicit server revision is below their floor. It is
	// exported so independently released consumers can recognize the contract.
	WireIncompatibleCloseCode websocket.StatusCode = 4002

	// wireIncompatibleClientReason is intentionally actionable and short enough
	// for the WebSocket close-reason limit. The detailed versions are logged.
	wireIncompatibleClientReason = "client wire protocol is below the server minimum; reload or upgrade the client"

	// statusUnknownSession (4004) is the close code for a WS connect whose
	// ?session= id the manager does not know (reaped, closed elsewhere, or a
	// restarted server). The upgrade is ACCEPTED and then closed with this
	// code: a plain pre-upgrade 404 surfaces in the browser as an opaque
	// failed connect (code 1006, reason unreadable from JS), which the client
	// can only treat as transient — an endless "Reconnecting…" flap against a
	// session that will never exist. Like 4001 it is DEFINITIVE ("this
	// session will never produce output"), and the client routes both to the
	// same ended state. 4004 for the 404 mnemonic.
	statusUnknownSession websocket.StatusCode = 4004

	// exitedAttachReplayGrace bounds how long a client that attaches to an
	// ALREADY-exited session may take to complete its resume exchange before
	// the definitive statusProcessExited close is sent. Without this grace the
	// close raced (and in practice beat) the resumeAck + final-screen replay,
	// so a client re-attaching to a dead session received nothing renderable —
	// it saw only an instant 4001 (or, on clients that treat every close as
	// transient, an infinite reconnect loop). The client sends resume as its
	// first frame after open, so the grace only ever runs its full length for
	// a client that never speaks the resume protocol.
	exitedAttachReplayGrace = 3 * time.Second

	// maxScrollLinesPerFrame bounds the lines packed into one scroll frame so the wire
	// num_lines (a uint16) can never be exceeded by the payload and a large drained burst
	// (a fast child can produce far more than 65535 lines in one 50ms flush) is split into
	// several < ~100KB frames instead of one multi-MB message. Mirrors handleResume's
	// replayChunk. Any value well under 65535 works; 1000 keeps each frame small.
	maxScrollLinesPerFrame = 1000

	// minResizeCols/minResizeRows are the smallest dimensions we
	// accept from a resize control message. Anything below is floored
	// up rather than dropped — iPad keyboard slide reports near-zero
	// during animations and we want the start path to fire even if
	// the first resize comes from such a frame.
	minResizeCols = 20
	minResizeRows = 5

	// maxResizeCols/maxResizeRows bound the eagerly-allocated grid. The VT
	// screen allocates cols*rows Cells, so the winsize field width (0xFFFF)
	// is not a memory bound: a 65535x65535 resize allocates ~4.3e9 Cells
	// (>250 GB) and OOMs the host. Cap far above any real display but well
	// below OOM territory; raise for a genuine ultra-wide layout.
	maxResizeCols = 1000
	maxResizeRows = 1000

	ctlTypeResize = "resize"
	ctlTypeResume = "resume"
	// ctlTypeUpgrade is the v4 typed-framing transition control: sent as the
	// first TEXT message by a client that received proof (resumeAck
	// serverWireVersion >= 4). Recognizing it latches the connection to typed
	// mode; the message itself is otherwise a no-op.
	ctlTypeUpgrade = "upgrade"
	ctlTypePing    = "ping"

	// scrollbackCapacity is the number of scrollback lines the server
	// retains for replay to new/reconnecting clients. Matches the
	// client's MAX_HISTORY so a full page refresh recovers all history
	// the client would have kept anyway.
	scrollbackCapacity = 1000
)

// Option configures optional behavior of the Handler.
type Option func(*handlerConfig)

// handlerConfig holds optional configuration applied via functional options.
type handlerConfig struct {
	logger             *slog.Logger
	acceptOptions      *websocket.AcceptOptions
	onProcessExit      func(error)
	theme              *vt.Theme
	workDir            string
	commandLogValue    string
	env                []string
	scrollbackCapacity int
	keepUnfocused      bool
	inputTitle         bool
}

// WithInputTitle enables the input-derived session title: the engine watches the
// input stream and latches the first eligible submitted line as the session's
// name (see inputtitle.go). Off by default.
//
// Turn it on for a session-per-conversation shell whose program sets no useful
// OSC window title, where "what the user asked for" is the best available name —
// the derived title then outranks the OSC title in the reported Title. Leave it
// off for a general-purpose terminal, where the foreground-process ladder is a
// better automatic label and the shell's own OSC title is usually meaningful.
//
// It observes only what a client actually sends to the PTY, so it is naming the
// session, not any one client: the label is identical for every attached client
// and survives a reload with no client round trip.
func WithInputTitle() Option {
	return func(c *handlerConfig) { c.inputTitle = true }
}

// WithWorkDir sets the working directory for the spawned process.
func WithWorkDir(dir string) Option {
	return func(c *handlerConfig) { c.workDir = dir }
}

// WithLogger injects a structured logger; nil disables logging.
func WithLogger(l *slog.Logger) Option {
	return func(c *handlerConfig) {
		if l == nil {
			// A nil *slog.Logger panics on method calls; use a discard handler.
			l = slog.New(slog.DiscardHandler)
		}
		c.logger = l
	}
}

// WithCommandLogValue replaces the value the process-start log line records
// as its "command" attribute. By default the line carries the child's full
// argv; a consumer whose argv embeds operator-supplied values that could
// carry a credential (CWE-532 — e.g. launch flags interpolated from a
// compose file) passes a fixed marker such as "[redacted]" so the launch
// event stays logged, and greppable by the stable "command" key, without
// disclosing the argument values. This is the engine's only argv-bearing
// log site, so the option makes the whole lifecycle log safe for sensitive
// argv without the consumer wrapping the logger in a redacting slog.Handler.
// An empty v is ignored (the package's skip-zero option convention), keeping
// the default argv logging.
func WithCommandLogValue(v string) Option {
	return func(c *handlerConfig) {
		if v != "" {
			c.commandLogValue = v
		}
	}
}

// WithEnv sets additional environment variables for the spawned process.
func WithEnv(env []string) Option {
	return func(c *handlerConfig) { c.env = env }
}

// WithScrollbackCapacity sets the number of scrollback lines retained
// for replay to reconnecting clients. Default is 1000. Negative values
// are treated as 0 (scrollback disabled).
func WithScrollbackCapacity(n int) Option {
	return func(c *handlerConfig) {
		c.scrollbackCapacity = max(n, 0)
	}
}

// WithAcceptOptions sets WebSocket accept options (e.g. allowed origins).
func WithAcceptOptions(o *websocket.AcceptOptions) Option {
	return func(c *handlerConfig) { c.acceptOptions = o }
}

// WithOnProcessExit registers a callback invoked when the child process exits.
func WithOnProcessExit(fn func(error)) Option {
	return func(c *handlerConfig) { c.onProcessExit = fn }
}

// WithKeepUnfocused makes the server hold the child process in the "unfocused"
// state for DEC 1004 focus reporting: whenever the process enables focus
// reporting (CSI ?1004h), the server writes a focus-out (ESC [ O) to its PTY,
// and it never writes a focus-in. A process that gates behavior on focus (for
// example kiro-cli, which only emits its OSC 9 turn/permission notifications
// while it believes it is unfocused) then keeps emitting, so the session
// manager's status classifier can observe those notifications. Off by default:
// a generic terminal wants real focus reporting (vim, etc.), so only a consumer
// that relies on the unfocused-notifier behavior enables it. The browser client
// is expected to emit no focus bytes of its own under this model.
func WithKeepUnfocused() Option {
	return func(c *handlerConfig) { c.keepUnfocused = true }
}

// WithTheme sets the default foreground, background, and cursor colors the
// terminal reports to programs via OSC 10/11/12 queries (and restores on
// OSC 110/111/112 reset). Pass the colors your client actually renders so
// color-probing apps — light/dark detection, "reset to default" — see the real
// theme. Defaults to vt.DefaultTheme (a dark scheme). Build colors with vt.RGB.
func WithTheme(t vt.Theme) Option {
	return func(c *handlerConfig) { c.theme = &t }
}

// sessionState persists across WS reconnects for the same logical
// client. The client identifies its session via the resume control
// message; the server uses sessionState.bytesReceived as the ack value
// to send back, which the client compares to its sent count to
// determine which bytes (if any) need retransmission after a blip.
type sessionState struct {
	lastSeen      time.Time
	bytesReceived uint64
}

// clientState tracks per-WS-connection state. session is resolved
// from the sessionId in the resume control message. session is stored
// as an atomic.Pointer so IncrementReceived can test whether a session
// is attached without taking registry.mu; the pointed-to sessionState's
// fields are guarded by the clientRegistry's mutex (registry.mu), not h.mu.
type clientState struct {
	session atomic.Pointer[sessionState]
	// lastAckSent is the most recent inputAck value actually written to this
	// socket (stamped on a content frame by dispatchFrame, sent bare by a
	// no-frame scheduler pass's ackOnly sweep, or carried by handleResume's
	// resumeAck). The sweep compares it to the session's bytesReceived so input
	// into a silent app is acknowledged on the next dirty pass. Atomic because
	// handleResume (per-connection goroutine) and flushLoop both write it.
	lastAckSent atomic.Uint64
	// cols/rows are this socket's most recently requested terminal size,
	// guarded by clientRegistry.mu (NOT by the atomic session pointer). They
	// feed MinLiveSize so a disconnect can relax the shared screen to the
	// smallest size the remaining sockets need.
	cols int
	rows int
}

// Handler serves /ws and tracks shared screen state. Multiple WS clients
// can attach concurrently; the VT screen is the session state.
//
// h.started is atomic.Bool so the fast-path check in handleWS does not
// race with ensureStarted's write under h.mu. Screen and PTY state is
// guarded by h.mu; client tracking lives in the clientRegistry with its
// own lock. flushLoop snapshots the per-flush data under h.mu and then
// performs ws.Write outside the lock so a slow client can't block
// readLoop / handleControl / new handleWS connections.
type Handler struct {
	// inputTitle derives a session name from the input stream when the consumer
	// asked for it (WithInputTitle); nil otherwise, and title() reads nil as "".
	// Guarded by h.mu, like the screen state it sits beside.
	inputTitle *inputTitleDeriver
	cmd        *exec.Cmd
	screen     *vt.Screen
	registry   *clientRegistry
	builder    *flushFrameBuilder
	scrollback *scrollbackRing
	cancel     context.CancelFunc
	ptmx       *os.File
	procExitCh chan struct{}
	// dirty is the flush scheduler's wakeup: 1-buffered so any number of
	// markDirty pokes coalesce into one pending signal. flushLoop sleeps on
	// it when idle — no ticker, no periodic wakeups (P4).
	dirty     chan struct{}
	healTimer *time.Timer
	// redrawSettleUntil / redrawLastData implement the redraw-settle hold
	// (armRedrawSettle / redrawHoldUntil). Both guarded by h.mu; a zero
	// redrawSettleUntil means the hold is inactive.
	redrawSettleUntil time.Time
	redrawLastData    time.Time
	pendingClipboard  []byte
	command           []string
	cfg               handlerConfig
	bootEpoch         int64
	lastActivity      atomic.Int64
	mu                sync.Mutex
	started           atomic.Bool
	// sizeEstablished is latched true once the PTY has real dimensions (the
	// eager start's default size, or a client resize) and never cleared: the
	// flush builder emits nothing before it, so clients never see a frame
	// rendered against the zero-size screen. It does NOT mean "a resize
	// happened this tick" (its former name, `resized`, invited that reading).
	sizeEstablished          bool
	scrollbackClearedPending bool
	paletteChangedPending    bool
	lastFocusReporting       bool
	// autoTitleWarned makes the automatic-title probe's failure note once-per-
	// session rather than once-per-sweep (see probeAutoTitle). Guarded by h.mu.
	autoTitleWarned bool
}

// NewHandler returns a terminal handler. command is the argv to spawn
// (required, must be non-empty). Optional behavior is configured via
// functional Option values.
func NewHandler(command []string, opts ...Option) *Handler {
	cfg := handlerConfig{
		scrollbackCapacity: scrollbackCapacity,
		logger:             slog.Default(),
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	var vtOpts []vt.Option
	if cfg.theme != nil {
		vtOpts = append(vtOpts, vt.WithTheme(*cfg.theme))
	}
	var derived *inputTitleDeriver
	if cfg.inputTitle {
		derived = &inputTitleDeriver{}
	}
	return &Handler{
		inputTitle: derived,
		command:    command,
		cfg:        cfg,
		screen:     vt.New(defaultRows, defaultCols, vtOpts...),
		registry:   newClientRegistry(cfg.logger),
		builder:    &flushFrameBuilder{},
		scrollback: newScrollbackRing(cfg.scrollbackCapacity),
		bootEpoch:  time.Now().UnixNano(),
		procExitCh: make(chan struct{}),
		dirty:      make(chan struct{}, 1),
	}
}

// markDirty pokes the flush scheduler: some state that could produce a frame
// changed (PTY output, resize, resume, input needing an ack sweep). Non-
// blocking — the 1-buffered channel coalesces any number of pokes into one
// pending wakeup, which is all the scheduler needs.
func (h *Handler) markDirty() {
	select {
	case h.dirty <- struct{}{}:
	default:
	}
}

// RegisterRoutes wires /ws on mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/ws", h.handleWS)
}

// ServeHTTP implements http.Handler, delegating to the WebSocket handler.
// Used by the host application to wire the terminal as an http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handleWS(w, r)
}

// Shutdown cancels the readLoop and flushLoop goroutines and closes
// the PTY. Safe to call even if the process was never started.
func (h *Handler) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.healTimer != nil {
		h.healTimer.Stop()
	}
	if h.cancel != nil {
		h.cancel()
	}
	if h.ptmx != nil {
		_ = h.ptmx.Close() // best-effort during shutdown
	}
}

// Title returns the current window title (set by the process via OSC 0/2), for
// a session manager or UI to label the session. Empty until the process sets a
// title. Safe for concurrent use; read under the same lock that guards the VT
// screen.
func (h *Handler) Title() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.screen.Title
}

// Exited reports whether the child process has exited. Non-blocking and
// race-free (procExitCh is closed exactly once, by the process monitor). False
// for a handler whose process was never started.
func (h *Handler) Exited() bool {
	select {
	case <-h.procExitCh:
		return true
	default:
		return false
	}
}

// LastActivity returns the time of the most recent PTY output, or the zero time
// if the process has produced nothing yet. The status stream uses it to derive
// working (recent output) vs idle (quiescent). Lock-free.
func (h *Handler) LastActivity() time.Time {
	ns := h.lastActivity.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Notification returns the last OSC 9 notification message and its sequence
// number (vt.Screen.NotificationSeq). A reader detects a fresh notification when
// the sequence advances, even if the message text repeats. Safe for concurrent
// use.
func (h *Handler) Notification() (msg string, seq uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.screen.Notification, h.screen.NotificationSeq
}

// Progress returns the session's last ConEmu OSC 9;4 progress state: -1 when
// none has been seen (the process never reported progress), else the state
// (0 off, 1 value, 2 error, 3 indeterminate, 4 paused). The status stream maps
// an active state (1 or 3) to working, so a progress-reporting program (kiro-cli
// while the agent works) drives the working indicator without relying on raw
// output activity. Safe for concurrent use.
func (h *Handler) Progress() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.screen.Progress
}

// statusSnapshot returns the status-relevant screen state — the OSC 9;4
// progress state, the last OSC 9 notification message and its sequence, and
// the OSC 0/2 window title — under a SINGLE lock acquisition, so the status
// sweep's per-session snapshot is internally consistent. Reading the same
// fields through the individual getters (Progress, Notification, Title) takes
// the lock once per call, and a PTY chunk parsed between two of those calls
// can pair a stale active progress with a fresh turn-end notification — the
// inconsistent pairing computeStatus must not see (its fresh-latch precedence
// guards the state machine; this getter removes the torn read).
func (h *Handler) statusSnapshot() (progress int, notifMsg string, notifSeq uint64, title, derivedTitle string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.screen.Progress, h.screen.Notification, h.screen.NotificationSeq,
		h.screen.Title, h.inputTitle.title()
}

// observeInputTitle feeds one input chunk to the derived-title state machine, and
// is a no-op unless WithInputTitle was set. Cheap enough to call per frame: it is
// a byte loop that stops entirely once a title has latched.
func (h *Handler) observeInputTitle(chunk []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inputTitle != nil {
		h.inputTitle.observe(chunk)
	}
}

// titles returns the two handler-owned title sources under ONE lock acquisition:
// the program's OSC window title and the input-derived title. One getter rather
// than two so a caller cannot pair a fresh OSC title with a stale derived one.
func (h *Handler) titles() (osc, derived string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.screen.Title, h.inputTitle.title()
}

// StartEager starts the child process now at a default size, rather than lazily
// on the first client message. A session manager calls this at Create time so a
// new session's process (and its activity signal) exist from creation; the first
// client attach still sends the real resize. Idempotent.
func (h *Handler) StartEager() error {
	return h.ensureStarted(0, 0)
}

// exitAwareCloseCode returns statusProcessExited (4001) when the child process
// has exited (procExitCh is closed), otherwise a normal closure. The
// non-blocking receive is race-free: channel operations synchronize and a
// closed channel is always ready.
func (h *Handler) exitAwareCloseCode() websocket.StatusCode {
	select {
	case <-h.procExitCh:
		return statusProcessExited
	default:
		return websocket.StatusNormalClosure
	}
}

// closeOnProcExit closes the client WS with statusProcessExited (4001) when the
// child process exits, so the client can tell "process ended" (terminal, close
// the tab) from a transient drop (reconnect). Canceling the read's context
// instead would make coder/websocket fail the connection abnormally (1006)
// rather than send a clean 4001, so this closes the socket directly;
// coder/websocket permits Close concurrent with the read loop's Read. It also
// returns when the client leaves (ctx done), so it never leaks.
//
// A client that attaches to an ALREADY-exited session (re-opening a dead tab,
// or a page reload whose saved session died meanwhile) is given up to
// exitedAttachReplayGrace to complete its resume exchange first — resumeServed
// is closed by handleWS once handleResume has synchronously written the
// resumeAck, modes/title, final screen, and history replay — so the client can
// render the session's last state before the definitive 4001 lands. Closing
// immediately (the previous behavior) raced the replay and reliably beat it,
// leaving the client with nothing but the close. The mid-session exit path
// (the process dies while the client is attached) keeps the immediate close:
// that client already holds the screen, and prompt 4001 delivery is the
// contract.
func (h *Handler) closeOnProcExit(ctx context.Context, ws *websocket.Conn, resumeServed <-chan struct{}) {
	alreadyExited := h.Exited()
	select {
	case <-ctx.Done():
		return
	case <-h.procExitCh:
	}
	if alreadyExited {
		t := time.NewTimer(exitedAttachReplayGrace)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-resumeServed:
		case <-t.C:
		}
	}
	ws.Close(statusProcessExited, "") // #nosec G104 -- best-effort
}

// ensureStarted spawns the process if not already running, sized at
// the given dimensions. cols/rows ≤ 0 fall back to defaults so callers
// who don't yet know the client size can still start the process.
// Idempotent: concurrent callers all see started==true after the
// first returns; cols/rows on subsequent calls are ignored.
func (h *Handler) ensureStarted(cols, rows int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started.Load() {
		return nil
	}
	if len(h.command) == 0 {
		return errors.New("terminal: empty command")
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	cmd := exec.CommandContext(ctx, h.command[0], h.command[1:]...) // #nosec G204
	// Force-kill a child that ignores the PTY-close SIGHUP: Shutdown/reap cancels ctx
	// (default Cancel = SIGKILL) and WaitDelay bounds the grace so cmd.Wait cannot
	// block the monitor goroutine forever.
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = h.cfg.workDir
	// Advertise a capable, well-known terminal identity so apps enable their
	// full feature set. TERM/COLORTERM unlock 256-color + truecolor. TERM_PROGRAM
	// iTerm.app (>= 3.6.6) is the single identity that unlocks OSC 9;4 progress
	// for BOTH kiro-cli (allowlists iTerm.app/WezTerm/Windows Terminal) and
	// Claude Code (iTerm.app >= 3.6.6), plus DEC 2026 synchronized output — all
	// of which this engine implements. Capabilities it does NOT implement
	// (inline images, the kitty IMAGE protocol, and the kitty keyboard flags
	// beyond the implemented disambiguate subset — see vt/kitty.go and the
	// README keyboard section) are consumed silently and never mis-rendered,
	// so over-claiming degrades gracefully rather than corrupting the screen.
	// h.cfg.env is appended last so a consumer's WithEnv can override any of
	// these (last value wins).
	env := append(os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"TERM_PROGRAM=iTerm.app",
		"TERM_PROGRAM_VERSION=3.6.6",
	)
	env = append(env, h.cfg.env...)
	cmd.Env = env
	if cols < 1 {
		cols = defaultCols
	}
	if rows < 1 {
		rows = defaultRows
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return err
	}
	h.ptmx = ptmx
	h.cmd = cmd
	h.started.Store(true)
	h.sizeEstablished = true
	h.screen.Resize(rows, cols)
	loggedCommand := any(h.command)
	if h.cfg.commandLogValue != "" {
		loggedCommand = h.cfg.commandLogValue
	}
	h.cfg.logger.Info("terminal: process started",
		"pid", cmd.Process.Pid, "command", loggedCommand, "cols", cols, "rows", rows)

	// PTY reader goroutine — feeds VT screen and notifies clients.
	go h.readLoop(ctx)
	// Flush scheduler — sends screen updates to all clients.
	go h.flushLoop(ctx)
	// Process monitor — reaps the child (so it does not linger as a
	// zombie), fires the documented onProcessExit callback with the
	// exit status, and cancels the read/flush loops on natural child
	// exit so the scheduler goroutine does not leak after the process dies.
	go func() {
		werr := cmd.Wait() // reap; werr carries the exit status
		// Guarantee client notification (procExitCh drives the 4001 close) and
		// loop teardown even if the consumer onProcessExit callback panics; a
		// callback panic must not crash the server or strand attached clients.
		// procExitCh is closed exactly once: this monitor runs once per handler.
		defer func() {
			// Broadcast process exit so attached WS clients close with
			// statusProcessExited (4001) rather than a normal closure.
			close(h.procExitCh)
			cancel() // stop readLoop/flushLoop on child exit
			// Free the PTY master fd immediately on natural exit; otherwise an
			// exited-but-undeleted session holds it until Shutdown/reap (reaper
			// is off by default). A later Shutdown's second Close is a no-op.
			h.mu.Lock()
			if h.ptmx != nil {
				_ = h.ptmx.Close() // #nosec G104 -- best-effort; child already exited
			}
			h.mu.Unlock()
			if r := recover(); r != nil {
				h.cfg.logger.Error("terminal: onProcessExit callback panicked", "panic", r)
			}
		}()
		// Symmetric with the "process started" INFO above so operators see the
		// session lifecycle end and its exit status in the logs, not just the
		// start. werr is nil on a clean (exit 0) shutdown; a child exiting
		// non-zero is a normal session end, not a server fault, so this stays
		// INFO (avoids WARN-spam on ordinary command exits).
		h.cfg.logger.Info("terminal: process exited",
			"pid", cmd.Process.Pid, "error", werr)
		if h.cfg.onProcessExit != nil {
			h.cfg.onProcessExit(werr)
		}
	}()
	return nil
}

func (h *Handler) readLoop(ctx context.Context) {
	buf := make([]byte, ptyReadBuf)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := h.ptmx.Read(buf)
		if n > 0 {
			h.handlePTYData(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// focusOutSeq is the DEC 1004 focus-out report (ESC [ O). Written to the PTY
// under WithKeepUnfocused when the process enables focus reporting.
var focusOutSeq = []byte("\x1b[O")

// focusOutOnEnable returns focusOutSeq when WithKeepUnfocused is set and focus
// reporting just rose from disabled to enabled since the last call, else nil. It
// updates the tracked last state, so it fires once per enable edge (a process
// that toggles 1004 off then on is re-pinned to unfocused). The caller holds
// h.mu and writes the returned bytes to the PTY outside the lock.
func (h *Handler) focusOutOnEnable() []byte {
	if !h.cfg.keepUnfocused {
		return nil
	}
	now := h.screen.FocusReporting
	rising := now && !h.lastFocusReporting
	h.lastFocusReporting = now
	if rising {
		return focusOutSeq
	}
	return nil
}

// handlePTYData feeds raw PTY output to the screen under h.mu and writes
// any query response back outside the lock so a slow write never stalls
// goroutines waiting on h.mu.
func (h *Handler) handlePTYData(data []byte) {
	h.lastActivity.Store(time.Now().UnixNano())
	var resp []byte
	h.mu.Lock()
	if !h.redrawSettleUntil.IsZero() {
		// Redraw-settle hold armed (post-resize): every PTY byte is redraw
		// output, so it extends the quiet window (redrawHoldUntil).
		h.redrawLastData = time.Now()
	}
	h.screen.Write(data) //nolint:errcheck // screen.Write always returns nil
	if h.screen.TakeScrollbackCleared() {
		// ED3 (erase scrollback): the app discarded its saved lines (kiro-cli
		// does this on every resize redraw). Clear the retained ring to match a
		// real terminal — Clear preserves committed so absolute indices stay
		// monotonic — and flag the next frame to tell clients to drop history.
		h.scrollback.Clear()
		h.scrollbackClearedPending = true
	}
	if h.screen.TakePaletteChanged() {
		// OSC 4/104 changed the palette; defer a full repaint to the next frame.
		h.paletteChangedPending = true
	}
	if clip := h.screen.TakeClipboard(); len(clip) > 0 {
		// OSC 52 copy; hand it to the next frame as a clipboard message.
		h.pendingClipboard = clip
	}
	resp = h.screen.TakeResponse()
	// Keep-unfocused: if the process just enabled focus reporting, pin it to
	// unfocused so a focus-gated notifier keeps emitting (see WithKeepUnfocused).
	if fo := h.focusOutOnEnable(); fo != nil {
		resp = append(resp, fo...)
	}
	h.mu.Unlock()
	// PTY output is the primary dirty source: wake the flush scheduler.
	h.markDirty()
	if len(resp) > 0 {
		h.ptmx.Write(resp) //nolint:errcheck // best-effort
	}
}

// flushFrame is the per-flush snapshot built under h.mu and consumed
// outside the lock. Holding the lock during the network write would
// stall every other goroutine on a slow client; the snapshot pattern
// keeps the lock window bounded to local memory work.
type flushFrame struct {
	clients          map[*websocket.Conn]uint64
	rows             [][]vt.WireRun
	scrollLines      [][]vt.WireRun
	changed          []int
	modesPayload     []byte
	titlePayload     []byte
	clipboardPayload []byte
	base             uint64 // absolute index of the top screen row (changed[y] -> base+y)
	scrollFirstIdx   uint64 // absolute index of scrollLines[0]
	curRow           int
	curCol           int
	screenHeight     int
	cursorStyle      uint8
	cursorHidden     bool
	cursorBlink      bool
	altActive        bool
	bell             bool
	// scrollbackCleared signals the client to drop its scrollback history
	// (all indices below base) because the app issued ED3 (erase scrollback).
	scrollbackCleared bool
}

// buildFrame computes the next outbound frame under h.mu. Returns a nil frame
// if there is nothing to send (no resize yet, flush held, no attached
// clients, or no changed rows and no scroll lines). holdUntil is non-zero
// when a flush hold is active — a DEC 2026 synchronized-output hold or the
// post-resize redraw-settle hold — and the scheduler arms a retry at that
// deadline so a final held redraw with no subsequent PTY byte still flushes
// (a trigger-only scheduler would strand it).
// retainSuspendedScrollback drains lines produced with no attached clients into
// the retained main-screen history. The caller holds h.mu. One-shot signals
// deliberately remain pending for the next attach.
func (h *Handler) retainSuspendedScrollback() {
	if !h.sizeEstablished {
		return
	}
	drained := h.screen.DrainScrollback()
	if h.screen.InAltScreen || h.builder.altTransitionPending(h.screen) || len(drained) == 0 {
		return
	}
	h.scrollback.Append(drained)
}

func (h *Handler) buildFrame() (frame *flushFrame, holdUntil time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()

	clients := h.registry.Snapshot()
	if len(clients) == 0 {
		// Zero-client suspension (P4): nobody to render for. Retain history
		// but skip RenderRowWire/diff entirely. One-shot signals (palette
		// repaint, ED3 clear, OSC 52 clipboard) stay pending for the next
		// attach, whose builder reset forces their visible effect.
		h.retainSuspendedScrollback()
		return nil, time.Time{}
	}
	// An OSC 4/104 palette change re-colors already-drawn cells; force a full
	// repaint so every visible row re-resolves through the new palette. The
	// Reset persists if Build produces no frame this pass (flush held / no
	// resize yet), so the repaint still lands on the next frame.
	if h.paletteChangedPending {
		h.builder.Reset()
		h.paletteChangedPending = false
	}
	if h.screen.IsFlushHeld() {
		holdUntil = h.screen.FlushHoldUntil
	}
	// The redraw-settle hold (armRedrawSettle) gates frames exactly like a
	// DEC 2026 hold but is immune to the child's ESU; fold its deadline in so
	// the scheduler arms a retry at whichever hold lapses last.
	if hu := h.redrawHoldUntil(time.Now()); hu.After(holdUntil) {
		holdUntil = hu
	}
	committedBefore := h.scrollback.Committed()
	if holdUntil.IsZero() {
		frame = h.builder.Build(h.screen, h.sizeEstablished, clients, committedBefore)
	}
	if frame != nil && len(frame.scrollLines) > 0 {
		h.scrollback.Append(frame.scrollLines)
	}
	if frame != nil && h.scrollbackClearedPending {
		frame.scrollbackCleared = true
		// scrollbackCleared only rides a screen message (dispatchFrame gates the
		// screen payload on len(changed) > 0). A frame with no changed rows -- a
		// title- or modes-only change arriving after ED3 -- sets the flag but emits
		// no screen payload, silently dropping the clear signal (the client keeps
		// history the server discarded until a resume). Fold the cursor row into
		// changed so a screen payload carries the flag this frame, mirroring the
		// bell handling in flush_builder.go. frame came from Build here (the
		// clipboard-only frame is created later), so frame.rows/curRow are valid.
		frame.changed = appendRowIfMissing(frame.changed, frame.curRow, len(frame.rows))
		h.scrollbackClearedPending = false
	}
	// OSC 52 clipboard is a one-shot event that can arrive with no screen
	// change, so ensure it rides a frame even when Build produced none.
	if len(h.pendingClipboard) > 0 {
		if frame == nil {
			frame = &flushFrame{clients: clients}
		}
		frame.clipboardPayload = encodeClipboardMsg(0, h.pendingClipboard)
		h.pendingClipboard = nil
	}
	return frame, holdUntil
}

// flushPass runs one scheduler pass: build, then dispatch the frame or sweep
// bare acks (input into a silent app must still trim the client outbox).
// Returns the DEC 2026 hold deadline when the pass was suppressed by a hold.
func (h *Handler) flushPass() (holdUntil time.Time) {
	frame, holdUntil := h.buildFrame()
	if frame != nil {
		h.dispatchFrame(frame)
		return holdUntil
	}
	h.sweepAcks()
	return holdUntil
}

// flushLoop is the event-driven coalescing flush scheduler (P4; it replaced
// the permanent 50 ms ticker). Semantics:
//
//   - IDLE: with nothing dirty, the loop sleeps on the dirty channel — zero
//     wakeups, zero renders, no matter how many idle sessions a server holds.
//   - FIRST output after idle flushes IMMEDIATELY: first-byte echo latency is
//     the connect RTT, not a tick-alignment lottery (the old ticker added
//     0-50 ms to every isolated keystroke echo).
//   - SUSTAINED output batches exactly like the ticker did: after each pass
//     the loop absorbs pokes for one flushInterval before flushing again, so
//     consecutive frames stay >= flushInterval apart.
//   - A DEC 2026 hold (synchronized output) arms a retry at the release
//     deadline: the final held redraw flushes even when no PTY byte follows
//     the hold (the deadline case a trigger-only scheduler would strand).
//     A new poke during the hold sleep re-passes early, which re-reads the
//     (possibly extended) deadline; passes stay suppressed while held.
//
// Dirty sources: PTY output (handlePTYData), resize, resume/attach, and
// reliable input needing an ack sweep (deliverInput). Zero-client suspension
// lives in buildFrame: a poke with nobody attached drains scrollback into the
// ring and skips all render/diff work.
func (h *Handler) flushLoop(ctx context.Context) {
	for h.waitForDirty(ctx) {
		if !h.flushBurst(ctx) {
			return
		}
	}
}

// flushBurst passes immediately, then keeps passing while work arrives with
// passes spaced one flushInterval apart. A full quiet window ends the burst;
// context cancellation ends the scheduler.
func (h *Handler) flushBurst(ctx context.Context) bool {
	for {
		holdUntil := h.flushPass()
		if !holdUntil.IsZero() {
			if !h.waitForHoldRetry(ctx, holdUntil) {
				return false
			}
			continue
		}
		gotMore, alive := h.waitForBatchWindow(ctx)
		if !alive {
			return false
		}
		if !gotMore {
			return true
		}
	}
}

// waitForDirty blocks without a timer while the scheduler is idle.
func (h *Handler) waitForDirty(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-h.dirty:
		return true
	}
}

// waitForHoldRetry waits until the current synchronized-output hold expires,
// or returns early on another dirty poke so the live deadline can be re-read.
func (h *Handler) waitForHoldRetry(ctx context.Context, holdUntil time.Time) bool {
	timer := time.NewTimer(max(time.Until(holdUntil), 0))
	select {
	case <-ctx.Done():
		timer.Stop()
		return false
	case <-h.dirty:
		timer.Stop()
		return true
	case <-timer.C:
		return true
	}
}

// waitForBatchWindow preserves the ticker-era sustained-output cadence. A
// dirty poke during the window waits out its remainder; a quiet window returns
// gotMore=false so flushLoop can go back to its timer-free idle wait.
func (h *Handler) waitForBatchWindow(ctx context.Context) (gotMore, alive bool) {
	timer := time.NewTimer(flushInterval)
	select {
	case <-ctx.Done():
		timer.Stop()
		return false, false
	case <-h.dirty:
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, false
		case <-timer.C:
			return true, true
		}
	case <-timer.C:
		// A poke racing the timer edge stays buffered in h.dirty and
		// re-enters through waitForDirty immediately.
		return false, true
	}
}

// sweepAcks sends a bare ackOnly frame to every client whose session ledger
// advanced without a content frame carrying the new value this pass — input
// into a silent app (no echo, no output; e.g. `read -s`) would otherwise stay
// unacked indefinitely, leaving the client outbox untrimmed and widening the
// window where a later ledger loss drops (previously) or duplicated input.
// Called from flushLoop only on passes that produced no frame; passes WITH a
// frame stamp the fresh ack on every payload via withClientAck instead.
func (h *Handler) sweepAcks() {
	targets := h.registry.AckSweepTargets()
	if len(targets) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for ws, ack := range targets {
		ws.Write(ctx, websocket.MessageBinary, encodeAckOnly(ack)) //nolint:errcheck // best-effort
	}
}

// dispatchFrame encodes a frame's payloads once and fans them out to
// every connected client as binary WebSocket frames. It is called from
// flushLoop with h.mu NOT held — a slow client only blocks itself, not
// readLoop / handleControl / new handleWS connections. Extracted from
// flushLoop so that select-loop stays small and readable.
func (h *Handler) dispatchFrame(frame *flushFrame) {
	if len(frame.changed) > 0 || len(frame.scrollLines) > 0 {
		h.cfg.logger.Debug("terminal: flush",
			"changed", len(frame.changed),
			"scroll_lines", len(frame.scrollLines),
			"clients", len(frame.clients))
	}

	// Pre-encode payloads once; identical bytes for every client.
	var screenPayload []byte
	if len(frame.changed) > 0 {
		screenPayload = encodeScreenMsg(frame.base, frame.screenHeight, frame.curRow, frame.curCol,
			0, frame.changed, frame.rows, frame.cursorStyle, frame.cursorHidden, frame.cursorBlink, frame.bell, frame.altActive, frame.scrollbackCleared)
	}
	// Split a large drained burst across several frames so num_lines never overflows the
	// uint16 count and no single frame reaches multiple MB. Each chunk keeps its absolute
	// firstIndex, so the client applies every line at the right index (idempotent), exactly
	// as handleResume's chunked replay does.
	var scrollPayloads [][]byte
	for i := 0; i < len(frame.scrollLines); i += maxScrollLinesPerFrame {
		end := min(i+maxScrollLinesPerFrame, len(frame.scrollLines))
		scrollPayloads = append(scrollPayloads,
			encodeScrollMsg(0, frame.scrollFirstIdx+uint64(i), frame.scrollLines[i:end]))
	}

	// Assemble the per-client write sequence once, preserving the send order
	// (modes, title, clipboard, screen, scroll chunks).
	payloads := make([][]byte, 0, 4+len(scrollPayloads))
	for _, p := range [][]byte{frame.modesPayload, frame.titlePayload, frame.clipboardPayload, screenPayload} {
		if p != nil {
			payloads = append(payloads, p)
		}
	}
	payloads = append(payloads, scrollPayloads...)
	if len(payloads) == 0 {
		return
	}
	// Fan out concurrently: one goroutine per client, each writing ITS frames
	// in order. Serial fan-out let one wedged client stall every other
	// client's output for up to 5s × payload count (judgement finding); now a
	// wedged client costs only itself, and the tick blocks at most one 5s
	// window total. Per-connection write serialization is coder/websocket's
	// (concurrent writers to one conn are internally locked — handleResume /
	// sweepAcks already overlap with this loop today); withClientAck clones
	// the shared template per call, so goroutines never share a buffer.
	var wg sync.WaitGroup
	for ws, ack := range frame.clients {
		wg.Add(1)
		go func(ws *websocket.Conn, ack uint64) {
			defer wg.Done()
			writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, p := range payloads {
				ws.Write(writeCtx, websocket.MessageBinary, withClientAck(p, ack)) //nolint:errcheck // best-effort
			}
		}(ws, ack)
	}
	wg.Wait()
	// Record what each client was just told, so the next no-frame tick's ack
	// sweep doesn't resend a value a content frame already carried.
	h.registry.NoteAcksSent(frame.clients)
}

// controlMsg is a JSON control message from the client.
type controlMsg struct {
	// HaveThrough is the highest absolute line index the client already
	// holds in its store. Sent in resume control messages so the server
	// replays exactly the lines the client is missing (indices greater
	// than HaveThrough), aligned by absolute identity rather than by a
	// fragile count. -1 means the client holds nothing (cold load / DOM
	// eviction) and wants the full retained history. The server clamps
	// the replay start into the retained range and reports any eviction
	// gap via the resumeAck bounds.
	HaveThrough *int64 `json:"haveThrough"`
	Type        string `json:"type"`
	SessionID   string `json:"sessionId,omitempty"`
	SentBytes   uint64 `json:"sentBytes,omitempty"`
	Cols        int    `json:"cols,omitempty"`
	Rows        int    `json:"rows,omitempty"`
	// ProtocolVersion is the client's wire-protocol revision (resume only).
	// 0 means version-silent legacy client and remains tolerated. A declared
	// revision below MinSupportedClientWireVersion is refused; a higher-than-
	// current revision warns but continues because it may retain this server's
	// compatible baseline.
	ProtocolVersion int `json:"protocolVersion,omitempty"`
}

// handleWS upgrades to WebSocket, spawns the configured command in a
// PTY, and bridges bytes both ways until either side closes.
func (h *Handler) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, h.cfg.acceptOptions)
	if err != nil {
		h.cfg.logger.Warn("terminal: ws accept", "error", err)
		return
	}
	ws.SetReadLimit(wsReadLimit)

	// Note: the child process is preferably started on the first resize message so it
	// boots at the correct dimensions. As a fallback we still call ensureStarted
	// here in case the client never sends a resize (e.g. tests).

	// Register this client.
	state := h.registry.Add(ws)
	// Force the next flush to send all rows so this client sees the
	// current screen, even if no resize is sent.
	h.mu.Lock()
	h.builder.Reset()
	h.mu.Unlock()
	// Wake the scheduler for the attach itself: a client that never speaks
	// resume or resize (a bare reader) must still receive the current screen
	// — under the old ticker the next tick delivered it; the event-driven
	// loop needs the poke (this also ends any zero-client suspension).
	h.markDirty()

	defer func() {
		dCols, dRows := h.registry.Remove(ws)
		h.maybeHealSize(dCols, dRows)
		ws.Close(h.exitAwareCloseCode(), "") // #nosec G104 -- best-effort
	}()

	// Cancellable context tied to the client's request — pingLoop
	// will cancel it if the WS becomes unresponsive (Jacobson/Karels
	// RTO timeout). The read loop below exits when ctx is canceled
	// because ws.Read() honors ctx cancellation.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go pingLoop(ctx, cancel, ws, h.cfg.logger)

	// Close promptly with 4001 when the child process exits, so the client can
	// distinguish "process ended" from a transient drop (see closeOnProcExit
	// and exitAwareCloseCode). resumeServed defers that close on an
	// attach-to-already-exited session until the first resume exchange has been
	// written, so the client renders the final screen before the 4001.
	resumeServed := make(chan struct{})
	var resumeOnce sync.Once
	markResumeServed := func() { resumeOnce.Do(func() { close(resumeServed) }) }
	go h.closeOnProcExit(ctx, ws, resumeServed)

	// Read input from this client and write to the shared PTY.
	h.clientReadLoop(ctx, ws, state, markResumeServed)
}

// clientReadLoop pumps one client's messages until the socket dies or a
// protocol violation closes it.
//
// v4 typed-framing state (docs/wire-v4-typed-framing.md §4): a binary
// bootstrap resume declaring protocolVersion >= 4 ARMS the connection; the
// first valid recognized TEXT control on an armed connection LATCHES typed
// mode (text = control, binary = full-alphabet PTY input). Until the latch,
// binary frames keep exact v3 semantics — the 0x00 sentinel plus the
// parse-fallback — so v3 and version-silent clients ride that path forever.
func (h *Handler) clientReadLoop(ctx context.Context, ws *websocket.Conn, state *clientState, markResumeServed func()) {
	var armed, latched bool
	for {
		typ, msg, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var closed bool
			armed, latched, closed = h.handleTextControl(ws, state, msg, armed, latched, markResumeServed)
			if closed {
				return
			}
			continue
		}
		if len(msg) == 0 {
			continue // ignored without latching (documented in the design)
		}
		var ok bool
		armed, ok = h.handleBinaryFrame(ws, state, msg, armed, latched, markResumeServed)
		if !ok {
			return
		}
	}
}

// handleBinaryFrame routes one binary message: pre-latch, a 0x00-leading
// frame is tried as a v3 sentinel control (with the parse-fallback delivering
// non-JSON frames to the PTY whole, leading NUL included, so no input byte is
// ever silently swallowed and acks stay on frame boundaries); post-latch, and
// for all non-sentinel frames, the bytes are PTY input. Returns the updated
// armed state and ok=false when the connection must end (PTY start/write
// failure).
func (h *Handler) handleBinaryFrame(ws *websocket.Conn, state *clientState, msg []byte, armed, latched bool, onResumeServed func()) (newArmed, ok bool) {
	if !latched && msg[0] == 0x00 {
		if d := h.handleControl(ws, state, msg[1:], onResumeServed); d.parsed {
			return armed || d.armsV4, !d.closed
		}
		// Parse-fallback: fall through and deliver the WHOLE frame as input.
	}
	// Ensure process is started (fallback if no resize was sent).
	// h.started is atomic.Bool so the fast-path read does not race
	// with ensureStarted's write. cols/rows of 0 select defaults.
	if !h.started.Load() {
		if err := h.ensureStarted(0, 0); err != nil {
			h.cfg.logger.Error("terminal: process start failed", "error", err)
			return armed, false
		}
	}
	if _, err := h.ptmx.Write(msg); err != nil {
		h.cfg.logger.Debug("terminal: pty write", "error", err)
		return armed, false
	}
	// Derive the session title from the same bytes the program received (after
	// the write, so input that never reached it never names the session). One
	// binary frame is one atomic input event, which is the chunk boundary the
	// deriver's escape parser relies on.
	h.observeInputTitle(msg)
	// Increment session bytesReceived for the resume protocol.
	// state.session is set when the client sends its first resume
	// control message; without it we silently skip — the client is
	// either not using the protocol or hasn't initialized yet.
	h.registry.IncrementReceived(state, len(msg))
	// Wake the scheduler even though no PTY output may follow (a silent
	// reader, e.g. `read -s`): the pass's ack sweep is what trims the
	// client's outbox for input that produces no echo.
	h.markDirty()
	return armed, true
}

// handleTextControl applies the v4 text-frame policy
// (docs/wire-v4-typed-framing.md §4) to one text message: text is only ever a
// control channel, it can never become PTY input, and anything that is not a
// valid control on an armed connection closes the connection rather than
// risking framing-state poison. Returns the updated (armed, latched) state and
// closed=true when it has closed the connection.
func (h *Handler) handleTextControl(ws *websocket.Conn, state *clientState, msg []byte, armed, latched bool, onResumeServed func()) (newArmed, newLatched, closed bool) {
	if !utf8.Valid(msg) {
		// RFC 6455 §5.6 requires valid UTF-8 in text messages and
		// coder/websocket does not validate it; Go's encoding/json would
		// silently replace invalid sequences, so reject explicitly.
		h.cfg.logger.Warn("terminal: closing on invalid UTF-8 text frame", "bytes", len(msg))
		_ = ws.Close(websocket.StatusInvalidFramePayloadData, "control frames must be valid UTF-8")
		return armed, latched, true
	}
	if len(msg) == 0 || !armed {
		// No v3 peer ever sends text, so pre-arm (or empty) text is a
		// protocol violation; closing is the only response that cannot
		// poison the framing state.
		h.cfg.logger.Warn("terminal: closing on unexpected text frame", "armed", armed, "bytes", len(msg))
		_ = ws.Close(websocket.StatusUnsupportedData, "unexpected text frame")
		return armed, latched, true
	}
	d := h.handleControl(ws, state, msg, onResumeServed)
	switch {
	case d.closed:
		return armed, latched, true
	case !d.parsed:
		h.cfg.logger.Warn("terminal: closing on unparseable text control", "bytes", len(msg))
		_ = ws.Close(websocket.StatusUnsupportedData, "unparseable control frame")
		return armed, latched, true
	case d.known:
		latched = true // the transition (and every later recognized text control) latches
		if d.armsV4 {
			armed = true // a text resume keeps the arm current (idempotent)
		}
	case !latched:
		// Valid JSON but an unrecognized type before any latch: refuse
		// rather than guess (post-latch, unknown types are tolerated and
		// dropped, matching the binary path's long-standing behavior).
		h.cfg.logger.Warn("terminal: closing on unrecognized text control before upgrade")
		_ = ws.Close(websocket.StatusUnsupportedData, "unrecognized control before upgrade")
		return armed, latched, true
	}
	return armed, latched, false
}

// controlDisposition reports how handleControl classified one control payload,
// so the read loop can drive the v4 framing state machine (arm/latch) and the
// v3 parse-fallback without re-parsing the JSON.
type controlDisposition struct {
	parsed bool // payload was valid control JSON
	known  bool // c.Type was a recognized control type
	armsV4 bool // a resume declaring protocolVersion >= typedFramingMinVersion
	closed bool // compatibility enforcement closed the connection
}

// handleControl dispatches one client control message (binary sentinel payload
// or whole text message — the transport is the caller's concern). onResumeServed
// is invoked after a resume exchange has been fully written to the socket
// (resumeAck + modes/title + window frame + history replay); handleWS uses it to
// release the deferred process-exited close for a client that attached to an
// already-exited session (see closeOnProcExit).
func (h *Handler) handleControl(ws *websocket.Conn, state *clientState, payload []byte, onResumeServed func()) controlDisposition {
	var c controlMsg
	if err := json.Unmarshal(payload, &c); err != nil {
		h.cfg.logger.Debug("terminal: bad control frame", "error", err, "bytes", len(payload))
		return controlDisposition{}
	}
	d := controlDisposition{parsed: true}
	switch c.Type {
	case ctlTypeResume:
		d.known = true
		if c.ProtocolVersion != 0 && c.ProtocolVersion < minSupportedClientWireVersion {
			h.cfg.logger.Warn("terminal: refusing client below wire-protocol compatibility floor",
				"client", c.ProtocolVersion, "server", wireProtocolVersion,
				"min_supported", minSupportedClientWireVersion,
				"hint", "reload or upgrade the client")
			_ = ws.Close(WireIncompatibleCloseCode, wireIncompatibleClientReason)
			d.closed = true
			return d
		}
		d.armsV4 = c.ProtocolVersion >= typedFramingMinVersion
		// A higher revision may retain this server's compatible baseline, so it
		// warns but is not refused. Version-silent clients remain tolerated.
		if c.ProtocolVersion > wireProtocolVersion {
			h.cfg.logger.Warn("terminal: client wire-protocol version is newer than server",
				"client", c.ProtocolVersion, "server", wireProtocolVersion,
				"min_supported", minSupportedClientWireVersion,
				"hint", "upgrade the server if terminal behavior is incorrect")
		}
		if c.SessionID != "" {
			// A nil (omitted) haveThrough means the client holds nothing and
			// wants full history (-1), not "have line 0" (which would drop
			// index 0).
			ht := int64(-1)
			if c.HaveThrough != nil {
				ht = *c.HaveThrough
			}
			h.handleResume(ws, state, c.SessionID, ht, c.SentBytes)
			if onResumeServed != nil {
				onResumeServed()
			}
		}
	case ctlTypeResize:
		d.known = true
		h.handleResize(state, c.Cols, c.Rows)
	case ctlTypePing:
		d.known = true
		h.handlePing(ws)
	case ctlTypeUpgrade:
		// The v4 transition control: recognizing it is what latches typed
		// framing in the read loop; nothing else to do.
		d.known = true
	default:
		h.cfg.logger.Debug("terminal: unrecognized control type", "type", c.Type)
	}
	return d
}

// handlePing answers a client liveness probe with a pong. The client
// sends a ping only after a stretch of inbound silence to tell apart an
// idle-but-healthy socket from one iOS froze during sleep; the pong (or
// any other frame) clears its probe. Best-effort: a write failure means
// the socket is already gone, which the client's probe timeout will catch.
func (h *Handler) handlePing(ws *websocket.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws.Write(ctx, websocket.MessageBinary, encodePongMsg()) //nolint:errcheck // best-effort liveness reply
}

// handleResume looks up or creates the session for sessionID, attaches
// it to state, replies with a resumeAck carrying the server's current
// bytesReceived count plus the absolute-index bounds of retained
// history, sends a full-repaint frame of the current window (carrying
// the live alt-screen state) and replays the lines the client is missing
// (by absolute index), and leaves the next flush to repaint the window
// idempotently.
//
// The order of the window frame and the replay depends on the live alt
// state, because the window frame is what sets the client's alt flag and
// the client drops scroll frames while that flag is set (store.ts
// applyScroll):
//   - main screen (winAlt == false): the window frame precedes the replay
//     so a client with a stale alt flag (disconnected in alt, app left alt
//     while away) leaves alt before the replayed history lands; otherwise
//     it silently drops those frames (finding l-f38).
//   - alt screen (winAlt == true): the replay precedes the window frame so
//     a client not yet in alt (fresh load / second tab on an in-alt
//     session) stores the main-screen history before the window frame
//     flips it into alt; otherwise that history is lost (the h-f1
//     regression).
//
// haveThrough is the highest absolute line index the client already
// holds (-1 = none). The server replays lines with index > haveThrough,
// clamped into the retained range; the resumeAck's oldestIndex lets the
// client detect an eviction gap when its haveThrough is older than what
// the ring still holds.
//
// sentBytes is the client's claimed total of reliable input bytes sent this
// session. When the resume key misses the registry (idle GC or cap eviction
// reclaimed the ledger) while sentBytes > 0, the client believed it had a
// ledger the server no longer holds — the server cannot vouch for any of
// that input (it cannot distinguish forgotten-after-applying from
// lost-having-applied-nothing), so the resumeAck carries an explicit
// ledger-lost flag and the client drops-and-notifies deterministically
// instead of guessing from an ambiguous received=0.
func (h *Handler) handleResume(ws *websocket.Conn, state *clientState, sessionID string, haveThrough int64, sentBytes uint64) {
	ack, created := h.registry.ResolveSession(state, sessionID)
	ledgerLost := created && sentBytes > 0
	if ledgerLost {
		// The client half of the event gcIdleSessions logged server-side;
		// together the two lines make a forgotten-ledger incident correlatable
		// end to end.
		h.cfg.logger.Info("terminal: resume key missed with claimed sent bytes; signaling ledger loss",
			"session_id", LogID(sessionID), "sent_bytes", sentBytes)
	}

	h.mu.Lock()
	// Force a full repaint on the next flush so the resuming client sees
	// the current window rebuilt from scratch rather than diffed against
	// a previous-window cache it never received.
	h.builder.Reset()
	// Commit any pending drain to history at its absolute index before
	// computing the replay, so lines that scrolled while the client was
	// away are retained (the old code discarded them here).
	drained := h.screen.DrainScrollback()
	// Match Build's guard: drain that straddles an alt-screen transition belongs to the
	// buffer just left and must not enter main history.
	if !h.screen.InAltScreen && !h.builder.altTransitionPending(h.screen) && len(drained) > 0 {
		h.scrollback.Append(drained)
	}
	committed := h.scrollback.Committed()
	oldest := h.scrollback.OldestIndex()
	var from uint64
	if haveThrough >= 0 {
		from = uint64(haveThrough) + 1
	}
	firstAbs, replay := h.scrollback.LinesFrom(from)
	// Snapshot the current window under h.mu so it can be encoded into a
	// full-repaint screen frame and sent relative to the replay (below; the
	// order depends on winAlt). The base equals committed in all cases: on the
	// main screen the window sits just past committed history; in alt the base
	// is frozen there too.
	winBase := committed
	winRows := make([][]vt.WireRun, h.screen.Height)
	for y := range h.screen.Height {
		winRows[y] = h.screen.RenderRowWire(y)
	}
	winCurRow, winCurCol := h.screen.CursorPos()
	winHeight := h.screen.Height
	winAlt := h.screen.InAltScreen
	winCursorStyle := h.screen.CursorStyle
	winCursorHidden := h.screen.CursorHidden
	winCursorBlink := h.screen.CursorBlink
	// Snapshot and encode the current DEC private modes and title under h.mu so
	// the resuming client's input encoding (app-cursor arrows, SGR mouse, etc.)
	// is correct immediately, rather than defaulting until the next diff-driven
	// flush (<= flushInterval) re-announces them via builder.Reset. Encode
	// directly from screen state — do NOT use builder.buildModesPayload, which
	// mutates the per-Handler builder's shared announce-state and would starve a
	// concurrently connecting second client of its own modes frame.
	modesPayload := encodeModesMsg(h.screen.BracketedPaste, h.screen.AppCursorKeys,
		h.screen.MouseSGR, h.screen.FocusReporting, h.screen.AppKeypad,
		h.screen.ReverseVideo, h.screen.MousePixels, h.screen.MouseMode, h.screen.KeyboardFlags())
	titlePayload := encodeTitleMsg(h.screen.Title)
	h.mu.Unlock()

	// Build the full-repaint changed list (every window row) and encode the
	// window frame outside the lock.
	winChanged := make([]int, winHeight)
	for i := range winChanged {
		winChanged[i] = i
	}
	windowPayload := encodeScreenMsg(winBase, winHeight, winCurRow, winCurCol, ack,
		winChanged, winRows, winCursorStyle, winCursorHidden, winCursorBlink, false, winAlt, false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// resumeAck first so the client can trim its outbox and learn the
	// history bounds (for gap detection) before the replay lands.
	ws.Write(ctx, websocket.MessageBinary, encodeResumeAck(ack, h.bootEpoch, committed, oldest, ledgerLost)) //nolint:errcheck // best-effort
	state.lastAckSent.Store(ack)

	// Resend current modes/title inline (before the window/replay) so input
	// encoding is correct before the user can type; a fresh tab starts at
	// default modes and would otherwise misencode arrows/mouse for one flush.
	ws.Write(ctx, websocket.MessageBinary, withClientAck(modesPayload, ack)) //nolint:errcheck // best-effort
	if titlePayload != nil {
		ws.Write(ctx, websocket.MessageBinary, withClientAck(titlePayload, ack)) //nolint:errcheck // best-effort
	}

	// replayHistory sends the missing committed lines in chunks, each tagged
	// with its absolute first index so the client applies them idempotently.
	replayHistory := func() {
		const replayChunk = 50
		for i := 0; i < len(replay); i += replayChunk {
			end := min(i+replayChunk, len(replay))
			payload := encodeScrollMsg(ack, firstAbs+uint64(i), replay[i:end])
			ws.Write(ctx, websocket.MessageBinary, payload) //nolint:errcheck // best-effort
		}
	}

	// The client gates scroll application on its alt flag (store.ts applyScroll),
	// and the window frame is what sets that flag to winAlt:
	//   - winAlt == false: window FIRST so a client with a stale alt flag
	//     (disconnected in alt, app left alt while away) leaves alt before the
	//     replay lands (finding l-f38).
	//   - winAlt == true: replay FIRST so a client not yet in alt (fresh load /
	//     second tab on an in-alt session) stores the main-screen history before
	//     the window frame puts it into alt; otherwise the replay is dropped and
	//     that history is lost until the next non-alt reconnect.
	if winAlt {
		replayHistory()
		ws.Write(ctx, websocket.MessageBinary, windowPayload) //nolint:errcheck // best-effort
	} else {
		ws.Write(ctx, websocket.MessageBinary, windowPayload) //nolint:errcheck // best-effort
		replayHistory()
	}

	// A fresh attach ends any zero-client suspension: poke the scheduler so
	// the diff-driven flush (against the Reset builder above) repaints the
	// window idempotently on the first pass.
	h.markDirty()
}

// clampResize floors the requested dimensions to a sane minimum and caps them
// at the eager-allocation ceiling. Floored (rather than dropped) so a near-zero
// reading from an iPad keyboard-slide animation still drives ensureStarted on
// first connect — dropping the resize would leave the process unstarted until
// the client sent raw input.
func clampResize(cols, rows int) (clampedCols, clampedRows int) {
	clampedCols = min(max(cols, minResizeCols), maxResizeCols)
	clampedRows = min(max(rows, minResizeRows), maxResizeRows)
	return clampedCols, clampedRows
}

// handleResize applies a client's requested size to the shared PTY + screen
// (last-writer-wins: the most recent resize from any attached client sets the
// size) and records the clamped size on that client's socket, so a later
// disconnect can heal the screen to the smallest size the remaining clients
// need (see maybeHealSize).
func (h *Handler) handleResize(state *clientState, cols, rows int) {
	cols, rows = clampResize(cols, rows)
	// Start the child process on first resize so it knows the correct dimensions
	// from the start (avoids initial paint at wrong size).
	if !h.started.Load() {
		if err := h.ensureStarted(cols, rows); err != nil {
			h.cfg.logger.Error("terminal: process start failed", "error", err)
			return
		}
	}
	h.registry.RecordSize(state, cols, rows)
	h.applySize(cols, rows, "resize received")
}

// armRedrawSettle starts the redraw-settle hold: flushes stay suppressed until
// the child's redraw output has been quiet for redrawSettleQuiet, capped at
// redrawSettleCap from now. Caller holds h.mu.
//
// This exists because the screen-level flush hold (vt.Screen.HoldFlush) cannot
// hide a SIGWINCH redraw: it shares its deadline with DEC 2026, and the
// child's first CSI ?2026l (ESU) clears it. kiro-cli brackets its post-resize
// transcript reprint in many small BSU/ESU chunks, so the first chunk released
// the resize hold milliseconds in, and every subsequent flush pass streamed a
// mid-reprint window to the clients — on a phone, seconds of history visibly
// churning through the screen after each keyboard/rotation resize. The settle
// hold is handler-level state the child's escape sequences cannot touch: the
// redraw is over when the child goes quiet, not when it closes a bracket.
func (h *Handler) armRedrawSettle(now time.Time) {
	h.redrawSettleUntil = now.Add(redrawSettleCap)
	// The arm itself counts as activity so the hold lasts at least
	// redrawSettleQuiet even if the child's first redraw byte is still in
	// flight (SIGWINCH delivery latency); flushing before it arrives would
	// show the pre-redraw reflowed screen — the state the hold exists to hide.
	h.redrawLastData = now
}

// redrawHoldUntil returns the moment the redraw-settle hold lapses, or the
// zero time when it is inactive, settled (quiet long enough), or capped.
// Lapsing disarms the hold. Caller holds h.mu.
func (h *Handler) redrawHoldUntil(now time.Time) time.Time {
	if h.redrawSettleUntil.IsZero() {
		return time.Time{}
	}
	deadline := h.redrawLastData.Add(redrawSettleQuiet)
	if h.redrawSettleUntil.Before(deadline) {
		deadline = h.redrawSettleUntil
	}
	if !now.Before(deadline) {
		h.redrawSettleUntil = time.Time{}
		return time.Time{}
	}
	return deadline
}

// applySize resizes the PTY and the shared VT screen and, when the dimensions
// actually change, holds flushes over the SIGWINCH redraw window so clients
// don't see the child's transient cleared-screen / mid-reprint states. The
// hold releases when the redraw output settles (see armRedrawSettle). A
// same-size call (a client reconnect re-sending its size) arms nothing: the
// kernel suppresses SIGWINCH for an unchanged winsize, so there is no redraw
// to hide and live output must not stall. Shared by the live resize path and
// the disconnect heal.
func (h *Handler) applySize(cols, rows int, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.started.Load() || h.ptmx == nil {
		return
	}
	// Clamp again so applySize is safe regardless of caller (the heal path
	// passes MinLiveSize values). Idempotent for the live path, which already
	// clamped in handleResize.
	cols, rows = clampResize(cols, rows)
	sizeChanged := cols != h.screen.Width || rows != h.screen.Height
	if err := pty.Setsize(h.ptmx, &pty.Winsize{
		// #nosec G115 -- clampResize bounds cols/rows to [minResize, maxResize<=1000], >0, just above; no uint16 overflow. gosec can't see through the helper.
		Cols: uint16(cols), Rows: uint16(rows),
	}); err != nil {
		h.cfg.logger.Debug("terminal: resize", "error", err)
	}
	h.screen.Resize(rows, cols)
	if sizeChanged {
		h.armRedrawSettle(time.Now())
	}
	h.cfg.logger.Info("terminal: "+reason, "rows", rows, "cols", cols)
	h.sizeEstablished = true
	h.builder.Reset()
	// The resize hold suppresses passes until the app's redraw settles; the
	// poke makes the scheduler arm the release deadline so the repaint
	// flushes even if the app writes nothing after the hold window.
	h.markDirty()
}

// maybeHealSize arms a debounced size recompute when the client that just
// disconnected was the one dictating the current shared screen size (its last
// reported size equals the screen's). Only that case can strand a survivor at a
// departed client's size — e.g. a desktop left clamped to a phone's size after
// the phone closes its tab. Any other departure is skipped: some other client,
// or a live resize, still holds the current size, so there is nothing to relax.
// Debounced via healDebounce so a brief reconnect at the same size is a no-op
// rather than a grow-then-shrink flap.
func (h *Handler) maybeHealSize(dCols, dRows int) {
	if dCols <= 0 || dRows <= 0 {
		return // the departed client never reported a size
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.started.Load() || dCols != h.screen.Width || dRows != h.screen.Height {
		return // the departed client was not holding the current size
	}
	if h.healTimer != nil {
		h.healTimer.Stop()
	}
	h.healTimer = time.AfterFunc(healDebounce, h.healSize)
}

// healSize relaxes the shared screen to the smallest size across the clients
// still connected, so a survivor no longer stays clamped to a departed client's
// size. Runs from the debounced healTimer. A no-op when no surviving client has
// a known size, or when the smallest already equals the current screen (e.g.
// the departed client reconnected within the debounce and re-reported the same
// size, so it is counted again).
func (h *Handler) healSize() {
	cols, rows, ok := h.registry.MinLiveSize()
	if !ok {
		return
	}
	h.mu.Lock()
	unchanged := !h.started.Load() || (cols == h.screen.Width && rows == h.screen.Height)
	h.mu.Unlock()
	if unchanged {
		return
	}
	h.applySize(cols, rows, "size healed after client departure")
}
