//go:build linux

package terminal

// Tests for the marker reap domain. Like the containment tests, these use the
// real kernel and real child processes rather than a fake /proc: the claim under
// test is about what the kernel does with an inherited environment across
// setsid() and re-parenting, and a fake would assert the author's model instead
// of the platform's behaviour.
//
// "Gone" is always decided by the domain's own scan, which reads
// /proc/<pid>/environ. A zombie's mm is gone, so its environ reads empty and it
// never matches — which is the correct reading (an uncollected exit status is
// the zombie reaper's problem, not a process to signal) and it also keeps these
// tests independent of whether anything reaps in the ambient environment.

import (
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// newTestReap builds a reap domain with a captured logger, matching what
// newSessionReap produces without needing a started Handler.
func newTestReap(t *testing.T) (*sessionReap, *recordingHandler) {
	t.Helper()
	rec := &recordingHandler{}
	h := &Handler{cfg: handlerConfig{
		logger:        slog.New(rec),
		containmentID: "reap-test",
	}}
	s := h.newSessionReap()
	if s == nil {
		t.Fatal("newSessionReap returned nil with reaping enabled")
	}
	if s.marker == "" {
		t.Fatal("reap domain has an empty marker")
	}
	return s, rec
}

// requireSetsid skips when util-linux's setsid is unavailable, since the escape
// case cannot be staged without it.
func requireSetsid(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("setsid")
	if err != nil {
		t.Skipf("setsid not available: %v", err)
	}
	return path
}

// startMarked spawns a shell carrying the domain's marker and returns the head.
// The caller reaps the head itself, exactly as the engine's monitor does.
func startMarked(t *testing.T, s *sessionReap, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	// Marker first, mirroring the engine's spawn path (reap.go explains why).
	cmd.Env = append([]string{s.envPair()}, os.Environ()...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start marked tree: %v", err)
	}
	t.Cleanup(func() {
		for _, pid := range reapFindByMarker(s.marker) {
			reapKill(pid)
		}
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

// waitForMembers polls until the domain has at least n members, so no test races
// the shell's own forking.
func waitForMembers(t *testing.T, s *sessionReap, n int) []int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if members := reapFindByMarker(s.marker); len(members) >= n {
			return members
		}
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d marked processes ever appeared", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The load-bearing property: the marker crosses setsid(), so the domain spans a
// tree that no process-group kill could reach in one call.
func TestReapMarkerCrossesSetsid(t *testing.T) {
	t.Parallel()
	requireSetsid(t)
	s, _ := newTestReap(t)
	head := startMarked(t, s, "setsid sleep 60 & sleep 60 & wait")

	members := waitForMembers(t, s, 3)

	sessions := map[int]bool{}
	for _, pid := range members {
		sessions[sidOf(t, pid)] = true
	}
	if len(sessions) < 2 {
		t.Fatalf("the marked tree spans %d session(s); the setsid escapee is the case this domain exists for, so the fixture is invalid", len(sessions))
	}
	if !slices.Contains(members, head.Process.Pid) {
		t.Errorf("the head pid %d is not in its own domain", head.Process.Pid)
	}
}

func TestReapTeardownReclaimsTheWholeTree(t *testing.T) {
	t.Parallel()
	requireSetsid(t)
	s, rec := newTestReap(t)
	head := startMarked(t, s, "setsid sleep 60 & sleep 60 & wait")
	waitForMembers(t, s, 3)

	// Exactly what exec.CommandContext's default Cancel does: SIGKILL the head
	// and nothing else. Everything still standing afterwards is an escapee.
	if err := head.Process.Kill(); err != nil {
		t.Fatalf("kill head: %v", err)
	}
	_, _ = head.Process.Wait()
	if len(reapFindByMarker(s.marker)) == 0 {
		t.Fatal("the tree died with the head, so this test is not exercising the escape case")
	}

	s.teardown()

	if left := reapFindByMarker(s.marker); len(left) != 0 {
		t.Fatalf("teardown left %d marked process(es) alive: %v", len(left), left)
	}
	attrs, ok := rec.find("terminal: session reap reclaimed escaped processes")
	if !ok {
		t.Fatal("no reclaim WARN logged for a session that had survivors")
	}
	if got, _ := attrs["survivors"].(int64); got < 1 {
		t.Errorf("survivors = %v, want >= 1", attrs["survivors"])
	}
	if _, present := attrs["resident_bytes"]; !present {
		t.Error("reclaim WARN is missing resident_bytes, the field that says what the session was still holding")
	}
}

// A tree that ended on its own must cost one scan and produce no log line, since
// that is the overwhelmingly common case and a WARN per session would be noise.
func TestReapTeardownIsSilentWhenTheTreeEndedItself(t *testing.T) {
	t.Parallel()
	s, rec := newTestReap(t)
	head := startMarked(t, s, "true")
	_, _ = head.Process.Wait()

	s.teardown()

	if _, ok := rec.find("terminal: session reap reclaimed escaped processes"); ok {
		t.Fatal("a tree that exited on its own must not produce a reclaim WARN")
	}
}

// The escalation half: a survivor that discards SIGTERM can only be ended by the
// SIGKILL round, and the WARN must say so rather than reporting a clean reclaim.
func TestReapTeardownEscalatesToKillForASIGTERMIgnorer(t *testing.T) {
	t.Parallel()
	s, rec := newTestReap(t)
	// The IGNORER has to be the survivor, not the head: the head is SIGKILLed
	// below, so a trap on it would prove nothing.
	head := startMarked(t, s, `sh -c 'trap "" TERM; sleep 60' & wait`)
	waitForMembers(t, s, 2)
	if err := head.Process.Kill(); err != nil {
		t.Fatalf("kill head: %v", err)
	}
	_, _ = head.Process.Wait()

	s.teardown()

	if left := reapFindByMarker(s.marker); len(left) != 0 {
		t.Fatalf("a SIGTERM-ignoring tree survived teardown: %v", left)
	}
	attrs, ok := rec.find("terminal: session reap reclaimed escaped processes")
	if !ok {
		t.Fatal("no reclaim WARN for an escalated teardown")
	}
	if forced, _ := attrs["kill_forced"].(int64); forced < 1 {
		t.Errorf("kill_forced = %v, want >= 1 for a tree that discards SIGTERM", attrs["kill_forced"])
	}
}

// Two sessions must be mutually invisible, or one tab's teardown ends another's
// work. This is why the marker is random per session rather than derived from the
// session id.
func TestReapDomainsDoNotSeeEachOther(t *testing.T) {
	t.Parallel()
	a, _ := newTestReap(t)
	b, _ := newTestReap(t)
	if a.marker == b.marker {
		t.Fatal("two sessions were minted the same marker")
	}
	startMarked(t, a, "sleep 60")
	waitForMembers(t, a, 1)

	if found := reapFindByMarker(b.marker); len(found) != 0 {
		t.Fatalf("domain b matched %d of domain a's processes: %v", len(found), found)
	}
	b.teardown()
	if len(reapFindByMarker(a.marker)) == 0 {
		t.Fatal("tearing down an unrelated domain killed this one's tree")
	}
}

// A consumer's WithEnv is appended after the engine's own variables, and os/exec
// keeps the LAST value for a repeated key — so without the strip in the spawn
// path, a consumer setting this key would replace the engine's marker and switch
// reaping off for that session without a word. Driven through the real spawn path
// rather than a hand-built env, because the strip is part of that path.
func TestReapMarkerSurvivesADuplicateKeyFromWithEnv(t *testing.T) {
	t.Parallel()
	h := NewHandler([]string{"/bin/sleep", "60"},
		WithWorkDir("/"),
		WithLogger(nil),
		WithEnv([]string{reapMarkerEnv + "=consumer-supplied"}),
	)
	if err := h.ensureStarted(80, 24); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}
	defer h.Shutdown()

	if h.reap == nil {
		t.Fatal("no reap domain was minted for a session with reaping on by default")
	}
	if h.reap.marker == "consumer-supplied" {
		t.Fatal("the consumer's value became the domain's marker")
	}
	members := reapFindByMarker(h.reap.marker)
	if !slices.Contains(members, h.cmd.Process.Pid) {
		t.Fatalf("a consumer setting %s displaced the engine's marker: session pid %d is outside its own domain (members=%v)",
			reapMarkerEnv, h.cmd.Process.Pid, members)
	}
}

func TestStripReapMarker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"no marker", []string{"A=1", "B=2"}, []string{"A=1", "B=2"}},
		{"drops the marker", []string{"A=1", reapMarkerEnv + "=x", "B=2"}, []string{"A=1", "B=2"}},
		{"drops every occurrence", []string{reapMarkerEnv + "=x", reapMarkerEnv + "=y"}, nil},
		{"empty value still drops", []string{reapMarkerEnv + "="}, nil},
		{"a longer key that merely starts the same is kept", []string{reapMarkerEnv + "_EXTRA=x"}, []string{reapMarkerEnv + "_EXTRA=x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := stripReapMarker(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("stripReapMarker(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The strip must not mutate the consumer's slice: handlerConfig.env is the
// caller's backing array, and a session that quietly edited it would corrupt
// every later session built from the same options.
func TestStripReapMarkerDoesNotMutateItsInput(t *testing.T) {
	t.Parallel()
	in := []string{"A=1", reapMarkerEnv + "=x", "B=2"}
	original := slices.Clone(in)
	_ = stripReapMarker(in)
	if !slices.Equal(in, original) {
		t.Errorf("stripReapMarker mutated its input: %q, want %q", in, original)
	}
}

// The pid-recycle guard: reapAlive re-checks the marker rather than testing mere
// existence, so an unrelated process that inherits a recycled pid is not ours.
func TestReapAliveRejectsAPidOutsideTheDomain(t *testing.T) {
	t.Parallel()
	s, _ := newTestReap(t)
	if reapAlive(os.Getpid(), s.marker) {
		t.Fatal("the test process matched a domain it never joined")
	}
	if reapAlive(1, s.marker) {
		t.Fatal("pid 1 matched a session domain")
	}
	if reapAlive(os.Getpid(), "") {
		t.Fatal("an empty marker matched a process; that would make every domain universal")
	}
}

func TestReapNilDomainIsNoop(t *testing.T) {
	t.Parallel()
	var s *sessionReap
	if got := s.envPair(); got != "" {
		t.Fatalf("nil domain envPair = %q, want empty", got)
	}
	s.teardown() // must not panic
}

func TestReapingIsOnByDefaultAndOptOutWorks(t *testing.T) {
	t.Parallel()
	var def handlerConfig
	if def.noReap {
		t.Fatal("reaping must be ON by default: an unreaped tree holds its memory for the container's lifetime")
	}
	var off handlerConfig
	WithoutSessionReap()(&off)
	if !off.noReap {
		t.Fatal("WithoutSessionReap did not set the opt-out")
	}
	if got := (&Handler{cfg: off}).newSessionReap(); got != nil {
		t.Fatal("newSessionReap returned a domain despite the opt-out")
	}
}

func TestReapResidentReportsBytesForALiveTree(t *testing.T) {
	t.Parallel()
	s, _ := newTestReap(t)
	startMarked(t, s, "sleep 60")
	members := waitForMembers(t, s, 1)
	if got := reapResident(members); got == 0 {
		t.Fatal("reapResident returned 0 for a live tree; the reclaim WARN would understate the leak")
	}
	if got := reapResident(nil); got != 0 {
		t.Errorf("reapResident(nil) = %d, want 0", got)
	}
}

// The crash-then-close sequence calls teardown from more than one place, so the
// ladder must run exactly once.
func TestReapTeardownIsIdempotentUnderConcurrency(t *testing.T) {
	t.Parallel()
	s, rec := newTestReap(t)
	head := startMarked(t, s, `sh -c 'sleep 60' & wait`)
	waitForMembers(t, s, 2)
	_ = head.Process.Kill()
	_, _ = head.Process.Wait()

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(s.teardown)
	}
	wg.Wait()

	rec.mu.Lock()
	count := 0
	for _, r := range rec.records {
		if r.Message == "terminal: session reap reclaimed escaped processes" {
			count++
		}
	}
	rec.mu.Unlock()
	if count > 1 {
		t.Fatalf("teardown ran its ladder %d times; sync.Once should make that exactly one", count)
	}
}

// statusFieldKB is the shared procfs field reader; a missing field must be a
// clean miss rather than a zero that reads as a real measurement.
func TestStatusFieldKB(t *testing.T) {
	t.Parallel()
	self := "/proc/" + strconv.Itoa(os.Getpid()) + "/status"
	if _, ok := statusFieldKB(self, "VmRSS:"); !ok {
		t.Error("VmRSS not found in this process's own status file")
	}
	if _, ok := statusFieldKB(self, "NoSuchField:"); ok {
		t.Error("a missing field reported ok")
	}
	if _, ok := statusFieldKB("/proc/nonexistent-pid/status", "VmRSS:"); ok {
		t.Error("an unreadable status file reported ok")
	}
}
