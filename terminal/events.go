package terminal

// The status stream (Server-Sent Events at /api/sessions/events) drives each
// tab's activity indicator. A single sweep recomputes every session's status on
// a fixed interval and pushes only changes to subscribers, which debounces the
// working/idle flap for free. Status derives from process liveness, OSC 9;4
// progress, and output activity (working/idle/exited); a consumer's classifier
// maps an OSC 9 notification to a latched needs-input or done state (Tier 2).
// One stream serves all tabs (not one per tab) to stay under the browser's
// per-origin connection cap.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	// maxSubscribers is the fixed ceiling on concurrent status-stream (SSE)
	// subscribers; it bounds subscriber goroutines/buffers and stops runaway
	// connections. A few devices per deployment is the expected load.
	maxSubscribers = 10
	// statusSweepInterval is how often per-session status is recomputed.
	statusSweepInterval = 250 * time.Millisecond
	// subscriberBuffer bounds a subscriber's pending events; a subscriber that
	// falls this far behind is dropped rather than blocking the sweep.
	subscriberBuffer = 64
	// sseKeepAlive is the idle interval between SSE keepalive comments, so
	// proxies do not close a quiet stream.
	sseKeepAlive = 15 * time.Second
	// sseWriteTimeout bounds each SSE write so a wedged subscriber (client
	// socket dead but not yet FIN'd) is detected in seconds instead of waiting
	// for the OS TCP timeout. Mirrors the WS per-client write deadline in
	// dispatchFrame. 10s is far above a healthy client's sub-ms flush of a
	// small SSE frame and below the 15s keepalive, so a dead client is caught
	// before the next keepalive fires; a healthy-but-slow client is unaffected.
	sseWriteTimeout = 10 * time.Second
)

// statusEvent is one status-stream message: a session's current status and
// title. Removed=true signals the session is gone (closed or reaped) so the
// client drops the tab.
type statusEvent struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Title     string    `json:"title"` // resolved display title (effectiveTitle)
	// ClientTitle is the raw stored client-derived title, carried alongside Title
	// so a consumer can read the pushed label directly (bypassing the precedence
	// in Title). Not carried on a Removed event.
	ClientTitle string `json:"clientTitle"`
	// PinnedTitle is the raw user-set name, carried for the same reason and so a
	// second client learns of a rename (or its removal) made elsewhere. Not
	// carried on a Removed event.
	PinnedTitle     string `json:"pinnedTitle"`
	Removed         bool   `json:"removed,omitempty"`
	ReportsActivity bool   `json:"reportsActivity"`
}

// statusTracker holds the per-session state the status computation needs beyond
// the handler: the last emitted status/title (to detect changes), the last
// notification sequence classified, the latched needs-input/done state, and the
// automatic-title confirmation window.
type statusTracker struct {
	candidateSince  time.Time // when candidatePGID was first observed
	lastStatus      string
	lastTitle       string
	lastClientTitle string // last emitted raw client title (to detect a title-only PUT)
	lastPinnedTitle string // last emitted raw pinned name (to detect a rename / clear)
	latched         string // "", StatusInput, or StatusDone
	notifSeen       uint64
	candidatePGID   int  // foreground pgid awaiting the confirmation window
	lastReports     bool // last emitted reportsActivity (to detect a false->true flip)
}

// autoTitleConfirm is how long a foreground process must hold the terminal
// before its name is adopted as the session's automatic title. A command that
// lives 30ms (ls, git status) must never flash into a tab label.
//
// It is elapsed time, not a sample count: "the same pgid on two consecutive
// sweeps" would be only statusSweepInterval apart and therefore phase-sensitive,
// adopting a 260ms command or missing a 400ms one depending on where the ticks
// landed. At a 250ms sweep this is the third consecutive observation, so adoption
// lands 500-750ms after the command starts.
//
// 500ms is chosen, not inherited. tmux's NAME_INTERVAL is also 500ms but is a
// different mechanism — a rate limit on how often it recomputes a window name,
// which is why tmux CAN briefly show a short-lived command. This is a
// confirmation window, which cannot.
const autoTitleConfirm = 500 * time.Millisecond

// confirmAutoTitle folds one sweep's probe into the session's confirmed
// server-derived title. The sweep is the ONLY writer of session.autoTitle, so
// List and snapshot read a single confirmed value instead of each probing procfs
// and disagreeing with the live stream. Called from diffStatuses phase 3, with
// m.mu held (it writes session and tracker state).
//
// The asymmetry is deliberate. Adopting a running process is debounced by
// autoTitleConfirm; RESTING (the process finished, so the shell is in the
// foreground again) is immediate, because a stale name is worse than a brief
// correct one. While a candidate is inside its confirmation window the previous
// title is HELD rather than reset to the cwd, so `vim` giving way to `less` does
// not detour through the directory name.
func (m *SessionManager) confirmAutoTitle(s *session, in *statusRaw, tr *statusTracker) {
	p := in.autoProbe
	if !p.ok {
		return // no information this sweep (OSC-titled, exited, unsupported platform)
	}
	if p.procName != "" {
		if tr.candidatePGID != p.pgid {
			tr.candidatePGID = p.pgid
			tr.candidateSince = time.Now()
			return // window restarts; hold the current title
		}
		if time.Since(tr.candidateSince) >= autoTitleConfirm {
			s.autoTitle = p.procName
		}
		return // hold while the window runs
	}
	// Nothing is running: the shell owns the terminal again. Rest at the cwd
	// basename, or keep the seeded command basename when the cwd is unreadable.
	tr.candidatePGID = 0
	tr.candidateSince = time.Time{}
	if p.cwdBase != "" {
		s.autoTitle = p.cwdBase
	}
}

func (m *SessionManager) sweepLoop(ctx context.Context) {
	t := time.NewTicker(statusSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, ev := range m.diffStatuses() {
				m.broadcast(&ev)
			}
		}
	}
}

// statusRaw carries one session's handler-derived status inputs, read in
// diffStatuses's lock-free phase (each getter takes only that handler's h.mu).
type statusRaw struct {
	createdAt time.Time
	handler   *Handler
	tr        *statusTracker
	id        string
	notifMsg  string
	oscTitle  string
	// autoProbe is the foreground-process probe result for this sweep: the
	// candidate pgid plus the name/cwd it resolves to. Read in phase 2 (procfs +
	// one ioctl, no locks held) and folded into the confirmation window in
	// phase 3. Zero when no probe ran this sweep: an OSC-titled session (the
	// automatic title is the LAST rung, so it must not pay for procfs reads) or an
	// exited one.
	//
	// Placed after the strings and before the scalars deliberately: its own
	// trailing non-pointer tail (pgid, ok) then falls outside the struct's
	// pointer-scan range (govet fieldalignment). Adding a field here without
	// keeping pointer-bearing types ahead of scalar ones re-trips that linter.
	autoProbe autoTitleProbe
	notifSeq  uint64
	progress  int
	exited    bool
}

// read fills the handler-derived fields (diffStatuses phase 2). It takes only
// the handler's own locks — never the manager's. The screen-derived fields
// (progress, notification, title) come from ONE lock acquisition
// (statusSnapshot), so a PTY chunk parsed mid-read cannot pair a stale active
// progress with a fresh turn-end notification (the torn snapshot that lost a
// done latch).
func (it *statusRaw) read() {
	it.exited = it.handler.Exited()
	it.progress, it.notifMsg, it.notifSeq, it.oscTitle = it.handler.statusSnapshot()
	// Skip the probe entirely when the program named itself: the automatic title
	// is the LAST rung, so an OSC-titled session must never pay for procfs reads.
	if it.oscTitle == "" && !it.exited {
		it.autoProbe = it.handler.probeAutoTitle()
	}
}

// diffStatuses recomputes every session's status and returns the events for
// sessions whose status, effective title, raw client title, raw pinned name, or
// reported activity changed since the last sweep, plus removed events for
// sessions that vanished. Broadcasting happens outside the lock (see sweepLoop).
// It is also the sole writer of each session's server-derived automatic title
// (see confirmAutoTitle).
//
// It runs in three phases so the manager lock is never held across handler
// getters: each getter takes that handler's h.mu, and one wedged handler under
// m.mu would stall every manager path (List, create/close, snapshot) for as
// long as the handler stays stuck — 4×/s, forever. Phase 1 snapshots the
// session set under m.mu; phase 2 reads each handler's inputs with no manager
// lock (a stuck handler now stalls only the sweep goroutine); phase 3 re-takes
// m.mu to run the tracker state machine and change detection (snapshot() reads
// tracker fields under m.mu, so mutating them outside would race). A session
// closed between phases is skipped in phase 3 and emits its removed event in
// the same sweep; one added between phases is picked up next sweep (250ms).
func (m *SessionManager) diffStatuses() []statusEvent {
	// Phase 1: snapshot sessions + tracker refs under m.mu. No handler calls.
	m.mu.Lock()
	items := make([]statusRaw, 0, len(m.sessions))
	for id, s := range m.sessions {
		tr := m.trackers[id]
		if tr == nil {
			tr = &statusTracker{}
			m.trackers[id] = tr
		}
		items = append(items, statusRaw{id: id, createdAt: s.createdAt, handler: s.handler, tr: tr})
	}
	m.mu.Unlock()

	// Phase 2: read handler inputs lock-free (per-handler h.mu only).
	for i := range items {
		items[i].read()
	}

	// Phase 3: tracker state machine + change detection under m.mu.
	var events []statusEvent
	m.mu.Lock()
	for i := range items {
		it := &items[i]
		s, live := m.sessions[it.id]
		if !live {
			continue // closed while computing; the removed sweep below emits it
		}
		tr := it.tr
		status := m.computeStatus(it, tr)
		// The stored titles are re-read under m.mu — a PUT during phase 2 must not
		// be masked by the phase-1 capture.
		clientTitle := s.clientTitle
		pinnedTitle := s.pinnedTitle
		// The sweep is the ONLY writer of the server-derived automatic title, so
		// List and snapshot read one confirmed value instead of each probing
		// procfs and disagreeing with this stream.
		m.confirmAutoTitle(s, it, tr)
		title := effectiveTitle(pinnedTitle, it.oscTitle, clientTitle, s.autoTitle)
		// reportsActivity is sticky: Progress() stays >= 0 once any OSC 9;4 has
		// been seen (state 0 is "cleared", not "never seen" = -1), and a latched
		// notification is the other genuine OSC 9 signal. The client reveals the
		// tab's activity dot only when this is set.
		reports := it.progress >= 0 || tr.latched != ""
		// Emit on a raw client-title or pinned-name change too: a PUT can change
		// only one of those (OSC title and status unchanged), and a consumer
		// reading them directly needs that pushed even when the effective title is
		// unmoved (a pin or an OSC title masking the rung below). Without the
		// pinned guard, a rename would not reach a second browser watching the
		// same session until something else changed.
		if status != tr.lastStatus || title != tr.lastTitle || clientTitle != tr.lastClientTitle ||
			pinnedTitle != tr.lastPinnedTitle || reports != tr.lastReports {
			tr.lastStatus = status
			tr.lastTitle = title
			tr.lastClientTitle = clientTitle
			tr.lastPinnedTitle = pinnedTitle
			tr.lastReports = reports
			events = append(events, statusEvent{
				ID: it.id, Status: status, Title: title, ClientTitle: clientTitle,
				PinnedTitle: pinnedTitle, CreatedAt: it.createdAt, ReportsActivity: reports,
			})
		}
	}
	for id, tr := range m.trackers {
		if _, ok := m.sessions[id]; !ok {
			delete(m.trackers, id)
			events = append(events, statusEvent{ID: id, Status: StatusExited, Title: tr.lastTitle, Removed: true, ReportsActivity: tr.lastReports})
		}
	}
	m.mu.Unlock()
	return events
}

// computeStatus derives a session's status from the handler inputs captured in
// diffStatuses's lock-free phase. Callers hold m.mu (it mutates the tracker's
// latch state, which snapshot() reads under m.mu). Precedence: exited, then a
// notification classified in THIS sweep (a fresh latch), then working (OSC 9;4
// progress active), then a latch from an earlier sweep, then idle (the
// default / new-session / at-rest state).
// Working comes ONLY from OSC 9;4 progress — never from raw output activity — so
// a program that reports no progress never flaps to working merely because the
// user is typing at its prompt (the reveal gate then keeps its dot hidden).
func (m *SessionManager) computeStatus(in *statusRaw, tr *statusTracker) string {
	if in.exited {
		return StatusExited
	}
	latchedNow := m.applyNotification(in, tr)
	// A progress-reporting program (kiro-cli, Claude Code) drives working from
	// its OSC 9;4 progress: an active state (1 value, 3 indeterminate) means the
	// agent is working. A new working phase supersedes a latch from an EARLIER
	// sweep (a new turn starting clears the stale done/input dot) — but never
	// one applied in THIS sweep: at a turn boundary the snapshot can pair the
	// fresh "Response complete" / "Permission required" notification with a
	// progress value that still reads active (the notification flushed a chunk
	// ahead of the progress-off), and clearing the just-set latch here would
	// consume the notification for good (notifSeen has advanced) — the next
	// sweep then lands on idle and the tab's done/input dot is silently lost.
	// Honoring the fresh latch is self-correcting: if the agent truly is still
	// working, the next sweep has no new notification and its active progress
	// clears the latch then.
	if (in.progress == 1 || in.progress == 3) && !latchedNow {
		tr.latched = ""
		return StatusWorking
	}
	// A latched notification state (needs-input or done) persists through the
	// quiet gap after the turn until the next working phase clears it.
	if tr.latched != "" {
		return tr.latched
	}
	return StatusIdle
}

// applyNotification updates the tracker's latch from a new OSC 9 notification
// via the consumer's classifier, if any, and reports whether this call latched
// a state — so computeStatus can give a notification classified in the current
// sweep precedence over a concurrently captured active progress value. The
// classified state (StatusInput or StatusDone) persists until a LATER sweep's
// working phase clears it (see computeStatus). An unclassified message leaves
// the latch unchanged.
func (m *SessionManager) applyNotification(in *statusRaw, tr *statusTracker) bool {
	if m.classifier == nil {
		return false
	}
	if in.notifSeq <= tr.notifSeen {
		return false
	}
	tr.notifSeen = in.notifSeq
	if cls, ok := m.classifier(in.notifMsg); ok {
		tr.latched = cls
		return true
	}
	return false
}

func (m *SessionManager) broadcast(ev *statusEvent) {
	m.subsMu.Lock()
	for ch := range m.subs {
		select {
		case ch <- *ev:
		default:
			// Subscriber is too far behind; drop it. The hub owns closing the
			// channel; the handler goroutine sees !ok and unsubscribes (a no-op
			// once already removed here).
			m.logger.Warn("terminal: status subscriber dropped (buffer full)", "buffer", subscriberBuffer)
			delete(m.subs, ch)
			close(ch)
		}
	}
	m.subsMu.Unlock()
}

func (m *SessionManager) subscribe() (chan statusEvent, bool) {
	// The subscriber cap is a fixed const (maxSubscribers): a small ceiling that
	// bounds runaway subscriber goroutines/buffers while leaving safe headroom
	// for several devices per deployment. The count and the compared ceiling are
	// both known under subsMu (the lock guarding the subscriber map), so no other
	// lock is involved.
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	if len(m.subs) >= maxSubscribers {
		return nil, false
	}
	ch := make(chan statusEvent, subscriberBuffer)
	m.subs[ch] = struct{}{}
	return ch, true
}

func (m *SessionManager) unsubscribe(ch chan statusEvent) {
	m.subsMu.Lock()
	if _, ok := m.subs[ch]; ok {
		delete(m.subs, ch)
		close(ch)
	}
	m.subsMu.Unlock()
}

// snapshot returns the current status of every session for the initial sync a
// new subscriber receives, via the same refinedStatus read List serves (the two
// sources must agree; see refinedStatus).
//
// Two-phase like diffStatuses and List: manager state under m.mu, handler
// getters (Exited/Progress/Title, each taking h.mu) after it is released.
func (m *SessionManager) snapshot() []statusEvent {
	type snapItem struct {
		handler    *Handler
		lastStatus string
		autoTitle  string
		ev         statusEvent
		latched    bool
	}
	m.mu.Lock()
	items := make([]snapItem, 0, len(m.sessions))
	for id, s := range m.sessions {
		tr := m.trackers[id]
		it := snapItem{
			ev: statusEvent{
				ID: id, ClientTitle: s.clientTitle, PinnedTitle: s.pinnedTitle,
				CreatedAt: s.createdAt,
			},
			handler:   s.handler,
			autoTitle: s.autoTitle,
		}
		if tr != nil {
			it.lastStatus = tr.lastStatus
			it.latched = tr.latched != ""
		}
		items = append(items, it)
	}
	m.mu.Unlock()

	out := make([]statusEvent, 0, len(items))
	for i := range items {
		it := &items[i]
		it.ev.Status = refinedStatus(it.lastStatus, it.handler)
		it.ev.Title = effectiveTitle(it.ev.PinnedTitle, it.handler.Title(), it.ev.ClientTitle, it.autoTitle)
		it.ev.ReportsActivity = it.handler.Progress() >= 0 || it.latched
		out = append(out, it.ev)
	}
	return out
}

// EventsHandler serves the status stream at SessionEventsPath
// (/api/sessions/events, SSE). A subscriber is counted as a present client
// (suppressing the idle reaper) and first receives an initial sync of every
// session's current status, then a stream of changes. A subscriber that falls
// behind its bounded buffer is dropped; the number of concurrent subscribers
// is bounded by the fixed maxSubscribers cap. Mounted for you by
// MountSessionRoutes / MountAPI; exported so consumer tests can stub it.
func (m *SessionManager) EventsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Flush through a ResponseController so the stream works behind
		// middleware that wraps the ResponseWriter (an access log, security
		// headers): unlike a direct w.(http.Flusher) assertion, it follows the
		// Unwrap chain. Probe support up front with the same chain walk so we can
		// 500 before committing a status — a probe Flush() before the headers are
		// written would implicitly send a 200 and drop the SSE headers below.
		if !supportsFlush(w) {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		rc := http.NewResponseController(w)
		// Subscribe before the snapshot so a change during the snapshot is
		// queued (delivered after it) rather than missed.
		ch, ok := m.subscribe()
		if !ok {
			m.logger.Warn("terminal: status subscriber rejected (at cap)", "max_subscribers", maxSubscribers)
			http.Error(w, "too many subscribers", http.StatusServiceUnavailable)
			return
		}
		defer m.unsubscribe(ch)
		m.clientConnected()
		defer m.clientDisconnected()

		writeSSEHeaders(w)
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) // bound the snapshot burst too
		for _, ev := range m.snapshot() {
			if !writeSSE(w, &ev) {
				return
			}
		}
		_ = rc.Flush()
		streamEvents(r.Context(), w, rc, ch)
	})
}

// supportsFlush reports whether w, or any ResponseWriter it unwraps to, supports
// flushing. It walks the Unwrap chain the way http.ResponseController does, so
// the SSE stream works behind a ResponseWriter-wrapping middleware whose wrapper
// implements Unwrap() (e.g. an access-log recorder). It is an upfront probe
// because a real Flush() before the headers are written would implicitly commit
// a 200 and drop the event-stream headers.
func supportsFlush(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}

// writeSSEHeaders sets the SSE response headers and the 200 status.
func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // ask proxies not to buffer the stream
	w.WriteHeader(http.StatusOK)
}

// streamEvents forwards status events and periodic keepalives to one subscriber
// until the client disconnects (ctx done) or the subscriber is dropped (channel
// closed by the hub for falling behind).
func streamEvents(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, ch <-chan statusEvent) {
	keep := time.NewTicker(sseKeepAlive)
	defer keep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keep.C:
			if !writeKeepAlive(w, rc) {
				return
			}
		case ev, ok := <-ch:
			if !ok || !writeSSEFlush(w, rc, &ev) {
				return
			}
		}
	}
}

// writeKeepAlive emits an SSE keepalive comment and flushes, returning false if
// the client connection is gone (so the stream loop exits).
func writeKeepAlive(w http.ResponseWriter, rc *http.ResponseController) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) // unsupported writer degrades to no deadline (prior behavior)
	if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// writeSSEFlush writes one event frame and flushes, returning false if the
// client connection is gone.
func writeSSEFlush(w http.ResponseWriter, rc *http.ResponseController, ev *statusEvent) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) // unsupported writer degrades to no deadline (prior behavior)
	if !writeSSE(w, ev) {
		return false
	}
	return rc.Flush() == nil
}

// writeSSE writes one event as an SSE data frame. Returns false if the client
// connection is gone (write failed). A malformed event is skipped, not fatal.
func writeSSE(w http.ResponseWriter, ev *statusEvent) bool {
	b, err := json.Marshal(ev)
	if err != nil {
		return true
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err == nil
}
