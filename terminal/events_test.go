package terminal

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// inputClassifier maps the two kiro-cli notification strings the way web-terminal-kiro
// does, for exercising the latched needs-input/done state machine.
func inputClassifier(msg string) (string, bool) {
	switch msg {
	case "Permission required":
		return StatusInput, true
	case "Response complete":
		return StatusDone, true
	}
	return "", false
}

// computeStatusFromHandler adapts the diffStatuses split for the state-machine
// tests: capture h's status inputs (the production phase-2 read) and run the
// tracker state machine on them, as one call.
func (m *SessionManager) computeStatusFromHandler(h *Handler, tr *statusTracker) string {
	in := statusRaw{handler: h}
	in.read()
	return m.computeStatus(&in, tr)
}

func handlerOf(t *testing.T, m *SessionManager, id string) *Handler {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		t.Fatalf("session %s not found", id)
	}
	return s.handler
}

// TestComputeStatusLatchesInput verifies a classified needs-input notification
// latches input and persists (the process is blocked, so no progress and no
// output), then an active progress signal (the turn resuming) clears it.
func TestComputeStatusLatchesInput(t *testing.T) {
	m := NewSessionManager(catFactory, WithStatusClassifier(inputClassifier))
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)
	tr := &statusTracker{}

	// A prompt followed by a needs-input notification: latched input.
	h.handlePTYData([]byte("Allow? \x1b]9;Permission required\x07"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusInput {
		t.Fatalf("after notification, status = %q, want %q", st, StatusInput)
	}
	// No resume: the latch persists across sweeps.
	if st := m.computeStatusFromHandler(h, tr); st != StatusInput {
		t.Fatalf("latch did not persist, status = %q, want %q", st, StatusInput)
	}
	// The turn resumes: an active progress signal (OSC 9;4;3) clears the latch
	// and reports working.
	h.handlePTYData([]byte("\x1b]9;4;3\x07"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusWorking {
		t.Fatalf("after resume progress, status = %q, want %q", st, StatusWorking)
	}
}

// TestComputeStatusDoneSupersedesInput verifies a classified done notification
// ("Response complete") replaces an input latch with the done state.
func TestComputeStatusDoneSupersedesInput(t *testing.T) {
	m := NewSessionManager(catFactory, WithStatusClassifier(inputClassifier))
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)
	tr := &statusTracker{}

	h.handlePTYData([]byte("\x1b]9;Permission required\x07"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusInput {
		t.Fatalf("precondition: status = %q, want %q", st, StatusInput)
	}
	h.handlePTYData([]byte("\x1b]9;Response complete\x07"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusDone {
		t.Fatalf("done did not supersede input latch; status = %q, want %q", st, StatusDone)
	}
}

// TestComputeStatusWorkingFromProgress verifies an active OSC 9;4 progress state
// (3 indeterminate) reports working, and clearing it (0) drops to idle when
// nothing is latched.
func TestComputeStatusWorkingFromProgress(t *testing.T) {
	m := NewSessionManager(catFactory, WithStatusClassifier(inputClassifier))
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)
	tr := &statusTracker{}

	h.handlePTYData([]byte("\x1b]9;4;3\x07"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusWorking {
		t.Fatalf("progress 3: status = %q, want %q", st, StatusWorking)
	}
	h.handlePTYData([]byte("\x1b]9;4;0\x07"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusIdle {
		t.Fatalf("progress 0 with no latch: status = %q, want %q", st, StatusIdle)
	}
}

// TestComputeStatusDoneLatchPersistsThenClears verifies "Response complete"
// latches done through the quiet gap, and the next working progress clears it.
func TestComputeStatusDoneLatchPersistsThenClears(t *testing.T) {
	m := NewSessionManager(catFactory, WithStatusClassifier(inputClassifier))
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)
	tr := &statusTracker{}

	h.handlePTYData([]byte("\x1b]9;4;0\x07\x1b]9;Response complete\x07"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusDone {
		t.Fatalf("after done notification: status = %q, want %q", st, StatusDone)
	}
	// Persists across a quiet sweep (no progress, no output-driven flip).
	if st := m.computeStatusFromHandler(h, tr); st != StatusDone {
		t.Fatalf("done latch did not persist: status = %q, want %q", st, StatusDone)
	}
	// Next turn starts working, clearing the done latch.
	h.handlePTYData([]byte("\x1b]9;4;3\x07"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusWorking {
		t.Fatalf("next working phase: status = %q, want %q", st, StatusWorking)
	}
}

// TestComputeStatusFreshLatchOutranksActiveProgress pins the turn-boundary
// race fix: when one sweep's snapshot pairs a NEW classified notification with
// a progress value that still reads active (kiro-cli flushes "Response
// complete" a chunk ahead of the OSC 9;4;0 progress-off, and a sweep tick
// lands in the gap), the fresh latch wins — done/input, not working — and
// persists once progress clears. The old code consumed the notification and
// destroyed the just-set latch in the same call, landing every later sweep on
// idle: the intermittently blank (hollow) tab dot at turn end. The poisoned
// pairing cannot be produced through handlePTYData (a chunk parses atomically
// under h.mu), so the snapshot is constructed directly.
func TestComputeStatusFreshLatchOutranksActiveProgress(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{name: "turn-end done", msg: "Response complete", want: StatusDone},
		{name: "needs-input", msg: "Permission required", want: StatusInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewSessionManager(catFactory, WithStatusClassifier(inputClassifier))
			t.Cleanup(m.Shutdown)
			tr := &statusTracker{}

			// Sweep N: the poisoned snapshot — fresh notification, progress
			// still reading active.
			in := &statusRaw{progress: 3, notifMsg: tc.msg, notifSeq: 1}
			if st := m.computeStatus(in, tr); st != tc.want {
				t.Errorf("poisoned snapshot: status = %q, want %q", st, tc.want)
			}
			// Sweep N+1: progress cleared, nothing new — the latch persists
			// (the old code landed here on idle).
			in = &statusRaw{progress: 0, notifMsg: tc.msg, notifSeq: 1}
			if st := m.computeStatus(in, tr); st != tc.want {
				t.Errorf("after progress clears: status = %q, want %q", st, tc.want)
			}
		})
	}
}

// TestComputeStatusStaleLatchStillClearedByWorking verifies the fresh-latch
// precedence lasts one sweep only (self-correcting): when progress remains
// active on the NEXT sweep with no new notification — the agent genuinely kept
// working, e.g. a queued follow-up turn — the now-stale latch clears and the
// session reports working, exactly as before the fix.
func TestComputeStatusStaleLatchStillClearedByWorking(t *testing.T) {
	m := NewSessionManager(catFactory, WithStatusClassifier(inputClassifier))
	t.Cleanup(m.Shutdown)
	tr := &statusTracker{}

	in := &statusRaw{progress: 3, notifMsg: "Response complete", notifSeq: 1}
	if st := m.computeStatus(in, tr); st != StatusDone {
		t.Fatalf("poisoned snapshot: status = %q, want %q", st, StatusDone)
	}
	// Same active progress, no new notification: the latch is stale now and
	// the genuine working phase supersedes it.
	if st := m.computeStatus(in, tr); st != StatusWorking {
		t.Fatalf("next sweep: status = %q, want %q", st, StatusWorking)
	}
	if tr.latched != "" {
		t.Fatalf("latch = %q, want cleared", tr.latched)
	}
	// That working phase later ends with no notification replay: idle is
	// correct (in production the queued turn's own end brings its own
	// "Response complete" with a new sequence).
	in = &statusRaw{progress: 0, notifMsg: "Response complete", notifSeq: 1}
	if st := m.computeStatus(in, tr); st != StatusIdle {
		t.Fatalf("after the working phase ends: status = %q, want %q", st, StatusIdle)
	}
}

// TestComputeStatusNoWorkingFromOutput verifies a program that never reports
// OSC 9;4 progress stays idle even while producing output: working now comes
// ONLY from OSC 9;4, so a plain shell under web-terminal-server never flaps to
// working merely because it (or the user typing at the prompt) produced bytes.
// The reveal gate then keeps such a session's tab dot hidden.
func TestComputeStatusNoWorkingFromOutput(t *testing.T) {
	m := NewSessionManager(catFactory) // no classifier: a generic shell
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)
	tr := &statusTracker{}

	h.handlePTYData([]byte("some output"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusIdle {
		t.Fatalf("output with no OSC 9;4 progress: status = %q, want %q (no output-activity fallback)", st, StatusIdle)
	}
}

// TestReportsActivitySticky verifies the reportsActivity flag the client uses to
// reveal the per-tab activity dot: false until an OSC 9;4 signal appears, then
// sticky true even after the progress is cleared (Progress stays >= 0), so the
// dot stays revealed while the session idles rather than flickering away.
func TestReportsActivitySticky(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)

	reports := func() bool {
		for _, info := range m.List() {
			if info.ID == id {
				return info.ReportsActivity
			}
		}
		t.Fatalf("session %s not in List()", id)
		return false
	}

	if reports() {
		t.Fatalf("fresh session (no OSC 9;4) reportsActivity = true, want false")
	}
	h.handlePTYData([]byte("\x1b]9;4;3\x07")) // active progress
	if !reports() {
		t.Fatalf("after OSC 9;4;3 reportsActivity = false, want true")
	}
	h.handlePTYData([]byte("\x1b]9;4;0\x07")) // clear progress
	if !reports() {
		t.Fatalf("after clearing progress reportsActivity = false, want true (sticky: Progress stays >= 0)")
	}
}

// TestListCarriesRefinedStatus is the reload regression guard: GET
// /api/sessions must report the same refined status the SSE stream pushes.
// List used to compute only coarse liveness (idle/exited) and ignored the
// tracker, so a page reload — whose stream-open reconcile GETs the list and
// repaints every tab dot from it — visibly downgraded a latched done/input dot
// back to the hollow idle state until the next real status transition.
func TestListCarriesRefinedStatus(t *testing.T) {
	m := NewSessionManager(catFactory, WithStatusClassifier(inputClassifier))
	t.Cleanup(m.Shutdown)
	m.stopSweep() // drive the sweep by hand, deterministically

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	statusInList := func() string {
		t.Helper()
		for _, info := range m.List() {
			if info.ID == id {
				return info.Status
			}
		}
		t.Fatalf("session %s not in List()", id)
		return ""
	}

	// Before any sweep has computed a status: the coarse new-session default.
	if got := statusInList(); got != StatusIdle {
		t.Fatalf("fresh session List status = %q, want %q", got, StatusIdle)
	}

	// A turn completes: progress clears, the done notification latches, and one
	// sweep records it. The list must then agree with what the stream pushes.
	handlerOf(t, m, id).handlePTYData([]byte("\x1b]9;4;0\x07\x1b]9;Response complete\x07"))
	_ = m.diffStatuses()
	if got := statusInList(); got != StatusDone {
		t.Fatalf("after swept done notification, List status = %q, want %q", got, StatusDone)
	}

	// The next turn starts working: the latch clears and the list follows.
	handlerOf(t, m, id).handlePTYData([]byte("\x1b]9;4;3\x07"))
	_ = m.diffStatuses()
	if got := statusInList(); got != StatusWorking {
		t.Fatalf("after resume progress, List status = %q, want %q", got, StatusWorking)
	}
}

// TestListExitedWinsOverStaleSweptStatus verifies live process exit outranks
// the sweep's last computed status in List: a session whose process died since
// the last sweep reports exited immediately, never an up-to-a-tick-stale
// done/working.
func TestListExitedWinsOverStaleSweptStatus(t *testing.T) {
	m := NewSessionManager(catFactory, WithStatusClassifier(inputClassifier))
	t.Cleanup(m.Shutdown)
	m.stopSweep()

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)
	h.handlePTYData([]byte("\x1b]9;Response complete\x07"))
	_ = m.diffStatuses() // the sweep records done

	// The process dies with no further sweep: exit must win over the stale done.
	h.Shutdown()
	waitExited(t, h)
	for _, info := range m.List() {
		if info.ID == id && info.Status != StatusExited {
			t.Fatalf("List status = %q, want %q (exit outranks the stale swept status)", info.Status, StatusExited)
		}
	}
}

// TestComputeStatusExited verifies an exited process reports exited regardless
// of activity or latch.
func TestComputeStatusExited(t *testing.T) {
	m := NewSessionManager(func(string) *Handler {
		return NewHandler([]string{"/bin/true"}, WithLogger(nil))
	})
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)
	waitExited(t, h)
	if st := m.computeStatusFromHandler(h, &statusTracker{}); st != StatusExited {
		t.Fatalf("status = %q, want %q", st, StatusExited)
	}
}

// TestEventsHandlerInitialSync verifies a new SSE subscriber immediately
// receives the current status of existing sessions (initial sync).
func TestEventsHandlerInitialSync(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	srv := httptest.NewServer(m.EventsHandler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") && strings.Contains(line, id) {
			return // initial sync delivered the session
		}
	}
	t.Fatalf("initial sync did not include session %s (scan err: %v)", id, sc.Err())
}

// TestEventsHandlerSubscriberCap verifies the fixed subscriber cap: exactly
// maxSubscribers concurrent status-stream (SSE) subscribers are admitted, and
// the next one is rejected with HTTP 503. Driven deterministically — the
// background sweep is stopped so its broadcasts cannot drop an unread
// subscriber mid-test, the maxSubscribers slots are filled by driving
// subscribe() directly (no HTTP, no sleeps), and the over-cap request goes
// through EventsHandler to assert the real 503. It references the const (not a
// literal 10) so it tracks maxSubscribers if the cap ever changes.
func TestEventsHandlerSubscriberCap(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	m.stopSweep() // deterministic: no background broadcast racing on m.subs

	// Fill exactly maxSubscribers slots; each must be admitted.
	for i := range maxSubscribers {
		if _, ok := m.subscribe(); !ok {
			t.Fatalf("subscriber %d/%d rejected below the cap; want admitted", i+1, maxSubscribers)
		}
	}

	// The cap is full (len(subs) == maxSubscribers): the next subscribe is
	// rejected, and EventsHandler surfaces that as HTTP 503.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events", nil)
	m.EventsHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("over-cap request status = %d, want %d (too many subscribers)", rec.Code, http.StatusServiceUnavailable)
	}
}

// unwrapOnlyWriter mimics an access-log middleware wrapper (web-terminal-server's
// statusWriter, web-terminal-kiro's statusRecorder): it exposes the underlying
// ResponseWriter via Unwrap but does NOT itself implement http.Flusher
// (embedding the ResponseWriter interface does not promote Flush). It pins that
// EventsHandler finds the flusher through the Unwrap chain rather than a direct
// type assertion, which is what makes SSE work behind middleware.
type unwrapOnlyWriter struct {
	http.ResponseWriter
}

func (u unwrapOnlyWriter) Unwrap() http.ResponseWriter { return u.ResponseWriter }

// noFlushWriter is a bare ResponseWriter that supports neither Flush nor Unwrap.
type noFlushWriter struct{}

func (noFlushWriter) Header() http.Header         { return http.Header{} }
func (noFlushWriter) Write(b []byte) (int, error) { return len(b), nil }
func (noFlushWriter) WriteHeader(int)             {}

// TestSupportsFlush pins the Unwrap-chain walk: a direct flusher is supported, a
// wrapper that only implements Unwrap to a flusher is supported (the case a
// direct w.(http.Flusher) assertion misses), and a writer with neither is not.
func TestSupportsFlush(t *testing.T) {
	rec := httptest.NewRecorder() // implements http.Flusher directly
	if !supportsFlush(rec) {
		t.Error("httptest.ResponseRecorder should support flush directly")
	}
	if !supportsFlush(unwrapOnlyWriter{rec}) {
		t.Error("a wrapper that Unwraps to a flusher must be reported as supporting flush")
	}
	if supportsFlush(noFlushWriter{}) {
		t.Error("a writer with neither Flush nor Unwrap must not be reported as supporting flush")
	}
}

// TestEventsHandlerFlushesBehindMiddleware is the regression guard for the SSE
// status stream served behind a ResponseWriter-wrapping middleware. The engine
// wraps every request in an access-log recorder that implements Unwrap but not
// Flush; a direct w.(http.Flusher) assertion would fail there and 500. This
// serves EventsHandler behind exactly such a wrapper and asserts the stream
// opens (200 + text/event-stream) and delivers the initial sync.
func TestEventsHandlerFlushesBehindMiddleware(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.EventsHandler().ServeHTTP(unwrapOnlyWriter{w}, r)
	})
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a direct flusher assertion would 500 here)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data:") && strings.Contains(line, id) {
			return // stream flushed the initial sync through the middleware
		}
	}
	t.Fatalf("SSE stream delivered no data behind middleware (scan err: %v)", sc.Err())
}

// stopSweep cancels the background status sweep so a test can drive
// diffStatuses deterministically without the 250ms ticker racing on the shared
// trackers. Safe to call once before the first tick; Shutdown then finds
// sweepCancel already nil.
func (m *SessionManager) stopSweep() {
	m.mu.Lock()
	if m.sweepCancel != nil {
		m.sweepCancel()
		m.sweepCancel = nil
	}
	m.mu.Unlock()
}

// TestDiffStatusesEmitsClientTitleChange verifies the status sweep detects a
// change to the raw client title even when the OSC-derived effective title,
// status, and reported activity are all unchanged: a PUT /title on a session
// whose program already emits an OSC window title moves only clientTitle, and a
// consumer that reads clientTitle directly must still get that pushed. It also
// pins that the emitted event carries ClientTitle (the raw label) alongside the
// unchanged OSC-derived Title.
func TestDiffStatusesEmitsClientTitleChange(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	// Drive diffStatuses by hand: stop the background sweep (it has not ticked
	// yet — its first tick is 250ms out) so it cannot race on m.trackers between
	// the change and the assertion.
	m.stopSweep()

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// An OSC window title fixes the effective title; a later client-title change
	// then moves ONLY clientTitle (OSC-first keeps Title == "osc label").
	handlerOf(t, m, id).handlePTYData([]byte("\x1b]2;osc label\x07"))

	findEvent := func(events []statusEvent) (statusEvent, bool) {
		t.Helper()
		for _, ev := range events {
			if ev.ID == id {
				return ev, true
			}
		}
		return statusEvent{}, false
	}

	// First sweep establishes the tracker baseline (Title="osc label",
	// ClientTitle="").
	base, ok := findEvent(m.diffStatuses())
	if !ok {
		t.Fatalf("baseline sweep emitted no event for session %s", id)
	}
	if base.Title != "osc label" || base.ClientTitle != "" {
		t.Fatalf("baseline event = {Title:%q ClientTitle:%q}, want {\"osc label\", \"\"}", base.Title, base.ClientTitle)
	}

	// A quiescent sweep (nothing changed) emits nothing for the session, so the
	// next emit is attributable to the client-title change alone.
	if ev, ok := findEvent(m.diffStatuses()); ok {
		t.Fatalf("quiescent sweep unexpectedly emitted an event: %+v", ev)
	}

	// A client-title-only change: OSC title, status, and activity all unchanged.
	if !m.SetSessionTitle(id, "msg") {
		t.Fatal("SetSessionTitle(known id) = false, want true")
	}
	ev, ok := findEvent(m.diffStatuses())
	if !ok {
		t.Fatal("sweep after client-title-only change emitted no event; the clientTitle change was not detected")
	}
	if ev.ClientTitle != "msg" {
		t.Fatalf("event ClientTitle = %q, want %q", ev.ClientTitle, "msg")
	}
	if ev.Title != "osc label" {
		t.Fatalf("event Title = %q, want %q (OSC-derived title must be unchanged)", ev.Title, "osc label")
	}
}

// TestBroadcastDropsSlowSubscriber pins the non-blocking fan-out: a subscriber
// whose bounded buffer overflows is dropped (removed from m.subs and its channel
// closed) instead of blocking the sweep. Without the select-default drop in
// broadcast, the overflowing send would block this goroutine forever.
func TestBroadcastDropsSlowSubscriber(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	m.stopSweep() // no background sweep broadcasts racing on m.subs

	ch, ok := m.subscribe()
	if !ok {
		t.Fatal("subscribe = false, want a fresh subscriber channel")
	}

	ev := statusEvent{ID: "s", Status: StatusIdle}
	// Exactly subscriberBuffer sends fill the buffer without overflowing.
	for range subscriberBuffer {
		m.broadcast(&ev)
	}
	m.subsMu.Lock()
	_, present := m.subs[ch]
	m.subsMu.Unlock()
	if !present {
		t.Fatal("subscriber dropped before overflow; want still present after exactly subscriberBuffer sends")
	}

	// One more send overflows: the select default branch drops and closes it.
	m.broadcast(&ev)
	m.subsMu.Lock()
	_, present = m.subs[ch]
	m.subsMu.Unlock()
	if present {
		t.Fatal("subscriber not dropped after buffer overflow; want removed from m.subs")
	}

	// The dropped channel is closed: drain the buffered events, then a receive
	// reports closed (the signal the handler goroutine uses to unsubscribe).
	for range subscriberBuffer {
		<-ch
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel not closed after drop; want closed once buffered events drained")
	}
}

// TestDiffStatusesWedgedHandlerDoesNotStallManager pins the lock split in
// diffStatuses / List / snapshot: a handler stuck under its own h.mu (a wedged
// PTY callback, a stalled classifier input) stalls only the paths that must
// read THAT handler's state (the sweep, a List over it) — never the manager
// lock itself. Pre-split, those paths held m.mu across the handler getters
// 4×/s, so one stuck handler froze every m.mu path (create, close, title PUT,
// WS attach, SSE subscribe) for as long as it stayed stuck.
func TestDiffStatusesWedgedHandlerDoesNotStallManager(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)

	// Wedge the handler: hold its h.mu so every status getter blocks. Both the
	// sweep and a List() must genuinely block on it (they read handler state).
	h.mu.Lock()
	unwedged := false
	defer func() {
		if !unwedged {
			h.mu.Unlock()
		}
	}()
	sweepDone := make(chan struct{})
	listDone := make(chan struct{})
	go func() {
		_ = m.diffStatuses() // blocks in phase 2 on the wedged handler
		close(sweepDone)
	}()
	go func() {
		_ = m.List() // blocks in its handler-read phase on the wedged handler
		close(listDone)
	}()
	select {
	case <-sweepDone:
		t.Fatal("diffStatuses returned while the handler was wedged; the test lost its premise")
	case <-listDone:
		t.Fatal("List returned while the handler was wedged; the test lost its premise")
	case <-time.After(50 * time.Millisecond):
	}

	// The manager lock must stay available while both are stuck: a pure-m.mu
	// path (the title PUT) completes promptly.
	titleDone := make(chan struct{})
	go func() {
		m.SetSessionTitle(id, "still responsive")
		close(titleDone)
	}()
	select {
	case <-titleDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SetSessionTitle blocked while a handler was wedged: a handler getter is being called under m.mu again")
	}

	// Unwedge; both stuck paths complete.
	h.mu.Unlock()
	unwedged = true
	for name, ch := range map[string]chan struct{}{"diffStatuses": sweepDone, "List": listDone} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not finish after the handler was released", name)
		}
	}
}
