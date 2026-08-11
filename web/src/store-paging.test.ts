// Unit tests for the store's DEMAND-PAGING half (docs/paged-scrollback.md §5):
// the residency split between a live tail and a disposable browse cache, the
// solicited-range doctrine, the single resume-ack transition (bounds, cap flip,
// replay-jump prediction), the paging floor, and the two exclusions that keep
// disposable cache out of persistence and out of the eviction watermark.
//
// Pure data structure, no DOM. The renderer supplies `viewportAbs` in
// production; here the tests supply it directly, which is the point of the
// store not guessing it.

import { describe, it, expect } from "vitest";
import {
  LineStore,
  BROWSE_CACHE_CAP,
  COMPATIBILITY_TAIL_CAP,
  PAGE_SIZE,
  PREFETCH_THRESHOLD,
  RESIDENT_TAIL_CAP,
} from "./store.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
}

/** A scroll/history frame of `count` lines starting at `firstIndex`. */
function pageMsg(firstIndex: number, count: number): ScrollMessage {
  return {
    type: "scroll",
    firstIndex,
    lines: Array.from({ length: count }, (_, i) => row(`L${firstIndex + i}`)),
  };
}

function screenMsg(base: number, height: number, opts: Partial<{ altActive: boolean }> = {}): ScreenMessage {
  const rows: WireRun[][] = new Array<WireRun[]>(height);
  const changed: number[] = [];
  for (let y = 0; y < height; y++) {
    rows[y] = row(`W${base + y}`);
    changed.push(y);
  }
  return {
    type: "screen",
    base,
    rows,
    changed,
    cursor: [0, 0],
    altActive: opts.altActive ?? false,
    scrollbackCleared: false,
  };
}

/** Every retained absolute index, ascending. */
function heldKeys(s: LineStore): number[] {
  const out: number[] = [];
  s.forEachLine((abs) => out.push(abs));
  return out.sort((a, b) => a - b);
}

/** Fill the store with a contiguous live tail `[0, count)` via ordinary scroll. */
function fillTail(s: LineStore, count: number): void {
  s.applyScroll(pageMsg(0, count));
}

/**
 * Apply a solicited history page the way the connection layer does: mark the
 * window solicited, deliver, then release. Anything that skips `noteSolicited`
 * is testing the UNSOLICITED path, which is a different contract.
 */
function fetchPage(s: LineStore, fromAbs: number, count: number, viewportAbs: number): void {
  s.noteSolicited(fromAbs, fromAbs + count);
  s.applyHistoryScroll(pageMsg(fromAbs, count), viewportAbs);
  s.clearSolicited();
}

describe("LineStore paging: capability and the cap flip", () => {
  it("starts at the compatibility cap and only the ack moves it", () => {
    const s = new LineStore();
    // Before the ack the client cannot know whether the server pages, so it
    // must retain what today's client retains: dropping to the small resident
    // target first would discard history no fetch could bring back.
    expect(s.tailCap()).toBe(COMPATIBILITY_TAIL_CAP);
    s.applyResumeAck({
      epochChanged: false,
      committed: null,
      serverOldest: null,
      paging: true,
      sentHaveThrough: -1,
      sentReplayMax: null,
      viewportAbs: -1,
      following: true,
    });
    expect(s.tailCap()).toBe(RESIDENT_TAIL_CAP);
  });

  it("an unsupported server leaves the cap alone", () => {
    const s = new LineStore();
    s.applyResumeAck({
      epochChanged: false,
      committed: 100,
      serverOldest: 0,
      paging: false,
      sentHaveThrough: -1,
      sentReplayMax: null,
      viewportAbs: -1,
      following: true,
    });
    expect(s.tailCap()).toBe(COMPATIBILITY_TAIL_CAP);
    expect(s.browseCacheSize()).toBe(0);
  });

  it("an explicitly supplied cap makes the flip a no-op", () => {
    // A consumer that asked for N lines gets N in both regimes; the flip exists
    // to lower the DEFAULT, not to override a caller.
    const s = new LineStore(300);
    expect(s.tailCap()).toBe(300);
    s.applyResumeAck({
      epochChanged: false,
      committed: null,
      serverOldest: null,
      paging: true,
      sentHaveThrough: -1,
      sentReplayMax: null,
      viewportAbs: -1,
      following: true,
    });
    expect(s.tailCap()).toBe(300);
  });

  it("an explicit cap EQUAL to the engine default is still the consumer's decision", () => {
    // "Omitted" is a distinct statement from any value, so it is expressed by
    // absence. While it was expressed as the default VALUE, a consumer asking
    // for exactly that number was read as silence and had its cap replaced by
    // the small post-flip target — reachable from web-terminal-ui, whose
    // `scrollbackLines` is a plain number the caller chooses.
    const explicit = new LineStore(COMPATIBILITY_TAIL_CAP);
    expect(explicit.tailCap()).toBe(COMPATIBILITY_TAIL_CAP);
    explicit.applyResumeAck({
      epochChanged: false,
      committed: null,
      serverOldest: null,
      paging: true,
      sentHaveThrough: -1,
      sentReplayMax: null,
      viewportAbs: 0,
      following: false,
    });
    expect(explicit.tailCap()).toBe(COMPATIBILITY_TAIL_CAP); // held, not flipped

    const omitted = new LineStore();
    omitted.applyResumeAck({
      epochChanged: false,
      committed: null,
      serverOldest: null,
      paging: true,
      sentHaveThrough: -1,
      sentReplayMax: null,
      viewportAbs: 0,
      following: false,
    });
    expect(omitted.tailCap()).toBe(RESIDENT_TAIL_CAP); // engine's choice, flipped
  });

  it("the flip reclassifies the excess tail as cache instead of evicting it", () => {
    const s = new LineStore();
    const total = RESIDENT_TAIL_CAP + 400;
    // Based at 1000, not 0, so `hasTrimmedHistory` is not vacuously false: with
    // oldest > 0 it reports true the moment the eviction watermark moves.
    s.applyScroll(pageMsg(1000, total));
    expect(heldKeys(s).length).toBe(total);
    expect(s.hasTrimmedHistory()).toBe(false);

    // A reader parked deep in the reclassified band keeps the full cache.
    s.applyResumeAck({
      epochChanged: false,
      committed: null,
      serverOldest: null,
      paging: true,
      sentHaveThrough: -1,
      sentReplayMax: null,
      viewportAbs: 1010,
      following: true,
    });
    expect(s.browseCacheSize()).toBe(400);
    // Nothing was deleted: 400 lines moved classification, they did not vanish.
    expect(heldKeys(s).length).toBe(total);
    // And the reclassification did NOT advance the eviction watermark, which is
    // what would otherwise make the client claim it had trimmed that history
    // (the renderer would draw the "earlier output trimmed" marker over
    // content it is still holding).
    expect(s.hasTrimmedHistory()).toBe(false);
  });

  it("the flip never reclassifies a live window row", () => {
    // The browse budget may evict cache; a window row must never be evictable.
    // Note the band is the OLDEST `excess` tail keys while the window is the
    // NEWEST `height`, and `keep = max(supportedTarget, height)`, so the two
    // cannot overlap arithmetically — confirmPaging's `k < winFloor` filter is
    // belt-and-braces for a future `keep` that stops dominating the height, and
    // no fixture can make it bite today. This test pins the OUTCOME.
    const s = new LineStore();
    fillTail(s, RESIDENT_TAIL_CAP + 300);
    s.applyScreen(screenMsg(RESIDENT_TAIL_CAP + 300, 24));
    s.applyResumeAck({
      epochChanged: false,
      committed: null,
      serverOldest: null,
      paging: true,
      sentHaveThrough: -1,
      sentReplayMax: null,
      viewportAbs: 5,
      following: false,
    });
    const win = s.getWindow();
    for (let y = 0; y < win.height; y++) {
      expect(s.getLine(win.base + y), `window row ${y} must still be held`).toBeDefined();
    }
  });

  it("a FOLLOWING reader drains the reclassified band to the prefetch floor", () => {
    // Following means the reader is looking at the live tail, so the band is
    // cache nobody is reading: it drains to the small target immediately
    // instead of waiting for a TTL.
    const s = new LineStore();
    fillTail(s, RESIDENT_TAIL_CAP + 900);
    s.applyScreen(screenMsg(RESIDENT_TAIL_CAP + 900, 24));
    const win = s.getWindow();
    s.applyResumeAck({
      epochChanged: false,
      committed: null,
      serverOldest: null,
      paging: true,
      sentHaveThrough: -1,
      sentReplayMax: null,
      viewportAbs: win.base, // following
      following: true,
    });
    // An EXACT value, not an upper bound: `toBeLessThanOrEqual` also passes at 0,
    // which is what the store reports when the flip reclassified nothing at all,
    // so the loose oracle could not tell "drained to the floor" from "never
    // classified anything" — the failure it exists to catch.
    expect(s.browseCacheSize()).toBe(PREFETCH_THRESHOLD);
  });
});

describe("LineStore paging: the replay jump", () => {
  // Every case here holds a tail SMALLER than the resident target, so the cap
  // flip reclassifies nothing and any cache seen afterwards is the jump's work
  // alone. (An earlier draft filled past the cap and passed on the flip's band
  // while claiming to assert the jump — the two have to be isolated.)
  const HELD = 1000;

  it("reclassifies the stranded band when the server's ring moved past haveThrough", () => {
    const s = new LineStore();
    fillTail(s, HELD); // holds [0, 1000)
    s.applyScreen(screenMsg(HELD, 24)); // a live window to retire
    expect(s.getWindow().height).toBe(24);
    // The socket said it had through 999; the server's oldest is now 2000, so
    // the replay begins there and everything held is an island below the hole.
    s.applyResumeAck({
      epochChanged: false,
      committed: 5000,
      serverOldest: 2000,
      paging: true,
      sentHaveThrough: HELD - 1,
      sentReplayMax: null,
      viewportAbs: 100, // parked in the stranded band
      following: false,
    });
    expect(s.browseCacheSize()).toBe(HELD);
    expect(heldKeys(s).length).toBe(HELD + 24); // reclassified, not evicted
    // The jump band RETIRES the window descriptor, because the band includes
    // the old window rows and the incoming batch's own window frame
    // re-establishes it at the new base. While it is retired no window-derived
    // bound may be evaluated, which is why this is asserted and not incidental.
    expect(s.getWindow().height).toBe(0);
  });

  it("predicts from the value this socket SENT, not from a moved `highest`", () => {
    // The design decision this pins: a frame landing between the resume send
    // and the ack moves `highest`, and predicting from it would compute a base
    // ABOVE the replay's real start, hide the jump, and leave the stranded band
    // classified as tail for enforceCap to eat silently.
    const s = new LineStore();
    fillTail(s, HELD); // [0,1000): what the socket reported
    s.applyScroll(pageMsg(HELD, 1200)); // frames that landed after the send
    expect(s.highestIndex()).toBe(2199);

    s.applyResumeAck({
      epochChanged: false,
      committed: 5000,
      serverOldest: 2000, // above sentHaveThrough+1, but BELOW highest+1
      paging: true,
      sentHaveThrough: HELD - 1,
      sentReplayMax: null,
      viewportAbs: 100,
      following: false,
    });
    // Predicted start 2000 > the sent base 1000, so the band [0,1000) is
    // stranded. Predicting from `highest` would have computed base 2200 and
    // declared no jump at all.
    expect(s.browseCacheSize()).toBe(HELD);
  });

  it("predicts the jump from the SENT replayMax, and does not without it", () => {
    // The server clamps the replay start to committed - replayMax. The client
    // must predict from the value it SENT: with a server-side-only clamp the
    // stranded band would stay classified as tail, where enforceCap eats it.
    const withMax = new LineStore();
    fillTail(withMax, HELD);
    withMax.applyResumeAck({
      epochChanged: false,
      committed: 5000,
      serverOldest: 0, // the server retains everything: no eviction gap
      paging: true,
      sentHaveThrough: HELD - 1,
      sentReplayMax: 500, // but this socket asked for at most 500 lines
      viewportAbs: 100,
      following: false,
    });
    expect(withMax.browseCacheSize()).toBe(HELD);

    // The identical ack with no replayMax: the replay resumes at haveThrough+1,
    // so there is no hole and nothing is stranded.
    const without = new LineStore();
    fillTail(without, HELD);
    without.applyResumeAck({
      epochChanged: false,
      committed: 5000,
      serverOldest: 0,
      paging: true,
      sentHaveThrough: HELD - 1,
      sentReplayMax: null,
      viewportAbs: 100,
      following: false,
    });
    expect(without.browseCacheSize()).toBe(0);
  });

  it("runs the budget pass when the JUMP alone reclassified (no flip band)", () => {
    // The two reclassifying steps are independent: a store whose tail is already
    // under the post-flip target moves nothing at the flip, so the jump is the
    // only source of cache. The budget pass is gated on EITHER having moved
    // something — gating it on the flip alone leaves the jump's band unbudgeted,
    // which is the whole population §5.3 names as primary.
    const s = new LineStore();
    // A tail well under RESIDENT_TAIL_CAP: confirmPaging has nothing to move.
    s.applyScroll(pageMsg(0, 900));
    expect(s.browseCacheSize()).toBe(0);
    s.applyResumeAck({
      epochChanged: false,
      committed: 100_000,
      serverOldest: 90_000, // above what this socket holds: a jump
      paging: true,
      sentHaveThrough: 899,
      sentReplayMax: null,
      viewportAbs: 100_000, // following the live tail, so the small target applies
      following: true,
    });
    // 900 rows were reclassified and then drained to the prefetch floor. Without
    // the pass they would all still be here.
    expect(s.browseCacheSize()).toBeLessThanOrEqual(PREFETCH_THRESHOLD);
    expect(s.browseCacheSize()).toBeGreaterThan(0);
  });

  it("no jump when the replay will resume exactly where the client left off", () => {
    const s = new LineStore();
    fillTail(s, HELD);
    s.applyScreen(screenMsg(HELD, 24));
    const base = s.getWindow().base;
    s.applyResumeAck({
      epochChanged: false,
      committed: HELD,
      serverOldest: 0,
      paging: true,
      sentHaveThrough: HELD - 1,
      sentReplayMax: null,
      viewportAbs: 100,
      following: false,
    });
    expect(s.browseCacheSize()).toBe(0);
    // The window descriptor survives: only a real jump retires it.
    expect(s.getWindow().base).toBe(base);
    expect(s.getWindow().height).toBe(24);
  });

  it("an ack with no bounds tail runs the capability read and nothing else", () => {
    // A server too old to carry the bounds tail sends neither value. Inventing
    // zeros would lower the floor and forge a jump.
    const s = new LineStore();
    fillTail(s, 1000);
    s.raisePagingFloor(2000);
    s.applyResumeAck({
      epochChanged: false,
      committed: null,
      serverOldest: null,
      paging: true,
      sentHaveThrough: 999,
      sentReplayMax: null,
      viewportAbs: 100,
      following: false,
    });
    expect(s.tailCap()).toBe(RESIDENT_TAIL_CAP); // capability read ran
    expect(s.pagingFloorIndex()).toBe(2000); // floor untouched
    expect(s.serverOldestIndex()).toBe(-1); // no bounds recorded
    expect(s.browseCacheSize()).toBe(0); // no jump forged
  });

  it("an epoch change resets the cap along with the content", () => {
    const s = new LineStore();
    fillTail(s, 2000);
    s.applyResumeAck({
      epochChanged: false,
      committed: null,
      serverOldest: null,
      paging: true,
      sentHaveThrough: -1,
      sentReplayMax: null,
      viewportAbs: -1,
      following: true,
    });
    expect(s.tailCap()).toBe(RESIDENT_TAIL_CAP);

    s.applyResumeAck({
      epochChanged: true,
      committed: null,
      serverOldest: null,
      paging: false,
      sentHaveThrough: -1,
      sentReplayMax: null,
      viewportAbs: -1,
      following: true,
    });
    // A different server process shares no absolute indices AND no declared
    // capability, so the cap reverts to the compatibility value.
    expect(heldKeys(s).length).toBe(0);
    expect(s.tailCap()).toBe(COMPATIBILITY_TAIL_CAP);
    expect(s.pagingFloorIndex()).toBe(0);
  });
});

describe("LineStore paging: solicited ranges and the browse cache", () => {
  it("stores a solicited page below the watermark that an ordinary scroll would refuse", () => {
    const s = new LineStore(100);
    // Force an eviction so the watermark is above 0.
    s.applyScroll(pageMsg(0, 150));
    expect(s.hasTrimmedHistory()).toBe(true);

    // Unsolicited re-delivery of an evicted range is refused (the guard that
    // stops a late frame from resurrecting trimmed history).
    s.applyScroll(pageMsg(0, 5));
    expect(s.getLine(0)).toBeUndefined();

    // The same range, SOLICITED, is exactly what the reader asked for.
    fetchPage(s, 0, 5, 0);
    expect(s.getLine(0)).toBeDefined();
    expect(s.browseCacheSize()).toBe(5);
  });

  it("clips a reply to the solicited window, routing the rest through ordinary guards", () => {
    // A timed-out larger reply racing a shrunken retry: only the intersection
    // gets solicited treatment, so a stale oversized frame cannot apply content
    // this attempt never asked for.
    const s = new LineStore(100);
    s.applyScroll(pageMsg(0, 150));
    s.noteSolicited(20, 25);
    s.applyHistoryScroll(pageMsg(0, 40), 20);
    s.clearSolicited();
    // Inside the window: stored and classified as cache.
    for (let abs = 20; abs < 25; abs++) {
      expect(s.getLine(abs), `solicited line ${abs}`).toBeDefined();
    }
    expect(s.browseCacheSize()).toBe(5);
    // Outside it: refused by the ordinary below-watermark guard.
    expect(s.getLine(0)).toBeUndefined();
    expect(s.getLine(19)).toBeUndefined();
  });

  it("classifies only the intersection as cache, even when the surplus is storable", () => {
    // The clip is a CLASSIFICATION boundary, not only a storage one. With no
    // watermark to refuse the surplus, an unclipped reply would fold lines this
    // attempt never solicited into the disposable band — where the cache budget
    // may evict them and the live tail loses rows nothing will re-fetch.
    const s = new LineStore();
    fillTail(s, 200); // no eviction, so nothing is refused on staleness
    s.noteSolicited(100, 110);
    s.applyHistoryScroll(pageMsg(90, 30), 100); // spans 90..119
    s.clearSolicited();
    expect(s.browseCacheSize()).toBe(10);
    // Every line is present; they differ only in classification.
    for (let abs = 90; abs < 120; abs++) {
      expect(s.getLine(abs), `line ${abs}`).toBeDefined();
    }
  });

  it("counts DISTINCT cached lines, so overlapping refetches cannot over-evict", () => {
    const s = new LineStore();
    fillTail(s, 200);
    fetchPage(s, 500, 100, 500);
    expect(s.browseCacheSize()).toBe(100);
    // Overlapping refetch: the count must stay the number of distinct cached
    // lines, not accumulate the replies' line counts.
    fetchPage(s, 550, 100, 550);
    expect(s.browseCacheSize()).toBe(150);
    fetchPage(s, 500, 100, 500);
    expect(s.browseCacheSize()).toBe(150);
  });

  it("counts cache in RETAINED lines when a reclassified band spans holes", () => {
    // A repeated jump ack over a band that already spans holes. Cache size must
    // equal the RETAINED rows in it, never the width of the range: the band
    // covers [oldest, haveThrough] whatever holes that range has, and a size
    // derived from the range makes the store report a live tail it does not
    // have — the tail budget then evicts against a number nothing holds.
    //
    // Deterministic because the generated residency property only produces this
    // shape (a SECOND jump ack over an already-hole-spanning band) in a minority
    // of runs, which is not a regression guard.
    const s = new LineStore();
    s.applyScroll(pageMsg(0, 1)); // a sparse tail: two lines, 99 holes between
    s.applyScroll(pageMsg(100, 1));

    const jumpAck = (sentHaveThrough: number): void => {
      s.applyResumeAck({
        epochChanged: false,
        committed: sentHaveThrough + 200,
        serverOldest: sentHaveThrough + 100, // above the sent base: a jump
        paging: true,
        sentHaveThrough,
        sentReplayMax: null,
        viewportAbs: 0,
        following: false,
      });
    };

    jumpAck(100); // reclassifies [0,101): 2 retained lines across 99 holes
    expect(s.browseCacheSize()).toBe(2);

    s.applyScroll(pageMsg(200, 1)); // the batch's tail lands above the hole
    jumpAck(200); // reclassifies [0,201) — recounted against the band above

    expect(s.browseCacheSize()).toBe(3);
    // The count is the whole point: a wrong one makes the store report a live
    // tail it does not have, and the tail budget then evicts against it.
    let retained = 0;
    s.forEachLine(() => {
      retained++;
    });
    expect(retained).toBe(3);
    expect(s.snapshot(1)).toBeNull(); // every line is cache, so nothing persists
  });

  it("counts rows fetched INTO a band that already covers their indices", () => {
    // A page landing in a HOLE inside an already-classified band. The
    // range-based model computed its delta by re-measuring the band after the
    // rows were stored, so the new rows were already inside the old measurement
    // and the delta came out zero: 3048 rows held, 1524 counted. The
    // under-report inflates `tailCount` by the difference, and the tail budget
    // then evicts live rows to pay for cache it does not believe it has.
    const s = new LineStore();
    s.applyScroll(pageMsg(0, 50));
    s.applyScroll(pageMsg(40_000_000, 50));
    s.applyResumeAck({
      epochChanged: false,
      committed: 60_000_000,
      serverOldest: 50_000_000,
      paging: true,
      sentHaveThrough: 40_000_049,
      sentReplayMax: null,
      viewportAbs: 0,
      following: false,
    });
    expect(s.browseCacheSize()).toBe(100);

    // Ten rows at indices the band already spans but the store never held.
    fetchPage(s, 20_000_000, 10, 0);

    expect(s.browseCacheSize()).toBe(110);
    expect(heldKeys(s).length).toBe(110);
  });

  it("holds the cache to its budget, evicting the edge furthest from the reader", () => {
    const s = new LineStore();
    fillTail(s, 100);
    // Fetch well past the budget in pages, with the reader parked at the TOP of
    // the fetched region, so the far edge is the high end.
    for (let base = 0; base < BROWSE_CACHE_CAP + 1000; base += 500) {
      fetchPage(s, 10000 + base, 500, 10000);
    }
    expect(s.browseCacheSize()).toBeLessThanOrEqual(BROWSE_CACHE_CAP);
    // The reader's own neighbourhood survived.
    expect(s.getLine(10000)).toBeDefined();
  });

  it("evicts the end FARTHER from the reader, whichever end that is", () => {
    // The regression guard for a real defect: the old code asked
    // `iv.lo >= viewportAbs` to find an interval's far edge, which is false
    // whenever the reader sits INSIDE the interval — the normal deep-scroll
    // shape, since the cache grows as one contiguous run and the reader scrolls
    // up into it. So it always took victims from the LOW end: the direction an
    // up-scrolling reader is heading, re-fetching what it had just dropped,
    // while the pages behind the reader were never freed at all.
    //
    // This test's earlier version asserted the resulting overshoot as if it were
    // the intended "never evict under the reader" behavior, which is how the bug
    // survived a red-check: its fixture parked the reader exactly at `iv.lo`, the
    // one position a real reader never occupies.
    const s = new LineStore();
    fillTail(s, 100);
    const readerAt = 10_100; // near the LOW edge of what we are about to cache
    for (let base = 0; base < BROWSE_CACHE_CAP + 1000; base += 500) {
      fetchPage(s, 10_000 + base, 500, readerAt);
    }

    // The budget is a budget again: the far end went, so the cache is bounded.
    expect(s.browseCacheSize()).toBeLessThanOrEqual(BROWSE_CACHE_CAP);
    // The reader's own neighbourhood survived, which is the other half of the
    // rule — the exemption spans PREFETCH_THRESHOLD either side.
    expect(s.getLine(readerAt)).toBeDefined();
    // Still inside both the cache and the exemption band ([reader-500, reader+501)).
    expect(s.getLine(readerAt + PREFETCH_THRESHOLD - 100)).toBeDefined();
    expect(s.getLine(10_000)).toBeDefined(); // the cache's low edge, also exempt
    // And what went is the high end, farthest from a reader near the low edge.
    expect(s.getLine(10_000 + BROWSE_CACHE_CAP + 999)).toBeUndefined();
  });

  it("evicts the low end when the reader is near the top of the cache", () => {
    // The mirror case, so the choice is pinned as a COMPARISON rather than a
    // fixed direction: same shape, reader parked near the high edge.
    const s = new LineStore();
    fillTail(s, 100);
    const top = 10_000 + BROWSE_CACHE_CAP + 999;
    for (let base = 0; base < BROWSE_CACHE_CAP + 1000; base += 500) {
      fetchPage(s, 10_000 + base, 500, top);
    }
    expect(s.browseCacheSize()).toBeLessThanOrEqual(BROWSE_CACHE_CAP);
    expect(s.getLine(top)).toBeDefined();
    expect(s.getLine(10_000)).toBeUndefined(); // the far (low) end went
  });

  it("keeps a cache that SPANS the reader rather than blanking rows under it", () => {
    // The viewport exemption, at the one target that reaches it. The resume pass
    // drains to PREFETCH_THRESHOLD, and the exempt band is 2 * PREFETCH_THRESHOLD
    // + 1 wide, so a cache straddling the reader but narrower than the band is
    // ENTIRELY exempt while still over target. Nothing is evictable and the pass
    // stops: a bounded overshoot is the designed trade against blanking the rows
    // the reader is looking at.
    const s = new LineStore();
    s.applyScroll(pageMsg(0, 400));
    s.applyScroll(pageMsg(600, 400)); // the reader's position, 500, is the hole
    s.applyResumeAck({
      epochChanged: false,
      committed: 5000,
      serverOldest: 4000,
      paging: true,
      sentHaveThrough: 999,
      sentReplayMax: null,
      viewportAbs: 500,
      following: false,
    });
    expect(s.browseCacheSize()).toBe(800);
    expect(s.browseCacheSize()).toBeGreaterThan(PREFETCH_THRESHOLD);
    // Both edges of the reader's neighbourhood survived, which is the point.
    expect(s.getLine(399)).toBeDefined();
    expect(s.getLine(600)).toBeDefined();
  });

  it("a page landing during alt is stored below the frozen base and refused at or above it", () => {
    const s = new LineStore();
    fillTail(s, 200);
    s.applyScreen(screenMsg(200, 10));
    s.applyScreen(screenMsg(200, 10, { altActive: true }));
    fetchPage(s, 190, 20, 190); // spans 190..209, the frozen base is 200
    expect(s.getLine(195)).toBeDefined();
    // At or above the frozen main base would corrupt the window region.
    expect(s.browseCacheSize()).toBeLessThanOrEqual(10);
  });
});

describe("LineStore paging: retained-range geometry is memoized", () => {
  // The gap geometry is asked for far more often than it changes — once per
  // scroll event by the fetch trigger, once per flush by the gap markers — and
  // computing it copies and sorts the whole residency (95 us at 4500 rows, so
  // 5.7 ms per 60-event scroll frame). It is computed once per key-set change
  // instead. A STALE memo is the hazard: it either hides a hole the renderer
  // must mark, or invents one the trigger then tries to fetch forever.
  const spans = (s: LineStore): [number, number][] =>
    s.retainedRanges().map((iv) => [iv.lo, iv.hi]);

  it("follows an INSERT, a DELETE, a reset and a hydrate", () => {
    const s = new LineStore(60);
    s.applyScroll(pageMsg(0, 50));
    expect(spans(s)).toEqual([[0, 50]]);

    // INSERT (applyLine): a line beyond the tail opens a hole behind it.
    s.applyScroll(pageMsg(100, 1));
    expect(spans(s)).toEqual([
      [0, 50],
      [100, 101],
    ]);

    // DELETE (forget) with NO insert behind it, which is the only shape that
    // isolates this invalidation: an ED3 frame applies its window row afterwards
    // and the insert's invalidation would mask a missing one here.
    s.noteSolicited(200, 210);
    s.applyHistoryScroll(pageMsg(200, 10), 205);
    s.clearSolicited();
    expect(spans(s)).toEqual([
      [0, 50],
      [100, 101],
      [200, 210],
    ]);
    s.dropBrowseCache(-1, false); // deletes the fetched page and inserts nothing
    expect(spans(s)).toEqual([
      [0, 50],
      [100, 101],
    ]);

    // DELETE through the ED3 path too, which does apply a row behind it.
    s.applyScreen({ ...screenMsg(100, 1), scrollbackCleared: true });
    expect(spans(s)).toEqual([[100, 101]]);

    // RESET.
    s.reset();
    expect(spans(s)).toEqual([]);

    // HYDRATE, which writes keys directly rather than through applyLine.
    const src = new LineStore(60);
    src.applyScroll(pageMsg(700, 20));
    const snap = src.snapshot(1);
    expect(snap).not.toBeNull();
    const hydrated = LineStore.fromSnapshot(snap, 60);
    expect(hydrated).not.toBeNull();
    expect(spans(hydrated!)).toEqual([[700, 720]]);
  });

  it("hands out a copy, so a caller cannot corrupt later answers", () => {
    const s = new LineStore();
    s.applyScroll(pageMsg(0, 10));
    const first = s.retainedRanges();
    const iv = first[0];
    expect(iv).toBeDefined();
    iv!.lo = 999; // a caller mutating what it was given
    first.push({ lo: -5, hi: -1 });
    expect(spans(s)).toEqual([[0, 10]]);
  });
});

describe("LineStore paging: the paging floor", () => {
  it("rises monotonically and only on a safe integer", () => {
    const s = new LineStore();
    expect(s.pagingFloorIndex()).toBe(0);
    s.raisePagingFloor(500);
    expect(s.pagingFloorIndex()).toBe(500);
    s.raisePagingFloor(200); // a stale reply cannot lower it
    expect(s.pagingFloorIndex()).toBe(500);
    s.raisePagingFloor(Number.NaN);
    s.raisePagingFloor(Number.POSITIVE_INFINITY);
    expect(s.pagingFloorIndex()).toBe(500);
  });

  it("a server that reports retaining older history reopens the floor", () => {
    // The server is the authority on what survives: if its reported oldest is
    // below the floor, the floor was raised against a ring that has since been
    // re-established (or the client's floor came from a different socket).
    const s = new LineStore();
    s.raisePagingFloor(900);
    s.noteResumeBounds(5000, 100);
    expect(s.pagingFloorIndex()).toBe(100);
    expect(s.serverOldestIndex()).toBe(100);
  });

  it("stops offering a frontier the floor has closed", () => {
    const s = new LineStore();
    s.applyScroll(pageMsg(1000, 50)); // holds [1000,1050)
    s.noteResumeBounds(1050, 500); // the server has older history
    expect(s.absentEdgesNear(1000, 600).length).toBeGreaterThan(0);
    // The server proved nothing at or below 1000 survives.
    s.raisePagingFloor(1000);
    expect(s.absentEdgesNear(1000, 600)).toEqual([]);
  });

  it("offers an interior hole even when the frontier is closed", () => {
    const s = new LineStore();
    s.applyScroll(pageMsg(0, 20));
    s.applyScroll(pageMsg(100, 20)); // hole at [20,100)
    s.raisePagingFloor(0);
    const edges = s.absentEdgesNear(95, 50);
    expect(edges.map((g) => [g.lo, g.hi])).toEqual([[20, 100]]);
  });

  it("drops an interior hole the floor has swallowed", () => {
    // The floor means the server PROVED nothing at or below it survives, so a
    // hole whose high edge is at the floor is unfetchable: offering it would
    // spend a request per approach on a range that can only come back empty.
    const s = new LineStore();
    s.applyScroll(pageMsg(0, 10));
    s.applyScroll(pageMsg(100, 10)); // hole at [10,100)
    expect(s.absentEdgesNear(50, 100).length).toBe(1);
    s.raisePagingFloor(100);
    expect(s.absentEdgesNear(50, 100)).toEqual([]);
  });

  it("orders candidate edges by nearness to the reader", () => {
    const s = new LineStore();
    s.applyScroll(pageMsg(0, 10)); // [0,10)
    s.applyScroll(pageMsg(50, 10)); // [50,60): hole [10,50)
    s.applyScroll(pageMsg(200, 10)); // [200,210): hole [60,200)
    const edges = s.absentEdgesNear(70, 200);
    expect(edges.length).toBe(2);
    expect(edges[0]?.hi).toBe(50); // nearer high edge first
  });
});

describe("LineStore paging: the two exclusions", () => {
  it("snapshot persists the live tail and excludes cache flush against it", () => {
    // A fetched page can sit with NO numeric gap against the tail, so a
    // contiguity test alone would serialize it. The cache is disposable by
    // construction (recovery is one fetch), and persisting it would also
    // hydrate interior holes into a fresh store.
    const s = new LineStore();
    fillTail(s, 100); // tail [0,100)
    s.applyScreen(screenMsg(100, 5));
    fetchPage(s, 0, 40, 0); // reclassifies [0,40) as cache: FLUSH against the tail

    const snap = s.snapshot(7);
    expect(snap).not.toBeNull();
    expect(snap!.oldest).toBe(40);
    expect(snap!.lines.every(([abs]) => abs >= 40)).toBe(true);
    // And it is one contiguous run, which is what keeps a hydrated store's
    // derived watermark honest.
    const absList = snap!.lines.map(([abs]) => abs);
    expect(absList).toEqual(Array.from({ length: absList.length }, (_, i) => absList[0]! + i));
  });

  it("snapshot returns null when everything held is cache", () => {
    const s = new LineStore();
    s.noteSolicited(0, 50);
    s.applyHistoryScroll(pageMsg(0, 50), 0);
    s.clearSolicited();
    expect(s.browseCacheSize()).toBe(50);
    expect(s.snapshot(7)).toBeNull();
  });

  it("evicting cache does not advance the eviction watermark", () => {
    // The watermark drives the trim marker ("earlier output trimmed"). Cache is
    // refetchable, so dropping it is not a trim and must not claim to be one.
    // Based at 1000 so the watermark branch is REACHABLE: with oldest > 0, one
    // advanced watermark flips hasTrimmedHistory to true.
    const s = new LineStore();
    s.applyScroll(pageMsg(1000, 50));
    expect(s.hasTrimmedHistory()).toBe(false);

    // Overfill the cache so the budget pass has to evict some of it.
    for (let base = 0; base < BROWSE_CACHE_CAP + 1100; base += 500) {
      fetchPage(s, 5000 + base, 500, 5000);
    }
    expect(s.browseCacheSize()).toBeLessThanOrEqual(BROWSE_CACHE_CAP);
    expect(s.hasTrimmedHistory()).toBe(false);

    // And the wholesale TTL drop is not a trim either.
    s.dropBrowseCache(5000, false);
    expect(s.browseCacheSize()).toBe(0);
    expect(s.hasTrimmedHistory()).toBe(false);
  });
});

describe("LineStore paging: dropBrowseCache", () => {
  it("drops the cache and restores the bounds to the live tail", () => {
    const s = new LineStore();
    fillTail(s, 50); // [0,50)
    fetchPage(s, 500, 100, 500);
    expect(s.oldestIndex()).toBe(0);
    expect(s.highestIndex()).toBe(599);
    s.dropBrowseCache(0, false);
    expect(s.browseCacheSize()).toBe(0);
    expect(heldKeys(s)).toEqual(Array.from({ length: 50 }, (_, i) => i));
    expect(s.highestIndex()).toBe(49);
  });

  it("skips a visible page whose reader is parked ON cache", () => {
    // The TTL is an inactivity signal, and a reader staring at a long stack
    // trace is inactive while looking straight at cache content.
    const s = new LineStore();
    fillTail(s, 50);
    fetchPage(s, 500, 100, 500);
    s.dropBrowseCache(550, true);
    expect(s.browseCacheSize()).toBe(100);
  });

  it("drops a HIDDEN page's cache even under the reader's position", () => {
    // Without this condition the skip would retain exactly the deep-scrolled
    // cache the hidden-page TTL exists to free.
    const s = new LineStore();
    fillTail(s, 50);
    fetchPage(s, 500, 100, 500);
    s.dropBrowseCache(550, false);
    expect(s.browseCacheSize()).toBe(0);
  });

  it("is a no-op with no cache at all", () => {
    const s = new LineStore();
    fillTail(s, 50);
    s.dropBrowseCache(10, true);
    expect(heldKeys(s).length).toBe(50);
  });
});


describe("LineStore paging: classification survives every removal path", () => {
  // Classification lives in a second container keyed by the same indices
  // (`browse`), so it is only as correct as its least-maintained deletion path.
  // Two paths did not maintain it and both were found by review rather than by
  // these tests: ED3's applyScrollbackCleared, and truncateBelowWindow — which
  // fires on every soft-keyboard resize, the most common event on the device
  // this design exists for. Every removal now goes through one private
  // forget(); these cases pin the OUTCOME at each entry point so the next path
  // added cannot quietly skip it.
  //
  // The observable is `tailCount` — not exported, but derived: the tail cap is
  // enforced against `lines.size - browse.size`, so cache entries outliving
  // their rows understate the tail and the cap stops trimming. A store holding
  // far more than its cap after one of these operations IS the corruption.

  /** Every retained index, and how many of them the store calls cache. */
  function census(s: LineStore): { total: number; browse: number; tail: number } {
    let total = 0;
    s.forEachLine(() => {
      total++;
    });
    const browse = s.browseCacheSize();
    return { total, browse, tail: total - browse };
  }

  it("keeps the count honest when ED3 clears history under the cache", () => {
    const s = new LineStore(1000);
    s.applyScroll(pageMsg(0, 500));
    s.applyScreen(screenMsg(500, 10));
    fetchPage(s, 100, 200, 100); // 200 of the retained lines become cache
    expect(s.browseCacheSize()).toBe(200);

    // ED3: the application cleared its scrollback, so everything below the
    // window goes — cache included.
    s.applyScreen({ ...screenMsg(500, 10), scrollbackCleared: true });

    const after = census(s);
    expect(after.browse).toBe(0);
    expect(after.tail).toBe(after.total);
    expect(after.tail).toBeGreaterThanOrEqual(0);
    // And the tail cap still bites afterwards, which an over-counted cache
    // would have prevented.
    s.applyScroll(pageMsg(1000, 2000));
    expect(census(s).total).toBeLessThanOrEqual(Math.max(1000, s.getWindow().height));
  });

  it("keeps the count honest when a resize truncates below the window", () => {
    // The soft-keyboard path: the screen shrinks, so rows stranded above the new
    // window bottom are dropped. If those rows were cache, the count must follow.
    const s = new LineStore(1000);
    s.applyScroll(pageMsg(0, 300));
    s.applyScreen(screenMsg(300, 40)); // tall window: rows 300..339
    fetchPage(s, 320, 20, 320); // classify rows INSIDE the window region as cache
    const before = s.browseCacheSize();
    expect(before).toBeGreaterThan(0);

    s.applyScreen(screenMsg(300, 10)); // keyboard opens: window shrinks to 300..309

    const after = census(s);
    expect(after.tail).toBe(after.total - after.browse);
    expect(after.tail).toBeGreaterThanOrEqual(0);
    // Nothing above the new window bottom survived, cache or not.
    expect(s.getLine(330)).toBeUndefined();
    // The cap still enforces.
    s.applyScroll(pageMsg(2000, 2000));
    expect(census(s).total).toBeLessThanOrEqual(Math.max(1000, s.getWindow().height));
  });

  it("keeps the count honest when the cap evicts and when the TTL drops", () => {
    const s = new LineStore(1000);
    s.applyScroll(pageMsg(0, 900));
    fetchPage(s, 5000, 500, 5000);
    expect(s.browseCacheSize()).toBe(500);

    // Cap eviction (tail victims only) must not touch the cache count.
    s.applyScroll(pageMsg(900, 400));
    expect(s.browseCacheSize()).toBe(500);
    expect(census(s).tail).toBeGreaterThanOrEqual(0);

    s.dropBrowseCache(0, false);
    const after = census(s);
    expect(after.browse).toBe(0);
    expect(after.tail).toBe(after.total);
  });

  it("drops a cache spanning a vast hole without walking the hole", () => {
    // The reclassified band reaches from `oldest` through the client's
    // haveThrough, so a client reconnecting far behind classifies rows either
    // side of every index it is missing. Cache work must cost the ROWS, never
    // that width: the range-based model this replaced walked the width, which is
    // millions of Map lookups for a bounded number of rows. The ceiling below is
    // what makes a reintroduction of that shape fail here.
    const s = new LineStore();
    s.applyScroll(pageMsg(0, 50));
    s.applyScroll(pageMsg(40_000_000, 50)); // a 40M-index hole between the two runs
    s.applyResumeAck({
      epochChanged: false,
      committed: 60_000_000,
      serverOldest: 50_000_000, // above what this socket holds: a jump
      paging: true,
      sentHaveThrough: 40_000_049,
      sentReplayMax: null,
      viewportAbs: 0,
      following: false,
    });
    expect(s.browseCacheSize()).toBe(100); // 100 retained lines across a 40M hull

    const started = Date.now();
    s.dropBrowseCache(0, false);
    const elapsed = Date.now() - started;

    expect(s.browseCacheSize()).toBe(0);
    // Generous by three orders of magnitude against the hull walk, which is
    // ~40M Map lookups: the point is the distinction, not the exact budget.
    expect(elapsed).toBeLessThan(1000);
  });
});


describe("LineStore paging: the budget walk terminates over a hole-spanning band", () => {
  // Termination of the budget walk, over the shape that broke it: a cache whose
  // low edge sits above a wide hole. A range-based victim pool picks an EDGE, and
  // an edge that is a hole holds no line, so removing it changes nothing and the
  // walk re-picks it forever while the count never moves — an infinite loop, on
  // the main thread, reachable from an ordinary reconnect. The key-set model
  // cannot express that (every victim is a held row and every removal shrinks the
  // set), so this pins the contract against a range-based reintroduction.
  //
  // NOTE the failure mode this pins is a HANG, not a wrong value, so a
  // regression here shows up as a timed-out test rather than an assertion. That
  // is the honest shape for it; a synchronous loop cannot be interrupted by the
  // test runner, which is also why it would freeze a browser tab.
  it("completes a budget pass when the band's edge is all holes", () => {
    const s = new LineStore();
    // Two small runs separated by a wide hole, then a jump ack that reclassifies
    // the whole hull. The low edge of that band is a hole, which is the shape
    // that stalls a walk whose progress depends on deleting a real line.
    s.applyScroll(pageMsg(0, 5));
    s.applyScroll(pageMsg(500_000, 5));
    s.applyResumeAck({
      epochChanged: false,
      committed: 900_000,
      serverOldest: 800_000, // above what this socket holds: a jump
      paging: true,
      sentHaveThrough: 500_004,
      sentReplayMax: null,
      viewportAbs: 500_000,
      following: false,
    });
    expect(s.browseCacheSize()).toBe(10);

    // Force a pass that must evict: fetch far away with the reader parked at the
    // new page, so the old band is the far interval and its hole-edge is walked.
    for (let base = 0; base < 4000; base += 500) {
      fetchPage(s, 900_000 + base, 500, 900_000 + base);
    }
    // Reaching this line at all is the assertion; the value is a sanity check.
    expect(s.browseCacheSize()).toBeGreaterThan(0);
    expect(s.browseCacheSize()).toBeLessThanOrEqual(BROWSE_CACHE_CAP + PAGE_SIZE);
  });
});


describe("LineStore paging: the ED3 (scrollbackCleared) lifecycle", () => {
  /** A screen frame carrying ED3, as the wire decodes it. */
  function ed3(base: number, height: number): ScreenMessage {
    return { ...screenMsg(base, height), scrollbackCleared: true };
  }

  it("cancels the in-flight request window, so the reply cannot resurrect the erased rows", () => {
    // The solicited window is the ONE exemption from the stale-re-send guard, so
    // it is the one path by which erased history can come back. A request in
    // flight when the app erases its scrollback must stop being permission.
    const s = new LineStore();
    fillTail(s, 300);
    s.noteSolicited(50, 150); // a page request covering rows about to be erased

    s.applyScreen(ed3(280, 20));
    expect(heldKeys(s).filter((k) => k < 280)).toEqual([]);

    // The reply arrives after the erase. Without the cancel it applies through
    // the solicited exemption and the erased rows are back on screen.
    s.applyHistoryScroll(pageMsg(50, 100), 290);
    expect(heldKeys(s).filter((k) => k < 280)).toEqual([]);
  });

  it("snaps the paging floor to the cleared bound, so the trigger stops asking", () => {
    const s = new LineStore();
    fillTail(s, 300);
    s.noteResumeBounds(300, 0); // the server still retains from 0
    expect(s.pagingFloorIndex()).toBe(0);
    expect(s.absentEdgesNear(280, PREFETCH_THRESHOLD).length).toBe(0); // nothing absent yet

    s.applyScreen(ed3(280, 20));

    expect(s.pagingFloorIndex()).toBe(280);
    // The frontier below the window is now closed: the app discarded that range,
    // so re-fetching it would put erased output back in front of the user.
    expect(s.absentEdgesNear(280, PREFETCH_THRESHOLD)).toEqual([]);
  });

  it("runs the lifecycle even when no line is held below the cleared bound", () => {
    // The early return covers the LINE DROP only. An in-flight request and a
    // stale floor are not conditional on this client's residency.
    const s = new LineStore();
    s.applyScroll(pageMsg(500, 20));
    s.noteSolicited(100, 200);

    s.applyScreen(ed3(500, 20)); // holds nothing below 500

    expect(s.pagingFloorIndex()).toBe(500);
    s.applyHistoryScroll(pageMsg(100, 100), 505);
    expect(heldKeys(s).filter((k) => k < 500)).toEqual([]);
  });

  it("runs the whole lifecycle on an ALT-active frame", () => {
    // The server raises the ED3 flag in the PTY path and attaches it to whatever
    // frame it builds next, with no alt gate — it even force-appends a row so the
    // flag cannot be dropped, which on an alt tick forces an ALT payload carrying
    // it. `clear; vim foo` as one line is enough: both land in one flush interval.
    //
    // Handling ED3 inside the main-screen branch made every alt-active ED3 a total
    // no-op, and the erased history then spliced CONTIGUOUSLY into later output —
    // the ring's Clear() preserves `committed`, so there is no index gap, no gap
    // marker, and no trim marker to show for it.
    const s = new LineStore();
    fillTail(s, 300);
    s.applyScreen(screenMsg(300, 20));
    s.noteSolicited(50, 150);
    s.applyScreen({ ...screenMsg(300, 20, { altActive: true }), scrollbackCleared: true });

    expect(heldKeys(s).filter((k) => k < 300)).toEqual([]);
    expect(s.pagingFloorIndex()).toBe(300);
    // And the in-flight reply cannot bring them back.
    s.applyHistoryScroll(pageMsg(50, 100), 310);
    expect(heldKeys(s).filter((k) => k < 300)).toEqual([]);
  });

  it("a ROWS-LESS alt frame does not collapse the alt grid", () => {
    // The pre-ack forward strips rows on either buffer. On the alt path a
    // zero-row frame reached enterAltIfNeeded(0) and emptied the grid until the
    // next full frame repainted it.
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 24, { altActive: true }));
    expect(s.getAltRows().length).toBe(24);

    s.applyScreen({ ...screenMsg(0, 24, { altActive: true }), changed: [], rows: [] });

    expect(s.getAltRows().length).toBe(24);
  });

  it("refuses an erased index from an UNCORRELATED frame, but not the app's reprint", () => {
    // A page request whose data timeout already released single-flight comes back
    // uncorrelated, so it arrives through the ordinary scroll path — classified
    // TAIL, outside the cache budget and the TTL both, so it never leaves. ED3
    // deliberately does not advance the trim watermark (the marker would be
    // permanent noise on an app that clears every redraw), so a SECOND watermark
    // records what the application erased.
    //
    // It tracks what was DROPPED, not the cleared bound, and the second half of
    // this test is why: an app reprinting its transcript after the clear commits
    // at NEW indices that are still BELOW the new window base, and that content
    // must apply. Snapping the refusal to the bound instead refuses it.
    const s = new LineStore();
    fillTail(s, 400); // holds [0, 400)
    s.applyScreen(screenMsg(700, 4)); // window far above: base 700
    s.applyScreen({ ...screenMsg(700, 4), scrollbackCleared: true });
    expect(heldKeys(s).filter((k) => k < 700)).toEqual([]);

    // The erased rows, re-delivered as ordinary (uncorrelated) history.
    s.applyScroll(pageMsg(100, 50));
    expect(heldKeys(s).filter((k) => k < 400)).toEqual([]);

    // The app's own reprint: new commits, above everything the client held before
    // the clear, still below the window base.
    s.applyScroll(pageMsg(500, 200));
    expect(heldKeys(s).filter((k) => k >= 500 && k < 700).length).toBe(200);
  });

  it("a ROWS-LESS ED3 frame clears history without wiping the live rows", () => {
    // The connection layer's pre-ack suppression forwards ED3 as a rows-less
    // screen frame ({...msg, changed: [], rows: []}), because the resume batch
    // repaints the screen. Taking the window from it sets height 0, which puts
    // the window bottom BELOW its own base and makes every retained row look
    // stranded above the window: measured wiping a 3024-line store to 0, with no
    // trim marker, on a busy session's first frame.
    const s = new LineStore();
    fillTail(s, 3000);
    s.applyScreen(screenMsg(3000, 24)); // a real window on top of the history
    const before = heldKeys(s).length;
    expect(before).toBeGreaterThan(3000);

    const signal: ScreenMessage = { ...ed3(3000, 24), changed: [], rows: [] };
    s.applyScreen(signal);

    // History below the window is gone (the ED3 did its job) and the live window
    // rows are untouched (the frame carried no geometry to redefine them with).
    expect(heldKeys(s).filter((k) => k < 3000)).toEqual([]);
    expect(heldKeys(s).length).toBe(24);
    expect(s.getWindow().height).toBe(24);
    expect(s.highestIndex()).toBe(3023);
  });
});
