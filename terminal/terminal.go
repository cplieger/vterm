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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/cplieger/web-terminal-engine/v5/vt"
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
	// child's redraw output has been quiet for redrawSettleQuiet. Quiet must
	// exceed the child's inter-chunk rendering gaps (a program batching its
	// redraw in DEC 2026 brackets writes them milliseconds apart), and it is
	// the NORMAL exit from the hold.
	//
	// redrawSettleCap bounds one held stretch. Lapsing it does NOT fall back to
	// streaming: a redraw larger than the cap is ordinary (a phone-width
	// terminal reprinting a few thousand lines takes seconds), and releasing
	// mid-redraw is what put a partial screen on the wire every flushInterval
	// for the rest of it — the churn this hold exists to prevent. Instead the
	// lapse lets ONE full repaint through and re-arms, up to
	// redrawSettleMaxRearms times, so the client sees a coherent whole screen
	// per cap interval. The re-arm budget is what keeps a program that streams
	// continuously after a resize (`tail -f` on a busy log) from being
	// throttled indefinitely: once spent, the hold disarms for good and output
	// streams normally.
	//
	// The cap also bounds vt.Screen.Drained in the common case, whose only
	// live-path consumer is inside flushFrameBuilder.Build: every lapse builds a
	// frame, so drained lines normally wait at most one cap interval before being
	// committed AND sent. The exception is a DEC 2026 (synchronized output) hold
	// overlapping a lapse, since that gate also skips Build and this hold cannot
	// clear it; there the accumulation lasts as long as the child keeps the 2026
	// hold armed, which is a pre-existing property of that hold rather than of
	// this one. What must NOT be done about it is draining on a held pass: Build
	// is also what turns drained lines into the frame that carries them to
	// clients, so a drain outside Build reaches the retained ring without ever
	// reaching an attached client, which is a silent history hole rather than a
	// saving.
	redrawSettleQuiet     = 150 * time.Millisecond
	redrawSettleCap       = time.Second
	redrawSettleMaxRearms = 3

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

	// The demand-paged-scrollback constants (docs/paged-scrollback.md). The
	// client holds a small resident tail and fetches older history on demand
	// through the `history` control; these bound what one request costs the
	// server and what the pairing is allowed to promise.
	//
	// historyPageSize is the largest number of lines one page reply may carry.
	// It matches maxScrollLinesPerFrame so a page is exactly one standard
	// scroll frame; pageByteBudget bounds it further (a styled page serves
	// fewer lines rather than a bigger frame).
	historyPageSize = maxScrollLinesPerFrame
	// paginationMinRing is the ring depth at or above which the server DECLARES
	// paging (resumeAckFlagHistoryPaging). It is DERIVED from maxReplayLines and
	// must stay derived: the resume replay is bounded unconditionally, so any ring
	// deeper than that bound withholds rows from a fresh attach, and paging is the
	// only way the client can ever ask for them.
	//
	// An independent value put a hole in that reasoning. It was 5000 — the legacy
	// client resident default — on the argument that the bit invites the client to
	// shrink its resident tail, so a ring too shallow to back the flip must not
	// invite it. That argument was sound while a non-paging resume replayed the
	// whole ring, and the unconditional replay bound retired it: at capacity 3000
	// the replay delivered the newest 2000, the bit stayed clear, and 1000 lines
	// still live in the authoritative ring were permanently unreachable — viewer-
	// visible loss, not a delayed fetch. Declaring paging there costs the client
	// the tail flip it can now afford (it fetches what it releases) and nothing
	// else. Exported as MinPagingCapacity.
	paginationMinRing = maxReplayLines + 1
	// maxReplayLines caps the client-requested resume replay bound
	// (controlMsg.ReplayMax). Sized on the resident-tail order, deliberately
	// NOT the ring depth: it keeps the resume batch's byte-time planning-
	// bounded (~200 KB of typical plain output, ~20 KB/s over the batch's 10 s
	// write context) even for a consumer that configured a much larger client
	// cap. The client clamps to the same constant before sending, so the SENT
	// value equals the HONORED value — an identity the client's replay-jump
	// prediction depends on (§4.5).
	maxReplayLines = 2000
	// historyBurst/historyRefill are the per-socket history token bucket: the
	// server-side floor against accidental bursts and unfair socket churn. The
	// client paces itself faster (one token per 2s) so a healthy client stays
	// under this floor with margin rather than by coincidence. This is fairness
	// and burst suppression, not an aggregate abuse bound: the registry admits
	// several sockets per session, exactly as the shipped resume throttle does.
	historyBurst  = 4.0
	historyRefill = 1500 * time.Millisecond
	// historyWriteTimeout bounds one page reply's write. It matches the resume
	// batch's context rather than the 5 s live-dispatch one, because a reply
	// holds the socket's writeMu and dispatchFrame's fan-out waits on it, so
	// the blast radius of a slow write is the session; 10 s is the worst
	// constant already accepted for that class, and is deliberately not
	// extended. The client's data timeout (8 s) fires first by design.
	historyWriteTimeout = 10 * time.Second

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
	// ctlTypeHistory requests a page of retained scrollback by absolute index
	// (demand-paged scrollback, docs/paged-scrollback.md §4.1). Adding a control
	// type is back-compatible in both directions: an older server logs it as
	// unrecognized and returns, which is exactly the "not supported" answer a
	// newer client infers from the resumeAck's missing capability bit.
	ctlTypeHistory = "history"

	// defaultScrollbackCapacity is the number of scrollback lines the server
	// retains for replay and for demand-paged history requests. It is the
	// reachable depth of a session's history: how far back a user can scroll.
	//
	// 100000 is sized from real workloads rather than from another terminal's
	// default. The largest agent session measured on a live box (2026-08, 193
	// sessions on disk) rendered an estimated 53,000-73,000 terminal lines, and
	// the p99 session ~61,000 — so a smaller ceiling truncates exactly the long
	// sessions whose history is worth scrolling, while the median session
	// (~2,500 lines) never reaches it. The cost is paid only as history is
	// actually produced (the ring grows on demand, see scrollbackRing), at
	// roughly 345 bytes per retained line: ~34 MB per session at the ceiling,
	// against the 200-430 MB a kiro-cli process tree already costs.
	//
	// Operators override it per deployment with ScrollbackEnvVar; there is no
	// "unlimited" sentinel, because a sufficiently large number IS unlimited
	// once the ring stopped preallocating. 0 disables scrollback entirely.
	defaultScrollbackCapacity = 100_000
)

// ScrollbackEnvVar is the environment variable consumers read to let an operator
// override the retained-history depth (WithScrollbackCapacity).
//
// The engine owns the NAME so the apps that share this knob cannot drift apart
// — web-terminal-server, web-terminal-kiro and vibekit all embed this handler,
// and a knob spelled three ways is three knobs. The engine deliberately does NOT
// read the variable itself: no library in this fleet reads os.Getenv, because a
// library that reads process state takes configuration out of its caller's
// hands. Consumers read it (envx.IntStrict) and pass WithScrollbackCapacity.
//
// The name carries no component prefix on purpose. It is set by an operator who
// knows the app they run, not the library serving its HTTP, so a WT_ prefix
// leaked an internal name at them and bought no disambiguation. The keys the
// engine INJECTS into a session's child environment are the opposite case and do
// keep the prefix — see reapMarkerEnv.
//
// Values: 0 disables scrollback; 1..MinPagingCapacity-1 is honoured but too
// shallow to declare demand paging, so a consumer should clamp up and say so
// (see MinPagingCapacity); anything larger is retained as asked, and there is no
// upper bound to trip over — the ring allocates only what it fills.
const ScrollbackEnvVar = "SCROLLBACK"

// DefaultScrollbackCapacity is the retained-history depth a handler uses when a
// consumer sets none. Exported so a consumer can REPORT the effective depth at
// startup without hardcoding the number — an operator debugging "my scrollback
// stops early" needs to see it, and a consumer that omits the option cannot
// otherwise name it.
const DefaultScrollbackCapacity = defaultScrollbackCapacity

// MinPagingCapacity is the retained-history depth at or above which the handler
// DECLARES demand-paged scrollback to its client (resumeAck ackFlags bit1).
//
// Exported because it is a cliff, not a preference: at or above it the client
// can fetch any history the bounded resume replay withheld; below it the replay
// carries the whole ring, so there is nothing withheld and nothing to fetch.
// Configuring into the gap that used to exist between this and the replay bound
// lost history outright. Prefer ClampScrollbackCapacity over comparing against
// this yourself.
const MinPagingCapacity = paginationMinRing

// ClampScrollbackCapacity turns an operator-supplied retained-history depth into
// the one to configure, plus a human-readable reason when it had to change it.
//
// The three apps embedding this handler share one env var, so they must share
// one interpretation of its awkward middle: a depth between 1 and
// MinPagingCapacity is honoured by the ring but too shallow to declare paging,
// and the resulting client behavior is the opposite of what the operator asked
// for — the browser stops demand-loading and holds its whole legacy resident
// cap, so asking for less server history spends more phone memory. That is
// clamped UP and explained rather than obeyed quietly.
//
// 0 passes through: disabling scrollback is a coherent request (retain nothing
// beyond the live screen) and it does not have the inverted outcome, because a
// client cannot page against a server with no history at all. There is no upper
// bound: the ring allocates only what it fills, so a deliberately enormous
// number is the supported way to say "never truncate".
//
// The returned reason is empty when the input needed no adjustment.
func ClampScrollbackCapacity(n int) (capacity int, reason string) {
	if n < 0 {
		return 0, fmt.Sprintf("%s=%d is negative; treating it as 0 (scrollback disabled)", ScrollbackEnvVar, n)
	}
	if n > 0 && n < MinPagingCapacity {
		return MinPagingCapacity, fmt.Sprintf(
			"%s=%d is below the %d lines needed to offer demand-paged history, which would make the browser "+
				"hold its whole legacy buffer instead of paging: using %d",
			ScrollbackEnvVar, n, MinPagingCapacity, MinPagingCapacity)
	}
	return n, ""
}

// Option configures optional behavior of the Handler.
type Option func(*handlerConfig)

// handlerConfig holds optional configuration applied via functional options.
type handlerConfig struct {
	logger        *slog.Logger
	originPolicy  *OriginPolicy
	onProcessExit func(error)
	theme         *vt.Theme
	// containment/containmentID/containSample configure per-session process
	// containment (WithContainment, WithContainmentSampleInterval). A nil
	// containment disables it, which is the default.
	containment        *Containment
	containmentID      SessionID
	workDir            string
	commandLogValue    string
	env                []string
	containSample      time.Duration
	scrollbackCapacity int
	// minContrast is the minimum-contrast floor passed to the screen
	// (WithMinimumContrast). Zero, like 1, leaves the floor off. Grouped with the
	// other non-pointer scalars rather than beside theme, so it does not split
	// the struct's leading pointer run.
	minContrast   float64
	keepUnfocused bool
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

// WithScrollbackCapacity sets the number of scrollback lines retained for
// replay to reconnecting clients and for demand-paged history requests — the
// depth a user can scroll back to.
//
// Default is defaultScrollbackCapacity (100000). Negative values are treated as
// 0, which disables scrollback: the live screen still works and absolute indices
// still advance, but nothing survives scrolling off. The buffer grows on demand,
// so a large capacity costs nothing until the session fills it, and there is
// deliberately no "unlimited" sentinel — set a number larger than any session
// will reach.
//
// Below MinPagingCapacity the handler does not declare demand paging (see that
// constant: the failure is counter-intuitive, so prefer clamping up).
// Operators override this via ScrollbackEnvVar; read it in the consumer.
func WithScrollbackCapacity(n int) Option {
	return func(c *handlerConfig) {
		c.scrollbackCapacity = max(n, 0)
	}
}

// WithOriginPolicy widens the browser origins allowed to open this handler's
// WebSocket beyond same-origin. Build the policy with NewOriginPolicy; a nil
// policy (the default) means same-origin only.
//
// Pass the SAME policy to WithManagerOriginPolicy when the handler is served
// through a SessionManager, so a widened origin also reaches the manager's
// unknown-session socket. See OriginPolicy for why this gate exists at all: an
// app-level cross-origin middleware never sees a WebSocket handshake.
//
// It replaced WithAcceptOptions, which handed consumers the dependency's whole
// AcceptOptions struct: of its seven fields exactly one (OriginPatterns) was
// useful here, one (InsecureSkipVerify) was a footgun, and the rest were dead
// (Subprotocols — the engine's client negotiates none) or actively harmful
// (CompressionMode would re-deflate per client the payload dispatchFrame encodes
// once; OnPingReceived/OnPongReceived can break the adaptive-RTO ping loop).
func WithOriginPolicy(p *OriginPolicy) Option {
	return func(c *handlerConfig) { c.originPolicy = p }
}

// WithOnProcessExit registers a callback invoked when the child process exits.
func WithOnProcessExit(fn func(error)) Option {
	return func(c *handlerConfig) { c.onProcessExit = fn }
}

// WithKeepUnfocused makes the server hold the child process in the "unfocused"
// state for DEC 1004 focus reporting when keep is true: whenever the process
// enables focus reporting (CSI ?1004h), the server writes a focus-out (ESC [ O)
// to its PTY, and it never writes a focus-in. A process that gates behavior on
// focus (for example kiro-cli, which only emits its OSC 9 turn/permission
// notifications while it believes it is unfocused) then keeps emitting, so the
// session manager's status classifier can observe those notifications.
//
// keep=false reproduces the no-option default — real focus reporting, which is
// what a generic terminal wants (vim, etc.) — so a consumer can thread its own
// flag instead of conditionally appending the option. Later options win: the
// last WithKeepUnfocused in the option list decides. The browser client is
// expected to emit no focus bytes of its own when the hold is enabled.
func WithKeepUnfocused(keep bool) Option {
	return func(c *handlerConfig) { c.keepUnfocused = keep }
}

// WithTheme sets the default foreground, background, and cursor colors the
// terminal reports to programs via OSC 10/11/12 queries (and restores on
// OSC 110/111/112 reset). Pass the colors your client actually renders so
// color-probing apps — light/dark detection, "reset to default" — see the real
// theme. Defaults to vt.DefaultTheme (a dark scheme). Build colors with vt.RGB.
func WithTheme(t vt.Theme) Option {
	return func(c *handlerConfig) { c.theme = &t }
}

// WithMinimumContrast sets a floor on the WCAG contrast ratio between a run's
// text and its background, clamped to 1..21. A foreground below the floor is
// blended toward white or black until it reaches it; backgrounds and default
// foregrounds are left alone. Off by default (1), matching xterm.js's
// minimumContrastRatio.
//
// Pass 4.5 (the WCAG AA floor for body text, and VS Code's default for its
// integrated terminal) when your client renders on a dark background. A terminal
// program selects a palette SLOT and cannot know what your client resolves it
// to, so this is the only place the mismatch can be corrected. See
// vt.WithMinimumContrast for the full rationale.
func WithMinimumContrast(ratio float64) Option {
	return func(c *handlerConfig) { c.minContrast = ratio }
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
	// resumeLast/resumeTokens are the per-socket resume throttle's token
	// bucket (see resumeControl). Owned by the socket's read loop — one
	// socket's control messages are processed serially — so no lock and no
	// atomics. resumeLast sits up here beside the session pointer because
	// time.Time carries a *Location (fieldalignment).
	resumeLast time.Time
	// historyLast/historyTokens are the per-socket `history` throttle's token
	// bucket (see takeHistoryToken), the same read-loop-owned shape as the
	// resume pair above. time.Time carries a *Location, so it sits up here
	// beside the others (fieldalignment).
	historyLast time.Time
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
	// resumeTokens is the throttle bucket's fill; its timestamp half
	// (resumeLast) lives beside the session pointer above.
	resumeTokens float64
	// historyTokens is the history bucket's fill; its timestamp half
	// (historyLast) lives beside the session pointer above.
	historyTokens float64
	// writeMu serializes the two CONTENT-frame writers to this socket: the
	// flush dispatcher's per-client payload loop (dispatchFrame) and
	// handleResume's snapshot+batch. coder/websocket serializes individual
	// Write calls, but not multi-write SEQUENCES — without this lock a live
	// flush could interleave into the middle of a resume batch, or, worse,
	// a resume batch whose window was snapshotted BEFORE a flush frame could
	// be written AFTER it, regressing the client to an older screen until the
	// next full repaint (the reconnect-during-output race). handleResume
	// holds it across snapshot AND writes, so its window is at least as new
	// as anything already written; the dispatcher acquires it BLOCKING, so a
	// live frame built during a resume batch is DELAYED behind it rather
	// than dropped — its payloads are not regenerable (buildFrame already
	// committed the scroll lines and consumed the one-shots), so a drop
	// would be a permanent scrollback hole for the resuming client. See
	// writeClientPayloads for the cost bound and the stale-vs-durable
	// payload split.
	writeMu sync.Mutex
	// resumeGen counts resume snapshots taken for this socket. Bumped by
	// handleResume inside the same h.mu section as its screen snapshot; a
	// frame whose generation (captured at client-snapshot time) no longer
	// matches was built before the resume's snapshot, so dispatchFrame
	// strips it to its durable payloads: the stale screen/modes/title
	// snapshots are dropped (writing them after the batch would regress the
	// client), while scroll lines and clipboard still go out — the batch
	// does not re-deliver those (its replay starts above the client's
	// haveThrough, which already spans the frame's lines), so dropping them
	// would lose history. See writeClientPayloads.
	resumeGen atomic.Uint64
}

// resumeBurst / resumeRefillEvery parameterize the per-socket resume
// throttle: a full bucket serves resumeBurst back-to-back resumes, then one
// more per resumeRefillEvery. Deliberately generous — the gate exists to
// bound a hostile flood's amplification (each resume is a full write
// transaction), not to meter legitimate churn: a reconnect storm, rapid
// ledger switches and a wire upgrade together stay under the burst, and the
// sustained rate still caps a 1000/s spammer at 30 batches/min.
const (
	resumeBurst       = 10
	resumeRefillEvery = 2 * time.Second
)

// takeResumeToken spends one token from the socket's resume bucket,
// reporting false when it is empty. Called only from the socket's read loop
// (control messages are serialized per socket), so the state needs no lock.
// The bucket starts full: resumeLast's zero value dates the last refill to
// the epoch, so the first call tops it up to the burst.
func (st *clientState) takeResumeToken() bool {
	now := time.Now()
	if !st.resumeLast.IsZero() {
		st.resumeTokens += now.Sub(st.resumeLast).Seconds() / resumeRefillEvery.Seconds()
	} else {
		st.resumeTokens = resumeBurst
	}
	if st.resumeTokens > resumeBurst {
		st.resumeTokens = resumeBurst
	}
	st.resumeLast = now
	if st.resumeTokens < 1 {
		return false
	}
	st.resumeTokens--
	return true
}

// takeHistoryToken spends one token from the socket's history bucket,
// reporting false when it is empty. Same read-loop-owned, lock-free shape as
// takeResumeToken (controls are serialized per socket) and the same
// starts-full convention: historyLast's zero value dates the last refill to
// the epoch, so the first call tops it up to the burst.
//
// Scope, stated precisely (docs/paged-scrollback.md §4.4): this is per-socket
// FAIRNESS and accidental-burst suppression, not an aggregate abuse bound. The
// registry admits several sockets per session with no admission cap, so N
// sockets hold N buckets and reconnect churn renews credits — the same shape
// the shipped resume throttle has, against the same authenticated audience.
// The client paces itself more slowly than this floor refills (one token per
// 2s against 1.5s here), so the slack absorbs clock and latency jitter and a
// healthy client stays under the floor with margin rather than by coincidence.
func (st *clientState) takeHistoryToken() bool {
	now := time.Now()
	if !st.historyLast.IsZero() {
		st.historyTokens += now.Sub(st.historyLast).Seconds() / historyRefill.Seconds()
	} else {
		st.historyTokens = historyBurst
	}
	if st.historyTokens > historyBurst {
		st.historyTokens = historyBurst
	}
	st.historyLast = now
	if st.historyTokens < 1 {
		return false
	}
	st.historyTokens--
	return true
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
	cmd        *exec.Cmd
	screen     *vt.Screen
	registry   *clientRegistry
	builder    *flushFrameBuilder
	scrollback *scrollbackRing
	cancel     context.CancelFunc
	ptmx       *os.File
	// contain is this session's cgroup when containment is enabled, nil
	// otherwise. Read WITHOUT the lock by the process monitor and the cost
	// sampler, which is safe only because both are created by the `go` statements
	// in ensureStarted AFTER this write: goroutine creation is synchronized before
	// the goroutine runs. Holding h.mu at the write is not what makes those reads
	// safe, since neither reader takes it. Any NEW reader outside those two
	// goroutines needs h.mu or an atomic, including anything added to Close.
	// (It is also reassigned to nil on the start-failure path below.)
	contain *sessionCgroup
	// reap is this session's marker reap domain, nil when the consumer opted
	// out. Read under exactly the same rules as contain above: written here
	// before the monitor goroutine is created, read only by that goroutine.
	reap       *sessionReap
	procExitCh chan struct{}
	// done closes once EVERY goroutine this handler started has returned: the PTY
	// reader, the flush scheduler, the cost sampler, and the process monitor with
	// all of its teardown (child reaped, containment ended, clients notified, the
	// marker domain swept). Nil until ensureStarted runs, which is what makes
	// wait() a no-op on a handler that never spawned anything.
	//
	// It is derived from h.wg rather than closed by hand, so it carries ONE
	// meaning. Closing it as the monitor's own last act would have reported
	// "teardown finished" while the reader and flush loops were still unwinding,
	// which is a different claim and the one a leak test does not want. Guarded
	// by h.mu.
	done chan struct{}
	// dirty is the flush scheduler's wakeup: 1-buffered so any number of
	// markDirty pokes coalesce into one pending signal. flushLoop sleeps on
	// it when idle — no ticker, no periodic wakeups (P4).
	dirty     chan struct{}
	healTimer *time.Timer
	// redrawSettleUntil / redrawLastData implement the redraw-settle hold
	// (armRedrawSettle / redrawHoldUntil). Both guarded by h.mu; a zero
	// redrawSettleUntil means the hold is inactive. The hold's two scalar
	// companions (redrawRearms, redrawCoalesceNow) sit with the other
	// non-pointer fields at the end of the struct, for field alignment.
	redrawSettleUntil time.Time
	redrawLastData    time.Time
	pendingClipboard  []byte
	command           []string
	// exitErr retains what cmd.Wait() reported when the process monitor reaped
	// the child: nil for a clean exit, an *exec.ExitError for a non-zero or
	// signalled one. Guarded by h.mu and written exactly once, BEFORE
	// procExitCh closes, so Exited() == true implies this value is final (see
	// the monitor in ensureStarted). Read through ExitError.
	exitErr      error
	cfg          handlerConfig
	bootEpoch    int64
	lastActivity atomic.Int64
	// wg counts every goroutine ensureStarted launches, so the handler can answer
	// "is anything of mine still running" instead of leaving the caller to assume
	// it from the process having exited. Added to only from ensureStarted, which
	// holds h.mu, and never after done is armed.
	wg      sync.WaitGroup
	mu      sync.Mutex
	started atomic.Bool
	// shutdownRequested latches true when the SERVER asked this session to end
	// (Close, which is also what SessionManager.Close, the idle reaper and
	// Shutdown reach it through). It is the input that keeps a server-initiated
	// teardown out of the crashed classification — see crashedExit. Atomic, not
	// h.mu-guarded: the process monitor reads it after cmd.Wait() returns, and
	// Close holds h.mu while it stores.
	shutdownRequested atomic.Bool
	// redrawRearms counts how many times the redraw-settle cap has lapsed
	// mid-redraw and coalesced instead of releasing, bounding the coalescing at
	// redrawSettleMaxRearms. Guarded by h.mu. Placed ahead of the bool block so
	// the bools stay contiguous (govet fieldalignment).
	redrawRearms int
	// sizeEstablished is latched true once the PTY has real dimensions (the
	// eager start's default size, or a client resize) and never cleared: the
	// flush builder emits nothing before it, so clients never see a frame
	// rendered against the zero-size screen. It does NOT mean "a resize
	// happened this tick" (its former name, `resized`, invited that reading).
	sizeEstablished          bool
	scrollbackClearedPending bool
	paletteChangedPending    bool
	lastFocusReporting       bool
	// redrawCoalesceNow marks the one flush pass that must build a FULL repaint
	// because the redraw-settle cap lapsed while the child was still writing
	// (see redrawHoldUntil). Consumed by buildFrame. Guarded by h.mu.
	redrawCoalesceNow bool
	// autoTitleWarned makes the automatic-title probe's failure note once-per-
	// session rather than once-per-sweep (see probeAutoTitle). Guarded by h.mu.
	autoTitleWarned bool
}

// NewHandler returns a terminal handler. command is the argv to spawn
// (required, must be non-empty). Optional behavior is configured via
// functional Option values.
func NewHandler(command []string, opts ...Option) *Handler {
	cfg := handlerConfig{
		scrollbackCapacity: defaultScrollbackCapacity,
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
	if cfg.minContrast > vt.MinimumContrastOff {
		vtOpts = append(vtOpts, vt.WithMinimumContrast(cfg.minContrast))
	}
	return &Handler{
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

// Close ends the session and returns immediately, without waiting for the
// teardown it starts. Safe to call even if the process was never started, and
// safe to call more than once.
//
// It kills the child (SIGKILL, via the cancelled context) and closes the PTY.
// Everything after that runs on the process monitor's goroutine and outlives
// this call: reaping the child, ending the containment cgroup, sweeping the
// marker domain for escapees, and telling attached clients the process is gone.
// Use Shutdown when the caller must know that work finished.
//
// This is the form for a caller that must not block: SessionManager.Close serves
// an HTTP DELETE, and the idle reaper runs on a ticker. Neither can afford the
// several seconds a teardown may legitimately take, and neither needs to,
// because the server keeps running and the monitor completes on its own.
//
// It latches "the server ended this session", which the exit classification
// reads: a child the server kills (SIGKILL via the cancelled context, or SIGHUP
// from the PTY closing) exited because it was told to, and reporting that as a
// crash would paint every routine restart red. See crashedExit.
func (h *Handler) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Stored before cancel() so the store is ordered ahead of the kill it
	// triggers, and therefore ahead of the cmd.Wait() return the monitor
	// classifies.
	h.shutdownRequested.Store(true)
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

// Shutdown ends the session and waits for its teardown to finish, bounded by
// ctx. It returns ctx.Err() if the budget expires first and nil once every
// goroutine the handler started has returned.
//
// This is the form for a caller that is about to stop the process. Close alone
// leaves the cgroup teardown, the /proc sweep and the client notification in
// flight, and a process that exits underneath them loses whatever had not
// finished. Under a container that is absorbed by the runtime tearing the
// container down; outside one, or across an in-process restart, it is not.
//
// The wait is worth more than its own success: an expiry is the only signal that
// a teardown exceeded its budget, which is otherwise silent. Log it. There is no
// useful branch to take, because the caller is stopping either way.
//
// Timing to size ctx against: cmd.WaitDelay bounds the child's reap at 5s, and
// the containment and marker-domain ladders each spend up to three containGrace
// windows plus a /proc scan. A grace shorter than that is a deliberate choice to
// hear about the overrun rather than to wait it out.
func (h *Handler) Shutdown(ctx context.Context) error {
	h.Close()
	return h.wait(ctx)
}

// wait blocks until every goroutine ensureStarted launched has returned, or ctx
// expires. Returns nil for a handler that never started one.
//
// Unexported because the only caller that needs the halves apart is
// SessionManager, which must signal all of its sessions before waiting on any of
// them so their teardown windows overlap instead of summing. A consumer holding
// one handler wants Shutdown.
func (h *Handler) wait(ctx context.Context) error {
	h.mu.Lock()
	done := h.done
	h.mu.Unlock()
	if done == nil {
		return nil
	}
	return waitClosed(ctx, done)
}

// waitClosed blocks until done closes or ctx expires, preferring done when both
// are already ready.
//
// The default case is load-bearing, not an optimization. A bare two-case select
// chooses UNIFORMLY AT RANDOM among ready cases, so a teardown that had already
// finished would report an expiry roughly half the time whenever the caller's
// budget had also run out — which is exactly the shape of a second Shutdown on a
// fatal path, and a wrong answer that only shows up in half the runs is worse
// than a consistently wrong one.
func waitClosed(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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

// ExitError returns what cmd.Wait() reported for the child: nil when the process
// exited cleanly (status 0), was never started, or has not exited yet, and
// otherwise the wait error — an *exec.ExitError for a non-zero or signalled
// exit. Pair it with Exited to tell "not dead yet" from "died cleanly"; the
// value is final once Exited reports true. Safe for concurrent use.
//
// This is the same error WithOnProcessExit's callback receives, retained so a
// consumer that did not register one (or that reads state on its own schedule)
// can still tell a clean exit from a crash. The engine's own status stream uses
// it to report exited vs crashed (see crashedExit).
func (h *Handler) ExitError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exitErr
}

// setExitError retains the reaped child's wait error. Called exactly once, by
// the process monitor, before procExitCh closes.
func (h *Handler) setExitError(werr error) {
	h.mu.Lock()
	h.exitErr = werr
	h.mu.Unlock()
}

// exitOutcome reports whether the child has exited and, when it has, whether the
// exit counts as a crash under crashedExit's rule. One call so the status sweep
// reads liveness and outcome as a pair rather than deriving the second from a
// second, later observation.
func (h *Handler) exitOutcome() (exited, crashed bool) {
	if !h.Exited() {
		return false, false
	}
	return true, crashedExit(h.ExitError(), h.shutdownRequested.Load())
}

// crashedExit classifies a reaped child's exit: true means the program died in a
// way an operator should see as a failure (StatusCrashed), false means it ended
// normally (StatusExited).
//
// The rule, and why each case falls where it does:
//
//   - werr == nil — exit status 0. Clean, never a crash.
//   - serverInitiated — the SERVER ended this session (Close, and therefore
//     Shutdown, SessionManager.Close and the idle reaper too). The child is killed by the
//     cancelled context (SIGKILL) or hung up by the PTY closing, so its wait
//     status is signalled through no fault of its own. Not a crash: classifying
//     it as one would turn every routine server shutdown, every closed tab and
//     every reap into a fleet of red dots — the single worst failure mode this
//     boundary has, so the server's own intent outranks the wait status.
//   - SIGHUP — the controlling terminal went away. The only thing that closes
//     this session's PTY master is the engine itself (Close, or the monitor
//     after the child is already reaped), so a hangup means "the session ended",
//     not "the program failed". Excluded independently of serverInitiated
//     because the PTY close and the flag are set by the same teardown but
//     observed through different mechanisms, and a hangup is not evidence of
//     failure whichever way it arrived.
//   - any other *exec.ExitError — a non-zero exit status or a terminating signal
//     the program was not asked for (SIGSEGV, SIGKILL from an OOM killer,
//     SIGTERM from outside). This is the crash case, and the only one.
//   - any other error — Wait itself failed (a lost child, ErrWaitDelay without
//     an exit status). We have no evidence about the program's own outcome, so
//     the safe answer is the quiet one: not a crash. A crash claim needs
//     positive evidence, because a false crash is the expensive direction.
func crashedExit(werr error, serverInitiated bool) bool {
	if werr == nil || serverInitiated {
		return false
	}
	var ee *exec.ExitError
	if !errors.As(werr, &ee) {
		return false
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGHUP {
		return false
	}
	return true
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

// ProgressValue returns the percentage carried by the session's last OSC 9;4
// progress sequence: -1 when absent or unknown (no sequence seen, or a state
// that carries no percentage), else 0-100. A consumer pairs it with Progress to
// render a determinate bar; -1 is not 0%. Safe for concurrent use.
//
// The engine's own status sweep does not call this: it reads the same field
// through statusSnapshot, which batches every status input under one lock. Use
// this when you want the value on its own.
func (h *Handler) ProgressValue() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.screen.ProgressValue
}

// ScrollbackBounds returns the absolute-index bounds of this session's retained
// scrollback history: committed is the index of the next line to be committed
// (one past the newest committed line, and the absolute index of the current
// top screen row), oldest is the index of the oldest line still retained.
// committed-oldest is therefore the number of lines retained, and every index
// in [oldest, committed) can still be replayed on resume; anything below oldest
// has been evicted by the retention cap (WithScrollbackCapacity).
//
// Both are 0 for a session that has committed nothing yet. Indices are
// monotonic for the life of the session and are never reused, so a consumer can
// compare two observations to see how much history advanced or was evicted.
//
// These are the same two values the resume handshake reports to a client; this
// accessor exists so a consumer can observe them without decoding a wire frame.
// Read-only by design: history bounds are produced by the child's output and the
// configured capacity, never set from outside.
//
// Returned as a pair under a SINGLE lock acquisition, for the same reason as
// titles() and statusSnapshot: read through two calls, a commit landing in
// between yields a pair that never existed (an oldest from after an eviction
// beside a committed from before it), which a consumer would read as a
// larger-than-real retained range. Safe for concurrent use.
func (h *Handler) ScrollbackBounds() (committed, oldest uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.scrollback.Committed(), h.scrollback.OldestIndex()
}

// screenStatus is one atomic read of the status-relevant screen state, returned
// as a struct rather than a result list so the sweep can grow another status
// input without either growing a longer tuple or taking the lock twice.
type screenStatus struct {
	notifMsg string // the last OSC 9 notification message
	title    string // the OSC 0/2 window title
	notifSeq uint64 // increments per captured notification (a repeat is still new)
	progress int    // OSC 9;4 state: -1 none, 0 clear, 1 value, 2 error, 3 indeterminate, 4 warning
	// progressValue is the OSC 9;4 percentage: -1 absent/unknown, else 0-100.
	progressValue int
}

// statusSnapshot returns the status-relevant screen state — the OSC 9;4
// progress state and its percentage, the last OSC 9 notification message and
// its sequence, and the OSC 0/2 window title — under a SINGLE lock
// acquisition, so the status sweep's per-session snapshot is internally
// consistent. Reading the same fields through the individual getters
// (Progress, ProgressValue, Notification, Title) takes the lock once per call,
// and a PTY chunk parsed between two of those calls can pair a stale active
// progress with a fresh turn-end notification — the inconsistent pairing
// computeStatus must not see (its fresh-latch precedence guards the state
// machine; this getter removes the torn read). Every field a sweep reads
// belongs in here for that reason, the percentage included: a bar at 40% next
// to a state that has already moved on is the same defect in visible form.
func (h *Handler) statusSnapshot() screenStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return screenStatus{
		notifMsg:      h.screen.Notification,
		title:         h.screen.Title,
		notifSeq:      h.screen.NotificationSeq,
		progress:      h.screen.Progress,
		progressValue: h.screen.ProgressValue,
	}
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
	// Force-kill a child that ignores the PTY-close SIGHUP: Close/reap cancels ctx
	// (default Cancel = SIGKILL) and WaitDelay bounds the grace so cmd.Wait cannot
	// block the monitor goroutine forever.
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = h.cfg.workDir
	h.reap = h.newSessionReap()
	cmd.Env = h.childEnv(h.reap)
	if cols < 1 {
		cols = defaultCols
	}
	if rows < 1 {
		rows = defaultRows
	}
	h.contain = h.beginContainment(cmd)
	ptmx, err := startSessionPTY(cmd, cols, rows)
	// The cgroup fd is only needed at clone time; holding it would leak one
	// descriptor per session. Released on the failure path too.
	h.contain.releaseFD()
	if err != nil {
		h.contain.teardown()
		h.contain = nil
		// A spawn that never produced a process has no tree to reap, but the
		// domain is dropped anyway so a later teardown cannot scan for a marker
		// nothing ever carried.
		h.reap = nil
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
	// Captured once: the monitor below needs it after cmd.Wait, where reading
	// cmd.Process would race the exec package's own bookkeeping.
	pid := cmd.Process.Pid
	h.cfg.logger.Info("terminal: process started",
		"pid", pid, "command", loggedCommand, "cols", cols, "rows", rows)

	// PTY reader goroutine — feeds VT screen and notifies clients.
	h.wg.Go(func() { h.readLoop(ctx) })
	// Flush scheduler — sends screen updates to all clients.
	h.wg.Go(func() { h.flushLoop(ctx) })
	// Periodic per-session cost line, when the consumer asked for one. Stopped
	// explicitly by the monitor before teardown, not just by ctx.
	stopSampler := h.startCostSampler(ctx)
	// Process monitor — reaps the child (so it does not linger as a
	// zombie), fires the documented onProcessExit callback with the
	// exit status, and cancels the read/flush loops on natural child
	// exit so the scheduler goroutine does not leak after the process dies.
	h.wg.Go(func() {
		werr := cmd.Wait() // reap; werr carries the exit status
		// os/exec is done with this pid, so the zombie sweep may stop excluding
		// it. One mutex, no allocation, and ahead of anything that can block.
		spawnForget(pid)
		// Retain the outcome BEFORE procExitCh closes, so any reader that sees
		// Exited() == true also sees the final exit error (the status sweep reads
		// the pair through exitOutcome). Nothing here can panic — a mutex and one
		// assignment — so it is safe ahead of the teardown defer below.
		h.setExitError(werr)
		// Registered FIRST so LIFO runs it LAST: the marker reap walks /proc, and
		// the client-visible exit broadcast (procExitCh, below) must not wait on a
		// scan. Reclaiming a stranded tree a few milliseconds later costs nothing;
		// delaying every session's 4001 close by a scan is user-visible, and it
		// also moved Exited() late enough to break the attach-after-exit contract
		// test that pins the replay-before-4001 grace.
		defer h.reap.teardown()
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
			// exited-but-undeleted session holds it until Close/reap (reaper
			// is off by default). A later Close's second ptmx.Close is a no-op.
			h.mu.Lock()
			if h.ptmx != nil {
				_ = h.ptmx.Close() // #nosec G104 -- best-effort; child already exited
			}
			h.mu.Unlock()
			if r := recover(); r != nil {
				h.cfg.logger.Error("terminal: onProcessExit callback panicked", "panic", r)
			}
		}()
		// Containment teardown is owned HERE, and only here. Close does not
		// run it: Close holds h.mu, and cancelling the context kills the head
		// process, so this Wait returns and teardown happens on that path too.
		// One owner plus the handle's own sync.Once means a crash-then-close
		// sequence cannot double-run it.
		// Stop sampling before the cgroup is removed, then tear down.
		stopSampler()
		h.contain.teardown()

		// Symmetric with the "process started" INFO above so operators see the
		// session lifecycle end and its exit status in the logs, not just the
		// start. werr is nil on a clean (exit 0) shutdown; a child exiting
		// non-zero is a normal session end, not a server fault, so this stays
		// INFO (avoids WARN-spam on ordinary command exits).
		//
		// mem_peak_bytes/tasks_peak are the session's true high-water marks
		// (cgroup memory.peak/pids.peak), not a sample, and they stay readable
		// after the members are gone. tasks_peak counts TASKS, not processes:
		// the pids controller reports TIDs. Zero when containment is off, so the
		// line keeps its shape either way.
		memPeak, tasksPeak := h.contain.peaks()
		h.cfg.logger.Info("terminal: process exited",
			"pid", pid, "error", werr,
			"mem_peak_bytes", memPeak, "tasks_peak", tasksPeak)
		if h.cfg.onProcessExit != nil {
			h.cfg.onProcessExit(werr)
		}
	})
	// Arm the completion signal. A bare `go` on purpose: this goroutine's whole
	// job is to observe h.wg, so counting it in h.wg would deadlock. It holds no
	// session state and ends with the four above.
	h.done = make(chan struct{})
	go func(done chan struct{}) {
		h.wg.Wait()
		close(done)
	}(h.done)
	return nil
}

// childEnv assembles the environment for a session's process.
//
// Advertise a capable, well-known terminal identity so apps enable their full
// feature set. TERM/COLORTERM unlock 256-color + truecolor. TERM_PROGRAM
// iTerm.app (>= 3.6.6) is the single identity that unlocks OSC 9;4 progress for
// BOTH kiro-cli (allowlists iTerm.app/WezTerm/Windows Terminal) and Claude Code
// (iTerm.app >= 3.6.6), plus DEC 2026 synchronized output — all of which this
// engine implements. Capabilities it does NOT implement (inline images, the kitty
// IMAGE protocol, and the kitty keyboard flags beyond the implemented
// disambiguate subset — see vt/kitty.go and the README keyboard section) are
// consumed silently and never mis-rendered, so over-claiming degrades gracefully
// rather than corrupting the screen.
//
// h.cfg.env is appended last so a consumer's WithEnv can override any of these
// (last value wins).
//
// The reap marker is PREPENDED, ahead of even os.Environ(), so it sits at the
// front of /proc/<pid>/environ and the reap scan can read a bounded prefix per
// pid instead of a whole ARG_MAX environment (see reap.go).
//
// BOTH env sources are stripped of that key first, not just the consumer's.
// os/exec keeps the LAST value for a repeated key, so any later assignment
// displaces the engine's freshly minted marker, and the session's tree then
// carries a marker the engine never minted: the scan matches nothing and reaping
// is silently off. The INHERITED environment is the likelier carrier of the two
// and the one the engine controls least — a server started from inside one of
// these very sessions inherits that session's live marker, so every session it
// spawns would inherit it too and the whole process would reap nothing.
func (h *Handler) childEnv(reap *sessionReap) []string {
	inherited := stripReapMarker(os.Environ())
	consumer := stripReapMarker(h.cfg.env)
	env := make([]string, 0, len(inherited)+len(consumer)+5)
	if pair := reap.envPair(); pair != "" {
		env = append(env, pair)
	}
	env = append(env, inherited...)
	env = append(env,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"TERM_PROGRAM=iTerm.app",
		"TERM_PROGRAM_VERSION=3.6.6",
	)
	return append(env, consumer...)
}

// startSessionPTY forks the session's process on a new PTY and records its pid as
// os/exec-owned.
//
// The spawn lock is held across the fork itself, not merely the registration:
// the zombie sweep must never be able to see a child that exists but is not yet
// recorded as owned, or it could collect the head's exit status out from under
// cmd.Wait and turn every session's exit into an unknown one (see zombiereap.go).
func startSessionPTY(cmd *exec.Cmd, cols, rows int) (*os.File, error) {
	spawnLock()
	defer spawnUnlock()
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}) // #nosec G115 -- cols/rows are floored above and bounded by the client protocol
	if err == nil && cmd.Process != nil {
		spawnRegister(cmd.Process.Pid)
	}
	return ptmx, err
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
	clients map[*websocket.Conn]uint64
	// writers/gens carry the dispatch-safety metadata captured with the same
	// registry snapshot as clients: the per-socket state whose writeMu/
	// resumeGen gate the actual writes, and the resume generation this frame
	// was built against (see clientState.resumeGen and dispatchFrame).
	writers          map[*websocket.Conn]*clientState
	gens             map[*websocket.Conn]uint64
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

// buildFrame computes the next outbound frame under h.mu. Returns a nil frame
// if there is nothing to send (no resize yet, flush held, no attached
// clients, or no changed rows and no scroll lines). holdUntil is non-zero
// when a flush hold is active — a DEC 2026 synchronized-output hold or the
// post-resize redraw-settle hold — and the scheduler arms a retry at that
// deadline so a final held redraw with no subsequent PTY byte still flushes
// (a trigger-only scheduler would strand it).
func (h *Handler) buildFrame() (frame *flushFrame, holdUntil time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()

	clients, writers, gens := h.registry.Snapshot()
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
		if h.redrawCoalesceNow {
			// The redraw-settle cap lapsed while the child was still writing, so
			// this pass is the coalesced update for that interval. Force a FULL
			// repaint: a diff against the client's cache would describe a screen
			// caught mid-redraw, which is the partial state the hold exists to
			// hide. Consumed here rather than at the lapse so a DEC 2026 hold
			// overlapping the lapse cannot swallow it.
			h.builder.Reset()
			h.redrawCoalesceNow = false
		}
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
		frame.clipboardPayload = encodeClipboardMsg(h.pendingClipboard)
		h.pendingClipboard = nil
	}
	if frame != nil {
		frame.writers, frame.gens = writers, gens
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
	durable := durableSubset(frame.clipboardPayload, scrollPayloads)
	// Fan out concurrently: one goroutine per client, each writing ITS frames
	// in order. Serial fan-out let one wedged client stall every other
	// client's output for up to 5s × payload count (judgement finding); now a
	// wedged client costs only itself, and the tick blocks at most one 5s
	// window total. Per-connection write serialization is coder/websocket's
	// (concurrent writers to one conn are internally locked); the multi-write
	// SEQUENCES that must not interleave — this loop's payload run and
	// handleResume's snapshot+batch — are serialized by the per-client
	// writeMu below, and a frame that predates a resume snapshot is stripped
	// to its durable payloads by the generation check (see clientState).
	// sweepAcks stays outside the lock on purpose: a bare ackOnly
	// interleaving into a resume batch is harmless (the client ignores an
	// ack at or below its trimmed count, and the resumeAck is the
	// authoritative sync). withClientAck clones the shared template per
	// call, so goroutines never share a buffer.
	var wg sync.WaitGroup
	// delivered collects, per client that had at least one payload written,
	// the ack value those payloads actually carried (the captured ack, or
	// the restamped one when the sequence was stripped to its durable
	// subset) — so the ack bookkeeping below never records a value no frame
	// carried.
	var deliveredMu sync.Mutex
	delivered := make(map[*websocket.Conn]uint64, len(frame.clients))
	for ws, ack := range frame.clients {
		wg.Add(1)
		go func(ws *websocket.Conn, ack uint64) {
			defer wg.Done()
			if stamped, ok := writeClientPayloads(ws, ack, frame.writers[ws], frame.gens[ws], payloads, durable); ok {
				deliveredMu.Lock()
				delivered[ws] = stamped
				deliveredMu.Unlock()
			}
		}(ws, ack)
	}
	wg.Wait()
	// Record what each client was just told, so the next no-frame tick's ack
	// sweep doesn't resend a value a content frame already carried. A client
	// stripped to a non-empty durable subset IS recorded, at the restamped
	// value its payloads actually carried (writeClientPayloads returns it);
	// a client with nothing written is excluded — its authoritative ack is
	// the resumeAck its batch already wrote. NoteAcksSent is monotonic, so
	// a recorded value can never regress lastAckSent below what the batch
	// stored.
	h.registry.NoteAcksSent(delivered)
}

// durableSubset assembles the payloads that survive a resume-generation
// mismatch: the ones a resume batch does NOT re-deliver (see
// writeClientPayloads). Relative order matches the full dispatch sequence
// (clipboard, then scroll chunks). Returns nil when the frame carries no
// durable payloads — the common case for pure screen/modes diffs — and
// returns scrollPayloads as-is when there is no clipboard (no copy; the
// slice and its payloads are read-only downstream).
func durableSubset(clipboardPayload []byte, scrollPayloads [][]byte) [][]byte {
	if clipboardPayload == nil {
		return scrollPayloads
	}
	durable := make([][]byte, 0, 1+len(scrollPayloads))
	durable = append(durable, clipboardPayload)
	return append(durable, scrollPayloads...)
}

// writeClientPayloads writes one dispatch pass's payload sequence to one
// client, under the resume gate, and reports whether anything was written:
//
//   - The socket's writeMu is acquired BLOCKING: a resume batch in flight
//     delays this sequence rather than dropping it, because the frame's
//     payloads are not regenerable — buildFrame has already committed its
//     scrollLines to the ring and consumed the one-shots (scrollbackCleared,
//     clipboard, bell), so a skipped frame would be a permanent scrollback
//     hole for the resuming client (its post-batch haveThrough jumps past
//     the missing lines, and resume replays only above haveThrough). The
//     cost is bounded: the batch's own writes run under a 10s budget, a
//     healthy batch is milliseconds, and only the resuming client's
//     goroutine waits — but the pass's wg.Wait does inherit that bound on
//     top of its own 5s write budget (the pre-existing wedged-client
//     exposure, same class, larger constant; see dispatchFrame's comment).
//
//   - A frame whose resume generation no longer matches was built BEFORE
//     the batch's screen snapshot, so its STATE payloads (screen, modes,
//     title) are dropped: writing them after the batch would regress the
//     client to pre-snapshot state the batch already superseded. Its
//     DURABLE payloads (scroll chunks, clipboard) are still written,
//     because the batch does not re-deliver them: the frame's scroll lines
//     sit at or below the snapshot's committed index while the resuming
//     client's haveThrough — its own window bottom — already spans their
//     indices, so the batch's LinesFrom(haveThrough+1) replay starts PAST
//     them (and under ring-cap pressure they may already be evicted from
//     the ring entirely); dropping them would be a permanent scrollback
//     hole. The clipboard payload is a consumed one-shot (OSC 52) no later
//     frame will carry again. Writing scroll after the batch is safe:
//     chunks carry absolute indices and the client applies them
//     idempotently. Two residuals, accepted and bounded:
//
//     1. CLOSED 2026-08 (owner-ratified): the client's alt gate now accepts
//     history strictly below its frozen main win.base (store.ts
//     applyScroll), so a client the batch flipped into alt STORES the
//     durable chunk instead of dropping it — the lines surface at alt
//     exit's store rebuild. Only a client bundle predating that fix still
//     drops there, losing exactly what pre-split code lost; wire-compatible
//     both ways.
//
//     2. Bell and the ED3 scrollbackCleared flag ride the dropped screen
//     payload and are lost for the resuming client alone, when its resume
//     acquires the write lock inside the window between this frame's
//     registry.Snapshot() (buildFrame's first statement, before any row
//     renders) and this goroutine's writeMu acquisition — a full render
//     plus a full payload encode, i.e. milliseconds under a heavy drain,
//     not microseconds. The ED3 loss self-heals at the app's next ED3
//     (kiro-cli re-emits it on every resize redraw); until then that
//     client over-retains history the server dropped. The pre-fix
//     behavior for that client was the ordering race itself.
//
// A nil state is tolerated for frames constructed without a registry
// snapshot (tests); buildFrame always populates writers, so production
// frames never take the ungated path.
//
// Returns the ack value actually stamped on the written payloads (the
// captured ack, or the restamped one on a strip) and whether anything was
// written — so the caller's delivered bookkeeping records what the wire
// carried, never a pre-resume value a strip replaced.
func writeClientPayloads(ws *websocket.Conn, ack uint64, st *clientState, gen uint64, payloads, durable [][]byte) (uint64, bool) {
	stripped := false
	if st != nil {
		st.writeMu.Lock()
		defer st.writeMu.Unlock()
		if st.resumeGen.Load() != gen {
			payloads = durable
			stripped = true
		}
	}
	if len(payloads) == 0 {
		// Nothing to write (a state-only frame stripped bare): report
		// undelivered BEFORE the restamp below, so the empty path's
		// contract — nothing written, nothing recorded — stays observable
		// against the captured ack.
		return 0, false
	}
	if stripped {
		// Restamp with the freshest ack this socket was told. Under this
		// writeMu, after a generation mismatch, that is exactly the latest
		// resume batch's resumeAck value: the batch stored it under this
		// same lock before we acquired it, no other resume can run while we
		// hold it, and no sweep runs concurrently (flushLoop is serial, and
		// sweeps happen only on passes that produced no frame). The
		// captured ack predates the resume; after a LEDGER-LOSS resume it
		// can even exceed the client's reset outbox accounting, where
		// stamping it could falsely trim retransmitted-but-unacked input on
		// the client (applyAck's min(received, bytesSent)). Re-sending the
		// resumeAck's own value is a no-op under the client's monotone
		// applyAck guard.
		ack = st.lastAckSent.Load()
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, p := range payloads {
		ws.Write(writeCtx, websocket.MessageBinary, withClientAck(p, ack)) //nolint:errcheck // best-effort
	}
	return ack, true
}

// controlMsg is a JSON control message from the client.
//
// Field ORDER is alignment-driven (govet fieldalignment), not semantic: the
// slice and strings lead, then the pointer, then the 8-byte scalars, then the
// machine ints. Read the comments for meaning, not the layout.
type controlMsg struct {
	// HaveThrough is the highest absolute line index the client already
	// holds in its store. Sent in resume control messages so the server
	// replays exactly the lines the client is missing (indices greater
	// than HaveThrough), aligned by absolute identity rather than by a
	// fragile count. -1 means the client holds nothing (cold load / DOM
	// eviction) and wants the full retained history. The server clamps
	// the replay start into the retained range and reports any eviction
	// gap via the resumeAck bounds.
	HaveThrough *int64    `json:"haveThrough"`
	Type        string    `json:"type"`
	SessionID   SessionID `json:"sessionId,omitempty"`
	// ReplayMax bounds the resume replay to the newest N missing lines, so an
	// attach costs at most the client's own residency however deep the ring is.
	// Decoded as RawMessage and parsed FIELD-LOCALLY (parseReplayMax) because
	// this handler drops any control whose unmarshal returns an error: a
	// malformed value on an ADVISORY field must read as absent, never cost the
	// client its whole resume. Absent means full replay — today's behavior for
	// every client older than this field. Honored only when the server declares
	// paging, so a client that sends it optimistically to a non-paging server
	// still gets its full backfill. See docs/paged-scrollback.md §4.5.
	ReplayMax json.RawMessage `json:"replayMax,omitempty"`
	SentBytes uint64          `json:"sentBytes,omitempty"`
	// FromAbs/MaxLines carry a `history` request: serve at most MaxLines
	// retained lines starting at absolute index FromAbs. Signed on the wire and
	// validated BEFORE any conversion to uint64 (§4.1) — the subtraction-form
	// overflow guard in historyControl is the reason these are int64 rather
	// than uint64.
	FromAbs  int64 `json:"fromAbs,omitempty"`
	MaxLines int64 `json:"maxLines,omitempty"`
	Cols     int   `json:"cols,omitempty"`
	Rows     int   `json:"rows,omitempty"`
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
	ws, err := acceptWS(w, r, h.cfg.originPolicy)
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
// v4 typed-framing state (the negotiation contract is WireProtocolVersion's
// doc comment in wire_binary.go): a binary bootstrap resume declaring
// protocolVersion >= 4 ARMS the connection; the
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

// handleTextControl applies the v4 text-frame policy (see
// WireProtocolVersion in wire_binary.go) to one text message: text is only ever a
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

// resumeControl is handleControl's resume case: the wire-version gate, the
// v4 framing latch, the per-socket spam throttle, and the handleResume
// dispatch. Extracted verbatim (gocognit) — behavior is handleControl's.
func (h *Handler) resumeControl(ws *websocket.Conn, state *clientState, c *controlMsg, d controlDisposition, onResumeServed func()) controlDisposition {
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
	if c.SessionID == "" {
		return d
	}
	if !state.takeResumeToken() {
		// Resume-spam throttle (2026-08, owner-ratified): each resume runs a
		// full per-socket write transaction — writeMu held across a screen
		// snapshot, a builder reset and a replay of up to the whole retained
		// ring — so an unthrottled client could pin the handler's flush
		// pipeline with back-to-back batches. The bucket (burst 10, one
		// token per 2s) sits far above any legitimate cadence (measured
		// phone churn: ~one resume per 4 minutes; ledger switches and wire
		// upgrades add a handful in quick succession, covered by the burst).
		// An over-limit resume is DROPPED — no resumeAck — which a healthy
		// client never experiences and a hostile one cannot tell from
		// network loss. The v4 framing latch above stays armed either way,
		// so a throttled client's later controls still parse.
		h.cfg.logger.Warn("terminal: resume throttled",
			"session_id", LogID(c.SessionID))
		return d
	}
	// A nil (omitted) haveThrough means the client holds nothing and wants
	// full history (-1), not "have line 0" (which would drop index 0).
	ht := int64(-1)
	if c.HaveThrough != nil {
		ht = *c.HaveThrough
	}
	replayMax := parseReplayMax(c.ReplayMax)
	h.handleResume(ws, state, c.SessionID, ht, c.SentBytes, replayMax)
	if onResumeServed != nil {
		onResumeServed()
	}
	return d
}

// maxSafeInteger is JavaScript's exact-integer ceiling (2^53 − 1). Absolute
// line indices cross the wire as JSON numbers, so a value above this cannot be
// represented exactly on the client and is rejected rather than silently
// rounded (docs/paged-scrollback.md §4.1).
const maxSafeInteger int64 = 1<<53 - 1

// shrinkToBudget returns the number of leading lines of `lines` whose encoded
// size, plus the scroll header, fits pageByteBudget — always at least one line,
// because the per-row ceiling (capRowRuns) guarantees any single row fits.
//
// It keeps a PREFIX (the oldest lines), and that direction is FORCED, not
// stylistic: shrinking from the low end would move the reply's firstIndex above
// the request's fromAbs, which §4.3 defines as the CLAMP signal — every styled
// page would then read as "history permanently trimmed" and paint a false
// marker. The clamp encoding and this direction are coupled; change neither
// alone.
func shrinkToBudget(lines [][]vt.WireRun) int {
	size := encodedScrollHeaderSize
	for i, line := range lines {
		size += encodedRowSize(capRowRuns(line))
		if size > pageByteBudget {
			return max(i, 1)
		}
	}
	return len(lines)
}

// historyControl serves a `history` request: at most maxLines retained lines
// starting at absolute index fromAbs. It is the demand-paging read path
// (docs/paged-scrollback.md §4.2), served INLINE on the socket's read loop like
// every other control, and written under the socket's writeMu so a reply can
// never interleave into a resume batch.
//
// The serve is the INTERSECTION of the request window and the retained range —
// never lines the client did not ask for — so every non-empty reply's
// firstIndex lies inside [fromAbs, end) and the client's correlation always
// succeeds. An EMPTY reply carries the request's own fromAbs (never
// LinesRange's empty-case firstAbs, which is `committed` — an index far outside
// the window) and means "nothing in this range is retained", which the client
// reads as a permanent trim.
func (h *Handler) historyControl(ws *websocket.Conn, state *clientState, c *controlMsg) {
	// A history control before the socket's first successful resume has no
	// attached session to answer for, and ignoring it keeps pre-resume sockets
	// cost-free. The client orders this by construction (its bootstrap sends
	// resume first and controls are FIFO).
	if state.session.Load() == nil {
		h.cfg.logger.Debug("terminal: history control before resume; ignoring")
		return
	}
	if !h.historyPagingDeclared() {
		// Not advertised for this ring depth, so a client should never have
		// sent one. Debug rather than Warn: a stale bundle is a pairing
		// artifact, not abuse.
		h.cfg.logger.Debug("terminal: history control but paging not declared",
			"scrollback_capacity", h.cfg.scrollbackCapacity)
		return
	}
	// Validation order is normative: maxLines FIRST, then fromAbs against the
	// SUBTRACTION form of the safe-integer bound. The addition form
	// (fromAbs+maxLines <= maxSafeInteger) is itself the overflow it exists to
	// reject, so `end` below is exact in both languages.
	if c.MaxLines < 1 || c.MaxLines > historyPageSize {
		h.cfg.logger.Debug("terminal: history maxLines out of range", "max_lines", c.MaxLines)
		return
	}
	if c.FromAbs < 0 || c.FromAbs > maxSafeInteger-c.MaxLines {
		h.cfg.logger.Debug("terminal: history fromAbs out of range", "from_abs", c.FromAbs)
		return
	}
	if !state.takeHistoryToken() {
		h.cfg.logger.Warn("terminal: history throttled")
		return
	}

	fromAbs := uint64(c.FromAbs) // #nosec G115 -- validated non-negative above
	end := fromAbs + uint64(c.MaxLines)

	h.mu.Lock()
	oldest := h.scrollback.OldestIndex()
	committed := h.scrollback.Committed()
	start := max(fromAbs, oldest)
	lim := min(end, committed)
	var lines [][]vt.WireRun
	if start < lim {
		_, lines = h.scrollback.LinesRange(start, int(lim-start)) // #nosec G115 -- bounded by maxLines
	}
	h.mu.Unlock()

	firstIndex := fromAbs
	if len(lines) > 0 {
		// The byte budget can serve FEWER lines than asked for. A short page is
		// never a terminator: the client's next paced trigger requests the
		// remainder.
		lines = lines[:shrinkToBudget(lines)]
		firstIndex = start
	}

	ctx, cancel := context.WithTimeout(context.Background(), historyWriteTimeout)
	defer cancel()
	// Stamped with the socket's current ack but deliberately NOT recorded in
	// lastAckSent: the next ack-only sweep may send one redundant ack, which is
	// harmless because the client's apply is monotonic.
	ack := state.lastAckSent.Load()
	state.writeMu.Lock()
	defer state.writeMu.Unlock()
	ws.Write(ctx, websocket.MessageBinary, encodeScrollMsg(ack, firstIndex, lines)) //nolint:errcheck // best-effort
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
		return h.resumeControl(ws, state, &c, d, onResumeServed)
	case ctlTypeResize:
		d.known = true
		h.handleResize(state, c.Cols, c.Rows)
	case ctlTypePing:
		d.known = true
		h.handlePing(ws)
	case ctlTypeHistory:
		d.known = true
		h.historyControl(ws, state, &c)
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

// testResumeBatchHold, when non-nil, is invoked by handleResume after its
// batch writes complete but BEFORE the write-lock release. Test-only
// (resume_ordering_test.go): it lets a test hold a REAL resume batch open at
// its most adversarial instant and drive live output against the dispatch
// gate deterministically. Atomic so a test's store cannot race a straggling
// handler goroutine (httptest.Close does not wait for hijacked conns).
// Never set in production.
var testResumeBatchHold atomic.Pointer[func()]

// historyPagingDeclared reports whether this server advertises demand-paged
// scrollback to clients (resumeAckFlagHistoryPaging) and therefore honors the
// `history` control and the resume replay bound. Two conditions, both
// necessary: the handler serves the control (always true for this build), and
// the ring is at least paginationMinRing deep. The depth half is what keeps the
// two bounds consistent: a ring the resume replay can TRUNCATE must declare
// paging, or the withheld rows are unreachable for the life of the session
// (docs/paged-scrollback.md §4.5). Below the threshold the replay carries the
// whole ring, so there is nothing to page for.
func (h *Handler) historyPagingDeclared() bool {
	return h.cfg.scrollbackCapacity >= paginationMinRing
}

// parseReplayMax extracts controlMsg.ReplayMax's advisory value. It returns
// nil for absent, null, malformed (fractional, string, overflowing), and
// out-of-domain (< 1) values — every one of which means "no bound, replay in
// full", today's behavior. A valid value is clamped DOWN to maxReplayLines,
// mirroring the client's own pre-send clamp so the sent value and the honored
// value are the same number (the client's replay-jump prediction depends on
// that identity; §4.5).
func parseReplayMax(raw json.RawMessage) *int64 {
	if len(raw) == 0 {
		return nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil
	}
	if n < 1 {
		return nil
	}
	bounded := min(n, maxReplayLines)
	return &bounded
}

// replayStart returns the absolute index the resume replay should begin at.
//
// The base is "everything the client is missing" (haveThrough + 1), clamped so
// the replay carries at most maxReplayLines however deep the ring. The clamp is
// UNCONDITIONAL — not gated on the client having asked, and not on this server
// declaring paging — because the ring's depth is an operator number that can be
// hundreds of thousands of lines, and a resume that streams all of it is tens of
// megabytes written under one write lock inside a single 10s context. There is no
// depth at which replaying the whole ring is the right answer, so no caller gets
// to opt out of the bound; a client may only ask for LESS.
//
// The client applies the same clamp to the value it sends, so the bound the
// server honors always equals the one the client predicted — the identity its
// replay-jump detection depends on (docs/paged-scrollback.md §4.5). Clamping the
// START rather than the end is what makes the jump detectable at all: the client
// sees a first index above what it holds and reclassifies the stranded band.
func replayStart(committed uint64, haveThrough int64, replayMax *int64) uint64 {
	var from uint64
	if haveThrough >= 0 {
		from = uint64(haveThrough) + 1
	}
	bound := uint64(maxReplayLines)
	if replayMax != nil {
		bound = min(bound, uint64(*replayMax)) // #nosec G115 -- parseReplayMax guarantees 1..maxReplayLines
	}
	// uint64 guard: when committed is at or below the bound the floor is 0, so
	// the clamp contributes nothing and `from` stands.
	if committed > bound {
		return max(from, committed-bound)
	}
	return from
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
// replayMax lets the client ask for FEWER than the maxReplayLines the replay is
// bounded to regardless (see replayStart: the bound is unconditional, because no
// ring depth makes streaming the whole ring the right answer). parseReplayMax has
// already clamped it to the same ceiling, so the number the client sent and the
// number honored here are identical — the identity the client's replay-jump
// prediction depends on (docs/paged-scrollback.md §4.5).
func (h *Handler) handleResume(ws *websocket.Conn, state *clientState, sessionID SessionID, haveThrough int64, sentBytes uint64, replayMax *int64) {
	ack, created := h.registry.ResolveSession(state, sessionID)
	// Capability declaration for the resumeAck's historyPaging bit. It no longer
	// gates the replay clamp: that bound is unconditional, so a shallow-ring
	// server bounds its replay too.
	paging := h.historyPagingDeclared()
	ledgerLost := created && sentBytes > 0
	if ledgerLost {
		// The client half of the event gcIdleSessions logged server-side;
		// together the two lines make a forgotten-ledger incident correlatable
		// end to end.
		h.cfg.logger.Info("terminal: resume key missed with claimed sent bytes; signaling ledger loss",
			"session_id", LogID(sessionID), "sent_bytes", sentBytes)
	}

	// The whole exchange — screen snapshot AND the multi-write batch — runs
	// under this socket's write lock, with the generation bumped inside the
	// same h.mu section as the snapshot. Together they close the
	// reconnect-during-output race: a live flush either finished writing
	// before the lock was acquired (then the snapshot below is at least as
	// new as what it wrote), or it blocks on the lock until this batch is
	// done, and the generation check then strips any frame built before
	// this snapshot down to its durable payloads, so no stale screen state
	// is written after the batch (builds hold h.mu, so a frame carrying the
	// new generation was necessarily built after the snapshot). Without
	// this, a flush could land out of order around the batch and the
	// batch's older window overwrote a newer screen — a reconnect during
	// active output showed an old or mixed screen until an unrelated full
	// repaint (R4 adversarial finding, gpt).
	state.writeMu.Lock()
	defer state.writeMu.Unlock()

	h.mu.Lock()
	state.resumeGen.Add(1)
	// Force a full repaint on the next flush. Taken INSIDE the snapshot's
	// h.mu section, so the first frame built after this instant — including
	// one built while the batch below is still writing, which the blocking
	// dispatch gate delivers right after it — is a FULL frame of the then-
	// current screen: the resuming client converges in one frame instead of
	// receiving a diff against a cache it never had, and every other client
	// gets the same full repaint an attach always forced.
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
	from := replayStart(committed, haveThrough, replayMax)
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
	ws.Write(ctx, websocket.MessageBinary, encodeResumeAck(ack, h.bootEpoch, committed, oldest, ledgerLost, paging)) //nolint:errcheck // best-effort
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

	// Test seam: lets resume_ordering_test hold a REAL batch open (writeMu
	// still held, batch writes done) to drive live output against the gate
	// deterministically. Nil in production.
	if hold := testResumeBatchHold.Load(); hold != nil {
		(*hold)()
	}

	// A fresh attach ends any zero-client suspension: poke the scheduler so
	// the full-repaint flush (against the builder reset taken with the
	// snapshot above) repaints the window idempotently on the first pass.
	// The deferred writeMu.Unlock runs right after this poke; the pass it
	// wakes blocks on the lock for at most that gap.
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
	// A fresh arm is a fresh redraw, so it gets the full coalescing budget.
	h.redrawRearms = 0
	h.redrawCoalesceNow = false
}

// disarmRedrawSettle ends the hold for good and records which exit fired, so a
// program that never goes quiet after a resize is observable rather than silent.
// Caller holds h.mu.
func (h *Handler) disarmRedrawSettle(reason string) {
	h.redrawSettleUntil = time.Time{}
	h.cfg.logger.Debug("terminal: redraw-settle hold released", "reason", reason, "rearms", h.redrawRearms)
}

// redrawHoldUntil returns the moment the redraw-settle hold could next change
// state, or the zero time when the hold is inactive or this pass may flush.
// Caller holds h.mu.
//
// Three exits, and they are exhaustive: the child went quiet (the redraw is
// over, release for good); the cap lapsed while the child is still writing and
// coalescing budget remains (let ONE full repaint through, then re-arm); or the
// cap lapsed with the budget spent (release for good and stream normally). Only
// the middle one is new, and it is what stops a redraw longer than the cap from
// streaming its remainder a partial screen at a time.
func (h *Handler) redrawHoldUntil(now time.Time) time.Time {
	if h.redrawSettleUntil.IsZero() {
		return time.Time{}
	}
	quietAt := h.redrawLastData.Add(redrawSettleQuiet)
	if !now.Before(quietAt) {
		h.disarmRedrawSettle("settled")
		return time.Time{}
	}
	if !now.Before(h.redrawSettleUntil) {
		if h.redrawRearms >= redrawSettleMaxRearms {
			// The child has been writing continuously for the whole budget, so
			// this is a stream rather than a redraw. Stop holding it.
			h.disarmRedrawSettle("rearm budget exhausted")
			return time.Time{}
		}
		h.redrawRearms++
		h.redrawCoalesceNow = true
		h.redrawSettleUntil = now.Add(redrawSettleCap)
		return time.Time{}
	}
	if h.redrawSettleUntil.Before(quietAt) {
		return h.redrawSettleUntil
	}
	return quietAt
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
