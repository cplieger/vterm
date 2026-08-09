//go:build linux

package terminal

// Linux primitives for zombie reaping (see zombiereap.go for why this is a
// separate concern from session reaping, and why it is a sweep rather than a
// SIGCHLD handler).

import (
	"bytes"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// installSubreaper asks the kernel to re-parent this process's orphaned
// descendants onto it rather than onto init.
//
// A no-op in effect when the process is already PID 1, which is the deployment
// this was written for; it matters for the other one, where a server runs behind
// an init shim and would otherwise never see the orphans at all.
func installSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

// findOrphanZombies returns the pids of zombies whose parent is this process.
//
// Parses only the two fields it needs out of /proc/<pid>/stat, and parses them
// from the RIGHT of the closing parenthesis: field 2 is the executable name in
// parentheses and it may itself contain spaces and parentheses, so splitting the
// line on whitespace from the left is the classic way to misread this file.
func findOrphanZombies() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		state, ppid, ok := statStateAndPPID("/proc/" + e.Name() + "/stat")
		if !ok || state != 'Z' || ppid != self {
			continue
		}
		out = append(out, pid)
	}
	return out
}

// statStateAndPPID reads the process state and parent pid from a procfs stat
// line.
func statStateAndPPID(path string) (state byte, ppid int, ok bool) {
	f, err := os.Open(path) // #nosec G304 -- procfs path built from a numeric pid
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.LimitReader(f, reapStatusMaxBytes))
	if err != nil {
		return 0, 0, false
	}
	// Everything before the LAST ')' is pid + comm; the fields after it are
	// positionally reliable regardless of what the executable was called.
	commEnd := bytes.LastIndexByte(buf, ')')
	if commEnd < 0 {
		return 0, 0, false
	}
	fields := bytes.Fields(buf[commEnd+1:])
	if len(fields) < 2 || len(fields[0]) == 0 {
		return 0, 0, false
	}
	parent, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], parent, true
}

// waitNoHang collects pid's exit status without blocking, reporting whether it
// took one.
//
// WNOHANG is what keeps a sweep from ever stalling the reaper goroutine: a pid
// that stopped being a zombie between the scan and this call simply yields
// nothing.
func waitNoHang(pid int) bool {
	var ws unix.WaitStatus
	got, err := unix.Wait4(pid, &ws, unix.WNOHANG, nil)
	return err == nil && got == pid
}
