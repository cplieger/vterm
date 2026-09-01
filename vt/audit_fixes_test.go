package vt

import "testing"

// Tests for the VT audit fix batch (P0 marker-dispatch, cursor correctness,
// P1, and the P2 completeness set). Grouped by concern.

// --- P0: marker-aware CSI dispatch (m / u / s / r) ---

// XTMODKEYS (CSI > Pp ; Pv m) must NOT be routed to SGR. kiro-cli emits
// ESC[>4;1m at init; before the fix that set bold+underline on the live style.
func TestXTMODKEYS_NotTreatedAsSGR(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[>4;1m"))
	if s.style != (Style{}) {
		t.Errorf("CSI >4;1m mutated style = %+v, want zero (XTMODKEYS is not SGR)", s.style)
	}
	if len(s.response) != 0 {
		t.Errorf("CSI >4;1m produced a response %q, want none", s.response)
	}
	// A plain SGR must still work.
	s.Write([]byte("\x1b[1m"))
	if !s.style.Bold {
		t.Errorf("plain CSI 1m did not set bold")
	}
}

// XTQMODKEYS (CSI ? Pp m) is likewise not SGR.
func TestXTQMODKEYS_NotTreatedAsSGR(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[?4m"))
	if s.style != (Style{}) {
		t.Errorf("CSI ?4m mutated style = %+v, want zero", s.style)
	}
}

// The kitty keyboard query (CSI ? u) must not move the cursor (the marker form
// must not be routed to restoreCursor) and now answers with the current flags
// (CSI ? flags u) — the terminal implements the protocol and syncs the flags to
// the client, so advertising support is correct.
func TestKittyKeyboardQuery_ReportsFlags(t *testing.T) {
	s := New(10, 20)
	s.Write([]byte("\x1b[5;7H")) // row 4, col 6 (0-based)
	s.Write([]byte("\x1b[?u"))   // kitty flags query
	if row, col := s.CursorPos(); row != 4 || col != 6 {
		t.Errorf("CSI ?u moved cursor to %d,%d, want 4,6", row, col)
	}
	if got, want := string(s.response), "\x1b[?0u"; got != want {
		t.Errorf("CSI ?u = %q, want %q (flags 0 initially)", got, want)
	}
}

// CSI ? u push/pop/set change flag state but must not move the cursor and must
// not emit a reply (only the query replies).
func TestKittyKeyboardVariants_NoCursorMoveNoReply(t *testing.T) {
	for _, seq := range []string{"\x1b[>1u", "\x1b[<1u", "\x1b[=1;1u"} {
		s := New(10, 20)
		s.Write([]byte("\x1b[3;3H"))
		s.Write([]byte(seq))
		if row, col := s.CursorPos(); row != 2 || col != 2 {
			t.Errorf("%q moved cursor to %d,%d, want 2,2", seq, row, col)
		}
		if len(s.response) != 0 {
			t.Errorf("%q produced a response %q, want none (only the query replies)", seq, s.response)
		}
	}
}

// CSI ? Pm s (XTSAVE) / CSI ? Pm r (XTRESTORE) must not be handled as
// SCOSC / DECSTBM.
func TestXTSAVERESTORE_NotCursorOrRegion(t *testing.T) {
	s := New(10, 20)
	s.Write([]byte("\x1b[5;5H")) // cursor row 4 col 4
	s.Write([]byte("\x1b[?1049s"))
	if row, col := s.CursorPos(); row != 4 || col != 4 {
		t.Errorf("CSI ?1049s moved cursor to %d,%d, want 4,4 (must not home)", row, col)
	}
	if s.scrollTop != 0 || s.scrollBottom != s.Height-1 {
		t.Errorf("CSI ?...r altered scroll region: top=%d bottom=%d", s.scrollTop, s.scrollBottom)
	}
}

// XTVERSION (CSI > q) is answered with a generic name.
func TestXTVERSION_Answered(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[>q"))
	if got, want := string(s.response), "\x1bP>|web-terminal-engine\x1b\\"; got != want {
		t.Errorf("CSI >q = %q, want %q", got, want)
	}
}

// --- Cursor-correctness cluster ---

// RIS homes the cursor and signals a scrollback clear.
func TestRIS_HomesCursorAndClearsScrollback(t *testing.T) {
	s := New(10, 20)
	s.Write([]byte("\x1b[5;5H"))
	s.Write([]byte("\x1bc")) // RIS
	if row, col := s.CursorPos(); row != 0 || col != 0 {
		t.Errorf("RIS left cursor at %d,%d, want 0,0", row, col)
	}
	if !s.scrollbackCleared {
		t.Errorf("RIS did not set ScrollbackCleared")
	}
}

// RI clears the pending-wrap state.
func TestRI_ClearsPendingWrap(t *testing.T) {
	s := New(5, 3)
	s.Write([]byte("ABC")) // fills the row; pendingWrap armed at the right margin
	if !s.pendingWrap {
		t.Fatalf("precondition: pendingWrap should be armed after filling the row")
	}
	s.Write([]byte("\x1bM")) // RI
	if s.pendingWrap {
		t.Errorf("RI did not clear pendingWrap")
	}
}

// IL and DL move the cursor to column 0.
func TestILDL_MoveToColumnZero(t *testing.T) {
	s := New(10, 20)
	s.Write([]byte("\x1b[3;10H")) // row 2, col 9
	s.Write([]byte("\x1b[L"))     // IL
	if _, col := s.CursorPos(); col != 0 {
		t.Errorf("IL left column at %d, want 0", col)
	}
	s.Write([]byte("\x1b[3;10H"))
	s.Write([]byte("\x1b[M")) // DL
	if _, col := s.CursorPos(); col != 0 {
		t.Errorf("DL left column at %d, want 0", col)
	}
}

// VPA respects origin mode (row relative to the scroll region top).
func TestVPA_OriginModeRelative(t *testing.T) {
	s := New(12, 20)
	s.Write([]byte("\x1b[4;9r")) // scroll region rows 3..8 (0-based)
	s.Write([]byte("\x1b[?6h"))  // origin mode on
	s.Write([]byte("\x1b[3d"))   // VPA row 3 (1-based) -> region-relative
	if row, _ := s.CursorPos(); row != 3+2 {
		t.Errorf("VPA 3 under origin mode -> row %d, want %d (scrollTop+2)", row, 3+2)
	}
}

// DECSTBM with an invalid (empty) region is a no-op and does not move the cursor.
func TestDECSTBM_InvalidRegionNoOp(t *testing.T) {
	s := New(10, 20)
	s.Write([]byte("\x1b[5;5H")) // cursor row 4 col 4
	s.Write([]byte("\x1b[8;3r")) // invalid: top(7) >= bottom(2)
	if row, col := s.CursorPos(); row != 4 || col != 4 {
		t.Errorf("invalid DECSTBM moved cursor to %d,%d, want 4,4", row, col)
	}
	if s.scrollTop != 0 || s.scrollBottom != s.Height-1 {
		t.Errorf("invalid DECSTBM changed region: top=%d bottom=%d", s.scrollTop, s.scrollBottom)
	}
}

// --- P1 ---

// CUU stops at the top margin when the cursor starts inside the region;
// CUD stops at the bottom margin.
func TestCursorUpDown_RespectScrollMargins(t *testing.T) {
	s := New(10, 20)
	s.Write([]byte("\x1b[3;8r")) // region rows 2..7 (0-based); homes cursor to 0,0
	s.Write([]byte("\x1b[6;1H")) // row 5 (inside region)
	s.Write([]byte("\x1b[100A")) // CUU far
	if row, _ := s.CursorPos(); row != 2 {
		t.Errorf("CUU from inside region -> row %d, want 2 (top margin)", row)
	}
	s.Write([]byte("\x1b[6;1H")) // back to row 5
	s.Write([]byte("\x1b[100B")) // CUD far
	if row, _ := s.CursorPos(); row != 7 {
		t.Errorf("CUD from inside region -> row %d, want 7 (bottom margin)", row)
	}
}

// DECSTR (soft reset) preserves tab stops; RIS (hard reset) clears them.
func TestDECSTR_PreservesTabStops(t *testing.T) {
	s := New(10, 20)
	s.Write([]byte("\x1b[1;4H")) // col 3
	s.Write([]byte("\x1bH"))     // HTS: set a tab stop at col 3
	if s.tabStops == nil || !s.tabStops[3] {
		t.Fatalf("precondition: HTS did not set a stop at col 3")
	}
	s.Write([]byte("\x1b[!p")) // DECSTR
	if s.tabStops == nil || !s.tabStops[3] {
		t.Errorf("DECSTR cleared tab stops; want them preserved")
	}
	s.Write([]byte("\x1bc")) // RIS
	if s.tabStops != nil {
		t.Errorf("RIS did not reset tab stops (tabStops = %v)", s.tabStops)
	}
}

// IRM (CSI 4 h) inserts characters instead of overwriting.
func TestIRM_InsertMode(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("ABC"))
	s.Write([]byte("\x1b[G"))  // col 0
	s.Write([]byte("\x1b[4h")) // IRM on
	s.Write([]byte("X"))
	if got := s.RowString(0); got != "XABC" {
		t.Errorf("IRM insert: row = %q, want %q", got, "XABC")
	}
	if !s.InsertMode {
		t.Errorf("InsertMode not set after CSI 4h")
	}
	s.Write([]byte("\x1b[4l")) // IRM off
	if s.InsertMode {
		t.Errorf("InsertMode still set after CSI 4l")
	}
}

// Underline style subparams: 4:0 = off, 4:2 = double, 4:3 (curly) = single.
func TestUnderlineStyleSubparams(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b[4:3m")) // curly -> single underline
	if !s.style.Underline || s.style.DoubleUnderline {
		t.Errorf("4:3m -> Underline=%v Double=%v, want true/false", s.style.Underline, s.style.DoubleUnderline)
	}
	s.Write([]byte("\x1b[4:0m")) // off
	if s.style.Underline || s.style.DoubleUnderline {
		t.Errorf("4:0m -> Underline=%v Double=%v, want false/false", s.style.Underline, s.style.DoubleUnderline)
	}
	s.Write([]byte("\x1b[4:2m")) // double
	if s.style.DoubleUnderline != true || s.style.Underline {
		t.Errorf("4:2m -> Underline=%v Double=%v, want false/true", s.style.Underline, s.style.DoubleUnderline)
	}
}

// --- P2 completeness ---

// DECIC inserts blank columns; DECDC deletes columns.
func TestDECIC_DECDC(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("ABCDE"))
	s.Write([]byte("\x1b[2G"))  // cursor to col 1 (on 'B')
	s.Write([]byte("\x1b[2'}")) // DECIC 2
	if got := s.RowString(0); got != "A  BCDE" {
		t.Errorf("DECIC: row = %q, want %q", got, "A  BCDE")
	}
	s2 := New(2, 10)
	s2.Write([]byte("ABCDE"))
	s2.Write([]byte("\x1b[2G"))  // col 1
	s2.Write([]byte("\x1b[2'~")) // DECDC 2
	if got := s2.RowString(0); got != "ADE" {
		t.Errorf("DECDC: row = %q, want %q", got, "ADE")
	}
}

// DECALN (ESC # 8) fills the screen with 'E' and homes the cursor.
func TestDECALN(t *testing.T) {
	s := New(3, 4)
	s.Write([]byte("\x1b[2;2H")) // move cursor off home
	s.Write([]byte("\x1b#8"))
	for y := range 3 {
		if got := s.RowString(y); got != "EEEE" {
			t.Errorf("DECALN row %d = %q, want EEEE", y, got)
		}
	}
	if row, col := s.CursorPos(); row != 0 || col != 0 {
		t.Errorf("DECALN left cursor at %d,%d, want 0,0", row, col)
	}
}

// DECSCA-protected cells survive selective erase (DECSEL) but not ordinary EL.
func TestDECSCA_SelectiveErase(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b[1\"q")) // DECSCA: protect
	s.Write([]byte("AB"))
	s.Write([]byte("\x1b[0\"q")) // DECSCA: unprotect
	s.Write([]byte("CD"))
	s.Write([]byte("\x1b[?2K")) // DECSEL: selective erase whole line
	if got := s.RowString(0); got != "AB" {
		t.Errorf("DECSEL: row = %q, want %q (protected AB survive, CD erased)", got, "AB")
	}
	s.Write([]byte("\x1b[2K")) // plain EL: erases everything, incl. protected
	if got := s.RowString(0); got != "" {
		t.Errorf("EL after DECSEL: row = %q, want empty", got)
	}
}

// A bright-color style round-trips through the SGR emitter (DECRQSS m),
// e.g. bright red fg (91) is reported as 91, not 39.
func TestBrightColorRoundTrip(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b[91m")) // bright red fg
	s.response = nil
	s.Write([]byte("\x1bP$qm\x1b\\")) // DECRQSS SGR
	if got, want := string(s.response), "\x1bP1$r0;91m\x1b\\"; got != want {
		t.Errorf("DECRQSS after 91m = %q, want %q", got, want)
	}
}

// The ITU colon RGB forms parse correctly: 5-element 38:2:r:g:b and 6-element
// 38:2::r:g:b (with an empty color-space slot).
func TestExtColorColonForms(t *testing.T) {
	want := Color{Type: 3, R: 10, G: 20, B: 30}
	s := New(2, 10)
	s.Write([]byte("\x1b[38:2:10:20:30m"))
	if s.style.FG != want {
		t.Errorf("38:2:10:20:30m -> FG %+v, want %+v", s.style.FG, want)
	}
	s2 := New(2, 10)
	s2.Write([]byte("\x1b[38:2::10:20:30m"))
	if s2.style.FG != want {
		t.Errorf("38:2::10:20:30m (6-element) -> FG %+v, want %+v", s2.style.FG, want)
	}
}

// The DEC Special Graphics set maps 0x5F to a blank and 0x71 to the horizontal
// line; UK NRCS maps '#' to '£'.
func TestCharsetGraphicsAndUK(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b(0")) // designate G0 = DEC Special Graphics
	s.Write([]byte("q_"))     // 'q' -> ─ ; '_' -> blank
	if got := s.Cells[0][0].Ch; got != '\u2500' {
		t.Errorf("graphics 'q' -> %q, want ─ (U+2500)", got)
	}
	if got := s.Cells[0][1].Ch; got != ' ' {
		t.Errorf("graphics '_' -> %q, want blank", got)
	}
	u := New(2, 10)
	u.Write([]byte("\x1b(A")) // designate G0 = UK NRCS
	u.Write([]byte("#"))
	if got := u.Cells[0][0].Ch; got != '\u00a3' {
		t.Errorf("UK '#' -> %q, want £ (U+00A3)", got)
	}
}

// OSC 10/11/12 color queries are answered from the fixed theme colors.
func TestOSCColorQueries(t *testing.T) {
	cases := []struct {
		seq  string
		want string
	}{
		{"\x1b]10;?\x1b\\", "\x1b]10;rgb:dddd/dede/e1e1\x1b\\"}, // fg
		{"\x1b]11;?\x1b\\", "\x1b]11;rgb:0000/0000/0000\x1b\\"}, // bg (black)
		{"\x1b]12;?\x1b\\", "\x1b]12;rgb:dddd/dede/e1e1\x1b\\"}, // cursor
	}
	for _, tc := range cases {
		s := New(5, 20)
		s.Write([]byte(tc.seq))
		if got := string(s.response); got != tc.want {
			t.Errorf("query %q -> %q, want %q", tc.seq, got, tc.want)
		}
	}
	// The SET form (non-"?") is ignored and produces no response.
	s := New(5, 20)
	s.Write([]byte("\x1b]11;rgb:ff/00/00\x1b\\"))
	if len(s.response) != 0 {
		t.Errorf("OSC 11 set produced a response %q, want none", s.response)
	}
}

// TestOSCColorThemeConfigurable verifies WithTheme overrides the colors reported
// by OSC 10/11/12 queries, and that OSC 110 (reset) restores the configured
// foreground rather than the built-in default.
func TestOSCColorThemeConfigurable(t *testing.T) {
	theme := Theme{
		Foreground: RGB(0x11, 0x22, 0x33),
		Background: RGB(0x44, 0x55, 0x66),
		Cursor:     RGB(0x77, 0x88, 0x99),
	}
	query := func(s *Screen, seq string) string {
		s.response = s.response[:0]
		s.Write([]byte(seq))
		out := string(s.response)
		s.response = s.response[:0]
		return out
	}

	s := New(5, 20, WithTheme(theme))
	for seq, want := range map[string]string{
		"\x1b]10;?\x1b\\": "\x1b]10;rgb:1111/2222/3333\x1b\\", // fg
		"\x1b]11;?\x1b\\": "\x1b]11;rgb:4444/5555/6666\x1b\\", // bg
		"\x1b]12;?\x1b\\": "\x1b]12;rgb:7777/8888/9999\x1b\\", // cursor
	} {
		if got := query(s, seq); got != want {
			t.Errorf("query %q -> %q, want %q", seq, got, want)
		}
	}

	// Override the fg, then reset: OSC 110 must restore the configured theme fg.
	s.Write([]byte("\x1b]10;rgb:ff/00/00\x1b\\"))
	if got := query(s, "\x1b]10;?\x1b\\"); got != "\x1b]10;rgb:ffff/0000/0000\x1b\\" {
		t.Errorf("after set, fg query -> %q", got)
	}
	s.Write([]byte("\x1b]110\x1b\\"))
	if got := query(s, "\x1b]10;?\x1b\\"); got != "\x1b]10;rgb:1111/2222/3333\x1b\\" {
		t.Errorf("after OSC 110 reset, fg query -> %q, want configured theme fg", got)
	}
}
