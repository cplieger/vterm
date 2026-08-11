package vt

import "testing"

// Tests for the second feature batch: OSC 4 palette, OSC 52 clipboard,
// mouse mode 1016 (SGR-pixels), DECNKM (?66), and LNM (mode 20).

// --- OSC 4 palette ---

// OSC 4 overrides a palette index; the override reaches the wire color for both
// the basic (SGR 3x) and 256-color (38;5;N) forms of that index.
func TestOSC4PaletteOverride(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b]4;1;rgb:00/ff/00\x07")) // index 1 -> pure green
	if !s.paletteChanged {
		t.Errorf("OSC 4 set did not mark PaletteChanged")
	}
	s.Write([]byte("\x1b[31mX")) // SGR 31 = basic fg index 1
	runs := s.RenderRowWire(0)
	if len(runs) == 0 || runs[0].F != 0x00ff00 {
		t.Errorf("OSC 4 override (basic): run F = %#06x, want 0x00ff00", runs[0].F)
	}
	s2 := New(2, 10)
	s2.Write([]byte("\x1b]4;1;rgb:00/ff/00\x07\x1b[38;5;1mY"))
	if r2 := s2.RenderRowWire(0); len(r2) == 0 || r2[0].F != 0x00ff00 {
		t.Errorf("OSC 4 override (256-color): run F = %#06x, want 0x00ff00", r2[0].F)
	}
}

// OSC 4 with a "?" spec reports the current color of the index.
func TestOSC4Query(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b]4;1;?\x07"))
	// Default palette index 1 is 0xcc0403; reported as 16-bit-per-channel.
	want := "\x1b]4;1;rgb:cccc/0404/0303\x1b\\"
	if got := string(s.response); got != want {
		t.Errorf("OSC 4 query = %q, want %q", got, want)
	}
}

// OSC 104 resets a palette override back to the default color.
func TestOSC104Reset(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b]4;1;rgb:00/ff/00\x07"))
	s.Write([]byte("\x1b]104;1\x07")) // reset index 1
	s.Write([]byte("\x1b[31mX"))
	if runs := s.RenderRowWire(0); len(runs) == 0 || runs[0].F != 0xcc0403 {
		t.Errorf("after OSC 104 reset: run F = %#06x, want default 0xcc0403", runs[0].F)
	}
}

// The #RRGGBB color-spec form is also accepted.
func TestOSC4HashSpec(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b]4;2;#0000ff\x07\x1b[32mX")) // index 2 -> blue; SGR 32 = index 2
	if runs := s.RenderRowWire(0); len(runs) == 0 || runs[0].F != 0x0000ff {
		t.Errorf("OSC 4 #RRGGBB: run F = %#06x, want 0x0000ff", runs[0].F)
	}
}

// --- OSC 52 clipboard ---

// OSC 52 SET decodes the base64 payload into PendingClipboard.
func TestOSC52ClipboardSet(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b]52;c;aGVsbG8=\x07")) // base64("hello")
	if got := string(s.pendingClipboard); got != "hello" {
		t.Errorf("OSC 52 set: PendingClipboard = %q, want hello", got)
	}
}

// OSC 52 GET ("?") is denied by default (AllowScreenReport off): no clipboard
// event, no response. The read-back is gated behind AllowScreenReport (the
// DECRQCRA inject-vector precedent); see TestOSC52QueryRoundTrip for the
// enabled path.
func TestOSC52QueryDenied(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b]52;c;?\x07"))
	if s.pendingClipboard != nil {
		t.Errorf("OSC 52 query should be denied, got PendingClipboard %q", s.pendingClipboard)
	}
	if len(s.response) != 0 {
		t.Errorf("OSC 52 query should not respond, got %q", s.response)
	}
}

// --- Mouse mode 1016 (SGR-pixels) ---

func TestMouse1016Tracked(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b[?1016h"))
	if !s.MousePixels {
		t.Error("?1016h did not set MousePixels")
	}
	s.response = nil
	s.Write([]byte("\x1b[?1016$p")) // DECRQM
	if got, want := string(s.response), "\x1b[?1016;1$y"; got != want {
		t.Errorf("DECRQM ?1016 = %q, want %q", got, want)
	}
	s.Write([]byte("\x1b[?1016l"))
	if s.MousePixels {
		t.Error("?1016l did not clear MousePixels")
	}
}

// --- DECNKM (?66 application keypad) ---

func TestDECNKM(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b[?66h"))
	if !s.AppKeypad {
		t.Error("?66h did not set AppKeypad")
	}
	s.response = nil
	s.Write([]byte("\x1b[?66$p"))
	if got, want := string(s.response), "\x1b[?66;1$y"; got != want {
		t.Errorf("DECRQM ?66 = %q, want %q", got, want)
	}
	s.Write([]byte("\x1b[?66l"))
	if s.AppKeypad {
		t.Error("?66l did not clear AppKeypad")
	}
}

// --- LNM (mode 20, newline mode) ---

func TestLNM(t *testing.T) {
	s := New(3, 10)
	s.Write([]byte("\x1b[20h")) // LNM on
	if !s.LineFeedNewLine {
		t.Error("CSI 20h did not set LineFeedNewLine")
	}
	// With LNM, the LF also carriage-returns, so "cd" starts at column 0.
	s.Write([]byte("ab\ncd"))
	if got := s.RowString(1); got != "cd" {
		t.Errorf("LNM row 1 = %q, want %q", got, "cd")
	}
	s.response = nil
	s.Write([]byte("\x1b[20$p"))
	if got, want := string(s.response), "\x1b[20;1$y"; got != want {
		t.Errorf("DECRQM 20 = %q, want %q", got, want)
	}
	s.Write([]byte("\x1b[20l"))
	if s.LineFeedNewLine {
		t.Error("CSI 20l did not clear LineFeedNewLine")
	}
}

// Without LNM, a bare LF preserves the column (regression guard for the flag).
func TestLNMOffPreservesColumn(t *testing.T) {
	s := New(3, 10)
	s.Write([]byte("ab\ncd"))
	if got := s.RowString(1); got != "  cd" {
		t.Errorf("no LNM: row 1 = %q, want %q", got, "  cd")
	}
}

// --- Reverse-wraparound (mode ?45) ---

func TestReverseWraparound(t *testing.T) {
	s := New(3, 5)
	s.Write([]byte("\x1b[2;1H")) // row 1, col 0

	// Without ?45, BS at the left margin stays put.
	s.Write([]byte("\b"))
	if row, col := s.CursorPos(); row != 1 || col != 0 {
		t.Errorf("BS at col 0 without ?45 -> %d,%d, want 1,0", row, col)
	}

	// With ?45, BS at the left margin wraps to the end of the previous line.
	s.Write([]byte("\x1b[?45h\b"))
	if row, col := s.CursorPos(); row != 0 || col != 4 {
		t.Errorf("BS at col 0 with ?45 -> %d,%d, want 0,4 (end of previous line)", row, col)
	}

	// At the top of the screen, BS under ?45 wraps around to the bottom-right —
	// xterm's version-0 mode-45 behavior (the classic X10.4 upper-left ->
	// lower-right wrap), which esctest asserts by default (--xterm-reverse-wrap=0)
	// via test_BS_ReverseWrapGoesToBottom. The 2023 xterm split that limits ?45
	// and moves the wrap-around to ?1045 is a later, non-default version.
	s.Write([]byte("\x1b[1;1H\b"))
	if row, col := s.CursorPos(); row != 2 || col != 4 {
		t.Errorf("BS at top-left with ?45 -> %d,%d, want 2,4 (wrap around to bottom-right)", row, col)
	}

	s.Write([]byte("\x1b[?45l"))
	if s.ReverseWrap {
		t.Error("?45l did not clear ReverseWrap")
	}
}

// Reverse-wraparound requires DECAWM (autowrap): with ?45 set but ?7 reset, a
// Backspace at the left margin must NOT wrap back (xterm gates ?45 on ?7).
func TestReverseWraparoundRequiresAutowrap(t *testing.T) {
	s := New(3, 5)
	s.Write([]byte("\x1b[?45h\x1b[?7l")) // reverse-wrap on, autowrap OFF
	s.Write([]byte("\x1b[2;1H\b"))       // row 1, col 0, then BS
	if row, col := s.CursorPos(); row != 1 || col != 0 {
		t.Errorf("BS with ?45 but autowrap off -> %d,%d, want 1,0 (no reverse wrap)", row, col)
	}
}

// --- DECRQCRA (rectangular-area checksum, CSI Pid;Pp;Pt;Pl;Pb;Pr * y) ---

// DECRQCRA reports the negated 16-bit ordinal sum of a rectangle as
// DCS Pid ! ~ hhhh ST, matching xterm (patch < 279, esctest's default). This is
// the primitive esctest uses to read the screen back for content assertions.
func TestDECRQCRAChecksum(t *testing.T) {
	s := New(2, 10)
	s.AllowScreenReport = true
	s.Write([]byte("AB")) // row 0: 'A'(0x41) at col0, 'B'(0x42) at col1

	// Single cell containing 'A': checksum = 0x10000 - 0x41 = 0xFFBF.
	s.response = nil
	s.Write([]byte("\x1b[1;0;1;1;1;1*y")) // Pid=1 Pp=0 rect=(top1,left1,bottom1,right1)
	if got, want := string(s.response), "\x1bP1!~FFBF\x1b\\"; got != want {
		t.Errorf("DECRQCRA single cell 'A' = %q, want %q", got, want)
	}

	// Two cells "AB": checksum = 0x10000 - (0x41+0x42) = 0xFF7D.
	s.response = nil
	s.Write([]byte("\x1b[2;0;1;1;1;2*y")) // Pid=2, cols 1..2
	if got, want := string(s.response), "\x1bP2!~FF7D\x1b\\"; got != want {
		t.Errorf("DECRQCRA cells 'AB' = %q, want %q", got, want)
	}

	// A blank (unwritten) cell counts as space (0x20): checksum = 0xFFE0.
	s.response = nil
	s.Write([]byte("\x1b[3;0;1;5;1;5*y")) // Pid=3, a blank cell at col 5
	if got, want := string(s.response), "\x1bP3!~FFE0\x1b\\"; got != want {
		t.Errorf("DECRQCRA blank cell = %q, want %q", got, want)
	}
}

// DECRQCRA is gated: with AllowScreenReport off (production default) it produces
// no response, so it can't be used to scrape and re-inject screen content.
func TestDECRQCRAGatedOff(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("AB"))
	s.response = nil
	s.Write([]byte("\x1b[1;0;1;1;1;1*y"))
	if len(s.response) != 0 {
		t.Errorf("DECRQCRA with AllowScreenReport off should not respond, got %q", s.response)
	}
}

// --- OSC 52 clipboard query (gated behind AllowScreenReport) ---

// With AllowScreenReport enabled, an OSC 52 query round-trips the session
// selection back as base64. An empty request target list is reported as "s0"
// (xterm's default), matching esctest's ManipulateSelectionData contract.
func TestOSC52QueryRoundTrip(t *testing.T) {
	s := New(2, 10)
	s.AllowScreenReport = true
	s.Write([]byte("\x1b]52;;dGVzdGluZyAxMjM=\x07")) // set, empty target, base64("testing 123")
	if got := string(s.pendingClipboard); got != "testing 123" {
		t.Fatalf("OSC 52 set: PendingClipboard = %q, want %q", got, "testing 123")
	}
	s.response = nil
	s.Write([]byte("\x1b]52;;?\x07")) // query, empty target
	if got, want := string(s.response), "\x1b]52;s0;dGVzdGluZyAxMjM=\x1b\\"; got != want {
		t.Errorf("OSC 52 query = %q, want %q", got, want)
	}
}

// The query reply echoes an explicit request target list (e.g. "p" for primary).
func TestOSC52QueryEchoesTarget(t *testing.T) {
	s := New(2, 10)
	s.AllowScreenReport = true
	s.Write([]byte("\x1b]52;p;aGk=\x07")) // set primary = base64("hi")
	s.response = nil
	s.Write([]byte("\x1b]52;p;?\x07"))
	if got, want := string(s.response), "\x1b]52;p;aGk=\x1b\\"; got != want {
		t.Errorf("OSC 52 query (target p) = %q, want %q", got, want)
	}
}

// TestOSC52QueryStripsControlRunesFromTarget verifies the OSC 52 query reply
// strips C0/C1/DEL from the echoed target list. The reply is injected into the
// PTY as input, so a CR/LF smuggled into an attacker-set target must not survive
// into the reply, where it would inject a command line (same hardening as the
// XTWINOPS title report). The existing TestOSC52QueryEchoesTarget uses a
// control-free target ("p"), so it does not exercise stripControlRunes here.
func TestOSC52QueryStripsControlRunesFromTarget(t *testing.T) {
	s := New(2, 10)
	s.AllowScreenReport = true
	s.Write([]byte("\x1b]52;p;aGk=\x07")) // set primary = base64("hi")
	s.response = nil
	s.Write([]byte("\x1b]52;p\r\n;?\x07")) // query; CR/LF smuggled into the target
	if got, want := string(s.response), "\x1b]52;p;aGk=\x1b\\"; got != want {
		t.Errorf("OSC 52 query with CR/LF in target = %q, want %q (control runes stripped)", got, want)
	}
}

// An empty OSC 52 payload clears the selection, so a later query reports empty.
func TestOSC52ClearEmptiesSelection(t *testing.T) {
	s := New(2, 10)
	s.AllowScreenReport = true
	s.Write([]byte("\x1b]52;c;aGk=\x07")) // set
	s.Write([]byte("\x1b]52;c;\x07"))     // empty payload clears
	s.response = nil
	s.Write([]byte("\x1b]52;c;?\x07"))
	if got, want := string(s.response), "\x1b]52;c;\x1b\\"; got != want {
		t.Errorf("OSC 52 query after clear = %q, want %q", got, want)
	}
}

// The OSC 52 base64 decode is lenient: esctest's harness appends a stray "'"
// (a repr() quirk in strip_binary) that a strict decoder rejects. The selection
// must still round-trip.
func TestOSC52LenientDecodeTolerantOfStrayByte(t *testing.T) {
	s := New(2, 10)
	s.AllowScreenReport = true
	s.Write([]byte("\x1b]52;;dGVzdGluZyAxMjM='\x07")) // trailing quote, as esctest sends
	s.response = nil
	s.Write([]byte("\x1b]52;;?\x07"))
	if got, want := string(s.response), "\x1b]52;s0;dGVzdGluZyAxMjM=\x1b\\"; got != want {
		t.Errorf("OSC 52 query after stray-byte set = %q, want %q", got, want)
	}
}

func TestDecodeBase64Lenient(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"padded", "dGVzdA==", "test", true},
		{"unpadded", "dGVzdA", "test", true},
		{"trailing_quote", "dGVzdA=='", "test", true},    // esctest strip_binary quirk
		{"embedded_newline", "dGVz\ndA==", "test", true}, // RFC 2045 line break
		{"only_stray", "!!!", "", true},                  // filters to empty (valid empty decode)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := decodeBase64Lenient(c.in)
			if ok != c.ok {
				t.Fatalf("decodeBase64Lenient(%q) ok = %v, want %v", c.in, ok, c.ok)
			}
			if string(got) != c.want {
				t.Errorf("decodeBase64Lenient(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// --- DECNCSM (?95, no clear on column-mode change) ---

// DECRQM reports the tracked set/reset state of DECNCSM. New() is level 5, so
// the mode is recognized. (Before this, ?95$p reported 0 "not recognized".)
func TestDECNCSMDECRQM(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[?95h")) // DECSET DECNCSM
	if !s.noClearOnColumn {
		t.Fatal("?95h did not set noClearOnColumn")
	}
	s.response = nil
	s.Write([]byte("\x1b[?95$p"))
	if got, want := string(s.response), "\x1b[?95;1$y"; got != want {
		t.Errorf("DECRQM ?95 (set) = %q, want %q", got, want)
	}
	s.Write([]byte("\x1b[?95l")) // DECRESET
	s.response = nil
	s.Write([]byte("\x1b[?95$p"))
	if got, want := string(s.response), "\x1b[?95;2$y"; got != want {
		t.Errorf("DECRQM ?95 (reset) = %q, want %q", got, want)
	}
}

// DECCOLM performs its documented clear+home side effect even without
// Allow80To132 (only the declined 80<->132 resize is gated on ?40). The width
// must stay put (the browser owns it).
func TestDECCOLMClearsWithoutAllow80(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("X"))        // 'X' at (0,0)
	s.Write([]byte("\x1b[?3h")) // DECSET DECCOLM, no Allow80To132
	if got := s.RowString(0); got != "" {
		t.Errorf("DECCOLM should clear the screen without Allow80To132; row 0 = %q, want empty", got)
	}
	if s.Width != 80 {
		t.Errorf("DECCOLM must not resize (browser owns width); Width = %d, want 80", s.Width)
	}
}

// With DECNCSM set at level 5, a DECCOLM column-mode change does NOT clear.
func TestDECNCSMSuppressesClear(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[?95h")) // DECNCSM on (New() is level 5)
	s.Write([]byte("X"))
	s.Write([]byte("\x1b[?3h")) // DECCOLM
	if got := s.RowString(0); got != "X" {
		t.Errorf("DECNCSM should suppress the DECCOLM clear; row 0 = %q, want X", got)
	}
}

// Allow80To132 (?40) is tracked and reported by DECRQM.
func TestAllow80To132DECRQM(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[?40h"))
	s.response = nil
	s.Write([]byte("\x1b[?40$p"))
	if got, want := string(s.response), "\x1b[?40;1$y"; got != want {
		t.Errorf("DECRQM ?40 (set) = %q, want %q", got, want)
	}
}

// --- Set/Reset Title Modes (XTSMTITLE / XTRMTITLE) ---

// With set-hex (mode 0) on, an OSC 0/1/2 title payload is a hex byte string.
func TestTitleSetHexDecodes(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b[>0t"))        // XTSMTITLE set-hex
	s.Write([]byte("\x1b]2;6162\x07")) // "6162" == hex "ab"
	if s.Title != "ab" {
		t.Errorf("set-hex title = %q, want ab", s.Title)
	}
}

// TestTitleSetHexInvalidFallsBackToRaw verifies decodeTitle's documented
// contract: with set-hex on, an OSC 0/1/2 payload that is not valid hex falls
// back to the raw string rather than blanking the title.
func TestTitleSetHexInvalidFallsBackToRaw(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b[>0t"))       // XTSMTITLE set-hex on
	s.Write([]byte("\x1b]2;xyz\x07")) // "xyz" is not valid hex (odd length, non-hex)
	if s.Title != "xyz" {
		t.Errorf("set-hex title with invalid hex = %q, want %q (raw fallback, not blanked)", s.Title, "xyz")
	}
}

// With query-hex (mode 1) on, the WINOP 21 title report is hex-encoded.
func TestTitleQueryHexEncodes(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b]2;ab\x07")) // title "ab" (set-hex off)
	s.Write([]byte("\x1b[>1t"))      // XTSMTITLE query-hex
	s.response = nil
	s.Write([]byte("\x1b[21t")) // WINOP 21 report window title
	if got, want := string(s.response), "\x1b]l6162\x1b\\"; got != want {
		t.Errorf("query-hex report = %q, want %q", got, want)
	}
}

// XTRMTITLE (CSI > Pm T) disables the listed title-mode features.
func TestTitleModesResetByXTRMTITLE(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b[>0;1t"))      // set-hex + query-hex on
	s.Write([]byte("\x1b[>0;1T"))      // XTRMTITLE reset both
	s.Write([]byte("\x1b]2;6162\x07")) // literal now (set-hex off)
	if s.Title != "6162" {
		t.Errorf("after XTRMTITLE the title should be literal: got %q, want 6162", s.Title)
	}
}

// RIS resets the title modes to their default (all off); DECSTR leaves them.
func TestTitleModesResetByRIS(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b[>0t"))        // set-hex on
	s.Write([]byte("\x1bc"))           // RIS
	s.Write([]byte("\x1b]2;6162\x07")) // literal after RIS
	if s.Title != "6162" {
		t.Errorf("RIS should reset title modes: got %q, want 6162", s.Title)
	}
}

// TestTitleReportStripsControlRunes verifies XTWINOPS 20/21 title/icon reports
// strip C0/C1/DEL control runes on the query-hex-off (default, non-hex
// encodeTitle) path. The report is injected into the PTY as input, so a title
// carrying embedded CR/LF (smuggled in via set-hex mode, which admits arbitrary
// bytes) must not echo those bytes back. Covers stripControlRunes and the
// non-hex encodeTitle branch for both the icon (WINOP 20) and window (WINOP 21)
// reports.
func TestTitleReportStripsControlRunes(t *testing.T) {
	cases := []struct {
		name, setSeq, report, want string
	}{
		{
			name:   "window title CR/LF stripped (WINOP 21)",
			setSeq: "\x1b]2;61620d0a63\x07", // hex "ab\r\nc": embedded CR+LF
			report: "\x1b[21t",
			want:   "\x1b]labc\x1b\\",
		},
		{
			name:   "icon label NUL/DEL/C1 stripped (WINOP 20)",
			setSeq: "\x1b]1;69007fc29b6e\x07", // hex "i" NUL DEL U+009B "n"
			report: "\x1b[20t",
			want:   "\x1b]Lin\x1b\\",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(2, 10)
			s.Write([]byte("\x1b[>0t")) // XTSMTITLE set-hex on (admits raw bytes)
			s.Write([]byte(tc.setSeq))
			s.response = nil
			s.Write([]byte(tc.report)) // query-hex off -> non-hex encodeTitle path
			if got := string(s.response); got != tc.want {
				t.Errorf("%s report = %q, want %q (control runes stripped)", tc.name, got, tc.want)
			}
		})
	}
}
