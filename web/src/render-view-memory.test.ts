// @vitest-environment happy-dom
//
// Per-view scroll memory: captureViewMemory + bind({ view })
// (docs/scroll-position-fidelity.md §3).
//
// The behavior under test is a ROUND TRIP across a rebuild that spans several
// frames, on a container that CLAMPS scrollTop like a browser. Both halves of
// that sentence are load-bearing, and their absence is why the bug shipped:
//
//   - the rebuild builds at most MAX_ROWS_PER_FRAME (300) rows per frame, so a
//     restore issued once, at frame 1, is replayed against ~301 of up to 5000
//     rows; and
//   - a real container clamps the write to scrollHeight - clientHeight, so that
//     replay silently lands at the bottom of the PARTIAL content and the saved
//     position is gone. Nothing retried it.
//
// So every assertion here is made by LINE (the data-abs at the viewport top),
// never by pixel offset, and the drain is pumped frame by frame rather than
// awaited in one go.

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
function scrollMsg(firstIndex: number, texts: string[]): ScrollMessage {
  return { type: "scroll", firstIndex, lines: texts.map(row) };
}

describe("per-view scroll memory across a multi-frame rebuild", () => {
  let output: HTMLDivElement;
  let termWrap: HTMLDivElement;
  let geom: RowGeometry;
  let frames: FrameRequestCallback[];

  /** Run exactly one animation frame's worth of scheduled work. */
  function pumpFrame(): void {
    const due = frames;
    frames = [];
    for (const cb of due) {
      cb(performance.now());
    }
  }
  /** Drain every frame the renderer chains, bounded so a bug cannot hang. */
  function pumpUntilIdle(limit = 100): number {
    let n = 0;
    while (frames.length > 0 && n < limit) {
      pumpFrame();
      n++;
    }
    return n;
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

  /** The absolute index of the row currently at the viewport top. */
  function lineAtViewportTop(): number {
    const target = termWrap.scrollTop;
    let best = -1;
    for (const el of Array.from(output.children) as HTMLElement[]) {
      const abs = Number(el.dataset["abs"] ?? "");
      if (!Number.isFinite(abs)) {
        continue;
      }
      if (el.offsetTop >= target) {
        best = abs;
        break;
      }
    }
    return best;
  }

  /** The child element currently at the viewport top, marker or not. */
  function rowAtTop(): HTMLElement | null {
    const target = termWrap.scrollTop;
    for (const el of Array.from(output.children) as HTMLElement[]) {
      if (el.offsetTop >= target) {
        return el;
      }
    }
    return null;
  }

  function userScrollTo(top: number): void {
    termWrap.scrollTop = top;
    termWrap.dispatchEvent(new Event("scroll"));
  }

  beforeEach(() => {
    frames = [];
    vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback): number => {
      frames.push(cb);
      return frames.length;
    });
    vi.stubGlobal("cancelAnimationFrame", (): void => {
      /* the pump drops un-run callbacks with the array */
    });
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    output = document.getElementById("term-output") as HTMLDivElement;
    geom = installRowGeometry({ output, termWrap, rowHeight: ROW_H, clientHeight: VIEWPORT_H });
    render.init({ output, termWrap });
    render.updateFontMetrics();
    scroll.init({ scrollEl: termWrap });
  });

  afterEach(() => {
    geom.restore();
    vi.unstubAllGlobals();
  });

  it("captures the LINE at the viewport top, not a pixel offset", () => {
    render.bind(populated(700));
    pumpUntilIdle();
    userScrollTo(300 * ROW_H);

    const view = render.captureViewMemory();
    expect(view).not.toBeNull();
    expect(view?.abs).toBe(300);
    expect(view?.following).toBe(false);
  });

  it("restores a scrolled-up reader to the same LINE, over a drain the saved pixel offset could not survive", () => {
    // Tab A: 700 lines, reader parked 300 lines in.
    const tabA = populated(700);
    render.bind(tabA);
    pumpUntilIdle();
    userScrollTo(300 * ROW_H);
    const saved = render.captureViewMemory();
    expect(saved?.abs).toBe(300);

    // Away to tab B, then back. The rebuild spans several frames: 700 rows at
    // 300/frame. THE point of the test is that the restore is not a one-shot at
    // frame 1 — at that moment the container is a fraction of its final height
    // and any pixel write would be clamped away.
    render.bind(populated(50, 5000));
    pumpUntilIdle();
    render.bind(tabA, { view: saved });
    const framesUsed = pumpUntilIdle();
    expect(framesUsed).toBeGreaterThan(1);

    expect(lineAtViewportTop()).toBe(300);
    expect(scroll.isUserScrolledUp()).toBe(true);
  });

  it("puts a tab left FOLLOWING back at the bottom, and keeps following", () => {
    const tabA = populated(700);
    render.bind(tabA);
    pumpUntilIdle();
    // At the bottom: the pin owns this, so the saved view says "following".
    const saved = render.captureViewMemory();
    expect(saved?.following).toBe(true);

    render.bind(populated(50, 5000));
    pumpUntilIdle();
    render.bind(tabA, { view: saved });
    pumpUntilIdle();

    expect(scroll.isUserScrolledUp()).toBe(false);
    expect(termWrap.scrollTop).toBe(termWrap.scrollHeight - VIEWPORT_H);
  });

  it("restores the same LINE even when the tab's content grew while it was away", () => {
    // The case a saved PIXEL offset gets wrong by construction: the same offset
    // denotes a different line once the session has produced more output, and a
    // backgrounded tab's session keeps working.
    const tabA = populated(700);
    render.bind(tabA);
    pumpUntilIdle();
    userScrollTo(300 * ROW_H);
    const saved = render.captureViewMemory();

    render.bind(populated(50, 5000));
    pumpUntilIdle();
    // 400 more lines arrive for tab A while it is not bound.
    const grown: string[] = [];
    for (let i = 0; i < 400; i++) {
      grown.push(`late ${String(i)}`);
    }
    tabA.applyScroll(scrollMsg(704, grown));

    render.bind(tabA, { view: saved });
    pumpUntilIdle();

    expect(lineAtViewportTop()).toBe(300);
  });

  it("the incoming tab's follow state gates the FIRST frame, not a later one", () => {
    // The stale-global-flag bug: the follow flag is one per kernel, so binding a
    // following tab while holding in another left the controller holding, the
    // post-flush pin no-op'd, and the cached screen rendered above the viewport.
    const tabA = populated(700);
    render.bind(tabA);
    pumpUntilIdle();
    const followingView = render.captureViewMemory();

    const tabB = populated(700, 9000);
    render.bind(tabB);
    pumpUntilIdle();
    // Mid-content by DOCUMENT ORDER, not by absolute index: under honest
    // geometry a row's pixel offset says nothing about its line number, and a
    // position past the end would simply clamp back to the bottom (leaving the
    // reader following, which would make the assertion below vacuous).
    userScrollTo(300 * ROW_H); // holding in B
    expect(scroll.isUserScrolledUp()).toBe(true);

    render.bind(tabA, { view: followingView });
    // ONE frame only: the state must already be right.
    pumpFrame();
    expect(scroll.isUserScrolledUp()).toBe(false);
  });

  it("a second switch mid-drain does not land the first tab's line in the second tab's store", () => {
    const tabA = populated(700);
    render.bind(tabA);
    pumpUntilIdle();
    userScrollTo(300 * ROW_H);
    const savedA = render.captureViewMemory();

    const tabC = populated(700, 20000);
    render.bind(tabA, { view: savedA });
    pumpFrame(); // mid-drain, restore still armed
    render.bind(tabC); // switch again, no view
    pumpUntilIdle();

    // Tab C holds 20000..20703; A's line 300 must not have been applied here.
    expect(lineAtViewportTop()).not.toBe(300);
    expect(render.pendingRestoreAbs()).toBeNull();
  });

  it("a user scroll mid-drain cancels the restore instead of fighting it", () => {
    const tabA = populated(700);
    render.bind(tabA);
    pumpUntilIdle();
    userScrollTo(300 * ROW_H);
    const saved = render.captureViewMemory();

    render.bind(populated(50, 5000));
    pumpUntilIdle();
    render.bind(tabA, { view: saved });
    pumpFrame(); // one frame of the drain
    expect(render.pendingRestoreAbs()).toBe(300);

    userScrollTo(20 * ROW_H); // the user takes over
    const chosen = lineAtViewportTop();
    pumpUntilIdle();

    expect(render.pendingRestoreAbs()).toBeNull();
    // The reader keeps the line THEY chose, not the one the restore wanted. The
    // chosen line is whatever sat 20 rows into the partially-built surface, which
    // under honest geometry is not 20 — the point is that it is theirs and it is
    // held (by the read anchor) as the rest of the backlog builds above it.
    expect(chosen).not.toBe(300);
    expect(lineAtViewportTop()).toBe(chosen);
  });

  it("the rebuild's own clamp does NOT cancel the restore", () => {
    // The regression this pins: the position seam (scroll.ts's onScrollPosition)
    // fires for the browser's clamps as well as for gestures, including the clamp
    // the wipe itself causes — so cancelling from that signal made the rebuild
    // cancel the restore it exists to serve. Cancellation is established by
    // comparing against what this module last WROTE instead.
    const tabA = populated(700);
    render.bind(tabA);
    pumpUntilIdle();
    userScrollTo(300 * ROW_H);
    const saved = render.captureViewMemory();

    render.bind(tabA, { view: saved });
    // The wipe collapsed the content and the container clamped to 0, which fires
    // a scroll event before any row is rebuilt.
    termWrap.dispatchEvent(new Event("scroll"));
    expect(render.pendingRestoreAbs()).toBe(300);

    pumpUntilIdle();
    expect(lineAtViewportTop()).toBe(300);
  });

  it("gives up honestly when the saved line is no longer in the store", () => {
    const tabA = populated(700);
    render.bind(tabA);
    pumpUntilIdle();
    userScrollTo(300 * ROW_H);
    const saved = render.captureViewMemory();

    // Rebind a store that never held line 300.
    render.bind(populated(50, 5000), { view: saved });
    pumpUntilIdle();

    expect(render.pendingRestoreAbs()).toBeNull();
    expect(scroll.isUserScrolledUp()).toBe(true); // the saved follow state still applies
  });

  it("returns null on the alternate screen rather than remembering an alt row", () => {
    const s = populated(100);
    render.bind(s);
    pumpUntilIdle();
    s.applyScreen({
      ...screenMsg(104, [row("a0"), row("a1"), row("a2"), row("a3")], [0, 1, 2, 3]),
      altActive: true,
    } as ScreenMessage);
    pumpUntilIdle();

    expect(render.captureViewMemory()).toBeNull();
  });

  it("keeps an armed restore alive across a row-height change instead of cancelling it", () => {
    // The trap in the guard that stops updateFontMetrics clobbering a bind's
    // restore: declining to arm is right, but the reflow that provoked the call
    // has ALREADY moved scrollTop, so leaving the existing arm's baseline stale
    // makes the next flush read that move as a gesture and cancel the very
    // restore the guard is protecting — losing both anchors instead of one.
    const tabA = populated(3000);
    render.bind(tabA);
    pumpUntilIdle();
    userScrollTo(100 * ROW_H);
    const saved = render.captureViewMemory();
    expect(saved?.abs).toBe(100);

    render.bind(populated(50, 9000));
    pumpUntilIdle();
    render.bind(tabA, { view: saved });
    // Several frames of the drain: line 100 is deep in a 3000-row backlog that
    // fills newest-first, so it stays unbuilt while the read anchor's per-frame
    // corrections move scrollTop off zero — which is the state the hazard needs.
    pumpFrame();
    pumpFrame();
    pumpFrame();
    expect(render.pendingRestoreAbs()).toBe(100);

    // A font load / zoom / CSS change lands mid-restore. It must SHRINK the rows:
    // taller rows make the container taller, so nothing clamps and the hazard
    // never arises — which is how the first version of this test passed against
    // the bug. Shorter rows lower the maximum and the clamp is destructive.
    geom.setRowHeight(8);
    render.updateFontMetrics();

    expect(render.pendingRestoreAbs()).toBe(100); // still armed, not cancelled
    pumpUntilIdle();
    // Without the baseline refresh the next flush reads the reflow's clamp as a
    // gesture and cancels the restore, so the reader is left wherever the clamp
    // put them instead of on their line.
    expect(lineAtViewportTop()).toBe(100);
  });

  it("expires an armed restore on read, not only inside a flush", () => {
    // applyPendingRestore is reached only from a flush, so an idle surface (queue
    // drained, nothing inbound, no flush scheduled) would hold an arm forever and
    // keep answering the resume transition with a line the reader has long left.
    const tabA = populated(700);
    render.bind(tabA);
    pumpUntilIdle();
    userScrollTo(300 * ROW_H);
    const saved = render.captureViewMemory();

    render.bind(populated(50, 5000));
    pumpUntilIdle();
    render.bind(tabA, { view: saved });
    pumpFrame();
    expect(render.pendingRestoreAbs()).toBe(300);

    // Long after the deadline, with no flush in between.
    vi.setSystemTime(Date.now() + 60_000);
    expect(render.pendingRestoreAbs()).toBeNull();
  });

  it("announces a shrink only for a pass that actually removed rows", () => {
    // The announcement tells the scroll controller "this upward move is my clamp,
    // not a gesture". A pass that removed nothing cannot have clamped, and
    // announcing anyway is unsound on Safari, which updates scrollTop PAST the
    // maximum during an overscroll bounce: the settle back is a downward move in
    // value with no content change, and an arm would swallow the user's own drag.
    const s = new LineStore(120);
    const texts: string[] = [];
    for (let i = 0; i < 100; i++) {
      texts.push(`line ${String(i)}`);
    }
    s.applyScroll(scrollMsg(0, texts));
    s.applyScreen(screenMsg(100, [row("w0"), row("w1"), row("w2"), row("w3")], [0, 1, 2, 3]));
    render.bind(s);
    pumpUntilIdle();

    const spy = vi.spyOn(scroll, "noteContentShrink");

    // Through the renderer, not the store: a direct store mutation schedules no
    // flush, so both halves below would be vacuously true.
    // Growth only: 10 more history lines, still under the 120 cap.
    render.handleScroll(scrollMsg(104, ["a", "b", "c", "d", "e", "f", "g", "h", "i", "j"]));
    pumpUntilIdle();
    expect(spy).not.toHaveBeenCalled();

    // Now push past the cap so eviction actually removes rows.
    const flood: string[] = [];
    for (let i = 0; i < 60; i++) {
      flood.push(`flood ${String(i)}`);
    }
    render.handleScroll(scrollMsg(114, flood));
    pumpUntilIdle();
    expect(spy).toHaveBeenCalled();
    spy.mockRestore();
  });

  it("adopts the tail for an explicitly NULL view, instead of inheriting the outgoing tab's flag", () => {
    // A tab never visited, or one left on the alternate screen, has no anchor to
    // restore — but "no memory" still carries a follow intent, and it is the tail.
    // Without this the null case adopted NOTHING, so those tabs kept whatever flag
    // the tab the user just left had: the stale-global-flag bug, narrowed to the
    // tabs that have nothing saved rather than closed.
    const tabA = populated(300);
    render.bind(tabA);
    pumpUntilIdle();
    userScrollTo(100 * ROW_H); // holding in A
    expect(scroll.isUserScrolledUp()).toBe(true);

    render.bind(populated(300, 9000), { view: null });
    pumpFrame(); // ONE frame: the state must already be right

    expect(scroll.isUserScrolledUp()).toBe(false);
  });

  it("re-resolves past a GAP marker when a cap trim takes the anchored row", () => {
    // firstRowAtOrAfter's half of the marker rule. A cap trim (unlike a discard)
    // keeps the re-resolve, so the reader is moved to the nearest SURVIVING row —
    // and a gap marker sitting at an index above the lost anchor must not be it.
    const s = new LineStore(140);
    const texts: string[] = [];
    for (let i = 0; i < 100; i++) {
      texts.push(`line ${String(i)}`);
    }
    s.applyScroll(scrollMsg(0, texts));
    s.applyScreen(screenMsg(100, [row("w0"), row("w1"), row("w2"), row("w3")], [0, 1, 2, 3]));
    render.bind(s);
    pumpUntilIdle();
    // A far paged-in region leaves a gap, and a marker whose data-abs (the gap's
    // LOW index) sits ABOVE the rows about to be trimmed.
    const far: string[] = [];
    for (let i = 0; i < 30; i++) {
      far.push(`far ${String(i)}`);
    }
    render.noteSolicited(5000, 5030);
    render.handleHistoryReply(scrollMsg(5000, far), null);
    render.clearSolicited();
    pumpUntilIdle();
    expect(output.querySelector(".term-gap-marker")).not.toBeNull();

    userScrollTo(10 * ROW_H); // reading in the oldest run, which the cap will trim
    expect(scroll.isUserScrolledUp()).toBe(true);

    const corrections: number[] = [];
    const spy = vi
      .spyOn(scroll, "adjustForContentShift")
      .mockImplementation((delta: number): void => {
        corrections.push(delta);
      });
    // Push past the cap so the oldest run (including the anchored row) is evicted.
    const more: string[] = [];
    for (let i = 0; i < 60; i++) {
      more.push(`more ${String(i)}`);
    }
    s.applyScroll(scrollMsg(5030, more));
    pumpUntilIdle();
    spy.mockRestore();

    // A correction happened (this is a cap trim, so the anchor re-resolves rather
    // than standing down) and it was computed from a real ROW. With the marker
    // skipped by attribute instead of identity, firstRowAtOrAfter returns the gap
    // marker and the reader is pinned to an annotation about a hole.
    const el = rowAtTop();
    expect(el).not.toBeNull();
    expect(el?.classList.contains("term-gap-marker")).toBe(false);
    expect(el?.classList.contains("term-row")).toBe(true);
  });

  it("never anchors on a marker: the trim marker is not a reading position", () => {
    // A marker is an annotation ABOUT a hole, so holding it in place pins the
    // annotation rather than the text. With a marker as first child and the
    // viewport at the very top, the captured line must be the first real row.
    const s = new LineStore(60);
    const texts: string[] = [];
    for (let i = 0; i < 200; i++) {
      texts.push(`line ${String(i)}`);
    }
    s.applyScroll(scrollMsg(0, texts));
    s.applyScreen(screenMsg(200, [row("w0"), row("w1"), row("w2"), row("w3")], [0, 1, 2, 3]));
    render.bind(s);
    pumpUntilIdle();
    expect(output.querySelector(".term-trim-marker")).not.toBeNull();

    userScrollTo(0);
    const view = render.captureViewMemory();
    expect(view).not.toBeNull();
    expect(view?.abs).toBeGreaterThanOrEqual(0);
  });

  it("never anchors on a GAP marker, which unlike the trim marker carries a data-abs", () => {
    // The trap: a per-gap marker's data-abs is its gap's LOW index — the first
    // ABSENT line — so skipping markers by "has no data-abs" skips only the trim
    // marker and returns a gap marker as a reading position. rowEls can never
    // resolve that index, so a restore anchored there could not land, and a
    // viewportAbs built on it names a line the store does not hold. Membership in
    // rowEls, not the attribute, is what makes a child a content row.
    const s = new LineStore();
    const texts: string[] = [];
    for (let i = 0; i < 100; i++) {
      texts.push(`line ${String(i)}`);
    }
    s.applyScroll(scrollMsg(0, texts));
    s.applyScreen(screenMsg(100, [row("w0"), row("w1"), row("w2"), row("w3")], [0, 1, 2, 3]));
    render.bind(s);
    pumpUntilIdle();
    // A paged-in region far above the tail leaves a real gap, and a marker in it.
    const far: string[] = [];
    for (let i = 0; i < 40; i++) {
      far.push(`far ${String(i)}`);
    }
    render.noteSolicited(5000, 5040);
    render.handleHistoryReply(scrollMsg(5000, far), null);
    render.clearSolicited();
    pumpUntilIdle();
    const marker = output.querySelector<HTMLElement>(".term-gap-marker");
    expect(marker).not.toBeNull();
    expect(marker?.dataset["abs"]).toBeDefined(); // the trap's precondition

    // Park the viewport exactly on the marker. The 40 rows after it are what let
    // a CLAMPING container put it at the top at all; with only a few, the marker
    // is always inside the last viewport-full and the search never lands on it —
    // which is how the first draft of this test passed against the very bug it
    // was written to catch.
    const markerTop = (marker as HTMLElement).offsetTop;
    userScrollTo(markerTop);
    expect(termWrap.scrollTop).toBe(markerTop); // the park actually took
    const view = render.captureViewMemory();

    expect(view).not.toBeNull();
    // Whatever line it picked, the surface must actually HOLD it.
    const picked = output.querySelector(`[data-abs="${String(view?.abs)}"]`);
    expect(picked).not.toBeNull();
    expect(picked?.classList.contains("term-gap-marker")).toBe(false);
    expect(view?.abs).not.toBe(Number(marker?.dataset["abs"]));
  });

  it("stands down instead of re-anchoring when ED3 discards the region under the reader", () => {
    // Symptom (b): an inline TUI emits ED3 on every resize redraw, the server
    // clears its ring but keeps committing monotonically, so the SAME text comes
    // back at HIGHER absolute indices while the old copies are dropped. The
    // anchored index is then not merely missing — the nearest surviving row is
    // content from a different region, and holding it at the reader's screen
    // position is the "random jump on resize" they reported. A cap trim is the
    // opposite case and must still re-resolve (pinned by render-read-anchor).
    const s = new LineStore();
    const texts: string[] = [];
    for (let i = 0; i < 400; i++) {
      texts.push(`line ${String(i)}`);
    }
    s.applyScroll(scrollMsg(0, texts));
    s.applyScreen(screenMsg(400, [row("w0"), row("w1"), row("w2"), row("w3")], [0, 1, 2, 3]));
    render.bind(s);
    pumpUntilIdle();

    userScrollTo(150 * ROW_H + 7); // reading mid-history, mid-row
    expect(scroll.isUserScrolledUp()).toBe(true);

    // The claim is about what the renderer DOES, not only where the viewport ends
    // up: after a region discard the anchor must stand down rather than invent a
    // correction. The end position cannot express that, because the discard
    // collapses the content and the browser's clamp lands on 0 either way — so
    // watch the correction itself.
    const corrections: number[] = [];
    const spy = vi
      .spyOn(scroll, "adjustForContentShift")
      .mockImplementation((delta: number): void => {
        corrections.push(delta);
      });

    // The redraw: scrollback erased, and the transcript re-committed above.
    const before = termWrap.scrollTop;
    const reprint: string[] = [];
    for (let i = 0; i < 200; i++) {
      reprint.push(`reprint ${String(i)}`);
    }
    render.handleScreen({
      ...screenMsg(700, [row("r0"), row("r1"), row("r2"), row("r3")], [0, 1, 2, 3]),
      scrollbackCleared: true,
    } as ScreenMessage);
    render.handleScroll(scrollMsg(500, reprint));
    pumpUntilIdle();

    // 204 surviving rows against a 10-row viewport, so there is real scroll range
    // and a wrong correction is visible. (With only a screenful left, every
    // candidate position clamps to 0 and no assertion can tell a stand-down from
    // a re-anchor — the trap the first draft of this test fell into.)
    expect(termWrap.scrollHeight - VIEWPORT_H).toBeGreaterThan(0);
    // The reader's line is gone, so there is nothing to HOLD: the discard
    // collapsed the content under them and the browser's clamp already destroyed
    // the offset — that part is unrecoverable and is not what this fix claims.
    // What it claims is that no FURTHER correction is invented: the viewport is
    // left exactly where the clamp put it, instead of being dragged so that
    // unrelated reprinted text sits at the old reading position.
    spy.mockRestore();

    expect(before).toBeGreaterThan(0); // the reader really was mid-history
    // No correction invented. Without the stand-down, firstRowAtOrAfter resolves
    // to the reprint's first line — unrelated content at a new index — and pins it
    // to the reader's old screen position, which is the jump they reported.
    expect(corrections.filter((d) => d !== 0)).toEqual([]);
    expect(scroll.isUserScrolledUp()).toBe(true); // and still holding
  });
});
