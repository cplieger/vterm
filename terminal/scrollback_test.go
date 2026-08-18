package terminal

import (
	"fmt"
	"slices"
	"testing"

	"github.com/cplieger/web-terminal-engine/v5/vt"
)

func makeLine(text string) []vt.WireRun {
	return []vt.WireRun{{T: text, F: -1, B: -1, Uc: -1}}
}

func TestScrollbackRing_Basic(t *testing.T) {
	r := newScrollbackRing(5)
	if r.Len() != 0 {
		t.Fatalf("expected empty ring, got %d", r.Len())
	}

	r.Append([][]vt.WireRun{makeLine("a"), makeLine("b"), makeLine("c")})
	if r.Len() != 3 {
		t.Fatalf("expected 3, got %d", r.Len())
	}

	lines := r.Lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0][0].T != "a" || lines[1][0].T != "b" || lines[2][0].T != "c" {
		t.Fatalf("unexpected content: %v", lines)
	}
}

func TestScrollbackRing_Eviction(t *testing.T) {
	r := newScrollbackRing(3)
	r.Append([][]vt.WireRun{makeLine("a"), makeLine("b"), makeLine("c")})
	r.Append([][]vt.WireRun{makeLine("d"), makeLine("e")})

	if r.Len() != 3 {
		t.Fatalf("expected 3 (capped), got %d", r.Len())
	}
	lines := r.Lines()
	if lines[0][0].T != "c" || lines[1][0].T != "d" || lines[2][0].T != "e" {
		t.Fatalf("expected [c,d,e], got %v", []string{lines[0][0].T, lines[1][0].T, lines[2][0].T})
	}
}

func TestScrollbackRing_Clear(t *testing.T) {
	r := newScrollbackRing(5)
	r.Append([][]vt.WireRun{makeLine("a"), makeLine("b")})
	r.Clear()
	if r.Len() != 0 {
		t.Fatalf("expected 0 after clear, got %d", r.Len())
	}
	if lines := r.Lines(); lines != nil {
		t.Fatalf("expected nil lines after clear, got %v", lines)
	}
}

func TestScrollbackRing_WrapAround(t *testing.T) {
	r := newScrollbackRing(4)
	// Fill completely
	r.Append([][]vt.WireRun{makeLine("1"), makeLine("2"), makeLine("3"), makeLine("4")})
	// Overwrite oldest two
	r.Append([][]vt.WireRun{makeLine("5"), makeLine("6")})

	lines := r.Lines()
	if len(lines) != 4 {
		t.Fatalf("expected 4, got %d", len(lines))
	}
	expected := []string{"3", "4", "5", "6"}
	for i, exp := range expected {
		if lines[i][0].T != exp {
			t.Errorf("lines[%d] = %q, want %q", i, lines[i][0].T, exp)
		}
	}
}

// TestScrollbackRing_AbsoluteIndices verifies the absolute-index accounting
// that the resume protocol depends on: Committed advances monotonically,
// OldestIndex tracks eviction, and indices never repeat.
//
// The subtests are ordered stages over ONE ring — each appends to the state
// the previous left — so they must run in sequence and do not pass alone
// under -run. That is the point: the accounting being tested IS the
// accumulation across appends.
func TestScrollbackRing_AbsoluteIndices(t *testing.T) {
	r := newScrollbackRing(5)
	t.Run("fresh ring starts at zero", func(t *testing.T) {
		if r.Committed() != 0 || r.OldestIndex() != 0 {
			t.Errorf("fresh ring: committed=%d oldest=%d, want 0/0", r.Committed(), r.OldestIndex())
		}
	})
	t.Run("appends advance committed, not oldest", func(t *testing.T) {
		r.Append([][]vt.WireRun{makeLine("a"), makeLine("b"), makeLine("c")})
		if r.Committed() != 3 {
			t.Errorf("after 3 appends: committed=%d, want 3", r.Committed())
		}
		if r.OldestIndex() != 0 {
			t.Errorf("no eviction yet: oldest=%d, want 0", r.OldestIndex())
		}
	})
	t.Run("overflow keeps committed growing and advances oldest", func(t *testing.T) {
		r.Append([][]vt.WireRun{makeLine("d"), makeLine("e"), makeLine("f"), makeLine("g")})
		if r.Committed() != 7 {
			t.Errorf("after 7 appends: committed=%d, want 7", r.Committed())
		}
		if r.OldestIndex() != 2 {
			t.Errorf("after evicting 2: oldest=%d, want 2 (committed 7 - count 5)", r.OldestIndex())
		}
	})
}

// TestScrollbackRing_LinesFrom verifies index-aligned replay: LinesFrom returns
// the retained tail at and after a given absolute index, with the true first
// index so the caller can detect an eviction gap.
func TestScrollbackRing_LinesFrom(t *testing.T) {
	r := newScrollbackRing(5)
	r.Append([][]vt.WireRun{makeLine("0"), makeLine("1"), makeLine("2"), makeLine("3"), makeLine("4")})

	t.Run("exact alignment", func(t *testing.T) {
		// Ask from index 2, get [2,3,4] starting at 2.
		first, lines := r.LinesFrom(2)
		if first != 2 || len(lines) != 3 || lines[0][0].T != "2" || lines[2][0].T != "4" {
			t.Errorf("LinesFrom(2) = first %d lines %v, want first 2 [2 3 4]", first, lineTexts(lines))
		}
	})
	t.Run("at committed there is nothing to replay", func(t *testing.T) {
		if first, lines := r.LinesFrom(5); first != 5 || lines != nil {
			t.Errorf("LinesFrom(5) = first %d lines %v, want first 5 nil", first, lineTexts(lines))
		}
	})
	t.Run("an evicted index clamps up and signals the gap", func(t *testing.T) {
		// Force eviction: indices 0..1 drop out (cap 5, now 8 committed).
		r.Append([][]vt.WireRun{makeLine("5"), makeLine("6"), makeLine("7")})
		if r.OldestIndex() != 3 {
			t.Fatalf("oldest=%d, want 3", r.OldestIndex())
		}
		// Request from evicted index 0: clamp up to oldest (3) and signal the
		// gap by returning first > requested.
		first, lines := r.LinesFrom(0)
		if first != 3 {
			t.Errorf("LinesFrom(0) after eviction: first=%d, want 3 (gap signal)", first)
		}
		if len(lines) != 5 || lines[0][0].T != "3" || lines[4][0].T != "7" {
			t.Errorf("LinesFrom(0) lines = %v, want [3 4 5 6 7]", lineTexts(lines))
		}
	})
}

// TestScrollbackRing_ClearPreservesCommitted verifies Clear drops retained
// lines but keeps the committed counter, so absolute indices never repeat
// within a session even after a clear.
func TestScrollbackRing_ClearPreservesCommitted(t *testing.T) {
	r := newScrollbackRing(5)
	r.Append([][]vt.WireRun{makeLine("a"), makeLine("b")})
	r.Clear()
	if r.Committed() != 2 {
		t.Fatalf("after clear: committed=%d, want 2 (preserved)", r.Committed())
	}
	r.Append([][]vt.WireRun{makeLine("c")})
	if r.Committed() != 3 {
		t.Fatalf("append after clear: committed=%d, want 3 (index 2 not reused)", r.Committed())
	}
}

// TestScrollbackRing_ZeroCapacityAdvancesCommitted verifies a disabled
// scrollback still advances the absolute base so the live window's base stays
// correct; nothing is retained for replay.
func TestScrollbackRing_ZeroCapacityAdvancesCommitted(t *testing.T) {
	r := newScrollbackRing(0)
	r.Append([][]vt.WireRun{makeLine("a"), makeLine("b"), makeLine("c")})
	if r.Committed() != 3 {
		t.Fatalf("zero-cap committed=%d, want 3", r.Committed())
	}
	if r.Len() != 0 {
		t.Fatalf("zero-cap retains %d lines, want 0", r.Len())
	}
	if _, lines := r.LinesFrom(0); lines != nil {
		t.Fatalf("zero-cap LinesFrom(0) = %v, want nil", lineTexts(lines))
	}
}

func lineTexts(lines [][]vt.WireRun) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if len(l) > 0 {
			out[i] = l[0].T
		}
	}
	return out
}

// TestScrollbackRing_GrowsOnDemand pins the allocation shape: the buffer grows
// toward capacity instead of being allocated at it.
//
// This is load-bearing rather than cosmetic. Capacity is an operator-set number
// (WT_SCROLLBACK) that is meant to be settable absurdly high — the answer to
// "I want unlimited history" is a huge number, not a sentinel — so allocating
// at it charges every session 24 bytes per CONFIGURED line before it prints
// anything: 2.3 MB at the 100k default, 240 MB for an operator who asks for
// ten million.
func TestScrollbackRing_GrowsOnDemand(t *testing.T) {
	const capacity = 100_000
	r := newScrollbackRing(capacity)
	if got := cap(r.buf); got != 0 {
		t.Errorf("fresh ring allocated %d slots; want 0", got)
	}

	r.Append([][]vt.WireRun{makeLine("a"), makeLine("b"), makeLine("c")})
	if got := r.Len(); got != 3 {
		t.Fatalf("retained %d lines, want 3", got)
	}
	// Grown to hold what exists, not to the configured ceiling.
	if got := len(r.buf); got != 3 {
		t.Errorf("buffer length %d after 3 lines; want 3", got)
	}
	if cap(r.buf) >= capacity {
		t.Errorf("buffer capacity reached %d after 3 lines; it must not preallocate the ceiling", cap(r.buf))
	}
	// And reads work in the growing phase, which is where an index-vs-length
	// mix-up would show up.
	if got := lineTexts(r.Lines()); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("Lines() = %v, want [a b c]", got)
	}
	first, from := r.LinesFrom(1)
	if first != 1 || !slices.Equal(lineTexts(from), []string{"b", "c"}) {
		t.Errorf("LinesFrom(1) = (%d, %v), want (1, [b c])", first, lineTexts(from))
	}
}

// TestScrollbackRing_GrowsThenWraps drives the ring across the growth/eviction
// boundary one line at a time, which is where the two Append branches meet:
// retention, the oldest index and content must be continuous across the seam.
func TestScrollbackRing_GrowsThenWraps(t *testing.T) {
	const capacity = 4
	r := newScrollbackRing(capacity)
	for i := range 10 {
		r.Append([][]vt.WireRun{makeLine(fmt.Sprintf("L%d", i))})

		wantLen := min(i+1, capacity)
		if got := r.Len(); got != wantLen {
			t.Fatalf("after %d lines: Len()=%d, want %d", i+1, got, wantLen)
		}
		if got, want := r.Committed(), uint64(i+1); got != want {
			t.Fatalf("after %d lines: Committed()=%d, want %d", i+1, got, want)
		}
		wantOldest := uint64(max(0, i+1-capacity)) // #nosec G115 -- small test values
		if got := r.OldestIndex(); got != wantOldest {
			t.Fatalf("after %d lines: OldestIndex()=%d, want %d", i+1, got, wantOldest)
		}
		// Content is the newest `wantLen` lines, in order.
		want := make([]string, 0, wantLen)
		for k := i + 1 - wantLen; k <= i; k++ {
			want = append(want, fmt.Sprintf("L%d", k))
		}
		if got := lineTexts(r.Lines()); !slices.Equal(got, want) {
			t.Fatalf("after %d lines: Lines()=%v, want %v", i+1, got, want)
		}
	}
}

// TestScrollbackRing_ClearReleasesAndRegrows pins both halves of Clear on a
// GROWING buffer.
//
// Correctness: a buffer left untouched by Clear (length intact, count zeroed)
// puts the ring in an impossible state where Append's growth branch writes past
// index 0 while the readers still index from 0 — so the next line committed
// reads back as a pre-Clear one. Zeroing the length fixes that.
//
// Memory: `nil` rather than `buf[:0]`, because only releasing the array frees
// the retained rows, and freeing them is precisely what an application clearing
// its scrollback (ED3) is asking for. At the 100k default that array is 2.3 MB
// of pointers holding every row alive.
func TestScrollbackRing_ClearReleasesAndRegrows(t *testing.T) {
	r := newScrollbackRing(10)
	r.Append([][]vt.WireRun{makeLine("old1"), makeLine("old2"), makeLine("old3")})
	r.Clear()
	if got := len(r.buf); got != 0 {
		t.Errorf("buffer length %d after Clear; want 0, or the next append reads back a stale row", got)
	}
	if got := cap(r.buf); got != 0 {
		t.Errorf("buffer still holds a %d-slot array after Clear; want it released so the rows can be freed", got)
	}

	r.Append([][]vt.WireRun{makeLine("new1")})
	if got := lineTexts(r.Lines()); !slices.Equal(got, []string{"new1"}) {
		t.Errorf("Lines() = %v after clear+append, want [new1] (a stale row means the buffer outlived its contents)", got)
	}
	// The absolute index of the new line is 3, and reading from it must find it.
	first, from := r.LinesFrom(3)
	if first != 3 || !slices.Equal(lineTexts(from), []string{"new1"}) {
		t.Errorf("LinesFrom(3) = (%d, %v), want (3, [new1])", first, lineTexts(from))
	}
}
