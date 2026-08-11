// @vitest-environment happy-dom
//
// The renderer's DEMAND-PAGING half (docs/paged-scrollback.md §5.4-5.5): the
// fetch controller (when a request fires, where it is anchored, and the guards
// that keep it dormant) and the gap markers (a projection of the store's
// geometry, so a gap that heals from either edge updates without any caller
// knowing which edge moved).
//
// Drives the REAL render.ts under happy-dom, with the transport replaced by
// spies — this layer's job is deciding WHAT to ask for, not sending it.

import { describe, it, expect, beforeEach, vi } from "vitest";
import * as render from "./render.js";
import * as scroll from "./scroll.js";
import { PAGE_SIZE, PREFETCH_THRESHOLD } from "./store.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

const CELL_PX = 8;

function installCanvasStub(): void {
  HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
    return {
      font: "",
      measureText: (text: string): { width: number } => ({ width: text.length * CELL_PX }),
    };
  } as typeof HTMLCanvasElement.prototype.getContext;
}

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
}

function scrollMsg(firstIndex: number, count: number): ScrollMessage {
  return {
    type: "scroll",
    firstIndex,
    lines: Array.from({ length: count }, (_, i) => row(`L${firstIndex + i}`)),
  };
}

function screenMsg(base: number, height: number, opts: { altActive?: boolean } = {}): ScreenMessage {
  const rows: WireRun[][] = [];
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
    cursorHidden: true,
    cursorStyle: 0,
    cursorBlink: false,
    altActive: opts.altActive ?? false,
  };
}

interface Requests {
  calls: [number, number][];
}

let reqs: Requests;
let budget: number;
let output: HTMLElement;

function initRender(opts: { wireTransport?: boolean } = {}): void {
  document.body.innerHTML =
    `<div class="term"><div class="term-wrap"><div class="term-output"></div></div></div>`;
  const termWrap = document.querySelector<HTMLElement>(".term-wrap")!;
  output = document.querySelector<HTMLElement>(".term-output")!;
  installCanvasStub();
  render.resetScreen();
  render.init({
    output,
    termWrap,
    ...(opts.wireTransport === false
      ? {}
      : {
          requestHistory: (fromAbs: number, maxLines: number): boolean => {
            reqs.calls.push([fromAbs, maxLines]);
            return true;
          },
          historyBudget: (): number => budget,
        }),
  });
  render.updateFontMetrics();
}

/** Let the render flush run (it is rAF/timeout driven). */
async function flush(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 20));
}

const ROW_PX = 17;

/**
 * Park the reader on the row at `abs`.
 *
 * happy-dom reports `offsetTop` 0 for every element, which collapses the
 * renderer's binary search for the top visible row onto the first child — so a
 * test that wants a real reading position has to supply a layout. Derive one
 * from `data-abs` (rows are in ascending order, one row tall each) and put the
 * scroll offset on the target row.
 */
function parkViewportAt(abs: number): void {
  Object.defineProperty(HTMLElement.prototype, "offsetTop", {
    configurable: true,
    get(this: HTMLElement): number {
      const a = Number(this.dataset["abs"] ?? "");
      return Number.isFinite(a) ? a * ROW_PX : 0;
    },
  });
  vi.spyOn(scroll, "isUserScrolledUp").mockReturnValue(true);
  vi.spyOn(scroll, "currentScrollTop").mockReturnValue(abs * ROW_PX);
}

/** Undo parkViewportAt's prototype patch (mocks are restored separately). */
function unparkViewport(): void {
  delete (HTMLElement.prototype as unknown as Record<string, unknown>)["offsetTop"];
}

/** The gap markers currently in the DOM, in document order. */
function gapMarkers(): { abs: string; label: string; trimmed: boolean }[] {
  return [...output.querySelectorAll<HTMLElement>(".term-gap-marker")].map((el) => ({
    abs: el.dataset["abs"] ?? "",
    label: el.textContent ?? "",
    trimmed: el.classList.contains("term-gap-trimmed"),
  }));
}

/** The top-of-store marker's label and permanence, or null when absent. */
function topMarker(): { label: string; trimmed: boolean } | null {
  const el = output.querySelector<HTMLElement>(".term-trim-marker");
  if (el === null) {
    return null;
  }
  return { label: el.textContent ?? "", trimmed: el.classList.contains("term-gap-trimmed") };
}

describe("render: the fetch controller", () => {
  beforeEach(() => {
    reqs = { calls: [] };
    budget = PAGE_SIZE;
    vi.restoreAllMocks();
    initRender();
  });

  it("stays dormant when the consumer wired no transport", async () => {
    // A consumer on an unsupported server never passes requestHistory, and the
    // controller must then cost nothing rather than guessing.
    initRender({ wireTransport: false });
    render.handleScroll(scrollMsg(0, 20));
    render.handleScroll(scrollMsg(100, 20));
    render.handleScreen(screenMsg(120, 4));
    await flush();
    expect(reqs.calls).toEqual([]);
  });

  it("asks for nothing when the store has no absent edge near the reader", async () => {
    render.handleScroll(scrollMsg(0, 200));
    render.handleScreen(screenMsg(200, 4));
    await flush();
    expect(reqs.calls).toEqual([]);
  });

  it("fires for an interior gap the reader is approaching", async () => {
    render.handleScroll(scrollMsg(0, 20)); // [0,20)
    render.handleScroll(scrollMsg(5000, 20)); // [5000,5020): a wide hole below
    render.handleScreen(screenMsg(5020, 4));
    await flush();
    expect(reqs.calls.length).toBe(1);
    const [fromAbs, maxLines] = reqs.calls[0]!;
    // APPROACH-ANCHORED from below: the reader is at the window (5020), above
    // the gap's top edge (5000), so the page fetched is the one ENDING at the
    // gap's top — the rows the reader is about to reach.
    expect(fromAbs + maxLines).toBe(5000);
    expect(maxLines).toBe(PAGE_SIZE);
  });

  it("anchors both the length AND the offset to the CURRENT budget", async () => {
    // A shrunken length with a full-size anchor would serve a range ending a
    // page away from the reader, leaving the rows under them blank while the far
    // end filled in — the exact failure the adaptive budget would otherwise
    // introduce.
    budget = 125;
    render.handleScroll(scrollMsg(0, 20));
    render.handleScroll(scrollMsg(5000, 20));
    render.handleScreen(screenMsg(5020, 4));
    await flush();
    const [fromAbs, maxLines] = reqs.calls[0]!;
    expect(maxLines).toBe(125);
    expect(fromAbs).toBe(5000 - 125);
  });

  it("never asks below the paging floor", async () => {
    render.handleScroll(scrollMsg(0, 20));
    render.handleScroll(scrollMsg(5000, 20));
    render.handleScreen(screenMsg(5020, 4));
    // The server proved nothing at or below 4950 survives.
    render.handleHistoryReply(scrollMsg(4950, 0), 4950);
    await flush();
    for (const [fromAbs] of reqs.calls) {
      expect(fromAbs).toBeGreaterThanOrEqual(4950);
    }
    expect(reqs.calls.length).toBeGreaterThan(0);
  });

  it("does not fetch while the alternate screen is active", async () => {
    // The event paths are scrollback-UI-only, but the pending-demand retry fires
    // from a CLOCK, so this guard is the load-bearing one: without it a vim
    // session pages history nobody can see, re-arming forever.
    render.handleScroll(scrollMsg(0, 20));
    render.handleScroll(scrollMsg(5000, 20));
    render.handleScreen(screenMsg(5020, 4, { altActive: true }));
    await flush();
    reqs.calls.length = 0;
    render.maybeFetchHistory(); // the timer path, called directly
    expect(reqs.calls).toEqual([]);
  });

  it("ignores a nonsense budget instead of sending a malformed request", async () => {
    render.handleScroll(scrollMsg(0, 20));
    render.handleScroll(scrollMsg(5000, 20));
    render.handleScreen(screenMsg(5020, 4));
    await flush();
    reqs.calls.length = 0;
    for (const bad of [0, -5, 1.5, Number.NaN, Number.POSITIVE_INFINITY]) {
      budget = bad;
      render.maybeFetchHistory();
    }
    expect(reqs.calls).toEqual([]);
  });

  it("stops firing once the gap it was chasing has healed", async () => {
    render.handleScroll(scrollMsg(0, 20));
    render.handleScroll(scrollMsg(1000, 20));
    render.handleScreen(screenMsg(1020, 4));
    await flush();
    expect(reqs.calls.length).toBeGreaterThan(0);

    // Fill the whole hole: [20,1000).
    render.noteSolicited(20, 1000);
    render.handleHistoryReply(scrollMsg(20, 980), null);
    render.clearSolicited();
    await flush();
    reqs.calls.length = 0;
    render.maybeFetchHistory();
    expect(reqs.calls).toEqual([]);
  });
});

describe("render: gap markers", () => {
  beforeEach(() => {
    reqs = { calls: [] };
    budget = PAGE_SIZE;
    vi.restoreAllMocks();
    initRender();
  });

  it("marks an interior hole so two regions never read as contiguous", async () => {
    render.handleScroll(scrollMsg(0, 5));
    render.handleScroll(scrollMsg(100, 5));
    render.handleScreen(screenMsg(105, 2));
    await flush();
    const markers = gapMarkers();
    expect(markers.length).toBe(1);
    expect(markers[0]?.abs).toBe("5"); // keyed by the gap's low edge
    expect(markers[0]?.label).toBe("earlier output not loaded");
    expect(markers[0]?.trimmed).toBe(false);
  });

  it("sits between the two regions it separates", async () => {
    render.handleScroll(scrollMsg(0, 5));
    render.handleScroll(scrollMsg(100, 5));
    render.handleScreen(screenMsg(105, 2));
    await flush();
    const kids = [...output.children] as HTMLElement[];
    const markerIdx = kids.findIndex((el) => el.classList.contains("term-gap-marker"));
    expect(markerIdx).toBeGreaterThan(0);
    // Everything before it is below the gap; everything after is above.
    const before = kids.slice(0, markerIdx).filter((el) => el.dataset["abs"] !== undefined);
    const after = kids.slice(markerIdx + 1).filter((el) => el.classList.contains("term-row"));
    expect(Math.max(...before.map((el) => Number(el.dataset["abs"])))).toBeLessThan(100);
    expect(Math.min(...after.map((el) => Number(el.dataset["abs"])))).toBeGreaterThanOrEqual(100);
  });

  it("says TRIMMED once the floor proves the hole is unrecoverable", async () => {
    render.handleScroll(scrollMsg(0, 5));
    render.handleScroll(scrollMsg(100, 5));
    render.handleScreen(screenMsg(105, 2));
    await flush();
    expect(gapMarkers()[0]?.trimmed).toBe(false);

    // An empty reply for the whole hole: the server holds none of it.
    render.handleHistoryReply(scrollMsg(5, 0), 100);
    await flush();
    const markers = gapMarkers();
    expect(markers.length).toBe(1);
    expect(markers[0]?.trimmed).toBe(true);
    expect(markers[0]?.label).toBe("earlier output trimmed");
  });

  it("disappears when the gap heals, from either edge", async () => {
    render.handleScroll(scrollMsg(0, 5));
    render.handleScroll(scrollMsg(100, 5));
    render.handleScreen(screenMsg(105, 2));
    await flush();
    expect(gapMarkers().length).toBe(1);

    // Heal from the TOP edge first: the marker moves rather than vanishing,
    // which is what makes it a projection instead of a mutation log.
    render.noteSolicited(50, 100);
    render.handleHistoryReply(scrollMsg(50, 50), null);
    render.clearSolicited();
    await flush();
    const markers = gapMarkers();
    expect(markers.length).toBe(1);
    expect(markers[0]?.abs).toBe("5");

    // Fill the rest.
    render.noteSolicited(5, 50);
    render.handleHistoryReply(scrollMsg(5, 45), null);
    render.clearSolicited();
    await flush();
    expect(gapMarkers()).toEqual([]);
  });

  it("marks every hole when there are several", async () => {
    render.handleScroll(scrollMsg(0, 5));
    render.handleScroll(scrollMsg(100, 5));
    render.handleScroll(scrollMsg(200, 5));
    render.handleScreen(screenMsg(205, 2));
    await flush();
    expect(gapMarkers().map((m) => m.abs)).toEqual(["5", "105"]);
  });
});

describe("render: the resume transition", () => {
  beforeEach(() => {
    reqs = { calls: [] };
    budget = PAGE_SIZE;
    vi.restoreAllMocks();
    initRender();
  });

  it("hands the store the reader's position, not a guess", async () => {
    // The store cannot know where the reader is, and the decision it makes with
    // it (drain the reclassified band, or keep it) is the difference between a
    // reader losing their page and a following client holding stale cache.
    render.handleScroll(scrollMsg(0, 3000));
    render.handleScreen(screenMsg(3000, 4));
    await flush();
    render.applyResumeTransition({
      epochChanged: false,
      committed: 8000,
      serverOldest: 6000,
      paging: true,
      sentHaveThrough: 3003,
      sentReplayMax: null,
    });
    // A jump was predicted (the replay starts at 6000, far above 3004), so the
    // stranded band is now disposable cache rather than tail.
    expect(render.browseCacheSize()).toBeGreaterThan(0);
  });

  it("reports the replay bound the store's residency justifies", () => {
    // The client asks for no more replay than it intends to keep resident: this
    // is what keeps a phone's attach cheap on a deep server ring.
    const max = render.replayMaxForResume();
    expect(Number.isInteger(max)).toBe(true);
    expect(max).toBeGreaterThan(0);
  });

  it("drops the browse cache only when the TTL says the reader is away", async () => {
    render.handleScroll(scrollMsg(0, 100));
    render.handleScreen(screenMsg(100, 4));
    render.noteSolicited(5000, 5100);
    render.handleHistoryReply(scrollMsg(5000, 100), null);
    render.clearSolicited();
    await flush();
    expect(render.browseCacheSize()).toBe(100);

    try {
      // Visible page, reader parked on cache: the TTL is an inactivity signal
      // and the reader is looking straight at it.
      parkViewportAt(5050);
      // Guard against the fixture silently failing to move the reader, which
      // would make the skip below pass for the wrong reason.
      expect(output.querySelector('.term-row[data-abs="5050"]')).not.toBeNull();
      render.dropBrowseCache(true);
      expect(render.browseCacheSize()).toBe(100);

      // Hidden page: no reader, so it goes.
      render.dropBrowseCache(false);
      expect(render.browseCacheSize()).toBe(0);
    } finally {
      unparkViewport();
    }
  });

  it("drops a visible page's cache when the reader is not on it", async () => {
    render.handleScroll(scrollMsg(0, 100));
    render.handleScreen(screenMsg(100, 4));
    render.noteSolicited(5000, 5100);
    render.handleHistoryReply(scrollMsg(5000, 100), null);
    render.clearSolicited();
    await flush();

    try {
      parkViewportAt(50); // in the live tail, far from the cache
      render.dropBrowseCache(true);
      expect(render.browseCacheSize()).toBe(0);
    } finally {
      unparkViewport();
    }
  });

  it("timestamps browse activity so a consumer can age the cache", async () => {
    expect(render.lastBrowseActivityMs()).toBe(0);
    render.handleScroll(scrollMsg(0, 100));
    render.noteSolicited(5000, 5100);
    render.handleHistoryReply(scrollMsg(5000, 100), null);
    render.clearSolicited();
    await flush();
    expect(render.lastBrowseActivityMs()).toBeGreaterThan(0);
  });
});

describe("render: paging constants", () => {
  it("keeps the trigger threshold inside one page", () => {
    // The trigger looks PREFETCH_THRESHOLD lines ahead and asks for at most
    // PAGE_SIZE: a threshold wider than a page would arm a fetch the reply can
    // never satisfy, re-firing on every flush.
    expect(PREFETCH_THRESHOLD).toBeLessThanOrEqual(PAGE_SIZE);
  });
});


describe("render: the top-of-store marker", () => {
  // Three honest statements about the history above what is held
  // (docs/paged-scrollback.md §5.4), where there used to be one. A bounded resume
  // replay routinely lands a client above index 0 with the server still holding
  // the rest, and the single `hasTrimmedHistory()` predicate then either said
  // NOTHING — presenting a partial transcript as the beginning of the session —
  // or said "trimmed" about history the trigger was about to fetch.
  beforeEach(() => {
    reqs = { calls: [] };
    budget = PAGE_SIZE;
    vi.restoreAllMocks();
    initRender();
  });

  async function boundedResume(opts: { paging: boolean; serverOldest: number }): Promise<void> {
    // A replay that starts at 5000: the client holds [5000, 5100) and nothing below.
    render.handleScroll(scrollMsg(5000, 100));
    render.handleScreen(screenMsg(5100, 4));
    render.applyResumeTransition({
      epochChanged: false,
      committed: 5104,
      serverOldest: opts.serverOldest,
      paging: opts.paging,
      sentHaveThrough: -1,
      sentReplayMax: 2000,
    });
    await flush();
  }

  it("says NOT LOADED while the frontier is still fetchable", async () => {
    await boundedResume({ paging: true, serverOldest: 0 });
    expect(topMarker()).toEqual({ label: "earlier output not loaded", trimmed: false });
  });

  it("says TRIMMED once the floor proves nothing below survives", async () => {
    await boundedResume({ paging: true, serverOldest: 0 });
    // An empty frontier reply: the server proved it retains nothing down there.
    render.handleHistoryReply({ type: "scroll", firstIndex: 4000, lines: [] }, 5000);
    await flush();
    expect(topMarker()).toEqual({ label: "earlier output trimmed", trimmed: true });
  });

  it("says TRIMMED when the server's own retained edge is at or above the tail", async () => {
    // Reachable in ordinary steady state: a supplied cap deeper than the ring, so
    // the client accumulates more than the server keeps.
    await boundedResume({ paging: true, serverOldest: 5000 });
    expect(topMarker()).toEqual({ label: "earlier output trimmed", trimmed: true });
  });

  it("falls back to the client's own trim bookkeeping without paging", async () => {
    // No fetch exists, so the only honest statement is what this client did
    // itself. Here it evicted nothing and the server retains everything, so the
    // marker would be a lie either way.
    await boundedResume({ paging: false, serverOldest: 0 });
    expect(topMarker()).toBeNull();
  });

  it("shows nothing at all when the store holds index 0", async () => {
    render.handleScroll(scrollMsg(0, 100));
    render.handleScreen(screenMsg(100, 4));
    render.applyResumeTransition({
      epochChanged: false,
      committed: 104,
      serverOldest: 0,
      paging: true,
      sentHaveThrough: -1,
      sentReplayMax: 2000,
    });
    await flush();
    expect(topMarker()).toBeNull();
  });
});
