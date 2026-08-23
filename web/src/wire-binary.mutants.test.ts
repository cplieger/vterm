// The decoder's two DROP paths, told apart by what they report.
//
// Both answer null, so a caller cannot distinguish them and no existing test
// does: the frames are dropped either way. The diagnostic is the whole
// difference, and it is the only thing an operator has when a session starts
// losing frames — "undersized" says a frame arrived that is too short to be a
// frame at all (a proxy or a mock speaking the wrong protocol), while
// "malformed/truncated" says the header parsed and the body ran out (a real
// frame cut by a network split, which the next full flush repairs). Reading the
// second where the first belongs sends the reader looking for a network fault
// that is not there.
//
// The frames are BUILT here from the layout in wire-binary.ts's header, the same
// way wire-binary-frames.test.ts does it, so the header this file calls a header
// is the wire's and not the decoder's.

import { describe, it, expect, vi, afterEach } from "vitest";

import { decodeWireBinary } from "./wire-binary.js";
import type { ScreenMessage } from "./types.js";

const MSG_SCREEN = 0;

/** Little-endian frame writer, in the wire's own field order. */
class FrameBytes {
  readonly out: number[] = [];
  u8(v: number): this {
    this.out.push(v & 0xff);
    return this;
  }
  u16(v: number): this {
    this.out.push(v & 0xff, (v >> 8) & 0xff);
    return this;
  }
  i32(v: number): this {
    const dv = new DataView(new ArrayBuffer(4));
    dv.setInt32(0, v, true);
    for (let i = 0; i < 4; i++) {
      this.out.push(dv.getUint8(i));
    }
    return this;
  }
  u64(v: number): this {
    const dv = new DataView(new ArrayBuffer(8));
    dv.setBigUint64(0, BigInt(v), true);
    for (let i = 0; i < 8; i++) {
      this.out.push(dv.getUint8(i));
    }
    return this;
  }
  utf8(s: string): this {
    for (const b of new TextEncoder().encode(s)) {
      this.out.push(b);
    }
    return this;
  }
  buffer(): ArrayBuffer {
    return new Uint8Array(this.out).buffer;
  }
}

/** The 27-byte screen header: type, inputAck, base, cursor, height, changed
 *  count, cursor style and flags. Nothing after it. */
function screenHeader(numChanged: number, screenHeight = 1): FrameBytes {
  return new FrameBytes()
    .u8(MSG_SCREEN)
    .u64(0) // inputAck
    .u64(0) // base
    .u16(0) // cursorRow
    .u16(0) // cursorCol
    .u16(screenHeight)
    .u16(numChanged)
    .u8(0) // cursorStyle
    .u8(0); // cursorFlags
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("wire-binary: which drop the decoder reports", () => {
  it("reports an eight-byte frame as UNDERSIZED, with its length", () => {
    // One byte short of the 9-byte minimum header (type + inputAck). The length
    // is part of the report because it is the only clue to what sent it.
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const short = new Uint8Array([0, 0, 0, 0, 0, 0, 0, 0]).buffer;

    expect(decodeWireBinary(short)).toBeNull();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(warn).toHaveBeenCalledWith("vterm: dropped undersized wire frame", 8);
  });

  it("reports an empty frame as UNDERSIZED rather than letting the decode fail", () => {
    // The degenerate case: a zero-length binary frame, which a proxy can emit on
    // a half-open connection. The guard is what keeps it out of the decode path,
    // where every read would fail on the first byte.
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);

    expect(decodeWireBinary(new ArrayBuffer(0))).toBeNull();

    expect(warn).toHaveBeenCalledWith("vterm: dropped undersized wire frame", 0);
  });

  it("reports a header that outlives its body as MALFORMED/TRUNCATED", () => {
    // A complete 27-byte screen header claiming one changed row, and no row
    // data: the frame is long enough to parse as a frame and still runs out
    // mid-body, which is what a network split mid-frame looks like.
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const truncated = screenHeader(1).buffer();
    expect(truncated.byteLength).toBe(27);

    expect(decodeWireBinary(truncated)).toBeNull();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(warn).toHaveBeenCalledWith("vterm: dropped malformed/truncated wire frame", 27);
  });
});

describe("wire-binary: a run's optional hyperlink", () => {
  it("carries no url field at all when the run's url length is zero", () => {
    // The wire spends a u16 on every run's url length, and zero means "this run
    // is not a hyperlink" — not "this run is a hyperlink to the empty string".
    // The decoded run must therefore have no `u` KEY, which is what the
    // renderer's own hyperlink test reads.
    const f = screenHeader(1)
      .u16(0) // changed row index
      .u16(1) // one run
      .u16(2)
      .utf8("hi")
      .i32(-1) // fg
      .i32(-1) // bg
      .u16(0) // attrs
      .i32(-1) // underline colour
      .u16(0); // url length: none

    const msg = decodeWireBinary(f.buffer()) as ScreenMessage | null;

    expect(msg?.type).toBe("screen");
    const run = msg?.rows[0]?.[0];
    expect(run).toBeDefined();
    expect(Object.keys(run ?? {}).sort()).toEqual(["a", "b", "f", "t", "uc"]);
    expect(run?.t).toBe("hi");
  });
});
