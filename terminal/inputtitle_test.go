package terminal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// TestIsEligibleTitle pins the two-rule filter, including the limitations it
// deliberately accepts (documented on isEligibleTitle).
func TestIsEligibleTitle(t *testing.T) {
	cases := map[string]bool{
		"refactor the auth module": true,
		"fix":                      true, // three characters is the floor
		"  fix  ":                  true, // trimmed before measuring
		"y":                        false,
		"ok":                       false,
		"1":                        false,
		"   ":                      false,
		"":                         false,
		"/model":                   false, // a bare slash command is a directive
		"/clear":                   false,
		"/a":                       false, // short AND a command
		"/tmp/x.log what is this":  true,  // a path is a message, not a command
		"/model opus":              true,  // accepted limitation: an argument makes it eligible
		"yes":                      true,  // accepted limitation: no affirmation stop-list
		"héé":                      true,  // three RUNES, not three bytes
	}
	for line, want := range cases {
		t.Run(line, func(t *testing.T) {
			if got := isEligibleTitle(line); got != want {
				t.Errorf("isEligibleTitle(%q) = %v, want %v", line, got, want)
			}
		})
	}
}

// feed runs a sequence of input chunks through a fresh deriver and returns the
// latched title. Each element is ONE atomic input event, which is the contract
// the escape parser relies on — so the split points are part of the test.
func feed(chunks ...string) string {
	d := &inputTitleDeriver{}
	for _, c := range chunks {
		d.observe([]byte(c))
	}
	return d.title()
}

// TestInputTitleDeriverLatch is the core behaviour: the FIRST eligible submitted
// line becomes the name and nothing later replaces it.
func TestInputTitleDeriverLatch(t *testing.T) {
	if got := feed("refactor the auth module\r"); got != "refactor the auth module" {
		t.Errorf("first line = %q, want the submitted line", got)
	}
	// Later lines are ignored, however substantial.
	if got := feed("refactor the auth module\r", "and also update the docs\r"); got != "refactor the auth module" {
		t.Errorf("latched title = %q, want the FIRST line to stick", got)
	}
	// An ineligible first line does not consume the latch.
	if got := feed("y\r", "ok\r", "/model\r", "now the real request\r"); got != "now the real request" {
		t.Errorf("latched title = %q, want the first ELIGIBLE line", got)
	}
	// Nothing submitted yet: no title.
	if got := feed("half a sentence"); got != "" {
		t.Errorf("unsubmitted line = %q, want empty", got)
	}
	// \n submits as well as \r.
	if got := feed("newline submits\n"); got != "newline submits" {
		t.Errorf("newline submit = %q", got)
	}
	// Two lines in ONE chunk fold into one logical message rather than submitting
	// twice, even with control bytes between them: foldNewline scans the whole
	// remainder for printable content, which is what keeps a pasted block's START
	// in the title. (See TestInputTitleDeriverMultilinePaste for the paste cases.)
	if got := feed("first part\r\x01second part\r"); got != "first part second part" {
		t.Errorf("two lines in one chunk = %q, want them folded", got)
	}
}

// TestInputTitleDeriverLineEditing covers the editing bytes the deriver models.
func TestInputTitleDeriverLineEditing(t *testing.T) {
	cases := map[string]struct {
		chunks []string
		want   string
	}{
		"backspace drops a character": {[]string{"helllo worldX", "\x7f", "\r"}, "helllo world"},
		"backspace drops a whole rune": {
			// "café" then one backspace must remove the é (two bytes), not half of it.
			[]string{"caf\u00e9x", "\x7f", "\x7f", "\r"}, "caf",
		},
		"ctrl-c cancels the line":     {[]string{"abandon this", "\x03", "the real one\r"}, "the real one"},
		"ctrl-h backspaces":           {[]string{"oopsX", "\x08", "\r"}, "oops"},
		"backspace on an empty line":  {[]string{"\x7f", "\x7f", "still fine\r"}, "still fine"},
		"control bytes are ignored":   {[]string{"tab\there\r"}, "tabhere"},
		"trailing space is trimmed":   {[]string{"padded   \r"}, "padded"},
		"leading space is trimmed":    {[]string{"   padded\r"}, "padded"},
		"empty submit is not a title": {[]string{"\r", "\r", "real request here\r"}, "real request here"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := feed(tc.chunks...); got != tc.want {
				t.Errorf("feed(%q) = %q, want %q", tc.chunks, got, tc.want)
			}
		})
	}
}

// TestInputTitleDeriverEscapeSequences is the subtle half: cursor keys and other
// escape sequences must not land in the title, and a LONE ESC must not swallow the
// next character typed.
func TestInputTitleDeriverEscapeSequences(t *testing.T) {
	cases := map[string]struct {
		chunks []string
		want   string
	}{
		// Arrow keys (CSI) contribute nothing.
		"arrow keys ignored": {[]string{"before", "\x1b[A", "\x1b[D", "after\r"}, "beforeafter"},
		// SS3 (application-mode cursor keys) likewise.
		"ss3 ignored": {[]string{"before", "\x1bOA", "after\r"}, "beforeafter"},
		// A lone ESC is a complete input event; the NEXT chunk must start clean.
		// Carrying parser state across chunks made this eat the "n".
		"lone esc does not eat the next character": {
			[]string{"\x1b", "next prompt here\r"}, "next prompt here",
		},
		// A lone ESC followed by "[" as its own event: the "[" is content, and the
		// digits after it must not be swallowed as CSI parameters.
		"esc then a literal bracket": {[]string{"\x1b", "[123] a real message\r"}, "[123] a real message"},
		// A CSI arriving in the same chunk as text is still parsed.
		"csi inline with text": {[]string{"real\x1b[C message\r"}, "real message"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := feed(tc.chunks...); got != tc.want {
				t.Errorf("feed(%q) = %q, want %q", tc.chunks, got, tc.want)
			}
		})
	}
}

// TestInputTitleDeriverMultilinePaste pins the rule that keeps a pasted message's
// START as the title rather than only its last line — both with bracketed-paste
// guards and without them (an agent shell that keeps a pasted block as one
// prompt delivers text + newline + text in a single event).
func TestInputTitleDeriverMultilinePaste(t *testing.T) {
	// Bracketed paste: the internal newline folds to a space, and the paste's own
	// trailing Enter (a separate event) submits.
	got := feed("\x1b[200~first line\nsecond line\x1b[201~", "\r")
	if got != "first line second line" {
		t.Errorf("bracketed paste = %q, want the whole message", got)
	}
	// No guards, one event: trailing content marks the newline as a soft break.
	if got := feed("first line\nsecond line\r"); got != "first line second line" {
		t.Errorf("unbracketed paste = %q, want the whole message", got)
	}
	// A human pressing Enter sends the newline as the END of its own event, so it
	// submits rather than folding.
	if got := feed("first line\r", "second line\r"); got != "first line" {
		t.Errorf("two separate Enters = %q, want only the first line", got)
	}
	// A newline followed only by more control bytes is still a submit.
	if got := feed("first line\r\n"); got != "first line" {
		t.Errorf("CRLF = %q, want a single submit", got)
	}
	// The paste MODE survives across chunks even though the escape parser does not.
	if got := feed("\x1b[200~first", " line\nsecond", " line\x1b[201~\r"); got != "first line second line" {
		t.Errorf("paste spanning chunks = %q, want the whole message", got)
	}
}

// TestInputTitleDeriverBounds pins the storage guard and the UTF-8 requirement: a
// huge paste is truncated, and a line that is not valid text is refused outright
// rather than stored as replacement characters.
func TestInputTitleDeriverBounds(t *testing.T) {
	long := strings.Repeat("x", maxDerivedTitleRunes+200)
	got := feed(long + "\r")
	if n := utf8.RuneCountInString(got); n != maxDerivedTitleRunes {
		t.Errorf("long line stored %d runes, want %d", n, maxDerivedTitleRunes)
	}
	// Invalid UTF-8 is refused, and the latch stays open for a real line.
	if got := feed("\xff\xfe\xfd\r", "a real request\r"); got != "a real request" {
		t.Errorf("after invalid UTF-8 = %q, want the latch still open", got)
	}
	// Valid multibyte text is preserved.
	if got := feed("réparer l'authentification\r"); got != "réparer l'authentification" {
		t.Errorf("multibyte line = %q", got)
	}
}

// FuzzInputTitleDeriver drives the deriver with arbitrary input, which is exactly
// what it receives in production: raw bytes from a WebSocket client. The
// invariants are what the rest of the engine relies on — the latched title is
// always valid UTF-8, always control-free (it reaches tab labels, SSE frames and
// potentially log lines), always within its bound, and never changes once set.
func FuzzInputTitleDeriver(f *testing.F) {
	f.Add([]byte("refactor the auth module\r"))
	f.Add([]byte("y\rok\r/model\rthe real request\r"))
	f.Add([]byte("\x1b[200~a\nb\x1b[201~\r"))
	f.Add([]byte("\x1b"))
	f.Add([]byte("\x1b[999999999999~\r"))
	f.Add([]byte("\x7f\x7f\x7f\r"))
	f.Add([]byte("\xff\xfe\r"))
	f.Add([]byte("caf\xc3\xa9\x7f\r"))
	f.Add([]byte{0x00, 0x03, 0x0d})

	f.Fuzz(func(t *testing.T, data []byte) {
		d := &inputTitleDeriver{}
		// Split into chunks at a data-derived boundary so the per-chunk parser
		// reset is exercised too, not just one monolithic feed.
		split := 0
		if len(data) > 0 {
			split = int(data[0]) % (len(data) + 1)
		}
		d.observe(data[:split])
		first := d.title()
		d.observe(data[split:])
		got := d.title()

		if got == "" {
			return
		}
		if !utf8.ValidString(got) {
			t.Fatalf("latched title is not valid UTF-8: %q", got)
		}
		if utf8.RuneCountInString(got) > maxDerivedTitleRunes {
			t.Fatalf("latched title is %d runes, over the %d bound", utf8.RuneCountInString(got), maxDerivedTitleRunes)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("latched title contains a control character %q: %q", r, got)
			}
		}
		if !isEligibleTitle(got) {
			t.Fatalf("latched an ineligible title: %q", got)
		}
		// Latching is permanent: a title seen after the first chunk must survive.
		if first != "" && got != first {
			t.Fatalf("latched title changed from %q to %q", first, got)
		}
		// Further input can never change it either.
		d.observe([]byte("a completely different message\r"))
		if after := d.title(); after != got {
			t.Fatalf("title changed after latching: %q -> %q", got, after)
		}
	})
}

// TestInputTitleReachesTheWireOverWS is the wiring test the unit tests cannot be:
// it drives a REAL WebSocket client, so it fails if handleBinaryFrame ever stops
// feeding the deriver — the one link between "the state machine is correct" and
// "the session gets named".
func TestInputTitleReachesTheWireOverWS(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithWorkDir("/"), WithLogger(nil), WithInputTitle())
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		h.Shutdown()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:bodyclose // library contract: Body is nil on success
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "") // #nosec G104 -- best-effort test cleanup

	// One frame per atomic input event, exactly as a client sends them.
	for _, frame := range []string{"y", "\r", "refactor the auth module", "\r"} {
		if err := ws.Write(ctx, websocket.MessageBinary, []byte(frame)); err != nil {
			t.Fatalf("ws write %q: %v", frame, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, derived := h.titles(); derived != "" {
			if derived != "refactor the auth module" {
				t.Fatalf("derived title = %q, want the first ELIGIBLE line", derived)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("input never produced a derived title; is handleBinaryFrame still feeding the deriver?")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestInputTitleOffByDefault pins the mechanism-not-policy rule: an engine
// consumer that did not ask for a derived title never gets one, however much is
// typed.
func TestInputTitleOffByDefault(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	t.Cleanup(h.Shutdown)
	h.observeInputTitle([]byte("refactor the auth module\r"))
	if _, derived := h.titles(); derived != "" {
		t.Fatalf("derived title = %q with WithInputTitle unset, want empty", derived)
	}
}

// TestInputTitleInSessionPrecedence is the end-to-end contract through the public
// SessionInfo: a derived name outranks the program's OSC title (the only reason to
// ask for derivation is that the program's own title is not worth showing), and a
// user's pin outranks the derived name.
func TestInputTitleInSessionPrecedence(t *testing.T) {
	m := NewSessionManager(func(string) *Handler {
		return NewHandler([]string{"/bin/cat"}, WithLogger(nil), WithInputTitle())
	})
	t.Cleanup(m.Shutdown)
	m.stopSweep()

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	titleOf := func(t *testing.T) string {
		t.Helper()
		for _, info := range m.List() {
			if info.ID == id {
				return info.Title
			}
		}
		t.Fatalf("session %s not in List()", id)
		return ""
	}
	h := handlerOf(t, m, id)

	// An OSC title alone names the session.
	h.handlePTYData([]byte("\x1b]2;osc label\x07"))
	if got := titleOf(t); got != "osc label" {
		t.Fatalf("Title with only an OSC title = %q, want %q", got, "osc label")
	}

	// The derived name outranks it.
	h.observeInputTitle([]byte("refactor the auth module\r"))
	if got := titleOf(t); got != "refactor the auth module" {
		t.Fatalf("Title = %q, want the derived name to outrank the OSC title", got)
	}

	// And a pin outranks the derived name.
	if !m.SetSessionPinnedTitle(id, "my name") {
		t.Fatal("SetSessionPinnedTitle = false")
	}
	if got := titleOf(t); got != "my name" {
		t.Fatalf("Title = %q, want the pin to outrank everything", got)
	}
	// Clearing the pin reveals the derived name again, not the OSC title.
	if !m.ClearSessionPinnedTitle(id) {
		t.Fatal("ClearSessionPinnedTitle = false")
	}
	if got := titleOf(t); got != "refactor the auth module" {
		t.Fatalf("Title after clearing the pin = %q, want the derived name", got)
	}
}

// TestInputTitlePushedOnTheStatusStream pins that a session naming itself midway
// through reaches subscribers: the sweep must emit when the derived title lands,
// or a second client would not see the name until something else changed.
func TestInputTitlePushedOnTheStatusStream(t *testing.T) {
	m := NewSessionManager(func(string) *Handler {
		return NewHandler([]string{"/bin/cat"}, WithLogger(nil), WithInputTitle())
	})
	t.Cleanup(m.Shutdown)
	m.stopSweep()

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	findEvent := func(events []statusEvent) (statusEvent, bool) {
		for _, ev := range events {
			if ev.ID == id {
				return ev, true
			}
		}
		return statusEvent{}, false
	}
	if _, ok := findEvent(m.diffStatuses()); !ok {
		t.Fatal("baseline sweep emitted no event")
	}
	if ev, ok := findEvent(m.diffStatuses()); ok {
		t.Fatalf("quiescent sweep unexpectedly emitted %+v", ev)
	}

	handlerOf(t, m, id).observeInputTitle([]byte("name this session\r"))
	ev, ok := findEvent(m.diffStatuses())
	if !ok {
		t.Fatal("sweep after the title latched emitted no event")
	}
	if ev.Title != "name this session" {
		t.Fatalf("event Title = %q, want the derived name", ev.Title)
	}
}
