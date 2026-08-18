//go:build linux

package terminal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// recordingHandler collects slog records so a test can assert on the containment
// log contract (the reclaim WARN and its counts) without touching slog.Default,
// which keeps these tests parallel-safe.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// find returns the first record with the given message and its attributes.
func (h *recordingHandler) find(msg string) (map[string]any, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message != msg {
			continue
		}
		attrs := map[string]any{}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.Any()
			return true
		})
		return attrs, true
	}
	return nil, false
}

// newTestContainment builds a Containment over a scratch cgroup nested inside
// systemd's delegated user cgroup, so the tests need no capability and cannot
// touch a cgroup they did not create. Skips when that tree is unavailable or not
// writable, which is the CI case.
func newTestContainment(t *testing.T) (*Containment, *recordingHandler) {
	t.Helper()
	delegated := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service", os.Getuid(), os.Getuid())
	if _, err := os.Stat(delegated); err != nil {
		t.Skipf("no delegated user cgroup at %s: %v", delegated, err)
	}
	// A subtest's name contains "/", which would make this a nested path whose
	// parent does not exist; the mkdir would fail and the test would silently
	// skip instead of running. Flatten it.
	safeName := strings.ReplaceAll(t.Name(), "/", "-")
	root := filepath.Clean(filepath.Join(delegated, fmt.Sprintf("wt-gotest-%d-%s", os.Getpid(), safeName)))
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Skipf("cannot create scratch cgroup %s: %v", root, err)
	}
	t.Cleanup(func() {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.IsDir() {
				_ = os.WriteFile(filepath.Join(root, e.Name(), "cgroup.kill"), []byte("1"), 0o644)
				_ = os.Remove(filepath.Join(root, e.Name()))
			}
		}
		_ = os.Remove(root)
	})

	rec := &recordingHandler{}
	c, err := NewContainment(CgroupRoot(root), "wt-", slog.New(rec))
	if err != nil {
		if errors.Is(err, errContainmentUnsupported) {
			t.Skipf("containment unsupported here: %v", err)
		}
		t.Fatalf("NewContainment: %v", err)
	}
	return c, rec
}

// startInCgroup starts argv in sc's cgroup with its own session, mirroring how
// pty.StartWithSize spawns the PTY child (Setsid true).
func startInCgroup(t *testing.T, sc *sessionCgroup, argv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	sc.applyTo(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", argv, err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd
}

// alive reports whether pid is a live (non-zombie) process.
func alive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	i := len(s) - 1
	for ; i >= 0 && s[i] != ')'; i-- {
	}
	if i < 0 || i+2 >= len(s) {
		return false
	}
	return s[i+2] != 'Z'
}

// waitGone polls until pid is gone or the deadline passes.
func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !alive(pid)
}

// sidOf reads a process's session id, used to prove the bait really did escape
// the head's session.
func sidOf(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatalf("read stat %d: %v", pid, err)
	}
	s := string(b)
	i := len(s) - 1
	for ; i >= 0 && s[i] != ')'; i-- {
	}
	var state string
	var ppid, pgrp, sid int
	if _, err := fmt.Sscan(s[i+2:], &state, &ppid, &pgrp, &sid); err != nil {
		t.Fatalf("parse stat %d: %v", pid, err)
	}
	return sid
}

// TestContainmentReclaimsSetsidEscapee is THE regression test: the exact defect
// containment exists for. A cgroup member in its own session is unreachable by
// kill(-pgid) and by the PTY-close SIGHUP, so nothing but the cgroup can end it.
//
// If teardown is gutted this test fails, which is the property that makes it
// worth having; TestEscapeeSurvivesWithoutContainment records the same bait's
// behavior with no containment so the baseline is explicit rather than assumed.
func TestContainmentReclaimsSetsidEscapee(t *testing.T) {
	c, rec := newTestContainment(t)
	sc, err := c.create("escapee")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	head := startInCgroup(t, sc, "sleep", "60")
	escapee := startInCgroup(t, sc, "sleep", "60")
	sc.releaseFD()

	headSID, escSID := sidOf(t, head.Process.Pid), sidOf(t, escapee.Process.Pid)
	if headSID == escSID {
		t.Fatalf("bait is invalid: escapee shares the head's session %d, so a group kill would reach it", headSID)
	}

	// Exactly what exec.CommandContext's default Cancel does: SIGKILL the head
	// process, nothing else.
	if err := head.Process.Kill(); err != nil {
		t.Fatalf("kill head: %v", err)
	}
	_, _ = head.Process.Wait()

	if !alive(escapee.Process.Pid) {
		t.Fatal("escapee died with the head, so this test is not exercising the escape case")
	}

	sc.teardown()

	if !waitGone(escapee.Process.Pid, 3*time.Second) {
		t.Fatalf("escapee pid %d survived teardown", escapee.Process.Pid)
	}
	if _, err := os.Stat(sc.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cgroup %s still exists after teardown (%v)", sc.dir, err)
	}
	attrs, ok := rec.find("terminal: containment reclaimed escaped processes")
	if !ok {
		t.Fatal("no reclaim WARN logged for a session that had a survivor")
	}
	if got := attrs["survivors"]; got == nil || got.(int64) < 1 {
		t.Errorf("survivors = %v, want >= 1", got)
	}
}

// TestEscapeeSurvivesWithoutContainment is the red check. The bait must be
// something the UNGUARDED code actually loses: without a cgroup, killing the head
// leaves the escapee running, which is the leak measured in production.
func TestEscapeeSurvivesWithoutContainment(t *testing.T) {
	head := exec.Command("sleep", "60")
	head.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := head.Start(); err != nil {
		t.Fatalf("start head: %v", err)
	}
	escapee := exec.Command("sleep", "60")
	escapee.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := escapee.Start(); err != nil {
		t.Fatalf("start escapee: %v", err)
	}
	t.Cleanup(func() {
		_ = escapee.Process.Kill()
		_, _ = escapee.Process.Wait()
	})

	_ = head.Process.Kill()
	_, _ = head.Process.Wait()

	time.Sleep(300 * time.Millisecond)
	if !alive(escapee.Process.Pid) {
		t.Fatal("escapee died without containment, so the guarded test proves nothing")
	}
}

// TestContainmentEscalation covers both rungs of the teardown ladder: a process
// that honors SIGTERM must be reclaimed by the graceful pass, and one that ignores
// it must still die via the cgroup.kill backstop.
func TestContainmentEscalation(t *testing.T) {
	t.Run("honors SIGTERM", func(t *testing.T) {
		c, rec := newTestContainment(t)
		sc, err := c.create("graceful")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		victim := startInCgroup(t, sc, "sleep", "60")
		sc.releaseFD()
		sc.teardown()

		if !waitGone(victim.Process.Pid, 3*time.Second) {
			t.Fatalf("victim %d survived teardown", victim.Process.Pid)
		}
		attrs, ok := rec.find("terminal: containment reclaimed escaped processes")
		if !ok {
			t.Fatal("expected a reclaim WARN")
		}
		if forced, _ := attrs["kill_forced"].(int64); forced != 0 {
			t.Errorf("kill_forced = %d, want 0 for a process that honors SIGTERM", forced)
		}
	})

	t.Run("ignores SIGTERM", func(t *testing.T) {
		c, _ := newTestContainment(t)
		sc, err := c.create("stubborn")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// Ignores TERM and keeps the cgroup populated across the grace window:
		// the inner sleep dies, the shell loops and starts another.
		victim := startInCgroup(t, sc, "sh", "-c", `trap "" TERM; while :; do sleep 5; done`)
		sc.releaseFD()
		time.Sleep(200 * time.Millisecond) // let the trap install
		sc.teardown()

		if !waitGone(victim.Process.Pid, 3*time.Second) {
			t.Fatalf("SIGTERM-ignoring victim %d survived teardown; the cgroup.kill backstop did not fire", victim.Process.Pid)
		}
	})
}

// TestContainmentAccountsPeak checks the logging half of the feature. The number
// is only meaningful because the child is placed at clone time; a migrated child
// would report memory it never accounted, since cgroup v2 does not move charges.
func TestContainmentAccountsPeak(t *testing.T) {
	c, _ := newTestContainment(t)
	sc, err := c.create("accounting")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Touch a few MB so the peak is unambiguously above the noise floor.
	startInCgroup(t, sc, "sh", "-c", `a=$(head -c 4000000 /dev/zero | tr "\0" "x"); sleep 3; echo "${#a}" >/dev/null`)
	sc.releaseFD()

	deadline := time.Now().Add(3 * time.Second)
	var mem uint64
	for time.Now().Before(deadline) {
		if mem, _, _ = sc.current(); mem > 1<<20 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mem <= 1<<20 {
		t.Fatalf("memory.current = %d, expected the child's allocation to be charged here", mem)
	}

	sc.teardown()
	peak, _ := sc.peaks()
	if peak < mem {
		t.Errorf("memory.peak %d < observed current %d; peak must be a high-water mark", peak, mem)
	}
}

// TestContainmentTeardownIdempotent covers the real sequence a crashed session
// produces: teardown runs when the process exits, and the user may close the dead
// tab minutes later.
func TestContainmentTeardownIdempotent(t *testing.T) {
	c, _ := newTestContainment(t)
	sc, err := c.create("idempotent")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Latch a non-zero report first, so the repeat comparison is not 0 == 0.
	startInCgroup(t, sc, "sh", "-c", `a=$(head -c 3000000 /dev/zero | tr "\0" "x"); sleep 0.4; echo "${#a}" >/dev/null`)
	sc.releaseFD()
	time.Sleep(700 * time.Millisecond)
	sc.teardown()
	peak1, pids1 := sc.peaks()
	if peak1 == 0 {
		t.Fatal("nothing latched: this test would compare 0 to 0 and prove nothing")
	}
	sc.teardown() // must not panic, must not change the latched report
	sc.teardown()
	peak2, pids2 := sc.peaks()
	if peak1 != peak2 || pids1 != pids2 {
		t.Errorf("repeat teardown changed the report: (%d,%d) then (%d,%d)", peak1, pids1, peak2, pids2)
	}
}

// TestContainmentReclaimsSubCgroup covers the case where teardown's own
// accounting could disagree with the kernel's: cgroup.procs is per-cgroup while
// cgroup.events populated is recursive, and rmdir refuses a cgroup with children.
// A session's shell runs as root in a writable cgroup tree, so it can create a
// sub-cgroup; before the recursive enumeration this reported zero survivors,
// skipped the SIGTERM pass, silenced the reclaim warning, and left a directory
// neither teardown nor the startup sweep could ever remove.
func TestContainmentReclaimsSubCgroup(t *testing.T) {
	c, rec := newTestContainment(t)
	sc, err := c.create("nested")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Enable controllers on the session cgroup so it can have children, then put
	// the victim ONLY in the child.
	if err := os.WriteFile(filepath.Join(sc.dir, "cgroup.subtree_control"), []byte("+pids"), 0o600); err != nil {
		t.Skipf("cannot delegate into the session cgroup: %v", err)
	}
	kid := filepath.Join(sc.dir, "kid")
	if err := os.Mkdir(kid, 0o755); err != nil {
		t.Fatalf("mkdir sub-cgroup: %v", err)
	}
	victim := exec.Command("sleep", "60")
	victim.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := victim.Start(); err != nil {
		t.Fatalf("start victim: %v", err)
	}
	t.Cleanup(func() { _ = victim.Process.Kill(); _, _ = victim.Process.Wait() })
	if err := os.WriteFile(filepath.Join(kid, "cgroup.procs"), []byte(strconv.Itoa(victim.Process.Pid)), 0o600); err != nil {
		t.Fatalf("place victim in the sub-cgroup: %v", err)
	}
	sc.releaseFD()

	// The bait: the top-level cgroup.procs is empty while the subtree is populated.
	if got := readPids(filepath.Join(sc.dir, "cgroup.procs")); len(got) != 0 {
		t.Fatalf("bait not planted: top-level cgroup.procs is %v, want empty", got)
	}
	if !sc.populated() {
		t.Fatal("bait not planted: subtree reports not populated")
	}
	if n := len(sc.liveProcs()); n != 1 {
		t.Fatalf("liveProcs found %d processes, want 1: enumeration is not recursive", n)
	}

	sc.teardown()

	if !waitGone(victim.Process.Pid, 3*time.Second) {
		t.Errorf("victim %d in a sub-cgroup survived teardown", victim.Process.Pid)
	}
	if _, err := os.Stat(sc.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("session cgroup %s not removed (%v): rmdir is not recursive, so the sub-cgroup must be removed first", sc.dir, err)
	}
	if _, ok := rec.find("terminal: containment reclaimed escaped processes"); !ok {
		t.Error("no reclaim WARN for a survivor in a sub-cgroup: the survivor count is not seeing the subtree")
	}
}

// TestProbeCloneIntoCgroupRejectsBadFD pins the probe's reject path. Only the pass
// condition was covered, and a probe gutted to `return nil` is the design's named
// worst case: a host where clone3 is blocked boots reporting containment enabled
// and then fails every session spawn, leaving the user with no terminal at all.
func TestProbeCloneIntoCgroupRejectsBadFD(t *testing.T) {
	t.Parallel()
	if err := probeCloneIntoCgroup(-1); err == nil {
		t.Fatal("probe accepted an invalid cgroup fd, so a host that cannot place children would report itself supported")
	}
}

// TestContainmentCreateRejectsReservedNames covers the two leaf names the package
// uses itself. A session id of "server" would target the cgroup this process lives
// in.
func TestContainmentCreateRejectsReservedNames(t *testing.T) {
	c, _ := newTestContainment(t)
	for _, id := range []SessionID{containServerLeaf, containProbeLeaf} {
		if _, err := c.create(id); err == nil {
			t.Errorf("create(%q) succeeded, want a refusal: it is a reserved internal leaf", id)
		}
	}
}

// TestContainmentCreateRecreatesStale pins the no-adopt rule: an adopted cgroup
// carries the previous occupant's peak, which would make the cost report silently
// wrong, so a colliding directory is removed and recreated.
func TestContainmentCreateRecreatesStale(t *testing.T) {
	c, _ := newTestContainment(t)
	stale := c.path("collide")
	if err := os.Mkdir(stale, 0o755); err != nil {
		t.Fatalf("pre-create %s: %v", stale, err)
	}
	// POISON THE BAIT. An empty stale cgroup reports memory.peak 0, so asserting
	// "peak == 0" on the new handle passes whether create recreated the directory
	// or adopted it, which is the behavior under test. Run a process in the stale
	// cgroup first so its peak is non-zero and only a real recreate can read 0.
	fd, err := syscall.Open(stale, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open stale: %v", err)
	}
	dirty := exec.Command("sh", "-c", `a=$(head -c 3000000 /dev/zero | tr "\0" "x"); echo "${#a}" >/dev/null`)
	dirty.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: fd}
	runErr := dirty.Run()
	_ = syscall.Close(fd)
	if runErr != nil {
		t.Fatalf("dirty the stale cgroup: %v", runErr)
	}
	stalePeak := readUint(filepath.Join(stale, "memory.peak"))
	if stalePeak == 0 {
		t.Fatalf("bait not planted: stale cgroup still reports peak 0, so this test cannot distinguish adopt from recreate")
	}

	sc, err := c.create("collide")
	if err != nil {
		t.Fatalf("create over a stale cgroup: %v", err)
	}
	sc.releaseFD()
	t.Cleanup(func() { sc.teardown() })
	if peak, _ := sc.peak(); peak != 0 {
		t.Errorf("fresh cgroup reports peak %d (stale was %d): the stale directory was adopted, not recreated", peak, stalePeak)
	}
}

// TestContainmentSweepIsPrefixBound checks the startup sweep cannot remove a
// cgroup this package did not create. The sweep runs inside NewContainment, so the
// assertion is that a foreign sibling survived it.
func TestContainmentSweepIsPrefixBound(t *testing.T) {
	c, _ := newTestContainment(t)
	foreign := filepath.Join(c.root, "not-ours")
	if err := os.Mkdir(foreign, 0o755); err != nil {
		t.Fatalf("create foreign cgroup: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(foreign) })

	leftover := c.path("leftover-from-a-previous-run")
	if err := os.Mkdir(leftover, 0o755); err != nil {
		t.Fatalf("create leftover: %v", err)
	}
	c.sweep()

	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("sweep removed a cgroup it does not own: %v", err)
	}
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sweep left its own leftover behind (%v)", err)
	}
	if _, err := os.Stat(c.path(containServerLeaf)); err != nil {
		t.Errorf("sweep removed the server leaf it must keep: %v", err)
	}
}

// TestContainmentSanitizesSessionID checks an id that cannot escape its root,
// end to end rather than only through the pure function.
func TestContainmentSanitizesSessionID(t *testing.T) {
	c, _ := newTestContainment(t)
	sc, err := c.create("../../escape-attempt")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sc.releaseFD()
	t.Cleanup(func() { sc.teardown() })
	want := c.path("escape-attempt")
	if sc.dir != want {
		t.Errorf("cgroup dir = %q, want %q", sc.dir, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected the cgroup inside the root: %v", err)
	}
}

// TestContainmentRejectsUnusableID covers the one create input with no safe
// interpretation.
func TestContainmentRejectsUnusableID(t *testing.T) {
	c, _ := newTestContainment(t)
	for _, id := range []SessionID{"", "..", "///"} {
		if _, err := c.create(id); err == nil {
			t.Errorf("create(%q) succeeded, want an error", id)
		}
	}
}

// TestProbeCloneIntoCgroupAcceptsRealCgroup pins the availability probe's pass
// condition, which is what stands between a host that cannot place children and a
// user with no terminal at all.
func TestProbeCloneIntoCgroupAcceptsRealCgroup(t *testing.T) {
	c, _ := newTestContainment(t)
	dir := c.path("probe-target")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(dir) })
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := probeCloneIntoCgroup(fd); err != nil {
		t.Fatalf("probe rejected a usable cgroup: %v", err)
	}
}

// TestReadHelpers covers the small parsers on the inputs a cgroup file actually
// produces, including the "max" that memory.max reports and the empty file a
// missing controller leaves.
func TestReadHelpers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	if got := readUint(write("n", "12345\n")); got != 12345 {
		t.Errorf("readUint = %d, want 12345", got)
	}
	if got := readUint(write("max", "max\n")); got != 0 {
		t.Errorf("readUint(max) = %d, want 0", got)
	}
	if got := readUint(filepath.Join(dir, "absent")); got != 0 {
		t.Errorf("readUint(absent) = %d, want 0", got)
	}
	pids := readPids(write("procs", "10\n20\nnotapid\n-3\n0\n30\n"))
	want := []int{10, 20, 30}
	if len(pids) != len(want) {
		t.Fatalf("readPids = %v, want %v", pids, want)
	}
	for i := range want {
		if pids[i] != want[i] {
			t.Errorf("readPids[%d] = %d, want %d", i, pids[i], want[i])
		}
	}
	if got := readTrim(write("s", "  hello \n")); got != "hello" {
		t.Errorf("readTrim = %q, want %q", got, "hello")
	}
}
