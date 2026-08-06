package vt

import (
	"regexp"
	"strings"
)

// WireRun is a contiguous run of cells with the same style.
// Text is the run's content; FG/BG are 0xRRGGBB or -1 for default.
// Attr is a bit mask: 1=bold, 2=italic, 4=underline, 8=inverse,
// 16=strikethrough, 32=dim, 64=hidden, 128=blink, 256=overline,
// 512=double-underline, 1024=autolink (heuristic URL, see AttrAutolink).
type WireRun struct {
	// T is the text content of the run.
	T string `json:"t"`
	// U is the hyperlink URI (empty means no link): an app-provided OSC 8
	// link, or — when A carries AttrAutolink — the full URL of a
	// server-detected bare link, joined across soft-wrap continuations.
	U string `json:"u,omitempty"`
	// F is the foreground color as 0xRRGGBB, or -1 for default.
	F int32 `json:"f,omitempty"`
	// B is the background color as 0xRRGGBB, or -1 for default.
	B int32 `json:"b,omitempty"`
	// Uc is the underline color as 0xRRGGBB, or -1 for default.
	Uc int32 `json:"uc,omitempty"`
	// A is a bitmask of SGR attributes (bold=1, italic=2, underline=4, etc.).
	A uint16 `json:"a,omitempty"`
}

// Default flag for FG/BG meaning "use theme default".
const wireDefaultColor = int32(-1)

// AttrAutolink is the WireRun.A bit marking a HEURISTICALLY detected URL (the
// server-side autolinker below), as opposed to an app-provided OSC 8 link
// (which carries U with no bit). The client styles autolinks with a persistent
// underline (`.term-autolink`) because the anchor hugs exactly the matched URL
// text, while OSC 8 links underline on hover only (an app can hold one link
// open across decorative cells). Bit 1024; bits 1..512 are SGR (see WireRun.A).
const AttrAutolink = 1024

// maxAutolinkRows caps how many soft-wrapped rows are joined when scanning for
// URLs, bounding the per-row work. Four rows cover any real URL even at phone
// widths (~40 cols); a chain longer than the cap keeps its most recent rows.
const maxAutolinkRows = 4

// urlRE follows the client's autolink pattern (render.ts URL_RE, the xterm.js
// addon-web-links shape) with one deliberate narrowing: URL characters are
// ASCII-only (RFC 3986), so a match terminates at any non-ASCII rune. This
// keeps a wide glyph — and its U+FFFF continuation placeholder, which is an
// internal sentinel that must never leak into an href — out of stamped links,
// and never splits a wide pair across an anchor boundary.
var urlRE = regexp.MustCompile("(?:https?|HTTPS?)://[^\\s\"'!*(){}|\\\\^<>`\\x{80}-\\x{10FFFF}]*[^\\s\"':,.!?{}|\\\\^~[\\]`()<>\\x{80}-\\x{10FFFF}]")

// RenderRowWire returns a row as a slice of style runs for the canvas
// renderer. Same-style consecutive cells are coalesced into a single run, and
// bare URLs in the row's text — joined across soft-wrap continuations — are
// stamped as autolinks (see stampAutolinks).
func (s *Screen) RenderRowWire(y int) []WireRun {
	if y < 0 || y >= s.Height {
		return nil
	}
	return s.stampAutolinks(y, s.cellsToRuns(s.Cells[y]))
}

// cellsToRuns converts a row of cells to wire runs (same-style coalesced).
// A method (not a free function) so color resolution can consult the Screen's
// OSC 4 palette overrides.
func (s *Screen) cellsToRuns(row []Cell) []WireRun {
	var runs []WireRun
	if len(row) == 0 {
		return runs
	}
	var buf strings.Builder
	prev := row[0].Style
	prevURL := row[0].Hyperlink
	for x, cell := range row {
		if x > 0 && (cell.Style != prev || cell.Hyperlink != prevURL) {
			runs = append(runs, s.makeRunWithURL(buf.String(), prev, prevURL))
			buf.Reset()
			prev = cell.Style
			prevURL = cell.Hyperlink
		}
		ch := cell.Ch
		if ch == 0 {
			ch = '\uFFFF'
		}
		buf.WriteRune(ch)
	}
	if buf.Len() > 0 {
		runs = append(runs, s.makeRunWithURL(buf.String(), prev, prevURL))
	}
	return runs
}

func (s *Screen) makeRunWithURL(text string, st Style, url string) WireRun {
	fg, bg := st.FG, st.BG
	if st.Inverse {
		fg, bg = bg, fg
	}
	r := WireRun{T: text, U: url}
	r.F = s.colorToWire(fg)
	r.B = s.colorToWire(bg)
	r.Uc = s.colorToWire(st.UnderlineColor)
	if st.Bold {
		r.A |= 1
	}
	if st.Italic {
		r.A |= 2
	}
	if st.Underline {
		r.A |= 4
	}
	if st.Inverse {
		r.A |= 8
	}
	if st.Strikethrough {
		r.A |= 16
	}
	if st.Dim {
		r.A |= 32
	}
	if st.Hidden {
		r.A |= 64
	}
	if st.Blink {
		r.A |= 128
	}
	if st.Overline {
		r.A |= 256
	}
	if st.DoubleUnderline {
		r.A |= 512
	}
	return r
}

// rowMatchText renders a row's cells as match text for the URL scanner: one
// rune per cell (column index == rune index), with wide-char continuation
// cells as U+FFFF exactly like cellsToRuns — a URL cannot contain a wide
// glyph, so the placeholder correctly breaks any match that would cross one.
func rowMatchText(row []Cell) string {
	var b strings.Builder
	b.Grow(len(row))
	for _, cell := range row {
		ch := cell.Ch
		if ch == 0 {
			ch = '\uFFFF'
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// linkSpan is one detected URL's overlap with a single row: cell columns
// [startCol, endCol) carry the FULL matched URL (which may extend into
// neighboring rows of the wrap chain).
type linkSpan struct {
	url      string
	startCol int
	endCol   int
}

// chainRow is one row's contribution to a joined chain: the text that takes
// part in the URL scan, and the column of the row at which that text starts.
// indent is 0 for the row that opens a chain and for a soft-wrap continuation
// (the whole row is text); it is the width of the application's own
// continuation indent on a hard-wrap continuation, whose leading blanks are
// layout rather than content and must not enter the join.
type chainRow struct {
	text   string
	indent int
}

// blankCell reports whether a cell occupies a column with no glyph: a written
// space, or an unwritten cell (Ch == 0, which is also a wide glyph's second
// half — a URL cannot contain one, so treating it as blank only ever declines
// a join).
func blankCell(c Cell) bool {
	return c.Ch == 0 || c.Ch == ' '
}

// leadingBlankCells counts a row's leading blank columns.
func leadingBlankCells(row []Cell) int {
	n := 0
	for n < len(row) && blankCell(row[n]) {
		n++
	}
	return n
}

// hardWrapIndent reports the continuation indent when row cur continues row
// prev as ONE logical line the APPLICATION wrapped itself, or -1 when cur
// starts its own line.
//
// An Ink-style TUI (kiro-cli, the client this engine is built for, is one) lays
// its own text out: it breaks at the terminal width, re-emits its indent on
// every continuation, and writes each resulting row as a separate line. The
// terminal therefore never autowraps and the soft-wrap chain never fires, so a
// URL such output splits reaches the scanner as unrelated per-row fragments —
// only the first row matches, with an href truncated at the break, and the rows
// after it get no link at all. Recognise that shape, conservatively:
//
//   - prev must be FULL (a glyph in its last column), so the break happened at
//     the right margin rather than because the line ended there;
//   - both rows must open with the same non-empty blank run. Equality is what
//     makes it a continuation indent rather than a coincidence, and requiring it
//     non-empty keeps two ordinary full-width lines (a table, `ls` output) from
//     joining, since an unindented pair is indistinguishable from them;
//   - cur must carry a glyph after that indent (implied: the blank run is
//     shorter than the row).
//
// A pair that passes still changes nothing unless a URL match actually crosses
// the boundary, so a false positive costs a wrong href only where a URL ends
// exactly at the right margin above an equally-indented line — which today
// already yields an href truncated at that same column.
func hardWrapIndent(prev, cur []Cell) int {
	if len(prev) == 0 || len(prev) != len(cur) {
		return -1
	}
	if blankCell(prev[len(prev)-1]) {
		return -1
	}
	indent := leadingBlankCells(prev)
	if indent == 0 || indent >= len(cur) || indent != leadingBlankCells(cur) {
		return -1
	}
	return indent
}

// continuesRow reports whether screen row y continues row y-1 as one logical
// line for the URL scan — a soft wrap, or an application hard wrap.
//
// The soft-wrap flag is checked first and wins: on a soft-wrapped row the
// leading blanks ARE content (the application wrote them there), so treating
// them as an indent and dropping them would join text across a real space.
func (s *Screen) continuesRow(y int) bool {
	if y <= 0 || y >= s.Height || y >= len(s.wrapped) {
		return false
	}
	if s.wrapped[y] {
		return true
	}
	return hardWrapIndent(s.Cells[y-1], s.Cells[y]) >= 0
}

// chainRowFor renders row r's contribution to a chain that opens at row start.
func (s *Screen) chainRowFor(r, start int) chainRow {
	row := chainRow{text: rowMatchText(s.Cells[r])}
	softWrap := r < len(s.wrapped) && s.wrapped[r]
	// A continuation that is not a soft wrap passed hardWrapIndent, so its
	// leading blank run is the application's indent: layout, not content.
	if r > start && !softWrap {
		row.indent = leadingBlankCells(s.Cells[r])
		row.text = string([]rune(row.text)[row.indent:])
	}
	return row
}

// boundChain caps a chain at maxAutolinkRows entries, dropping from whichever
// end is farther from the focus row (older rows first on a tie).
func boundChain(rows []chainRow, focus int) (bounded []chainRow, focusOut int) {
	for len(rows) > maxAutolinkRows {
		if focus > len(rows)-1-focus {
			rows = rows[1:]
			focus--
		} else {
			rows = rows[:len(rows)-1]
		}
	}
	return rows, focus
}

// chainRows assembles the wrap chain containing screen row y from the live
// grid, prefixed by the retained drain tail when the chain begins in
// already-drained history. It returns the chain's rows and the index of row y
// within them.
//
// The drain tail carries SOFT-wrap continuations only (drainTopRow retains it
// on the wrapped flag), so a hard-wrapped chain whose first row has already
// scrolled into history joins only its on-screen rows. That window is narrow in
// practice: an application that lays out its own text emits the whole wrapped
// run in one frame, so the rows below are present when the first one drains,
// which is when its stamp is computed.
func (s *Screen) chainRows(y int) (rows []chainRow, focus int) {
	start := y
	for start > 0 && s.continuesRow(start) {
		start--
	}
	var tail []string
	if start == 0 && len(s.wrapped) > 0 && s.wrapped[0] {
		tail = s.drainTail // chain begins in drained history
	}
	end := y
	for end+1 < s.Height && s.continuesRow(end+1) {
		end++
	}
	rows = make([]chainRow, 0, len(tail)+(end-start+1))
	for _, text := range tail {
		rows = append(rows, chainRow{text: text})
	}
	for r := start; r <= end; r++ {
		rows = append(rows, s.chainRowFor(r, start))
	}
	return boundChain(rows, len(tail)+(y-start))
}

// stampAutolinks detects bare URLs in the wrap chain containing row y and
// stamps the overlap with row y into the given runs: the affected cells get
// the FULL matched URL in U plus the AttrAutolink bit, splitting runs at link
// boundaries. Derived at render time, never stored into cells, so edits are
// picked up on the next render with no invalidation. App-provided OSC 8 links
// (runs already carrying U) are authoritative and never overwritten. This is
// what makes a URL that wraps across rows fully clickable: every row segment
// gets an anchor with the complete href, where the old client-side per-row
// regex left row 2 unlinked and row 1's href truncated at the wrap column.
func (s *Screen) stampAutolinks(y int, runs []WireRun) []WireRun {
	rows, focus := s.chainRows(y)
	spans := autolinkSpans(rows, focus)
	if len(spans) == 0 {
		return runs
	}
	return applyLinkSpans(runs, spans)
}

// autolinkSpans scans the joined chain text for URLs and maps each match's
// overlap back onto the focus row's cell columns. Rows join without
// separators: a soft-wrapped row is by definition full-width, so its text
// abuts the continuation exactly as typed, and a hard-wrap continuation
// contributes the text AFTER its indent for the same reason. The focus row's
// own indent is added back when mapping an offset to a column.
func autolinkSpans(rows []chainRow, focus int) []linkSpan {
	var builder strings.Builder
	for _, row := range rows {
		builder.WriteString(row.text)
	}
	joined := builder.String()
	if !strings.Contains(joined, "://") {
		return nil
	}
	// Rune offset of the focus row within the joined text, and its length.
	rowStart := 0
	for i := range focus {
		rowStart += len([]rune(rows[i].text))
	}
	rowLen := len([]rune(rows[focus].text))
	rowEnd := rowStart + rowLen
	indent := rows[focus].indent

	joinedRunes := []rune(joined)
	var spans []linkSpan
	for _, m := range urlRE.FindAllStringIndex(joined, -1) {
		// Byte offsets → rune offsets (URL chars are ASCII, but the
		// surrounding text may not be).
		ms := len([]rune(joined[:m[0]]))
		me := ms + len([]rune(joined[m[0]:m[1]]))
		if me <= rowStart || ms >= rowEnd {
			continue // match does not touch the focus row
		}
		spans = append(spans, linkSpan{
			url:      string(joinedRunes[ms:me]),
			startCol: max(ms, rowStart) - rowStart + indent,
			endCol:   min(me, rowEnd) - rowStart + indent,
		})
	}
	return spans
}

// applyLinkSpans splits runs at link-span boundaries and stamps the covered
// sub-runs with the span's full URL + AttrAutolink. Runs already carrying an
// app-provided OSC 8 URL pass through untouched.
func applyLinkSpans(runs []WireRun, spans []linkSpan) []WireRun {
	out := make([]WireRun, 0, len(runs)+2*len(spans))
	col := 0
	for _, run := range runs {
		runes := []rune(run.T)
		if run.U != "" { // app link is authoritative
			out = append(out, run)
			col += len(runes)
			continue
		}
		segStart := 0 // rune index within this run of the pending segment
		for segStart < len(runes) {
			absCol := col + segStart
			sp := spanCovering(spans, absCol)
			if sp == nil {
				// Plain segment: extend to the next span start (or run end).
				segEnd := len(runes)
				if next := nextSpanStart(spans, absCol); next-col < segEnd {
					segEnd = next - col
				}
				out = append(out, runSlice(run, runes, segStart, segEnd, "", 0))
				segStart = segEnd
				continue
			}
			segEnd := min(sp.endCol-col, len(runes))
			out = append(out, runSlice(run, runes, segStart, segEnd, sp.url, AttrAutolink))
			segStart = segEnd
		}
		col += len(runes)
	}
	return out
}

// spanCovering returns the span containing column col, or nil.
func spanCovering(spans []linkSpan, col int) *linkSpan {
	for i := range spans {
		if col >= spans[i].startCol && col < spans[i].endCol {
			return &spans[i]
		}
	}
	return nil
}

// nextSpanStart returns the smallest span start greater than col, or a
// sentinel beyond any row width when none follows.
func nextSpanStart(spans []linkSpan, col int) int {
	next := 1 << 30
	for i := range spans {
		if spans[i].startCol > col && spans[i].startCol < next {
			next = spans[i].startCol
		}
	}
	return next
}

// runSlice builds a copy of run covering runes [start, end) with the given
// link URL and extra attribute bits.
func runSlice(run WireRun, runes []rune, start, end int, url string, attr uint16) WireRun {
	r := run
	r.T = string(runes[start:end])
	r.U = url
	r.A |= attr
	return r
}

func (s *Screen) colorToWire(c Color) int32 {
	switch c.Type {
	case 0:
		return wireDefaultColor
	case 1:
		// Basic 8/16: an OSC 4 override wins, else the default ANSI palette.
		// (Reading a nil paletteOverride map is safe and returns ok=false.)
		if v, ok := s.paletteOverride[c.Val]; ok {
			return v
		}
		return basic16RGB(c.Val)
	case 2:
		if v, ok := s.paletteOverride[c.Val]; ok {
			return v
		}
		return color256RGB(c.Val)
	case 3:
		return int32(c.R)<<16 | int32(c.G)<<8 | int32(c.B)
	}
	return wireDefaultColor
}

func basic16RGB(idx uint8) int32 {
	pal := [16]int32{
		0x000000, 0xaa0000, 0x00aa00, 0xaa5500,
		0x0000aa, 0xaa00aa, 0x00aaaa, 0xaaaaaa,
		0x555555, 0xff5555, 0x55ff55, 0xffff55,
		0x5555ff, 0xff55ff, 0x55ffff, 0xffffff,
	}
	if int(idx) < len(pal) {
		return pal[idx]
	}
	return 0xaaaaaa
}

func color256RGB(idx uint8) int32 {
	if idx < 16 {
		return basic16RGB(idx)
	}
	if idx < 232 {
		i := idx - 16
		b := i % 6
		g := (i / 6) % 6
		r := i / 36
		toVal := func(v uint8) int32 {
			if v == 0 {
				return 0
			}
			return int32(55 + int(v)*40) // #nosec G115 -- bounded palette value
		}
		return toVal(r)<<16 | toVal(g)<<8 | toVal(b)
	}
	v := int32(8 + int(idx-232)*10) // #nosec G115 -- bounded grayscale ramp
	return v<<16 | v<<8 | v
}
