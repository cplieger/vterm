// @vitest-environment happy-dom
//
// The reading position must survive content vanishing ABOVE it. When the
// retention cap is reached, every new line evicts one from the top of history —
// so a user scrolled up to read has the content above them shrink continuously
// while output streams. Unless the viewport follows it, whatever they are reading
// slides one line further up per evicted row.
//
// Chrome and Firefox fix this natively (scroll anchoring, `overflow-anchor`) and
// the renderer used to rely on that. WebKit has never implemented it, so on
// Safari — reported from an iPad — the view crawled upward for as long as the
// agent kept writing. render.ts anchors the position by hand now; these tests
// pin that, and they fail if restoreReadAnchor is removed.
//
// happy-dom has no layout, so offsetTop is faked from DOM order (rows are
// uniform-height and in document order, which is exactly what the binary search
// in captureReadAnchor relies on) and the scroll element's geometry is derived
// from the child count.

import { describe, it, expect, beforeEach, afterEach } from "vitest";
import * as render from "./render.js";
import * as scroll from "./scroll.js";
import { LineStore } from "./store.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

const ROW_H = 17;
const VIEWPORT_H = 170; // 10 rows visible

interface FakeCtx {
  font: string;
  measureText: (t: string) => { width: number };
}
HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
  const ctx: FakeCtx = { font: "", measureText: (t: string) => ({ width: t.length * 8 }) };
  return ctx;
} as typeof HTMLCanvasElement.prototype.getContext;

function row(text: string): WireRun[] {
  return [{ t: text }];
}
function screenMsg(base: number, rows: WireRun[][], changed: number[]): ScreenMessage {
  return {
    type: "screen",
    base,
    rows,
    changed,
    cursor: [0, 0],
    cursorHidden: true,
    cursorStyle: 0,
    cursorBlink: false,
  };
}
function scrollMsg(firstIndex: number, texts: string[]): ScrollMessage {
  return { type: "scroll", firstIndex, lines: texts.map(row) };
}
const tick = (): Promise<void> => new Promise((r) => setTimeout(r, 20));

describe("render: the reading position holds when history is evicted above it", () => {
  let outputEl: HTMLDivElement;
  let termWrap: HTMLDivElement;
  let scrollTop = 0;
  let offsetTopDescriptor: PropertyDescriptor | undefined;

  beforeEach(() => {
    // offsetTop from DOM order: the geometry the anchor's binary search assumes.
    offsetTopDescriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetTop");
    Object.defineProperty(HTMLElement.prototype, "offsetTop", {
      configurable: true,
      get(this: HTMLElement): number {
        const parent = this.parentElement;
        if (!parent) {
          return 0;
        }
        return Array.prototype.indexOf.call(parent.children, this) * ROW_H;
      },
    });

    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    outputEl = document.getElementById("term-output") as HTMLDivElement;
    scrollTop = 0;
    committed = 0;
    Object.defineProperty(termWrap, "scrollHeight", {
      configurable: true,
      get: () => outputEl.children.length * ROW_H,
    });
    Object.defineProperty(termWrap, "clientHeight", { configurable: true, get: () => VIEWPORT_H });
    Object.defineProperty(termWrap, "scrollTop", {
      configurable: true,
      get: () => scrollTop,
      set: (v: number) => {
        scrollTop = v;
      },
    });

    render.init({ output: outputEl, termWrap });
    render.updateFontMetrics();
    scroll.init({ scrollEl: termWrap });
    // A small retention cap, so eviction is reachable without 5000 rows — but
    // roomy enough that a reader parked mid-buffer is not themselves trimmed.
    render.bind(new LineStore(60));
  });

  afterEach(() => {
    if (offsetTopDescriptor) {
      Object.defineProperty(HTMLElement.prototype, "offsetTop", offsetTopDescriptor);
    }
  });

  /** Scroll to `top` the way a user does, so follow/hold derives from the event. */
  function userScrollTo(top: number): void {
    termWrap.scrollTop = top;
    termWrap.dispatchEvent(new Event("scroll"));
  }

  const WINDOW_H = 4;
  let committed = 0;

  /** Commit `n` more history lines, the window following along above them. */
  async function commitLines(n: number): Promise<void> {
    for (let i = 0; i < n; i++) {
      render.handleScroll(scrollMsg(committed, [`history ${String(committed)}`]));
      committed++;
    }
    render.handleScreen(
      screenMsg(committed, [row("w0"), row("w1"), row("w2"), row("w3")], [0, 1, 2, WINDOW_H - 1]),
    );
    await tick();
  }

  /** Fill to the store's retention cap so further lines evict from the top. */
  async function fillToCap(): Promise<void> {
    await commitLines(60);
  }

  /** The row element currently at the top of the viewport. */
  function rowAtViewportTop(): HTMLElement {
    const idx = Math.round(termWrap.scrollTop / ROW_H);
    return outputEl.children[idx] as HTMLElement;
  }

  /** Where a row sits ON SCREEN: its offset within the container minus the
   *  scroll offset. This is what the user sees, and it is what must not change
   *  when content above the row appears or disappears. */
  function screenPosOf(el: HTMLElement): number {
    return el.offsetTop - termWrap.scrollTop;
  }

  it("keeps the same line under the reader while history is evicted", async () => {
    await fillToCap();
    const rowsBefore = outputEl.children.length;

    userScrollTo(30 * ROW_H); // parked mid-buffer, reading
    expect(scroll.isUserScrolledUp()).toBe(true);
    const readingAt = termWrap.scrollTop;
    const reading = rowAtViewportTop();
    const wasAt = screenPosOf(reading);
    const text = reading.textContent;

    // More committed lines: at the cap, each evicts one row from the top, so the
    // content above the reader shrinks.
    await commitLines(5);
    expect(outputEl.children.length).toBeLessThan(rowsBefore + 5); // eviction happened
    expect(reading.parentElement).toBe(outputEl); // the reader's row survived

    // THE invariant: the row the user was reading is still in the same place on
    // screen. Without the manual anchor its offsetTop drops by the evicted height
    // while scrollTop stays put, so it slides up out of view.
    expect(screenPosOf(reading)).toBe(wasAt);
    expect(reading.textContent).toBe(text); // and it is still the same line
    expect(termWrap.scrollTop).toBeLessThan(readingAt); // the viewport really moved
    expect(scroll.isUserScrolledUp()).toBe(true); // and did not snap to following
  });

  it("holds across a long stream, not just one flush", async () => {
    await fillToCap();
    userScrollTo(30 * ROW_H);
    const reading = rowAtViewportTop();
    const wasAt = screenPosOf(reading);

    // Twelve separate flushes, the shape of a streaming agent. Drift compounds,
    // so a per-flush error of one row is unmissable by the end.
    for (let i = 0; i < 12; i++) {
      await commitLines(2);
    }

    expect(reading.parentElement).toBe(outputEl);
    expect(screenPosOf(reading)).toBe(wasAt);
    expect(scroll.isUserScrolledUp()).toBe(true);
  });

  it("still pins to the bottom while following", async () => {
    await fillToCap();
    userScrollTo(outputEl.children.length * ROW_H); // at the bottom = following
    expect(scroll.isUserScrolledUp()).toBe(false);

    await commitLines(3);

    // Following is unchanged by the anchor work: the pin still owns the position.
    expect(scroll.isUserScrolledUp()).toBe(false);
    expect(termWrap.scrollTop).toBe(termWrap.scrollHeight);
  });

  it("leaves the position alone when nothing above it changed", async () => {
    await fillToCap();
    userScrollTo(30 * ROW_H);
    const before = termWrap.scrollTop;

    // Redraw the live window only: rows change below the reader, none above.
    render.handleScreen(
      screenMsg(committed, [row("x0"), row("x1"), row("x2"), row("x3")], [0, 1, 2, 3]),
    );
    await tick();

    expect(termWrap.scrollTop).toBe(before);
  });
});

// The correction measures on-screen DRIFT, not the content-height change, so it
// is idempotent: on a browser with native scroll anchoring (Chrome, Firefox) the
// row is already in the right place by the time this runs, and correcting the
// content delta again would throw the view the other way. This simulates that
// browser by compensating scrollTop the way native anchoring does, and asserts
// the renderer then leaves it alone.
describe("render: manual anchoring does not fight native scroll anchoring", () => {
  let outputEl: HTMLDivElement;
  let termWrap: HTMLDivElement;
  let scrollTop = 0;
  let offsetTopDescriptor: PropertyDescriptor | undefined;
  let realRemove: () => void;
  let committed = 0;

  beforeEach(() => {
    offsetTopDescriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetTop");
    Object.defineProperty(HTMLElement.prototype, "offsetTop", {
      configurable: true,
      get(this: HTMLElement): number {
        const parent = this.parentElement;
        if (!parent) {
          return 0;
        }
        return Array.prototype.indexOf.call(parent.children, this) * ROW_H;
      },
    });
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    outputEl = document.getElementById("term-output") as HTMLDivElement;
    scrollTop = 0;
    committed = 0;
    Object.defineProperty(termWrap, "scrollHeight", {
      configurable: true,
      get: () => outputEl.children.length * ROW_H,
    });
    Object.defineProperty(termWrap, "clientHeight", { configurable: true, get: () => VIEWPORT_H });
    Object.defineProperty(termWrap, "scrollTop", {
      configurable: true,
      get: () => scrollTop,
      set: (v: number) => {
        scrollTop = v;
      },
    });
    // Native scroll anchoring, modelled faithfully: the browser adjusts scrollTop
    // SYNCHRONOUSLY as the content above the anchor is removed, so script never
    // observes the un-compensated state. A MutationObserver cannot stand in for
    // this — its callback is a microtask, so it would run after the flush and both
    // a correct and a double-correcting implementation would look identical.
    realRemove = HTMLElement.prototype.remove;
    HTMLElement.prototype.remove = function patchedRemove(this: HTMLElement): void {
      const wasAbove = this.parentElement === outputEl && this.offsetTop < scrollTop;
      realRemove.call(this);
      if (wasAbove) {
        scrollTop -= ROW_H;
      }
    };

    render.init({ output: outputEl, termWrap });
    render.updateFontMetrics();
    scroll.init({ scrollEl: termWrap });
    render.bind(new LineStore(60));
  });

  afterEach(() => {
    HTMLElement.prototype.remove = realRemove;
    if (offsetTopDescriptor) {
      Object.defineProperty(HTMLElement.prototype, "offsetTop", offsetTopDescriptor);
    }
  });

  it("leaves the position alone when the browser already corrected it", async () => {
    const commitLines = async (n: number): Promise<void> => {
      for (let i = 0; i < n; i++) {
        render.handleScroll(scrollMsg(committed, [`history ${String(committed)}`]));
        committed++;
      }
      render.handleScreen(
        screenMsg(committed, [row("w0"), row("w1"), row("w2"), row("w3")], [0, 1, 2, 3]),
      );
      await new Promise((r) => setTimeout(r, 20));
    };
    await commitLines(60);

    termWrap.scrollTop = 30 * ROW_H;
    termWrap.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(true);

    const reading = outputEl.children[30] as HTMLElement;
    const wasAt = reading.offsetTop - termWrap.scrollTop;

    await commitLines(5);

    // Exactly as correct as the Safari case, and by doing nothing rather than by
    // double-correcting: the row is where it was, not 2x the eviction away.
    expect(reading.parentElement).toBe(outputEl);
    expect(reading.offsetTop - termWrap.scrollTop).toBe(wasAt);
  });
});
