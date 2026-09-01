package vt

import "fmt"

// Kitty keyboard protocol (progressive keyboard enhancement).
//
// The protocol lets a program running in the terminal opt into richer, more
// unambiguous key reporting. It is negotiated entirely through CSI-u escape
// codes written by the program to the PTY (parsed here); the actual key
// *encoding* happens client-side, so the terminal layer syncs the current flags
// to the client (see KeyboardFlags) and the client's keyboard encoder switches
// between legacy and kitty CSI-u encoding accordingly.
//
// Negotiation sequences (all terminated by 'u'):
//
//	CSI ? u              query   -> reply CSI ? flags u
//	CSI > flags u        push    (flags default 0)
//	CSI < number u       pop     (number default 1)
//	CSI = flags ; mode u set     (mode 1 set-all / 2 set-given / 3 reset-given)
//
// Spec: https://sw.kovidgoyal.net/kitty/keyboard-protocol/
//
// Honored flags: only disambiguate (0x1), the headline enhancement giving apps
// unambiguous Esc/Ctrl/Alt key events. NOT honored: report-event-types (0x2,
// needs key-release reporting our keydown-only input path lacks),
// report-alternate-keys (0x4, needs codepoints the browser doesn't expose),
// report-all-keys-as-escape-codes (0x8) and report-associated-text (0x10,
// both incompatible with the client's hidden-textarea/IME input model).
//
// Incoming flags are masked to the honored subset on store, so CSI ? u
// truthfully reports only what is honored; an app needing an unsupported flag
// can detect the gap (set-then-query) and fall back — a partial implementation
// the spec sanctions. Adding a flag later means extending both this mask and
// the client encoder together.
const (
	kbdDisambiguate uint8 = 1 << 0 // 0x1  disambiguate escape codes

	// kbdHonoredMask is the set of progressive-enhancement flags this terminal
	// implements. Flags outside it (0x2, 0x4, 0x8, 0x10) are masked off on
	// store; see the comment above.
	kbdHonoredMask = kbdDisambiguate // 0x01

	// maxKbdStack bounds each screen's push/pop stack. The spec asks terminals
	// to cap the stack against denial-of-service; when full the oldest entry is
	// evicted on push.
	maxKbdStack = 16
)

// kbdProtocol holds one screen's kitty keyboard-protocol state: the current
// progressive-enhancement flags (masked to kbdHonoredMask) and the push/pop
// stack of previously-current values. The main and alternate screens keep
// independent instances (the spec requires separate stacks per screen) so an
// editor can enable the protocol on the alt screen without disturbing the
// shell's main-screen mode.
type kbdProtocol struct {
	stack []uint8
	flags uint8
}

// activeKbd returns the keyboard-protocol state for the screen currently in
// effect, so every CSI-u operation and the KeyboardFlags accessor address the
// right stack.
func (s *Screen) activeKbd() *kbdProtocol {
	if s.InAltScreen {
		return &s.altKbd
	}
	return &s.mainKbd
}

// KeyboardFlags reports the active screen's current kitty keyboard
// progressive-enhancement flags (0 when the protocol is disabled). The terminal
// layer syncs this to the client so its key encoder switches between legacy and
// kitty CSI-u encoding; switching screens flips to the other stack's flags,
// which the client observes as a modes change.
func (s *Screen) KeyboardFlags() uint8 {
	return s.activeKbd().flags
}

// reportKeyboardFlags answers the query CSI ? u with CSI ? flags u, reporting
// the active screen's current honored flags. Always answered (it leaks no
// screen contents), so a probing app that also requests device attributes gets
// the reply before the DA answer and thus detects protocol support.
func (s *Screen) reportKeyboardFlags() {
	s.response = fmt.Appendf(s.response, "\x1b[?%du", s.activeKbd().flags)
}

// pushKeyboardFlags implements CSI > flags u: save the current flags onto the
// stack and make the given (masked) flags current. The stack is bounded; when
// full the oldest entry is evicted (DoS guard, per spec).
func (s *Screen) pushKeyboardFlags(flags int) {
	k := s.activeKbd()
	if len(k.stack) >= maxKbdStack {
		k.stack = k.stack[1:] // evict the oldest entry
	}
	k.stack = append(k.stack, k.flags)
	k.flags = uint8(flags) & kbdHonoredMask //nolint:gosec // masked to 0x01
}

// popKeyboardFlags implements CSI < number u: pop n entries, restoring the flags
// present before the corresponding pushes. Popping past the bottom empties the
// stack and resets the flags to 0 (per spec).
func (s *Screen) popKeyboardFlags(n int) {
	k := s.activeKbd()
	if n < 1 {
		n = 1
	}
	for ; n > 0 && len(k.stack) > 0; n-- {
		k.flags = k.stack[len(k.stack)-1]
		k.stack = k.stack[:len(k.stack)-1]
	}
	if n > 0 {
		// Popped past the bottom of the stack: reset all flags.
		k.flags = 0
	}
}

// setKeyboardFlags implements CSI = flags ; mode u: modify the active screen's
// current flags in place. mode 3 resets the given bits, mode 2 sets them, and
// mode 1 (the default) replaces all bits with the given set. Flags are masked
// to the honored subset first.
func (s *Screen) setKeyboardFlags(flags, mode int) {
	k := s.activeKbd()
	f := uint8(flags) & kbdHonoredMask //nolint:gosec // masked to 0x01
	switch mode {
	case 3: // reset the given bits
		k.flags &^= f
	case 2: // set the given bits
		k.flags |= f
	default: // mode 1 (or unspecified): given bits set, all others reset
		k.flags = f
	}
}
