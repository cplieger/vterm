package vt

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

func TestRuneWidth(t *testing.T) {
	tests := []struct {
		name string
		r    rune
		want int
	}{
		{"ASCII a", 'a', 1},
		{"CJK ideograph", '中', 2},
		{"Fullwidth A", 'Ａ', 2},
		{"Hangul syllable", '한', 2},
		{"Combining acute", '\u0301', 0},
		{"Combining diaeresis", '\u0308', 0},
		{"Zero-width space", '\u200B', 0},
		{"Ambiguous (degree)", '°', 1},
		{"CJK Ext B", 0x20000, 2},
		{"Latin space", ' ', 1},
		{"IDEOGRAPHIC HALF FILL SPACE", '\u303F', 1},
		// C1 zero-width band [0x7F,0xA0): DEL and the interior are width 0,
		// the byte just below (0x7E) and NBSP (0xA0, the first byte above) are 1.
		{"DEL is zero-width", 0x7F, 0},
		{"just below DEL stays width 1", 0x7E, 1},
		{"C1 band interior", 0x9F, 0},
		{"NBSP is width 1", 0xA0, 1},
		// Combining set now comes from the stdlib (unicode.Mn/Me/Cf), current to
		// the toolchain's Unicode version, with SOFT HYPHEN carved back to 1.
		{"Soft hyphen is width 1 (Cf carve-out)", 0x00AD, 1},
		{"Post-Unicode-5.0 combining mark U+1AB0", 0x1AB0, 0},
		// Single-codepoint emoji with default emoji presentation are Wide (2),
		// matching go-runewidth and modern terminals. Text-presentation symbols
		// and regional-indicator flags stay width 1 (clustering is out of scope).
		{"Emoji green circle (reported)", 0x1F7E2, 2},
		{"Emoji grinning face", 0x1F600, 2},
		{"Emoji rocket", 0x1F680, 2},
		{"Emoji party popper", 0x1F389, 2},
		{"Emoji thumbs up", 0x1F44D, 2},
		{"Emoji white star (BMP)", 0x2B50, 2},
		{"Emoji check mark button (BMP)", 0x2705, 2},
		{"Emoji sparkles (BMP)", 0x2728, 2},
		{"Emoji hourglass (BMP)", 0x231B, 2},
		{"Text-presentation snowman stays width 1", 0x2603, 1},
		{"Text-presentation heart stays width 1", 0x2764, 1},
		{"Non-emoji dingbat stays width 1", 0x2700, 1},
		{"Regional indicator (flag) stays width 1", 0x1F1EA, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runeWidth(tt.r); got != tt.want {
				t.Errorf("runeWidth(%U) = %d, want %d", tt.r, got, tt.want)
			}
		})
	}
}

// TestInTableEndpointsMatch checks that inTable includes both endpoints of a
// range: the lower bound of the first wideRanges entry (U+1100) and the upper
// bound of the last (U+3FFFD) must both report as members.
func TestInTableEndpointsMatch(t *testing.T) {
	if !inTable(0x1100, wideRanges) {
		t.Errorf("inTable(0x1100, wideRanges) = false, want true (lower endpoint of first range)")
	}
	if !inTable(0x3FFFD, wideRanges) {
		t.Errorf("inTable(0x3FFFD, wideRanges) = false, want true (upper endpoint of last range)")
	}
}

func TestPutWideChar(t *testing.T) {
	s := New(3, 10)
	// Write a CJK character — should occupy 2 cells.
	s.Write([]byte("中"))
	if s.curX != 2 {
		t.Fatalf("curX after wide char = %d, want 2", s.curX)
	}
	if s.Cells[0][0].Ch != '中' {
		t.Fatalf("cell[0][0] = %U, want '中'", s.Cells[0][0].Ch)
	}
	// Spacer cell should be 0 (rendered as \uFFFF on wire).
	if s.Cells[0][1].Ch != 0 {
		t.Fatalf("cell[0][1] = %U, want 0 (spacer)", s.Cells[0][1].Ch)
	}
}

func TestPutCombiningMark(t *testing.T) {
	s := New(3, 10)
	// Write 'e' then combining acute accent — cursor should not advance.
	s.Write([]byte("e\xCC\x81")) // e + U+0301
	if s.curX != 1 {
		t.Fatalf("curX after combining = %d, want 1", s.curX)
	}
}

func TestWideCharAtRightEdgeWraps(t *testing.T) {
	s := New(3, 5)
	// Fill 4 cells with 'a', then write a wide char at col 4 (only 1 cell left).
	s.Write([]byte("aaaa"))
	if s.curX != 4 {
		t.Fatalf("curX after 4 a's = %d, want 4", s.curX)
	}
	s.Write([]byte("中"))
	// Wide char should wrap to next line.
	if s.curY != 1 {
		t.Fatalf("curY after wrap = %d, want 1", s.curY)
	}
	if s.curX != 2 {
		t.Fatalf("curX after wrap = %d, want 2", s.curX)
	}
	// The wide char should be at row 1, col 0.
	if s.Cells[1][0].Ch != '中' {
		t.Fatalf("cell[1][0] = %U, want '中'", s.Cells[1][0].Ch)
	}
	// The last cell of row 0 should be a space (dead cell from wrap).
	if s.Cells[0][4].Ch != ' ' {
		t.Fatalf("cell[0][4] = %U, want ' ' (dead cell)", s.Cells[0][4].Ch)
	}
}

func TestWideCharWireRuns(t *testing.T) {
	s := New(1, 6)
	s.Write([]byte("中a"))
	runs := s.RenderRowWire(0)
	// All same style → single run. Text should be: 中 + \uFFFF + a + spaces.
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	runes := []rune(runs[0].T)
	if runes[0] != '中' {
		t.Errorf("rune[0] = %U, want '中'", runes[0])
	}
	if runes[1] != '\uFFFF' {
		t.Errorf("rune[1] = %U, want \\uFFFF (spacer)", runes[1])
	}
	if runes[2] != 'a' {
		t.Errorf("rune[2] = %U, want 'a'", runes[2])
	}
}

// TestPutWideEmoji verifies a single-codepoint emoji occupies two cells (glyph
// + spacer) like a CJK wide char — the fix for emoji clipping into the next cell.
func TestPutWideEmoji(t *testing.T) {
	s := New(3, 10)
	s.Write([]byte("🟢")) // U+1F7E2, emoji presentation → 2 cells
	t.Run("cursor advances two cells", func(t *testing.T) {
		if s.curX != 2 {
			t.Errorf("curX after wide emoji = %d, want 2", s.curX)
		}
	})
	t.Run("glyph occupies the first cell", func(t *testing.T) {
		if s.Cells[0][0].Ch != 0x1F7E2 {
			t.Errorf("cell[0][0] = %U, want U+1F7E2", s.Cells[0][0].Ch)
		}
	})
	t.Run("second cell is the spacer", func(t *testing.T) {
		if s.Cells[0][1].Ch != 0 {
			t.Errorf("cell[0][1] = %U, want 0 (spacer)", s.Cells[0][1].Ch)
		}
	})
}

// TestEmojiRangesSortedNonOverlapping guards inTable's binary-search invariant:
// emojiRanges must be ascending, non-overlapping, and each range non-inverted. A
// transcription slip breaking the ordering would silently misclassify some emoji.
func TestEmojiRangesSortedNonOverlapping(t *testing.T) {
	for i, iv := range emojiRanges {
		if iv.first > iv.last {
			t.Errorf("emojiRanges[%d] inverted: {%U, %U}", i, iv.first, iv.last)
		}
		if i > 0 && iv.first <= emojiRanges[i-1].last {
			t.Errorf("emojiRanges[%d] {%U, %U} not strictly after emojiRanges[%d] {%U, %U}",
				i, iv.first, iv.last, i-1, emojiRanges[i-1].first, emojiRanges[i-1].last)
		}
	}
}

// TestWideRangesSortedNonOverlapping guards the same binary-search invariant
// for wideRanges, which is generated (scripts/gen-width-tables.py) as exact
// coalesced UAX#11 W/F intervals: ascending, non-overlapping, non-inverted.
func TestWideRangesSortedNonOverlapping(t *testing.T) {
	for i, iv := range wideRanges {
		if iv.first > iv.last {
			t.Errorf("wideRanges[%d] inverted: {%U, %U}", i, iv.first, iv.last)
		}
		if i > 0 && iv.first <= wideRanges[i-1].last {
			t.Errorf("wideRanges[%d] {%U, %U} not strictly after wideRanges[%d] {%U, %U}",
				i, iv.first, iv.last, i-1, wideRanges[i-1].first, wideRanges[i-1].last)
		}
	}
}

// TestTablesNotBehindStdlibUnicode guards the one drift the generated tables
// cannot self-report. runeWidth answers from TWO Unicode sources: the
// generated wideRanges/emojiRanges (pinned to tablesUnicodeVersion by
// scripts/gen-width-tables.py) and the stdlib's own Mn/Me/Cf tables, which
// track whatever UCD release the toolchain ships. The tables LEADING the
// stdlib is the normal, harmless state — they are generated from the newest
// UCD long before a Go release picks it up, so a codepoint can be wide before
// it is categorized. The tables TRAILING the stdlib is not: the stdlib would
// then know a codepoint the width tables have never seen, and a rune assigned
// in the newer release could be classified zero-width by one half while the
// other half still treats it as unassigned-and-narrow.
//
// Nothing else notices, because the generator records its UCD release only in
// a comment. Failure here means one action: bump UNICODE_VERSION in
// scripts/gen-width-tables.py, re-run it, and re-run ./vt/... plus the
// display-conformance tiers.
func TestTablesNotBehindStdlibUnicode(t *testing.T) {
	tables, err := parseUnicodeVersion(tablesUnicodeVersion)
	if err != nil {
		t.Fatalf("tablesUnicodeVersion %q: %v", tablesUnicodeVersion, err)
	}
	stdlib, err := parseUnicodeVersion(unicode.Version)
	if err != nil {
		t.Fatalf("unicode.Version %q: %v", unicode.Version, err)
	}
	if slices.Compare(tables[:], stdlib[:]) < 0 {
		t.Errorf("generated width tables are at UCD %s but the toolchain's unicode package is at %s: "+
			"the tables must never trail the stdlib. Bump UNICODE_VERSION in "+
			"scripts/gen-width-tables.py, re-run it, then re-run ./vt/... and the "+
			"display-conformance tiers.", tablesUnicodeVersion, unicode.Version)
	}
}

// parseUnicodeVersion splits a "major.minor.patch" UCD version into comparable
// components. Both inputs are compile-time constants from trusted sources, so
// a parse failure is a bug in one of them, not untrusted input.
func parseUnicodeVersion(v string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != len(out) {
		return out, fmt.Errorf("want 3 dot-separated components, got %d", len(parts))
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, fmt.Errorf("component %d (%q): %w", i, p, err)
		}
		out[i] = n
	}
	return out, nil
}
