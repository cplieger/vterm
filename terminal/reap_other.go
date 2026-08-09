//go:build !linux

package terminal

// Non-Linux stubs for the marker reap domain. The boundary is a /proc scan for
// an inherited environment variable, and no other platform exposes another
// process's environment through the filesystem, so the domain finds nothing and
// every step of the ladder degrades to a no-op.
//
// The marker is still injected into the child environment on these platforms:
// it is inert, costs one variable, and keeps the spawn path identical across
// builds rather than platform-forked at the one place a divergence would be
// hardest to test.

// reapFindByMarker finds nothing off Linux, which makes teardownOnce return
// after its first pass without signalling or logging.
func reapFindByMarker(string) []int { return nil }

// reapAlive reports nothing alive, so a settle window drains immediately.
func reapAlive(int, string) bool { return false }

func reapTerm(int) bool { return false }

func reapKill(int) bool { return false }

func reapResident([]int) uint64 { return 0 }
