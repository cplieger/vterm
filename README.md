# web-terminal-engine

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/web-terminal-engine/v3.svg)](https://pkg.go.dev/github.com/cplieger/web-terminal-engine/v3)
[![npm](https://img.shields.io/npm/v/@cplieger/web-terminal-engine)](https://www.npmjs.com/package/@cplieger/web-terminal-engine)
[![JSR](https://jsr.io/badges/@cplieger/web-terminal-engine)](https://jsr.io/@cplieger/web-terminal-engine)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/web-terminal-engine/badges/coverage.json)](https://github.com/cplieger/web-terminal-engine/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/web-terminal-engine/badges/mutation.json)](https://github.com/cplieger/web-terminal-engine/issues?q=label%3Agremlins-tracker)
[![Mutation (TS)](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/web-terminal-engine/badges/mutation-ts.json)](https://github.com/cplieger/web-terminal-engine/issues?q=label%3Astryker-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13225/badge)](https://www.bestpractices.dev/projects/13225)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/web-terminal-engine/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/web-terminal-engine)

> Cross-language terminal emulator and session engine (Go) with browser renderer (TypeScript).

A standalone library that bridges a PTY to a browser WebSocket. The Go packages provide a VT100/VT500 screen buffer with SGR support and a WebSocket-based terminal session handler with reconnect, scrollback replay, and adaptive ping. The TypeScript package provides the browser-side renderer, keyboard mapper, mouse encoder, and binary wire decoder. No app-specific dependencies; only the standard library, `github.com/coder/websocket`, `github.com/creack/pty`, and `github.com/cplieger/runesafe`.

## Install

- Go: `go get github.com/cplieger/web-terminal-engine/v3@latest`
- TS: `npx jsr add @cplieger/web-terminal-engine` or `npm i @cplieger/web-terminal-engine`

## Usage

```go
import (
    "log/slog"
    "net/http"

    "github.com/cplieger/web-terminal-engine/v3/terminal"
)

h := terminal.NewHandler(
    []string{"/bin/bash"},
    terminal.WithWorkDir("/home/user"),
    terminal.WithLogger(slog.Default()),
)
mux := http.NewServeMux()
h.RegisterRoutes(mux)
// or use h as an http.Handler directly:
// mux.Handle("/ws", h)
```

```typescript
import { render, keyboard, mouse, decodeWireBinary } from "@cplieger/web-terminal-engine";

render.init({
  output: document.getElementById("term-output")!,
  termWrap: document.getElementById("term")!,
});
// On WebSocket binary message:
const msg = decodeWireBinary(event.data);
if (msg?.type === "screen") render.handleScreen(msg);
```

## API

### Go packages

- **`vt`**: VT100/VT500 screen buffer. `New(rows, cols)`, `Write([]byte)`, `Resize(rows, cols)`, `RenderRowWire(y)`, `DrainScrollback()`, `CursorPos()`, `HoldFlush()`, `ReleaseFlush()`, `IsFlushHeld()`, `RenderViewport()`, `RowString(y)`; atomic one-shot event drains `TakeResponse()`, `TakeClipboard()`, `TakeBell()`, `TakeScrollbackCleared()`, `TakePaletteChanged()`. Public fields: `Cells`, `Width`, `Height`, `Title`, `MouseMode`, `InAltScreen`, cursor/mode state.
- **`terminal`**: WebSocket session handler. `NewHandler(command, ...Option)`, `RegisterRoutes(mux)`, `ServeHTTP(w, r)`, `Shutdown()`; options `WithWorkDir`, `WithLogger`, `WithEnv`, `WithScrollbackCapacity`, `WithAcceptOptions`, `WithOnProcessExit`, `WithKeepUnfocused`, `WithTheme`, `WithCommandLogValue` (the process-start line's `command` attribute — the engine's only argv-bearing log site — records the given fixed marker, e.g. `"[redacted]"`, instead of the child argv, for consumers whose argv embeds operator-supplied values that could carry a credential; empty is ignored, keeping the default argv logging). Two release/logging helpers round out the surface: `LogID(id)` truncates a session id to a correlation-safe 8-byte prefix plus an ellipsis — the session id is a WS resume capability token, so every consumer that logs one must pass it through here rather than re-deriving the truncation (CWE-532); for the same reason the engine sets `Cache-Control: no-store` on every response that carries an id in full (the two session JSON bodies and the SSE stream), so a consumer inherits the cache half of that protection instead of bolting on its own middleware; `WirePairIncompatibility(serverRev, serverMinClient, clientRev, clientMinServer)` is the peer-less build-time form of the wire-compatibility decision the runtime already makes twice, so a consumer's release gate can refuse a mismatched Go/TS pair before the image ships instead of discovering it as a close-4002 at first connect (returns "" when compatible, else a reason; both cross-side floors exclusive; a non-positive input is a caller error rather than runtime's tolerated version-silent 0; and a half whose own minimum-peer floor exceeds its own revision is reported as self-inconsistent — corrupt or mis-extracted input — BEFORE any cross-side skew verdict, so a garbage pair never yields a confident "bump your pin" diagnosis). `(*Handler).ScrollbackBounds()` returns the session's retained-history bounds as an atomic pair — the committed index (one past the newest committed line) and the oldest still-replayable index — so a consumer can observe which absolute indices a resume can still ask for without decoding a wire frame; read-only, since bounds follow the child's output and the configured capacity. Handles PTY lifecycle, binary wire protocol, reconnect with scrollback replay, adaptive ping. `SessionManager` (`NewSessionManager`) fronts N PTY-backed sessions with `WebSocketHandler()` (`/ws?session=<id>`), `RESTHandler()` (`/api/sessions`, plus `PUT`/`DELETE /api/sessions/{id}/pinned-title` to set and clear a session's user-chosen name), and `EventsHandler()` (SSE `/api/sessions/events`); status values working/idle/failed/warning/exited/crashed are derived server-side (working/failed/warning from the program's own OSC 9;4 progress report, read with iTerm2's state semantics; `exited` vs `crashed` splits an ordinary session end — status 0, or any exit the server itself caused, such as a closed session, the idle reaper or a shutdown — from a non-zero or signalled one, so a routine restart is never reported as a failure, and `(*Handler).ExitError()` exposes the retained `cmd.Wait()` error behind that decision), while input/done are latched from an OSC 9 notification through a pluggable classifier; each status event also carries the OSC 9;4 percentage as `progressValue` (-1 when the program reported none, which is not 0%) and delivers a fresh OSC 9 notification as `notification` + `notificationSeq`, so a consumer with no classifier installed still receives the message instead of it being dropped. Each session's `title` is RESOLVED server-side from four inputs in precedence order — the user's pinned name, the name derived from what the user typed, the window title the program set via OSC 0/2, and the foreground process or working directory — so every attached client shows the same label without re-deriving the ladder, and `pinnedTitle` travels alongside it so a client can tell a chosen name from an inferred one. When several clients share one session, a live resize is last-writer-wins and the shared screen relaxes to the smallest remaining client's size on disconnect. `MountSessionRoutes(mux, ws, rest, events, ...MountOption)` mounts exactly the route constants `WSPath`, `SessionsPath` + `SessionsSubtreePath` (the REST handler needs both mounts), and `SessionEventsPath`; the paths mirror the TS client's exported defaults, and additions are release-noted. `WithCreateGate(mw)` wraps the REST handler with caller-supplied middleware to rate-limit session creation (each POST forks a process). `(*SessionManager).MountAPI(mux, opts...)` is the one-manager convenience.

  Spawned processes inherit the server's environment plus a default terminal identity: `TERM=xterm-256color`, `COLORTERM=truecolor`, `TERM_PROGRAM=iTerm.app`, and `TERM_PROGRAM_VERSION=3.6.6`, so apps detect truecolor, OSC 9;4 progress reporting, and DEC 2026 synchronized output. `WithEnv` values are appended after these defaults, so a consumer entry for the same variable overrides them.

### TypeScript (`web/`, published as `@cplieger/web-terminal-engine` on NPM and JSR)

- **`render`**: DOM renderer driven by `ScreenMessage` / `ScrollMessage` frames. `init`, `handleScreen`, `handleScroll`, `updateFontMetrics`, `computeSize`, `getCursorPx`, `setPredictedCursor`, `resetScreen`, `resetScrollback`, `getHighestIndex`, `noteResumeBounds`, `updateReverseVideo`.
- **`keyboard`**: Translates `KeyboardEvent` to terminal byte sequences. `mapKeyboardEvent`, `bracketTextForPaste`, `prepareTextForTerminal`. Honors `applicationCursor`, `applicationKeypad`, `bracketedPaste`, and the kitty keyboard **disambiguate** flag (emits `CSI u` key events when the app enables the protocol).
- **`toolbar`**: On-screen mobile keyboard toolbar wiring touch buttons to a send sink, with sticky-Ctrl state and ARIA painting. `bindMobileToolbar(opts)` returns a `MobileToolbarController`; `DEFAULT_TOOLBAR_IDS` names the default button ids. Reuses the `keyboard` encodings, so toolbar keys match physical keys byte for byte.
- **`mouse`**: SGR 1006 mouse + focus reporting encoder. `init`, `encodeSGR`, `MouseInputHandler`.
- **`scroll`**: Auto-follow tracker for the scroll container. `init`, `stickToBottom`, `scrollToBottom`, `isUserScrolledUp`.
- **`modes`**: DEC private mode state (synced from server's `ModesMessage`). `setModes`, `isBracketedPaste`, `isApplicationCursor`, `getMouseMode`, `isMouseSGR`, `isFocusReporting`, `isApplicationKeypad`, `isReverseVideo`, `getKeyboardFlags`.
- **`decodeWireBinary(buf)`**: Top-level decoder for binary WebSocket frames; returns a `ServerMessage` or `null` for invalid/truncated frames.
- **Wire compatibility metadata**: `WIRE_PROTOCOL_VERSION`, `MIN_SUPPORTED_SERVER_WIRE_VERSION`, `WIRE_INCOMPATIBLE_CLOSE_CODE`, and `WIRE_COMPATIBILITY` describe the TypeScript release's directional contract. The Go `terminal` package exposes the complementary `WireProtocolVersion`, `MinSupportedClientWireVersion`, and `WireIncompatibleCloseCode` constants. The same values also ship as a generated, language-neutral JSON artifact at the package root of every npm and JSR release (`wire-compatibility.json`, importable from npm as `@cplieger/web-terminal-engine/wire-compatibility.json`), so a Dockerfile or shell release gate can read them with `jq` instead of scraping TypeScript source; it is generated from `WIRE_COMPATIBILITY`, carries a `schemaVersion` a consumer must check, and is guarded by a regenerate-and-diff test plus a conformance test pinning it to both the TypeScript and the Go constants. Its consumer contract and what counts as a breaking change to it are documented in [web/README.md](web/README.md#wire-compatibility-manifest).
- **`connection`**: Client → server WebSocket lifecycle, including socket ownership, exponential-backoff reconnect, and the resume/inputAck reliability layer (outbox + server-restart detection). Public methods are `init(callbacks)`, `connect`, `sendBinary`, `sendResize`, and `reconnectNow`; the `wsPath` callback option defaults to `"/ws"`. The module decodes frames and applies `modes.setModes` internally, so consumers only dispatch screen/scroll messages to `render`. It pairs with the Go `terminal` handler's resume protocol. `controlFrame` and `wsURL` are also exported for advanced use.
- **`connectStatusStream`** and the `SessionInfo` / `SessionStatus` / `StatusStream` types: The SSE client for `/api/sessions/events` that drives per-tab status. `SessionInfo` carries both the resolved `title` and the user's `pinnedTitle`. `LineStore` and `CONTROL_FRAME_PREFIX` are also exported, as are the `WS_PATH` / `SESSIONS_PATH` / `SESSION_EVENTS_PATH` route constants mirroring the Go `terminal` package's mount contract.
- **Wire types**: `WireRun`, `ScreenMessage`, `ScrollMessage`, `ModesMessage`, `TitleMessage`, `ResumeAckMessage`, `ServerMessage`, and `ControlMessage` are re-exported from the package root.

## Wire Protocol

The Go server and TypeScript client communicate over WebSocket frames rather than shared code. The authoritative byte-level definition is the code itself: the Go encoder (`terminal/wire_binary.go`), the Go `WireRun` types (`vt/wire.go`), and the TS decoder (`web/src/wire-binary.ts`). Round-trip fuzz tests and the `wire-golden/*.bin` fixtures keep those implementations aligned.

- **Binary and typed framing.** Server → client frames are binary messages with little-endian integers. Since wire v4, client → server control messages (resize, resume, ping) are text frames containing bare JSON, while binary frames carry raw terminal input with the full byte alphabet. Every socket first sends a v3-compatible `0x00`-prefixed binary resume and upgrades only after the resumeAck proves a v4 server.
- **Absolute line indexing.** Every line receives a monotonic absolute index. Applying the same line is idempotent, resume aligns by index, eviction gaps are detectable, and a server epoch identifies restarts across reconnects.
- **Compatibility revisions and directional floors.** The Go and TypeScript artifacts can be upgraded independently; package-version equality isn't required. Each side exports its current wire revision and its receiver floor (the constants listed in [API](#api)). Both sides currently emit revision 4 and accept declared peers from revision 3. Version-silent peers remain supported. A declared revision below the receiver's floor is refused with close code 4002; a higher revision warns but continues because it may retain the compatible baseline. TypeScript consumers can surface these outcomes with `onWireVersionMismatch` and `onWireIncompatible`. A definitive incompatibility blocks automatic reconnects until `disconnect()` clears the terminal state, normally after the stale half is updated and the page reloads. Frozen previous-revision fixtures and the previous published decoder test both compatibility directions.

Client → server input for DEC modes (SGR 1006 mouse, focus reporting, application keypad) is encoded by the TS `mouse` / `keyboard` modules and consumed server-side by `vt`. For VT/DEC features intentionally absent from the wire, see [Unsupported by Design](#unsupported-by-design).

## Unsupported by Design

The following VT/DEC features are **intentionally not implemented**. Input bytes for these sequences are consumed (not echoed or half-rendered) and produce no visible effect, except where a row notes a performed side effect. This is a deliberate design choice, not a TODO.

| Category                       | Sequences                                                                                                          | Rationale                                                                                                                                                                                                                                                      |
| ------------------------------ | ------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Double-width/height lines      | DECDWL, DECDHL                                                                                                     | Requires line-level rendering attribute + renderer changes; purely legacy VT220 feature unused by modern apps.                                                                                                                                                 |
| Programmatic resize / geometry | DECCOLM (132-column) width change, XTWINOPS window resize/move/iconify/maximize, DECSLPP/DECSNLS                   | The browser viewport and PTY winsize own the terminal size, and a browser tab has no OS window to move. Only the size _change_ is declined: DECCOLM's clear/home side effects, the title stack (22/23), and the size/label reports (18/19/20/21) are honored.  |
| DCS device control             | tmux control-mode passthrough                                                                                      | Not modeled; consumed silently. DECRQSS and the XTGETTCAP color-count query share the same DCS parser and **are** supported (see the note below).                                                                                                              |
| Graphics protocols             | Sixel, ReGIS, Kitty image protocol, iTerm inline images                                                            | Specialized rendering pipeline incompatible with the DOM-based renderer.                                                                                                                                                                                       |
| NRCS national charsets         | All national replacement character sets except UK (DEC Special Graphics, UK NRCS `#`→`£`, and ASCII are supported) | Legacy internationalization mechanism superseded by UTF-8. No modern app emits these.                                                                                                                                                                          |
| Exotic SGR attributes          | Fonts 10-20, framed/encircled (51/52/54), superscript/subscript (73-75), ideogram (60-65)                          | No modern terminal or app uses these attributes; they have no visual representation in standard monospace fonts.                                                                                                                                               |
| X11 Xcms color specifications  | CIE Lab/Luv/XYZ/uvY/xyY, rgbi intensity, TekHVC in OSC 4/5/10-19                                                   | libX11 device-colorimetry, not the VT/ANSI spec; no CLI tool emits them. The `rgb:` / `#hex` forms and the palette + dynamic-color set/query/reset are all supported.                                                                                          |
| ZWJ emoji grapheme clustering  | Zero-width joiner sequences are not clustered into single cells                                                    | Requires ICU-level grapheme segmentation. Individual emoji codepoints render correctly; only multi-codepoint ZWJ sequences (family emoji, skin-tone modifiers) may misalign.                                                                                   |

> **Note on device queries:** several report/query sequences sharing the DCS or CSI parsers **are** supported for conformance. **DECRQSS** (`DCS $ q … ST`) answers SGR (`m`), scroll region (`r`), cursor style (`SP q`), protection (`" q`), conformance level (`" p`), left/right margins (`s`) and lines-per-page/screen (`t`, `* |`), replying `DCS 0 $ r ST` for anything else. **XTGETTCAP** (`DCS + q … ST`) answers the color-count capability (256). **DECRQCRA** (rectangular-area checksum) and **OSC 52** clipboard read-back are answered only when `Screen.AllowScreenReport` is enabled; both inject their reply into the PTY, so they default off. The VT model is validated against the [esctest2](https://github.com/ThomasDickey/esctest2) conformance suite (see CONTRIBUTING).
>
> **Note on the kitty keyboard protocol:** the [progressive-enhancement](https://sw.kovidgoyal.net/kitty/keyboard-protocol/) negotiation **is** implemented: `CSI ? u` (query) is answered, and `CSI > u` / `CSI < u` / `CSI = u` (push / pop / set) manage per-screen flag stacks, so an app that queries for keyboard enhancement (e.g. crossterm/Codex) detects support. Only the **disambiguate** flag (`0x1`) is honored: the current flag is synced to the client, which then encodes unambiguous `CSI u` key events (Escape, Ctrl/Alt combinations, functional keys, and the keypad's `KP_*` navigation codes) while plain text still flows as text. The other flags (report-event-types `0x2`, report-alternate-keys `0x4`, report-all-keys `0x8`, report-associated-text `0x10`) are masked off; `0x8`/`0x10` are incompatible with the browser's hidden-textarea/IME input model. The query reports only the honored flag, so an app that needs a masked-off one detects the gap and falls back. This is distinct from the kitty **image** protocol, which is unsupported (see the graphics row above).

## Related projects

The web-terminal family builds on this engine:

- [`@cplieger/web-terminal-ui`](https://github.com/cplieger/web-terminal-ui):
  the reference touch-first browser UI for the TypeScript renderer.
- [`web-terminal-server`](https://github.com/cplieger/web-terminal-server): a
  ready-to-run container that bridges a PTY command to the browser over HTTP +
  WebSocket.

Apps built on the engine:

- [`vibekit`](https://github.com/cplieger/vibekit)
- [`web-terminal-kiro`](https://github.com/cplieger/web-terminal-kiro)

## Contributing

Issues and PRs are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
conventions and how to run the checks locally.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0. See [LICENSE](LICENSE).
