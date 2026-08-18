package vt

import (
	"fmt"
	"strconv"
)

// DCS (Device Control String) handlers.
// Supported:
//   - DECRQSS (DCS $ q <selector> ST) — Request Status String
//
// Routing: intermediate='$', final='q' → DECRQSS

// dcsHook is called on entry to DcsPassthrough. Routes to the appropriate handler.
func (s *Screen) dcsHook(finalByte byte) {
	switch {
	case s.numInterm == 1 && s.pIntermed[0] == '$' && finalByte == 'q':
		s.dcsFunc = dcsDecrqss
		s.dcsBuf = s.dcsBuf[:0]
	case s.numInterm == 1 && s.pIntermed[0] == '+' && finalByte == 'q':
		s.dcsFunc = dcsXTGetTcap
		s.dcsBuf = s.dcsBuf[:0]
	default:
		s.dcsFunc = dcsIgnored
	}
}

// dcsPut receives data bytes during DcsPassthrough.
func (s *Screen) dcsPut(b byte) {
	switch s.dcsFunc {
	case dcsDecrqss, dcsXTGetTcap:
		if len(s.dcsBuf) < maxDCSLen {
			s.dcsBuf = append(s.dcsBuf, b)
		}
	case dcsIgnored:
		// Don't buffer unknown DCS data
	}
}

// dcsUnhook is called on exit from DcsPassthrough. Dispatches the completed DCS.
func (s *Screen) dcsUnhook() {
	switch s.dcsFunc {
	case dcsDecrqss:
		s.handleDecrqss(s.dcsBuf)
	case dcsXTGetTcap:
		s.handleXTGetTcap(s.dcsBuf)
	}
	s.dcsFunc = dcsNone
}

// handleXTGetTcap answers XTGETTCAP (DCS + q <names> ST), the terminfo/termcap
// capability query. Names are hex-encoded and ';'-separated. We answer the one
// capability an app needs to size its palette — "Co"/"colors" (the number of
// colors) → 256 — and reply "invalid" (DCS 0 + r <name> ST) for everything
// else, so a probing app falls back to its terminfo database. This narrow
// answer is what lets a color-count query resolve to 256; broader terminfo
// modeling is intentionally out of scope.
func (s *Screen) handleXTGetTcap(query []byte) {
	for _, part := range splitSemis(string(query)) {
		name := decodeHexString(part)
		if name == "" {
			// Malformed (non-hex) capability name. XTGETTCAP names are hex
			// per the spec, so echoing the raw bytes back would inject
			// attacker-controlled control runes (CR/LF) into the PTY as
			// input. Skip it.
			continue
		}
		if name == "Co" || name == "colors" {
			// Reply value is the hex encoding of the decimal string "256".
			s.response = fmt.Appendf(s.response, "\x1bP1+r%s=%s\x1b\\", part, encodeHexString("256"))
			continue
		}
		s.response = fmt.Appendf(s.response, "\x1bP0+r%s\x1b\\", part)
	}
}

// splitSemis splits a ';'-separated string, returning nil for empty input.
func splitSemis(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := range len(s) {
		if s[i] == ';' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// decodeHexString decodes a hex-encoded byte string (e.g. "436F" -> "Co"),
// returning "" on malformed input.
func decodeHexString(h string) string {
	if len(h)%2 != 0 {
		return ""
	}
	b := make([]byte, 0, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		v, err := strconv.ParseUint(h[i:i+2], 16, 8)
		if err != nil {
			return ""
		}
		b = append(b, byte(v))
	}
	return string(b)
}

// encodeHexString hex-encodes a string (e.g. "256" -> "323536"), matching the
// XTGETTCAP reply format.
func encodeHexString(s string) string {
	const hexdigits = "0123456789ABCDEF"
	out := make([]byte, 0, len(s)*2)
	for i := range len(s) {
		out = append(out, hexdigits[s[i]>>4], hexdigits[s[i]&0xf])
	}
	return string(out)
}

// handleDecrqss processes a DECRQSS query and appends the response to s.response.
// Valid response: DCS 1 $ r <data> ST
// Invalid response: DCS 0 $ r ST
//
//nolint:gocyclo // flat DECRQSS selector dispatch (one status string per selector)
func (s *Screen) handleDecrqss(query []byte) {
	q := string(query)
	switch q {
	case "m": // SGR
		data := sgrParamsString(s.style)
		s.response = fmt.Appendf(s.response, "\x1bP1$r%sm\x1b\\", data)
	case "r": // DECSTBM (scroll region)
		top := s.scrollTop + 1
		bottom := s.scrollBottom + 1
		s.response = fmt.Appendf(s.response, "\x1bP1$r%d;%dr\x1b\\", top, bottom)
	case " q": // DECSCUSR (cursor style)
		s.response = fmt.Appendf(s.response, "\x1bP1$r%d q\x1b\\", s.CursorStyle)
	case "\"q": // DECSCA (character protection) — report current attribute
		ps := 0
		if s.curProtected {
			ps = 1
		}
		s.response = fmt.Appendf(s.response, "\x1bP1$r%d\"q\x1b\\", ps)
	case "\"p": // DECSCL (conformance level) — report the tracked level
		level := s.conformanceLevel
		if level == 0 {
			level = 65
		}
		s.response = fmt.Appendf(s.response, "\x1bP1$r%d;1\"p\x1b\\", level)
	case "s": // DECSLRM (left/right margins) — report the active margins
		s.response = fmt.Appendf(s.response, "\x1bP1$r%d;%ds\x1b\\", s.leftBound()+1, s.rightBound()+1)
	case "t": // DECSLPP (lines per page) — report the tracked value, else the height
		lpp := s.linesPerPage
		if lpp == 0 {
			lpp = s.Height
		}
		s.response = fmt.Appendf(s.response, "\x1bP1$r%dt\x1b\\", lpp)
	case "*|": // DECSNLS (lines per screen) — report the tracked value, else the height
		lps := s.linesPerScreen
		if lps == 0 {
			lps = s.Height
		}
		s.response = fmt.Appendf(s.response, "\x1bP1$r%d*|\x1b\\", lps)
	case "*x": // DECSACE (attribute change extent) — not tracked; report default
		s.response = append(s.response, "\x1bP1$r0*x\x1b\\"...)
	case "$}": // DECSASD (active status display) — main display (0)
		s.response = append(s.response, "\x1bP1$r0$}\x1b\\"...)
	case "$~": // DECSSDT (status display type) — none (0)
		s.response = append(s.response, "\x1bP1$r0$~\x1b\\"...)
	default:
		// Unrecognized query
		s.response = append(s.response, "\x1bP0$r\x1b\\"...)
	}
}
