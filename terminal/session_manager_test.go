package terminal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// catFactory builds sessions running /bin/cat, which stays alive in a PTY
// (waiting for input) so sessions do not exit mid-test. Logger is discarded.
func catFactory(string) *Handler {
	return NewHandler([]string{"/bin/cat"}, WithLogger(nil))
}

// hasDirective reports whether a Cache-Control value carries the named
// directive, tolerating either ordering and surrounding whitespace so the cache
// tests pin the POLICY rather than one exact spelling of the header.
func hasDirective(header, want string) bool {
	for part := range strings.SplitSeq(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), want) {
			return true
		}
	}
	return false
}

// shutdownManager tears a manager down and fails the test if any teardown did
// not finish inside the budget. Most callers use it from t.Cleanup, which is why
// it builds its own context instead of taking t.Context(): t.Context is already
// cancelled by the time cleanups run, so a wait against it would report an
// expiry on every single test.
//
// The budget is deliberately generous against the real ceiling (cmd.WaitDelay
// bounds the child reap at 5s, and the containment and marker ladders each spend
// up to three containGrace windows), so a failure here means teardown genuinely
// hung rather than that the runner was loaded.
func shutdownManager(t *testing.T, m *SessionManager) {
	t.Helper()
	const budget = 20 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Errorf("SessionManager.Shutdown(ctx) = %v, want nil (teardown must finish within %v)", err, budget)
	}
}

func TestSessionManagerCreateListClose(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })

	id1, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // ensure distinct createdAt ordering
	id2, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	list := m.List()
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}
	// Sorted by creation time: id1 before id2.
	if list[0].ID != id1 || list[1].ID != id2 {
		t.Fatalf("List order = [%s %s], want [%s %s]", list[0].ID, list[1].ID, id1, id2)
	}

	if !m.Close(id1) {
		t.Fatal("Close(id1) = false, want true")
	}
	if m.Close(id1) {
		t.Fatal("Close(id1) twice = true, want false (already gone)")
	}
	if m.Close("nonexistent") {
		t.Fatal("Close(unknown) = true, want false")
	}
	if len(m.List()) != 1 {
		t.Fatalf("List len after close = %d, want 1", len(m.List()))
	}
}

// TestSessionManagerReaperOwnershipKeyed is the N4 regression guard: the reaper
// keys on client presence, not on a per-session socket, so a socketless session
// is NOT reaped while any client is connected, and all sessions are reaped only
// after no client for the idle window. Drives maybeReap directly (in-package)
// with a forced idleSince so it is deterministic and does not depend on the
// background ticker (the 1h window keeps that goroutine quiet during the test).
func TestSessionManagerReaperOwnershipKeyed(t *testing.T) {
	m := NewSessionManager(catFactory, WithIdleReaper(time.Hour))
	t.Cleanup(func() { shutdownManager(t, m) })

	if _, err := m.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A client is present (e.g. another tab's WS or the status stream). Even
	// with the idle window fully elapsed, no session is reaped.
	m.clientConnected()
	m.forceIdleSince(time.Now().Add(-2 * time.Hour))
	m.maybeReap()
	if got := len(m.List()); got != 2 {
		t.Fatalf("reaped %d sessions while a client was present; want 0 reaped (2 remain)", 2-got)
	}

	// Client leaves and the window elapses: all sessions are reaped together.
	m.clientDisconnected()
	m.forceIdleSince(time.Now().Add(-2 * time.Hour))
	m.maybeReap()
	if got := len(m.List()); got != 0 {
		t.Fatalf("List len after reap = %d, want 0", got)
	}
}

// forceIdleSince overrides idleSince for deterministic reaper tests.
func (m *SessionManager) forceIdleSince(ts time.Time) {
	m.mu.Lock()
	m.idleSince = ts
	m.mu.Unlock()
}

func TestSessionManagerREST(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })
	srv := httptest.NewServer(m.RESTHandler())
	t.Cleanup(srv.Close)

	// POST creates a session (201 + id).
	resp, err := http.Post(srv.URL+"/api/sessions", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201", resp.StatusCode)
	}
	var created SessionInfo
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID == "" {
		t.Fatal("POST returned empty id")
	}

	// GET lists the created session.
	respL, err := http.Get(srv.URL + "/api/sessions")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var list []SessionInfo
	_ = json.NewDecoder(respL.Body).Decode(&list)
	respL.Body.Close()
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("GET list = %+v, want one session %s", list, created.ID)
	}

	// DELETE removes it (204), a second DELETE is 404.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodDelete, srv.URL+"/api/sessions/"+string(created.ID), nil)
	respD, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	respD.Body.Close()
	if respD.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", respD.StatusCode)
	}
	req2, _ := http.NewRequestWithContext(t.Context(), http.MethodDelete, srv.URL+"/api/sessions/"+string(created.ID), nil)
	respD2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("DELETE 2: %v", err)
	}
	respD2.Body.Close()
	if respD2.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE unknown status = %d, want 404", respD2.StatusCode)
	}
}

func TestSessionManagerWSUnknownSession(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })
	srv := httptest.NewServer(m.WebSocketHandler())
	t.Cleanup(srv.Close)

	// A plain GET (no WS upgrade) for an unknown session answers 426 — the
	// SAME status a known session's handler gives a non-upgrade request, so
	// a probe cannot distinguish session existence (the old 404 was an
	// oracle). A real WS dial gets the definitive 4004 close instead
	// (TestWebSocketUnknownSessionClosesDefinitively).
	resp, err := http.Get(srv.URL + "/ws?session=bogus")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("unknown session status = %d, want 426", resp.StatusCode)
	}
}

// TestSessionManagerWSAttach dials a real session through the manager and
// verifies the connection is served (a screen frame arrives) and that client
// presence is tracked around the attachment.
func TestSessionManagerWSAttach(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	srv := httptest.NewServer(m.WebSocketHandler())
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?session=" + string(id)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	//nolint:bodyclose // coder/websocket Dial nils resp.Body on success
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	// Reading at least one frame proves the manager routed to the session.
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatalf("ws read: %v", err)
	}
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

// TestSessionManager_ConcurrentCreateCloseList stresses the manager's own
// mutex, documented "Safe for concurrent use": many goroutines Create,
// List, Close, and toggle client presence concurrently while the always-on
// sweepLoop reads the same maps/counters. Run under -race to surface data
// races on sessions/trackers and the created/closed/reaped counters. The
// registry and pingStat carry dedicated -race tests; the manager only had
// incidental sweepLoop coverage. catFactory keeps each PTY alive so Close is
// the only remover. Uses a done-channel barrier so no new import is needed.
func TestSessionManager_ConcurrentCreateCloseList(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })

	const goroutines = 9
	const iters = 12
	done := make(chan struct{}, goroutines)
	for g := range goroutines {
		go func(id int) {
			for range iters {
				switch id % 4 {
				case 0:
					if sid, err := m.Create(); err == nil {
						m.Close(sid)
					}
				case 1:
					_ = m.List()
				case 3:
					// Exercise the title-change's mutex-guarded clientTitle write path
					// concurrently with the always-on sweep's effectiveTitle read.
					if sid, err := m.Create(); err == nil {
						m.SetSessionTitle(sid, "concurrent label")
						m.Close(sid)
					}
				default:
					m.clientConnected()
					m.clientDisconnected()
				}
			}
			done <- struct{}{}
		}(g)
	}
	for range goroutines {
		<-done
	}
}

// TestSessionManagerSetSessionTitle verifies the client-fallback title: on a
// known id SetSessionTitle stores the fallback so List() reports it while the
// OSC title is empty (a fresh session's handler.Title() is empty), an OSC title
// then wins over the fallback (OSC-first precedence), and an unknown id returns
// false.
func TestSessionManagerSetSessionTitle(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	titleOf := func() string {
		t.Helper()
		for _, info := range m.List() {
			if info.ID == id {
				return info.Title
			}
		}
		t.Fatalf("session %s not in List()", id)
		return ""
	}
	clientTitleOf := func() string {
		t.Helper()
		for _, info := range m.List() {
			if info.ID == id {
				return info.ClientTitle
			}
		}
		t.Fatalf("session %s not in List()", id)
		return ""
	}

	// A fresh session has no client title.
	if got := clientTitleOf(); got != "" {
		t.Fatalf("fresh session ClientTitle = %q, want empty", got)
	}

	// Known id: the fallback is stored and reported while the OSC title is empty.
	if !m.SetSessionTitle(id, "client label") {
		t.Fatal("SetSessionTitle(known id) = false, want true")
	}
	if got := titleOf(); got != "client label" {
		t.Fatalf("List Title with empty OSC title = %q, want %q (client fallback)", got, "client label")
	}
	if got := clientTitleOf(); got != "client label" {
		t.Fatalf("List ClientTitle after SetSessionTitle = %q, want %q", got, "client label")
	}

	// OSC-first: an OSC window title (set via OSC 2, which needs no PTY reply)
	// wins over the stored client fallback.
	handlerOf(t, m, id).handlePTYData([]byte("\x1b]2;osc label\x07"))
	if got := titleOf(); got != "osc label" {
		t.Fatalf("List Title with OSC title set = %q, want %q (OSC wins)", got, "osc label")
	}
	// ClientTitle is the RAW stored label, unaffected by the OSC title: it still
	// reports "client label" even though the effective Title is now "osc label".
	if got := clientTitleOf(); got != "client label" {
		t.Fatalf("List ClientTitle with OSC title set = %q, want %q (raw, OSC does not mask it)", got, "client label")
	}

	// Unknown id: no session, returns false.
	if m.SetSessionTitle("nonexistent", "x") {
		t.Fatal("SetSessionTitle(unknown id) = true, want false")
	}
}

// TestSessionManagerSetTitleREST exercises PUT /api/sessions/{id}/title: 204
// sets the fallback (List then reflects it), 404 for an unknown id, 400 for a
// body that cannot be decoded.
func TestSessionManagerSetTitleREST(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })
	srv := httptest.NewServer(m.RESTHandler())
	t.Cleanup(srv.Close)

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	put := func(t *testing.T, sessionID SessionID, body string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut,
			srv.URL+"/api/sessions/"+string(sessionID)+"/title", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("204 sets the title and List reflects it", func(t *testing.T) {
		if code := put(t, id, `{"title":"hello"}`); code != http.StatusNoContent {
			t.Fatalf("PUT known id status = %d, want 204", code)
		}
		// OSC title is empty for a fresh session, so the pushed title resolves.
		found := false
		for _, info := range m.List() {
			if info.ID == id {
				found = true
				if info.Title != "hello" {
					t.Errorf("List Title after PUT = %q, want %q", info.Title, "hello")
				}
			}
		}
		if !found {
			t.Errorf("session %s not in List()", id)
		}
	})

	t.Run("404 on an unknown id", func(t *testing.T) {
		// A valid body: decode succeeds, the lookup fails.
		if code := put(t, "nonexistent", `{"title":"hello"}`); code != http.StatusNotFound {
			t.Errorf("PUT unknown id status = %d, want 404", code)
		}
	})

	t.Run("400 on a malformed body", func(t *testing.T) {
		// Against a known id: decode fails before the lookup.
		if code := put(t, id, `{bad json`); code != http.StatusBadRequest {
			t.Errorf("PUT malformed body status = %d, want 400", code)
		}
	})
}

// TestSanitizeTitle verifies the tab-label sanitizer at both of its bounds: a
// client-derived title truncates at maxClientTitleRunes, a user-pinned name at
// the tighter maxPinnedTitleRunes, and control characters (< 0x20) and DEL (0x7f)
// are stripped either way so a title cannot inject newlines/control into the UI.
func TestSanitizeTitle(t *testing.T) {
	// Truncation is per-bound: the same 600-rune input reduces to each cap.
	if got := sanitizeTitle(strings.Repeat("a", 600), maxClientTitleRunes); len([]rune(got)) != maxClientTitleRunes {
		t.Fatalf("sanitizeTitle(600 runes, client) length = %d, want %d", len([]rune(got)), maxClientTitleRunes)
	}
	if got := sanitizeTitle(strings.Repeat("a", 600), maxPinnedTitleRunes); len([]rune(got)) != maxPinnedTitleRunes {
		t.Fatalf("sanitizeTitle(600 runes, pinned) length = %d, want %d", len([]rune(got)), maxPinnedTitleRunes)
	}
	// A short plain title is unchanged.
	if got := sanitizeTitle("plain title", maxClientTitleRunes); got != "plain title" {
		t.Fatalf("sanitizeTitle(plain) = %q, want %q", got, "plain title")
	}
	// Control characters and DEL are stripped, surrounding runes preserved.
	if got := sanitizeTitle("a\nb\x1bc\x7fd\te", maxPinnedTitleRunes); got != "abcde" {
		t.Fatalf("sanitizeTitle(control chars) = %q, want %q", got, "abcde")
	}
	// The cap counts RETAINED runes, so a control-heavy input is not shortened by
	// the characters it drops.
	if got := sanitizeTitle(strings.Repeat("a\n", 200), maxPinnedTitleRunes); got != strings.Repeat("a", maxPinnedTitleRunes) {
		t.Fatalf("sanitizeTitle(control-heavy) = %q, want %d a's", got, maxPinnedTitleRunes)
	}
}

// TestWebSocketUnknownSessionClosesDefinitively pins the unknown-session WS
// contract: a real WebSocket dial to an id the manager does not know is
// ACCEPTED and then closed with the definitive 4004 application code, which
// the browser client reads and routes to its ended state. A pre-upgrade 404
// is unreadable from browser JS (an opaque code-1006 failed connect), so the
// old behavior condemned a client with a stale id (reaped session, restarted
// server) to an endless reconnect loop.
func TestWebSocketUnknownSessionClosesDefinitively(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })
	mux := http.NewServeMux()
	m.MountAPI(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	//nolint:bodyclose // coder/websocket Dial nils resp.Body on success
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+WSPath+"?session=nope", nil)
	if err != nil {
		t.Fatalf("ws dial to an unknown session must be accepted (then closed 4004), got dial error: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "") // #nosec G104 -- best-effort test cleanup

	_, _, rerr := ws.Read(ctx)
	if rerr == nil {
		t.Fatal("expected the read to fail with the 4004 close, got a frame")
	}
	if got := websocket.CloseStatus(rerr); got != statusUnknownSession {
		t.Fatalf("close status = %d, want %d (statusUnknownSession); read err: %v", got, statusUnknownSession, rerr)
	}
}

// TestSessionSurfaceRefusesCaching pins the cache policy on every session-surface
// response that carries a session id. The id is the /ws attach + resume
// credential, so a cache that stores one of these bodies holds a live credential
// on disk; this package already refuses to log an id whole (LogID), and the
// header is the same refusal aimed at caches.
//
// It is a CONTRACT test, not a coverage test: consumers were closing this gap
// unevenly by hand (one wrapped the surface in its own middleware, one set no
// directive at all) before the engine took the default, so a consumer that drops
// its own middleware after upgrading is relying on these assertions holding.
func TestSessionSurfaceRefusesCaching(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })

	t.Run("REST", func(t *testing.T) {
		srv := httptest.NewServer(m.RESTHandler())
		t.Cleanup(srv.Close)

		// POST returns the new id, GET returns every live id: both are bodies a
		// cache must not keep. The two 204/404 responses carry no id and are
		// deliberately not asserted here.
		resp, err := http.Post(srv.URL+"/api/sessions", "application/json", nil)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		var created SessionInfo
		_ = json.NewDecoder(resp.Body).Decode(&created)
		resp.Body.Close()
		if created.ID == "" {
			t.Fatal("POST returned no id, so this test would assert nothing")
		}
		if cc := resp.Header.Get("Cache-Control"); !hasDirective(cc, "no-store") {
			t.Errorf("POST /api/sessions Cache-Control = %q, want no-store: the response body carries the session id %s", cc, LogID(string(created.ID)))
		}
		t.Cleanup(func() { m.Close(created.ID) })

		respL, err := http.Get(srv.URL + "/api/sessions")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		respL.Body.Close()
		if cc := respL.Header.Get("Cache-Control"); !hasDirective(cc, "no-store") {
			t.Errorf("GET /api/sessions Cache-Control = %q, want no-store: the response body lists every live session id", cc)
		}
	})

	t.Run("SSE", func(t *testing.T) {
		srv := httptest.NewServer(m.EventsHandler())
		t.Cleanup(srv.Close)

		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET events: %v", err)
		}
		defer resp.Body.Close()
		// Each event carries statusEvent.ID in full, so the stream needs the
		// same prohibition as the REST bodies. no-cache alone only forces
		// revalidation -- it still lets a cache store the response -- so it is
		// asserted as PRESENT (middleboxes sniff for it) but not as sufficient.
		cc := resp.Header.Get("Cache-Control")
		if !hasDirective(cc, "no-store") {
			t.Errorf("events Cache-Control = %q, want no-store: every event carries a full session id", cc)
		}
		if !hasDirective(cc, "no-cache") {
			t.Errorf("events Cache-Control = %q, want the conventional SSE no-cache retained alongside no-store", cc)
		}
	})
}

// snapHeader returns the response header as a CLIENT would see it: the snapshot
// httptest takes when WriteHeader runs, not the live map.
//
// Every cache assertion below reads through this rather than rec.Header(),
// because the live map keeps accepting writes after the status line is sent
// while the wire response does not. A wrapper that set the header AFTER calling
// the next handler would still show up in rec.Header() and pass, which is
// exactly how a header wrapper looks correct and does nothing.
func snapHeader(rec *httptest.ResponseRecorder) http.Header {
	return rec.Result().Header
}

// TestSessionRESTNoStoreCoversEveryResponse pins the cache policy on the
// session REST responses that carry NO session id in their body — which is every
// response except the two writeJSON bodies TestSessionSurfaceRefusesCaching
// already covers.
//
// What these responses expose is the cache KEY, not the body: each of these URLs
// carries a session id as a path segment, and that id is the /ws attach + resume
// capability. A 404 or 405 with no cache directive at all is heuristically
// cacheable under RFC 9110 §15.1, so a cache could retain an entry keyed by a
// live token while the body itself is only net/http's constant error text.
//
// The synthesized rows are the point of the test. The inner mux writes its own
// 404, 405 and path-cleaning redirect with no handler involved, so nothing in
// the handler table could have covered them.
func TestSessionRESTNoStoreCoversEveryResponse(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })
	h := m.RESTHandler()

	// liveID creates a session for ONE case to act on, so a case that closes or
	// renames its target cannot disturb another's. Returned as a plain string
	// because every caller splices it into a URL path.
	liveID := func(t *testing.T) string {
		t.Helper()
		id, err := m.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		t.Cleanup(func() { m.Close(id) })
		return string(id)
	}

	// The two path shapes the table needs: a fixed collection path, and a
	// subtree path under a session created for that one case. Keeping them as
	// helpers lets every row below stay on one line.
	fixed := func(p string) func(*testing.T) string {
		return func(*testing.T) string { return p }
	}
	sub := func(suffix string) func(*testing.T) string {
		return func(t *testing.T) string { return SessionsSubtreePath + liveID(t) + suffix }
	}
	const titleBody = `{"title":"x"}`

	cases := []struct {
		// path builds the request target, taking a fresh session id when the
		// route needs one.
		path       func(*testing.T) string
		name       string
		method     string
		body       string
		wantStatus int
	}{
		// The six REST method routes the mount contract declares.
		{name: "create", method: http.MethodPost, wantStatus: http.StatusCreated, path: fixed(SessionsPath)},
		{name: "list", method: http.MethodGet, wantStatus: http.StatusOK, path: fixed(SessionsPath)},
		{name: "close", method: http.MethodDelete, wantStatus: http.StatusNoContent, path: sub("")},
		{name: "set title", method: http.MethodPut, body: titleBody, wantStatus: http.StatusNoContent, path: sub("/title")},
		{name: "set pinned title", method: http.MethodPut, body: titleBody, wantStatus: http.StatusNoContent, path: sub("/pinned-title")},
		{name: "clear pinned title", method: http.MethodDelete, wantStatus: http.StatusNoContent, path: sub("/pinned-title")},

		// Handler-written refusals on a token-bearing path.
		{name: "close, unknown id (404)", method: http.MethodDelete, wantStatus: http.StatusNotFound, path: fixed(SessionsSubtreePath + "deadbeef")},
		{name: "set title, undecodable body (400)", method: http.MethodPut, body: "not json", wantStatus: http.StatusBadRequest, path: sub("/title")},

		// The inner mux's SYNTHESIZED responses: no handler runs for these, so
		// only a wrapper upstream of the mux can carry the header.
		{name: "mux 404, subtree root", method: http.MethodGet, wantStatus: http.StatusNotFound, path: fixed(SessionsSubtreePath)},
		{name: "mux 404, unserved depth", method: http.MethodDelete, wantStatus: http.StatusNotFound, path: sub("/title/extra")},
		{name: "mux 405, unserved method on a session path", method: http.MethodGet, wantStatus: http.StatusMethodNotAllowed, path: sub("")},
		{name: "mux 405, unserved method on the collection", method: http.MethodPatch, wantStatus: http.StatusMethodNotAllowed, path: fixed(SessionsPath)},
		// A doubled slash is not a clean path, so the mux answers with a
		// redirect whose Location echoes the cleaned, still token-bearing URL.
		{name: "mux path-cleaning redirect", method: http.MethodDelete, wantStatus: http.StatusTemporaryRedirect, path: func(t *testing.T) string { return SessionsSubtreePath + "/" + liveID(t) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path(t)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, path, strings.NewReader(tc.body)))

			// The status is asserted so a route that stops answering the way
			// this test assumes fails loudly instead of silently passing the
			// header check on some other response.
			if rec.Code != tc.wantStatus {
				t.Errorf("%s %s = %d, want %d", tc.method, path, rec.Code, tc.wantStatus)
			}
			if cc := snapHeader(rec).Get("Cache-Control"); !hasDirective(cc, "no-store") {
				t.Errorf("%s %s Cache-Control = %q, want no-store: the URL carries a live session id", tc.method, path, cc)
			}
		})
	}
}

// TestSessionRESTNoStorePrecedence pins WHO wins when a Cache-Control is already
// on the response, which is the reason the policy needs no opt-out option.
//
// The two halves differ deliberately. The wrapper YIELDS: the responses it
// covers carry no credential, only a sensitive URL, so an outer stack that
// states a policy stays that header's single writer — that is the documented
// escape hatch for a consumer wanting something stricter, or genuinely wanting a
// route cached. writeJSON OVERWRITES: its bodies contain the session id itself,
// so that prohibition is not the consumer's to relax.
func TestSessionRESTNoStorePrecedence(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })

	const outer = "max-age=60"
	// preset is the outer consumer: a wrapper that states its own policy before
	// the engine's handler ever runs, exactly as a middleware chain would.
	preset := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", outer)
			next.ServeHTTP(w, r)
		})
	}
	h := preset(m.RESTHandler())

	t.Run("wrapper preserves an outer value", func(t *testing.T) {
		// A 405 is a wrapper-only response: no handler runs, so the value on it
		// is whichever of the two the precedence rule picked.
		id, err := m.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		t.Cleanup(func() { m.Close(id) })

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, SessionsSubtreePath+string(id), nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s = %d, want 405 (the wrapper-only response this case needs)", SessionsSubtreePath+string(id), rec.Code)
		}
		if got := snapHeader(rec).Get("Cache-Control"); got != outer {
			t.Errorf("Cache-Control = %q, want the outer %q preserved: an already-set value is the deliberate override", got, outer)
		}
	})

	t.Run("writeJSON overrides an outer value", func(t *testing.T) {
		// GET /api/sessions lists every live id, so the engine's prohibition
		// wins here even though the same outer middleware asked for caching.
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, SessionsPath, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", SessionsPath, rec.Code)
		}
		if cc := snapHeader(rec).Get("Cache-Control"); !hasDirective(cc, "no-store") {
			t.Errorf("Cache-Control = %q, want no-store: this body lists live session ids, so an outer max-age must not win", cc)
		}
	})
}

// firstWriteHeader records the response header as it stood when the handler
// first committed the response, whichever call did it: an explicit WriteHeader,
// or a Write that implies a 200.
type firstWriteHeader struct {
	http.ResponseWriter
	// snap is the header at commit time, nil until the response is committed.
	snap http.Header
}

func (f *firstWriteHeader) WriteHeader(status int) {
	if f.snap == nil {
		f.snap = f.Header().Clone()
	}
	f.ResponseWriter.WriteHeader(status)
}

func (f *firstWriteHeader) Write(b []byte) (int, error) {
	if f.snap == nil {
		f.snap = f.Header().Clone()
	}
	return f.ResponseWriter.Write(b)
}

// TestSessionRESTNoStoreSetBeforeBody pins the ORDERING the policy depends on:
// the header is in the map before the handler commits the response.
//
// This is the failure mode that makes a header wrapper look correct and do
// nothing. net/http sends the header map as it stood when the response was
// committed and silently ignores later mutations, so a wrapper that set the
// value after calling the next handler would change the in-memory map — and be
// visible to a test reading it — while sending no header on the wire at all.
func TestSessionRESTNoStoreSetBeforeBody(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, m) })
	h := m.RESTHandler()

	// One response of each shape: a body written by a handler, a body written by
	// the mux with no handler involved, and a bodyless 204.
	cases := []struct {
		name, method, path string
	}{
		{"handler-written body", http.MethodGet, SessionsPath},
		{"mux-synthesized body", http.MethodGet, SessionsSubtreePath},
		{"bodyless 204", http.MethodDelete, ""}, // path filled in below
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				id, err := m.Create()
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				t.Cleanup(func() { m.Close(id) })
				path = SessionsSubtreePath + string(id)
			}
			fw := &firstWriteHeader{ResponseWriter: httptest.NewRecorder()}
			h.ServeHTTP(fw, httptest.NewRequest(tc.method, path, nil))

			if fw.snap == nil {
				t.Fatalf("%s %s committed no response, so this test would assert nothing", tc.method, path)
			}
			if cc := fw.snap.Get("Cache-Control"); !hasDirective(cc, "no-store") {
				t.Errorf("Cache-Control at commit time = %q, want no-store: a header set after the response is committed is dropped on the wire", cc)
			}
		})
	}
}
