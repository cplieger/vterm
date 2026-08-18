//go:build linux

package terminal

// Linux discovery tests for the automatic session title. These run against real
// kernel interfaces (TIOCGPGRP on a live pty, procfs for the name and cwd)
// because that IS the contract: there is no parser left to fixture, so a test
// that mocked procfs would assert nothing about what the kernel actually reports.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// startPTY runs a command on a fresh pty and returns the master plus the child
// pid. pty.StartWithSize puts the child in a new session with the pty as its
// controlling terminal, so the child is its own session and process-group leader
// — which is exactly the shape probeForeground's "is the shell in the
// foreground?" test (pgid == rootPID) relies on.
func startPTY(t *testing.T, name string, arg ...string) (*os.File, int) {
	t.Helper()
	cmd := exec.Command(name, arg...) // #nosec G204 -- fixed test commands
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("pty.StartWithSize(%s): %v", name, err)
	}
	t.Cleanup(func() {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return ptmx, cmd.Process.Pid
}

// TestForegroundPGIDLivePTY pins the ioctl: on a freshly started pty the child is
// the session leader, so the foreground process group is the child itself.
func TestForegroundPGIDLivePTY(t *testing.T) {
	ptmx, pid := startPTY(t, "/bin/cat")

	pgid, err := foregroundPGID(ptmx)
	if err != nil {
		t.Fatalf("foregroundPGID: %v", err)
	}
	if pgid != pid {
		t.Fatalf("foregroundPGID = %d, want the session leader %d", pgid, pid)
	}
}

// TestForegroundPGIDNotATerminal pins the failure direction: the ioctl on a
// non-tty returns an error rather than a plausible-looking pgid, so the ladder
// falls through instead of naming the session after process 0.
func TestForegroundPGIDNotATerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if pgid, err := foregroundPGID(f); err == nil {
		t.Fatalf("foregroundPGID(%s) = %d, nil; want an error", os.DevNull, pgid)
	}
}

// TestProcessNameLive reads a name for a process that exists (this test binary)
// and returns empty for one that does not. The empty case is the documented
// pipeline degradation: when the foreground group's LEADER has exited, procfs has
// no entry and the ladder rests at the cwd.
func TestProcessNameLive(t *testing.T) {
	if got := processName(os.Getpid()); got == "" {
		t.Fatal("processName(self) = \"\", want the test binary's name")
	} else if got != filepath.Base(got) {
		t.Fatalf("processName(self) = %q, want a basename with no path separators", got)
	}

	// A pid that has exited: start and reap a child, then read it. The pid could
	// in principle be recycled between the wait and the read, which would make
	// this read a live process instead — so assert only that it does not panic and
	// returns empty or a basename, not that it is empty.
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("/bin/true: %v", err)
	}
	dead := cmd.Process.Pid
	if got := processName(dead); got != "" && got != filepath.Base(got) {
		t.Fatalf("processName(reaped pid) = %q, want empty or a basename", got)
	}
}

// TestProcessCwdBaseLive reads this process's own working directory through the
// same procfs symlink the ladder uses, and pins that a nonexistent pid yields ""
// rather than an error the caller has to handle.
func TestProcessCwdBaseLive(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if got, want := processCwdBase(os.Getpid()), filepath.Base(wd); got != want {
		t.Fatalf("processCwdBase(self) = %q, want %q", got, want)
	}
	// procfs has no entry for pid 0; the ladder must read that as "unknown".
	if got := processCwdBase(0); got != "" {
		t.Fatalf("processCwdBase(0) = %q, want empty", got)
	}
}

// TestProbeForegroundShellAtRest is the resting case, which is what a generic
// terminal shows most of the time: nothing is running, so the probe reports no
// process name and offers the working directory as the label.
func TestProbeForegroundShellAtRest(t *testing.T) {
	ptmx, pid := startPTY(t, "/bin/cat")

	p := probeForeground(ptmx, pid)
	if !p.ok {
		t.Fatal("probe ok = false, want true on a live pty")
	}
	if p.pgid != pid {
		t.Fatalf("probe pgid = %d, want %d", p.pgid, pid)
	}
	if p.procName != "" {
		t.Fatalf("probe procName = %q, want empty: the session's own process is in the foreground", p.procName)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if want := filepath.Base(wd); p.cwdBase != want {
		t.Fatalf("probe cwdBase = %q, want %q", p.cwdBase, want)
	}
}

// TestProbeForegroundNotStarted pins the "no information" signal: an unstarted
// session must report ok=false so the confirmation window HOLDS the seeded
// command basename rather than treating a failed probe as "nothing is running".
func TestProbeForegroundNotStarted(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	if p := h.probeAutoTitle(); p.ok {
		t.Fatalf("probeAutoTitle on an unstarted handler = %+v, want ok=false", p)
	}
}

// jobControlWorks reports whether the available /bin/sh puts a foreground child
// in its own process group when asked to (set -m). This is a PREREQUISITE probe,
// deliberately separate from the assertions that depend on it: a test that skipped
// on "the title never appeared" would convert a broken probe into a silent pass,
// since that is the same signature.
func jobControlWorks(t *testing.T) bool {
	t.Helper()
	ptmx, pid := startPTY(t, "/bin/sh")
	if _, err := ptmx.WriteString("set -m\nsleep 30\n"); err != nil {
		t.Fatalf("write to pty: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p := probeForeground(ptmx, pid); p.pgid > 0 && p.pgid != pid {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestProbeForegroundRunningChild is the rung-1 case: with job control enabled, a
// foreground command gets its own process group, so the probe names it. This is
// the behaviour the whole automatic title exists for — a tab that reads "vim"
// instead of the shell's name.
func TestProbeForegroundRunningChild(t *testing.T) {
	if !jobControlWorks(t) {
		t.Skip("/bin/sh does not honour set -m here; job control is the shell's prerequisite, not the probe's behaviour")
	}
	ptmx, pid := startPTY(t, "/bin/sh")
	if _, err := ptmx.WriteString("set -m\nsleep 30\n"); err != nil {
		t.Fatalf("write to pty: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		p := probeForeground(ptmx, pid)
		if p.procName == "sleep" {
			if p.pgid == pid {
				t.Fatalf("probe reported the shell's own pgid %d while naming a child", p.pgid)
			}
			return // rung 1 reached
		}
		if time.Now().After(deadline) {
			// The prerequisite held, so this IS the probe failing.
			t.Fatalf("foreground child never named; last probe: %+v", p)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAutoTitleLadderThroughManager is the end-to-end ladder the design promised:
// a real session on a real pty, driven by the real status sweep, observed through
// the public SessionInfo.Title. It covers what the probe-level tests cannot — that
// confirmAutoTitle, the sweep's sole-writer rule, and List all agree — plus the
// two transitions that matter: resting at the cwd, adopting a confirmed
// foreground command, and returning to rest when it exits.
func TestAutoTitleLadderThroughManager(t *testing.T) {
	if !jobControlWorks(t) {
		t.Skip("/bin/sh does not honour set -m here; the foreground-adoption rung needs job control")
	}
	m := NewSessionManager(func(SessionID) *Handler {
		return NewHandler([]string{"/bin/sh"}, WithLogger(nil))
	})
	t.Cleanup(func() { shutdownManager(t, m) })

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	titleOf := func() string {
		for _, info := range m.List() {
			if info.ID == id {
				return info.Title
			}
		}
		return ""
	}
	awaitTitle := func(t *testing.T, want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if titleOf() == want {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("title never became %q (last: %q)", want, titleOf())
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// Rung 3: the shell is at rest, so the label is the session's directory. (The
	// seeded rung-4 value is "sh", so this also proves a sweep ran and refined it.)
	awaitTitle(t, filepath.Base(wd))

	// Rung 1: a foreground command that outlives the confirmation window.
	h := handlerOf(t, m, id)
	if _, err := h.ptmx.WriteString("set -m\nsleep 30\n"); err != nil {
		t.Fatalf("write to pty: %v", err)
	}
	awaitTitle(t, "sleep")

	// Back to rung 3 when it goes away: falling back is NOT debounced.
	if _, err := h.ptmx.Write([]byte{0x03}); err != nil { // Ctrl-C
		t.Fatalf("interrupt: %v", err)
	}
	awaitTitle(t, filepath.Base(wd))
}

// TestCleanProcName pins the sanitizer on the one automatic-title rung fed by
// untrusted input: argv[0] is chosen by whoever started the process, so on a
// shared terminal a hostile name must not reach a tab label, an SSE frame, or a
// log line intact (CWE-117). Invalid UTF-8 returns EMPTY rather than a mangled
// name, so the ladder falls to the next rung instead of rendering garbage.
func TestCleanProcName(t *testing.T) {
	long := strings.Repeat("n", procNameMaxRunes+40)
	cases := map[string]struct {
		in   string
		want string
	}{
		"plain":            {"vim", "vim"},
		"controls dropped": {"vi\nm\x1b[31m", "vim[31m"},
		"del dropped":      {"vi\x7fm", "vim"},
		"invalid utf8":     {"vim\xff\xfe", ""},
		"empty":            {"", ""},
		"bounded":          {long, strings.Repeat("n", procNameMaxRunes)},
		"multibyte kept":   {"café", "café"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := cleanProcName(tc.in); got != tc.want {
				t.Errorf("cleanProcName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestReadArgv0 covers the bounded procfs read's failure directions against real
// files: a NUL-separated argv yields its first element, and a buffer with NO
// terminator yields empty rather than a truncated prefix — half a path is a worse
// label than falling through to comm.
func TestReadArgv0(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, name string, content []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		return p
	}

	if got := readArgv0(write(t, "normal", []byte("/usr/bin/vim\x00-p\x00file\x00"))); got != "/usr/bin/vim" {
		t.Errorf("readArgv0(normal) = %q, want %q", got, "/usr/bin/vim")
	}
	// No NUL anywhere in the bounded read: indistinguishable from a truncated
	// prefix, so it must be rejected.
	if got := readArgv0(write(t, "unterminated", []byte(strings.Repeat("x", procNameMaxBytes+50)))); got != "" {
		t.Errorf("readArgv0(unterminated) = %q, want empty", got)
	}
	if got := readArgv0(write(t, "empty", nil)); got != "" {
		t.Errorf("readArgv0(empty) = %q, want empty", got)
	}
	if got := readArgv0(write(t, "all-nul", []byte{0, 0, 0})); got != "" {
		t.Errorf("readArgv0(all NUL) = %q, want empty", got)
	}
	if got := readArgv0(filepath.Join(dir, "does-not-exist")); got != "" {
		t.Errorf("readArgv0(missing) = %q, want empty", got)
	}
}
