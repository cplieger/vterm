// The fetch trigger's input geometry: which absent edges the store offers a
// reader, in what order, and the two pieces of bookkeeping that decide what is
// still worth asking for (the paging floor and the in-flight request window).
//
// `absentEdgesNear` is the whole trigger contract. An edge it offers that the
// server cannot serve costs a round trip per scroll event; an edge it withholds
// is history the reader can never reach. The frontier below the lowest held
// index is a pseudo-gap derived from policy, not geometry, so all three of its
// preconditions are pinned here in both directions.
//
// Pure data structure, no DOM.

import { describe, it, expect } from "vitest";
import { LineStore } from "./store.js";
import type { ScrollMessage, WireRun } from "./types.js";

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

function heldCount(s: LineStore): number {
  let n = 0;
  s.forEachLine(() => n++);
  return n;
}

describe("LineStore.absentEdgesNear: the bottom frontier", () => {
  it("offers the frontier when the server still retains history below the tail", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(100, 10));
    s.noteResumeBounds(110, 0);

    expect(s.absentEdgesNear(100, 500)).toEqual([{ lo: 0, hi: 100 }]);
  });

  it("withholds the frontier once the server's oldest reaches what the tail holds", () => {
    // The server retains nothing older than index 100 and the store holds 100:
    // there is no longer anything below to ask for.
    const s = new LineStore();
    s.applyScroll(scrollMsg(100, 10));
    s.noteResumeBounds(110, 100);

    expect(s.absentEdgesNear(100, 500)).toEqual([]);
  });

  it("withholds the frontier while the store still holds index 0", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 10));

    expect(s.absentEdgesNear(0, 500)).toEqual([]);
  });
});

describe("LineStore.absentEdgesNear: the nearness band", () => {
  /** Two retained runs with one interior hole, and no frontier on offer. */
  function twoRuns(): LineStore {
    const s = new LineStore();
    s.applyScroll(scrollMsg(1000, 10));
    s.applyScroll(scrollMsg(3000, 10));
    s.noteResumeBounds(3010, 1000); // nothing older survives: frontier closed
    return s;
  }

  it("leaves out a hole the reader is nowhere near", () => {
    expect(twoRuns().absentEdgesNear(5000, 500)).toEqual([]);
  });

  it("includes a hole whose low edge is exactly one threshold away", () => {
    expect(twoRuns().absentEdgesNear(510, 500)).toEqual([{ lo: 1010, hi: 3000 }]);
  });

  it("includes a hole whose high edge is exactly one threshold away", () => {
    expect(twoRuns().absentEdgesNear(3500, 500)).toEqual([{ lo: 1010, hi: 3000 }]);
  });
});

describe("LineStore.absentEdgesNear: ordering by nearness to the reader", () => {
  /** Three retained runs, so two interior holes, and no frontier on offer. */
  function threeRuns(): LineStore {
    const s = new LineStore();
    s.applyScroll(scrollMsg(1000, 10));
    s.applyScroll(scrollMsg(3000, 10));
    s.applyScroll(scrollMsg(5000, 10));
    s.noteResumeBounds(5010, 1000);
    return s;
  }

  it("puts the upper hole first for a reader below it", () => {
    // Edges are ranked by distance to the hole's HIGH edge — the side the reader
    // scrolls into. From 4900 that is the upper hole, which is the SECOND in
    // index order, so an unsorted answer reads backwards.
    expect(threeRuns().absentEdgesNear(4900, 5000)).toEqual([
      { lo: 3010, hi: 5000 },
      { lo: 1010, hi: 3000 },
    ]);
  });

  it("puts the lower hole first for a reader just above it", () => {
    expect(threeRuns().absentEdgesNear(1200, 5000)).toEqual([
      { lo: 1010, hi: 3000 },
      { lo: 3010, hi: 5000 },
    ]);
  });
});

describe("LineStore: the paging floor", () => {
  it("does not fall when the server reports an oldest above it", () => {
    // Within one epoch a ring's oldest only rises, so a report ABOVE the floor
    // is ordinary progress and tells the floor nothing. Only a report BELOW it
    // is the repair path for a floor raised by a mis-correlated clamp.
    const s = new LineStore();
    s.raisePagingFloor(500);
    s.noteResumeBounds(2000, 800);

    expect(s.pagingFloorIndex()).toBe(500);
  });

  it("ignores an invalid server oldest, the floor repair included", () => {
    const s = new LineStore();
    s.noteResumeBounds(2000, -1);
    s.noteResumeBounds(2000, 1.5);

    expect(s.serverOldestIndex()).toBe(-1);
    expect(s.pagingFloorIndex()).toBe(0);
  });

  it("accepts a server oldest of exactly 0", () => {
    const s = new LineStore();
    s.noteResumeBounds(2000, 0);

    expect(s.serverOldestIndex()).toBe(0);
  });
});

describe("LineStore: the in-flight request window", () => {
  it("does not record an empty window, so a reply correlated against it is dropped", () => {
    // Every concession the bulk path makes is paid for by a request the client
    // currently has out. An empty window is not a request, so the reply carries
    // no permission and is refused whole rather than downgraded.
    const s = new LineStore();
    s.noteSolicited(10, 10);
    s.applyHistoryScroll(scrollMsg(10, 2), 10);

    expect(heldCount(s)).toBe(0);
    expect(s.oldestIndex()).toBe(-1);
  });

  it("does not record a window with a non-finite edge", () => {
    const s = new LineStore();
    s.noteSolicited(Number.NaN, 5);
    s.applyHistoryScroll(scrollMsg(0, 2), 0);

    expect(heldCount(s)).toBe(0);
  });

  it("retracts the window on release, so a late reply is dropped whole", () => {
    const s = new LineStore();
    s.noteSolicited(0, 3);
    s.clearSolicited();
    s.applyHistoryScroll(scrollMsg(0, 3), 0);

    expect(heldCount(s)).toBe(0);
    expect(s.browseCacheSize()).toBe(0);
  });
});
