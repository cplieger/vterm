// Behaviours of render.ts that the existing tiers exercise but never assert.
// Four areas, each one a contract a reader can check without opening render.ts:
//
//   - Row geometry. Every overlay the renderer owns (the caret, the predicted
//     cursor, and the position it reports to a consumer's IME view) resolves a
//     row's top in the SCROLL CONTAINER's coordinate space, which is not the
//     row's `offsetTop`: the stylesheet makes the row container a positioned
//     element, so rows report offsets in ITS space, and the missing term is the
//     container's own offset plus any border a wrapper grows. The model below
//     declares that geometry — non-uniform rows, a positioned row container, a
//     bordered wrapper — rather than measuring it, because the coordinate
//     arithmetic IS the subject and a real box would be whatever this file's
//     fixture markup happens to lay out at.
//   - The caret's cell ownership. A Wide glyph owns two cells and the caret
//     covers both; the decision is a measurement against the cell, so its
//     BOUNDARY and the FACE it measures with are both observable.
//   - The alternate screen. Entering it replaces the whole scrollback with one
//     ephemeral grid; leaving it rebuilds from the store. In between, a frame
//     that repaints a few rows must touch only those rows' DOM.
//   - Row order and the markers projected into it. `output`'s children are kept
//     in ascending `data-abs` order, and the gap markers are re-derived from the
//     store's geometry every flush, so a marker moves when its gap's edge does.
//
// Expected numbers come from the layout model declared here, never from running
// render.ts.

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import * as render from "./render.js";
import * as scroll from "./scroll.js";
import { LineStore, PAGE_SIZE } from "./store.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

// A real browser's ESM module namespace is non-configurable, so `vi.spyOn` on a
// module export cannot install itself: the property is not redefinable. The
// emulator ran behind a transform that rewrote exports into configurable
// getters, which is why these spies used to work without this line.
//
// `spy: true` asks vitest for the module with its exports wrapped in spies that
// CALL THROUGH by default, so nothing is stubbed out wholesale — the same
// scroll.ts runs, and only the exports a test explicitly overrides behave
// differently. render.ts imports the same mocked module, which is what makes
// this the parked-reader seam these tests need.
//
// One consequence to know when reading the call-count assertions below: an
// auto-spy starts recording at module load, and `vi.spyOn` on a property that is
// already a mock hands back THAT spy rather than a fresh one. So a spy taken
// mid-test carries whatever the setup above it already did, and every site whose
// assertion is a COUNT clears it first (`.mockClear()`) to mean what it says —
// the calls the action under test causes.
vi.mock("./scroll.js", { spy: true });

// --- The font model ---------------------------------------------------------
//
// A monospace face at an 8px cell, with two deviations that make the caret's
// cell-ownership decision observable: one Wide glyph (two cells) and one glyph
// drawn at EXACTLY one and a half cells, which is the boundary of the
// "is this double-width?" test. The bold and italic faces are drawn wider, as
// a real font's are.
const CELL_PX = 8;
const LINE_PX = 17;
const ADVANCE = new Map<string, number>([
  ["漢", CELL_PX * 2],
  // U+018E, drawn at 1.5 cells: neither one cell nor two.
  ["\u018e", CELL_PX * 1.5],
]);

function advance(ch: string, font: string): number {
  let w = ADVANCE.get(ch) ?? CELL_PX;
  if (font.includes("bold")) {
    w += 2;
  }
  if (font.includes("italic")) {
    w += 1;
  }
  return w;
}

// --- The layout model -------------------------------------------------------
//
// termWrap (the positioned scroll container)
//   └── output (position: relative, offset 7px inside it, 3px top border)
//         └── one row per line, 13px tall
//
// Rows are 13px tall on purpose: the renderer's fallback for a row it has NOT
// built is uniform-grid arithmetic at the CELL height (17px), so a layout whose
// rows are a different height is the only one under which "answered from the
// row element" and "answered from the grid" are distinguishable.
const ROW_PX = 13;
const OUTPUT_OFFSET_TOP = 7;
const OUTPUT_BORDER_TOP = 3;
const TERMWRAP_OFFSET_TOP = 29;
const TERMWRAP_BORDER_TOP = 5;
// Viewport-relative rects, for the space-independent fallback path.
const ROW_RECT_TOP = 500;
const TERMWRAP_RECT_TOP = 100;

/** The top of the row for `abs`, in termWrap space, per the model above. */
function modelRowTop(abs: number): number {
  return abs * ROW_PX + OUTPUT_BORDER_TOP + OUTPUT_OFFSET_TOP;
}

/** The same row's top derived from rects, which is what an offset chain that
 *  never reaches termWrap (display:none, detached) has to fall back to. */
function modelRectRowTop(abs: number, scrollTop: number): number {
  return ROW_RECT_TOP + abs * ROW_PX - TERMWRAP_RECT_TOP + scrollTop;
}

let output: HTMLDivElement;
let termWrap: HTMLDivElement;
/** When set, the offset-parent chain never reaches termWrap: what a browser
 *  reports for a row inside a `display: none` subtree. */
let offsetChainBroken = false;
/** "sync" runs a scheduled frame on return from the call that scheduled it;
 *  "manual" holds them for runFrame(). */
let frameMode: "sync" | "manual" = "sync";
const queuedFrames: FrameRequestCallback[] = [];
/** History requests the renderer made, as [fromAbs, maxLines]. */
let requests: [number, number][] = [];
/** What the transport says it can carry right now. */
let historyBudget = PAGE_SIZE;

const patched: [string, PropertyDescriptor | undefined][] = [];

function patchProto(name: string, get: (this: HTMLElement) => unknown): void {
  patched.push([name, Object.getOwnPropertyDescriptor(HTMLElement.prototype, name)]);
  Object.defineProperty(HTMLElement.prototype, name, { configurable: true, get });
}

function restoreProtos(): void {
  for (const [name, desc] of patched.reverse()) {
    if (desc === undefined) {
      Reflect.deleteProperty(HTMLElement.prototype, name);
    } else {
      Object.defineProperty(HTMLElement.prototype, name, desc);
    }
  }
  patched.length = 0;
}

interface FakeCtx {
  font: string;
  measureText: (t: string) => { width: number };
}

/** Every text measurement the renderer took, with the face it took it with:
 *  the two-tier width cache is observable only through the traffic it
 *  suppresses. */
const measurements: { text: string; font: string }[] = [];
/** How many measuring canvases the renderer has asked for. */
let contextsCreated = 0;

/** How many times `text` was measured with the given face. */
function timesMeasured(text: string, face: "base" | "bold" | "italic"): number {
  return measurements.filter(
    (m) =>
      m.text === text &&
      m.font.includes("bold") === (face === "bold") &&
      m.font.includes("italic") === (face === "italic"),
  ).length;
}

let realGetContext: typeof HTMLCanvasElement.prototype.getContext;
let realRect: typeof HTMLElement.prototype.getBoundingClientRect;
let realRAF: typeof globalThis.requestAnimationFrame;
let realCAF: typeof globalThis.cancelAnimationFrame;

function installStubs(): void {
  realGetContext = HTMLCanvasElement.prototype.getContext;
  realRect = HTMLElement.prototype.getBoundingClientRect;
  // One context per font variant, each remembering the font it was configured
  // with, so a measurement taken with the wrong face is visible.
  HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
    contextsCreated++;
    const ctx: FakeCtx = {
      font: "",
      measureText: (text: string): { width: number } => {
        measurements.push({ text, font: ctx.font });
        let w = 0;
        for (const ch of text) {
          w += advance(ch, ctx.font);
        }
        return { width: w };
      },
    };
    return ctx;
  } as typeof HTMLCanvasElement.prototype.getContext;
  HTMLElement.prototype.getBoundingClientRect = function fakeRect(this: HTMLElement): DOMRect {
    const width = [...(this.textContent ?? "")].length * CELL_PX;
    let top = 0;
    if (this === termWrap) {
      top = TERMWRAP_RECT_TOP;
    } else {
      const abs = this.dataset["abs"];
      if (abs !== undefined) {
        top = ROW_RECT_TOP + Number(abs) * ROW_PX;
      }
    }
    return {
      x: 0,
      y: top,
      width,
      height: LINE_PX,
      top,
      left: 0,
      right: width,
      bottom: top + LINE_PX,
      toJSON: () => ({}),
    } as DOMRect;
  };
  patchProto("offsetTop", function offsetTop(this: HTMLElement): number {
    if (offsetChainBroken) {
      return 0;
    }
    const abs = this.dataset["abs"];
    if (abs !== undefined) {
      return Number(abs) * ROW_PX;
    }
    if (this === output) {
      return OUTPUT_OFFSET_TOP;
    }
    if (this === termWrap) {
      return TERMWRAP_OFFSET_TOP;
    }
    return 0;
  });
  patchProto("offsetParent", function offsetParent(this: HTMLElement): Element | null {
    return offsetChainBroken ? null : this.parentElement;
  });
  patchProto("clientTop", function clientTop(this: HTMLElement): number {
    if (this === output) {
      return OUTPUT_BORDER_TOP;
    }
    if (this === termWrap) {
      return TERMWRAP_BORDER_TOP;
    }
    return 0;
  });
  // Drive the rAF-batched flush synchronously so each frame renders on return.
  realRAF = globalThis.requestAnimationFrame;
  realCAF = globalThis.cancelAnimationFrame;
  globalThis.requestAnimationFrame = ((cb: FrameRequestCallback): number => {
    if (frameMode === "manual") {
      queuedFrames.push(cb);
      return undefined as unknown as number;
    }
    cb(0);
    return undefined as unknown as number;
  }) as typeof globalThis.requestAnimationFrame;
  globalThis.cancelAnimationFrame = (() => undefined) as typeof globalThis.cancelAnimationFrame;
}

/** Run exactly one scheduled frame. Only meaningful in "manual" frame mode,
 *  which is how a per-frame budget becomes observable at all: under the
 *  synchronous stub a flush that reschedules itself drains the whole backlog
 *  before returning. */
function runFrame(): void {
  const cb = queuedFrames.shift();
  expect(cb, "a frame must be scheduled").not.toBeUndefined();
  cb!(0);
}

/** Run every frame scheduled so far, including ones they schedule in turn. */
function drainFrames(): void {
  let guard = 0;
  while (queuedFrames.length > 0) {
    expect((guard += 1), "the drain must terminate").toBeLessThan(100);
    queuedFrames.shift()!(0);
  }
}

function restoreStubs(): void {
  restoreProtos();
  HTMLCanvasElement.prototype.getContext = realGetContext;
  HTMLElement.prototype.getBoundingClientRect = realRect;
  globalThis.requestAnimationFrame = realRAF;
  globalThis.cancelAnimationFrame = realCAF;
}

function attach(
  opts: { padding?: string; onCursorMove?: () => void; paging?: boolean } = {},
): void {
  document.body.innerHTML = `<div class="term"><div class="term-output"></div></div>`;
  termWrap = document.querySelector<HTMLDivElement>(".term")!;
  output = document.querySelector<HTMLDivElement>(".term-output")!;
  termWrap.style.fontSize = "16px";
  termWrap.style.fontFamily = "monospace";
  termWrap.style.padding = opts.padding ?? "0px";
  termWrap.style.lineHeight = `${String(LINE_PX)}px`;
  // scrollTop belongs to the declared model, like the rects and offsets above
  // it. A real container with no overflow cannot scroll, so a real browser
  // clamps `termWrap.scrollTop = 25` straight back to 0 — and the rect-delta
  // fallback then measures a model row against a live offset of zero, which is
  // neither the model's answer nor a meaningful one. An own property on this
  // element (never a prototype patch, so no other element in the page is
  // affected) keeps the whole coordinate model in one place; it stores 0 by
  // default, which is what every other test here already assumed.
  let scrollTop = 0;
  Object.defineProperty(termWrap, "scrollTop", {
    configurable: true,
    get: () => scrollTop,
    set: (v: number) => {
      scrollTop = v;
    },
  });
  render.init({
    output,
    termWrap,
    ...(opts.onCursorMove === undefined ? {} : { onCursorMove: opts.onCursorMove }),
    ...(opts.paging === true
      ? {
          requestHistory: (fromAbs: number, maxLines: number): boolean => {
            requests.push([fromAbs, maxLines]);
            return true;
          },
          historyBudget: (): number => historyBudget,
        }
      : {}),
  });
  render.updateFontMetrics();
}

/** Park the reader on the row holding `abs`: the renderer resolves the reading
 *  position by searching the rows for the first one at or below the scroll
 *  offset, so a position is a (follow state, offset) pair. */
function parkReaderAt(abs: number): void {
  vi.spyOn(scroll, "isUserScrolledUp").mockReturnValue(true);
  vi.spyOn(scroll, "currentScrollTop").mockReturnValue(abs * ROW_PX);
}

function run(text: string, over: Partial<WireRun> = {}): WireRun {
  return { t: text, f: -1, b: -1, uc: -1, a: 0, ...over };
}

function row(text: string): WireRun[] {
  return [run(text)];
}

interface FrameOpts {
  rows: WireRun[][];
  cursor?: [number, number];
  base?: number;
  changed?: number[];
  cursorHidden?: boolean;
  cursorBlink?: boolean;
  altActive?: boolean;
}

function paint(opts: FrameOpts): void {
  const msg: ScreenMessage = {
    type: "screen",
    base: opts.base ?? 0,
    rows: opts.rows,
    cursor: opts.cursor ?? [0, 0],
    changed: opts.changed ?? opts.rows.map((_, i) => i),
    cursorHidden: opts.cursorHidden ?? false,
    cursorStyle: 0,
    cursorBlink: opts.cursorBlink ?? false,
    ...(opts.altActive === undefined ? {} : { altActive: opts.altActive }),
  };
  render.handleScreen(msg);
}

function scrollMsg(firstIndex: number, count: number): ScrollMessage {
  return {
    type: "scroll",
    firstIndex,
    lines: Array.from({ length: count }, (_, i) => row(`L${String(firstIndex + i)}`)),
  };
}

function caret(): HTMLElement {
  const el = document.querySelector<HTMLElement>(".term-cursor-overlay");
  expect(el, "the caret overlay must exist").not.toBeNull();
  return el!;
}

/** `output`'s children as `data-abs` values, `null` for a child with none. */
function childAbs(): (number | null)[] {
  return [...output.children].map((el) => {
    const abs = (el as HTMLElement).dataset["abs"];
    return abs === undefined ? null : Number(abs);
  });
}

/** The absolute indices of the CONTENT rows in `output`, in document order
 *  (markers carry a `data-abs` of their own and are not rows). */
function rowIndices(): number[] {
  return [...output.querySelectorAll<HTMLElement>(".term-row")].map((el) =>
    Number(el.dataset["abs"]),
  );
}

beforeEach(() => {
  offsetChainBroken = false;
  frameMode = "sync";
  queuedFrames.length = 0;
  requests = [];
  historyBudget = PAGE_SIZE;
  installStubs();
  attach();
  // After attach: its updateFontMetrics drops both width tiers and every
  // measuring context, so a count taken before it would describe the previous
  // test's font.
  measurements.length = 0;
  contextsCreated = 0;
});

afterEach(() => {
  restoreStubs();
});

describe("a row's top is resolved in the scroll container's space", () => {
  it("adds the row container's own offset and border to the row's offset", () => {
    // The row reports 2 * 13px inside `output`, which sits 7px into termWrap
    // behind a 3px border. Anything that stops at the row's own offsetTop puts
    // every overlay 10px above the glyph it describes.
    paint({ rows: [row("zero"), row("one"), row("two")], cursor: [2, 0] });
    expect(render.getCursorPx().top).toBe(modelRowTop(2));
    expect(caret().style.top).toBe(`${String(modelRowTop(2))}px`);
  });

  it("stops accumulating at the scroll container, not above it", () => {
    // termWrap is where the overlays' coordinate space begins, so its own
    // offset inside the page (29px here) and its border are NOT part of a row's
    // top. A walk that runs past it lands the caret 29px+ down the page.
    paint({ rows: [row("zero"), row("one")], cursor: [1, 0] });
    expect(render.getCursorPx().top).toBe(modelRowTop(1));
    expect(render.getCursorPx().top).toBeLessThan(TERMWRAP_OFFSET_TOP + modelRowTop(1));
  });

  it("answers from the row's own box, not the uniform grid, for a built row", () => {
    // Rows are 13px in this layout and a cell is 17px, so the grid arithmetic
    // the renderer uses for a row it has NOT built is the wrong answer for one
    // it has. Real rows are not uniform either (a wrapped line, a marker above
    // them), which is why the built row's own offset is authoritative.
    paint({ rows: [row("zero"), row("one"), row("two")], cursor: [2, 0] });
    const gridTop = 2 * LINE_PX;
    expect(modelRowTop(2)).not.toBe(gridTop);
    expect(render.getCursorPx().top).toBe(modelRowTop(2));
  });

  it("falls back to a rect delta when the offset chain never reaches the container", () => {
    // display:none, a detached row, or termWrap unpositioned: the offset chain
    // ends somewhere else entirely and the accumulated sum is meaningless. The
    // rect delta is space-independent, and re-basing it by scrollTop is what
    // turns a VIEWPORT-relative measurement into the CONTENT-relative one the
    // overlays live in.
    paint({ rows: [row("zero"), row("one"), row("two")], cursor: [2, 0] });
    offsetChainBroken = true;
    termWrap.scrollTop = 25;
    expect(render.getCursorPx().top).toBe(modelRectRowTop(2, 25));
  });
});

describe("the caret covers the cells its glyph owns", () => {
  it("covers one cell for a glyph the font draws at exactly one and a half", () => {
    // Cell ownership is a property of the CHARACTER (UAX#11), and the renderer
    // decides it by measuring against 1.5 cells. A glyph drawn at exactly that
    // width is a font that draws a single-cell character wide, not a Wide
    // character: the caret covers one cell, and a `>=` there paints the block
    // caret over the neighbouring column's glyph.
    paint({ rows: [row("\u018eab")], cursor: [0, 0] });
    expect(caret().textContent).toBe("\u018e");
    expect(caret().style.width).toBe(`${String(CELL_PX)}px`);
  });

  it("decides on the base face, so bold and italic do not widen the cell", () => {
    // The bold face draws this glyph at 14px and the italic face at 13px, both
    // past the 12px threshold, while the character still owns exactly one cell.
    // Measuring the run's own face here would make the caret cover two columns
    // for emphasised text and one for the same character in plain text.
    paint({ rows: [[run("\u018eab", { a: 1 })]], cursor: [0, 0] });
    expect(caret().style.width).toBe(`${String(CELL_PX)}px`);
    paint({ rows: [[run("\u018eab", { a: 2 })]], cursor: [0, 0] });
    expect(caret().style.width).toBe(`${String(CELL_PX)}px`);
  });

  it("covers both cells of a Wide glyph", () => {
    paint({ rows: [row("漢\uFFFFb")], cursor: [0, 0] });
    expect(caret().style.width).toBe(`${String(CELL_PX * 2)}px`);
  });
});

describe("the overlays are hidden from assistive technology", () => {
  it("marks the caret aria-hidden, so its copy of the glyph is not read twice", () => {
    // The block caret carries a COPY of the character under it as its text
    // content. Exposed to a screen reader, every cursor move announces that
    // character a second time on top of the row that already contains it.
    paint({ rows: [row("abc")], cursor: [0, 1] });
    expect(caret().textContent).toBe("b");
    expect(caret().getAttribute("aria-hidden")).toBe("true");
  });

  it("marks the predicted-cursor overlay aria-hidden", () => {
    render.setPredictedCursor(1, 2, true);
    const el = document.querySelector<HTMLElement>(".pred-cursor");
    expect(el, "the predicted-cursor overlay must exist").not.toBeNull();
    expect(el!.getAttribute("aria-hidden")).toBe("true");
  });
});

describe("the terminal's padding is cached until re-measured", () => {
  it("re-reads the padding on updateFontMetrics and not after it", () => {
    // The overlay positioners need the padding on every flush, and a live
    // computed-style read there forces a style recalc, so it is cached and the
    // consumer's `updateFontMetrics` is the documented refresh. Both halves are
    // asserted: the call picks a restyle up, and a later restyle without a call
    // does not leak in.
    attach({ padding: "0px" });
    paint({ rows: [row("abc")], cursor: [0, 0] });
    expect(render.getCursorPx().top).toBe(modelRowTop(0));

    termWrap.style.padding = "9px";
    render.updateFontMetrics();
    expect(render.getCursorPx().left).toBe(9);

    termWrap.style.padding = "40px";
    expect(render.getCursorPx().left).toBe(9);
  });
});

describe("the alternate screen replaces the buffer it covers", () => {
  it("paints the grid instead of the absolute rows it was showing", () => {
    // Alt rows are ephemeral grid positions, not history: they carry no absolute
    // index, and the scrollback they cover is gone from the DOM for as long as
    // the TUI is up. The grid here is deliberately the same HEIGHT as the main
    // screen, which is the ordinary case (a full-screen app on the same
    // terminal) and the one where a length comparison alone cannot tell that a
    // rebuild is needed.
    paint({ rows: [row("m0"), row("m1"), row("m2")], cursor: [0, 0] });
    expect(childAbs()).toEqual([0, 1, 2]);

    paint({ rows: [row("t0"), row("t1"), row("t2")], cursor: [0, 0], altActive: true });
    expect(childAbs()).toEqual([null, null, null]);
    expect([...output.children].map((el) => el.textContent)).toEqual(["t0", "t1", "t2"]);
  });

  it("positions the caret on the grid row, having dropped the row map", () => {
    // The row map holds MAIN-screen rows, whose elements are no longer in the
    // document once the grid replaces them. Left in place, the caret would
    // resolve its row through one of those detached elements; the grid is
    // uniform, so the honest answer is the cell height times the grid row.
    paint({ rows: [row("m0"), row("m1"), row("m2")], cursor: [0, 0] });
    paint({ rows: [row("t0"), row("t1"), row("t2")], cursor: [2, 0], altActive: true });
    expect(caret().style.top).toBe(`${String(2 * LINE_PX)}px`);
  });

  it("keeps the elements of the rows a frame did not change", () => {
    // A TUI that repaints two lines (a progress bar, a vim edit) must not cost a
    // full-screen DOM rebuild. Element identity is the observable: a rebuilt row
    // is a new element, which drops any native selection inside it and forces
    // layout for rows whose content is unchanged.
    paint({ rows: [row("t0"), row("t1"), row("t2")], cursor: [0, 0], altActive: true });
    const before = [...output.children];

    paint({
      rows: [row("t0"), row("changed"), row("t2")],
      changed: [1],
      cursor: [0, 0],
      altActive: true,
    });
    expect([...output.children]).toEqual(before);
  });

  it("repaints the row a frame did change", () => {
    paint({ rows: [row("t0"), row("t1"), row("t2")], cursor: [0, 0], altActive: true });
    paint({
      rows: [row("t0"), row("changed"), row("t2")],
      changed: [1],
      cursor: [0, 0],
      altActive: true,
    });
    expect([...output.children].map((el) => el.textContent)).toEqual(["t0", "changed", "t2"]);
  });

  it("rebuilds the whole grid when its height changes", () => {
    // A resize while the TUI is up: the row count no longer matches the DOM, so
    // a per-row reconcile has no element to update for the new rows.
    paint({ rows: [row("t0"), row("t1")], cursor: [0, 0], altActive: true });
    paint({
      rows: [row("r0"), row("r1"), row("r2"), row("r3")],
      cursor: [0, 0],
      altActive: true,
    });
    expect([...output.children].map((el) => el.textContent)).toEqual(["r0", "r1", "r2", "r3"]);
  });

  it("tells the consumer the cursor moved on an alt frame too", () => {
    // The consumer's IME view and hidden textarea follow the caret, and the alt
    // branch returns before the shared end-of-flush bookkeeping, so this hook is
    // wired there separately or not at all.
    const moves = vi.fn();
    attach({ onCursorMove: moves });
    paint({ rows: [row("m0")], cursor: [0, 0] });
    const beforeAlt = moves.mock.calls.length;
    paint({ rows: [row("t0"), row("t1")], cursor: [1, 0], altActive: true });
    expect(moves.mock.calls.length).toBeGreaterThan(beforeAlt);
  });

  it("rebuilds the absolute buffer from the store on exit", () => {
    // Leaving the TUI restores the transcript, INCLUDING the scrollback the grid
    // covered — which no frame re-sends, so it can only come from the store. A
    // rebuild that only repainted the exit frame's own rows would drop every
    // line above the window.
    render.handleScroll(scrollMsg(0, 5));
    paint({ rows: [row("W5"), row("W6")], base: 5, cursor: [0, 0] });
    expect(childAbs()).toEqual([0, 1, 2, 3, 4, 5, 6]);

    paint({ rows: [row("t0"), row("t1")], base: 5, cursor: [0, 0], altActive: true });
    expect(childAbs()).toEqual([null, null]);

    paint({ rows: [row("W5"), row("W6")], base: 5, cursor: [0, 0], altActive: false });
    expect(childAbs()).toEqual([0, 1, 2, 3, 4, 5, 6]);
  });
});

describe("rows are kept in ascending absolute order", () => {
  it("splices a history page in above the rows already on screen", () => {
    // Pages arrive newest-first (the reader scrolls up), so an insert is
    // routinely BELOW every row in the DOM. Appending in arrival order would
    // read the transcript backwards, and it would also break the binary
    // searches over `output`'s children that resolve a reading position.
    render.handleScroll(scrollMsg(100, 3));
    paint({ rows: [row("W103")], base: 103, cursor: [0, 0] });
    expect(rowIndices()).toEqual([100, 101, 102, 103]);

    render.handleScroll(scrollMsg(50, 2));
    expect(rowIndices()).toEqual([50, 51, 100, 101, 102, 103]);
  });
});

describe("a gap marker is a projection of its gap", () => {
  beforeEach(() => {
    attach({ paging: true });
  });

  /** Two retained regions with a hole between them, the reader at the top one. */
  function twoRegions(): void {
    render.handleScroll(scrollMsg(0, 5)); // [0, 5)
    render.handleScroll(scrollMsg(100, 5)); // [100, 105)
    paint({ rows: [row("W105"), row("W106")], base: 105, cursor: [0, 0] });
  }

  function marker(): HTMLElement {
    const el = output.querySelector<HTMLElement>(".term-gap-marker");
    expect(el, "the gap marker must be rendered").not.toBeNull();
    return el!;
  }

  /** The absolute index of the row immediately below the marker. */
  function markerSitsAbove(): string | undefined {
    const next = marker().nextElementSibling as HTMLElement | null;
    return next?.dataset["abs"];
  }

  it("announces itself to a screen reader as well as to a sighted reader", () => {
    // A sighted reader sees the row; a reader who does not gets nothing from an
    // unlabelled div, and the whole point of the marker is that the two regions
    // must not read as contiguous.
    twoRegions();
    expect(marker().getAttribute("role")).toBe("status");
    expect(marker().getAttribute("aria-label")).toBe("earlier output not loaded");
  });

  it("moves up with its gap's high edge when the hole heals from the top", () => {
    // The gap [5,100) becomes [5,50) when the rows above 50 arrive, so the
    // marker belongs above row 50 — the hole it describes is now entirely below
    // them. Left where it was, it would claim the newly-arrived rows are older
    // than the hole, which is the splice the marker exists to prevent.
    twoRegions();
    expect(markerSitsAbove()).toBe("100");

    render.noteSolicited(50, 100);
    render.handleHistoryReply(scrollMsg(50, 50), null);
    render.clearSolicited();
    expect(markerSitsAbove()).toBe("50");
  });

  it("survives a flush that did not move either of its edges", () => {
    // The marker is a live region (role=status). Re-creating it on every flush
    // re-announces an unchanged fact to a screen reader on each of the 60 frames
    // a second a busy session produces.
    twoRegions();
    const first = marker();
    paint({ rows: [row("W105"), row("W106b")], base: 105, cursor: [0, 0] });
    expect(marker()).toBe(first);
  });
});

describe("the fetch controller asks for exactly the hole it can reach", () => {
  beforeEach(() => {
    attach({ paging: true });
  });

  /** Two retained regions with `hole` lines missing between them, and a window
   *  above both. Returns the gap as [lo, hi). */
  function holeOf(lo: number, hole: number): { lo: number; hi: number } {
    render.handleScroll(scrollMsg(0, lo)); // [0, lo)
    const hi = lo + hole;
    render.handleScroll(scrollMsg(hi, 5)); // [hi, hi + 5)
    paint({ rows: [row("w0"), row("w1")], base: hi + 5, cursor: [0, 0] });
    return { lo, hi };
  }

  it("asks for the page ENDING at the hole when the reader is on its top edge", () => {
    // Approach-anchored: the rows the reader is about to reach are the ones
    // fetched, so a wide hole heals from the side being read. The reader sitting
    // exactly ON the high edge is still approaching from below it — anchoring
    // the other way would land the page a whole hole-width away and leave the
    // rows under them blank while the far end filled in.
    const gap = holeOf(5, 95);
    historyBudget = 20;
    requests = [];
    parkReaderAt(gap.hi);
    render.maybeFetchHistory();
    expect(requests.length).toBe(1);
    const [fromAbs, maxLines] = requests[0]!;
    expect(fromAbs + maxLines).toBe(gap.hi);
  });

  it("starts at the paging floor when the reader is above the hole", () => {
    // Approaching from above, the request starts at the hole's low edge — but
    // never below the floor the server has already proved holds nothing, or
    // every retry re-asks for rows that cannot come back.
    const gap = holeOf(5, 95);
    // The server answered an earlier request for [5, 50) with nothing.
    render.handleHistoryReply(scrollMsg(5, 0), 50);
    historyBudget = 20;
    requests = [];
    parkReaderAt(gap.lo - 1);
    render.maybeFetchHistory();
    expect(requests.length).toBe(1);
    expect(requests[0]![0]).toBe(50);
  });

  it("never asks for more lines than the hole holds", () => {
    // A three-line hole with a full page of budget: asking for the budget would
    // request rows the store already has, which the apply path then has to
    // re-classify against the retained window for no reason.
    holeOf(5, 3);
    historyBudget = PAGE_SIZE;
    requests = [];
    parkReaderAt(4);
    render.maybeFetchHistory();
    expect(requests.length).toBe(1);
    expect(requests[0]![1]).toBe(3);
  });

  it("still asks when only one line is missing", () => {
    // The smallest real hole. A guard that skipped it would leave a permanent
    // one-line hole with a marker over it and no request that can ever close it.
    holeOf(5, 1);
    historyBudget = PAGE_SIZE;
    requests = [];
    parkReaderAt(4);
    render.maybeFetchHistory();
    expect(requests).toEqual([[5, 1]]);
  });

  it("still asks when the transport can carry only one line", () => {
    // The adaptive budget shrinks under pressure. One line is a legal request,
    // and refusing it stalls paging for as long as the transport stays slow.
    const gap = holeOf(5, 95);
    historyBudget = 1;
    requests = [];
    parkReaderAt(gap.hi);
    render.maybeFetchHistory();
    expect(requests).toEqual([[gap.hi - 1, 1]]);
  });

  it("asks for nothing when the floor has condemned the whole hole", () => {
    // The floor is at the hole's high edge: every row in it is proven gone, so
    // there is no range left to request. Sending the empty range instead would
    // put a zero-length request on the wire on every trigger, forever.
    const gap = holeOf(5, 95);
    render.handleHistoryReply(scrollMsg(5, 0), gap.hi);
    historyBudget = 20;
    requests = [];
    parkReaderAt(gap.hi);
    render.maybeFetchHistory();
    expect(requests).toEqual([]);
  });
});

describe("the caret blink survives a flush", () => {
  beforeEach(() => {
    // Stand the interval down under REAL timers first: the attachment in the
    // outer hook started one, and a real timer cannot be cleared through the
    // fake API that replaces the globals below.
    paint({ rows: [row("x")], cursor: [0, 0], cursorBlink: false });
    // Fake ONLY the interval APIs: vitest's default set replaces
    // requestAnimationFrame too, and the harness's own rAF stub is what makes a
    // frame render on return from the call that scheduled it.
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("does not restart the phase on a frame that changed no blink state", () => {
    // The blink interval is reconciled on EVERY flush, which is only safe
    // because an unchanged mode is a no-op. Reconfiguring regardless resets the
    // phase to solid on every frame, so a busy terminal's caret never blinks at
    // all — it is re-solidified faster than the 530ms toggle.
    paint({ rows: [row("abc")], cursor: [0, 0], cursorBlink: true });
    vi.advanceTimersByTime(530);
    expect(termWrap.classList.contains("cursor-blink-off")).toBe(true);

    paint({ rows: [row("abd")], cursor: [0, 0], cursorBlink: true });
    expect(termWrap.classList.contains("cursor-blink-off")).toBe(true);
  });
});

describe("re-attaching leaves nothing behind", () => {
  beforeEach(() => {
    frameMode = "manual";
  });

  it("wipes the rows off a surface it re-attaches to", () => {
    // init is the attachment boundary and installs a fresh store, so nothing
    // repaints the rows that are on screen: they have to go with the old store.
    paint({ rows: [row("a"), row("b")], cursor: [0, 0] });
    runFrame();
    expect(output.children.length).toBe(2);
    render.init({ output, termWrap });
    expect(output.children.length).toBe(0);
  });

  it("takes its overlays off the surface it leaves", () => {
    // The caret and the predicted cursor are renderer-owned children of the
    // scroll container. One left behind on a torn-down surface keeps that
    // element's subtree alive and, if the consumer re-attaches to the same DOM
    // later, paints a second caret.
    paint({ rows: [row("a")], cursor: [0, 0] });
    runFrame();
    render.setPredictedCursor(1, 2, true);
    const oldWrap = termWrap;
    expect(oldWrap.querySelector(".term-cursor-overlay")).not.toBeNull();
    expect(oldWrap.querySelector(".pred-cursor")).not.toBeNull();

    attach();
    expect(oldWrap.querySelector(".term-cursor-overlay")).toBeNull();
    expect(oldWrap.querySelector(".pred-cursor")).toBeNull();
  });

  it("drops a view restore armed against the surface it leaves", () => {
    // An armed restore is re-asserted on later frames, so it holds a write of
    // scrollTop against the row it named. Surviving the attachment boundary,
    // that write lands on a surface the consumer has already torn down.
    paint({ rows: [row("a"), row("b"), row("c")], cursor: [0, 0] });
    runFrame();
    parkReaderAt(1);
    render.updateFontMetrics();
    expect(render.pendingRestoreAbs()).toBe(1);

    render.init({ output, termWrap });
    expect(render.pendingRestoreAbs()).toBeNull();
  });
});

describe("a restyle re-anchors a holding reader, not a following one", () => {
  beforeEach(() => {
    frameMode = "manual";
  });

  it("arms a restore for the line a holding reader is on", () => {
    // A font or line-height change rescales every row offset, so the reader's
    // line moves under them with nothing to correct it — a pure restyle need not
    // schedule a flush at all. The line index is what the reader cares about, so
    // it is captured BEFORE the metrics change and re-asserted on later frames
    // until the row it names has actually been built.
    paint({ rows: [row("a"), row("b"), row("c")], cursor: [0, 0] });
    runFrame();
    parkReaderAt(2);
    render.updateFontMetrics();
    expect(render.pendingRestoreAbs()).toBe(2);
  });

  it("arms nothing for a following reader", () => {
    // The bottom pin already owns a following viewport's position, and an arm
    // there would fight it for as long as it stayed live.
    paint({ rows: [row("a"), row("b"), row("c")], cursor: [0, 0] });
    runFrame();
    render.updateFontMetrics();
    expect(render.pendingRestoreAbs()).toBeNull();
  });
});

describe("one frame builds a bounded number of rows", () => {
  beforeEach(() => {
    frameMode = "manual";
  });

  it("leaves the row past the per-frame budget for the next frame", () => {
    // 301 rows to build behind a 300-row budget. The budget is what keeps one
    // burst — a resume replay, `cat bigfile` — from blocking paint for seconds,
    // so it is a hard limit and not an approximate one: the row past it waits.
    render.handleScroll(scrollMsg(0, 300));
    paint({ rows: [row("w300"), row("w301")], base: 300, cursor: [0, 0] });
    runFrame();
    expect(render.pendingRowCount()).toBe(1);
  });

  it("builds a window backlog from the top of the screen down", () => {
    // A window taller than the budget: the rows that build first are the ones at
    // the TOP of the screen, in reading order. Building from the bottom leaves a
    // blank band across the top of a screen the reader is looking at.
    const rows = Array.from({ length: 311 }, (_, i) => row(`w${String(i)}`));
    paint({ rows, base: 0, cursor: [0, 0] });
    runFrame();
    expect(childAbs()[0]).toBe(0);
    expect(childAbs().at(-1)).toBe(300);
  });
});

describe("the width cache measures each glyph once per face", () => {
  // A two-tier cache (a flat array for regular ASCII, a map for everything
  // else) has one observable: the measurement traffic it suppresses. An 80-cell
  // row at 60 frames a second is 4800 text-metrics calls a second if a tier
  // stops hitting, each one a synchronous layout read the browser cannot batch.

  it("measures a repeated ASCII glyph once, however many cells it fills", () => {
    paint({ rows: [row("aaaa")], cursor: [0, 0] });
    expect(timesMeasured("a", "base")).toBe(1);
  });

  it("measures a repeated emphasised glyph once", () => {
    // Bold and italic are keyed apart from the regular tier because they are
    // measured with a different face, so the second tier has to hit for them or
    // every emphasised run re-measures on every build.
    paint({ rows: [[run("bbbb", { a: 1 })]], cursor: [0, 0] });
    expect(timesMeasured("b", "bold")).toBe(1);
  });

  it("keeps a glyph's measurement when another glyph of the same face arrives", () => {
    // The cap on the second tier is a memory BOUND, not a policy: it exists so a
    // CJK-heavy session cannot accumulate keys forever. Emptying the map on every
    // insert instead holds it at one entry, which is a cache that can never hit
    // twice running.
    paint({ rows: [[run("bc", { a: 1 })], [run("b", { a: 1 })]], cursor: [0, 0] });
    expect(timesMeasured("b", "bold")).toBe(1);
  });

  it("keeps one measuring canvas per face, not one per glyph", () => {
    // Four faces exist (regular, bold, italic, both). A canvas per measurement
    // is an allocation per glyph on the row-build path, and each one is a live
    // 2D context the session never releases.
    paint({ rows: [[run("bcd", { a: 1 })]], cursor: [0, 0] });
    const afterFirstRun = contextsCreated;
    paint({ rows: [[run("efg", { a: 1 })]], cursor: [0, 0] });
    expect(contextsCreated).toBe(afterFirstRun);
  });
});

describe("a request window belongs to the store that opened it", () => {
  // A correlated page is admitted on concessions — admission below the
  // stale-re-send watermark, suppressed per-line cap enforcement, disposable
  // classification — that are paid for by a request the client currently has
  // out. So the window has to close exactly when the question stops being
  // asked, and the answer to a retracted question is dropped whole.

  it("applies a page while the window it answers is open", () => {
    render.handleScroll(scrollMsg(0, 5));
    render.noteSolicited(50, 55);
    render.handleHistoryReply(scrollMsg(50, 5), null);
    expect(rowIndices()).toEqual([0, 1, 2, 3, 4, 50, 51, 52, 53, 54]);
    expect(render.browseCacheSize()).toBe(5);
  });

  it("drops a page whose question was retracted", () => {
    render.handleScroll(scrollMsg(0, 5));
    render.noteSolicited(50, 55);
    render.clearSolicited();
    render.handleHistoryReply(scrollMsg(50, 5), null);
    expect(rowIndices()).toEqual([0, 1, 2, 3, 4]);
    expect(render.browseCacheSize()).toBe(0);
  });

  it("closes the outgoing store's window when the renderer is bound away", () => {
    // The socket that issued the request is being switched away with the store,
    // so the request cannot be answered. Left open, that range keeps a standing
    // exemption from the apply guards with no socket and no timer left to close
    // it, and any later frame in it can resurrect an evicted row.
    render.handleScroll(scrollMsg(0, 5));
    render.noteSolicited(50, 55);
    const left = render.boundStore();
    render.bind(new LineStore());
    render.bind(left);
    render.handleHistoryReply(scrollMsg(50, 5), null);
    expect(rowIndices()).toEqual([0, 1, 2, 3, 4]);
    expect(render.browseCacheSize()).toBe(0);
  });
});

describe("a resume transition answers the follow question this layer owns", () => {
  // The store used to derive "is the reader at the tail" from a window
  // descriptor the transition itself retires. This layer knows, so it answers:
  // `isUserScrolledUp`, overridden by an armed restore, which means "in history
  // at the anchor" even while the browser holds a clamped mid-rebuild offset.

  function transition(): { viewportAbs: number; following: boolean } {
    const seen = vi.spyOn(render.boundStore(), "applyResumeAck");
    render.applyResumeTransition({
      epochChanged: false,
      committed: null,
      serverOldest: null,
      paging: false,
      sentHaveThrough: render.getReplayBoundary(),
      sentReplayMax: null,
    });
    expect(seen).toHaveBeenCalledTimes(1);
    return seen.mock.calls[0]![0];
  }

  it("reports following for a reader at the live tail", () => {
    paint({ rows: [row("a"), row("b")], cursor: [0, 0] });
    expect(transition().following).toBe(true);
  });

  it("reports not-following for a reader holding a position in history", () => {
    paint({ rows: [row("a"), row("b")], cursor: [0, 0] });
    vi.spyOn(scroll, "isUserScrolledUp").mockReturnValue(true);
    expect(transition().following).toBe(false);
  });

  it("reports not-following while a restore is armed, whatever the live offset says", () => {
    // A tab switch reconnects, so this transition routinely runs mid-rebuild at
    // an offset the browser clamped to partial content. The live flag there
    // describes the transient; the armed anchor describes the position the reader
    // is regaining, and it is the one whose rows must survive.
    frameMode = "manual";
    paint({ rows: [row("a"), row("b"), row("c")], cursor: [0, 0] });
    runFrame();
    const scrolledUp = vi.spyOn(scroll, "isUserScrolledUp").mockReturnValue(true);
    vi.spyOn(scroll, "currentScrollTop").mockReturnValue(2 * ROW_PX);
    render.updateFontMetrics();
    expect(render.pendingRestoreAbs()).toBe(2);

    scrolledUp.mockReturnValue(false);
    const ack = transition();
    expect(ack.following).toBe(false);
    expect(ack.viewportAbs).toBe(2);
  });
});

describe("an attachment boundary leaves nothing of ours on the old surface", () => {
  it("takes the gap markers off the surface it re-attaches away from", () => {
    // The markers are renderer-owned children of the row container, and init
    // swaps that container, so nothing else removes them. One left behind keeps a
    // torn-down surface's subtree alive and reads, on a surface the consumer may
    // show again, as a hole in a transcript that has none.
    attach({ paging: true });
    render.handleScroll(scrollMsg(0, 5));
    render.handleScroll(scrollMsg(100, 5));
    paint({ rows: [row("W105")], base: 105, cursor: [0, 0] });
    const oldOutput = output;
    expect(oldOutput.querySelector(".term-gap-marker")).not.toBeNull();

    attach({ paging: true });
    expect(oldOutput.querySelector(".term-gap-marker")).toBeNull();
  });

  it("owes no rows for the store it left", () => {
    // `pendingRowCount` is the consumer's "still catching up" affordance. A
    // backlog carried across the boundary names rows of a store nothing will
    // build, so the affordance never clears.
    frameMode = "manual";
    render.handleScroll(scrollMsg(0, 400));
    runFrame();
    expect(render.pendingRowCount()).toBeGreaterThan(0);

    render.init({ output, termWrap });
    expect(render.pendingRowCount()).toBe(0);
  });
});

describe("a store swap starts from the incoming store alone", () => {
  it("owes no rows for the store it was bound to before", () => {
    frameMode = "manual";
    render.handleScroll(scrollMsg(0, 400));
    runFrame();
    expect(render.pendingRowCount()).toBeGreaterThan(0);

    render.bind(new LineStore());
    expect(render.pendingRowCount()).toBe(0);
  });

  it("owes exactly the rows the incoming store holds", () => {
    // The rebuild queues the store's retained keys. An index that is not one of
    // them spends a slot of the per-frame build budget on a row that does not
    // exist, and is reported to the consumer as outstanding work that never
    // drains.
    frameMode = "manual";
    render.handleScroll(scrollMsg(0, 4));
    paint({ rows: [row("W4"), row("W5")], base: 4, cursor: [0, 0] });
    runFrame();
    const held = render.boundStore();
    let retained = 0;
    held.forEachLine(() => {
      retained += 1;
    });
    expect(retained).toBe(6);

    render.bind(held);
    expect(render.pendingRowCount()).toBe(retained);
  });

  it("queues nothing absolute for a store on the alternate screen", () => {
    // The alt grid paints from the ephemeral rows inside the flush, and the alt
    // branch returns before the drain — so an absolute row queued here cannot be
    // built until alt exits, which re-queues everything anyway. The consumer is
    // meanwhile told about a backlog that stands still for the whole TUI session.
    frameMode = "manual";
    render.handleScroll(scrollMsg(0, 4));
    paint({ rows: [row("t0"), row("t1")], base: 4, cursor: [0, 0], altActive: true });
    drainFrames();

    render.bind(render.boundStore());
    expect(render.pendingRowCount()).toBe(0);
  });

  it("announces the wipe to the scroll controller at the offset it wiped from", () => {
    // The wipe is the largest content shrink this module performs and it happens
    // outside a flush, so the flush's own announcement cannot cover it.
    // Unannounced, the clamp it causes has to be inferred, and the inference
    // reads it as "the user scrolled up" — which silently switches auto-follow
    // off.
    frameMode = "manual";
    paint({ rows: [row("a"), row("b"), row("c")], cursor: [0, 0] });
    runFrame();
    vi.spyOn(scroll, "currentScrollTop").mockReturnValue(2 * ROW_PX);
    const shrink = vi.spyOn(scroll, "noteContentShrink").mockClear();

    render.bind(new LineStore());
    expect(shrink.mock.calls).toEqual([[2 * ROW_PX]]);
  });

  it("drops a view restore armed against the store it leaves", () => {
    // An armed restore is re-asserted on later frames and is what the resume
    // transition consults for "which rows must survive". Surviving a swap, it
    // answers with a line of a store the renderer no longer reflects.
    frameMode = "manual";
    paint({ rows: [row("a"), row("b"), row("c")], cursor: [0, 0] });
    runFrame();
    parkReaderAt(2);
    render.updateFontMetrics();
    expect(render.pendingRestoreAbs()).toBe(2);

    render.bind(new LineStore());
    expect(render.pendingRestoreAbs()).toBeNull();
  });
});

describe("the first line of the session is an ordinary reading position", () => {
  it("captures line 0 as the view memory of a reader parked on it", () => {
    // A reading position is decided by row identity, and row 0's identity is no
    // weaker than any other's. A guard that treats index 0 as "no row" reports
    // the line below instead, so a reader at the top of the transcript is
    // restored one line down every time.
    paint({ rows: [row("a"), row("b"), row("c")], cursor: [0, 0] });
    parkReaderAt(0);
    expect(render.captureViewMemory()?.abs).toBe(0);
  });

  it("fetches from a hole's low edge for a reader parked on line 0", () => {
    // Approach-anchored paging: this reader is ABOVE the hole, so the page
    // starts at its low edge. Resolving their position to the window base
    // instead would anchor the request to the far end — a whole hole-width away
    // from where they are reading.
    attach({ paging: true });
    render.handleScroll(scrollMsg(0, 5));
    render.handleScroll(scrollMsg(100, 5));
    paint({ rows: [row("W105")], base: 105, cursor: [0, 0] });
    historyBudget = 20;
    requests = [];
    parkReaderAt(0);

    render.maybeFetchHistory();
    expect(requests).toEqual([[5, 20]]);
  });
});

describe("a scroll-position event drives the paging trigger", () => {
  it("asks for the hole the reader just approached", () => {
    // The reader moving toward a hole is the primary trigger. The post-flush one
    // fires only when a frame arrives, and an idle session produces none — which
    // is the session that needs the fetch most.
    attach({ paging: true });
    render.handleScroll(scrollMsg(0, 5));
    render.handleScroll(scrollMsg(100, 5));
    paint({ rows: [row("W105")], base: 105, cursor: [0, 0] });
    historyBudget = 20;
    requests = [];
    parkReaderAt(100);

    render.handleScrollPosition();
    expect(requests).toEqual([[80, 20]]);
  });
});

describe("the top-of-store marker states what is true above the oldest line held", () => {
  function declarePaging(): void {
    render.applyResumeTransition({
      epochChanged: false,
      committed: null,
      serverOldest: null,
      paging: true,
      sentHaveThrough: render.getReplayBoundary(),
      sentReplayMax: null,
    });
  }

  it("says the history above is not loaded once the server declares paging", () => {
    // Three states, and this is the RECOVERABLE one: a bounded resume replay
    // routinely leaves a fetchable frontier. The resume ack is the only thing
    // that knows, and it has to reach the DOM without waiting for the next
    // inbound frame, because an idle session produces none.
    render.handleScroll(scrollMsg(100, 5));
    paint({ rows: [row("W105")], base: 105, cursor: [0, 0] });
    expect(output.querySelector(".term-trim-marker")).toBeNull();

    declarePaging();
    const marker = output.querySelector(".term-trim-marker");
    expect(marker?.textContent).toBe("earlier output not loaded");
    expect(output.firstChild).toBe(marker);
  });

  it("does not re-insert an unchanged marker on a later flush", () => {
    // role=status makes it a live region: re-inserting it re-announces an
    // unchanged fact on each of the 60 frames a second a busy session produces.
    render.handleScroll(scrollMsg(100, 5));
    paint({ rows: [row("W105")], base: 105, cursor: [0, 0] });
    declarePaging();
    const marker = output.querySelector(".term-trim-marker");
    expect(marker).not.toBeNull();

    const inserts = vi.spyOn(output, "insertBefore");
    paint({ rows: [row("W105b")], base: 105, cursor: [0, 0] });
    expect(inserts.mock.calls.filter((c) => c[0] === marker)).toEqual([]);
  });
});

describe("the caret's blink timer follows the cursor's state", () => {
  beforeEach(() => {
    // Stand the interval down under REAL timers first: the attachment in the
    // outer hook started one, and a real timer cannot be cleared through the
    // fake API that replaces the globals below.
    paint({ rows: [row("x")], cursor: [0, 0], cursorBlink: false });
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("leaves no timer behind when the caret stops blinking", () => {
    // Both "no timer" states (blinking disabled, page hidden) have event-driven
    // restarts, so a timer is not needed to notice the way back. One left running
    // is a wakeup every few seconds for the rest of the session, which on a phone
    // is battery spent on nothing.
    paint({ rows: [row("abc")], cursor: [0, 0], cursorBlink: true });
    expect(vi.getTimerCount()).toBe(1);

    paint({ rows: [row("abd")], cursor: [0, 0], cursorBlink: false });
    expect(vi.getTimerCount()).toBe(0);
  });

  it("polls slowly instead of blinking while the application hides the cursor", () => {
    // DECTCM: a full-screen TUI paints its own cursor cell. The 530ms toggle
    // would only restyle a display:none overlay, so the timer downshifts to a
    // slow re-check — still polling for the cursor's return, at a fraction of the
    // wakeups. For an agent front-end this is the session's steady state.
    paint({ rows: [row("abc")], cursor: [0, 0], cursorBlink: true, cursorHidden: true });
    expect(vi.getTimerCount()).toBe(1);

    vi.advanceTimersByTime(530);
    expect(termWrap.classList.contains("cursor-blink-off")).toBe(false);
  });
});

describe("a flush that moved nothing corrects nothing", () => {
  /** Park the reader 6px above the top of row 2's box, so the anchored row's
   *  on-screen position is a non-zero number the arithmetic has to cancel. */
  function parkJustAboveRow2(): void {
    vi.spyOn(scroll, "isUserScrolledUp").mockReturnValue(true);
    vi.spyOn(scroll, "currentScrollTop").mockReturnValue(2 * ROW_PX - 6);
  }

  it("measures the read anchor's drift on screen, not the content above it", () => {
    // The correction is how far the anchored row moved ON SCREEN. Chrome and
    // Firefox anchor natively, so their offset change arrives with a matching
    // scrollTop change and the screen position is already right: this measures
    // zero and does nothing. Correcting the content delta instead would
    // double-compensate there and throw the view the other way.
    paint({ rows: [row("a"), row("b"), row("c")], cursor: [0, 0] });
    parkJustAboveRow2();
    const shift = vi.spyOn(scroll, "adjustForContentShift").mockClear();

    paint({ rows: [row("a"), row("b"), row("c2")], changed: [2], cursor: [0, 0] });
    expect(shift.mock.calls).toEqual([[0]]);
  });

  it("lands an already-satisfied view restore without moving the reader", () => {
    // The restore is re-asserted until the row it names is built, so it runs
    // against a viewport that is usually already correct. A non-zero correction
    // there moves the reader away from the very line the restore exists to hold.
    frameMode = "manual";
    paint({ rows: [row("a"), row("b"), row("c")], cursor: [0, 0] });
    runFrame();
    parkJustAboveRow2();
    render.updateFontMetrics();
    expect(render.pendingRestoreAbs()).toBe(2);
    const shift = vi.spyOn(scroll, "adjustForContentShift").mockClear();

    runFrame();
    expect(render.pendingRestoreAbs()).toBeNull();
    expect(shift.mock.calls).toEqual([[0]]);
  });
});

describe("an alt frame touches only the rows it changed", () => {
  it("leaves the spans of an unchanged row in place", () => {
    // The row DIV's identity survives either way (the reconcile reuses it), so
    // the observable is one level down: a rebuilt row replaces its spans, which
    // drops any native selection inside it and forces layout for content that did
    // not change.
    paint({ rows: [row("t0"), row("t1"), row("t2")], cursor: [0, 0], altActive: true });
    const before = [...output.children].map((el) => el.firstElementChild);

    paint({
      rows: [row("t0"), row("changed"), row("t2")],
      changed: [1],
      cursor: [0, 0],
      altActive: true,
    });
    const after = [...output.children].map((el) => el.firstElementChild);
    expect(after[0]).toBe(before[0]);
    expect(after[2]).toBe(before[2]);
    expect(after[1]).not.toBe(before[1]);
  });
});

describe("dropping the browse cache takes it off the screen", () => {
  it("repaints without waiting for the next inbound frame", () => {
    // The consumer owns the TTL and calls this from a timer, so nothing else is
    // coming: rows the store just evicted would stay on screen — and stay
    // scrollable — until the session next produced output.
    render.handleScroll(scrollMsg(0, 5));
    paint({ rows: [row("W5")], base: 5, cursor: [0, 0] });
    render.noteSolicited(50, 55);
    render.handleHistoryReply(scrollMsg(50, 5), null);
    render.clearSolicited();
    expect(rowIndices()).toContain(50);

    render.dropBrowseCache(false);
    expect(rowIndices()).not.toContain(50);
  });
});
describe("a line that leaves and comes back gets its row back", () => {
  it("rebuilds the row for a line the store evicted and then received again", () => {
    // The row map is keyed by absolute index, so an entry left behind for an
    // evicted line makes the next arrival of that line an UPDATE of an element
    // that is no longer in the document: the row silently never comes back,
    // leaving a hole in the transcript that no later frame can heal.
    render.handleScroll(scrollMsg(0, 5));
    paint({ rows: [row("W5")], base: 5, cursor: [0, 0] });
    render.noteSolicited(50, 55);
    render.handleHistoryReply(scrollMsg(50, 5), null);
    render.clearSolicited();
    expect(rowIndices()).toContain(50);

    render.dropBrowseCache(false);
    expect(rowIndices()).not.toContain(50);

    render.noteSolicited(50, 55);
    render.handleHistoryReply(scrollMsg(50, 5), null);
    render.clearSolicited();
    expect(rowIndices()).toContain(50);
  });
});

describe("leaving the alternate screen is a content shrink like any other", () => {
  it("announces the wipe it performs on the way out", () => {
    // The exit branch tears the whole grid down before rebuilding from the
    // store. Unannounced, the clamp that causes has to be inferred from its
    // arithmetic signature, and that inference is what silently switches
    // auto-follow off.
    render.handleScroll(scrollMsg(0, 5));
    paint({ rows: [row("t0"), row("t1")], base: 5, cursor: [0, 0], altActive: true });
    const shrink = vi.spyOn(scroll, "noteContentShrink").mockClear();

    paint({ rows: [row("W5"), row("W6")], base: 5, cursor: [0, 0], altActive: false });
    expect(shrink).toHaveBeenCalledTimes(1);
  });
});

describe("an unchanged gap marker stays quiet", () => {
  /** Two retained regions with a hole between them. */
  function twoRegionsWithHole(): HTMLElement {
    attach({ paging: true });
    render.handleScroll(scrollMsg(0, 5));
    render.handleScroll(scrollMsg(100, 5));
    paint({ rows: [row("W105"), row("W106")], base: 105, cursor: [0, 0] });
    const el = output.querySelector<HTMLElement>(".term-gap-marker");
    expect(el, "the gap marker must be rendered").not.toBeNull();
    return el!;
  }

  it("does not re-label a marker whose gap did not move", () => {
    // The marker is a live region (role=status). Re-writing its label
    // re-announces an unchanged fact to a screen reader on each of the 60 frames
    // a second a busy session produces.
    const el = twoRegionsWithHole();
    const attrs = vi.spyOn(el, "setAttribute");

    paint({ rows: [row("W105"), row("W106b")], base: 105, cursor: [0, 0] });
    expect(attrs.mock.calls.filter((c) => c[0] === "aria-label")).toEqual([]);
  });

  it("does not re-insert a marker that is already in the right place", () => {
    // Re-inserting a live region is a move — a removal and an insertion — which
    // announces it again just as a re-label does.
    const el = twoRegionsWithHole();
    const inserts = vi.spyOn(output, "insertBefore");

    paint({ rows: [row("W105"), row("W106b")], base: 105, cursor: [0, 0] });
    expect(inserts.mock.calls.filter((c) => c[0] === el)).toEqual([]);
  });
});
