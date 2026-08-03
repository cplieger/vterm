//go:build !linux

package terminal

import (
	"fmt"
	"log/slog"
	"os/exec"
)

// Containment is the non-Linux stub. Per-session containment is a cgroup v2
// feature, so it exists only on Linux; every consumer path below is a no-op so
// the package compiles and behaves identically to having the feature switched
// off.
type Containment struct{}

// NewContainment always fails on this platform. Consumers should log the error
// once and continue without containment, which is the same degradation path a
// Linux host without a writable cgroup v2 root takes.
func NewContainment(_, _ string, log *slog.Logger) (*Containment, error) {
	containLogger(log).Debug("terminal: containment unavailable (not Linux)")
	return nil, fmt.Errorf("%w: cgroup v2 is Linux-only", errContainmentUnsupported)
}

// create never succeeds, so a handler on this platform never holds a
// sessionCgroup and every method below is unreachable in practice.
func (c *Containment) create(string) (*sessionCgroup, error) {
	return nil, errContainmentUnsupported
}

// sessionCgroup is the non-Linux stub, kept so the handler's call sites compile
// unchanged across platforms.
type sessionCgroup struct{}

func (s *sessionCgroup) applyTo(*exec.Cmd)           {}
func (s *sessionCgroup) releaseFD()                  {}
func (s *sessionCgroup) teardown()                   {}
func (s *sessionCgroup) peaks() (uint64, int)        { return 0, 0 }
func (s *sessionCgroup) current() (uint64, int, int) { return 0, 0, 0 }
