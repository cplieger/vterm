package terminal

import (
	"fmt"
	"testing"

	"github.com/cplieger/web-terminal-engine/v4/vt"
)

// FuzzScrollbackRing_replayMatchesModel is a model-based oracle for the
// resume/replay index math. Against a full in-memory log of every appended
// line, the ring must report Committed == lines appended, retain exactly
// min(cap, total) lines, expose OldestIndex == committed-retained, and have
// LinesFrom(q) return the contiguous tail at and after max(q, oldest) with
// every line aligned to its absolute index. Catches off-by-one bugs in
// eviction, OldestIndex, and the LinesFrom clamp/skip that fixed table cases
// can miss.
func FuzzScrollbackRing_replayMatchesModel(f *testing.F) {
	f.Add(5, 3, uint64(0))
	f.Add(3, 8, uint64(2))
	f.Add(0, 4, uint64(0))
	f.Add(4, 130, uint64(50))
	f.Add(7, 7, uint64(100))
	f.Fuzz(func(t *testing.T, capacity, numLines int, queryFrom uint64) {
		capacity = ((capacity % 65) + 65) % 65
		numLines = ((numLines % 513) + 513) % 513
		queryFrom %= 700

		r := newScrollbackRing(capacity)
		for i := range numLines {
			r.Append([][]vt.WireRun{makeLine(fmt.Sprintf("L%d", i))})
		}

		if got := r.Committed(); got != uint64(numLines) {
			t.Fatalf("Committed() = %d, want %d", got, numLines)
		}
		wantCount := numLines
		switch {
		case capacity == 0:
			wantCount = 0
		case numLines > capacity:
			wantCount = capacity
		}
		if got := r.Len(); got != wantCount {
			t.Fatalf("Len() = %d, want %d (cap=%d appended=%d)", got, wantCount, capacity, numLines)
		}
		wantOldest := uint64(numLines - wantCount)
		if got := r.OldestIndex(); got != wantOldest {
			t.Fatalf("OldestIndex() = %d, want %d", got, wantOldest)
		}

		firstAbs, lines := r.LinesFrom(queryFrom)
		if wantCount == 0 || queryFrom >= uint64(numLines) {
			if firstAbs != uint64(numLines) || lines != nil {
				t.Fatalf("LinesFrom(%d) = (first=%d, %d lines), want (first=%d, nil)",
					queryFrom, firstAbs, len(lines), numLines)
			}
			return
		}
		wantFirst := max(queryFrom, wantOldest)
		if firstAbs != wantFirst {
			t.Fatalf("LinesFrom(%d) firstAbs = %d, want %d (oldest=%d)", queryFrom, firstAbs, wantFirst, wantOldest)
		}
		if want := numLines - int(wantFirst); len(lines) != want {
			t.Fatalf("LinesFrom(%d) returned %d lines, want %d", queryFrom, len(lines), want)
		}
		for i, line := range lines {
			abs := wantFirst + uint64(i)
			want := fmt.Sprintf("L%d", abs)
			got := ""
			if len(line) > 0 {
				got = line[0].T
			}
			if got != want {
				t.Fatalf("LinesFrom(%d)[%d] = %q, want %q (absolute index %d)", queryFrom, i, got, want, abs)
			}
		}
	})
}
