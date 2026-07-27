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

func TestSessionManagerCreateListClose(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)

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
	t.Cleanup(m.Shutdown)

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
	t.Cleanup(m.Shutdown)
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
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, srv.URL+"/api/sessions/"+created.ID, nil)
	respD, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	respD.Body.Close()
	if respD.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", respD.StatusCode)
	}
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, srv.URL+"/api/sessions/"+created.ID, nil)
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
	t.Cleanup(m.Shutdown)
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
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	srv := httptest.NewServer(m.WebSocketHandler())
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?session=" + id
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	t.Cleanup(m.Shutdown)

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
	t.Cleanup(m.Shutdown)
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
	t.Cleanup(m.Shutdown)
	srv := httptest.NewServer(m.RESTHandler())
	t.Cleanup(srv.Close)

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	put := func(t *testing.T, sessionID, body string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut,
			srv.URL+"/api/sessions/"+sessionID+"/title", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// 204: valid body against a known id.
	if code := put(t, id, `{"title":"hello"}`); code != http.StatusNoContent {
		t.Fatalf("PUT known id status = %d, want 204", code)
	}
	// List reflects the pushed title (OSC title is empty for a fresh session).
	found := false
	for _, info := range m.List() {
		if info.ID == id {
			found = true
			if info.Title != "hello" {
				t.Fatalf("List Title after PUT = %q, want %q", info.Title, "hello")
			}
		}
	}
	if !found {
		t.Fatalf("session %s not in List()", id)
	}

	// 404: valid body against an unknown id (decode succeeds, lookup fails).
	if code := put(t, "nonexistent", `{"title":"hello"}`); code != http.StatusNotFound {
		t.Fatalf("PUT unknown id status = %d, want 404", code)
	}

	// 400: malformed body against a known id (decode fails before lookup).
	if code := put(t, id, `{bad json`); code != http.StatusBadRequest {
		t.Fatalf("PUT malformed body status = %d, want 400", code)
	}
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
	t.Cleanup(m.Shutdown)
	mux := http.NewServeMux()
	m.MountAPI(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	t.Cleanup(m.Shutdown)

	// hasDirective reports whether a Cache-Control value carries the named
	// directive, tolerating either ordering and surrounding whitespace so the
	// test pins the POLICY rather than one exact spelling of the header.
	hasDirective := func(header, want string) bool {
		for part := range strings.SplitSeq(header, ",") {
			if strings.EqualFold(strings.TrimSpace(part), want) {
				return true
			}
		}
		return false
	}

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
			t.Errorf("POST /api/sessions Cache-Control = %q, want no-store: the response body carries the session id %s", cc, LogID(created.ID))
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

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
