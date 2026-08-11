package terminal

import "net/http"

// The session-manager HTTP surface's route topology. These paths are a
// CONTRACT shared with the engine's TypeScript client (the UI's tabs feature
// defaults to SessionsPath and its activity monitor to SessionEventsPath) and
// hardcoded inside RESTHandler's method patterns, so a consumer cannot mount
// the handlers anywhere else and keep a working client. MountSessionRoutes
// wires exactly this set; adding a path to it is a release-noted,
// changelog-visible API change, never a silent addition.
const (
	// WSPath is the terminal WebSocket route. The client connects per session
	// with ?session=<id> (see WebSocketHandler).
	WSPath = "/ws"
	// SessionsPath is the exact-match session REST route: POST (create) and
	// GET (list). It is also the path a create gate throttles (see
	// WithCreateGate).
	SessionsPath = "/api/sessions"
	// SessionsSubtreePath is the session REST subtree: DELETE /{id} (close),
	// PUT /{id}/title (set the client-fallback title), PUT + DELETE
	// /{id}/pinned-title (set / clear the user's name), and PUT /order (set the
	// shared display order every viewer sees). ServeMux treats the trailing-slash
	// pattern as a distinct mount, so the REST handler is mounted at both
	// SessionsPath and this subtree to receive every method.
	//
	// /order is a literal segment where its siblings take an {id}. ServeMux
	// prefers the more specific literal, and no session id can collide with it
	// (ids are hex), so the two patterns cannot overlap.
	SessionsSubtreePath = "/api/sessions/"
	// SessionEventsPath is the session status stream (SSE) route. It is a more
	// specific pattern than SessionsSubtreePath, so ServeMux routes it to the
	// events handler rather than the REST DELETE /{id} pattern.
	SessionEventsPath = "/api/sessions/events"
)

// The cache policy the session REST surface states, and the header it states it
// on. One spelling for the whole package so the wrapper below and writeJSON
// cannot drift apart into two different answers.
const (
	cacheControlHeader = "Cache-Control"
	noStorePolicy      = "no-store"
)

// withNoStore states the session REST surface's cache policy on the response
// header map BEFORE h runs. It is applied at two sites — inside RESTHandler
// (upstream of the inner mux) and in MountSessionRoutes (upstream of a create
// gate) — because neither alone covers everything; see MountSessionRoutes.
//
// What is at risk here is the cache KEY, not the body. Every session REST path
// past the collection carries a session id as a path segment, and that id IS the
// capability the /ws attach and resume present (the reason LogID exists and the
// reason writeJSON refuses to let the two JSON bodies be stored). The responses
// this wrapper newly covers carry no credential in their bodies at all — the
// inner mux's 404 and 405 are net/http's constant error text, a path-cleaning
// redirect is a Location header, a gate refusal is the consumer's throttle body.
// The exposure is that a cache retains an ENTRY keyed by a token-bearing URL,
// which RFC 9110 §15.1 invites by listing 404 and 405 among the statuses a cache
// may reuse heuristically when the response carries no explicit freshness
// information — and with no directive at all, those responses carried none.
//
// Setting the header here rather than in each handler settles the ordering
// requirement STRUCTURALLY: the value is in the map before any handler can call
// WriteHeader, after which a late Set is silently dropped and a wrapper that
// looks correct does nothing. This site cannot be missed either — it is upstream
// of every route in the mux, of every http.Error those routes reach, and of the
// responses the mux synthesizes for a path or method it does not serve, which no
// handler is invoked to write.
//
// A value already present is left exactly as it is. That is the deliberate
// escape hatch, and it is why the policy needs no opt-out MountOption: a
// consumer that wants a different answer — a stricter "no-store, no-cache,
// must-revalidate", or genuinely wanting one of these routes cached — says so by
// setting the header in its own outer middleware, which then stays that header's
// single writer. The package cannot rank a stricter value against a weaker one
// without parsing directives, so it treats any present value as intentional.
// writeJSON is the one deliberate exception and overwrites: its two bodies
// CONTAIN the id, so their prohibition is not the consumer's to relax.
func withNoStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if w.Header().Get(cacheControlHeader) == "" {
			w.Header().Set(cacheControlHeader, noStorePolicy)
		}
		h.ServeHTTP(w, r)
	})
}

// MountOption configures MountSessionRoutes.
type MountOption func(*mountConfig)

// mountConfig holds resolved MountSessionRoutes configuration.
type mountConfig struct {
	createGate func(http.Handler) http.Handler
}

// WithCreateGate wraps the session REST handler with mw before mounting, so a
// caller cannot spawn PTY-backed child processes without bound: Create eagerly
// forks one process per admitted POST, which is exactly the
// expensive-shared-resource shape an aggregate rate limit exists for. The gate
// is INJECTED rather than built in because throttle policy (tuning, the 429
// body, whether to gate at all) is the consumer's decision and the engine
// takes no HTTP-middleware dependency; pass a middleware that scopes itself to
// the create request — e.g. webhttp.SessionCreateRateLimit(SessionsPath),
// whose predicate gates POST SessionsPath and passes GET/DELETE/PUT through.
// The gate wraps the REST handler on BOTH its mounts (SessionsPath and
// SessionsSubtreePath) and never wraps the WebSocket or events handlers. A nil
// mw is ignored (no gate).
func WithCreateGate(mw func(http.Handler) http.Handler) MountOption {
	return func(c *mountConfig) {
		if mw != nil {
			c.createGate = mw
		}
	}
}

// MountSessionRoutes wires the session-manager HTTP surface on mux — the
// engine-owned mount contract, in code:
//
//	WSPath              -> ws     (terminal WebSocket, ?session=<id>)
//	SessionsPath        -> rest   (POST create, GET list; create-gated)
//	SessionsSubtreePath -> rest   (DELETE /{id}, PUT /{id}/title,
//	                               PUT + DELETE /{id}/pinned-title,
//	                               PUT /order; create-gated)
//	SessionEventsPath   -> events (status SSE)
//
// Exactly these four mounts and no others: the engine's debug or future
// routes never appear implicitly, and any addition to the set is a
// release-noted API change (see the path constants). The handlers are passed
// in — normally WebSocketHandler, RESTHandler, and EventsHandler of one
// SessionManager, which MountAPI wires for you — so a consumer's tests can
// exercise routing and middleware with stubs, without a real PTY.
//
// The mount shape encodes two ServeMux subtleties consumers previously
// re-derived by hand: the REST handler needs BOTH the exact path and the
// subtree mount (its internal patterns span /api/sessions and
// /api/sessions/{id}...), and the SSE path, being more specific than the
// subtree, routes to events rather than the REST DELETE /{id} pattern.
//
// The REST mounts are wrapped in the surface's cache policy (withNoStore),
// outside any create gate, so every response on a token-bearing path states
// Cache-Control — including the ones no handler writes. ws and events are not
// wrapped: one is a handshake, the other sets its own stricter value.
func MountSessionRoutes(mux *http.ServeMux, ws, rest, events http.Handler, opts ...MountOption) {
	var c mountConfig
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	if c.createGate != nil {
		rest = c.createGate(rest)
	}
	// Outside the gate, deliberately. A gate refusal (webhttp's 429) is written
	// BY the gate and never reaches the REST handler, so RESTHandler's own
	// withNoStore cannot cover it — and a refusal lands on the same token-bearing
	// paths as everything else, with the same cache-key exposure. Wrapping the
	// gated handler covers both it and everything it delegates to. Applied
	// unconditionally, so a consumer inherits the policy by bumping the engine
	// rather than by remembering an option; RESTHandler has already set the
	// header by the time the inner copy runs, which that copy then leaves alone.
	// Only rest is wrapped: ws is a WebSocket handshake, and events sets its own
	// stricter "no-cache, no-store" (see writeSSEHeaders).
	rest = withNoStore(rest)
	mux.Handle(WSPath, ws)
	mux.Handle(SessionsPath, rest)
	mux.Handle(SessionsSubtreePath, rest)
	mux.Handle(SessionEventsPath, events)
}

// MountAPI mounts this manager's WebSocket, REST, and status-stream handlers
// on mux per the MountSessionRoutes contract. It is the convenience for the
// common case of one manager serving one mux; use MountSessionRoutes directly
// to inject stub handlers in tests.
func (m *SessionManager) MountAPI(mux *http.ServeMux, opts ...MountOption) {
	MountSessionRoutes(mux, m.WebSocketHandler(), m.RESTHandler(), m.EventsHandler(), opts...)
}
