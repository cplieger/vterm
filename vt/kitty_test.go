package vt

import (
	"strconv"
	"strings"
	"testing"
)

// queryKbd issues the kitty query CSI ? u and returns the reply, clearing the
// response buffer before and after so each call reads only its own answer.
func queryKbd(s *Screen) string {
	s.response = s.response[:0]
	s.Write([]byte("\x1b[?u"))
	out := string(s.response)
	s.response = s.response[:0]
	return out
}

// CSI = flags ; mode u sets flags: mode 1 replaces all, mode 2 sets the given
// bits, mode 3 resets the given bits. Only the disambiguate bit (0x1) is
// honored, so the sequence exercises the three modes against that bit.
func TestKittySetFlagsModes(t *testing.T) {
	s := New(5, 20)
	if got := queryKbd(s); got != "\x1b[?0u" {
		t.Fatalf("initial query = %q, want CSI ?0u", got)
	}
	s.Write([]byte("\x1b[=1;1u")) // set-all -> 1
	if got := queryKbd(s); got != "\x1b[?1u" {
		t.Errorf("after =1;1u query = %q, want CSI ?1u", got)
	}
	s.Write([]byte("\x1b[=1;3u")) // reset bit 0x1 -> 0
	if got := queryKbd(s); got != "\x1b[?0u" {
		t.Errorf("after =1;3u query = %q, want CSI ?0u", got)
	}
	s.Write([]byte("\x1b[=1;2u")) // set bit 0x1 -> 1
	if got := queryKbd(s); got != "\x1b[?1u" {
		t.Errorf("after =1;2u query = %q, want CSI ?1u", got)
	}
	s.Write([]byte("\x1b[=0;1u")) // set-all -> 0
	if got := queryKbd(s); got != "\x1b[?0u" {
		t.Errorf("after =0;1u query = %q, want CSI ?0u", got)
	}
}

// Unsupported flags (everything but disambiguate 0x1) are masked off on store,
// so the query truthfully reports only the honored subset. This lets an app
// that needs an unsupported flag detect the gap and fall back.
func TestKittyUnsupportedFlagsMaskedOff(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[=31;1u")) // request all five flags (0x1f)
	if got := queryKbd(s); got != "\x1b[?1u" {
		t.Errorf("after =31;1u query = %q, want CSI ?1u (only 0x1 honored)", got)
	}
	s2 := New(5, 20)
	s2.Write([]byte("\x1b[>30u")) // push flags 0x1e (no disambiguate bit) -> masked to 0
	if got := queryKbd(s2); got != "\x1b[?0u" {
		t.Errorf("after >30u query = %q, want CSI ?0u (only unsupported bits requested)", got)
	}
}

// CSI > flags u pushes (saving the current flags) and CSI < n u pops (restoring
// them). Push with no parameter defaults to flags 0.
func TestKittyPushPop(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[>1u")) // push: save 0, current = 1
	if got := queryKbd(s); got != "\x1b[?1u" {
		t.Errorf("after push >1u query = %q, want CSI ?1u", got)
	}
	s.Write([]byte("\x1b[>u")) // push with no param: save 1, current = 0
	if got := queryKbd(s); got != "\x1b[?0u" {
		t.Errorf("after push >u (default 0) query = %q, want CSI ?0u", got)
	}
	s.Write([]byte("\x1b[<u")) // pop 1 (default): restore 1
	if got := queryKbd(s); got != "\x1b[?1u" {
		t.Errorf("after pop <u query = %q, want CSI ?1u", got)
	}
	s.Write([]byte("\x1b[<1u")) // pop 1: restore 0
	if got := queryKbd(s); got != "\x1b[?0u" {
		t.Errorf("after pop <1u query = %q, want CSI ?0u", got)
	}
}

// Popping past the bottom of the stack resets all flags to 0 (per spec).
func TestKittyPopEmptiesResets(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[=1;1u")) // current = 1, stack empty
	s.Write([]byte("\x1b[<5u"))   // pop more than the stack holds -> reset
	if got := queryKbd(s); got != "\x1b[?0u" {
		t.Errorf("after over-pop query = %q, want CSI ?0u (reset)", got)
	}
}

// The push stack is bounded: pushing past the cap evicts the oldest entry and
// never panics; subsequent pops still restore the most-recent saved values.
func TestKittyStackBounded(t *testing.T) {
	s := New(5, 20)
	for range maxKbdStack + 8 {
		s.Write([]byte("\x1b[>1u")) // push flags=1 repeatedly (saves prior current)
	}
	// Pop everything back out; must not panic and must end at flags 0.
	for range maxKbdStack + 8 {
		s.Write([]byte("\x1b[<1u"))
	}
	if got := queryKbd(s); got != "\x1b[?0u" {
		t.Errorf("after drain query = %q, want CSI ?0u", got)
	}
}

// Eviction is not silent: the cap means the value saved by the FIRST of
// maxKbdStack+1 pushes is dropped, so a pop that deep finds the stack already
// empty and resets to 0 instead of restoring it. Without the cap the same pop
// would hand the application back a mode it set before the flood.
func TestKittyPushEvictsTheOldestSavedFlags(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[=1;1u")) // current = 1, the value the first push saves
	for range maxKbdStack + 1 {
		s.Write([]byte("\x1b[>0u")) // push: save current, current = 0
	}
	s.Write([]byte("\x1b[<" + strconv.Itoa(maxKbdStack+1) + "u")) // pop every push
	if got := queryKbd(s); got != "\x1b[?0u" {
		t.Errorf("after %d pushes and a %d-deep pop, query = %q, want CSI ?0u (the oldest saved 1 was evicted)",
			maxKbdStack+1, maxKbdStack+1, got)
	}
}

// CSI < n u pops n entries, not one: a two-deep pop unwinds both levels and
// restores the flags that were current before the FIRST of two pushes.
func TestKittyPopUnwindsEveryRequestedLevel(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[=1;1u")) // current = 1
	s.Write([]byte("\x1b[>0u"))   // push: save 1, current = 0
	s.Write([]byte("\x1b[>0u"))   // push: save 0, current = 0
	s.Write([]byte("\x1b[<2u"))   // pop both levels
	if got := queryKbd(s); got != "\x1b[?1u" {
		t.Errorf("after two pushes and CSI <2u, query = %q, want CSI ?1u (both levels unwound)", got)
	}
}

// The main and alternate screens keep independent flags: enabling the protocol
// on the alt screen must not disturb the main-screen mode (spec requirement).
func TestKittyAltScreenIndependent(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[=1;1u")) // main flags = 1
	if s.KeyboardFlags() != 1 {
		t.Fatalf("main KeyboardFlags = %d, want 1", s.KeyboardFlags())
	}
	s.Write([]byte("\x1b[?1049h")) // enter alt screen
	if got := queryKbd(s); got != "\x1b[?0u" {
		t.Errorf("alt-screen initial query = %q, want CSI ?0u (independent stack)", got)
	}
	s.Write([]byte("\x1b[=1;1u")) // alt flags = 1 (independently of main)
	if s.KeyboardFlags() != 1 {
		t.Errorf("alt KeyboardFlags = %d, want 1", s.KeyboardFlags())
	}
	s.Write([]byte("\x1b[<u"))     // pop the alt stack empty -> alt resets to 0
	s.Write([]byte("\x1b[?1049l")) // exit alt screen
	if s.KeyboardFlags() != 1 {
		t.Errorf("main KeyboardFlags after alt exit = %d, want 1 (undisturbed)", s.KeyboardFlags())
	}
	if got := queryKbd(s); got != "\x1b[?1u" {
		t.Errorf("main query after alt exit = %q, want CSI ?1u", got)
	}
}

// RIS (hard reset) clears the flags/stacks for both screens.
func TestKittyRISResets(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[=1;1u"))  // main flags = 1
	s.Write([]byte("\x1b[?1049h")) // alt
	s.Write([]byte("\x1b[=1;1u"))  // alt flags = 1
	s.Write([]byte("\x1bc"))       // RIS
	if s.InAltScreen {
		t.Fatalf("RIS did not leave the alt screen")
	}
	if got := queryKbd(s); got != "\x1b[?0u" {
		t.Errorf("main query after RIS = %q, want CSI ?0u", got)
	}
	s.Write([]byte("\x1b[?1049h")) // re-enter alt
	if got := queryKbd(s); got != "\x1b[?0u" {
		t.Errorf("alt query after RIS = %q, want CSI ?0u", got)
	}
}

// Detection: the query reply precedes a following primary-DA answer, so an app
// that sends CSI ?u then CSI c gets the kitty reply first (the mechanism apps
// use to detect protocol support).
func TestKittyQueryPrecedesDA(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[=1;1u"))
	s.response = s.response[:0]
	s.Write([]byte("\x1b[?u")) // query
	s.Write([]byte("\x1b[c"))  // primary DA
	got := string(s.response)
	if !strings.HasPrefix(got, "\x1b[?1u") {
		t.Errorf("combined response = %q, want it to start with the kitty reply CSI ?1u", got)
	}
	if !strings.Contains(got, "\x1b[?65;") {
		t.Errorf("combined response = %q, want it to also contain the primary DA reply", got)
	}
}

// A pop that lands EXACTLY on the empty stack restores the saved bottom value
// (not a blind reset); only popping PAST the bottom resets to 0. This pins the
// boundary in the spec rule "a pop that empties the stack resets all flags".
func TestKittyPopToExactEmptyRestores(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[=1;1u")) // current = 1
	s.Write([]byte("\x1b[>0u"))   // push: save 1, current = 0 (stack holds the saved 1)
	s.Write([]byte("\x1b[<1u"))   // pop exactly one -> restore the saved 1, stack now empty
	if got := queryKbd(s); got != "\x1b[?1u" {
		t.Errorf("pop-to-exact-empty query = %q, want CSI ?1u (restore saved value, not reset)", got)
	}
}

// CSI = u with no parameters uses flags 0 and mode 1 (replace-all), clearing the
// current flags.
func TestKittySetNoParamsClears(t *testing.T) {
	s := New(5, 20)
	s.Write([]byte("\x1b[=1;1u")) // current = 1
	s.Write([]byte("\x1b[=u"))    // no params: flags default 0, mode default 1 -> current = 0
	if got := queryKbd(s); got != "\x1b[?0u" {
		t.Errorf("CSI =u query = %q, want CSI ?0u (defaults clear)", got)
	}
}
