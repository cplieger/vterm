// Package vt implements a minimal VT100 screen buffer for intercepting
// Ink's cursor-up + overwrite rendering pattern. It maintains a rows×cols
// grid, processes escape sequences to update it, and captures lines that
// scroll off the top (the "scrollback drain").
//
// File layout:
//
//	types.go   — public types (Color, Style, Cell, parser state enum)
//	screen.go  — the Screen struct, ctor, write entry, basic state ops
//	parse.go   — VT500-style byte-at-a-time state machine
//	csi.go     — CSI sequence dispatch + cell-level operations
//	sgr.go     — SGR (color/attribute) parsing + ANSI emission helpers
//	wire.go    — wire format: per-row style runs (WireRun) for the canvas renderer
//	contrast.go — minimum-contrast floor over a run's resolved fg/bg pair
//
// Derived from github.com/tonistiigi/vt100 (MIT license, Docker BuildKit).
package vt

import (
	"math"
	"strings"
	"time"
)

// Screen is a minimal VT100 screen buffer with SGR support.
type Screen struct {
	FlushHoldUntil  time.Time
	ansiModeState   map[int]bool
	paletteOverride map[uint8]int32
	dynColors       map[int]int32
	specialColors   map[int]int32
	decModeState    map[int]bool
	savedModeValues map[int]bool
	Title           string
	iconTitle       string
	hyperlink       string
	// Notification holds the last OSC 9 desktop-notification message (ESC ] 9 ;
	// <text>), stripped of control bytes and length-clamped. The engine stays
	// generic and does not interpret the text; a consumer's status classifier
	// maps it (see NotificationSeq for edge detection).
	Notification string
	tabStops     []bool
	Drained      [][]WireRun
	Cells        [][]Cell
	// wrapped[y] marks row y as a soft-wrap CONTINUATION of row y-1: autowrap
	// (not a hard newline) carried the text onto it. It is the row-chain
	// signal the URL autolinker joins rows with (see wire.go stampAutolinks),
	// the same per-line isWrapped model xterm.js keeps for its link addon.
	// The flag travels with row IDENTITY: every site that shifts whole rows
	// (scroll, drain, IL/DL, resize, alt switch) mirrors its move here, and a
	// row replaced wholesale gets false. In-place content edits (boxed
	// scrolls, rect fills, partial erases) deliberately leave it: a stale
	// true only joins text the regex then fails to match — harmless — while
	// missing a true would break a real wrapped URL. Cleared with history on
	// ED2/ED3/RIS.
	wrapped []bool
	// drainTail retains the text of the most recently drained (scrolled-off)
	// rows while the row now at the top of the screen still continues them,
	// so a URL chain that straddles the scrollback boundary can still be
	// joined when stamping live rows and later drains. nil when the top row
	// starts its own chain. Bounded to maxAutolinkRows-1 entries. Main screen
	// only (alt drains are discarded by the flush builder).
	drainTail []string
	// pendingClipboard holds an OSC 52 copy payload queued for the consumer;
	// drained atomically via TakeClipboard.
	pendingClipboard []byte
	selectionData    []byte
	// response holds bytes the emulator queued for the APPLICATION (query
	// replies: DA, DSR, DECRQSS, XTGETTCAP, kitty progressive-enhancement
	// queries, …); drained atomically via TakeResponse and written back to
	// the PTY by the consumer.
	response   []byte
	titleStack []titleEntry
	// mainKbd/altKbd hold the kitty keyboard-protocol flags and push/pop stack
	// for the main and alternate screens respectively (see kitty.go). They are
	// independent per the spec; activeKbd() selects by InAltScreen.
	mainKbd kbdProtocol
	altKbd  kbdProtocol
	altScreenState
	ParserState
	CursorState
	linesPerScreen   int
	scrollTop        int
	conformanceLevel int
	linesPerPage     int
	scrollBottom     int
	leftMargin       int
	rightMargin      int
	Height           int
	Width            int
	// Progress holds the last ConEmu OSC 9;4 progress state: -1 when none has
	// been seen, else the state (0 off, 1 value, 2 error, 3 indeterminate,
	// 4 paused). kiro-cli emits it while the agent is working, so the status
	// layer maps an active state (1 or 3) to working. Set only by an OSC 9;4
	// sequence; a program that never emits one leaves it at -1 (the signal that
	// the status layer should fall back to output activity).
	Progress int
	// ProgressValue holds the percentage carried by the last OSC 9;4 progress
	// sequence: -1 when absent or unknown (no sequence seen yet, a state that
	// does not carry one — 0 clear and 3 indeterminate — or state 2 in its
	// indeterminate-error form), else 0-100 (a numeric value outside that range
	// is clamped into it). iTerm2 states 1 (value) and 4 (warning) require it;
	// state 2 may carry it. -1 is deliberately distinct from 0: a session that
	// has reported no percentage is not a session at 0%.
	ProgressValue int
	// NotificationSeq increments each time a new OSC 9 notification is captured,
	// so a reader (the status layer) detects a fresh notification even when the
	// message text repeats. Starts at 0 (no notification seen).
	NotificationSeq uint64
	// minContrast is the minimum WCAG contrast ratio enforced between a run's
	// text and its background (contrast.go). 1 disables the floor and is the
	// default; WithMinimumContrast raises it. Sits between two 8-byte-aligned
	// fields on purpose: theme's 12 bytes pack against lastPrintedRune below it,
	// so placing this float64 after theme would cost 8 bytes of padding.
	minContrast float64
	// theme holds the default fg/bg/cursor colors reported by OSC 10/11/12 and
	// restored by OSC 110/111/112. Configurable via WithTheme; DefaultTheme()
	// otherwise (a dark scheme matching web-terminal-ui).
	theme            Theme
	lastPrintedRune  rune
	MouseMode        uint16
	lastPrintedStyle Style
	charsetState
	OriginMode      bool
	noClearOnColumn bool
	titleSetHex     bool
	titleQueryHex   bool
	CursorBlink     bool
	AutoWrap        bool
	CursorStyle     uint8
	// bellRing / scrollbackCleared / paletteChanged are one-shot events
	// (BEL received, ED3 erased the scrollback, OSC 4/104 recolored the
	// palette) drained atomically via their Take* methods.
	bellRing          bool
	scrollbackCleared bool
	MouseSGR          bool
	FocusReporting    bool
	AppKeypad         bool
	ReverseVideo      bool
	pendingWrap       bool
	AppCursorKeys     bool
	InsertMode        bool
	LRMarginMode      bool
	MousePixels       bool
	LineFeedNewLine   bool
	ReverseWrap       bool
	curIsoProtected   bool
	CursorHidden      bool
	BracketedPaste    bool
	allow80To132      bool
	curProtected      bool
	moreFix           bool
	AllowScreenReport bool
	paletteChanged    bool
}

// TakeResponse returns the bytes the emulator queued for the application
// (query replies such as DA, DSR, DECRQSS, XTGETTCAP) and clears the queue.
// One-shot drain: the caller owns the returned slice and writes it back to
// the PTY. Callers must serialize access to the Screen (one mutex-guarded
// owner), as with every other Screen method.
func (s *Screen) TakeResponse() []byte {
	r := s.response
	s.response = nil
	return r
}

// TakeClipboard returns an OSC 52 copy payload queued since the last take
// (nil when none) and clears it. The consumer forwards it to the system
// clipboard (the terminal package emits it as a clipboard wire frame).
func (s *Screen) TakeClipboard() []byte {
	c := s.pendingClipboard
	s.pendingClipboard = nil
	return c
}

// TakeBell reports whether BEL was received since the last take, clearing
// the flag. The consumer typically forwards it as a one-frame bell signal.
func (s *Screen) TakeBell() bool {
	b := s.bellRing
	s.bellRing = false
	return b
}

// TakeScrollbackCleared reports whether the application erased the
// scrollback (ED3 / RIS) since the last take, clearing the flag. The
// consumer discards its retained history to match a real terminal.
func (s *Screen) TakeScrollbackCleared() bool {
	c := s.scrollbackCleared
	s.scrollbackCleared = false
	return c
}

// TakePaletteChanged reports whether OSC 4/104 (or a special-color set/reset)
// changed the palette since the last take, clearing the flag. The consumer
// schedules a full repaint so already-drawn cells re-resolve through the new
// palette.
func (s *Screen) TakePaletteChanged() bool {
	p := s.paletteChanged
	s.paletteChanged = false
	return p
}

// CursorState holds cursor position, saved cursor, and current style.
// Embedded in Screen.
// savedCursor is the full state DECSC/DECRC (and DEC mode 1048) preserve:
// position, SGR, charsets, origin/autowrap modes, the deferred-wrap flag, and
// the DECSCA protection attribute. xterm keeps a separate slot for the main and
// alternate screens (see savedSlot).
type savedCursor struct {
	style       Style
	charsets    [4]charset
	x           int
	y           int
	gl          uint8
	origin      bool
	pendingWrap bool
	protected   bool
	valid       bool
}

// CursorState holds the cursor position, the per-screen saved cursors
// (DECSC/DECRC), and the current SGR style. Embedded in Screen.
type CursorState struct {
	style     Style
	mainSaved savedCursor
	altSaved  savedCursor
	curY      int
	curX      int
}

// altScreenState holds alt-screen save/restore state. Embedded in Screen.
type altScreenState struct {
	savedMainCells [][]Cell
	// savedMainWrapped mirrors savedMainCells for the per-row soft-wrap
	// flags, so main-screen wrap chains survive an alt-screen session.
	savedMainWrapped []bool
	// altCells is the persistent alternate-screen buffer. It survives across
	// switches so re-entering via mode 47 shows the prior contents; modes 1047
	// and 1049 clear it (on exit / on enter respectively). While InAltScreen is
	// true it aliases s.Cells. nil until the alt screen is first entered.
	altCells              [][]Cell
	savedMainCurX         int
	savedMainCurY         int
	savedMainScrollTop    int
	savedMainScrollBottom int
	savedMainStyle        Style
	// InAltScreen indicates the alternate screen buffer is active.
	InAltScreen bool
}

// Option configures a Screen at construction time. Pass options to New.
type Option func(*Screen)

// WithTheme sets the default foreground, background, and cursor colors the
// screen reports on OSC 10/11/12 queries and restores on OSC 110/111/112 reset.
// Consumers should pass their real rendering colors so color-probing apps see
// the terminal's actual appearance. Defaults to DefaultTheme.
func WithTheme(t Theme) Option {
	return func(s *Screen) { s.theme = t }
}

// DefaultTheme is the built-in dark color scheme (light-grey text on a black
// background) matching web-terminal-ui's default CSS, used when no WithTheme
// option is given.
func DefaultTheme() Theme {
	return Theme{
		Foreground: RGB(0xdd, 0xde, 0xe1),
		Background: RGB(0x00, 0x00, 0x00),
		Cursor:     RGB(0xdd, 0xde, 0xe1),
	}
}

// MinimumContrastOff disables the minimum-contrast floor. It is the default, so
// the engine renders exactly the palette entry an application selected.
const MinimumContrastOff = 1.0

// WithMinimumContrast sets a floor on the WCAG contrast ratio between a run's
// text and its background. A foreground below the floor is blended toward white
// or black until it reaches it (see contrast.go); backgrounds are never changed,
// and neither is a DEFAULT foreground, which the client's own CSS owns.
//
// The ratio is clamped to 1..21. 1 is off and the default, matching xterm.js's
// minimumContrastRatio, so upgrading the engine never silently recolors an
// existing consumer. 4.5 is the WCAG AA floor for body text and the value VS
// Code applies to every integrated terminal; pass it when your client renders on
// a dark background and your users read output they do not control.
//
// Reach for this in addition to, not instead of, a palette suited to your
// background: the floor is what covers an application's OSC 4 overrides, the
// dark corners of the 256-color cube, and truecolor an application picked blind.
// The engine's own basic-16 default already clears 4.5:1 on black in 13 of 16
// slots.
func WithMinimumContrast(ratio float64) Option {
	return func(s *Screen) {
		if math.IsNaN(ratio) {
			s.minContrast = MinimumContrastOff
			return
		}
		s.minContrast = math.Min(21, math.Max(MinimumContrastOff, ratio))
	}
}

// New creates a screen buffer of the given dimensions. Optional behavior (e.g.
// the reported color theme) is configured via functional Option values.
func New(rows, cols int, opts ...Option) *Screen {
	s := &Screen{Height: rows, Width: cols, Cells: make([][]Cell, rows), wrapped: make([]bool, rows), scrollTop: 0, scrollBottom: rows - 1, rightMargin: cols - 1, conformanceLevel: 65, AutoWrap: true, CursorBlink: true, theme: DefaultTheme(), minContrast: MinimumContrastOff}
	s.singleShft = -1
	s.Progress = progressAbsent      // no OSC 9;4 progress seen yet
	s.ProgressValue = progressAbsent // no OSC 9;4 percentage seen yet (absent/unknown)
	for i := range s.Cells {
		s.Cells[i] = makeRow(cols, Color{})
	}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	return s
}

// Write processes raw PTY output one byte at a time, updating the screen buffer.
func (s *Screen) Write(dt []byte) (int, error) {
	for _, b := range dt {
		s.feed(b)
	}
	return len(dt), nil
}

// DrainScrollback returns and clears accumulated scrolled-off lines.
func (s *Screen) DrainScrollback() [][]WireRun {
	d := s.Drained
	s.Drained = nil
	return d
}

// HoldFlush requests that the flush loop skip flushing the screen until
// the given time. Used to hide partial state during atomic batches —
// the caller is CSI ?2026h ("synchronized output mode"). Subsequent calls
// extend the hold but never shorten it. (The resize/SIGWINCH redraw window
// is hidden by the terminal handler's own settle hold, not this one: this
// hold is cleared by the child's next CSI ?2026l, which would expose the
// rest of a bracketed multi-chunk redraw.)
func (s *Screen) HoldFlush(until time.Time) {
	if until.After(s.FlushHoldUntil) {
		s.FlushHoldUntil = until
	}
}

// ReleaseFlush clears any pending flush hold (called on CSI ?2026l).
func (s *Screen) ReleaseFlush() {
	s.FlushHoldUntil = time.Time{}
}

// IsFlushHeld reports whether the flush gate is currently held.
func (s *Screen) IsFlushHeld() bool {
	return time.Now().Before(s.FlushHoldUntil)
}

// CursorPos returns the current cursor row and column (0-indexed).
func (s *Screen) CursorPos() (row, col int) {
	return s.curY, s.curX
}

// Resize adjusts the screen dimensions, preserving existing content where
// possible. When dimensions actually change, cells are cleared so the host application's
// SIGWINCH redraw starts from a clean slate; on a no-op resize (e.g. client
// reconnect at the same size), content is preserved.
func (s *Screen) Resize(rows, cols int) {
	cols = max(cols, 1)
	rows = max(rows, 1)
	s.resizeHeight(rows)
	s.resizeWidth(cols)
	if s.curY >= s.Height {
		s.curY = s.Height - 1
	}
	if s.curX >= s.Width {
		s.curX = s.Width - 1
	}
	// Reset scroll region to full screen on resize.
	s.scrollTop = 0
	s.scrollBottom = s.Height - 1
	// Reset the left/right margins to the full width; a dimension change
	// invalidates any DECSLRM box (xterm does the same on DECCOLM).
	s.leftMargin = 0
	s.rightMargin = s.Width - 1
	// Note: we deliberately do NOT clear cells or reset the cursor here.
	// the host application starts at the correct dimensions (first resize message
	// triggers ensureStarted) so initial-paint stale content is no longer
	// a concern. SIGWINCH will trigger the host application to redraw, which will
	// overwrite cells in place. Clearing here causes a visible "blank
	// screen + cursor at top-left" flash on every keyboard transition.
	s.resizeSavedMain(rows, cols)
}

// resizeHeight adjusts the buffer to the new row count, preserving content.
// The grow behaviour is mode-aware, because a full-screen app and a
// line-oriented shell want opposite things:
//
//   - Alt-screen (a TUI like kiro-cli, vim, htop): prepend empty rows at the
//     TOP and move the cursor down with the content, so the existing screen
//     stays pinned to the bottom until the app's SIGWINCH-driven repaint lands.
//     Appending BELOW the cursor instead left empty rows visible until the
//     repaint — the "black gap between content and the input bar" seen on an
//     iPhone → iPad device switch. Combined with the client's trim of trailing
//     empty rows in render.ts, this keeps that gap impossible.
//
//   - Normal screen (a shell like bash/sh): append empty rows at the BOTTOM and
//     leave the cursor on its row, so existing output stays anchored at the TOP
//     the way xterm/iTerm/Terminal.app show a shell. A shell does not repaint
//     its scrollback on SIGWINCH, so prepending at the top would strand the
//     prompt in the middle of the screen with a band of blank rows above it
//     (the reported "shell shows in the middle of the page" bug): the PTY
//     starts at defaultRows and the first client resize grows it, so the very
//     first prompt would otherwise land mid-screen. The client trims the
//     trailing rows we add here, so there is no bottom gap in this mode either.
//
// Shrinking drops rows from the bottom in both modes.
func (s *Screen) resizeHeight(rows int) {
	if rows > s.Height {
		grow := rows - s.Height
		newRows := make([][]Cell, grow, grow+s.Height)
		for i := range newRows {
			newRows[i] = makeRow(s.Width, Color{})
		}
		if s.InAltScreen {
			// Keep the alt-screen content pinned to the bottom; the app
			// repaints on the SIGWINCH that follows.
			s.Cells = append(newRows, s.Cells...)
			s.wrapped = append(make([]bool, grow), s.wrapped...)
			s.curY += grow
		} else {
			// Keep the shell's output anchored at the top; the cursor stays on
			// its row and the trailing blanks are trimmed client-side.
			s.Cells = append(s.Cells, newRows...)
			s.wrapped = append(s.wrapped, make([]bool, grow)...)
		}
		s.Height = rows
	}
	if rows < s.Height {
		s.Cells = s.Cells[:rows]
		s.wrapped = s.wrapped[:rows]
		s.Height = rows
	}
}

// resizeWidth adjusts the buffer to the new column count, preserving each row's
// content. Tab stops are preserved and extended with the default every-8
// pattern for newly exposed columns.
func (s *Screen) resizeWidth(cols int) {
	if cols == s.Width {
		return
	}
	for i := range s.Cells {
		old := s.Cells[i]
		s.Cells[i] = makeRow(cols, Color{})
		copy(s.Cells[i], old)
	}
	s.Width = cols
	if s.tabStops == nil {
		return
	}
	newStops := make([]bool, cols)
	copy(newStops, s.tabStops)
	for i := len(s.tabStops); i < cols; i++ {
		if i > 0 && i%8 == 0 {
			newStops[i] = true
		}
	}
	s.tabStops = newStops
}

// resizeSavedMain rebuilds the saved main-screen buffer to the new dimensions
// while in alt-screen mode, so exiting alt-screen restores correctly at the new
// size. rows and cols are the already-clamped target dimensions.
func (s *Screen) resizeSavedMain(rows, cols int) {
	if s.savedMainCells == nil {
		return
	}
	resized := make([][]Cell, rows)
	for i := range resized {
		row := makeRow(cols, Color{})
		if i < len(s.savedMainCells) {
			copy(row, s.savedMainCells[i])
		}
		resized[i] = row
	}
	s.savedMainCells = resized
	if s.savedMainCurY >= rows {
		s.savedMainCurY = rows - 1
	}
	if s.savedMainCurX >= cols {
		s.savedMainCurX = cols - 1
	}
}

// RenderViewport returns the entire screen as ANSI-colored text. Used by
// tests and the legacy debug endpoint.
func (s *Screen) RenderViewport() string {
	var buf strings.Builder
	for y := range s.Cells {
		var prev Style
		for x, cell := range s.Cells[y] {
			if x == 0 || cell.Style != prev {
				buf.WriteString(sgrSequence(cell.Style))
			}
			prev = cell.Style
			ch := cell.Ch
			if ch == 0 { // wide-char spacer cell -> render as a space, matching RowString/reportRectChecksum
				ch = ' '
			}
			buf.WriteRune(ch)
		}
		buf.WriteString("\x1b[0m")
		if y < len(s.Cells)-1 {
			buf.WriteString("\r\n")
		}
	}
	return buf.String()
}

// clearWrapState resets every soft-wrap continuation flag and drops the
// retained cross-boundary chain tail. Called where the screen's content and
// its relationship to history are severed wholesale (ED2, RIS).
func (s *Screen) clearWrapState() {
	for i := range s.wrapped {
		s.wrapped[i] = false
	}
	s.drainTail = nil
}

// RowString returns the text content of a row (no styling).
func (s *Screen) RowString(y int) string {
	if y < 0 || y >= len(s.Cells) {
		return ""
	}
	var buf strings.Builder
	for _, cell := range s.Cells[y] {
		ch := cell.Ch
		if ch == 0 {
			ch = ' '
		}
		buf.WriteRune(ch)
	}
	return strings.TrimRight(buf.String(), " ")
}

// --- Cell-level helpers used across files ---

// leftBound and rightBound return the active horizontal scrolling bounds: the
// DECSLRM left/right margins when DECLRMM is set, otherwise the full width.
// Every column-clamp, wrap, and scroll decision goes through these so the
// left/right-margin box and the normal full-width path share one code path.
func (s *Screen) leftBound() int {
	if s.LRMarginMode {
		return s.leftMargin
	}
	return 0
}

func (s *Screen) rightBound() int {
	if s.LRMarginMode {
		return s.rightMargin
	}
	return s.Width - 1
}

// withinHMargins reports whether column x lies within [leftBound, rightBound].
func (s *Screen) withinHMargins(x int) bool {
	return x >= s.leftBound() && x <= s.rightBound()
}

// wrapEdge is the column at which autowrap triggers for the current cursor: the
// right margin when the cursor is inside the left/right margins, else the
// screen's last column (text outside the margins wraps at the screen edge).
func (s *Screen) wrapEdge() int {
	if s.withinHMargins(s.curX) {
		return s.rightBound()
	}
	return s.Width - 1
}

// wrapColumn is the column autowrap lands on after crossing wrapEdge: the left
// margin when wrapping inside the box, else column 0.
func (s *Screen) wrapColumn() int {
	if s.withinHMargins(s.curX) {
		return s.leftBound()
	}
	return 0
}

func (s *Screen) put(r rune) {
	w := runeWidth(r)

	// Width-0: combining mark — no column consumed.
	if w == 0 {
		return
	}

	// Pending wrap: if the previous put landed the cursor on the wrap edge and
	// another char arrives, wrap to the next line first (to the left margin when
	// inside the box). xterm.js behavior.
	if s.pendingWrap {
		s.pendingWrap = false
		s.curX = s.wrapColumn()
		s.curY++
		s.scrollIfNeeded()
		s.markWrapContinuation()
	}

	// Width-2: if only 1 cell remains before the wrap edge, wrap first.
	if w == 2 && s.curX >= s.wrapEdge() && s.AutoWrap {
		s.Cells[s.curY][s.curX] = Cell{Ch: ' ', Style: s.style}
		s.curX = s.wrapColumn()
		s.curY++
		s.scrollIfNeeded()
		s.markWrapContinuation()
	}

	// IRM (ANSI insert mode, CSI 4 h): shift the row right by the glyph width
	// at the cursor so the new character is inserted rather than overwriting;
	// cells pushed past the right margin are lost (xterm behavior).
	if s.InsertMode {
		s.insertChars(w)
	}

	if s.curY < s.Height && s.curX < s.Width {
		s.Cells[s.curY][s.curX] = Cell{Ch: r, Style: s.style, Hyperlink: s.hyperlink, Protected: s.curProtected, IsoProtected: s.curIsoProtected}
	}
	s.lastPrintedRune = r
	s.lastPrintedStyle = s.style

	if w == 2 {
		// Place spacer/continuation cell.
		s.curX++
		if s.curX < s.Width && s.curY < s.Height {
			s.Cells[s.curY][s.curX] = Cell{Ch: 0, Style: s.style, Hyperlink: s.hyperlink, Protected: s.curProtected, IsoProtected: s.curIsoProtected}
		}
	}

	// Advance, arming a deferred wrap at the effective right edge. The edge is
	// the right margin inside the box and the screen edge outside it; on very
	// narrow screens a width-2 spacer can push curX past the last column, so the
	// >= Width case clamps defensively.
	edge := s.wrapEdge()
	switch {
	case s.curX >= s.Width:
		s.curX = s.Width - 1
		s.pendingWrap = s.AutoWrap
	case s.curX >= edge:
		// At the wrap edge, arm a deferred wrap only when autowrap (DECAWM) is
		// on. With DECAWM off the cursor stays and the next printable overwrites.
		s.curX = edge
		s.pendingWrap = s.AutoWrap
	default:
		s.curX++
	}
}

func (s *Screen) scrollIfNeeded() {
	if s.curX >= s.Width {
		s.curX = 0
		s.curY++
	}
	if s.curY > s.scrollBottom {
		s.scrollUpOnce()
		s.curY = s.scrollBottom
	}
	if s.curY >= s.Height {
		s.drainTopRow()
		copy(s.Cells, s.Cells[1:])
		s.Cells[s.Height-1] = makeRow(s.Width, s.style.BG)
		copy(s.wrapped, s.wrapped[1:])
		s.wrapped[s.Height-1] = false
		s.curY = s.Height - 1
	}
}

// markWrapContinuation records that the row the cursor just wrapped ONTO is a
// soft continuation of the row above it. Called at the two autowrap sites in
// put(), after any scroll has settled the cursor row. Only a true full-row
// wrap chains rows for the autolinker; a wrap inside a DECSLRM left/right
// margin box continues box content, not the row, so it is not recorded.
func (s *Screen) markWrapContinuation() {
	if s.leftBound() != 0 || s.rightBound() != s.Width-1 {
		return
	}
	if s.curY >= 0 && s.curY < len(s.wrapped) {
		s.wrapped[s.curY] = true
	}
}

// drainTopRow commits row 0 to the pending scrollback drain (stamping any
// detected URLs first, chain-joined across the drain boundary) and maintains
// drainTail: when the row about to become the new top row soft-continues the
// drained one, the drained row's text is retained so the chain can still be
// joined; otherwise the retained tail is dropped. Alt-screen drains are
// discarded by the flush builder, so the tail only tracks the main screen.
func (s *Screen) drainTopRow() {
	// Row 0's trailing blanks are content, not padding, when row 1 soft-wraps
	// from it — the same flag that decides whether the drain tail is retained
	// for chain joining, read once for both.
	continuesBelow := len(s.wrapped) > 1 && s.wrapped[1]
	runs := s.stampAutolinks(0, s.cellsToRuns(s.Cells[0], continuesBelow))
	s.Drained = append(s.Drained, runs)
	if s.InAltScreen {
		return
	}
	if continuesBelow {
		s.drainTail = append(s.drainTail, rowMatchText(s.Cells[0]))
		if len(s.drainTail) > maxAutolinkRows-1 {
			s.drainTail = s.drainTail[len(s.drainTail)-(maxAutolinkRows-1):]
		}
	} else {
		s.drainTail = nil
	}
}

// eraseMode selects which per-cell protection attributes an erase spares.
type eraseMode uint8

const (
	eraseAll         eraseMode = iota // clear every cell (plain ED 2, scroll blanks, RIS)
	eraseSpareISO                     // spare ISO (SPA/EPA) guarded cells — regular ED 0/1, EL, ECH
	eraseSpareDECSCA                  // spare DECSCA-protected cells — DECSERA
	eraseSpareBoth                    // spare DECSCA OR ISO — DECSED/DECSEL (xterm backward-compat)
)

// eraseRegionMode blanks the rectangle [y1,x1]..[y2,x2], honoring the given
// protection mode. Coordinates outside the screen are skipped.
//
//nolint:gocognit // per-cell bounds + protection-mode guard over a 2-D region
func (s *Screen) eraseRegionMode(y1, x1, y2, x2 int, mode eraseMode) {
	blank := Cell{Ch: ' ', Style: Style{BG: s.style.BG}}
	for y := y1; y <= y2; y++ {
		if y < 0 || y >= s.Height {
			continue
		}
		row := s.Cells[y]
		for x := x1; x <= x2; x++ {
			if x < 0 || x >= s.Width {
				continue
			}
			switch mode {
			case eraseSpareISO:
				if row[x].IsoProtected {
					continue
				}
			case eraseSpareDECSCA:
				if row[x].Protected {
					continue
				}
			case eraseSpareBoth:
				if row[x].Protected || row[x].IsoProtected {
					continue
				}
			case eraseAll:
			}
			row[x] = blank
		}
	}
}

// eraseRegion clears the rectangle unconditionally (no protection). Used by the
// scroll blanks, RIS, and other full clears.
func (s *Screen) eraseRegion(y1, x1, y2, x2 int) {
	s.eraseRegionMode(y1, x1, y2, x2, eraseAll)
}

func makeRow(cols int, bg Color) []Cell {
	r := make([]Cell, cols)
	for i := range r {
		r[i] = Cell{Ch: ' ', Style: Style{BG: bg}}
	}
	return r
}

// blankCols clears columns [x1, x2] of row y to a blank cell with the current
// background. Used by the boxed (left/right-margin) scroll/insert/delete paths.
func (s *Screen) blankCols(y, x1, x2 int) {
	if y < 0 || y >= s.Height {
		return
	}
	row := s.Cells[y]
	for x := x1; x <= x2 && x < s.Width; x++ {
		if x >= 0 {
			row[x] = Cell{Ch: ' ', Style: Style{BG: s.style.BG}}
		}
	}
}

// enterAltScreen saves the main-screen state and switches to the alternate
// buffer. The three DECSET modes differ (matching xterm):
//   - 47 / 1047: the cursor and SGR are shared with the main screen (not moved
//     or reset); the alt buffer is NOT cleared on entry, and mode 47 preserves
//     it across switches.
//   - 1049: saves the cursor (restored on exit), clears the alt buffer, and
//     homes the cursor in it.
//
// Idempotent: a second set while already in alt is a no-op.
func (s *Screen) enterAltScreen(mode int) {
	if s.InAltScreen {
		return
	}
	// Save main-screen cells (deep copy — the alt buffer mutates in place) and
	// the main cursor/scroll region for restore on exit.
	saved := make([][]Cell, len(s.Cells))
	for i, row := range s.Cells {
		saved[i] = append([]Cell(nil), row...)
	}
	s.savedMainCells = saved
	s.savedMainWrapped = append([]bool(nil), s.wrapped...)
	s.savedMainCurY = s.curY
	s.savedMainCurX = s.curX
	s.savedMainStyle = s.style
	s.savedMainScrollTop = s.scrollTop
	s.savedMainScrollBottom = s.scrollBottom

	// Switch to the persistent alt buffer, creating it on first use. 1049 always
	// starts cleared; a dimension change since the last session (the stashed
	// buffer no longer matches the screen) also forces a fresh buffer.
	if mode == 1049 || !s.altCellsFit() {
		s.altCells = make([][]Cell, s.Height)
		for i := range s.altCells {
			s.altCells[i] = makeRow(s.Width, Color{})
		}
	}
	s.Cells = s.altCells
	// The alt session starts with no wrap chains; a mode-47 re-enter reusing
	// stashed alt CONTENT loses its old chains (a legacy mode, cosmetic only —
	// a stale-false flag just skips autolink joining).
	s.wrapped = make([]bool, s.Height)
	s.scrollTop = 0
	s.scrollBottom = s.Height - 1
	if mode == 1049 {
		// 1049 homes the cursor and resets SGR in the cleared alt buffer; 47 and
		// 1047 leave both shared with the main screen.
		s.curY, s.curX = 0, 0
		s.style = Style{}
	}
	s.InAltScreen = true
}

// altCellsFit reports whether the stashed alt buffer matches the current screen
// dimensions, so it can be reused; a resize since the last alt session leaves
// it stale and forces a fresh buffer.
func (s *Screen) altCellsFit() bool {
	return len(s.altCells) == s.Height && s.Height > 0 && len(s.altCells[0]) == s.Width
}

// exitAltScreen restores the saved main-screen state. Mode governs the alt
// buffer's fate and whether the cursor is restored (see enterAltScreen): 1047
// and 1049 clear the alt buffer on exit, 47 keeps it; only 1049 restores the
// saved cursor (47/1047 leave the cursor shared with the alt session).
func (s *Screen) exitAltScreen(mode int) {
	if !s.InAltScreen || s.savedMainCells == nil {
		return
	}
	// Persist (or discard) the alt buffer for a later re-enter.
	s.altCells = s.Cells
	if mode == 1047 || mode == 1049 {
		s.altCells = nil
	}
	// Resize-resilient restore: if dimensions changed while in alt,
	// truncate or pad rows to match current height.
	restored := make([][]Cell, s.Height)
	for i := range restored {
		switch {
		case i < len(s.savedMainCells) && len(s.savedMainCells[i]) == s.Width:
			restored[i] = s.savedMainCells[i]
		case i < len(s.savedMainCells):
			// Width changed — copy what fits, pad with spaces.
			row := makeRow(s.Width, Color{})
			copy(row, s.savedMainCells[i])
			restored[i] = row
		default:
			restored[i] = makeRow(s.Width, Color{})
		}
	}
	s.Cells = restored
	// Restore the main screen's wrap flags alongside its cells, padded or
	// truncated to the current height like the cells above.
	restoredWrapped := make([]bool, s.Height)
	copy(restoredWrapped, s.savedMainWrapped)
	s.wrapped = restoredWrapped
	s.savedMainWrapped = nil
	s.style = s.savedMainStyle
	s.scrollTop = s.savedMainScrollTop
	s.scrollBottom = s.savedMainScrollBottom
	if mode == 1049 {
		s.curY = s.savedMainCurY
		s.curX = s.savedMainCurX
	}
	if s.curY >= s.Height {
		s.curY = s.Height - 1
	}
	if s.curX >= s.Width {
		s.curX = s.Width - 1
	}
	if s.scrollBottom >= s.Height {
		s.scrollBottom = s.Height - 1
	}
	s.savedMainCells = nil
	s.InAltScreen = false
}
