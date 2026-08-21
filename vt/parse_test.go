package vt

import (
	"strings"
	"testing"
)

// --- C1 control bytes (0x80-0x9F) ---

// TestC1BytesInGroundEmitReplacement verifies every C1 byte emits U+FFFD in
// Ground (UTF-8 mode) and does not change parser state.
func TestC1BytesInGroundEmitReplacement(t *testing.T) {
	for b := byte(0x80); b <= 0x9F; b++ {
		s := New(1, 5)
		s.Write([]byte{b, 'X'}) // C1 byte, then a printable
		if s.Cells[0][0].Ch != 0xFFFD {
			t.Errorf("byte 0x%02x in Ground: Cells[0][0].Ch = %U, want U+FFFD", b, s.Cells[0][0].Ch)
		}
		// The parser did not start a sequence and stays in Ground: the following
		// 'X' prints in the next cell rather than being consumed as a sequence.
		if s.Cells[0][1].Ch != 'X' {
			t.Errorf("byte 0x%02x in Ground: Cells[0][1].Ch = %U, want 'X' (parser stayed in Ground)", b, s.Cells[0][1].Ch)
		}
	}
}

// TestInvalidUTF8LeadBytesEmitReplacement verifies the invalid UTF-8 lead
// bytes 0xF8-0xFF (no valid UTF-8 sequence begins with these) each emit one
// U+FFFD in Ground and leave the parser in Ground - the same replacement
// error model the C1 and overlong paths use.
func TestInvalidUTF8LeadBytesEmitReplacement(t *testing.T) {
	for b := 0xF8; b <= 0xFF; b++ {
		s := New(1, 5)
		s.Write([]byte{byte(b), 'X'})
		if got := s.Cells[0][0].Ch; got != 0xFFFD {
			t.Errorf("lead byte 0x%02x in Ground: Cells[0][0].Ch = %U, want U+FFFD", b, got)
		}
		// Parser stays in Ground (no multi-byte sequence begins with these):
		// the following 'X' prints in the next cell.
		if s.Cells[0][1].Ch != 'X' {
			t.Errorf("lead byte 0x%02x: Cells[0][1].Ch = %U, want 'X' (parser stayed in Ground)", b, s.Cells[0][1].Ch)
		}
	}
}

// TestC1_0x9B_InGroundDoesNotStartCSI verifies 0x9B in Ground emits U+FFFD and
// the following bytes are printed literally (it does not begin a CSI).
func TestC1_0x9B_InGroundDoesNotStartCSI(t *testing.T) {
	s := New(1, 10)
	s.Write([]byte{0x9B, '3', '1', 'm'})
	if s.Cells[0][0].Ch != 0xFFFD {
		t.Errorf("0x9B in Ground: got %U, want U+FFFD", s.Cells[0][0].Ch)
	}
	if s.Cells[0][1].Ch != '3' {
		t.Errorf("after 0x9B: cell[1] got %U, want '3'", s.Cells[0][1].Ch)
	}
}

// TestC1_0x90_InGroundDoesNotStartDCS verifies 0x90 (8-bit DCS introducer) in
// Ground emits U+FFFD and does NOT begin a device-control string: the bytes
// that follow print literally instead of being swallowed as DCS data.
func TestC1_0x90_InGroundDoesNotStartDCS(t *testing.T) {
	s := New(1, 10)
	s.Write([]byte{0x90})  // 8-bit DCS introducer — suppressed in Ground
	s.Write([]byte("$qm")) // would be consumed as DCS data if 0x90 had started one
	if s.Cells[0][0].Ch != 0xFFFD {
		t.Errorf("0x90 in Ground: Cells[0][0].Ch = %U, want U+FFFD", s.Cells[0][0].Ch)
	}
	if s.Cells[0][1].Ch != '$' {
		t.Errorf("0x90 in Ground did not start a DCS: Cells[0][1].Ch = %U, want '$' (printed literally)", s.Cells[0][1].Ch)
	}
}

// TestC1_0x9B_InEscapeStartsCSI verifies 0x9B (8-bit CSI) initiates a CSI when
// the parser is already in a non-Ground (Escape) state: a CUP sequence
// introduced by 0x9B moves the cursor.
func TestC1_0x9B_InEscapeStartsCSI(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte{0x1B})   // ESC (into a non-Ground state)
	s.Write([]byte{0x9B})   // 8-bit CSI introducer
	s.Write([]byte("3;5H")) // CUP row 3, col 5
	if row, col := s.CursorPos(); row != 2 || col != 4 {
		t.Errorf("ESC then 0x9B then 3;5H: cursor = %d,%d, want 2,4 (0x9B started a CSI)", row, col)
	}
}

// TestC1_0x90_InEscapeStartsDCS verifies 0x90 (8-bit DCS) initiates a DCS from
// the Escape state: the DCS body is swallowed (not printed) and a printable
// after the ST prints normally, proving the bytes were consumed as DCS data.
func TestC1_0x90_InEscapeStartsDCS(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte{0x1B})             // ESC (into a non-Ground state)
	s.Write([]byte{0x90})             // 8-bit DCS introducer
	s.Write([]byte("Xignored\x1b\\")) // DCS body + ST — consumed, never printed
	s.Write([]byte("Z"))              // prints only after the DCS returns to Ground
	if got := s.RowString(0); got != "Z" {
		t.Errorf("ESC then 0x90 then DCS body: RowString(0) = %q, want %q (0x90 started a DCS that swallowed its body)", got, "Z")
	}
}

// TestC1_0x9D_InEscapeStartsOSC verifies 0x9D (8-bit OSC) initiates an OSC from
// the Escape state: an OSC 2 sequence introduced by 0x9D sets the window title.
func TestC1_0x9D_InEscapeStartsOSC(t *testing.T) {
	s := New(5, 80)
	s.Write([]byte{0x1B})          // ESC (into a non-Ground state)
	s.Write([]byte{0x9D})          // 8-bit OSC introducer
	s.Write([]byte("2;hello\x07")) // OSC 2 (set window title) + BEL terminator
	if s.Title != "hello" {
		t.Errorf("ESC then 0x9D then OSC 2: Title = %q, want %q (0x9D started an OSC)", s.Title, "hello")
	}
}

// TestC1_0x9C_InCsiEntryGoesToGround verifies 0x9C (8-bit ST) aborts an
// in-progress CSI back to Ground: a printable after the abort lands at column 0
// (a 'Z' still inside the CSI would be dispatched as CBT, not printed).
func TestC1_0x9C_InCsiEntryGoesToGround(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[")) // begin a CSI
	s.Write([]byte{0x9C})    // 8-bit ST aborts it
	s.Write([]byte("Z"))     // prints at column 0 only if the parser returned to Ground
	if got := s.RowString(0); got != "Z" {
		t.Errorf("CSI then 0x9C then 'Z': RowString(0) = %q, want %q (0x9C aborted the CSI to Ground)", got, "Z")
	}
}

// TestC1_UTF8MultibyteNotCorrupted verifies a valid multi-byte UTF-8 sequence
// decodes correctly (C1 handling does not corrupt it).
func TestC1_UTF8MultibyteNotCorrupted(t *testing.T) {
	s := New(1, 10)
	s.Write([]byte{0xE6, 0xBC, 0xA2}) // 漢
	if s.Cells[0][0].Ch != '漢' {
		t.Errorf("UTF-8 漢: got %U", s.Cells[0][0].Ch)
	}
}

// --- C0 controls execute mid-sequence ---

// TestC0_BEL_ExecutesMidCSI verifies a C0 control (BEL) executes even while a
// CSI sequence is being parsed.
func TestC0_BEL_ExecutesMidCSI(t *testing.T) {
	s := New(5, 80)
	s.Write([]byte("\x1b[1\x07;1H")) // BEL mid-CSI
	if !s.bellRing {
		t.Error("BEL not executed mid-CSI")
	}
}

// TestC0_LF_ExecutesMidEscape verifies LF executes mid-escape (cursor moves
// down) before the escape completes.
func TestC0_LF_ExecutesMidEscape(t *testing.T) {
	s := New(5, 80)
	s.Write([]byte("\x1b\nH")) // ESC, LF, then ESC H
	if row, _ := s.CursorPos(); row < 1 {
		t.Errorf("LF not executed mid-escape: row=%d, want >=1", row)
	}
}

// --- CAN / SUB abort ---

// TestCAN_AbortsCSICleanly verifies CAN aborts a partial CSI (the pending SGR
// never applies) and the parser recovers so the next sequence parses normally.
func TestCAN_AbortsCSICleanly(t *testing.T) {
	s := New(5, 80)
	s.Write([]byte("\x1b[31")) // partial CSI — would be SGR 31 (red fg) if completed
	s.Write([]byte{0x18})      // CAN aborts the sequence
	s.Write([]byte("m"))       // 'm' now prints literally instead of finishing an SGR
	if s.Cells[0][0].Ch != 'm' {
		t.Fatalf("after CAN, Cells[0][0].Ch = %q, want 'm' (CAN must abort the CSI so 'm' prints)", s.Cells[0][0].Ch)
	}
	if s.Cells[0][0].Style != (Style{}) {
		t.Errorf("after CAN, Cells[0][0].Style = %+v, want zero (the aborted SGR 31 must not apply)", s.Cells[0][0].Style)
	}
	// The parser is back in Ground: a fresh CSI takes effect.
	s.Write([]byte("\x1b[2;3H"))
	if row, col := s.CursorPos(); row != 1 || col != 2 {
		t.Errorf("post-CAN CUP = %d,%d, want 1,2", row, col)
	}
}

// TestSUB_AbortsCSI verifies SUB aborts a private-mode CSI (the alt-screen mode
// is not entered) and returns to Ground.
func TestSUB_AbortsCSI(t *testing.T) {
	s := New(5, 80)
	s.Write([]byte("\x1b[?1049")) // partial private-mode CSI (would enter alt screen)
	s.Write([]byte{0x1A})         // SUB aborts before the final byte
	s.Write([]byte("h"))          // 'h' prints literally instead of completing ?1049h
	if s.InAltScreen {
		t.Fatal("SUB must abort the CSI; ?1049 must not complete and enter the alt screen")
	}
	if s.Cells[0][0].Ch != 'h' {
		t.Errorf("after SUB, Cells[0][0].Ch = %q, want 'h' (SUB aborts, so 'h' prints in Ground)", s.Cells[0][0].Ch)
	}
}

// --- Split / malformed sequences across writes ---

// TestParserSplitCSIAcrossWrites verifies a CSI split one byte per Write call
// still parses correctly (parser state persists across writes).
func TestParserSplitCSIAcrossWrites(t *testing.T) {
	s := New(5, 80)
	for _, b := range []byte("\x1b[3;5H") {
		s.Write([]byte{b})
	}
	if row, col := s.CursorPos(); row != 2 || col != 4 {
		t.Fatalf("split CSI H = %d,%d, want 2,4", row, col)
	}
}

// TestParserSplitUTF8AcrossWrites verifies a multi-byte UTF-8 rune split across
// Write calls is reassembled correctly.
func TestParserSplitUTF8AcrossWrites(t *testing.T) {
	s := New(5, 80)
	s.Write([]byte{0xE6})
	s.Write([]byte{0xBC})
	s.Write([]byte{0xA2})
	if s.Cells[0][0].Ch != '漢' {
		t.Fatalf("split UTF-8: got %q, want '漢'", s.Cells[0][0].Ch)
	}
}

// TestParserInvalidUTF8Continuation verifies an invalid continuation byte
// aborts the truncated UTF-8 sequence by emitting one U+FFFD for the ill-formed
// lead (the same error model every other malformed-UTF-8 path uses), then
// re-processes the interrupting byte in Ground — without leaving the cursor out
// of bounds.
func TestParserInvalidUTF8Continuation(t *testing.T) {
	s := New(5, 80)
	s.Write([]byte{0xE6, 'A'}) // 0xE6 starts a 3-byte rune, 'A' is not a continuation
	row, col := s.CursorPos()
	if col < 0 || col >= s.Width {
		t.Fatalf("col %d out of bounds", col)
	}
	if row < 0 || row >= s.Height {
		t.Fatalf("row %d out of bounds", row)
	}
	// The truncated lead renders as U+FFFD and the interrupting byte is then
	// printed in Ground, so no byte is silently dropped.
	if s.Cells[0][0].Ch != 0xFFFD {
		t.Errorf("Cells[0][0].Ch = %U, want U+FFFD for truncated lead", s.Cells[0][0].Ch)
	}
	if s.Cells[0][1].Ch != 'A' {
		t.Errorf("Cells[0][1].Ch = %U, want 'A' (re-processed interrupting byte)", s.Cells[0][1].Ch)
	}
}

// TestParserMalformedCSIMissingFinal verifies a CSI interrupted by a new ESC is
// abandoned and the following sequence parses correctly.
func TestParserMalformedCSIMissingFinal(t *testing.T) {
	s := New(5, 80)
	s.Write([]byte("\x1b[123\x1b[1;1H"))
	if row, col := s.CursorPos(); row != 0 || col != 0 {
		t.Fatalf("malformed CSI recovery = %d,%d, want 0,0", row, col)
	}
}

// TestRapidESCTransitions verifies many back-to-back ESC bytes leave the parser
// recoverable: a full sequence written after the storm still takes effect.
func TestRapidESCTransitions(t *testing.T) {
	s := New(5, 80)
	s.Write([]byte(strings.Repeat("\x1b\x1b\x1b", 100))) // a storm of ESC bytes
	s.Write([]byte("\x1b[2;3HZ"))                        // CUP row 2, col 3, then print 'Z'
	if s.Cells[1][2].Ch != 'Z' {
		t.Fatalf("after ESC storm, Cells[1][2].Ch = %U, want 'Z' (parser must recover)", s.Cells[1][2].Ch)
	}
	if row, col := s.CursorPos(); row != 1 || col != 3 {
		t.Errorf("cursor after recovery = %d,%d, want 1,3", row, col)
	}
}

// TestInterleavedESCAndCSI verifies a fresh ESC[ abandons a prior incomplete
// CSI and the second sequence takes effect.
func TestInterleavedESCAndCSI(t *testing.T) {
	s := New(5, 80)
	s.Write([]byte("\x1b[\x1b[1;1H"))
	if row, col := s.CursorPos(); row != 0 || col != 0 {
		t.Fatalf("got %d,%d, want 0,0", row, col)
	}
}

// --- ESC dispatch: RI, RIS, backspace ---

// TestReverseIndexScrollsAndMoves verifies RI (ESC M) moves the cursor up when
// it is below the scroll top, and scrolls content down when it is at the top.
func TestReverseIndexScrollsAndMoves(t *testing.T) {
	// curY != scrollTop -> cursor moves up.
	s := New(3, 10)
	s.curY = 2
	s.Write([]byte("\x1bM"))
	if row, _ := s.CursorPos(); row != 1 {
		t.Errorf("RI with curY=2: row=%d, want 1", row)
	}
	// curY == scrollTop -> content scrolls down, cursor stays.
	s2 := New(3, 10)
	s2.Write([]byte("TOP"))
	s2.Write([]byte("\x1bM"))
	if got := s2.RowString(1); got != "TOP" {
		t.Errorf("RI at scrollTop: RowString(1) = %q, want TOP", got)
	}
	if row, _ := s2.CursorPos(); row != 0 {
		t.Errorf("RI at scrollTop: row=%d, want 0", row)
	}
}

// TestReverseIndexCursorClamp verifies RI clamps the cursor at the top: from
// row 0 above the region it stays put, inside the region it decrements by one.
func TestReverseIndexCursorClamp(t *testing.T) {
	cases := []struct {
		name      string
		scrollTop int
		startY    int
		wantY     int
	}{
		{"above region at row zero", 1, 0, 0},
		{"inside region", 1, 3, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(10, 10)
			s.scrollTop = tc.scrollTop
			s.curY = tc.startY
			s.dispatchEsc('M') // RI
			if s.curY != tc.wantY {
				t.Errorf("RI(scrollTop=%d, startY=%d) curY = %d, want %d", tc.scrollTop, tc.startY, s.curY, tc.wantY)
			}
		})
	}
}

// TestRISErasesScreen verifies RIS (ESC c) clears the entire screen.
func TestRISErasesScreen(t *testing.T) {
	s := New(4, 5)
	for y := range s.Cells {
		for x := range s.Cells[y] {
			s.Cells[y][x] = Cell{Ch: 'X'}
		}
	}
	s.dispatchEsc('c') // RIS
	for y := range s.Cells {
		for x := range s.Cells[y] {
			if s.Cells[y][x].Ch != ' ' {
				t.Errorf("after RIS Cells[%d][%d].Ch = %q, want ' '", y, x, s.Cells[y][x].Ch)
			}
		}
	}
}

// TestRISResetsStyleAndCursor verifies RIS resets the SGR style and unhides the
// cursor.
func TestRISResetsStyleAndCursor(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("\x1b[1;31m")) // bold + red
	s.Write([]byte("\x1b[?25l"))  // hide cursor
	s.Write([]byte("\x1bc"))      // RIS
	if s.CursorHidden {
		t.Error("RIS did not unhide cursor")
	}
	if s.style != (Style{}) {
		t.Errorf("RIS did not reset style: %+v", s.style)
	}
}

// TestBackspaceDecrementsCurX verifies BS (0x08) moves the cursor one column
// left.
func TestBackspaceDecrementsCurX(t *testing.T) {
	s := New(1, 20)
	s.Write([]byte("hello")) // cursor at col 5
	s.Write([]byte{0x08})    // BS
	if _, col := s.CursorPos(); col != 4 {
		t.Errorf("backspace from column 5: col=%d, want 4", col)
	}
}

// TestBackspaceAtColumnZeroStaysPut verifies BS (0x08) at the left margin is a
// no-op: the cursor must not move past column 0 into a negative column.
func TestBackspaceAtColumnZeroStaysPut(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte{0x08}) // BS while already at column 0
	if _, col := s.CursorPos(); col != 0 {
		t.Errorf("backspace at column 0: col=%d, want 0 (must not decrement below 0)", col)
	}
}

// TestUTF8ValidationEmitsReplacement verifies decodeUTF8Bytes rejects malformed
// multi-byte sequences by emitting U+FFFD into the cell: overlong encodings,
// surrogate code points, values above U+10FFFF, and U+FFFF itself (which collides
// with the wire wide-continuation sentinel in cellsToRuns, so a real U+FFFF must
// never reach a cell).
func TestUTF8ValidationEmitsReplacement(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"overlong 2-byte NUL", []byte{0xC0, 0x80}},
		{"overlong 3-byte solidus", []byte{0xE0, 0x80, 0xAF}},
		{"overlong 4-byte NUL", []byte{0xF0, 0x80, 0x80, 0x80}},
		{"surrogate U+D800", []byte{0xED, 0xA0, 0x80}},
		{"surrogate U+DFFF", []byte{0xED, 0xBF, 0xBF}},
		{"U+FFFF wire-sentinel collision", []byte{0xEF, 0xBF, 0xBF}},
		{"above U+10FFFF", []byte{0xF7, 0xBF, 0xBF, 0xBF}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(1, 5)
			s.Write(tc.in)
			if got := s.Cells[0][0].Ch; got != 0xFFFD {
				t.Errorf("%s: Cells[0][0].Ch = %U, want U+FFFD", tc.name, got)
			}
		})
	}
}

// TestUTF8AcceptsShortestFormBoundaries is the other side of the overlong
// rejection above: the SMALLEST code point each sequence length may legally
// encode is well-formed, as is the highest code point Unicode defines. Rejecting
// these would replace legitimate text (U+0800 opens the CJK-punctuation range,
// U+10000 the supplementary planes) with U+FFFD.
//
// U+0080 is well-formed too, but it is a C1 CONTROL, so it is consumed rather
// than printed: it takes no column and leaves the cell blank. That is the
// distinction the case pins — a control, not ill-formed input.
func TestUTF8AcceptsShortestFormBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		in       []byte
		wantCell rune
		wantCol  int
	}{
		{name: "shortest 2-byte form U+0080", in: []byte{0xC2, 0x80}, wantCell: ' ', wantCol: 0},
		{name: "shortest 3-byte form U+0800", in: []byte{0xE0, 0xA0, 0x80}, wantCell: 0x0800, wantCol: 1},
		{name: "shortest 4-byte form U+10000", in: []byte{0xF0, 0x90, 0x80, 0x80}, wantCell: 0x10000, wantCol: 1},
		{name: "highest code point U+10FFFF", in: []byte{0xF4, 0x8F, 0xBF, 0xBF}, wantCell: 0x10FFFF, wantCol: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(1, 5)
			if _, err := s.Write(tc.in); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := s.Cells[0][0].Ch; got != tc.wantCell {
				t.Errorf("Write(% x): Cells[0][0].Ch = %U, want %U", tc.in, got, tc.wantCell)
			}
			if _, got := s.CursorPos(); got != tc.wantCol {
				t.Errorf("Write(% x): CursorPos() column = %d, want %d", tc.in, got, tc.wantCol)
			}
		})
	}
}

// TestCarriageReturnHonorsLeftMargin: inside a DECSLRM box, CR returns to the
// LEFT MARGIN rather than to column 0 — including when the cursor already sits
// exactly on the margin, which must not send it to the screen edge. A cursor
// left of the margin is outside the box, so its CR goes to column 0 (xterm's
// left/right-margin behavior).
func TestCarriageReturnHonorsLeftMargin(t *testing.T) {
	const leftMarginCol = 5 // DECSLRM 6;15 -> 0-indexed left margin 5
	cases := []struct {
		name string
		cup  string
		want int
	}{
		{name: "right_of_the_margin", cup: "\x1b[1;11H", want: leftMarginCol},
		{name: "on_the_margin", cup: "\x1b[1;6H", want: leftMarginCol},
		{name: "left_of_the_margin", cup: "\x1b[1;3H", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(3, 20)
			if _, err := s.Write([]byte("\x1b[?69h\x1b[6;15s" + tc.cup + "\r")); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, got := s.CursorPos(); got != tc.want {
				t.Errorf("CursorPos() column after CUP %q + CR = %d, want %d", tc.cup, got, tc.want)
			}
		})
	}
}

// TestDECALNResetsRegionAndMargins: the alignment test (ESC # 8) restores the
// scroll region and the left/right margins to the screen's own bounds, so a
// DECSLRM box set before it is gone afterwards. The margins are read back
// through DECRQSS with DECLRMM re-enabled, because that report only reflects the
// stored margins while the mode is on.
func TestDECALNResetsRegionAndMargins(t *testing.T) {
	s := New(6, 20)
	// A scroll region and a margin box, then DECALN.
	if _, err := s.Write([]byte("\x1b[?69h\x1b[2;5r\x1b[3;10s\x1b#8")); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.TakeResponse()
	if _, err := s.Write([]byte("\x1bP$qr\x1b\\")); err != nil {
		t.Fatalf("write scroll-region query: %v", err)
	}
	if got, want := string(s.TakeResponse()), "\x1bP1$r1;6r\x1b\\"; got != want {
		t.Errorf("DECRQSS DECSTBM after DECALN = %q, want %q (full screen)", got, want)
	}
	// DECALN also turns DECLRMM off; re-enable it and the margins report as the
	// screen's own edges.
	if _, err := s.Write([]byte("\x1b[?69h\x1bP$qs\x1b\\")); err != nil {
		t.Fatalf("write margin query: %v", err)
	}
	if got, want := string(s.TakeResponse()), "\x1bP1$r1;20s\x1b\\"; got != want {
		t.Errorf("DECRQSS DECSLRM after DECALN = %q, want %q (full width)", got, want)
	}
}
