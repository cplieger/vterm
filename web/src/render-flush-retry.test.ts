// @vitest-environment happy-dom
//
// Bounded error-path reschedule (d-u4-1, guarding the l-f28 retry).
//
// flushRender's catch reschedules a flush when flushRenderInner throws mid-drain
// so a partial/transient failure still finishes painting (l-f28). But the drain
// loop deletes a queued row only AFTER upsertRow succeeds, so a row whose build
// throws deterministically stays queued: an unbounded catch -> rAF -> throw
// reschedule would be a ~60fps busy loop, even on an idle session. These tests
// pin the two required behaviors with a fake requestAnimationFrame (so the "rAF
// loop" is pumped deterministically, no real frames, no sleeps) and a fake
// LineStore whose getLine throws on demand:
//   1. a row that ALWAYS throws -> the reschedule is BOUNDED (it gives up and
//      stops scheduling frames) rather than looping forever;
//   2. a row that throws ONCE then succeeds (a transient font/measureText race)
//      -> the retry still drains the row.

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import * as render from "./render.js";
import { LineStore as RealLineStore } from "./store.js";
import type { WireRun } from "./types.js";
import type { LineStore, StoreChanges, WindowState } from "./store.js";

// happy-dom has no Canvas2D; render's measureChar needs measureText. The
// transient test builds a real row on the successful retry, so stub it.
interface FakeCtx {
  font: string;
  measureText: (t: string) => { width: number };
}
HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
  const ctx: FakeCtx = {
    font: "",
    measureText: (text: string) => ({ width: text.length * 8 }),
  };
  return ctx;
} as typeof HTMLCanvasElement.prototype.getContext;

// --- Deterministic requestAnimationFrame ---
// Callbacks are queued, never auto-run; the test pumps them so the reschedule
// chain is fully deterministic and terminates (or is caught by a safety cap).
let rafQueue: FrameRequestCallback[] = [];
let rafCalls = 0;
let rafId = 0;
let realRaf: typeof requestAnimationFrame;
let realCaf: typeof cancelAnimationFrame;

function installFakeRaf(): void {
  rafQueue = [];
  rafCalls = 0;
  rafId = 0;
  globalThis.requestAnimationFrame = ((cb: FrameRequestCallback): number => {
    rafCalls++;
    rafQueue.push(cb);
    return ++rafId;
  }) as typeof requestAnimationFrame;
  // No-op: the test never leaves a real frame pending; init()/resetScreen() call
  // cancelAnimationFrame but drive pendingFrame back to undefined themselves.
  globalThis.cancelAnimationFrame = (() => undefined) as typeof cancelAnimationFrame;
}

// pump drains the fake rAF queue, invoking each callback (which may reschedule
// and push another). Returns how many callbacks ran. The safety cap is the
// anti-infinite-loop guard: a genuinely unbounded reschedule would hit it with
// a non-empty queue, failing the "it terminated" assertions below.
const SAFETY_CAP = 200;
function pump(): number {
  let iterations = 0;
  while (rafQueue.length > 0) {
    if (iterations >= SAFETY_CAP) {
      break;
    }
    const cb = rafQueue.shift()!;
    iterations++;
    cb(0);
  }
  return iterations;
}

// A fake LineStore whose getLine(target) throws for the first `throwCount`
// calls, then returns a one-run line. Cast through unknown: it implements only
// the methods the render flush path touches (test-fake escape hatch).
function makeFakeStore(target: number, throwCount: number): LineStore {
  let thrown = 0;
  const win: WindowState = {
    base: 0,
    height: 24,
    // Cursor sits on a DIFFERENT row than `target`, so the throw happens in the
    // drain loop (not the unconditional cursor-row build) and getLine(cursor)
    // returns undefined harmlessly.
    cursorRow: 5,
    cursorCol: 0,
    cursorStyle: 0,
    cursorHidden: true,
    cursorBlink: false,
  };
  const changes: StoreChanges = {
    dirtyLines: [],
    evictedLines: [],
    windowChanged: false,
    altChanged: false,
    fullReset: false,
  };
  const fake = {
    reset: (): void => undefined,
    // bind() closes the OUTGOING store's solicited window, so every store the
    // renderer is ever bound to must answer this — including a fake.
    clearSolicited: (): void => undefined,
    getWindow: (): WindowState => win,
    isAlt: (): boolean => false,
    getAltRows: (): WireRun[][] => [],
    hasTrimmedHistory: (): boolean => false,
    highestIndex: (): number => target,
    oldestIndex: (): number => target,
    // rebuild() (via bind) enqueues the target row through forEachLine.
    forEachLine: (cb: (abs: number, runs: WireRun[]) => void): void => {
      cb(target, []);
    },
    drainChanges: (): StoreChanges => changes,
    getLine: (abs: number): WireRun[] | undefined => {
      if (abs !== target) {
        return undefined;
      }
      if (thrown < throwCount) {
        thrown++;
        throw new Error("vterm-test: upsertRow boom");
      }
      return [{ t: "x", f: -1, b: -1, a: 0, uc: -1 }];
    },
  };
  return fake as unknown as LineStore;
}

describe("render: bounded error-path reschedule (d-u4-1)", () => {
  let outputEl: HTMLDivElement;
  let termWrap: HTMLDivElement;
  let errorSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    realRaf = globalThis.requestAnimationFrame;
    realCaf = globalThis.cancelAnimationFrame;
    installFakeRaf();
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    outputEl = document.getElementById("term-output") as HTMLDivElement;
    render.resetScreen();
    render.init({ output: outputEl, termWrap });
    render.updateFontMetrics();
    // Discard any flush scheduled during setup; init() already reset pendingFrame
    // to undefined, so the test's bind() schedules cleanly from a clean slate.
    rafQueue = [];
    rafCalls = 0;
    errorSpy = vi.spyOn(console, "error").mockImplementation(() => undefined);
  });

  afterEach(() => {
    errorSpy.mockRestore();
    globalThis.requestAnimationFrame = realRaf;
    globalThis.cancelAnimationFrame = realCaf;
    // Re-bind a REAL store: render.ts holds a module-level `store`, vitest runs
    // with isolate:false, and bind(makeFakeStore(...)) above would otherwise
    // leave the fake (no applyScreen/applyScroll) bound for whichever test file
    // shares this worker next — the cross-file "store.applyScreen is not a
    // function" flake whose failing file moved run to run on main.
    render.bind(new RealLineStore());
  });

  function gaveUp(): boolean {
    return errorSpy.mock.calls.some(
      (args: unknown[]) =>
        typeof args[0] === "string" && args[0].includes("giving up render retry"),
    );
  }

  it("stops rescheduling when a row throws deterministically (no busy loop)", () => {
    render.bind(makeFakeStore(0, Number.MAX_SAFE_INTEGER)); // getLine(0) always throws

    const iterations = pump();

    // Terminated before the safety cap, with nothing left rescheduled: proves
    // the catch stopped scheduling frames rather than looping ~60fps forever.
    expect(iterations).toBeLessThan(SAFETY_CAP);
    expect(rafQueue.length).toBe(0);
    // Small, bounded frame count (1 bind + a few capped retries), not indefinite.
    expect(rafCalls).toBeLessThanOrEqual(8);
    // The row never drained (its build always threw).
    expect(outputEl.querySelector('[data-abs="0"]')).toBeNull();
    // It logged the give-up once it hit the no-progress cap.
    expect(gaveUp()).toBe(true);
  });

  it("still drains a row whose build throws once then succeeds (transient)", () => {
    render.bind(makeFakeStore(0, 1)); // getLine(0) throws once, then returns a line

    const iterations = pump();

    // Settled with the row painted: the transient throw was retried successfully.
    expect(rafQueue.length).toBe(0);
    expect(iterations).toBeLessThanOrEqual(4);
    expect(outputEl.querySelector('[data-abs="0"]')).not.toBeNull();
    // A single transient error must NOT trip the give-up path.
    expect(gaveUp()).toBe(false);
  });
});
