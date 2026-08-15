// @vitest-environment happy-dom
//
// render.bind / render.rebuild: the per-tab store swap the tabs feature uses on
// every switch (design sections 5, 6, 8). Behaviors pinned here:
// 1. bind() points the one renderer at a different, independently-populated
//    LineStore and rebuilds the DOM from it; the previous store's content is
//    gone and the new store's content is shown.
// 2. The rebuild is viewport-first: the live-window row is present after the
//    first frame even when scrollback exceeds the per-frame build budget, so a
//    switch paints the visible screen immediately.
// 3. A bound store already in the alternate screen rebuilds into the alt grid.

import { describe, it, expect, beforeEach, vi } from "vitest";
import * as render from "./render.js";
import { LineStore } from "./store.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

interface FakeCtx {
  font: string;
  measureText: (t: string) => { width: number };
}
HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
  const ctx: FakeCtx = { font: "", measureText: (t: string) => ({ width: t.length * 8 }) };
  return ctx;
} as typeof HTMLCanvasElement.prototype.getContext;

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
}
function screenMsg(
  base: number,
  rows: WireRun[][],
  changed: number[],
  cursor: [number, number] = [0, 0],
): ScreenMessage {
  return {
    type: "screen",
    base,
    rows,
    changed,
    cursor,
    cursorHidden: true,
    cursorStyle: 0,
    cursorBlink: false,
  };
}
function scrollMsg(firstIndex: number, texts: string[]): ScrollMessage {
  return { type: "scroll", firstIndex, lines: texts.map(row) };
}
const tick = (): Promise<void> => new Promise((r) => setTimeout(r, 20));
function texts(out: HTMLElement): string[] {
  return Array.from(out.children)
    .map((c) => (c.textContent ?? "").trim())
    .filter((t) => t.length > 0);
}

describe("render.bind / rebuild (per-tab store swap)", () => {
  let outputEl: HTMLDivElement;
  let termWrap: HTMLDivElement;

  beforeEach(() => {
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    outputEl = document.getElementById("term-output") as HTMLDivElement;
    render.init({ output: outputEl, termWrap });
    render.updateFontMetrics();
  });

  it("swaps to a pre-populated store and rebuilds the DOM from it", async () => {
    // Tab A: fed through the renderer's own (default) store.
    render.handleScroll(scrollMsg(0, ["a", "b", "c"]));
    await tick();
    expect(texts(outputEl)).toEqual(["a", "b", "c"]);

    // Tab B: an independent cache, populated directly.
    const other = new LineStore();
    other.applyScroll(scrollMsg(0, ["x", "y"]));
    other.applyScreen(screenMsg(2, [row("z")], [0]));

    render.bind(other);
    await tick();

    const shown = texts(outputEl);
    expect(shown).toContain("x");
    expect(shown).toContain("y");
    expect(shown).toContain("z");
    expect(shown).not.toContain("a"); // the previous store's content is gone
    expect(render.boundStore()).toBe(other);
  });

  it("rebuilds viewport-first: the live-window row paints in the first frame", () => {
    // Pump animation frames by hand: a real-timer tick() can span a variable
    // number of rAF callbacks on a loaded CI runner, making "exactly one
    // frame" racy. Capturing the callbacks makes each pump exactly one flush.
    const frames: FrameRequestCallback[] = [];
    const realRaf = globalThis.requestAnimationFrame;
    const realCaf = globalThis.cancelAnimationFrame;
    globalThis.requestAnimationFrame = (cb: FrameRequestCallback): number => {
      frames.push(cb);
      return frames.length;
    };
    globalThis.cancelAnimationFrame = (id: number) => {
      void id; // captured frames are never cancelled in this test
    };
    try {
      const s = new LineStore();
      const N = 700; // > MAX_ROWS_PER_FRAME (300)
      s.applyScroll(
        scrollMsg(
          0,
          Array.from({ length: N }, (_, i) => `h${i}`),
        ),
      );
      s.applyScreen(screenMsg(N, [row("LIVE")], [0]));

      render.bind(s);
      frames.shift()?.(performance.now()); // exactly one frame

      // Even though 700 scrollback rows exceed one frame's budget, the live
      // window row builds first, so it is present after a single frame.
      const liveRow = outputEl.querySelector(`[data-abs="${String(N)}"]`);
      expect(liveRow).not.toBeNull();
      expect((liveRow?.textContent ?? "").trim()).toBe("LIVE");

      // The deepest scrollback has not all been built yet (budgeted across frames).
      expect(outputEl.querySelector(`[data-abs="0"]`)).toBeNull();

      // Let the remaining frames drain one at a time; everything lands exactly once.
      for (let i = 0; i < 15 && outputEl.querySelector(`[data-abs="0"]`) === null; i++) {
        const cb = frames.shift();
        if (!cb) {
          break;
        }
        cb(performance.now());
      }
      expect(outputEl.querySelector(`[data-abs="0"]`)).not.toBeNull();
    } finally {
      globalThis.requestAnimationFrame = realRaf;
      globalThis.cancelAnimationFrame = realCaf;
    }
  });

  it("rebuilds a store that is in the alternate screen into the alt grid", async () => {
    const s = new LineStore();
    s.applyScreen({
      type: "screen",
      base: 0,
      rows: [row("alt0"), row("alt1")],
      changed: [0, 1],
      cursor: [0, 0],
      altActive: true,
      cursorHidden: true,
      cursorStyle: 0,
      cursorBlink: false,
    });

    render.bind(s);
    await tick();

    const shown = texts(outputEl);
    expect(shown).toContain("alt0");
    expect(shown).toContain("alt1");
  });

  // --- The wipe covers content space, not just the rows ---
  //
  // The caret, the predicted cursor, the IME view and the consumer's hidden
  // textarea all sit INSIDE the scroll container with a `top` in content
  // coordinates, so each one holds the container's scrollable overflow at the
  // offset it was last placed at. Measured in Chromium on a 4769-row tab:
  // leaving them behind left the scroller with ZERO rows of content and an
  // 81081px scroll range, the viewport parked at 80281px over nothing (which
  // paints as a black pane), and `stickToBottom` refusing to correct it because
  // it measured `distanceFromBottom() === 0` against the phantom height.
  //
  // happy-dom has no layout, so these pin the mechanical resets the geometry
  // depends on, and each one is synchronous inside `bind` on purpose: the
  // browser paints once between the switch and the first flush, and the clamp
  // has to land before `noteContentShrink` and `pendingRestore.lastWrote` read
  // the offset.

  it("hides the caret overlay synchronously in bind, before any frame runs", async () => {
    render.handleScreen({
      ...screenMsg(0, [row("live")], [0], [0, 2]),
      cursorHidden: false,
    });
    await tick();
    const caret = termWrap.querySelector(".term-cursor-overlay");
    expect(caret?.classList.contains("visible")).toBe(true);

    // No frame is pumped after this: the caret must already be out of content
    // space when bind returns.
    render.bind(new LineStore(), { view: null });
    expect(caret?.classList.contains("visible")).toBe(false);
  });

  it("hides the predicted cursor synchronously in bind", async () => {
    render.handleScreen(screenMsg(0, [row("a"), row("b")], [0, 1]));
    await tick();
    render.setPredictedCursor(1, 5, true);
    const pred = termWrap.querySelector(".pred-cursor");
    expect(pred?.classList.contains("visible")).toBe(true);

    render.bind(new LineStore(), { view: null });
    expect(pred?.classList.contains("visible")).toBe(false);
  });

  it("fires onCursorMove during the wipe, so the consumer's overlays move too", async () => {
    // The IME view and the hidden textarea are the CONSUMER's and sit in the
    // same content space; the cursor seam is the only way to reach them, and it
    // has to fire while the DOM is wiped rather than a frame later.
    const moves: number[] = [];
    render.init({
      output: outputEl,
      termWrap,
      onCursorMove: () => {
        moves.push(render.getCursorPx().top);
      },
    });
    render.updateFontMetrics();
    render.handleScroll(scrollMsg(0, ["a", "b", "c"]));
    await tick();
    const beforeBind = moves.length;
    expect(beforeBind).toBeGreaterThan(0);

    render.bind(new LineStore(), { view: null });
    // Synchronous: no frame has run since bind returned.
    expect(moves.length).toBe(beforeBind + 1);
  });
});

describe("render.init maxLines (the consumer-plumbed retained-line cap)", () => {
  let outputEl: HTMLDivElement;
  let termWrap: HTMLDivElement;

  beforeEach(() => {
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    outputEl = document.getElementById("term-output") as HTMLDivElement;
  });

  it("caps the implicit store (and therefore the DOM row budget) at the given value", async () => {
    // A consumer with a phone-sized memory budget passes maxLines through
    // createTerminal -> render.init; the implicit store then evicts at that
    // cap instead of the 5000 default, which bounds both the retained
    // WireRun arrays and the DOM rows built from them.
    render.init({ output: outputEl, termWrap, maxLines: 8 });
    render.updateFontMetrics();
    const lines = Array.from({ length: 12 }, (_, i) => `l${String(i)}`);
    render.handleScroll(scrollMsg(0, lines));
    await tick();
    expect(render.getHighestIndex()).toBe(11);
    // Cap 8 (batch of 1 at this size): the oldest rows evicted, newest kept.
    expect(render.boundStore().oldestIndex()).toBe(4);
    expect(outputEl.querySelectorAll(".term-row").length).toBeLessThanOrEqual(8);
    expect(texts(outputEl)).toContain("l11");
    expect(texts(outputEl)).not.toContain("l0");
  });

  it("always installs a fresh implicit store: a custom cap does not leak into the next attachment", async () => {
    // gpt R1 F2: the store is module-global, so a re-init WITHOUT the option
    // used to keep (and merely reset) whatever capped store the previous
    // attachment installed — a destroy/remount silently inherited an 8-line
    // terminal. init is the attachment boundary: it now always constructs a
    // fresh store, capped only when THIS call says so.
    render.init({ output: outputEl, termWrap, maxLines: 8 });
    render.updateFontMetrics();
    const lines = Array.from({ length: 12 }, (_, i) => `l${String(i)}`);
    render.handleScroll(scrollMsg(0, lines));
    await tick();
    expect(render.boundStore().oldestIndex()).toBe(4); // capped at 8

    render.init({ output: outputEl, termWrap }); // re-attach, option omitted
    render.updateFontMetrics();
    render.handleScroll(scrollMsg(0, lines));
    await tick();
    // Default cap restored: nothing evicted at 12 lines.
    expect(render.boundStore().oldestIndex()).toBe(0);
    expect(texts(outputEl)).toContain("l0");
  });

  it("warns on and ignores a non-positive or non-integer cap (default applies)", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    try {
      render.init({ output: outputEl, termWrap, maxLines: 0 });
      render.updateFontMetrics();
      const lines = Array.from({ length: 12 }, (_, i) => `l${String(i)}`);
      render.handleScroll(scrollMsg(0, lines));
      await tick();
      // Nothing evicted: the bogus cap was ignored, not applied as "retain 0".
      expect(render.boundStore().oldestIndex()).toBe(0);
      expect(texts(outputEl)).toContain("l0");
      expect(texts(outputEl)).toContain("l11");
      expect(warn).toHaveBeenCalledWith(expect.stringContaining("maxLines"));
    } finally {
      warn.mockRestore();
    }
  });
});
