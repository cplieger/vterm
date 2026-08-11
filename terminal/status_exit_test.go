package terminal

// Status-layer tests for how a session's END is reported: exited for an ordinary
// one, crashed for a failure.
//
// The boundary these tests pin (crashedExit owns the rule):
//
//   - exit status 0 -> exited;
//   - a non-zero exit status -> crashed;
//   - a terminating signal the program was not asked for -> crashed;
//   - anything the SERVER caused (Shutdown, and therefore SessionManager.Close
//     and the idle reaper) -> exited, whatever wait status the kill produced,
//     because reporting a routine shutdown as a crash paints a whole fleet red;
//   - SIGHUP -> exited, because a hangup means the terminal went away, and the
//     only thing that closes a session's PTY master is this engine.
//
// Real children throughout (sh -c 'exit 3', sh -c 'kill -9 $$'), so the tests
// exercise the actual cmd.Wait() error shape rather than a hand-built one: the
// classification reads *exec.ExitError and its wait status, and a mock would let
// a wrong assumption about either survive.

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// exitedHandler starts command in its own handler and waits for the process
// monitor to reap it, so ExitError is final before the caller classifies.
func exitedHandler(t *testing.T, command ...string) *Handler {
	t.Helper()
	h := NewHandler(command, WithWorkDir("/"), WithLogger(nil))
	t.Cleanup(h.Shutdown)
	if err := h.StartEager(); err != nil {
		t.Fatalf("StartEager(%v): %v", command, err)
	}
	waitExited(t, h)
	return h
}

// waitExited blocks until the child has been reaped, failing the test if it
// never is.
//
// Waits on the channel the process monitor closes rather than polling Exited(),
// for two reasons. It returns the instant the monitor latches, so a generous
// budget costs the happy path nothing — which matters because none of these
// tests assert anything about how FAST a reap is, only that the outcome is final
// before they classify it. And the old 5s poll was an arbitrary number that
// failed the suite for slowness it never meant to measure: it fired once under
// full-suite load, where a machine running the scheduler tests' real-time sleeps
// alongside dozens of child processes can starve this monitor goroutine well
// past 5s without anything being wrong.
//
// The failure message diagnoses instead of just reporting the timeout: it says
// whether the child is still running, already a zombie (so the exit happened and
// only the monitor is behind), or gone entirely.
func waitExited(t *testing.T, h *Handler) {
	t.Helper()
	const budget = 30 * time.Second
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-h.procExitCh:
		return
	case <-timer.C:
		t.Fatalf("child was not reaped within %v: %s", budget, childStateForDiag(h))
	}
}

// childStateForDiag describes the child's procfs state for a reap-timeout
// message, so the failure names which half stalled: the process not exiting, or
// the monitor not latching an exit that already happened.
func childStateForDiag(h *Handler) string {
	h.mu.Lock()
	cmd := h.cmd
	h.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return "no child process on the handler"
	}
	pid := cmd.Process.Pid
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return fmt.Sprintf("pid %d is gone from /proc, so it exited and the monitor never latched", pid)
	}
	// State is the field after the closing paren of the parenthesized comm,
	// which may itself contain spaces and parens (see alive()).
	s := string(b)
	i := len(s) - 1
	for ; i >= 0 && s[i] != ')'; i-- {
	}
	if i < 0 || i+2 >= len(s) {
		return fmt.Sprintf("pid %d has an unparseable /proc stat line", pid)
	}
	if s[i+2] == 'Z' {
		return fmt.Sprintf("pid %d is a zombie, so it exited and the monitor has not latched yet", pid)
	}
	return fmt.Sprintf("pid %d is still running in state %q", pid, s[i+2:i+3])
}

// TestExitOutcomeClassification walks the documented boundary with real
// children. Each case asserts the status the sweep would report, which is the
// contract a consumer's red dot is drawn from.
func TestExitOutcomeClassification(t *testing.T) {
	cases := []struct {
		name    string
		command []string
		want    string
	}{
		{name: "clean exit 0", command: []string{"/bin/sh", "-c", "exit 0"}, want: StatusExited},
		{name: "non-zero exit", command: []string{"/bin/sh", "-c", "exit 3"}, want: StatusCrashed},
		{name: "highest non-zero exit", command: []string{"/bin/sh", "-c", "exit 255"}, want: StatusCrashed},
		{name: "killed by SIGKILL", command: []string{"/bin/sh", "-c", "kill -9 $$"}, want: StatusCrashed},
		{name: "killed by SIGSEGV", command: []string{"/bin/sh", "-c", "kill -SEGV $$"}, want: StatusCrashed},
		// A hangup is the terminal going away, not the program failing.
		{name: "killed by SIGHUP", command: []string{"/bin/sh", "-c", "kill -HUP $$"}, want: StatusExited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewSessionManager(catFactory)
			t.Cleanup(m.Shutdown)
			h := exitedHandler(t, tc.command...)
			if st := m.computeStatusFromHandler(h, &statusTracker{}); st != tc.want {
				t.Errorf("%v: status = %q, want %q (ExitError = %v)", tc.command, st, tc.want, h.ExitError())
			}
		})
	}
}

// TestExitOutcomeCleanExitRetainsNoError pins the other half of the accessor's
// contract: a clean exit retains nil, so a consumer can tell "ended cleanly"
// from "ended badly" without parsing anything.
func TestExitOutcomeCleanExitRetainsNoError(t *testing.T) {
	h := exitedHandler(t, "/bin/sh", "-c", "exit 0")
	if err := h.ExitError(); err != nil {
		t.Errorf("ExitError() = %v, want nil after a clean exit", err)
	}
	if _, crashed := h.exitOutcome(); crashed {
		t.Error("exitOutcome() reported a crash for exit 0")
	}
}

// TestExitOutcomeNonZeroRetainsError is its mirror: a failed exit retains the
// wait error, which is what makes the crashed classification possible at all.
func TestExitOutcomeNonZeroRetainsError(t *testing.T) {
	h := exitedHandler(t, "/bin/sh", "-c", "exit 3")
	if h.ExitError() == nil {
		t.Fatal("ExitError() = nil, want the wait error after exit 3")
	}
	exited, crashed := h.exitOutcome()
	if !exited || !crashed {
		t.Errorf("exitOutcome() = (%v, %v), want (true, true)", exited, crashed)
	}
}

// TestExitOutcomeBeforeExitIsClean pins the pre-exit reading: a live session is
// not exited and carries no exit error, so a consumer polling the accessor never
// mistakes "still running" for "ended cleanly" as long as it checks Exited too.
func TestExitOutcomeBeforeExitIsClean(t *testing.T) {
	h := NewHandler([]string{"/bin/sh", "-c", "sleep 30"}, WithWorkDir("/"), WithLogger(nil))
	t.Cleanup(h.Shutdown)
	if err := h.StartEager(); err != nil {
		t.Fatalf("StartEager: %v", err)
	}
	exited, crashed := h.exitOutcome()
	if exited || crashed {
		t.Errorf("exitOutcome() = (%v, %v) for a live process, want (false, false)", exited, crashed)
	}
	if err := h.ExitError(); err != nil {
		t.Errorf("ExitError() = %v for a live process, want nil", err)
	}
}

// TestServerInitiatedShutdownIsExitedNotCrashed is the case the whole rule turns
// on, asserted deliberately: a session the SERVER ended reports exited, even
// though the child is killed and its wait status is signalled. The sleeping
// child never gets a chance to exit on its own, so every path here — the
// cancelled context's SIGKILL, the PTY-close SIGHUP — is server-caused. If this
// case ever reported crashed, one operator restart would light up every tab.
func TestServerInitiatedShutdownIsExitedNotCrashed(t *testing.T) {
	cases := []struct {
		name string
		// end terminates the session the way a particular server path does, and
		// returns the handler whose exit is then classified.
		end func(t *testing.T, m *SessionManager) *Handler
	}{
		{
			name: "handler Shutdown",
			end: func(t *testing.T, m *SessionManager) *Handler {
				t.Helper()
				h := sleepSession(t, m)
				h.Shutdown()
				return h
			},
		},
		{
			name: "SessionManager Close",
			end: func(t *testing.T, m *SessionManager) *Handler {
				t.Helper()
				id, err := m.Create()
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				h := handlerOf(t, m, id)
				requireLive(t, h)
				if !m.Close(id) {
					t.Fatal("Close reported the session was not found")
				}
				return h
			},
		},
		{
			name: "idle reaper",
			end: func(t *testing.T, m *SessionManager) *Handler {
				t.Helper()
				h := sleepSession(t, m)
				// Backdate the idle clock so the window has elapsed, then run the
				// reaper's own teardown pass.
				m.mu.Lock()
				m.idleSince = time.Now().Add(-time.Hour)
				m.mu.Unlock()
				m.maybeReap()
				return h
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The reaper is configured for every case (its loop ticks every 30s,
			// so it never fires inside the test) and only the reaper case above
			// drives a pass by hand.
			m := NewSessionManager(sleepFactory, WithIdleReaper(time.Minute))
			t.Cleanup(m.Shutdown)
			m.stopSweep()

			h := tc.end(t, m)
			waitExited(t, h)
			if st := m.computeStatusFromHandler(h, &statusTracker{}); st != StatusExited {
				t.Errorf("status = %q, want %q (a server-initiated end is not a crash; ExitError = %v)",
					st, StatusExited, h.ExitError())
			}
			if _, crashed := h.exitOutcome(); crashed {
				t.Errorf("exitOutcome() reported a crash; ExitError = %v", h.ExitError())
			}
		})
	}
}

// sleepFactory builds a session whose child outlives the test unless something
// kills it, so a shutdown path is the only way it can end.
func sleepFactory(string) *Handler {
	return NewHandler([]string{"/bin/sh", "-c", "sleep 30"}, WithWorkDir("/"), WithLogger(nil))
}

// sleepSession creates a manager session and returns its live handler. Create
// spawns the process eagerly, so the child is already running.
func sleepSession(t *testing.T, m *SessionManager) *Handler {
	t.Helper()
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)
	requireLive(t, h)
	return h
}

// requireLive fails the test unless h's child is still running, so a shutdown
// case is racing a live child rather than an already-dead handler.
func requireLive(t *testing.T, h *Handler) {
	t.Helper()
	if h.Exited() {
		t.Fatal("the long-lived child exited immediately; the shutdown case would prove nothing")
	}
}

// TestCrashedOutranksEveryProgressStateAndLatch pins the precedence rule the new
// status has to obey: a dead process's status is its exit, whatever the program's
// last progress state said and whatever the tracker had latched. The
// failed/warning progress states are in the table on purpose — they are the two
// statuses most easily confused with a crash, and a session that reported failed
// and then exited 0 is exited, not crashed.
func TestCrashedOutranksEveryProgressStateAndLatch(t *testing.T) {
	progressStates := []struct {
		name  string
		value int
	}{
		{name: "never reported", value: -1},
		{name: "cleared", value: 0},
		{name: "determinate", value: 1},
		{name: "error", value: 2},
		{name: "indeterminate", value: 3},
		{name: "warning", value: 4},
	}
	latches := []struct {
		name string
		set  string
	}{
		{name: "no latch", set: ""},
		{name: "input latched", set: StatusInput},
		{name: "done latched", set: StatusDone},
	}
	exits := []struct {
		name    string
		want    string
		crashed bool
	}{
		{name: "clean exit", crashed: false, want: StatusExited},
		{name: "crashed exit", crashed: true, want: StatusCrashed},
	}
	m := NewSessionManager(catFactory, WithStatusClassifier(inputClassifier))
	t.Cleanup(m.Shutdown)
	for _, ex := range exits {
		for _, ps := range progressStates {
			for _, la := range latches {
				t.Run(ex.name+"/"+ps.name+"/"+la.name, func(t *testing.T) {
					tr := &statusTracker{latched: la.set}
					in := &statusRaw{
						progress: ps.value, progressValue: -1,
						exited: true, crashed: ex.crashed,
					}
					if st := m.computeStatus(in, tr); st != ex.want {
						t.Errorf("status = %q, want %q (exit outranks progress %d and latch %q)",
							st, ex.want, ps.value, la.set)
					}
				})
			}
		}
	}
}

// TestListReportsCrashedForAFailedExit checks the point-in-time read agrees with
// the stream: List and the SSE initial sync go through refinedStatus, and the two
// sources MUST agree about HOW a session ended, or a reload silently downgrades a
// crashed tab to a plain exited one.
func TestListReportsCrashedForAFailedExit(t *testing.T) {
	m := NewSessionManager(func(string) *Handler {
		return NewHandler([]string{"/bin/sh", "-c", "exit 3"}, WithWorkDir("/"), WithLogger(nil))
	})
	t.Cleanup(m.Shutdown)
	m.stopSweep()

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := handlerOf(t, m, id)
	waitExited(t, h)

	found := false
	for _, info := range m.List() {
		if info.ID != id {
			continue
		}
		found = true
		if info.Status != StatusCrashed {
			t.Errorf("List status = %q, want %q", info.Status, StatusCrashed)
		}
	}
	if !found {
		t.Fatalf("session %s missing from List", id)
	}
	// The sweep's own view has to match it.
	if ev := eventFor(t, m.diffStatuses(), id); ev.Status != StatusCrashed {
		t.Errorf("sweep event status = %q, want %q", ev.Status, StatusCrashed)
	}
}
