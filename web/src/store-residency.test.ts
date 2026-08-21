// Residency accounting under the two operations that move lines between the
// resident tail and the disposable browse cache without deleting anything — the
// resume-ack transition and the cap-eviction walk — plus the bounds the store
// reports afterwards.
//
// `oldestIndex()` and `highestIndex()` are not conveniences: the frontier
// geometry reads the first and resume sends the second as `haveThrough`. A walk
// that leaves either stale asks the server for the wrong range, and a budget
// pass that runs when nothing was reclassified drains the depth the reader is
// looking at.
//
// Pure data structure, no DOM.

import { describe, it, expect } from "vitest";
import { BROWSE_CACHE_CAP, LineStore, RESIDENT_TAIL_CAP } from "./store.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
}

function scrollMsg(firstIndex: number, count: number): ScrollMessage {
  const lines: WireRun[][] = [];
  for (let i = 0; i < count; i++) {
    lines.push(row(`L${firstIndex + i}`));
  }
  return { type: "scroll", firstIndex, lines };
}

function screenMsg(base: number, height: number, changed?: number[]): ScreenMessage {
  const rows: WireRun[][] = [];
  for (let y = 0; y < height; y++) {
    rows.push(row(`W${base + y}`));
  }
  return {
    type: "screen",
    base,
    rows,
    changed: changed ?? rows.map((_, y) => y),
    cursor: [0, 0],
    altActive: false,
    scrollbackCleared: false,
  };
}

/** Every retained absolute index, ascending. */
function heldKeys(s: LineStore): number[] {
  const out: number[] = [];
  s.forEachLine((abs) => out.push(abs));
  return out.sort((a, b) => a - b);
}

/** Deliver a solicited page the way the connection layer does. */
function fetchPage(s: LineStore, fromAbs: number, count: number, viewportAbs: number): void {
  s.noteSolicited(fromAbs, fromAbs + count);
  s.applyHistoryScroll(scrollMsg(fromAbs, count), viewportAbs);
  s.clearSolicited();
}

/** A resume ack with the bounds tail present; callers override what they test. */
function ack(over: {
  committed: number;
  serverOldest: number;
  sentHaveThrough: number;
  sentReplayMax?: number | null;
  viewportAbs?: number;
  following?: boolean;
}): Parameters<LineStore["applyResumeAck"]>[0] {
  return {
    epochChanged: false,
    committed: over.committed,
    serverOldest: over.serverOldest,
    paging: true,
    sentHaveThrough: over.sentHaveThrough,
    sentReplayMax: over.sentReplayMax ?? null,
    viewportAbs: over.viewportAbs ?? -1,
    following: over.following ?? false,
  };
}

describe("LineStore.applyResumeAck: a transition that reclassifies nothing", () => {
  it("leaves the reader's cache at full depth", () => {
    // Step 5's budget pass is gated on something having MOVED into the cache.
    // An ack that flips no band and predicts no jump has moved nothing, so a
    // following reader's ack must not drain a cache built by earlier fetches —
    // the small containment target is for bands this ack created.
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 1000));
    fetchPage(s, 0, 600, 0);
    expect(s.browseCacheSize()).toBe(600);

    s.applyResumeAck(
      ack({
        committed: 1000,
        serverOldest: 0,
        sentHaveThrough: 999,
        viewportAbs: 999,
        following: true,
      }),
    );

    expect(s.browseCacheSize()).toBe(600);
    expect(s.tailCap()).toBe(RESIDENT_TAIL_CAP);
  });

  it("leaves the cache alone when the ack declares no paging at all", () => {
    // An unsupported pairing skips the flip and the jump both, so nothing was
    // reclassified and step 5 has nothing to contain. The cache a previous
    // pairing built is the TTL's to drop, not this ack's.
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 1000));
    fetchPage(s, 0, 600, 0);

    s.applyResumeAck({
      epochChanged: false,
      committed: 1000,
      serverOldest: 0,
      paging: false,
      sentHaveThrough: 999,
      sentReplayMax: null,
      viewportAbs: 999,
      following: true,
    });

    expect(s.browseCacheSize()).toBe(600);
    expect(s.pagingDeclared()).toBe(false);
  });
});

describe("LineStore.applyResumeAck: predicting the replay jump", () => {
  it("predicts no jump when the ring resumes exactly at the client's next index", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(10, 10));

    s.applyResumeAck(ack({ committed: 100, serverOldest: 20, sentHaveThrough: 19 }));

    expect(s.browseCacheSize()).toBe(0);
  });

  it("predicts no jump when the sent replayMax reaches back past the watermark", () => {
    // committed - replayMax lands at 0, below what the client already has, so a
    // bounded replay changes nothing about where the batch starts.
    const s = new LineStore();
    s.applyScroll(scrollMsg(10, 10));

    s.applyResumeAck(
      ack({ committed: 100, serverOldest: 5, sentHaveThrough: 19, sentReplayMax: 100 }),
    );

    expect(s.browseCacheSize()).toBe(0);
  });

  it("strands the client's own oldest line when the watermark sits exactly on it", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(10, 10));

    s.applyResumeAck(ack({ committed: 100, serverOldest: 50, sentHaveThrough: 10 }));

    expect(s.browseCacheSize()).toBe(1);
  });

  it("keeps the window descriptor when the store holds no line to strand", () => {
    // A cursor-only frame gives the store a window and no content. The jump
    // predicate fires vacuously there, and retiring the descriptor would drop
    // the live screen's eviction protection for a screen still on display.
    const s = new LineStore();
    s.applyScreen(screenMsg(100, 3, []));

    s.applyResumeAck(ack({ committed: 200, serverOldest: 150, sentHaveThrough: 99 }));

    const win = s.getWindow();
    expect(win.base).toBe(100);
    expect(win.height).toBe(3);
  });

  it("retires the window on a real jump and tells the renderer it went", () => {
    const s = new LineStore();
    s.applyScreen(screenMsg(100, 3));
    s.drainChanges();

    s.applyResumeAck(ack({ committed: 300, serverOldest: 200, sentHaveThrough: 102 }));

    expect(s.getWindow().height).toBe(0);
    expect(s.drainChanges().windowChanged).toBe(true);
  });
});

describe("LineStore: cap eviction with a browse cache resident", () => {
  it("steps over the cache and takes only tail victims", () => {
    // A cache drop is not a trim: the walk hops a browse interval exactly the
    // way it hops the live window, so `oldestIndex()` stays the GLOBAL minimum
    // and the same index can be re-fetched later.
    const s = new LineStore(5);
    s.applyScroll(scrollMsg(100, 5));
    fetchPage(s, 0, 4, 0);
    expect(s.browseCacheSize()).toBe(4);

    s.applyScroll(scrollMsg(105, 2));

    expect(heldKeys(s)).toEqual([0, 1, 2, 3, 102, 103, 104, 105, 106]);
    expect(s.browseCacheSize()).toBe(4);
    expect(s.oldestIndex()).toBe(0);
    expect(s.highestIndex()).toBe(106);
  });

  it("recomputes the tail bound after hopping the live window", () => {
    // A line stranded ABOVE the window bottom is ordinary evictable history, and
    // the hop means the cursor is no longer the minimum key — both bounds have
    // to be recomputed, or `highestIndex()` names an evicted line and resume
    // tells the server the client already has it.
    const s = new LineStore(1);
    s.applyScreen(screenMsg(0, 5));
    s.applyScroll(scrollMsg(100, 1));

    expect(heldKeys(s)).toEqual([0, 1, 2, 3, 4]);
    expect(s.highestIndex()).toBe(4);
    expect(s.oldestIndex()).toBe(0);
  });

  it("recomputes an oldest bound that is neither 0 nor 1", () => {
    const s = new LineStore(1);
    s.applyScreen(screenMsg(10, 5));
    s.applyScroll(scrollMsg(100, 1));

    expect(heldKeys(s)).toEqual([10, 11, 12, 13, 14]);
    expect(s.oldestIndex()).toBe(10);
  });

  it("recomputes a post-hop highest bound of exactly 0", () => {
    // A one-row screen at base 0 with one stranded line above it: after the hop
    // evicts the stray, the only key left is 0, and that is what both bounds
    // must report.
    const s = new LineStore(1);
    s.applyScreen(screenMsg(0, 1));
    s.applyScroll(scrollMsg(100, 1));

    expect(heldKeys(s)).toEqual([0]);
    expect(s.highestIndex()).toBe(0);
    expect(s.oldestIndex()).toBe(0);
  });

  it("recomputes a highest bound of exactly 0", () => {
    // A one-row screen at the very start of a session: every other retained line
    // is stranded above it, and the recomputed tail bound is 0 — a real index,
    // not the empty sentinel.
    const s = new LineStore();
    s.applyScroll(scrollMsg(5, 1));

    s.applyScreen(screenMsg(0, 1));

    expect(heldKeys(s)).toEqual([0]);
    expect(s.highestIndex()).toBe(0);
    expect(s.oldestIndex()).toBe(0);
  });
});

describe("LineStore: the browse budget's viewport exemption", () => {
  /**
   * Two cached runs either side of a hole, reclassified against the small
   * containment target by a resume ack — the shape §5.3's exemption is written
   * for, and the only one where the target is smaller than the band.
   */
  function spanningCache(viewportAbs: number, following: boolean): LineStore {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 400));
    s.applyScroll(scrollMsg(600, 400));
    s.applyResumeAck({
      epochChanged: false,
      committed: 5000,
      serverOldest: 2000,
      paging: true,
      sentHaveThrough: 999,
      sentReplayMax: null,
      viewportAbs,
      following,
    });
    return s;
  }

  it("keeps a cache that fits inside the reader's band rather than blanking it", () => {
    // The reader sits in the hole at 500 and the whole 800-row cache lies within
    // one threshold of them, so nothing is evictable. The pass accepts the
    // bounded overshoot instead of dropping rows under the reader.
    const s = spanningCache(500, true);
    expect(s.browseCacheSize()).toBe(800);
  });

  it("evicts the far end for a reader parked near the bottom of the cache", () => {
    // Same band, reader at 100: the top of the cache is outside the exemption
    // and is the end farther away, so it goes first and the reader's
    // neighbourhood survives.
    const s = spanningCache(100, true);
    expect(s.browseCacheSize()).toBe(500);
    expect(s.getLine(0)).toBeDefined();
    expect(s.getLine(999)).toBeUndefined();
  });

  it("takes the older end when both ends are exactly as far from the reader", () => {
    // Documented tie-break: the low end goes, matching the tail cap's
    // oldest-first bias. The cache spans [1, 3000) and the reader sits at 1500,
    // equidistant from both edges.
    const s = new LineStore();
    s.noteSolicited(1, 3000);
    s.applyHistoryScroll(scrollMsg(1, 2999), 1500);
    s.clearSolicited();

    expect(s.browseCacheSize()).toBe(BROWSE_CACHE_CAP);
    expect(s.getLine(1)).toBeUndefined();
    expect(s.getLine(2999)).toBeDefined();
  });

  it("orders the victim pool by index, not by the order pages arrived", () => {
    // A reader scrolling UP fetches descending, so the cache's insertion order
    // is the reverse of its index order. Which end is "far" is a question about
    // indices.
    const s = new LineStore();
    s.applyScroll(scrollMsg(600, 400));
    s.applyScroll(scrollMsg(0, 400));
    s.applyResumeAck({
      epochChanged: false,
      committed: 5000,
      serverOldest: 2000,
      paging: true,
      sentHaveThrough: 999,
      sentReplayMax: null,
      viewportAbs: 100,
      following: true,
    });

    expect(s.browseCacheSize()).toBe(500);
    expect(s.getLine(0)).toBeDefined();
    expect(s.getLine(999)).toBeUndefined();
  });
});

describe("LineStore: the bounds after a bulk removal", () => {
  it("reports index 0 as the oldest when the cache above it is dropped", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 3));
    s.applyScroll(scrollMsg(10, 1));
    fetchPage(s, 10, 1, 10);

    s.dropBrowseCache(0, true);

    expect(heldKeys(s)).toEqual([0, 1, 2]);
    expect(s.oldestIndex()).toBe(0);
    expect(s.highestIndex()).toBe(2);
  });

  it("reports the lowest key as the oldest when rows arrived newest-first", () => {
    // A frame's `changed` list is the server's order, not the client's: rows can
    // land descending, so the recompute cannot lean on insertion order.
    const s = new LineStore();
    s.applyScreen(screenMsg(100, 3, [2, 1, 0]));
    s.applyScroll(scrollMsg(200, 1));
    fetchPage(s, 200, 1, 200);

    s.dropBrowseCache(0, true);

    expect(heldKeys(s)).toEqual([100, 101, 102]);
    expect(s.oldestIndex()).toBe(100);
    expect(s.highestIndex()).toBe(102);
  });
});

describe("LineStore: what the trim marker reads", () => {
  it("reports no trim on a store that has evicted nothing and asked nothing", () => {
    // With no eviction of its own and no server bound to compare against, the
    // store has no grounds to claim history is gone — the renderer's marker
    // would then be permanent noise on a session that trimmed nothing.
    const s = new LineStore();
    s.applyScroll(scrollMsg(100, 3));

    expect(s.hasTrimmedHistory()).toBe(false);
  });

  it("reports a trim after evicting exactly index 0", () => {
    const s = new LineStore(2);
    s.applyScroll(scrollMsg(0, 3));

    expect(s.oldestIndex()).toBe(1);
    expect(s.hasTrimmedHistory()).toBe(true);
  });

  it("reports no trim when the only eviction was ABOVE the live window", () => {
    // The stranded line a malformed stream parks above the window is evictable
    // history, and dropping it advances the watermark — but the store still
    // holds index 0, so nothing OLDER than what it has was trimmed.
    const s = new LineStore(1);
    s.applyScreen(screenMsg(0, 5));
    s.applyScroll(scrollMsg(100, 1));

    expect(s.oldestIndex()).toBe(0);
    expect(s.hasTrimmedHistory()).toBe(false);
  });

  it("reports no trim while the client still holds history the server has dropped", () => {
    // The server's ring starts at 50 and this client holds from 10: the depth
    // below 50 is in the client's hands, not gone, so the marker must stay off.
    const s = new LineStore();
    s.applyScroll(scrollMsg(10, 5));
    s.noteResumeBounds(200, 50);

    expect(s.hasTrimmedHistory()).toBe(false);
  });
});

describe("LineStore: the exact edges of the two staleness watermarks", () => {
  /** A hydrated store: the tail is [100, 105) and everything below it is stale. */
  function hydrated(): LineStore {
    const source = new LineStore();
    source.applyScroll(scrollMsg(100, 5));
    const store = LineStore.fromSnapshot(source.snapshot(0));
    if (store === null) {
      throw new Error("fixture: the snapshot should hydrate");
    }
    return store;
  }

  it("refuses a re-send AT the eviction watermark, not just below it", () => {
    // The watermark means "lines at or below this are stale". A hydrated store
    // derives it as `oldest - 1`, so index 99 is exactly it.
    const s = hydrated();

    s.applyScroll(scrollMsg(99, 1));

    expect(s.getLine(99)).toBeUndefined();
  });

  it("refuses an uncorrelated re-send AT the erased watermark, not just below it", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 10));
    const ed3 = screenMsg(5, 5);
    ed3.scrollbackCleared = true;
    s.applyScreen(ed3); // drops 0..4, so the erased watermark is 4

    s.applyScroll(scrollMsg(4, 1));

    expect(s.getLine(4)).toBeUndefined();
  });

  it("treats the row one past the window bottom as outside the window", () => {
    // The window exemption is what lets a retreating base repaint the screen
    // below the watermark (a hydrated tail with the window under it). One row
    // past its bottom is not the writable screen, so a stale re-send there stays
    // refused.
    const s = hydrated();
    s.applyScreen(screenMsg(95, 3)); // window rows 95..97, admitted by exemption

    s.applyScroll(scrollMsg(98, 1));

    expect(s.getLine(97)).toBeDefined();
    expect(s.getLine(98)).toBeUndefined();
  });
});

describe("LineStore: a window that sits below everything retained", () => {
  it("empties both reported bounds when every line is stranded below it", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(100, 3));

    s.applyScreen(screenMsg(0, 1, []));

    expect(heldKeys(s)).toEqual([]);
    expect(s.oldestIndex()).toBe(-1);
    expect(s.highestIndex()).toBe(-1);
  });

  it("reports the highest SURVIVING key after a shrink, not the last one scanned", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(5, 1));

    s.applyScreen(screenMsg(0, 2, [1, 0]));

    expect(heldKeys(s)).toEqual([0, 1]);
    expect(s.highestIndex()).toBe(1);
    expect(s.oldestIndex()).toBe(0);
  });
});
