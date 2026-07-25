//go:build linux

package terminal

// Linux discovery for the automatic session title (see proctitle.go for the
// ladder and why it exists). Two kernel interfaces:
//
//   - TIOCGPGRP on the pty master gives the foreground process group. This is
//     the exact operation tmux uses (tcgetpgrp in osdep-linux.c). An earlier
//     draft parsed field 8 (tpgid) out of /proc/<pid>/stat to avoid a new
//     dependency; golang.org/x/sys/unix is the cheaper answer — it is the Go
//     project's maintained syscall layer, four sibling repos in this fleet
//     already depend on it directly, and it deletes a hand-written parser of a
//     kernel text format whose second field can itself contain spaces and
//     parentheses.
//   - procfs for the name and the cwd. cmdline (not comm) is read for the name,
//     also matching tmux, because the kernel truncates comm at TASK_COMM_LEN
//     (16 bytes including the NUL); comm is the fallback for a process with an
//     empty cmdline.
//
// Everything here is best-effort: any failure returns a zero value and the
// caller falls down the ladder.

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// procNameMaxBytes bounds the cmdline read. argv[0] is a path; anything past a
// kilobyte is not a name we would render in a tab chip, and the bound keeps a
// hostile argv from being copied into memory in full.
const procNameMaxBytes = 1024

func probeForeground(ptmx *os.File, rootPID int) autoTitleProbe {
	p := autoTitleProbe{ok: true, cwdBase: processCwdBase(rootPID)}
	pgid, err := foregroundPGID(ptmx)
	if err != nil || pgid <= 0 {
		return p // no controlling terminal, or the ioctl failed: rest at the cwd
	}
	p.pgid = pgid
	if pgid == rootPID {
		return p // the session's own shell is in the foreground; nothing is running
	}
	p.procName = processName(pgid)
	return p
}

// foregroundPGID reads the pty's foreground process group.
//
// SyscallConn().Control rather than ptmx.Fd(): os.File.Fd() switches the
// descriptor to blocking mode, and this pty is being read by a live goroutine.
// Control hands the raw fd to the closure while keeping the file registered with
// the runtime poller. (pty.Setsize already calls Fd() on the same handle, so the
// package has been living with that; new code should not add to it.)
func foregroundPGID(ptmx *os.File) (int, error) {
	rc, err := ptmx.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pgid int
	var ctlErr error
	if err := rc.Control(func(fd uintptr) {
		pgid, ctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
	}); err != nil {
		return 0, err
	}
	if ctlErr != nil {
		return 0, ctlErr
	}
	return pgid, nil
}

// processName returns the basename of a process's argv[0], falling back to its
// comm. Empty when the process is gone (the documented pipeline case: the group
// leader exited while another member runs) or unreadable.
//
// The result is SANITIZED before it is returned. argv[0] is chosen by whoever
// started the process, so on a shared terminal it is untrusted input that ends
// up in a tab label, an SSE frame, and any log line that carries the title —
// exactly the CWE-117 shape the client-supplied titles are already bounded for.
// A name is additionally required to be valid UTF-8, since procfs hands back raw
// bytes and an invalid sequence would reach JSON encoding as replacement
// characters.
func processName(pid int) string {
	if argv0 := readArgv0(procPath(pid, "cmdline")); argv0 != "" {
		return cleanProcName(filepath.Base(argv0))
	}
	// comm is a single line, already a bare name (kernel-truncated to 15 bytes).
	if comm, err := os.ReadFile(procPath(pid, "comm")); err == nil { // #nosec G304 -- procPath(int, literal)
		return cleanProcName(string(bytes.TrimSpace(comm)))
	}
	return ""
}

// procNameMaxRunes bounds a process-derived label. A basename longer than this is
// not a tab label anyone reads, and the bound keeps a hostile argv out of every
// SSE frame at full length.
const procNameMaxRunes = 64

// cleanProcName drops control characters and DEL, rejects invalid UTF-8 outright,
// and bounds the length. Returning "" on invalid input is deliberate: the ladder
// treats empty as "no name available" and falls to the next rung, which is a
// better outcome than rendering a mangled one.
func cleanProcName(s string) string {
	if !utf8.ValidString(s) {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= procNameMaxRunes {
			break
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// readArgv0 extracts argv[0] from a NUL-separated procfs cmdline. Returns "" for
// a missing, empty, or all-NUL cmdline (a kernel thread has no argv), and also
// when no NUL appears within the bounded read: without a terminator we cannot
// tell a complete argv[0] from a truncated prefix, and half a path is a worse
// label than falling through to comm.
func readArgv0(path string) string {
	f, err := os.Open(path) // #nosec G304 -- path is procPath(int, literal), not user input
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, procNameMaxBytes)
	n, err := f.Read(buf)
	if n <= 0 || (err != nil && n == 0) {
		return ""
	}
	argv0, _, found := bytes.Cut(buf[:n], []byte{0})
	if !found {
		return ""
	}
	return string(argv0)
}

// processCwdBase returns the basename of a process's working directory, or "" if
// the symlink cannot be read. Unreadable is normal rather than exceptional:
// readlink on procfs cwd is governed by PTRACE_MODE_READ_FSCREDS, so a child that
// changed credentials is opaque, and a procfs mounted with hidepid hides other
// users' entries entirely.
func processCwdBase(pid int) string {
	cwd, err := os.Readlink(procPath(pid, "cwd"))
	if err != nil || cwd == "" {
		return ""
	}
	// filepath.Base("/") is "/", which is the right label for a session at the
	// filesystem root rather than an empty one.
	return filepath.Base(cwd)
}

// procPath builds a procfs path for a pid. Plain concatenation rather than
// filepath.Join: these are fixed kernel paths with no cleaning to do, and Join's
// first argument is conventionally a directory without embedded separators.
func procPath(pid int, leaf string) string {
	return "/proc/" + strconv.Itoa(pid) + "/" + leaf
}
