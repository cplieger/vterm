package terminal

import (
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// maxResumeSessions caps the number of retained resume sessions. The session
// map is keyed by client-supplied sessionID, so without a cap a client
// sending many distinct sessionIDs could grow it unbounded (CWE-770)
// until the 60-minute idle GC reclaims them. The cap is generous enough
// that legitimate multi-tab reconnects never hit it; only abusive clients
// do, so there is no behavior change for normal use.
const maxResumeSessions = 1024

// clientRegistry tracks connected WebSocket clients and their session
// state. It owns its own mutex so client add/remove and session
// lookup don't contend with the screen/PTY lock in Handler.
type clientRegistry struct {
	clients  map[*websocket.Conn]*clientState
	sessions map[string]*sessionState
	logger   *slog.Logger
	mu       sync.Mutex
}

// newClientRegistry returns an initialized registry. The logger receives
// session-GC notices; pass the handler's configured logger so WithLogger(nil)
// silences them and a custom logger receives them.
func newClientRegistry(logger *slog.Logger) *clientRegistry {
	return &clientRegistry{
		clients:  make(map[*websocket.Conn]*clientState),
		sessions: make(map[string]*sessionState),
		logger:   logger,
	}
}

// Add registers a new WebSocket connection and returns its state.
func (r *clientRegistry) Add(ws *websocket.Conn) *clientState {
	state := &clientState{}
	r.mu.Lock()
	r.clients[ws] = state
	r.mu.Unlock()
	return state
}

// Remove unregisters a WebSocket connection and returns the terminal size that
// client had last reported (0, 0 if it never sent a resize). The caller uses it
// to decide whether the departure should heal the shared screen size (see
// Handler.maybeHealSize).
//
// It also stamps lastSeen on the departing client's resume ledger, which is
// what makes the idle-GC window mean "60 minutes since a client last HELD this
// ledger". Without the stamp, lastSeen only advanced on attach and on
// client->server input, so a tab open for hours reading agent output without
// typing carried an hours-old lastSeen while still connected — protected only
// by the GC's attached-session skip — and went GC-eligible the instant its
// socket dropped. A phone that slept for 30 minutes then came back to a
// reclaimed ledger and a false "server restarted" banner, because the window
// the iOS-suspension rationale sized had already elapsed before the sleep
// began. Stamping here restarts the clock at the disconnect.
func (r *clientRegistry) Remove(ws *websocket.Conn) (cols, rows int) {
	r.mu.Lock()
	if st := r.clients[ws]; st != nil {
		cols, rows = st.cols, st.rows
		if sess := st.session.Load(); sess != nil {
			sess.lastSeen = time.Now()
		}
	}
	delete(r.clients, ws)
	r.mu.Unlock()
	return cols, rows
}

// RecordSize stores a client's most recently requested terminal size on its
// per-socket state. Per-socket (not per-session) on purpose: in managed mode
// several devices on the same server session share one sessionState, so only
// the socket distinguishes their sizes. Guarded by r.mu.
func (r *clientRegistry) RecordSize(state *clientState, cols, rows int) {
	r.mu.Lock()
	state.cols = cols
	state.rows = rows
	r.mu.Unlock()
}

// MinLiveSize returns the smallest cols and smallest rows across all currently
// connected clients that have reported a size; ok is false when none has. Each
// dimension is minimized independently so the result fits inside every client's
// viewport (every attached client can see the whole screen). Guarded by r.mu.
func (r *clientRegistry) MinLiveSize() (cols, rows int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.clients {
		if st.cols <= 0 || st.rows <= 0 {
			continue
		}
		if !ok {
			cols, rows, ok = st.cols, st.rows, true
			continue
		}
		if st.cols < cols {
			cols = st.cols
		}
		if st.rows < rows {
			rows = st.rows
		}
	}
	return cols, rows, ok
}

// Snapshot returns the connected clients with everything a dispatch pass
// needs to write to them safely without the lock: each client's session ack
// value, its clientState (whose writeMu/resumeGen gate the actual writes —
// see dispatchFrame), and the resume generation observed NOW. A frame built
// against this snapshot is written to a client only while its generation is
// unchanged; a resume that begins after the snapshot bumps the generation,
// which is what keeps a frame built before the resume's own screen snapshot
// from being written after it (the stale-after-newer inversion).
func (r *clientRegistry) Snapshot() (
	acks map[*websocket.Conn]uint64,
	writers map[*websocket.Conn]*clientState,
	gens map[*websocket.Conn]uint64,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	acks = make(map[*websocket.Conn]uint64, len(r.clients))
	writers = make(map[*websocket.Conn]*clientState, len(r.clients))
	gens = make(map[*websocket.Conn]uint64, len(r.clients))
	for ws, state := range r.clients {
		var ack uint64
		if sess := state.session.Load(); sess != nil {
			ack = sess.bytesReceived
		}
		acks[ws] = ack
		writers[ws] = state
		gens[ws] = state.resumeGen.Load()
	}
	return acks, writers, gens
}

// ResolveSession looks up or creates a session for the given ID,
// attaches it to the client state, and returns the session's current
// bytesReceived plus whether the session had to be created (a key miss —
// the caller uses it with the client's claimed sentBytes to signal ledger
// loss on the resumeAck rather than leaving the client to guess from an
// ambiguous received=0).
// A key hit refreshes lastSeen so an attached, input-idle client (a pure
// viewer that reconnects but never types) is not GC-eligible merely because
// IncrementReceived never ran for it. Remove stamps it again on detach, so
// between the two the retention window below always measures time since a
// client last held the ledger.
// Opportunistically GCs sessions unattached for >60 min. The 60-minute window
// is long enough to survive iOS Safari aggressively unloading a backgrounded
// tab (which can keep the same sessionId via sessionStorage but suspends
// the WebSocket for an unbounded period). A shorter window (the previous
// 10-minute one) caused a duplicate-resend bug: tab suspended >10 min →
// session GC'd → reconnect creates new session with bytesReceived=0 →
// client's resumeAck handler trims nothing → retransmitOutbox replays
// every queued chunk → the child re-receives the same input as fresh
// keystrokes and queues duplicate messages.
func (r *clientRegistry) ResolveSession(state *clientState, sessionID string) (ack uint64, created bool) {
	r.mu.Lock()
	sess, ok := r.sessions[sessionID]
	if !ok {
		created = true
		sess = &sessionState{lastSeen: time.Now()}
		r.sessions[sessionID] = sess
		r.gcIdleSessions()
		r.evictOldestSession()
	} else {
		sess.lastSeen = time.Now()
	}
	state.session.Store(sess)
	ack = sess.bytesReceived
	r.mu.Unlock()
	return ack, created
}

// attachedSessions returns the set of sessions currently attached to a live
// client, so the GC and the cap eviction never reclaim a ledger out from
// under a connected socket. The caller MUST hold r.mu.
func (r *clientRegistry) attachedSessions() map[*sessionState]bool {
	m := make(map[*sessionState]bool, len(r.clients))
	for _, st := range r.clients {
		if s := st.session.Load(); s != nil {
			m[s] = true
		}
	}
	return m
}

// gcIdleSessions deletes sessions unattached for longer than the 60-minute
// retention window, skipping sessions attached to a live client (a
// connected viewer's ledger must never be reclaimed under it). lastSeen is
// stamped on attach, on input, and on DETACH, so "idle" here is time since
// the last client let go — not time since the last keystroke. A GC'd
// session that had received bytes is logged: a later reconnect with that
// sessionId takes the explicit ledger-lost path on the resumeAck
// (drop-and-notify), and the log is the server half of that event.
// The caller MUST hold r.mu.
func (r *clientRegistry) gcIdleSessions() {
	attached := r.attachedSessions()
	for id, s := range r.sessions {
		if attached[s] {
			continue
		}
		if time.Since(s.lastSeen) > 60*time.Minute {
			if s.bytesReceived > 0 {
				r.logger.Info("terminal: gc'd idle session with received bytes",
					"session_id", LogID(id),
					"bytes_received", s.bytesReceived,
					"idle", time.Since(s.lastSeen).Round(time.Second))
			}
			delete(r.sessions, id)
		}
	}
}

// evictOldestSession caps the retained session count at maxResumeSessions by
// evicting the oldest-lastSeen entry (backstops the idle GC against a
// client minting many sessionIDs inside the 60-minute window). Sessions
// attached to a live client are preferred-last victims: an abuser minting
// ids can then only evict other abandoned ledgers, not a connected
// client's resume state. If every retained session is attached
// (pathological), the cap still holds by evicting the oldest overall.
// The caller MUST hold r.mu.
func (r *clientRegistry) evictOldestSession() {
	if len(r.sessions) <= maxResumeSessions {
		return
	}
	attached := r.attachedSessions()
	var oldestID string
	var oldest time.Time
	var oldestAnyID string
	var oldestAny time.Time
	for id, sx := range r.sessions {
		if oldestAnyID == "" || sx.lastSeen.Before(oldestAny) {
			oldestAnyID, oldestAny = id, sx.lastSeen
		}
		if attached[sx] {
			continue
		}
		if oldestID == "" || sx.lastSeen.Before(oldest) {
			oldestID, oldest = id, sx.lastSeen
		}
	}
	if oldestID == "" {
		oldestID = oldestAnyID // every session attached: cap still binds
	}
	if oldestID != "" {
		delete(r.sessions, oldestID)
	}
}

// AckSweepTargets returns the clients whose session's bytesReceived has
// advanced past the last ack actually written to them, and optimistically
// records the new value as sent. flushLoop calls it on ticks that produced
// no content frame, then writes a bare ackOnly frame to each target — so
// input into a silent app (no echo, no output) still acks within one tick.
// Optimistic recording is deliberate: a failed best-effort write costs one
// deferred trim (the next content frame, sweep advance, or resume re-syncs),
// never correctness, because the resume exchange is the authoritative sync.
func (r *clientRegistry) AckSweepTargets() map[*websocket.Conn]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var m map[*websocket.Conn]uint64
	for ws, state := range r.clients {
		sess := state.session.Load()
		if sess == nil {
			continue
		}
		if ack := sess.bytesReceived; ack != state.lastAckSent.Load() {
			if m == nil {
				m = make(map[*websocket.Conn]uint64)
			}
			m[ws] = ack
			state.lastAckSent.Store(ack)
		}
	}
	return m
}

// NoteAcksSent records the ack values a dispatched content frame carried to
// each client (via withClientAck), so the next AckSweepTargets pass does not
// send a redundant ackOnly for a value the client already received. The
// store is monotonic (forward-only): a generation-stripped durable write
// lands AFTER the resume batch that already recorded a newer resumeAck, and
// its older-but-genuinely-carried ack must not regress lastAckSent ("the
// highest ack this socket was told" — the invariant the sweep's dedupe
// reads). Legitimate backward re-syncs (ledger replacement) go through
// handleResume's unconditional Store, never through here.
func (r *clientRegistry) NoteAcksSent(acks map[*websocket.Conn]uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ws, ack := range acks {
		state, ok := r.clients[ws]
		if !ok {
			continue
		}
		for {
			old := state.lastAckSent.Load()
			if ack <= old || state.lastAckSent.CompareAndSwap(old, ack) {
				break
			}
		}
	}
}

// IncrementReceived adds n to the session's bytesReceived counter.
func (r *clientRegistry) IncrementReceived(state *clientState, n int) {
	if n <= 0 {
		return
	}
	if sess := state.session.Load(); sess != nil {
		r.mu.Lock()
		sess.bytesReceived += uint64(n)
		sess.lastSeen = time.Now()
		r.mu.Unlock()
	}
}
