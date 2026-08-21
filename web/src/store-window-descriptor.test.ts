// The store's change-tracking contract: which descriptor fields make the
// renderer repaint, what a store reports before any frame has arrived, and what
// a full reset restores.
//
// The renderer drains three booleans (`windowChanged`, `altChanged`,
// `fullReset`) plus two index lists, and does nothing it was not told about. A
// descriptor field `windowEqual` forgets to compare is therefore a caret that
// never moves, and a tracking set `reset()` forgets to clear is a DOM rebuild
// for rows the store no longer holds.
//
// Pure data structure, no DOM.

import { describe, it, expect } from "vitest";
import { LineStore } from "./store.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
}

/**
 * A screen frame whose every row is marked changed, with the cursor fields the
 * caller cares about. Omitted cursor fields are left OFF the message on
 * purpose: the store's own defaults are part of what these tests pin.
 */
function screenMsg(
  base: number,
  height: number,
  opts: Partial<{
    cursor: [number, number];
    cursorStyle: number;
    cursorHidden: boolean;
    cursorBlink: boolean;
    changed: number[];
    altActive: boolean;
  }> = {},
): ScreenMessage {
  const rows: WireRun[][] = [];
  for (let y = 0; y < height; y++) {
    rows.push(row(`W${base + y}`));
  }
  const msg: ScreenMessage = {
    type: "screen",
    base,
    rows,
    changed: opts.changed ?? rows.map((_, y) => y),
    cursor: opts.cursor ?? [0, 0],
    altActive: opts.altActive ?? false,
    scrollbackCleared: false,
  };
  if (opts.cursorStyle !== undefined) {
    msg.cursorStyle = opts.cursorStyle;
  }
  if (opts.cursorHidden !== undefined) {
    msg.cursorHidden = opts.cursorHidden;
  }
  if (opts.cursorBlink !== undefined) {
    msg.cursorBlink = opts.cursorBlink;
  }
  return msg;
}

function scrollMsg(firstIndex: number, count: number): ScrollMessage {
  const lines: WireRun[][] = [];
  for (let i = 0; i < count; i++) {
    lines.push(row(`L${firstIndex + i}`));
  }
  return { type: "scroll", firstIndex, lines };
}

describe("LineStore: the window descriptor a fresh store reports", () => {
  it("reports an empty window with a steady, visible block cursor", () => {
    const s = new LineStore();
    expect(s.getWindow()).toEqual({
      base: 0,
      height: 0,
      cursorRow: 0,
      cursorCol: 0,
      cursorStyle: 0,
      cursorHidden: false,
      cursorBlink: false,
    });
  });

  it("has nothing for the renderer to drain before the first frame", () => {
    const s = new LineStore();
    expect(s.drainChanges()).toEqual({
      dirtyLines: [],
      evictedLines: [],
      windowChanged: false,
      altChanged: false,
      fullReset: false,
    });
  });

  it("defaults an omitted cursorHidden/cursorBlink to a visible, steady cursor", () => {
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 2));
    const win = s.getWindow();
    expect(win.cursorHidden).toBe(false);
    expect(win.cursorBlink).toBe(false);
  });
});

describe("LineStore: what makes the window descriptor dirty", () => {
  it("marks the window changed on the first screen frame", () => {
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 3));
    expect(s.drainChanges().windowChanged).toBe(true);
  });

  it("clears the window and alt flags on drain, so a second drain is quiet", () => {
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 3));
    s.drainChanges();
    const second = s.drainChanges();
    expect(second.windowChanged).toBe(false);
    expect(second.altChanged).toBe(false);
  });

  it("reports no window change for a byte-identical repeated frame", () => {
    const s = new LineStore();
    s.applyScreen(screenMsg(4, 3, { cursor: [1, 2] }));
    s.drainChanges();
    s.applyScreen(screenMsg(4, 3, { cursor: [1, 2] }));
    expect(s.drainChanges().windowChanged).toBe(false);
  });

  it("marks the window changed when only the cursor ROW moved", () => {
    const s = new LineStore();
    s.applyScreen(screenMsg(4, 3, { cursor: [0, 2] }));
    s.drainChanges();
    s.applyScreen(screenMsg(4, 3, { cursor: [1, 2] }));
    const out = s.drainChanges();
    expect(out.windowChanged).toBe(true);
    expect(s.getWindow().cursorRow).toBe(1);
  });

  it("marks the window changed when only the cursor COLUMN moved", () => {
    const s = new LineStore();
    s.applyScreen(screenMsg(4, 3, { cursor: [1, 2] }));
    s.drainChanges();
    s.applyScreen(screenMsg(4, 3, { cursor: [1, 7] }));
    const out = s.drainChanges();
    expect(out.windowChanged).toBe(true);
    expect(s.getWindow().cursorCol).toBe(7);
  });

  it("marks the window changed when only the DECSCUSR style changed", () => {
    const s = new LineStore();
    s.applyScreen(screenMsg(4, 3, { cursorStyle: 0 }));
    s.drainChanges();
    s.applyScreen(screenMsg(4, 3, { cursorStyle: 4 }));
    const out = s.drainChanges();
    expect(out.windowChanged).toBe(true);
    expect(s.getWindow().cursorStyle).toBe(4);
  });
});

describe("LineStore: which content change makes a row dirty", () => {
  /** Apply one styled row at index 0 and drop the resulting change record. */
  function withRow(runs: WireRun[]): LineStore {
    const s = new LineStore();
    s.applyScroll({ type: "scroll", firstIndex: 0, lines: [runs] });
    s.drainChanges();
    return s;
  }

  function reapply(s: LineStore, runs: WireRun[]): number[] {
    s.applyScroll({ type: "scroll", firstIndex: 0, lines: [runs] });
    return s.drainChanges().dirtyLines;
  }

  it("treats a byte-identical row as a no-op, which is what re-delivery relies on", () => {
    const s = withRow([{ t: "hi", f: 1, b: 2, a: 3, uc: 4 }]);
    expect(reapply(s, [{ t: "hi", f: 1, b: 2, a: 3, uc: 4 }])).toEqual([]);
  });

  it("repaints a row whose foreground colour changed", () => {
    const s = withRow([{ t: "hi", f: 1 }]);
    expect(reapply(s, [{ t: "hi", f: 2 }])).toEqual([0]);
  });

  it("repaints a row whose background colour changed", () => {
    const s = withRow([{ t: "hi", b: 1 }]);
    expect(reapply(s, [{ t: "hi", b: 2 }])).toEqual([0]);
  });

  it("repaints a row whose style attributes changed", () => {
    const s = withRow([{ t: "hi", a: 1 }]);
    expect(reapply(s, [{ t: "hi", a: 5 }])).toEqual([0]);
  });

  it("repaints a row whose underline colour changed", () => {
    const s = withRow([{ t: "hi", uc: 1 }]);
    expect(reapply(s, [{ t: "hi", uc: 2 }])).toEqual([0]);
  });

  it("repaints a row that gained a run without changing the text it already had", () => {
    const s = withRow([{ t: "hi" }]);
    expect(reapply(s, [{ t: "hi" }, { t: "!", f: 3 }])).toEqual([0]);
    expect(s.getLine(0)?.length).toBe(2);
  });
});

describe("LineStore: what a full reset restores", () => {
  it("restores every bound it reports to a consumer", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 4));
    s.noteResumeBounds(100, 2);
    s.raisePagingFloor(3);
    s.noteSolicited(0, 2);
    s.applyHistoryScroll(scrollMsg(0, 2), 0);
    s.clearSolicited();
    expect(s.browseCacheSize()).toBe(2);

    s.reset();

    expect(s.oldestIndex()).toBe(-1);
    expect(s.highestIndex()).toBe(-1);
    expect(s.serverOldestIndex()).toBe(-1);
    expect(s.browseCacheSize()).toBe(0);
    expect(s.pagingFloorIndex()).toBe(0);
    expect(s.lastBrowseActivityMs()).toBe(0);
  });

  it("clears the pending row changes, so the renderer rebuilds no dropped row", () => {
    // Both sets have to go: after a reset the renderer wipes every row it holds,
    // so a leftover dirty index rebuilds a row the store no longer has and a
    // leftover evicted index drops a row that no longer exists.
    const s = new LineStore(2);
    s.applyScroll(scrollMsg(0, 3)); // at the cap: index 0 is evicted, 1 and 2 dirty
    s.reset();
    const out = s.drainChanges();
    expect(out.dirtyLines).toEqual([]);
    expect(out.evictedLines).toEqual([]);
  });

  it("flags the window, the alt grid and the full wipe for the renderer", () => {
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 2));
    s.drainChanges();
    s.reset();
    const out = s.drainChanges();
    expect(out.windowChanged).toBe(true);
    expect(out.altChanged).toBe(true);
    expect(out.fullReset).toBe(true);
  });

  it("keeps the declared paging capability, which describes the server not the content", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 4));
    s.applyResumeAck({
      epochChanged: false,
      committed: 4,
      serverOldest: 0,
      paging: true,
      sentHaveThrough: -1,
      sentReplayMax: null,
      viewportAbs: -1,
      following: true,
    });
    expect(s.pagingDeclared()).toBe(true);

    // reset() is public API a consumer is invited to call on alt-screen entry
    // (render.resetScreen / render.resetScrollback), nowhere near a resume. Only
    // a resumeAck can restate the capability, so clearing it here would leave
    // the client asserting "earlier output trimmed" about history the trigger
    // can still fetch, until the next reconnect.
    s.reset();

    expect(s.pagingDeclared()).toBe(true);
  });
});
