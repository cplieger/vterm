package vt

import (
	"fmt"
	"strings"
	"testing"
)

// restoredAltCursor enters alt-screen, overrides the saved main cursor, exits
// alt-screen, and returns the restored (clamped) cursor on an 5x8 screen.
func restoredAltCursor(savedX, savedY int) (gotX, gotY int) {
	s := New(5, 8)
	s.enterAltScreen(1049)
	s.savedMainCurX = savedX
	s.savedMainCurY = savedY
	s.exitAltScreen(1049)
	return s.curX, s.curY
}

// exitedAltScreen builds an alt-screen with the given saved main-screen
// scrollBottom, exits alt-screen, and returns the resulting Screen so the
// post-restore scrollBottom clamp can be inspected.
func exitedAltScreen(t *testing.T, height, width, savedScrollBottom int) *Screen {
	t.Helper()
	s := New(height, width)
	s.InAltScreen = true
	s.savedMainCells = make([][]Cell, height)
	for i := range s.savedMainCells {
		s.savedMainCells[i] = make([]Cell, width)
	}
	s.savedMainScrollBottom = savedScrollBottom
	s.exitAltScreen(1049)
	return s
}

// TestNewInitializesSingleShiftSentinel verifies New() seeds singleShft with
// the -1 "no single shift pending" sentinel.
func TestNewInitializesSingleShiftSentinel(t *testing.T) {
	s := New(2, 20)
	if got := int(s.singleShft); got != -1 {
		t.Errorf("New().singleShft = %d, want -1", got)
	}
}

// --- RenderViewport ---

// TestRenderViewportStartsRunPerRow verifies a row of cells sharing one uniform
// non-default style emits its SGR sequence exactly once (at the row start).
func TestRenderViewportStartsRunPerRow(t *testing.T) {
	s := New(1, 2)
	s.Cells[0][0] = Cell{Ch: 'A', Style: Style{Bold: true}}
	s.Cells[0][1] = Cell{Ch: 'B', Style: Style{Bold: true}}
	out := s.RenderViewport()
	if n := strings.Count(out, "\x1b[0;1m"); n != 1 {
		t.Errorf("uniform bold row emitted bold SGR %d times, want 1; out=%q", n, out)
	}
}

// TestRenderViewportEmitsOnStyleChange verifies a cell whose style differs from
// the previous cell emits a fresh SGR sequence.
func TestRenderViewportEmitsOnStyleChange(t *testing.T) {
	s := New(1, 2)
	s.Cells[0][0] = Cell{Ch: 'A', Style: Style{Bold: true}}
	s.Cells[0][1] = Cell{Ch: 'B', Style: Style{Italic: true}}
	out := s.RenderViewport()
	if !strings.Contains(out, "\x1b[0;3m") {
		t.Errorf("did not emit italic SGR on style change; out=%q", out)
	}
}

// TestRenderViewportRowSeparators verifies CRLF separates rows but is not
// appended after the last row.
func TestRenderViewportRowSeparators(t *testing.T) {
	s := New(3, 1)
	got := s.RenderViewport()
	want := "\x1b[0m \x1b[0m\r\n\x1b[0m \x1b[0m\r\n\x1b[0m \x1b[0m"
	if got != want {
		t.Errorf("RenderViewport(3x1) = %q, want %q", got, want)
	}
	if n := strings.Count(got, "\r\n"); n != 2 {
		t.Errorf("CRLF count = %d, want 2", n)
	}
}

// --- RowString ---

// TestRowStringOutOfRangeRow verifies RowString returns "" for a row index
// equal to Height rather than indexing out of range.
func TestRowStringOutOfRangeRow(t *testing.T) {
	s := New(3, 5)
	var got string
	if didPanic(func() { got = s.RowString(3) }) {
		t.Fatalf("RowString(len(Cells)) panicked: out-of-range row not guarded")
	}
	if got != "" {
		t.Errorf("RowString(3) = %q, want \"\"", got)
	}
}

// --- put guards ---

// TestPutGuardCurY verifies put() does not write (or panic) when curY equals
// Height.
func TestPutGuardCurY(t *testing.T) {
	s := New(4, 6)
	s.curY = 4 // == Height
	s.curX = 0
	if didPanic(func() { s.put('A') }) {
		t.Errorf("put with curY==Height panicked: write guard not effective")
	}
}

// TestPutGuardCurX verifies put() does not write (or panic) when curX equals
// Width.
func TestPutGuardCurX(t *testing.T) {
	s := New(4, 6)
	s.curY = 0
	s.curX = 6 // == Width
	if didPanic(func() { s.put('A') }) {
		t.Errorf("put with curX==Width panicked: write guard not effective")
	}
}

// TestPutSpacerGuardCurY verifies the width-2 spacer write is guarded against
// curY == Height.
func TestPutSpacerGuardCurY(t *testing.T) {
	s := New(4, 6)
	s.curY = 4                                // == Height
	s.curX = 2                                // not Width-1, so the early width-2 wrap does not fire
	if didPanic(func() { s.put('\u4e16') }) { // CJK wide rune, width 2
		t.Errorf("put(width-2) with curY==Height panicked: spacer guard not effective")
	}
}

// --- scrollIfNeeded ---

// TestScrollIfNeededWrapsAtWidth verifies scrollIfNeeded wraps to the next line
// when curX equals Width.
func TestScrollIfNeededWrapsAtWidth(t *testing.T) {
	s := New(10, 5)
	s.curX = 5 // == Width
	s.curY = 0
	s.scrollIfNeeded()
	if s.curX != 0 || s.curY != 1 {
		t.Errorf("scrollIfNeeded curX==Width: got (curX=%d, curY=%d), want (0, 1)", s.curX, s.curY)
	}
}

// TestScrollIfNeededNoScrollAtBottom verifies the region does not scroll when
// curY equals scrollBottom.
func TestScrollIfNeededNoScrollAtBottom(t *testing.T) {
	s := New(3, 4)
	s.Cells[0][0] = Cell{Ch: 'A'}
	s.curX = 0
	s.curY = 2 // == scrollBottom
	s.scrollIfNeeded()
	if len(s.Drained) != 0 {
		t.Errorf("scrollIfNeeded curY==scrollBottom drained %d line(s), want 0", len(s.Drained))
	}
	if s.Cells[0][0].Ch != 'A' {
		t.Errorf("scrollIfNeeded curY==scrollBottom scrolled buffer: Cells[0][0]=%q, want 'A'", s.Cells[0][0].Ch)
	}
}

// TestScrollIfNeededDrainsAtHeight verifies the buffer scrolls (drains one line)
// when curY reaches Height.
func TestScrollIfNeededDrainsAtHeight(t *testing.T) {
	s := New(3, 4)
	s.Cells[0][0] = Cell{Ch: 'A'}
	s.scrollBottom = 3 // == Height, isolates the curY>=Height branch
	s.curX = 0
	s.curY = 3 // == Height
	s.scrollIfNeeded()
	if len(s.Drained) != 1 {
		t.Errorf("scrollIfNeeded curY==Height drained %d line(s), want 1", len(s.Drained))
	}
	if s.curY != 2 {
		t.Errorf("scrollIfNeeded curY==Height: curY=%d, want 2", s.curY)
	}
}

// --- eraseRegion ---

// TestEraseRegionInclusiveRow verifies eraseRegion's row range is inclusive: a
// single-row region (y1==y2) erases that row.
func TestEraseRegionInclusiveRow(t *testing.T) {
	s := New(3, 5)
	s.Cells[1][0] = Cell{Ch: 'X'}
	s.eraseRegion(1, 0, 1, 0)
	if s.Cells[1][0].Ch != ' ' {
		t.Errorf("eraseRegion(1,0,1,0): Cells[1][0]=%q, want ' '", s.Cells[1][0].Ch)
	}
}

// TestEraseRegionSkipsOutOfRangeRow verifies a row index equal to Height is
// skipped rather than indexed out of range.
func TestEraseRegionSkipsOutOfRangeRow(t *testing.T) {
	s := New(3, 4)
	if didPanic(func() { s.eraseRegion(3, 0, 3, 0) }) { // y == Height
		t.Errorf("eraseRegion at y==Height panicked: out-of-range row not skipped")
	}
}

// TestEraseRegionSkipsOutOfRangeCol verifies a column index equal to Width is
// skipped rather than indexed out of range.
func TestEraseRegionSkipsOutOfRangeCol(t *testing.T) {
	s := New(3, 4)
	if didPanic(func() { s.eraseRegion(0, 4, 0, 4) }) { // x == Width
		t.Errorf("eraseRegion at x==Width panicked: out-of-range column not skipped")
	}
}

// --- alt screen ---

// TestEnterAltScreenScrollBottom verifies enterAltScreen sets scrollBottom to
// Height-1.
func TestEnterAltScreenScrollBottom(t *testing.T) {
	s := New(5, 8)
	s.scrollBottom = 99 // poison; enterAltScreen must overwrite with Height-1
	s.enterAltScreen(1049)
	if s.scrollBottom != 4 {
		t.Errorf("enterAltScreen scrollBottom = %d, want 4 (Height-1)", s.scrollBottom)
	}
}

// TestExitAltScreenClampsCurY verifies exitAltScreen clamps the restored cursor
// row to Height-1 when it is out of range, and preserves an in-range row.
func TestExitAltScreenClampsCurY(t *testing.T) {
	cases := []struct {
		name   string
		savedY int
		wantY  int
	}{
		{"equal height clamps", 5, 4},
		{"in range unchanged", 0, 0},
	}
	for _, c := range cases {
		gotX, gotY := restoredAltCursor(0, c.savedY)
		if gotY != c.wantY {
			t.Errorf("%s: exitAltScreen curY (savedY=%d) = %d, want %d", c.name, c.savedY, gotY, c.wantY)
		}
		if gotX != 0 {
			t.Errorf("%s: exitAltScreen curX = %d, want 0", c.name, gotX)
		}
	}
}

// TestExitAltScreenClampsCurX verifies exitAltScreen clamps the restored cursor
// column to Width-1 when it is out of range, and preserves an in-range column.
func TestExitAltScreenClampsCurX(t *testing.T) {
	cases := []struct {
		name   string
		savedX int
		wantX  int
	}{
		{"equal width clamps", 8, 7},
		{"in range unchanged", 0, 0},
	}
	for _, c := range cases {
		gotX, gotY := restoredAltCursor(c.savedX, 0)
		if gotX != c.wantX {
			t.Errorf("%s: exitAltScreen curX (savedX=%d) = %d, want %d", c.name, c.savedX, gotX, c.wantX)
		}
		if gotY != 0 {
			t.Errorf("%s: exitAltScreen curY = %d, want 0", c.name, gotY)
		}
	}
}

// TestExitAltScreenClampsScrollBottom verifies exitAltScreen clamps the restored
// scrollBottom to Height-1 when it is out of range, and preserves an in-range
// value.
func TestExitAltScreenClampsScrollBottom(t *testing.T) {
	s := exitedAltScreen(t, 5, 10, 5) // savedScrollBottom == Height
	if s.scrollBottom != 4 {
		t.Errorf("exitAltScreen(savedScrollBottom=5, height=5): scrollBottom = %d, want 4", s.scrollBottom)
	}
	s2 := exitedAltScreen(t, 5, 10, 2)
	if s2.scrollBottom != 2 {
		t.Errorf("exitAltScreen(savedScrollBottom=2, height=5): scrollBottom = %d, want 2", s2.scrollBottom)
	}
}

// TestAltScreenEnterExitPreservesMain verifies the alt-screen enter/exit
// sequence (CSI ?1049h/l) restores the main screen content on exit.
func TestAltScreenEnterExitPreservesMain(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("Main"))
	s.Write([]byte("\x1b[?1049h"))
	if !s.InAltScreen {
		t.Fatal("not in alt screen")
	}
	s.Write([]byte("Alt"))
	s.Write([]byte("\x1b[?1049l"))
	if s.InAltScreen {
		t.Fatal("still in alt screen")
	}
	if s.Cells[0][0].Ch != 'M' {
		t.Errorf("main screen not restored: got %q", s.Cells[0][0].Ch)
	}
}

// TestExitAltScreen1049RestoresPen verifies CSI ?1049l restores the main
// screen's SGR pen, so an attribute set inside the alt session cannot leak into
// the rendition the parent shell draws with. DECRESET 1049 is "Use Normal Screen
// Buffer and restore cursor as in DECRC" (xterm ctlseqs), and DECRC restores
// character attributes alongside the position (see restoreCursor), so the pen
// travels with the cursor.
//
// This is the property that makes the engine immune to the class of bug kiro-cli
// 2.19.1 fixed by writing SGR 0 after ?1049l: nothing here needs that trailing
// reset. Underline is the attribute that bug stranded, so it is the one used.
//
// A DECSET mode flag is deliberately NOT in the restore set, and the second
// assertion pins that asymmetry as intentional rather than a second leak: only
// the cursor's own DECRC state is saved and restored here, while a mode flag is
// session state the application owns and turns off itself.
func TestExitAltScreen1049RestoresPen(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("\x1b[1m"))     // main-screen pen: bold
	s.Write([]byte("\x1b[?1049h")) // 1049 resets the pen in the cleared alt buffer
	if s.style != (Style{}) {
		t.Errorf("enter 1049: pen = %+v, want zero (1049 resets SGR on entry)", s.style)
	}
	s.Write([]byte("\x1b[4m"))     // underline, the attribute the upstream bug stranded
	s.Write([]byte("\x1b[?2004h")) // a mode flag turned on inside the alt session
	s.Write([]byte("\x1b[?1049l"))
	if want := (Style{Bold: true}); s.style != want {
		t.Errorf("exit 1049: pen = %+v, want %+v (underline set in alt must not leak out)", s.style, want)
	}
	if !s.BracketedPaste {
		t.Error("exit 1049: BracketedPaste = false, want true (a DECSET mode flag is not part of the alt-screen restore set)")
	}
}

// TestExitAltScreen47SharesPen verifies modes 47 and 1047 share the SGR pen with
// the main screen in BOTH directions: not reset on entry, and not restored on
// exit. Neither DECRESET 47 nor DECRESET 1047 carries a "restore cursor as in
// DECRC" clause in xterm ctlseqs — only 1048 and 1049 do — so an attribute set
// inside a mode-47 session stays set after the switch back.
//
// Guards the mode gate on the pen restore in exitAltScreen. Without it the
// restore ran for every mode, and bold+underline came back as bold alone.
func TestExitAltScreen47SharesPen(t *testing.T) {
	for _, mode := range []int{47, 1047} {
		t.Run(fmt.Sprintf("mode%d", mode), func(t *testing.T) {
			s := New(5, 10)
			s.Write([]byte("\x1b[1m")) // main-screen pen: bold
			s.Write(fmt.Appendf(nil, "\x1b[?%dh", mode))
			if want := (Style{Bold: true}); s.style != want {
				t.Errorf("enter %d: pen = %+v, want %+v (47/1047 do not reset SGR)", mode, s.style, want)
			}
			s.Write([]byte("\x1b[4m"))
			s.Write(fmt.Appendf(nil, "\x1b[?%dl", mode))
			if want := (Style{Bold: true, Underline: true}); s.style != want {
				t.Errorf("exit %d: pen = %+v, want %+v (SGR is shared with the main screen, not restored)", mode, s.style, want)
			}
		})
	}
}

// --- Allocation contracts ---------------------------------------------------
//
// Screen.Write is on the path of every byte a session's program produces, and
// RenderRowWire (wire_test.go) is on the path of every rendered frame, so a
// per-byte or per-cell allocation in either is paid at output rate. Until these
// landed, nothing in the repo asserted a cost.
//
// Why an assertion rather than the chart. This repo contributes two series to
// the weekly benchmark tracker, BenchmarkScreenWrite (4472 B/op, 10 allocs/op)
// and BenchmarkRenderRowWire (592 B/op, 11 allocs/op), and unlike the other
// enrolled repos neither is at zero. The tracker compares a series against its
// own previous run and alerts above a ratio, so 10 allocs/op becoming 14 is a
// ratio of 1.4 and stays silent, and 11 becoming 16 is 1.45 and stays silent.
// A 40% allocation regression on a terminal emulator's hot path is therefore
// invisible to the chart, permanently. Because nothing here sits at zero, there
// is no infinite ratio to trip the alert either — this repo is the clearest case
// of the class the tracker cannot see, and a contract is the only mechanism that
// catches it. Hence bounds, not zeros, wherever a path legitimately allocates.
//
// What the measurement found, so the next reader does not re-derive it:
//
//   - Screen.Write is allocation-FREE, exactly, for every byte class ordinary
//     program output consists of: printable ASCII, multi-byte UTF-8, wide CJK,
//     combining marks, malformed UTF-8, SGR, and CSI cursor addressing. The
//     parser keeps its state in fixed-size arrays on the Screen and put() writes
//     into a grid New already allocated, so the count is 0 at 41 bytes and still
//     0 at 272 KB — including a repaint that touches all 40,000 cells of a
//     100x400 grid.
//   - The sequences that CAPTURE a payload do allocate: OSC 0 title, OSC 8
//     hyperlink, OSC 4 palette, OSC 52 clipboard, DCS query replies. Their count
//     is a small constant per SEQUENCE and independent of the payload's LENGTH,
//     which is the amplification property worth gating, because the program on
//     the other end of the PTY picks that length.
//   - A scrolled line costs exactly ONE allocation at every screen width — the
//     blank row scrollUpOnce mints (csi.go). Rows are NOT recycled, so the
//     contract is one-per-line rather than zero; what it pins is that the cost
//     is per ROW and never per cell.
//   - Carrying a scrolled row into scrollback costs about 8 to 11 allocations
//     per line (its wire runs and the builders that produce them), growing only
//     logarithmically with the row width.
//
// Two mechanics decide whether a number here means anything at all:
//
//   - A Screen is STATEFUL and testing.AllocsPerRun runs its closure hundreds of
//     times, so a closure that writes into the same screen without returning it
//     to where it started measures something different on every iteration: it
//     fills the grid, scrolls it, and appends to Drained, which grows without
//     bound until a consumer drains it. Measured while writing this file: a
//     probe payload containing one VT byte left 895,977 drained rows behind and
//     reported a per-line number that meant nothing. Each contract below
//     therefore picks one of two shapes deliberately and says which. A FIXED
//     POINT, where the payload returns cursor and grid to the state they started
//     in so the thousandth call measures what the first did — that is what the
//     trailing carriage return and the leading CUP in these frames are for. Or a
//     STEADY STATE, warmed outside the measurement and drained inside it, which
//     is what the production flush loop does on every frame anyway. A fresh
//     screen per call is not available: the reset would have to happen inside
//     the measured closure and would dominate it.
//   - AllocsPerRun divides with integer division, so a true average of 0.9
//     reports as 0. Warm-up has to reach the steady state BEFORE the measurement
//     starts, or one early cheap iteration floors the whole result. The
//     scroll-region contract below warms 40 line feeds for exactly this reason.
//
// None of these use t.Parallel: AllocsPerRun pins GOMAXPROCS to 1 and counts
// allocations process-wide, so a parallel sibling's allocations land in the
// measurement.

// allocRuns is the AllocsPerRun sample count for the write contracts. High
// enough that a per-byte regression is unmissable, low enough that the 272 KB
// fixtures stay inside a fast unit test.
const allocRuns = 50

// allocRepaintFrame builds one full-screen repaint: absolute cursor addressing
// to each row in turn, a colour change, then a row of glyphs, so a single frame
// writes every cell of a rows x cols grid. The leading CUP is what makes
// repeating it a fixed point — the frame leaves the cursor on the last row and
// the next copy homes it again — and absolute addressing is used instead of
// \r\n because a newline on the last row would scroll, which is a separate
// contract below.
func allocRepaintFrame(rows, cols int) string {
	var b strings.Builder
	b.WriteString("\x1b[H")
	for y := 1; y <= rows; y++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[38;5;%dm", y, 16+y%216)
		b.WriteString(strings.Repeat("x", cols-2))
	}
	return b.String()
}

// TestScreenWriteIsAllocationFreeAtEveryInputSize is the core contract in this
// file: the cost of writing N bytes into the screen must not track N.
//
// This is the regression that would cost real memory on a busy terminal and
// that the chart cannot see. A terminal engine's write path runs on every byte
// a program emits, so an allocation charged per byte, per rune, or per cell
// turns a chatty build log into allocation pressure proportional to output
// volume — while BenchmarkScreenWrite, which writes one fixed ~1 KB frame,
// would move from 10 allocs/op to a number the tracker's ratio still tolerates.
//
// Every frame here ends with a carriage return, so writing it repeatedly is a
// fixed point: the cursor returns to column 0 of row 0, the same cells are
// overwritten, nothing wraps, nothing scrolls, and Drained stays empty (asserted
// per case, so a fixture that drifts into scrolling fails instead of quietly
// charting the scroll path). That is a real shape rather than a contrivance — a
// spinner or a progress bar redrawing one line is exactly it — and the frame
// that repaints EVERY cell is the next test.
//
// The sizes span 41 bytes to 272 KB, three orders of magnitude, which is far
// more than enough that a per-byte allocation cannot hide: at one allocation per
// byte the largest case would report a quarter of a million.
func TestScreenWriteIsAllocationFreeAtEveryInputSize(t *testing.T) {
	// Each frame is one line of at most 60 columns plus a carriage return, so
	// none of them wraps on an 80-column screen.
	frames := map[string]string{
		"printable_ascii":       strings.Repeat("x", 40) + "\r",
		"multibyte_utf8":        strings.Repeat("héllo wörld ", 3) + "\r",
		"wide_cjk":              strings.Repeat("日本語", 10) + "\r",
		"combining_marks":       strings.Repeat("e\u0301", 40) + "\r",
		"malformed_utf8":        strings.Repeat("\xff\xfe\x80", 13) + "\r",
		"sgr_colour_runs":       strings.Repeat("\x1b[31ma\x1b[32mb", 20) + "\r",
		"truecolour_runs":       strings.Repeat("\x1b[38;2;10;20;30ma", 20) + "\r",
		"csi_cursor_addressing": strings.Repeat("\x1b[1;10H\x1b[Kx", 8) + "\r",
	}
	// 1, 25 and 800 copies of a frame: for printable_ascii that is 41 bytes,
	// 1025 bytes and 32 KB; for truecolour_runs it reaches 272 KB.
	repeats := []int{1, 25, 800}

	for name, frame := range frames {
		t.Run(name, func(t *testing.T) {
			for _, reps := range repeats {
				payload := []byte(strings.Repeat(frame, reps))
				s := New(24, 80)
				// Warm outside the measurement: the first write is the only one
				// that changes the grid, and AllocsPerRun's own single warm-up
				// pass would otherwise be doing that work.
				for range 3 {
					s.Write(payload)
				}
				if y, x := s.CursorPos(); y != 0 || x != 0 {
					t.Fatalf("Screen.Write(%s frame, %d bytes) left the cursor at row %d col %d, want 0,0: the fixture must be a fixed point or the measurement below is averaging different screen states",
						name, len(payload), y, x)
				}
				if len(s.Drained) != 0 {
					t.Fatalf("Screen.Write(%s frame, %d bytes) drained %d rows, want 0: the fixture must not scroll, or it is measuring the scrollback path instead",
						name, len(payload), len(s.Drained))
				}
				got := testing.AllocsPerRun(allocRuns, func() {
					s.Write(payload)
				})
				if got != 0 {
					t.Errorf("Screen.Write(%s frame, %d bytes) allocated %v times per run, want 0: a byte of PTY output must cost no allocation, or output volume turns into allocation pressure on every session",
						name, len(payload), got)
				}
			}
		})
	}
}

// TestScreenWriteIsAllocationFreeRepaintingEveryCell answers the objection the
// test above invites: its frames redraw one row, so they touch 40 cells however
// many bytes they carry, and a cost charged per CELL rather than per byte would
// survive it.
//
// These frames address every row absolutely and fill it, so one frame writes
// every cell in the grid — 1,920 on the default screen and 40,000 on the
// 100x400 case, which is wider and taller than any real client. Repeating a
// frame is still a fixed point because it opens with CUP home.
func TestScreenWriteIsAllocationFreeRepaintingEveryCell(t *testing.T) {
	grids := map[string]struct {
		rows, cols, reps int
	}{
		"default_80x24":   {24, 80, 16},
		"wide_400x100":    {100, 400, 2},
		"narrow_40x24":    {24, 40, 16},
		"tall_24x1000":    {1000, 24, 2},
		"single_row_1x80": {1, 80, 16},
	}
	for name, g := range grids {
		t.Run(name, func(t *testing.T) {
			payload := []byte(strings.Repeat(allocRepaintFrame(g.rows, g.cols), g.reps))
			s := New(g.rows, g.cols)
			for range 3 {
				s.Write(payload)
			}
			if len(s.Drained) != 0 {
				t.Fatalf("Screen.Write(%d repaints of a %dx%d grid, %d bytes) drained %d rows, want 0: absolute cursor addressing must not scroll",
					g.reps, g.rows, g.cols, len(payload), len(s.Drained))
			}
			got := testing.AllocsPerRun(allocRuns, func() {
				s.Write(payload)
			})
			if got != 0 {
				t.Errorf("Screen.Write(%d repaints of a %dx%d grid, %d bytes, %d cells per frame) allocated %v times per run, want 0: a repaint must reuse the cell grid New allocated rather than allocate per cell",
					g.reps, g.rows, g.cols, len(payload), g.rows*g.cols, got)
			}
		})
	}
}

// TestScreenWriteAllocationCountIsIndependentOfChunking pins the same cost
// against the axis the CONSUMER controls rather than the program: the terminal
// handler calls Write once per PTY read, so the same output arrives as one large
// slice on a quiet system and as dozens of small ones under load.
//
// The parser carries its state across calls (parse.go), which is what makes a
// sequence split across two reads work; this says that carrying it costs
// nothing, so no per-call buffer has appeared on a path that runs at read rate.
func TestScreenWriteAllocationCountIsIndependentOfChunking(t *testing.T) {
	payload := []byte(strings.Repeat(strings.Repeat("x", 40)+"\r", 800))
	// Split at 512 bytes, which lands mid-frame and mid-escape-sequence rather
	// than on a frame boundary, so the split state is real parser state.
	chunks := make([][]byte, 0, len(payload)/512+1)
	for i := 0; i < len(payload); i += 512 {
		chunks = append(chunks, payload[i:min(i+512, len(payload))])
	}

	whole := New(24, 80)
	split := New(24, 80)
	writeSplit := func() {
		for _, c := range chunks {
			split.Write(c)
		}
	}
	for range 3 {
		whole.Write(payload)
		writeSplit()
	}
	if whole.RowString(0) != split.RowString(0) {
		t.Fatalf("Screen.Write(%d bytes) as one call and as %d chunks produced different row 0 content (%d bytes against %d): the two fixtures must be equivalent",
			len(payload), len(chunks), len(whole.RowString(0)), len(split.RowString(0)))
	}

	gotWhole := testing.AllocsPerRun(allocRuns, func() {
		whole.Write(payload)
	})
	gotSplit := testing.AllocsPerRun(allocRuns, writeSplit)
	if gotWhole != 0 || gotSplit != 0 {
		t.Errorf("Screen.Write(%d bytes) allocated %v times per run as one call and %v times as %d chunks, want 0 and 0: parser state carried across Write boundaries must not cost a per-call buffer, or a busy session pays per PTY read",
			len(payload), gotWhole, gotSplit, len(chunks))
	}
}

// TestScreenWriteAllocationCountIsIndependentOfSequencePayloadLength covers the
// classes that are NOT allocation-free, and pins the property that matters for
// them.
//
// A sequence that captures a payload has to materialise it: OSC 0 keeps the
// title, OSC 8 keeps the URI on every cell it stamps, OSC 4 records a palette
// override, OSC 52 queues a clipboard write, and a DCS query queues a reply for
// the application. Each costs a small constant, and the constant is what a
// consumer pays per sequence. What must not happen is the count tracking the
// payload's LENGTH, because that length is chosen by the program on the other
// end of the PTY: a title or a clipboard payload can be as long as the OSC
// buffer allows (maxOSCLen, 4 KiB), so a count that grew with it would be an
// amplification vector reachable by anything that can print.
//
// The lengths span 4 bytes to 4000, a thousandfold, and the assertion is an
// equality against the count measured at the shortest payload rather than a
// threshold picked to pass. TakeResponse and TakeClipboard are called inside the
// closure because the consumer drains both every frame; without that the queues
// grow and the measurement drifts.
func TestScreenWriteAllocationCountIsIndependentOfSequencePayloadLength(t *testing.T) {
	sequences := map[string]func(n int) string{
		"osc0_window_title": func(n int) string {
			return "\x1b]0;" + strings.Repeat("t", n) + "\x07"
		},
		"osc8_hyperlink_uri": func(n int) string {
			return "\x1b]8;;https://example.com/" + strings.Repeat("p", n) + "\x07ab\x1b]8;;\x07"
		},
		"osc52_clipboard": func(n int) string {
			return "\x1b]52;c;" + strings.Repeat("aGVsbG8h", n/8+1) + "\x07"
		},
		"dcs_decrqss_query": func(n int) string {
			return "\x1bP$q" + strings.Repeat("m", n) + "\x1b\\"
		},
		"csi_parameter_list": func(n int) string {
			return "\x1b[" + strings.Repeat("1;", n) + "1mx\r"
		},
	}
	// 64 rather than 4 is the shortest measured length for the comparison: a
	// handful of bytes can sit under a size class the allocator rounds up to, so
	// the first step is a step in the ALLOCATOR and not in this package (the DCS
	// reply measured 1 at 4 bytes and 2 from 64 bytes up, then stayed flat to
	// 4000). The 4-byte case is still measured and logged, just not the baseline.
	lengths := []int{4, 64, 1024, 4000}

	for name, build := range sequences {
		t.Run(name, func(t *testing.T) {
			counts := make([]float64, len(lengths))
			for i, n := range lengths {
				payload := []byte(build(n))
				s := New(24, 80)
				body := func() {
					s.Write(payload)
					s.TakeResponse()
					s.TakeClipboard()
				}
				for range 3 {
					body()
				}
				counts[i] = testing.AllocsPerRun(allocRuns, body)
			}
			// counts[1] is the 64-byte reading; every longer payload must match
			// it exactly.
			for i, got := range counts[1:] {
				if got != counts[1] {
					t.Errorf("Screen.Write(%s, %d-byte payload) allocated %v times per run, want %v (its count at a 64-byte payload): the cost of a captured sequence must not track a length the program on the other end of the PTY chooses",
						name, lengths[i+1], got, counts[1])
				}
			}
			t.Logf("%s: a constant %v allocations from a 64-byte to a 4000-byte payload (%v at 4 bytes)", name, counts[1], counts[0])
		})
	}
}

// TestScreenWriteNoOpIsAllocationFree checks the reverse direction. A write that
// changes nothing runs constantly — an empty PTY read, a bare carriage return, a
// redundant SGR reset, a cursor home before an unchanged frame, a sequence this
// emulator does not implement — so a buffer allocated on the way to doing
// nothing is a real defect on a path that never stops.
func TestScreenWriteNoOpIsAllocationFree(t *testing.T) {
	noOps := map[string][]byte{
		"nil_slice":            nil,
		"empty_slice":          {},
		"cursor_home":          []byte("\x1b[H"),
		"carriage_return":      []byte("\r"),
		"sgr_reset":            []byte("\x1b[0m"),
		"unimplemented_csi":    []byte("\x1b[99999;99999Z"),
		"cancelled_sequence":   []byte("\x1b[31\x18"),
		"identical_repaint":    []byte("\x1b[Hhello world"),
		"save_restore_cursor":  []byte("\x1b7\x1b8"),
		"unimplemented_esc_dl": []byte("\x1b#3"),
	}
	for name, payload := range noOps {
		t.Run(name, func(t *testing.T) {
			s := New(24, 80)
			s.Write([]byte("\x1b[Hhello world"))
			for range 3 {
				s.Write(payload)
			}
			got := testing.AllocsPerRun(500, func() {
				s.Write(payload)
			})
			if got != 0 {
				t.Errorf("Screen.Write(%s, %d bytes) allocated %v times per run, want 0: a write that changes nothing must cost nothing, and every one of these arrives constantly on an idle session",
					name, len(payload), got)
			}
		})
	}
}

// TestScrollAllocationCostIsOneRowPerLineAtEveryWidth pins what a scrolled line
// costs, and it is one of the two places the expected property did NOT hold:
// rows are not recycled. scrollUpOnce (csi.go) shifts the surviving rows down by
// one slice element each and mints a FRESH blank row with makeRow, so a scroll
// allocates once per scrolled line rather than never.
//
// One allocation per line is still the contract worth holding, because it says
// the cost is charged per ROW and never per cell: the count is identical at 40,
// 80 and 400 columns, so a 400-column screen scrolls for the same number of
// allocations as a 40-column one (more bytes, same count). A rewrite that
// rebuilt each row cell by cell, or that copied cell ranges instead of moving
// row slices, would show up here as a count proportional to the width while
// BenchmarkScreenWrite barely moved.
//
// The fixture sets a scroll region starting at row 2 (DECSTBM), which keeps
// scrollUpOnce off the scrollback path — a full-screen scroll also drains, and
// draining is the next contract. Warming with 40 line feeds parks the cursor at
// the bottom margin BEFORE the measurement, so every measured call scrolls
// exactly as many times as the payload has line feeds; without that warm-up the
// early calls do not scroll at all and AllocsPerRun's integer division floors
// the result to something lower and meaningless.
func TestScrollAllocationCostIsOneRowPerLineAtEveryWidth(t *testing.T) {
	widths := []int{40, 80, 400}
	lineCounts := []int{1, 8, 64, 512}

	for _, lines := range lineCounts {
		payload := []byte(strings.Repeat(strings.Repeat("x", 20)+"\r\n", lines))
		counts := make([]float64, len(widths))
		for i, cols := range widths {
			s := New(24, cols)
			s.Write([]byte("\x1b[2;24r"))
			s.Write([]byte(strings.Repeat("\r\n", 40)))
			for range 3 {
				s.Write(payload)
			}
			if len(s.Drained) != 0 {
				t.Fatalf("Screen.Write(%d line feeds inside a scroll region, %d columns) drained %d rows, want 0: a region that does not start at row 0 must not reach the scrollback path",
					lines, cols, len(s.Drained))
			}
			counts[i] = testing.AllocsPerRun(allocRuns, func() {
				s.Write(payload)
			})
			if counts[i] != float64(lines) {
				t.Errorf("Screen.Write(%d line feeds inside a scroll region, %d columns) allocated %v times per run, want %d (one blank row per scrolled line): a higher count means a scroll now costs more than the row it mints, and a lower one means rows are being recycled, which is an improvement worth retightening this contract to",
					lines, cols, counts[i], lines)
			}
		}
		for i, got := range counts[1:] {
			if got != counts[0] {
				t.Errorf("Screen.Write(%d line feeds inside a scroll region) allocated %v times per run at %d columns and %v at %d, want the same count: scroll cost must be charged per row and never per cell",
					lines, got, widths[i+1], counts[0], widths[0])
			}
		}
	}
}

// TestScrollbackDrainAllocationCostIsBoundedPerScrolledLine pins the full-screen
// scroll, which is the one a shell actually produces: scrollUpOnce drains row 0
// into Drained first (screen.go drainTopRow), converting it to wire runs and
// scanning it for URLs, and the consumer's flush loop takes the result with
// DrainScrollback on the next frame.
//
// The closure therefore writes AND drains, because that pair is the production
// steady state. Draining inside the measurement is what makes the number mean
// something: Drained is unbounded until a consumer empties it, so a closure that
// only writes measures a screen whose retained history grows on every one of
// AllocsPerRun's iterations.
//
// Two properties, both bounds rather than equalities because the per-line cost
// legitimately includes strings.Builder growth, which steps with the row width:
// the per-line cost is a small constant (measured 8.02 at 40 columns rising to
// 11.02 at 400), and it does not grow with the number of lines scrolled in one
// write, which is what would happen if the drain became quadratic in the batch.
func TestScrollbackDrainAllocationCostIsBoundedPerScrolledLine(t *testing.T) {
	// Deliberately generous: the point is that the number is a small constant
	// per line, not that it is any particular value.
	const maxAllocsPerLine = 16.0
	// A tenfold width increase may add this much per line and no more. Measured
	// premium is 3 (8.02 to 11.02), all of it strings.Builder growth steps.
	const maxWidthPremium = 4.0

	widths := []int{40, 400}
	lineCounts := []int{8, 64, 512}
	rates := make(map[int][]float64, len(widths))

	for _, cols := range widths {
		for _, lines := range lineCounts {
			payload := []byte(strings.Repeat(strings.Repeat("x", cols/2)+"\r\n", lines))
			s := New(24, cols)
			body := func() {
				s.Write(payload)
				s.DrainScrollback()
			}
			for range 5 {
				body()
			}
			got := testing.AllocsPerRun(allocRuns, body)
			rate := got / float64(lines)
			rates[cols] = append(rates[cols], rate)
			if rate > maxAllocsPerLine {
				t.Errorf("Screen.Write(%d scrolled lines, %d columns, %d bytes) plus DrainScrollback allocated %v times per run, a rate of %v per line, want at most %v: a scrolled row must cost a small constant to carry into scrollback, or a chatty build log turns into allocation pressure proportional to its own length",
					lines, cols, len(payload), got, rate, maxAllocsPerLine)
			}
		}
		first, last := rates[cols][0], rates[cols][len(rates[cols])-1]
		if last > first+1 {
			t.Errorf("Screen.Write(scrolled lines, %d columns) plus DrainScrollback allocated %v per line at %d lines and %v per line at %d, want the larger batch to cost no more per line: a rate that grows with the batch means the drain is no longer one pass per row",
				cols, first, lineCounts[0], last, lineCounts[len(lineCounts)-1])
		}
	}

	narrow, wide := rates[widths[0]][len(rates[widths[0]])-1], rates[widths[1]][len(rates[widths[1]])-1]
	if wide > narrow+maxWidthPremium {
		t.Errorf("Screen.Write(%d scrolled lines) plus DrainScrollback allocated %v per line at %d columns and %v per line at %d, want at most %v more: a per-line cost that tracks the width is a per-cell cost, and a wide client would pay for every line a program prints",
			lineCounts[len(lineCounts)-1], narrow, widths[0], wide, widths[1], maxWidthPremium)
	}
	t.Logf("scrollback drain costs %v allocations per line at %d columns and %v at %d", narrow, widths[0], wide, widths[1])
}

// TestMarginsDefaultToTheScreenEdges: with no DECSLRM ever sent, enabling
// DECLRMM finds the left/right margins already at the screen's own columns, and
// a resize moves them to the new width. A margin past the last column would put
// the autowrap edge outside the buffer.
func TestMarginsDefaultToTheScreenEdges(t *testing.T) {
	s := New(4, 20)
	if _, err := s.Write([]byte("\x1b[?69h\x1bP$qs\x1b\\")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := string(s.TakeResponse()), "\x1bP1$r1;20s\x1b\\"; got != want {
		t.Errorf("DECRQSS DECSLRM on a fresh 20-column screen = %q, want %q", got, want)
	}
	s.Resize(4, 30)
	if _, err := s.Write([]byte("\x1bP$qs\x1b\\")); err != nil {
		t.Fatalf("write after resize: %v", err)
	}
	if got, want := string(s.TakeResponse()), "\x1bP1$r1;30s\x1b\\"; got != want {
		t.Errorf("DECRQSS DECSLRM after Resize(4, 30) = %q, want %q", got, want)
	}
}

// TestAutowrapEdgeInsideAndOutsideAMarginBox: text typed inside a DECSLRM box
// wraps at the RIGHT MARGIN and lands on the left margin, while text typed
// outside the box wraps at the screen's last column and lands on column 0. The
// cursor sitting exactly ON an edge is the case that decides which of the two
// rules applies, so each case fills its edge column and then writes one more
// character.
func TestAutowrapEdgeInsideAndOutsideAMarginBox(t *testing.T) {
	// DECSLRM 5;10 -> 0-indexed margins 4..9 on a 20-column screen.
	const box = "\x1b[?69h\x1b[5;10s"
	cases := []struct {
		name          string
		cup           string
		wantFirstRow  string
		wantSecondRow string
		wantCursorCol int
	}{
		{
			name:          "inside_the_box_wraps_at_the_right_margin",
			cup:           "\x1b[1;10H", // the right margin, 0-indexed 9
			wantFirstRow:  "         a",
			wantSecondRow: "    b",
			wantCursorCol: 5,
		},
		{
			name:          "outside_the_box_wraps_at_the_screen_edge",
			cup:           "\x1b[1;20H", // the last column, right of the box
			wantFirstRow:  "                   a",
			wantSecondRow: "b",
			wantCursorCol: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(3, 20)
			if _, err := s.Write([]byte(box + tc.cup + "ab")); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := s.RowString(0); got != tc.wantFirstRow {
				t.Errorf("RowString(0) = %q, want %q", got, tc.wantFirstRow)
			}
			if got := s.RowString(1); got != tc.wantSecondRow {
				t.Errorf("RowString(1) = %q, want %q", got, tc.wantSecondRow)
			}
			if _, got := s.CursorPos(); got != tc.wantCursorCol {
				t.Errorf("CursorPos() column = %d, want %d", got, tc.wantCursorCol)
			}
		})
	}
}

// TestAltScreenBufferLifetimePerMode pins what each alt-screen mode promises
// about the buffer it switches to. Mode 47 is the legacy shape: it shares the
// cursor with the main screen and its buffer survives an exit, so re-entering
// finds the previous alt session's content. Mode 1049 is the modern one every
// full-screen TUI uses: it always starts cleared with the cursor homed, and it
// discards the buffer on exit, so a second enter cannot leak the first session's
// screen.
func TestAltScreenBufferLifetimePerMode(t *testing.T) {
	t.Run("mode_47_keeps_its_buffer_and_the_cursor", func(t *testing.T) {
		s := New(3, 10)
		if _, err := s.Write([]byte("main")); err != nil {
			t.Fatalf("write main: %v", err)
		}
		if _, err := s.Write([]byte("\x1b[?47h")); err != nil {
			t.Fatalf("enter alt: %v", err)
		}
		if _, got := s.CursorPos(); got != 4 {
			t.Errorf("CursorPos() column after CSI ?47h = %d, want 4 (mode 47 shares the cursor)", got)
		}
		if _, err := s.Write([]byte("\x1b[Halt\x1b[?47l")); err != nil {
			t.Fatalf("write alt and exit: %v", err)
		}
		if got := s.RowString(0); got != "main" {
			t.Errorf("RowString(0) after exit = %q, want %q", got, "main")
		}
		if _, err := s.Write([]byte("\x1b[?47h")); err != nil {
			t.Fatalf("re-enter alt: %v", err)
		}
		if got := s.RowString(0); got != "alt" {
			t.Errorf("RowString(0) on mode 47 re-enter = %q, want %q (the buffer survives an exit)", got, "alt")
		}
	})

	t.Run("mode_1049_starts_cleared_and_homed", func(t *testing.T) {
		s := New(3, 10)
		if _, err := s.Write([]byte("main")); err != nil {
			t.Fatalf("write main: %v", err)
		}
		if _, err := s.Write([]byte("\x1b[?1049h")); err != nil {
			t.Fatalf("enter alt: %v", err)
		}
		if row, col := s.CursorPos(); row != 0 || col != 0 {
			t.Errorf("CursorPos() after CSI ?1049h = (%d, %d), want (0, 0)", row, col)
		}
		if _, err := s.Write([]byte("alt\x1b[?1049l\x1b[?1049h")); err != nil {
			t.Fatalf("write alt, exit and re-enter: %v", err)
		}
		if got := s.RowString(0); got != "" {
			t.Errorf("RowString(0) on mode 1049 re-enter = %q, want empty (1049 always starts cleared)", got)
		}
	})

	t.Run("a_screen_with_no_rows_reuses_no_stashed_buffer", func(t *testing.T) {
		// A zero-dimension screen has no row 0 to compare widths against, so the
		// stashed buffer can never match and must not be inspected.
		s := New(0, 10)
		if _, err := s.Write([]byte("\x1b[?47h\x1b[?47l\x1b[?47h")); err != nil {
			t.Fatalf("alt enter/exit/enter: %v", err)
		}
		if !s.InAltScreen {
			t.Errorf("InAltScreen = false after CSI ?47h, want true")
		}
	})
}
