import { describe, it, expect } from "vitest";
import fc from "fast-check";
import { interiorGaps, retainedIntervals, type Interval } from "./intervals.js";

/** A readable `[lo,hi)` list, so a failure diff names the shape not the objects. */
function pairs(set: readonly Interval[]): [number, number][] {
  return set.map((iv) => [iv.lo, iv.hi]);
}

/** Every index the set claims, for cross-checking a set operation elementwise. */
function members(set: readonly Interval[]): number[] {
  const out: number[] = [];
  for (const iv of set) {
    for (let i = iv.lo; i < iv.hi; i++) {
      out.push(i);
    }
  }
  return out;
}

/**
 * The set invariant every operation must preserve: sorted, non-empty, and no
 * two intervals overlapping OR touching. Touching pairs are the interesting
 * half — a set holding `[0,10)` and `[10,20)` separately would show the
 * renderer a gap boundary where the store has none.
 */
function assertNormalized(set: readonly Interval[], label: string): void {
  for (const iv of set) {
    expect(iv.hi, `${label}: empty or inverted interval ${JSON.stringify(iv)}`).toBeGreaterThan(iv.lo);
  }
  for (let i = 1; i < set.length; i++) {
    const prev = set[i - 1];
    const cur = set[i];
    expect(prev, `${label}: sparse set`).toBeDefined();
    expect(cur, `${label}: sparse set`).toBeDefined();
    expect(cur!.lo, `${label}: ${JSON.stringify(set)} is unsorted or has a touching pair`).toBeGreaterThan(
      prev!.hi,
    );
  }
}

describe("intervals: retainedIntervals (the gap source)", () => {
  it("coalesces contiguous keys into runs", () => {
    expect(pairs(retainedIntervals([]))).toEqual([]);
    expect(pairs(retainedIntervals([7]))).toEqual([[7, 8]]);
    expect(pairs(retainedIntervals([3, 1, 2]))).toEqual([[1, 4]]); // unsorted input
    expect(pairs(retainedIntervals([0, 1, 2, 10, 11, 50]))).toEqual([
      [0, 3],
      [10, 12],
      [50, 51],
    ]);
  });

  it("finds a hole INSIDE the live tail", () => {
    // The case that forces gaps to derive from the key set rather than from
    // browse membership: an `unsupported` eviction-gap resume legally leaves
    // two disjoint tail runs, and a cache-membership set would draw the rows
    // either side of that hole as adjacent, with no marker.
    const keys = [100, 101, 102, 400, 401];
    const runs = retainedIntervals(keys);
    expect(pairs(runs)).toEqual([
      [100, 103],
      [400, 402],
    ]);
    expect(pairs(interiorGaps(runs))).toEqual([[103, 400]]);
  });

  it("interiorGaps excludes the frontier below the lowest key", () => {
    // The frontier's lower edge is a POLICY question (pagingFloor: what is
    // still worth asking for), so the store derives it separately; geometry
    // alone cannot answer it.
    expect(pairs(interiorGaps(retainedIntervals([500, 501])))).toEqual([]);
    expect(pairs(interiorGaps([]))).toEqual([]);
    expect(pairs(interiorGaps([{ lo: 0, hi: 10 }]))).toEqual([]);
  });

  it("interiorGaps emits no zero-width gap for a touching input", () => {
    // A normalized set never holds a touching pair, but interiorGaps is
    // exported and takes any list. A zero-width gap would reach the renderer as
    // a marker for a hole with no rows in it, and the trigger as a fetch target
    // of zero lines.
    expect(pairs(interiorGaps([{ lo: 0, hi: 10 }, { lo: 10, hi: 20 }]))).toEqual([]);
  });
});

describe("intervals: properties", () => {
  it("retainedIntervals round-trips any key set", () => {
    fc.assert(
      fc.property(fc.array(fc.nat(300), { maxLength: 60 }), (keys) => {
        const runs = retainedIntervals(keys);
        assertNormalized(runs, "retainedIntervals");
        expect(new Set(members(runs))).toEqual(new Set(keys));
      }),
    );
  });

  /**
   * Whether interiorGaps covers exactly the holes between the lowest and
   * highest key. A pure predicate, so the property below can assert ONCE per
   * run: at fast-check's 1000 runs a per-index expect() is ~300k calls, which
   * put this test on the 2s timeout and made it fail under CI load.
   */
  function gapsAreExactHoles(keys: number[]): boolean {
    const held = new Set(keys);
    const covered = new Set(members(interiorGaps(retainedIntervals(keys))));
    for (const g of covered) {
      if (held.has(g)) {
        return false; // a gap must never claim a held index
      }
    }
    if (held.size === 0) {
      return covered.size === 0;
    }
    const lo = Math.min(...held);
    const hi = Math.max(...held);
    for (let i = lo; i <= hi; i++) {
      if (!held.has(i) && !covered.has(i)) {
        return false; // a hole inside the span must fall in exactly one gap
      }
    }
    return true;
  }

  it("interiorGaps are exactly the holes between the lowest and highest key", () => {
    fc.assert(
      fc.property(fc.array(fc.nat(300), { maxLength: 60 }), (keys) => {
        expect(gapsAreExactHoles(keys), `keys ${JSON.stringify(keys)}`).toBe(true);
      }),
    );
  });
});
