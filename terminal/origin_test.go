package terminal

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// OriginPolicy is the terminal socket's ONLY cross-origin gate: a WebSocket
// handshake is a GET, and net/http.CrossOriginProtection returns early for safe
// methods, so no app-level CSRF middleware ever inspects the upgrade. These
// tests therefore cover both the operator-facing validation and the live
// decision, on BOTH accept sites.

func TestNewOriginPolicy_validation(t *testing.T) {
	tests := map[string]struct {
		entry     string
		wantKey   string // "" = the entry must be reported invalid
		wantWhyNo string
		wantSkip  bool // blank entries are skipped, not reported
	}{
		"plain https":           {entry: "https://embed.example.com", wantKey: "https://embed.example.com"},
		"http with port":        {entry: "http://localhost:7681", wantKey: "http://localhost:7681"},
		"ipv6 literal":          {entry: "https://[::1]:7681", wantKey: "https://[::1]:7681"},
		"uppercase host folded": {entry: "https://EMBED.Example.COM", wantKey: "https://embed.example.com"},
		"uppercase scheme folded": {
			entry: "HTTPS://embed.example.com", wantKey: "https://embed.example.com",
		},
		"default https port dropped": {
			entry: "https://embed.example.com:443", wantKey: "https://embed.example.com",
			wantWhyNo: "a browser omits the default port, so keeping it would match nothing",
		},
		"default http port dropped": {
			entry: "http://embed.example.com:80", wantKey: "http://embed.example.com",
		},
		"non-default port kept": {
			entry: "https://embed.example.com:8443", wantKey: "https://embed.example.com:8443",
		},

		"blank skipped":   {entry: "   ", wantSkip: true},
		"no scheme":       {entry: "embed.example.com", wantWhyNo: "a bare host is not an origin"},
		"scheme-relative": {entry: "//embed.example.com", wantWhyNo: "no scheme"},
		"null":            {entry: "null", wantWhyNo: "sandboxed iframes and file:// pages all send null"},
		"ws scheme":       {entry: "ws://embed.example.com", wantWhyNo: "an Origin is never ws://"},
		"file scheme":     {entry: "file://", wantWhyNo: "no host, and not http(s)"},
		"custom scheme":   {entry: "chrome-extension://abcdef", wantWhyNo: "not http(s)"},
		"trailing path":   {entry: "https://embed.example.com/", wantWhyNo: "an origin has no path"},
		"deeper path":     {entry: "https://embed.example.com/app", wantWhyNo: "an origin has no path"},
		"query":           {entry: "https://embed.example.com?x=1", wantWhyNo: "an origin has no query"},
		"fragment":        {entry: "https://embed.example.com#f", wantWhyNo: "an origin has no fragment"},
		"userinfo":        {entry: "https://user@embed.example.com", wantWhyNo: "an origin has no userinfo"},
		"wildcard host":   {entry: "https://*.example.com", wantWhyNo: "no wildcards, by design"},
		"bare wildcard":   {entry: "*", wantWhyNo: "would mean allow-everything"},
		"wildcard with schema": {
			entry: "https://*", wantWhyNo: "would mean allow-every-https-origin",
		},
		"host only with port": {entry: "embed.example.com:8443", wantWhyNo: "parses as scheme 'embed.example.com'"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			p, invalid := NewOriginPolicy(tc.entry)

			switch {
			case tc.wantSkip:
				if len(invalid) != 0 {
					t.Errorf("blank entry reported invalid %v; it should be skipped silently", invalid)
				}
				if p.Active() {
					t.Error("a blank entry produced an active policy")
				}
			case tc.wantKey == "":
				if len(invalid) != 1 || invalid[0] != tc.entry {
					t.Errorf("invalid = %v, want [%q] (%s)", invalid, tc.entry, tc.wantWhyNo)
				}
				if p.Active() {
					t.Errorf("an all-invalid list produced an ACTIVE policy; it must collapse to same-origin only")
				}
			default:
				if len(invalid) != 0 {
					t.Fatalf("invalid = %v, want none", invalid)
				}
				if !p.Active() {
					t.Fatal("policy is inactive")
				}
				if _, ok := p.allowed[tc.wantKey]; !ok {
					t.Errorf("key = %v, want %q (%s)", keysOf(p), tc.wantKey, tc.wantWhyNo)
				}
			}
		})
	}
}

func keysOf(p *OriginPolicy) []string {
	out := make([]string, 0, len(p.allowed))
	for k := range p.allowed {
		out = append(out, k)
	}
	return out
}

// TestNewOriginPolicy_mixedList pins the drop-and-report contract: the good
// entries survive, the bad ones come back for the caller to warn about, and a
// blank does not count as bad.
func TestNewOriginPolicy_mixedList(t *testing.T) {
	p, invalid := NewOriginPolicy("https://a.example", "", "nonsense", "https://b.example", "*")
	if !p.Active() {
		t.Fatal("policy inactive despite two valid entries")
	}
	if len(p.allowed) != 2 {
		t.Errorf("allowed = %v, want 2 entries", keysOf(p))
	}
	if len(invalid) != 2 || invalid[0] != "nonsense" || invalid[1] != "*" {
		t.Errorf("invalid = %v, want [nonsense *]", invalid)
	}
}

// TestNewOriginPolicy_dedupes proves two spellings of one origin collapse, so a
// count logged at startup is the number of distinct origins.
func TestNewOriginPolicy_dedupes(t *testing.T) {
	p, invalid := NewOriginPolicy("https://a.example", "https://A.EXAMPLE:443")
	if len(invalid) != 0 {
		t.Fatalf("invalid = %v", invalid)
	}
	if len(p.allowed) != 1 {
		t.Errorf("allowed = %v, want 1 entry", keysOf(p))
	}
}

func TestOriginPolicy_Allows(t *testing.T) {
	active, invalid := NewOriginPolicy("https://embed.example.com", "http://localhost:7681")
	if len(invalid) != 0 {
		t.Fatalf("setup: invalid = %v", invalid)
	}

	tests := map[string]struct {
		policy *OriginPolicy
		host   string
		origin string
		why    string
		want   bool
	}{
		"no origin header is a non-browser client": {
			policy: nil, host: "term.example.com", origin: "", want: true,
			why: "a CLI or probe sends no Origin and the header cannot govern it anyway",
		},
		"same origin, nil policy": {
			policy: nil, host: "term.example.com", origin: "https://term.example.com", want: true,
		},
		"same origin, different case": {
			policy: nil, host: "term.example.com", origin: "https://TERM.example.com", want: true,
		},
		"same host with port": {
			policy: nil, host: "term.example.com:8443", origin: "https://term.example.com:8443", want: true,
		},
		"cross origin refused by nil policy": {
			policy: nil, host: "term.example.com", origin: "https://evil.example", want: false,
			why: "the default must be same-origin only",
		},
		"cross origin allowed by policy": {
			policy: active, host: "term.example.com", origin: "https://embed.example.com", want: true,
		},
		"allowed entry with default port omitted by browser": {
			policy: active, host: "term.example.com", origin: "https://embed.example.com:443", want: true,
		},
		"cross origin not in policy": {
			policy: active, host: "term.example.com", origin: "https://other.example", want: false,
		},
		"right host wrong scheme": {
			policy: active, host: "term.example.com", origin: "http://embed.example.com", want: false,
			why: "http and https are different origins; this is the whole reason a scheme is required",
		},
		"right host wrong port": {
			policy: active, host: "term.example.com", origin: "http://localhost:9999", want: false,
		},
		"suffix of an allowed origin": {
			policy: active, host: "term.example.com", origin: "https://evilembed.example.com", want: false,
			why: "matching is exact, so no suffix or glob confusion",
		},
		"subdomain of an allowed origin": {
			policy: active, host: "term.example.com", origin: "https://x.embed.example.com", want: false,
		},
		"null origin": {
			policy: active, host: "term.example.com", origin: "null", want: false,
			why: "every sandboxed iframe and file:// page sends null",
		},
		"unparseable origin": {
			policy: active, host: "term.example.com", origin: "ht tp://%%", want: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/ws", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if got := tc.policy.Allows(r); got != tc.want {
				t.Errorf("Allows(host=%q, origin=%q) = %v, want %v (%s)",
					tc.host, tc.origin, got, tc.want, tc.why)
			}
		})
	}
}

// TestOriginPolicy_acceptOptions pins the one place InsecureSkipVerify is set,
// and that it is NOT set on the default path.
func TestOriginPolicy_acceptOptions(t *testing.T) {
	var inactive *OriginPolicy
	if opts := inactive.acceptOptions(); opts != nil {
		t.Errorf("inactive policy = %+v, want nil so coder/websocket's same-origin check stays the gate", opts)
	}

	empty, _ := NewOriginPolicy("nonsense")
	if opts := empty.acceptOptions(); opts != nil {
		t.Errorf("all-invalid policy = %+v, want nil", opts)
	}

	active, _ := NewOriginPolicy("https://embed.example.com")
	opts := active.acceptOptions()
	if opts == nil || !opts.InsecureSkipVerify {
		t.Fatalf("active policy = %+v, want InsecureSkipVerify (Allows has already decided, exactly)", opts)
	}
}

// --- live decision, through both accept sites -----------------------------

// dialWithOrigin dials url with an Origin header and returns the connection plus
// the refusal status (0 when no response arrived). It closes the response body
// itself — coder/websocket nils it on success but returns a real body on a
// refusal, and returning it would push that obligation onto every caller.
func dialWithOrigin(t *testing.T, url, origin string) (conn *websocket.Conn, status int, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	opts.HTTPHeader.Set("Origin", origin)
	conn, resp, err := websocket.Dial(ctx, url, opts)
	if resp != nil {
		if resp.Body != nil {
			resp.Body.Close()
		}
		status = resp.StatusCode
	}
	return conn, status, err
}

// TestOriginPolicy_handlerSocket exercises the per-session Handler upgrade.
func TestOriginPolicy_handlerSocket(t *testing.T) {
	p, invalid := NewOriginPolicy("https://embed.example.com")
	if len(invalid) != 0 {
		t.Fatalf("setup: invalid = %v", invalid)
	}
	h := NewHandler([]string{"/bin/cat"}, WithOriginPolicy(p), WithLogger(nil))
	defer h.Shutdown()

	mux := http.NewServeMux()
	mux.Handle("/ws", h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	t.Run("allowed origin connects", func(t *testing.T) {
		ws, _, err := dialWithOrigin(t, wsURL, "https://embed.example.com")
		if err != nil {
			t.Fatalf("dial from an allowed origin failed: %v", err)
		}
		ws.Close(websocket.StatusNormalClosure, "")
	})

	t.Run("foreign origin refused 403", func(t *testing.T) {
		ws, status, err := dialWithOrigin(t, wsURL, "https://evil.example")
		if err == nil {
			ws.Close(websocket.StatusNormalClosure, "")
			t.Fatal("dial from a foreign origin succeeded")
		}
		if status != http.StatusForbidden {
			t.Errorf("status = %d, want 403 (err: %v)", status, err)
		}
	})
}

// TestOriginPolicy_managerUnknownSessionSocket exercises the SessionManager's
// unknown-session upgrade — the site that used to hardcode nil options and so
// silently ignored a configured allowlist. An allowed origin must reach the
// definitive 4004 close; a foreign one must be refused before the upgrade.
func TestOriginPolicy_managerUnknownSessionSocket(t *testing.T) {
	p, invalid := NewOriginPolicy("https://embed.example.com")
	if len(invalid) != 0 {
		t.Fatalf("setup: invalid = %v", invalid)
	}
	factory := func(string) *Handler { return NewHandler([]string{"/bin/cat"}, WithLogger(nil)) }
	m := NewSessionManager(factory, WithManagerOriginPolicy(p), WithManagerLogger(nil))
	defer m.Shutdown()

	mux := http.NewServeMux()
	mux.Handle("/ws", m.WebSocketHandler())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?session=does-not-exist"

	t.Run("allowed origin reaches the 4004 close", func(t *testing.T) {
		ws, _, err := dialWithOrigin(t, wsURL, "https://embed.example.com")
		if err != nil {
			t.Fatalf("dial from an allowed origin failed: %v", err)
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, readErr := ws.Read(ctx)
		if got := websocket.CloseStatus(readErr); got != statusUnknownSession {
			t.Errorf("close status = %d, want %d (the readable unknown-session code)",
				got, statusUnknownSession)
		}
	})

	t.Run("foreign origin refused 403", func(t *testing.T) {
		ws, status, err := dialWithOrigin(t, wsURL, "https://evil.example")
		if err == nil {
			ws.Close(websocket.StatusNormalClosure, "")
			t.Fatal("dial from a foreign origin succeeded")
		}
		if status != http.StatusForbidden {
			t.Errorf("status = %d, want 403 (err: %v)", status, err)
		}
	})
}

// TestOriginPolicy_defaultIsSameOriginOnly is the regression guard for the
// posture a consumer gets by configuring nothing: a foreign origin is refused on
// both sockets even though no policy was ever built.
func TestOriginPolicy_defaultIsSameOriginOnly(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()
	factory := func(string) *Handler { return NewHandler([]string{"/bin/cat"}, WithLogger(nil)) }
	m := NewSessionManager(factory, WithManagerLogger(nil))
	defer m.Shutdown()

	mux := http.NewServeMux()
	mux.Handle("/ws", h)
	mux.Handle("/managed", m.WebSocketHandler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/ws", "/managed?session=nope"} {
		t.Run(path, func(t *testing.T) {
			ws, _, err := dialWithOrigin(t,
				"ws"+strings.TrimPrefix(srv.URL, "http")+path, "https://evil.example")
			if err == nil {
				ws.Close(websocket.StatusNormalClosure, "")
				t.Fatal("a foreign origin connected with no policy configured")
			}
		})
	}
}

// TestAcceptWSIsTheOnlyUpgradePath is the anti-drift guard the whole design
// rests on.
//
// When a policy is active, acceptOptions sets InsecureSkipVerify, because
// acceptWS has already made the stricter decision. That is only safe while
// acceptWS is the SOLE route to websocket.Accept: a new upgrade site that called
// the library directly with an active policy's options would have no origin check
// at all, and one that passed nil would silently ignore a configured policy —
// which is exactly the drift this file was written to end.
//
// Source-level rather than behavioural because the property is "no other call
// site exists", which no runtime assertion can observe.
func TestAcceptWSIsTheOnlyUpgradePath(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	found := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- package's own source, from ReadDir
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if n := strings.Count(string(src), "websocket.Accept("); n > 0 {
			found[name] = n
		}
	}

	want := map[string]int{"origin.go": 1}
	if len(found) != len(want) || found["origin.go"] != want["origin.go"] {
		t.Errorf("websocket.Accept call sites = %v, want %v.\n"+
			"Route every upgrade through acceptWS instead, so the origin policy "+
			"cannot be skipped or silently ignored.", found, want)
	}
}

// TestAcceptWSRefusalIsLogSafe guards the refusal path's log line. acceptWS
// builds its error from two request-supplied values, Origin and Host, and both
// callers log that error.
//
// The hostile input is deliberately the REACHABLE set, not the textbook one.
// CR/LF record forgery cannot arrive this way: net/http's header parser ends a
// value at CRLF and its client refuses to send one, so a case built around
// "\r\nlevel=ERROR" fails on the dial and proves nothing. What net/http passes
// through is every byte from 0x80 up, plus unbounded length.
//
// Only the BOUND assertion below is non-vacuous on this sink, and that is worth
// knowing rather than hiding: the caller's %q already escapes the C1, bidi and
// U+2028 runes for a TextHandler, so removing the sanitizer leaves the rune
// checks green and fails on length alone (verified by doing exactly that). The
// rune checks are kept as the guard for a sink that does not quote; the direct,
// non-vacuous coverage of the class stripping is TestLogSafeHeader.
//
// Driven end to end rather than by calling logSafeHeader directly, because the
// property is "nothing unsanitized reaches the logger" and only the full path
// shows that. The logger is the handler's own (WithLogger), so nothing here
// touches the global default and the test needs no serialization.
func TestAcceptWSRefusalIsLogSafe(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandler([]string{"/bin/cat"},
		WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))))
	defer h.Shutdown()

	mux := http.NewServeMux()
	mux.Handle("/ws", h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	unsafeRunes := []rune{'\u009b', '\u202e', '\u2028'}
	hostile := "https://evil.example" + string(unsafeRunes) + strings.Repeat("A", 4096)

	ws, status, err := dialWithOrigin(t, wsURL, hostile)
	if err == nil {
		ws.Close(websocket.StatusNormalClosure, "")
		t.Fatal("dial from a hostile origin succeeded")
	}
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (err: %v)", status, err)
	}

	logged := buf.String()
	if logged == "" {
		t.Fatal("the refusal logged nothing, so this test proves nothing about its content")
	}
	// slog writes exactly one trailing newline per record, so a second one means
	// a record was forged.
	if n := strings.Count(logged, "\n"); n != 1 {
		t.Errorf("log holds %d newlines, want exactly 1 (a forged record got through):\n%q", n, logged)
	}
	for _, r := range unsafeRunes {
		if strings.ContainsRune(logged, r) {
			t.Errorf("log carries unsafe rune %U verbatim:\n%q", r, logged)
		}
	}
	// The cap bounds the line: 4096 'A's cannot all survive.
	if strings.Contains(logged, strings.Repeat("A", maxLoggedOriginBytes+4)) {
		t.Errorf("log line is not bounded by maxLoggedOriginBytes=%d:\n%q",
			maxLoggedOriginBytes, logged)
	}
}

// TestLogSafeHeader covers the sanitizer directly, which is where the rune-class
// half of the contract can actually be observed. Asserting it through the log
// line cannot: slog's TextHandler quotes the value, so an unsanitized C1 or bidi
// rune shows up escaped either way (see TestAcceptWSRefusalIsLogSafe).
func TestLogSafeHeader(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want string
	}{
		"ordinary origin is untouched": {
			in:   "https://embed.example.com:8443",
			want: "https://embed.example.com:8443",
		},
		"C1 introducer becomes a space": {
			in:   "https://a\u009bb",
			want: "https://a b",
		},
		"bidi override becomes a space": {
			in:   "https://a\u202eb",
			want: "https://a b",
		},
		"JS line terminator becomes a space": {
			in:   "https://a\u2028b",
			want: "https://a b",
		},
		"CR and LF become spaces": {
			in:   "https://a\r\nlevel=ERROR",
			want: "https://a  level=ERROR",
		},
		"empty stays empty": {in: "", want: ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := logSafeHeader(tc.in); got != tc.want {
				t.Errorf("logSafeHeader(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("over-long input is capped with a marker", func(t *testing.T) {
		t.Parallel()
		got := logSafeHeader(strings.Repeat("A", 4096))
		// The preset places its "..." marker OUTSIDE the cap, so the bound is
		// maxLoggedOriginBytes plus the marker (runesafe.md, API).
		if len(got) > maxLoggedOriginBytes+3 {
			t.Errorf("len = %d, want <= %d", len(got), maxLoggedOriginBytes+3)
		}
		if !strings.HasSuffix(got, "...") {
			t.Errorf("got %q, want a truncation marker suffix", got)
		}
	})
}
