// Binary-frame decoder tests for the fields the existing wire suites leave
// unpinned: little-endian colour words, an OSC-8 URI inside a run, the sparse
// row array's declared height, the bell flag, the full modes bitmap, and the
// two frames that carry no payload (pong and an unknown type).
//
// The frames are BUILT here from the layout documented in wire-binary.ts's
// header and mirrored from the Go encoder (terminal/wire_binary.go): the
// builder is the wire SPEC, so encode-then-decode is a real round trip rather
// than the decoder checked against a copy of itself. Every multi-byte integer
// is little-endian.
//
// Why these fields: wire-v2.test.ts and wire.test.ts write every colour as -1
// (0xFFFFFFFF), which reads identically in either byte order, and never set a
// run's url length, so a mis-decoded colour or a dropped hyperlink is invisible
// to them.

import { describe, it, expect, vi } from "vitest";
import { decodeWireBinary } from "./wire-binary.js";
import type { ScreenMessage, ModesMessage, WireRun } from "./types.js";

const MSG_SCREEN = 0;
const MSG_MODES = 3;
const MSG_PONG = 5;

/** One styled run as the wire carries it: t, fg, bg, attrs, underline colour, url. */
interface SpecRun {
  t: string;
  f: number;
  b: number;
  a: number;
  uc: number;
  u?: string;
}

const utf8 = (s: string): Uint8Array => new TextEncoder().encode(s);

function runBytes(run: SpecRun): number[] {
  const t = utf8(run.t);
  const u = utf8(run.u ?? "");
  const out: number[] = [];
  const u16 = (v: number): void => {
    out.push(v & 0xff, (v >> 8) & 0xff);
  };
  const i32 = (v: number): void => {
    const dv = new DataView(new ArrayBuffer(4));
    dv.setInt32(0, v, true);
    out.push(dv.getUint8(0), dv.getUint8(1), dv.getUint8(2), dv.getUint8(3));
  };
  u16(t.length);
  out.push(...t);
  i32(run.f);
  i32(run.b);
  u16(run.a);
  i32(run.uc);
  u16(u.length);
  out.push(...u);
  return out;
}

function rowBytes(runs: SpecRun[]): number[] {
  const out: number[] = [runs.length & 0xff, (runs.length >> 8) & 0xff];
  for (const r of runs) {
    out.push(...runBytes(r));
  }
  return out;
}

/** A screen frame: type, inputAck, base, cursor, height, changed count, style/flags, rows. */
function screenFrame(opts: {
  inputAck?: number;
  base?: number;
  cursorRow?: number;
  cursorCol?: number;
  screenHeight: number;
  cursorStyle?: number;
  cursorFlags?: number;
  changed: { idx: number; runs: SpecRun[] }[];
}): ArrayBuffer {
  const out: number[] = [MSG_SCREEN];
  const u16 = (v: number): void => {
    out.push(v & 0xff, (v >> 8) & 0xff);
  };
  const u64 = (v: number): void => {
    const dv = new DataView(new ArrayBuffer(8));
    dv.setBigUint64(0, BigInt(v), true);
    for (let i = 0; i < 8; i++) {
      out.push(dv.getUint8(i));
    }
  };
  u64(opts.inputAck ?? 0);
  u64(opts.base ?? 0);
  u16(opts.cursorRow ?? 0);
  u16(opts.cursorCol ?? 0);
  u16(opts.screenHeight);
  u16(opts.changed.length);
  out.push(opts.cursorStyle ?? 0);
  out.push(opts.cursorFlags ?? 0);
  for (const c of opts.changed) {
    u16(c.idx);
    out.push(...rowBytes(c.runs));
  }
  return new Uint8Array(out).buffer;
}

/** A modes frame: type, inputAck, flags byte, mouseMode u16, optional v3 kbdFlags. */
function modesFrame(flags: number, mouseMode: number, keyboardFlags?: number): ArrayBuffer {
  const out: number[] = [MSG_MODES, 0, 0, 0, 0, 0, 0, 0, 0];
  out.push(flags);
  out.push(mouseMode & 0xff, (mouseMode >> 8) & 0xff);
  if (keyboardFlags !== undefined) {
    out.push(keyboardFlags);
  }
  return new Uint8Array(out).buffer;
}

/** A header-only frame of the given type, padded to `byteLength`. */
function bareFrame(msgType: number, byteLength = 9): ArrayBuffer {
  const out = new Uint8Array(byteLength);
  out[0] = msgType;
  return out.buffer;
}

describe("wire-binary: colour words are little-endian", () => {
  it("decodes a run's foreground, background and underline colours", () => {
    // Values chosen so a big-endian read cannot produce them: 1 would read as
    // 0x01000000, 0x0000ff as 0xff000000 (negative as i32).
    const buf = screenFrame({
      screenHeight: 1,
      changed: [{ idx: 0, runs: [{ t: "hi", f: 1, b: 0x0000ff, a: 0, uc: 0x00abcdef }] }],
    });
    const msg = decodeWireBinary(buf) as ScreenMessage;
    const run = msg.rows[0]?.[0] as WireRun;
    expect(run.f).toBe(1);
    expect(run.b).toBe(0x0000ff);
    expect(run.uc).toBe(0x00abcdef);
  });

  it("decodes a negative colour sentinel as -1, not as a large unsigned word", () => {
    const buf = screenFrame({
      screenHeight: 1,
      changed: [{ idx: 0, runs: [{ t: "x", f: -1, b: -1, a: 0, uc: -1 }] }],
    });
    const msg = decodeWireBinary(buf) as ScreenMessage;
    expect(msg.rows[0]?.[0]?.f).toBe(-1);
  });
});

describe("wire-binary: a run's OSC-8 URI", () => {
  it("decodes the URI and keeps the cursor aligned for the run after it", () => {
    // The url length gates BOTH the field and the cursor advance: a decoder
    // that skipped the read would leave the cursor mid-URI and mis-decode the
    // next run's text.
    const buf = screenFrame({
      screenHeight: 1,
      changed: [
        {
          idx: 0,
          runs: [
            { t: "link", f: -1, b: -1, a: 0, uc: -1, u: "https://example.com/a" },
            { t: "after", f: 7, b: -1, a: 0, uc: -1 },
          ],
        },
      ],
    });
    const msg = decodeWireBinary(buf) as ScreenMessage;
    expect(msg.rows[0]?.[0]?.u).toBe("https://example.com/a");
    expect(msg.rows[0]?.[1]?.t).toBe("after");
    expect(msg.rows[0]?.[1]?.f).toBe(7);
  });

  it("omits the url field entirely for a run with a zero-length URI", () => {
    const buf = screenFrame({
      screenHeight: 1,
      changed: [{ idx: 0, runs: [{ t: "plain", f: -1, b: -1, a: 0, uc: -1, u: "" }] }],
    });
    const msg = decodeWireBinary(buf) as ScreenMessage;
    expect(msg.rows[0]?.[0]).not.toHaveProperty("u");
  });
});

describe("wire-binary: the sparse row array", () => {
  it("is as long as the frame's screen height, not as its highest changed index", () => {
    // The renderer indexes rows by screen row and fills the untouched ones from
    // its own state; a short array would silently drop the rows below the last
    // changed one.
    const buf = screenFrame({
      screenHeight: 8,
      changed: [{ idx: 2, runs: [{ t: "only", f: -1, b: -1, a: 0, uc: -1 }] }],
    });
    const msg = decodeWireBinary(buf) as ScreenMessage;
    expect(msg.rows.length).toBe(8);
    expect(msg.changed).toEqual([2]);
  });
});

describe("wire-binary: cursor_flags bit1 is the bell", () => {
  it("decodes bell=true when bit1 is set", () => {
    const buf = screenFrame({
      screenHeight: 1,
      cursorFlags: 2,
      changed: [{ idx: 0, runs: [{ t: "x", f: -1, b: -1, a: 0, uc: -1 }] }],
    });
    const msg = decodeWireBinary(buf) as ScreenMessage;
    expect(msg.bell).toBe(true);
  });

  it("decodes bell=false when only the neighbouring bits are set", () => {
    // bit0 = cursorHidden, bit2 = cursorBlink: neither may leak into bell.
    const buf = screenFrame({
      screenHeight: 1,
      cursorFlags: 1 | 4,
      changed: [{ idx: 0, runs: [{ t: "x", f: -1, b: -1, a: 0, uc: -1 }] }],
    });
    const msg = decodeWireBinary(buf) as ScreenMessage;
    expect(msg.bell).toBe(false);
    expect(msg.cursorHidden).toBe(true);
    expect(msg.cursorBlink).toBe(true);
  });
});

describe("wire-binary: the modes bitmap", () => {
  // bit0 bracketedPaste, bit1 appCursor, bit2 mouseSGR, bit3 focusReporting,
  // bit4 appKeypad, bit5 reverseVideo, bit6 mousePixels.
  it("decodes every mode as enabled when all seven bits are set", () => {
    const msg = decodeWireBinary(modesFrame(0x7f, 1002, 1)) as ModesMessage;
    expect(msg.bracketedPaste).toBe(true);
    expect(msg.applicationCursor).toBe(true);
    expect(msg.mouseSGR).toBe(true);
    expect(msg.focusReporting).toBe(true);
    expect(msg.applicationKeypad).toBe(true);
    expect(msg.reverseVideo).toBe(true);
    expect(msg.mousePixels).toBe(true);
    expect(msg.mouseMode).toBe(1002);
    expect(msg.keyboardFlags).toBe(1);
  });

  it("decodes every mode as disabled when no bit is set", () => {
    const msg = decodeWireBinary(modesFrame(0, 0)) as ModesMessage;
    expect(msg.bracketedPaste).toBe(false);
    expect(msg.applicationCursor).toBe(false);
    expect(msg.mouseSGR).toBe(false);
    expect(msg.focusReporting).toBe(false);
    expect(msg.applicationKeypad).toBe(false);
    expect(msg.reverseVideo).toBe(false);
    expect(msg.mousePixels).toBe(false);
    expect(msg.keyboardFlags).toBe(0);
  });

  it("decodes one mode without lighting the others", () => {
    // bit3 alone: the focus-reporting slot must not be read from a neighbour.
    const msg = decodeWireBinary(modesFrame(8, 0)) as ModesMessage;
    expect(msg.focusReporting).toBe(true);
    expect(msg.applicationKeypad).toBe(false);
    expect(msg.mouseSGR).toBe(false);
  });
});

describe("wire-binary: frames with no payload to deliver", () => {
  it("drops a pong frame silently", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    expect(decodeWireBinary(bareFrame(MSG_PONG))).toBeNull();
    expect(warn).not.toHaveBeenCalled();
  });

  it("drops an unknown message type and reports it", () => {
    // Forward compatibility: an unrecognised tag must not be decoded as some
    // other message. The frame is long enough for the branches above to
    // succeed if the type check were bypassed.
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const buf = new Uint8Array(bareFrame(99, 12));
    buf[9] = 1; // a u16 length of 1 …
    buf[11] = 0x78; // … and one byte of text, decodable as a title/clipboard body
    expect(decodeWireBinary(buf.buffer)).toBeNull();
    expect(warn).toHaveBeenCalledWith("vterm: unknown wire message type", 99);
  });
});
