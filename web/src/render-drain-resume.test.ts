// handleScrollPosition: the renderer's scroll-position contract, its bounded
// drain recovery, and the reschedule rule's canary
// (docs/tab-switch-repaint.md §3.1, §4.1).
//
// The gate is the subject, not the resume. `flushRender` runs three position
// invariants unconditionally (an armed view restore, the read anchor, the bottom
// pin) and only the DRAIN is queue-gated, so a flush scheduled from a scroll
// handler would move the viewport at moments the renderer never flushed at
// before. Two tests exist to prove that does not happen, and one of them
// (`would have pinned`) exists to prove the other is not vacuous: it drives a
// real flush from the same state and shows the pin firing, so a resume that
// silently scheduled one could not pass.
//
// Frame discipline in this file: rAF callbacks are held in a Map under monotonic
// handles and are CANCELLED, never dropped by reassigning the queue. Dropping
// them was an earlier version's bug: the module's `pendingFrame` slot stayed
// occupied, so the renderer believed a frame was scheduled, every later
// `scheduleFlush` no-oped, and two alt tests asserted over states they had not
// built. A test that discards a scheduled callback is testing a browser that does
// not exist.

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import * as render from "./render.js";
import * as scroll from "./scroll.js";
import { LineStore } from "./store.js";
import { installRowGeometry, type RowGeometry } from "./test-helpers/scroll-fixture.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

interface FakeCtx {
  font: string;
  measureText: (t: string) => { width: number };
}
HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
  const ctx: FakeCtx = { font: "", measureText: (t: string) => ({ width: t.length * 8 }) };
  return ctx;
} as typeof HTMLCanvasElement.prototype.getContext;

const ROW_H = 17;
const VIEWPORT_H = 170; // 10 rows visible
const WINDOW_H = 4;

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
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
// `altActive`, NOT `alt`. An earlier version of this file used `alt: true`, which
// the store simply ignores, so every "alt" assertion here ran against a
// main-screen store and proved nothing at all.
function altMsg(rows: WireRun[][]): ScreenMessage {
  return {
    ...screenMsg(
      0,
      rows,
      rows.map((_, i) => i),
    ),
    altActive: true,
  };
}
function scrollMsg(firstIndex: number, texts: string[]): ScrollMessage {
  return { type: "scroll", firstIndex, lines: texts.map(row) };
}

/** A store holding `count` history lines plus a live window above them. */
function populated(count: number, from = 0): LineStore {
  const s = new LineStore();
  const texts: string[] = [];
  for (let i = 0; i < count; i++) {
    texts.push(`line ${String(from + i)}`);
  }
  s.applyScroll(scrollMsg(from, texts));
  const base = from + count;
  s.applyScreen(
    screenMsg(base, [row("w0"), row("w1"), row("w2"), row("w3")], [0, 1, 2, WINDOW_H - 1]),
  );
  return s;
}

interface Frames {
  pump: () => void;
  pumpUntilIdle: (limit?: number) => number;
  pending: () => number;
  install: () => void;
  /** Queue the next callback but return no handle, so the renderer's record of a
   *  scheduled frame is lost while the callback itself still runs. This is how the
   *  canary's target state gets built: rows owed, the module's `pendingFrame` slot
   *  empty, not alt, the error streak below the cap. That state is unreachable
   *  through the renderer's own paths — which is the invariant the canary asserts —
   *  so the only honest way to test the assertion is to break the environment it
   *  trusts, rather than to add a test-only export to a published API. */
  loseNextHandle: () => void;
}

function frameHarness(): Frames {
  let next = 1;
  let queue = new Map<number, FrameRequestCallback>();
  let lose = false;
  const runDue = (): void => {
    const due = [...queue.values()];
    queue.clear();
    for (const cb of due) {
      cb(performance.now());
    }
  };
  return {
    install() {
      next = 1;
      queue = new Map();
      lose = false;
      vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback): number | undefined => {
        const h = next++;
        queue.set(h, cb);
        if (lose) {
          lose = false;
          return undefined;
        }
        return h;
      });
      vi.stubGlobal("cancelAnimationFrame", (h: number): void => {
        queue.delete(h);
      });
    },
    pump: runDue,
    pumpUntilIdle(limit = 100) {
      let n = 0;
      while (queue.size > 0 && n < limit) {
        runDue();
        n++;
      }
      return n;
    },
    pending: () => queue.size,
    loseNextHandle() {
      lose = true;
    },
  };
}

describe("handleScrollPosition drain recovery", () => {
  let output: HTMLDivElement;
  let termWrap: HTMLDivElement;
  let geom: RowGeometry;
  let frames: Frames;
  let warn: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    frames = frameHarness();
    frames.install();
    // Spied in this describe too, so an accidental canary firing inside a resume
    // scenario cannot pass unnoticed.
    warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    output = document.getElementById("term-output") as HTMLDivElement;
    geom = installRowGeometry({ output, termWrap, rowHeight: ROW_H, clientHeight: VIEWPORT_H });
    render.init({ output, termWrap });
    render.updateFontMetrics();
    scroll.init({ scrollEl: termWrap });
  });

  afterEach(() => {
    expect(warn).not.toHaveBeenCalled();
    warn.mockRestore();
    geom.restore();
    vi.unstubAllGlobals();
  });

  it("finishes a drain the bounded error path gave up on", () => {
    // The reachable stranded state, and the only one: after
    // MAX_RENDER_NO_PROGRESS_RETRIES passes that throw with zero rows built, the
    // catch in flushRender deliberately stops rescheduling and logs. The queue is
    // still owed and no frame is coming. On an idle session nothing arrives to
    // retry it, which is what makes a scroll the user's only lever.
    const err = vi.spyOn(console, "error").mockImplementation(() => undefined);
    try {
      const store = populated(900);
      const realGetLine = store.getLine.bind(store);
      let failing = true;
      store.getLine = ((abs: number) => {
        if (failing) {
          throw new Error("row build fails deterministically");
        }
        return realGetLine(abs);
      }) as typeof store.getLine;

      render.bind(store);
      frames.pumpUntilIdle();
      const stranded = render.pendingRowCount();
      expect(stranded).toBeGreaterThan(0);
      expect(frames.pending()).toBe(0); // gave up: nothing scheduled
      expect(output.children.length).toBe(0); // and nothing built

      failing = false;
      render.handleScrollPosition();
      expect(frames.pending()).toBe(1);
      frames.pumpUntilIdle();

      expect(render.pendingRowCount()).toBe(0);
      expect(output.children.length).toBeGreaterThan(0);
    } finally {
      err.mockRestore();
    }
  });

  it("retries a give-up ONCE, not once per scroll event", () => {
    // The give-up exists so a deterministically throwing row cannot become a
    // 60fps loop. A scroll-driven retry with no bound just moves that loop
    // outside the module: a flick is one position event per frame, and the
    // position callback fires for the browser's own clamps too.
    const err = vi.spyOn(console, "error").mockImplementation(() => undefined);
    try {
      const store = populated(900);
      store.getLine = (() => {
        throw new Error("permanently stuck row");
      }) as typeof store.getLine;

      render.bind(store);
      frames.pumpUntilIdle();
      expect(render.pendingRowCount()).toBeGreaterThan(0);
      expect(frames.pending()).toBe(0);
      const afterGiveUp = err.mock.calls.length;

      // The first scroll spends the one retry.
      render.handleScrollPosition();
      expect(frames.pending()).toBe(1);
      frames.pumpUntilIdle();
      const afterRetry = err.mock.calls.length;
      expect(afterRetry).toBeGreaterThan(afterGiveUp);

      // Fifty more position events, as a gesture would produce, schedule nothing
      // and log nothing.
      for (let i = 0; i < 50; i++) {
        render.handleScrollPosition();
      }
      expect(frames.pending()).toBe(0);
      expect(err.mock.calls.length).toBe(afterRetry);
    } finally {
      err.mockRestore();
    }
  });

  it("does not spend the retry one frame before the give-up exists", () => {
    // The catch reaches `streak === MAX` from its own progress-less branch and
    // schedules a frame on the way out, so for exactly one frame the streak sits at
    // the cap with a flush pending. A scroll landing there must not spend the retry:
    // `scheduleFlush` would no-op on the occupied slot, so the spend buys nothing,
    // and the real give-up one frame later would find its lever already gone.
    const err = vi.spyOn(console, "error").mockImplementation(() => undefined);
    try {
      const store = populated(900);
      const realGetLine = store.getLine.bind(store);
      let failing = true;
      store.getLine = ((abs: number) => {
        if (failing) {
          throw new Error("stuck for now");
        }
        return realGetLine(abs);
      }) as typeof store.getLine;

      render.bind(store);
      // Pump exactly to the pass that takes the streak to the cap. Three throwing
      // passes take it 0 -> 1 -> 2 -> 3, and each one schedules on the way out, so
      // here the streak is AT the cap with a frame still pending and the give-up has
      // not happened yet. (The fourth pass is the give-up.)
      frames.pump();
      frames.pump();
      frames.pump();
      expect(frames.pending()).toBe(1);

      // A scroll here must be a no-op, not a spend.
      render.handleScrollPosition();
      expect(frames.pending()).toBe(1);

      // Now let the give-up land, and the retry must still be available.
      frames.pumpUntilIdle();
      expect(frames.pending()).toBe(0);
      failing = false;
      render.handleScrollPosition();
      expect(frames.pending()).toBe(1);
      frames.pumpUntilIdle();
      expect(render.pendingRowCount()).toBe(0);
    } finally {
      err.mockRestore();
    }
  });

  it("re-arms the retry when anything clears the streak, so it is not one per attachment", () => {
    // The defect this guards: carrying the spent-retry state in a flag beside the
    // streak meant clearing it at every site that clears the streak, and two of
    // the five were missed. One give-up plus one scroll then disabled the resume
    // for the rest of the attachment, and nothing could see it — that state
    // presents to the canary as a plain give-up. A rebuild is the cheapest of the
    // clearing paths, and a tab switch performs one.
    const err = vi.spyOn(console, "error").mockImplementation(() => undefined);
    try {
      const store = populated(900);
      const realGetLine = store.getLine.bind(store);
      let failing = true;
      store.getLine = ((abs: number) => {
        if (failing) {
          throw new Error("stuck for now");
        }
        return realGetLine(abs);
      }) as typeof store.getLine;

      render.bind(store);
      frames.pumpUntilIdle();
      expect(frames.pending()).toBe(0);

      // Spend the one retry, which throws again and gives up again.
      render.handleScrollPosition();
      frames.pumpUntilIdle();
      expect(frames.pending()).toBe(0);
      render.handleScrollPosition();
      expect(frames.pending()).toBe(0); // spent

      // A rebuild (what a tab switch does) clears the streak, so the lever returns.
      render.rebuild();
      frames.pumpUntilIdle();
      failing = false;
      render.handleScrollPosition();

      expect(frames.pending()).toBe(1);
      frames.pumpUntilIdle();
      expect(render.pendingRowCount()).toBe(0);
    } finally {
      err.mockRestore();
    }
  });

  it("schedules nothing when the queue is empty", () => {
    render.bind(populated(40));
    frames.pumpUntilIdle();
    expect(render.pendingRowCount()).toBe(0);

    render.handleScrollPosition();

    expect(frames.pending()).toBe(0);
  });

  it("does not pin a reader parked inside the bottom tolerance", () => {
    // scroll.ts engages follow anywhere within BOTTOM_TOLERANCE_PX (24) of the
    // tail, while stickToBottom pins on ANY non-zero gap. So a reader who stops
    // a few pixels short is following AND not at the bottom, which is exactly
    // the state an ungated flush-on-scroll would snap to the bottom.
    render.bind(populated(900));
    frames.pumpUntilIdle();

    const tail = termWrap.scrollHeight - termWrap.clientHeight;
    termWrap.scrollTop = tail - 8;
    termWrap.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(false);
    const parked = termWrap.scrollTop;
    expect(parked).toBeLessThan(tail);

    render.handleScrollPosition();
    frames.pumpUntilIdle();

    expect(termWrap.scrollTop).toBe(parked);
  });

  it("would have pinned that same reader if a flush had run, so the gate is what holds it", () => {
    // The red check for the test above. Same state, but a real flush is driven
    // (an inbound frame), and the pin fires. Without this, a resume that
    // scheduled a flush unconditionally would still pass the previous test if the
    // pin happened to be a no-op for an unrelated reason.
    render.bind(populated(900));
    frames.pumpUntilIdle();

    const tail = termWrap.scrollHeight - termWrap.clientHeight;
    termWrap.scrollTop = tail - 8;
    termWrap.dispatchEvent(new Event("scroll"));
    const parked = termWrap.scrollTop;

    render.handleScreen(screenMsg(900, [row("w0"), row("w1"), row("w2"), row("w3")], [0]));
    frames.pumpUntilIdle();

    expect(termWrap.scrollTop).toBeGreaterThan(parked);
  });

  it("stays out of the way of a drain that is still scheduled", () => {
    // The single-slot guard in scheduleFlush already covers this, so the resume
    // must not double-schedule mid-rebuild.
    render.bind(populated(900));
    frames.pump();
    expect(render.pendingRowCount()).toBeGreaterThan(0);
    expect(frames.pending()).toBe(1);

    render.handleScrollPosition();

    expect(frames.pending()).toBe(1);
  });

  it("refuses while the alt screen suspends a non-empty queue", () => {
    // Alt is the reschedule rule's third outcome: suspended, resumption edge is
    // alt exit. The suspended state is reached through the FLUSH, not through
    // rebuild (rebuild clears the queue when the store is already alt), so it has
    // to be built that way: queue owed, one flush takes the alt branch and does
    // not reschedule, queue still owed with no frame pending.
    const store = populated(900);
    render.bind(store);
    frames.pump();
    const owed = render.pendingRowCount();
    expect(owed).toBeGreaterThan(0);

    store.applyScreen(altMsg([row("alt0"), row("alt1")]));
    frames.pumpUntilIdle();

    // The suspension really is in place: rows still owed, nothing scheduled.
    expect(render.pendingRowCount()).toBe(owed);
    expect(frames.pending()).toBe(0);
    expect(output.querySelectorAll("[data-abs]").length).toBe(0);

    render.handleScrollPosition();

    // Refused by its own guard, not by the flush body's early return.
    expect(frames.pending()).toBe(0);
    expect(output.querySelectorAll("[data-abs]").length).toBe(0);
  });

  it("resumes at the named edge: leaving alt drains the suspended queue", () => {
    // The other half of the same invariant. If alt exit did not re-queue, the
    // refusal above would be a leak rather than a suspension.
    const store = populated(900);
    render.bind(store);
    frames.pump();
    store.applyScreen(altMsg([row("alt0")]));
    frames.pumpUntilIdle();
    expect(output.querySelectorAll("[data-abs]").length).toBe(0);

    store.applyScreen(screenMsg(900, [row("w0"), row("w1"), row("w2"), row("w3")], [0, 1, 2, 3]));
    render.handleScreen(screenMsg(900, [row("w0"), row("w1"), row("w2"), row("w3")], [0, 1, 2, 3]));
    frames.pumpUntilIdle();

    expect(output.querySelectorAll("[data-abs]").length).toBeGreaterThan(0);
    expect(render.pendingRowCount()).toBe(0);
  });
});

describe("the reschedule rule's canary", () => {
  let output: HTMLDivElement;
  let termWrap: HTMLDivElement;
  let geom: RowGeometry;
  let frames: Frames;
  let warn: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    frames = frameHarness();
    frames.install();
    warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    output = document.getElementById("term-output") as HTMLDivElement;
    geom = installRowGeometry({ output, termWrap, rowHeight: ROW_H, clientHeight: VIEWPORT_H });
    render.init({ output, termWrap });
    render.updateFontMetrics();
    scroll.init({ scrollEl: termWrap });
  });

  afterEach(() => {
    warn.mockRestore();
    geom.restore();
    vi.unstubAllGlobals();
  });

  it("stays silent through a multi-frame backlog, where a frame is always scheduled", () => {
    render.bind(populated(1500));
    for (let i = 0; i < 10 && frames.pending() > 0; i++) {
      frames.pump();
      expect(warn).not.toHaveBeenCalled();
    }
    expect(warn).not.toHaveBeenCalled();
  });

  it("stays silent while alt suspends a non-empty queue", () => {
    const store = populated(900);
    render.bind(store);
    frames.pump();
    expect(render.pendingRowCount()).toBeGreaterThan(0);

    store.applyScreen(altMsg([row("alt0")]));
    frames.pumpUntilIdle();

    // Rows are still owed with nothing scheduled, but the suspension is NAMED.
    expect(render.pendingRowCount()).toBeGreaterThan(0);
    expect(frames.pending()).toBe(0);
    expect(warn).not.toHaveBeenCalled();
  });

  it("stays silent after a give-up, which is named and already logged", () => {
    const err = vi.spyOn(console, "error").mockImplementation(() => undefined);
    try {
      const store = populated(900);
      store.getLine = (() => {
        throw new Error("stuck");
      }) as typeof store.getLine;
      render.bind(store);
      frames.pumpUntilIdle();

      expect(render.pendingRowCount()).toBeGreaterThan(0);
      expect(frames.pending()).toBe(0);
      expect(err).toHaveBeenCalled();
      expect(warn).not.toHaveBeenCalled();
    } finally {
      err.mockRestore();
    }
  });

  it("reports an unnamed stall, once per episode, and re-arms after it heals", () => {
    // The positive case. Every named state is excluded above, so without this the
    // canary is only ever proven silent, and deleting the warn would pass the whole
    // battery.
    render.bind(populated(1500));
    frames.loseNextHandle();
    frames.pump();

    // Rows owed, the renderer believes nothing is scheduled, not alt, streak 0.
    expect(render.pendingRowCount()).toBeGreaterThan(0);
    expect(warn).toHaveBeenCalledTimes(1);
    expect(String(warn.mock.calls[0]?.[0])).toContain("no scheduled frame");

    // The same episode does not spam: the callback is still queued in the harness,
    // so pumping it reaches the canary again with the slot still empty.
    frames.loseNextHandle();
    frames.pump();
    expect(warn).toHaveBeenCalledTimes(1);

    // Heal it and drain fully. The latch clears on the first healthy end.
    frames.pumpUntilIdle();
    expect(render.pendingRowCount()).toBe(0);

    // A NEW stall must be heard. A once-per-process latch would stay silent here,
    // which is what made the first version of this canary go deaf after one report.
    render.bind(populated(1500));
    frames.loseNextHandle();
    frames.pump();
    expect(warn).toHaveBeenCalledTimes(2);
  });

  it("re-arms across init, the attachment boundary", () => {
    render.bind(populated(1500));
    frames.loseNextHandle();
    frames.pump();
    expect(warn).toHaveBeenCalledTimes(1);

    render.init({ output, termWrap });
    render.updateFontMetrics();
    render.bind(populated(1500));
    frames.loseNextHandle();
    frames.pump();

    expect(warn).toHaveBeenCalledTimes(2);
  });
});
