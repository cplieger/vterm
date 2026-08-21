// The resume replay boundary: what a client claims to already hold.
//
// The rule under test is that a row is claimable only once the SERVER has
// committed it at that absolute index. A row from a screen frame is not: it lands
// at `base + y`, which is a screen position the app repaints in place, so what the
// store holds there is the last thing drawn. Claiming it tells the server "no need
// to re-send", the server replays strictly above the claim, and the drawn copy is
// never corrected. It then surfaces as scrollback beneath the new window, which is
// how a frozen composer box ends up parked above the live region after a reattach.
//
// Every case here fails against the pre-fix store for the intended reason, except
// the two marked as invariants, which are labelled as such rather than counted as
// evidence.

import { describe, it, expect } from "vitest";
import { LineStore } from "./store.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
}

function screenMsg(base: number, height: number, altActive = false): ScreenMessage {
  const rows: WireRun[][] = [];
  const changed: number[] = [];
  for (let y = 0; y < height; y++) {
    rows.push(row(`win${String(y)}`));
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
    altActive,
  };
}

function scrollMsg(firstIndex: number, count: number, tag = "hist"): ScrollMessage {
  const lines: WireRun[][] = [];
  for (let i = 0; i < count; i++) {
    lines.push(row(`${tag}${String(firstIndex + i)}`));
  }
  return { type: "scroll", firstIndex, lines };
}

describe("replayBoundary: what the store will not ask for again", () => {
  it("stops below a live window while highestIndex stays at its bottom row", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 100));
    s.applyScreen(screenMsg(100, 24));

    expect(s.highestIndex()).toBe(123);
    // The window occupies [100, 124). None of it is claimable, so the boundary is
    // the last committed line below it. This is the whole fix in one assertion.
    expect(s.replayBoundary()).toBe(99);
  });

  it("equals highestIndex for a store fed only by durable frames", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 40));
    expect(s.replayBoundary()).toBe(s.highestIndex());
    expect(s.replayBoundary()).toBe(39);
  });

  it("reports -1 for an empty store", () => {
    const s = new LineStore();
    expect(s.replayBoundary()).toBe(-1);
    expect(s.highestIndex()).toBe(-1);
  });

  it("reports -1 when the whole store is one provisional window at base 0", () => {
    // A first attach that has seen only a screen frame can claim nothing, so it
    // asks for a full retained replay. -1 is the same value a fresh store sends,
    // which is what the connection layer already handles.
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 24));
    expect(s.highestIndex()).toBe(23);
    expect(s.replayBoundary()).toBe(-1);
  });

  it("keeps the floor across a window that slides without its scroll chunks", () => {
    // The dispatch order is screen THEN scroll, each written as its own message
    // with errors ignored, so "the base advanced but the committed copies never
    // arrived" is an ordinary truncation. The rows the window left behind are
    // still only ever drawn, so the boundary must not move up to meet the new base.
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 100));
    s.applyScreen(screenMsg(100, 24));
    expect(s.replayBoundary()).toBe(99);

    // The window slides by 10. No scroll frame follows.
    s.applyScreen(screenMsg(110, 24));
    expect(s.highestIndex()).toBe(133);
    expect(s.replayBoundary()).toBe(99);
  });

  it("raises the floor when the scroll chunks do arrive", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 100));
    s.applyScreen(screenMsg(100, 24));
    s.applyScreen(screenMsg(110, 24));
    // The rows that scrolled off, delivered as the server committed them.
    s.applyScroll(scrollMsg(100, 10));
    expect(s.replayBoundary()).toBe(109);
  });

  it("leaves the floor alone for a durable frame that does not reach it", () => {
    // A chunk landing above the floor leaves rows between the floor and the chunk
    // still provisional, so raising past it would claim exactly what this
    // mechanism exists to exclude.
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 100));
    s.applyScreen(screenMsg(100, 24));
    s.applyScroll(scrollMsg(150, 5));
    expect(s.replayBoundary()).toBe(99);
  });

  it("reads the FROZEN main base during an alternate-screen session", () => {
    // An alt frame routes to the ephemeral grid and never touches the abs map, so
    // it adds nothing provisional. The floor stays where the last main frame put
    // it, which is the region alt exit has to restore.
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 100));
    s.applyScreen(screenMsg(100, 24));
    const beforeAlt = s.replayBoundary();
    s.applyScreen(screenMsg(100, 10, true));
    expect(s.isAlt()).toBe(true);
    expect(s.replayBoundary()).toBe(beforeAlt);
    expect(s.replayBoundary()).toBe(99);
  });

  it("never exceeds highestIndex (invariant, not evidence of the fix)", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 10));
    s.applyScreen(screenMsg(10, 4));
    expect(s.replayBoundary()).toBeLessThanOrEqual(s.highestIndex());
  });

  it("returns to -1 after a reset, floor included (invariant)", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 10));
    s.applyScreen(screenMsg(10, 4));
    s.reset();
    expect(s.replayBoundary()).toBe(-1);
    // A fresh epoch shares no indices, so a surviving floor would be meaningless
    // and could hold the boundary below a whole new session's output.
    s.applyScroll(scrollMsg(0, 5));
    expect(s.replayBoundary()).toBe(4);
  });
});

describe("replayBoundary: across persistence", () => {
  it("survives a snapshot round trip, so a reload does not claim drawn rows", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 100));
    s.applyScreen(screenMsg(100, 24));
    expect(s.replayBoundary()).toBe(99);

    const snap = s.snapshot(7);
    expect(snap).not.toBeNull();
    const back = LineStore.fromSnapshot(JSON.parse(JSON.stringify(snap)) as unknown);
    expect(back).not.toBeNull();
    // Hydration has no window descriptor, so without the persisted floor the
    // reloaded store would claim its whole tail, window rows included.
    expect(back!.replayBoundary()).toBe(99);
    expect(back!.highestIndex()).toBe(123);
  });

  it("treats a snapshot without the field as all-confirmed", () => {
    // The compatibility case: an entry written before the field existed. Absent
    // means the pre-existing behaviour, so the whole tail reads as claimable.
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 20));
    s.applyScreen(screenMsg(20, 4));
    const snap = JSON.parse(JSON.stringify(s.snapshot(7))) as Record<string, unknown>;
    delete snap["unconfirmedFrom"];

    const back = LineStore.fromSnapshot(snap);
    expect(back).not.toBeNull();
    expect(back!.replayBoundary()).toBe(back!.highestIndex());
  });

  it("treats a malformed floor as absent rather than rejecting the snapshot", () => {
    // Being wrong in this direction costs a replayed screenful. Rejecting would
    // throw away a whole session's history over a field that is an optimisation of
    // correctness, not a precondition for it.
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, 20));
    s.applyScreen(screenMsg(20, 4));
    for (const bad of ["12", -1, 1.5, Number.NaN, null, {}]) {
      const snap = JSON.parse(JSON.stringify(s.snapshot(7))) as Record<string, unknown>;
      snap["unconfirmedFrom"] = bad;
      const back = LineStore.fromSnapshot(snap);
      expect(
        back,
        `snapshot with unconfirmedFrom=${JSON.stringify(bad)} must still hydrate`,
      ).not.toBeNull();
      expect(back!.replayBoundary()).toBe(back!.highestIndex());
    }
  });
});
