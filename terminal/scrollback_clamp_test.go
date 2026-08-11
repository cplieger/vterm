package terminal

import (
	"strconv"
	"strings"
	"testing"
)

// ClampScrollbackCapacity is consumer-facing API — three apps share one env var
// and delegate its awkward middle here precisely so they cannot each invent an
// interpretation — and it shipped with no test of its own. The apps' tests cover
// their own plumbing, not this policy.
func TestClampScrollbackCapacity(t *testing.T) {
	tests := []struct {
		name       string
		in         int
		wantCap    int
		wantReason bool
	}{
		// 0 is a coherent request and passes through: a client cannot page
		// against a server with no history, so there is no inverted outcome to
		// protect the operator from.
		{"zero disables retention and is obeyed silently", 0, 0, false},
		{"negative is not a depth, so it reads as disabled and says so", -1, 0, true},
		{"a depth that can offer paging passes through", MinPagingCapacity, MinPagingCapacity, false},
		{"the default passes through", DefaultScrollbackCapacity, DefaultScrollbackCapacity, false},
		// The middle: honoured by the ring, too shallow to declare paging, and
		// the resulting client behavior is the OPPOSITE of what was asked for —
		// less server history buys more phone memory. Clamped up and explained.
		{"one line is raised to the paging floor", 1, MinPagingCapacity, true},
		{"just below the floor is raised", MinPagingCapacity - 1, MinPagingCapacity, true},
		// No upper bound: the ring allocates only what it fills, so an absurd
		// number is the supported way to say "never truncate".
		{"an absurd depth is honoured, because that is how you say never truncate", 1 << 30, 1 << 30, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := ClampScrollbackCapacity(tc.in)
			if got != tc.wantCap {
				t.Errorf("capacity = %d, want %d", got, tc.wantCap)
			}
			if (reason != "") != tc.wantReason {
				t.Errorf("reason = %q, want a reason: %v", reason, tc.wantReason)
			}
			if reason == "" {
				return
			}
			// The reason is read by an operator in a log line, so it must name the
			// variable they set and the number they set it to — a bare "adjusted"
			// sends them to the source.
			if !strings.Contains(reason, ScrollbackEnvVar) {
				t.Errorf("reason %q does not name %s", reason, ScrollbackEnvVar)
			}
			if !strings.Contains(reason, strconv.Itoa(tc.in)) {
				t.Errorf("reason %q does not quote the supplied value %d", reason, tc.in)
			}
		})
	}
}

// The clamp's output must always be a capacity the handler will actually declare
// paging for, or zero. That is the whole point of the middle being clamped, and
// it is a property of two constants that a future edit could move apart.
func TestClampScrollbackCapacityAlwaysPageableOrOff(t *testing.T) {
	for _, in := range []int{-5, 0, 1, 2, 999, MinPagingCapacity - 1, MinPagingCapacity, 20000, DefaultScrollbackCapacity} {
		capacity, _ := ClampScrollbackCapacity(in)
		if capacity == 0 {
			continue // retention disabled: nothing to page for
		}
		h := NewHandler([]string{"/bin/cat"}, WithScrollbackCapacity(capacity), WithLogger(nil))
		if !h.historyPagingDeclared() {
			t.Errorf("clamp(%d) = %d, which does not declare paging: the clamp exists to make that impossible", in, capacity)
		}
	}
}
