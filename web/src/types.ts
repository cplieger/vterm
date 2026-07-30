// Wire types matching the Go vt package's wire format.

/**
 * A single styled run of text within a terminal row. The Go server emits these
 * as flat arrays per row; the renderer reconstructs cells from them.
 */
export interface WireRun {
  /** UTF-8 text content of this run. */
  t: string;
  /** Foreground color (palette index or RGB packed); -1 means default. */
  f?: number;
  /** Background color (palette index or RGB packed); -1 means default. */
  b?: number;
  /** Underline color (palette index or RGB packed); -1 means default. */
  uc?: number;
  /**
   * Style attribute bitflags:
   * 1=bold, 2=italic, 4=underline, 8=inverse, 16=strike, 32=dim, 64=hidden,
   * 128=blink, 256=overline, 512=double-underline.
   */
  a?: number;
  /** OSC 8 hyperlink URI (empty/absent means no link). */
  u?: string;
}

/**
 * Full or incremental screen update from the server. `changed` lists row
 * indices in `rows` that actually changed since the last frame; the renderer
 * only re-paints those rows.
 */
export interface ScreenMessage {
  /** Discriminator — always `"screen"`. */
  type: "screen";
  /** All rows of the visible screen (length = screen height). */
  rows: WireRun[][];
  /**
   * Absolute line index of the top screen row (row 0). A changed row at
   * window index `y` has absolute index `base + y`. The client stores
   * every line by absolute index, so applying a row is idempotent and
   * never duplicates (see the #web-terminal-engine steering doc,
   * "Design rationale").
   */
  base: number;
  /** Cursor position as [row, col], zero-indexed within the window. */
  cursor: [number, number];
  /** Indices into `rows` that changed; an empty array means cursor-only update. */
  changed: number[];
  /**
   * True while the alternate screen is active. Alt-screen content is
   * ephemeral (no history accrual): the client renders it as an overlay
   * and restores the main buffer on exit. `base` is frozen while alt.
   */
  altActive?: boolean;
  /** DECSCUSR cursor style (0-6); 0 = default block. */
  cursorStyle?: number;
  /** True if the cursor is currently hidden (DECTCEM off). */
  cursorHidden?: boolean;
  /** True if the cursor is currently blinking. */
  cursorBlink?: boolean;
  /** True if the server emitted a BEL since the last screen update. */
  bell?: boolean;
  /**
   * True when the app issued ED3 (`CSI 3 J`, "erase saved lines") this frame:
   * the client drops its scrollback history (all lines below `base`), matching
   * a real terminal. Inline TUIs (kiro-cli) emit ED3 on every resize redraw to
   * discard the previous frame; honoring it stops stale frames accumulating.
   */
  scrollbackCleared?: boolean;
  /** Server-confirmed bytesReceived for the input ACK protocol. */
  inputAck?: number;
}

/**
 * A batch of committed history lines, addressed by absolute index. Used both
 * for lines that scrolled off the live window and for resume replay.
 */
export interface ScrollMessage {
  /** Discriminator — always `"scroll"`. */
  type: "scroll";
  /** Absolute line index of `lines[0]`; line `i` has index `firstIndex + i`. */
  firstIndex: number;
  /** Lines in order (oldest to newest), applied by absolute index. */
  lines: WireRun[][];
  /** Server-confirmed bytesReceived for the input ACK protocol. */
  inputAck?: number;
}

/**
 * Server acknowledgement of a client `resume` control message. Carries the
 * input-ack count (to trim the outbox) plus the absolute-index bounds of
 * retained history (to detect an eviction gap on resume).
 */
export interface ResumeAckMessage {
  /** Discriminator — always `"resumeAck"`. */
  type: "resumeAck";
  /** Bytes the server received from this session before the resume. */
  received: number;
  /**
   * Server boot-time nanoseconds since unix epoch. Optional for back-compat
   * with pre-CONN-01 server builds (which omit it).
   */
  serverEpoch?: number;
  /** Absolute index of the next line to commit (one past the newest retained). */
  committed?: number;
  /**
   * Absolute index of the oldest retained line. If this exceeds the client's
   * highest-held index + 1, history between them was evicted: the client
   * shows a "history trimmed" marker rather than misaligning.
   */
  oldestIndex?: number;
  /**
   * The server's wire-protocol revision (third length-gated tail, absent on
   * older servers). A value OUTSIDE the client's supported range means one
   * side runs a stale bundle/binary; the connection module warns and fires
   * `onWireVersionMismatch` so the UI can offer a reload. Versions inside
   * the range are normal pairings (a v3 server just means the socket stays
   * in sentinel mode); a value >= 4 additionally triggers the typed-framing
   * upgrade (docs/wire-v4-typed-framing.md).
   */
  serverWireVersion?: number;
  /**
   * Set when the resume key missed the server's registry while this client
   * claimed sent bytes: the input ledger was reclaimed (idle GC / cap
   * eviction), so the server cannot vouch for any previously sent input.
   * The connection module responds deterministically with its designed loss
   * semantic — drop the outbox rather than replay possibly-duplicate input —
   * and notifies (`onServerRestart`) only when that outbox actually held
   * unacked bytes; a fully-acked session lost nothing and resets silently.
   * Absent on older servers (the `received === 0 && bytesAcked > 0` heuristic
   * remains as their fallback).
   */
  ledgerLost?: boolean;
}

/**
 * Bare input-ack frame. Sent from the server's flush tick when input was
 * applied but no content frame carried the advanced ack within the tick
 * (e.g. typing into a no-echo `read -s`), so outbox trimming never depends
 * on the app producing output. Transport-internal: the connection module
 * consumes it and does not forward it to `onMessage`.
 */
export interface AckOnlyMessage {
  /** Discriminator — always `"ackOnly"`. */
  type: "ackOnly";
  /** Server-confirmed bytesReceived for the input ACK protocol. */
  inputAck: number;
}

/**
 * Snapshot of the server's terminal mode flags. The client updates its input
 * encoding behavior (keyboard, mouse, paste) based on these.
 */
export interface ModesMessage {
  /** Discriminator — always `"modes"`. */
  type: "modes";
  /** DEC 2004: paste content is wrapped with ESC[200~ ... ESC[201~. */
  bracketedPaste: boolean;
  /** DECCKM: cursor keys send ESC O instead of ESC [. */
  applicationCursor: boolean;
  /** DECKPAM: keypad keys send application-mode sequences. */
  applicationKeypad: boolean;
  /** DEC 1006: mouse coordinates are encoded in SGR (CSI <) form. */
  mouseSGR: boolean;
  /** DEC 1004: focus in/out events are reported as ESC[I / ESC[O. */
  focusReporting: boolean;
  /** DEC 5: screen is in reverse-video mode. */
  reverseVideo: boolean;
  /** DEC 1016: mouse reports carry pixel coordinates instead of cell coords. */
  mousePixels: boolean;
  /** Mouse tracking: 0=off, 1000=normal, 1002=button-event, 1003=any-event. */
  mouseMode: number;
  /**
   * Kitty keyboard progressive-enhancement flags (bit0 disambiguate, bit1
   * report-event-types, bit2 report-alternate-keys); 0 means the protocol is
   * disabled and keys use legacy encoding. The keyboard encoder emits kitty
   * CSI-u sequences for the set flags.
   */
  keyboardFlags: number;
  /** Server-confirmed bytesReceived for the input ACK protocol. */
  inputAck?: number;
}

/**
 * Window title set by the server (OSC 0/1/2). The client typically reflects
 * this into `document.title`.
 */
export interface TitleMessage {
  /** Discriminator — always `"title"`. */
  type: "title";
  /** New window title text. */
  title: string;
  /** Server-confirmed bytesReceived for the input ACK protocol. */
  inputAck?: number;
}

/**
 * Clipboard text an app copied via OSC 52 (`OSC 52 ; c ; <base64> ST`). The
 * client writes it to the system clipboard. Set-only: the query form is
 * refused server-side, so this message never carries a read request.
 */
export interface ClipboardMessage {
  /** Discriminator — always `"clipboard"`. */
  type: "clipboard";
  /** Text to place on the system clipboard. */
  text: string;
  /** Server-confirmed bytesReceived for the input ACK protocol. */
  inputAck?: number;
}

/** Discriminated union of all messages the server can send to the client. */
export type ServerMessage =
  | ScreenMessage
  | ScrollMessage
  | ResumeAckMessage
  | ModesMessage
  | TitleMessage
  | ClipboardMessage
  | AckOnlyMessage;

/**
 * Discriminated union of all control messages the client can send to the
 * server (multiplexed alongside raw input bytes; see the wire protocol notes).
 */
export type ControlMessage =
  | { type: "resize"; cols: number; rows: number }
  | {
      type: "resume";
      sessionId: string;
      sentBytes: number;
      haveThrough: number;
      protocolVersion: number;
    }
  | { type: "ping" }
  /**
   * v4 typed-framing transition (docs/wire-v4-typed-framing.md §4): the first
   * TEXT message a client sends after the resumeAck proves the server speaks
   * wire revision >= 4. Receiving it latches the server's connection to typed
   * mode (text = control, binary = full-alphabet PTY input); otherwise a no-op.
   */
  | { type: "upgrade" };
