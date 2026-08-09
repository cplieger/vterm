//go:build !linux

package terminal

import "errors"

// Non-Linux stubs for zombie reaping. PR_SET_CHILD_SUBREAPER and the procfs stat
// interface are both Linux, and the deployment this feature exists for is a
// Linux container whose server is PID 1, so elsewhere StartZombieReaper installs
// nothing and sweeps nothing.

func installSubreaper() error {
	return errors.New("terminal: child-subreaper is Linux-only")
}

// findOrphanZombies finds nothing, so every sweep collects nothing and the
// reaper's log line never fires.
func findOrphanZombies() []int { return nil }

func waitNoHang(int) bool { return false }
