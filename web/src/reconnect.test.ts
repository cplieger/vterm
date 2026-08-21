// Tests for the WebSocket reconnect-backoff scheduler.
//
// Behaviors tested:
// 1. The base-delay ladder: the base doubles each attempt and saturates at
//    MAX_DELAY_MS (hardcoded sequence, not a recomputation of the formula).
// 2. scheduledMs is in [currentBaseMs, currentBaseMs + JITTER_MS).
// 3. scheduledMs is always >= currentBaseMs (jitter is additive, not subtractive).
// 4. With deterministic random=0, the scheduled wait is exactly currentBaseMs.
// 5. Eventually-monotonic: from any starting base, repeated calls
//    converge to MAX_DELAY_MS as the new base.
// 6. Robustness: malformed random() (NaN, negative, >=1, +/-Infinity)
//    yields a finite, non-negative scheduledMs.

import { describe, it, expect } from "vitest";
import fc from "fast-check";

import { nextBackoffDelay, INITIAL_DELAY_MS, MAX_DELAY_MS, JITTER_MS } from "./reconnect.js";

describe("nextBackoffDelay backoff ladder", () => {
  // The observable backoff ladder: the base delay doubles on each attempt and
  // saturates at MAX_DELAY_MS. Expectations are hardcoded — this pins the exact
  // sequence the reconnect strategy climbs, rather than re-deriving it from
  // `min(base * 2, cap)` (which would just assert the implementation against a
  // copy of itself). nextBaseMs does not depend on the jitter RNG.
  const ladder: { base: number; nextBase: number }[] = [
    { base: 500, nextBase: 1000 }, // INITIAL_DELAY_MS: first retry after a clean connection
    { base: 1000, nextBase: 2000 },
    { base: 2000, nextBase: 4000 },
    { base: 4000, nextBase: 8000 }, // reaches the cap exactly
    { base: 6000, nextBase: 8000 }, // 12000 would overshoot the cap -> clamped
    { base: 8000, nextBase: 8000 }, // already capped -> stays put
  ];
  for (const { base, nextBase } of ladder) {
    it(`base ${base}ms advances to ${nextBase}ms next attempt`, () => {
      expect(nextBackoffDelay(base).nextBaseMs).toBe(nextBase);
    });
  }
});

describe("nextBackoffDelay property", () => {
  it("nextBaseMs never exceeds MAX_DELAY_MS", () => {
    fc.assert(
      fc.property(
        fc.double({ min: 0, max: 1_000_000, noNaN: true, noDefaultInfinity: true }),
        (base) => {
          const { nextBaseMs } = nextBackoffDelay(base);
          expect(nextBaseMs).toBeLessThanOrEqual(MAX_DELAY_MS);
        },
      ),
    );
  });

  it("scheduledMs lies in [currentBaseMs, currentBaseMs + JITTER_MS)", () => {
    fc.assert(
      fc.property(
        fc.double({ min: 0, max: 100_000, noNaN: true, noDefaultInfinity: true }),
        fc.double({ min: 0, max: 0.999, noNaN: true, noDefaultInfinity: true }),
        (base, r) => {
          const { scheduledMs } = nextBackoffDelay(base, () => r);
          expect(scheduledMs).toBeGreaterThanOrEqual(base);
          expect(scheduledMs).toBeLessThan(base + JITTER_MS);
        },
      ),
    );
  });

  it("with random=0, scheduledMs equals currentBaseMs (no jitter)", () => {
    fc.assert(
      fc.property(
        fc.double({ min: 0, max: 100_000, noNaN: true, noDefaultInfinity: true }),
        (base) => {
          const { scheduledMs } = nextBackoffDelay(base, () => 0);
          expect(scheduledMs).toBe(base);
        },
      ),
    );
  });

  it("malformed random (NaN/negative/>=1/Infinity) still yields finite non-negative scheduledMs", () => {
    const malformed = [NaN, -0.5, -1, 1, 1.5, Infinity, -Infinity];
    for (const bad of malformed) {
      const { scheduledMs } = nextBackoffDelay(INITIAL_DELAY_MS, () => bad);
      expect(Number.isFinite(scheduledMs)).toBe(true);
      expect(scheduledMs).toBeGreaterThanOrEqual(INITIAL_DELAY_MS);
      expect(scheduledMs).toBeLessThan(INITIAL_DELAY_MS + JITTER_MS);
    }
  });

  it("repeated calls from any starting base eventually saturate at MAX_DELAY_MS", () => {
    fc.assert(
      fc.property(
        fc.double({ min: 0.5, max: 10_000, noNaN: true, noDefaultInfinity: true }),
        (start) => {
          let base = start;
          // 30 iterations is well above ceil(log2(MAX_DELAY_MS / start))
          // for any start >= 0.5, so this loop always saturates.
          for (let i = 0; i < 30; i++) {
            base = nextBackoffDelay(base).nextBaseMs;
          }
          expect(base).toBe(MAX_DELAY_MS);
        },
      ),
    );
  });

  it("scales the jitter by JITTER_MS, so the spread is the documented 250ms", () => {
    // The bound-checking properties above pass for any jitter smaller than
    // JITTER_MS, including none at all — and jitter with no spread is what the
    // per-attempt randomisation exists to prevent (every tab that lost the
    // connection together would retry in the same instant). Hardcoded midpoint
    // and endpoints, so the actual magnitude is pinned.
    expect(nextBackoffDelay(500, () => 0.5).scheduledMs).toBe(625);
    expect(nextBackoffDelay(500, () => 0.8).scheduledMs).toBe(700);
    expect(nextBackoffDelay(8000, () => 0.5).scheduledMs).toBe(8125);
  });

  it("INITIAL_DELAY_MS first call produces scheduledMs in [500, 750)", () => {
    fc.assert(
      fc.property(fc.double({ min: 0, max: 0.999, noNaN: true, noDefaultInfinity: true }), (r) => {
        const { scheduledMs, nextBaseMs } = nextBackoffDelay(INITIAL_DELAY_MS, () => r);
        expect(scheduledMs).toBeGreaterThanOrEqual(500);
        expect(scheduledMs).toBeLessThan(750);
        expect(nextBaseMs).toBe(1000); // 500 * 2
      }),
    );
  });
});
