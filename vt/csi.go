package vt

import (
	"fmt"
	"log/slog"
	"time"
)

// csiHandler executes one dispatched CSI command. A handler owns everything
// after the parser: it reads its parameters (s.paramVal) and s.privateMarker
// itself, exactly as the former switch arms did.
type csiHandler = func(s *Screen)

// csiCUP implements CUP / HVP — cursor position (shared by 'H' and 'f').
func csiCUP(s *Screen) {
	s.cursorPosition(s.paramVal(0, 1)-1, s.paramVal(1, 1)-1)
}

// csiCHA implements CHA ('G') / HPA ('`') — cursor horizontal absolute
// (origin-mode aware column); the two finals are aliases of the same motion.
func csiCHA(s *Screen) {
	s.curX = s.originCol(s.paramVal(0, 1) - 1)
}

// csiClearsWrap marks the cursor-affecting CSI finals that clear the deferred
// autowrap state before dispatch (printing at the right margin sets it; any
// cursor motion cancels it).
var csiClearsWrap = [256]bool{
	'A': true, 'B': true, 'C': true, 'D': true, 'E': true, 'F': true, 'G': true,
	'H': true, 'f': true, 'd': true, 'e': true, '`': true, 'a': true, 'I': true,
	'Z': true, 'u': true,
}

// csiMain is the final-byte dispatch table for CSI sequences without an
// intermediate byte. Adding a control is one entry here (or one in
// csiIntermediate when it carries an intermediate byte); an absent entry logs
// the unhandled-CSI line in dispatchCSI.
var csiMain = [256]csiHandler{
	// CUU — cursor up (stops at the top margin when starting in-region)
	'A': func(s *Screen) { s.cursorUp(s.paramVal(0, 1)) },
	// CUD — cursor down (stops at the bottom margin when starting in-region)
	'B': func(s *Screen) { s.cursorDown(s.paramVal(0, 1)) },
	// CUF — cursor forward (stops at the right margin when starting in-region)
	'C': func(s *Screen) { s.cursorRight(s.paramVal(0, 1)) },
	// CUB — cursor back (stops at the left margin when starting in-region)
	'D': func(s *Screen) { s.cursorLeft(s.paramVal(0, 1)) },
	// CNL — cursor next line (region-aware down, then to the left margin)
	'E': func(s *Screen) {
		s.cursorDown(s.paramVal(0, 1))
		s.curX = s.leftBound()
	},
	// CPL — cursor previous line (region-aware up, then to the left margin)
	'F': func(s *Screen) {
		s.cursorUp(s.paramVal(0, 1))
		s.curX = s.leftBound()
	},
	// CHA — cursor horizontal absolute (origin-mode aware column)
	'G': csiCHA,
	// CUP / HVP — cursor position
	'H': csiCUP,
	'f': csiCUP,
	// ED (CSI J) / DECSED (CSI ? J — selective, spares protected cells)
	'J': func(s *Screen) { s.eraseInDisplay(s.paramVal(0, 0), s.privateMarker == '?') },
	// EL (CSI K) / DECSEL (CSI ? K — selective, spares protected cells)
	'K': func(s *Screen) { s.eraseInLine(s.paramVal(0, 0), s.privateMarker == '?') },
	// ICH — insert characters
	'@': func(s *Screen) { s.insertChars(s.paramVal(0, 1)) },
	// IL — insert lines
	'L': func(s *Screen) { s.insertLines(s.paramVal(0, 1)) },
	// DL — delete lines
	'M': func(s *Screen) { s.deleteLines(s.paramVal(0, 1)) },
	// DCH — delete characters
	'P': func(s *Screen) { s.deleteChars(s.paramVal(0, 1)) },
	// SU — scroll up
	'S': func(s *Screen) { s.scrollUp(s.paramVal(0, 1)) },
	// SD — scroll down. CSI > Pm T is XTRMTITLE (reset title modes).
	'T': func(s *Screen) {
		if s.privateMarker == '>' {
			s.setTitleModes(false)
		} else {
			s.scrollDown(s.paramVal(0, 1))
		}
	},
	// SD (the '^' alias never carries the XTRMTITLE form)
	'^': func(s *Screen) { s.scrollDown(s.paramVal(0, 1)) },
	// ECH — erase characters
	'X': func(s *Screen) { s.eraseChars(s.paramVal(0, 1)) },
	// CHT — cursor forward tabulation
	'I': func(s *Screen) { s.cursorTabForward(s.paramVal(0, 1)) },
	// CBT — cursor backward tabulation
	'Z': func(s *Screen) { s.cursorTabBackward(s.paramVal(0, 1)) },
	// HPA — horizontal position absolute (origin-mode aware column)
	'`': csiCHA,
	// HPR — horizontal position relative (relative move; no re-origin)
	'a': func(s *Screen) {
		s.curX += s.paramVal(0, 1)
		s.clampCursor()
	},
	// VPA — vertical position absolute (origin-mode aware row; column unchanged)
	'd': func(s *Screen) { s.curY = s.originRow(s.paramVal(0, 1) - 1) },
	// VPR — vertical position relative (same as CUD)
	'e': func(s *Screen) { s.cursorDown(s.paramVal(0, 1)) },
	// REP — repeat last printed character
	'b': func(s *Screen) { s.repeatLastChar(s.paramVal(0, 1)) },
	// DA — device attributes
	'c': func(s *Screen) { s.deviceAttributes() },
	// TBC — tab clear
	'g': func(s *Screen) { s.tabClear(s.paramVal(0, 0)) },
	// DECSTBM — set scroll region. CSI ? Pm r is XTRESTORE (restore the DEC
	// private mode values saved by XTSAVE).
	'r': func(s *Screen) {
		switch s.privateMarker {
		case 0:
			s.setScrollRegion()
		case '?':
			s.xtRestoreModes()
		}
	},
	// SGR — select graphic rendition. CSI > Pp;Pv m is XTMODKEYS
	// (modifyOtherKeys) and CSI ? Pp m is XTQMODKEYS — key-option controls,
	// NOT SGR. Routing them to applySGR would apply bogus text attributes
	// (kiro-cli emits CSI > 4 ; 1 m at init). We don't implement
	// modifyOtherKeys, so consume the marked forms as no-ops.
	'm': func(s *Screen) {
		if s.privateMarker == 0 {
			s.applySGR()
		}
	},
	// CSI Pl;Pr s is DECSLRM (set left/right margins) when DECLRMM is
	// enabled; otherwise CSI s is SCOSC (save cursor). CSI ? Pm s is XTSAVE
	// (save DEC private mode values for later XTRESTORE).
	's': func(s *Screen) {
		switch s.privateMarker {
		case 0:
			if s.LRMarginMode {
				s.setLeftRightMargins()
			} else {
				s.saveCursor()
			}
		case '?':
			s.xtSaveModes()
		}
	},
	// Plain CSI u is SCORC (restore cursor). The private-marker forms are the
	// kitty keyboard protocol — query (?), push (>), pop (<) and set (=) the
	// progressive-enhancement flags. The flags are synced to the client, whose
	// key encoder produces the matching kitty CSI-u sequences, so advertising
	// support here is safe. See kitty.go.
	'u': func(s *Screen) {
		switch s.privateMarker {
		case 0:
			s.restoreCursor()
		case '?': // query current flags -> CSI ? flags u
			s.reportKeyboardFlags()
		case '>': // push flags (default 0)
			s.pushKeyboardFlags(s.paramVal(0, 0))
		case '<': // pop n entries (default 1)
			s.popKeyboardFlags(s.paramVal(0, 1))
		case '=': // set flags with mode (default mode 1)
			s.setKeyboardFlags(s.paramVal(0, 0), s.paramVal(1, 1))
		}
	},
	// XTVERSION (CSI > q) — report a generic terminal name so probing apps get
	// an answer instead of stalling. Kept intentionally generic (no
	// known-terminal name/version) so apps don't enable terminal-specific
	// quirks. Plain CSI q is DECLL (load LEDs), which we don't support.
	'q': func(s *Screen) {
		if s.privateMarker == '>' {
			s.response = append(s.response, "\x1bP>|web-terminal-engine\x1b\\"...)
		}
	},
	// SM / DECSET — set modes
	'h': func(s *Screen) { s.applyModes(true) },
	// RM / DECRST — reset modes
	'l': func(s *Screen) { s.applyModes(false) },
	// Window manipulation. CSI > Pm t is XTSMTITLE (set title modes).
	't': func(s *Screen) {
		if s.privateMarker == '>' {
			s.setTitleModes(true)
		} else {
			s.windowManipulation()
		}
	},
	// DSR — device status report
	'n': func(s *Screen) { s.deviceStatusReport() },
}

// csiIntermediate is the dispatch table for CSI sequences carrying an
// intermediate byte, keyed by {first intermediate, final}. A known
// intermediate prefix CONSUMES its sequence even when the final byte has no
// entry (see dispatchCSIIntermediate); an unknown prefix falls through to the
// main dispatch, preserving the pre-table behavior.
var csiIntermediate = map[[2]byte]csiHandler{
	// SL — shift left
	{' ', '@'}: func(s *Screen) { s.shiftLeft(s.paramVal(0, 1)) },
	// SR — shift right
	{' ', 'A'}: func(s *Screen) { s.shiftRight(s.paramVal(0, 1)) },
	// DECSCUSR — set cursor style
	{' ', 'q'}: func(s *Screen) {
		v := s.paramVal(0, 0)
		if v <= 6 {
			s.CursorStyle = uint8(v) // #nosec G115 -- v bounded [0,6]
			s.CursorBlink = v == 0 || v%2 == 1
		}
	},
	// DECSTR — soft terminal reset
	{'!', 'p'}: func(s *Screen) { s.softReset() },
	// DECRQM — request mode (with or without the '?' prefix)
	{'$', 'p'}: func(s *Screen) { s.handleDECRQM() },
	// DECFRA — fill rectangular area with a character
	{'$', 'x'}: func(s *Screen) { s.fillRect() },
	// DECERA — erase rectangular area (to blanks)
	{'$', 'z'}: func(s *Screen) { s.eraseRect(false) },
	// DECSERA — selective erase rectangular area (spares DECSCA cells)
	{'$', '{'}: func(s *Screen) { s.eraseRect(true) },
	// DECCRA — copy rectangular area
	{'$', 'v'}: func(s *Screen) { s.copyRect() },
	// DECSCA — select character protection: Ps=1 protects cells printed from
	// now on; Ps=0/2 clears the attribute.
	{'"', 'q'}: func(s *Screen) { s.curProtected = s.paramVal(0, 0) == 1 },
	// DECSCL — conformance level. Track the requested level (60+n) so a
	// DECRQSS DECSCL query reports it back; no other behavioral effect.
	{'"', 'p'}: func(s *Screen) { s.conformanceLevel = s.paramVal(0, 65) },
	// DECIC — insert blank columns at the cursor column across the region
	{'\'', '}'}: func(s *Screen) { s.insertColumns(s.paramVal(0, 1)) },
	// DECDC — delete columns at the cursor column across the region
	{'\'', '~'}: func(s *Screen) { s.deleteColumns(s.paramVal(0, 1)) },
	// DECRQCRA — request rectangular-area checksum
	{'*', 'y'}: func(s *Screen) { s.reportRectChecksum() },
	// DECSACE — set attribute change extent, configuring rectangular ops we
	// don't implement; consumed as a no-op.
	{'*', 'x'}: func(*Screen) {},
	// DECSNLS — set number of lines per screen. The browser viewport owns the
	// row count, so the requested value is tracked only (for the DECRQSS "*|"
	// report), not applied as a resize.
	{'*', '|'}: func(s *Screen) { s.linesPerScreen = s.paramVal(0, 0) },
}

func (s *Screen) dispatchCSI(final byte) {
	// Any cursor-affecting CSI clears pending wrap.
	if csiClearsWrap[final] {
		s.pendingWrap = false
	}

	// Intermediate-byte sequences dispatch separately. An unrecognized
	// intermediate returns false and falls through to the main final-byte
	// dispatch below, preserving the pre-table behavior.
	if s.numInterm > 0 && s.dispatchCSIIntermediate(final) {
		return
	}

	if h := csiMain[final]; h != nil {
		h(s)
		return
	}
	if final != 0 {
		slog.Info("vt: unhandled CSI", "cmd", string(final), "marker", s.privateMarker)
	}
}

// dispatchCSIIntermediate handles CSI sequences carrying an intermediate byte
// via the csiIntermediate table. It returns true when it consumed the
// sequence; an unrecognized intermediate PREFIX returns false so the caller
// falls through to the main final-byte dispatch. A known prefix with an
// unknown final is consumed (logged per prefix, except '!' which was
// historically silent), never falling through — matching the pre-table
// per-prefix default arms. Called only when s.numInterm > 0.
func (s *Screen) dispatchCSIIntermediate(final byte) bool {
	intermediate := s.pIntermed[0]
	if h, ok := csiIntermediate[[2]byte{intermediate, final}]; ok {
		h(s)
		return true
	}
	switch intermediate {
	case ' ':
		slog.Info("vt: unhandled CSI SP", "cmd", string(final), "args", s.paramVal(0, 0))
		return true
	case '!':
		return true // historically consumed without logging
	case '$':
		slog.Info("vt: unhandled CSI $", "cmd", string(final))
		return true
	case '"':
		slog.Info("vt: unhandled CSI \"", "cmd", string(final))
		return true
	case '\'':
		slog.Info("vt: unhandled CSI '", "cmd", string(final))
		return true
	case '*':
		slog.Info("vt: unhandled CSI *", "cmd", string(final))
		return true
	}
	return false
}

// clampCursor confines the cursor to the screen bounds. The cursor-movement
// handlers move the cursor freely and then call this; clamping the axis that
// did not move is a no-op, so one helper serves every movement command.
func (s *Screen) clampCursor() {
	s.curX = min(max(s.curX, 0), s.Width-1)
	s.curY = min(max(s.curY, 0), s.Height-1)
}

// cursorUp moves the cursor up n rows, respecting the scroll region: a cursor
// at or below the top margin stops at the top margin; one already above the
// margin (outside the region) may travel to the screen top. Matches xterm's
// CursorUp (min = cur_row >= top_marg ? top_marg : 0).
func (s *Screen) cursorUp(n int) {
	low := 0
	if s.curY >= s.scrollTop {
		low = s.scrollTop
	}
	s.curY = max(s.curY-n, low)
	s.clampCursor()
}

// cursorDown moves the cursor down n rows, respecting the scroll region: a
// cursor at or above the bottom margin stops at the bottom margin; one already
// below the margin may travel to the screen bottom. Matches xterm's CursorDown
// (max = cur_row <= bot_marg ? bot_marg : max_row).
func (s *Screen) cursorDown(n int) {
	high := s.Height - 1
	if s.curY <= s.scrollBottom {
		high = s.scrollBottom
	}
	s.curY = min(s.curY+n, high)
	s.clampCursor()
}

// cursorPosition implements CUP/HVP. The coordinates are 0-based (the caller
// converts from the 1-based CSI parameters). In origin mode both axes are
// relative to the scroll region / left-right margins and clamped to them.
func (s *Screen) cursorPosition(y, x int) {
	s.curY = s.originRow(y)
	s.curX = s.originCol(x)
}

// originRow maps a 0-based row parameter to an absolute row. In origin mode the
// row is relative to the top margin and clamped to the bottom margin; otherwise
// it is clamped to the screen. Shared by CUP/HVP and VPA.
func (s *Screen) originRow(y int) int {
	y = max(y, 0)
	if s.OriginMode {
		return min(y+s.scrollTop, s.scrollBottom)
	}
	return min(y, s.Height-1)
}

// originCol maps a 0-based column parameter to an absolute column. In origin
// mode the column is relative to the left margin and clamped to the right
// margin; otherwise it is clamped to the screen. Shared by CUP/HVP, CHA, HPA.
func (s *Screen) originCol(x int) int {
	x = max(x, 0)
	if s.OriginMode {
		return min(x+s.leftBound(), s.rightBound())
	}
	return min(x, s.Width-1)
}

// cursorRight implements CUF: move right n columns, stopping at the right margin
// when the cursor starts at or left of it, else at the screen edge (xterm).
func (s *Screen) cursorRight(n int) {
	high := s.Width - 1
	if s.curX <= s.rightBound() {
		high = s.rightBound()
	}
	s.curX = min(s.curX+n, high)
	s.clampCursor()
}

// cursorLeft implements CUB: move left n columns. Under reverse-wraparound
// (DEC ?45 together with DECAWM) it steps back exactly like a run of
// Backspaces, wrapping past the left margin to the end of the previous line;
// xterm drives CUB and BS through the same reverse-wrap path, so CUB(n) must
// match n Backspaces (esctest's CUB reverse-wrap cases assert this identity).
// Without reverse-wrap it moves left in one step, stopping at the left margin
// when the cursor starts at or right of it, else at column 0 (xterm).
func (s *Screen) cursorLeft(n int) {
	if n < 1 {
		n = 1
	}
	if s.ReverseWrap && s.AutoWrap {
		for range n {
			s.backspace()
		}
		return
	}
	low := 0
	if s.curX >= s.leftBound() {
		low = s.leftBound()
	}
	s.curX = max(s.curX-n, low)
	s.clampCursor()
}

// backspace handles BS (0x08): move the cursor left one column, honoring
// reverse-wraparound (DEC ?45), which only takes effect together with DECAWM
// (autowrap) — xterm gates it on both. At the left margin under reverse-wrap it
// wraps back to the end of the previous line; from the top of the scroll region
// it wraps around to the bottom margin's right edge — xterm's version-0 mode-45
// behavior (the classic X10.4 upper-left→lower-right wrap, bounded to the
// scroll region). The 2023 split that limits ?45 and moves the wrap-around to
// ?1045 is a later version esctest does not assert by default.
func (s *Screen) backspace() {
	revWrap := s.ReverseWrap && s.AutoWrap
	left := s.leftBound()
	wasPending := s.pendingWrap
	s.pendingWrap = false
	switch {
	case wasPending && revWrap:
		// In the deferred-wrap ("do-wrap") position, a backspace under
		// reverse-wrap only cancels the pending wrap (done above); the cursor
		// stays on the last column so the next glyph overwrites it.
	case s.curX > left:
		// Ordinary move left, stopping at the left margin.
		s.curX--
	case revWrap && s.curY > s.scrollTop:
		// At (or left of) the left margin, reverse-wrap back to the right margin
		// of the previous line.
		s.curY--
		s.curX = s.rightBound()
	case revWrap && s.curY <= s.scrollTop:
		// At the top of the scroll region, wrap around to the bottom margin's
		// right edge rather than stopping (version-0 mode-45 semantics).
		s.curY = s.scrollBottom
		s.curX = s.rightBound()
	case s.curX > 0 && s.curX < left:
		// Left of the left margin (no reverse-wrap): move left toward column 0.
		s.curX--
	}
}

// reportRectChecksum answers DECRQCRA with DCS Pid ! ~ hhhh ST, where hhhh is
// the checksum of the requested rectangle. Gated behind AllowScreenReport (see
// the field doc): the reply is written back into the PTY as input, so it is a
// screen-scrape-and-inject vector unless explicitly enabled.
//
// The algorithm matches xterm at patch level < 279 (esctest's default
// --xterm-checksum): the negation of the 16-bit sum of the cells' character
// ordinals, counting blank/erased cells as space (0x20). esctest un-negates the
// value and compares one cell at a time, so this lets AssertScreenCharsInRectEqual
// recover the exact character present in each cell. Coordinates are 1-based and
// origin-mode-relative, matching CUP.
func (s *Screen) reportRectChecksum() {
	if !s.AllowScreenReport {
		return
	}
	pid := s.paramVal(0, 0)
	// params: 0=Pid 1=Pp(page, ignored) 2=Pt 3=Pl 4=Pb 5=Pr. rectBounds applies
	// origin-mode offset/clamp on both axes (matching CUP and the DECFRA/DECCRA
	// rectangle ops), so a checksum requested under origin mode reads the right
	// cells.
	y1, x1, y2, x2 := s.rectBounds(s.paramVal(2, 0), s.paramVal(3, 0), s.paramVal(4, 0), s.paramVal(5, 0))
	var sum uint32
	for y := y1; y <= y2; y++ {
		for x := x1; x <= x2; x++ {
			ch := s.Cells[y][x].Ch
			if ch == 0 {
				ch = ' ' // wide-char spacer / unset cell counts as blank
			}
			sum += uint32(ch)
		}
	}
	checksum := (0x10000 - sum%0x10000) % 0x10000 // negated 16-bit ordinal sum
	s.response = fmt.Appendf(s.response, "\x1bP%d!~%04X\x1b\\", pid, checksum)
}

// rectBounds resolves a 1-based rectangle (Pt;Pl;Pb;Pr, 0 = default) to 0-based
// inclusive bounds. Coordinates are origin-mode-relative (offset by the region
// origin and clamped to the region) when origin mode is set, else absolute and
// clamped to the screen. The rectangular ops deliberately ignore the left/right
// margins for clamping (DEC: they operate on the page, not the scroll box).
func (s *Screen) rectBounds(pt, pl, pb, pr int) (y1, x1, y2, x2 int) {
	rowLo, rowHi, colLo, colHi := 0, s.Height-1, 0, s.Width-1
	oy, ox := 0, 0
	if s.OriginMode {
		oy, ox = s.scrollTop, s.leftBound()
		rowLo, rowHi, colLo, colHi = s.scrollTop, s.scrollBottom, s.leftBound(), s.rightBound()
	}
	top, left := pt, pl
	if top < 1 {
		top = 1
	}
	if left < 1 {
		left = 1
	}
	y1 = min(max(top-1+oy, rowLo), rowHi)
	x1 = min(max(left-1+ox, colLo), colHi)
	y2 = rowHi
	if pb >= 1 {
		y2 = min(max(pb-1+oy, rowLo), rowHi)
	}
	x2 = colHi
	if pr >= 1 {
		x2 = min(max(pr-1+ox, colLo), colHi)
	}
	return y1, x1, y2, x2
}

// fillRect implements DECFRA (CSI Pch ; Pt ; Pl ; Pb ; Pr $ x): fill the
// rectangle with the character Pch, using the current SGR/protection.
func (s *Screen) fillRect() {
	ch := rune(s.paramVal(0, 0)) //nolint:gosec // paramVal is capped at 65535, well within rune range
	if ch < 0x20 {
		ch = ' '
	}
	y1, x1, y2, x2 := s.rectBounds(s.paramVal(1, 0), s.paramVal(2, 0), s.paramVal(3, 0), s.paramVal(4, 0))
	for y := y1; y <= y2; y++ {
		for x := x1; x <= x2; x++ {
			s.Cells[y][x] = Cell{Ch: ch, Style: s.style, Protected: s.curProtected}
		}
	}
}

// eraseRect implements DECERA (CSI Pt;Pl;Pb;Pr $ z) and, when selective is true,
// DECSERA ($ {): erase the rectangle to blanks. DECSERA spares DECSCA-protected
// cells (Cell.Protected) but not ISO/SPA-EPA-guarded cells, which we don't model.
func (s *Screen) eraseRect(selective bool) {
	// DECERA clears the rectangle unconditionally; DECSERA spares DECSCA-
	// protected cells (and, unlike DECSED/DECSEL, does NOT spare ISO cells).
	m := eraseAll
	if selective {
		m = eraseSpareDECSCA
	}
	y1, x1, y2, x2 := s.rectBounds(s.paramVal(0, 0), s.paramVal(1, 0), s.paramVal(2, 0), s.paramVal(3, 0))
	s.eraseRegionMode(y1, x1, y2, x2, m)
}

// copyRect implements DECCRA (CSI Pts;Pls;Pbs;Prs;Pps;Ptd;Pld;Ppd $ v): copy the
// source rectangle to the destination top-left. A single page is assumed (page
// params ignored). The copy goes through a buffer so overlapping source and
// destination rectangles are handled correctly.
func (s *Screen) copyRect() {
	sy1, sx1, sy2, sx2 := s.rectBounds(s.paramVal(0, 0), s.paramVal(1, 0), s.paramVal(2, 0), s.paramVal(3, 0))
	rowLo, rowHi, colLo, colHi := 0, s.Height-1, 0, s.Width-1
	oy, ox := 0, 0
	if s.OriginMode {
		oy, ox = s.scrollTop, s.leftBound()
		rowLo, rowHi, colLo, colHi = s.scrollTop, s.scrollBottom, s.leftBound(), s.rightBound()
	}
	dtop, dleft := s.paramVal(5, 0), s.paramVal(6, 0)
	if dtop < 1 {
		dtop = 1
	}
	if dleft < 1 {
		dleft = 1
	}
	dy0 := min(max(dtop-1+oy, rowLo), rowHi)
	dx0 := min(max(dleft-1+ox, colLo), colHi)
	h, w := sy2-sy1+1, sx2-sx1+1
	if h <= 0 || w <= 0 {
		return
	}
	buf := make([][]Cell, h)
	for i := range buf {
		buf[i] = append([]Cell(nil), s.Cells[sy1+i][sx1:sx2+1]...)
	}
	for i := range h {
		for j := range w {
			dy, dx := dy0+i, dx0+j
			if dy >= 0 && dy < s.Height && dx >= 0 && dx < s.Width {
				s.Cells[dy][dx] = buf[i][j]
			}
		}
	}
}

// eraseInDisplay implements ED (CSI J) and, when selective is true, DECSED
// (CSI ? J), which spares DECSCA-protected cells.
func (s *Screen) eraseInDisplay(mode int, selective bool) {
	// Regular ED spares ISO-guarded cells; selective ED (DECSED) spares DECSCA
	// AND ISO cells (xterm keeps ISO for backward compatibility).
	regular := eraseSpareISO
	if selective {
		regular = eraseSpareBoth
	}
	switch mode {
	case 0:
		s.eraseRegionMode(s.curY, s.curX, s.curY, s.Width-1, regular)
		s.eraseRegionMode(s.curY+1, 0, s.Height-1, s.Width-1, regular)
	case 1:
		s.eraseRegionMode(0, 0, s.curY-1, s.Width-1, regular)
		s.eraseRegionMode(s.curY, 0, s.curY, s.curX, regular)
	case 2:
		// Full-screen erase: DECSED still spares protected cells, but plain ED 2
		// clears everything so a reset (which uses ED 2) wipes any guarded cells.
		m := eraseAll
		if selective {
			m = eraseSpareBoth
		}
		s.eraseRegionMode(0, 0, s.Height-1, s.Width-1, m)
		s.Drained = nil
		s.clearWrapState()
	case 3:
		// ED3 — "Erase Saved Lines" (xterm). Clears the scrollback buffer
		// ONLY; the visible screen is left untouched. The VT has no handle on
		// the terminal-layer scrollback ring, so it raises a flag the handler
		// observes to clear the ring and tell the client to drop its history.
		// Inline TUIs (kiro-cli) emit ED3 on every resize redraw to discard the
		// previous frame before repainting; honoring it is exactly what keeps a
		// real terminal from accumulating stale frames on resize. The pending
		// (not-yet-committed) drain is scrollback-bound, so it is discarded too.
		s.scrollbackCleared = true
		s.Drained = nil
		// The retained cross-boundary chain tail describes history that no
		// longer exists; the on-screen flags describe rows that remain.
		s.drainTail = nil
	}
}

// eraseInLine implements EL (CSI K) and, when selective is true, DECSEL
// (CSI ? K), which spares DECSCA-protected cells.
func (s *Screen) eraseInLine(mode int, selective bool) {
	m := eraseSpareISO
	if selective {
		m = eraseSpareBoth
	}
	switch mode {
	case 0:
		s.eraseRegionMode(s.curY, s.curX, s.curY, s.Width-1, m)
	case 1:
		s.eraseRegionMode(s.curY, 0, s.curY, s.curX, m)
	case 2:
		s.eraseRegionMode(s.curY, 0, s.curY, s.Width-1, m)
	}
}

// eraseChars implements ECH (CSI X): erase n cells from the cursor rightward,
// clamped to the row end.
func (s *Screen) eraseChars(n int) {
	end := s.curX + n - 1
	if end >= s.Width {
		end = s.Width - 1
	}
	// ECH respects ISO (SPA/EPA) protection like ED/EL.
	s.eraseRegionMode(s.curY, s.curX, s.curY, end, eraseSpareISO)
}

// scrollUp implements SU (CSI S): scroll the region up n lines (clamped to the
// region height).
func (s *Screen) scrollUp(n int) {
	n = min(n, s.scrollBottom-s.scrollTop+1)
	for range n {
		s.scrollUpOnce()
	}
}

// scrollDown implements SD (CSI T / CSI ^): scroll the region down n lines.
func (s *Screen) scrollDown(n int) {
	n = min(n, s.scrollBottom-s.scrollTop+1)
	for range n {
		s.scrollDownOnce()
	}
}

// cursorTabForward implements CHT (CSI I): advance the cursor n tab stops,
// stopping at the right margin.
func (s *Screen) cursorTabForward(n int) {
	for range n {
		if !s.tabOnce() {
			break
		}
	}
}

// tabOnce advances the cursor to the next tab stop, stopping at the right margin
// (xterm/DEC behavior: tabs stop at the right margin, not the screen edge, when
// the cursor is at or left of it). Returns false when the cursor could not
// advance (already at the stop edge), so callers can stop early.
func (s *Screen) tabOnce() bool {
	right := s.rightBound()
	next := s.nextTabStop(s.curX)
	switch {
	case s.curX <= right && next > right:
		if s.curX == right {
			return false
		}
		s.curX = right
	case next >= s.Width:
		if s.curX == s.Width-1 {
			return false
		}
		s.curX = s.Width - 1
	default:
		s.curX = next
	}
	return true
}

// cursorTabBackward implements CBT (CSI Z): move the cursor back n tab stops,
// stopping at column 0.
func (s *Screen) cursorTabBackward(n int) {
	for range n {
		s.curX = s.prevTabStop(s.curX)
		if s.curX <= 0 {
			s.curX = 0
			break
		}
	}
}

// repeatLastChar implements REP (CSI b): reprint the last printed rune n times
// using the style it was printed with.
func (s *Screen) repeatLastChar(n int) {
	if s.lastPrintedRune == 0 {
		return
	}
	saved := s.style
	s.style = s.lastPrintedStyle
	for range n {
		s.put(s.lastPrintedRune)
	}
	s.style = saved
}

// deviceAttributes implements DA (CSI c) for the primary, secondary ('>') and
// tertiary ('=') variants. The engine now implements a VT500-level feature set
// (left/right margins, rectangular editing, selective erase, color, DECRQM/
// DECRQSS status strings), so it advertises xterm's VT525 profile — the same
// answer real xterm gives at max VT level 5 — rather than a conservative VT220.
func (s *Screen) deviceAttributes() {
	switch s.privateMarker {
	case '>':
		// Secondary DA (DA2): CSI > 64 ; Pv ; 0 c. 64 = VT525-class model (xterm
		// at VT level 5); Pv is the firmware/patch level. daFirmwareVersion sits
		// in xterm's plausible range so version-probing apps get a sane answer.
		s.response = fmt.Appendf(s.response, "\x1b[>64;%d;0c", daFirmwareVersion)
	case '=':
		// Tertiary DA (DA3): DCS ! | <unit id> ST. A fixed all-zero site id.
		s.response = append(s.response, "\x1bP!|00000000\x1b\\"...)
	default:
		// Primary DA: CSI ? 65 ; ... c. 65 = VT525 conformance; the trailing
		// list is xterm's default level-5 feature set:
		//   1 132-cols  2 printer  6 selective-erase  9 NRCS  15 DEC-technical
		//   16 locator  17 terminal-state  18 user-windows  21 horizontal-scroll
		//   22 color    28 rectangular-editing          29 ANSI-text-locator
		if s.paramVal(0, 0) == 0 {
			s.response = append(s.response, "\x1b[?65;1;2;6;9;15;16;17;18;21;22;28;29c"...)
		}
	}
}

// tabClear implements TBC (CSI g): clear the stop at the cursor (Ps=0) or all
// stops (Ps=3).
func (s *Screen) tabClear(mode int) {
	switch mode {
	case 0:
		s.clearTabStop(s.curX)
	case 3:
		s.clearAllTabStops()
	}
}

// setScrollRegion implements DECSTBM (CSI r). On a valid region it sets the
// margins and homes the cursor (to the region top-left under origin mode, else
// the screen top-left). An invalid/empty region (top >= bottom) is a no-op and
// leaves the cursor untouched, matching xterm.
func (s *Screen) setScrollRegion() {
	top := max(s.paramVal(0, 1)-1, 0)
	bottom := min(s.paramVal(1, s.Height)-1, s.Height-1)
	if top >= bottom {
		return
	}
	s.scrollTop = top
	s.scrollBottom = bottom
	if s.OriginMode {
		s.curY, s.curX = s.scrollTop, s.leftBound()
	} else {
		s.curY, s.curX = 0, 0
	}
	s.pendingWrap = false
}

// setLeftRightMargins implements DECSLRM (CSI Pl ; Pr s), active only under
// DECLRMM. It sets the left/right margins and homes the cursor (to the region
// top-left under origin mode, else the screen top-left). An invalid range
// (left >= right, or out of bounds) is ignored, matching xterm.
func (s *Screen) setLeftRightMargins() {
	left := max(s.paramVal(0, 1)-1, 0)
	right := min(s.paramVal(1, s.Width)-1, s.Width-1)
	if left >= right {
		return
	}
	s.leftMargin = left
	s.rightMargin = right
	if s.OriginMode {
		s.curY, s.curX = s.scrollTop, s.leftMargin
	} else {
		s.curY, s.curX = 0, 0
	}
	s.pendingWrap = false
}

// windowManipulation answers the subset of XTWINOPS (CSI t) we support:
// report the text-area size in characters (Ps=18), and the title stack
// save/restore (Ps=22/23) that tmux and vim use to preserve the window title.
// Pixel-size reports (14/16) are intentionally omitted: the VT has no font
// metrics, so any pixel answer would be a guess.
func (s *Screen) windowManipulation() {
	switch s.paramVal(0, 0) {
	case 18: // report text area size in characters: CSI 8 ; height ; width t
		s.response = fmt.Appendf(s.response, "\x1b[8;%d;%dt", s.Height, s.Width)
	case 19: // report screen size in characters (no separate window chrome here)
		s.response = fmt.Appendf(s.response, "\x1b[9;%d;%dt", s.Height, s.Width)
	case 20: // report icon label: OSC L <icon> ST
		s.response = fmt.Appendf(s.response, "\x1b]L%s\x1b\\", s.encodeTitle(s.iconTitle))
	case 21: // report window title: OSC l <title> ST
		s.response = fmt.Appendf(s.response, "\x1b]l%s\x1b\\", s.encodeTitle(s.Title))
	case 22: // push icon and/or window title onto the respective stack
		s.pushTitle(s.paramVal(1, 0))
	case 23: // pop icon and/or window title from the respective stack
		s.popTitle(s.paramVal(1, 0))
	default:
		// DECSLPP (CSI Ps t with Ps>=24): set lines per page. xterm resizes the
		// window; the browser viewport owns the row count here, so the value is
		// tracked only (for the DECRQSS "t" report), not applied as a resize.
		if s.paramVal(0, 0) >= 24 {
			s.linesPerPage = s.paramVal(0, 0)
		}
	}
}

// setTitleModes implements XTSMTITLE (CSI > Pm t, set=true) and XTRMTITLE
// (CSI > Pm T, set=false): each parameter toggles one title-mode feature.
//
//	0 = set window/icon labels using hexadecimal
//	1 = query window/icon labels using hexadecimal
//	2 = set window/icon labels using UTF-8
//	3 = query window/icon labels using UTF-8
//
// Only the hex features change behavior: the engine's titles are always UTF-8,
// so the UTF-8 features (2, 3) are accepted as no-ops. A bare CSI > t / CSI > T
// carries the engine's default parameter 0 (paramCount() is never 0 here:
// parserClear seeds numGroups=1 on CSI entry and finalizeParams only grows the
// group, never below 1; see parse.go), so it toggles the set-hex feature,
// consistent with how applySGR and windowManipulation treat a bare CSI as an
// explicit 0.
func (s *Screen) setTitleModes(set bool) {
	for i := range s.paramCount() {
		switch s.paramVal(i, 0) {
		case 0: // set-hex
			s.titleSetHex = set
		case 1: // query-hex
			s.titleQueryHex = set
		}
	}
}

// pushTitle implements XTWINOPS 22 ; Ps: push one combined entry snapshotting
// both the current icon and window titles onto the shared title stack. Ps (0
// icon+window, 1 icon, 2 window) selects what a later pop restores, not what is
// stored — matching xterm, which snapshots both and pops whole entries.
func (s *Screen) pushTitle(_ int) {
	entry := titleEntry{icon: s.iconTitle, window: s.Title}
	s.titleStack = append(s.titleStack, entry)
	if len(s.titleStack) > maxTitleStack {
		s.titleStack = s.titleStack[len(s.titleStack)-maxTitleStack:]
	}
}

// popTitle implements XTWINOPS 23 ; Ps: pop the top entry (if any) and restore
// the icon title (Ps 0 or 1) and/or the window title (Ps 0 or 2) from it. One
// pop consumes one entry regardless of Ps, so a pop-icon after a push-both
// leaves the stack empty and a following pop-window is a no-op.
func (s *Screen) popTitle(which int) {
	n := len(s.titleStack)
	if n == 0 {
		return
	}
	top := s.titleStack[n-1]
	s.titleStack = s.titleStack[:n-1]
	if which == 0 || which == 1 {
		s.iconTitle = top.icon
	}
	if which == 0 || which == 2 {
		s.Title = top.window
	}
}

// deviceStatusReport implements DSR (CSI n / CSI ? n): cursor-position report
// (Ps=6) and, for the ANSI form, terminal-status report (Ps=5).
func (s *Screen) deviceStatusReport() {
	// In origin mode the cursor position is reported relative to the top/left
	// margins (so CUP(1,1) reads back as row 1, col 1); otherwise it is absolute.
	row, col := s.curY+1, s.curX+1
	if s.OriginMode {
		row = max(s.curY-s.scrollTop+1, 1)
		col = max(s.curX-s.leftBound()+1, 1)
	}
	if s.privateMarker == '?' {
		s.decDeviceStatus(row, col)
		return
	}
	switch s.paramVal(0, 0) {
	case 5:
		s.response = append(s.response, "\x1b[0n"...)
	case 6:
		s.response = fmt.Appendf(s.response, "\x1b[%d;%dR", row, col)
	}
}

// decDeviceStatus answers the DEC-private DSR queries (CSI ? Ps n). Besides the
// cursor-position report (DECXCPR, Ps=6) these are legacy device queries
// (printer, keyboard, locator, UDK, data integrity, session, macro space,
// memory checksum); we answer each with a legal "not available / ready / empty"
// response so probing apps get a valid reply instead of stalling. None leak
// screen contents, so unlike DECRQCRA they are always answered.
func (s *Screen) decDeviceStatus(row, col int) {
	switch s.paramVal(0, 0) {
	case 6: // DECXCPR — extended cursor position: CSI ? Pl ; Pc ; Pp R. The page
		// (Pp) is always 1 here (a browser terminal has no page memory). We
		// advertise VT level 4+ via DA2, so the page must be present.
		s.response = fmt.Appendf(s.response, "\x1b[?%d;%d;1R", row, col)
	case 15: // printer status — no printer
		s.response = append(s.response, "\x1b[?13n"...)
	case 25: // user-defined keys — locked
		s.response = append(s.response, "\x1b[?21n"...)
	case 26: // keyboard status: CSI ? 27 ; Pn ; Pst ; Ptyp n. VT level 4+ (which
		// we advertise) carries all four fields: language 0 (unknown), status 0
		// (ready), type 0 (LK201).
		s.response = append(s.response, "\x1b[?27;0;0;0n"...)
	case 55: // locator status — no locator
		s.response = append(s.response, "\x1b[?53n"...)
	case 56: // locator type — cannot identify
		s.response = append(s.response, "\x1b[?57;0n"...)
	case 62: // DECMSR — macro space report (none available)
		s.response = append(s.response, "\x1b[0*{"...)
	case 63: // DECCKSR — memory checksum of macro Pid (we hold none -> 0000)
		s.response = fmt.Appendf(s.response, "\x1bP%d!~0000\x1b\\", s.paramVal(1, 0))
	case 75: // data integrity — ready, no errors
		s.response = append(s.response, "\x1b[?70n"...)
	case 85: // multiple-session status — not configured
		s.response = append(s.response, "\x1b[?83n"...)
	}
}

// applyModes handles CSI Ps h/l (set/reset). DEC private modes carry the '?'
// marker; ANSI modes carry no marker.
//
//nolint:gocognit // flat set/reset dispatch over DEC-private and ANSI modes
func (s *Screen) applyModes(set bool) {
	switch s.privateMarker {
	case '?':
		for i := range s.paramCount() {
			mode := s.paramVal(i, 0)
			if set {
				s.setMode(mode)
			} else {
				s.resetMode(mode)
			}
		}
	case 0:
		// ANSI (non-private) modes.
		for i := range s.paramCount() {
			mode := s.paramVal(i, 0)
			switch mode {
			case 4: // IRM — insert/replace mode
				s.InsertMode = set
			case 20: // LNM — newline mode: LF/VT/FF also perform a carriage return
				s.LineFeedNewLine = set
			default:
				// Settable no-op ANSI modes (KAM, SRM): track the bit for DECRQM.
				if settableANSIModes[mode] {
					s.trackANSIMode(mode, set)
				}
			}
		}
	default:
		// Markers >, <, = are not valid for h/l — ignore.
	}
}

//nolint:gocyclo // mode dispatch is inherently branchy
func (s *Screen) setMode(mode int) {
	switch mode {
	case 1:
		s.AppCursorKeys = true
	case 5:
		s.ReverseVideo = true
	case 6:
		s.OriginMode = true
		s.curY, s.curX = s.scrollTop, s.leftBound()
	case 7:
		s.AutoWrap = true
	case 12:
		s.CursorBlink = true
	case 3: // DECCOLM — track the mode bit and perform the column-mode change
		s.trackDECMode(3, true)
		s.setColumnMode()
	case 25:
		s.CursorHidden = false
	case 40:
		s.allow80To132 = true // Allow80To132 — gates only the (declined) DECCOLM resize
	case 41:
		s.moreFix = true // more(1) fix — TAB honors a pending wrap
	case 45:
		s.ReverseWrap = true // reverse-wraparound
	case 66:
		s.AppKeypad = true // DECNKM — application keypad (private-mode form of ESC =)
	case 69:
		// DECLRMM — enable DECSLRM left/right margins. A VT level-4 feature, so
		// it is only honored at conformance level 4+ (DECSCL), matching xterm.
		if s.conformanceLevel >= 64 {
			s.LRMarginMode = true
		}
	case 95:
		s.noClearOnColumn = true // DECNCSM — no clear on column-mode change
	case 47, 1047, 1049:
		s.enterAltScreen(mode)
	case 1048:
		s.saveCursor()
	case 1000, 1002, 1003:
		s.MouseMode = uint16(mode) // #nosec G115 -- mode is one of 1000/1002/1003, fits uint16
	case 1004:
		s.FocusReporting = true
	case 1006:
		s.MouseSGR = true
	case 1016:
		s.MousePixels = true // SGR-pixels mouse (client reports pixel coords)
	case 2004:
		s.BracketedPaste = true
	case 2026:
		s.HoldFlush(time.Now().Add(time.Second))
	default:
		// Settable no-op modes (DECSCLM, DECPFF, DECPEX, DECHEBM, DECNRCM,
		// DECBKM): track the set bit so DECRQM reports it. DECCOLM (3) has its
		// own case above.
		if settableDECModes[mode] {
			s.trackDECMode(mode, true)
		}
	}
}

//nolint:gocyclo // mode dispatch is inherently branchy
func (s *Screen) resetMode(mode int) {
	switch mode {
	case 1:
		s.AppCursorKeys = false
	case 5:
		s.ReverseVideo = false
	case 6:
		s.OriginMode = false
		s.curY, s.curX = 0, 0
	case 7:
		s.AutoWrap = false
	case 12:
		s.CursorBlink = false
	case 3: // DECCOLM off — track the bit and perform the column-mode change
		s.trackDECMode(3, false)
		s.setColumnMode()
	case 25:
		s.CursorHidden = true
	case 40:
		s.allow80To132 = false
	case 41:
		s.moreFix = false
	case 45:
		s.ReverseWrap = false
	case 66:
		s.AppKeypad = false // DECNKM off (numeric keypad)
	case 69:
		// DECLRMM off also resets the margins to the full width (xterm).
		s.LRMarginMode = false
		s.leftMargin = 0
		s.rightMargin = s.Width - 1
	case 95:
		s.noClearOnColumn = false
	case 47, 1047, 1049:
		s.exitAltScreen(mode)
		s.Drained = nil
	case 1048:
		s.restoreCursor()
	case 1000:
		if s.MouseMode == 1000 {
			s.MouseMode = 0
		}
	case 1002:
		if s.MouseMode == 1002 {
			s.MouseMode = 0
		}
	case 1003:
		if s.MouseMode == 1003 {
			s.MouseMode = 0
		}
	case 1004:
		s.FocusReporting = false
	case 1006:
		s.MouseSGR = false
	case 1016:
		s.MousePixels = false
	case 2004:
		s.BracketedPaste = false
	case 2026:
		s.ReleaseFlush()
	default:
		// Settable no-op modes: clear the tracked bit so DECRQM reports reset.
		if settableDECModes[mode] {
			s.trackDECMode(mode, false)
		}
	}
}

// trackDECMode records the set/reset bit of a no-op DEC private mode (lazily
// allocating the map) so DECRQM reports a consistent settable state.
func (s *Screen) trackDECMode(mode int, on bool) {
	if s.decModeState == nil {
		s.decModeState = make(map[int]bool)
	}
	s.decModeState[mode] = on
}

// trackANSIMode is trackDECMode's counterpart for the settable no-op ANSI modes
// (KAM, SRM).
func (s *Screen) trackANSIMode(mode int, on bool) {
	if s.ansiModeState == nil {
		s.ansiModeState = make(map[int]bool)
	}
	s.ansiModeState[mode] = on
}

// xtSaveModes implements XTSAVE (CSI ? Pm s): record the current set/reset state
// of each listed DEC private mode so a later XTRESTORE can restore it.
func (s *Screen) xtSaveModes() {
	if s.savedModeValues == nil {
		s.savedModeValues = make(map[int]bool)
	}
	for i := range s.paramCount() {
		mode := s.paramVal(i, 0)
		s.savedModeValues[mode] = s.decModeStatus(mode) == 1
	}
}

// xtRestoreModes implements XTRESTORE (CSI ? Pm r): re-apply each listed DEC
// private mode to the state saved by the last XTSAVE (defaulting to reset for a
// mode that was never saved).
func (s *Screen) xtRestoreModes() {
	for i := range s.paramCount() {
		mode := s.paramVal(i, 0)
		if s.savedModeValues[mode] {
			s.setMode(mode)
		} else {
			s.resetMode(mode)
		}
	}
}

// setColumnMode performs the DECCOLM (?3) column-mode side effects. Only the
// 80<->132 RESIZE is gated on Allow80To132 (?40), and the engine declines it
// regardless (the browser viewport owns the width). The other documented side
// effects — reset the scroll region and left/right margins, home the cursor,
// and erase the screen — happen on every column-mode change, independent of
// ?40: DEC terminals have no Allow80To132 gate (an xterm safety resource for
// the X-window resize), and the xterm-maintained esctest suite asserts the
// clear even when ?40 is unset (test_DECSET_DECNCSM). The erase is suppressed
// only when DECNCSM (?95) is set at conformance level 5 (a VT level-5 feature);
// the cursor-home and margin reset still happen, per DECNCSM's definition
// (it suppresses the clear alone).
func (s *Screen) setColumnMode() {
	s.scrollTop = 0
	s.scrollBottom = s.Height - 1
	s.LRMarginMode = false
	s.leftMargin = 0
	s.rightMargin = s.Width - 1
	s.curX, s.curY = 0, 0
	s.pendingWrap = false
	if s.noClearOnColumn && s.conformanceLevel >= 65 {
		return
	}
	s.eraseRegion(0, 0, s.Height-1, s.Width-1)
	s.Drained = nil
}

// --- DECRQM handler (uses new param structure) ---

// handleDECRQM answers DECRQM (CSI Ps $ p / CSI ? Ps $ p) with a DECRPM report.
// DECRQM is a VT level-3 feature, so at conformance level 2 (DECSCL 62) the
// query is silently ignored — matching xterm, where a level-2 terminal does not
// recognize DECRQM.
func (s *Screen) handleDECRQM() {
	if s.conformanceLevel < 63 {
		return
	}
	mode := s.paramVal(0, 0)
	if s.privateMarker == '?' {
		s.response = fmt.Appendf(s.response, "\x1b[?%d;%d$y", mode, s.decModeStatus(mode))
	} else {
		s.response = fmt.Appendf(s.response, "\x1b[%d;%d$y", mode, s.ansiModeStatus(mode))
	}
}

// --- Cell-level operations used by CSI handlers ---

// insertChars implements ICH (CSI @) and backs IRM insert. It shifts the cells
// from the cursor to the right margin rightward by n, losing cells pushed past
// the right margin. No effect if the cursor is outside the left/right margins.
func (s *Screen) insertChars(n int) {
	if s.curY < 0 || s.curY >= s.Height || !s.withinHMargins(s.curX) {
		return
	}
	right := s.rightBound()
	row := s.Cells[s.curY]
	for x := right; x >= s.curX+n; x-- {
		row[x] = row[x-n]
	}
	for x := s.curX; x < s.curX+n && x <= right; x++ {
		row[x] = Cell{Ch: ' ', Style: Style{BG: s.style.BG}}
	}
}

// deleteChars implements DCH (CSI P): shift cells from the cursor to the right
// margin leftward by n, filling the vacated cells at the right margin with
// blanks. No effect if the cursor is outside the left/right margins.
func (s *Screen) deleteChars(n int) {
	if s.curY < 0 || s.curY >= s.Height || !s.withinHMargins(s.curX) {
		return
	}
	right := s.rightBound()
	row := s.Cells[s.curY]
	n = min(n, right-s.curX+1)
	for x := s.curX; x <= right-n; x++ {
		row[x] = row[x+n]
	}
	for x := right - n + 1; x <= right; x++ {
		row[x] = Cell{Ch: ' ', Style: Style{BG: s.style.BG}}
	}
}

// insertColumns implements DECIC (CSI Ps ' }): insert n blank columns at the
// cursor column in every row of the scroll region, shifting content right;
// content pushed past the right edge is lost. No effect if the cursor is
// outside the vertical scroll region.
func (s *Screen) insertColumns(n int) {
	if s.curY < s.scrollTop || s.curY > s.scrollBottom || !s.withinHMargins(s.curX) {
		return
	}
	right := s.rightBound()
	n = min(n, right-s.curX+1)
	if n <= 0 {
		return
	}
	for y := s.scrollTop; y <= s.scrollBottom; y++ {
		row := s.Cells[y]
		for x := right; x >= s.curX+n; x-- {
			row[x] = row[x-n]
		}
		for x := s.curX; x < s.curX+n; x++ {
			row[x] = Cell{Ch: ' ', Style: Style{BG: s.style.BG}}
		}
	}
}

// deleteColumns implements DECDC (CSI Ps ' ~): delete n columns at the cursor
// column in every row of the scroll region, shifting content left and filling
// the right edge with blanks. No effect if the cursor is outside the vertical
// scroll region.
func (s *Screen) deleteColumns(n int) {
	if s.curY < s.scrollTop || s.curY > s.scrollBottom || !s.withinHMargins(s.curX) {
		return
	}
	right := s.rightBound()
	n = min(n, right-s.curX+1)
	if n <= 0 {
		return
	}
	for y := s.scrollTop; y <= s.scrollBottom; y++ {
		row := s.Cells[y]
		for x := s.curX; x <= right-n; x++ {
			row[x] = row[x+n]
		}
		for x := right - n + 1; x <= right; x++ {
			row[x] = Cell{Ch: ' ', Style: Style{BG: s.style.BG}}
		}
	}
}

// prepareLineShift applies the prologue IL and DL share. Both refuse outside
// the scroll region or the horizontal margins, move the cursor to the left
// margin, clear any deferred wrap, and clamp the count to the rows remaining
// between the cursor and the bottom of the region. Returns the active margins,
// whether they span the full width, the clamped count, and ok=false when the
// operation does not apply. The row-shifting loops stay in each caller: their
// direction is the only thing that differs and it is what the esctest2
// conformance suite pins, so it is deliberately not parameterised behind a flag.
func (s *Screen) prepareLineShift(n int) (left, right, count int, fullWidth, ok bool) {
	if s.curY < s.scrollTop || s.curY > s.scrollBottom || !s.withinHMargins(s.curX) {
		return 0, 0, 0, false, false
	}
	left, right = s.leftBound(), s.rightBound()
	s.curX = left
	s.pendingWrap = false
	return left, right, min(n, s.scrollBottom-s.curY+1), left == 0 && right == s.Width-1, true
}

func (s *Screen) insertLines(n int) {
	left, right, n, fullWidth, ok := s.prepareLineShift(n)
	if !ok {
		return
	}
	for range n {
		if fullWidth {
			for y := s.scrollBottom; y > s.curY; y-- {
				s.Cells[y] = s.Cells[y-1]
				s.wrapped[y] = s.wrapped[y-1]
			}
			s.Cells[s.curY] = makeRow(s.Width, s.style.BG)
			s.wrapped[s.curY] = false
			continue
		}
		for y := s.scrollBottom; y > s.curY; y-- {
			copy(s.Cells[y][left:right+1], s.Cells[y-1][left:right+1])
		}
		s.blankCols(s.curY, left, right)
	}
}

func (s *Screen) deleteLines(n int) {
	left, right, n, fullWidth, ok := s.prepareLineShift(n)
	if !ok {
		return
	}
	for range n {
		if fullWidth {
			for y := s.curY; y < s.scrollBottom; y++ {
				s.Cells[y] = s.Cells[y+1]
				s.wrapped[y] = s.wrapped[y+1]
			}
			s.Cells[s.scrollBottom] = makeRow(s.Width, s.style.BG)
			s.wrapped[s.scrollBottom] = false
			continue
		}
		for y := s.curY; y < s.scrollBottom; y++ {
			copy(s.Cells[y][left:right+1], s.Cells[y+1][left:right+1])
		}
		s.blankCols(s.scrollBottom, left, right)
	}
}

func (s *Screen) scrollUpOnce() {
	left, right := s.leftBound(), s.rightBound()
	if left == 0 && right == s.Width-1 {
		// Full-width scroll: move whole rows; only a full-screen scroll drains
		// to scrollback (partial-height/partial-width content never does).
		if s.scrollTop == 0 && s.scrollBottom == s.Height-1 {
			s.drainTopRow()
		}
		for y := s.scrollTop; y < s.scrollBottom; y++ {
			s.Cells[y] = s.Cells[y+1]
			s.wrapped[y] = s.wrapped[y+1]
		}
		s.Cells[s.scrollBottom] = makeRow(s.Width, s.style.BG)
		s.wrapped[s.scrollBottom] = false
		return
	}
	// Boxed scroll (DECSLRM): shift only [left..right] up within the region.
	// Cell contents move but row identity does not, so the wrap flags stay.
	for y := s.scrollTop; y < s.scrollBottom; y++ {
		copy(s.Cells[y][left:right+1], s.Cells[y+1][left:right+1])
	}
	s.blankCols(s.scrollBottom, left, right)
}

func (s *Screen) lineDown() {
	if s.curY == s.scrollBottom {
		// At the bottom margin, scroll only when the cursor is within the
		// left/right margins; outside the box IND/LF neither scroll nor move.
		if s.withinHMargins(s.curX) {
			s.scrollUpOnce()
		}
		return
	}
	if s.curY < s.Height-1 {
		s.curY++
	}
}

func (s *Screen) scrollDownOnce() {
	left, right := s.leftBound(), s.rightBound()
	if left == 0 && right == s.Width-1 {
		for y := s.scrollBottom; y > s.scrollTop; y-- {
			s.Cells[y] = s.Cells[y-1]
			s.wrapped[y] = s.wrapped[y-1]
		}
		s.Cells[s.scrollTop] = makeRow(s.Width, s.style.BG)
		s.wrapped[s.scrollTop] = false
		return
	}
	// Boxed scroll (DECSLRM): shift only [left..right] down within the region.
	// Cell contents move but row identity does not, so the wrap flags stay.
	for y := s.scrollBottom; y > s.scrollTop; y-- {
		copy(s.Cells[y][left:right+1], s.Cells[y-1][left:right+1])
	}
	s.blankCols(s.scrollTop, left, right)
}

// decBackIndex implements DECBI (ESC 6): move the cursor left one column. At the
// left margin it instead scrolls the margin box right by one column (a blank
// column appears at the left margin); at the screen's left edge (column 0, no
// real margin) it is ignored. DEC STD 070 allows movement outside the margins.
func (s *Screen) decBackIndex() {
	s.pendingWrap = false
	switch left := s.leftBound(); {
	case s.curX == left:
		// At the left margin (or column 0 when there are no margins), scroll the
		// region right; a blank column appears at the margin.
		s.scrollColumnsRight()
	case s.curX > 0:
		s.curX--
	}
}

// decForwardIndex implements DECFI (ESC 9): the mirror of DECBI. At the right
// margin it scrolls the margin box left by one column; at the screen's right
// edge it is ignored.
func (s *Screen) decForwardIndex() {
	s.pendingWrap = false
	switch right := s.rightBound(); {
	case s.curX == right:
		// At the right margin (or the last column when there are no margins),
		// scroll the region left; a blank column appears at the margin.
		s.scrollColumnsLeft()
	case s.curX < s.Width-1:
		s.curX++
	}
}

// scrollColumnsRight shifts columns [left..right] one to the right within the
// vertical scroll region, blanking the left margin column (DECBI at the margin).
func (s *Screen) scrollColumnsRight() {
	left, right := s.leftBound(), s.rightBound()
	for y := s.scrollTop; y <= s.scrollBottom; y++ {
		row := s.Cells[y]
		for x := right; x > left; x-- {
			row[x] = row[x-1]
		}
		row[left] = Cell{Ch: ' ', Style: Style{BG: s.style.BG}}
	}
}

// scrollColumnsLeft shifts columns [left..right] one to the left within the
// vertical scroll region, blanking the right margin column (DECFI at the margin).
func (s *Screen) scrollColumnsLeft() {
	left, right := s.leftBound(), s.rightBound()
	for y := s.scrollTop; y <= s.scrollBottom; y++ {
		row := s.Cells[y]
		for x := left; x < right; x++ {
			row[x] = row[x+1]
		}
		row[right] = Cell{Ch: ' ', Style: Style{BG: s.style.BG}}
	}
}

func (s *Screen) shiftLeft(n int) {
	if n >= s.Width {
		for y := s.scrollTop; y <= s.scrollBottom; y++ {
			for x := range s.Width {
				s.Cells[y][x] = Cell{Ch: ' ', Style: Style{BG: s.style.BG}}
			}
		}
		return
	}
	for y := s.scrollTop; y <= s.scrollBottom; y++ {
		row := s.Cells[y]
		for x := range s.Width - n {
			row[x] = row[x+n]
		}
		for x := s.Width - n; x < s.Width; x++ {
			row[x] = Cell{Ch: ' ', Style: Style{BG: s.style.BG}}
		}
	}
}

func (s *Screen) shiftRight(n int) {
	if n >= s.Width {
		for y := s.scrollTop; y <= s.scrollBottom; y++ {
			for x := range s.Width {
				s.Cells[y][x] = Cell{Ch: ' ', Style: Style{BG: s.style.BG}}
			}
		}
		return
	}
	for y := s.scrollTop; y <= s.scrollBottom; y++ {
		row := s.Cells[y]
		for x := s.Width - 1; x >= n; x-- {
			row[x] = row[x-n]
		}
		for x := range n {
			row[x] = Cell{Ch: ' ', Style: Style{BG: s.style.BG}}
		}
	}
}

func (s *Screen) softReset() {
	s.style = Style{}
	s.scrollTop = 0
	s.scrollBottom = s.Height - 1
	s.mainSaved = savedCursor{}
	s.altSaved = savedCursor{}
	s.pendingWrap = false
	s.CursorHidden = false
	s.CursorStyle = 0
	s.BracketedPaste = false
	s.AppCursorKeys = false
	s.AppKeypad = false
	s.OriginMode = false
	s.AutoWrap = true
	s.ReverseVideo = false
	s.InsertMode = false
	s.curProtected = false
	s.curIsoProtected = false
	s.LineFeedNewLine = false
	s.ReverseWrap = false
	s.LRMarginMode = false
	s.leftMargin = 0
	s.rightMargin = s.Width - 1
	s.allow80To132 = false
	s.noClearOnColumn = false
	s.moreFix = false
	s.MouseMode = 0
	s.MouseSGR = false
	s.MousePixels = false
	s.FocusReporting = false
	// Settable no-op modes (DECBKM, DECSCLM, KAM, SRM, …) return to their reset
	// default so DECRQM reports [mode, 2] after a soft reset.
	s.decModeState = nil
	s.ansiModeState = nil
	// Tab stops are intentionally NOT reset here: DECSTR (soft reset) must
	// preserve them. RIS (hard reset) clears them in its own handler.
	s.resetCharsets()
}

// maxCSIArgValue is the maximum value a single CSI parameter can take.
const maxCSIArgValue = 65535

// maxTitleStack caps the XTWINOPS (CSI 22/23 t) title save/restore stack.
const maxTitleStack = 64

// daFirmwareVersion is the Pv (firmware/patch level) reported in the secondary
// device-attributes reply (CSI > 64 ; Pv ; 0 c). xterm reports its patch level
// here; a value in xterm's historical range keeps version-probing apps happy
// (esctest asserts 314 <= Pv <= 999 for a VT525-class terminal).
const daFirmwareVersion = 410
