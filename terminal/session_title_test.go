package terminal

// Tests for the session title model: effectiveTitle's precedence, the user-pinned
// name (REST + precedence + status-stream push), and the server-derived automatic
// title's confirmation window. The Linux discovery half is in
// proctitle_linux_test.go.

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEffectiveTitlePrecedence walks EVERY combination of the four title sources
// (2^4 = 16) rather than a hand-picked table, so the order is pinned by
// construction and a new rung cannot be added without extending the check. The
// rungs slice IS the precedence: the highest-priority source present must win.
func TestEffectiveTitlePrecedence(t *testing.T) {
	rungs := []struct {
		name  string
		value string
		set   func(*titleSources)
	}{
		{"pinned", "PIN", func(s *titleSources) { s.pinned = "PIN" }},
		{"osc", "OSC", func(s *titleSources) { s.osc = "OSC" }},
		{"client", "CLI", func(s *titleSources) { s.client = "CLI" }},
		{"auto", "AUTO", func(s *titleSources) { s.auto = "AUTO" }},
	}
	for mask := range 1 << len(rungs) {
		var src titleSources
		var present []string
		want := ""
		for i, r := range rungs {
			if mask&(1<<i) == 0 {
				continue
			}
			r.set(&src)
			present = append(present, r.name)
			if want == "" {
				want = r.value // rungs are in precedence order, so the first set wins
			}
		}
		name := "none"
		if len(present) > 0 {
			name = strings.Join(present, "+")
		}
		t.Run(name, func(t *testing.T) {
			if got := effectiveTitle(&src); got != want {
				t.Errorf("effectiveTitle(%+v) = %q, want %q", src, got, want)
			}
		})
	}
}

// TestPinnedTitleOutranksAutomaticSources is R4 end to end through the manager:
// a pinned name wins over both a client-derived title and the automatic title,
// and clearing it reveals the source below rather than a blank label. The
// "reveals a real name" half is the whole reason the pin MASKS the automatic
// title instead of replacing it.
func TestPinnedTitleOutranksAutomaticSources(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })

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

	if !m.SetSessionTitle(id, "client label") {
		t.Fatal("SetSessionTitle = false, want true")
	}
	if got := titleOf(t); got != "client label" {
		t.Fatalf("Title with only a client label = %q, want %q", got, "client label")
	}

	if !m.SetSessionPinnedTitle(id, "my name") {
		t.Fatal("SetSessionPinnedTitle = false, want true")
	}
	if got := titleOf(t); got != "my name" {
		t.Fatalf("Title with a pin = %q, want %q (the pin must outrank the client label)", got, "my name")
	}
	// The raw pin is exposed so a UI can offer to remove it.
	for _, info := range m.List() {
		if info.ID == id && info.PinnedTitle != "my name" {
			t.Fatalf("PinnedTitle = %q, want %q", info.PinnedTitle, "my name")
		}
	}

	if !m.ClearSessionPinnedTitle(id) {
		t.Fatal("ClearSessionPinnedTitle = false, want true")
	}
	if got := titleOf(t); got != "client label" {
		t.Fatalf("Title after clearing the pin = %q, want the masked source %q", got, "client label")
	}

	// Unknown ids are reported, not silently accepted.
	if m.SetSessionPinnedTitle("nonexistent", "x") {
		t.Fatal("SetSessionPinnedTitle(unknown id) = true, want false")
	}
	if m.ClearSessionPinnedTitle("nonexistent") {
		t.Fatal("ClearSessionPinnedTitle(unknown id) = true, want false")
	}
}

// TestAutoTitleSeededFromCommand pins the ladder's last rung and the reason it is
// seeded at Create rather than computed on demand: a List served before the first
// status sweep must still name the session. catFactory runs /bin/cat, so the
// basename is "cat".
func TestAutoTitleSeededFromCommand(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })
	m.stopSweep() // no sweep has run, so only the Create-time seed can answer

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, info := range m.List() {
		if info.ID == id && info.Title != "cat" {
			t.Fatalf("Title of a fresh unsweept session = %q, want the command basename %q", info.Title, "cat")
		}
	}
}

// TestListReadsConfirmedAutoTitle proves List reports the sweep's CONFIRMED
// automatic title rather than computing one of its own: with a sentinel written
// into the session's stored value, List must return exactly that. This is the
// single-source-of-truth rule — a List that probed independently would both
// bypass the confirmation window and disagree with the live status stream.
func TestListReadsConfirmedAutoTitle(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })
	m.stopSweep()

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	m.mu.Lock()
	m.sessions[id].autoTitle = "sentinel-not-a-real-process"
	m.mu.Unlock()

	for _, info := range m.List() {
		if info.ID == id && info.Title != "sentinel-not-a-real-process" {
			t.Fatalf("List Title = %q, want the stored confirmed value", info.Title)
		}
	}
}

// TestConfirmAutoTitleWindow is the confirmation state machine, driven directly
// with a synthetic candidateSince so the 500ms window is exercised without
// sleeping. Four behaviours: a fresh candidate is NOT adopted, one that has held
// the terminal long enough IS, a changed candidate restarts the window while
// HOLDING the previous title (so vim -> less does not detour through the
// directory name), and the shell regaining the foreground rests immediately.
func TestConfirmAutoTitleWindow(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })

	// newCase builds an isolated session + tracker; confirmAutoTitle needs no
	// manager lock here because nothing else touches these.
	newCase := func(startTitle string) (*session, *statusTracker) {
		return &session{autoTitle: startTitle}, &statusTracker{}
	}
	probe := func(pgid int, name, cwd string) *statusRaw {
		return &statusRaw{autoProbe: autoTitleProbe{ok: true, pgid: pgid, procName: name, cwdBase: cwd}}
	}

	t.Run("fresh candidate is not adopted", func(t *testing.T) {
		s, tr := newCase("workspace")
		m.confirmAutoTitle(s, probe(42, "vim", "workspace"), tr)
		if s.autoTitle != "workspace" {
			t.Errorf("autoTitle = %q, want the held %q (a 30ms command must not flash into the label)", s.autoTitle, "workspace")
		}
		if tr.candidatePGID != 42 || tr.candidateSince.IsZero() {
			t.Errorf("window not armed: candidatePGID=%d candidateSince=%v", tr.candidatePGID, tr.candidateSince)
		}
	})

	t.Run("an armed candidate inside the window is still not adopted", func(t *testing.T) {
		// The distinguishing case against a sample-COUNT implementation: the
		// candidate is already armed and matches, so "the second observation
		// adopts" would pass here. Only elapsed time may decide.
		s, tr := newCase("workspace")
		tr.candidatePGID = 42
		tr.candidateSince = time.Now().Add(-autoTitleConfirm + 50*time.Millisecond)
		m.confirmAutoTitle(s, probe(42, "vim", "workspace"), tr)
		if s.autoTitle != "workspace" {
			t.Errorf("autoTitle = %q, want the held %q: %v had not elapsed yet", s.autoTitle, "workspace", autoTitleConfirm)
		}
	})

	t.Run("candidate held past the window is adopted", func(t *testing.T) {
		s, tr := newCase("workspace")
		tr.candidatePGID = 42
		tr.candidateSince = time.Now().Add(-autoTitleConfirm - time.Millisecond)
		m.confirmAutoTitle(s, probe(42, "vim", "workspace"), tr)
		if s.autoTitle != "vim" {
			t.Errorf("autoTitle = %q, want %q after the confirmation window", s.autoTitle, "vim")
		}
	})

	t.Run("a new candidate restarts the window and holds the old title", func(t *testing.T) {
		s, tr := newCase("vim") // vim was confirmed earlier
		tr.candidatePGID = 42
		tr.candidateSince = time.Now().Add(-time.Hour)
		m.confirmAutoTitle(s, probe(99, "less", "workspace"), tr)
		if s.autoTitle != "vim" {
			t.Errorf("autoTitle = %q, want the held %q (no detour through the cwd)", s.autoTitle, "vim")
		}
		if tr.candidatePGID != 99 {
			t.Errorf("candidatePGID = %d, want the new candidate 99", tr.candidatePGID)
		}
		if time.Since(tr.candidateSince) > time.Second {
			t.Error("candidateSince was not reset for the new candidate")
		}
	})

	t.Run("shell regaining the foreground rests immediately", func(t *testing.T) {
		s, tr := newCase("vim")
		tr.candidatePGID = 42
		tr.candidateSince = time.Now().Add(-time.Hour)
		m.confirmAutoTitle(s, probe(0, "", "workspace"), tr)
		if s.autoTitle != "workspace" {
			t.Errorf("autoTitle = %q, want %q — falling back is NOT debounced", s.autoTitle, "workspace")
		}
		if tr.candidatePGID != 0 || !tr.candidateSince.IsZero() {
			t.Errorf("window not disarmed: candidatePGID=%d candidateSince=%v", tr.candidatePGID, tr.candidateSince)
		}
	})

	t.Run("an unreadable cwd keeps the seeded command basename", func(t *testing.T) {
		s, tr := newCase("cat")
		m.confirmAutoTitle(s, probe(0, "", ""), tr)
		if s.autoTitle != "cat" {
			t.Errorf("autoTitle = %q, want the seeded %q when the cwd is unreadable", s.autoTitle, "cat")
		}
	})

	t.Run("no probe holds the current title", func(t *testing.T) {
		s, tr := newCase("vim")
		m.confirmAutoTitle(s, &statusRaw{}, tr) // ok=false: OSC-titled, exited, or unsupported
		if s.autoTitle != "vim" {
			t.Errorf("autoTitle = %q, want the held %q when no probe ran", s.autoTitle, "vim")
		}
	})
}

// TestSessionManagerPinnedTitleREST exercises the pinned-title routes: PUT sets
// (204) and is bounded and sanitized, an empty title is a 400 rather than a
// silent clear (clearing has its own verb), DELETE clears and is idempotent, and
// both 404 on an unknown id.
//
// The subtests are ordered stages over ONE session's pin (set, sanitize,
// truncate, refuse, clear) and must run IN SEQUENCE: a rejected write is
// asserted against the pin the previous stage left in place, which is what
// proves the rejection changed nothing. Consequence, stated so nobody is
// surprised by it: the refuse-stage subtests do NOT pass when run alone with
// -run, because the pin they expect to survive was never set. Only the
// clear stage is self-sufficient (it sets its own pin first). A stage that
// fails also invalidates the stages after it — read the FIRST failure.
func TestSessionManagerPinnedTitleREST(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })
	srv := httptest.NewServer(m.RESTHandler())
	t.Cleanup(srv.Close)

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	do := func(t *testing.T, method string, sessionID SessionID, body string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), method,
			srv.URL+"/api/sessions/"+string(sessionID)+"/pinned-title", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	pinOf := func(t *testing.T) string {
		t.Helper()
		for _, info := range m.List() {
			if info.ID == id {
				return info.PinnedTitle
			}
		}
		t.Fatalf("session %s not in List()", id)
		return ""
	}

	t.Run("PUT sets the pin", func(t *testing.T) {
		if code := do(t, http.MethodPut, id, `{"title":"my tab"}`); code != http.StatusNoContent {
			t.Fatalf("PUT known id status = %d, want 204", code)
		}
		if got := pinOf(t); got != "my tab" {
			t.Errorf("PinnedTitle after PUT = %q, want %q", got, "my tab")
		}
	})

	t.Run("control characters are stripped", func(t *testing.T) {
		if code := do(t, http.MethodPut, id, `{"title":"a\nb"}`); code != http.StatusNoContent {
			t.Fatalf("PUT with control chars status = %d, want 204", code)
		}
		if got := pinOf(t); got != "ab" {
			t.Errorf("PinnedTitle = %q, want control characters stripped (%q)", got, "ab")
		}
	})

	t.Run("a long title truncates at the pinned cap", func(t *testing.T) {
		long := strings.Repeat("x", 200)
		if code := do(t, http.MethodPut, id, `{"title":"`+long+`"}`); code != http.StatusNoContent {
			t.Fatalf("PUT long title status = %d, want 204", code)
		}
		if got := pinOf(t); len([]rune(got)) != maxPinnedTitleRunes {
			t.Errorf("PinnedTitle length = %d, want the pinned cap %d", len([]rune(got)), maxPinnedTitleRunes)
		}
	})

	t.Run("an effectively empty title is a 400, not a silent clear", func(t *testing.T) {
		// The destructive operation has its own verb and must not be reachable
		// by an accidentally-empty (or whitespace-only, or all-control) body.
		//
		// Set the pin here rather than inheriting the previous case's, for the
		// reason the DELETE case gives: run alone the earlier cases have not
		// executed, and "the pin did not change" asserted against a pin nobody
		// set passes vacuously. Naming the expected value also beats asserting
		// its LENGTH, which the truncation case happened to make unique.
		const keep = "pinned before the rejections"
		if code := do(t, http.MethodPut, id, `{"title":"`+keep+`"}`); code != http.StatusNoContent {
			t.Fatalf("PUT before the rejections status = %d, want 204", code)
		}
		for _, body := range []string{`{"title":""}`, `{"title":"   "}`, `{"title":"\n\n"}`, `{}`} {
			if code := do(t, http.MethodPut, id, body); code != http.StatusBadRequest {
				t.Errorf("PUT %s status = %d, want 400", body, code)
			}
		}
		if got := pinOf(t); got != keep {
			t.Errorf("a rejected PUT changed the pin to %q, want %q untouched", got, keep)
		}
	})

	t.Run("a malformed body is a 400", func(t *testing.T) {
		if code := do(t, http.MethodPut, id, `{bad json`); code != http.StatusBadRequest {
			t.Errorf("PUT malformed body status = %d, want 400", code)
		}
	})

	t.Run("PUT on an unknown id is a 404", func(t *testing.T) {
		if code := do(t, http.MethodPut, "nonexistent", `{"title":"x"}`); code != http.StatusNotFound {
			t.Errorf("PUT unknown id status = %d, want 404", code)
		}
	})

	t.Run("DELETE clears and is idempotent", func(t *testing.T) {
		// Set the pin here rather than inheriting it: run alone (go test -run
		// '.../DELETE_clears…') the earlier stages have not executed, and a
		// clear asserted against a never-set pin passes vacuously.
		if code := do(t, http.MethodPut, id, `{"title":"pinned before clear"}`); code != http.StatusNoContent {
			t.Fatalf("PUT before DELETE status = %d, want 204", code)
		}
		if got := pinOf(t); got != "pinned before clear" {
			t.Fatalf("PinnedTitle before DELETE = %q, want %q", got, "pinned before clear")
		}
		if code := do(t, http.MethodDelete, id, ""); code != http.StatusNoContent {
			t.Fatalf("DELETE status = %d, want 204", code)
		}
		if got := pinOf(t); got != "" {
			t.Errorf("PinnedTitle after DELETE = %q, want empty", got)
		}
		// Idempotent: clearing an unpinned session still succeeds.
		if code := do(t, http.MethodDelete, id, ""); code != http.StatusNoContent {
			t.Errorf("second DELETE status = %d, want 204 (idempotent)", code)
		}
	})

	t.Run("DELETE on an unknown id is a 404", func(t *testing.T) {
		if code := do(t, http.MethodDelete, "nonexistent", ""); code != http.StatusNotFound {
			t.Errorf("DELETE unknown id status = %d, want 404", code)
		}
	})

	t.Run("a body over the 4 KiB cap is rejected", func(t *testing.T) {
		if code := do(t, http.MethodPut, id, `{"title":"`+strings.Repeat("y", 5000)+`"}`); code != http.StatusBadRequest {
			t.Errorf("PUT oversized body status = %d, want 400", code)
		}
	})
}

// TestDiffStatusesEmitsPinnedTitleChange is the lastPinnedTitle push guard:
// setting and clearing a pin must reach subscribers even when nothing else about
// the session moved. Without it a rename made in one browser would not appear in
// another until some unrelated change happened to push an event.
func TestDiffStatusesEmitsPinnedTitleChange(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })
	m.stopSweep()

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// An OSC title fixes the effective title, so a pin/clear is the only thing
	// that can move afterwards... except that a pin OUTRANKS the OSC title, so
	// Title moves too; the guard is what makes the CLEAR observable.
	handlerOf(t, m, id).handlePTYData([]byte("\x1b]2;osc label\x07"))

	findEvent := func(events []statusEvent) (statusEvent, bool) {
		t.Helper()
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

	if !m.SetSessionPinnedTitle(id, "my tab") {
		t.Fatal("SetSessionPinnedTitle = false, want true")
	}
	ev, ok := findEvent(m.diffStatuses())
	if !ok {
		t.Fatal("sweep after a pin emitted no event")
	}
	if ev.PinnedTitle != "my tab" || ev.Title != "my tab" {
		t.Fatalf("event = {Title:%q PinnedTitle:%q}, want both %q", ev.Title, ev.PinnedTitle, "my tab")
	}

	if !m.ClearSessionPinnedTitle(id) {
		t.Fatal("ClearSessionPinnedTitle = false, want true")
	}
	ev, ok = findEvent(m.diffStatuses())
	if !ok {
		t.Fatal("sweep after clearing the pin emitted no event")
	}
	if ev.PinnedTitle != "" {
		t.Fatalf("event PinnedTitle after clear = %q, want empty", ev.PinnedTitle)
	}
	if ev.Title != "osc label" {
		t.Fatalf("event Title after clear = %q, want the revealed %q", ev.Title, "osc label")
	}
}

// TestCommandBase pins the portable last rung, including the degenerate cases the
// ladder must not panic on.
func TestCommandBase(t *testing.T) {
	cases := map[string]struct {
		command []string
		want    string
	}{
		"absolute path":  {[]string{"/usr/bin/vim", "-p"}, "vim"},
		"bare name":      {[]string{"cat"}, "cat"},
		"trailing slash": {[]string{"/bin/"}, "bin"},
		"empty command":  {nil, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := NewHandler(tc.command, WithLogger(nil))
			if got := h.commandBase(); got != tc.want {
				t.Errorf("commandBase(%v) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
	// filepath.Base's contract for the filesystem root is the label we want for a
	// session sitting there, so pin it rather than leaving it implied.
	if got := filepath.Base("/"); got != "/" {
		t.Fatalf("filepath.Base(\"/\") = %q, want %q", got, "/")
	}
}
