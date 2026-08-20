// Package vt handles OSC (Operating System Command) dispatch.
//
// Supported:
//   - OSC 0 ; Pt BEL/ST — Set icon name and window title to Pt
//   - OSC 1 ; Pt BEL/ST — Set icon name only (tracked separately from title)
//   - OSC 2 ; Pt BEL/ST — Set window title to Pt
//   - OSC 4 / 104 — set/query and reset palette colors (index 0-255)
//   - OSC 5 / 105 — set/query and reset the special colors (bold/underline/…)
//   - OSC 8 ; params ; URI BEL/ST — Set/clear hyperlink (URI empty = clear)
//   - OSC 9 ; Pt BEL/ST — desktop notification: Pt is captured into
//     Notification for the status layer. The ConEmu subcommand form
//     (OSC 9 ; Ps [; ...]) is not a notification; subcommand 4 (progress) is
//     captured into Progress + ProgressValue, any other numeric subcommand is
//     ignored
//   - OSC 10-19 ; spec|? BEL/ST — set/query the dynamic colors (default
//     fg/bg/cursor/…), answered from the configured Theme; see WithTheme
//   - OSC 52 ; Pc ; Pd — clipboard: SET pushes to the browser clipboard and
//     retains the session selection; QUERY reports it back (only when
//     AllowScreenReport is enabled — see handleOsc52)
//   - OSC 110-119 — reset a dynamic color to its Theme default
//
// Out-of-scope (buffered then ignored):
//   - OSC 7  (Current directory)
//   - OSC 133 (shell integration), OSC 777 (notifications)
//   - X11 Xcms color-space specs in OSC 4/5/10-19 (CIE*/rgbi/TekHVC)
//
// The OSC payload format is: <numeric-id> ; <string-data>
// The numeric prefix is parsed as decimal digits up to the first ';'.
package vt

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/cplieger/runesafe/v2"
)

// decodeTitle applies the XTSMTITLE set-mode to an incoming OSC 0/1/2 title
// payload: when set-hex (mode 0) is active the payload is a hex-encoded byte
// string (xterm), decoded here; otherwise it is used verbatim. Invalid hex
// falls back to the raw string so a malformed sequence can't blank the title.
func (s *Screen) decodeTitle(data string) string {
	if !s.titleSetHex {
		return data
	}
	if b, err := hex.DecodeString(data); err == nil {
		return string(b)
	}
	return data
}

// encodeTitle applies the XTSMTITLE query-mode to a title reported by
// XTWINOPS 20/21: when query-hex (mode 1) is active the label is hex-encoded
// (xterm); otherwise it is reported with C0/C1/DEL control runes stripped.
// The report is injected into the PTY as input, so echoing an attacker-set
// title verbatim would let an embedded CR/LF inject a command line.
func (s *Screen) encodeTitle(title string) string {
	if s.titleQueryHex {
		return hex.EncodeToString([]byte(title))
	}
	return stripControlRunes(title)
}

// stripControlRunes drops C0 (incl. CR/LF), DEL, and C1 control runes from a
// string that will be echoed back into the PTY as input.
func stripControlRunes(t string) string {
	var b strings.Builder
	b.Grow(len(t))
	for _, r := range t {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// dispatchOsc processes the buffered OSC payload and resets the buffer.
//
//nolint:gocyclo // flat dispatch over the OSC command ids
func (s *Screen) dispatchOsc() {
	payload := s.oscBuf
	s.oscBuf = s.oscBuf[:0]

	if len(payload) == 0 {
		return
	}

	// Parse numeric prefix (digits before first ';').
	var id int
	i := 0
	for i < len(payload) && payload[i] >= '0' && payload[i] <= '9' {
		id = id*10 + int(payload[i]-'0')
		i++
	}

	// Skip the ';' separator if present.
	var data string
	if i < len(payload) && payload[i] == ';' {
		data = string(payload[i+1:])
	}

	switch id {
	case 0:
		// OSC 0 sets both the icon name and the window title.
		title := s.decodeTitle(data)
		s.Title = title
		s.iconTitle = title
	case 2:
		// OSC 2 sets the window title only.
		s.Title = s.decodeTitle(data)
	case 1:
		// OSC 1 sets the icon name only. Tracked separately from the window
		// title so XTWINOPS 20 (report icon label) round-trips; it is not sent
		// to the client (which shows the window title).
		s.iconTitle = s.decodeTitle(data)
	case 8:
		// OSC 8 ; params ; URI — set/clear hyperlink.
		// Format: "params;URI" where params is key=value pairs separated
		// by ':' (the 'id=' param is parsed but not used). Empty URI clears.
		s.handleOsc8(data)
	case 4:
		// OSC 4 — set/query a palette color (index 0-255) or a special color
		// (index 256+). Resolved server-side (colorToWire), so an override
		// reaches the client through the normal RGB runs.
		s.handleOscPalette(data)
	case 5:
		// OSC 5 — set/query a special color by number k (bold/underline/blink/
		// reverse/italic), the OSC 4 index 256+k under a shorter addressing.
		s.handleOscSpecialColor(data)
	case 10, 11, 12, 13, 14, 15, 16, 17, 18, 19:
		// Set or query a dynamic color (default fg/bg/cursor/mouse/highlight/…).
		// Both set and query round-trip through dynColors; see handleOscColor.
		s.handleOscColor(id, data)
	case 9:
		// OSC 9 — desktop notification (Pt captured into Notification) or a
		// ConEmu subcommand (numeric first field); subcommand 4 is progress,
		// captured into Progress/ProgressValue. See handleOsc9.
		s.handleOsc9(data)
	case 52:
		// OSC 52 — clipboard. SET always works (kiro-cli uses this to copy);
		// the query form is denied by default (letting a remote app read the
		// clipboard is a data-exfiltration risk most terminals disable) and
		// answered only when AllowScreenReport opts in — see handleOsc52's
		// gated read-back of the engine-side selection buffer.
		s.handleOsc52(data)
	case 104:
		// OSC 104 — reset one or all OSC 4 palette overrides.
		s.handleOscPaletteReset(data)
	case 105:
		// OSC 105 — reset one or all special colors (set via OSC 5 / OSC 4 256+).
		s.handleSpecialColorReset(data)
	case 110, 111, 112, 113, 114, 115, 116, 117, 118, 119:
		// Reset a dynamic color (OSC 11x resets OSC 1x, i.e. slot id-100) back
		// to its theme default by dropping any stored override.
		if s.dynColors != nil {
			delete(s.dynColors, id-100)
		}
	default:
		// Unknown/out-of-scope OSC (7, 133, 777, ...): consumed, ignored.
	}
}

// maxNotificationLen caps a captured OSC 9 notification message (in runes) so a
// hostile or runaway program cannot grow the stored string without bound.
const maxNotificationLen = 256

// handleOsc9 handles the two OSC 9 forms. A human-readable message
// (ESC ] 9 ; <text> ST) is captured into Notification for the status layer,
// stripped of control bytes and length-clamped because it is attacker-influenced
// terminal output. The ConEmu subcommand form (OSC 9 ; Ps [; ...] where the
// first field is numeric) is not a notification: subcommand 4 is progress
// reporting (OSC 9 ; 4 [; st [; pr]]), captured into Progress/ProgressValue so
// the status layer can derive working/error/warning, and any other numeric
// subcommand is ignored. The engine stays generic: it stores whatever
// notification text arrives and does not interpret it. Mapping a message to a
// session status (kiro-cli's "Response complete" / "Permission required") is the
// consumer's job, so no application strings are hard-coded here.
//
// The two forms are told apart by the FIRST ';'-delimited field, and only a
// PURELY numeric one is a subcommand — a message that merely starts with digits
// ("4 files changed") is free text and must arrive as a notification. The
// degenerate subcommand, a payload that is nothing but digits and carries no
// separator at all, is a subcommand too: that is how the abbreviated progress
// form (ESC ] 9 ; 4 ST, "stop the progress bar") reaches setProgress instead of
// being captured as the notification text "4".
func (s *Screen) handleOsc9(data string) {
	if data == "" {
		return
	}
	head, rest, hasFields := strings.Cut(data, ";")
	if isAllDigits(head) {
		if head == "4" {
			s.setProgress(rest, hasFields)
		}
		// Every other ConEmu subcommand is consumed and ignored, deliberately:
		// driven from terminal output, 9;7 (run a process), 9;6 (GUI macro) and
		// 9;2 (message box) are remote-execution and UI-hijack primitives. Do not
		// "complete" ConEmu support here.
		return
	}
	msg := sanitizeNotification(data)
	if msg == "" {
		return
	}
	s.Notification = msg
	s.NotificationSeq++
}

// OSC 9;4 progress states, and the marker for an absent percentage. State 1
// (determinate value) has no constant because no branch tests for it: it is
// covered by the range check plus the "requires pr" default in progressPercent.
const (
	progressAbsent             = -1 // no percentage reported (ProgressValue) / no state seen (Progress)
	progressStateClear         = 0  // stop / clear the progress bar; pr n/a
	progressStateError         = 2  // error state; pr optional (omitted = indeterminate error)
	progressStateIndeterminate = 3  // indeterminate, animates; pr n/a
	progressStateWarning       = 4  // iTerm2: warning at pr percent (ConEmu calls it paused); pr required
	progressPercentMax         = 100
)

// setProgress records a ConEmu/iTerm2 OSC 9;4 progress report from the payload
// following "4" ("st", "st;pr", or nothing at all). hasFields reports whether a
// ';' followed the subcommand, which is what separates the abbreviated form
// (ESC ] 9 ; 4 ST — no state field, stops the progress bar) from an explicit but
// empty state field ("9;4;", malformed).
//
// State 4 is read with iTerm2's semantics (warning at pr percent) rather than
// ConEmu's (paused), because the engine advertises TERM_PROGRAM=iTerm.app — that
// identity is what makes a client emit these sequences — and iTerm2's definition
// is the more specified of the two.
//
// A progress report is a STATE, not an event: it persists until the program
// sends another one, and only state 0 (or the abbreviated form) clears it.
// Neither source defines the malformed cases, so these are OUR documented
// choices: a non-numeric or out-of-range st, and a pr that is not a number at
// all, leave the previous state untouched rather than inventing one; a numeric
// pr outside 0-100 is clamped into range; fields beyond pr are ignored.
func (s *Screen) setProgress(rest string, hasFields bool) {
	if !hasFields {
		// Abbreviated form: no state field at all means stop the progress bar.
		s.Progress = progressStateClear
		s.ProgressValue = progressAbsent
		return
	}
	fields := strings.Split(rest, ";")
	st, err := strconv.Atoi(fields[0])
	if err != nil || st < progressStateClear || st > progressStateWarning {
		return // unparseable or out-of-range state: ignore, leave Progress alone
	}
	value, ok := progressPercent(st, fields)
	if !ok {
		return // malformed pr for this state: ignore the whole sequence
	}
	s.Progress = st
	s.ProgressValue = value
}

// progressPercent resolves the percentage an OSC 9;4 payload reports for state
// st, given the ';'-split fields after the subcommand (fields[0] is st). It
// returns the value to store and whether the sequence is well-formed; !ok means
// the caller must leave the previous state untouched.
func progressPercent(st int, fields []string) (int, bool) {
	if st == progressStateClear || st == progressStateIndeterminate {
		// pr is n/a for these states, so a stray one is not retained.
		return progressAbsent, true
	}
	if len(fields) < 2 {
		// pr omitted. Legal only for the error state, whose indeterminate form
		// carries no percentage; states 1 and 4 require one, and inventing a
		// value for them would report a percentage the program never sent.
		return progressAbsent, st == progressStateError
	}
	pr, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false // present but not a number at all: refuse to guess
	}
	return min(max(pr, 0), progressPercentMax), true
}

// isAllDigits reports whether s is non-empty and every byte is an ASCII digit.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// sanitizeNotification drops every rune of runesafe's unsafe classes — C0
// controls (newlines included) + DEL, C1 controls, the Unicode Bidi_Control
// set (Trojan-Source reordering), and U+2028/U+2029 (JS line terminators) —
// from an OSC 9 message and clamps it to maxNotificationLen runes, so the
// stored text is safe to place in a struct field, a log line, or a status
// event. The class definition is runesafe's, not a local list: this function
// used to hand-roll C0/C1 only, and the missed Bidi/2028-29 classes reached
// every consumer's classifier and log attributes (the drift the shared
// predicate exists to prevent). Dropping (not space-replacing) is a composed
// policy on runesafe.IsUnsafe, keeping the historical output shape.
func sanitizeNotification(data string) string {
	var b strings.Builder
	n := 0
	for _, r := range data {
		if n >= maxNotificationLen {
			break
		}
		if runesafe.IsUnsafe(r, false) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// handleOscPalette implements OSC 4: one or more "index ; spec" pairs. A spec
// of "?" queries the current color (answered as OSC 4 ; index ; rgb:.. ST); any
// other spec sets an override for that palette index. A set marks the palette
// changed so the handler forces a repaint (already-drawn cells re-resolve).
func (s *Screen) handleOscPalette(data string) {
	parts := strings.Split(data, ";")
	for i := 0; i+1 < len(parts); i += 2 {
		idx, err := strconv.Atoi(parts[i])
		if err != nil || idx < 0 || idx > maxSpecialColorIndex {
			continue
		}
		spec := parts[i+1]
		// Indices 256+ address the special colors (bold/underline/etc.), which
		// don't drive rendering here — they only round-trip through set/query.
		if idx >= specialColorBase {
			s.setOrQuerySpecialColor(4, idx, idx, spec)
			continue
		}
		if spec == "?" {
			rgb := s.colorToWire(Color{Type: 2, Val: uint8(idx)})
			r, g, b := rgbChannels(rgb)
			s.response = fmt.Appendf(s.response, "\x1b]4;%d;rgb:%02x%02x/%02x%02x/%02x%02x\x1b\\",
				idx, r, r, g, g, b, b)
			continue
		}
		rgb, ok := parseXColor(spec)
		if !ok {
			continue
		}
		if s.paletteOverride == nil {
			s.paletteOverride = make(map[uint8]int32)
		}
		s.paletteOverride[uint8(idx)] = rgb
		s.paletteChanged = true
	}
}

// Special colors (xterm OSC 5 / OSC 4 index >= 256) are the bold, underline,
// blink, reverse, and italic color registers. They live above the 256-color
// palette; the engine tracks them only for the OSC set/query round-trip (they
// don't affect rendering, which follows the client theme).
const (
	specialColorBase     = 256
	maxSpecialColorIndex = 263 // 8 registers: base .. base+7
)

// handleOscSpecialColor implements OSC 5: each "k ; spec" pair addresses special
// color k (== OSC 4 index 256+k). A "?" spec queries; any other sets.
func (s *Screen) handleOscSpecialColor(data string) {
	parts := strings.Split(data, ";")
	for i := 0; i+1 < len(parts); i += 2 {
		k, err := strconv.Atoi(parts[i])
		if err != nil || k < 0 || k > maxSpecialColorIndex-specialColorBase {
			continue
		}
		s.setOrQuerySpecialColor(5, k, specialColorBase+k, parts[i+1])
	}
}

// setOrQuerySpecialColor sets (or, for spec=="?", reports) special color at the
// given internal index. num is the number echoed in the reply and osc is the
// OSC id to echo (4 or 5), so the same register is addressable either way.
func (s *Screen) setOrQuerySpecialColor(osc, num, idx int, spec string) {
	if spec == "?" {
		r, g, b := rgbChannels(s.specialColorRGB(idx))
		s.response = fmt.Appendf(s.response, "\x1b]%d;%d;rgb:%02x%02x/%02x%02x/%02x%02x\x1b\\",
			osc, num, r, r, g, g, b, b)
		return
	}
	rgb, ok := parseXColor(spec)
	if !ok {
		return
	}
	if s.specialColors == nil {
		s.specialColors = make(map[int]int32)
	}
	s.specialColors[idx] = rgb
}

// specialColorRGB returns the override RGB for a special-color index, or black
// (the unset default) when none is set.
func (s *Screen) specialColorRGB(idx int) int32 {
	if c, ok := s.specialColors[idx]; ok {
		return c
	}
	return 0
}

// rgbChannels splits a packed 0xRRGGBB color into its 8-bit red, green, and
// blue components.
func rgbChannels(rgb int32) (r, g, b byte) {
	return byte(rgb >> 16), byte(rgb >> 8), byte(rgb) //nolint:gosec // the low 24 bits are an RGB triple
}

// handleSpecialColorReset implements OSC 105: with no data reset every special
// color, else reset the listed ones (OSC 5 numbering).
func (s *Screen) handleSpecialColorReset(data string) {
	if s.specialColors == nil {
		return
	}
	if data == "" {
		s.specialColors = nil
		return
	}
	for p := range strings.SplitSeq(data, ";") {
		k, err := strconv.Atoi(p)
		if err != nil || k < 0 || k > maxSpecialColorIndex-specialColorBase {
			continue
		}
		delete(s.specialColors, specialColorBase+k)
	}
}

// handleOscPaletteReset implements OSC 104: with no data reset the whole
// palette, else reset the listed indices. Only marks the palette changed if an
// override was actually removed.
func (s *Screen) handleOscPaletteReset(data string) {
	if s.paletteOverride == nil {
		return
	}
	if data == "" {
		s.paletteOverride = nil
		s.paletteChanged = true
		return
	}
	for p := range strings.SplitSeq(data, ";") {
		idx, err := strconv.Atoi(p)
		if err != nil || idx < 0 || idx > 255 {
			continue
		}
		if _, ok := s.paletteOverride[uint8(idx)]; ok {
			delete(s.paletteOverride, uint8(idx))
			s.paletteChanged = true
		}
	}
}

// handleOsc52 implements OSC 52 clipboard manipulation. Payload is
// "<targets>;<data>" where data is base64 (set), "?" (query), or neither
// (clear). On SET the decoded text is surfaced via PendingClipboard for the
// handler to push to the browser clipboard, and retained as the session
// selection buffer. On QUERY the retained buffer is reported back base64-
// encoded — but only when AllowScreenReport is enabled, the same gate used for
// DECRQCRA: the reply is injected into the PTY as input, and clipboard
// read-back is a data-exfiltration vector, so production leaves it off (a
// query is then silently ignored, xterm's behavior with allowWindowOps off).
// The reported buffer is only ever what an in-session OSC 52 SET wrote; the vt
// has no access to the real OS clipboard.
func (s *Screen) handleOsc52(data string) {
	targets, payload, ok := strings.Cut(data, ";")
	if !ok {
		return
	}
	if payload == "?" {
		if !s.AllowScreenReport {
			return
		}
		// Reply with the target list the request used; an empty request list
		// means the configurable primary/clipboard selection, reported as "s0"
		// (xterm's default), matching the first-selection-found rule.
		if targets == "" {
			targets = "s0"
		}
		enc := base64.StdEncoding.EncodeToString(s.selectionData)
		// Strip C0/C1/DEL from the echoed target list: the reply is injected
		// into the PTY as input, so a CR/LF in an attacker-set target would
		// inject a command line (same hardening as encodeTitle / XTGETTCAP).
		s.response = fmt.Appendf(s.response, "\x1b]52;%s;%s\x1b\\", stripControlRunes(targets), enc)
		return
	}
	// A base64 payload sets the selection (and pushes it to the browser
	// clipboard); anything else — an empty or non-base64 payload — clears it,
	// per xterm's "neither a base64 string nor ?" rule.
	if decoded, ok := decodeBase64Lenient(payload); ok && len(decoded) > 0 {
		s.selectionData = decoded
		s.pendingClipboard = decoded
		return
	}
	s.selectionData = nil
}

// decodeBase64Lenient decodes an OSC 52 payload, ignoring any bytes outside the
// base64 alphabet before decoding. This matches xterm (and RFC 2045, which says
// a decoder must ignore characters not in the alphabet, e.g. the line breaks
// long base64 is often wrapped at) and tolerates stray bytes such as the
// trailing quote the esctest harness appends via a repr() quirk. Tries padded
// StdEncoding first, then unpadded RawStdEncoding.
func decodeBase64Lenient(payload string) ([]byte, bool) {
	var b strings.Builder
	b.Grow(len(payload))
	for i := range len(payload) {
		switch c := payload[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '+', c == '/', c == '=':
			b.WriteByte(c)
		}
	}
	cleaned := b.String()
	if decoded, err := base64.StdEncoding.DecodeString(cleaned); err == nil {
		return decoded, true
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(cleaned, "=")); err == nil {
		return decoded, true
	}
	return nil, false
}

// parseXColor parses an X11 color spec into 0xRRGGBB. Supports the two forms
// terminals emit: "rgb:h/h/h" (1-4 hex digits per channel, scaled to 8-bit)
// and "#RGB" / "#RRGGBB" / "#RRRGGGBBB" / "#RRRRGGGGBBBB".
//
//nolint:gocognit // two flat prefix-keyed parse branches (rgb: and #hex)
func parseXColor(spec string) (int32, bool) {
	switch {
	case strings.HasPrefix(spec, "rgb:"):
		comps := strings.Split(spec[4:], "/")
		if len(comps) != 3 {
			return 0, false
		}
		var out int32
		for _, cstr := range comps {
			v, ok := scaleHexTo8(cstr)
			if !ok {
				return 0, false
			}
			out = out<<8 | int32(v)
		}
		return out, true
	case strings.HasPrefix(spec, "#"):
		// X11 "#" form: each channel's digits are the HIGH-order bits of a
		// 16-bit value (left-justified), NOT proportionally scaled. #f -> 0xf000
		// -> 0xf0; #fff and #f000 also -> 0xf0. (Contrast rgb:, which scales.)
		h := spec[1:]
		if h == "" || len(h) > 12 || len(h)%3 != 0 {
			return 0, false
		}
		n := len(h) / 3
		var out int32
		for c := range 3 {
			v, ok := leftJustifyHexTo8(h[c*n : c*n+n])
			if !ok {
				return 0, false
			}
			out = out<<8 | int32(v)
		}
		return out, true
	}
	return 0, false
}

// leftJustifyHexTo8 parses a 1-4 digit hex component the X11 "#" way: the value
// is placed in the high bits of a 16-bit field, and the top 8 bits are taken.
func leftJustifyHexTo8(h string) (uint8, bool) {
	if len(h) < 1 || len(h) > 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return 0, false
	}
	v16 := v << (16 - 4*uint(len(h)))
	return uint8((v16 >> 8) & 0xff), true
}

// scaleHexTo8 parses a 1-4 digit hex component and scales it to 8-bit: an
// n-digit value V represents V/(16^n - 1) of full scale.
func scaleHexTo8(h string) (uint8, bool) {
	if len(h) < 1 || len(h) > 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return 0, false
	}
	maxv := uint64(1)<<(4*len(h)) - 1
	scaled := (v*255 + maxv/2) / maxv
	return uint8(scaled), true // #nosec G115 -- scaled <= 255
}

// handleOscColor handles OSC 10/11/12 default-color set and query. A "?" spec
// queries the slot; a non-"?" spec is stored in dynColors so a later query (and
// the OSC 11x reset) round-trips it. A set is NOT propagated to the client
// renderer, which this VT does not drive. Answering the query from the Theme keeps apps
// that probe the background for light/dark detection (or block waiting for a
// reply) from stalling or guessing the wrong theme.
func (s *Screen) handleOscColor(id int, data string) {
	// One or more specs; each advances to the next dynamic-color slot (xterm:
	// OSC 10 with N specs sets colors 10, 11, …). "?" queries the slot's color.
	for i, spec := range strings.Split(data, ";") {
		slot := id + i
		if slot > 19 {
			break
		}
		if spec == "?" {
			r, g, b := rgbChannels(s.dynColor(slot))
			// xterm reports 16-bit-per-channel; duplicate each 8-bit value.
			s.response = fmt.Appendf(s.response, "\x1b]%d;rgb:%02x%02x/%02x%02x/%02x%02x\x1b\\",
				slot, r, r, g, g, b, b)
			continue
		}
		rgb, ok := parseXColor(spec)
		if !ok {
			continue
		}
		if s.dynColors == nil {
			s.dynColors = make(map[int]int32)
		}
		s.dynColors[slot] = rgb
	}
}

// dynColor returns the current OSC 10-19 color for a slot, falling back to the
// configured theme default when no override is set.
func (s *Screen) dynColor(slot int) int32 {
	if c, ok := s.dynColors[slot]; ok {
		return c
	}
	switch slot {
	case 11, 14, 16, 17: // background, mouse-bg, Tektronix-bg, highlight-bg
		return s.theme.Background
	case 12: // text cursor color
		return s.theme.Cursor
	default: // 10 fg, 13 mouse-fg, 15/18 Tek, 19 highlight-fg
		return s.theme.Foreground
	}
}

// handleOsc8 sets or clears the pen's hyperlink from an OSC 8 payload (the
// bytes after "8;").
//
// Format: params;URI — params may contain id=... separated by ':'. An empty URI
// closes the current hyperlink.
//
// A payload with no second semicolon carries no URI field and is MALFORMED, so
// it is ignored: the sequence is discarded and whatever hyperlink the pen holds
// stays. That matches ghostty, whose OSC 8 parser moves to its invalid state
// when the second separator is missing, and it is the conservative direction —
// clearing instead would let one corrupt byte inside a URL end a link the
// application had legitimately opened, mid-label.
//
// The unbounded-bleed failure this looks like it should guard against comes from
// somewhere else entirely: an application that opens a link and never closes it.
// No spelling of OSC 8 fixes that, and the pen's escape hatches are the three
// sequences that reset it (empty URI, DECSTR, RIS) plus a screen switch.
func (s *Screen) handleOsc8(data string) {
	// Split on first ';' to separate params from URI.
	_, uri, ok := strings.Cut(data, ";")
	if !ok {
		return
	}
	// Empty URI closes the hyperlink; non-empty sets it.
	s.hyperlink = uri
}
