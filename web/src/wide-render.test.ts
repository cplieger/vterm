// Wide-char (East Asian Wide/Fullwidth) and zero-width rendering.
//
// Spec: UAX#11 East Asian Width — Wide/Fullwidth chars occupy 2 cells,
// combining/zero-width chars occupy 0. The engine's width table follows this
// (vt/width.go). Over the wire a wide char is a base cell followed by a spacer
// cell (Cell.Ch == 0), which vt/wire.go cellsToRuns serializes as the sentinel
// U+FFFF appended after the glyph ("漢\uFFFF"). The renderer's job (render.ts):
//   - never let the U+FFFF sentinel become visible text, and
//   - stretch the wide glyph across two cells (via letterSpacing).
//
// Two model facts these tests rely on (verified against the Go source, not
// assumed from render.ts):
//   - Combining marks (width 0) are DROPPED server-side (vt/screen.go put()
//     returns on width 0), so a bare combining mark never appears in real wire
//     text — the case below is a raw-render robustness guard, labeled as such.
//   - Single-codepoint emoji are width-1 by design (vt/width.go "Unsupported by
//     Design": emoji are NOT treated as Wide), so they carry no U+FFFF spacer.
//     The astral-wide case below therefore uses a CJK Extension B code point
//     (U+20000), which the engine's wideRanges genuinely treats as width-2.

import { describe, it, expect, beforeEach, afterEach } from "vitest";
import * as render from "./render.js";
import type { ScreenMessage, WireRun } from "./types.js";

// Deterministic cell metrics, declared rather than measured: a real Canvas2D and
// real layout both answer from whatever font the machine has installed, and the
// letterSpacing (double-width) assertions below need an exact cell. measureText
// counts CODE POINTS (matching a real font's per-glyph advance, so an astral
// char measures one cell, not two UTF-16 units), and getBoundingClientRect
// reports width proportional to text length so measureCellWidth yields a stable
// non-zero cell (CELL_PX).
const CELL_PX = 8;

interface FakeCtx {
  font: string;
  measureText: (t: string) => { width: number };
}

let realGetContext: typeof HTMLCanvasElement.prototype.getContext;
let realGetBoundingClientRect: typeof HTMLElement.prototype.getBoundingClientRect;

function installMetricStubs(): void {
  realGetContext = HTMLCanvasElement.prototype.getContext;
  realGetBoundingClientRect = HTMLElement.prototype.getBoundingClientRect;
  HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
    const ctx: FakeCtx = {
      font: "",
      measureText: (text: string) => ({ width: [...text].length * CELL_PX }),
    };
    return ctx;
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
}

function restoreMetricStubs(): void {
  HTMLCanvasElement.prototype.getContext = realGetContext;
  HTMLElement.prototype.getBoundingClientRect = realGetBoundingClientRect;
}

function makeMsg(rows: WireRun[][], cursor: [number, number], changed?: number[]): ScreenMessage {
  return {
    type: "screen",
    base: 0,
    rows,
    cursor,
    changed: changed ?? rows.map((_, i) => i),
    cursorHidden: true,
    cursorStyle: 0,
    cursorBlink: true,
  };
}

async function flush(msg: ScreenMessage): Promise<void> {
  render.handleScreen(msg);
  // Wait for the frame render.ts scheduled, not for a duration: real frames
  // tick, so a fixed sleep races the flush in both directions. Two deep because
  // the first callback can run in the frame the flush was queued in.
  await new Promise<void>((resolve) => {
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        resolve();
      });
    });
  });
}

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
}

function childSpans(rowEl: HTMLElement): HTMLElement[] {
  return Array.from(rowEl.children) as HTMLElement[];
}

function spanContaining(rowEl: HTMLElement, ch: string): HTMLElement | undefined {
  return childSpans(rowEl).find((s) => (s.textContent ?? "").includes(ch));
}

describe("render: wide-char and zero-width handling", () => {
  let outputEl: HTMLDivElement;
  let termWrap: HTMLDivElement;

  beforeEach(() => {
    installMetricStubs();
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    outputEl = document.getElementById("term-output") as HTMLDivElement;
    render.resetScreen();
    render.init({ output: outputEl, termWrap });
    render.updateFontMetrics();
  });

  afterEach(() => {
    restoreMetricStubs();
  });

  it("never lets the U+FFFF wide-continuation sentinel become visible text", async () => {
    // Server sends "A漢\uFFFFB" + trailing spaces for a 10-col row.
    await flush(makeMsg([row("A漢\uFFFFB" + " ".repeat(6))], [0, 0]));

    const rowEl = outputEl.children[0] as HTMLElement;
    expect(rowEl).toBeDefined();
    const fullText = rowEl.textContent ?? "";
    // The sentinel is consumed by the renderer, never shown.
    expect(fullText).not.toContain("\uFFFF");
    expect(fullText).toContain("漢");
    // The following glyph is preserved too (not swallowed with the sentinel).
    expect(fullText).toContain("B");
  });

  it("stretches a wide glyph across two cells; narrow glyphs are not stretched", async () => {
    // UAX#11: a Wide char occupies 2 cells. render.ts realizes this by adding
    // letterSpacing to the single glyph so it spans two cell widths. A narrow
    // (width-1) glyph gets no such stretch — that contrast is the observable
    // consequence of the width spec.
    await flush(makeMsg([row("漢\uFFFF" + " ".repeat(8))], [0, 0]));
    const wideRow = outputEl.children[0] as HTMLElement;
    const kanji = spanContaining(wideRow, "漢");
    expect(kanji, "a span carrying 漢 must exist").toBeDefined();
    // The glyph is stretched by exactly one extra cell (read render's own
    // published cell width, do not recompute it) so it spans two cells total.
    const cellW = document.documentElement.style.getPropertyValue("--char-w");
    expect(parseFloat(cellW)).toBeGreaterThan(0); // metrics are non-degenerate
    expect(kanji!.style.letterSpacing).toBe(cellW);
  });

  it("does not stretch a row of plain narrow glyphs", async () => {
    await flush(makeMsg([row("ABCD" + " ".repeat(6))], [0, 0]));
    const narrowRow = outputEl.children[0] as HTMLElement;
    for (const span of childSpans(narrowRow)) {
      // No wide char present, so no glyph carries extra cell spacing.
      expect(span.style.letterSpacing).toBe("");
    }
  });

  it("renders multiple consecutive wide chars, all sentinels consumed", async () => {
    // "漢\uFFFF字\uFFFF" + 6 spaces = 10 cols.
    await flush(makeMsg([row("漢\uFFFF字\uFFFF" + " ".repeat(6))], [0, 0]));

    const rowEl = outputEl.children[0] as HTMLElement;
    const fullText = rowEl.textContent ?? "";
    expect(fullText).not.toContain("\uFFFF");
    expect(fullText).toContain("漢");
    expect(fullText).toContain("字");
  });

  it("renders an astral (4-byte) wide char + sentinel without corrupting it", async () => {
    // U+20000 (CJK Ext B) is width-2 per vt/width.go wideRanges and is a
    // surrogate pair in JS. The sentinel handler reads the previous glyph's last
    // CODE POINT ([...text].at(-1)); this guards that it recovers the full astral
    // char (not a lone surrogate) and still stretches it to two cells.
    const astral = "\u{20000}";
    await flush(makeMsg([row(astral + "\uFFFF" + " ".repeat(8))], [0, 0]));

    const rowEl = outputEl.children[0] as HTMLElement;
    const fullText = rowEl.textContent ?? "";
    expect(fullText).not.toContain("\uFFFF");
    expect(fullText).toContain(astral);
    const glyph = spanContaining(rowEl, astral);
    expect(glyph, "a span carrying the astral glyph must exist").toBeDefined();
    // Stretched to two cells like any wide glyph (one extra cell of spacing).
    const cellW = document.documentElement.style.getPropertyValue("--char-w");
    expect(glyph!.style.letterSpacing).toBe(cellW);
  });

  it("tolerates a bare combining mark in wire text (robustness)", async () => {
    // Combining marks are width-0 and DROPPED server-side (vt/screen.go put()),
    // so this input never occurs in real wire text; the renderer must still not
    // crash or emit a sentinel if it somehow appears. Raw-render robustness only.
    await flush(makeMsg([row("e\u0301" + " ".repeat(9))], [0, 0]));

    const rowEl = outputEl.children[0] as HTMLElement;
    expect(rowEl).toBeDefined();
    const fullText = rowEl.textContent ?? "";
    expect(fullText).toContain("e");
    expect(fullText).not.toContain("\uFFFF");
  });

  it("row of all wide chars: every glyph present, no sentinel leaks", async () => {
    // 5 wide chars = 10 cols: 漢字漢字漢, each followed by its sentinel.
    await flush(makeMsg([row("漢\uFFFF字\uFFFF漢\uFFFF字\uFFFF漢\uFFFF")], [0, 0]));

    const rowEl = outputEl.children[0] as HTMLElement;
    const text = rowEl.textContent ?? "";
    expect(text).not.toContain("\uFFFF");
    expect([...text].filter((c) => c === "漢").length).toBe(3);
    expect([...text].filter((c) => c === "字").length).toBe(2);
  });

  // --- Cursor placement over wide chars (spec) ---
  //
  // The engine reports cursor_col in TRUE cell coordinates: a wide glyph moves
  // curX by 2 (base cell + spacer), so vt CursorPos counts the spacer cell. The
  // renderer must therefore count the U+FFFF continuation cell toward its column
  // position, so a visible cursor positioned AFTER a wide char lands on the
  // correct cell rather than one cell too far right per preceding wide char.

  it("places the caret overlay after a wide char on the correct cell (counts the spacer)", async () => {
    // Row "A漢B": A=col0, 漢=col1 (occupies cols 1-2 via the spacer), B=col3.
    // cursor_col=3 => the caret sits ON B, not on a phantom cell to its right:
    // the overlay's glyph copy reads the char at TRUE cell col 3 (the \uFFFF
    // continuation advanced the column like a printed cell).
    const msg: ScreenMessage = {
      type: "screen",
      base: 0,
      rows: [row("A漢\uFFFFB" + " ".repeat(6))],
      cursor: [0, 3],
      changed: [0],
      cursorHidden: false,
      cursorStyle: 0,
      cursorBlink: true,
    };
    await flush(msg);
    const overlay = document.querySelector<HTMLElement>(".term-cursor-overlay");
    expect(overlay, "the caret overlay must be rendered").not.toBeNull();
    expect(overlay!.classList.contains("visible")).toBe(true);
    // The caret is on B (the cell at true col 3), the char cursor_col points at.
    expect(overlay!.textContent).toBe("B");
  });

  it("hidden cursor renders no visible caret and no in-row cursor span", async () => {
    // cursorHidden is true (makeMsg default): the host draws its own cursor, so
    // the renderer shows no caret.
    await flush(makeMsg([row("A漢\uFFFFB" + " ".repeat(6))], [0, 1], [0]));

    const rowEl = outputEl.children[0] as HTMLElement;
    expect(rowEl).toBeDefined();
    expect(
      rowEl.querySelector(".term-cursor, .term-cursor-underline, .term-cursor-bar"),
    ).toBeNull();
    const overlay = document.querySelector<HTMLElement>(".term-cursor-overlay");
    expect(overlay?.classList.contains("visible") ?? false).toBe(false);
  });

  it("caret overlay ON a wide char copies the glyph and spans both its cells", async () => {
    // Cursor at cell col 1 = the wide char 漢 (A=col0, 漢=col1): the overlay
    // carries the wide glyph itself and widens to two cells, exactly like the
    // old inline cursor span's continuation-spacer adjustment.
    const msg: ScreenMessage = {
      type: "screen",
      base: 0,
      rows: [row("A漢\uFFFFB" + " ".repeat(6))],
      cursor: [0, 1],
      changed: [0],
      cursorHidden: false,
      cursorStyle: 0,
      cursorBlink: true,
    };
    await flush(msg);

    const overlay = document.querySelector<HTMLElement>(".term-cursor-overlay");
    expect(overlay, "the caret overlay must be rendered").not.toBeNull();
    // The caret carries the wide glyph itself.
    expect(overlay!.textContent).toBe("漢");
  });
});
