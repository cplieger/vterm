//go:build linux

package terminal

// Tests for zombie reaping.
//
// One deliberate omission: nothing here calls sweepZombies or StartZombieReaper
// against the live process. A sweep collects EVERY orphan zombie parented on this
// process that the engine did not spawn, and in a test binary that set includes
// the not-yet-waited children of every sibling test — the containment and reap
// tests spawn real processes with exec.Command and reap them by hand. A global
// sweep here would steal their statuses and fail them at random. The pieces are
// tested individually, and the property that actually matters (the sweep never
// touches a pid os/exec owns) is asserted on reapIfUnowned, which is the guard
// the sweep is built out of.

import (
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// registered reports whether pid is currently in the os/exec-owned registry.
func registered(pid int) bool {
	spawned.mu.RLock()
	defer spawned.mu.RUnlock()
	_, ok := spawned.m[pid]
	return ok
}

// The whole safety property of the feature: a pid whose status os/exec still owns
// must never be waited on by the reaper, or cmd.Wait returns "no child
// processes" and the engine's exit contract (ExitError, the exited/crashed split,
// onProcessExit) silently reports nothing for every session.
func TestReapIfUnownedRefusesAnExecOwnedPid(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	spawnLock()
	spawnRegister(pid)
	spawnUnlock()
	t.Cleanup(func() { spawnForget(pid) })

	if !registered(pid) {
		t.Fatal("spawnRegister did not record the pid")
	}
	if reapIfUnowned(pid) {
		t.Fatalf("the reaper collected pid %d while os/exec still owned it", pid)
	}

	// And once os/exec is done with it, the exclusion lifts.
	spawnForget(pid)
	if registered(pid) {
		t.Fatal("spawnForget did not release the pid")
	}
}

// A live session's pid must be registered for as long as its monitor holds
// cmd.Wait, and released afterwards. This is the wiring the guard above depends
// on, asserted through the real spawn path rather than by calling the registry
// directly.
func TestSessionPidIsRegisteredWhileExecOwnsItAndReleasedAfter(t *testing.T) {
	t.Parallel()
	exited := make(chan error, 1)
	h := NewHandler([]string{"/bin/sh", "-c", "exit 7"},
		WithWorkDir("/"),
		WithLogger(nil),
		WithOnProcessExit(func(err error) { exited <- err }),
	)
	if err := h.ensureStarted(80, 24); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}
	defer h.Close()
	pid := h.cmd.Process.Pid

	var werr error
	select {
	case werr = <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("session never exited")
	}

	// The exit status still arrived intact — this is the contract a careless
	// reaper destroys.
	var ee *exec.ExitError
	if werr == nil {
		t.Fatal("onProcessExit got a nil error for a command that exited 7; a reaper stole the status")
	}
	if !asExitError(werr, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("onProcessExit error = %v, want exit status 7", werr)
	}

	// The monitor releases the pid immediately after cmd.Wait, so by the time the
	// callback has fired it must be gone from the registry.
	deadline := time.Now().Add(2 * time.Second)
	for registered(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d is still registered after its session exited; the sweep would skip it forever", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// asExitError is errors.As with a concrete target, kept local so the test reads
// as one assertion.
func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

// The collection half: an orphan re-parented onto this process is reapable, and
// reaping it is what frees the pid slot. Uses one pid this test created and never
// a sweep, for the reason in the file header.
func TestReapIfUnownedCollectsAnOrphanZombie(t *testing.T) {
	if err := installSubreaper(); err != nil {
		t.Skipf("cannot become a child subreaper: %v", err)
	}
	// Not parallel: it installs a process-wide flag and inspects orphan state.

	// The middle shell exits at once, orphaning the sleep; the subreaper flag
	// re-parents it here, and it becomes our zombie when the sleep ends.
	out, err := exec.Command("/bin/sh", "-c", "sleep 0.3 & echo $!").Output()
	if err != nil {
		t.Fatalf("stage orphan: %v", err)
	}
	orphan, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse orphan pid from %q: %v", out, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(findOrphanZombies(), orphan) {
		if time.Now().After(deadline) {
			t.Fatalf("orphan pid %d never appeared as a zombie parented on this process", orphan)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !reapIfUnowned(orphan) {
		t.Fatalf("failed to collect orphan zombie %d", orphan)
	}
	if slices.Contains(findOrphanZombies(), orphan) {
		t.Errorf("orphan %d is still a zombie after being collected", orphan)
	}
	// Collecting twice must be a clean miss, not a hang: the sweep can race
	// itself across two ticks.
	if reapIfUnowned(orphan) {
		t.Errorf("collected orphan %d twice", orphan)
	}
}

func TestFindOrphanZombiesExcludesThisProcessAndLiveChildren(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	zombies := findOrphanZombies()
	if slices.Contains(zombies, os.Getpid()) {
		t.Error("findOrphanZombies included this process")
	}
	if slices.Contains(zombies, cmd.Process.Pid) {
		t.Error("findOrphanZombies included a live child")
	}
}

func TestWaitNoHangOnAnUnrelatedPidIsACleanMiss(t *testing.T) {
	t.Parallel()
	// pid 1 is never a child of this process, so wait must fail rather than block.
	done := make(chan bool, 1)
	go func() { done <- waitNoHang(1) }()
	select {
	case got := <-done:
		if got {
			t.Error("waitNoHang claimed to collect pid 1")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitNoHang blocked on a pid that is not our child; WNOHANG is missing")
	}
}

// statStateAndPPID has to survive an executable name containing spaces and
// parentheses, which is the classic way to misparse /proc/<pid>/stat.
func TestStatStateAndPPIDParsesAHostileComm(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		line      string
		wantState byte
		wantPPID  int
		wantOK    bool
	}{
		{"plain", "42 (sleep) Z 7 42 42 0 -1 0", 'Z', 7, true},
		{"comm with spaces and parens", "42 (my (odd) proc) S 99 42 42", 'S', 99, true},
		{"comm containing a close paren last", "42 (weird)name) R 5 42 42", 'R', 5, true},
		{"no close paren", "42 sleep Z 7", 0, 0, false},
		{"truncated after comm", "42 (sleep)", 0, 0, false},
		{"non-numeric ppid", "42 (sleep) Z notapid", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeTemp(t, tc.line)
			state, ppid, ok := statStateAndPPID(path)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if state != tc.wantState || ppid != tc.wantPPID {
				t.Errorf("state/ppid = %q/%d, want %q/%d", state, ppid, tc.wantState, tc.wantPPID)
			}
		})
	}
	if _, _, ok := statStateAndPPID("/proc/nonexistent-pid/stat"); ok {
		t.Error("an unreadable stat file reported ok")
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/stat"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp stat: %v", err)
	}
	return path
}

// The sweep interval is floored so a consumer cannot ask for a /proc walk every
// millisecond, and 0 means the default rather than a hot loop.
func TestStartZombieReaperStopsCleanlyAndFloorsTheInterval(t *testing.T) {
	t.Parallel()
	if zombieSweepFloor < time.Second {
		t.Errorf("zombieSweepFloor = %v; a sub-second floor would spend real CPU re-walking /proc", zombieSweepFloor)
	}
	if zombieSweepDefault < zombieSweepFloor {
		t.Errorf("zombieSweepDefault %v is below the floor %v", zombieSweepDefault, zombieSweepFloor)
	}

	// A long interval means the sweep goroutine parks on its ticker and never
	// fires, so this exercises start/stop without collecting anything.
	stop := StartZombieReaper(nil, time.Hour)
	if stop == nil {
		t.Fatal("StartZombieReaper returned a nil stop function")
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop() did not return; the sweep goroutine is leaked")
	}
	stop() // idempotent enough not to panic on a second call
}
