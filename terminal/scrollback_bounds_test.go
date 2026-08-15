package terminal

import (
	"sync"
	"testing"

	"github.com/cplieger/web-terminal-engine/v4/vt"
)

// appendLines commits n lines to the handler's history the way the production
// paths do: under h.mu, through the ring. Tests use it instead of touching
// h.scrollback directly so the concurrency test below exercises the accessor
// against a writer that respects the same lock.
func appendLines(t *testing.T, h *Handler, n int) {
	t.Helper()
	lines := make([][]vt.WireRun, n)
	for i := range lines {
		lines[i] = []vt.WireRun{{T: "line"}}
	}
	h.mu.Lock()
	h.scrollback.Append(lines)
	h.mu.Unlock()
}

// TestScrollbackBounds pins the public accessor's contract across the three
// states a session's history can be in: nothing committed yet, some history
// retained in full, and history past the retention cap (where the oldest
// replayable index has moved off 0). The pair is what a consumer needs to know
// which absolute indices it can still ask for on resume, so a wrong oldest is a
// silently misaligned replay rather than a visible error.
func TestScrollbackBounds(t *testing.T) {
	t.Run("a fresh session reports empty bounds", func(t *testing.T) {
		h := NewHandler([]string{"/bin/true"})
		committed, oldest := h.ScrollbackBounds()
		if committed != 0 || oldest != 0 {
			t.Errorf("ScrollbackBounds() = (%d, %d), want (0, 0) before anything is committed", committed, oldest)
		}
	})

	t.Run("history within the cap is retained from index 0", func(t *testing.T) {
		h := NewHandler([]string{"/bin/true"}, WithScrollbackCapacity(10))
		appendLines(t, h, 4)
		committed, oldest := h.ScrollbackBounds()
		if committed != 4 {
			t.Errorf("committed = %d, want 4 (one past the newest committed line)", committed)
		}
		if oldest != 0 {
			t.Errorf("oldest = %d, want 0; nothing was evicted below the cap", oldest)
		}
	})

	t.Run("past the retention cap oldest tracks the eviction point", func(t *testing.T) {
		const capacity = 4
		h := NewHandler([]string{"/bin/true"}, WithScrollbackCapacity(capacity))
		appendLines(t, h, 10)
		committed, oldest := h.ScrollbackBounds()
		if committed != 10 {
			t.Errorf("committed = %d, want 10; eviction must not stall the monotonic index", committed)
		}
		if oldest != 6 {
			t.Errorf("oldest = %d, want 6 (10 committed - %d retained)", oldest, capacity)
		}
		if committed-oldest != capacity {
			t.Errorf("retained range = %d lines, want the capacity %d", committed-oldest, capacity)
		}
	})

	t.Run("with scrollback disabled the range is empty but committed advances", func(t *testing.T) {
		h := NewHandler([]string{"/bin/true"}, WithScrollbackCapacity(0))
		appendLines(t, h, 5)
		committed, oldest := h.ScrollbackBounds()
		if committed != 5 {
			t.Errorf("committed = %d, want 5; the absolute index advances even with retention off", committed)
		}
		if oldest != committed {
			t.Errorf("oldest = %d, want %d; nothing is replayable when scrollback is disabled", oldest, committed)
		}
	})

	t.Run("ED3 empties the range without rewinding committed", func(t *testing.T) {
		h := NewHandler([]string{"/bin/true"})
		appendLines(t, h, 3)
		h.handlePTYData([]byte("\x1b[3J")) // erase scrollback; generates no PTY reply
		committed, oldest := h.ScrollbackBounds()
		if committed != 3 {
			t.Errorf("committed = %d, want 3; indices stay monotonic across an erase", committed)
		}
		if oldest != 3 {
			t.Errorf("oldest = %d, want 3; an erased ring retains nothing", oldest)
		}
	})
}

// TestScrollbackBounds_concurrent is the locking half of the contract, and only
// means anything under -race: the accessor must take the same lock the commit
// path takes. It also asserts the pair is internally consistent (oldest never
// past committed, the retained range never wider than the cap) — the defect a
// two-call implementation would produce, where an eviction landing between the
// two reads yields a pair no moment in the session ever had.
func TestScrollbackBounds_concurrent(t *testing.T) {
	const (
		capacity = 8
		rounds   = 200
		readers  = 4
	)
	h := NewHandler([]string{"/bin/true"}, WithScrollbackCapacity(capacity))

	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Go(func() {
		defer close(done)
		for range rounds {
			h.mu.Lock()
			h.scrollback.Append([][]vt.WireRun{{{T: "x"}}})
			h.mu.Unlock()
		}
	})

	var mismatches int
	var mu sync.Mutex
	for range readers {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				committed, oldest := h.ScrollbackBounds()
				if oldest > committed || committed-oldest > capacity {
					mu.Lock()
					mismatches++
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()

	if mismatches != 0 {
		t.Errorf("%d observations violated oldest <= committed && committed-oldest <= %d; the pair is not read atomically", mismatches, capacity)
	}
	committed, oldest := h.ScrollbackBounds()
	if committed != rounds {
		t.Errorf("committed = %d after %d commits, want %d", committed, rounds, rounds)
	}
	if oldest != rounds-capacity {
		t.Errorf("oldest = %d, want %d", oldest, rounds-capacity)
	}
}
