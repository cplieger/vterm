// DECSCNM (reverse video, DEC private mode 5) conformance, end to end: the wire
// decoder lifts bit 5 into a ModesMessage, modes.ts holds that state, and
// render.ts reflects it on screen. Spec-first — expectations come from the
// ANSI/DEC definition of reverse video (swap the screen's default fg/bg), not
// from the renderer's internals.
import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { decodeWireBinary } from "./wire-binary.js";
import type { ModesMessage } from "./types.js";
import * as modes from "./modes.js";
import * as render from "./render.js";

describe("DECSCNM (reverse video) wire decoding", () => {
  it("decodes reverseVideo=true from modes message with bit 5 set", () => {
    // Build a modes message: type=3, ack=0 (8 bytes), flags=0x20 (bit 5), mouseMode=0
    const buf = new ArrayBuffer(12);
    const view = new DataView(buf);
    view.setUint8(0, 3); // MSG_MODES
    // inputAck = 0 (bytes 1-8, already zero)
    view.setUint8(9, 0x20); // flags: bit 5 = reverse video
    view.setUint16(10, 0, true); // mouseMode = 0

    const msg = decodeWireBinary(buf) as ModesMessage;
    expect(msg).not.toBeNull();
    expect(msg.type).toBe("modes");
    expect(msg.reverseVideo).toBe(true);
    expect(msg.bracketedPaste).toBe(false);
  });

  it("decodes reverseVideo=false when bit 5 is not set", () => {
    const buf = new ArrayBuffer(12);
    const view = new DataView(buf);
    view.setUint8(0, 3); // MSG_MODES
    view.setUint8(9, 0x01); // flags: bit 0 only (bracketed paste)
    view.setUint16(10, 0, true);

    const msg = decodeWireBinary(buf) as ModesMessage;
    expect(msg).not.toBeNull();
    expect(msg.type).toBe("modes");
    expect(msg.reverseVideo).toBe(false);
    expect(msg.bracketedPaste).toBe(true);
  });

  it("modes.setModes stores and exposes reverseVideo", () => {
    modes.setModes(true, false, false, false, 0, false, true);
    expect(modes.isReverseVideo()).toBe(true);
    modes.setModes(true, false, false, false, 0, false, false);
    expect(modes.isReverseVideo()).toBe(false);
  });
});

describe("DECSCNM (reverse video) display effect (spec)", () => {
  // The decode + modes-state tests above stop at the client's mode flag. The
  // renderer must then reflect it on screen: per the spec, reverse video swaps
  // the screen's default foreground/background. The engine expresses that swap
  // as a `term-reverse-video` class on the terminal wrapper (the CSS bundle
  // does the actual color inversion), so the observable contract is "the class
  // tracks the mode". This is the display half the decode tests leave untested.
  let termWrap: HTMLDivElement;

  beforeEach(() => {
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    const outputEl = document.getElementById("term-output") as HTMLDivElement;
    render.init({ output: outputEl, termWrap });
  });

  afterEach(() => {
    // modes is a module-global singleton and vitest runs with isolate:false;
    // reset reverse-video off so this state never leaks into another test file.
    modes.setModes(true, false, false, false, 0, false, false);
  });

  it("marks the terminal reverse-video when DECSCNM is active", () => {
    modes.setModes(true, false, false, false, 0, false, true);
    render.updateReverseVideo();
    expect(termWrap.classList.contains("term-reverse-video")).toBe(true);
  });

  it("clears the reverse-video mark when DECSCNM is turned off", () => {
    modes.setModes(true, false, false, false, 0, false, true);
    render.updateReverseVideo();
    modes.setModes(true, false, false, false, 0, false, false);
    render.updateReverseVideo();
    expect(termWrap.classList.contains("term-reverse-video")).toBe(false);
  });
});
