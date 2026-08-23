// Renders the captured the host application WS frame sequence (typing "abc",
// space, "d", LEFT, LEFT, "X") through render.handleScreen and
// inspects the resulting DOM after each step.
//
// The bug under test: the visible cursor (the inverse-video character
// that Ink draws) sometimes does not move when the content of row 19
// changes by exactly the cursor cell. This test asserts that after
// each frame, the inverse-styled span sits at the column matching
// msg.cursor[1].

import { describe, it, expect, beforeEach } from "vitest";
import * as render from "./render.js";
import type { ScreenMessage, WireRun } from "./types.js";

// measureChar() in render.ts requires `getContext("2d").measureText`, and a real
// Canvas2D measures whatever font the machine has installed. A fixed-width
// metric is declared instead so the cell arithmetic is deterministic.
interface FakeCtx {
  font: string;
  measureText: (t: string) => { width: number };
}
HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
  const ctx: FakeCtx = {
    font: "",
    measureText: (text: string) => ({ width: text.length * 8 }),
  };
  return ctx;
} as typeof HTMLCanvasElement.prototype.getContext;

// One row payload describing a the host application prompt at the bottom of the
// terminal: `<text default> <inverse char> <trailing spaces>`. The
// inverse char is the visible cursor.
function row19(textBeforeCursor: string, cursorChar: string, padTo = 120): WireRun[] {
  const trailing = padTo - textBeforeCursor.length - cursorChar.length;
  return [
    { t: textBeforeCursor, f: -1, b: -1, a: 0, uc: -1 },
    { t: cursorChar, f: -1, b: -1, a: 8, uc: -1 }, // a=8 = inverse
    { t: " ".repeat(Math.max(0, trailing)), f: -1, b: -1, a: 0, uc: -1 },
  ];
}

function blankRow(width = 120): WireRun[] {
  return [{ t: " ".repeat(width), f: -1, b: -1, a: 0, uc: -1 }];
}

function frame(rowsByIdx: Record<number, WireRun[]>, cursor: [number, number]): ScreenMessage {
  const screenH = 30;
  const rows: WireRun[][] = new Array(screenH);
  const changed: number[] = [];
  for (const k of Object.keys(rowsByIdx)) {
    const idx = Number(k);
    rows[idx] = rowsByIdx[idx]!;
    changed.push(idx);
  }
  return {
    type: "screen",
    base: 0,
    rows,
    cursor,
    changed,
    cursorHidden: true, // the host application hides the native cursor
    cursorStyle: 0,
    cursorBlink: true,
  };
}

// flushFrame pumps a screen message through handleScreen and then waits for the
// frame render.ts scheduled — the frame itself, not a duration. Real frames
// tick, so a fixed sleep races the flush in both directions and Chromium
// throttles rAF outright when the page is not visible. Two deep because the
// first callback can run in the frame the flush was queued in.
async function flushFrame(msg: ScreenMessage): Promise<void> {
  render.handleScreen(msg);
  await new Promise<void>((resolve) => {
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        resolve();
      });
    });
  });
}

describe("render: cursor cell updates with inline inverse-video character", () => {
  let outputEl: HTMLDivElement;
  let termWrap: HTMLDivElement;

  beforeEach(() => {
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    outputEl = document.getElementById("term-output") as HTMLDivElement;
    render.resetScreen();
    render.init({ output: outputEl, termWrap });
    render.updateFontMetrics();
  });

  it("renders the inverse-cursor span at the right column after every keystroke", async () => {
    // Establish the screen with row 19 already showing some prompt
    // placeholder + cursor at col 0 (exact pre-typing baseline).
    const initialRows: Record<number, WireRun[]> = {};
    for (let i = 0; i < 30; i++) {
      initialRows[i] = blankRow();
    }
    initialRows[19] = row19("", " ", 120);
    await flushFrame(frame(initialRows, [19, 0]));

    // After typing "abc": row 19 = "abc" + inv " " + trailing
    // cursor at col 3.
    await flushFrame(frame({ 19: row19("abc", " ") }, [19, 3]));
    expectInverseAtCol(outputEl, 19, 3, " ");

    // After space: row 19 = "abc " + inv " " + trailing; cursor (19,4)
    await flushFrame(frame({ 19: row19("abc ", " ") }, [19, 4]));
    expectInverseAtCol(outputEl, 19, 4, " ");

    // After "d": row 19 = "abc d" + inv " "; cursor (19,5)
    await flushFrame(frame({ 19: row19("abc d", " ") }, [19, 5]));
    expectInverseAtCol(outputEl, 19, 5, " ");

    // After Left arrow: row 19 = "abc " + inv "d" + trailing; cursor (19,4)
    // Note inverse char is now 'd' (Ink moved cursor onto the 'd').
    await flushFrame(frame({ 19: row19("abc ", "d") }, [19, 4]));
    expectInverseAtCol(outputEl, 19, 4, "d");

    // After 2nd Left arrow: row 19 = "abc" + inv " " + "d" + trailing;
    // BUT now the format is different: a normal "d" follows the inverse
    // space. So the row payload is 4 runs: "abc" / inv " " / "d" / trailing.
    await flushFrame(
      frame(
        {
          19: [
            { t: "abc", f: -1, b: -1, a: 0, uc: -1 },
            { t: " ", f: -1, b: -1, a: 8, uc: -1 },
            { t: "d", f: -1, b: -1, a: 0, uc: -1 },
            { t: " ".repeat(115), f: -1, b: -1, a: 0, uc: -1 },
          ],
        },
        [19, 3],
      ),
    );
    expectInverseAtCol(outputEl, 19, 3, " ");

    // After "X": row 19 = "abcX" + inv " " + "d" + trailing; cursor (19,4)
    await flushFrame(
      frame(
        {
          19: [
            { t: "abcX", f: -1, b: -1, a: 0, uc: -1 },
            { t: " ", f: -1, b: -1, a: 8, uc: -1 },
            { t: "d", f: -1, b: -1, a: 0, uc: -1 },
            { t: " ".repeat(114), f: -1, b: -1, a: 0, uc: -1 },
          ],
        },
        [19, 4],
      ),
    );
    expectInverseAtCol(outputEl, 19, 4, " ");
  });
});

// expectInverseAtCol asserts that the row at the given absolute index in the
// output element has an inverse-styled span containing `expectedChar` whose
// starting column is `col`. Spans are flat; we reconstruct columns by summing
// the textContent length of preceding spans.
//
// The visible cursor these frames carry is the application's own inverse-video
// cell (the native cursor is hidden: every frame sets cursorHidden). Per the
// ANSI spec, inverse video over default colors swaps the theme's default
// fg/bg; render.ts expresses that as `color: var(--bg); background: var(--text)`
// (matching the tier-1 assertion in render-attributes.test.ts). Detect the
// inverse cell by that exact swap — requiring BOTH properties, so a renderer
// that dropped half the swap (an invisible inverse blank) would fail here.
function expectInverseAtCol(
  output: HTMLElement,
  rowIdx: number,
  col: number,
  expectedChar: string,
): void {
  const rowEl = output.children[rowIdx] as HTMLElement | undefined;
  expect(rowEl, `row[${rowIdx}] must exist`).toBeDefined();
  const spans = Array.from(rowEl!.children) as HTMLElement[];
  let cumCol = 0;
  let foundInverseAt = -1;
  let foundInverseText = "";
  for (const span of spans) {
    const text = span.textContent ?? "";
    const isInverse = span.style.background === "var(--text)" && span.style.color === "var(--bg)";
    if (isInverse) {
      // First inverse span found; record its starting column and the
      // expected char (text content's first character).
      foundInverseAt = cumCol;
      foundInverseText = text;
      break;
    }
    cumCol += [...text].length;
  }
  expect(foundInverseAt, `expected inverse-styled span at col ${col}`).toBe(col);
  expect([...foundInverseText][0]).toBe(expectedChar);
}
