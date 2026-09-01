package vt

import "testing"

func TestOSC8SetsHyperlinkBEL(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b]8;;http://example.com\x07"))
	s.Write([]byte("link"))
	for i := range 4 {
		if s.Cells[0][i].Hyperlink != "http://example.com" {
			t.Fatalf("cell[0][%d] hyperlink = %q, want %q", i, s.Cells[0][i].Hyperlink, "http://example.com")
		}
	}
}

func TestOSC8SetsHyperlinkST(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b]8;;http://example.com\x1b\\"))
	s.Write([]byte("hi"))
	for i := range 2 {
		if s.Cells[0][i].Hyperlink != "http://example.com" {
			t.Fatalf("cell[0][%d] hyperlink = %q, want %q", i, s.Cells[0][i].Hyperlink, "http://example.com")
		}
	}
}

func TestOSC8ClearsOnEmptyURI(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b]8;;http://example.com\x07"))
	s.Write([]byte("AB"))
	s.Write([]byte("\x1b]8;;\x07"))
	s.Write([]byte("CD"))
	// AB should have the link
	for i := range 2 {
		if s.Cells[0][i].Hyperlink != "http://example.com" {
			t.Fatalf("cell[0][%d] hyperlink = %q, want link", i, s.Cells[0][i].Hyperlink)
		}
	}
	// CD should not
	for i := 2; i < 4; i++ {
		if s.Cells[0][i].Hyperlink != "" {
			t.Fatalf("cell[0][%d] hyperlink = %q, want empty", i, s.Cells[0][i].Hyperlink)
		}
	}
}

func TestOSC8WithIdParam(t *testing.T) {
	s := New(24, 80)
	// id= param is parsed but not used; URI still attaches
	s.Write([]byte("\x1b]8;id=foo;http://example.com\x07"))
	s.Write([]byte("X"))
	if s.Cells[0][0].Hyperlink != "http://example.com" {
		t.Fatalf("cell hyperlink = %q, want %q", s.Cells[0][0].Hyperlink, "http://example.com")
	}
}

func TestOSC8RunsSplitOnURLBoundary(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b]8;;http://a.com\x07"))
	s.Write([]byte("AA"))
	s.Write([]byte("\x1b]8;;http://b.com\x07"))
	s.Write([]byte("BB"))
	s.Write([]byte("\x1b]8;;\x07"))
	s.Write([]byte("CC"))

	runs := s.RenderRowWire(0)
	// Should have at least 3 runs: AA(url=a), BB(url=b), CC+rest(no url)
	if len(runs) < 3 {
		t.Fatalf("expected at least 3 runs, got %d", len(runs))
	}
	if runs[0].U != "http://a.com" {
		t.Fatalf("run[0].U = %q, want %q", runs[0].U, "http://a.com")
	}
	if runs[1].U != "http://b.com" {
		t.Fatalf("run[1].U = %q, want %q", runs[1].U, "http://b.com")
	}
	// The last run(s) should have no URL
	lastRun := runs[len(runs)-1]
	if lastRun.U != "" {
		t.Fatalf("last run U = %q, want empty", lastRun.U)
	}
}

// A malformed OSC 8 carries no URI field, so it is ignored and the pen keeps
// whatever link it holds. Matches ghostty, whose parser invalidates a payload
// missing the second separator; clearing instead would let one corrupt byte
// inside a URL end a link the application legitimately opened.
func TestOSC8MalformedNoSecondSemicolonIsIgnored(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b]8;http://example.com\x07"))
	s.Write([]byte("X"))
	if got := s.Cells[0][0].Hyperlink; got != "" {
		t.Fatalf("cell hyperlink = %q, want empty (malformed OSC 8 sets no link)", got)
	}

	s = New(24, 80)
	s.Write([]byte("\x1b]8;;http://a.com\x07AA"))
	s.Write([]byte("\x1b]8;\x1b\\")) // one semicolon: no URI field
	s.Write([]byte("BB"))
	if got := s.Cells[0][2].Hyperlink; got != "http://a.com" {
		t.Fatalf("cell after a malformed OSC 8 = %q, want the still-open link", got)
	}
}

// The hyperlink is a pen attribute, so DECSTR, RIS and a screen switch clear it
// while SGR 0 — which the OSC 8 contract keeps separate from the link — does not.
func TestOSC8PenClearedByResetsAndScreenSwitchButNotBySGR(t *testing.T) {
	for _, tc := range []struct {
		name, seq string
		want      string
	}{
		{"SGR 0", "\x1b[0m", "http://a.com"},
		{"ED 2", "\x1b[2J", "http://a.com"},
		{"DECSTR", "\x1b[!p", ""},
		{"RIS", "\x1bc", ""},
		{"alt screen enter 1049", "\x1b[?1049h", ""},
		{"alt screen enter 47", "\x1b[?47h", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(24, 80)
			s.Write([]byte("\x1b]8;;http://a.com\x07A"))
			if s.hyperlink != "http://a.com" {
				t.Fatalf("setup: pen = %q, want the opened link", s.hyperlink)
			}
			s.Write([]byte(tc.seq))
			if s.hyperlink != tc.want {
				t.Errorf("pen after %s = %q, want %q", tc.name, s.hyperlink, tc.want)
			}
			s.Write([]byte("B"))
			// RIS and a screen switch move the cursor, so read where the write landed.
			cell := s.Cells[s.curY][max(0, s.curX-1)]
			if cell.Hyperlink != tc.want {
				t.Errorf("cell written after %s carries hyperlink %q, want %q", tc.name, cell.Hyperlink, tc.want)
			}
		})
	}
}

// Leaving the alt screen clears the pen too: the link belongs to the screen
// being left, and its close may have been written there.
func TestOSC8PenClearedOnAltScreenExit(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[?1049h"))                 // to alt
	s.Write([]byte("\x1b]8;;http://alt.com\x07A")) // open a link on the alt screen
	s.Write([]byte("\x1b[?1049l"))                 // back to main
	if s.hyperlink != "" {
		t.Errorf("pen after leaving the alt screen = %q, want empty", s.hyperlink)
	}
	s.Write([]byte("B"))
	if got := s.Cells[s.curY][max(0, s.curX-1)].Hyperlink; got != "" {
		t.Errorf("cell written on the main screen carries hyperlink %q, want empty", got)
	}
}
