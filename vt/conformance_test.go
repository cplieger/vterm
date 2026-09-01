package vt

import (
	"testing"
)

// TestDECSC_RoundTrip verifies that DECSC/DECRC saves and restores the DEC STD
// 070 cursor state: position, style (SGR), origin mode, and charset. It also
// verifies that DECAWM (autowrap) is deliberately NOT part of that state.
func TestDECSC_RoundTrip(t *testing.T) {
	s := New(24, 80)

	s.Write([]byte("\x1b[1;31m")) // bold + red FG
	s.Write([]byte("\x1b[?6h"))   // origin mode on
	s.Write([]byte("\x1b[?7l"))   // autowrap off
	s.Write([]byte("\x1b(0"))     // G0 = DEC Special Graphics
	s.Write([]byte("\x1b[5;10H")) // move cursor to row 5, col 10

	s.Write([]byte("\x1b7")) // save cursor

	s.Write([]byte("\x1b[0m"))   // reset style
	s.Write([]byte("\x1b[?6l"))  // origin mode off
	s.Write([]byte("\x1b[?7h"))  // autowrap on
	s.Write([]byte("\x1b(B"))    // G0 = ASCII
	s.Write([]byte("\x1b[1;1H")) // move cursor to 1,1

	s.Write([]byte("\x1b8")) // restore cursor

	row, col := s.CursorPos()
	if row != 4 || col != 9 { // 0-indexed: row 5 → 4, col 10 → 9
		t.Errorf("cursor position: got (%d,%d), want (4,9)", row, col)
	}
	if !s.style.Bold {
		t.Error("style.Bold not restored")
	}
	if s.style.FG.Type != 1 || s.style.FG.Val != 1 { // basic red = type 1, val 1
		t.Errorf("style.FG not restored: got %+v", s.style.FG)
	}
	if !s.OriginMode {
		t.Error("OriginMode not restored")
	}
	// DECAWM is NOT saved/restored by DECSC/DECRC (DEC STD 070), so the value
	// set AFTER the save (autowrap on) must survive the restore.
	if !s.AutoWrap {
		t.Error("AutoWrap should NOT be restored by DECRC; it must keep its post-save value (on)")
	}
	if s.gsets[0] != charsetGraphic {
		t.Error("G0 charset not restored to DEC Special Graphics")
	}
}

// TestDECSC_Mode1048 verifies that mode 1048 h/l also saves/restores full state.
func TestDECSC_Mode1048(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[1m"))     // bold
	s.Write([]byte("\x1b[3;5H"))   // row 3, col 5
	s.Write([]byte("\x1b[?1048h")) // save

	s.Write([]byte("\x1b[0m")) // reset
	s.Write([]byte("\x1b[1;1H"))

	s.Write([]byte("\x1b[?1048l")) // restore

	row, col := s.CursorPos()
	if row != 2 || col != 4 {
		t.Errorf("cursor position: got (%d,%d), want (2,4)", row, col)
	}
	if !s.style.Bold {
		t.Error("style.Bold not restored via mode 1048")
	}
}

// TestTabStops_SetAndClear verifies HTS sets, TBC(0) clears at cursor, TBC(3) clears all.
func TestTabStops_SetAndClear(t *testing.T) {
	s := New(24, 80)

	// Default: tabs at 8, 16, 24, ...
	// Move to col 0, tab should go to 8
	s.Write([]byte("\t"))
	_, col := s.CursorPos()
	if col != 8 {
		t.Errorf("default tab: got col %d, want 8", col)
	}

	// Set a custom tab at col 5
	s.Write([]byte("\x1b[1G")) // move to col 1 (0-indexed: 0)
	s.Write([]byte("\x1b[6G")) // move to col 6 (0-indexed: 5)
	s.Write([]byte("\x1bH"))   // HTS at col 5

	// Tab from col 0 should now stop at 5
	s.Write([]byte("\x1b[1G")) // col 0
	s.Write([]byte("\t"))
	_, col = s.CursorPos()
	if col != 5 {
		t.Errorf("custom tab: got col %d, want 5", col)
	}

	// Clear tab at col 5 (TBC 0)
	s.Write([]byte("\x1b[6G")) // move to col 5
	s.Write([]byte("\x1b[0g")) // clear tab at cursor
	s.Write([]byte("\x1b[1G")) // col 0
	s.Write([]byte("\t"))
	_, col = s.CursorPos()
	if col != 8 {
		t.Errorf("after clear at 5, tab: got col %d, want 8", col)
	}

	// Clear all tabs (TBC 3)
	s.Write([]byte("\x1b[3g")) // clear all
	s.Write([]byte("\x1b[1G")) // col 0
	s.Write([]byte("\t"))
	_, col = s.CursorPos()
	// With no tabs set, cursor goes to end of line
	if col != 79 {
		t.Errorf("after clear all, tab: got col %d, want 79 (end)", col)
	}
}

// TestTabStops_CBT verifies backward tab (CSI Z) honors custom tab stops.
func TestTabStops_CBT(t *testing.T) {
	s := New(24, 80)

	// Set tab at col 12
	s.Write([]byte("\x1b[13G")) // col 12
	s.Write([]byte("\x1bH"))    // HTS

	// Move to col 20, CBT should go back to 16 (default), then 12 (custom)
	s.Write([]byte("\x1b[21G")) // col 20
	s.Write([]byte("\x1b[Z"))   // CBT 1
	_, col := s.CursorPos()
	if col != 16 {
		t.Errorf("CBT from 20: got col %d, want 16", col)
	}
	s.Write([]byte("\x1b[Z")) // CBT 1 more
	_, col = s.CursorPos()
	if col != 12 {
		t.Errorf("CBT from 16: got col %d, want 12", col)
	}
}

// TestDSR5 verifies that DSR 5 (operating status) replies with CSI 0 n.
func TestDSR5(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[5n"))
	if string(s.response) != "\x1b[0n" {
		t.Errorf("DSR 5 response: got %q, want %q", string(s.response), "\x1b[0n")
	}
}

// TestDECXCPR verifies that CSI ? 6 n returns the extended cursor position report.
func TestDECXCPR(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[10;20H")) // move to row 10, col 20
	s.Write([]byte("\x1b[?6n"))    // DECXCPR
	// VT400+ (which the engine advertises) includes the page number (always 1).
	want := "\x1b[?10;20;1R"
	if string(s.response) != want {
		t.Errorf("DECXCPR response: got %q, want %q", string(s.response), want)
	}
}

// TestDECRQM_DEC verifies DECRQM for DEC private modes returns correct DECRPM.
func TestDECRQM_DEC(t *testing.T) {
	s := New(24, 80)

	// Mode 7 (DECAWM) should be set by default
	s.Write([]byte("\x1b[?7$p"))
	want := "\x1b[?7;1$y" // set
	if string(s.response) != want {
		t.Errorf("DECRQM ?7: got %q, want %q", string(s.response), want)
	}
	s.response = nil

	// Mode 6 (DECOM) should be reset by default
	s.Write([]byte("\x1b[?6$p"))
	want = "\x1b[?6;2$y" // reset
	if string(s.response) != want {
		t.Errorf("DECRQM ?6: got %q, want %q", string(s.response), want)
	}
	s.response = nil

	// Unknown mode should return 0
	s.Write([]byte("\x1b[?9999$p"))
	want = "\x1b[?9999;0$y"
	if string(s.response) != want {
		t.Errorf("DECRQM ?9999: got %q, want %q", string(s.response), want)
	}
}

// TestDECSCNM verifies that mode 5 (DECSCNM) sets/resets ReverseVideo.
func TestDECSCNM(t *testing.T) {
	s := New(24, 80)

	if s.ReverseVideo {
		t.Error("ReverseVideo should be false initially")
	}

	s.Write([]byte("\x1b[?5h")) // set
	if !s.ReverseVideo {
		t.Error("ReverseVideo should be true after CSI ?5h")
	}

	s.Write([]byte("\x1b[?5l")) // reset
	if s.ReverseVideo {
		t.Error("ReverseVideo should be false after CSI ?5l")
	}

	// Verify via DECRQM
	s.Write([]byte("\x1b[?5h"))
	s.response = nil
	s.Write([]byte("\x1b[?5$p"))
	want := "\x1b[?5;1$y"
	if string(s.response) != want {
		t.Errorf("DECRQM ?5 after set: got %q, want %q", string(s.response), want)
	}
}

// TestDECRQM_ANSI verifies DECRQM for ANSI modes (no '?' prefix): IRM (4) and
// LNM (20) report reset (2); an unrecognized mode reports not-recognized (0).
func TestDECRQM_ANSI(t *testing.T) {
	cases := []struct {
		seq  string
		want string
	}{
		{"\x1b[4$p", "\x1b[4;2$y"},
		{"\x1b[20$p", "\x1b[20;2$y"},
		{"\x1b[99$p", "\x1b[99;0$y"},
	}
	for _, tc := range cases {
		s := New(24, 80)
		s.Write([]byte(tc.seq))
		if got := string(s.response); got != tc.want {
			t.Errorf("DECRQM %q = %q, want %q", tc.seq, got, tc.want)
		}
	}
}

// TestAutoWrapMarginGating verifies DECAWM (CSI ?7h / ?7l) gates the right-margin
// wrap. With autowrap on (the default) a printable written past the last column
// wraps to the next row; with autowrap off the cursor parks at the last column and
// the next printable overwrites it in place.
func TestAutoWrapMarginGating(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		wantRow  int
		wantLast rune
		wantNext rune
	}{
		{"autowrap on wraps to next row", "\x1b[?7h", 1, 'E', 'F'},
		{"autowrap off overwrites last cell", "\x1b[?7l", 0, 'F', ' '},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(3, 5)
			s.Write([]byte(tc.mode))
			s.Write([]byte("ABCDE")) // fills the row; cursor parks at the last column
			s.Write([]byte("F"))     // the overflow character
			if s.curY != tc.wantRow {
				t.Errorf("%s: curY = %d, want %d", tc.name, s.curY, tc.wantRow)
			}
			if got := s.Cells[0][4].Ch; got != tc.wantLast {
				t.Errorf("%s: Cells[0][4].Ch = %q, want %q", tc.name, got, tc.wantLast)
			}
			if got := s.Cells[1][0].Ch; got != tc.wantNext {
				t.Errorf("%s: Cells[1][0].Ch = %q, want %q", tc.name, got, tc.wantNext)
			}
		})
	}
}
