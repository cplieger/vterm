// Shared render harness for the DOM-tier display-conformance tests (tier 1
// per-attribute, tier 2 cross-language golden). It drives the REAL render.ts in
// a real browser and returns the DOM a caller can assert against — the tests
// state the SPEC and check compliance; this file only provides the plumbing.
import * as render from "../render.js";
import type { ScreenMessage, WireRun } from "../types.js";

// Fixed cell metric so measureText is deterministic. A real Canvas2D measures
// the actual font, which varies with what the machine has installed, so the
// arithmetic every caller asserts against is pinned here instead.
const CELL_PX = 8;

// Rows in the rendered window. Content under test goes on row 0; the cursor is
// parked on the last row so it never adds a span to the row being asserted.
const SCREEN_H = 8;

function installCanvasStub(): void {
  HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
    return {
      font: "",
      measureText: (text: string): { width: number } => ({ width: text.length * CELL_PX }),
    };
  } as typeof HTMLCanvasElement.prototype.getContext;
}

/**
 * flushFrame waits for the frame render.ts scheduled, rather than for a
 * duration. render.ts batches DOM writes behind
 * `pendingFrame = requestAnimationFrame(flushRender)`, so the DOM is current
 * only once that callback has RUN. Two frames deep because one rAF resolves at
 * the START of the frame the flush is queued in, which can be the same frame:
 * the second callback is guaranteed to run after the first has completed.
 *
 * A fixed sleep was correct only while frames never ticked. Against real frames
 * it is a race in both directions — too short under load, and needlessly slow
 * otherwise — and Chromium additionally throttles rAF when the page is not
 * visible, which no duration can account for.
 */
async function flushFrame(): Promise<void> {
  await new Promise<void>((resolve) => {
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        resolve();
      });
    });
  });
}

/**
 * initHarness resets and initializes the render module against a fresh DOM.
 * Call in beforeEach. Returns the output element.
 */
export function initHarness(): HTMLElement {
  document.body.innerHTML = `<div class="term-wrap"><div class="term-output"></div></div>`;
  const termWrap = document.querySelector<HTMLElement>(".term-wrap")!;
  const output = document.querySelector<HTMLElement>(".term-output")!;
  installCanvasStub();
  render.resetScreen();
  render.init({ output, termWrap });
  render.updateFontMetrics();
  return output;
}

/**
 * renderRow renders `runs` on row 0 of a fresh window and returns that row's
 * child elements (spans / anchors), awaiting the render flush.
 */
export async function renderRow(runs: WireRun[]): Promise<HTMLElement[]> {
  const blank: WireRun[] = [{ t: " ".repeat(40), f: -1, b: -1, a: 0, uc: -1 }];
  const rows: WireRun[][] = [];
  const changed: number[] = [];
  for (let i = 0; i < SCREEN_H; i++) {
    rows[i] = i === 0 ? runs : blank;
    changed.push(i);
  }
  const msg: ScreenMessage = {
    type: "screen",
    base: 0,
    rows,
    cursor: [SCREEN_H - 1, 0],
    changed,
    cursorHidden: true,
    cursorStyle: 0,
    cursorBlink: false,
  };
  render.handleScreen(msg);
  await flushFrame();
  const output = document.querySelector<HTMLElement>(".term-output")!;
  const rowEl = output.children[0] as HTMLElement;
  return Array.from(rowEl.children) as HTMLElement[];
}

/** firstTextSpan returns the first element whose text is non-blank. */
export function firstTextSpan(spans: HTMLElement[]): HTMLElement | undefined {
  return spans.find((s) => (s.textContent ?? "").trim().length > 0);
}

/**
 * renderScreen renders a full decoded ScreenMessage through the real renderer
 * and returns the output element, awaiting the flush. Used by the cross-language
 * golden tier (the message comes from the Go-generated fixture).
 */
export async function renderScreen(msg: ScreenMessage): Promise<HTMLElement> {
  render.handleScreen(msg);
  await flushFrame();
  return document.querySelector<HTMLElement>(".term-output")!;
}

/** rowSpans returns the child elements of the rendered row at absolute `index`. */
export function rowSpans(output: HTMLElement, index: number): HTMLElement[] {
  const rowEl = output.children[index] as HTMLElement | undefined;
  return rowEl ? (Array.from(rowEl.children) as HTMLElement[]) : [];
}
