package terminal

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Session reaping is the engine's capability-free answer to the same problem
// containment solves with a cgroup: a PTY child can escape every signal-scoped
// boundary the engine otherwise has. A process that calls setsid() leaves both
// its process group and its session, so neither kill(-pgid) nor the PTY-close
// SIGHUP (delivered to the controlling terminal's FOREGROUND process group) can
// reach it, and a process re-parented to init has no ancestry left to walk.
//
// The boundary used here is an inherited environment marker. Every session's
// child is spawned with one unguessable variable, execve copies the environment
// into every descendant for free, and the marker survives exactly the two events
// that defeat the alternatives: setsid() does not clear the environment, and
// neither does re-parenting. So "every process belonging to this session" is a
// /proc scan, with no capability, no cgroup, no mount and no PID namespace.
//
// Measured on the container this was written for: a full scan of 17,547 pids
// costs ~81ms, the marker was inherited by every member of a tree whose KAS
// runtime had setsid()'d away, and a process-group kill reached 1 of the 2
// process groups the same tree spanned.
//
// Two honest limits, neither of which the alternatives improve on for free:
//
//   - A descendant that execve()s with a deliberately scrubbed environment
//     (env -i) escapes the domain. Nothing short of a cgroup or a PID namespace
//     catches that, which is why Containment still exists as the stronger,
//     opt-in boundary for hosts willing to grant it.
//   - The scan enumerates, so a tree that forks DURING teardown can outrun one
//     pass. The ladder below rescans before each escalation rather than reusing
//     the first pass's pid set, which bounds it to a fork racing the final
//     SIGKILL round.
//
// Reaping is unconditional: leaving a closed session's tree alive is the defect
// rather than a feature. (An opt-out shipped through v4 as WithoutSessionReap;
// it had no caller anywhere in the fleet and was removed at v5 — work meant to
// outlive a tab belongs outside the session's process tree, e.g. a detached
// service, not behind an engine knob.)

const (
	// reapMarkerEnv names the variable every session's process tree inherits.
	// The engine owns this key: a consumer's WithEnv cannot usefully shadow it,
	// because the scan matches the full KEY=VALUE pair and the value is random.
	//
	// It keeps the WT_ prefix, which the operator-facing knobs (ScrollbackEnvVar
	// and the consumer convention in the README) dropped. This key is not a knob:
	// the engine INJECTS it into the child environment, where it shares one flat
	// namespace with everything else the system and the user's shell set, so the
	// prefix is real disambiguation rather than a leaked component name.
	reapMarkerEnv = "WT_SESSION_REAP"

	// reapMarkerBytes sizes the random marker value. 16 bytes is far past
	// collision relevance for a per-session token and keeps the whole pair
	// inside the first procfs read (see reapEnvMaxBytes).
	reapMarkerBytes = 16

	// reapPoll is the liveness poll interval inside a settle window. Bounded by
	// containGrace, so at most containGrace/reapPoll cheap per-pid checks —
	// never a full scan, which is why the settle window costs microseconds
	// rather than another 81ms pass.
	reapPoll = 20 * time.Millisecond
)

// sessionReap is one session's reap domain: the marker its tree carries, plus
// the once-guard that makes teardown idempotent across the crash-then-close
// sequence.
type sessionReap struct {
	log    *slog.Logger
	id     SessionID
	marker string
	once   sync.Once
}

// newSessionReap mints a reap domain for one session, or returns nil when the
// marker could not be generated.
//
// A failed marker is a nil domain rather than a fabricated one: a predictable
// or empty marker would either match nothing or, worse, match another session's
// tree, and no reaping is strictly better than reaping the wrong processes.
//
// The domain borrows its LOG LABEL from containmentID, which is the only session
// identity a bare Handler has (WithContainment sets it, and a nil Containment
// still sets the id — so a consumer that wants labelled reap lines without
// cgroups passes WithContainment(nil, id), which is exactly what a consumer on a
// host without a writable cgroup root already does). An unlabelled session logs
// an empty one rather than inventing a name.
func (h *Handler) newSessionReap() *sessionReap {
	buf := make([]byte, reapMarkerBytes)
	if _, err := rand.Read(buf); err != nil {
		containLogger(h.cfg.logger).Warn("terminal: session reaping unavailable for session",
			"session", LogID(h.cfg.containmentID), "error", err)
		return nil
	}
	return &sessionReap{
		log:    containLogger(h.cfg.logger),
		id:     h.cfg.containmentID,
		marker: hex.EncodeToString(buf),
	}
}

// envPair returns the marker assignment to prepend to the child environment, or
// "" for a nil domain so the caller needs no branch.
//
// Prepended rather than appended, deliberately: it puts the pair at the front of
// /proc/<pid>/environ, which is what lets the scan read a bounded prefix instead
// of a whole ARG_MAX environment for every pid on the host.
func (s *sessionReap) envPair() string {
	if s == nil {
		return ""
	}
	return reapMarkerEnv + "=" + s.marker
}

// stripReapMarker drops any assignment to the marker key from an environment
// slice.
//
// This is not tidiness, it is what makes the marker authoritative. os/exec
// deduplicates the environment it passes to execve and keeps the LAST value for
// a repeated key, so any assignment appended after the engine's own prepended
// marker replaces it outright. The session's tree would then carry a marker the
// engine never minted, the scan would match nothing, and reaping would be
// silently off for that session.
//
// The spawn path applies it to BOTH sources it composes (childEnv): a consumer's
// WithEnv, and the process's own inherited environment. The inherited one matters
// more, because it is the source the engine does not control: a server started
// from inside one of these sessions inherits that session's live marker. Two
// tests pin the pair — TestReapMarkerSurvivesADuplicateKeyFromWithEnv and
// TestReapMarkerSurvivesAnInheritedMarker; without either strip, one fails.
func stripReapMarker(env []string) []string {
	prefix := reapMarkerEnv + "="
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}

// teardown reclaims whatever the session's tree left behind. Nil-safe and
// exactly-once, matching sessionCgroup.teardown so the monitor can call both
// unconditionally.
func (s *sessionReap) teardown() {
	if s == nil {
		return
	}
	s.once.Do(s.teardownOnce)
}

// teardownOnce is the escalation ladder: settle, SIGTERM, settle, SIGKILL.
//
// It mirrors sessionCgroup.teardownOnce step for step, including the WARN's
// field names, so an operator reading logs cannot tell which boundary reclaimed
// a session — only that one did. The one structural difference is that a cgroup
// has a pollable cgroup.events file and a marker domain does not, so each settle
// window polls the known members instead of waiting on the kernel.
//
// The common case is one scan and no log line at all: a well-behaved tree is
// already gone when the head is reaped, so the first pass finds nothing.
func (s *sessionReap) teardownOnce() {
	members := reapFindByMarker(s.marker)
	if len(members) == 0 {
		return
	}
	// cmd.Wait returning means the HEAD was reaped, not that the tree finished.
	// Give the ordinary exit path its window before signalling anything.
	if s.waitGone(members, containGrace) {
		if members = reapFindByMarker(s.marker); len(members) == 0 {
			return
		}
	}

	residentBytes := reapResident(members)
	survivors := len(members)
	termed := 0
	for _, pid := range members {
		if reapTerm(pid) {
			termed++
		}
	}

	forced := 0
	if !s.waitGone(members, containGrace) {
		left := reapFindByMarker(s.marker)
		forced = len(left)
		for _, pid := range left {
			reapKill(pid)
		}
		s.waitGone(left, containGrace)
	}

	// Fires only when something had to be reclaimed. term_reclaimed and
	// kill_forced separate a tree that honoured SIGTERM from one that had to be
	// killed.
	s.log.Warn("terminal: session reap reclaimed escaped processes",
		"session", LogID(s.id),
		"survivors", survivors,
		"term_reclaimed", max(termed-forced, 0),
		"kill_forced", forced,
		"resident_bytes", residentBytes)
}

// waitGone polls the given pids until none is still a live member of this
// domain, or the budget expires. Reports whether the set drained.
//
// Checks only the pids handed to it, never the whole host: the expensive full
// scan happens once per escalation step, not once per poll.
func (s *sessionReap) waitGone(pids []int, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		alive := false
		for _, pid := range pids {
			if reapAlive(pid, s.marker) {
				alive = true
				break
			}
		}
		if !alive {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(reapPoll)
	}
}
