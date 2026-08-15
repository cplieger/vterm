package terminal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mountStubs builds a mux wired through MountSessionRoutes with three named
// stubs, returning the mux and a hit recorder keyed by stub name.
func mountStubs(t *testing.T, opts ...MountOption) (*http.ServeMux, map[string]int) {
	t.Helper()
	hits := make(map[string]int)
	stub := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits[name]++
			w.WriteHeader(http.StatusOK)
		})
	}
	mux := http.NewServeMux()
	MountSessionRoutes(mux, stub("ws"), stub("rest"), stub("events"), opts...)
	return mux, hits
}

// TestMountSessionRoutesTopology pins the mount contract: each of the four
// documented paths routes to its designated handler, including the two
// ServeMux subtleties the helper owns (the REST exact+subtree double mount,
// and the SSE path winning over the REST subtree by specificity).
func TestMountSessionRoutesTopology(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodGet, WSPath, "ws"},
		{http.MethodGet, WSPath + "?session=abc", "ws"},
		{http.MethodPost, SessionsPath, "rest"},
		{http.MethodGet, SessionsPath, "rest"},
		{http.MethodDelete, SessionsSubtreePath + "some-id", "rest"},
		{http.MethodPut, SessionsSubtreePath + "some-id/title", "rest"},
		{http.MethodGet, SessionEventsPath, "events"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			mux, hits := mountStubs(t)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s = %d, want 200", tc.method, tc.path, rec.Code)
			}
			if hits[tc.want] != 1 {
				t.Errorf("%s %s routed to %v, want exactly one hit on %q", tc.method, tc.path, hits, tc.want)
			}
		})
	}
}

// TestMountSessionRoutesCreateGate pins the WithCreateGate contract: the gate
// wraps the REST handler on both its mounts and never wraps the WebSocket or
// events handlers, and a nil gate is ignored.
func TestMountSessionRoutesCreateGate(t *testing.T) {
	gated := 0
	gate := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gated++
			next.ServeHTTP(w, r)
		})
	}

	mux, hits := mountStubs(t, WithCreateGate(gate))
	serve := func(method, path string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	}

	serve(http.MethodPost, SessionsPath)
	if gated != 1 || hits["rest"] != 1 {
		t.Errorf("POST %s: gate hits = %d, rest hits = %d, want 1 and 1", SessionsPath, gated, hits["rest"])
	}
	serve(http.MethodDelete, SessionsSubtreePath+"id")
	if gated != 2 || hits["rest"] != 2 {
		t.Errorf("DELETE subtree: gate hits = %d, rest hits = %d, want 2 and 2 (gate wraps both REST mounts)", gated, hits["rest"])
	}
	serve(http.MethodGet, WSPath)
	serve(http.MethodGet, SessionEventsPath)
	if gated != 2 {
		t.Errorf("gate hits after /ws + events = %d, want 2 (gate must not wrap ws or events)", gated)
	}

	// A nil gate is skipped: routing still works.
	mux2, hits2 := mountStubs(t, WithCreateGate(nil))
	rec := httptest.NewRecorder()
	mux2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, SessionsPath, nil))
	if rec.Code != http.StatusOK || hits2["rest"] != 1 {
		t.Errorf("nil gate: GET %s = %d with rest hits %d, want 200 and 1", SessionsPath, rec.Code, hits2["rest"])
	}
}

// TestMountAPIWiresManagerHandlers is the convenience-path smoke test: a real
// manager mounted via MountAPI answers the session list on SessionsPath.
func TestMountAPIWiresManagerHandlers(t *testing.T) {
	factory := func(id string) *Handler {
		return NewHandler([]string{"/bin/cat"})
	}
	mgr := NewSessionManager(factory)
	t.Cleanup(func() { shutdownManager(t, mgr) })

	mux := http.NewServeMux()
	mgr.MountAPI(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, SessionsPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", SessionsPath, rec.Code)
	}
	var infos []SessionInfo
	if err := json.NewDecoder(rec.Body).Decode(&infos); err != nil {
		t.Fatalf("list response is not a JSON session array: %v", err)
	}
	if len(infos) != 0 {
		t.Errorf("fresh manager listed %d sessions, want 0", len(infos))
	}

	// A plain (non-upgrade) GET with an unknown session id gets Accept's 426
	// through the same mount — the SAME answer a known id gives, so a probe
	// cannot distinguish session existence (the old 404-vs-426 oracle). A
	// real WebSocket dial to an unknown id gets the accepted-then-closed 4004
	// treatment (TestWebSocketUnknownSessionClosesDefinitively).
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, WSPath+"?session=nope", nil))
	if rec2.Code != http.StatusUpgradeRequired {
		t.Errorf("GET %s?session=nope = %d, want 426 (upgrade required, matching the known-session answer)", WSPath, rec2.Code)
	}
}

// TestMountSessionRoutesNoStore pins the cache policy at the MOUNT, which is the
// half RESTHandler's own wrapper cannot reach.
//
// A create gate wraps the REST handler (see WithCreateGate), so a refusal is
// written BY the gate and the REST handler never runs. The gate refuses on the
// same token-bearing paths as every other response, with the same cache-key
// exposure, so the mount wraps OUTSIDE the gate. The scope half matters as much:
// ws is a WebSocket handshake and events states its own stricter policy, so
// neither may be touched.
func TestMountSessionRoutesNoStore(t *testing.T) {
	// refuseGate is the shape of a real throttle: it answers 429 itself and
	// never calls through, so nothing downstream can set the header for it.
	refused := 0
	refuseGate := func(_ http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			refused++
			http.Error(w, "too many requests", http.StatusTooManyRequests)
		})
	}

	t.Run("gate refusal carries the header", func(t *testing.T) {
		mux, hits := mountStubs(t, WithCreateGate(refuseGate))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, SessionsPath, nil))

		if rec.Code != http.StatusTooManyRequests || refused != 1 {
			t.Fatalf("POST %s = %d with %d refusals, want 429 and 1", SessionsPath, rec.Code, refused)
		}
		if hits["rest"] != 0 {
			t.Fatalf("the gate called through (%d rest hits), so this case would not test the gate's own response", hits["rest"])
		}
		if cc := rec.Result().Header.Get("Cache-Control"); !hasDirective(cc, "no-store") {
			t.Errorf("gate refusal Cache-Control = %q, want no-store: a refusal lands on a token-bearing path too", cc)
		}
	})

	t.Run("gate refusal on the subtree carries the header", func(t *testing.T) {
		mux, _ := mountStubs(t, WithCreateGate(refuseGate))
		path := SessionsSubtreePath + "some-id"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, path, nil))
		if cc := rec.Result().Header.Get("Cache-Control"); !hasDirective(cc, "no-store") {
			t.Errorf("DELETE %s Cache-Control = %q, want no-store (the gate wraps both REST mounts)", path, cc)
		}
	})

	// The stubs write a bare 200 and set no header of their own, so whatever the
	// REST rows carry came from the mount rather than from a handler.
	t.Run("both REST mounts carry the header without a gate", func(t *testing.T) {
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, SessionsPath},
			{http.MethodGet, SessionsPath},
			{http.MethodDelete, SessionsSubtreePath + "some-id"},
			{http.MethodPut, SessionsSubtreePath + "some-id/title"},
		} {
			mux, _ := mountStubs(t)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if cc := rec.Result().Header.Get("Cache-Control"); !hasDirective(cc, "no-store") {
				t.Errorf("%s %s Cache-Control = %q, want no-store", tc.method, tc.path, cc)
			}
		}
	})

	// Scope: the mount must not reach past the REST handler. Asserted against
	// stubs, so a value here could only have come from the mount.
	t.Run("ws and events are not wrapped", func(t *testing.T) {
		for _, tc := range []struct{ name, path string }{
			{"ws", WSPath},
			{"events", SessionEventsPath},
		} {
			mux, _ := mountStubs(t)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if cc := rec.Result().Header.Get("Cache-Control"); cc != "" {
				t.Errorf("%s Cache-Control = %q, want empty: the mount wraps only the REST handler", tc.name, cc)
			}
		}
	})
}

// TestMountAPINoStoreOnRealHandlers is the convenience-path counterpart: through
// MountAPI, with the manager's real handlers, the REST surface states the policy
// and the SSE stream keeps its own stricter value rather than inheriting it.
func TestMountAPINoStoreOnRealHandlers(t *testing.T) {
	mgr := NewSessionManager(catFactory)
	t.Cleanup(func() { shutdownManager(t, mgr) })
	mux := http.NewServeMux()
	mgr.MountAPI(mux)

	t.Run("REST 405 on a session path", func(t *testing.T) {
		path := SessionsSubtreePath + "some-id"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s = %d, want 405", path, rec.Code)
		}
		if cc := rec.Result().Header.Get("Cache-Control"); !hasDirective(cc, "no-store") {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
	})

	// The SSE stream sets "no-cache, no-store" itself (writeSSEHeaders). Pinned
	// here because the mount deliberately does not wrap events: if it ever did,
	// the wrapper's no-store would land first and the conventional no-cache
	// middleboxes sniff for would be lost.
	t.Run("SSE keeps its own policy", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+SessionEventsPath, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET events: %v", err)
		}
		defer resp.Body.Close()
		cc := resp.Header.Get("Cache-Control")
		if !hasDirective(cc, "no-store") {
			t.Errorf("events Cache-Control = %q, want no-store", cc)
		}
		if !hasDirective(cc, "no-cache") {
			t.Errorf("events Cache-Control = %q, want the conventional SSE no-cache retained: the mount must not overwrite it with a bare no-store", cc)
		}
	})
}
