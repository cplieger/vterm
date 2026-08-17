# @cplieger/web-terminal-engine

[![npm](https://img.shields.io/npm/v/@cplieger/web-terminal-engine)](https://www.npmjs.com/package/@cplieger/web-terminal-engine)
[![JSR](https://jsr.io/badges/@cplieger/web-terminal-engine)](https://jsr.io/@cplieger/web-terminal-engine)

> Browser virtual terminal renderer for the [`cplieger/web-terminal-engine`](https://github.com/cplieger/web-terminal-engine) Go module: DOM-based VT500 screen with OSC 8 hyperlink support, scrollback, keyboard mapper, mouse encoder, and binary wire decoder. Zero runtime dependencies.

The browser half of the web-terminal-engine cross-language terminal library. Pairs with the Go server-side packages (`vt`, `terminal`) over a versioned WebSocket protocol; see the [project README](https://github.com/cplieger/web-terminal-engine#readme) for the full story.

## Install

```sh
npx jsr add @cplieger/web-terminal-engine   # JSR (preferred)
npm i @cplieger/web-terminal-engine          # NPM
```

## Usage

```typescript
import {
  render,
  keyboard,
  mouse,
  scroll,
  modes,
  decodeWireBinary,
} from "@cplieger/web-terminal-engine";

const wrap = document.getElementById("term") as HTMLElement;
const out = document.getElementById("term-output") as HTMLElement;

render.init({ output: out, termWrap: wrap }); // optional: maxLines (retained-line cap, default 5000)
scroll.init({ scrollEl: wrap });
mouse.init({
  send: (data) => ws.send(data),
  cellSize: () => ({ width: cellW, height: cellH }),
  termElement: () => wrap,
});

ws.binaryType = "arraybuffer";
ws.addEventListener("message", (ev) => {
  const msg = decodeWireBinary(ev.data);
  if (!msg) return;
  switch (msg.type) {
    case "screen":
      render.handleScreen(msg);
      break;
    case "scroll":
      render.handleScroll(msg);
      break;
    case "modes":
      modes.setModes(
        msg.bracketedPaste,
        msg.applicationCursor,
        msg.mouseSGR,
        msg.focusReporting,
        msg.mouseMode,
        msg.applicationKeypad,
        msg.reverseVideo,
      );
      break;
    case "title":
      document.title = msg.title;
      break;
  }
});

document.addEventListener("keydown", (ev) => {
  const r = keyboard.mapKeyboardEvent(ev);
  if (r.kind === "send") {
    ws.send(r.bytes);
    ev.preventDefault();
  }
  if (r.kind === "scroll-up" || r.kind === "scroll-down") ev.preventDefault();
});
```

## API

- **`render`** — DOM renderer driven by `ScreenMessage` / `ScrollMessage` frames. `init` (accepts `maxLines`, the retained-line cap — memory-constrained consumers pass a smaller budget; history above the cap is evicted from the top in batches, and the live screen is never evicted, so a cap at or below the terminal height keeps the full screen with no scrollback), `handleScreen`, `handleScroll`, `updateFontMetrics`, `computeSize`, `getCursorPx`, `setPredictedCursor`, `resetScreen`, `resetScrollback`, `getHighestIndex`, `noteResumeBounds`, `updateReverseVideo`.
- **`keyboard`** — Translates `KeyboardEvent` to terminal byte sequences. `mapKeyboardEvent`, `bracketTextForPaste`, `prepareTextForTerminal`, `ctrlByteFor`, plus the shared logical-key encodings (`plainCursorKeySeq`, `plainEscapeSeq`) the toolbar module reuses. Honors `applicationCursor`, `applicationKeypad`, `bracketedPaste`.
- **`toolbar`** — On-screen mobile toolbar widget (moved out of `keyboard` in v3). `bindMobileToolbar({toolbar, send, ids?})` wires `pointerdown` handlers for an on-screen Ctrl/arrows/Tab/Enter/Esc toolbar (with sticky-Ctrl semantics and kitty/DECCKM-aware arrows byte-identical to the physical-key path), returning a `MobileToolbarController` exposing `applyStickyCtrl`, `setCtrlArmed`, `isCtrlArmed`, and `dispose`.
- **`mouse`** — SGR 1006 mouse + focus reporting encoder. `init`, `encodeSGR`, `MouseInputHandler`. Auto-gates on `mouseMode > 0`.
- **`scroll`** — Auto-follow tracker for the scroll container. `init`, `stickToBottom`, `scrollToBottom`, `isUserScrolledUp`.
- **`modes`** — DEC private mode state (synced from server's `ModesMessage`). `setModes`, `isBracketedPaste`, `isApplicationCursor`, `getMouseMode`, `isMouseSGR`, `isFocusReporting`, `isApplicationKeypad`, `isReverseVideo`.
- **`decodeWireBinary(buf)`**: Top-level decoder for the binary WebSocket frames. Returns a `ServerMessage` or `null` for invalid/truncated frames.
- **Wire compatibility metadata**: `WIRE_PROTOCOL_VERSION`, `MIN_SUPPORTED_SERVER_WIRE_VERSION`, `WIRE_INCOMPATIBLE_CLOSE_CODE`, and `WIRE_COMPATIBILITY` publish this client release's directional contract. The same values ship as a language-neutral JSON artifact for non-TypeScript consumers — see [Wire compatibility manifest](#wire-compatibility-manifest).
- **`connection`**: Client → server WebSocket lifecycle: owns the socket, exponential-backoff reconnect, and the resume/inputAck reliability layer (outbox + server-restart detection). `init(callbacks)`, `connect`, `sendBinary(bytes)`, `sendResize`, `reconnectNow`. The callbacks expose `onMessage(ServerMessage)`, `onOpen`/`onClose`/`onConnecting`/`onOutboxFull`/`onServerRestart`, `onWireVersionMismatch`, and the definitive `onWireIncompatible`; a `computeSize()` provider; and an optional `wsPath` (defaults to `"/ws"`). An explicit below-floor server revision or close code 4002 stops automatic reconnects until `disconnect()` or a page reload. Version-silent and future-revision servers remain tolerated. The module decodes frames internally and applies `modes.setModes`, so a consumer only needs to dispatch screen/scroll to `render`. Prefer this over wiring `WebSocket` + `decodeWireBinary` by hand unless you need full control.
- **`controlFrame(msg)` / `wsURL(proto, host, path?)`** — Low-level helpers for the client → server protocol (0x00-prefixed JSON control frames, WebSocket URL building). Used internally by `connection`; exported for advanced consumers.

Wire types (`WireRun`, `ScreenMessage`, `ScrollMessage`, `ModesMessage`, `TitleMessage`, `ResumeAckMessage`, `ServerMessage`, `ControlMessage`) are re-exported from the package root and match the Go server's wire format byte-for-byte.

## Wire compatibility manifest

The same compatibility numbers are also published as a language-neutral JSON artifact, so a consumer that cannot import TypeScript — a Dockerfile, a shell release gate, a CI script in any language — can read them without scraping source:

```sh
# npm consumer
jq -r .wireCompatibility.protocolVersion \
  node_modules/@cplieger/web-terminal-engine/wire-compatibility.json

# JSR consumer (published as an included file; JSR exports are modules only)
curl -fsSL https://jsr.io/@cplieger/web-terminal-engine/<version>/wire-compatibility.json
```

```json
{
  "schemaVersion": 1,
  "generatedBy": "web/src/test-helpers/wire-manifest.ts",
  "wireCompatibility": {
    "protocolVersion": 4,
    "minimumServerProtocolVersion": 3,
    "incompatibleCloseCode": 4002
  }
}
```

It is generated from `WIRE_COMPATIBILITY` (there is no second copy of the numbers), checked into the repo because the publish pipeline runs no build step, and guarded by a regenerate-and-diff test so it cannot go stale. Its values are pinned to both the TypeScript constants and the Go `terminal` constants, so the three surfaces cannot diverge.

**What you may rely on** (semver-governed public artifact, versioned by the package version):

- The file is present at the package root of every npm tarball and JSR publish, at path `wire-compatibility.json`, and is importable from npm as `@cplieger/web-terminal-engine/wire-compatibility.json`.
- `schemaVersion` is an integer describing this file's LAYOUT, not the wire protocol. Read it first and reject a value you do not understand.
- Within a `schemaVersion`, the fields under `wireCompatibility` keep their names, types and meanings, and equal the correspondingly named `WIRE_COMPATIBILITY` members of the same release.
- New fields may be added under either object in a MINOR release, so parse permissively (ignore unknown keys).

**What is not part of the contract**: `generatedBy` is an informational provenance note. The manifest deliberately carries no package version — the publish step injects that into `package.json` / `jsr.json` after checkout, so a version here would ship frozen at the repo placeholder; read the package version from those files.

**Breaking changes to the manifest** (MAJOR, and always release-noted): removing or renaming a field, changing a field's type or meaning, moving the file, or bumping `schemaVersion`. A change to the wire revision NUMBERS is not a manifest break — reporting them is what the file is for.

## Browser-only

This package depends on `document`, `HTMLElement`, `MessageChannel`, and other DOM APIs, so it only runs in browser-like environments. The companion Go server runs anywhere Go does.

## License

MPL-2.0. See [LICENSE](LICENSE).
