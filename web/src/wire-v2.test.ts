// Wire v2 decoder tests: the absolute-index additions to the binary
// protocol (base on screen, firstIndex on scroll, committed/oldestIndex
// on resumeAck, altActive cursor-flag bit). These lock the byte layout
// that pairs with the Go encoder in terminal/wire_binary.go (the
// authoritative byte definition); see the README "Wire Protocol" section
// for the model.

import { describe, it, expect } from "vitest";
import { decodeWireBinary } from "./wire-binary.js";
import type {
  ScreenMessage,
  ScrollMessage,
  ResumeAckMessage,
  ModesMessage,
  ClipboardMessage,
} from "./types.js";

// emptyRowPayload encodes a single run of the given text with default
// colors and no attributes/url: [2]numRuns=1, [2]tlen, text, [4]fg=-1,
// [4]bg=-1, [2]attrs=0, [4]uc=-1, [2]urlLen=0.
function rowPayloadLen(text: string): number {
  return 2 + (2 + new TextEncoder().encode(text).length + 4 + 4 + 2 + 4 + 2);
}
function writeRowPayload(dv: DataView, u8: Uint8Array, off: number, text: string): number {
  const tb = new TextEncoder().encode(text);
  dv.setUint16(off, 1, true);
  off += 2; // numRuns
  dv.setUint16(off, tb.length, true);
  off += 2;
  u8.set(tb, off);
  off += tb.length;
  dv.setInt32(off, -1, true);
  off += 4; // fg
  dv.setInt32(off, -1, true);
  off += 4; // bg
  dv.setUint16(off, 0, true);
  off += 2; // attrs
  dv.setInt32(off, -1, true);
  off += 4; // uc
  dv.setUint16(off, 0, true);
  off += 2; // urlLen
  return off;
}

describe("wire v2 decoder", () => {
  it("screen frame carries the absolute base and alt-active flag", () => {
    const text = "hi";
    const buf = new ArrayBuffer(1 + 8 + 8 + 2 + 2 + 2 + 2 + 1 + 1 + 2 + rowPayloadLen(text));
    const dv = new DataView(buf);
    const u8 = new Uint8Array(buf);
    let off = 0;
    dv.setUint8(off, 0);
    off += 1; // type=screen
    dv.setBigUint64(off, 7n, true);
    off += 8; // inputAck=7
    dv.setBigUint64(off, 12345n, true);
    off += 8; // base=12345
    dv.setUint16(off, 2, true);
    off += 2; // cursorRow
    dv.setUint16(off, 3, true);
    off += 2; // cursorCol
    dv.setUint16(off, 30, true);
    off += 2; // screenHeight
    dv.setUint16(off, 1, true);
    off += 2; // numChanged
    dv.setUint8(off, 0);
    off += 1; // cursorStyle
    dv.setUint8(off, 8);
    off += 1; // cursorFlags: bit3 = altActive
    dv.setUint16(off, 5, true);
    off += 2; // changed row idx = 5
    writeRowPayload(dv, u8, off, text);

    const msg = decodeWireBinary(buf) as ScreenMessage;
    expect(msg).not.toBeNull();
    expect(msg.type).toBe("screen");
    expect(msg.base).toBe(12345);
    expect(msg.altActive).toBe(true);
    expect(msg.cursor).toEqual([2, 3]);
    expect(msg.changed).toEqual([5]);
    expect(msg.inputAck).toBe(7);
    expect(msg.rows[5]?.[0]?.t).toBe("hi");
  });

  it("screen frame decodes the scrollbackCleared flag (cursor_flags bit4)", () => {
    const text = "x";
    const buf = new ArrayBuffer(1 + 8 + 8 + 2 + 2 + 2 + 2 + 1 + 1 + 2 + rowPayloadLen(text));
    const dv = new DataView(buf);
    const u8 = new Uint8Array(buf);
    let off = 0;
    dv.setUint8(off, 0);
    off += 1; // type=screen
    dv.setBigUint64(off, 0n, true);
    off += 8; // inputAck
    dv.setBigUint64(off, 50n, true);
    off += 8; // base=50
    dv.setUint16(off, 0, true);
    off += 2; // cursorRow
    dv.setUint16(off, 0, true);
    off += 2; // cursorCol
    dv.setUint16(off, 30, true);
    off += 2; // screenHeight
    dv.setUint16(off, 1, true);
    off += 2; // numChanged
    dv.setUint8(off, 0);
    off += 1; // cursorStyle
    dv.setUint8(off, 16);
    off += 1; // cursorFlags: bit4 = scrollbackCleared
    dv.setUint16(off, 0, true);
    off += 2; // changed row idx = 0
    writeRowPayload(dv, u8, off, text);

    const msg = decodeWireBinary(buf) as ScreenMessage;
    expect(msg.type).toBe("screen");
    expect(msg.scrollbackCleared).toBe(true);
    expect(msg.altActive).toBe(false); // bit3 not set
  });

  it("decodes the mousePixels flag (DEC 1016) in a modes frame", () => {
    const buf = new ArrayBuffer(1 + 8 + 1 + 2);
    const dv = new DataView(buf);
    let off = 0;
    dv.setUint8(off, 3);
    off += 1; // type=modes
    dv.setBigUint64(off, 0n, true);
    off += 8; // inputAck
    dv.setUint8(off, 64);
    off += 1; // flags: bit6 = mousePixels
    dv.setUint16(off, 1006, true); // mouseMode

    const msg = decodeWireBinary(buf) as ModesMessage;
    expect(msg.type).toBe("modes");
    expect(msg.mousePixels).toBe(true);
    expect(msg.mouseSGR).toBe(false); // bit2 not set
  });

  it("decodes a clipboard frame (OSC 52) into a ClipboardMessage", () => {
    const tb = new TextEncoder().encode("hello");
    const buf = new ArrayBuffer(1 + 8 + 2 + tb.length);
    const dv = new DataView(buf);
    const u8 = new Uint8Array(buf);
    let off = 0;
    dv.setUint8(off, 6);
    off += 1; // type=clipboard
    dv.setBigUint64(off, 3n, true);
    off += 8; // inputAck=3
    dv.setUint16(off, tb.length, true);
    off += 2;
    u8.set(tb, off);

    const msg = decodeWireBinary(buf) as ClipboardMessage;
    expect(msg).not.toBeNull();
    expect(msg.type).toBe("clipboard");
    expect(msg.text).toBe("hello");
    expect(msg.inputAck).toBe(3);
  });

  it("scroll frame carries the absolute firstIndex", () => {
    const buf = new ArrayBuffer(1 + 8 + 8 + 2 + rowPayloadLen("a") + rowPayloadLen("b"));
    const dv = new DataView(buf);
    const u8 = new Uint8Array(buf);
    let off = 0;
    dv.setUint8(off, 1);
    off += 1; // type=scroll
    dv.setBigUint64(off, 0n, true);
    off += 8; // inputAck
    dv.setBigUint64(off, 1000n, true);
    off += 8; // firstIndex=1000
    dv.setUint16(off, 2, true);
    off += 2; // numLines=2
    off = writeRowPayload(dv, u8, off, "a");
    writeRowPayload(dv, u8, off, "b");

    const msg = decodeWireBinary(buf) as ScrollMessage;
    expect(msg).not.toBeNull();
    expect(msg.type).toBe("scroll");
    expect(msg.firstIndex).toBe(1000);
    expect(msg.lines.length).toBe(2);
    expect(msg.lines[0]?.[0]?.t).toBe("a");
    expect(msg.lines[1]?.[0]?.t).toBe("b");
  });

  it("resumeAck carries epoch, committed, and oldestIndex for gap detection", () => {
    const buf = new ArrayBuffer(1 + 8 + 8 + 8 + 8);
    const dv = new DataView(buf);
    let off = 0;
    dv.setUint8(off, 2);
    off += 1; // type=resumeAck
    dv.setBigUint64(off, 42n, true);
    off += 8; // inputAck/received=42
    dv.setBigUint64(off, 999n, true);
    off += 8; // serverEpoch
    dv.setBigUint64(off, 5000n, true);
    off += 8; // committed
    dv.setBigUint64(off, 4000n, true); // oldestIndex (last field; no further offset use)

    const msg = decodeWireBinary(buf) as ResumeAckMessage;
    expect(msg).not.toBeNull();
    expect(msg.type).toBe("resumeAck");
    expect(msg.received).toBe(42);
    expect(msg.serverEpoch).toBe(999);
    expect(msg.committed).toBe(5000);
    expect(msg.oldestIndex).toBe(4000);
  });

  it("older resumeAck without the index bounds still decodes (back-compat tail)", () => {
    const buf = new ArrayBuffer(1 + 8 + 8); // type + ack + epoch only
    const dv = new DataView(buf);
    dv.setUint8(0, 2);
    dv.setBigUint64(1, 0n, true);
    dv.setBigUint64(9, 555n, true);
    const msg = decodeWireBinary(buf) as ResumeAckMessage;
    expect(msg.type).toBe("resumeAck");
    expect(msg.serverEpoch).toBe(555);
    expect(msg.committed).toBeUndefined();
    expect(msg.oldestIndex).toBeUndefined();
  });
});
