package terminal

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// Containment is the per-session process-containment feature: every session's
// process tree is placed in its own cgroup at clone time, and the cgroup is what
// ends the session.
//
// It exists because a PTY child can escape every signal-scoped boundary the
// engine otherwise has. A process that calls setsid() leaves both its process
// group and its session, so neither kill(-pgid) nor the PTY-close SIGHUP (which
// the kernel delivers to the controlling terminal's foreground process group) can
// reach it. Observed in the wild: an agent runtime that setsid()s away, installs
// no stdin-EOF exit path, and therefore outlives its session indefinitely while
// holding hundreds of megabytes. A cgroup is not escapable by forking,
// double-forking, or setsid(), so it is the only boundary that holds.
//
// Platform support is Linux with cgroup v2 only. Everywhere else NewContainment
// returns an error and the feature stays off; a consumer that never enables it,
// or enables it on an unsupported host, gets byte-for-byte the previous behavior.
//
// Concretely, containment gives a session:
//
//   - deterministic teardown: SIGTERM to whatever survived, then cgroup.kill as
//     the backstop, so nothing is left behind regardless of how the child tree
//     behaves;
//   - a true memory high-water mark (memory.peak) for the logs, which is only
//     meaningful because the child is placed at clone time rather than migrated
//     afterwards (cgroup v2 does not move existing charges on migration).
//
// It deliberately does NOT reap idle sessions or impose a per-session memory
// ceiling. A ceiling would make the kernel OOM-kill a live session, and ending a
// session that a user might still be reading is exactly what the session
// manager's idle reaper is for; that remains a separate, opt-in decision
// (WithIdleReaper).
//
// Two SEPARATE features cover what this one does not, and keeping them apart is
// what makes each answerable: reap.go reclaims a closed session's surviving tree
// with no host support at all (on by default, and the boundary that works when
// this one is unavailable), and zombiereap.go collects the exit statuses of
// orphans re-parented onto a server that is its container's PID 1
// (StartZombieReaper, opt-in from the composition root).

// errContainmentUnsupported is returned by NewContainment on any platform or host
// that cannot support the feature. Consumers should log it once and continue
// without containment rather than failing to start.
var errContainmentUnsupported = errors.New("terminal: per-session containment unsupported on this host")

// CgroupRoot is the writable cgroup v2 mount NewContainment reshapes (vacating
// processes, enabling controllers). A distinct type rather than a second string
// because it sits beside the prefix in NewContainment's signature and a swapped
// pair is a CONFINEMENT change, not a typo: pointing the reshape at the wrong
// directory relocates unrelated workloads. With the root typed, swapped
// variables fail to compile, and a swapped literal announces itself at the call
// site (CgroupRoot("wt-") reads as wrong) before the runtime checks refuse it.
type CgroupRoot string

const (
	// containGrace bounds each of the two settle windows in teardown. Measured
	// exit latency for the runtime that motivated this feature is ~100ms after
	// SIGTERM, so a wedged tree costs at most 2*containGrace before the
	// cgroup.kill backstop fires.
	containGrace = 250 * time.Millisecond

	// containServerLeaf is the cgroup the server moves itself (and anything else
	// found in the delegated root) into. cgroup v2's "no internal processes"
	// constraint forbids enabling domain controllers on a cgroup that holds
	// processes, so the root must be emptied before session cgroups can account
	// anything.
	containServerLeaf = "server"

	// containProbeLeaf is the throwaway cgroup the startup probes use. Reserved
	// against session ids for the same reason containServerLeaf is.
	containProbeLeaf = "probe"

	// containMaxIDLen bounds a sanitized session id used as a directory name.
	containMaxIDLen = 64
)

// WithContainment places this session's process tree in its own cgroup under c
// and makes that cgroup the unit of teardown. id names the cgroup (sanitized;
// see sanitizeCgroupName) and should be the session id the consumer already uses
// for logging, so a cgroup on disk can be matched to a session in the logs.
//
// A nil c is a no-op, which is what makes this safe to wire unconditionally: a
// consumer can pass the result of NewContainment straight through without
// branching on whether the host supported it.
//
// Teardown runs exactly once per handler, owned by the process monitor after
// cmd.Wait returns. Close deliberately does not run it: Close holds the
// handler lock, and cancelling the context kills the head process, so the
// monitor's Wait returns and teardown happens there for both paths.
func WithContainment(c *Containment, id SessionID) Option {
	return func(cfg *handlerConfig) {
		cfg.containment = c
		cfg.containmentID = id
	}
}

// WithContainmentSampleInterval logs a live cost line per contained session on
// this interval (session id, current memory, pid count, age). Zero, the default,
// logs only at session end.
//
// Worth enabling for long-lived sessions, where "what is this session costing
// right now" otherwise has no answer short of inspecting the container by hand.
// The line is also the only visibility into a session whose child runtime has
// died but whose PTY process is still alive: containment reclaims at teardown and
// never earlier, so such a session keeps holding its memory for as long as it
// stays open, by design.
func WithContainmentSampleInterval(d time.Duration) Option {
	return func(cfg *handlerConfig) { cfg.containSample = d }
}

// sanitizeCgroupName reduces name to a single safe path segment for use as a
// cgroup directory name, or returns "" if nothing usable remains.
//
// It takes a string rather than a SessionID because it serves two callers: the
// session leaf (Containment.create, which converts) and the cgroup path PREFIX
// validated in NewContainment, which is not a session id and only shares the
// whitelist. The function sanitizes a name; it does not identify a session.
//
// This is a whitelist, not an escape: a cgroup name reaches mkdir under a root
// the server owns, so a traversal or an absolute path must be impossible rather
// than merely unlikely. Everything outside [A-Za-z0-9_-] is dropped, which
// removes "/", "..", NUL and every unicode surprise in one rule.
func sanitizeCgroupName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
		if b.Len() >= containMaxIDLen {
			break
		}
	}
	return b.String()
}

// containLogger returns a non-nil logger, matching WithLogger's handling of a nil
// logger so containment code can log unconditionally without nil checks.
func containLogger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.New(slog.DiscardHandler)
	}
	return l
}

// startCostSampler launches the periodic cost line for a contained session when
// the consumer asked for one. A no-op otherwise, so the caller needs no branch.
// Returns a stop function the process monitor calls BEFORE teardown: the
// handler's context is only cancelled in the monitor's deferred block, which runs
// after teardown and after the consumer's exit callback, so a tick in that window
// would read a removed cgroup and log a zero-cost line for a session that is gone.
func (h *Handler) startCostSampler(ctx context.Context) (stop func()) {
	if h.contain == nil || h.cfg.containSample <= 0 {
		return func() {}
	}
	sampleCtx, cancel := context.WithCancel(ctx)
	h.wg.Go(func() { h.costSampleLoop(sampleCtx, h.cfg.containSample) })
	return cancel
}

// costSampleLoop logs one cost line per interval for a contained session, until
// the session's context is cancelled.
//
// This answers "what is this session costing right now", which otherwise has no
// answer short of inspecting the container by hand. It is the only visibility
// into a session whose child runtime has died while its PTY process is still
// alive: containment reclaims at teardown and never earlier, so that session
// keeps holding its memory for as long as it stays open, deliberately, and the
// decision to close it stays the user's rather than a timer's.
//
// The fields are named for what the kernel actually reports. tasks is
// pids.current, which counts TASKS: one 11-thread process reports 11 (measured),
// and it also counts a task that exited without being reaped. Those two are not
// separable from any cgroup file, so this does NOT derive a zombie count from
// them; an earlier version did, and reported 10 phantom zombies for one healthy
// multi-threaded child.
func (h *Handler) costSampleLoop(ctx context.Context, interval time.Duration) {
	started := time.Now()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			mem, tasks, procs := h.contain.current()
			h.cfg.logger.Info("terminal: session cost",
				"session", LogID(h.cfg.containmentID),
				"mem_bytes", mem,
				"procs", procs,
				"tasks", tasks,
				"age_s", int(time.Since(started).Seconds()))
		}
	}
}

// beginContainment creates this session's cgroup and points cmd at it so the
// kernel places the child at clone time, returning nil when containment is off or
// unavailable for this session.
//
// Clone-time placement is not a nicety: cgroup v2 does not migrate existing
// charges, so a child moved in after it starts reports memory it never accounted
// and its peak is meaningless. pty.StartWithSize preserves the SysProcAttr this
// sets (it only adds Setsid/Setctty), so the PTY semantics are unchanged.
//
// A failure is never fatal. An uncontained session is exactly the behavior of a
// consumer that never enabled the feature, and a terminal the user can still
// reach beats a correct refusal.
func (h *Handler) beginContainment(cmd *exec.Cmd) *sessionCgroup {
	if h.cfg.containment == nil {
		return nil
	}
	sc, err := h.cfg.containment.create(h.cfg.containmentID)
	if err != nil {
		h.cfg.logger.Warn("terminal: containment unavailable for session",
			"session", LogID(h.cfg.containmentID), "error", err)
		return nil
	}
	sc.applyTo(cmd)
	return sc
}
