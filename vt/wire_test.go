package vt

import (
	"strings"
	"testing"
)

// TestWireRunEncoding locks the binary wire contract by asserting that
// WireRun field values and attribute bit positions match the canonical
// encoder in terminal/wire_binary.go (the authoritative byte layout).
func TestWireRunEncoding(t *testing.T) {
	s := New(3, 10)
	// Bold red FG (ANSI 1), default BG, with italic+underline.
	s.Write([]byte("\x1b[1;3;4;31mHi\x1b[0m rest"))
	runs := s.RenderRowWire(0)
	if len(runs) < 2 {
		t.Fatalf("expected >=2 runs, got %d", len(runs))
	}

	r := runs[0]
	if r.T != "Hi" {
		t.Errorf("run[0].T = %q, want %q", r.T, "Hi")
	}
	// Red (ANSI index 1) → 0xaa0000 per basic16RGB palette.
	if r.F != 0xaa0000 {
		t.Errorf("run[0].F = 0x%06x, want 0xaa0000", r.F)
	}
	// Default BG → -1.
	if r.B != -1 {
		t.Errorf("run[0].B = %d, want -1", r.B)
	}
	// Default underline color → -1.
	if r.Uc != -1 {
		t.Errorf("run[0].Uc = %d, want -1", r.Uc)
	}
	// Attrs: bold=1 | italic=2 | underline=4 = 7.
	if r.A != 7 {
		t.Errorf("run[0].A = %d, want 7 (bold|italic|underline)", r.A)
	}

	// Second run: plain text, default colors, no attrs.
	r2 := runs[1]
	if r2.F != -1 {
		t.Errorf("run[1].F = %d, want -1 (default)", r2.F)
	}
	if r2.A != 0 {
		t.Errorf("run[1].A = %d, want 0", r2.A)
	}
}

// TestWireRunRGBColor verifies true-color encoding in wire format.
func TestWireRunRGBColor(t *testing.T) {
	s := New(3, 10)
	s.Write([]byte("\x1b[38;2;255;128;0mX\x1b[0m"))
	runs := s.RenderRowWire(0)
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	// RGB(255,128,0) → 0xFF8000.
	if runs[0].F != 0xFF8000 {
		t.Errorf("F = 0x%06x, want 0xFF8000", runs[0].F)
	}
}

// TestWireRunAllAttributes verifies each attribute bit position per spec.
func TestWireRunAllAttributes(t *testing.T) {
	tests := []struct {
		seq  string
		name string
		bit  uint16
	}{
		{seq: "\x1b[1m", bit: 1, name: "bold"},
		{seq: "\x1b[3m", bit: 2, name: "italic"},
		{seq: "\x1b[4m", bit: 4, name: "underline"},
		{seq: "\x1b[7m", bit: 8, name: "inverse"},
		{seq: "\x1b[9m", bit: 16, name: "strikethrough"},
		{seq: "\x1b[2m", bit: 32, name: "dim"},
		{seq: "\x1b[8m", bit: 64, name: "hidden"},
		{seq: "\x1b[5m", bit: 128, name: "blink"},
		{seq: "\x1b[6m", bit: 128, name: "rapid-blink"},
		{seq: "\x1b[53m", bit: 256, name: "overline"},
		{seq: "\x1b[21m", bit: 512, name: "double-underline"},
	}
	for _, tc := range tests {
		s := New(1, 5)
		s.Write([]byte(tc.seq + "X\x1b[0m"))
		runs := s.RenderRowWire(0)
		if len(runs) == 0 {
			t.Errorf("%s: no runs", tc.name)
			continue
		}
		if runs[0].A&tc.bit == 0 {
			t.Errorf("%s: bit %d not set in A=%d", tc.name, tc.bit, runs[0].A)
		}
	}
}

// TestRenderRowWireRejectsOutOfRangeRow verifies the row-bounds guard:
// a row index equal to Height returns nil, while the last valid row is non-nil.
// The last row is given content first, because a row of nothing but padding
// legitimately encodes to zero runs now (trimTrailingBlanks) — so an empty
// screen could not tell the bounds guard apart from the trim.
func TestRenderRowWireRejectsOutOfRangeRow(t *testing.T) {
	s := New(3, 8) // valid rows 0..2
	s.Write([]byte("\x1b[3;1Hx"))
	if got := s.RenderRowWire(s.Height); got != nil {
		t.Errorf("RenderRowWire(Height=%d) = %v, want nil", s.Height, got)
	}
	if got := s.RenderRowWire(s.Height - 1); got == nil {
		t.Errorf("RenderRowWire(Height-1=%d) = nil, want non-nil", s.Height-1)
	}
}

// TestBasic16RGBPaletteBounds verifies the 16-entry palette lookup: in-range
// indices map to their colors, and an out-of-range index returns the gray
// fallback rather than panicking.
func TestBasic16RGBPaletteBounds(t *testing.T) {
	if got := basic16RGB(0); got != 0x000000 {
		t.Errorf("basic16RGB(0) = 0x%06x, want 0x000000", got)
	}
	if got := basic16RGB(15); got != 0xffffff {
		t.Errorf("basic16RGB(15) = 0x%06x, want 0xffffff", got)
	}
	if got := basic16RGB(16); got != 0xaaaaaa {
		t.Errorf("basic16RGB(16) = 0x%06x, want 0xaaaaaa (out-of-range fallback)", got)
	}
}

// wireRowText concatenates a wire row's run text, i.e. the row as the client
// sees it: one rune per cell column, U+FFFF for a wide glyph's second half.
func wireRowText(runs []WireRun) string {
	var text strings.Builder
	for _, run := range runs {
		text.WriteString(run.T)
	}
	return text.String()
}

// TestRenderRowWireWidePlaceholder verifies a wide character is followed by a
// U+FFFF spacer placeholder in the wire text, and that one rune still maps to
// one cell column.
func TestRenderRowWireWidePlaceholder(t *testing.T) {
	s := New(3, 10)
	s.Write([]byte("A漢B"))
	got := wireRowText(s.RenderRowWire(0))
	// Exact, not Contains: the row's six trailing padding cells no longer reach
	// the wire (trimTrailingBlanks), so this pins both the placeholder and the
	// trim in one comparison.
	if got != "A漢\uFFFFB" {
		t.Errorf("wire row = %q, want %q", got, "A漢\uFFFFB")
	}
	// Column alignment is the property the placeholder exists for: rune index
	// == cell column, which is what the client's caret positioner (glyphAt)
	// counts. Trailing padding no longer reaches the wire, so pin it on a row
	// whose LAST column is written — which also pins that MID-line blanks are
	// never trimmed.
	s2 := New(1, 10)
	s2.Write([]byte("A漢B\x1b[1;10Hz"))
	if got := wireRowText(s2.RenderRowWire(0)); got != "A漢\uFFFFB     z" {
		t.Errorf("wire row = %q, want %q (one rune per column, interior blanks kept)", got, "A漢\uFFFFB     z")
	}
}

// TestRenderRowWire256Color verifies the 256-color -> 0xRRGGBB wire conversion
// (color256RGB via colorToWire) for the <16 palette delegate, the 6x6x6 color
// cube, and the grayscale ramp, driven through the public SGR + RenderRowWire path.
func TestRenderRowWire256Color(t *testing.T) {
	cases := []struct {
		seq  string
		idx  int
		want int32
	}{
		{"\x1b[38;5;9mX\x1b[0m", 9, 0xff5555},     // <16: delegates to basic-16 palette
		{"\x1b[38;5;21mX\x1b[0m", 21, 0x0000ff},   // cube: pure blue
		{"\x1b[38;5;46mX\x1b[0m", 46, 0x00ff00},   // cube: pure green
		{"\x1b[38;5;196mX\x1b[0m", 196, 0xff0000}, // cube: pure red
		{"\x1b[38;5;232mX\x1b[0m", 232, 0x080808}, // grayscale ramp: darkest
		{"\x1b[38;5;255mX\x1b[0m", 255, 0xeeeeee}, // grayscale ramp: lightest
	}
	for _, tc := range cases {
		s := New(1, 4)
		s.Write([]byte(tc.seq))
		runs := s.RenderRowWire(0)
		if len(runs) == 0 {
			t.Fatalf("256-color %d: no runs", tc.idx)
		}
		if runs[0].F != tc.want {
			t.Errorf("256-color %d -> F = 0x%06x, want 0x%06x", tc.idx, runs[0].F, tc.want)
		}
	}
}

// TestTrailingBlankTrim drives the trailing-blank trim through the public
// RenderRowWire path, one case per guard. The trim's whole value is that a grid
// pads every row to the full width, so a short line otherwise ships (and
// selects, and copies) dozens of spaces the application never printed; each
// guard below names the case where a trailing blank is CONTENT instead.
func TestTrailingBlankTrim(t *testing.T) {
	const width = 10
	cases := []struct {
		name  string
		write string
		want  string
	}{
		{
			// The app's OWN trailing space goes too: in a cell grid it is
			// indistinguishable from padding, which is the ambiguity xterm.js
			// and tmux already accept on their copy paths.
			name:  "padding is dropped",
			write: "$ ",
			want:  "$",
		},
		{
			name:  "interior blanks are kept",
			write: "a\x1b[1;5Hb",
			want:  "a   b",
		},
		{
			name:  "leading blanks are kept",
			write: "\x1b[1;4Hb",
			want:  "   b",
		},
		{
			name:  "a fully blank row encodes to nothing",
			write: "",
			want:  "",
		},
		{
			// An erase writes the application's current background into the
			// cells it clears, so this tail is a painted region: dropping it
			// would repaint it in the theme default.
			name:  "a colored tail is style-carrying",
			write: "\x1b[41mx\x1b[K",
			want:  "x         ",
		},
		{
			// Same erase, default background: nothing to preserve.
			name:  "an uncolored erase tail is padding",
			write: "x\x1b[K",
			want:  "x",
		},
		{
			// A blank inside the app's own OSC 8 anchor is part of the link.
			name:  "blanks inside an OSC 8 link are kept",
			write: "\x1b]8;;https://example.com\x07ab  \x1b]8;;\x07",
			want:  "ab  ",
		},
		{
			// U+FFFF is a wide glyph's second half, not a space: trimming it
			// would strand the glyph without its spacer cell.
			name:  "a wide glyph's continuation is not a blank",
			write: "\x1b[1;9H漢",
			want:  "        漢\uFFFF",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(2, width)
			s.Write([]byte(tc.write))
			runs := s.RenderRowWire(0)
			if got := wireRowText(runs); got != tc.want {
				t.Errorf("wire row = %q, want %q", got, tc.want)
			}
			if tc.want == "" && len(runs) != 0 {
				t.Errorf("blank row encoded to %d runs, want 0 (the client substitutes a filler span so the row keeps its line height)", len(runs))
			}
		})
	}
}

// TestTrailingBlankTrimSpareSoftWrappedRow is the xterm.js #1286 case, and the
// reason cellsToRuns needs the wrap flag at all: on a row that autowrapped onto
// the row below, the trailing blanks are mid-line content of ONE logical line —
// the application wrote them between "foo" and "bar" — so trimming them would
// join the two words on copy.
//
// The two cases write the same first row and differ only in whether the line
// continues, which is exactly the distinction the guard makes.
func TestTrailingBlankTrimSpareSoftWrappedRow(t *testing.T) {
	const width = 10
	full := "foo       " // "foo" + 7 spaces == exactly one row

	s := New(3, width)
	s.Write([]byte(full + "bar"))
	if !s.wrapped[1] {
		t.Fatalf("fixture: row 1 is not marked a soft-wrap continuation, so this test cannot exercise the guard")
	}
	if got := wireRowText(s.RenderRowWire(0)); got != full {
		t.Errorf("soft-wrapped row = %q, want %q (its trailing blanks are mid-line content)", got, full)
	}
	// The continuation row is itself the end of the logical line, so its own
	// padding IS padding.
	if got := wireRowText(s.RenderRowWire(1)); got != "bar" {
		t.Errorf("continuation row = %q, want %q", got, "bar")
	}

	plain := New(3, width)
	plain.Write([]byte(full))
	if plain.wrapped[1] {
		t.Fatalf("fixture: row 1 must not be a continuation when nothing was written after the full row")
	}
	if got := wireRowText(plain.RenderRowWire(0)); got != "foo" {
		t.Errorf("unwrapped row = %q, want %q (nothing follows, so the tail is padding)", got, "foo")
	}
}

// TestTrailingBlankTrimOnDrainedRows pins the trim on the OTHER cellsToRuns
// caller: rows drained into scrollback. A row is encoded once, when it scrolls
// off, so a missing trim there would ship padding for the whole history — the
// bulk of a paging client's transfer.
func TestTrailingBlankTrimOnDrainedRows(t *testing.T) {
	const width = 10
	s := New(2, width)
	// Four lines through a 2-row screen: "one" and "two" drain, the rest stay.
	s.Write([]byte("one\r\ntwo\r\nthree\r\nfour"))
	if len(s.Drained) != 2 {
		t.Fatalf("drained %d rows, want 2", len(s.Drained))
	}
	for i, want := range []string{"one", "two"} {
		if got := wireRowText(s.Drained[i]); got != want {
			t.Errorf("drained row %d = %q, want %q", i, got, want)
		}
	}

	// Same guard as the live screen: a row that soft-wrapped onto the row below
	// keeps its tail when it drains.
	w := New(2, width)
	w.Write([]byte("foo       bar\r\nnext\r\nlast"))
	if len(w.Drained) == 0 {
		t.Fatalf("nothing drained")
	}
	if got := wireRowText(w.Drained[0]); got != "foo       " {
		t.Errorf("drained soft-wrapped row = %q, want %q", got, "foo       ")
	}
}
