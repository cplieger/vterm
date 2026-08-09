package terminal

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Zombie reaping is a separate question from session reaping, and conflating the
// two is how it stayed unfixed. Session reaping ends processes that are still
// ALIVE after their session closed (see reap.go). Zombie reaping collects an
// exit status nobody called wait() for; it cannot touch a live process and it
// frees no memory. What it frees is a PID slot and a kernel task struct.
//
// Why the engine has to own it: a server that runs as its container's PID 1
// inherits every orphan in the container by re-parenting, and Go's os/exec waits
// only on the children IT created. So every process whose own parent died —
// which for an agent shell means every language server, every git it forked, the
// lot — becomes a permanent zombie parked on the server. Measured on the
// container this was written for: 17,323 zombies (12,508 git, 3,155 gopls)
// against 88 live processes, all with the server as their parent, none of them
// reachable by any consumer's own wait().
//
// The dangerous version of this feature is a bare wait(-1) loop. os/exec owns
// the head pid of every session and its exit status IS the engine's exit
// contract (ExitError, the exited/crashed split, onProcessExit). A generic
// reaper that got there first would make cmd.Wait return "no child processes"
// and silently turn every clean exit into an unknown one. So the sweep below
// never waits on a pid the engine spawned, and the registry that decides is
// written under a lock the spawn path holds across the fork itself — which is
// what closes the window where a child could be born, exit, and be stolen before
// anyone registered it.
//
// Deliberately a periodic sweep rather than a SIGCHLD handler: signal.Notify is
// process-global state, SIGCHLD is what the Go runtime itself uses to drive
// os/exec, and a zombie costs nothing for a few seconds. A library should not
// reach for a process-wide signal disposition to solve a bounded hygiene
// problem.

const (
	// zombieSweepDefault is the sweep interval when a consumer passes 0. Long
	// enough that the scan is free at any plausible process count, short enough
	// that a pathological forker cannot exhaust pid_max between passes.
	zombieSweepDefault = 30 * time.Second

	// zombieSweepFloor is the lower bound on a consumer-supplied interval. The
	// sweep walks /proc, so a caller asking for milliseconds would spend real CPU
	// re-reading the same thousands of entries to no benefit.
	zombieSweepFloor = time.Second
)

// spawnRegistry records the pids whose exit status os/exec still owns, so the
// zombie sweep can refuse to wait on them.
//
// The mutex does double duty and the second job is the load-bearing one: the
// spawn path holds it for WRITE across pty.StartWithSize and the registration
// that follows, so no pid exists un-registered while the sweep can see it, and
// the sweep holds it for READ across its ownership check and the wait() that
// follows, so a registration cannot interleave between the two.
type spawnRegistry struct {
	m  map[int]struct{}
	mu sync.RWMutex
}

var spawned = spawnRegistry{m: make(map[int]struct{})}

// spawnLock/spawnUnlock bracket a spawn. Held across the fork, not just the map
// write — see the comment on spawnRegistry.
func spawnLock()   { spawned.mu.Lock() }
func spawnUnlock() { spawned.mu.Unlock() }

// spawnRegister records a freshly spawned pid. The caller holds the write lock.
func spawnRegister(pid int) {
	if pid > 0 {
		spawned.m[pid] = struct{}{}
	}
}

// spawnForget releases a pid once os/exec has reaped it, after which the sweep
// may collect it if it somehow reappears as an orphan's parent.
func spawnForget(pid int) {
	spawned.mu.Lock()
	delete(spawned.m, pid)
	spawned.mu.Unlock()
}

// reapIfUnowned waits on pid unless os/exec owns it, reporting whether a status
// was collected.
func reapIfUnowned(pid int) bool {
	spawned.mu.RLock()
	defer spawned.mu.RUnlock()
	if _, owned := spawned.m[pid]; owned {
		return false
	}
	return waitNoHang(pid)
}

// StartZombieReaper makes this process collect the exit statuses of orphans that
// have been re-parented onto it, and returns a stop function.
//
// Wire it from the composition root of a server that is (or may become) its
// container's PID 1. It installs the child-subreaper flag so orphans re-parent
// here even when the process is NOT PID 1 — behind an init shim, for example —
// and then sweeps every interval, waiting only on pids the engine did not spawn.
//
// interval of 0 means the default; anything below one second is raised to it.
// Off Linux this is a no-op returning a no-op stop, so a consumer wires it
// unconditionally.
//
// Safe to call once per process. Calling it twice starts two sweeps, which is
// harmless (wait() on an already-collected pid simply fails) but pointless.
func StartZombieReaper(log *slog.Logger, interval time.Duration) (stop func()) {
	l := containLogger(log)
	if interval <= 0 {
		interval = zombieSweepDefault
	}
	interval = max(interval, zombieSweepFloor)

	if err := installSubreaper(); err != nil {
		// Not fatal, and not even necessarily a degradation: when the process is
		// already PID 1 the flag changes nothing, and orphans arrive regardless.
		l.Debug("terminal: child-subreaper flag not set", "error", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := sweepZombies(); n > 0 {
					// INFO, not WARN: on a PID 1 server this is routine hygiene
					// for other people's children, not a fault of this process.
					// It stays visible because a sustained nonzero count is the
					// signature of a child runtime that never reaps its own.
					l.Info("terminal: reaped orphaned processes", "count", n)
				}
			}
		}
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

// sweepZombies collects every orphan zombie parented on this process that the
// engine does not own, returning how many statuses it took.
func sweepZombies() int {
	n := 0
	for _, pid := range findOrphanZombies() {
		if reapIfUnowned(pid) {
			n++
		}
	}
	return n
}
