package vt

// VT500-style table-driven state machine parser. Processes raw PTY bytes one
// at a time via a [14][256]uint16 flat transition table. Maintains state across
// Write() calls so partial sequences split across reads are handled correctly.

func (s *Screen) feed(b byte) {
	// UTF-8 continuation handling (only in Ground state).
	if s.utf8Len > 0 {
		if b&0xC0 == 0x80 {
			s.utf8Buf[s.utf8Got] = b
			s.utf8Got++
			if s.utf8Got == s.utf8Len {
				r := decodeUTF8Bytes(s.utf8Buf, s.utf8Len)
				s.utf8Len = 0
				s.put(r)
			}
			return
		}
		// Invalid continuation — the in-progress multi-byte sequence is
		// truncated. Emit one U+FFFD for the ill-formed lead (matching the
		// U+FFFD error model every other malformed-UTF-8 path uses: C1 bytes,
		// orphan continuations, and decodeUTF8Bytes), then re-process the
		// current byte in Ground.
		s.utf8Len = 0
		s.put(0xFFFD)
	}

	t := stateTable[s.pState][b]
	act := t.act()
	next := t.next()

	// CAN (0x18) and SUB (0x1A) abort sequences without firing exit actions.
	isCancelByte := b == 0x18 || b == 0x1A

	// Exit action of current state (if transitioning), but NOT on CAN/SUB.
	if next != s.pState && !isCancelByte {
		if ea := exitAction[s.pState]; ea != actNone {
			s.doAction(ea, b)
		}
	}

	if act != actNone {
		s.doAction(act, b)
	}

	if next != s.pState {
		s.pState = next
		if ea := entryAction[next]; ea != actNone {
			s.doAction(ea, b)
		}
	}
}

//nolint:gocyclo // action dispatch is inherently branchy
func (s *Screen) doAction(act action, b byte) {
	switch act {
	case actPrint:
		s.handlePrint(b)
	case actExecute:
		s.execControl(b)
	case actClear:
		s.parserClear()
	case actCollect:
		if s.numInterm < maxIntermed {
			s.pIntermed[s.numInterm] = b
			s.numInterm++
		}
	case actParam:
		s.handleParam(b)
	case actSubparam:
		s.handleSubparam()
	case actMarker:
		s.privateMarker = b
	case actEscDispatch:
		s.dispatchEsc(b)
	case actCsiDispatch:
		s.finalizeParams()
		s.dispatchCSI(b)
	case actHook:
		s.finalizeParams()
		s.dcsHook(b)
	case actPut:
		s.dcsPut(b)
	case actUnhook:
		s.dcsUnhook()
	case actOscStart:
		s.oscBuf = s.oscBuf[:0]
	case actOscPut:
		if len(s.oscBuf) < maxOSCLen {
			s.oscBuf = append(s.oscBuf, b)
		}
	case actOscEnd:
		s.dispatchOsc()
	case actIgnore:
		// do nothing
	}
}

func (s *Screen) handlePrint(b byte) {
	switch {
	case b < 0x80:
		s.put(s.translateChar(b))
	case b >= 0x80 && b < 0xC0:
		// Orphan continuation byte or C1 range (0x80-0x9F) arriving in Ground
		// when not inside a multi-byte sequence — emit replacement character.
		s.put(0xFFFD)
	case b >= 0xC0 && b < 0xE0:
		s.utf8Buf[0] = b
		s.utf8Len = 2
		s.utf8Got = 1
	case b >= 0xE0 && b < 0xF0:
		s.utf8Buf[0] = b
		s.utf8Len = 3
		s.utf8Got = 1
	case b >= 0xF0 && b < 0xF8:
		s.utf8Buf[0] = b
		s.utf8Len = 4
		s.utf8Got = 1
	default:
		s.put(0xFFFD)
	}
}

func (s *Screen) handleParam(b byte) {
	if s.ignoring {
		return
	}
	if b == ';' {
		s.pushParam(true)
	} else {
		s.paramSeen = true
		v := min(uint32(s.curParam)*10+uint32(b-'0'), maxCSIArgValue)
		// min caps v at maxCSIArgValue (65535), so the narrowing cannot overflow.
		s.curParam = uint16(v)
	}
}

func (s *Screen) handleSubparam() {
	if s.ignoring {
		return
	}
	s.pushParam(false)
}

// pushParam stores curParam in the flat array. newGroup=true means start a new
// semicolon-separated group after this value.
func (s *Screen) pushParam(newGroup bool) {
	if s.numParams >= maxParams {
		s.ignoring = true
		return
	}
	s.pParams[s.numParams] = s.curParam
	s.numParams++
	if s.numGroups > 0 {
		gStart := s.groupStartIdx(s.numGroups - 1)
		s.pGroupLen[gStart]++
	}
	s.curParam = 0
	s.paramSeen = false

	if newGroup {
		if s.numGroups < maxParams && s.numParams < maxParams {
			s.pGroupLen[s.numParams] = 0
			s.numGroups++
		}
	}
}

// finalizeParams always pushes the trailing param (even an empty/default 0). A bare
// "CSI m" therefore yields one group [0] (paramCount() == 1, not zero); applySGR's
// reset detection relies on numParams == 1 && pParams[0] == 0.
func (s *Screen) finalizeParams() {
	if s.ignoring {
		return
	}
	if s.numParams >= maxParams {
		return
	}
	s.pParams[s.numParams] = s.curParam
	s.numParams++
	if s.numGroups > 0 {
		gStart := s.groupStartIdx(s.numGroups - 1)
		s.pGroupLen[gStart]++
	}
}

func (s *Screen) parserClear() {
	s.numParams = 0
	s.numGroups = 0
	s.curParam = 0
	s.paramSeen = false
	s.numInterm = 0
	s.ignoring = false
	s.privateMarker = 0
	s.pGroupLen[0] = 0
	s.numGroups = 1
}

// groupStartIdx returns the pParams index where semicolon-group g starts: the
// sum of the lengths of all groups before g. Each group stores its length in
// pGroupLen at its own start index, so walk start(0)=0,
// start(k+1)=start(k)+pGroupLen[start(k)] — the same walk paramGroup uses.
func (s *Screen) groupStartIdx(g uint8) uint8 {
	var idx uint8
	for range g {
		idx += s.pGroupLen[idx]
	}
	return idx
}

// ParamGroup represents one semicolon-separated parameter group.
type ParamGroup struct {
	Params [8]uint16 // Params[0] = main param; Params[1:Len] = subparams
	Len    uint8
}

// paramGroup returns the i-th semicolon-separated group (0-indexed).
func (s *Screen) paramGroup(i int) ParamGroup {
	var g ParamGroup
	if i >= int(s.numGroups) {
		return g
	}
	var startIdx uint8
	for range i {
		if int(startIdx) >= maxParams {
			return g
		}
		startIdx += s.pGroupLen[startIdx]
	}
	if int(startIdx) >= maxParams {
		return g
	}
	length := s.pGroupLen[startIdx]
	if length == 0 {
		return g
	}
	length = min(length, 8, uint8(maxParams-int(startIdx)))
	g.Len = length
	for j := range length {
		g.Params[j] = s.pParams[startIdx+j]
	}
	return g
}

// paramCount returns the number of semicolon-separated groups.
func (s *Screen) paramCount() int {
	return int(s.numGroups)
}

// paramVal returns the main value of the i-th group, or def if absent/zero.
func (s *Screen) paramVal(i, def int) int {
	g := s.paramGroup(i)
	if g.Len == 0 {
		return def
	}
	v := int(g.Params[0])
	if v == 0 && def > 0 {
		return def
	}
	if v > maxCSIArgValue {
		return maxCSIArgValue
	}
	return v
}

func (s *Screen) execControl(b byte) {
	switch b {
	case 0x07:
		s.bellRing = true
	case '\b':
		s.backspace()
	case '\n', 0x0B, 0x0C: // LF, VT, FF
		s.pendingWrap = false
		if s.LineFeedNewLine {
			// LNM (ANSI mode 20): a line feed also carriage-returns.
			s.curX = 0
		}
		s.lineDown()
	case '\r':
		// CR moves to the left margin when the cursor is at or right of it,
		// else to the screen's left edge (xterm left/right-margin behavior).
		if s.curX >= s.leftBound() {
			s.curX = s.leftBound()
		} else {
			s.curX = 0
		}
		s.pendingWrap = false
	case '\t':
		switch {
		case !s.pendingWrap:
			s.tabOnce()
		case s.moreFix:
			// more(1) fix (DEC mode ?41): a TAB in the deferred-wrap position
			// honors the pending wrap (advance to the next line) before tabbing.
			s.pendingWrap = false
			s.curX = s.wrapColumn()
			s.curY++
			s.scrollIfNeeded()
			s.tabOnce()
		default:
			// Without the fix, a TAB in the deferred-wrap position is a no-op:
			// the cursor stays at the right margin and the pending wrap is
			// preserved, so the NEXT printable still wraps. This reproduces the
			// curses/more(1) behavior xterm's mode 41 works around.
		}
	case 0x0E: // SO — Shift Out: activate G1 in GL
		s.gl = 1
	case 0x0F: // SI — Shift In: activate G0 in GL
		s.gl = 0
	case 0x96: // SPA (C1) — start of guarded area
		s.curIsoProtected = true
	case 0x97: // EPA (C1) — end of guarded area
		s.curIsoProtected = false
	}
}

//nolint:gocyclo // flat dispatch over the ESC final bytes
func (s *Screen) dispatchEsc(b byte) {
	// '#' introduces the DEC line-size / alignment group (DECALN etc.);
	// '(', ')', '*', '+' designate character sets.
	if s.numInterm > 0 {
		if s.pIntermed[0] == '#' {
			s.decLineSize(b)
		} else {
			s.designateCharset(s.pIntermed[0], b)
		}
		return
	}
	switch b {
	case '7':
		s.saveCursor()
	case '8':
		s.restoreCursor()
	case 'H': // HTS
		s.setTabStop(s.curX)
	case 'D': // IND
		s.pendingWrap = false
		s.lineDown()
	case 'E': // NEL — index then carriage return. lineDown runs first so its
		// scroll decision uses the original column (outside the left/right
		// margins it must not scroll); then the CR rule places the column (to the
		// left margin when at/right of it, else to the screen edge).
		s.pendingWrap = false
		s.lineDown()
		if s.curX >= s.leftBound() {
			s.curX = s.leftBound()
		} else {
			s.curX = 0
		}
	case 'V': // SPA — start of guarded (ISO-protected) area
		s.curIsoProtected = true
	case 'W': // EPA — end of guarded area
		s.curIsoProtected = false
	case 'Z': // DECID — identify terminal (obsolete alias for primary DA)
		s.deviceAttributes()
	case 'M': // RI — reverse index. Clears the deferred wrap like IND/NEL.
		s.pendingWrap = false
		if s.curY == s.scrollTop {
			// At the top margin, scroll down only when within the left/right
			// margins; outside the box RI neither scrolls nor moves.
			if s.withinHMargins(s.curX) {
				s.scrollDownOnce()
			}
		} else if s.curY > 0 {
			s.curY--
		}
	case '6': // DECBI — back index
		s.decBackIndex()
	case '9': // DECFI — forward index
		s.decForwardIndex()
	case '=': // DECKPAM
		s.AppKeypad = true
	case '>': // DECKPNM
		s.AppKeypad = false
	case 'N': // SS2
		s.singleShft = 2
	case 'O': // SS3
		s.singleShft = 3
	case 'c': // RIS — hard reset. Beyond the attribute/screen reset, home the
		// cursor and clear the client scrollback (a hard reset discards
		// history); RIS also resets tab stops to the default every-8 grid.
		s.softReset()
		s.eraseRegion(0, 0, s.Height-1, s.Width-1)
		s.curY, s.curX = 0, 0
		s.pendingWrap = false
		s.Drained = nil
		s.clearWrapState()
		s.scrollbackCleared = true
		s.InAltScreen = false
		s.savedMainCells = nil
		s.savedMainWrapped = nil
		s.altCells = nil
		// RIS clears the kitty keyboard-protocol flags/stacks for both screens
		// (a hard reset returns keyboard reporting to legacy). DECSTR leaves them
		// untouched — the protocol is managed by its own CSI-u push/pop.
		s.mainKbd = kbdProtocol{}
		s.altKbd = kbdProtocol{}
		s.specialColors = nil
		s.BracketedPaste = false
		s.AppCursorKeys = false
		s.AppKeypad = false
		s.CursorHidden = false
		s.CursorStyle = 0
		s.ReverseVideo = false
		s.tabStops = nil
		// RIS resets the XTSMTITLE title modes to their default (all off).
		// DECSTR (soft reset) deliberately leaves them untouched, matching
		// xterm, so this lives in the RIS handler rather than softReset.
		s.titleSetHex = false
		s.titleQueryHex = false
	}
}

// decLineSize handles the ESC # <n> line-size / alignment group. Only DECALN
// (ESC # 8) is implemented: it fills the whole screen with 'E' (the classic
// alignment test) using default attributes, resets the scroll region, and homes
// the cursor. DECDHL/DECDWL/DECSWL (ESC # 3/4/5/6 — double-height, double-width
// and single-width lines) are intentionally not implemented: rendering them
// needs a per-line size attribute plus wire + client support, and they are
// effectively unused by modern TUIs. They are consumed as no-ops.
func (s *Screen) decLineSize(final byte) {
	if final != '8' { // DECALN only
		return
	}
	for y := range s.Cells {
		row := s.Cells[y]
		for x := range row {
			row[x] = Cell{Ch: 'E'}
		}
	}
	s.scrollTop = 0
	s.scrollBottom = s.Height - 1
	// DECALN also clears the left/right margins (DECLRMM off, full width).
	s.LRMarginMode = false
	s.leftMargin = 0
	s.rightMargin = s.Width - 1
	s.curY, s.curX = 0, 0
	s.pendingWrap = false
}

func decodeUTF8Bytes(buf [4]byte, n uint8) rune {
	var r rune
	switch n {
	case 2:
		r = rune(buf[0]&0x1F)<<6 | rune(buf[1]&0x3F)
		if r < 0x80 {
			return 0xFFFD // overlong
		}
	case 3:
		r = rune(buf[0]&0x0F)<<12 | rune(buf[1]&0x3F)<<6 | rune(buf[2]&0x3F)
		if r < 0x800 {
			return 0xFFFD // overlong
		}
	case 4:
		r = rune(buf[0]&0x07)<<18 | rune(buf[1]&0x3F)<<12 | rune(buf[2]&0x3F)<<6 | rune(buf[3]&0x3F)
		if r < 0x10000 {
			return 0xFFFD // overlong
		}
	default:
		return 0xFFFD
	}
	// Reject surrogates, > U+10FFFF, and U+FFFF (collides with the wire
	// wide-continuation sentinel in cellsToRuns — see wire.go).
	if r > 0x10FFFF || (r >= 0xD800 && r <= 0xDFFF) || r == 0xFFFF {
		return 0xFFFD
	}
	return r
}
