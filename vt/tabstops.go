package vt

// --- Tab stop management ---

// initTabStops lazily initializes the tab stop array with default stops every 8 columns.
func (s *Screen) initTabStops() {
	s.tabStops = make([]bool, s.Width)
	for i := 8; i < s.Width; i += 8 {
		s.tabStops[i] = true
	}
}

func (s *Screen) setTabStop(col int) {
	if s.tabStops == nil {
		s.initTabStops()
	}
	if col >= 0 && col < len(s.tabStops) {
		s.tabStops[col] = true
	}
}

func (s *Screen) clearTabStop(col int) {
	if s.tabStops == nil {
		s.initTabStops()
	}
	if col >= 0 && col < len(s.tabStops) {
		s.tabStops[col] = false
	}
}

func (s *Screen) clearAllTabStops() {
	s.tabStops = make([]bool, s.Width)
}

// nextTabStop returns the next tab stop column after col.
// If no tab stops are set, uses default every-8 behavior.
func (s *Screen) nextTabStop(col int) int {
	if s.tabStops == nil {
		return (col + 8) &^ 7
	}
	for i := col + 1; i < s.Width; i++ {
		if s.tabStops[i] {
			return i
		}
	}
	return s.Width
}

// prevTabStop returns the previous tab stop column before col.
func (s *Screen) prevTabStop(col int) int {
	if s.tabStops == nil {
		// Default tab stops every 8 columns, floored at column 0:
		// prevTabStop(0) would otherwise compute (-1 &^ 7) == -8, a negative
		// cursor column that only a downstream clamp currently saves.
		return max((col-1)&^7, 0)
	}
	for i := col - 1; i >= 0; i-- {
		if s.tabStops[i] {
			return i
		}
	}
	return 0
}

// --- DECSC / DECRC cursor save/restore ---

// savedSlot returns the DECSC/DECRC save slot for the active screen. xterm keeps
// the main and alternate screens' saved cursors separate.
func (s *Screen) savedSlot() *savedCursor {
	if s.InAltScreen {
		return &s.altSaved
	}
	return &s.mainSaved
}

// saveCursor saves the full cursor state per DECSC spec: position, style, origin
// mode, autowrap, active charset, the deferred-wrap flag, and DECSCA protection.
func (s *Screen) saveCursor() {
	*s.savedSlot() = savedCursor{
		style:       s.style,
		charsets:    s.gsets,
		x:           s.curX,
		y:           s.curY,
		gl:          s.gl,
		origin:      s.OriginMode,
		pendingWrap: s.pendingWrap,
		protected:   s.curProtected,
		valid:       true,
	}
}

// restoreCursor restores the full cursor state per DECRC spec.
func (s *Screen) restoreCursor() {
	slot := s.savedSlot()
	if !slot.valid {
		// No saved state — move to home per xterm behavior.
		s.curY, s.curX = 0, 0
		s.pendingWrap = false
		return
	}
	s.curY = slot.y
	s.curX = slot.x
	s.style = slot.style
	s.OriginMode = slot.origin
	// DECAWM (autowrap) is intentionally NOT restored: DEC STD 070 has DECSC/DECRC
	// save the cursor, SGR, charset, origin mode, protection, and the last-column
	// flag — but not autowrap. Restoring it would defeat a DECRESET(DECAWM).
	s.gsets = slot.charsets
	s.gl = slot.gl
	s.curProtected = slot.protected
	s.pendingWrap = slot.pendingWrap
	if s.curY >= s.Height {
		s.curY = s.Height - 1
	}
	if s.curX >= s.Width {
		s.curX = s.Width - 1
	}
}

// --- DECRQM (Request Mode) → DECRPM (Report Mode) ---

// decModeStatus returns the DECRPM Ps value for a DEC private mode.
// 1=set, 2=reset, 0=not recognized.
//
//nolint:gocyclo // flat per-mode status dispatch
func (s *Screen) decModeStatus(mode int) int {
	switch mode {
	case 1: // DECCKM
		return boolToModeStatus(s.AppCursorKeys)
	case 5: // DECSCNM
		return boolToModeStatus(s.ReverseVideo)
	case 6: // DECOM
		return boolToModeStatus(s.OriginMode)
	case 7: // DECAWM
		return boolToModeStatus(s.AutoWrap)
	case 12: // Cursor blink (att610)
		return boolToModeStatus(s.CursorBlink)
	case 25: // DECTCEM
		return boolToModeStatus(!s.CursorHidden)
	case 45: // reverse-wraparound
		return boolToModeStatus(s.ReverseWrap)
	case 66: // DECNKM (application keypad)
		return boolToModeStatus(s.AppKeypad)
	case 40: // Allow80To132 (gates the DECCOLM 80<->132 resize, which is declined)
		return boolToModeStatus(s.allow80To132)
	case 69: // DECLRMM (left/right margin mode)
		return boolToModeStatus(s.LRMarginMode)
	case 95: // DECNCSM (no clear on column-mode change)
		return boolToModeStatus(s.noClearOnColumn)
	case 47, 1047, 1049: // Alt screen
		return boolToModeStatus(s.InAltScreen)
	case 1000, 1002, 1003: // Mouse modes
		return boolToModeStatus(s.MouseMode == uint16(mode))
	case 1004: // Focus reporting
		return boolToModeStatus(s.FocusReporting)
	case 1006: // SGR mouse
		return boolToModeStatus(s.MouseSGR)
	case 1016: // SGR-pixels mouse
		return boolToModeStatus(s.MousePixels)
	case 2004: // Bracketed paste
		return boolToModeStatus(s.BracketedPaste)
	case 2026: // Synchronized output
		return boolToModeStatus(s.IsFlushHeld())
	default:
		// Settable no-op modes: report the tracked set/reset bit (default reset).
		if settableDECModes[mode] {
			return boolToModeStatus(s.decModeState[mode])
		}
		// Modes xterm recognizes but we don't implement: report the
		// xterm-canonical DECRPM value so a probing app sees a consistent
		// "recognized" status rather than "unknown".
		if v, ok := recognizedDECModes[mode]; ok {
			return v
		}
		return 0 // not recognized
	}
}

// settableDECModes are DEC private modes the engine recognizes and tracks the
// set/reset bit for (so DECRQM reports a consistent settable state, matching
// xterm) but otherwise implements as no-ops. DECCOLM (3) is the exception: its
// bit is tracked here and it also performs the column-mode clear side effect.
var settableDECModes = map[int]bool{
	3:  true, // DECCOLM (132-column) — bit tracked; 80<->132 resize declined
	4:  true, // DECSCLM (smooth scroll)
	18: true, // DECPFF (print form feed)
	19: true, // DECPEX (print extent)
	35: true, // DECHEBM (Hebrew)
	42: true, // DECNRCM (national replacement charset)
	67: true, // DECBKM (backarrow key)
}

// settableANSIModes are ANSI modes tracked the same way (set/reset bit reported
// via DECRQM) but implemented as no-ops.
var settableANSIModes = map[int]bool{
	2:  true, // KAM (keyboard action mode)
	12: true, // SRM (send/receive mode)
}

// recognizedDECModes maps DEC private modes we recognize but do not implement to
// their fixed xterm DECRPM value: 4 = permanently reset (no implementation).
// Settable no-op modes live in settableDECModes instead.
var recognizedDECModes = map[int]int{
	60: 4, // DECHCCM (horizontal cursor coupling)
}

// ansiModeStatus returns the DECRPM Ps value for an ANSI mode.
func (s *Screen) ansiModeStatus(mode int) int {
	switch mode {
	case 4: // IRM (insert/replace) — report the tracked state
		return boolToModeStatus(s.InsertMode)
	case 20: // LNM (newline mode)
		return boolToModeStatus(s.LineFeedNewLine)
	default:
		// Settable no-op modes (KAM, SRM): report the tracked bit (default reset).
		if settableANSIModes[mode] {
			return boolToModeStatus(s.ansiModeState[mode])
		}
		if v, ok := recognizedANSIModes[mode]; ok {
			return v
		}
		return 0 // not recognized
	}
}

// recognizedANSIModes maps legacy ANSI modes xterm recognizes but does not
// implement to their fixed xterm DECRPM value (4 = permanently reset). Settable
// no-op modes (KAM, SRM) live in settableANSIModes instead.
var recognizedANSIModes = map[int]int{
	1:  4, // GATM
	5:  4, // SRTM
	7:  4, // VEM
	10: 4, // HEM
	11: 4, // PUM
	13: 4, // FEAM
	14: 4, // FETM
	15: 4, // MATM
	16: 4, // TTM
	17: 4, // SATM
	18: 4, // TSM
	19: 4, // EBM
}

func boolToModeStatus(v bool) int {
	if v {
		return 1
	}
	return 2
}
