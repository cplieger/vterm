package terminal

// The status stream (Server-Sent Events at /api/sessions/events) drives each
// tab's activity indicator. A single sweep recomputes every session's status on
// a fixed interval and pushes only changes to subscribers, which debounces the
// working/idle flap for free. Status derives from process liveness, OSC 9;4
// progress, and output activity (working/idle/exited, or crashed when the
// process died badly); a consumer's classifier
// maps an OSC 9 notification to a latched needs-input or done state (Tier 2).
// One stream serves all tabs (not one per tab) to stay under the browser's
// per-origin connection cap.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
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
	// progressAbsent is the OSC 9;4 marker for "no percentage reported" —
	// vt.Screen.ProgressValue's absent value, mirrored here so the event path can
	// name it. Distinct from 0, which is a real 0%.
	progressAbsent = -1
)

// statusEvent is one status-stream message: a session's current status and
// title. Removed=true signals the session is gone (closed or reaped) so the
// client drops the tab.
type statusEvent struct {
	// Order is the session's position in the shared display order (see
	// SessionInfo.Order). Carried on every status event for a LIVE session, so a
	// reorder made by another client reaches this one, which is the read side of
	// tab-order sync.
	//
	// A pointer, and omitted on a Removed event, because the session has left the
	// order and has no position to report. A plain int could only say 0 there, and
	// 0 is a real position at the FRONT of the strip: a consumer that reads fields
	// before it checks Removed would be told the closing session just became the
	// first tab. Present-and-zero and absent are different answers, so the wire
	// has to be able to express both.
	//
	// This is the RAW rank, not a renumbered index. The two enumerations (List and
	// snapshot) restate each position as its index in the sequence they serve,
	// because a client builds a whole strip from them; a change set has no sequence
	// to index into, so it cannot. The consequence is the window a client must
	// respect: one reorder arrives as one event per moved session, so until the
	// whole tick is applied two sessions can hold the same position. Apply the
	// tick, then sort, and never derive an order you write back from a partly
	// applied view (see SetSessionOrder).
	//
	// First in the struct so the GC's pointer-scan range does not have to reach
	// past the scalar tail to find it (govet fieldalignment).
	Order     *int      `json:"order,omitempty"`
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
	PinnedTitle string `json:"pinnedTitle"`
	// Notification is the OSC 9 notification message captured for this session,
	// carried on the event so a consumer with NO status classifier still RECEIVES
	// it: "no classifier" means "I map notifications myself", not "discard them".
	// A notification is an EVENT, not a state — the engine latches nothing for it,
	// and it is carried only on the sweep that first observes it, never replayed.
	// NotificationSeq is vt.Screen.NotificationSeq, so a consumer detects a fresh
	// notification even when the text repeats.
	//
	// The text is untrusted program output. It is already sanitised by the vt
	// layer (control, bidi and line-terminator runes dropped, length clamped —
	// see vt.sanitizeNotification) and it travels to a client that already
	// receives the session's full terminal stream over the WebSocket, so carrying
	// it here is not a new exposure. A consumer still escapes it for whatever
	// surface it renders it on, exactly as it does the terminal stream.
	Notification    string `json:"notification,omitempty"`
	NotificationSeq uint64 `json:"notificationSeq,omitempty"`
	// ProgressValue is the OSC 9;4 percentage for this session: -1 when absent or
	// unknown, else 0-100 (see Handler.ProgressValue). Always carried — a session
	// that has reported no percentage is not a session at 0%, so the field must
	// not be omitempty. A change in this value alone emits an event, or a
	// consumer's determinate bar would stay at the first value it ever saw.
	ProgressValue   int  `json:"progressValue"`
	Removed         bool `json:"removed,omitempty"`
	ReportsActivity bool `json:"reportsActivity"`
}

// statusTracker holds the per-session state the status computation needs beyond
// the handler: the last emitted status/title/progress percentage (to detect
// changes), the last notification sequence classified and the last one
// delivered, the latched needs-input/done state, and the automatic-title
// confirmation window.
type statusTracker struct {
	candidateSince  time.Time // when candidatePGID was first observed
	lastStatus      string
	lastTitle       string
	lastClientTitle string // last emitted raw client title (to detect a title-only PUT)
	lastPinnedTitle string // last emitted raw pinned name (to detect a rename / clear)
	latched         string // "", StatusInput, or StatusDone
	notifSeen       uint64
	// notifDelivered is the last notification sequence CARRIED on an event.
	// Separate from notifSeen, which only advances when a classifier consumed the
	// message: delivery is unconditional, so the two would otherwise disagree for
	// a consumer with no classifier — the very consumer the delivery exists for.
	notifDelivered uint64
	candidatePGID  int // foreground pgid awaiting the confirmation window
	// lastProgressValue is the last emitted OSC 9;4 percentage. Zero-valued at 0
	// rather than -1 for a brand-new tracker, which only means the session's
	// first sweep also counts the percentage as changed — that sweep emits
	// anyway, because the status itself moves off "".
	lastProgressValue int
	// lastOrder is the last emitted display position. Zero-valued at 0 for a
	// brand-new tracker, same as lastProgressValue: that only means a session
	// already at position 0 does not count its position as changed on its first
	// sweep, which emits anyway because the status itself moves off "".
	lastOrder   int
	lastReports bool // last emitted reportsActivity (to detect a false->true flip)
}

// autoTitleConfirm is how long a foreground process must hold the terminal
// before its name is adopted as the session's automatic title. A command that
// lives 30ms (ls, git status) must never flash into a tab label.
//
// It is elapsed time, not a sample count: "the same pgid on two consecutive
// sweeps" would be only statusSweepInterval apart and therefore phase-sensitive,
// adopting a 260ms command or missing a 400ms one depending on where the ticks
// landed. Adoption therefore lands within one sweep past this window, whatever
// statusSweepInterval happens to be — do not restate the arithmetic in terms of a
// particular tick count, which rots the moment the interval changes.
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
			// Indexed, not ranged: a statusEvent is well past the value-copy
			// threshold, and broadcast takes a pointer anyway.
			evs := m.diffStatuses()
			for i := range evs {
				m.broadcast(&evs[i])
			}
		}
	}
}

// statusRaw carries one session's handler-derived status inputs, read in
// diffStatuses's lock-free phase (each getter takes only that handler's h.mu).
type statusRaw struct {
	createdAt    time.Time
	handler      *Handler
	tr           *statusTracker
	id           string
	notifMsg     string
	oscTitle     string
	derivedTitle string
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
	// order is the session's position in the shared display order. Unlike every
	// other field here it is filled in PHASE 3, not phase 1: see the comment at
	// that site. A reorder changes no handler state, so this is the only input that
	// makes the sweep emit for it.
	order    int
	progress int
	// progressValue is the OSC 9;4 percentage from the SAME snapshot as progress
	// (-1 when absent or unknown, else 0-100).
	progressValue int
	exited        bool
	// crashed is meaningful only when exited: true when the exit was a failure
	// rather than an ordinary end (see crashedExit).
	crashed bool
}

// read fills the handler-derived fields (diffStatuses phase 2). It takes only
// the handler's own locks — never the manager's. The screen-derived fields
// (progress, notification, title) come from ONE lock acquisition
// (statusSnapshot), so a PTY chunk parsed mid-read cannot pair a stale active
// progress with a fresh turn-end notification (the torn snapshot that lost a
// done latch).
func (it *statusRaw) read() {
	it.exited, it.crashed = it.handler.exitOutcome()
	sc := it.handler.statusSnapshot()
	it.progress = sc.progress
	it.progressValue = sc.progressValue
	it.notifMsg = sc.notifMsg
	it.notifSeq = sc.notifSeq
	it.oscTitle = sc.title
	it.derivedTitle = sc.derivedTitle
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
	if hold := testDiffPhaseHold.Load(); hold != nil {
		(*hold)()
	}

	// Phase 3: tracker state machine + change detection under m.mu.
	var events []statusEvent
	m.mu.Lock()
	// The display order is read HERE, not in phase 1, and that placement is the
	// whole point. It is manager-owned state, so a Close or a SetSessionOrder
	// during phase 2 (which runs lock-free, and can block on a wedged handler
	// getter) invalidates anything phase 1 captured. Emitting a position from that
	// stale snapshot pushes clients an arrangement the server has already replaced
	// — silently, because the tracker then records the stale value as delivered and
	// the next sweep sees no change to correct. Reading it under this lock, in the
	// same section that decides what to emit, is what makes an emitted position
	// current by construction.
	rank := m.rankLocked()
	ordered := len(m.order)
	for i := range items {
		it := &items[i]
		s, live := m.sessions[it.id]
		if !live {
			continue // closed while computing; the removed sweep below emits it
		}
		it.order = rankOf(rank, it.id, ordered)
		if ev, changed := m.sweepSession(s, it); changed {
			events = append(events, ev)
		}
	}
	for id, tr := range m.trackers {
		if _, ok := m.sessions[id]; !ok {
			delete(m.trackers, id)
			events = append(events, statusEvent{
				ID: id, Status: StatusExited, Title: tr.lastTitle, Removed: true,
				ReportsActivity: tr.lastReports, ProgressValue: progressAbsent,
			})
		}
	}
	m.mu.Unlock()
	return events
}

// testDiffPhaseHold, when non-nil, is invoked by diffStatuses between phase 2 and
// phase 3. Test-only (session_order_test.go): phase 2 runs lock-free and can block
// on a wedged handler getter, so that gap is where manager state legitimately
// changes underneath a sweep in flight, and holding the sweep open at exactly that
// instant is the only way to drive the case deterministically. Atomic for the same
// reason as testResumeBatchHold. Never set in production.
var testDiffPhaseHold atomic.Pointer[func()]

// sweepSession runs one session's tracker state machine and change detection
// (diffStatuses phase 3), returning the event to broadcast and whether anything
// changed. Callers hold m.mu: it reads the session's stored titles and mutates
// the tracker.
func (m *SessionManager) sweepSession(s *session, it *statusRaw) (statusEvent, bool) {
	tr := it.tr
	// A notification the tracker has not delivered yet. Read BEFORE
	// computeStatus, which advances the separate classifier cursor.
	notifNew := it.notifSeq > tr.notifDelivered
	status := m.computeStatus(it, tr)
	// The stored titles are re-read under m.mu — a PUT during phase 2 must not
	// be masked by the phase-1 capture.
	clientTitle := s.clientTitle
	pinnedTitle := s.pinnedTitle
	// The sweep is the ONLY writer of the server-derived automatic title, so
	// List and snapshot read one confirmed value instead of each probing
	// procfs and disagreeing with this stream.
	m.confirmAutoTitle(s, it, tr)
	title := effectiveTitle(&titleSources{
		pinned: pinnedTitle, derived: it.derivedTitle, osc: it.oscTitle,
		client: clientTitle, auto: s.autoTitle,
	})
	// reportsActivity is sticky: progress stays >= 0 once any OSC 9;4 has been
	// seen (state 0 is "cleared", not "never seen" = -1), and a latched
	// notification is the other genuine OSC 9 signal. The client reveals the
	// tab's activity dot only when this is set.
	reports := it.progress >= 0 || tr.latched != ""
	// Emit on a raw client-title or pinned-name change too: a PUT can change
	// only one of those (OSC title and status unchanged), and a consumer
	// reading them directly needs that pushed even when the effective title is
	// unmoved (a pin or an OSC title masking the rung below). Without the
	// pinned guard, a rename would not reach a second browser watching the
	// same session until something else changed. The progress percentage is in
	// here for the same reason: 10% -> 60% moves nothing else about the session,
	// so without it a consumer's determinate bar would stay where it started.
	// The display position is in here for the same reason and is the ONLY signal a
	// reorder produces: SetSessionOrder touches no handler and no title, so
	// without this the client that made the change would be the only one to see
	// it. A close shifts every later position down one and so emits for each of
	// those sessions, which is bounded by the tab count and costs one small frame
	// each.
	// A fresh notification always emits, since delivering the event IS the point.
	if status == tr.lastStatus && title == tr.lastTitle && clientTitle == tr.lastClientTitle &&
		pinnedTitle == tr.lastPinnedTitle && reports == tr.lastReports &&
		it.progressValue == tr.lastProgressValue && it.order == tr.lastOrder && !notifNew {
		return statusEvent{}, false
	}
	tr.lastStatus = status
	tr.lastTitle = title
	tr.lastClientTitle = clientTitle
	tr.lastPinnedTitle = pinnedTitle
	tr.lastReports = reports
	tr.lastProgressValue = it.progressValue
	tr.lastOrder = it.order
	// A copy, not &it.order: it points into diffStatuses' phase-1 slice, and an
	// event that outlives the sweep must not alias the sweep's own scratch state.
	pos := it.order
	ev := statusEvent{
		ID: it.id, Status: status, Title: title, ClientTitle: clientTitle,
		PinnedTitle: pinnedTitle, CreatedAt: it.createdAt, ReportsActivity: reports,
		ProgressValue: it.progressValue, Order: &pos,
	}
	// A notification rides along on the sweep that first observes it, and only
	// that sweep: it is an event, so replaying it on a later status-only change
	// would show the consumer the same notification twice.
	if notifNew {
		tr.notifDelivered = it.notifSeq
		ev.Notification = it.notifMsg
		ev.NotificationSeq = it.notifSeq
	}
	return ev, true
}

// computeStatus derives a session's status from the handler inputs captured in
// diffStatuses's lock-free phase. Callers hold m.mu (it mutates the tracker's
// latch state, which snapshot() reads under m.mu).
//
// Precedence, highest first:
//
//  1. the process is gone, nothing else matters — exited for an ordinary end,
//     crashed for a failed one (crashedExit draws the line);
//  2. a notification classified in THIS sweep (a fresh latch), which outranks
//     ANY progress state for the turn-boundary reason below;
//  3. the program's current OSC 9;4 progress state — working (1 value, 3
//     indeterminate), failed (2 error), or warning (4, iTerm2 semantics) — which
//     supersedes a latch from an EARLIER sweep and clears it, unless the state is
//     one that does not contradict the latch (progressSupersedesLatch);
//  4. a latch from an earlier sweep (needs-input or done), still current because
//     the program has reported no progress state that contradicts it;
//  5. idle — the default / new-session / at-rest state, which is also where a
//     cleared progress state (0) and a never-reported one (-1) land.
//
// Working comes ONLY from OSC 9;4 progress — never from raw output activity — so
// a program that reports no progress never flaps to working merely because the
// user is typing at its prompt (the reveal gate then keeps its dot hidden).
func (m *SessionManager) computeStatus(in *statusRaw, tr *statusTracker) string {
	if in.exited {
		// Top precedence, and the split lives here rather than in the caller so
		// every status consumer (sweep, List, SSE snapshot) reads one rule: a
		// dead session's LAST progress state or latch says nothing useful about
		// how it died.
		if in.crashed {
			return StatusCrashed
		}
		return StatusExited
	}
	latchedNow := m.applyNotification(in, tr)
	// A progress-reporting program (kiro-cli, Claude Code) drives its status from
	// its OSC 9;4 progress: an active state (1 value, 3 indeterminate) means the
	// agent is working, and the error and warning states are states in exactly
	// the same sense — they persist until the program itself changes them, rather
	// than being events that fire once.
	// A progress state supersedes a latch from an EARLIER sweep (a new turn
	// starting clears the stale done/input dot) — but never one applied in THIS
	// sweep: at a turn boundary the snapshot can pair the fresh "Response
	// complete" / "Permission required" notification with a progress value that
	// still reads active (the notification flushed a chunk ahead of the
	// progress-off), and clearing the just-set latch here would consume the
	// notification for good (notifSeen has advanced) — the next sweep then lands
	// on idle and the tab's done/input dot is silently lost. Honoring the fresh
	// latch is self-correcting: if the agent truly is still working, the next
	// sweep's progress state supersedes the now-stale latch — provided it is one
	// that CONTRADICTS the latch, which is progressSupersedesLatch's job.
	if st, ok := progressStatus(in.progress); ok && !latchedNow &&
		(tr.latched == "" || progressSupersedesLatch(in.progress)) {
		tr.latched = ""
		return st
	}
	// A latched notification state (needs-input or done) persists through the
	// quiet gap after the turn until a progress state that contradicts it arrives.
	if tr.latched != "" {
		return tr.latched
	}
	return StatusIdle
}

// progressStatus maps an OSC 9;4 progress state to the status it reports, and
// whether it reports one at all. State 0 (cleared) and -1 (never reported) carry
// no status of their own: they leave the decision to the latch and to idle.
func progressStatus(progress int) (string, bool) {
	switch progress {
	case 1, 3: // determinate value / indeterminate: the program is working
		return StatusWorking, true
	case 2: // error state (with a percentage, or indeterminate without one)
		return StatusFailed, true
	case 4: // iTerm2's warning at pr percent (ConEmu calls the same state paused)
		return StatusWarning, true
	}
	return "", false
}

// progressSupersedesLatch reports whether an OSC 9;4 progress state CONTRADICTS a
// latched notification status, which is what earns it the right to clear one.
// Only ever asked about a state progressStatus accepts.
//
// States 1 and 3 assert that an operation is under way, which contradicts both
// latches: "the turn finished" and "blocked, waiting on you" both say nothing is
// running. State 2 asserts a failure, an OUTCOME that must not stay masked by a
// stale success — a build that has since failed outranks an earlier "done".
//
// State 4 is the one that does not contradict either, and the specs are explicit
// about why: it is Windows' TBPF_PAUSED, "progress is currently stopped ... but
// can be resumed by the user. No error condition exists". That is not a rival
// claim to a needs-input latch, it is the SAME claim with less detail, and it is
// equally compatible with a finished turn, where nothing is progressing. Treating
// it as a rival (it was, by symmetry with state 2, until this was measured) hands
// the tab to the progress channel one sweep — 250ms — after the notification.
// kiro-cli makes that failure routine rather than theoretical: it sets state 4
// BECAUSE it is blocked on an approval, and parks state 4 at its context-usage
// percentage once idle, so the needs-input ring survived a quarter second of a
// minutes-long wait and the green done dot the same fraction of an idle period —
// the latter only above its 60% context threshold, which is what made the loss
// look intermittent. A latch this state cannot clear is not stuck: the moment the
// program resumes (1 or 3), fails (2), or dies, that supersedes it.
func progressSupersedesLatch(progress int) bool {
	return progress != 4
}

// applyNotification updates the tracker's latch from a new OSC 9 notification
// via the consumer's classifier, if any, and reports whether this call latched
// a state — so computeStatus can give a notification classified in the current
// sweep precedence over a concurrently captured active progress value. The
// classified state (StatusInput or StatusDone) persists until a later sweep
// reports a progress state that CONTRADICTS it (see computeStatus). An
// unclassified message leaves the latch unchanged.
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
// new subscriber receives, in compareSessionOrder, via the same refinedStatus
// read List serves (the two sources must agree, on order as well as on status;
// see refinedStatus and compareSessionOrder).
//
// Two-phase like diffStatuses and List: manager state under m.mu, handler
// getters after it is released. The screen-derived inputs come from ONE
// statusSnapshot call per handler, so the initial sync cannot pair one session's
// progress state with a percentage read a moment later. No notification is
// replayed here: a notification is an event, and a subscriber joining later did
// not miss a state.
func (m *SessionManager) snapshot() []statusEvent {
	type snapItem struct {
		handler    *Handler
		lastStatus string
		autoTitle  string
		ev         statusEvent
		rank       int
		latched    bool
	}
	m.mu.Lock()
	rank := m.rankLocked()
	ordered := len(m.order)
	items := make([]snapItem, 0, len(m.sessions))
	for id, s := range m.sessions {
		tr := m.trackers[id]
		it := snapItem{
			rank: rankOf(rank, id, ordered),
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

	out := make([]snapItem, 0, len(items))
	for i := range items {
		it := &items[i]
		it.ev.Status = refinedStatus(it.lastStatus, it.handler)
		sc := it.handler.statusSnapshot()
		it.ev.Title = effectiveTitle(&titleSources{
			pinned: it.ev.PinnedTitle, derived: sc.derivedTitle, osc: sc.title,
			client: it.ev.ClientTitle, auto: it.autoTitle,
		})
		it.ev.ProgressValue = sc.progressValue
		it.ev.ReportsActivity = sc.progress >= 0 || it.latched
		out = append(out, *it)
	}
	// Same order List serves, for the same reason it sorts at all: this is an
	// ENUMERATION of the session set, and a client building a tab strip from it
	// reads the sequence as the strip's order. Phase 1 ranged m.sessions, so
	// without this the stream pushed a fresh random order on every connect.
	//
	// diffStatuses is deliberately NOT sorted this way: it returns a CHANGE SET,
	// its removed events carry a zero createdAt, and a per-tick burst of
	// independent state updates has no enumeration order to get right.
	slices.SortFunc(out, func(a, b snapItem) int {
		return compareSessionOrder(
			sessionOrder{createdAt: a.ev.CreatedAt, id: a.ev.ID, rank: a.rank},
			sessionOrder{createdAt: b.ev.CreatedAt, id: b.ev.ID, rank: b.rank},
		)
	})
	// The published position is the index in THIS sequence, not the raw rank, so
	// the field a client sorts by is 0-based, dense and unique whatever the
	// manager's internal state happens to be. Deriving it from the rank map
	// instead would let two sessions the order does not name (an invariant
	// violation, hence unreachable — but the wire contract should not depend on
	// that) both claim the same position.
	evs := make([]statusEvent, 0, len(out))
	for i := range out {
		pos := i
		out[i].ev.Order = &pos
		evs = append(evs, out[i].ev)
	}
	return evs
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
		snap := m.snapshot()
		for i := range snap {
			if !writeSSE(w, &snap[i]) {
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
//
// no-cache is the conventional SSE directive but it only forces revalidation — it
// still permits a cache to STORE the response, and every event on this stream
// carries a full session id (statusEvent.ID), the same capability token the REST
// responses now refuse to let a cache keep. no-store is therefore carried
// alongside it: the conventional token stays for any middlebox that sniffs for it,
// and the stronger prohibition is the one that matches what the body contains.
func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-store")
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
