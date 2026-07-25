package terminal

// The input-derived session title: name a session after the first substantial
// thing the user typed into it.
//
// This lives server-side because the server is the source of truth for session
// state, and a name is session state. A client-side deriver (which is where this
// logic started) can only ever name what ITS OWN keyboard sent, so the label
// depended on which browser you happened to type in, needed a round trip to
// persist, needed a second rule to survive a reload, and two windows typing into
// one session could race each other. Deriving it here removes all four: every
// client — including one that attaches later, or a future non-browser client —
// reads the same name off the wire.
//
// It is a MECHANISM, not a policy. The engine derives nothing unless a consumer
// asks for it with WithInputTitle, because "the first line you typed is this
// session's name" suits a session-per-conversation agent shell and does not suit
// a general-purpose terminal, where the foreground-process ladder (proctitle.go)
// is the better automatic label. The consumer owns that judgement; see
// web-terminal-kiro (on) versus web-terminal-server (off).

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxDerivedTitleRunes bounds a stored derived title. Every view truncates the
// label visually (max-width plus ellipsis), so this is a guard against storing a
// huge paste, not a display cut.
const maxDerivedTitleRunes = 512

// bareSlashCommand matches a slash command with NO argument, which is an agent
// directive rather than a request. It must be argument-free: `/tmp/x.log what is
// this` is a real message and is indistinguishable from `/model opus` without
// knowing the agent's command set.
var bareSlashCommand = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9-]*$`)

// isEligibleTitle gates which submitted line may become a session's derived
// title. It is an ELIGIBILITY filter, not a substantiveness detector: it cannot
// know whether a message is meaningful, only that it is not obviously not.
//
// Two rules, both rejecting: a line shorter than 3 characters ("y", "ok", "1"),
// and a bare slash command.
//
// Accepted limitations, each of which the rename affordance is the answer to: a
// genuine first message of `yes`, `help` or `asdf` latches; a slash command WITH
// an argument latches; and a first message the user immediately interrupts
// latches, because an interrupt arriving after submission cannot retract it. An
// affirmation stop-list would be unbounded and language-specific, and gating on a
// completed turn would leave the session unnamed for its whole first turn.
func isEligibleTitle(line string) bool {
	t := strings.TrimSpace(line)
	if utf8.RuneCountInString(t) < 3 {
		return false
	}
	return !bareSlashCommand.MatchString(t)
}

// inputTitleDeriver is a line editor over the raw input byte stream: it
// reconstructs what the user typed well enough to recognise a submitted line,
// then LATCHES the first eligible one for the life of the session.
//
// It deliberately models only what a title needs — printable runs, backspace,
// Ctrl-C, and enough escape-sequence structure not to be fooled by arrow keys or
// a bracketed paste. It is not a terminal emulator and must never become one:
// everything it cannot model it ignores, which costs at worst a slightly wrong
// title on an exotic input.
//
// Not safe for concurrent use; the Handler owns one per session under h.mu.
type inputTitleDeriver struct {
	// Field order is load-bearing for govet fieldalignment: ending the
	// pointer-bearing prefix on the string keeps the slice's pointer earlier in
	// the struct. Re-check the linter when adding a field.
	latched string // the derived title, once found; "" until then
	line    []byte // the current line under construction
	// inPaste tracks a bracketed paste (ESC[200~ … ESC[201~) so newlines inside
	// a pasted multi-line message do not read as separate submissions. It is a
	// MODE the guards toggle, not a half-parsed sequence, so unlike the escape
	// scanner's state it survives across chunks.
	inPaste bool
}

// escape-sequence scanner states.
const (
	escNone = iota // ordinary bytes
	escSeen        // saw ESC
	escCSI         // inside a CSI (ESC [ …)
	escSS3         // inside an SS3 (ESC O …), one more byte
)

// pasteGuard reports a bracketed-paste boundary the scanner recognised.
type pasteGuard int

const (
	pasteNone pasteGuard = iota
	pasteStart
	pasteEnd
)

// escScanner consumes escape sequences so their bytes never reach the title: an
// arrow key must not add characters, and a CSI's parameter bytes must not be read
// as text. A separate type from the deriver because its state is per-CHUNK while
// the deriver's is per-session — see observe for why that distinction matters.
type escScanner struct {
	params []byte
	state  int
}

// scan reports whether b belonged to an escape sequence (and so is not content),
// plus any bracketed-paste boundary it recognised.
func (e *escScanner) scan(b byte) (consumed bool, guard pasteGuard) {
	switch e.state {
	case escSeen:
		switch b {
		case '[':
			e.state = escCSI
		case 'O':
			e.state = escSS3
		default:
			e.state = escNone
		}
		return true, pasteNone
	case escCSI:
		switch {
		case b >= 0x40 && b <= 0x7e: // final byte
			guard = e.pasteBoundary(b)
			e.params = e.params[:0]
			e.state = escNone
		case b >= 0x30 && b <= 0x3f: // parameter byte
			e.params = append(e.params, b)
		}
		return true, guard
	case escSS3:
		e.state = escNone // final byte
		return true, pasteNone
	}
	if b == 0x1b {
		e.state = escSeen
		return true, pasteNone
	}
	return false, pasteNone
}

// pasteBoundary recognises ESC[200~ (start) and ESC[201~ (end), the guards that
// mark a bracketed paste.
func (e *escScanner) pasteBoundary(final byte) pasteGuard {
	if final != '~' {
		return pasteNone
	}
	switch string(e.params) {
	case "200":
		return pasteStart
	case "201":
		return pasteEnd
	}
	return pasteNone
}

// observe feeds one atomic input chunk — one key's full sequence, one whole
// paste, one composition commit. Read the result with title(); every call after
// latching is a no-op.
//
// ONE CHUNK IS ONE INPUT EVENT, which is what makes the escape scanner safe to
// build fresh per chunk: a sequence never straddles two chunks, while a lone ESC
// (the Escape key, a mobile toolbar's ESC button) is a complete input by itself.
// Carrying the scanner state across chunks made that lone ESC swallow the next
// character typed — press Escape to cancel, type the next prompt, and its title
// lost its opening letter.
func (d *inputTitleDeriver) observe(chunk []byte) {
	if d.latched != "" {
		return
	}
	var esc escScanner // per-chunk, deliberately: see above
	for i := range chunk {
		b := chunk[i]
		if consumed, guard := esc.scan(b); consumed {
			switch guard {
			case pasteStart:
				d.inPaste = true
			case pasteEnd:
				d.inPaste = false
			case pasteNone:
			}
			continue
		}
		// Stop at the latch: the rest of this chunk cannot contribute to a name we
		// already have. (It could not overwrite it either — foldNewline scans the
		// whole remainder, so a chunk holding a second line folds rather than
		// submitting twice — but not doing the work is clearer than relying on
		// that.)
		if d.step(chunk, i, b) != "" {
			return
		}
	}
}

// step consumes one CONTENT byte (one the escape scanner did not claim) and
// returns the derived title if this byte completed an eligible line.
func (d *inputTitleDeriver) step(chunk []byte, i int, b byte) string {
	switch {
	case b == '\r' || b == '\n':
		if d.foldNewline(chunk, i) {
			return ""
		}
		return d.submit()
	case b == 0x7f || b == 0x08:
		d.backspace()
	case b == 0x03:
		d.line = d.line[:0] // Ctrl-C cancels the line
	case b >= 0x20:
		d.line = append(d.line, b) // printable ASCII or a UTF-8 byte
	}
	return ""
}

// foldNewline decides whether the newline at index i is a soft break inside one
// logical message rather than a submission, and folds it to a single space if so.
//
// Two cases fold: inside a bracketed paste, and a newline FOLLOWED by more
// printable input in the SAME chunk. A human pressing Enter sends the newline as
// the end of its own input event, whereas a paste (even one sent without
// bracketed-paste guards — an agent shell that keeps a pasted multi-line message
// as a single prompt) delivers text + newline + text together. Without this, such
// a paste left only its LAST line as the title.
func (d *inputTitleDeriver) foldNewline(chunk []byte, i int) bool {
	soft := d.inPaste
	if !soft {
		for _, nb := range chunk[i+1:] {
			if nb >= 0x20 {
				soft = true
				break
			}
		}
	}
	if !soft {
		return false
	}
	if len(d.line) > 0 && d.line[len(d.line)-1] != ' ' {
		d.line = append(d.line, ' ')
	}
	return true
}

// submit ends the current line and latches it when eligible, returning the
// latched title (or "" when the line was empty, ineligible, or not valid text).
func (d *inputTitleDeriver) submit() string {
	line := strings.TrimSpace(string(d.line))
	d.line = d.line[:0]
	// A client can send anything; a title that is not valid UTF-8 would reach
	// JSON encoding as replacement characters, so drop it rather than store it.
	if line == "" || !utf8.ValidString(line) || !isEligibleTitle(line) {
		return ""
	}
	d.latched = sanitizeTitle(line, maxDerivedTitleRunes)
	return d.latched
}

// backspace removes one whole codepoint: pop the UTF-8 continuation bytes, then
// the lead byte.
func (d *inputTitleDeriver) backspace() {
	for len(d.line) > 0 && d.line[len(d.line)-1]&0xc0 == 0x80 {
		d.line = d.line[:len(d.line)-1]
	}
	if len(d.line) > 0 {
		d.line = d.line[:len(d.line)-1]
	}
}

// title returns the latched derived title, or "" if nothing has latched yet. Safe
// on a nil receiver, which is how a Handler without WithInputTitle reads as "no
// derived title" with no branch at the call sites.
func (d *inputTitleDeriver) title() string {
	if d == nil {
		return ""
	}
	return d.latched
}
