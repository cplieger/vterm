// The store's two data boundaries: a persisted snapshot coming back in from
// OUTSIDE the program's memory, and the absolute index on an incoming frame.
//
// Everything the wire decoder produces is typed by construction; a snapshot
// inherits nothing, so `fromSnapshot` is the only path that admits
// externally-sourced run objects and the only place a bad field can reach the
// renderer's per-character loop. These tests drive that boundary with payloads
// a truncated write or a hand edit produces, and pin the write side of the same
// pair (what `snapshot` records about what it actually wrote).
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

/** A store holding a contiguous tail `[firstIndex, firstIndex + count)`. */
function filled(firstIndex: number, count: number): LineStore {
  const s = new LineStore();
  s.applyScroll(scrollMsg(firstIndex, count));
  return s;
}

/** A version-1 snapshot envelope around a hand-built `lines` payload. */
function envelope(lines: unknown): Record<string, unknown> {
  return { v: 1, serverEpoch: 0, oldest: 0, highest: 0, lines };
}

function textAt(s: LineStore, abs: number): string | undefined {
  const runs = s.getLine(abs);
  return runs === undefined ? undefined : runs.map((r) => r.t).join("");
}

function heldCount(s: LineStore): number {
  let n = 0;
  s.forEachLine(() => n++);
  return n;
}

describe("LineStore.fromSnapshot: the untrusted payload boundary", () => {
  it("rejects a null run instead of throwing on the field read", () => {
    // A null is the one non-object value `typeof` calls "object", so it is the
    // case that reaches the field reads below the guard rather than being
    // refused by them. Rejecting the snapshot degrades to a full resume;
    // throwing here would take the hydrate path down with it.
    expect(LineStore.fromSnapshot(envelope([[0, [null]]]))).toBeNull();
  });

  it("rejects a colour field that is not a number", () => {
    expect(LineStore.fromSnapshot(envelope([[0, [{ t: "hi", f: "red" }]]]))).toBeNull();
  });

  it("rejects a colour field that is a non-finite number", () => {
    expect(LineStore.fromSnapshot(envelope([[0, [{ t: "hi", b: Number.NaN }]]]))).toBeNull();
  });

  it("rejects an entry carrying a third element", () => {
    expect(LineStore.fromSnapshot(envelope([[0, [{ t: "hi" }], "extra"]]))).toBeNull();
  });

  it("accepts a well-formed payload, so the guards above refuse only bad ones", () => {
    const store = LineStore.fromSnapshot(envelope([[7, [{ t: "hi", f: 2 }]]]));
    expect(store?.oldestIndex()).toBe(7);
  });

  it("keeps the hydrated tail's own oldest row rewritable by live output", () => {
    // The hydrated store reports everything BELOW the tail as evicted, so the
    // staleness guard refuses a re-delivery there. The tail's oldest row is not
    // below the tail: an app rewriting it (a progress line at the top of the
    // restored screen) has to land.
    const snap = filled(100, 3).snapshot(0);
    const store = LineStore.fromSnapshot(snap);
    expect(store).not.toBeNull();
    store?.applyScroll({ type: "scroll", firstIndex: 100, lines: [row("rewritten")] });
    expect(store === null ? undefined : textAt(store, 100)).toBe("rewritten");
  });
});

describe("LineStore.snapshot: what it records about what it wrote", () => {
  it("treats a zero bound as absent and falls back to the store's own cap", () => {
    // A caller computing its bound can hand over 0. That is not "persist
    // nothing" — it is no decision at all, so the store's cap decides, exactly
    // as an omitted argument would.
    const snap = filled(0, 3).snapshot(7, 0);
    expect(snap?.lines.length).toBe(3);
  });

  it("keeps only the newest lines under a smaller explicit bound", () => {
    const snap = filled(0, 5).snapshot(7, 2);
    expect(snap?.lines.map(([abs]) => abs)).toEqual([3, 4]);
  });

  it("reports the bounds of the tail it wrote, down to index 0", () => {
    const snap = filled(0, 3).snapshot(7);
    expect(snap?.oldest).toBe(0);
    expect(snap?.highest).toBe(2);
  });
});

describe("LineStore: the absolute index on an incoming frame", () => {
  it("refuses a fractional absolute index, which no bound arithmetic could survive", () => {
    const s = new LineStore();
    s.applyScroll({ type: "scroll", firstIndex: 0.5, lines: [row("a"), row("b")] });
    expect(s.oldestIndex()).toBe(-1);
    expect(heldCount(s)).toBe(0);
  });

  it("stops reporting a line as evicted once a fetch put it back", () => {
    // A row the renderer is told is BOTH dirty and evicted loses: it drops the
    // DOM row it just built. Re-fetching an evicted index is the ordinary paging
    // path, so the two lists have to stay disjoint.
    const s = new LineStore(2);
    s.applyScroll(scrollMsg(0, 3));
    s.noteSolicited(0, 1);
    s.applyHistoryScroll(scrollMsg(0, 1), 0);
    s.clearSolicited();

    const out = s.drainChanges();
    expect(out.evictedLines).toEqual([]);
    expect(out.dirtyLines).toContain(0);
  });
});
