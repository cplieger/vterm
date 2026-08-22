package vt

import (
	"strings"
	"testing"
)

// collectLinks gathers (text, url) for every autolink-stamped run in a row's
// wire output, plus the concatenated stamped text for whole-URL assertions.
func collectLinks(runs []WireRun) (stamped []WireRun, joined string) {
	for _, r := range runs {
		if r.A&AttrAutolink != 0 {
			stamped = append(stamped, r)
			joined += r.T
		}
	}
	return stamped, joined
}

func TestAutolinkSingleRow(t *testing.T) {
	s := New(5, 40)
	if _, err := s.Write([]byte("see https://ex.com/a?b=1 end")); err != nil {
		t.Fatalf("write: %v", err)
	}
	runs := s.RenderRowWire(0)
	stamped, joined := collectLinks(runs)
	if len(stamped) == 0 {
		t.Fatal("no autolink stamped on a bare URL")
	}
	if joined != "https://ex.com/a?b=1" {
		t.Errorf("stamped text = %q, want the exact URL", joined)
	}
	for _, r := range stamped {
		if r.U != "https://ex.com/a?b=1" {
			t.Errorf("stamped run URL = %q, want full URL", r.U)
		}
	}
	// Surrounding text is not stamped.
	for _, r := range runs {
		if r.A&AttrAutolink == 0 && strings.Contains(r.T, "https") {
			t.Errorf("URL text left unstamped in run %q", r.T)
		}
		if r.A&AttrAutolink != 0 && (strings.Contains(r.T, "see") || strings.Contains(r.T, "end")) {
			t.Errorf("non-URL text stamped in run %q", r.T)
		}
	}
}

// TestAutolinkWrappedRow pins the phone-width regression: a URL that soft-wraps
// onto a second row must yield anchors on BOTH rows, each carrying the FULL
// href — the old per-row client regex left row 2 unlinked and row 1's href
// truncated at the wrap column (a broken tap on narrow screens).
func TestAutolinkWrappedRow(t *testing.T) {
	const url = "https://amzn.awsapps.com/start/#/device?user_code=ABCD-EFGH"
	s := New(5, 40) // 60-char URL wraps at col 40 after the 15-char prefix
	if _, err := s.Write([]byte("Open this URL: " + url)); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, joined0 := collectLinks(s.RenderRowWire(0))
	stamped1, joined1 := collectLinks(s.RenderRowWire(1))
	if joined0+joined1 != url {
		t.Fatalf("stamped text across rows = %q + %q, want the exact URL %q", joined0, joined1, url)
	}
	if len(stamped1) == 0 {
		t.Fatal("second (wrapped) row has no autolink — the reported phone bug")
	}
	for _, r := range append(append([]WireRun(nil), stamped1...), stamped1...) {
		if r.U != url {
			t.Errorf("wrapped-row run href = %q, want the FULL url", r.U)
		}
	}
	for _, r := range collectFirst(s.RenderRowWire(0)) {
		if r.U != url {
			t.Errorf("first-row run href = %q, want the FULL url (was truncated at the wrap column before)", r.U)
		}
	}
}

func collectFirst(runs []WireRun) []WireRun {
	out, _ := collectLinks(runs)
	return out
}

// TestAutolinkStampsEveryURLInARow: a row carrying two bare URLs gets an anchor
// for each, with its own href — a shell printing two links on one line is
// ordinary output, and stamping only the first would leave the second plain.
func TestAutolinkStampsEveryURLInARow(t *testing.T) {
	const (
		first  = "https://a.example/1"
		second = "https://b.example/2"
	)
	s := New(3, 60)
	if _, err := s.Write([]byte(first + " and " + second)); err != nil {
		t.Fatalf("write: %v", err)
	}
	stamped, joined := collectLinks(s.RenderRowWire(0))
	if joined != first+second {
		t.Errorf("stamped text = %q, want both URLs %q", joined, first+second)
	}
	if len(stamped) == 0 {
		t.Fatalf("RenderRowWire(0) stamped nothing on a row with two URLs")
	}
	if got := stamped[0].U; got != first {
		t.Errorf("first anchor href = %q, want %q", got, first)
	}
	if got := stamped[len(stamped)-1].U; got != second {
		t.Errorf("last anchor href = %q, want %q", got, second)
	}
}

// TestAutolinkAnchorStopsAtTheURLInsideAStyledRow: a URL that begins partway
// into a run must be anchored exactly, with the styled text before it and the
// plain text after it left alone. The run has to be split in three, and each
// piece keeps the run's own colors — a red "ERROR:" prefix stays red.
func TestAutolinkAnchorStopsAtTheURLInsideAStyledRow(t *testing.T) {
	const url = "https://ex.co"
	s := New(3, 60)
	if _, err := s.Write([]byte("\x1b[31mERROR: connection refused\x1b[0m " + url + " and retry")); err != nil {
		t.Fatalf("write: %v", err)
	}
	runs := s.RenderRowWire(0)
	stamped, joined := collectLinks(runs)
	if joined != url {
		t.Errorf("stamped text = %q, want exactly the URL %q", joined, url)
	}
	for _, r := range stamped {
		if r.U != url {
			t.Errorf("anchor href = %q, want %q", r.U, url)
		}
	}
	if len(runs) == 0 {
		t.Fatalf("RenderRowWire(0) = no runs")
	}
	// The styled prefix is a separate run and keeps its color.
	if got := runs[0].T; got != "ERROR: connection refused" {
		t.Errorf("run[0].T = %q, want %q", got, "ERROR: connection refused")
	}
	if got := runs[0].F; got != 0xcc0403 {
		t.Errorf("run[0].F = 0x%06x, want 0xcc0403 (the prefix stays red)", got)
	}
	if runs[0].A&AttrAutolink != 0 {
		t.Errorf("run[0] %q gained the autolink bit; only the URL is an anchor", runs[0].T)
	}
	if got := runs[len(runs)-1].T; got != " and retry" {
		t.Errorf("last run.T = %q, want %q (the text after the URL is not part of the anchor)", got, " and retry")
	}
}

// TestAutolinkAnchorStopsAtTheURLOnAWrappedRow is the same boundary on the far
// side of a soft wrap: the continuation row's anchor covers the rest of the URL
// and nothing beyond it, even though the match's offsets are measured in the
// joined chain rather than in the row.
func TestAutolinkAnchorStopsAtTheURLOnAWrappedRow(t *testing.T) {
	const url = "https://ex.com/aaaaaaaaaa" // 25 chars: wraps at 20 cols
	s := New(4, 20)
	if _, err := s.Write([]byte(url + " tail")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !s.wrapped[1] {
		t.Fatalf("fixture: row 1 is not a soft-wrap continuation, so the case under test did not arise")
	}
	_, joined0 := collectLinks(s.RenderRowWire(0))
	if joined0 != url[:20] {
		t.Errorf("row 0 stamped %q, want %q", joined0, url[:20])
	}
	stamped1, joined1 := collectLinks(s.RenderRowWire(1))
	if joined1 != url[20:] {
		t.Errorf("row 1 stamped %q, want %q (the anchor stops where the URL does)", joined1, url[20:])
	}
	for _, r := range stamped1 {
		if r.U != url {
			t.Errorf("row 1 anchor href = %q, want the FULL url %q", r.U, url)
		}
	}
	runs := s.RenderRowWire(1)
	if len(runs) == 0 {
		t.Fatalf("RenderRowWire(1) = no runs")
	}
	if got := runs[len(runs)-1].T; got != " tail" {
		t.Errorf("last run.T = %q, want %q (text after the URL is not part of the anchor)", got, " tail")
	}
}

// TestAutolinkScanWindowFollowsTheRenderedRow pins the bound on the URL scan: at
// most maxAutolinkRows rows of a wrap chain are joined, and the window is the
// rows NEAREST the one being rendered, so on a chain longer than the window
// different rows see different amounts of the URL. The fixture is a chain of
// five 20-column rows — a prefix line that wraps, then a 65-char URL across the
// remaining four — so the early rows' window still holds the prefix and stops
// short of the URL's tail, while the later rows' window has slid off the prefix
// and holds the whole URL. The bound is what keeps per-row rendering work
// constant; four rows cover any real URL at phone widths.
//
// Row 3 also pins the tie: it sits equidistant from both ends of the five-row
// chain, and the window keeps the OLDER end there (the URL's scheme lives at the
// chain's start, and a window without it yields no anchor at all — boundChain),
// so its href is the truncated one rather than the full URL.
func TestAutolinkScanWindowFollowsTheRenderedRow(t *testing.T) {
	const (
		prefix = "look at this link:  " // exactly 20 columns, so it soft-wraps
		url    = "https://ex.com/aaaaaaaaaa/bbbbbbbbbb/cccccccccc/dddddddddd/eeeeee"
	)
	if len(prefix) != 20 || len(url) != 65 {
		t.Fatalf("fixture: prefix is %d columns and url is %d chars, want 20 and 65", len(prefix), len(url))
	}
	s := New(7, 20)
	// An unrelated first line, then one logical line wrapping across rows 1..5.
	if _, err := s.Write([]byte("x\r\n" + prefix + url)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if s.wrapped[1] || !s.wrapped[5] {
		t.Fatalf("fixture: wrap flags are %v, want row 1 opening the chain and row 5 continuing it", s.wrapped)
	}

	cases := []struct {
		name    string
		row     int
		want    string // stamped text on that row
		wantURL string // href on that row, "" when the row must carry no anchor
	}{
		{name: "prefix_row", row: 1, want: "", wantURL: ""},
		{name: "url_starts_here", row: 2, want: url[:20], wantURL: url[:60]},
		{name: "window_still_holds_the_prefix", row: 3, want: url[20:40], wantURL: url[:60]},
		{name: "window_has_slid_off_the_prefix", row: 4, want: url[40:60], wantURL: url},
		{name: "chain_end", row: 5, want: url[60:], wantURL: url},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stamped, joined := collectLinks(s.RenderRowWire(tc.row))
			if joined != tc.want {
				t.Errorf("RenderRowWire(%d) stamped %q, want %q", tc.row, joined, tc.want)
			}
			if tc.wantURL == "" {
				if len(stamped) != 0 {
					t.Errorf("RenderRowWire(%d) stamped %d run(s), want none", tc.row, len(stamped))
				}
				return
			}
			if len(stamped) == 0 {
				t.Fatalf("RenderRowWire(%d) stamped nothing, want href %q", tc.row, tc.wantURL)
			}
			for _, r := range stamped {
				if r.U != tc.wantURL {
					t.Errorf("RenderRowWire(%d) anchor href = %q, want %q", tc.row, r.U, tc.wantURL)
				}
			}
		})
	}
}

// TestAutolinkHardNewlineDoesNotJoin: a hard newline is not a wrap; two
// adjacent rows must not be joined even when row texts abut URL-ishly.
func TestAutolinkHardNewlineDoesNotJoin(t *testing.T) {
	s := New(5, 40)
	if _, err := s.Write([]byte("https://ex.com/aaa\r\nbbb/ccc")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, joined0 := collectLinks(s.RenderRowWire(0))
	stamped1, _ := collectLinks(s.RenderRowWire(1))
	if joined0 != "https://ex.com/aaa" {
		t.Errorf("row 0 stamped %q, want just its own URL", joined0)
	}
	if len(stamped1) != 0 {
		t.Errorf("row 1 (after hard newline) stamped %v, want none", stamped1)
	}
}

// TestAutolinkAcrossDrainBoundary: a wrapped URL whose first row scrolls into
// history keeps full-href stamps on BOTH the drained lines (via the retained
// drain tail) and the still-live rows.
func TestAutolinkAcrossDrainBoundary(t *testing.T) {
	const url = "https://ex.com/aaaaaaaaaa/bbbbbbbbbb" // 36 chars: wraps at 20 cols
	s := New(2, 20)
	if _, err := s.Write([]byte(url)); err != nil {
		t.Fatalf("write url: %v", err)
	}
	// Both halves live: full URL on each row.
	_, j0 := collectLinks(s.RenderRowWire(0))
	_, j1 := collectLinks(s.RenderRowWire(1))
	if j0+j1 != url {
		t.Fatalf("live stamps = %q + %q, want %q", j0, j1, url)
	}

	// Scroll the URL into history line by line; each drained line must carry
	// the full-href stamp (the second via the drain tail).
	if _, err := s.Write([]byte("\r\nmore\r\nrest")); err != nil {
		t.Fatalf("write filler: %v", err)
	}
	drained := s.DrainScrollback()
	if len(drained) < 2 {
		t.Fatalf("drained %d lines, want >= 2", len(drained))
	}
	for i, line := range drained[:2] {
		stamped, joined := collectLinks(line)
		if len(stamped) == 0 {
			t.Fatalf("drained line %d has no autolink stamp", i)
		}
		for _, r := range stamped {
			if r.U != url {
				t.Errorf("drained line %d href = %q, want full url", i, r.U)
			}
		}
		if i == 0 && !strings.HasPrefix(url, joined) {
			t.Errorf("drained line 0 stamped %q, want a prefix of the url", joined)
		}
	}
}

// TestAutolinkOSC8Authoritative: an app-provided OSC 8 hyperlink is never
// overwritten or re-flagged by the autolinker, even when its visible text
// looks like a different URL.
func TestAutolinkOSC8Authoritative(t *testing.T) {
	s := New(5, 60)
	if _, err := s.Write([]byte("\x1b]8;;https://app.example/target\x1b\\https://visible.example/x\x1b]8;;\x1b\\")); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, r := range s.RenderRowWire(0) {
		if strings.Contains(r.T, "visible") {
			if r.U != "https://app.example/target" {
				t.Errorf("OSC 8 href = %q, want the app-provided target", r.U)
			}
			if r.A&AttrAutolink != 0 {
				t.Errorf("OSC 8 run gained the autolink bit; app links must stay hover-styled")
			}
		}
	}
}

// TestAutolinkED2ClearsChains: a full-screen erase severs wrap chains, so a
// later render must not join rows through pre-erase flags.
func TestAutolinkED2ClearsChains(t *testing.T) {
	s := New(5, 20)
	if _, err := s.Write([]byte("https://ex.com/aaaaaaaaaa")); err != nil { // wraps
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Write([]byte("\x1b[2J\x1b[H")); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if _, err := s.Write([]byte("tail/path")); err != nil { // row 0, no scheme
		t.Fatalf("write2: %v", err)
	}
	stamped, _ := collectLinks(s.RenderRowWire(0))
	if len(stamped) != 0 {
		t.Errorf("post-ED2 row stamped %v, want none (chain must be severed)", stamped)
	}
}

// TestAutolinkScrollRegionShiftKeepsChain: a wrapped pair shifted up by a
// full-width scroll region keeps its chain (flags travel with row identity).
func TestAutolinkScrollRegionShiftKeepsChain(t *testing.T) {
	const url = "https://ex.com/aaaaaaaaaa" // wraps at 20 cols
	s := New(4, 20)
	if _, err := s.Write([]byte(url + "\r\nx\r\ny")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Scroll the full screen up one line via CSI S: URL pair moves to rows -1/0…
	// row 0 drains; the pair is now history[0] + row 0.
	if _, err := s.Write([]byte("\x1b[S")); err != nil {
		t.Fatalf("scroll: %v", err)
	}
	_, j := collectLinks(s.RenderRowWire(0))
	if j != url[20:] {
		t.Errorf("post-scroll continuation row stamped %q, want %q", j, url[20:])
	}
	drained := s.DrainScrollback()
	if len(drained) == 0 {
		t.Fatal("expected a drained line from the region scroll")
	}
	stamped, _ := collectLinks(drained[len(drained)-1])
	for _, r := range stamped {
		if r.U != url {
			t.Errorf("drained first half href = %q, want full url", r.U)
		}
	}
}

// TestAutolinkUppercaseParity mirrors the client regex: HTTPS:// matches,
// mixed-case Https:// does not.
func TestAutolinkUppercaseParity(t *testing.T) {
	s := New(5, 60)
	if _, err := s.Write([]byte("HTTPS://EX.COM/A and Https://ex.com/b")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, joined := collectLinks(s.RenderRowWire(0))
	if !strings.Contains(joined, "HTTPS://EX.COM/A") {
		t.Errorf("uppercase URL not stamped; joined = %q", joined)
	}
	if strings.Contains(joined, "Https") {
		t.Errorf("mixed-case scheme stamped; client regex parity broken: %q", joined)
	}
}

// TestAutolinkBoxedMarginWrapNotChained: a wrap inside a DECSLRM left/right
// margin box continues box content, not the row, so no chain is recorded.
func TestAutolinkBoxedMarginWrapNotChained(t *testing.T) {
	s := New(5, 40)
	// Enable DECLRMM and set margins 10..29, then print a URL that wraps
	// within the box.
	if _, err := s.Write([]byte("\x1b[?69h\x1b[11;30s\x1b[1;11Hhttps://ex.com/aaaaaaaaaaaaaaaaaa")); err != nil {
		t.Fatalf("write: %v", err)
	}
	stamped, _ := collectLinks(s.RenderRowWire(1))
	for _, r := range stamped {
		if strings.HasPrefix(r.U, "https://ex.com/") && len(r.U) > 34 {
			t.Errorf("boxed wrap joined rows into %q; margin wraps must not chain", r.U)
		}
	}
}

// TestAutolinkWideCharBoundary: a wide glyph adjacent to a URL must terminate
// the match (its continuation placeholder is not a URL character).
func TestAutolinkWideCharBoundary(t *testing.T) {
	s := New(5, 40)
	if _, err := s.Write([]byte("https://ex.com/a\u4e16 tail")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, joined := collectLinks(s.RenderRowWire(0))
	if joined != "https://ex.com/a" {
		t.Errorf("stamped %q, want the URL to stop before the wide glyph", joined)
	}
}

// hardWrapLines writes each line as its own logical line (CR LF between them),
// which is how an Ink-style TUI emits text it wrapped itself: the terminal never
// autowraps, so no soft-wrap flag is recorded.
func hardWrapLines(t *testing.T, s *Screen, lines ...string) {
	t.Helper()
	if _, err := s.Write([]byte(strings.Join(lines, "\r\n"))); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestAutolinkAppHardWrapJoined pins the reported phone bug, measured live in
// web-terminal-kiro at 41 columns: kiro-cli wraps a long URL itself, re-emitting
// an 8-column indent on each continuation, so the terminal sees three separate
// full-width lines. Every row must carry an anchor with the WHOLE href — before
// the indent-aware join, row 0 got an href truncated at the break
// ("https://github.com/cplieger/.kiro", which silently opened the repo instead
// of the job) and rows 1 and 2 got no anchor at all, hence no underline.
func TestAutolinkAppHardWrapJoined(t *testing.T) {
	const url = "https://github.com/cplieger/.kiro/actions/runs/31094602847/job/92593457519"
	s := New(6, 41)
	hardWrapLines(t, s,
		"        https://github.com/cplieger/.kiro", // 8 + 33 = 41, exactly full
		"        /actions/runs/31094602847/job/925", // 8 + 33 = 41, exactly full
		"        93457519",
	)
	var joined strings.Builder
	for y := range 3 {
		stamped, part := collectLinks(s.RenderRowWire(y))
		if len(stamped) == 0 {
			t.Errorf("row %d has no autolink; a hard-wrapped URL must be linked on every row", y)
		}
		for _, r := range stamped {
			if r.U != url {
				t.Errorf("row %d href = %q, want the FULL url %q", y, r.U, url)
			}
		}
		joined.WriteString(part)
	}
	if joined.String() != url {
		t.Errorf("stamped text across rows = %q, want exactly %q", joined.String(), url)
	}
	// The indent is layout, not link text: the anchor must start at column 8.
	for _, r := range s.RenderRowWire(1) {
		if r.A&AttrAutolink != 0 && strings.HasPrefix(r.T, " ") {
			t.Errorf("continuation indent stamped as link text in run %q", r.T)
		}
	}
}

// TestAutolinkHardWrapRejects covers the three refusals that keep the
// indent-aware join from gluing unrelated lines. Each case writes a first line
// whose URL would extend if the join fired, and asserts the href stops there and
// the following line is not linked.
func TestAutolinkHardWrapRejects(t *testing.T) {
	cases := map[string]struct {
		first, second, want string
	}{
		// A short first line means the break was the line ending, not the margin.
		"first line not full": {
			first:  "        https://ex.com/aaa",
			second: "        bbb/ccc",
			want:   "https://ex.com/aaa",
		},
		// Different indents are two independent lines that happen to be adjacent.
		"indent mismatch": {
			first:  "        https://ex.com/aaaaaaaaaaaaaaaaaa", // 8 + 33 = 41, exactly full
			second: "    bbb/ccc",
			want:   "https://ex.com/aaaaaaaaaaaaaaaaaa",
		},
		// Two unindented full lines are indistinguishable from table or ls output.
		"no indent": {
			first:  "https://ex.com/aaaaaaaaaaaaaaaaaaaaaaaaaa", // 41, exactly full
			second: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			want:   "https://ex.com/aaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		// Nothing follows on the next row at all: an all-blank row shares no
		// indent with the line above, so it continues nothing.
		"next row blank": {
			first:  "        https://ex.com/aaaaaaaaaaaaaaaaaa", // 8 + 33 = 41, exactly full
			second: "",
			want:   "https://ex.com/aaaaaaaaaaaaaaaaaa",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := New(6, 41)
			hardWrapLines(t, s, tc.first, tc.second)
			_, joined0 := collectLinks(s.RenderRowWire(0))
			stamped1, joined1 := collectLinks(s.RenderRowWire(1))
			if tc.want != "" && joined0 != tc.want {
				t.Errorf("row 0 stamped %q, want %q", joined0, tc.want)
			}
			if len(stamped1) != 0 {
				t.Errorf("row 1 stamped %q; these lines must not join", joined1)
			}
		})
	}
}

// TestAutolinkSoftWrapIndentIsContent: on a SOFT-wrapped row the leading blanks
// are text the application wrote, not a continuation indent, so they must stay
// in the join and terminate the match. Dropping them (checking the hard-wrap
// shape before the wrapped flag) would glue "…/ab" to "tail" and produce an href
// nobody typed.
func TestAutolinkSoftWrapIndentIsContent(t *testing.T) {
	s := New(4, 20)
	// 20 chars exactly fill row 0 (3-space indent + a 17-char URL), then the
	// autowrap carries "   tail" onto row 1 with the same leading blank run.
	if _, err := s.Write([]byte("   https://ex.com/ab" + "   tail")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !s.wrapped[1] {
		t.Fatal("row 1 is not marked as a soft wrap; the case under test did not arise")
	}
	_, joined0 := collectLinks(s.RenderRowWire(0))
	if joined0 != "https://ex.com/ab" {
		t.Errorf("row 0 href = %q, want the URL to stop at the space it wrapped onto", joined0)
	}
	stamped1, joined1 := collectLinks(s.RenderRowWire(1))
	if len(stamped1) != 0 {
		t.Errorf("row 1 stamped %q, want none: its content is whitespace then plain text", joined1)
	}
}
