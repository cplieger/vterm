// Row-span construction: what render.ts paints for a run, and what it must NOT
// paint. Two properties the existing attribute tier cannot express, because it
// only ever asserts the POSITIVE direction of one attribute at a time:
//
//   - A default run (SGR 0, default colors) must paint NOTHING. An attribute
//     that leaked on by itself is invisible to a per-attribute test — every
//     "bold renders bold" assertion still passes when everything is bold.
//   - Cell-width compensation. A terminal cell is a fixed box, so a glyph the
//     font draws at any other advance is padded with letter-spacing to exactly
//     one cell (`cellWidth - measured`). That is why render.ts measures every
//     glyph per font VARIANT: a bold or italic face has its own advances, and
//     measuring the regular face for them lays the row out on the wrong grid.
//
// The font model below is therefore deliberately NOT uniform: one glyph is
// drawn narrower than a cell and the bold/italic faces are wider than the
// regular one, which is the ordinary state of affairs for a real font and the
// only condition under which the compensation is observable at all. Expected
// values come from the spec (a glyph occupies its cell; a Wide glyph occupies
// two), not from reading render.ts.

import { describe, it, expect, beforeEach, afterEach } from "vitest";
import * as render from "./render.js";
import { cssColor } from "./test-helpers/spec-colors.js";
import type { ScreenMessage, WireRun } from "./types.js";

// --- The font model ---------------------------------------------------------
//
// A monospace face whose advance is half the font size, with three deviations a
// real font also has: `i` is drawn narrow, and the bold and italic faces are
// wider than the regular one. `fontPx` is the CSS font-size the stubbed
// computed style reports, so a font CHANGE moves every advance at once.
let fontPx = 16;
const cellOf = (px: number): number => px / 2;

// Wide (UAX#11) glyphs: two cells at any size.
const WIDE = new Set(["漢", "字"]);
// Glyphs this face draws at half a cell. U+0100 is here on purpose: it is the
// first code point past render.ts's 256-entry fast width cache, and a font whose
// Latin Extended metrics differ from its ASCII ones is the ordinary case — which
// is why the renderer measures every glyph rather than assuming the cell.
const NARROW = new Set(["i", "\u0100"]);

function advance(ch: string, font: string): number {
  const sizeMatch = /(\d+)px/.exec(font);
  const cell = cellOf(sizeMatch === null ? fontPx : Number(sizeMatch[1]));
  let w = cell;
  if (WIDE.has(ch)) {
    w = cell * 2;
  } else if (NARROW.has(ch)) {
    w = cell / 2;
  }
  // A heavier or slanted face is drawn wider.
  if (font.includes("bold")) {
    w += 2;
  }
  if (font.includes("italic")) {
    w += 1;
  }
  return w;
}

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
  // A fresh context per variant, each remembering the font it was configured
  // with — render.ts keeps one 2D context per (bold, italic) variant and sets
  // `.font` on it once, so a stub that ignored `.font` could not tell the
  // variants apart.
  HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
    const ctx: FakeCtx = {
      font: "",
      measureText: (text: string): { width: number } => {
        let w = 0;
        for (const ch of text) {
          w += advance(ch, ctx.font);
        }
        return { width: w };
      },
    };
    return ctx;
  } as typeof HTMLCanvasElement.prototype.getContext;
  // The cell measurement reads a rect off a probe span of ten `M`s, and a real
  // font's advance depends on what the machine has installed, so the metric is
  // declared here: a width proportional to the text at the live size.
  HTMLElement.prototype.getBoundingClientRect = function fakeRect(this: HTMLElement): DOMRect {
    const width = [...(this.textContent ?? "")].length * cellOf(fontPx);
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
  // Drive the rAF-batched flush synchronously so each frame renders on return.
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

function attach(): void {
  document.body.innerHTML = `<div class="term"><div class="term-output"></div></div>`;
  termWrap = document.querySelector<HTMLDivElement>(".term")!;
  output = document.querySelector<HTMLDivElement>(".term-output")!;
  termWrap.style.fontSize = `${String(fontPx)}px`;
  termWrap.style.fontFamily = "monospace";
  termWrap.style.lineHeight = "17px";
  render.init({ output, termWrap });
  render.updateFontMetrics();
}

/** Render `runs` on the only row of a one-row screen and return its children. */
function renderRow(runs: WireRun[]): HTMLElement[] {
  const msg: ScreenMessage = {
    type: "screen",
    base: 0,
    rows: [runs],
    cursor: [0, 0],
    changed: [0],
    cursorHidden: true,
    cursorStyle: 0,
    cursorBlink: false,
  };
  render.handleScreen(msg);
  return rowSpans();
}

/** The current children of the first rendered row. */
function rowSpans(): HTMLElement[] {
  const rowEl = output.children[0] as HTMLElement;
  return Array.from(rowEl.children) as HTMLElement[];
}

function run(text: string, over: Partial<WireRun> = {}): WireRun {
  return { t: text, f: -1, b: -1, uc: -1, a: 0, ...over };
}

beforeEach(() => {
  fontPx = 16;
  installStubs();
  // init() is the attachment boundary: it installs a fresh store and wipes the
  // surface, so no separate reset is needed (and a reset before the attach
  // would flush against the previous test's detached elements).
  attach();
});

afterEach(() => {
  restoreStubs();
});

describe("a default run paints nothing", () => {
  it("SGR 0 with default colors sets no inline style and no class", () => {
    // Spec: with no attributes and no colors selected, the cell inherits the
    // terminal's own appearance. Anything pinned here is an attribute leaking
    // on for text that never asked for it — which every positive-direction
    // attribute test in the tier-1 file would still pass.
    const spans = renderRow([run("M")]);
    expect(spans.length).toBe(1);
    expect(spans[0]!.style.length).toBe(0);
    expect(spans[0]!.className).toBe("");
  });

  it("SGR 4 underline renders a single underline, not a doubled one", () => {
    // Spec: SGR 4 is one line, SGR 21 is two. The renderer spells the doubled
    // form as `underline double`, so a plain underline must carry no `double`.
    const spans = renderRow([run("M", { a: 4 })]);
    expect(spans[0]!.style.textDecoration).toBe("underline");
  });
});

describe("color index 0 is black, not 'default'", () => {
  it("renders an explicit black foreground and background", () => {
    // Spec: the wire uses -1 for "default"; 0 is a real color (black). A gate
    // that treats 0 as absent silently drops black text — invisible on a dark
    // theme, wrong on a light one.
    const spans = renderRow([run("M", { f: 0, b: 0 })]);
    // cssColor rather than a "#000000" literal: CSSOM does not round-trip the
    // authored hex, it reports the CSS Color 4 serialization, so both sides go
    // through the browser's own serializer from the same wire value.
    expect(spans[0]!.style.color).toBe(cssColor(0x000000));
    expect(spans[0]!.style.background).toBe(cssColor(0x000000));
  });

  it("keeps the colors of an inverse run that already carries them", () => {
    // The server performs the SGR 7 swap itself, so a run that arrives inverse
    // with a color already selected has been swapped. The renderer's own
    // theme-variable swap exists only for the case the server's is a no-op —
    // both colors default — and applying it here would throw the application's
    // chosen colors away.
    const withFg = renderRow([run("M", { a: 8, f: 0xff0000 })]);
    expect(withFg[0]!.style.color).toBe(cssColor(0xff0000));
    const withBg = renderRow([run("N", { a: 8, b: 0x00ff00 })]);
    expect(withBg[0]!.style.background).toBe(cssColor(0x00ff00));
  });
});

describe("an empty row still occupies a line", () => {
  it("renders a non-breaking space for a row the server sent with no runs", () => {
    // A row with no printable content is still a LINE: an element with no text
    // collapses to zero height, which would shorten the screen and misalign
    // every row below it. The renderer fills it with U+00A0.
    const spans = renderRow([]);
    expect(spans.length).toBe(1);
    expect(spans[0]!.textContent).toBe("\u00a0");
  });
});

describe("cell-width compensation", () => {
  it("pads a glyph the font draws narrower than one cell", () => {
    // The cell is 8px at this font size and `i` is drawn at 4px, so the glyph
    // carries 4px of letter-spacing to occupy exactly one cell.
    const spans = renderRow([run("i")]);
    expect(spans[0]!.style.letterSpacing).toBe("4px");
  });

  it("splits a run into separate spans where the compensation changes", () => {
    // `i` needs 4px of padding and `M` needs none, so one run of "iM" cannot be
    // a single span: letter-spacing is per-element, and one value for both
    // glyphs puts every following column on the wrong grid.
    const spans = renderRow([run("iM")]);
    expect(spans.map((s) => s.textContent)).toEqual(["i", "M"]);
    expect(spans[0]!.style.letterSpacing).toBe("4px");
    expect(spans[1]!.style.letterSpacing).toBe("");
  });

  it("measures a bold glyph with the bold face", () => {
    // The bold `M` is drawn 2px wider than the cell, so it is pulled back by
    // 2px. Measuring the regular face for it (or reading the regular face's
    // cached width) reports one exact cell and pads nothing.
    const spans = renderRow([run("M"), run("M", { a: 1 })]);
    expect(spans[0]!.style.letterSpacing).toBe("");
    expect(spans[1]!.style.letterSpacing).toBe("-2px");
  });

  it("measures an italic glyph with the italic face", () => {
    const spans = renderRow([run("M"), run("M", { a: 2 })]);
    expect(spans[0]!.style.letterSpacing).toBe("");
    expect(spans[1]!.style.letterSpacing).toBe("-1px");
  });

  it("measures a glyph past the flat-cache bound", () => {
    // U+0100 is the first code point the 256-entry fast cache does not cover, so
    // it takes the general path. It is drawn at half a cell by this face and
    // must be padded to a full one exactly like any other narrow glyph — a read
    // past the end of that cache yields no width at all and pads by NaN.
    const spans = renderRow([run("\u0100")]);
    expect(spans[0]!.style.letterSpacing).toBe("4px");
  });

  it("stretches a Wide glyph across the two cells it owns", () => {
    // UAX#11: a Wide glyph occupies two cells, delivered as the glyph plus the
    // U+FFFF continuation sentinel. At an 8px cell a 16px glyph needs no pull,
    // but it must span 2 cells = 16px, so the spacing is 16 - 16 = 0px.
    const spans = renderRow([run("漢\uFFFF")]);
    expect(spans.map((s) => s.textContent)).toEqual(["漢"]);
    expect(spans[0]!.style.letterSpacing).toBe("0px");
  });

  it("renders a row whose first cell is a stray continuation sentinel", () => {
    // The sentinel means "the previous glyph owns this cell too", so one in the
    // first cell describes a glyph that is not there. The renderer is the trust
    // boundary for decoded frames and a row is the unit it can lose: a throw
    // here aborts the whole flush, so the row must render its real text and
    // simply have nothing to widen.
    const spans = renderRow([run("\uFFFFab")]);
    expect(spans.map((s) => s.textContent)).toEqual(["ab"]);
  });
});

describe("a font change re-measures every glyph", () => {
  it("recomputes widths for both the regular and the bold face", () => {
    // Every measurement render.ts holds is keyed to the font it was taken
    // with: a per-code-point fast cache, a per-(face, glyph) map, and one
    // measuring context per face. A restyle (zoom, a web font finishing load,
    // a theme change) invalidates all three, and a consumer signals it by
    // calling updateFontMetrics. Anything left behind lays the new font out on
    // the old font's grid.
    //
    // At a 16px font: the cell is 8px, `i` is drawn at 4px (padded by 4px) and
    // bold `i` at 6px (padded by 2px).
    const before = renderRow([run("i"), run("i", { a: 1 })]);
    expect(before[0]!.style.letterSpacing).toBe("4px");
    expect(before[1]!.style.letterSpacing).toBe("2px");

    // Twice the font size: the cell becomes 12px, `i` 6px and bold `i` 8px, so
    // the padding of each grows to 6px and 4px. Rows already in the DOM were
    // laid out under the old metrics, so the repaint is what applies the new
    // ones — rebuild() is the consumer's "repaint from the store" seam.
    fontPx = 24;
    termWrap.style.fontSize = "24px";
    render.updateFontMetrics();
    render.rebuild();

    const after = rowSpans();
    expect(after[0]!.style.letterSpacing).toBe("6px");
    expect(after[1]!.style.letterSpacing).toBe("4px");
  });

  it("leaves no measuring element behind in the terminal", () => {
    // The cell measurement appends a probe span to the scroll container to pick
    // up the real font, then removes it. One left behind per call accumulates a
    // hidden absolutely-positioned element on every restyle.
    render.updateFontMetrics();
    render.updateFontMetrics();
    const strayProbes = Array.from(termWrap.children).filter((el) => el.tagName === "SPAN");
    expect(strayProbes.length).toBe(0);
  });
});

describe("the top-of-history notice", () => {
  it("announces trimmed history above the oldest line it holds", () => {
    // The store holds line 10 upward and the server has confirmed it retains
    // nothing older, so what is on screen is NOT the start of the session. The
    // notice says so, and it says so to a screen reader too: a sighted reader
    // sees the row, and one who does not gets nothing from an unlabelled div.
    render.handleScreen({
      type: "screen",
      base: 10,
      rows: [
        [{ t: "ten", f: -1, b: -1, a: 0, uc: -1 }],
        [{ t: "eleven", f: -1, b: -1, a: 0, uc: -1 }],
      ],
      cursor: [0, 0],
      changed: [0, 1],
      cursorHidden: true,
      cursorStyle: 0,
      cursorBlink: false,
    });
    render.noteResumeBounds(12, 10);

    const marker = output.querySelector<HTMLElement>(".term-trim-marker");
    expect(marker, "the trim marker must be rendered").not.toBeNull();
    expect(marker!.textContent).toBe("earlier output trimmed");
    expect(marker!.getAttribute("role")).toBe("status");
    expect(marker!.getAttribute("aria-label")).toBe("earlier output trimmed");
    // It describes everything above the oldest row, so it sits above them all.
    expect(output.firstElementChild).toBe(marker);
  });
});
