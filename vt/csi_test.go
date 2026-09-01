package vt

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// --- Intermediate-byte guard routing ---

// TestCSIBangGuardRequiresInterm verifies the DECSTR ('!' intermediate) branch
// only fires when an intermediate byte is actually present: a stale '!' left in
// the buffer with numInterm==0 must not trigger a soft reset.
func TestCSIBangGuardRequiresInterm(t *testing.T) {
	s := New(10, 10)
	s.Write([]byte("\x1b[!p"))   // DECSTR: leaves pIntermed[0]=='!', numInterm==1
	s.Write([]byte("\x1b[3;5r")) // DECSTBM: scrollTop=2, scrollBottom=4, numInterm back to 0
	if s.scrollTop != 2 {
		t.Fatalf("precondition: scrollTop = %d, want 2 after CSI 3;5 r", s.scrollTop)
	}
	s.Write([]byte("\x1b[p")) // CSI p with numInterm==0: 'p' unhandled, scroll region intact
	if s.scrollTop != 2 {
		t.Errorf("scrollTop after CSI p with numInterm==0 = %d, want 2 (DECSTR must require an intermediate)", s.scrollTop)
	}
}

// TestCSISpaceGuardRequiresInterm verifies the SP-intermediate branch only fires
// with an intermediate present: a stale ' ' with numInterm==0 must run the
// main-switch 'A' (cursor up), not the SP-branch 'A' (SR).
func TestCSISpaceGuardRequiresInterm(t *testing.T) {
	s := New(10, 10)
	s.Write([]byte("\x1b[6;1H")) // row 5
	s.Write([]byte("\x1b[ q"))   // DECSCUSR: leaves pIntermed[0]==' ', numInterm==1
	s.Write([]byte("\x1b[2A"))   // CSI 2 A with numInterm==0 -> cursor up
	if row, _ := s.CursorPos(); row != 3 {
		t.Errorf("curY after CSI 2 A with numInterm==0 = %d, want 3 (SP branch must require an intermediate)", row)
	}
}

// TestCSIDollarGuardRequiresInterm verifies the '$' (DECRQM) branch only fires
// with an intermediate present: a stale '$' with numInterm==0 must not emit a
// DECRQM reply.
func TestCSIDollarGuardRequiresInterm(t *testing.T) {
	s := New(10, 10)
	s.Write([]byte("\x1b[$p")) // DECRQM: leaves pIntermed[0]=='$', numInterm==1
	s.response = nil
	s.Write([]byte("\x1b[6p")) // CSI 6 p with numInterm==0: 'p' unhandled, no reply
	if len(s.response) != 0 {
		t.Errorf("len(Response) after CSI 6 p with numInterm==0 = %d, want 0 (DECRQM must require an intermediate)", len(s.response))
	}
}

// --- DECSCUSR cursor style / blink ---

// TestDECSCUSRCursorBlink verifies DECSCUSR sets both the cursor style and the
// blink flag: even styles are steady, 0 and odd styles blink.
func TestDECSCUSRCursorBlink(t *testing.T) {
	cases := []struct {
		seq       string
		v         int
		wantStyle uint8
		wantBlink bool
	}{
		{"\x1b[0 q", 0, 0, true},
		{"\x1b[1 q", 1, 1, true},
		{"\x1b[2 q", 2, 2, false},
		{"\x1b[3 q", 3, 3, true},
		{"\x1b[4 q", 4, 4, false},
		{"\x1b[5 q", 5, 5, true},
		{"\x1b[6 q", 6, 6, false},
	}
	for _, tc := range cases {
		s := New(2, 5)
		// Force the opposite prior state so a passing assertion proves a write.
		s.CursorBlink = !tc.wantBlink
		s.CursorStyle = 99
		s.Write([]byte(tc.seq))
		if s.CursorBlink != tc.wantBlink {
			t.Errorf("CursorBlink after %q (v=%d) = %v, want %v", tc.seq, tc.v, s.CursorBlink, tc.wantBlink)
		}
		if s.CursorStyle != tc.wantStyle {
			t.Errorf("CursorStyle after %q (v=%d) = %d, want %d", tc.seq, tc.v, s.CursorStyle, tc.wantStyle)
		}
	}
}

// --- Cursor movement and clamping ---

// TestCursorMovementBasic verifies CUP positions the cursor (1-indexed -> 0-indexed)
// and CUU/CUD move it by one row.
func TestCursorMovementBasic(t *testing.T) {
	s := New(10, 20)
	s.Write([]byte("\x1b[5;10H"))
	if row, col := s.CursorPos(); row != 4 || col != 9 {
		t.Errorf("CUP 5;10 = %d,%d, want 4,9", row, col)
	}
	s.Write([]byte("\x1b[A")) // up
	if row, _ := s.CursorPos(); row != 3 {
		t.Errorf("CUU: row=%d, want 3", row)
	}
	s.Write([]byte("\x1b[B")) // down
	if row, _ := s.CursorPos(); row != 4 {
		t.Errorf("CUD: row=%d, want 4", row)
	}
}

// TestCUDClampsAtHeight verifies cursor-down clamps at the bottom row.
func TestCUDClampsAtHeight(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("\x1b[5B")) // down 5 from row 0 reaches Height(5)
	if row, _ := s.CursorPos(); row != 4 {
		t.Errorf("curY after CSI 5 B = %d, want 4 (clamped to Height-1)", row)
	}
}

// TestCHAClampsAtWidth verifies CHA (cursor horizontal absolute) clamps at the
// last column.
func TestCHAClampsAtWidth(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("\x1b[11G")) // CHA col 11 -> curX = 10 == Width
	if _, col := s.CursorPos(); col != 9 {
		t.Errorf("curX after CSI 11 G = %d, want 9 (clamped to Width-1)", col)
	}
}

// TestCUPClampsRowAtHeight verifies CUP clamps the row at Height-1.
func TestCUPClampsRowAtHeight(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("\x1b[6H")) // row 6 -> y = 5 == Height (origin mode off)
	if row, _ := s.CursorPos(); row != 4 {
		t.Errorf("curY after CSI 6 H = %d, want 4", row)
	}
}

// TestCUPClampsColAtWidth verifies CUP clamps the column at Width-1.
func TestCUPClampsColAtWidth(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("\x1b[1;11H")) // row 1, col 11 -> x = 10 == Width
	row, col := s.CursorPos()
	if row != 0 {
		t.Errorf("curY after CSI 1;11 H = %d, want 0", row)
	}
	if col != 9 {
		t.Errorf("curX after CSI 1;11 H = %d, want 9", col)
	}
}

// TestOriginModeRelativeCUP verifies that with origin mode on, CUP is relative
// to the scroll-region top.
func TestOriginModeRelativeCUP(t *testing.T) {
	s := New(10, 10)
	s.Write([]byte("\x1b[3;8r")) // scroll region rows 3-8
	s.Write([]byte("\x1b[?6h"))  // origin mode on
	s.Write([]byte("\x1b[1;1H")) // relative to region top
	if row, col := s.CursorPos(); row != 2 || col != 0 {
		t.Errorf("origin-mode CUP 1;1 = %d,%d, want 2,0", row, col)
	}
}

// --- Scroll region ---

// TestSetScrollRegion verifies DECSTBM sets the scroll region (1-indexed input
// to 0-indexed bounds).
func TestSetScrollRegion(t *testing.T) {
	s := New(10, 20)
	s.Write([]byte("\x1b[3;8r"))
	if s.scrollTop != 2 || s.scrollBottom != 7 {
		t.Errorf("scroll region top=%d bot=%d, want 2,7", s.scrollTop, s.scrollBottom)
	}
}

// TestDECSTBMBottomClamp verifies the scroll-region bottom clamps to Height-1.
func TestDECSTBMBottomClamp(t *testing.T) {
	s := New(10, 20)              // Height=10
	s.Write([]byte("\x1b[1;11r")) // bottom=11-1=10 (==Height), must clamp to 9
	if s.scrollBottom != 9 {
		t.Errorf("DECSTBM bottom=Height: scrollBottom = %d, want 9", s.scrollBottom)
	}
}

// TestDECSTBMTopEqualsBottom verifies a degenerate region (top==bottom) is
// rejected, leaving the region unchanged.
func TestDECSTBMTopEqualsBottom(t *testing.T) {
	s := New(10, 20)
	s.Write([]byte("\x1b[5;5r")) // top=4, bottom=4: region must stay 0..9
	if s.scrollTop != 0 || s.scrollBottom != 9 {
		t.Errorf("DECSTBM top==bottom: scrollTop,scrollBottom = %d,%d, want 0,9", s.scrollTop, s.scrollBottom)
	}
}

// TestScrollUpRegionHeightCap verifies SU drains at most region-height lines on
// a full-screen region.
func TestScrollUpRegionHeightCap(t *testing.T) {
	s := New(4, 5) // full-screen region: regionH=4
	s.Write([]byte("\x1b[4S"))
	if len(s.Drained) != 4 {
		t.Errorf("len(Drained) after CSI 4 S = %d, want 4", len(s.Drained))
	}
}

// TestScrollUpShiftsContent verifies SU shifts content up by one line.
func TestScrollUpShiftsContent(t *testing.T) {
	s := New(3, 5)
	s.Write([]byte("Line1\r\nLine2\r\nLine3"))
	s.Write([]byte("\x1b[1S"))
	if got := s.RowString(0); got != "Line2" {
		t.Errorf("after SU: row0=%q, want Line2", got)
	}
}

// TestScrollDownNoCapBelowRegion verifies SD by less than the region height
// performs a partial scroll rather than blanking the whole region.
func TestScrollDownNoCapBelowRegion(t *testing.T) {
	s := New(4, 5)
	s.Write([]byte("\x1b[1;1HAAAA"))
	s.Write([]byte("\x1b[2;1HBBBB"))
	s.Write([]byte("\x1b[3;1HCCCC"))
	s.Write([]byte("\x1b[4;1HDDDD"))
	s.Write([]byte("\x1b[1T")) // SD 1
	if got := s.RowString(1); got != "AAAA" {
		t.Errorf("RowString(1) after CSI 1 T = %q, want %q", got, "AAAA")
	}
}

// TestDeepScrollRegionOps verifies large scroll/insert/delete counts in a
// single-line scroll region keep the cursor in bounds.
func TestDeepScrollRegionOps(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("\x1b[3;3r")) // single-line region
	s.Write([]byte("\x1b[100S"))
	s.Write([]byte("\x1b[100T"))
	s.Write([]byte("\x1b[100L"))
	s.Write([]byte("\x1b[100M"))
	row, col := s.CursorPos()
	if col < 0 || col >= s.Width {
		t.Fatalf("col %d out of bounds", col)
	}
	if row < 0 || row >= s.Height {
		t.Fatalf("row %d out of bounds", row)
	}
}

// --- Erase ---

// TestEraseDisplayAll verifies ED 2 (CSI 2J) clears the whole screen.
func TestEraseDisplayAll(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("AAAAAAAAAA"))
	s.Write([]byte("\x1b[1;1H"))
	s.Write([]byte("\x1b[2J"))
	for x := range s.Width {
		if s.Cells[0][x].Ch != ' ' {
			t.Fatalf("ED 2J: cell[0][%d]=%q, want space", x, s.Cells[0][x].Ch)
		}
	}
}

// --- Insert / delete chars and lines ---

// TestInsertCharsShifts verifies ICH inserts blanks at the cursor and shifts
// existing content right.
func TestInsertCharsShifts(t *testing.T) {
	s := New(1, 10)
	s.Write([]byte("ABCDE"))
	s.Write([]byte("\x1b[1;2H")) // cursor at col 1
	s.Write([]byte("\x1b[2@"))   // insert 2 chars
	if s.Cells[0][1].Ch != ' ' || s.Cells[0][2].Ch != ' ' {
		t.Error("ICH did not insert spaces")
	}
	if s.Cells[0][3].Ch != 'B' {
		t.Errorf("ICH shifted wrong: got %q, want B", s.Cells[0][3].Ch)
	}
}

// TestInsertCharsAtLastCol verifies inserting more chars than fit at the last
// column keeps the cursor in bounds and blanks every column from the cursor to
// the right margin inclusive (the shifted-out tail is lost).
func TestInsertCharsAtLastCol(t *testing.T) {
	s := New(3, 5)
	s.Write([]byte("ABCDE"))
	s.Write([]byte("\x1b[4G"))  // col 3
	s.Write([]byte("\x1b[10@")) // insert more than available
	row, col := s.CursorPos()
	if col < 0 || col >= s.Width {
		t.Fatalf("col %d out of bounds", col)
	}
	if row < 0 || row >= s.Height {
		t.Fatalf("row %d out of bounds", row)
	}
	if got, want := s.RowString(0), "ABC"; got != want {
		t.Errorf("RowString(0) after CSI 10 @ at col 3 = %q, want %q (cols 3..4 blanked)", got, want)
	}
}

// TestInsertCharsGuardAtHeight verifies insertChars early-returns when curY is
// out of range rather than indexing out of bounds.
func TestInsertCharsGuardAtHeight(t *testing.T) {
	s := New(5, 5)
	s.curY = s.Height // out of range
	if didPanic(func() { s.insertChars(1) }) {
		t.Errorf("insertChars with curY==Height panicked; the guard must early-return")
	}
}

// TestDeleteCharsGuardAtHeight verifies deleteChars early-returns when curY is
// out of range.
func TestDeleteCharsGuardAtHeight(t *testing.T) {
	s := New(5, 5)
	s.curY = s.Height
	if didPanic(func() { s.deleteChars(1) }) {
		t.Errorf("deleteChars with curY==Height panicked; the guard must early-return")
	}
}

// TestDeleteCharsClampToWidth verifies the delete count clamps to Width-curX so
// columns left of the cursor survive and the loop stays in bounds.
func TestDeleteCharsClampToWidth(t *testing.T) {
	s := New(5, 10)
	s.curY = 0
	s.curX = 2
	for x, r := range "ABCDEFGHIJ" {
		s.Cells[0][x] = Cell{Ch: r}
	}
	if didPanic(func() { s.deleteChars(10) }) {
		t.Fatalf("deleteChars(10) panicked; clamp must be Width-curX")
	}
	if s.Cells[0][0].Ch != 'A' {
		t.Errorf("deleteChars(10) at curX=2: Cells[0][0].Ch = %q, want 'A' (cols left of cursor preserved)", s.Cells[0][0].Ch)
	}
	if s.Cells[0][1].Ch != 'B' {
		t.Errorf("deleteChars(10) at curX=2: Cells[0][1].Ch = %q, want 'B'", s.Cells[0][1].Ch)
	}
	if s.Cells[0][2].Ch != ' ' {
		t.Errorf("deleteChars(10) at curX=2: Cells[0][2].Ch = %q, want ' ' (erased region starts at cursor)", s.Cells[0][2].Ch)
	}
}

// TestInsertLinesGuardAtScrollBottom verifies insertLines proceeds when curY
// equals scrollBottom (blanking that row).
func TestInsertLinesGuardAtScrollBottom(t *testing.T) {
	s := New(5, 5)
	s.scrollTop = 0
	s.scrollBottom = 4
	s.curY = s.scrollBottom
	s.Cells[4][0] = Cell{Ch: 'Z'}
	s.insertLines(1)
	if s.Cells[4][0].Ch != ' ' {
		t.Errorf("insertLines at curY==scrollBottom: Cells[4][0].Ch = %q, want ' '", s.Cells[4][0].Ch)
	}
}

// TestInsertLinesClampToAvail verifies insertLines(1) shifts exactly one row
// down rather than clamping up to the available region height.
func TestInsertLinesClampToAvail(t *testing.T) {
	s := New(5, 5)
	s.scrollTop = 0
	s.scrollBottom = 4
	s.curY = 1
	s.Cells[1][0] = Cell{Ch: '1'}
	s.Cells[2][0] = Cell{Ch: '2'}
	s.Cells[3][0] = Cell{Ch: '3'}
	s.Cells[4][0] = Cell{Ch: '4'}
	s.insertLines(1)
	if s.Cells[2][0].Ch != '1' {
		t.Errorf("insertLines(1) at curY=1: Cells[2][0].Ch = %q, want '1'", s.Cells[2][0].Ch)
	}
	if s.Cells[4][0].Ch != '3' {
		t.Errorf("insertLines(1) at curY=1: Cells[4][0].Ch = %q, want '3'", s.Cells[4][0].Ch)
	}
}

// TestDeleteLinesGuardAtScrollBottom verifies deleteLines proceeds when curY
// equals scrollBottom.
func TestDeleteLinesGuardAtScrollBottom(t *testing.T) {
	s := New(5, 5)
	s.scrollTop = 0
	s.scrollBottom = 4
	s.curY = s.scrollBottom
	s.Cells[4][0] = Cell{Ch: 'Z'}
	s.deleteLines(1)
	if s.Cells[4][0].Ch != ' ' {
		t.Errorf("deleteLines at curY==scrollBottom: Cells[4][0].Ch = %q, want ' '", s.Cells[4][0].Ch)
	}
}

// TestDeleteLinesAvailClamp verifies the available-line count is region-bottom
// minus curY plus one, so deleting that many clears the region.
func TestDeleteLinesAvailClamp(t *testing.T) {
	s := New(5, 5)
	s.scrollTop = 0
	s.scrollBottom = 4
	s.curY = 1
	s.Cells[1][0] = Cell{Ch: '1'}
	s.Cells[2][0] = Cell{Ch: '2'}
	s.Cells[3][0] = Cell{Ch: '3'}
	s.Cells[4][0] = Cell{Ch: '4'}
	s.deleteLines(4)
	if s.Cells[1][0].Ch != ' ' {
		t.Errorf("deleteLines(4) at curY=1: Cells[1][0].Ch = %q, want ' '", s.Cells[1][0].Ch)
	}
	if s.Cells[2][0].Ch != ' ' {
		t.Errorf("deleteLines(4) at curY=1: Cells[2][0].Ch = %q, want ' '", s.Cells[2][0].Ch)
	}
}

// TestDeleteLinesClampToAvail verifies deleteLines(1) shifts exactly one row up.
func TestDeleteLinesClampToAvail(t *testing.T) {
	s := New(5, 5)
	s.scrollTop = 0
	s.scrollBottom = 4
	s.curY = 1
	s.Cells[1][0] = Cell{Ch: '1'}
	s.Cells[2][0] = Cell{Ch: '2'}
	s.Cells[3][0] = Cell{Ch: '3'}
	s.Cells[4][0] = Cell{Ch: '4'}
	s.deleteLines(1)
	if s.Cells[1][0].Ch != '2' {
		t.Errorf("deleteLines(1) at curY=1: Cells[1][0].Ch = %q, want '2'", s.Cells[1][0].Ch)
	}
	if s.Cells[3][0].Ch != '4' {
		t.Errorf("deleteLines(1) at curY=1: Cells[3][0].Ch = %q, want '4'", s.Cells[3][0].Ch)
	}
}

// TestLineDownClampsCurY verifies lineDown clamps curY to Height-1 when the
// cursor sits below the scroll region.
func TestLineDownClampsCurY(t *testing.T) {
	s := New(5, 5)
	s.scrollBottom = 2
	s.curY = s.Height - 1 // below the region -> increments to Height
	s.lineDown()
	if s.curY != s.Height-1 {
		t.Errorf("lineDown from curY=Height-1: curY = %d, want %d", s.curY, s.Height-1)
	}
}

// --- REP ---

// TestREPRepeatsLastRune verifies REP (CSI Pn b) reprints the last printed rune
// Pn more times, driven through the real print path.
func TestREPRepeatsLastRune(t *testing.T) {
	s := New(5, 5)
	s.Write([]byte("X"))       // print 'X' at col 0 (records it as the last rune)
	s.Write([]byte("\x1b[3b")) // REP 3 -> three more 'X' at cols 1,2,3
	if got := s.RowString(0); got != "XXXX" {
		t.Errorf("REP after printing 'X': RowString(0) = %q, want %q", got, "XXXX")
	}
}

// --- SL / SR (shift left/right) ---

// TestShiftRightShiftsContent verifies SR by less than the width shifts content
// right rather than clearing the whole region.
func TestShiftRightShiftsContent(t *testing.T) {
	s := New(1, 5)
	s.Write([]byte("X")) // Cells[0][0] = 'X'
	s.shiftRight(1)
	if s.Cells[0][1].Ch != 'X' {
		t.Errorf("shiftRight(1): Cells[0][1].Ch = %q, want 'X' (content shifted right)", s.Cells[0][1].Ch)
	}
}

// TestShiftRightClearsVacatedColumns verifies SR blanks the columns it vacates.
func TestShiftRightClearsVacatedColumns(t *testing.T) {
	s := New(1, 5)
	for x := range 5 {
		s.Cells[0][x] = Cell{Ch: rune('A' + x)}
	}
	s.shiftRight(2) // cols 2,3,4 = A,B,C; cols 0,1 cleared
	if s.Cells[0][0].Ch != ' ' {
		t.Errorf("shiftRight(2): Cells[0][0].Ch = %q, want ' '", s.Cells[0][0].Ch)
	}
	if s.Cells[0][1].Ch != ' ' {
		t.Errorf("shiftRight(2): Cells[0][1].Ch = %q, want ' '", s.Cells[0][1].Ch)
	}
	if s.Cells[0][2].Ch != 'A' {
		t.Errorf("shiftRight(2): Cells[0][2].Ch = %q, want 'A'", s.Cells[0][2].Ch)
	}
}

// TestShiftLeftClearsLastRow verifies a full-width shift-left clears every row
// in the scroll region, including the last.
func TestShiftLeftClearsLastRow(t *testing.T) {
	s := New(3, 4) // scrollBottom=2 (last row)
	s.Cells[2][0] = Cell{Ch: 'Z'}
	s.shiftLeft(s.Width) // full-clear branch
	if s.Cells[2][0].Ch != ' ' {
		t.Errorf("shiftLeft(Width): Cells[scrollBottom][0].Ch = %q, want ' '", s.Cells[2][0].Ch)
	}
}

// TestShiftRightClearsLastRow verifies a full-width shift-right clears the last
// row of the scroll region.
func TestShiftRightClearsLastRow(t *testing.T) {
	s := New(3, 4)
	s.Cells[2][0] = Cell{Ch: 'Z'}
	s.shiftRight(s.Width)
	if s.Cells[2][0].Ch != ' ' {
		t.Errorf("shiftRight(Width): Cells[scrollBottom][0].Ch = %q, want ' '", s.Cells[2][0].Ch)
	}
}

// --- softReset ---

// TestSoftResetScrollBottom verifies softReset restores the scroll-region bottom
// to Height-1.
func TestSoftResetScrollBottom(t *testing.T) {
	s := New(10, 20)
	s.scrollBottom = 3
	s.softReset()
	if s.scrollBottom != s.Height-1 {
		t.Errorf("softReset(): scrollBottom = %d, want %d", s.scrollBottom, s.Height-1)
	}
}

// --- Device status / attributes ---

// TestDeviceAttributesPrimary verifies the primary Device Attributes reply
// advertises the VT525 (level 5) feature profile the engine implements.
func TestDeviceAttributesPrimary(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[c"))
	want := "\x1b[?65;1;2;6;9;15;16;17;18;21;22;28;29c"
	if got := string(s.response); got != want {
		t.Errorf("DA1 = %q, want %q", got, want)
	}
}

// TestDSRCursorPosition verifies DSR 6 (CPR) reports the cursor position.
func TestDSRCursorPosition(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[5;10H"))
	s.Write([]byte("\x1b[6n"))
	if got := string(s.response); got != "\x1b[5;10R" {
		t.Errorf("DSR CPR = %q, want %q", got, "\x1b[5;10R")
	}
}

// TestCSIPrivateMarkerRouting verifies the private marker routes CSI dispatch:
// '?' enables a private mode (hide cursor) and '>' selects the secondary DA.
func TestCSIPrivateMarkerRouting(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[?25l")) // hide cursor
	if !s.CursorHidden {
		t.Error("CSI ?25l should hide cursor")
	}
	s.Write([]byte("\x1b[>c")) // secondary DA
	want := "\x1b[>64;410;0c"  // VT525-class model, firmware level 410
	if got := string(s.response); got != want {
		t.Errorf("secondary DA = %q, want %q", got, want)
	}
}

// --- Unhandled CSI logging ---

// TestUnhandledCSILogs verifies an unhandled CSI final byte emits the
// "unhandled CSI" log line.
func TestUnhandledCSILogs(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	s := New(5, 5)
	s.dispatchCSI('W') // 'W' is unhandled -> default branch
	if !strings.Contains(buf.String(), "unhandled CSI") {
		t.Errorf("dispatchCSI('W') log = %q, want it to contain \"unhandled CSI\"", buf.String())
	}
}

// TestEraseInLine verifies EL (CSI K): mode 0 erases cursor->end, mode 1 erases
// start->cursor, mode 2 erases the whole row, each preserving the other columns.
func TestEraseInLine(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want [10]rune
	}{
		{"mode 0 cursor to end", "\x1b[6G\x1b[K", [10]rune{'A', 'B', 'C', 'D', 'E', ' ', ' ', ' ', ' ', ' '}},
		{"mode 1 start to cursor", "\x1b[6G\x1b[1K", [10]rune{' ', ' ', ' ', ' ', ' ', ' ', 'G', 'H', 'I', 'J'}},
		{"mode 2 whole line", "\x1b[2K", [10]rune{' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(1, 10)
			s.Write([]byte("ABCDEFGHIJ"))
			s.Write([]byte(tc.seq))
			for x, want := range tc.want {
				if got := s.Cells[0][x].Ch; got != want {
					t.Errorf("EL %q: Cells[0][%d].Ch = %q, want %q", tc.seq, x, got, want)
				}
			}
		})
	}
}

// TestEraseChars verifies ECH (CSI X) erases n cells from the cursor rightward,
// clamped to the row end, leaving columns left of the cursor intact.
func TestEraseChars(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want [10]rune
	}{
		{"erase 3 from col 2", "\x1b[3G\x1b[3X", [10]rune{'A', 'B', ' ', ' ', ' ', 'F', 'G', 'H', 'I', 'J'}},
		{"clamped past row end", "\x1b[9G\x1b[100X", [10]rune{'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', ' ', ' '}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(1, 10)
			s.Write([]byte("ABCDEFGHIJ"))
			s.Write([]byte(tc.seq))
			for x, want := range tc.want {
				if got := s.Cells[0][x].Ch; got != want {
					t.Errorf("ECH %q: Cells[0][%d].Ch = %q, want %q", tc.seq, x, got, want)
				}
			}
		})
	}
}

// TestCursorTabForward verifies CHT (CSI I) advances the cursor by n tab stops
// (default stops every 8 columns), clamping at the right margin.
func TestCursorTabForward(t *testing.T) {
	cases := []struct {
		name    string
		seq     string
		wantCol int
	}{
		{"one stop from col 0", "\x1b[1G\x1b[I", 8},
		{"two stops from col 0", "\x1b[1G\x1b[2I", 16},
		{"clamped at right margin", "\x1b[1G\x1b[100I", 79},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(24, 80)
			s.Write([]byte(tc.seq))
			if _, col := s.CursorPos(); col != tc.wantCol {
				t.Errorf("CHT %q: col = %d, want %d", tc.seq, col, tc.wantCol)
			}
		})
	}
}

// TestWindowManipulationReportSize verifies CSI 18 t reports the text-area size
// as CSI 8 ; rows ; cols t, and that an unsupported parameter produces no reply.
func TestWindowManipulationReportSize(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[18t"))
	if got, want := string(s.response), "\x1b[8;24;80t"; got != want {
		t.Errorf("CSI 18 t = %q, want %q", got, want)
	}
	s.response = nil
	s.Write([]byte("\x1b[99t"))
	if len(s.response) != 0 {
		t.Errorf("CSI 99 t (unsupported) wrote %q, want no reply", string(s.response))
	}
}

func TestDECFRA_FillsRectangle(t *testing.T) {
	s := New(4, 10)
	// DECFRA (CSI Pch;Pt;Pl;Pb;Pr $ x): fill 'X' (88) into 1-based rows 1..2,
	// cols 1..3 -> 0-based rows 0..1, cols 0..2.
	s.Write([]byte("\x1b[88;1;1;2;3$x"))
	cases := []struct {
		y, x int
		ch   rune
	}{
		{0, 0, 'X'}, {0, 2, 'X'}, {1, 0, 'X'}, {1, 2, 'X'}, // inside the filled rect
		{0, 3, ' '}, {2, 0, ' '}, // just outside: right of the rect and below it stay blank
	}
	for _, c := range cases {
		if got := s.Cells[c.y][c.x].Ch; got != c.ch {
			t.Errorf("DECFRA Cells[%d][%d].Ch = %q, want %q", c.y, c.x, got, c.ch)
		}
	}
}

// --- Erase selectivity: the '?' private marker is the only difference ---

// TestEraseInDisplaySelectivity verifies the private marker is what makes an
// erase selective. ED (CSI J) spares only ISO-guarded cells, so it clears a
// DECSCA-protected cell; DECSED (CSI ? J) spares DECSCA cells. Swapping the two
// would let a full-screen reset leave guarded content on the display.
func TestEraseInDisplaySelectivity(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want string
	}{
		{"ED_clears_protected", "\x1b[2J", ""},
		{"DECSED_spares_protected", "\x1b[?2J", "AB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(2, 6)
			s.Write([]byte("\x1b[1\"q")) // DECSCA: protect cells printed from here on
			s.Write([]byte("AB"))
			s.Write([]byte(tc.seq))
			if got := s.RowString(0); got != tc.want {
				t.Errorf("RowString(0) after %q over DECSCA-protected cells = %q, want %q", tc.seq, got, tc.want)
			}
		})
	}
}

// --- Cursor motion bounds ---

// TestHPRClampsAtLastColumn verifies HPR (CSI a) — a purely relative move with
// no re-origin — lands on the last column when the requested offset runs past
// the screen. HPR is the one motion whose only clamp is the shared cursor clamp.
func TestHPRClampsAtLastColumn(t *testing.T) {
	s := New(4, 10)
	s.Write([]byte("\x1b[999a"))
	if _, col := s.CursorPos(); col != 9 {
		t.Errorf("col after CSI 999 a on a 10-column screen = %d, want 9", col)
	}
}

// TestCursorUpStopsAtTopMargin verifies CUU from a cursor sitting exactly ON the
// top margin stops there rather than travelling above the scroll region. xterm's
// CursorUp takes the margin as its floor whenever the cursor starts at or below
// it, so the boundary row is inside the region, not outside it.
func TestCursorUpStopsAtTopMargin(t *testing.T) {
	s := New(8, 10)
	s.Write([]byte("\x1b[3;6r")) // scroll region rows 3..6 (0-based 2..5)
	s.Write([]byte("\x1b[3;1H")) // cursor onto the top margin itself
	s.Write([]byte("\x1b[1A"))
	if row, _ := s.CursorPos(); row != 2 {
		t.Errorf("row after CUU from the top margin = %d, want 2 (the margin is the floor)", row)
	}
}

// TestCursorDownStopsAtBottomMargin verifies CUD from a cursor sitting exactly ON
// the bottom margin stops there rather than travelling below the scroll region,
// mirroring xterm's CursorDown ceiling.
func TestCursorDownStopsAtBottomMargin(t *testing.T) {
	s := New(8, 10)
	s.Write([]byte("\x1b[3;6r")) // scroll region rows 3..6 (0-based 2..5)
	s.Write([]byte("\x1b[6;1H")) // cursor onto the bottom margin itself
	s.Write([]byte("\x1b[1B"))
	if row, _ := s.CursorPos(); row != 5 {
		t.Errorf("row after CUD from the bottom margin = %d, want 5 (the margin is the ceiling)", row)
	}
}

// TestOriginModeColumnRelativeToLeftMargin verifies that under DECOM a column
// parameter is measured FROM the left margin: with the margin at column 2,
// CHA 3 lands on absolute column 4, not column 2.
func TestOriginModeColumnRelativeToLeftMargin(t *testing.T) {
	s := New(4, 10)
	s.Write([]byte("\x1b[?69h")) // DECLRMM — enable left/right margins
	s.Write([]byte("\x1b[3;8s")) // DECSLRM — margins at 0-based cols 2..7
	s.Write([]byte("\x1b[?6h"))  // DECOM — origin mode
	s.Write([]byte("\x1b[3G"))   // CHA to the third column of the box
	if _, col := s.CursorPos(); col != 4 {
		t.Errorf("col after CHA 3 under origin mode with the left margin at 2 = %d, want 4", col)
	}
}

// --- DECFRA rectangle resolution ---

// TestDECFRAOmittedBottomRightFillsToScreenEdge verifies an omitted Pb/Pr
// defaults to the screen's last row/column: DEC treats a missing rectangle
// coordinate as the page edge, and the resolved bound must stay inside the grid.
func TestDECFRAOmittedBottomRightFillsToScreenEdge(t *testing.T) {
	s := New(3, 5)
	s.Write([]byte("\x1b[88;1;1$x")) // DECFRA 'X' from row 1, col 1, no Pb/Pr
	for y := range 3 {
		if got, want := s.RowString(y), "XXXXX"; got != want {
			t.Errorf("RowString(%d) after DECFRA with Pb/Pr omitted = %q, want %q", y, got, want)
		}
	}
}

// TestDECFRAHonorsExplicitTopRow verifies an explicit Pt above 1 is used as
// given rather than collapsed to the first row: DECFRA 2;1;2;3 must leave row 1
// untouched.
func TestDECFRAHonorsExplicitTopRow(t *testing.T) {
	s := New(3, 5)
	s.Write([]byte("\x1b[88;2;1;2;3$x"))
	if got, want := s.RowString(0), ""; got != want {
		t.Errorf("RowString(0) after DECFRA with Pt=2 = %q, want %q (the rectangle starts below row 1)", got, want)
	}
	if got, want := s.RowString(1), "XXX"; got != want {
		t.Errorf("RowString(1) after DECFRA with Pt=2 = %q, want %q", got, want)
	}
}

// TestDECFRAOriginModeRowOffset verifies a DECFRA rectangle is origin-mode
// relative on the vertical axis: with the top margin on row 2 (0-based 1), rows
// 3..3 of the region resolve to absolute row 3, so neither the top of the region
// nor anything above it is touched.
func TestDECFRAOriginModeRowOffset(t *testing.T) {
	s := New(8, 6)
	s.Write([]byte("\x1b[2;6r")) // scroll region rows 2..6 (0-based 1..5)
	s.Write([]byte("\x1b[?6h"))  // DECOM
	s.Write([]byte("\x1b[88;3;1;3;3$x"))
	if got, want := s.RowString(3), "XXX"; got != want {
		t.Errorf("RowString(3) after origin-mode DECFRA rows 3..3 = %q, want %q", got, want)
	}
	for _, y := range []int{0, 1, 2, 4, 5, 6, 7} {
		if got := s.RowString(y); got != "" {
			t.Errorf("RowString(%d) after origin-mode DECFRA rows 3..3 = %q, want %q", y, got, "")
		}
	}
}

// TestDECFRAOriginModeColumnOffset verifies the same on the horizontal axis:
// with the left margin at column 2, columns 5..6 of the box resolve to absolute
// columns 6..7.
func TestDECFRAOriginModeColumnOffset(t *testing.T) {
	s := New(4, 10)
	s.Write([]byte("\x1b[?69h")) // DECLRMM
	s.Write([]byte("\x1b[3;8s")) // DECSLRM — margins at 0-based cols 2..7
	s.Write([]byte("\x1b[?6h"))  // DECOM
	s.Write([]byte("\x1b[88;1;5;1;6$x"))
	if got, want := s.RowString(0), "      XX"; got != want {
		t.Errorf("RowString(0) after origin-mode DECFRA cols 5..6 = %q, want %q", got, want)
	}
}

// --- DECSLRM parameter range ---

// TestDECSLRMMarginRange verifies how DECSLRM resolves its two parameters, read
// back through the DECRQSS "s" status string (1-based, so a full-width 20-column
// screen reports 1;20). A valid range is applied as given; a degenerate range
// (left == right) is rejected, matching xterm's requirement that the left margin
// be strictly left of the right one; and a right parameter past the screen is
// clamped to the last column.
func TestDECSLRMMarginRange(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want string
	}{
		{"valid_range_applied", "\x1b[3;8s", "\x1bP1$r3;8s\x1b\\"},
		{"degenerate_range_rejected", "\x1b[5;5s", "\x1bP1$r1;20s\x1b\\"},
		{"right_clamped_to_screen", "\x1b[1;999s", "\x1bP1$r1;20s\x1b\\"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(8, 20)
			s.Write([]byte("\x1b[?69h")) // DECLRMM — DECSLRM is inert without it
			s.Write([]byte(tc.seq))
			s.response = nil
			s.Write([]byte("\x1bP$qs\x1b\\")) // DECRQSS DECSLRM
			if got := string(s.response); got != tc.want {
				t.Errorf("DECRQSS \"s\" after %q = %q, want %q", tc.seq, got, tc.want)
			}
		})
	}
}

// --- DECSLPP / XTWINOPS parameter split ---

// TestDECSLPPTracksLinesFrom24Up verifies XTWINOPS splits at 24: Ps>=24 is
// DECSLPP (set lines per page) and is tracked for the DECRQSS "t" report, while
// a smaller unhandled Ps is not a page-length request and must leave the
// tracked value alone (the report then falls back to the screen height).
func TestDECSLPPTracksLinesFrom24Up(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want string
	}{
		{"lowest_page_length_tracked", "\x1b[24t", "\x1bP1$r24t\x1b\\"},
		{"below_range_not_tracked", "\x1b[1t", "\x1bP1$r8t\x1b\\"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(8, 20)
			s.Write([]byte(tc.seq))
			s.response = nil
			s.Write([]byte("\x1bP$qt\x1b\\")) // DECRQSS DECSLPP
			if got := string(s.response); got != tc.want {
				t.Errorf("DECRQSS \"t\" after %q = %q, want %q", tc.seq, got, tc.want)
			}
		})
	}
}

// --- Conformance-level gates ---

// TestDECLRMMRequiresLevel4 verifies DECLRMM (?69) is honored at DECSCL level 4
// and above and refused below it, matching xterm's gating of a VT level-4
// feature. The report is DECRQSS "s": margins take effect only when DECLRMM did.
func TestDECLRMMRequiresLevel4(t *testing.T) {
	cases := []struct {
		name  string
		level string
		want  string
	}{
		{"default_level_5", "", "\x1bP1$r3;8s\x1b\\"},
		{"level_4_lowest_honored", "\x1b[64\"p", "\x1bP1$r3;8s\x1b\\"},
		{"level_3_refused", "\x1b[63\"p", "\x1bP1$r1;20s\x1b\\"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(8, 20)
			s.Write([]byte(tc.level))
			s.Write([]byte("\x1b[?69h\x1b[3;8s"))
			s.response = nil
			s.Write([]byte("\x1bP$qs\x1b\\"))
			if got := string(s.response); got != tc.want {
				t.Errorf("DECRQSS \"s\" after DECSCL %q + DECLRMM + DECSLRM 3;8 = %q, want %q", tc.level, got, tc.want)
			}
		})
	}
}

// TestDECRQMAnsweredAtLevel3 verifies DECRQM — a VT level-3 feature — is
// answered at DECSCL level 3 and silently ignored at level 2, where a real
// terminal would not recognize the sequence.
func TestDECRQMAnsweredAtLevel3(t *testing.T) {
	cases := []struct {
		name  string
		level string
		want  string
	}{
		{"level_3_lowest_answered", "\x1b[63\"p", "\x1b[?7;1$y"},
		{"level_2_ignored", "\x1b[62\"p", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(8, 20)
			s.Write([]byte(tc.level))
			s.response = nil
			s.Write([]byte("\x1b[?7$p")) // DECRQM for DECAWM
			if got := string(s.response); got != tc.want {
				t.Errorf("DECRQM ?7 at DECSCL %q = %q, want %q", tc.level, got, tc.want)
			}
		})
	}
}

// --- XTSAVE / XTRESTORE ---

// TestXTRestoreReappliesSavedMode verifies XTRESTORE re-applies the state
// XTSAVE recorded: a mode saved while SET comes back set, even after being
// reset in between.
func TestXTRestoreReappliesSavedMode(t *testing.T) {
	s := New(4, 10)
	s.Write([]byte("\x1b[?7h")) // DECAWM on
	s.Write([]byte("\x1b[?7s")) // XTSAVE
	s.Write([]byte("\x1b[?7l")) // DECAWM off
	s.Write([]byte("\x1b[?7r")) // XTRESTORE
	if !s.AutoWrap {
		t.Errorf("AutoWrap after XTSAVE(set) / reset / XTRESTORE = %v, want true", s.AutoWrap)
	}
}

// --- DECCOLM and DECSTR margin reset ---

// TestDECCOLMResetsRegionAndMarginsToFullScreen verifies a column-mode change
// restores the scroll region and the left/right margins to the whole screen, and
// that the restored values are the screen's own bounds: re-enabling DECLRMM
// afterwards without a fresh DECSLRM must describe a full-width box.
func TestDECCOLMResetsRegionAndMarginsToFullScreen(t *testing.T) {
	s := New(8, 20)
	s.Write([]byte("\x1b[?69h\x1b[3;8s")) // a narrow margin box
	s.Write([]byte("\x1b[2;6r"))          // and a partial scroll region
	s.Write([]byte("\x1b[?3h"))           // DECCOLM
	s.response = nil
	s.Write([]byte("\x1bP$qr\x1b\\")) // DECRQSS DECSTBM
	if got, want := string(s.response), "\x1bP1$r1;8r\x1b\\"; got != want {
		t.Errorf("DECRQSS \"r\" after DECCOLM = %q, want %q (region back to the full screen)", got, want)
	}
	s.Write([]byte("\x1b[?69h")) // DECLRMM again, no DECSLRM
	s.response = nil
	s.Write([]byte("\x1bP$qs\x1b\\"))
	if got, want := string(s.response), "\x1bP1$r1;20s\x1b\\"; got != want {
		t.Errorf("DECRQSS \"s\" after DECCOLM then DECLRMM = %q, want %q (margins back to the full width)", got, want)
	}
}

// TestSoftResetRestoresFullWidthMargins verifies DECSTR returns the left/right
// margins to the screen's own bounds, so re-enabling DECLRMM after a soft reset
// describes a full-width box rather than one running past the last column.
func TestSoftResetRestoresFullWidthMargins(t *testing.T) {
	s := New(8, 20)
	s.Write([]byte("\x1b[?69h\x1b[3;8s")) // a narrow margin box
	s.Write([]byte("\x1b[!p"))            // DECSTR
	s.Write([]byte("\x1b[?69h"))          // DECLRMM again, no DECSLRM
	s.response = nil
	s.Write([]byte("\x1bP$qs\x1b\\"))
	if got, want := string(s.response), "\x1bP1$r1;20s\x1b\\"; got != want {
		t.Errorf("DECRQSS \"s\" after DECSTR then DECLRMM = %q, want %q", got, want)
	}
}

// --- DCH cell shifting ---

// TestDeleteCharsShiftsAndBlanksTail verifies DCH pulls every cell up to the
// right margin leftward by n and blanks exactly the n vacated cells at the
// margin, so the last column ends up blank rather than a duplicate.
func TestDeleteCharsShiftsAndBlanksTail(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want string
	}{
		{"delete_one", "\x1b[1P", "BCDEFGHIJ"},
		{"delete_two", "\x1b[2P", "CDEFGHIJ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(1, 10)
			s.Write([]byte("ABCDEFGHIJ"))
			s.Write([]byte("\x1b[1;1H"))
			s.Write([]byte(tc.seq))
			if got := s.RowString(0); got != tc.want {
				t.Errorf("RowString(0) after %q at col 0 = %q, want %q", tc.seq, got, tc.want)
			}
		})
	}
}

// --- DECIC / DECDC ---

// TestDECICInsertsAndClampsCount verifies DECIC shifts the row right by n and
// clamps n to the columns between the cursor and the right margin: a count at or
// past that span blanks the rest of the row and nothing outside it.
func TestDECICInsertsAndClampsCount(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want string
	}{
		{"insert_two_at_left", "\x1b[1;1H\x1b[2'}", "  ABCDEFGH"},
		{"count_equal_to_span", "\x1b[1;1H\x1b[10'}", ""},
		{"count_past_span_mid_row", "\x1b[1;4H\x1b[65535'}", "ABC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(1, 10)
			s.Write([]byte("ABCDEFGHIJ"))
			s.Write([]byte(tc.seq))
			if got := s.RowString(0); got != tc.want {
				t.Errorf("RowString(0) after DECIC %q = %q, want %q", tc.seq, got, tc.want)
			}
		})
	}
}

// TestDECDCDeletesAndClampsCount verifies DECDC shifts the row left by n,
// blanks the n vacated cells at the right margin, and clamps n to the columns
// between the cursor and that margin.
func TestDECDCDeletesAndClampsCount(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want string
	}{
		{"delete_two_at_left", "\x1b[1;1H\x1b[2'~", "CDEFGHIJ"},
		{"count_equal_to_span", "\x1b[1;1H\x1b[10'~", ""},
		{"count_past_span_mid_row", "\x1b[1;4H\x1b[65535'~", "ABC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(1, 10)
			s.Write([]byte("ABCDEFGHIJ"))
			s.Write([]byte(tc.seq))
			if got := s.RowString(0); got != tc.want {
				t.Errorf("RowString(0) after DECDC %q = %q, want %q", tc.seq, got, tc.want)
			}
		})
	}
}

// TestDECICAppliesToEveryRegionRow verifies DECIC is a column operation over the
// whole vertical scroll region, not just the cursor's row: the region's last row
// shifts whether the cursor sits on it or on the first row.
func TestDECICAppliesToEveryRegionRow(t *testing.T) {
	cases := []struct {
		name   string
		cursor string
	}{
		{"cursor_on_first_region_row", "\x1b[1;1H"},
		{"cursor_on_last_region_row", "\x1b[3;1H"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(3, 10)
			s.Write([]byte("\x1b[3;1HABCDEFGHIJ")) // content on the region's last row
			s.Write([]byte(tc.cursor))
			s.Write([]byte("\x1b[2'}"))
			if got, want := s.RowString(2), "  ABCDEFGH"; got != want {
				t.Errorf("RowString(2) after DECIC 2 with the cursor at %q = %q, want %q", tc.cursor, got, want)
			}
		})
	}
}

// TestDECDCAppliesToEveryRegionRow is TestDECICAppliesToEveryRegionRow's mirror
// for DECDC.
func TestDECDCAppliesToEveryRegionRow(t *testing.T) {
	cases := []struct {
		name   string
		cursor string
	}{
		{"cursor_on_first_region_row", "\x1b[1;1H"},
		{"cursor_on_last_region_row", "\x1b[3;1H"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(3, 10)
			s.Write([]byte("\x1b[3;1HABCDEFGHIJ"))
			s.Write([]byte(tc.cursor))
			s.Write([]byte("\x1b[2'~"))
			if got, want := s.RowString(2), "CDEFGHIJ"; got != want {
				t.Errorf("RowString(2) after DECDC 2 with the cursor at %q = %q, want %q", tc.cursor, got, want)
			}
		})
	}
}

// --- Row identity through the full-width line shifts ---

// TestInsertLinesCarriesSoftWrapFlag verifies a full-width IL moves whole rows,
// so a row's soft-wrap continuation flag travels with its content. The flag is
// observable through the wire encoding: a row that wrapped onto the row below
// keeps its trailing blanks, because there they are mid-line content rather than
// padding. A shift that moved cell contents but left the flags behind would ship
// the wrong row as the continuation.
func TestInsertLinesCarriesSoftWrapFlag(t *testing.T) {
	s := New(4, 10)
	s.Write([]byte("foo       bar")) // fills row 0 exactly, wraps "bar" onto row 1
	if got, want := wireRowText(s.RenderRowWire(0)), "foo       "; got != want {
		t.Fatalf("fixture: RenderRowWire(0) = %q, want %q (row 0 must soft-wrap onto row 1)", got, want)
	}
	s.Write([]byte("\x1b[1;1H\x1b[1L")) // IL 1 at the top: the pair moves down one row
	if got, want := s.RowString(1), "foo"; got != want {
		t.Fatalf("RowString(1) after IL 1 = %q, want %q", got, want)
	}
	if got, want := wireRowText(s.RenderRowWire(1)), "foo       "; got != want {
		t.Errorf("RenderRowWire(1) after IL 1 = %q, want %q (the wrap flag must move with the row)", got, want)
	}
}

// TestScrollDownCarriesSoftWrapFlag verifies the same for a full-width SD.
func TestScrollDownCarriesSoftWrapFlag(t *testing.T) {
	s := New(4, 10)
	s.Write([]byte("foo       bar"))
	if got, want := wireRowText(s.RenderRowWire(0)), "foo       "; got != want {
		t.Fatalf("fixture: RenderRowWire(0) = %q, want %q (row 0 must soft-wrap onto row 1)", got, want)
	}
	s.Write([]byte("\x1b[1T")) // SD 1
	if got, want := s.RowString(1), "foo"; got != want {
		t.Fatalf("RowString(1) after SD 1 = %q, want %q", got, want)
	}
	if got, want := wireRowText(s.RenderRowWire(1)), "foo       "; got != want {
		t.Errorf("RenderRowWire(1) after SD 1 = %q, want %q (the wrap flag must move with the row)", got, want)
	}
}
