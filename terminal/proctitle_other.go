//go:build !linux

package terminal

// Non-Linux stub for the automatic session title's platform-specific discovery
// (see proctitle.go). The foreground process group and the working directory both
// need a pty ioctl plus procfs, so on any other platform the ladder skips
// straight to its portable last rung, the configured command's basename, which
// the manager already seeded at Create time.
//
// ok=false, not a zero probe with ok=true: "no information this sweep" makes the
// confirmation window hold the seeded title, whereas "probed and found nothing"
// would invite it to clear.

import "os"

func probeForeground(_ *os.File, _ int) autoTitleProbe {
	return autoTitleProbe{}
}
