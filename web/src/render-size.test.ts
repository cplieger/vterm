// @vitest-environment happy-dom
//
// The two numbers the renderer derives for the transport rather than for the
// screen, both otherwise unexercised against the real DOM:
//
//   - computeSize(): the (cols, rows) a `resize` control message carries. It is
//     the CONTENT box divided by the cell, so the terminal's padding must come
//     off the measured box first — a size computed over the padding asks the
//     server for a screen wider and taller than the one that fits, and every
//     row then soft-wraps. Clamped to a floor so a collapsed or mid-layout
//     element can never ask for a zero-column pty.
//   - replayMaxForResume(): how much history to ask a server to replay on
//     attach. The client's own retention cap minus the live window it is about
//     to be sent anyway, so a reconnect does not download rows the cap would
//     immediately trim.

import { describe, it, expect, beforeEach, afterEach } from "vitest";
import * as render from "./render.js";
import type { ScreenMessage, WireRun } from "./types.js";

const CELL_PX = 8;

let realGetContext: typeof HTMLCanvasElement.prototype.getContext;
let realRect: typeof HTMLElement.prototype.getBoundingClientRect;
let realRAF: typeof globalThis.requestAnimationFrame;
let realCAF: typeof globalThis.cancelAnimationFrame;

let termWrap: HTMLDivElement;
let output: HTMLDivElement;

function installStubs(): void {
  realGetContext = HTMLCanvasElement.prototype.getContext;
  realRect = HTMLElement.prototype.getBoundingClientRect;
  HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
    return { font: "", measureText: (t: string) => ({ width: [...t].length * CELL_PX }) };
  } as typeof HTMLCanvasElement.prototype.getContext;
  HTMLElement.prototype.getBoundingClientRect = function fakeRect(this: HTMLElement): DOMRect {
    const width = [...(this.textContent ?? "")].length * CELL_PX;
    return {
      x: 0,
      y: 0,
      width,
      height: 17,
      top: 0,
      left: 0,
      right: width,
      bottom: 17,
      toJSON: () => ({}),
    } as DOMRect;
  };
  realRAF = globalThis.requestAnimationFrame;
  realCAF = globalThis.cancelAnimationFrame;
  globalThis.requestAnimationFrame = ((cb: FrameRequestCallback): number => {
    cb(0);
    return undefined as unknown as number;
  }) as typeof globalThis.requestAnimationFrame;
  globalThis.cancelAnimationFrame = (() => undefined) as typeof globalThis.cancelAnimationFrame;
}

function restoreStubs(): void {
  HTMLCanvasElement.prototype.getContext = realGetContext;
  HTMLElement.prototype.getBoundingClientRect = realRect;
  globalThis.requestAnimationFrame = realRAF;
  globalThis.cancelAnimationFrame = realCAF;
}

/**
 * Attach the renderer to a terminal element of a known BORDER box with known
 * padding. happy-dom has no layout, so clientWidth/clientHeight are defined
 * here the way a browser would report them: the padding box, padding included.
 */
function attachSized(opts: {
  clientWidth: number;
  clientHeight: number;
  padding: string;
  maxLines?: number;
}): void {
  document.body.innerHTML = `<div class="term"><div class="term-output"></div></div>`;
  termWrap = document.querySelector<HTMLDivElement>(".term")!;
  output = document.querySelector<HTMLDivElement>(".term-output")!;
  termWrap.style.fontSize = "16px";
  termWrap.style.fontFamily = "monospace";
  termWrap.style.lineHeight = "17px";
  termWrap.style.padding = opts.padding;
  Object.defineProperty(termWrap, "clientWidth", {
    configurable: true,
    get: () => opts.clientWidth,
  });
  Object.defineProperty(termWrap, "clientHeight", {
    configurable: true,
    get: () => opts.clientHeight,
  });
  render.init(
    opts.maxLines === undefined
      ? { output, termWrap }
      : { output, termWrap, maxLines: opts.maxLines },
  );
  render.updateFontMetrics();
}

beforeEach(() => {
  installStubs();
});

afterEach(() => {
  restoreStubs();
});

describe("computeSize reports the terminal's grid", () => {
  it("divides the content box, with the padding taken off both axes", () => {
    // A 500x300 padding box with 10px of padding on every side leaves a
    // 480x280 content box. At an 8px cell and a 17px line that is 60 columns
    // and 16 rows (280/17 = 16.47, and a partial row is not a row).
    attachSized({ clientWidth: 500, clientHeight: 300, padding: "10px" });
    expect(render.computeSize()).toEqual({ cols: 60, rows: 16 });
  });

  it("never asks for fewer than 20 columns or 5 rows", () => {
    // A collapsed or mid-layout element measures near zero. A pty sized from
    // that is unusable, so the size is floored.
    attachSized({ clientWidth: 40, clientHeight: 20, padding: "0px" });
    expect(render.computeSize()).toEqual({ cols: 20, rows: 5 });
  });
});

describe("replayMaxForResume bounds the resume replay", () => {
  it("asks for the retention cap minus the live window", () => {
    // A 100-line client cap with a 10-row screen: the screen arrives with the
    // resume anyway, so at most 90 lines of history are worth replaying.
    attachSized({ clientWidth: 500, clientHeight: 300, padding: "0px", maxLines: 100 });
    const rows: WireRun[][] = [];
    for (let i = 0; i < 10; i++) {
      rows.push([{ t: `line ${String(i)}`, f: -1, b: -1, a: 0, uc: -1 }]);
    }
    const msg: ScreenMessage = {
      type: "screen",
      base: 0,
      rows,
      cursor: [0, 0],
      changed: [0],
      cursorHidden: true,
      cursorStyle: 0,
      cursorBlink: false,
    };
    render.handleScreen(msg);
    expect(render.replayMaxForResume()).toBe(90);
  });
});
