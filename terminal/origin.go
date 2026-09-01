package terminal

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/cplieger/runesafe/v2"
)

// OriginPolicy is the set of browser origins allowed to open a terminal
// WebSocket, in addition to the serving origin itself.
//
// It carries the whole cross-origin decision for this engine's sockets, because
// nothing above it can. A WebSocket handshake is a GET, and
// net/http.CrossOriginProtection — the CSRF middleware every consumer of this
// engine runs — returns early for GET, HEAD and OPTIONS as safe methods
// (csrf.go, Check). So an app-level cross-origin middleware never inspects the
// upgrade at all, and this policy plus the same-origin floor beneath it is the
// only gate between a hostile page and an interactive shell.
//
// A nil *OriginPolicy is valid and means same-origin only. That is the default
// and the safe direction, so a consumer that configures nothing is protected.
// The type is immutable once built and safe for concurrent use.
type OriginPolicy struct {
	allowed map[string]struct{}
}

var errNotAnOrigin = errors.New("not a complete http(s) origin")

// NewOriginPolicy validates an operator-supplied origin allowlist and returns
// the policy plus the entries it dropped, so a caller can warn about a typo
// without failing startup (the drop-and-report shape webhttp's ParseHostList and
// ParseCIDRs use).
//
// Each entry must be a complete origin — scheme, host, optional port, and
// nothing else: "https://terminal.example.com", "http://localhost:7681",
// "https://[::1]:7681". The strictness is deliberate:
//
//   - A scheme is REQUIRED and must be http or https. "example.com" is not an
//     origin, and http://x and https://x are different origins that must not
//     collapse onto one entry.
//   - A path, query, fragment, or userinfo is refused. An origin has none of
//     them (RFC 6454), so such an entry could only ever be config that silently
//     matches nothing.
//   - "null" is refused, along with every other non-http(s) scheme. Browsers
//     send Origin: null for a sandboxed iframe and for a file:// page, so
//     allowing it would admit all of them at once.
//   - There are no wildcards. A pattern language buys one thing here (a whole
//     subdomain tree) and costs the "*" that quietly means allow-everything.
//
// A blank entry is skipped rather than reported, so a trailing comma in an env
// var is not an error. Matching is exact, on the origin lowercased and stripped
// of a default port.
//
// An all-invalid list yields an INACTIVE policy — same-origin only. That is the
// fail-closed direction for this type, and it is deliberately the opposite of
// webhttp's HostPolicy, where an all-invalid list becomes an active deny-all: a
// Host allowlist is a GATE, so an empty one must refuse everything, while an
// origin allowlist only ever WIDENS the same-origin floor, so an empty one must
// widen nothing.
func NewOriginPolicy(origins ...string) (policy *OriginPolicy, invalidEntries []string) {
	allowed := make(map[string]struct{}, len(origins))
	var invalid []string
	for _, raw := range origins {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		key, err := canonicalOrigin(entry)
		if err != nil {
			invalid = append(invalid, raw)
			continue
		}
		allowed[key] = struct{}{}
	}
	if len(allowed) == 0 {
		// nil is the single representation of "inactive", so there is no second
		// way to be inactive that a later check could miss.
		return nil, invalid
	}
	return &OriginPolicy{allowed: allowed}, invalid
}

// canonicalOrigin validates one operator entry and renders it as a comparison
// key, or reports why it is not a usable origin.
func canonicalOrigin(entry string) (string, error) {
	u, err := url.Parse(entry)
	if err != nil {
		return "", err
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", errNotAnOrigin
	case u.Host == "":
		return "", errNotAnOrigin
	case u.Opaque != "", u.User != nil, u.Path != "", u.RawQuery != "", u.ForceQuery, u.Fragment != "":
		return "", errNotAnOrigin
	case strings.ContainsAny(u.Host, "*?"):
		// Refuse a wildcard rather than store it. Matching is exact, so an entry
		// like "https://*.example.com" is not dangerous — it simply matches
		// nothing, because no browser sends that as an Origin. Silently
		// accepting it is the harm: the operator believes they allowed a
		// subdomain tree and allowed nobody. Reporting it invalid says so.
		return "", errNotAnOrigin
	}
	return originKey(u), nil
}

// originKey renders a parsed origin as the string both sides of the comparison
// are reduced to: host lowercased, and the scheme's default port dropped the way
// a browser's own Origin serialization drops it (RFC 6454 section 6.1), so an
// operator who writes "https://x:443" still matches the "https://x" a browser
// sends instead of silently matching nothing.
func originKey(u *url.URL) string {
	host := strings.ToLower(u.Host)
	switch {
	case u.Scheme == "https" && strings.HasSuffix(host, ":443"):
		host = strings.TrimSuffix(host, ":443")
	case u.Scheme == "http" && strings.HasSuffix(host, ":80"):
		host = strings.TrimSuffix(host, ":80")
	}
	return u.Scheme + "://" + host
}

// Active reports whether the policy allows any origin beyond same-origin.
// Useful for a startup log line; nil-safe.
func (p *OriginPolicy) Active() bool { return p != nil && len(p.allowed) > 0 }

// Allows reports whether r may open a terminal session under this policy.
// Exported so a consumer can apply the same decision to its own routes.
//
// A request with no Origin header is allowed: that is a non-browser client (a
// CLI, a test, a health probe), which this header cannot govern anyway. A
// same-origin request is allowed. Everything else must appear in the allowlist
// exactly.
//
// The same-origin comparison is host-only, because the Host header carries no
// scheme — the same limitation net/http.CrossOriginProtection documents about
// its own fallback. An http-to-https difference on one host is therefore not
// distinguished here, and HSTS is the mitigation.
//
// Nil-safe: a nil policy allows same-origin only.
func (p *OriginPolicy) Allows(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		// Unparseable, or the literal "null" a sandboxed iframe sends.
		return false
	}
	if strings.EqualFold(r.Host, u.Host) {
		return true
	}
	if p == nil {
		return false
	}
	_, ok := p.allowed[originKey(u)]
	return ok
}

// acceptOptions renders the policy for coder/websocket.
//
// Inactive: nil, which leaves the library's own same-origin check as the gate.
//
// Active: InsecureSkipVerify, because Allows has already made a strictly
// stronger decision than AcceptOptions.OriginPatterns can express. That matching
// is path.Match-based (accept.go, match), so "*" means allow-everything, the
// brackets of an IPv6 literal read as a character class, and the scheme is
// compared only when the pattern happens to contain "://". Handing it patterns
// would downgrade an exact decision to a glob one. The flag is reachable ONLY
// through acceptWS, which has just enforced the policy, and is never set on the
// default path.
func (p *OriginPolicy) acceptOptions() *websocket.AcceptOptions {
	if !p.Active() {
		return nil
	}
	return &websocket.AcceptOptions{InsecureSkipVerify: true}
}

// acceptWS enforces the origin policy and then upgrades the connection.
//
// This is the only call site of websocket.Accept in the package, so the
// Handler's session socket and the SessionManager's unknown-session socket
// cannot disagree about origin policy. They did before it existed: the manager
// hardcoded nil options while the handler honoured a configured allowlist, so a
// widened policy worked for a live session and silently reverted to same-origin
// for a reaped one — replacing the readable 4004 close with an opaque 403 and
// stranding the client in the very reconnect loop 4004 exists to break.
func acceptWS(w http.ResponseWriter, r *http.Request, p *OriginPolicy) (*websocket.Conn, error) {
	if !p.Allows(r) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return nil, fmt.Errorf("terminal: origin %q is not allowed for host %q",
			logSafeHeader(r.Header.Get("Origin")), logSafeHeader(r.Host))
	}
	return websocket.Accept(w, r, p.acceptOptions())
}

// maxLoggedOriginBytes bounds one request-supplied value in the refusal error.
// A legitimate origin is a scheme, a host and maybe a port, so 128 bytes is
// generous for every real one while refusing to carry an arbitrarily long
// header into the logs.
const maxLoggedOriginBytes = 128

// logSafeHeader prepares a request-supplied header value to appear in the
// refusal error above, which the two callers log.
//
// Both values it guards — Origin and Host — are chosen by whoever made the
// request, and the error is built at the one moment nobody trusts them: the
// refusal. Sanitizing HERE rather than at the log sites is the construction-time
// boundary this fleet uses for error-class text (runesafe.md, "Adoption rules"):
// the error is safe from the moment it exists, so no present or future caller
// can reach an unsanitized form of it.
//
// SanitizeSingleLineBounded is the log-bound preset: runesafe's four unsafe
// classes become spaces, CR and LF included, and the result is capped on a rune
// boundary.
//
// Which part of that is load-bearing depends on the sink, and both obvious
// answers are wrong, so both are worth stating.
//
// CR/LF record forgery (CWE-117) is NOT reachable through this path at all:
// net/http's own header parser ends a header value at CRLF, so Origin and Host
// cannot contain either byte to begin with, and its client refuses to send one.
// Stripping them is defence in depth for a front end that is not net/http.
//
// The rune classes that ARE reachable — everything net/http's
// validHeaderValueByte permits from 0x80 up, so C1 controls (U+009B is a
// single-byte CSI introducer to a terminal reading the log), the Bidi_Control
// set (U+202E reorders the line in a viewer) and U+2028/U+2029 — are already
// neutralized on TODAY's sink by the caller's %q, whose strconv quoting escapes
// them. That is measured, not assumed: removing this call leaves the rune
// assertions in TestAcceptWSRefusalIsLogSafe passing. They matter for a sink
// that does NOT quote, which is the case runesafe exists for (slog's JSONHandler
// emits C1 raw), and that is why the class stripping stays rather than being
// deleted as redundant.
//
// What this call buys on the current sink is the CAP. A header value has no
// useful bound, and one refused handshake should not write a multi-kilobyte log
// line; %q does nothing about length. TestAcceptWSRefusalIsLogSafe fails on that
// assertion alone with the sanitizer removed.
//
// The %q in the caller stays regardless: it is a second, independent escape, and
// it makes the substituted spaces visible rather than letting them merge into
// the sentence.
func logSafeHeader(v string) string {
	return runesafe.SanitizeSingleLineBounded(v, maxLoggedOriginBytes)
}
