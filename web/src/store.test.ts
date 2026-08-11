// Unit tests for the absolute-index line store (brick 2). These pin the
// properties the rebuild depends on: idempotent apply (no duplicates on
// re-delivery), correct absolute-index accounting, eviction with stale-drop,
// hole-skipping iteration, and alt-screen routing. Pure data structure, no DOM.

import { describe, it, expect } from "vitest";
import { LineStore } from "./store.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
}

// scrollMsg builds a scroll/history frame starting at firstIndex.
function scrollMsg(firstIndex: number, texts: string[]): ScrollMessage {
  return { type: "scroll", firstIndex, lines: texts.map(row) };
}

// screenMsg builds a screen frame: a window of `height` rows at `base`, with
// the given rows marked changed (sparse). cursor defaults to [0,0].
function screenMsg(
  base: number,
  height: number,
  changedRows: Record<number, string>,
  opts: Partial<{ cursor: [number, number]; altActive: boolean; scrollbackCleared: boolean }> = {},
): ScreenMessage {
  const rows: WireRun[][] = new Array<WireRun[]>(height);
  const changed: number[] = [];
  for (const [k, v] of Object.entries(changedRows)) {
    const y = Number(k);
    rows[y] = row(v);
    changed.push(y);
  }
  return {
    type: "screen",
    base,
    rows,
    changed,
    cursor: opts.cursor ?? [0, 0],
    altActive: opts.altActive ?? false,
    scrollbackCleared: opts.scrollbackCleared ?? false,
  };
}

function lineTexts(store: LineStore): { abs: number; text: string }[] {
  const out: { abs: number; text: string }[] = [];
  store.forEachLine((abs, runs) => out.push({ abs, text: runs.map((r) => r.t).join("") }));
  return out;
}

describe("LineStore", () => {
  it("applies screen window rows at base + y", () => {
    const s = new LineStore();
    s.applyScreen(screenMsg(10, 3, { 0: "a", 1: "b", 2: "c" }, { cursor: [2, 1] }));
    expect(lineTexts(s)).toEqual([
      { abs: 10, text: "a" },
      { abs: 11, text: "b" },
      { abs: 12, text: "c" },
    ]);
    expect(s.highestIndex()).toBe(12);
    expect(s.oldestIndex()).toBe(10);
    const w = s.getWindow();
    expect(w.base).toBe(10);
    expect(w.height).toBe(3);
    expect(w.cursorRow).toBe(2);
    expect(w.cursorCol).toBe(1);
  });

  it("applies scroll history lines at firstIndex + i", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, ["l0", "l1", "l2"]));
    expect(lineTexts(s)).toEqual([
      { abs: 0, text: "l0" },
      { abs: 1, text: "l1" },
      { abs: 2, text: "l2" },
    ]);
  });

  it("is idempotent: re-applying identical content is a no-op (the dup-prevention property)", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, ["x", "y", "z"]));
    s.drainChanges(); // clear initial dirty
    // Re-deliver the exact same batch (simulating a fast-burst re-send or a
    // doubled frame on reconnect).
    s.applyScroll(scrollMsg(0, ["x", "y", "z"]));
    const ch = s.drainChanges();
    expect(ch.dirtyLines).toEqual([]); // nothing re-rendered
    // And each index still appears exactly once with the right content.
    expect(lineTexts(s)).toEqual([
      { abs: 0, text: "x" },
      { abs: 1, text: "y" },
      { abs: 2, text: "z" },
    ]);
  });

  it("updates a line in place when content changes (Ink rewriting a window row)", () => {
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 2, { 0: "spin -", 1: "" }));
    s.drainChanges();
    s.applyScreen(screenMsg(0, 2, { 0: "spin \\" })); // row 0 redrawn
    const ch = s.drainChanges();
    expect(ch.dirtyLines).toEqual([0]);
    expect(s.getLine(0)?.[0]?.t).toBe("spin \\");
  });

  it("evicts from the oldest end at the cap and drops stale re-sends", () => {
    const s = new LineStore(3); // tiny cap
    s.applyScroll(scrollMsg(0, ["0", "1", "2", "3", "4"])); // 5 lines, cap 3
    // Oldest two (abs 0,1) evicted; 2,3,4 retained.
    expect(lineTexts(s)).toEqual([
      { abs: 2, text: "2" },
      { abs: 3, text: "3" },
      { abs: 4, text: "4" },
    ]);
    expect(s.oldestIndex()).toBe(2);
    const ch = s.drainChanges();
    expect(ch.evictedLines.sort((a, b) => a - b)).toEqual([0, 1]);

    // A stale re-send of an evicted index is dropped (not resurrected).
    s.applyScroll(scrollMsg(0, ["0-stale"]));
    const ch2 = s.drainChanges();
    expect(ch2.dirtyLines).toEqual([]);
    expect(s.getLine(0)).toBeUndefined();
  });

  it("a window repaint below the staleness floor is applied (guard 2 exempts the live window)", () => {
    // Found by the interleaved property test while pinning the cap walk: a
    // frame stream whose base RETREATS below everEvictedThrough (malformed —
    // the real server's base is a monotonic committed counter) must degrade
    // to a DRAWN screen, not to window rows silently dropped forever. The
    // live window is the terminal's writable region and is never stale.
    const s = new LineStore(50);
    s.applyScroll(scrollMsg(0, ["a", "b", "c"]));
    // Push past the cap so rows 0..2 are evicted and marked stale.
    const tall: Record<number, string> = {};
    for (let y = 0; y < 48; y++) {
      tall[y] = `w${String(3 + y)}`;
    }
    s.applyScreen(screenMsg(3, 48, tall));
    expect(s.getLine(0)).toBeUndefined(); // 0..2 evicted (stale floor >= 2)
    // A malformed stream retreats the base to 0: the window row must paint.
    s.applyScreen(screenMsg(0, 1, { 0: "drawn" }));
    expect(s.getLine(0)?.[0]?.t).toBe("drawn");
  });

  it("evicts in batches at the cap (hysteresis), so an at-cap stream shifts the DOM top once per batch", () => {
    // Caps >= 32 evict in batches of floor(cap/16) (capped at 256): eviction
    // starts only once the cap is EXCEEDED and then frees the whole batch, so
    // the next (batch - 1) appends evict nothing. One whole-scroller content
    // shift per batch, not per line — the WebKit tiled-layer churn this
    // exists to avoid — while the retained count never exceeds the cap.
    const cap = 64; // batch = 4
    const s = new LineStore(cap);
    const texts = Array.from({ length: cap }, (_, i) => `l${String(i)}`);
    s.applyScroll(scrollMsg(0, texts)); // exactly at the cap: nothing evicts
    expect(s.drainChanges().evictedLines).toEqual([]);
    expect(s.oldestIndex()).toBe(0);

    s.applyScroll(scrollMsg(cap, ["over"])); // 65th line: one batch evicts
    const first = s.drainChanges();
    expect(first.evictedLines.sort((a, b) => a - b)).toEqual([0, 1, 2, 3]);
    expect(s.oldestIndex()).toBe(4);

    // The freed headroom absorbs the next (batch - 1) appends eviction-free.
    s.applyScroll(scrollMsg(cap + 1, ["a", "b", "c"]));
    expect(s.drainChanges().evictedLines).toEqual([]);

    // The next append past the cap evicts the next batch; the count never
    // exceeded the cap at any point in between.
    s.applyScroll(scrollMsg(cap + 4, ["d"]));
    const second = s.drainChanges();
    expect(second.evictedLines.sort((a, b) => a - b)).toEqual([4, 5, 6, 7]);
    let retained = 0;
    s.forEachLine(() => retained++);
    expect(retained).toBeLessThanOrEqual(cap);
    // Stale re-sends below the batched eviction point stay dropped.
    s.applyScroll(scrollMsg(0, ["stale"]));
    expect(s.getLine(0)).toBeUndefined();
  });

  it("batch headroom comes from history only: a batch never eats live-window rows", () => {
    // R1 adversarial finding (fable/gpt): with a valid consumer cap near the
    // terminal height (cap 64 -> batch 4, screen height 62), retained history
    // at the overflow moment (3 rows) is SMALLER than the batch. The batch
    // target must stop at the window base rather than evicting window rows —
    // an evicted window row's index lands in everEvictedThrough, after which
    // guard 2 drops every repaint of that row until the base advances (a
    // permanently blank top row on a stable-base full-screen program).
    const height = 62;
    const winRows = (base: number): Record<number, string> => {
      const out: Record<number, string> = {};
      for (let y = 0; y < height; y++) {
        out[y] = `r${String(base + y)}`;
      }
      return out;
    };
    const s = new LineStore(64);
    s.applyScreen(screenMsg(0, height, winRows(0)));
    // Scroll 3 rows into history: rows 0..2 are history, window is 3..64.
    // Size 65 > 64 -> one eviction pass. It must take ONLY the 3 history rows.
    s.applyScreen(screenMsg(3, height, winRows(3)));
    expect(s.oldestIndex()).toBe(3);
    expect(s.getLine(3)).toBeDefined();
    s.drainChanges();
    // A repaint of the window base row is APPLIED, not dropped as stale.
    s.applyScreen(screenMsg(3, height, { 0: "repainted" }));
    expect(lineTexts(s)[0]).toEqual({ abs: 3, text: "repainted" });
  });

  it("a cap at or below the screen height keeps the full screen (history budget, floored at the window)", () => {
    // gpt R1 F1: scrollbackLines is public now, so a cap smaller than the
    // terminal (8 vs a 24-row screen) is reachable. The cap governs HISTORY;
    // the live screen is never truncated by it.
    const height = 24;
    const winRows = (base: number): Record<number, string> => {
      const out: Record<number, string> = {};
      for (let y = 0; y < height; y++) {
        out[y] = `r${String(base + y)}`;
      }
      return out;
    };
    const s = new LineStore(8);
    s.applyScreen(screenMsg(0, height, winRows(0)));
    // Every window row retained despite size (24) > cap (8).
    expect(s.oldestIndex()).toBe(0);
    expect(s.highestIndex()).toBe(height - 1);
    expect(lineTexts(s).length).toBe(height);
    // The window slides: rows 0..1 become history, and only they are evictable.
    s.applyScreen(screenMsg(2, height, winRows(2)));
    expect(s.oldestIndex()).toBe(2); // history evicted, window 2..25 intact
    expect(lineTexts(s).length).toBe(height);
    expect(s.getLine(2)).toBeDefined();
  });

  it("an alt-screen frame does not disturb the main window's retention floor", () => {
    // R4 (all three models): updateWindowCursor used to copy the ALT grid's
    // height into win.height, so a smaller alt frame (12 rows over a 24-row
    // main screen) lowered the advertised bound below the protected main rows
    // and pointed the guard-2 exemption at a phantom range. The alt grid's
    // height lives in altRows; win keeps the MAIN screen's geometry — the
    // region alt exit must restore.
    const height = 24;
    const winRows = (base: number): Record<number, string> => {
      const out: Record<number, string> = {};
      for (let y = 0; y < height; y++) {
        out[y] = `r${String(base + y)}`;
      }
      return out;
    };
    const s = new LineStore(8);
    s.applyScreen(screenMsg(0, height, winRows(0)));
    expect(lineTexts(s).length).toBe(height); // floored at the live screen
    // Enter a SMALLER alt screen (vim opened in a shorter logical grid).
    s.applyScreen(screenMsg(0, 12, { 0: "alt row" }, { altActive: true }));
    expect(s.isAlt()).toBe(true);
    expect(lineTexts(s).length).toBe(height); // main rows untouched
    // The separating observable (R5 review: without it this test passed under
    // the reverted fix): the window descriptor still reports the MAIN screen's
    // geometry during alt — the retention floor and the guard-2 exemption are
    // defined over it, and it is what alt exit restores. The alt grid's own
    // height lives in the altRows.
    expect(s.getWindow().height).toBe(height);
    expect(s.getAltRows().length).toBe(12);
    // Exit alt: the full main screen is still there to restore.
    s.applyScreen(screenMsg(0, height, {}));
    expect(s.isAlt()).toBe(false);
    expect(lineTexts(s).length).toBe(height);
    expect(s.getLine(0)).toBeDefined();
  });

  it("a scroll burst above a stale window cannot escape the cap (the resume-replay shape)", () => {
    // R2 adversarial finding (claude): the window guard stopped the eviction
    // LOOP once the oldest key reached the window base, but applyScroll does
    // not restore the tail invariant (only a screen frame runs
    // truncateBelowWindow), so history committed ABOVE the window — a resume
    // replay delivered before its window frame, or a malformed stream —
    // escaped the cap entirely (measured: 20k lines retained at cap 64).
    // The skip-over-window walk bounds every apply at max(cap, window height).
    const height = 24;
    const winRows = (base: number): Record<number, string> => {
      const out: Record<number, string> = {};
      for (let y = 0; y < height; y++) {
        out[y] = `r${String(base + y)}`;
      }
      return out;
    };
    const cap = 64;
    const s = new LineStore(cap);
    // History 60..99 below a window at 100..123 — an at-cap steady state.
    s.applyScroll(
      scrollMsg(
        60,
        Array.from({ length: 40 }, (_, i) => `h${String(60 + i)}`),
      ),
    );
    s.applyScreen(screenMsg(100, height, winRows(100)));
    // A 400-line replay lands ABOVE the stale window bottom, no screen frame.
    s.applyScroll(
      scrollMsg(
        124,
        Array.from({ length: 400 }, (_, i) => `r${String(124 + i)}`),
      ),
    );
    let retained = 0;
    s.forEachLine(() => retained++);
    expect(retained).toBeLessThanOrEqual(Math.max(cap, height));
    // The window survived the burst untouched...
    for (let y = 0; y < height; y++) {
      expect(s.getLine(100 + y)).toBeDefined();
    }
    // ...the surviving band is the NEWEST replay lines (evicted oldest-first;
    // the ones adjacent to the incoming window), so once the window frame
    // advances past the seam the retained set converges back to one
    // contiguous block. (R3 finding: a newest-first trim instead parked a
    // permanent interior hole that resume — which replays only above
    // haveThrough — could never backfill.)
    expect(s.getLine(523)).toBeDefined();
    expect(s.getLine(124)).toBeUndefined();
    // ...and was NOT poisoned as stale: a repaint of the window base row is
    // applied (the live window is exempt from the staleness guard).
    s.drainChanges();
    s.applyScreen(screenMsg(100, height, { 0: "repainted" }));
    expect(s.getLine(100)?.[0]?.t).toBe("repainted");
    // That screen frame also restored the tail invariant: the whole
    // above-window band went with it (truncateBelowWindow), leaving exactly
    // the window — the store healed back under the plain cap.
    retained = 0;
    s.forEachLine(() => retained++);
    expect(retained).toBe(height);
  });

  it("converges to one contiguous block after the window advances past a replay seam", () => {
    // R3/R4 adversarial rounds: eviction direction inside the above-window
    // band is load-bearing. Oldest-first keeps the NEWEST replay lines
    // adjacent to the incoming window, so once the window frame lands at the
    // replay head and output streams on, the stale rows and the seam evict
    // away and the retained set is contiguous again. (A newest-first trim
    // instead parked a permanent interior hole that resume — which replays
    // only above haveThrough — could never backfill.)
    const height = 24;
    const winRows = (base: number): Record<number, string> => {
      const out: Record<number, string> = {};
      for (let y = 0; y < height; y++) {
        out[y] = `r${String(base + y)}`;
      }
      return out;
    };
    const cap = 64;
    const s = new LineStore(cap);
    s.applyScroll(
      scrollMsg(
        60,
        Array.from({ length: 40 }, (_, i) => `h${String(60 + i)}`),
      ),
    );
    s.applyScreen(screenMsg(100, height, winRows(100)));
    s.applyScroll(
      scrollMsg(
        124,
        Array.from({ length: 400 }, (_, i) => `r${String(124 + i)}`),
      ),
    );
    // The real window frame lands at the replay head, then output streams on
    // (each frame slides the window down one row, the protocol shape).
    for (let i = 0; i <= 40; i++) {
      s.applyScreen(screenMsg(500 + i, height, winRows(500 + i)));
    }
    // The stale window rows and the seam have evicted away oldest-first:
    // what remains is one contiguous block ending at the live window bottom.
    const abses: number[] = [];
    s.forEachLine((abs) => abses.push(abs));
    expect(abses.length).toBeLessThanOrEqual(cap);
    expect(abses[abses.length - 1]).toBe(540 + height - 1);
    for (let i = 1; i < abses.length; i++) {
      expect(abses[i]! - abses[i - 1]!).toBe(1); // no interior gaps
    }
  });

  it("advances oldest across a large index gap when evicting at the cap (bounded scan, no integer walk)", () => {
    // A compromised or malformed server frame can deliver content at an
    // absolute index far above the retained low index. When the cap then
    // forces eviction of that low line, advancing `oldest` must scan the small
    // retained key set, never walk the integer gap to the next index -- a naive
    // `while (!has(oldest)) oldest++` fallback would iterate ~1e9 times here and
    // freeze the tab (an algorithmic-complexity DoS). The bounded key-scan lands
    // oldest on the surviving far block immediately.
    const s = new LineStore(3); // tiny cap
    s.applyScroll(scrollMsg(0, ["low"]));
    const far = 1_000_000_000;
    s.applyScroll(scrollMsg(far, ["a", "b", "c"])); // 4 lines, cap 3 -> evict abs 0
    expect(s.getLine(0)).toBeUndefined();
    expect(s.oldestIndex()).toBe(far);
    expect(s.highestIndex()).toBe(far + 2);
    expect(lineTexts(s)).toEqual([
      { abs: far, text: "a" },
      { abs: far + 1, text: "b" },
      { abs: far + 2, text: "c" },
    ]);
  });

  it("forEachLine iterates retained keys across a huge index gap without an integer walk", () => {
    // The absolute-index DoS guard (bounded key-scan, not a walk over the
    // integer range [oldest, highest]) is applied at four sites in store.ts;
    // only enforceCap's is regression-pinned. forEachLine is the hottest of
    // the four -- the renderer calls it every frame to order DOM rows -- yet
    // its only gap test ("skips holes") uses a 3-index gap that a naive
    // range walk would survive. With both a low block and a far block
    // retained (no cap eviction), a range-walk regression loops ~1e9 times
    // and times out; the bounded key-scan yields the four lines in
    // ascending order instantly.
    const s = new LineStore(1000); // cap high enough that nothing is evicted
    s.applyScroll(scrollMsg(0, ["a", "b"]));
    const far = 1_000_000_000;
    s.applyScroll(scrollMsg(far, ["y", "z"]));
    expect(lineTexts(s)).toEqual([
      { abs: 0, text: "a" },
      { abs: 1, text: "b" },
      { abs: far, text: "y" },
      { abs: far + 1, text: "z" },
    ]);
    expect(s.oldestIndex()).toBe(0);
    expect(s.highestIndex()).toBe(far + 1);
  });

  it("skips holes when iterating (trimmed-history gap shows as a jump in abs)", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, ["a", "b"]));
    // Jump to abs 5 (3,4 missing — e.g. an eviction-gap resume).
    s.applyScroll(scrollMsg(5, ["f", "g"]));
    expect(lineTexts(s)).toEqual([
      { abs: 0, text: "a" },
      { abs: 1, text: "b" },
      { abs: 5, text: "f" },
      { abs: 6, text: "g" },
    ]);
    expect(s.highestIndex()).toBe(6);
  });

  it("highestIndex is -1 when empty (resume haveThrough cold-start signal)", () => {
    const s = new LineStore();
    expect(s.highestIndex()).toBe(-1);
    s.applyScroll(scrollMsg(0, ["a"]));
    expect(s.highestIndex()).toBe(0);
  });

  it("reset clears everything and flags a full reset for the renderer", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, ["a", "b", "c"]));
    s.drainChanges();
    s.reset();
    expect(s.highestIndex()).toBe(-1);
    expect(lineTexts(s)).toEqual([]);
    const ch = s.drainChanges();
    expect(ch.fullReset).toBe(true);
    // After a reset, index 0 is valid again (new server boot).
    s.applyScroll(scrollMsg(0, ["fresh"]));
    expect(s.getLine(0)?.[0]?.t).toBe("fresh");
  });

  it("routes alt-screen frames to an ephemeral grid without touching the abs store", () => {
    const s = new LineStore();
    s.applyScroll(scrollMsg(0, ["history0", "history1"]));
    // Establish the MAIN window first (base 2), as every live session has
    // before an app enters alt — it is the frozen region the alt gate
    // protects.
    s.applyScreen(screenMsg(2, 2, { 0: "main 0", 1: "main 1" }));
    s.drainChanges();
    // Enter alt screen (e.g. vim): a 2-row ephemeral grid.
    s.applyScreen(screenMsg(2, 2, { 0: "~ alt 0", 1: "~ alt 1" }, { altActive: true }));
    expect(s.isAlt()).toBe(true);
    expect(s.getAltRows().map((r) => r.map((x) => x.t).join(""))).toEqual(["~ alt 0", "~ alt 1"]);
    // The history buffer is untouched while in alt.
    expect(lineTexts(s)).toEqual([
      { abs: 0, text: "history0" },
      { abs: 1, text: "history1" },
      { abs: 2, text: "main 0" },
      { abs: 3, text: "main 1" },
    ]);
    // A scroll frame AT or ABOVE the frozen main base during alt is dropped
    // (protocol invariant: the server never emits live scroll during alt,
    // and the frozen window region must not be rewritten under vim)...
    s.applyScroll(scrollMsg(4, ["should-not-apply"]));
    expect(s.getLine(4)).toBeUndefined();
    // ...but history strictly below it applies even during alt (2026-08 — a
    // resume replay or a post-resume durable chunk racing an alt flip must
    // not be lost; the write-ordering race's last residual). Alt paints from
    // the ephemeral grid, so the update surfaces at alt exit's rebuild.
    s.applyScroll(scrollMsg(0, ["history0+", "history1+"]));
    expect(s.getLine(0)?.[0]?.t).toBe("history0+");
    expect(s.getLine(1)?.[0]?.t).toBe("history1+");
    // Exit alt: grid cleared, history intact.
    s.applyScreen(screenMsg(2, 2, { 0: "history0", 1: "history1" }));
    expect(s.isAlt()).toBe(false);
    expect(s.getAltRows()).toEqual([]);
  });

  it("accepts alt-time history when no main window was ever seen (fresh attach into alt)", () => {
    // A fresh tab attaching straight into an in-alt session has no frozen
    // main window (the resume replay lands pre-flip on the normal path; this
    // covers a post-batch stripped durable chunk arriving after the flip).
    // With no window there is no region to protect, so the chunk stores
    // exactly as it would on the main path and surfaces at alt exit.
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 2, { 0: "~ alt 0", 1: "~ alt 1" }, { altActive: true }));
    expect(s.isAlt()).toBe(true);
    s.applyScroll(scrollMsg(0, ["late0"]));
    expect(s.getLine(0)?.[0]?.t).toBe("late0");
  });

  it("rejects invalid indices", () => {
    const s = new LineStore();
    // Negative and non-integer indices via a hand-built scroll frame.
    s.applyScroll({ type: "scroll", firstIndex: -5, lines: [row("neg")] });
    expect(s.highestIndex()).toBe(-1); // -5 rejected
  });

  it("drops a malformed line whose runs are not an array (apply-line guard 3)", () => {
    const s = new LineStore();
    // A malformed scroll frame (reachable via the JSON text-frame path, which
    // is parsed without structural validation) carries a line payload that is
    // not a WireRun array. Guard 3 must drop it rather than store a corrupt row.
    s.applyScroll({
      type: "scroll",
      firstIndex: 0,
      lines: ["not-a-run-array" as unknown as WireRun[]],
    });
    expect(s.highestIndex()).toBe(-1);
    expect(s.getLine(0)).toBeUndefined();
  });

  it("reports trimmed history from a client-side eviction (resync guard 8.2.2)", () => {
    const s = new LineStore(3); // tiny cap to force eviction
    s.applyScroll(scrollMsg(0, ["a", "b", "c", "d", "e"])); // evicts 0,1
    expect(s.oldestIndex()).toBeGreaterThan(0);
    expect(s.hasTrimmedHistory()).toBe(true);
  });

  it("reports trimmed history when the server retains less than the client asks for", () => {
    const s = new LineStore();
    // Fresh client; nothing evicted locally.
    expect(s.hasTrimmedHistory()).toBe(false);
    // Resume: the server's oldest retained line is 100 and it replays from there.
    s.noteResumeBounds(150, 100);
    s.applyScroll(scrollMsg(100, ["x", "y"]));
    // The client cannot show lines 0..99 — they were trimmed server-side.
    expect(s.hasTrimmedHistory()).toBe(true);
    // A later resume where the server still has everything clears the flag.
    s.noteResumeBounds(150, 0);
    expect(s.hasTrimmedHistory()).toBe(false);
  });

  it("ignores invalid resume bounds", () => {
    const s = new LineStore();
    s.noteResumeBounds(10, -1); // negative oldest ignored
    s.applyScroll(scrollMsg(5, ["a"]));
    expect(s.hasTrimmedHistory()).toBe(false);
  });

  it("evicts rows stranded below the window when the screen shrinks", () => {
    const s = new LineStore();
    // Tall screen: 5 rows, abs 0-4 all filled.
    s.applyScreen(screenMsg(0, 5, { 0: "a", 1: "b", 2: "c", 3: "d", 4: "e" }));
    expect(s.highestIndex()).toBe(4);
    s.drainChanges();
    // Terminal resized shorter: window is now 3 rows [0..2]; abs 3-4 are stranded
    // below the live window (the phantom-blank-tail bug) and must be evicted.
    s.applyScreen(screenMsg(0, 3, { 0: "a", 1: "b", 2: "c" }));
    expect(s.highestIndex()).toBe(2);
    expect(s.getLine(3)).toBeUndefined();
    expect(s.getLine(4)).toBeUndefined();
    const ch = s.drainChanges();
    expect(ch.evictedLines).toContain(3);
    expect(ch.evictedLines).toContain(4);
    expect(lineTexts(s)).toEqual([
      { abs: 0, text: "a" },
      { abs: 1, text: "b" },
      { abs: 2, text: "c" },
    ]);
  });

  it("keeps history above the window but drops phantom rows below it on shrink", () => {
    const s = new LineStore();
    // Tall screen filling abs 0-5.
    s.applyScreen(
      screenMsg(0, 6, { 0: "banner", 1: "b1", 2: "b2", 3: "b3", 4: "in", 5: "border" }),
    );
    expect(s.highestIndex()).toBe(5);
    s.drainChanges();
    // Shrink: window scrolls to base 2, height 3 -> [2..4]. abs 5 is stranded
    // below the window; abs 0-1 remain as history above it.
    s.applyScreen(screenMsg(2, 3, { 0: "b2", 1: "b3", 2: "in" }));
    expect(s.highestIndex()).toBe(4); // window bottom, not the stale 5
    expect(s.oldestIndex()).toBe(0); // history above the window is retained
    expect(s.getLine(5)).toBeUndefined();
  });

  it("does not evict on a normal same-height redraw (no spurious tail eviction)", () => {
    const s = new LineStore();
    s.applyScreen(screenMsg(0, 3, { 0: "a", 1: "b", 2: "c" }));
    s.drainChanges();
    // Same window, one row changes: nothing below the window bottom to evict.
    s.applyScreen(screenMsg(0, 3, { 2: "C" }));
    expect(s.highestIndex()).toBe(2);
    const ch = s.drainChanges();
    expect(ch.evictedLines).toEqual([]);
  });

  it("drops scrollback history below base when a frame sets scrollbackCleared (ED3)", () => {
    const s = new LineStore();
    // History (abs 0-4) plus a live window at base 5 (abs 5-7).
    s.applyScroll(scrollMsg(0, ["h0", "h1", "h2", "h3", "h4"]));
    s.applyScreen(screenMsg(5, 3, { 0: "a", 1: "b", 2: "c" }));
    expect(s.oldestIndex()).toBe(0);
    expect(s.highestIndex()).toBe(7);
    s.drainChanges();
    // ED3 repaint: same window at base 5 with scrollbackCleared — the history
    // (abs 0-4) is dropped, the window (abs 5-7) is kept and refreshed.
    s.applyScreen(screenMsg(5, 3, { 0: "A", 1: "b", 2: "c" }, { scrollbackCleared: true }));
    expect(s.oldestIndex()).toBe(5);
    expect(s.highestIndex()).toBe(7);
    expect(s.getLine(0)).toBeUndefined();
    expect(s.getLine(4)).toBeUndefined();
    const ch = s.drainChanges();
    expect(ch.evictedLines).toContain(0);
    expect(ch.evictedLines).toContain(4);
    expect(lineTexts(s)).toEqual([
      { abs: 5, text: "A" },
      { abs: 6, text: "b" },
      { abs: 7, text: "c" },
    ]);
  });
});

// --- Persistence: snapshot / fromSnapshot -----------------------------------
//
// The store can be serialized as plain data so a consumer may keep scrollback
// across a page discard (on iOS, a backgrounded tab being evicted is routine,
// and a reloaded page otherwise resumes with haveThrough = -1 and pulls the
// server's whole ring).
//
// The load-bearing part is what a snapshot deliberately does NOT carry, and the
// epoch it does; see StoreSnapshot's doc comment.

describe("LineStore persistence", () => {
  function seeded(n: number, from = 0): LineStore {
    const store = new LineStore();
    store.applyScroll(
      scrollMsg(
        from,
        Array.from({ length: n }, (_, i) => `L${from + i}`),
      ),
    );
    return store;
  }

  it("round-trips lines at their absolute indices, preserving every run field", () => {
    const store = new LineStore();
    const fancy: WireRun[] = [
      { t: "styled", f: -1, b: 4, uc: -1, a: 1 | 4, u: "https://example.test/x" },
      { t: "plain" },
    ];
    store.applyScroll({ type: "scroll", firstIndex: 7, lines: [fancy], inputAck: 0 });

    const snap = store.snapshot(1234);
    expect(snap).not.toBeNull();
    // Survives a structured clone, which is what an IndexedDB write does: no
    // class instances, no functions, no cycles.
    const cloned = structuredClone(snap!);
    const back = LineStore.fromSnapshot(cloned);
    expect(back).not.toBeNull();
    expect(back!.getLine(7)).toEqual(fancy);
    expect(back!.oldestIndex()).toBe(7);
    expect(back!.highestIndex()).toBe(7);
  });

  it("reports the depth a bounded tail does not have, so the trim marker is honest", () => {
    const store = seeded(10);
    const snap = store.snapshot(0, 4); // keep only the newest 4 of 10
    expect(snap!.lines.length).toBe(4);
    expect(snap!.oldest).toBe(6);
    expect(snap!.highest).toBe(9);

    const back = LineStore.fromSnapshot(snap)!;
    expect(back.oldestIndex()).toBe(6);
    // everEvictedThrough = oldest - 1, so the store knows history is missing
    // below the tail and the renderer shows "earlier output trimmed" rather than
    // implying the buffer is complete.
    expect(back.hasTrimmedHistory()).toBe(true);
  });

  it("hydrates every line as dirty so the renderer actually paints them", () => {
    const back = LineStore.fromSnapshot(seeded(3).snapshot(0))!;
    const changes = back.drainChanges();
    expect(changes.dirtyLines.sort((a, b) => a - b)).toEqual([0, 1, 2]);
  });

  it("carries the server boot epoch, which is what makes hydrating safe at all", () => {
    // Absolute indices are only meaningful within one server process. Without
    // this field a consumer cannot tell the connection layer which epoch its
    // restored content belongs to, and a hydrate across a server restart would
    // present stale content as live AND then have the new session's low-index
    // output refused by the staleness guard.
    expect(seeded(2).snapshot(987654321)!.serverEpoch).toBe(987654321);
    expect(seeded(2).snapshot(Number.NaN)!.serverEpoch).toBe(0);
  });

  it("refuses to snapshot an empty store, so it cannot overwrite a good one", () => {
    expect(new LineStore().snapshot(1)).toBeNull();
  });

  it("does not persist the window, the server bound, or the alternate screen", () => {
    const store = new LineStore();
    store.applyScreen(screenMsg(0, 2, { 0: "a", 1: "b" }));
    store.noteResumeBounds(50, 10);
    const snap = store.snapshot(1)!;
    // Only the documented keys. A future field must be added to StoreSnapshot
    // deliberately, with its own reasoning, rather than leaking in.
    expect(Object.keys(snap).sort()).toEqual(["highest", "lines", "oldest", "serverEpoch", "v"]);
  });

  it("discards a snapshot of a different shape version", () => {
    const snap = seeded(3).snapshot(1)!;
    expect(LineStore.fromSnapshot({ ...snap, v: snap.v + 1 })).toBeNull();
  });

  it("discards malformed snapshots instead of half-restoring one", () => {
    const good = seeded(3).snapshot(1)!;
    const cases: unknown[] = [
      null,
      undefined,
      "nope",
      {},
      { ...good, lines: [] },
      { ...good, lines: "nope" },
      { ...good, lines: [[0]] }, // pair too short
      { ...good, lines: [["0", [{ t: "x" }]]] }, // non-numeric index
      { ...good, lines: [[1.5, [{ t: "x" }]]] }, // non-integer index
      { ...good, lines: [[-1, [{ t: "x" }]]] }, // negative index
      { ...good, lines: [[0, "nope"]] }, // runs not an array
      {
        ...good,
        lines: [
          [5, [{ t: "x" }]],
          [3, [{ t: "y" }]],
        ],
      }, // not ascending
      // The run CONTENTS, which is the case the wire path cannot produce and a
      // snapshot can: a run reaching the renderer with a non-string `t` throws in
      // its per-character loop, the row stays queued, and every row above it is
      // blocked — permanently, because a hydrated store is never dropped.
      { ...good, lines: [[0, [null]]] },
      { ...good, lines: [[0, [42]]] },
      { ...good, lines: [[0, ["plain string"]]] },
      { ...good, lines: [[0, [{ f: 1 }]]] }, // no `t` at all
      { ...good, lines: [[0, [{ t: 5 }]]] },
      { ...good, lines: [[0, [{ t: "x", f: "red" }]]] },
      { ...good, lines: [[0, [{ t: "x", a: Number.NaN }]]] },
      { ...good, lines: [[0, [{ t: "x", u: 7 }]]] },
      {
        ...good,
        lines: [
          [5, [{ t: "x" }]],
          [5, [{ t: "y" }]],
        ],
      }, // duplicated index
    ];
    for (const bad of cases) {
      expect(LineStore.fromSnapshot(bad)).toBeNull();
    }
  });

  it("keeps applying new lines above a hydrated tail", () => {
    // The restored store has to be a working store, not just a rendered one:
    // the session continues at indices above the tail.
    const back = LineStore.fromSnapshot(seeded(4).snapshot(0))!;
    back.applyScroll(scrollMsg(4, ["L4"]));
    expect(back.highestIndex()).toBe(4);
    expect(lineTexts(back).map((l) => l.text)).toEqual(["L0", "L1", "L2", "L3", "L4"]);
  });

  it("refuses a line below the hydrated tail, which is the stale-index guard", () => {
    // A bounded tail means everything below it was deliberately dropped. A late
    // re-delivery of one of those lines must not reappear underneath the
    // restored content (apply-line guard 2, via everEvictedThrough).
    const back = LineStore.fromSnapshot(seeded(10).snapshot(0, 4))!;
    expect(back.oldestIndex()).toBe(6);
    back.applyScroll(scrollMsg(2, ["late"]));
    expect(back.getLine(2)).toBeUndefined();
    expect(back.oldestIndex()).toBe(6);
  });

  it("still paints the screen when the window arrives BELOW the hydrated tail", () => {
    // The counterpart to the guard above, and the case that protects this feature's
    // own stated worst case rather than its nice-to-have. A hydrated store starts
    // with a HIGH everEvictedThrough (oldest - 1), so if a window frame then arrives
    // at a low base without a restart having been detected — an index space
    // recreated in place, a server downgraded to reporting no epoch — apply-line
    // guard 2 would refuse every window row and the terminal would be wrong and
    // then permanently blank, which is precisely the failure the persisted epoch
    // exists to avoid. Guard 2's live-window exemption is what makes it degrade to
    // "working screen, no scrollback for a while" instead.
    //
    // The exemption belongs to the store's eviction/window rules, not to
    // persistence, but only persistence can CREATE a store whose everEvictedThrough
    // is high at construction — so the test belongs here, where it will fail if
    // that exemption is ever narrowed.
    const back = LineStore.fromSnapshot(seeded(1000, 3200).snapshot(1))!;
    expect(back.oldestIndex()).toBe(3200);
    expect(back.hasTrimmedHistory()).toBe(true);

    back.applyScreen(screenMsg(0, 2, { 0: "after-restart-0", 1: "after-restart-1" }));

    expect(back.getLine(0)?.[0]?.t).toBe("after-restart-0");
    expect(back.getLine(1)?.[0]?.t).toBe("after-restart-1");
  });

  it("honors a smaller retention cap on the hydrated store", () => {
    const back = LineStore.fromSnapshot(seeded(6).snapshot(0), 3);
    expect(back).not.toBeNull();
    // Appending past the cap evicts from the top rather than growing forever.
    back!.applyScroll(scrollMsg(6, ["L6", "L7", "L8", "L9"]));
    expect(back!.highestIndex()).toBe(9);
    expect(lineTexts(back!).length).toBeLessThanOrEqual(6);
  });

  it("accepts every field a real run carries, so a valid snapshot is not rejected", () => {
    // The other half of the run-contents check: it must not be so strict that the
    // engine's own output fails it. A run with every optional field, and one with
    // none beyond `t`, both have to survive.
    const store = new LineStore();
    store.applyScroll({
      type: "scroll",
      firstIndex: 0,
      lines: [
        [{ t: "full", f: -1, b: 4, uc: -1, a: 1 | 4, u: "https://example.test/x" }],
        [{ t: "bare" } as WireRun],
      ],
      inputAck: 0,
    });
    const back = LineStore.fromSnapshot(store.snapshot(1));
    expect(back).not.toBeNull();
    expect(back!.getLine(1)).toEqual([{ t: "bare" }]);
  });

  it("gives the hydrated store the SAME cap it trimmed with", () => {
    // An invalid cap has to fall back for both uses or they disagree: passing the
    // raw value to the constructor while trimming with the default produced a
    // store that kept 5000 lines and then evicted almost all of them on the next
    // append.
    for (const bad of [0, -1, 1.5, Number.NaN]) {
      const back = LineStore.fromSnapshot(seeded(6).snapshot(0), bad);
      expect(back).not.toBeNull();
      // Appending must not collapse the buffer: the default cap is in force.
      back!.applyScroll(scrollMsg(6, ["L6", "L7"]));
      expect(back!.highestIndex()).toBe(7);
      expect(lineTexts(back!).length).toBe(8);
    }
  });

  it("trims a snapshot larger than the cap instead of hydrating over budget", () => {
    // The cap is a memory budget — the renderer builds one DOM row per retained
    // line — and the same snapshot outlives a consumer lowering it, so a restore
    // must respect the cap on the way in rather than waiting for eviction to
    // claw it back after the rows are already built.
    const back = LineStore.fromSnapshot(seeded(20).snapshot(0), 8)!;
    expect(back.oldestIndex()).toBe(12);
    expect(back.highestIndex()).toBe(19);
    expect(lineTexts(back).length).toBe(8);
    // And it says so, rather than implying the buffer is complete.
    expect(back.hasTrimmedHistory()).toBe(true);
  });

  it("validates the whole payload before trimming, so a corrupt head still rejects", () => {
    // Trimming first would let a snapshot whose head is garbage restore its tail
    // as though nothing were wrong.
    const good = seeded(20).snapshot(0)!;
    const corruptHead: [number, WireRun[]][] = [...good.lines];
    corruptHead[0] = ["nope" as unknown as number, []];
    expect(LineStore.fromSnapshot({ ...good, lines: corruptHead }, 8)).toBeNull();
  });
});
