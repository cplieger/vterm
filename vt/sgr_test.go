package vt

import "testing"

// TestSGRRoundTrip verifies sgrSequence faithfully re-emits every Style
// attribute: parsing the emitted sequence reproduces the original Style.
func TestSGRRoundTrip(t *testing.T) {
	want := Style{
		Bold:            true,
		Dim:             true,
		Italic:          true,
		DoubleUnderline: true,
		Overline:        true,
		Blink:           true,
		Inverse:         true,
		Strikethrough:   true,
		Hidden:          true,
		FG:              Color{Type: 3, R: 10, G: 20, B: 30},
		BG:              Color{Type: 2, Val: 200},
		UnderlineColor:  Color{Type: 3, R: 1, G: 2, B: 3},
	}
	s := New(1, 4)
	s.Write([]byte(sgrSequence(want) + "X"))
	if got := s.Cells[0][0].Style; got != want {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, want)
	}
}

// setParamGroups builds n single-param semicolon groups directly on the parser
// state (mirrors what "\x1b[v0;v1;...m" produces): numGroups=len(vals), each
// group length 1 with pParams[i]=vals[i].
func setParamGroups(s *Screen, vals ...uint16) {
	s.numGroups = uint8(len(vals))
	s.numParams = uint8(len(vals))
	for i, v := range vals {
		s.pParams[i] = v
		s.pGroupLen[i] = 1
	}
}

// TestSGRSingleNonZeroParamApplies verifies that a single non-zero SGR
// parameter (CSI 1 m = bold) is applied rather than triggering the empty/zero
// reset fast path.
func TestSGRSingleNonZeroParamApplies(t *testing.T) {
	s := New(2, 10)
	s.Write([]byte("\x1b[1m"))

	if !s.style.Bold {
		t.Errorf("CSI 1 m: style.Bold = false, want true (a single non-zero SGR must apply, not reset)")
	}
}

// TestSGREmptyParamResetsStyle verifies that an empty SGR (CSI m) resets the
// style, matching SGR 0.
func TestSGREmptyParamResetsStyle(t *testing.T) {
	s := New(1, 10)
	s.Write([]byte("\x1b[1m"))
	s.Write([]byte("\x1b[m")) // empty param = SGR 0 (reset)
	s.Write([]byte("X"))
	if s.Cells[0][0].Ch != 'X' {
		t.Fatal("X not written")
	}
	if s.Cells[0][0].Style.Bold {
		t.Error("empty SGR did not reset bold")
	}
}

// TestSGRParamsStringUnderlineColor verifies sgrParamsString emits the 58;...
// underline-color parameters when an underline color is set, and omits them
// otherwise.
func TestSGRParamsStringUnderlineColor(t *testing.T) {
	if got := sgrParamsString(Style{UnderlineColor: Color{Type: 2, Val: 5}}); got != "0;58;5;5" {
		t.Errorf("sgrParamsString(underline 256-color 5) = %q, want %q", got, "0;58;5;5")
	}
	if got := sgrParamsString(Style{Bold: true}); got != "0;1" {
		t.Errorf("sgrParamsString(bold only) = %q, want %q", got, "0;1")
	}
}

// TestSGRParamsStringPaletteColors pins how a 16-color palette slot is
// re-emitted, which is what a DECRQSS "m" query reports back to the application.
// ECMA-48 assigns the first eight slots 30-37 (foreground) and 40-47
// (background); the bright half has no ECMA-48 codes and rides xterm's 90-97 /
// 100-107 instead. Emitting base+Val for a bright slot would produce 38 or 48,
// which a reader parses as the opening of an extended-color sequence rather than
// as a color at all.
func TestSGRParamsStringPaletteColors(t *testing.T) {
	cases := []struct {
		name string
		st   Style
		want string
	}{
		{"first normal foreground", Style{FG: Color{Type: 1, Val: 0}}, "0;30"},
		{"last normal foreground", Style{FG: Color{Type: 1, Val: 7}}, "0;37"},
		{"first bright foreground", Style{FG: Color{Type: 1, Val: 8}}, "0;90"},
		{"last bright foreground", Style{FG: Color{Type: 1, Val: 15}}, "0;97"},
		{"last normal background", Style{BG: Color{Type: 1, Val: 7}}, "0;47"},
		{"first bright background", Style{BG: Color{Type: 1, Val: 8}}, "0;100"},
		{"last bright background", Style{BG: Color{Type: 1, Val: 15}}, "0;107"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sgrParamsString(tc.st); got != tc.want {
				t.Errorf("sgrParamsString(%+v) = %q, want %q", tc.st, got, tc.want)
			}
		})
	}
}

// TestParseExtColorMissingFollowingGroup verifies that SGR 38 with no following
// mode group leaves the color unset and returns the same index (no advance).
func TestParseExtColorMissingFollowingGroup(t *testing.T) {
	s := &Screen{}
	setParamGroups(s, 38)
	var c Color
	if got := s.parseExtColorGroup(0, &c); got != 0 {
		t.Errorf("parseExtColorGroup(0) with [38]: ret = %d, want 0 (bail, no following group)", got)
	}
	if c.Type != 0 {
		t.Errorf("parseExtColorGroup(0) with [38]: c.Type = %d, want 0 (no color set)", c.Type)
	}
}

// TestParseExtColor256MissingValue verifies that SGR 38;5 (256-color) with no
// value group leaves the color unset and returns i+1.
func TestParseExtColor256MissingValue(t *testing.T) {
	s := &Screen{}
	setParamGroups(s, 38, 5)
	var c Color
	if got := s.parseExtColorGroup(0, &c); got != 1 {
		t.Errorf("parseExtColorGroup(0) with [38,5]: ret = %d, want 1 (missing value group)", got)
	}
	if c.Type != 0 {
		t.Errorf("parseExtColorGroup(0) with [38,5]: c.Type = %d, want 0 (no color set)", c.Type)
	}
}

// TestParseExtColorRGBMissingBlue verifies that SGR 38;2;R;G (RGB) with the blue
// group absent leaves the color unset and returns i+1.
func TestParseExtColorRGBMissingBlue(t *testing.T) {
	s := &Screen{}
	setParamGroups(s, 38, 2, 10, 20)
	var c Color
	if got := s.parseExtColorGroup(0, &c); got != 1 {
		t.Errorf("parseExtColorGroup(0) with [38,2,10,20]: ret = %d, want 1 (incomplete RGB)", got)
	}
	if c.Type != 0 {
		t.Errorf("parseExtColorGroup(0) with [38,2,10,20]: c.Type = %d, want 0 (no color set)", c.Type)
	}
}

// TestParseExtColor256EmptyValueGroup verifies that a counted-but-empty value
// group (Len 0) for SGR 38;5 must not set a color.
func TestParseExtColor256EmptyValueGroup(t *testing.T) {
	s := &Screen{}
	s.numGroups = 3
	s.numParams = 2
	s.pGroupLen[0] = 1
	s.pGroupLen[1] = 1
	s.pGroupLen[2] = 0 // empty value group
	s.pParams[0] = 38
	s.pParams[1] = 5
	var c Color
	if got := s.parseExtColorGroup(0, &c); got != 2 {
		t.Errorf("parseExtColorGroup(0) [38,5,<empty>]: ret = %d, want 2", got)
	}
	if c.Type != 0 {
		t.Errorf("parseExtColorGroup(0) [38,5,<empty>]: c.Type = %d, want 0 (empty value group must not set color)", c.Type)
	}
}

// TestParseExtColorRGBEmptyBlueGroup verifies that a counted-but-empty blue
// group (Len 0) for SGR 38;2;R;G must not set a color.
func TestParseExtColorRGBEmptyBlueGroup(t *testing.T) {
	s := &Screen{}
	s.numGroups = 5
	s.numParams = 4
	s.pGroupLen[0] = 1
	s.pGroupLen[1] = 1
	s.pGroupLen[2] = 1
	s.pGroupLen[3] = 1
	s.pGroupLen[4] = 0 // empty blue group
	s.pParams[0] = 38
	s.pParams[1] = 2
	s.pParams[2] = 10
	s.pParams[3] = 20
	var c Color
	if got := s.parseExtColorGroup(0, &c); got != 4 {
		t.Errorf("parseExtColorGroup(0) RGB empty-blue: ret = %d, want 4", got)
	}
	if c.Type != 0 {
		t.Errorf("parseExtColorGroup(0) RGB empty-blue: c.Type = %d, want 0 (empty blue group must not set color)", c.Type)
	}
}

// TestSGRSubparamColonRGB verifies colon-subparam RGB foreground (38:2:R:G:B).
func TestSGRSubparamColonRGB(t *testing.T) {
	s := New(1, 10)
	s.Write([]byte("\x1b[38:2:255:0:128mX"))
	cell := s.Cells[0][0]
	if cell.Style.FG.Type != 3 {
		t.Fatalf("expected RGB color type 3, got %d", cell.Style.FG.Type)
	}
	if cell.Style.FG.R != 255 || cell.Style.FG.G != 0 || cell.Style.FG.B != 128 {
		t.Errorf("RGB: got R=%d G=%d B=%d, want 255,0,128",
			cell.Style.FG.R, cell.Style.FG.G, cell.Style.FG.B)
	}
}

// TestSGRSubparamColon256 verifies colon-subparam 256-color foreground (38:5:N).
func TestSGRSubparamColon256(t *testing.T) {
	s := New(1, 10)
	s.Write([]byte("\x1b[38:5:196mX"))
	cell := s.Cells[0][0]
	if cell.Style.FG.Type != 2 {
		t.Fatalf("expected 256-color type 2, got %d", cell.Style.FG.Type)
	}
	if cell.Style.FG.Val != 196 {
		t.Errorf("256-color: got %d, want 196", cell.Style.FG.Val)
	}
}

// TestSGRSubparamSemicolonLegacyRGB verifies the legacy semicolon RGB form
// (38;2;R;G;B).
func TestSGRSubparamSemicolonLegacyRGB(t *testing.T) {
	s := New(1, 10)
	s.Write([]byte("\x1b[38;2;144;70;255mX"))
	cell := s.Cells[0][0]
	if cell.Style.FG.Type != 3 {
		t.Fatalf("expected RGB color type 3, got %d", cell.Style.FG.Type)
	}
	if cell.Style.FG.R != 144 || cell.Style.FG.G != 70 || cell.Style.FG.B != 255 {
		t.Errorf("RGB: got R=%d G=%d B=%d, want 144,70,255",
			cell.Style.FG.R, cell.Style.FG.G, cell.Style.FG.B)
	}
}

// TestSGRSubparamMixed verifies a mix of bold, colon-RGB foreground, and
// semicolon-256 background in one SGR sequence.
func TestSGRSubparamMixed(t *testing.T) {
	s := New(1, 10)
	s.Write([]byte("\x1b[1;38:2:10:20:30;48;5;200mX"))
	cell := s.Cells[0][0]
	if !cell.Style.Bold {
		t.Error("expected Bold")
	}
	if cell.Style.FG.Type != 3 || cell.Style.FG.R != 10 || cell.Style.FG.G != 20 || cell.Style.FG.B != 30 {
		t.Errorf("FG RGB: got %+v", cell.Style.FG)
	}
	if cell.Style.BG.Type != 2 || cell.Style.BG.Val != 200 {
		t.Errorf("BG 256: got %+v", cell.Style.BG)
	}
}

// TestSGRSubparamNonColorAttrPreservesFollowingGroups verifies that a colon
// subparam attached to a NON-color SGR attribute (4:3 = underline style 3) stays
// inside its own parameter group: the trailing :3 must not leak out as a separate
// SGR 3 (italic), and the groups that follow (31 = red FG, 1 = bold) still apply.
func TestSGRSubparamNonColorAttrPreservesFollowingGroups(t *testing.T) {
	s := New(1, 10)
	s.Write([]byte("\x1b[4:3;31;1mX"))
	st := s.Cells[0][0].Style
	if !st.Underline {
		t.Errorf("4:3;31;1: Underline = false, want true (group 0 = 4:3 = underline)")
	}
	if st.Italic {
		t.Errorf("4:3;31;1: Italic = true, want false (the :3 subparam must stay inside group 0, not leak as SGR 3)")
	}
	if st.FG.Type != 1 || st.FG.Val != 1 {
		t.Errorf("4:3;31;1: FG = %+v, want {Type:1 Val:1} (red, group 1 = 31)", st.FG)
	}
	if !st.Bold {
		t.Errorf("4:3;31;1: Bold = false, want true (group 2 = 1 must apply after the colon group)")
	}
}
