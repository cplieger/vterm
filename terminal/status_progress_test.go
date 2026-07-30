package terminal

// Status-layer tests for OSC 9, written against the OSC 9 spec brief.
//
// Two structural claims from the brief drive everything here:
//
//   - Form A (OSC 9;4) reports a STATE. It persists until the program changes
//     it, so no timeout and no output activity may clear it. The brief's state
//     table maps 1 and 3 to an active (working) state, 2 to an error state and 4
//     to a warning state, read with iTerm2's semantics because the engine
//     advertises TERM_PROGRAM=iTerm.app.
//   - Form B (OSC 9;<message>) is an EVENT. It happens once. A consumer with no
//     status classifier installed — web-terminal-server's configuration — must
//     still RECEIVE it, because "no classifier" means "I map notifications
//     myself", not "discard them".
//
// Where these tests disagree with the current implementation, the brief wins.

import (
	"testing"
)

// eventFor returns the status event for id from one sweep's output. A sweep can
// legitimately emit events for other sessions (and none at all when nothing
// changed), so a test states which it needs.
func eventFor(t *testing.T, events []statusEvent, id string) statusEvent {
	t.Helper()
	for _, ev := range events {
		if ev.ID == id {
			return ev
		}
	}
	t.Fatalf("no status event for session %s in %d event(s)", id, len(events))
	return statusEvent{}
}

// TestComputeStatusProgressStateMapping walks the brief's state table through
// the status layer: an active state (1 value, 3 indeterminate) is working, the
// error state (2) is failed, the warning state (4) is warning, and a cleared or
// never-seen state is idle. No classifier is installed, because a progress state
// is the program's own report and needs no consumer mapping.
func TestComputeStatusProgressStateMapping(t *testing.T) {
	cases := []struct {
		name     string
		progress int
		want     string
	}{
		{name: "never reported", progress: -1, want: StatusIdle},
		{name: "state 0 cleared", progress: 0, want: StatusIdle},
		{name: "state 1 determinate value", progress: 1, want: StatusWorking},
		{name: "state 2 error", progress: 2, want: StatusFailed},
		{name: "state 3 indeterminate", progress: 3, want: StatusWorking},
		{name: "state 4 warning", progress: 4, want: StatusWarning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewSessionManager(catFactory) // no classifier: the generic consumer
			t.Cleanup(m.Shutdown)
			tr := &statusTracker{}
			in := &statusRaw{progress: tc.progress, progressValue: -1}
			if st := m.computeStatus(in, tr); st != tc.want {
				t.Errorf("progress %d: status = %q, want %q", tc.progress, st, tc.want)
			}
		})
	}
}

// TestComputeStatusFailedPersistsUntilProgressChanges pins the brief's "these
// are STATES, not events" claim at the status layer for the error state: once a
// program reports OSC 9;4;2 the session stays failed across every later sweep,
// through further output, and is cleared only by a NEW progress state from the
// program. A failed build that quietly reverts to idle a moment later is a
// missed failure.
//
// Note there is deliberately no elapsed-time assertion: computeStatus takes no
// clock input at all, so a timeout could only arrive as a new code path. The
// repeated sweeps below are what such a path would have to survive.
func TestComputeStatusFailedPersistsUntilProgressChanges(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)
	tr := &statusTracker{}

	h.handlePTYData([]byte("\x1b]9;4;2;30\x07"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusFailed {
		t.Fatalf("after OSC 9;4;2, status = %q, want %q", st, StatusFailed)
	}
	// Sweep after sweep with nothing new: the state is the program's, so it holds.
	for i := range 5 {
		if st := m.computeStatusFromHandler(h, tr); st != StatusFailed {
			t.Fatalf("sweep %d: status = %q, want %q (a state must not decay)", i+2, st, StatusFailed)
		}
	}
	// Mere output activity is not a progress report and must not clear it.
	h.handlePTYData([]byte("retrying the failed step...\r\n"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusFailed {
		t.Fatalf("after unrelated output, status = %q, want %q", st, StatusFailed)
	}
	// Only the program changes it: a new active state resumes working.
	h.handlePTYData([]byte("\x1b]9;4;3\x07"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusWorking {
		t.Fatalf("after OSC 9;4;3, status = %q, want %q", st, StatusWorking)
	}
	// And the clear state drops to idle.
	h.handlePTYData([]byte("\x1b]9;4;0\x07"))
	if st := m.computeStatusFromHandler(h, tr); st != StatusIdle {
		t.Fatalf("after OSC 9;4;0, status = %q, want %q", st, StatusIdle)
	}
}

// TestComputeStatusFreshNotificationOutranksProgressState asserts the subtle
// half of the precedence rule explicitly, because adding failed/warning is
// exactly the change that could break it: a notification classified in THIS
// sweep still outranks ANY progress-derived state, for the turn-boundary reason
// computeStatus documents (the notification flushes a chunk ahead of the
// progress update, so the snapshot pairs a fresh notification with a progress
// value that has not caught up; consuming the notification here would lose the
// latch for good).
func TestComputeStatusFreshNotificationOutranksProgressState(t *testing.T) {
	cases := []struct {
		name     string
		progress int
		msg      string
		want     string
	}{
		{name: "done over error state", progress: 2, msg: "Response complete", want: StatusDone},
		{name: "done over warning state", progress: 4, msg: "Response complete", want: StatusDone},
		{name: "needs-input over error state", progress: 2, msg: "Permission required", want: StatusInput},
		{name: "needs-input over warning state", progress: 4, msg: "Permission required", want: StatusInput},
		{name: "done over active progress", progress: 3, msg: "Response complete", want: StatusDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewSessionManager(catFactory, WithStatusClassifier(inputClassifier))
			t.Cleanup(m.Shutdown)
			tr := &statusTracker{}
			in := &statusRaw{progress: tc.progress, progressValue: -1, notifMsg: tc.msg, notifSeq: 1}
			if st := m.computeStatus(in, tr); st != tc.want {
				t.Errorf("fresh %q with progress %d: status = %q, want %q", tc.msg, tc.progress, st, tc.want)
			}
		})
	}
}

// TestComputeStatusNewProgressStateSupersedesStaleLatch is the other half of the
// precedence rule: a progress state reported in a LATER sweep than the latch is
// the program's current word and supersedes it. The existing rule already does
// this for the active states; the brief's error and warning states are states in
// exactly the same sense, so a stale "done" must not mask a build that has since
// failed.
func TestComputeStatusNewProgressStateSupersedesStaleLatch(t *testing.T) {
	cases := []struct {
		name     string
		progress int
		want     string
	}{
		{name: "error state supersedes stale done", progress: 2, want: StatusFailed},
		{name: "warning state supersedes stale done", progress: 4, want: StatusWarning},
		{name: "active state supersedes stale done", progress: 3, want: StatusWorking},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewSessionManager(catFactory, WithStatusClassifier(inputClassifier))
			t.Cleanup(m.Shutdown)
			tr := &statusTracker{}

			// Sweep 1: the turn ends, done latches.
			in := &statusRaw{progress: 0, progressValue: -1, notifMsg: "Response complete", notifSeq: 1}
			if st := m.computeStatus(in, tr); st != StatusDone {
				t.Fatalf("sweep 1: status = %q, want %q", st, StatusDone)
			}
			// Sweep 2: a new progress state, no new notification. The latch is
			// stale now and the program's state wins.
			in = &statusRaw{progress: tc.progress, progressValue: -1, notifMsg: "Response complete", notifSeq: 1}
			if st := m.computeStatus(in, tr); st != tc.want {
				t.Errorf("sweep 2 with progress %d: status = %q, want %q", tc.progress, st, tc.want)
			}
			if tr.latched != "" {
				t.Errorf("latch = %q, want cleared by the new progress state", tr.latched)
			}
		})
	}
}

// TestDiffStatusesDeliversNotificationWithoutClassifier is the generic gap. With
// NO classifier installed (web-terminal-server's configuration) an OSC 9
// notification is currently observed and thrown away: applyNotification returns
// early, nothing latches, and no event is emitted — the message never reaches a
// subscriber. Per the brief a notification is an EVENT, so the engine's job is to
// DELIVER it; deciding what it means is the consumer's. The event therefore
// carries the text and its sequence number, and delivering one latches no status.
func TestDiffStatusesDeliversNotificationWithoutClassifier(t *testing.T) {
	m := NewSessionManager(catFactory) // no classifier: the generic consumer
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)

	// Drain the first-sweep baseline (the new session's initial idle event).
	m.diffStatuses()

	h.handlePTYData([]byte("\x1b]9;Response complete\x07"))
	ev := eventFor(t, m.diffStatuses(), id)
	if ev.Notification != "Response complete" {
		t.Errorf("event Notification = %q, want %q (a notification must reach a subscriber "+
			"even with no classifier)", ev.Notification, "Response complete")
	}
	if ev.NotificationSeq != 1 {
		t.Errorf("event NotificationSeq = %d, want 1", ev.NotificationSeq)
	}
	// An event, not a state: nothing is latched, so the status is unchanged.
	if ev.Status != StatusIdle {
		t.Errorf("event Status = %q, want %q (a notification must not latch a status)", ev.Status, StatusIdle)
	}

	// A repeated message is a second event: the sequence advances, so a consumer
	// can tell two identical notifications apart.
	h.handlePTYData([]byte("\x1b]9;Response complete\x07"))
	ev = eventFor(t, m.diffStatuses(), id)
	if ev.Notification != "Response complete" || ev.NotificationSeq != 2 {
		t.Errorf("repeat: Notification = %q seq = %d, want %q seq = 2",
			ev.Notification, ev.NotificationSeq, "Response complete")
	}
}

// TestDiffStatusesCarriesProgressValue verifies the OSC 9;4 percentage reaches
// the status event, including its ABSENCE. Absence is -1, not 0: a session that
// has reported no percentage is not a session at 0%, and a consumer rendering a
// determinate bar has to be able to tell those apart.
func TestDiffStatusesCarriesProgressValue(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)

	// Absence: the session has reported nothing yet.
	ev := eventFor(t, m.diffStatuses(), id)
	if ev.ProgressValue != -1 {
		t.Errorf("event ProgressValue = %d, want -1 (absent, which is not 0%%)", ev.ProgressValue)
	}

	// A determinate value reaches the event alongside the working status.
	h.handlePTYData([]byte("\x1b]9;4;1;50\x07"))
	ev = eventFor(t, m.diffStatuses(), id)
	if ev.ProgressValue != 50 {
		t.Errorf("event ProgressValue = %d, want 50", ev.ProgressValue)
	}
	if ev.Status != StatusWorking {
		t.Errorf("event Status = %q, want %q", ev.Status, StatusWorking)
	}

	// The error state carries its percentage too (state 2 with pr).
	h.handlePTYData([]byte("\x1b]9;4;2;75\x07"))
	ev = eventFor(t, m.diffStatuses(), id)
	if ev.ProgressValue != 75 || ev.Status != StatusFailed {
		t.Errorf("event ProgressValue = %d status = %q, want 75 / %q", ev.ProgressValue, ev.Status, StatusFailed)
	}

	// Clearing progress returns the percentage to absent.
	h.handlePTYData([]byte("\x1b]9;4;0\x07"))
	ev = eventFor(t, m.diffStatuses(), id)
	if ev.ProgressValue != -1 {
		t.Errorf("after clear, event ProgressValue = %d, want -1", ev.ProgressValue)
	}
}

// TestDiffStatusesEmitsOnProgressValueChange verifies the percentage is part of
// the sweep's change detection: a program advancing 10% -> 60% within the same
// progress state changes nothing else about the session, so unless the value
// itself counts as a change the consumer's bar is stuck at the first value it
// ever saw and only moves when something unrelated happens to move.
func TestDiffStatusesEmitsOnProgressValueChange(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)

	h.handlePTYData([]byte("\x1b]9;4;1;10\x07"))
	if ev := eventFor(t, m.diffStatuses(), id); ev.ProgressValue != 10 {
		t.Fatalf("first value: event ProgressValue = %d, want 10", ev.ProgressValue)
	}
	// Same state, new percentage: status and titles are unmoved, so only the
	// value can trigger the event.
	h.handlePTYData([]byte("\x1b]9;4;1;60\x07"))
	if ev := eventFor(t, m.diffStatuses(), id); ev.ProgressValue != 60 {
		t.Errorf("advanced value: event ProgressValue = %d, want 60", ev.ProgressValue)
	}
}
