package vt

import (
	"strconv"
	"strings"
)

//nolint:gocyclo,gocognit // SGR parameter parsing is a wide, flat attribute switch
func (s *Screen) applySGR() {
	// Handle empty params (SGR 0 = reset)
	if s.numGroups == 0 || (s.numGroups == 1 && s.numParams == 1 && s.pParams[0] == 0) {
		s.style = Style{}
		return
	}

	for i := 0; i < s.paramCount(); i++ {
		g := s.paramGroup(i)
		if g.Len == 0 {
			continue
		}
		p := int(g.Params[0])

		switch {
		case p == 0:
			s.style = Style{}
		case p == 1:
			s.style.Bold = true
		case p == 2:
			s.style.Dim = true
		case p == 3:
			s.style.Italic = true
		case p == 4:
			// Underline, with optional style subparam 4:x. 4:0 = off,
			// 4:2 = double; 4:1 single and 4:3/4:4/4:5 (curly/dotted/dashed)
			// all render as a single underline for now (distinct curly/dotted/
			// dashed rendering would need a wire attribute + client CSS). Plain
			// SGR 4 (no subparam) is a single underline.
			if g.Len >= 2 {
				switch g.Params[1] {
				case 0:
					s.style.Underline = false
					s.style.DoubleUnderline = false
				case 2:
					s.style.DoubleUnderline = true
					s.style.Underline = false
				default:
					s.style.Underline = true
					s.style.DoubleUnderline = false
				}
			} else {
				s.style.Underline = true
			}
		case p == 5:
			s.style.Blink = true
		case p == 6:
			s.style.Blink = true
		case p == 7:
			s.style.Inverse = true
		case p == 8:
			s.style.Hidden = true
		case p == 9:
			s.style.Strikethrough = true
		case p == 21:
			s.style.DoubleUnderline = true
			s.style.Underline = false
		case p == 22:
			s.style.Bold = false
			s.style.Dim = false
		case p == 23:
			s.style.Italic = false
		case p == 24:
			s.style.Underline = false
			s.style.DoubleUnderline = false
		case p == 25:
			s.style.Blink = false
		case p == 27:
			s.style.Inverse = false
		case p == 28:
			s.style.Hidden = false
		case p == 29:
			s.style.Strikethrough = false
		case p >= 30 && p <= 37:
			s.style.FG = Color{Type: 1, Val: uint8(p - 30)}
		case p == 38:
			i = s.parseExtColorGroup(i, &s.style.FG)
		case p == 39:
			s.style.FG = Color{}
		case p >= 40 && p <= 47:
			s.style.BG = Color{Type: 1, Val: uint8(p - 40)}
		case p == 48:
			i = s.parseExtColorGroup(i, &s.style.BG)
		case p == 49:
			s.style.BG = Color{}
		case p == 53:
			s.style.Overline = true
		case p == 55:
			s.style.Overline = false
		case p == 58:
			i = s.parseExtColorGroup(i, &s.style.UnderlineColor)
		case p == 59:
			s.style.UnderlineColor = Color{}
		case p >= 90 && p <= 97:
			s.style.FG = Color{Type: 1, Val: uint8(p - 90 + 8)}
		case p >= 100 && p <= 107:
			s.style.BG = Color{Type: 1, Val: uint8(p - 100 + 8)}
		}
	}
}

// parseExtColorGroup handles extended color (38/48/58) with both forms:
// - Colon subparam: 38:2:R:G:B or 38:5:N (single group with subparams)
// - Semicolon legacy: 38;2;R;G;B or 38;5;N (multiple groups)
// Returns the new group index after consuming.
func (s *Screen) parseExtColorGroup(i int, c *Color) int {
	g := s.paramGroup(i)
	if g.Len >= 3 {
		parseExtColorColon(g, c)
		return i
	}
	return s.parseExtColorLegacy(i, c)
}

// parseExtColorColon decodes the colon-subparam extended-color form
// (38:5:N or 38:2:R:G:B), where the mode and components share one parameter
// group. The caller guarantees g.Len >= 3.
func parseExtColorColon(g ParamGroup, c *Color) {
	switch int(g.Params[1]) {
	case 5: // 256-color: 38:5:N
		*c = Color{Type: 2, Val: clampByte(int(g.Params[2]))}
	case 2:
		// RGB. Two colon forms exist: the plain 38:2:R:G:B (Len 5) and the
		// ITU-T T.416 form 38:2:cs:R:G:B (Len 6) whose extra element is a
		// color-space id at index 2 (often empty, e.g. 38:2::R:G:B); in that
		// form R:G:B live at indices 3:4:5. 38:2:R:G (Len 4) is a lenient
		// partial. Reading the wrong indices for the 6-element form dropped
		// blue and shifted the channels.
		switch {
		case g.Len >= 6:
			*c = Color{Type: 3, R: clampByte(int(g.Params[3])), G: clampByte(int(g.Params[4])), B: clampByte(int(g.Params[5]))}
		case g.Len == 5:
			*c = Color{Type: 3, R: clampByte(int(g.Params[2])), G: clampByte(int(g.Params[3])), B: clampByte(int(g.Params[4]))}
		case g.Len == 4:
			*c = Color{Type: 3, R: clampByte(int(g.Params[2])), G: clampByte(int(g.Params[3]))}
		}
	}
}

// parseExtColorLegacy decodes the semicolon-legacy extended-color form
// (38;5;N or 38;2;R;G;B), where the mode and components are separate groups
// following group i. Returns the new group index after consuming.
func (s *Screen) parseExtColorLegacy(i int, c *Color) int {
	if i+1 >= s.paramCount() {
		return i
	}
	modeGroup := s.paramGroup(i + 1)
	if modeGroup.Len == 0 {
		return i + 1
	}
	switch int(modeGroup.Params[0]) {
	case 5:
		if i+2 < s.paramCount() {
			vg := s.paramGroup(i + 2)
			if vg.Len > 0 {
				*c = Color{Type: 2, Val: clampByte(int(vg.Params[0]))}
			}
			return i + 2
		}
	case 2:
		if i+4 < s.paramCount() {
			rg := s.paramGroup(i + 2)
			gg := s.paramGroup(i + 3)
			bg := s.paramGroup(i + 4)
			if rg.Len > 0 && gg.Len > 0 && bg.Len > 0 {
				*c = Color{Type: 3, R: clampByte(int(rg.Params[0])), G: clampByte(int(gg.Params[0])), B: clampByte(int(bg.Params[0]))}
			}
			return i + 4
		}
	}
	return i + 1
}

func clampByte(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// sgrSequence emits an ANSI SGR escape that reproduces the given Style.
func sgrSequence(st Style) string {
	return "\x1b[" + sgrParamsString(st) + "m"
}

func appendColorParams(params []string, c Color, base int) []string {
	switch c.Type {
	case 1:
		if c.Val < 8 {
			params = append(params, strconv.Itoa(base+int(c.Val)))
		} else {
			// Bright indices 8-15 map to 90-97 (fg, base 30) / 100-107 (bg,
			// base 40), NOT base+Val (which would emit 38/39/48/49 and higher).
			params = append(params, strconv.Itoa(base+60+int(c.Val)-8))
		}
	case 2:
		params = append(params, strconv.Itoa(base+8), "5", strconv.Itoa(int(c.Val)))
	case 3:
		params = append(params, strconv.Itoa(base+8), "2",
			strconv.Itoa(int(c.R)), strconv.Itoa(int(c.G)), strconv.Itoa(int(c.B)))
	}
	return params
}

func appendUnderlineColorParams(params []string, c Color) []string {
	switch c.Type {
	case 2:
		params = append(params, "58", "5", strconv.Itoa(int(c.Val)))
	case 3:
		params = append(params, "58", "2",
			strconv.Itoa(int(c.R)), strconv.Itoa(int(c.G)), strconv.Itoa(int(c.B)))
	}
	return params
}

// sgrParamsString returns the current SGR state as a parameter string.
// Used by DECRQSS response.
func sgrParamsString(st Style) string {
	if st == (Style{}) {
		return "0"
	}
	var params []string
	params = append(params, "0")
	if st.Bold {
		params = append(params, "1")
	}
	if st.Dim {
		params = append(params, "2")
	}
	if st.Italic {
		params = append(params, "3")
	}
	if st.Underline {
		params = append(params, "4")
	}
	if st.DoubleUnderline {
		params = append(params, "21")
	}
	if st.Blink {
		params = append(params, "5")
	}
	if st.Inverse {
		params = append(params, "7")
	}
	if st.Hidden {
		params = append(params, "8")
	}
	if st.Strikethrough {
		params = append(params, "9")
	}
	if st.Overline {
		params = append(params, "53")
	}
	params = appendColorParams(params, st.FG, 30)
	params = appendColorParams(params, st.BG, 40)
	if st.UnderlineColor.Type != 0 {
		params = appendUnderlineColorParams(params, st.UnderlineColor)
	}
	return strings.Join(params, ";")
}
