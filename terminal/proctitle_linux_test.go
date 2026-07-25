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

// TestProbeForegroundRunningChild is the rung-1 case end to end: with job control
// enabled, a foreground command gets its own process group, so the probe names it.
// This is the behaviour the whole automatic title exists for — a tab that reads
// "vim" instead of the shell's name.
//
// Job control is what makes a foreground child its own process group, and a shell
// only enables it when interactive, so the test asks for it explicitly (set -m).
// If the available /bin/sh does not honour that, the probe keeps reporting the
// shell and the test skips rather than failing: the mechanism under test is the
// probe, not the shell's job-control support.
func TestProbeForegroundRunningChild(t *testing.T) {
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
			t.Skipf("/bin/sh did not put a foreground child in its own process group "+
				"(last probe: %+v); job control is the shell's, not the probe's", p)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
