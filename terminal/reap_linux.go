//go:build linux

package terminal

// Linux primitives for the marker reap domain (see reap.go for why the boundary
// is an inherited environment variable rather than a process group).
//
// Everything here is procfs plus pidfd, the same two interfaces the rest of the
// package already uses: proctitle_linux.go reads /proc/<pid>/cmdline and comm,
// and containment_linux.go signals exclusively through a pidfd. Nothing here
// needs a capability, a writable cgroup tree, or a mount.

import (
	"bytes"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

const (
	// reapEnvMaxBytes bounds the per-pid environ read. The marker pair is
	// PREPENDED to the child environment (reap.go envPair), so it lands at the
	// front of the block and a small prefix is enough to recognise a member.
	//
	// The bound is what makes a full scan affordable: an environment can reach
	// ARG_MAX (megabytes), and reading all of it for every pid on a host running
	// thousands of processes would turn an 81ms sweep into a memory-churning one.
	reapEnvMaxBytes = 16 << 10

	// reapStatusMaxBytes bounds the /proc/<pid>/status read used for VmRSS and
	// the process state. The fields of interest sit in the first few hundred
	// bytes; the cap exists so a hostile or exotic entry cannot make a teardown
	// allocate without limit.
	reapStatusMaxBytes = 4 << 10
)

// reapFindByMarker returns every live pid whose environment carries marker.
//
// Skips this process (a server that matched its own marker would signal itself)
// and skips anything unreadable rather than reporting it: a pid that vanished
// mid-scan is the expected case, not an error, and a pid owned by another user
// is not ours to reap. A zombie is invisible here by construction — its mm is
// gone, so its environ reads empty — which is correct: an unreaped exit status
// is the zombie reaper's job, not a process to signal.
func reapFindByMarker(marker string) []int {
	if marker == "" {
		return nil
	}
	want := []byte(reapMarkerEnv + "=" + marker)
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
		if envHasMarker("/proc/"+e.Name()+"/environ", want) {
			out = append(out, pid)
		}
	}
	return out
}

// envHasMarker reports whether a bounded prefix of a procfs environ block
// contains the exact KEY=VALUE pair.
//
// Matches the whole pair, not the key: that is what makes the domain safe
// against a consumer setting the same key through WithEnv, and against a stale
// marker from an earlier session, since the value is per-session random.
func envHasMarker(path string, want []byte) bool {
	f, err := os.Open(path) // #nosec G304 -- procfs path built from a numeric pid
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.LimitReader(f, reapEnvMaxBytes))
	if err != nil || len(buf) == 0 {
		return false
	}
	for kv := range bytes.SplitSeq(buf, []byte{0}) {
		if bytes.Equal(kv, want) {
			return true
		}
	}
	return false
}

// reapAlive reports whether pid is still a live member of this domain.
//
// Re-checks the marker rather than merely testing existence, which is the
// pid-recycle guard: between two polls a pid can exit and be reused by an
// unrelated process, and treating that as "still draining" would stall teardown
// while treating it as ours would signal a stranger.
func reapAlive(pid int, marker string) bool {
	if marker == "" {
		return false
	}
	return envHasMarker("/proc/"+strconv.Itoa(pid)+"/environ", []byte(reapMarkerEnv+"="+marker))
}

// reapTerm sends SIGTERM to pid; reapKill sends SIGKILL. Both report whether the
// signal was delivered.
//
// Signalling goes through a pidfd, never a bare kill(pid), for the reason
// containment_linux.go's termLive states: a pid read from an enumeration can
// exit and be recycled before the signal lands, and this package refuses to ship
// that defect in either boundary.
func reapTerm(pid int) bool { return reapSignal(pid, unix.SIGTERM) }

func reapKill(pid int) bool { return reapSignal(pid, unix.SIGKILL) }

func reapSignal(pid int, sig unix.Signal) bool {
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return false // already gone
	}
	defer func() { _ = unix.Close(pidfd) }()
	return unix.PidfdSendSignal(pidfd, sig, nil, 0) == nil
}

// reapResident sums the resident memory of the given pids, in bytes.
//
// Reported in the reclaim WARN so the line answers "how much was this session
// still holding" — the number that makes an operator care about the leak at all.
// Best-effort: a pid that exits mid-sum contributes nothing.
func reapResident(pids []int) uint64 {
	var total uint64
	for _, pid := range pids {
		if kb, ok := statusFieldKB("/proc/"+strconv.Itoa(pid)+"/status", "VmRSS:"); ok {
			total += kb * 1024
		}
	}
	return total
}

// statusFieldKB reads one "Name: <n> kB" field out of a procfs status file.
func statusFieldKB(path, field string) (uint64, bool) {
	f, err := os.Open(path) // #nosec G304 -- procfs path built from a numeric pid
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.LimitReader(f, reapStatusMaxBytes))
	if err != nil {
		return 0, false
	}
	for line := range bytes.SplitSeq(buf, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte(field)) {
			continue
		}
		fields := bytes.Fields(line[len(field):])
		if len(fields) == 0 {
			return 0, false
		}
		kb, err := strconv.ParseUint(string(fields[0]), 10, 64)
		if err != nil {
			return 0, false
		}
		return kb, true
	}
	return 0, false
}
