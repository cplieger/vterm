package vt

import "testing"

// TestResizeGrowsAtBottomNormalScreen verifies that growing the height of a
// NORMAL screen (a line-oriented shell, not the alt-screen) appends the new
// empty rows at the BOTTOM and leaves existing output — and the cursor —
// anchored at the TOP, the way xterm/iTerm/Terminal.app show a shell. This is
// what keeps a fresh bash prompt at the top of the page: the PTY starts at
// defaultRows and the first client resize grows it, so prepending at the top
// here would strand the prompt in the middle with blank rows above it.
func TestResizeGrowsAtBottomNormalScreen(t *testing.T) {
	s := New(5, 10)
	// Mark the original content so we can locate it after resize.
	for x := range s.Width {
		s.Cells[0][x].Ch = 'A'
		s.Cells[4][x].Ch = 'E'
	}
	s.curY = 4 // cursor on the last row

	s.Resize(10, 10)

	if s.Height != 10 {
		t.Fatalf("Height = %d, want 10", s.Height)
	}
	// Original content stays put: row 0 and row 4 are unchanged.
	if s.Cells[0][0].Ch != 'A' {
		t.Errorf("expected 'A' to stay at row 0 col 0 after grow, got %q", s.Cells[0][0].Ch)
	}
	if s.Cells[4][0].Ch != 'E' {
		t.Errorf("expected 'E' to stay at row 4 col 0 after grow, got %q", s.Cells[4][0].Ch)
	}
	// Cursor stays on its row (no downward shift on a normal-screen grow).
	if s.curY != 4 {
		t.Errorf("curY = %d, want 4 (unchanged on a normal-screen grow)", s.curY)
	}
	// Newly-appended rows at the bottom should be empty.
	for y := 5; y < 10; y++ {
		for x := range s.Width {
			if s.Cells[y][x].Ch != 0 && s.Cells[y][x].Ch != ' ' {
				t.Errorf("row %d col %d should be empty, got %q", y, x, s.Cells[y][x].Ch)
			}
		}
	}
}

// TestResizeGrowsAtTopAltScreen verifies that growing the height while in the
// ALT screen (a full-screen TUI like kiro-cli) prepends the new rows at the TOP
// and moves the cursor down with the content, so the existing screen stays
// pinned to the bottom until the app's SIGWINCH-driven redraw lands. This is
// the "black gap between content and the input bar after an iPhone → iPad
// switch" fix, and it must survive the normal-screen change above.
func TestResizeGrowsAtTopAltScreen(t *testing.T) {
	s := New(5, 10)
	s.InAltScreen = true
	// Mark the original content so we can locate it after resize.
	for x := range s.Width {
		s.Cells[0][x].Ch = 'A'
		s.Cells[4][x].Ch = 'E'
	}
	s.curY = 4 // cursor on the last row

	s.Resize(10, 10)

	if s.Height != 10 {
		t.Fatalf("Height = %d, want 10", s.Height)
	}
	// Original row 0 should now be at row 5, original row 4 at row 9.
	if s.Cells[5][0].Ch != 'A' {
		t.Errorf("expected 'A' at row 5 col 0 after alt-screen grow, got %q", s.Cells[5][0].Ch)
	}
	if s.Cells[9][0].Ch != 'E' {
		t.Errorf("expected 'E' at row 9 col 0 after alt-screen grow, got %q", s.Cells[9][0].Ch)
	}
	// Cursor should have moved down by the grow amount.
	if s.curY != 9 {
		t.Errorf("curY = %d, want 9 (was 4 + grow=5)", s.curY)
	}
	// Newly-prepended rows should be empty.
	for y := range 5 {
		for x := range s.Width {
			if s.Cells[y][x].Ch != 0 && s.Cells[y][x].Ch != ' ' {
				t.Errorf("row %d col %d should be empty, got %q", y, x, s.Cells[y][x].Ch)
			}
		}
	}
}

// TestResizeShrinksFromBottom: shrinking still drops rows from the
// bottom (truncates s.Cells[:rows]). The cursor clamps into the new
// range. This is unchanged behaviour — verifying it didn't regress.
func TestResizeShrinksFromBottom(t *testing.T) {
	s := New(10, 10)
	for x := range s.Width {
		s.Cells[0][x].Ch = 'A'
		s.Cells[9][x].Ch = 'B'
	}
	s.curY = 9

	s.Resize(5, 10)

	if s.Height != 5 {
		t.Fatalf("Height = %d, want 5", s.Height)
	}
	if s.Cells[0][0].Ch != 'A' {
		t.Errorf("top row 'A' should survive shrink, got %q", s.Cells[0][0].Ch)
	}
	if s.curY != 4 {
		t.Errorf("curY = %d, want 4 (clamped from 9)", s.curY)
	}
}

// TestResizeWidthOnly verifies that growing/shrinking only the width
// (no height change) preserves all rows in place — no prepend/append.
func TestResizeWidthOnly(t *testing.T) {
	s := New(3, 5)
	s.Cells[1][0].Ch = 'X'

	s.Resize(3, 20)

	if s.Width != 20 || s.Height != 3 {
		t.Fatalf("dims = %dx%d, want 20x3", s.Width, s.Height)
	}
	if s.Cells[1][0].Ch != 'X' {
		t.Errorf("'X' should still be at row 1 col 0, got %q", s.Cells[1][0].Ch)
	}
}

// makeSavedMain builds a saved-main-screen buffer where row r carries the
// marker rune 'A'+r in column 0 (the rest spaces), so a row's identity survives
// a savedMainCells rebuild and can be asserted by content.
func makeSavedMain(rows, cols int) [][]Cell {
	g := make([][]Cell, rows)
	for i := range g {
		row := make([]Cell, cols)
		for j := range row {
			row[j] = Cell{Ch: ' '}
		}
		if cols > 0 {
			row[0] = Cell{Ch: rune('A' + i)}
		}
		g[i] = row
	}
	return g
}

// resizeSavedCursor enters alt-screen, overrides the saved main cursor, resizes,
// and returns the post-resize saved main cursor.
func resizeSavedCursor(savedX, savedY, newRows, newCols int) (gotX, gotY int) {
	s := New(8, 12)
	s.enterAltScreen(1049)
	s.savedMainCurX = savedX
	s.savedMainCurY = savedY
	s.Resize(newRows, newCols)
	return s.savedMainCurX, s.savedMainCurY
}

// TestResizeTabStopsGrow verifies that widening a non-nil tabStops slice fills
// default stops at positive multiples of 8 only, never at column 0, bounded by
// the new column count.
func TestResizeTabStopsGrow(t *testing.T) {
	s := New(5, 4)
	s.tabStops = make([]bool, 0) // non-nil, length 0 -> fill loop starts at i=0
	s.Resize(5, 16)              // width is a multiple of 8
	if got := len(s.tabStops); got != 16 {
		t.Fatalf("len(tabStops) after grow = %d, want 16", got)
	}
	if s.tabStops[0] {
		t.Errorf("tabStops[0] = true, want false (column 0 never gets a stop)")
	}
	if s.tabStops[1] {
		t.Errorf("tabStops[1] = true, want false (1 is not a multiple of 8)")
	}
	if s.tabStops[7] {
		t.Errorf("tabStops[7] = true, want false (7 is not a multiple of 8)")
	}
	if !s.tabStops[8] {
		t.Errorf("tabStops[8] = false, want true (8 is a positive multiple of 8)")
	}
	if s.tabStops[15] {
		t.Errorf("tabStops[15] = true, want false (15 is not a multiple of 8)")
	}
}

// TestResizeRebuildsSavedMainCells verifies a resize taken while a saved
// main-screen buffer exists rebuilds that buffer at the new dimensions.
func TestResizeRebuildsSavedMainCells(t *testing.T) {
	s := New(5, 10)
	s.enterAltScreen(1049) // populates savedMainCells (5x10)
	s.Resize(8, 20)
	if got := len(s.savedMainCells); got != 8 {
		t.Fatalf("savedMainCells rows after resize = %d, want 8", got)
	}
	if got := len(s.savedMainCells[0]); got != 20 {
		t.Errorf("savedMainCells cols after resize = %d, want 20", got)
	}
}

// TestResizeSavedMainCopyBounds verifies the bounded copy of saved rows into the
// resized saved buffer: existing rows are copied by content, new rows are blank,
// and the index guard never reads out of range.
func TestResizeSavedMainCopyBounds(t *testing.T) {
	s := New(2, 4)
	s.InAltScreen = true
	s.savedMainCells = makeSavedMain(2, 4) // row0[0]='A', row1[0]='B'
	s.Resize(4, 4)                         // grow rows 2 -> 4
	if got := len(s.savedMainCells); got != 4 {
		t.Fatalf("savedMainCells rows after resize = %d, want 4", got)
	}
	if s.savedMainCells[0][0].Ch != 'A' {
		t.Errorf("savedMainCells[0][0].Ch = %q, want 'A' (copied)", s.savedMainCells[0][0].Ch)
	}
	if s.savedMainCells[1][0].Ch != 'B' {
		t.Errorf("savedMainCells[1][0].Ch = %q, want 'B' (copied)", s.savedMainCells[1][0].Ch)
	}
	if s.savedMainCells[2][0].Ch != ' ' {
		t.Errorf("savedMainCells[2][0].Ch = %q, want ' ' (blank pad row)", s.savedMainCells[2][0].Ch)
	}
	if s.savedMainCells[3][0].Ch != ' ' {
		t.Errorf("savedMainCells[3][0].Ch = %q, want ' ' (blank pad row)", s.savedMainCells[3][0].Ch)
	}
}

// TestResizeClampsSavedCursorY verifies clamping of the saved main-screen cursor
// row to the new height (boundary clamps, in-range is preserved).
func TestResizeClampsSavedCursorY(t *testing.T) {
	s := New(3, 4)
	s.InAltScreen = true
	s.savedMainCells = makeSavedMain(3, 4)
	s.savedMainCurY = 4
	s.Resize(4, 4)
	if s.savedMainCurY != 3 {
		t.Errorf("savedMainCurY at boundary = %d, want 3 (clamped to rows-1)", s.savedMainCurY)
	}

	s2 := New(3, 4)
	s2.InAltScreen = true
	s2.savedMainCells = makeSavedMain(3, 4)
	s2.savedMainCurY = 0
	s2.Resize(4, 4)
	if s2.savedMainCurY != 0 {
		t.Errorf("savedMainCurY below limit = %d, want 0 (not clamped)", s2.savedMainCurY)
	}
}

// TestResizeSavedCursorXClamp verifies the saved main-screen cursor column is
// clamped to cols-1 when out of range and preserved otherwise.
func TestResizeSavedCursorXClamp(t *testing.T) {
	cases := []struct {
		name    string
		savedX  int
		newCols int
		wantX   int
	}{
		{"over clamps to cols-1", 8, 5, 4},
		{"equal clamps to cols-1", 5, 5, 4},
		{"under unchanged", 2, 5, 2},
	}
	for _, c := range cases {
		gotX, _ := resizeSavedCursor(c.savedX, 0, 8, c.newCols)
		if gotX != c.wantX {
			t.Errorf("%s: Resize savedMainCurX (savedX=%d, cols=%d) = %d, want %d",
				c.name, c.savedX, c.newCols, gotX, c.wantX)
		}
	}
}

// TestSaveCursorResizeSmallerRestore verifies DECSC then a shrink then DECRC
// leaves the cursor in bounds.
func TestSaveCursorResizeSmallerRestore(t *testing.T) {
	s := New(20, 80)
	s.Write([]byte("\x1b[16;71H")) // row 15, col 70
	s.Write([]byte("\x1b7"))       // DECSC
	s.Resize(5, 10)                // shrink
	s.Write([]byte("\x1b8"))       // DECRC
	row, col := s.CursorPos()
	if col < 0 || col >= s.Width {
		t.Fatalf("col %d out of bounds [0,%d) after restore post-resize", col, s.Width)
	}
	if row < 0 || row >= s.Height {
		t.Fatalf("row %d out of bounds [0,%d) after restore post-resize", row, s.Height)
	}
}

// TestResizeTo1ColMidCSI verifies resizing to a single column partway through a
// CSI sequence leaves the cursor in bounds once the sequence completes.
func TestResizeTo1ColMidCSI(t *testing.T) {
	s := New(5, 80)
	s.Write([]byte("\x1b[")) // start CSI
	s.Resize(3, 1)           // resize mid-sequence
	s.Write([]byte("1;1H"))  // complete
	row, col := s.CursorPos()
	if col < 0 || col >= s.Width {
		t.Fatalf("col %d out of bounds after resize mid-CSI", col)
	}
	if row < 0 || row >= s.Height {
		t.Fatalf("row %d out of bounds after resize mid-CSI", row)
	}
}

// --- Allocation contracts ---------------------------------------------------
//
// Resize is not the per-byte path, but it is not a startup-only path either: the
// terminal handler calls it on every client resize message, a live resize
// streams one per animation frame while a window edge is dragged, and when
// several clients share a session the screen relaxes to the smallest remaining
// client's size as each one disconnects. So the question is the same one the
// write and wire contracts ask — which quantity does the cost track — and the
// answer must be rows, not cells.
//
// The contract-writing mechanic that matters here is that Resize is IDEMPOTENT:
// the second call at the same size does nothing. Measuring a single Resize in an
// AllocsPerRun closure would therefore measure one real resize amortised over
// hundreds of no-ops, which is a number that looks stable and means nothing. The
// tests below measure a PAIR — resize away, resize back — which is genuinely
// periodic, and they say so in each case.
//
// What the measurement found, including the one place the expected property does
// not hold:
//
//   - A resize to the SAME size is free on the main screen. Nothing is
//     reallocated, which is what a client reconnecting at its current size needs.
//   - A width change costs exactly one allocation per ROW, at any width:
//     resizeWidth rebuilds each row to the new column count and copies the old
//     one in. 40 columns and 400 columns cost the same count.
//   - A height change costs one allocation per ADDED row plus one, at any width,
//     and shrinking is free (the row slice is truncated). Existing rows move as
//     slice elements rather than being rebuilt.
//   - A resize to the same size is NOT free while the alternate screen is
//     active. resizeSavedMain rebuilds the whole saved main-screen buffer with no
//     dimension check, so a no-op resize costs one allocation per saved row —
//     measured 25 on a 24-row screen and 101 on a 100-row one. That is the
//     reverse-direction defect this set was looking for, and it is pinned as
//     measured rather than as zero, with the assertion written so that fixing it
//     fails the test and says so.

// TestResizeToTheSameSizeIsAllocationFreeOnTheMainScreen pins the no-op path. A
// client that reconnects at the size it already had, and a second client
// attaching at the same dimensions, both land here; Resize's own doc comment
// contemplates exactly that case ("a no-op resize (e.g. client reconnect at the
// same size)").
//
// A single Resize call is the right thing to measure for once, because at the
// same size it IS the steady state: every iteration does the same nothing.
func TestResizeToTheSameSizeIsAllocationFreeOnTheMainScreen(t *testing.T) {
	sizes := map[string]struct{ rows, cols int }{
		"small_4x40":      {4, 40},
		"default_24x80":   {24, 80},
		"large_100x400":   {100, 400},
		"single_row_1x80": {1, 80},
	}
	for name, sz := range sizes {
		t.Run(name, func(t *testing.T) {
			s := New(sz.rows, sz.cols)
			s.Write([]byte("hello\r\nworld"))
			got := testing.AllocsPerRun(200, func() {
				s.Resize(sz.rows, sz.cols)
			})
			if got != 0 {
				t.Errorf("Resize(%d, %d) on a %dx%d screen allocated %v times per run, want 0: a resize that changes no dimension must not rebuild the grid, or every reconnect at an unchanged size copies the whole screen",
					sz.rows, sz.cols, sz.rows, sz.cols, got)
			}
		})
	}
}

// TestResizeAllocationCostIsPerRowNotPerCell pins the two real resize paths.
//
// Both are measured as a pair (away and back) because a single Resize is
// idempotent, and both are compared across a tenfold width change, which is the
// whole point: a 400-column screen must cost the same COUNT as a 40-column one.
// More bytes, same number of allocations — that is what "per row, not per cell"
// means, and a rewrite that assembled rows cell by cell would break it here while
// leaving both tracked benchmarks untouched, since neither resizes at all.
func TestResizeAllocationCostIsPerRowNotPerCell(t *testing.T) {
	t.Run("width_change_costs_one_row_each", func(t *testing.T) {
		// Each resize rebuilds every row, so the pair costs 2 per row. The +2
		// tolerance covers the tab-stop array, which resizeWidth also rebuilds
		// once a program has set an explicit stop.
		for _, rows := range []int{4, 24, 100} {
			counts := make(map[int]float64, 2)
			for _, cols := range []int{40, 400} {
				s := New(rows, cols)
				s.Write([]byte("hello\r\nworld"))
				for range 3 {
					s.Resize(rows, cols+10)
					s.Resize(rows, cols)
				}
				counts[cols] = testing.AllocsPerRun(100, func() {
					s.Resize(rows, cols+10)
					s.Resize(rows, cols)
				})
				if want := float64(2*rows) + 2; counts[cols] > want {
					t.Errorf("Resize(%d, %d) then Resize(%d, %d) allocated %v times per run, want at most %v (one row per resize per row): a width change must rebuild rows and not cells",
						rows, cols+10, rows, cols, counts[cols], want)
				}
			}
			if counts[40] != counts[400] {
				t.Errorf("a width-change pair on a %d-row screen allocated %v times per run at 40 columns and %v at 400, want the same count: resize cost must not track the column count",
					rows, counts[40], counts[400])
			}
			t.Logf("width change on a %d-row screen: %v allocations per away-and-back pair at both 40 and 400 columns", rows, counts[40])
		}
	})

	t.Run("height_change_costs_one_row_per_added_row", func(t *testing.T) {
		// Growing by delta rows allocates delta rows plus the slice holding
		// them; shrinking back truncates and allocates nothing. The pair is
		// therefore delta+1, and it must not depend on the width at all.
		const rows = 24
		for _, delta := range []int{1, 10, 100} {
			counts := make(map[int]float64, 2)
			for _, cols := range []int{40, 400} {
				s := New(rows, cols)
				s.Write([]byte("hello\r\nworld"))
				// Warm past the first grow: it also grows the row slice's
				// capacity, which later grows reuse.
				for range 3 {
					s.Resize(rows+delta, cols)
					s.Resize(rows, cols)
				}
				counts[cols] = testing.AllocsPerRun(100, func() {
					s.Resize(rows+delta, cols)
					s.Resize(rows, cols)
				})
				if want := float64(delta) + 2; counts[cols] > want {
					t.Errorf("Resize(%d, %d) then Resize(%d, %d) allocated %v times per run, want at most %v (one row per added row, plus the slice): growing the height must not touch the rows that already exist",
						rows+delta, cols, rows, cols, counts[cols], want)
				}
			}
			if counts[40] != counts[400] {
				t.Errorf("a height-change pair of +%d rows allocated %v times per run at 40 columns and %v at 400, want the same count: the cost of adding a row must not track its width",
					delta, counts[40], counts[400])
			}
			t.Logf("height change of +%d rows: %v allocations per away-and-back pair at both 40 and 400 columns", delta, counts[40])
		}
	})
}

// TestResizeInAltScreenRebuildsSavedMainBufferEvenWhenNothingChanged records a
// defect rather than a guarantee, which is why it asserts the cost it MEASURED
// instead of the cost it wants.
//
// resizeSavedMain (screen.go) has no dimension check: whenever the alternate
// screen is active it rebuilds every row of the saved main-screen buffer, so a
// resize to the size the screen already has costs one allocation per saved row
// plus one. The main-screen path is free in the same situation (the test above),
// and the alt screen is where a session spends its time whenever a full-screen
// program is running — which is the case this engine was built for. A reconnect
// or a second viewer attaching at the current size therefore copies the entire
// saved screen for nothing.
//
// The assertion is two-sided on purpose. An increase means the no-op got worse; a
// DECREASE means someone gave resizeSavedMain the dimension check it is missing,
// at which point this test should be deleted and the main-screen contract above
// extended to cover the alt screen. Either way the number stops being silent.
func TestResizeInAltScreenRebuildsSavedMainBufferEvenWhenNothingChanged(t *testing.T) {
	for _, rows := range []int{4, 24, 100} {
		for _, cols := range []int{40, 400} {
			s := New(rows, cols)
			s.Write([]byte("hello\r\nworld"))
			s.Write([]byte("\x1b[?1049h"))
			if !s.InAltScreen {
				t.Fatalf("Screen.Write(CSI ?1049h) on a %dx%d screen left InAltScreen false: the fixture must be in the alternate screen", rows, cols)
			}
			got := testing.AllocsPerRun(100, func() {
				s.Resize(rows, cols)
			})
			// One fresh row per saved row, plus the slice that holds them.
			want := float64(rows) + 1
			if got != want {
				t.Errorf("Resize(%d, %d) on a %dx%d screen in the alternate screen allocated %v times per run, want %v (resizeSavedMain rebuilds every saved row with no dimension check): a higher count means the no-op resize got more expensive; a lower one means the missing check was added, in which case delete this test and extend TestResizeToTheSameSizeIsAllocationFreeOnTheMainScreen to the alternate screen",
					rows, cols, rows, cols, got, want)
			}
		}
	}
}
