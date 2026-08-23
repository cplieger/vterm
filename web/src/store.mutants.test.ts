// Boundary and guard behaviour of the line store, at the sites where the
// existing suites drive the code but assert nothing that distinguishes an
// off-by-one from the real edge.
//
// Three themes, each with a defect it is written against:
//
//  - The PROVISIONAL FLOOR (`unconfirmedFrom`, read on the wire as
//    `replayBoundary()`). Every mistake here is a wrong `haveThrough`: too high
//    and the server never re-sends rows this client only ever saw the
//    application draw, which is the frozen-composer bug the floor exists to
//    prevent. So the floor's guards are driven with the frames that reach them —
//    a malformed base, an empty chunk, a frame straddling a frozen alt window.
//  - RESIDENCY (the tail cap, the browse budget, the cap flip). The invariants
//    are cheap to state and expensive to get wrong: a live window row must never
//    be reclassified as disposable cache, the rows within the prefetch band of
//    the reader must never be evicted, and `oldestIndex()`/`highestIndex()` must
//    name lines the store actually holds after a pass that removed some.
//  - HOSTILE OR MALFORMED FRAMES. `changed` indices outside the grid, a
//    negative base, a snapshot whose run array has a hole. Each of these
//    reaches a guard whose absence is a crash or a silently wiped store.
//
// Pure data structure, no DOM.

import { describe, it, expect, vi } from "vitest";
import { LineStore, PREFETCH_THRESHOLD } from "./store.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
}

/** A scroll/history frame of `count` lines starting at `firstIndex`. */
function scrollMsg(firstIndex: number, count: number, tag = "L"): ScrollMessage {
  const lines: WireRun[][] = [];
  for (let i = 0; i < count; i++) {
    lines.push(row(`${tag}${String(firstIndex + i)}`));
  }
  return { type: "scroll", firstIndex, lines };
}

/**
 * A screen frame of `height` rows at `base`. `changed` defaults to every row;
 * pass a shorter list to model a sparse frame (rows the server did not resend).
 */
function screenMsg(
  base: number,
  height: number,
  opts: Partial<{
    changed: number[];
    tag: string;
    altActive: boolean;
    scrollbackCleared: boolean;
    cursor: [number, number];
  }> = {},
): ScreenMessage {
  const tag = opts.tag ?? "W";
  const rows: WireRun[][] = [];
  for (let y = 0; y < height; y++) {
    rows.push(row(`${tag}${String(base + y)}`));
  }
  return {
    type: "screen",
    base,
    rows,
    changed: opts.changed ?? rows.map((_, y) => y),
    cursor: opts.cursor ?? [0, 0],
    altActive: opts.altActive ?? false,
    scrollbackCleared: opts.scrollbackCleared ?? false,
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
  s.applyHistoryScroll(scrollMsg(fromAbs, count, "P"), viewportAbs);
  s.clearSolicited();
}

/** A resume ack with the bounds tail present; callers override what they test. */
function ack(over: {
  committed: number;
  serverOldest: number;
  sentHaveThrough: number;
  paging?: boolean;
  sentReplayMax?: number | null;
  viewportAbs?: number;
  following?: boolean;
}): Parameters<LineStore["applyResumeAck"]>[0] {
  return {
    epochChanged: false,
    committed: over.committed,
    serverOldest: over.serverOldest,
    paging: over.paging ?? true,
    sentHaveThrough: over.sentHaveThrough,
    sentReplayMax: over.sentReplayMax ?? null,
    viewportAbs: over.viewportAbs ?? -1,
    following: over.following ?? false,
  };
}

describe("LineStore.replayBoundary: the value that goes on the wire", () => {
  it("claims nothing when the only line held is row 0 of the live window", () => {
    // The boundary is `min(highest, unconfirmedFrom - 1)`, and the whole point
    // is that a store whose content is entirely provisional claims NOTHING. A
    // one-row window at index 0 is the smallest store where the two readings
    // differ: `highest` is 0 and the honest answer is -1, so returning the
    // index itself would tell the server "no need to re-send row 0" about a row
    // this client has only ever seen the application draw.
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 1));
    expect(s.highestIndex()).toBe(0);
    expect(s.replayBoundary()).toBe(-1);
  });

  it("claims the window's rows once a durable frame commits them", () => {
    // The same store after the server commits those indices as history: the
    // floor rises past them and the boundary becomes the real highest.
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 1));
    s.applyScroll(scrollMsg(0, 1, "H"));
    expect(s.replayBoundary()).toBe(0);
  });
});

describe("LineStore: the provisional floor and malformed frames", () => {
  it("moves no floor and retains no negative index for a frame based below zero", () => {
    // Two guards over one hostile frame. markUnconfirmedFrom's: a base that is
    // not a non-negative integer cannot be a screen position, and moving the
    // floor there puts a negative value on the wire as `haveThrough`. Guard 1
    // of applyLine: `base + y` is not addressable while it is negative — the
    // renderer orders rows by that index and the snapshot writer validates it
    // away — while the rows that land at or above zero are ordinary content.
    //
    // The window is 20 rows so its bottom row stays above what the store holds;
    // a shorter one strands every retained line below the window instead, which
    // is a separate path (truncateBelowWindow) with its own effect.
    const s = new LineStore();
    s.applyScreen(screenMsg(10, 3));
    expect(s.replayBoundary()).toBe(9);

    s.applyScreen(screenMsg(-5, 20));

    expect(s.replayBoundary()).toBe(9);
    expect(s.getLine(-5)).toBeUndefined();
    expect(s.getLine(-1)).toBeUndefined();
    expect(s.oldestIndex()).toBe(0);
  });

  it("keeps the transcript when a screen frame is based below zero", () => {
    // The path the test above deliberately steps around, with a window short
    // enough to reach it. A base below zero puts the window BOTTOM
    // (`base + height - 1`) below every retained index, and truncateBelowWindow
    // reads that as "every line is stranded past the live window" and forgets
    // all of them. Nothing records it: the eviction watermark only moves on a
    // trim, so `hasTrimmedHistory()` stays false and the renderer draws no
    // "earlier output trimmed" marker — the transcript just disappears. Same
    // class as the rows-less guard one branch up, whose comment records the same
    // measurement (a 3024-line store wiped to 0 rows, silently).
    const s = new LineStore();
    s.applyScreen(screenMsg(10, 3));
    s.applyScroll(scrollMsg(10, 3, "H")); // committed history, not a drawn screen
    expect(heldKeys(s)).toEqual([10, 11, 12]);

    s.applyScreen(screenMsg(-5, 3));

    expect(heldKeys(s)).toEqual([10, 11, 12]);
    expect(s.oldestIndex()).toBe(10);
    expect(s.highestIndex()).toBe(12);
    expect(s.hasTrimmedHistory()).toBe(false);
  });

  it("applies the addressable rows of a below-zero frame without adopting its window", () => {
    // The other half: the frame is refused as GEOMETRY, not as content. Its rows
    // at or above zero are ordinary content (applyLine's guard 1 is what decides
    // that, as the test above pins), while the descriptor keeps the last base a
    // frame stated legitimately — so the retention bound, the guard-2 window
    // exemption and the renderer's window all keep naming real screen positions.
    const s = new LineStore();
    s.applyScreen(screenMsg(10, 3));

    s.applyScreen(screenMsg(-2, 5)); // rows at -2..2; only 0, 1 and 2 are addressable

    expect(heldKeys(s)).toEqual([0, 1, 2, 10, 11, 12]);
    expect(s.getWindow().base).toBe(10);
    expect(s.getWindow().height).toBe(3);
  });

  it("keeps a replay that arrived before any window frame when a below-zero frame follows", () => {
    // The same wipe reached with no window ESTABLISHED yet. A resume replays
    // history before the screen frame that describes the window (the ordering
    // enforceCap's comment names), so `win` is still the initial base 0/height 0
    // descriptor — a window bottom of -1, below every retained index. There is no
    // window, so nothing can be stranded past one.
    const s = new LineStore();
    s.applyScroll(scrollMsg(10, 3, "H")); // replay, still no window frame

    s.applyScreen(screenMsg(-5, 3));

    expect(heldKeys(s)).toEqual([10, 11, 12]);
    expect(s.hasTrimmedHistory()).toBe(false);
  });

  it("confirms nothing from a scroll frame that carries no lines", () => {
    // confirmRange's `count <= 0` guard. An empty chunk proves nothing about
    // any index, so it must not raise the floor: doing so would claim the
    // window rows at and above `firstIndex` as committed history on the
    // strength of a frame that delivered none of them.
    const s = new LineStore();
    s.applyScreen(screenMsg(10, 3));
    expect(s.replayBoundary()).toBe(9);

    s.applyScroll({ type: "scroll", firstIndex: 12, lines: [] });

    expect(s.replayBoundary()).toBe(9);
  });

  it("confirms nothing from a scroll frame whose first index is negative", () => {
    // The other half of the same guard. The frame's negative row is refused by
    // the index guard, so treating `firstIndex + count` as confirmed would
    // raise the floor over rows this frame never legitimately delivered.
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 3));
    expect(s.replayBoundary()).toBe(-1);

    s.applyScroll(scrollMsg(-1, 5, "H"));

    expect(s.replayBoundary()).toBe(-1);
  });

  it("confirms a durable frame that starts at index 0", () => {
    // The boundary case on the other side: index 0 is a legitimate first
    // index, so the guard must reject `< 0`, not `<= 0`. A store whose whole
    // window has been committed claims all of it.
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 3));
    s.applyScroll(scrollMsg(0, 3, "H"));

    expect(s.replayBoundary()).toBe(2);
  });

  it("leaves the floor alone when a durable frame lands entirely above it", () => {
    // The documented asymmetry: raising happens only when a durable frame
    // reaches the floor from at or below it. Rows 11-12 being committed says
    // nothing about row 10, which the application is still repainting, so the
    // floor stays at 10 and the boundary stays at 9.
    const s = new LineStore();
    s.applyScreen(screenMsg(10, 3));

    s.applyScroll(scrollMsg(11, 2, "H"));

    expect(s.replayBoundary()).toBe(9);
  });

  it("does not heal the floor on a frame that carries no lines", () => {
    // The heal shares the raise's `count > 0` gate, so an empty chunk moves the
    // floor in NEITHER direction. It is the one shape where an unguarded heal
    // is silent: the frame delivers nothing, so nothing else it does is
    // observable, and the floor it invents is read straight onto the wire.
    const s = new LineStore(2);
    s.applyScreen(screenMsg(0, 1)); // the floor drops to 0
    s.applyScreen(screenMsg(10, 1));
    s.applyScreen(screenMsg(11, 1)); // line 0 is evicted: oldest passes the floor
    expect(s.oldestIndex()).toBe(10);
    expect(s.replayBoundary()).toBe(-1);

    s.applyScroll({ type: "scroll", firstIndex: 12, lines: [] });

    expect(s.replayBoundary()).toBe(-1);
  });

  it("heals a floor left below everything the store still holds", () => {
    // The floor names an index that has since been evicted, so it protects
    // nothing while capping the boundary at -1 — a maximal replay on every
    // attach. A durable frame arriving above it repairs it through the same
    // writer as an ordinary raise.
    const s = new LineStore(3);
    s.applyScreen(screenMsg(0, 1)); // floor drops to 0
    s.applyScreen(screenMsg(10, 1)); // window moves; the floor stays at 0
    expect(s.replayBoundary()).toBe(-1);

    s.applyScroll(scrollMsg(11, 2, "H")); // evicts line 0, then confirms 11-12

    expect(heldKeys(s)).toEqual([10, 11, 12]);
    expect(s.replayBoundary()).toBe(12);
  });
});

describe("LineStore.absentEdgesNear: the bottom frontier", () => {
  it("offers no frontier when the server retains nothing older than we hold", () => {
    // The frontier is a pseudo-gap the trigger will actually REQUEST, so it is
    // offered only when there is something below to ask for. The server's own
    // oldest retained index is the authority: at or below it the range is gone,
    // and asking spends an RTT to be told so — then the reply's clamp raises the
    // floor, which is the client teaching itself its own history was trimmed.
    const s = new LineStore();
    s.applyScroll(scrollMsg(22, 3, "H"));
    s.noteResumeBounds(25, 22); // the server's oldest IS our lowest
    expect(s.serverOldestIndex()).toBe(22);

    expect(s.absentEdgesNear(22, 500)).toEqual([]);
  });

  it("offers the frontier when the server still holds older history", () => {
    // The same store against a server that reports deeper history: now the
    // frontier is real, and it runs from the floor up to the lowest line held.
    const s = new LineStore();
    s.applyScroll(scrollMsg(22, 3, "H"));
    s.noteResumeBounds(25, 5);

    expect(s.absentEdgesNear(22, 500)).toEqual([{ lo: 0, hi: 22 }]);
  });
});

describe("LineStore: history arriving while the alternate screen is up", () => {
  it("confirms only the prefix below the frozen main base", () => {
    // The alt gate drops every row at or above the frozen window base, so the
    // range confirmed has to be the ACCEPTED prefix. Confirming the whole frame
    // would raise the floor over rows the store just refused to store — the
    // wire value would then exclude them from the replay that is the only way
    // this client ever learns their committed content.
    const s = new LineStore();
    s.applyScreen(screenMsg(100, 5)); // main window 100..104, floor at 100
    s.applyScreen(screenMsg(100, 5, { altActive: true, tag: "A" }));
    expect(s.isAlt()).toBe(true);

    s.applyScroll(scrollMsg(98, 5, "H")); // 98,99 accepted; 100..102 refused

    expect(s.getLine(98)).toBeDefined();
    expect(s.getLine(99)).toBeDefined();
    expect(s.replayBoundary()).toBe(99);
  });
});

describe("LineStore: alt-grid writes from a frame that does not carry the row", () => {
  it("leaves a row named in `changed` but absent from `rows` intact", () => {
    // The alt row guard's `row !== undefined` conjunct. A sparse frame naming a
    // row it did not carry must not blank the grid: `altRows[y] = undefined`
    // reaches the renderer's per-row reconcile as a missing array.
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 3, { altActive: true, tag: "A" }));
    expect(s.getAltRows()).toHaveLength(3);

    const sparse = screenMsg(0, 3, { altActive: true, tag: "B" });
    // Row 1 is named as changed but not delivered — the shape a partial frame
    // produces once `rows` is built sparsely.
    delete sparse.rows[1];
    s.applyScreen(sparse);

    const rows = s.getAltRows();
    expect(rows[0]?.[0]?.t).toBe("B0");
    expect(rows[1]?.[0]?.t).toBe("A1");
    expect(rows[2]?.[0]?.t).toBe("B2");
  });

  it("ignores a `changed` index below the grid instead of writing outside it", () => {
    // `y >= 0`. A negative index is only reachable from a malformed or hostile
    // frame, and the cost of admitting it is a write outside the grid plus a
    // repaint the renderer cannot use. Nothing changed, so nothing is flagged.
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 3, { altActive: true, tag: "A" }));
    s.drainChanges();

    const hostile = screenMsg(0, 3, { altActive: true, tag: "A" });
    hostile.changed = [-1];
    // A row really present at index -1: without it the `row !== undefined`
    // conjunct would refuse the write on its own and the index guard would
    // never be exercised.
    Object.defineProperty(hostile.rows, "-1", { value: row("evil"), enumerable: true });
    s.applyScreen(hostile);

    expect(s.drainChanges().altChanged).toBe(false);
    expect(s.getAltRows()).toHaveLength(3);
  });
});

describe("LineStore: the alt-grid dirty flags", () => {
  it("does not flag the alt grid on a main-screen frame", () => {
    // exitAltIfNeeded is conditional for a reason: flagging the grid on every
    // main-screen frame makes the renderer reconcile an alt overlay that is not
    // there, on every flush of a normal session.
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 3));

    expect(s.drainChanges().altChanged).toBe(false);
  });
});

describe("LineStore: the retention bound after a window shrink", () => {
  it("re-checks the cap for a frame that lowers the bound without applying a line", () => {
    // The bound is `max(cap, window height)`, so a shorter window LOWERS it —
    // and a frame whose rows are byte-identical short-circuits the idempotency
    // guard, which is the only other place the cap is enforced. Without the
    // per-frame re-check the store keeps a tail the bound no longer allows,
    // for as long as the content stays unchanged.
    const s = new LineStore(5);
    // A 20-row window with only its first five rows delivered: the descriptor's
    // height sets the bound while the store holds far fewer window rows.
    s.applyScreen(screenMsg(20, 20, { changed: [0, 1, 2, 3, 4] }));
    s.applyScroll(scrollMsg(0, 15, "H"));
    expect(heldKeys(s)).toHaveLength(20);

    // Same base, same content, ten rows shorter: nothing applies, the bound
    // drops from 20 to 10.
    s.applyScreen(screenMsg(20, 10, { changed: [0, 1, 2, 3, 4] }));

    expect(heldKeys(s)).toEqual([10, 11, 12, 13, 14, 20, 21, 22, 23, 24]);
    expect(s.oldestIndex()).toBe(10);
  });
});

describe("LineStore.enforceCap: the walk's boundaries", () => {
  it("keeps the newest line when the cap is zero and there is no window", () => {
    // `new LineStore(0)` is a legal store (a cap at or below the screen height
    // keeps the screen and no scrollback), and the walk's `cursor < highest`
    // conjunct is the only thing that stops it evicting the one line it holds.
    // A store that shows nothing at all is not a smaller store, it is a broken
    // one.
    const s = new LineStore(0);
    s.applyScroll(scrollMsg(7, 1, "H"));

    expect(heldKeys(s)).toEqual([7]);
    expect(s.getLine(7)).toBeDefined();
  });

  it("keeps the newest line when a window descriptor exists but holds no row", () => {
    // The sibling conjunct. A cursor-only frame sets the descriptor without
    // delivering a row, so the store's only line sits BELOW a window it does
    // not overlap: the walk must still refuse to evict the last line.
    const s = new LineStore(0);
    s.applyScreen(screenMsg(10, 1, { changed: [] }));
    expect(heldKeys(s)).toEqual([]);

    s.applyScroll(scrollMsg(5, 1, "H"));

    expect(heldKeys(s)).toEqual([5]);
  });

  it("evicts the oldest history and never a live window row", () => {
    // The walk hops the window and keeps walking, so the victims are the
    // OLDEST evictable lines — not the newest, and never a screen row. An
    // off-by-one on the window's bottom row moves the protected band and the
    // walk starts eating the lines nearest the incoming window, which is
    // exactly what leaves a permanent hole under the live screen.
    const s = new LineStore(3);
    s.applyScreen(screenMsg(0, 1)); // window row 0, protected
    s.applyScroll(scrollMsg(1, 4, "H")); // 1..4 arrive above it

    expect(heldKeys(s)).toEqual([0, 3, 4]);
    expect(s.getLine(0)?.[0]?.t).toBe("W0");
  });

  it("steps over a browse interval to reach the tail above it", () => {
    // The hop predicate extends to browse membership: cache lives under its own
    // budget and live output must not evict it. The walk therefore steps past
    // the whole interval and trims the tail above, which is also what keeps
    // `oldestIndex()` the global minimum.
    const s = new LineStore(4);
    fetchPage(s, 0, 3, 0); // 0..2 become browse cache
    expect(s.browseCacheSize()).toBe(3);

    s.applyScroll(scrollMsg(3, 6, "H")); // 3..8 arrive as tail

    // The three cached lines survive; the tail is trimmed from its oldest end.
    expect(s.browseCacheSize()).toBe(3);
    expect(heldKeys(s)).toEqual([0, 1, 2, 5, 6, 7, 8]);
    expect(s.oldestIndex()).toBe(0);
    expect(s.highestIndex()).toBe(8);
  });

  it("hops to the lowest tail key above the cache, not the first the map yields", () => {
    // The hop resumes an OLDEST-first walk, so it has to find the MINIMUM tail
    // key above the interval. The store's map iterates in insertion order, and
    // the natural order of arrival is the window frame first with the replayed
    // history below it second — so "first key found" and "lowest key" routinely
    // differ. Taking the first evicts a line next to the live window while the
    // oldest one stays, and it advances the trim watermark over everything
    // between them, which refuses their re-delivery for the rest of the epoch.
    const s = new LineStore(12);
    s.applyScreen(screenMsg(20, 6)); // inserted first: 20..25
    s.applyScroll(scrollMsg(10, 6, "H")); // inserted second: 10..15
    fetchPage(s, 0, 3, 0); // the cache the walk has to step over
    expect(s.browseCacheSize()).toBe(3);

    s.applyScroll(scrollMsg(16, 1, "H")); // one line over the bound: one victim

    expect(s.getLine(10)).toBeUndefined();
    expect(s.getLine(20)).toBeDefined();
    expect(heldKeys(s)).toEqual([0, 1, 2, 11, 12, 13, 14, 15, 16, 20, 21, 22, 23, 24, 25]);
  });

  it("drops an evicted line from the dirty set", () => {
    // forget() owns every removal, and a line the renderer has not painted yet
    // can be evicted before the next drain. Leaving it in `dirtyLines` asks the
    // renderer to build a row the store no longer holds.
    const s = new LineStore(2);
    s.applyScroll(scrollMsg(0, 3, "H"));

    const changes = s.drainChanges();
    expect(changes.dirtyLines.sort((a, b) => a - b)).toEqual([1, 2]);
    expect(changes.evictedLines).toContain(0);
  });
});

describe("LineStore: ED3 (erase saved lines)", () => {
  it("does not lower the provisional floor when the erase bound is below it", () => {
    // The floor moves up with an erase and never down. A rows-less ED3 frame is
    // the shape that reaches this alone: it is a SIGNAL, so applyScreen returns
    // before the frame's own base can lower the floor. A floor dragged back
    // down stays there while `highest` climbs, and every attach replays
    // everything above it.
    const s = new LineStore();
    s.applyScreen(screenMsg(100, 3));
    expect(s.replayBoundary()).toBe(99);

    s.applyScreen({
      type: "screen",
      base: 50,
      rows: [],
      changed: [],
      cursor: [0, 0],
      scrollbackCleared: true,
    });

    expect(s.replayBoundary()).toBe(99);
    expect(s.pagingFloorIndex()).toBe(50);
  });
});

describe("LineStore: the browse budget's viewport exemption", () => {
  it("keeps the prefetch band above the reader and evicts the line past it", () => {
    // The exemption is `[viewport - PREFETCH_THRESHOLD, viewport +
    // PREFETCH_THRESHOLD]`, and both edges matter: one line too narrow blanks a
    // row the reader can see, one too wide pins cache the pass needed to free.
    // The reader is BELOW the cache here, so the pass takes victims from the
    // far (top) end and stops exactly at the band's upper edge.
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 1200, "H"));
    const viewport = 100;

    // A predicted replay jump reclassifies everything this socket claimed as
    // disposable cache, then the FOLLOWING containment target drains it.
    s.applyResumeAck(
      ack({
        committed: 9000,
        serverOldest: 5000,
        sentHaveThrough: 1199,
        viewportAbs: viewport,
        following: true,
      }),
    );

    expect(s.getLine(viewport + PREFETCH_THRESHOLD)).toBeDefined();
    expect(s.getLine(viewport + PREFETCH_THRESHOLD + 1)).toBeUndefined();
    expect(s.highestIndex()).toBe(viewport + PREFETCH_THRESHOLD);
    expect(s.browseCacheSize()).toBe(viewport + PREFETCH_THRESHOLD + 1);
  });

  it("reports bounds that name lines it still holds after the pass", () => {
    // Same pass, read through the accessors the frontier geometry and resume
    // both depend on. A pass that removed the highest cached lines without
    // recomputing leaves `highestIndex()` naming a line that is gone, and the
    // next resume sends it as `haveThrough`.
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 1200, "H"));
    s.applyResumeAck(
      ack({
        committed: 9000,
        serverOldest: 5000,
        sentHaveThrough: 1199,
        viewportAbs: 100,
        following: true,
      }),
    );

    const keys = heldKeys(s);
    expect(s.oldestIndex()).toBe(keys[0]);
    expect(s.highestIndex()).toBe(keys[keys.length - 1]);
    expect(s.getLine(s.highestIndex())).toBeDefined();
  });
});

describe("LineStore.classifyAsBrowse: reclassification is per key", () => {
  it("does not refresh the cache clock when a re-fetch moves nothing", () => {
    // `browseActivityMs` is the consumer's TTL input, and it is gated on
    // something having MOVED. A reply whose keys are already cache changes no
    // classification, so a client re-fetching a range it already holds cannot
    // hold the TTL open forever. The clock has to advance between the two
    // fetches for the difference to be observable at all.
    vi.useFakeTimers();
    try {
      vi.setSystemTime(1_000_000);
      const s = new LineStore();
      s.applyScroll(scrollMsg(1000, 50, "H"));
      fetchPage(s, 0, 20, 0);
      expect(s.browseCacheSize()).toBe(20);
      expect(s.lastBrowseActivityMs()).toBe(1_000_000);

      vi.setSystemTime(2_000_000);
      fetchPage(s, 0, 20, 0); // the same range again: every key is already cache

      expect(s.browseCacheSize()).toBe(20);
      expect(s.lastBrowseActivityMs()).toBe(1_000_000);
    } finally {
      vi.useRealTimers();
    }
  });

  it("refreshes the cache clock when a fetch really adds cache", () => {
    // The other direction of the same gate, so neither half can rot: a reader
    // paging steadily through history keeps the TTL open, because every page
    // moves keys the store did not have classified before.
    vi.useFakeTimers();
    try {
      vi.setSystemTime(1_000_000);
      const s = new LineStore();
      s.applyScroll(scrollMsg(1000, 50, "H"));
      fetchPage(s, 0, 20, 0);
      expect(s.lastBrowseActivityMs()).toBe(1_000_000);

      vi.setSystemTime(2_000_000);
      fetchPage(s, 100, 20, 100); // a range the cache does not hold yet

      expect(s.browseCacheSize()).toBe(40);
      expect(s.lastBrowseActivityMs()).toBe(2_000_000);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("LineStore.applyResumeAck: the cap flip", () => {
  it("reclassifies the oldest excess tail keys, in index order", () => {
    // The band is the OLDEST excess of the tail, which means the key list has
    // to be sorted (a Map iterates in insertion order, and a resume replay
    // delivers high indices before low ones) and filtered to tail keys (a
    // cached key is already cache). Taking the wrong end hands the rows next to
    // the live window to the disposable budget and keeps the ones furthest from
    // the reader.
    const s = new LineStore();
    s.applyScroll(scrollMsg(1000, 1400, "H")); // inserted first, indices high
    s.applyScroll(scrollMsg(0, 200, "H")); // inserted second, indices low
    fetchPage(s, 500, 100, 500); // a cached interior page, above the low run
    expect(s.browseCacheSize()).toBe(100);

    s.applyResumeAck(ack({ committed: 2400, serverOldest: 0, sentHaveThrough: 2399 }));

    // 1600 tail lines against a 1500 target: the oldest 100 join the cache.
    expect(s.browseCacheSize()).toBe(200);
    s.dropBrowseCache(-1, false);
    expect(s.oldestIndex()).toBe(100);
    expect(s.retainedRanges()).toEqual([
      { lo: 100, hi: 200 },
      { lo: 1000, hi: 2400 },
    ]);
  });

  it("never reclassifies a live window row, and reports that nothing moved", () => {
    // Two invariants at one site. The band stops below `win.base` because the
    // browse budget may evict what it holds and the window must not be
    // evictable; and when the filter leaves nothing, the flip reports FALSE, so
    // the ack's containment pass does not run and a reader's existing cache
    // keeps its depth.
    const s = new LineStore();
    s.applyScreen(screenMsg(700, 50)); // window 700..749
    s.applyScroll(scrollMsg(750, 1550, "H")); // tail above it: 1600 lines total
    fetchPage(s, 0, 600, 0); // 600 cached lines below the window
    expect(s.browseCacheSize()).toBe(600);

    s.applyResumeAck(
      ack({
        committed: 2300,
        serverOldest: 0,
        sentHaveThrough: 2299,
        viewportAbs: 2299,
        following: true,
      }),
    );

    // The excess band is 700..799 and the window floor is 700, so nothing is
    // eligible: the cache is exactly what the earlier fetch built.
    expect(s.browseCacheSize()).toBe(600);
    s.dropBrowseCache(-1, false);
    expect(s.getLine(700)).toBeDefined();
    expect(s.oldestIndex()).toBe(700);
  });

  it("trims the tail the retired window no longer protects", () => {
    // A predicted jump retires the window descriptor, which lowers the
    // retention bound from `max(cap, height)` to the cap for the rest of the
    // transition. The ack's own cap pass is what brings the tail back inside
    // it; without it the store keeps a screenful more than its budget until the
    // next applied line.
    const s = new LineStore();
    s.applyScreen(screenMsg(400, 1600)); // a 1600-row window at 400
    s.applyScroll(scrollMsg(0, 400, "H")); // 400 lines of history below it
    expect(heldKeys(s)).toHaveLength(2000);

    s.applyResumeAck(ack({ committed: 9000, serverOldest: 5000, sentHaveThrough: 300 }));

    expect(s.getWindow().height).toBe(0);
    expect(heldKeys(s)).toHaveLength(1808);
    expect(s.getLine(400)).toBeUndefined();
    expect(s.getLine(592)).toBeDefined();
  });

  it("leaves the window intact when the claim falls below everything held", () => {
    // The post-ED3 shape: `oldest === win.base` and the boundary claims
    // `base - 1`. The stranded band is empty, so there is no jump — and
    // retiring the descriptor anyway would drop the live screen's eviction
    // protection and its geometry for the rest of the transition.
    const s = new LineStore();
    s.applyScreen(screenMsg(700, 50));

    s.applyResumeAck(ack({ committed: 9000, serverOldest: 3000, sentHaveThrough: 699 }));

    expect(s.getWindow().base).toBe(700);
    expect(s.getWindow().height).toBe(50);
  });
});

describe("LineStore.snapshot: the bounded tail", () => {
  it("writes no more lines than the bound it was given", () => {
    // `maxLines` is a memory decision by the consumer (a repeated serialize on
    // a device already under pressure), so the walk stops at it. The depth not
    // kept is reported after hydration by the trim marker instead.
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 10, "H"));

    const snap = s.snapshot(7, 3);

    expect(snap).not.toBeNull();
    expect(snap?.lines).toHaveLength(3);
    expect(snap?.oldest).toBe(7);
    expect(snap?.highest).toBe(9);
  });
});

describe("LineStore.fromSnapshot: payloads from outside the program", () => {
  it("discards a snapshot whose run array has a hole", () => {
    // A structured-clone payload can carry a hole in an array, and the run
    // validator is the only thing between it and a store. Reading a field off
    // the hole throws, and a throw here is not a discarded snapshot — it is a
    // hydrate path that fails on every reload.
    const runs: unknown[] = [{ t: "a", f: -1, b: -1, a: 0, uc: -1 }];
    runs[1] = undefined; // the hole, after one perfectly good run
    const snap = {
      v: 1,
      serverEpoch: 0,
      oldest: 0,
      highest: 0,
      lines: [[0, runs]],
    };

    expect(LineStore.fromSnapshot(snap)).toBeNull();
  });

  it("hydrates a snapshot smaller than the cap in full", () => {
    // The restore is bounded to the store's own cap because a snapshot may
    // legitimately be larger than the cap this client runs with. The bound must
    // not bite when the payload is SMALLER: taking a tail of the difference
    // drops the oldest rows of a tail that fitted, and the hydrated store then
    // reports a trim that never happened.
    const restored = LineStore.fromSnapshot(
      {
        v: 1,
        serverEpoch: 1,
        oldest: 0,
        highest: 1,
        lines: [
          [0, row("a")],
          [1, row("b")],
        ],
      },
      3,
    );

    expect(restored).not.toBeNull();
    expect(restored?.oldestIndex()).toBe(0);
    expect(restored?.highestIndex()).toBe(1);
    expect(restored?.getLine(0)).toBeDefined();
  });

  it("claims nothing on resume when the whole hydrated tail was provisional", () => {
    // A saved floor of 0 means the saving store had confirmed none of what it
    // held, and zero is a legitimate floor — treating it as absent makes the
    // hydrated store claim its whole tail, and the server then replays nothing
    // above it, which is the frozen-composer bug across a page reload.
    const restored = LineStore.fromSnapshot({
      v: 1,
      serverEpoch: 1,
      oldest: 0,
      highest: 2,
      unconfirmedFrom: 0,
      lines: [
        [0, row("a")],
        [1, row("b")],
        [2, row("c")],
      ],
    });

    expect(restored).not.toBeNull();
    expect(restored?.highestIndex()).toBe(2);
    expect(restored?.replayBoundary()).toBe(-1);
  });
});
