// MSG_TITLE wire decoding (the TS half of the OSC 0/1/2 title path).
//
// Spec: xterm OSC 0/1/2 set the window/icon title (OSC 0 = icon + window,
// OSC 1 = icon, OSC 2 = window). That parsing is done by the Go engine; which
// OSC code maps to which title is a server-side concern. Over the wire the
// engine emits a resolved title string in a MSG_TITLE frame, and the TS
// contract under test here is: decode that binary frame into a TitleMessage
// carrying the exact title (the client then reflects it into document.title).
//
// The frames are built by a spec-faithful encoder (the inverse of the
// documented MSG_TITLE layout in wire-binary.ts) and decoded by the PRODUCTION
// decoder, so a decoder that mis-reads a field fails the round-trip. The
// encoder is the inverse-spec, not a copy of the decoder.

import { describe, it, expect } from "vitest";
import { decodeWireBinary } from "./wire-binary.js";
import type { TitleMessage } from "./types.js";

// MSG_TITLE frame layout (mirrors wire_binary.go / wire-binary.ts):
//   [1B] msg_type = 4
//   [8B] inputAck        (u64 little-endian)
//   [2B] title_byte_len  (u16 little-endian)
//   [NB] title           (UTF-8)
function encodeTitleMsg(title: string, inputAck = 0): ArrayBuffer {
  const titleBytes = new TextEncoder().encode(title);
  const buf = new ArrayBuffer(1 + 8 + 2 + titleBytes.length);
  const view = new DataView(buf);
  const bytes = new Uint8Array(buf);
  view.setUint8(0, 4); // MSG_TITLE tag
  view.setBigUint64(1, BigInt(inputAck), true); // inputAck (u64 LE)
  view.setUint16(9, titleBytes.length, true); // title byte length (u16 LE)
  bytes.set(titleBytes, 11); // UTF-8 title bytes
  return buf;
}

describe("wire-binary: MSG_TITLE decoding", () => {
  it("decodes a title message", () => {
    const msg = decodeWireBinary(encodeTitleMsg("hello world"));
    expect(msg).not.toBeNull();
    expect(msg!.type).toBe("title");
    expect((msg as TitleMessage).title).toBe("hello world");
  });

  it("decodes an empty title", () => {
    // OSC 2 with an empty string clears the window title — a valid frame.
    const msg = decodeWireBinary(encodeTitleMsg(""));
    expect(msg).not.toBeNull();
    expect((msg as TitleMessage).title).toBe("");
  });

  it("decodes a multi-byte UTF-8 title", () => {
    // Title byte length is counted in UTF-8 bytes, not code points; a CJK title
    // exercises the multi-byte length + TextDecoder path.
    const msg = decodeWireBinary(encodeTitleMsg("日本語タイトル"));
    expect(msg).not.toBeNull();
    expect((msg as TitleMessage).title).toBe("日本語タイトル");
  });

  it("decodes the inputAck field that precedes the title", () => {
    // The 8-byte inputAck (server-confirmed bytesReceived) sits between the tag
    // and the title; a decoder that mis-sizes it would shift the title read.
    const msg = decodeWireBinary(encodeTitleMsg("title", 4096));
    expect(msg).not.toBeNull();
    expect((msg as TitleMessage).title).toBe("title");
    expect((msg as TitleMessage).inputAck).toBe(4096);
  });

  it("returns null for a truncated title frame (graceful drop)", () => {
    // A frame that ends right after the header (tag + inputAck) with the
    // title-length field missing is malformed/truncated. The decoder's contract
    // is to drop such frames (return null), not throw — a mid-frame network
    // split must not crash the decode loop.
    const buf = new ArrayBuffer(9);
    new DataView(buf).setUint8(0, 4); // MSG_TITLE tag, then nothing
    expect(decodeWireBinary(buf)).toBeNull();
  });
});
