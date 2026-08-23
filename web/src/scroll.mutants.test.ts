// Five decisions in the scroll controller that the existing suites reach from
// one side only.
//
// Two are the exact edges of a boundary whose other side is already pinned: the
// clamp epsilon's own value (scroll.test.ts pins a 0.6px residual, which is
// inside the tolerance either way) and the bottom pin's zero case (every pin
// test starts with content below). Two are non-writes — the pin that must not
// touch the offset, and the zero-delta adjust that must not touch the direction
// baseline — and a non-write is invisible unless the container counts. The last
// is the unmounted controller: the renderer calls restoreView during bind,
// before any container exists.

import { describe, it, expect, vi } from "vitest";

import * as scroll from "./scroll.js";
import { makeClampingScrollEl, makeDeferredClampScrollEl } from "./test-helpers/scroll-fixture.js";

/** A clamping container that COUNTS writes to scrollTop, so a test can tell
 *  "wrote the value it already held" from "did not write". */
function makeCountingScrollEl(
  scrollHeight: number,
  clientHeight: number,
): { el: HTMLElement; writes: number[]; userScrollTo(top: number): void } {
  const el = document.createElement("div");
  let top = 0;
  const writes: number[] = [];
  const maxTop = (): number => Math.max(0, scrollHeight - clientHeight);
  Object.defineProperty(el, "scrollHeight", { get: () => scrollHeight, configurable: true });
  Object.defineProperty(el, "clientHeight", { get: () => clientHeight, configurable: true });
  Object.defineProperty(el, "scrollTop", {
    get: () => top,
    set: (v: number) => {
      writes.push(v);
      top = Math.max(0, Math.min(v, maxTop()));
    },
    configurable: true,
  });
  return {
    el,
    writes,
    userScrollTo(next: number): void {
      el.scrollTop = next;
      writes.pop(); // the gesture's own write is not the library's
      el.dispatchEvent(new Event("scroll"));
    },
  };
}

describe("scroll: the container listener registration", () => {
  it("registers the scroll listener as passive", () => {
    // A scroll listener cannot cancel the event, so the browser only needs to
    // know that in advance to keep the container off the main-thread-blocking
    // path. This asserts the REGISTRATION rather than a behaviour, because no
    // DOM implementation models the difference for a non-cancelable event.
    const f = makeClampingScrollEl(6000, 600);
    const spy = vi.spyOn(f.el, "addEventListener");

    scroll.init({ scrollEl: f.el });

    expect(spy).toHaveBeenCalledWith("scroll", expect.any(Function), { passive: true });
    spy.mockRestore();
  });
});

describe("scroll: the clamp epsilon's own edge", () => {
  it("leaves an offset exactly one pixel past the end uncorrected", () => {
    // CLAMP_EPSILON_PX is 1 and the test is strict, so one whole pixel past the
    // end is still inside the tolerance. That is the point of the constant:
    // scrollHeight and clientHeight are integers while scrollTop is fractional,
    // so an offset that reads a pixel over is what a correctly reconciled
    // container looks like, and correcting it would write on every shrink pass.
    const f = makeDeferredClampScrollEl(6000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(5400); // the maximum: distanceFromBottom is 0

    // Force the offset one pixel over. A write would be clamped back, which is
    // why this goes through the property rather than through scrollTop.
    let held = 5401;
    Object.defineProperty(f.el, "scrollTop", {
      get: () => held,
      set: (v: number) => {
        held = Math.max(0, Math.min(v, f.maxTop()));
      },
      configurable: true,
    });

    scroll.noteContentShrink(5401);
    scroll.reconcileScrollRange();

    expect(f.el.scrollTop).toBe(5401);
  });

  it("corrects an offset two pixels past the end", () => {
    // The far side of the same edge, so "uncorrected" above reads as a
    // tolerance and not as a correction that never happens.
    const f = makeDeferredClampScrollEl(6000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(5400);

    let held = 5402;
    Object.defineProperty(f.el, "scrollTop", {
      get: () => held,
      set: (v: number) => {
        held = Math.max(0, Math.min(v, f.maxTop()));
      },
      configurable: true,
    });

    scroll.noteContentShrink(5402);
    scroll.reconcileScrollRange();

    expect(f.el.scrollTop).toBe(5400);
  });
});

describe("scroll: the bottom pin at zero distance", () => {
  it("writes nothing when the following reader is already at the bottom", () => {
    // stickToBottom runs on every frame of a streaming session. At the bottom
    // there is nothing to move, and the guard is what keeps the pin from
    // assigning the offset it already holds sixty times a second — a write that
    // a real container answers by cancelling any smooth scroll in flight and
    // queueing another scroll event.
    const f = makeCountingScrollEl(6000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(5400); // at the bottom, following
    expect(scroll.isUserScrolledUp()).toBe(false);
    expect(f.writes).toEqual([]);

    scroll.stickToBottom();

    expect(f.writes).toEqual([]);
    expect(f.el.scrollTop).toBe(5400);
  });

  it("still writes when the following reader has content below", () => {
    // The control: the same call, one pixel of distance, and the pin acts.
    const f = makeCountingScrollEl(6000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(5399);
    expect(scroll.isUserScrolledUp()).toBe(false);

    scroll.stickToBottom();

    expect(f.writes).toEqual([5400]);
  });
});

describe("scroll: the zero-delta content shift", () => {
  it("leaves the direction baseline alone, so a pending user scroll still reads as downward", () => {
    // A browser delivers a scroll event asynchronously, so a rAF flush can run
    // between the user's move and its event — and the flush announces every
    // content shift, including the ones that turn out to be zero. A zero-delta
    // adjust that "harmlessly" wrote the offset back would sync the baseline to
    // the user's new position, and the event still in flight would then read as
    // no movement at all: the reader who scrolled down to the tail would be
    // left holding instead of following.
    const f = makeClampingScrollEl(6000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(3000); // scrolled up to read
    expect(scroll.isUserScrolledUp()).toBe(true);

    // The user scrolls back to the tail. The position moves now; the event is
    // queued for the next frame.
    f.el.scrollTop = 5400;
    scroll.adjustForContentShift(0);

    f.el.dispatchEvent(new Event("scroll"));

    expect(scroll.isUserScrolledUp()).toBe(false);
  });
});

describe("scroll: an unmounted controller", () => {
  /** The query that makes the import below a distinct URL, and therefore a
   *  distinct module evaluation. */
  const FRESH_INSTANCE_QUERY = "unmounted=1";

  it("ignores restoreView entirely, including the follow state", async () => {
    // The renderer calls restoreView from bind, which can run before any
    // container is attached. Applying the follow state there would leave the
    // module holding a state for a container it does not have, and the first
    // real init would then report a follow change that no user gesture caused.
    // A fresh module instance is the only way to observe the un-inited state.
    //
    // The query bust, not `vi.resetModules()`: the browser's module registry is
    // URL-keyed, so resetting and re-importing the same specifier returns the
    // CACHED instance — already inited by the suites above, and the test would
    // then assert against exactly the state it was written to avoid. The
    // extension stays `.ts` (the real file) rather than the `.js` the static
    // imports use, because this specifier is built at runtime and so IS the URL
    // the browser requests; written `.js`, the evaluation is attributed to a
    // file that does not exist and coverage silently reports zero for it.
    // Interpolated rather than written as one literal so tsc treats it as a
    // runtime specifier instead of trying to resolve `scroll.ts?unmounted=1`.
    const fresh = (await import(
      /* @vite-ignore */ `./scroll.ts?${FRESH_INSTANCE_QUERY}`
    )) as typeof scroll;

    expect(fresh.isUserScrolledUp()).toBe(false); // follows by default
    fresh.restoreView({ top: 4200, following: false });

    expect(fresh.isUserScrolledUp()).toBe(false);
    expect(fresh.currentScrollTop()).toBe(0);
  });
});
