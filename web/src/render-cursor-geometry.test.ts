// @vitest-environment happy-dom
//
// Caret geometry and shape: where the two overlays land, and what the DECSCUSR
// style byte turns into on screen.
//
// The caret is a single absolutely-positioned element in the scroll container,
// not a restyled span inside the row, so everything about it is arithmetic over
// (padding, cell size, column, row) — and every one of those terms is asserted
// here at a known metric. The predicted-cursor overlay runs the same arithmetic
// for a consumer echoing keystrokes ahead of the server.
//
// Spec: DECSCUSR (`CSI Ps SP q`) — 0/1 blinking block, 2 steady block,
// 3 blinking underline, 4 steady underline, 5 blinking bar, 6 steady bar. The
// renderer expresses the three SHAPES as classes (the blink phase is a separate
// mechanism), so 3 and 4 must both be an underline and 5 and 6 must both be a
// bar. Cell coordinates are TRUE cells: a Wide glyph owns two of them.

import { describe, it, expect, beforeEach, afterEach } from "vitest";
import * as render from "./render.js";
import type { ScreenMessage, WireRun } from "./types.js";

const CELL_PX = 8;
// East Asian Wide glyphs measure two cells; everything else measures one.
const WIDE = new Set(["漢"]);

interface FakeCtx {
  font: string;
  measureText: (t: string) => { width: number };
}

let realGetContext: typeof HTMLCanvasElement.prototype.getContext;
let realRect: typeof HTMLElement.prototype.getBoundingClientRect;
let realRAF: typeof globalThis.requestAnimationFrame;
let realCAF: typeof globalThis.cancelAnimationFrame;

function installStubs(): void {
  realGetContext = HTMLCanvasElement.prototype.getContext;
  realRect = HTMLElement.prototype.getBoundingClientRect;
  HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
    const ctx: FakeCtx = {
      font: "",
      measureText: (text: string): { width: number } => {
        let w = 0;
        for (const ch of text) {
          w += WIDE.has(ch) ? CELL_PX * 2 : CELL_PX;
        }
        return { width: w };
      },
    };
    return ctx;
  } as typeof HTMLCanvasElement.prototype.getContext;
  // happy-dom has no layout; the cell measurement reads a rect off a probe span
  // of ten `M`s.
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

let output: HTMLDivElement;
let termWrap: HTMLDivElement;

/** Attach the renderer to a fresh surface with a known box. */
function attach(opts: { padding?: string; lineHeight?: string } = {}): void {
  document.body.innerHTML = `<div class="term"><div class="term-output"></div></div>`;
  termWrap = document.querySelector<HTMLDivElement>(".term")!;
  output = document.querySelector<HTMLDivElement>(".term-output")!;
  termWrap.style.fontSize = "16px";
  termWrap.style.fontFamily = "monospace";
  termWrap.style.padding = opts.padding ?? "0px";
  termWrap.style.lineHeight = opts.lineHeight ?? "17px";
  render.init({ output, termWrap });
}

interface FrameOpts {
  rows: WireRun[][];
  cursor: [number, number];
  base?: number;
  cursorStyle?: number;
  cursorHidden?: boolean;
  altActive?: boolean;
}

function paint(opts: FrameOpts): void {
  const msg: ScreenMessage = {
    type: "screen",
    base: opts.base ?? 0,
    rows: opts.rows,
    cursor: opts.cursor,
    changed: opts.rows.map((_, i) => i),
    cursorHidden: opts.cursorHidden ?? false,
    cursorStyle: opts.cursorStyle ?? 0,
    cursorBlink: false,
  };
  render.handleScreen(opts.altActive === undefined ? msg : { ...msg, altActive: opts.altActive });
}

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
}

function caret(): HTMLElement {
  const el = document.querySelector<HTMLElement>(".term-cursor-overlay");
  expect(el, "the caret overlay must exist").not.toBeNull();
  return el!;
}

function predicted(): HTMLElement {
  const el = document.querySelector<HTMLElement>(".pred-cursor");
  expect(el, "the predicted-cursor overlay must exist").not.toBeNull();
  return el!;
}

beforeEach(() => {
  installStubs();
  attach();
  render.updateFontMetrics();
});

afterEach(() => {
  restoreStubs();
});

describe("DECSCUSR selects the caret shape", () => {
  it("styles 3 and 4 paint an underline caret", () => {
    paint({ rows: [row("abc")], cursor: [0, 0], cursorStyle: 3 });
    expect(caret().classList.contains("term-cursor-underline")).toBe(true);
    paint({ rows: [row("abc")], cursor: [0, 0], cursorStyle: 4 });
    expect(caret().classList.contains("term-cursor-underline")).toBe(true);
  });

  it("styles 5 and 6 paint a bar caret", () => {
    paint({ rows: [row("abc")], cursor: [0, 0], cursorStyle: 5 });
    expect(caret().classList.contains("term-cursor-bar")).toBe(true);
    paint({ rows: [row("abc")], cursor: [0, 0], cursorStyle: 6 });
    expect(caret().classList.contains("term-cursor-bar")).toBe(true);
  });

  it("styles 0 and 2 paint a block caret", () => {
    paint({ rows: [row("abc")], cursor: [0, 0], cursorStyle: 0 });
    expect(caret().classList.contains("term-cursor")).toBe(true);
    expect(caret().classList.contains("term-cursor-underline")).toBe(false);
    expect(caret().classList.contains("term-cursor-bar")).toBe(false);
    paint({ rows: [row("abc")], cursor: [0, 0], cursorStyle: 2 });
    expect(caret().classList.contains("term-cursor")).toBe(true);
    expect(caret().classList.contains("term-cursor-bar")).toBe(false);
  });
});

describe("the caret covers the cell the cursor is on", () => {
  it("sits at the cursor's column, one cell wide", () => {
    // Column 3 at an 8px cell, with no terminal padding.
    paint({ rows: [row("abcdef")], cursor: [0, 3] });
    expect(caret().style.left).toBe("24px");
    expect(caret().style.width).toBe("8px");
  });

  it("covers both cells of a Wide glyph", () => {
    // "A漢B": A is col 0, 漢 owns cols 1-2, B is col 3. On the wide glyph the
    // caret must cover the pair, or the block caret paints over half a glyph.
    paint({ rows: [row("A漢\uFFFFB")], cursor: [0, 1] });
    expect(caret().textContent).toBe("漢");
    expect(caret().style.width).toBe("16px");
  });

  it("does not cover two cells for a narrow glyph", () => {
    paint({ rows: [row("A漢\uFFFFB")], cursor: [0, 0] });
    expect(caret().textContent).toBe("A");
    expect(caret().style.width).toBe("8px");
  });

  it("shows a blank on a Wide glyph's continuation cell, never the sentinel", () => {
    // Cell 2 is the second half of 漢, delivered over the wire as U+FFFF. The
    // sentinel is an encoding artifact and must never reach the screen; a real
    // terminal paints an inverted blank there.
    paint({ rows: [row("A漢\uFFFFB")], cursor: [0, 2] });
    expect(caret().textContent).toBe("\u00a0");
  });

  it("takes its height from the line height", () => {
    attach({ lineHeight: "20px" });
    render.updateFontMetrics();
    paint({ rows: [row("abc")], cursor: [0, 0] });
    expect(caret().style.height).toBe("20px");
  });

  it("falls back to a 17px line when the line height is not a length", () => {
    // `line-height: normal` has no computed pixel value to parse.
    attach({ lineHeight: "normal" });
    render.updateFontMetrics();
    paint({ rows: [row("abc")], cursor: [0, 0] });
    expect(caret().style.height).toBe("17px");
  });

  it("positions itself from the grid row index on the alternate screen", () => {
    // Alt-screen rows are ephemeral and not registered by absolute index, so
    // the caret's offset comes from the cursor's row WITHIN the window (the
    // window's base and height stay those of the main screen while alt is up).
    paint({ rows: [row("main")], cursor: [0, 0], base: 5 });
    paint({
      rows: [row("alt0"), row("alt1"), row("alt2")],
      cursor: [2, 0],
      base: 5,
      altActive: true,
    });
    // Row 2 of the alt grid, at a 17px line: 2 * 17.
    expect(caret().style.top).toBe("34px");
  });

  it("reports the row it paints on to the consumer, not the top of the terminal", () => {
    // `getCursorPx` is the only channel to the consumer's IME view and hidden
    // textarea, so it must resolve the cursor's row the same way the caret
    // does. It did not: it answered the content origin for every row the
    // element map does not hold, which is EVERY row of an alt-screen session
    // (only the main-screen builder registers rows, and entering alt clears
    // them). The caret painted on row 2 while the consumer was told row 0 for
    // as long as the TUI was up.
    paint({ rows: [row("main")], cursor: [0, 0], base: 5 });
    paint({
      rows: [row("alt0"), row("alt1"), row("alt2")],
      cursor: [2, 0],
      base: 5,
      altActive: true,
    });
    expect(render.getCursorPx().top).toBe(34);
    // The same 34px the caret is painted at, one line above.
    expect(caret().style.top).toBe("34px");
  });
});

describe("the terminal's padding offsets the overlays", () => {
  it("is read at attach and refreshed by updateFontMetrics, not per frame", () => {
    // The padding is cached because the overlay positioners need it on every
    // flush and a live computed-style read there forces a style recalc. The
    // contract that buys is explicit: a consumer that restyles the terminal
    // calls updateFontMetrics. Assert both halves — a fresh attach picks the
    // padding up, and a restyle without the call does not move the caret.
    attach({ padding: "0px" });
    render.updateFontMetrics();
    attach({ padding: "9px" });
    paint({ rows: [row("abcdef")], cursor: [0, 1] });
    expect(caret().style.left).toBe("17px"); // 9 padding + 1 cell

    termWrap.style.padding = "40px";
    paint({ rows: [row("abcdef")], cursor: [0, 2] });
    expect(caret().style.left).toBe("25px"); // still the 9px read at attach

    render.updateFontMetrics();
    paint({ rows: [row("abcdef")], cursor: [0, 2] });
    expect(caret().style.left).toBe("56px"); // 40 padding + 2 cells
  });

  it("offsets the pixel position reported to a consumer's IME view", () => {
    attach({ padding: "9px" });
    render.updateFontMetrics();
    paint({ rows: [row("abcdef")], cursor: [0, 3] });
    const px = render.getCursorPx();
    expect(px.left).toBe(33); // 9 padding + 3 cells
    expect(px.cellH).toBe(17);
  });

  it("answers the content origin while the cursor has no row at all", () => {
    // The state a session wipe leaves behind (collapseContentSpaceOverlays): the
    // cursor is at -1 and the row map is empty, and the consumer's IME view and
    // hidden textarea are moved from this seam. There the content origin is the
    // accurate answer, not a row offset — the grid arithmetic that serves every
    // real row would put them a cell ABOVE the terminal's first line.
    attach({ padding: "9px" });
    render.updateFontMetrics();
    expect(render.getCursorPx().top).toBe(9);
  });
});

describe("the predicted-cursor overlay", () => {
  it("sits at the predicted cell of a row that has not been built yet", () => {
    // A consumer echoes a keystroke before the server's frame arrives, so the
    // row it names may not exist: the offset then comes from the window-
    // relative row index at the current line height.
    render.setPredictedCursor(2, 5, true);
    expect(predicted().classList.contains("visible")).toBe(true);
    expect(predicted().style.left).toBe("40px"); // 5 cells
    expect(predicted().style.top).toBe("34px"); // row 2 at a 17px line
  });

  it("hides when the consumer deactivates it", () => {
    render.setPredictedCursor(2, 5, true);
    expect(predicted().classList.contains("visible")).toBe(true);
    render.setPredictedCursor(2, 5, false);
    expect(predicted().classList.contains("visible")).toBe(false);
  });

  it("hides when the prediction has caught up with the real caret", () => {
    // Showing a prediction on top of the caret it predicted would paint two
    // cursors on one cell. It is suppressed only when BOTH coordinates match:
    // the same row one column on, or the same column one row down (the echo of
    // a newline), is still a live prediction.
    paint({ rows: [row("abcdef"), row("ghijkl")], cursor: [1, 5] });
    render.setPredictedCursor(1, 6, true);
    expect(predicted().classList.contains("visible")).toBe(true);
    render.setPredictedCursor(0, 5, true);
    expect(predicted().classList.contains("visible")).toBe(true);
    render.setPredictedCursor(1, 5, true);
    expect(predicted().classList.contains("visible")).toBe(false);
  });
});
