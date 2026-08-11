import { describe, it, expect } from "vitest";
import fc from "fast-check";
import { LineStore } from "./store.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

function rowOf(t: string): WireRun[] {
  return [{ t, f: -1, b: -1, a: 0, uc: -1 }];
}
function scrollOf(firstIndex: number, texts: string[]): ScrollMessage {
  return { type: "scroll", firstIndex, lines: texts.map(rowOf) };
}
function snap(s: LineStore): { abs: number; text: string }[] {
  const out: { abs: number; text: string }[] = [];
  s.forEachLine((abs, runs) => out.push({ abs, text: runs.map((r) => r.t).join("") }));
  return out;
}

describe("LineStore invariants (property)", () => {
  it("retained line count never exceeds the cap, for any sequence of scroll batches", () => {
    const cap = 50;
    fc.assert(
      fc.property(
        fc.array(
          fc.record({
            first: fc.nat(2000),
            texts: fc.array(fc.string({ maxLength: 8 }), { maxLength: 15 }),
          }),
          { maxLength: 40 },
        ),
        (batches) => {
          const s = new LineStore(cap);
          for (const b of batches) {
            s.applyScroll(scrollOf(b.first, b.texts));
          }
          expect(snap(s).length).toBeLessThanOrEqual(cap);
          if (s.highestIndex() >= 0) {
            expect(s.oldestIndex()).toBeGreaterThanOrEqual(0);
            expect(s.oldestIndex()).toBeLessThanOrEqual(s.highestIndex());
          }
        },
      ),
    );
  });

  it("retained count never exceeds max(cap, screen height) with interleaved screen frames", () => {
    // The scroll-only property above never exercises the live-window guard
    // (win.height stays 0), which is exactly how the R2 adversarial finding —
    // scroll bursts above a stale window escaping the cap — slipped past it.
    // This drives BOTH frame kinds, including bases that jump ahead of and
    // behind retained content, and asserts the hard bound plus the window's
    // integrity after every operation.
    const cap = 50;
    const screenOf = (base: number, height: number): ScreenMessage => ({
      type: "screen",
      base,
      rows: Array.from({ length: height }, (_, y) => rowOf(`w${String(base + y)}`)),
      changed: Array.from({ length: height }, (_, y) => y),
      cursor: [0, 0],
    });
    fc.assert(
      fc.property(
        fc.array(
          fc.oneof(
            fc.record({
              kind: fc.constant("scroll" as const),
              first: fc.nat(2000),
              texts: fc.array(fc.string({ maxLength: 8 }), { maxLength: 15 }),
            }),
            fc.record({
              kind: fc.constant("screen" as const),
              base: fc.nat(2000),
              height: fc.integer({ min: 1, max: 60 }),
            }),
          ),
          { maxLength: 40 },
        ),
        (ops) => {
          const s = new LineStore(cap);
          let winBase = -1;
          let winHeight = 0;
          for (const op of ops) {
            if (op.kind === "scroll") {
              s.applyScroll(scrollOf(op.first, op.texts));
            } else {
              s.applyScreen(screenOf(op.base, op.height));
              winBase = op.base;
              winHeight = op.height;
            }
            expect(snap(s).length).toBeLessThanOrEqual(Math.max(cap, winHeight));
          }
          // The live window is intact at the end whatever happened around it.
          if (winHeight > 0) {
            for (let y = 0; y < winHeight; y++) {
              expect(s.getLine(winBase + y)).toBeDefined();
            }
          }
        },
      ),
    );
  });

  it("re-delivering an identical batch is a no-op (idempotency: the dedup property resume relies on)", () => {
    fc.assert(
      fc.property(
        fc.nat(1000),
        fc.array(fc.string({ maxLength: 8 }), { minLength: 1, maxLength: 20 }),
        (first, texts) => {
          const s = new LineStore();
          s.applyScroll(scrollOf(first, texts));
          const after1 = snap(s);
          s.drainChanges();
          s.applyScroll(scrollOf(first, texts));
          const ch = s.drainChanges();
          expect(ch.dirtyLines).toEqual([]);
          expect(snap(s)).toEqual(after1);
        },
      ),
    );
  });

  it("every retained line holds the most recent content written to its index (model-based)", () => {
    // Model-based property (stronger than the count invariant above). The model
    // is a plain last-writer-wins dictionary from absolute index to text: it
    // knows nothing about the cap, eviction, or the oldest/highest bookkeeping,
    // so it is a genuine simplification of the store and cannot share an
    // eviction bug with it. WHICH lines survive is the eviction policy (covered
    // by the cap invariant + the unit tests); here we assert that for every line
    // the store chose to RETAIN, its content equals the last text written to
    // that index. This catches content corruption and index misalignment (an
    // off-by-one in `firstIndex + i`, a stale dedup) that a count-only invariant
    // sails past. Small index range + generous batches so lines collide and the
    // cap actually evicts.
    const cap = 40;
    fc.assert(
      fc.property(
        fc.array(
          fc.record({
            first: fc.nat(200),
            texts: fc.array(fc.string({ maxLength: 8 }), { maxLength: 30 }),
          }),
          { maxLength: 30 },
        ),
        (batches) => {
          const s = new LineStore(cap);
          const model = new Map<number, string>();
          for (const b of batches) {
            s.applyScroll(scrollOf(b.first, b.texts));
            b.texts.forEach((t, i) => model.set(b.first + i, t));
          }
          // Project the model onto exactly the indices the store retained, in
          // the store's own iteration order, and compare content. This is a
          // single assertion even for an empty store, so it never trips
          // requireAssertions.
          const retained = snap(s);
          const expected = retained.map(({ abs }) => ({ abs, text: model.get(abs) }));
          expect(retained).toEqual(expected);
        },
      ),
    );
  });
});


// The paging half (docs/paged-scrollback.md §7): the same generated-interleaving
// treatment, extended with a SOLICITED flag per batch so the guard-2 split is
// generated rather than pinned by example. Under the solicited-range doctrine an
// unsolicited line below the eviction watermark is refused and a solicited one at
// the same index is stored, so the two paths now diverge inside the same store
// and every residency invariant has to survive their interleaving.
//
// Page ranges are derived at run time as ranges strictly BELOW the retained
// frontier, which is the only shape the fetch controller produces (it fills gaps
// under the reader). That is deliberate modelling, not convenience: it is what
// makes "the top of the store is always live tail" a real invariant rather than
// an accident, and that invariant is what keeps a resume's `haveThrough` from
// naming a disposable cache line.
describe("LineStore paging invariants (property)", () => {
  const screenOf = (base: number, height: number): ScreenMessage => ({
    type: "screen",
    base,
    rows: Array.from({ length: height }, (_, y) => rowOf(`w${String(base + y)}`)),
    changed: Array.from({ length: height }, (_, y) => y),
    cursor: [0, 0],
  });

  /** Every retained index, ascending. */
  function keysOf(s: LineStore): number[] {
    return snap(s).map(({ abs }) => abs);
  }

  /**
   * The first invariant violation, or null. One string per RUN rather than an
   * expect() per invariant per operation: at 1000 runs the per-op form is >100k
   * expect() calls and lands on vitest's 2s timeout.
   */
  function residencyViolation(s: LineStore, label: string, midJump: boolean): string | null {
    const keys = keysOf(s);
    const total = keys.length;
    const browse = s.browseCacheSize();
    const win = s.getWindow();

    // 1. The bounds must not LIE. A stale `oldest` is the defect class no other
    //    invariant here can see: it silently changes which lines the eviction
    //    walk and the gap geometry consider present.
    const wantOldest = total === 0 ? -1 : keys[0]!;
    const wantHighest = total === 0 ? -1 : keys[total - 1]!;
    if (s.oldestIndex() !== wantOldest) {
      return `${label}: oldestIndex() = ${s.oldestIndex()}, but the minimum retained key is ${wantOldest}`;
    }
    if (s.highestIndex() !== wantHighest) {
      return `${label}: highestIndex() = ${s.highestIndex()}, but the maximum retained key is ${wantHighest}`;
    }
    // 2. The TAIL budget is the memory bound the design rests on: the live tail
    //    stays inside its cap (or the window, when the window is larger).
    //    Browse cache is bounded separately and may legally overshoot around
    //    the reader, so it is excluded here rather than folded in.
    const tail = total - browse;
    const tailBound = Math.max(s.tailCap(), win.height);
    if (tail > tailBound) {
      return `${label}: ${tail} live-tail lines exceed the bound ${tailBound} (cap ${s.tailCap()}, window ${win.height})`;
    }
    // 3. Browse can never exceed what is actually held.
    if (browse > total) {
      return `${label}: browse cache claims ${browse} of ${total} retained lines`;
    }
    // 4. The window is never evictable: every row of the store's CURRENT
    //    descriptor is present. (Read from the store, because a replay jump
    //    legitimately retires it.)
    for (let y = 0; y < win.height; y++) {
      if (s.getLine(win.base + y) === undefined) {
        return `${label}: window row ${win.base + y} (base ${win.base}, height ${win.height}) was evicted`;
      }
    }
    // 5. The TOP of the store is live tail, never cache. snapshot() walks down
    //    from `highest` and stops at the first browse member, so a non-null
    //    snapshot for a non-empty store is exactly that statement — and it is
    //    what keeps a resume's `haveThrough` from naming a refetchable line.
    //
    //    Scoped out of the post-jump transient, which the design names: a
    //    replay jump reclassifies the client's ENTIRE island as cache and
    //    retires the window, so between that ack and the batch's first frame
    //    there is legitimately no live tail at all (§5.2 — while the descriptor
    //    is retired no window-derived bound may be evaluated). The property
    //    found this state; scoping it is the honest reading, and the invariant
    //    is still asserted on every other operation, including the frames that
    //    land after the transition.
    if (!midJump && total > 0 && s.snapshot(1) === null) {
      return `${label}: the highest retained line is browse cache, so nothing is persistable`;
    }
    return null;
  }

  it("holds the residency invariants under interleaved frames, pages, acks and TTL drops", () => {
    fc.assert(
      fc.property(
        fc.array(
          fc.oneof(
            fc.record({
              kind: fc.constant("scroll" as const),
              count: fc.integer({ min: 0, max: 40 }),
            }),
            fc.record({
              kind: fc.constant("screen" as const),
              height: fc.integer({ min: 1, max: 30 }),
            }),
            fc.record({
              kind: fc.constant("page" as const),
              count: fc.integer({ min: 1, max: 200 }),
              gapAbove: fc.integer({ min: 0, max: 50 }),
              solicited: fc.boolean(),
            }),
            fc.record({
              kind: fc.constant("ack" as const),
              paging: fc.boolean(),
              jump: fc.boolean(),
            }),
            fc.record({ kind: fc.constant("ttl" as const), visible: fc.boolean() }),
          ),
          { maxLength: 25 },
        ),
        fc.integer({ min: 0, max: 3000 }),
        (ops, viewport) => {
          const s = new LineStore();
          let next = 0; // the next live index to append
          let violation: string | null = null;
          // Whether the store is in the post-jump transient: a jump ack has
          // reclassified everything held and retired the window, and the replay
          // batch that re-establishes a live tail has not landed yet. Any frame
          // ends it.
          let midJump = false;
          for (const op of ops) {
            switch (op.kind) {
              case "scroll": {
                s.applyScroll(scrollOf(next, Array.from({ length: op.count }, (_, i) => `L${next + i}`)));
                next += op.count;
                // An EMPTY frame is not the batch landing: it applies no line,
                // so it cannot end the post-jump transient. (The property found
                // this too — a zero-length scroll after a jump ack.)
                if (op.count > 0) {
                  midJump = false;
                }
                break;
              }
              case "screen": {
                s.applyScreen(screenOf(next, op.height));
                midJump = false;
                break;
              }
              case "page": {
                // A range strictly below the frontier, the only shape the
                // trigger produces. The SOLICITED flag is the generated half:
                // the unsolicited path is the ordinary scroll guard, which
                // refuses what it considers stale.
                const hi = s.oldestIndex() - op.gapAbove;
                const lo = Math.max(0, hi - op.count);
                if (hi <= lo) {
                  break;
                }
                const msg = scrollOf(lo, Array.from({ length: hi - lo }, (_, i) => `H${lo + i}`));
                if (op.solicited) {
                  s.noteSolicited(lo, hi);
                  s.applyHistoryScroll(msg, viewport);
                  s.clearSolicited();
                } else {
                  s.applyHistoryScroll(msg, viewport);
                }
                break;
              }
              case "ack": {
                // `jump` moves the server's oldest above what this socket says
                // it has, which is the eviction-gap shape every server produces.
                const haveThrough = s.highestIndex();
                const serverOldest = haveThrough + 100;
                s.applyResumeAck({
                  epochChanged: false,
                  committed: Math.max(next, haveThrough + 1),
                  serverOldest: op.jump ? serverOldest : 0,
                  paging: op.paging,
                  sentHaveThrough: haveThrough,
                  sentReplayMax: null,
                  viewportAbs: viewport,
                  // The renderer's answer, generated with the rest of the op: a
                  // following reader is at the tail, so the generator derives it
                  // from the viewport it chose rather than pinning one branch.
                  following: viewport >= s.highestIndex(),
                });
                // A jump is only PREDICTED when the ack declares paging and the
                // replay really starts above what this socket reported.
                const jumped = op.paging && op.jump && s.getWindow().height === 0 && s.browseCacheSize() > 0;
                midJump = midJump || jumped;
                if (jumped) {
                  // The batch that follows a jump lands ABOVE the hole — the
                  // server's replay starts at its own oldest, never inside the
                  // range the jump just condemned. Modelling that is required,
                  // not cosmetic: frames replayed at the condemned indices are a
                  // shape the server cannot emit, and the store deliberately
                  // does not reclassify a browse index back to tail (the two
                  // regions cannot overlap by construction — a fetched range is
                  // always below the window, and window bases only rise).
                  next = Math.max(next, serverOldest);
                }
                break;
              }
              case "ttl": {
                s.dropBrowseCache(viewport, op.visible);
                if (!op.visible && s.browseCacheSize() !== 0) {
                  violation ??= `ttl(hidden) left ${s.browseCacheSize()} browse lines`;
                }
                break;
              }
            }
            violation ??= residencyViolation(s, `after ${op.kind}`, midJump);
          }
          expect(violation, `ops ${JSON.stringify(ops)} viewport ${viewport}`).toBeNull();
        },
      ),
    );
  });

  it("a solicited range is admitted below the watermark and an unsolicited one is not", () => {
    // The guard-2 split itself, generated: the SAME range at the SAME index, one
    // path refused and the other stored, for any cap/overflow the generator
    // picks. The example test pins one case; this pins the doctrine.
    fc.assert(
      fc.property(
        fc.integer({ min: 10, max: 60 }),
        fc.integer({ min: 5, max: 40 }),
        fc.integer({ min: 1, max: 10 }),
        (cap, overflow, pageLen) => {
          const texts = Array.from({ length: cap + overflow }, (_, i) => `L${i}`);
          const build = (): LineStore => {
            const s = new LineStore(cap);
            s.applyScroll(scrollOf(0, texts));
            return s;
          };
          const unsolicited = build();
          const solicited = build();
          const evictedIdx = 0; // below the watermark by construction
          const page = scrollOf(evictedIdx, texts.slice(evictedIdx, evictedIdx + pageLen));

          unsolicited.applyScroll(page);
          solicited.noteSolicited(evictedIdx, evictedIdx + pageLen);
          solicited.applyHistoryScroll(page, evictedIdx);
          solicited.clearSolicited();

          expect({
            unsolicited: unsolicited.getLine(evictedIdx) !== undefined,
            solicited: solicited.getLine(evictedIdx) !== undefined,
            classified: solicited.browseCacheSize(),
          }).toEqual({ unsolicited: false, solicited: true, classified: pageLen });
        },
      ),
    );
  });
});
