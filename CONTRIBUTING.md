# Contributing to web-terminal-engine

web-terminal-engine is a cross-language terminal library: a Go VT100/VT500 emulator plus WebSocket session handler, and a browser-side TypeScript renderer. The two halves never share code. They communicate over a versioned wire protocol, so protocol compatibility is the first thing to understand before changing either side.

## Architecture

Three packages, two languages, one wire contract:

- **`vt/` (Go)**: The VT100/VT500 screen buffer. It parses terminal byte streams (CSI/OSC/DCS, SGR, DEC modes, charsets, mouse) into a cell grid and renders rows to the wire format. No I/O or networking.
- **`terminal/` (Go)**: The WebSocket session handler. It bridges a PTY (`github.com/creack/pty`) to a browser through `github.com/coder/websocket`, drives a `vt` screen, and adds reconnect, scrollback replay, adaptive ping, and the resume/inputAck reliability layer. `terminal/` also provides `SessionManager` (`session_manager.go`, `events.go`) for the multi-session `/ws?session=`, `/api/sessions`, and status-SSE surface.
- **`web/` (TypeScript)**: The browser renderer (`render`), keyboard mapper (`keyboard`), on-screen mobile toolbar (`toolbar`), mouse/focus encoder (`mouse`), DEC-mode state (`modes`), scroll tracker (`scroll`), socket lifecycle (`connection`), and binary frame decoder (`decodeWireBinary`). It is published as `@cplieger/web-terminal-engine` with zero runtime dependencies.

### The wire contract is load-bearing

The Go server and TS client agree on a byte-level WebSocket protocol without importing shared types. The code is authoritative, backed by round-trip fuzz tests and `wire-golden/*.bin` fixtures. The README's "Wire Protocol" section documents the consumer contract and design rationale. The canonical implementations are:

- Encoder (Go): `terminal/wire_binary.go`
- Wire row types (Go): `vt/wire.go`
- Decoder (TS): `web/src/wire-binary.ts`
- Client control/input path (TS): `web/src/connection.ts`, `web/src/wire.ts`, `web/src/wsurl.ts`
- Compatibility metadata (TS): `web/src/wire-compatibility.ts`

The Go and TypeScript artifacts can be installed and upgraded independently. Package-version equality isn't the compatibility contract. Each release exports a current wire revision and a directional receiver floor:

- Go: `terminal.WireProtocolVersion` and `terminal.MinSupportedClientWireVersion`
- TypeScript: `WIRE_PROTOCOL_VERSION`, `MIN_SUPPORTED_SERVER_WIRE_VERSION`, `WIRE_INCOMPATIBLE_CLOSE_CODE`, and `WIRE_COMPATIBILITY`

Keep the two current revisions equal. Their floors may diverge when one receiver retires an older decode path. A version-silent peer remains supported; a declared peer below the receiver's floor is refused with close code 4002; a higher revision warns and continues because it may preserve the compatible baseline.

When changing the protocol:

- Bump the revision for a breaking frame-layout or control-message change. Update both implementations, metadata exports, tests, and README contract in the same PR.
- Keep compatible evolution append-only when possible. Add server-to-client opcodes, use length-gated frame tails, and never change an existing field or opcode meaning in place.
- Raise only the affected receiver's floor when it can no longer decode a previously supported revision. Update that floor's export, enforcement tests, compatibility snapshot, and release notes together.
- State metadata changes in release notes: `Wire: revision 4; Go accepts declared clients from revision 3; TypeScript accepts declared servers from revision 3; version-silent peers remain supported.`

### Cross-version compatibility tests

The normal Go and Vitest jobs carry the compatibility gate:

- `wire-golden/v3-published.json` is the frozen wire-v3 fixture snapshot published at tag `v2.8.0`.
- The current TypeScript decoder must decode every frozen v3 fixture.
- The published `@cplieger/web-terminal-engine@2.8.0` decoder, installed under the test-only alias `@cplieger/web-terminal-engine-v3`, must tolerate every current fixture.
- Go and TypeScript tests require the frozen fixture revision to equal their declared floor.
- The current resumeAck golden carries the Go revision and is decoded against the TypeScript revision, keeping the current-revision mirrors equal.

Advance the frozen snapshot and pinned previous decoder only when intentionally raising a floor. Keep the outgoing-floor fixtures in the same change so the compatibility loss is explicit in review.

### Intentional non-features

The README's [Unsupported by Design](README.md#unsupported-by-design) table lists deliberate VT/DEC scope decisions. Input for those sequences is consumed but produces no effect. Don't file them as bugs or implement them without first proposing a scope change.

## Local development

The Go packages live at the repo root; the TypeScript package lives in `web/`. The two toolchains are independent.

### Go (`vt/`, `terminal/`)

```sh
go build ./...
go test ./...
go test -race ./...
golangci-lint run
golangci-lint fmt
```

`go.mod` targets Go 1.26+. Linting uses golangci-lint v2 (`.golangci.yaml`). `golangci-lint run` reports unformatted files, so run `golangci-lint fmt` before pushing. The config enables a strict linter set, including gosec, gocritic, revive, gocyclo and gocognit at complexity 15, sloglint in key/value mode, and gofumpt/gci formatting.

### TypeScript (`web/`)

```sh
cd web
npm install
npm run typecheck
npm run typecheck:tests
npm test
npm run test:e2e
npx eslint .
npx prettier --check .
```

The Playwright `e2e/` suite runs the real modules in headless Chromium. It checks display conformance against Go-generated `render-golden` fixtures, keyboard encoding against real browser events, and typed-framing negotiation against a real Go server. Install Chromium once with `npx playwright install chromium`. Happy-dom unit tests live under `src/`.

ESLint uses the `strictTypeChecked` and `stylisticTypeChecked` presets. `tsconfig.json` enables `exactOptionalPropertyTypes`, `noUncheckedIndexedAccess`, `noPropertyAccessFromIndexSignature`, and related strict checks. Vitest runs in a `node` environment with `requireAssertions`; `fc-strict-setup.ts` configures fast-check property tests.

CI (`.github/workflows/ci.yaml`) detects both surfaces and runs Go and TypeScript jobs in parallel. Run checks for the half you changed, or both for protocol changes.

## Conventions and gotchas

- **Tests live beside the code** (`*_test.go`, `*.test.ts`). The suites rely on fuzz and adversarial tests. Extend the fuzz corpus for parser or codec changes instead of adding only a happy path.
- **Public API is a contract.** Exported Go symbols and TypeScript package exports are documented in the READMEs. Keep them synchronized when adding or renaming public symbols.
- **Keep dependencies small.** The Go side uses the standard library, `coder/websocket`, `creack/pty`, and `cplieger/runesafe`; the TypeScript package has zero runtime dependencies.

## Publishing model

Releases are automated through `.github/workflows/release.yaml`. Repository releases publish the Go module as `github.com/cplieger/web-terminal-engine/v3` and the TypeScript package to npm and JSR as `@cplieger/web-terminal-engine`. Consumers install and upgrade those artifacts independently, with compatibility determined by wire metadata rather than package-version equality. Don't publish manually.

## Commits and PRs

Branch from `main`, keep changes focused, and open a PR. Commits follow [Conventional Commits](https://www.conventionalcommits.org/) (parsed by git-cliff for release notes): `feat:`, `fix:`, `sec:`, and the non-releasing `chore:`/`ci:`/`docs:`/`refactor:`/`test:` types. Wire-format changes that break compatibility use `feat!:` or a `BREAKING CHANGE:` footer.

## Conduct and security

By participating you agree to the [Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md). Report security vulnerabilities through the [security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md), never in a public issue.
