// Binary wire decoder. Mirrors wire_binary.go on the server.
//
// All multi-byte integers are little-endian. See the Go terminal package's
// wire_binary.go for the exact frame layout.

import type {
  WireRun,
  ScreenMessage,
  ScrollMessage,
  ResumeAckMessage,
  ModesMessage,
  TitleMessage,
  ClipboardMessage,
  ServerMessage,
} from "./types.js";

// Wire message type tags (mirrored from Go wire_binary.go constants).
const MSG_SCREEN = 0;
const MSG_SCROLL = 1;
const MSG_RESUME_ACK = 2;
const MSG_MODES = 3;
const MSG_TITLE = 4;
const MSG_PONG = 5;
const MSG_CLIPBOARD = 6;
const MSG_ACK_ONLY = 7;

const MODE_FLAG_BRACKETED_PASTE = 1;
const MODE_FLAG_APP_CURSOR_KEYS = 2;
const MODE_FLAG_MOUSE_SGR = 4;
const MODE_FLAG_FOCUS_REPORTING = 8;
const MODE_FLAG_APP_KEYPAD = 16;
const MODE_FLAG_REVERSE_VIDEO = 32;
const MODE_FLAG_MOUSE_PIXELS = 64;

// Shared across all one-shot decode() calls (no {stream:true} is used here), so
// a single module-level instance is safe and avoids allocating a fresh
// TextDecoder per styled run / OSC-8 URI on the hot decode path (a single
// scroll frame can carry thousands of runs).
const textDecoder = new TextDecoder();

class Cursor {
  view: DataView;
  bytes: Uint8Array;
  off = 0;
  constructor(buf: ArrayBuffer) {
    this.view = new DataView(buf);
    this.bytes = new Uint8Array(buf);
  }
  u8(): number {
    const v = this.view.getUint8(this.off);
    this.off += 1;
    return v;
  }
  u16(): number {
    const v = this.view.getUint16(this.off, true);
    this.off += 2;
    return v;
  }
  i32(): number {
    const v = this.view.getInt32(this.off, true);
    this.off += 4;
    return v;
  }
  u64(): number {
    // Number(v) loses precision above 2^53. That is safe for every u64 this
    // protocol decodes because of how each value is USED, not because they all fit:
    //   - base / firstIndex / committed / oldestIndex / inputAck (bytesReceived)
    //     stay well under 2^53 for any realistic session, so arithmetic is exact.
    //   - serverEpoch is the server's time.Now().UnixNano() (~1.77e18, far ABOVE
    //     2^53), so it DOES drop low bits here. That is harmless: it is only ever
    //     equality-compared to detect a restart, and Number() is deterministic, so
    //     one epoch always maps to one double and two distinct boots differ by far
    //     more than the ~256ns rounding granularity at that magnitude.
    // If a future u64 field needs exact values above 2^53, return BigInt instead.
    const v = this.view.getBigUint64(this.off, true);
    this.off += 8;
    return Number(v);
  }
  utf8(len: number): string {
    const slice = this.bytes.subarray(this.off, this.off + len);
    this.off += len;
    return textDecoder.decode(slice);
  }
}

function readRowRuns(c: Cursor): WireRun[] {
  const numRuns = c.u16();
  // eslint-disable-next-line @typescript-eslint/no-unsafe-assignment -- pre-allocated array filled immediately below
  const runs: WireRun[] = new Array(numRuns);
  for (let i = 0; i < numRuns; i++) {
    const tlen = c.u16();
    const t = c.utf8(tlen);
    const f = c.i32();
    const b = c.i32();
    const a = c.u16();
    const uc = c.i32();
    const ulen = c.u16();
    const u = ulen > 0 ? c.utf8(ulen) : undefined;
    runs[i] = u ? { t, f, b, a, uc, u } : { t, f, b, a, uc };
  }
  return runs;
}

/**
 * Decode a binary frame from the server's WebSocket into a `ServerMessage`.
 * Returns `null` for frames smaller than the minimum header (9 bytes) or for
 * truncated/invalid frames; the caller should drop such frames silently.
 *
 * Frame format mirrors the Go `vt` package's wire encoding:
 *   - 1 byte type tag
 *   - 8 bytes inputAck (u64 little-endian)
 *   - type-specific body (rows of styled runs for screen/scroll, etc.)
 *
 * @param buf  Raw binary frame received over WebSocket.
 * @returns    Decoded message, or `null` if the frame couldn't be parsed.
 */
export function decodeWireBinary(buf: ArrayBuffer): ServerMessage | null {
  if (buf.byteLength < 9) {
    console.warn("vterm: dropped undersized wire frame", buf.byteLength);
    return null;
  }
  try {
    return decodeWireBinaryInner(buf);
  } catch (err) {
    // Graceful frame drop: a RangeError means the frame was truncated
    // or malformed (e.g. network split mid-frame). Returning null lets
    // the caller skip this frame rather than crashing the decode loop.
    // The next flush will carry a complete snapshot anyway.
    if (err instanceof RangeError) {
      console.warn("vterm: dropped malformed/truncated wire frame", buf.byteLength);
      return null;
    }
    throw err;
  }
}

function decodeWireBinaryInner(buf: ArrayBuffer): ServerMessage | null {
  const c = new Cursor(buf);
  const msgType = c.u8();
  const inputAck = c.u64();

  if (msgType === MSG_RESUME_ACK) {
    // Optional tails, length-gated for back-compat with older servers:
    //   >= 17 bytes: + serverEpoch (restart detection)
    //   >= 33 bytes: + committed + oldestIndex (resume gap detection)
    //   >= 35 bytes: + serverWireVersion + ackFlags (bit0 = ledgerLost,
    //                  bit1 = historyPaging). Unknown bits are IGNORED, which
    //                  is what lets the server add capabilities here without a
    //                  protocol bump.
    let serverEpoch: number | undefined;
    let committed: number | undefined;
    let oldestIndex: number | undefined;
    let serverWireVersion: number | undefined;
    let ledgerLost: boolean | undefined;
    let historyPaging: boolean | undefined;
    if (buf.byteLength >= 17) {
      serverEpoch = c.u64();
    }
    if (buf.byteLength >= 33) {
      committed = c.u64();
      oldestIndex = c.u64();
    }
    if (buf.byteLength >= 35) {
      serverWireVersion = c.u8();
      const ackFlags = c.u8();
      ledgerLost = (ackFlags & 1) !== 0;
      historyPaging = (ackFlags & 2) !== 0;
    }
    const msg: ResumeAckMessage = { type: "resumeAck", received: inputAck };
    if (serverEpoch !== undefined) {
      msg.serverEpoch = serverEpoch;
    }
    if (committed !== undefined) {
      msg.committed = committed;
    }
    if (oldestIndex !== undefined) {
      msg.oldestIndex = oldestIndex;
    }
    if (serverWireVersion !== undefined) {
      msg.serverWireVersion = serverWireVersion;
    }
    if (ledgerLost !== undefined) {
      msg.ledgerLost = ledgerLost;
    }
    if (historyPaging !== undefined) {
      msg.historyPaging = historyPaging;
    }
    return msg;
  }
  if (msgType === MSG_ACK_ONLY) {
    // Bare ack: header only (type + inputAck). Applied to the outbox by the
    // connection module and never forwarded to consumers.
    return { type: "ackOnly", inputAck };
  }
  if (msgType === MSG_SCREEN) {
    // Sparse row array: the frame carries only changed rows (indexed by
    // row_idx). The client allocates a full screenHeight-sized array but
    // only the indices listed in `changed` are populated. Absolute index
    // of a changed row `y` is `base + y`.
    const base = c.u64();
    const cursorRow = c.u16();
    const cursorCol = c.u16();
    const screenHeight = c.u16();
    const numChanged = c.u16();
    const cursorStyle = c.u8();
    const cursorFlags = c.u8();
    const cursorHidden = (cursorFlags & 1) !== 0;
    const bell = (cursorFlags & 2) !== 0;
    const cursorBlink = (cursorFlags & 4) !== 0;
    const altActive = (cursorFlags & 8) !== 0;
    const scrollbackCleared = (cursorFlags & 16) !== 0;
    // eslint-disable-next-line @typescript-eslint/no-unsafe-assignment -- pre-allocated array filled below
    const rows: WireRun[][] = new Array(screenHeight);
    // eslint-disable-next-line @typescript-eslint/no-unsafe-assignment -- pre-allocated array filled below
    const changed: number[] = new Array(numChanged);
    for (let i = 0; i < numChanged; i++) {
      const idx = c.u16();
      changed[i] = idx;
      rows[idx] = readRowRuns(c);
    }
    const msg: ScreenMessage = {
      type: "screen",
      rows,
      base,
      cursor: [cursorRow, cursorCol],
      changed,
      altActive,
      cursorStyle,
      cursorHidden,
      cursorBlink,
      bell,
      scrollbackCleared,
      inputAck,
    };
    return msg;
  }
  if (msgType === MSG_SCROLL) {
    const firstIndex = c.u64();
    const numLines = c.u16();
    // eslint-disable-next-line @typescript-eslint/no-unsafe-assignment -- pre-allocated array filled below
    const lines: WireRun[][] = new Array(numLines);
    for (let i = 0; i < numLines; i++) {
      lines[i] = readRowRuns(c);
    }
    const msg: ScrollMessage = { type: "scroll", firstIndex, lines, inputAck };
    return msg;
  }
  if (msgType === MSG_MODES) {
    const flags = c.u8();
    const mouseMode = c.u16();
    // kbdFlags (kitty keyboard protocol) was appended in wire protocol v3.
    // Read it only if the frame carries it, so a pre-v3 server's shorter frame
    // decodes with the protocol disabled rather than reading past the buffer.
    const keyboardFlags = c.off < c.bytes.length ? c.u8() : 0;
    const msg: ModesMessage = {
      type: "modes",
      bracketedPaste: (flags & MODE_FLAG_BRACKETED_PASTE) !== 0,
      applicationCursor: (flags & MODE_FLAG_APP_CURSOR_KEYS) !== 0,
      applicationKeypad: (flags & MODE_FLAG_APP_KEYPAD) !== 0,
      mouseSGR: (flags & MODE_FLAG_MOUSE_SGR) !== 0,
      focusReporting: (flags & MODE_FLAG_FOCUS_REPORTING) !== 0,
      reverseVideo: (flags & MODE_FLAG_REVERSE_VIDEO) !== 0,
      mousePixels: (flags & MODE_FLAG_MOUSE_PIXELS) !== 0,
      mouseMode,
      keyboardFlags,
      inputAck,
    };
    return msg;
  }
  if (msgType === MSG_TITLE) {
    const titleLen = c.u16();
    const title = c.utf8(titleLen);
    const msg: TitleMessage = { type: "title", title, inputAck };
    return msg;
  }
  if (msgType === MSG_CLIPBOARD) {
    const textLen = c.u16();
    const text = c.utf8(textLen);
    const msg: ClipboardMessage = { type: "clipboard", text, inputAck };
    return msg;
  }
  if (msgType === MSG_PONG) {
    // Liveness pong: no payload to deliver. The message listener already
    // refreshed the activity clock on arrival, so there is nothing to do
    // here — returning null keeps it out of the onMessage path.
    return null;
  }
  console.warn("vterm: unknown wire message type", msgType);
  return null;
}
