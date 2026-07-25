package terminal

// The server-derived automatic session title: what a session is called when its
// program set no OSC window title, no client pushed a derived label, and the
// user pinned no name. It is the LAST rung of effectiveTitle's precedence.
//
// This is the standard terminal behaviour every other terminal implements and
// this engine previously lacked: name the tab after what is running in it. tmux
// does the same thing (tcgetpgrp on the pty, then read the process), and Windows
// Terminal/VS Code fall back to the shell's title or process for the same reason
// — a keystroke-derived label is a poor substitute for asking the kernel.
//
// The ladder, in order:
//
//  1. the foreground process-group leader's name, when it is not the session's
//     own root process (something is actually running: vim, ssh, npm);
//  2. the basename of the session's working directory (the shell is at rest, and
//     the cwd is what distinguishes several idle shells from each other);
//  3. the basename of the configured command (always available, every platform).
//
// Rung 3 lives here, in platform-neutral code, so a non-Linux build still names
// its sessions. Rungs 1 and 2 need the pty ioctl and procfs, so their discovery
// lives in proctitle_linux.go with a zero-valued stub in proctitle_other.go.
//
// Known limitation, inherited from tmux: the group LEADER is the representative
// process. A pipeline whose leader exits while another member keeps running
// (`sleep 1 | grep x`) leaves its procfs entry gone even though the foreground
// group is alive, so the label rests at the cwd instead. The alternative is
// scanning all of procfs for a live group member on every sweep.

import "path/filepath"

// autoTitleProbe is one sweep's worth of automatic-title inputs, gathered by
// probeAutoTitle outside every lock. The manager folds it into the confirmation
// window (see confirmAutoTitle) rather than using it directly, because a
// foreground process that lives 30ms must not flash into a tab label.
type autoTitleProbe struct {
	// err carries WHY a probe learned nothing, for the one-line-per-session debug
	// note. Only set when ok is false.
	//
	// Field order here is load-bearing for govet fieldalignment: an interface is
	// two pointer words, a string is one pointer plus a length, so putting err
	// first and the strings after it ends the struct's pointer-scan range at a
	// string's length word. Adding a field means re-checking the linter.
	err error
	// procName is the foreground process-group leader's name, or "" when the
	// session's own shell is in the foreground, when the leader is gone, or when
	// the platform cannot tell. Non-empty means "something is running".
	procName string
	// cwdBase is the basename of the session's working directory, or "" when it
	// cannot be read or was not needed (a named foreground process outranks it, so
	// the readlink is skipped entirely in that case). This is the RESTING title:
	// read from the session's root process, so it tracks the shell's own cd rather
	// than a foreground child's directory.
	cwdBase string
	// pgid identifies the candidate for the confirmation window: the window
	// restarts whenever this changes. Zero when unknown.
	pgid int
	// ok reports that the probe learned something. False (a stopped session, a
	// platform without the syscalls, a procfs that answered nothing at all) means
	// "no information this sweep", which HOLDS the last confirmed title rather
	// than clearing it. This is deliberately distinct from "probed successfully
	// and nothing is running", which is ok=true with an empty procName.
	ok bool
}

// commandBase is the automatic title's last rung: the basename of the configured
// command. Always available and immutable, so the manager seeds every session
// with it at Create time and a List served before the first sweep still names
// the session.
func (h *Handler) commandBase() string {
	if len(h.command) == 0 {
		return ""
	}
	return filepath.Base(h.command[0])
}

// probeAutoTitle gathers this sweep's automatic-title inputs. Called from the
// status sweep's lock-free phase: it takes h.mu only to snapshot the pty handle
// and the root pid (both written once by ensureStarted), then performs the ioctl
// and the procfs reads with no lock held, so a slow or unreadable procfs stalls
// only the sweep goroutine.
//
// ONLY THE STATUS SWEEP MAY CALL THIS. A second caller would get an unconfirmed,
// un-debounced sample, and if it wrote the result anywhere it would break the
// single-writer rule that lets List and the SSE snapshot agree (see
// confirmAutoTitle).
func (h *Handler) probeAutoTitle() autoTitleProbe {
	h.mu.Lock()
	ptmx := h.ptmx
	warned := h.autoTitleWarned
	var rootPID int
	if h.cmd != nil && h.cmd.Process != nil {
		rootPID = h.cmd.Process.Pid
	}
	h.mu.Unlock()
	if ptmx == nil || rootPID <= 0 {
		return autoTitleProbe{} // not started: no information, hold what we have
	}
	p := probeForeground(ptmx, rootPID)
	// One debug line per session, never per sweep: an operator on a procfs-less or
	// hidepid-restricted host otherwise has no way to tell "this platform cannot"
	// from "this build is broken", since both look like a tab named after the
	// command. Logged outside h.mu; the sweep is the only caller, so the
	// check-then-set cannot interleave.
	if p.err != nil && !warned {
		h.mu.Lock()
		h.autoTitleWarned = true
		h.mu.Unlock()
		h.cfg.logger.Debug("terminal: automatic session title unavailable, using the command name",
			"error", p.err)
	}
	return p
}

// probeForeground is the platform-specific half, implemented per build tag. It
// resolves the pty's foreground process group and the session's cwd; deciding
// what to DO with them is confirmAutoTitle's job.
//
// Both implementations return a zero autoTitleProbe rather than an error on every
// failure path: each rung of the ladder is best-effort, and a session that cannot
// be probed keeps the title it already has.
