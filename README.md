# web-terminal-engine

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/web-terminal-engine/v5.svg)](https://pkg.go.dev/github.com/cplieger/web-terminal-engine/v5)
[![npm](https://img.shields.io/npm/v/@cplieger/web-terminal-engine)](https://www.npmjs.com/package/@cplieger/web-terminal-engine)
[![JSR](https://jsr.io/badges/@cplieger/web-terminal-engine)](https://jsr.io/@cplieger/web-terminal-engine)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/web-terminal-engine/badges/coverage.json)](https://github.com/cplieger/web-terminal-engine/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/web-terminal-engine/badges/mutation.json)](https://github.com/cplieger/web-terminal-engine/issues?q=label%3Agremlins-tracker)
[![Mutation (TS)](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/web-terminal-engine/badges/mutation-ts.json)](https://github.com/cplieger/web-terminal-engine/issues?q=label%3Astryker-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13225/badge)](https://www.bestpractices.dev/projects/13225)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/web-terminal-engine/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/web-terminal-engine)

> Cross-language terminal emulator and session engine (Go) with browser renderer (TypeScript).

A standalone library that bridges a PTY to a browser WebSocket. The Go packages provide a VT100/VT500 screen buffer with SGR support and a WebSocket-based terminal session handler with reconnect, scrollback replay, and adaptive ping. The TypeScript package provides the browser-side renderer, keyboard mapper, mouse encoder, and binary wire decoder. No app-specific dependencies; only the standard library, `github.com/coder/websocket`, `github.com/creack/pty`, `golang.org/x/sys`, and `github.com/cplieger/runesafe/v2`.

## Install

- Go: `go get github.com/cplieger/web-terminal-engine/v5@latest`
- TS: `npx jsr add @cplieger/web-terminal-engine` or `npm i @cplieger/web-terminal-engine`

## Usage

```go
import (
    "log/slog"
    "net/http"

    "github.com/cplieger/web-terminal-engine/v5/terminal"
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
- **`terminal`**: WebSocket session handler. `NewHandler(command, ...Option)`, `RegisterRoutes(mux)`, `ServeHTTP(w, r)`, and the shutdown pair `Close()` / `Shutdown(ctx) error` — `Close` ends the session and returns at once, leaving the cgroup teardown, the `/proc` sweep and the client notification in flight, while `Shutdown` does the same and then waits for all of it, returning `ctx.Err()` if the budget expires first. Use `Close` from a request handler or a timer, and `Shutdown` when the process is about to stop, because a process that exits underneath an unfinished teardown loses it. `SessionManager` exposes only the blocking form (`Shutdown(ctx) error`), which signals every session before waiting on any of them so their teardown windows overlap rather than sum, and reports how many were still unfinished when a budget expired; a manager is single-use, since `Shutdown` cancels the status sweep and the idle reaper and restarts neither. Options `WithWorkDir`, `WithLogger`, `WithEnv`, `WithScrollbackCapacity`, `WithOriginPolicy`, `WithOnProcessExit`, `WithKeepUnfocused`, `WithTheme`, `WithMinimumContrast` (a WCAG contrast floor between a run's text and its background, clamped 1..21 and off at 1 by default; see [Colors](#colors)), `WithCommandLogValue` (the process-start line's `command` attribute — the engine's only argv-bearing log site — records the given fixed marker, e.g. `"[redacted]"`, instead of the child argv, for consumers whose argv embeds operator-supplied values that could carry a credential; empty is ignored, keeping the default argv logging). Two release/logging helpers round out the surface: `LogID(id)` truncates a session id to a correlation-safe prefix plus an ellipsis — the first 8 bytes, cut back to a rune boundary, since a client-supplied resume id is arbitrary bytes and a blind byte prefix would put invalid UTF-8 into the log stream — the session id is a WS resume capability token, so every consumer that logs one must pass it through here rather than re-deriving the truncation (CWE-532); for the same reason the engine sets `Cache-Control: no-store` across the whole session surface: the two session JSON bodies and the SSE stream carry an id in full, and the REST surface states the policy on _every_ response it produces — including the ones no handler writes, its mux's 404, 405 and path-cleaning redirect, whose URLs carry the id as a path segment even though their bodies do not. A consumer inherits the cache half of that protection instead of bolting on its own middleware; a `Cache-Control` a consumer's own outer middleware already set is preserved, which is both the way to state a stricter policy and the escape hatch for deliberately caching one of these routes (the two JSON bodies are the exception and always `no-store`, since the id is in the body). `WirePairIncompatibility(WirePair{Server, Client})` is the peer-less build-time form of the wire-compatibility decision the runtime already makes twice, so a consumer's release gate can refuse a mismatched Go/TS pair before the image ships instead of discovering it as a close-4002 at first connect (each `WireEnd` labels one half's revision and minimum-peer floor, and `WirePair` labels which half is which; returns "" when compatible, else a reason; both cross-side floors exclusive; a non-positive input is a caller error rather than runtime's tolerated version-silent 0; and a half whose own minimum-peer floor exceeds its own revision is reported as self-inconsistent — corrupt or mis-extracted input — BEFORE any cross-side skew verdict, so a garbage pair never yields a confident "bump your pin" diagnosis). `(*Handler).ScrollbackBounds()` returns the session's retained-history bounds as an atomic pair — the committed index (one past the newest committed line) and the oldest still-replayable index — so a consumer can observe which absolute indices a resume can still ask for without decoding a wire frame; read-only, since bounds follow the child's output and the configured capacity. Handles PTY lifecycle, binary wire protocol, reconnect with scrollback replay, adaptive ping. `SessionManager` (`NewSessionManager`) fronts N PTY-backed sessions with `WebSocketHandler()` (`/ws?session=<id>`), `RESTHandler()` (`/api/sessions`, plus `PUT`/`DELETE /api/sessions/{id}/pinned-title` to set and clear a session's user-chosen name), and `EventsHandler()` (SSE `/api/sessions/events`); `PUT /api/sessions/order` sets the display order every viewer of a server shares, by sending every live session id in the wanted order (refused with 409 unless the list names the live set exactly, which is what makes the write atomic and a stale caller's view detectable); each session's position comes back as `order` on both the list and the status stream, so a reorder made in one browser moves the tab in every other one. Both enumerations are served in that shared order, then oldest `createdAt`, then session id, so a client that builds a tab strip from whichever one reaches it first gets the same strip every time; status values working/idle/failed/warning/exited/crashed are derived server-side (working/failed/warning from the program's own OSC 9;4 progress report, read with iTerm2's state semantics; `exited` vs `crashed` splits an ordinary session end — status 0, or any exit the server itself caused, such as a closed session, the idle reaper or a shutdown — from a non-zero or signalled one, so a routine restart is never reported as a failure, and `(*Handler).ExitError()` exposes the retained `cmd.Wait()` error behind that decision), while input/done are latched from an OSC 9 notification through a pluggable classifier, and a latch is superseded by any later progress state that CONTRADICTS it — the active states 1/3 and the error state 2, but not the paused state 4, which asserts "stopped, resumable by the user" and so agrees with a needs-input latch rather than rivalling it; each status event also carries the OSC 9;4 percentage as `progressValue` (-1 when the program reported none, which is not 0%) and delivers a fresh OSC 9 notification as `notification` + `notificationSeq`, so a consumer with no classifier installed still receives the message instead of it being dropped. Each session's `title` is RESOLVED server-side from four inputs in precedence order — the user's pinned name, the window title the program set via OSC 0/2, a client-derived automatic title a client asked the server to remember (`PUT /api/sessions/{id}/title`, e.g. the first line the user submitted), and the foreground process or working directory — so every attached client shows the same label without re-deriving the ladder, and `pinnedTitle` travels alongside it so a client can tell a chosen name from an inferred one. When several clients share one session, a live resize is last-writer-wins and the shared screen relaxes to the smallest remaining client's size on disconnect. `MountSessionRoutes(mux, SessionHandlers{WS, REST, Events}, ...MountOption)` mounts exactly the route constants `WSPath`, `SessionsPath` + `SessionsSubtreePath` (the REST handler needs both mounts), and `SessionEventsPath`; the paths mirror the TS client's exported defaults, and additions are release-noted. `WithCreateGate(mw)` wraps the REST handler with caller-supplied middleware to rate-limit session creation (each POST forks a process); the mount states the `no-store` policy outside that gate, so a throttle refusal on a token-bearing path carries it too. `(*SessionManager).MountAPI(mux, opts...)` is the one-manager convenience.

  Spawned processes inherit the server's environment plus a default terminal identity: `TERM=xterm-256color`, `COLORTERM=truecolor`, `TERM_PROGRAM=iTerm.app`, and `TERM_PROGRAM_VERSION=3.6.6`, so apps detect truecolor, OSC 9;4 progress reporting, and DEC 2026 synchronized output. `WithEnv` values are appended after these defaults, so a consumer entry for the same variable overrides them.

  **Session reaping is on by default**, and it is the reason a closed tab does not leak its process tree. A PTY child that calls `setsid()` leaves both its process group and its session, so neither `kill(-pgid)` nor the PTY-close `SIGHUP` can reach it, and a process re-parented to init has no ancestry left to walk; agent runtimes do exactly this and some install no stdin-EOF exit path at all, so they outlive their session indefinitely holding hundreds of megabytes. The engine therefore spawns every session with one unguessable environment marker, which `execve` copies into every descendant and which survives both `setsid()` and re-parenting, and at session end it reclaims whatever still carries that marker: settle, `SIGTERM`, settle, `SIGKILL`, logging a single `session reap reclaimed escaped processes` line with `survivors`, `term_reclaimed`, `kill_forced` and `resident_bytes` only when something actually had to be reclaimed. This needs no capability, no cgroup, no mount and no PID namespace — measured reclaiming a `setsid()` escapee in 354ms inside an unprivileged container, where a full scan of 17,547 pids costs ~81ms. Two limits: a descendant that `execve`s with a deliberately scrubbed environment escapes the domain, and because the scan enumerates, a tree that forks during teardown can outrun one pass (each escalation rescans rather than reusing the first pass's pid set). Reaping is unconditional. `Containment` remains the stronger, opt-in boundary that nothing can escape, and it is now the only thing a cgroup buys: per-session `memory.peak`/`pids.peak`, plus a kill domain immune to both limits above.

  One property this depends on, stated because a consumer can observe it: the marker is the **only** assignment to its key in the child environment. `os/exec` keeps the LAST value for a repeated key, so the engine strips that key from both environments it composes — your `WithEnv`, and the server's own inherited environment — before prepending the value it minted. Setting `WT_SESSION_REAP` yourself therefore does nothing rather than silently switching reaping off, and a server launched from inside one of these sessions does not hand its parent session's marker to every session it spawns.

  **`StartZombieReaper(log, interval)`** is the separate, opt-in answer to a separate problem. Session reaping ends processes that are still ALIVE; this collects exit statuses nobody called `wait()` for. A server running as its container's PID 1 inherits every orphan in the container by re-parenting, and Go's `os/exec` waits only on the children it created — so every language server and every `git` a session forked becomes a permanent zombie parked on the server (measured: 17,323 zombies against 88 live processes). Wire it from the composition root of a server that is, or may become, PID 1; it installs `PR_SET_CHILD_SUBREAPER` so orphans arrive even behind an init shim, then sweeps every `interval` (0 = 30s, floored at 1s) and returns a stop function. It is deliberately a periodic sweep rather than a `SIGCHLD` handler, because `signal.Notify` is process-global state and `SIGCHLD` is what the Go runtime itself uses to drive `os/exec`. It never waits on a pid the engine spawned: the registry that decides is written under a lock the spawn path holds across the fork, so a generic `wait(-1)` can never steal the head's status and turn a clean exit into an unknown one.

### TypeScript (`web/`, published as `@cplieger/web-terminal-engine` on NPM and JSR)

- **`render`**: DOM renderer driven by `ScreenMessage` / `ScrollMessage` frames. `init`, `handleScreen`, `handleScroll`, `updateFontMetrics`, `computeSize`, `getCursorPx`, `setPredictedCursor`, `resetScreen`, `resetScrollback`, `getHighestIndex`, `noteResumeBounds`, `updateReverseVideo`.
- **`keyboard`**: Translates `KeyboardEvent` to terminal byte sequences. `mapKeyboardEvent`, `bracketTextForPaste`, `prepareTextForTerminal`. Honors `applicationCursor`, `applicationKeypad`, `bracketedPaste`, and the kitty keyboard **disambiguate** flag (emits `CSI u` key events when the app enables the protocol).
- **`toolbar`**: On-screen mobile keyboard toolbar wiring touch buttons to a send sink, with sticky-Ctrl state and ARIA painting. `bindMobileToolbar(opts)` returns a `MobileToolbarController`; `DEFAULT_TOOLBAR_IDS` names the default button ids. Reuses the `keyboard` encodings, so toolbar keys match physical keys byte for byte.
- **`mouse`**: SGR 1006 mouse + focus reporting encoder. `init`, `encodeSGR`, `MouseInputHandler`.
- **`scroll`**: Auto-follow tracker for the scroll container. `init`, `stickToBottom`, `scrollToBottom`, `isUserScrolledUp`.
- **`modes`**: DEC private mode state (synced from server's `ModesMessage`). `setModes`, `isBracketedPaste`, `isApplicationCursor`, `getMouseMode`, `isMouseSGR`, `isFocusReporting`, `isApplicationKeypad`, `isReverseVideo`, `getKeyboardFlags`.
- **`decodeWireBinary(buf)`**: Top-level decoder for binary WebSocket frames; returns a `ServerMessage` or `null` for invalid/truncated frames.
- **Wire compatibility metadata**: `WIRE_PROTOCOL_VERSION`, `MIN_SUPPORTED_SERVER_WIRE_VERSION`, `WIRE_INCOMPATIBLE_CLOSE_CODE`, and `WIRE_COMPATIBILITY` describe the TypeScript release's directional contract. The Go `terminal` package exposes the complementary `WireProtocolVersion`, `MinSupportedClientWireVersion`, and `WireIncompatibleCloseCode` constants. The same values also ship as a generated, language-neutral JSON artifact at the package root of every npm and JSR release (`wire-compatibility.json`, importable from npm as `@cplieger/web-terminal-engine/wire-compatibility.json`), so a Dockerfile or shell release gate can read them with `jq` instead of scraping TypeScript source; it is generated from `WIRE_COMPATIBILITY`, carries a `schemaVersion` a consumer must check, and is guarded by a regenerate-and-diff test plus a conformance test pinning it to both the TypeScript and the Go constants. Its consumer contract and what counts as a breaking change to it are documented in [web/README.md](web/README.md#wire-compatibility-manifest).
- **`connection`**: Client → server WebSocket lifecycle, including socket ownership, exponential-backoff reconnect, and the resume/inputAck reliability layer (outbox + server-restart detection). Public methods are `init(callbacks)`, `connect`, `sendBinary`, `sendResize`, `reconnectNow`, and `requestHistory(fromAbs, maxLines)`; the `wsPath` callback option defaults to `"/ws"`. The module decodes frames and applies `modes.setModes` internally, so consumers only dispatch screen/scroll messages to `render`. It pairs with the Go `terminal` handler's resume protocol. `controlFrame` and `wsURL` are also exported for advanced use.
- **Demand-paged scrollback**: a deep server ring stays reachable without the browser holding it. The server declares the capability in its resumeAck, bounds every reconnect's replay, and serves `history` requests for ranges the client asks for; the client keeps a small resident tail plus a disposable cache of the pages a reader actually visited. **It is off until a consumer wires it**, and it fails silently rather than loudly if half-wired, so the whole seam is worth doing at once: pass `requestHistory` and `historyBudget` to `render.init`, pass `onScrollPosition: render.handleScrollPosition` to `scroll.init` (the renderer's whole scroll-position contract in one hook: it re-evaluates the fetch trigger, whose only signal this is — `onUserScrollChange` fires on a follow/hold TOGGLE, so browsing an idle session would never fetch — and it finishes a drain that stopped with rows queued), and give `connection.init` the six store-and-viewport callbacks it declares (`getReplayMax`, `onHistoryReply`, `onResumeTransition`, `noteSolicited`, `clearSolicited`, `onHistoryRetry`). The consumer also owns the cache's inactivity TTL, because the engine has no notion of a page or a tab (`render.browseCacheSize`, `lastBrowseActivityMs`, `dropBrowseCache`). `@cplieger/web-terminal-ui` wires all of it; read `kernel.ts` as the reference.
- **Scrollback persistence**: `LineStore.snapshot(serverEpoch, maxLines?)` returns the newest retained lines as plain, `structuredClone`-safe data (or `null` for an empty store, so a caller cannot overwrite a good snapshot with an empty one), and the static `LineStore.fromSnapshot(snap, maxLines?)` rehydrates one — returning `null`, never throwing and never half-restoring, because every failure has the same correct handling: start empty and take a full resume. It exists so a page the browser DISCARDED can resume with a delta instead of refilling its whole buffer over the wire, which is the normal case on iOS rather than an edge case. Storage is the consumer's; the engine only supplies the data (`@cplieger/web-terminal-ui` wires the lifecycle and the storage seam). The `serverEpoch` argument is required rather than optional and is the part to get right: absolute line indices only mean anything within one server process, so a restored store must be checked against the live server or it will present stale content as live AND then have the new session's low-index output refused by its own staleness guard. Read the epoch with `connection.serverEpochOf(sessionId)` when saving, and seed it back with `connection.adoptPersistedEpoch(sessionId, epoch)` BEFORE connecting, which routes a mismatch into the existing `onServerRestart` path. `connection.currentSessionId()` resolves the unmanaged single-terminal id (per-tab, `sessionStorage`-backed) for a consumer that needs a key for it.
- **`connectStatusStream`** and the `SessionInfo` / `SessionStatus` / `StatusStream` types: The SSE client for `/api/sessions/events` that drives per-tab status. `SessionInfo` carries the resolved `title`, the user's `pinnedTitle`, and `order` — the session's position in the display order the server owns, which is what a consumer sorts its tabs by (absent from a pre-3.10.0 server, and from a status event marking a session removed). `LineStore` and `CONTROL_FRAME_PREFIX` are also exported, as are the `WS_PATH` / `SESSIONS_PATH` / `SESSION_EVENTS_PATH` route constants mirroring the Go `terminal` package's mount contract.
- **Wire types**: `WireRun`, `ScreenMessage`, `ScrollMessage`, `ModesMessage`, `TitleMessage`, `ResumeAckMessage`, `ServerMessage`, and `ControlMessage` are re-exported from the package root.

## Cross-origin access

A terminal socket is an interactive shell, so who may open one matters. The
engine allows **same-origin only** by default, and that default is load-bearing:
a WebSocket handshake is a `GET`, and `net/http.CrossOriginProtection` — the
stdlib CSRF middleware a consumer is likely to be running — returns early for
`GET`, `HEAD` and `OPTIONS` as safe methods. It therefore never inspects the
upgrade. An app-level cross-origin middleware does **not** protect this socket;
the engine's own check is the gate.

To embed a terminal in a page served from another origin, build a policy and pass
it to both the handler and the manager:

```go
policy, invalid := terminal.NewOriginPolicy("https://embed.example.com")
if len(invalid) > 0 {
    log.Printf("ignoring malformed allowed origins: %v", invalid)
}

h := terminal.NewHandler(cmd, terminal.WithOriginPolicy(policy))
mgr := terminal.NewSessionManager(factory, terminal.WithManagerOriginPolicy(policy))
```

Each entry is a complete origin — scheme, host, optional port, nothing else.
Matching is exact and case-insensitive, with the scheme's default port dropped
the way a browser's own `Origin` serialization drops it. A malformed entry is
reported rather than stored, so a typo cannot silently widen or silently narrow
the policy; an all-invalid list leaves same-origin only. There are deliberately
no wildcards, no `Origin: null`, and no way to disable the check: a pattern
language buys a subdomain tree and costs the `*` that means allow-everything, and
`null` is what every sandboxed iframe and `file://` page sends.

`(*OriginPolicy).Allows(r)` is exported so a consumer can apply the same decision
to its own routes, and `Active()` reports whether anything beyond same-origin is
permitted, for a startup log line.

## Retained history

Each session keeps the lines that scroll off its screen in an in-memory ring,
addressed by absolute line index. That depth is how far back a user can scroll
and what a reconnect can replay.

`WithScrollbackCapacity(n)` sets it; the default is
`terminal.DefaultScrollbackCapacity` (100000 lines). What that costs depends on
what the session printed: measured on this ring, a full 100000 lines is about
7 MB of short lines, 21 MB of ordinary 80-column lines, and 64 MB of dense
200-column styled output. The buffer GROWS as history is produced rather than
being allocated at the ceiling, so a large capacity costs a short-lived session
nothing. That is also why there is no "unlimited" sentinel: a number larger than
any session will reach IS unlimited. `0` disables retention entirely (the live screen still works;
nothing survives scrolling off).

Consumers that expose the depth to an operator should use the engine's own name
for the variable rather than inventing one, so that the apps sharing this handler
cannot drift apart:

| Symbol | Purpose |
| --- | --- |
| `ScrollbackEnvVar` | `SCROLLBACK` — the variable name to read. The engine does NOT read it itself; the consumer does, and passes the option. |
| `DefaultScrollbackCapacity` | The default, exported so a consumer can report the effective depth at startup without hardcoding it. |
| `MinPagingCapacity` | The depth at or above which the handler declares demand-paged scrollback to the client. |
| `ClampScrollbackCapacity(n)` | Turns an operator's number into the one to configure, plus a reason string when it had to change it. |

`ClampScrollbackCapacity` exists because one range behaves opposite to intent: a
depth between 1 and `MinPagingCapacity` is retained happily by the ring but is too
shallow for the server to offer paged history, and a paging-capable client then
falls back to holding its entire legacy buffer resident. Lowering the server
number to save memory therefore spends more of it on the client, so that range is
raised and explained rather than obeyed silently. `0` is passed through: a client
cannot page against a server holding no history at all, so the inversion cannot
arise there.

## Environment variable names for consumers

The engine reads no environment itself — a consumer parses its own configuration
and passes options in. But the apps built on this engine are operated by the same
people, so the ones that answer the same question use the same NAME, and that
convention is documented here rather than in each app's README.

The names carry no component prefix. An operator setting one knows the app they
run, not the library serving its HTTP, so a `WT_` prefix named an internal
component at them and bought no disambiguation; bare names also land on the
convention the other cplieger repos already use. `SCROLLBACK` is the engine's own
(`ScrollbackEnvVar` above), and the rest are the names the first-party consumers
settled on: web-terminal-server and web-terminal-kiro both read them, so an
operator who runs one already knows the other, and a new consumer that adopts
them inherits that.

| Variable | What the consumer does with it |
| --- | --- |
| `ADDR` | The listen address, `host:port`. |
| `WORKDIR` | The working directory for the process the PTY runs. |
| `SCROLLBACK` | Retained history depth — the engine's own variable; see above. |
| `ALLOWED_HOSTS` | Exact `Host` values to serve, as an anti-DNS-rebinding gate. Unset is permissive. |
| `TRUSTED_PROXIES` | CIDRs or IPs whose forwarded-for header may name the real client. |
| `LOG_LEVEL` | Log level at boot. |

The keys the engine INJECTS into a session's child environment are the opposite
case and keep the `WT_` prefix: `WT_SESSION_REAP` (above) is not a knob an
operator sets, it lands in one flat namespace beside everything else the system
and the user's shell set, so there the prefix is real disambiguation.

Two rules that make the set worth having. A name is shared only when the BEHAVIOUR
is the same — a knob one app reads and another ignores is not shared, it is a
coincidence, so give an app-specific knob an app-specific name. And nothing here
is read by the engine, so adopting a name is a consumer decision; this table is a
convention, not an interface, and there is no compatibility shim if a consumer
picks differently.

## Colors

The 256-color cube and the grayscale ramp follow the xterm formula, and truecolor
is the RGB triple the program sent. The **16 base colors** (SGR 30-37 / 90-97, and
40-47 / 100-107 as background) are different: no spec assigns them RGB, so every
terminal picks. This engine's choice is
[kitty's published default palette](https://github.com/kovidgoyal/kitty/blob/master/kitty/options/definition.py),
because kitty's own default background is pure black, which is what the reference
UI renders on. Slots resolve **server-side**, so a run reaches the browser as a
resolved `0xRRGGBB`; a program overrides any slot at runtime with OSC 4.

That resolution is also why the terminal has to own legibility. A program selects a
palette SLOT and cannot know what RGB you resolved it to, so it cannot tell whether
its choice is readable on your background:

```go
h := terminal.NewHandler(cmd, terminal.WithMinimumContrast(4.5))
```

A foreground below the floor is blended toward white or black until it clears the
ratio against that run's own background. Same idea as xterm.js's
`minimumContrastRatio` (VS Code defaults it to 4.5, the WCAG AA floor for body
text) and iTerm2's Minimum Contrast. Off by default, so a version bump never
recolors an existing consumer. Four things it never does: change a background,
change a default foreground (your CSS owns that, and swaps it under DECSCNM),
reveal concealed text (SGR 8), or alter what an OSC 4 query reports — a program
must read back the entry it set, not the value the renderer painted over it.

Pass a floor **in addition to** a palette suited to your background, not instead of
one: the palette keeps the lift rare, and the floor covers what a palette cannot —
a program's own OSC 4 overrides, the dark corners of the 256-color cube, and
truecolor a program picked blind.

## Wire Protocol

The Go server and TypeScript client communicate over WebSocket frames rather than shared code. The authoritative byte-level definition is the code itself: the Go encoder (`terminal/wire_binary.go`), the Go `WireRun` types (`vt/wire.go`), and the TS decoder (`web/src/wire-binary.ts`). Round-trip fuzz tests and the `wire-golden/*.bin` fixtures keep those implementations aligned.

- **Binary and typed framing.** Server → client frames are binary messages with little-endian integers. Since wire v4, client → server control messages (resize, resume, ping, upgrade, and the `history` page request) are text frames containing bare JSON, while binary frames carry raw terminal input with the full byte alphabet. A control sent in the wrong encoding is not a control: past the upgrade the server reads a binary frame as terminal input, so it lands in the program's stdin. Every socket first sends a v3-compatible `0x00`-prefixed binary resume and upgrades only after the resumeAck proves a v4 server.
- **Absolute line indexing.** Every line receives a monotonic absolute index. Applying the same line is idempotent, resume aligns by index, eviction gaps are detectable, and a server epoch identifies restarts across reconnects.
- **Compatibility revisions and directional floors.** The Go and TypeScript artifacts can be upgraded independently; package-version equality is not required. Each side exports its current wire revision and its receiver floor (the constants listed in [API](#api)). Both sides currently emit revision 4 and accept declared peers from revision 3. Version-silent peers remain supported. A declared revision below the receiver's floor is refused with close code 4002; a higher revision warns but continues because it may retain the compatible baseline. TypeScript consumers can surface these outcomes with `onWireVersionMismatch` and `onWireIncompatible`. A definitive incompatibility blocks automatic reconnects until `disconnect()` clears the terminal state, normally after the stale half is updated and the page reloads. Frozen previous-revision fixtures and the previous published decoder test both compatibility directions.

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

MPL-2.0. See [LICENSE](LICENSE).
