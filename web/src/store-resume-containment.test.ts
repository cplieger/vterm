// The resume-ack budget pass's CONTAINMENT decision, in both directions.
//
// docs/paged-scrollback.md §5.3 makes a normative claim about it — "a FOLLOWING
// viewport is OUTSIDE every reclassified band by definition (it is looking at
// the live tail, not at cache — the common case, and the one §3's memory picture
// is keyed on)" — so a disposable band drains to `prefetchThreshold`, while a
// reader in history keeps the full `browseCacheCap`.
//
// The store does NOT decide which of those it is looking at; the renderer passes
// `following`, because it is the only layer that knows (docs/scroll-position-
// fidelity.md §7.3). Every version that inferred it here was wrong somewhere:
// from `viewportAbs >= win.base` it read a descriptor the replay-jump step
// deliberately RETIRES, so it answered "not following" for every predicted jump
// and kept 2500 where the design budgets 500 — ~2000 lines over budget on the
// path §5.3 names as the primary real population. Narrowing it with a
// cache-membership test then broke the opposite direction, draining a reader
// whose anchor row simply was not held.
//
// So these tests drive `following` as an INPUT and pin that the store honors it
// both ways, plus the case that made the inference untenable: a reader in
// history whose anchor row the store does not hold at all.

import { describe, it, expect } from "vitest";
import { BROWSE_CACHE_CAP, LineStore, PREFETCH_THRESHOLD } from "./store.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
}
function screenMsg(base: number, height: number): ScreenMessage {
  const rows: WireRun[][] = [];
  const changed: number[] = [];
  for (let y = 0; y < height; y++) {
    rows.push(row(`w${String(y)}`));
    changed.push(y);
  }
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
function scrollMsg(firstIndex: number, count: number): ScrollMessage {
  const lines: WireRun[][] = [];
  for (let i = 0; i < count; i++) {
    lines.push(row(`line ${String(firstIndex + i)}`));
  }
  return { type: "scroll", firstIndex, lines };
}

/** A store holding `tail` history lines plus a live window above them. */
function withTail(tail: number): LineStore {
  const s = new LineStore();
  s.applyScroll(scrollMsg(0, tail));
  s.applyScreen(screenMsg(tail, 4));
  return s;
}

describe("resume-ack budget pass: containment for a following reader", () => {
  it("drains a replay-jump band to the small target when the reader is at the tail", () => {
    const s = withTail(3000);
    // A FOLLOWING reader is looking at the live window, so its absolute index is
    // the window base — which is exactly what the renderer reports for a
    // following viewport.
    const viewportAbs = 3000;

    // A long-absence attach. sentHaveThrough is the store's HIGHEST index — what
    // the real client sends (getHaveThrough -> render.getHighestIndex) — so it is
    // the window's BOTTOM row, not the last history line. That matters: the jump
    // band is [oldest, sentHaveThrough + 1), so it spans the old window rows the
    // following reader is looking at. A band that stopped below the window would
    // not contain them and the bug would not reproduce.
    s.applyResumeAck({
      epochChanged: false,
      committed: 10000,
      serverOldest: 6000,
      paging: true,
      sentHaveThrough: 3003,
      sentReplayMax: null,
      viewportAbs,
      following: true,
    });

    // The band is disposable and the reader is not in it, so only a scroll-up
    // buffer stays hot. The bound is the design's own, not `prefetchThreshold`
    // exactly: the pass exempts every line within `prefetchThreshold` of the
    // viewport and accepts that remainder as stated overshoot, so the ceiling is
    // `2 * prefetchThreshold + 1` (the statically-asserted invariant in §5.3).
    expect(s.browseCacheSize()).toBeLessThanOrEqual(2 * PREFETCH_THRESHOLD + 1);
    // The regression, as the number it actually produced: 2500 retained where the
    // design budgets ~500 is ~2000 lines of phone memory nothing accounts for.
    expect(s.browseCacheSize()).toBeLessThan(BROWSE_CACHE_CAP);
  });

  it("keeps the full cache for a reader in history whose anchor row is NOT held", () => {
    // The case that killed the inference. Two doors reach it, both ordinary: a
    // HOLE inside the cache, and an ARMED RESTORE whose remembered row the store
    // has since dropped — which is the anchor the renderer deliberately prefers
    // over the live measurement during a rebuild.
    //
    // A membership-based answer says "not on cache, so drain", and the reader
    // loses their depth: measured 801 rows kept instead of the cap. Where the
    // reader IS does not depend on whether that one row is currently resident.
    // Two retained runs with a genuine hole between them, and the anchor IN the
    // hole — the shape a prior budget pass or cap trim leaves behind.
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 1200));
    s.applyScroll(scrollMsg(1400, 1600));
    s.applyScreen(screenMsg(3000, 4));
    const anchorAbs = 1300;
    expect(s.getLine(anchorAbs)).toBeUndefined(); // the reader's row is not held

    s.applyResumeAck({
      epochChanged: false,
      committed: 10000,
      serverOldest: 6000,
      paging: true,
      sentHaveThrough: 3003,
      sentReplayMax: null,
      viewportAbs: anchorAbs,
      following: false, // in history, wherever that row went
    });

    expect(s.browseCacheSize()).toBe(BROWSE_CACHE_CAP);
  });

  it("keeps the full cache for a reader parked INSIDE the reclassified band", () => {
    // The other half, so the fix cannot be "always take the small target": a deep
    // reader's rows must survive the transition, and the TTL cleans up later.
    const s = withTail(3000);
    const viewportAbs = 1200; // parked in history, inside [0, 3000)

    s.applyResumeAck({
      epochChanged: false,
      committed: 10000,
      serverOldest: 6000,
      paging: true,
      sentHaveThrough: 3003,
      sentReplayMax: null,
      viewportAbs,
      following: false,
    });

    // Asserted as the CAP exactly, which is §5.3's contract for a reader inside
    // the band ("or the full browseCacheCap when it does"). A range assertion is
    // not enough here, and this is measured rather than assumed: taking the small
    // target instead leaves 2304, because the viewport exemption protects most of
    // the band and the pass accepts the rest as stated overshoot. So `> 500` and
    // even `> 2 * PREFETCH_THRESHOLD + 1` both hold for the WRONG behavior — the
    // first two versions of this assertion were vacuous, and a hard-coded
    // `inside = false` (the exact failure this test names) passed them both.
    expect(s.browseCacheSize()).toBe(BROWSE_CACHE_CAP);
  });
});
