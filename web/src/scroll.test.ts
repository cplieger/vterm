// Brick-4 scroll controller. scrollHeight / clientHeight are declared rather
// than measured, because the geometry IS the premise: the follow/hold state
// machine is what is under test, and a container given real overflow would
// answer from whatever this file's fixture markup lays out at. Following derives
// from scroll position plus movement direction, asymmetrically (any upward move
// with content below holds; only a DOWNWARD move landing at the bottom
// re-engages, so a shrink clamp can never engage follow under a reader who
// scrolled up), and stickToBottom only pins when following.
//
// Fixture note: the mock starts at scrollTop 0 while reporting 700px of scroll
// range, which is a state the real container is never in (init's contract is
// "following", i.e. at the bottom). Tests that care about the follow transition
// therefore establish the bottom explicitly first, exactly as a real session
// does through the render pin.

import { describe, it, expect, beforeEach } from "vitest";
import * as scroll from "./scroll.js";
import { makeClampingScrollEl, makeDeferredClampScrollEl } from "./test-helpers/scroll-fixture.js";

function makeScrollEl(scrollHeight: number, clientHeight: number): HTMLElement {
  const el = document.createElement("div");
  let top = 0;
  Object.defineProperty(el, "scrollHeight", { get: () => scrollHeight, configurable: true });
  Object.defineProperty(el, "clientHeight", { get: () => clientHeight, configurable: true });
  Object.defineProperty(el, "scrollTop", {
    get: () => top,
    set: (v: number) => {
      top = v;
    },
    configurable: true,
  });
  return el;
}

function scrollTo(el: HTMLElement, top: number): void {
  el.scrollTop = top;
  el.dispatchEvent(new Event("scroll"));
}

describe("scroll controller (brick 4)", () => {
  let el: HTMLElement;
  let changes: boolean[];

  beforeEach(() => {
    el = makeScrollEl(1000, 300); // 700px of scroll range
    changes = [];
    scroll.init({ scrollEl: el, onUserScrollChange: (up) => changes.push(up) });
  });

  it("starts in the following state", () => {
    expect(scroll.isUserScrolledUp()).toBe(false);
  });

  it("flips to holding when the user scrolls up past the tolerance, and back", () => {
    scrollTo(el, 700); // at the bottom, following
    scrollTo(el, 0); // distance from bottom = 700 -> holding
    expect(scroll.isUserScrolledUp()).toBe(true);
    expect(changes).toEqual([true]);
    scrollTo(el, 700); // a DOWNWARD move landing at the bottom -> following
    expect(scroll.isUserScrolledUp()).toBe(false);
    expect(changes).toEqual([true, false]);
  });

  it("treats within-tolerance as following (24px)", () => {
    scrollTo(el, 700); // at the bottom, following
    scrollTo(el, 680); // 20px up, inside the tolerance but a real gap -> holding
    expect(scroll.isUserScrolledUp()).toBe(true);
    scrollTo(el, 690); // downward but 10px short of the bottom, inside the
    expect(scroll.isUserScrolledUp()).toBe(false); // tolerance -> following
    scrollTo(el, 669); // 21px up, distance 31 (> 24) -> holding
    expect(scroll.isUserScrolledUp()).toBe(true);
  });

  it("disengages follow on ANY upward scroll that leaves content below", () => {
    // Even a few px of upward movement inside the bottom tolerance is user
    // intent to hold. With a tolerance-only rule this stayed "following" and
    // the next render pin yanked the user back down (see the streaming test
    // below for the full race).
    scrollTo(el, 700); // at the bottom, following
    scrollTo(el, 694); // 6px up — still within the 24px tolerance
    expect(scroll.isUserScrolledUp()).toBe(true);
  });

  it("a slow upward drag escapes the per-frame pin during heavy streaming", () => {
    // The streaming fight: each frame the renderer flushes and pins to the
    // bottom, so a slow drag's per-frame increment restarted from the bottom
    // and (tolerance-only) never accumulated past 24px — the user was yanked
    // down every few ms. Direction-based disengage flips holding on the FIRST
    // upward tick, so the next pin is a no-op.
    scrollTo(el, 700); // bottom
    scroll.stickToBottom(); // frame N pin (no-op at the bottom)
    scrollTo(el, 692); // first drag tick, 8px up
    scroll.stickToBottom(); // frame N+1 pin must not fight the drag
    expect(el.scrollTop).toBe(692);
    expect(scroll.isUserScrolledUp()).toBe(true);
  });

  it("keeps following when a content shrink clamps scrollTop to the new bottom", () => {
    // Top-row eviction / a clear shrinks scrollHeight; the browser clamps
    // scrollTop DOWN to the new maximum — an upward move that is NOT the user
    // and lands exactly at the bottom. Auto-follow must survive it.
    let sh = 1000;
    const el2 = document.createElement("div");
    let top = 0;
    Object.defineProperty(el2, "scrollHeight", { get: () => sh, configurable: true });
    Object.defineProperty(el2, "clientHeight", { get: () => 300, configurable: true });
    Object.defineProperty(el2, "scrollTop", {
      get: () => top,
      set: (v: number) => {
        top = v;
      },
      configurable: true,
    });
    scroll.init({ scrollEl: el2 });
    scrollTo(el2, 700); // at the bottom, following
    sh = 900; // eviction shrinks the content
    scrollTo(el2, 600); // the browser's clamp to the new bottom (upward move)
    expect(scroll.isUserScrolledUp()).toBe(false); // still following
    scroll.stickToBottom();
    expect(el2.scrollTop).toBe(600); // already at the (new) bottom, no churn
  });

  it("stickToBottom pins to the bottom while following", () => {
    expect(el.scrollTop).toBe(0);
    scroll.stickToBottom();
    // The bottom is scrollHeight - clientHeight, written out. This fixture stores
    // whatever it is handed, so it is the one that can tell the difference
    // between the pin computing the bottom and the pin writing scrollHeight and
    // trusting the container to clamp it back.
    expect(el.scrollTop).toBe(700);
  });

  it("stickToBottom does nothing while holding (does not yank the reader)", () => {
    scrollTo(el, 100); // holding
    scroll.stickToBottom();
    expect(el.scrollTop).toBe(100); // unchanged
  });

  it("scrollToBottom forces the bottom and re-engages following", () => {
    scrollTo(el, 700); // at the bottom, following
    scrollTo(el, 0); // holding
    expect(scroll.isUserScrolledUp()).toBe(true);
    scroll.scrollToBottom();
    expect(el.scrollTop).toBe(700);
    expect(scroll.isUserScrolledUp()).toBe(false);
  });

  it("a content shrink that clamps a HOLDING reader to the bottom keeps holding", () => {
    // The asymmetry's whole reason to exist. A program that erases its
    // scrollback (ED3) or a cap eviction collapses scrollHeight, and the
    // browser clamps scrollTop DOWN to the new maximum. By position that clamp
    // is indistinguishable from the user arriving at the tail; by DIRECTION it
    // is not, because a user returning to the tail moves down and a clamp moves
    // up. Deriving from position alone re-engaged auto-follow under a reader who
    // had deliberately scrolled up, and every line after that pinned them to the
    // bottom (measured: scrolled up 20000px, an ED3 collapsed the content, the
    // viewport was pinned to the tail from then on).
    let sh = 1000;
    const el2 = document.createElement("div");
    let top = 0;
    Object.defineProperty(el2, "scrollHeight", { get: () => sh, configurable: true });
    Object.defineProperty(el2, "clientHeight", { get: () => 300, configurable: true });
    Object.defineProperty(el2, "scrollTop", {
      get: () => top,
      set: (v: number) => {
        top = v;
      },
      configurable: true,
    });
    scroll.init({ scrollEl: el2 });
    scrollTo(el2, 700); // at the bottom, following
    scrollTo(el2, 200); // the user scrolls up to read
    expect(scroll.isUserScrolledUp()).toBe(true);

    sh = 300; // the app erases its scrollback: only the live screen is left
    scrollTo(el2, 0); // the browser clamps to the new maximum (an upward move)
    expect(scroll.isUserScrolledUp()).toBe(true); // still holding

    // And the pin must keep its hands off, however much output follows.
    sh = 2000;
    scroll.stickToBottom();
    expect(el2.scrollTop).toBe(0);
  });

  it("a scroll event that did not move the position infers nothing", () => {
    // The state that makes this matter: HOLDING while the position happens to be
    // the bottom, which a content shrink produces (see the test above). An
    // unmoved event there reports "at the bottom", and deriving follow from
    // position alone would re-engage on it. Asserted after a shrink rather than
    // at a mid position, because at a mid position the old position-derive also
    // answers "holding" and the test would pass either way.
    let sh = 1000;
    const el2 = document.createElement("div");
    let top = 0;
    Object.defineProperty(el2, "scrollHeight", { get: () => sh, configurable: true });
    Object.defineProperty(el2, "clientHeight", { get: () => 300, configurable: true });
    Object.defineProperty(el2, "scrollTop", {
      get: () => top,
      set: (v: number) => {
        top = Math.max(0, Math.min(v, sh - 300)); // clamp like a real container
      },
      configurable: true,
    });
    scroll.init({ scrollEl: el2 });
    scrollTo(el2, 700); // at the bottom, following
    scrollTo(el2, 200); // the user scrolls up to read
    expect(scroll.isUserScrolledUp()).toBe(true);

    sh = 500; // content shrinks so 200 IS now the bottom, with no clamp needed
    el2.dispatchEvent(new Event("scroll")); // unmoved, and reporting the bottom
    expect(scroll.isUserScrolledUp()).toBe(true);
  });
});

// noteContentShrink is how the layer that REMOVES rows tells this module that
// the clamp it is about to see is not a user scrolling up. It exists because the
// arithmetic signature the epsilon tests is lossy: scrollHeight/clientHeight are
// integer-rounded while scrollTop is fractional, so a clamp can present as an
// upward move with a real gap under browser zoom or a fractional DPR, and the
// reader is then left "holding at the bottom" with auto-follow silently off.
//
// These use the CLAMPING fixture, because a clamp is the whole subject: a
// container that stores whatever offset it is handed cannot produce one.
describe("noteContentShrink (announced clamps)", () => {
  it("an UNannounced upward move of the same magnitude still disengages", () => {
    // Deleting the direction rule must fail this one.
    //
    // Its former sibling, "an announced shrink of any magnitude preserves
    // follow", was deleted 2026-08-21: makeClampingScrollEl.setScrollHeight
    // dispatches the clamp's scroll event from inside itself, so `wasShrink` was
    // already consumed before noteContentShrink could arm it and the test passed
    // with `if (wasShrink)` made unreachable. scroll-arming.test.ts covers that
    // branch on the ordering a browser really produces, with a fixture that does
    // not dispatch, and it does fail on that mutant.
    const f = makeClampingScrollEl(10000, 300);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(9700);
    expect(scroll.isUserScrolledUp()).toBe(false);

    f.userScrollTo(100); // the user drags up: nothing announced
    expect(scroll.isUserScrolledUp()).toBe(true);
  });

  it("does not arm when the removal cannot move the position", () => {
    // Rows removed BELOW the viewport, a removal smaller than the bottom gap, or
    // a wipe whose height is held up by an absolutely-positioned overlay: no
    // clamp, so no event, so an arm would linger and swallow the next gesture.
    const f = makeClampingScrollEl(10000, 300);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(4000); // holding, far from both ends
    expect(scroll.isUserScrolledUp()).toBe(true);

    const before = f.el.scrollTop;
    f.setScrollHeight(9000); // still far above scrollTop + clientHeight: no clamp
    scroll.noteContentShrink(before);
    expect(f.el.scrollTop).toBe(before); // nothing moved

    // Back to the bottom, then a genuine drag up. If the arm had lingered it
    // would swallow this and the reader would be stuck following.
    f.userScrollTo(8700);
    expect(scroll.isUserScrolledUp()).toBe(false);
    f.userScrollTo(8600);
    expect(scroll.isUserScrolledUp()).toBe(true);
  });

  it("never survives more than one event", () => {
    const f = makeClampingScrollEl(10000, 300);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(9700);
    const before = f.el.scrollTop;
    f.setScrollHeight(5000);
    scroll.noteContentShrink(before); // armed, and consumed by the clamp's event
    f.el.dispatchEvent(new Event("scroll"));
    // A real upward gesture immediately after must register.
    f.userScrollTo(1000);
    expect(scroll.isUserScrolledUp()).toBe(true);
  });

  it("a subpixel clamp residual keeps follow even unannounced", () => {
    // The fractional-layout case the epsilon is actually for: scrollHeight and
    // clientHeight are integers, scrollTop is not, so a clamp can land a fraction
    // of a pixel short of the bottom.
    const el = document.createElement("div");
    let top = 0;
    Object.defineProperty(el, "scrollHeight", { get: () => 1000, configurable: true });
    Object.defineProperty(el, "clientHeight", { get: () => 300, configurable: true });
    Object.defineProperty(el, "scrollTop", {
      get: () => top,
      set: (v: number) => {
        top = v;
      },
      configurable: true,
    });
    scroll.init({ scrollEl: el });
    el.scrollTop = 700;
    el.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(false);
    el.scrollTop = 699.4; // 0.6px of residual: an upward move, still the bottom
    el.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(false);
  });
});

describe("per-view scroll memory seam (currentScrollTop / restoreScrollTop)", () => {
  let el: HTMLElement;

  beforeEach(() => {
    el = makeScrollEl(1000, 300);
    scroll.init({ scrollEl: el });
  });

  it("reads the live offset through currentScrollTop", () => {
    scrollTo(el, 250);
    expect(scroll.currentScrollTop()).toBe(250);
  });

  it("restoring a mid position holds; restoring the bottom re-engages follow", () => {
    // A tabbed shell re-entering a tab whose user had scrolled up: the write
    // fires a scroll event (as any scrollTop assignment does in a browser) and
    // the follow/hold state re-derives from it like a user scroll.
    scroll.restoreScrollTop(100);
    el.dispatchEvent(new Event("scroll")); // the fixture's setter fires none
    expect(scroll.currentScrollTop()).toBe(100);
    expect(scroll.isUserScrolledUp()).toBe(true);

    scroll.restoreScrollTop(700); // back to the bottom (distance 0)
    el.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(false);
  });
});

// adjustForContentShift is manual scroll anchoring: it exists because WebKit has
// never implemented the native kind, so on Safari/iPadOS nothing held the reading
// position when rows were evicted from the top of history during streaming.
describe("adjustForContentShift", () => {
  let el: HTMLElement;
  let changes: boolean[];

  beforeEach(() => {
    el = makeScrollEl(1000, 300);
    changes = [];
    scroll.init({ scrollEl: el, onUserScrollChange: (up) => changes.push(up) });
  });

  it("moves the viewport by the height that vanished above the reading position", () => {
    scrollTo(el, 400); // scroll up to read
    expect(scroll.isUserScrolledUp()).toBe(true);

    // Two 17px rows evicted from the top of history: the content the user is
    // reading is now 34px higher, so the viewport must follow it up by 34px.
    scroll.adjustForContentShift(-34);
    expect(el.scrollTop).toBe(366);
  });

  it("moves the other way when content is inserted above (the trim marker)", () => {
    scrollTo(el, 400);
    scroll.adjustForContentShift(20);
    expect(el.scrollTop).toBe(420);
  });

  it("does not disengage or re-engage following", () => {
    scrollTo(el, 400);
    changes.length = 0;
    scroll.adjustForContentShift(-34);
    el.dispatchEvent(new Event("scroll")); // the adjust's own event
    expect(scroll.isUserScrolledUp()).toBe(true);
    expect(changes).toEqual([]); // no follow flip, no churn
  });

  it("is a no-op while following, where the bottom pin owns the position", () => {
    scrollTo(el, 700); // at the bottom
    expect(scroll.isUserScrolledUp()).toBe(false);
    scroll.adjustForContentShift(-34);
    expect(el.scrollTop).toBe(700);
  });

  it("is a no-op for a zero delta", () => {
    scrollTo(el, 400);
    scroll.adjustForContentShift(0);
    expect(el.scrollTop).toBe(400);
  });

  it("does not re-engage follow when the correction lands the reader at the bottom", () => {
    // A reader holding a few px short of the tail while content grows ABOVE
    // them: the anchor correction pushes the viewport down by that growth and
    // can land exactly at the bottom. That is the library moving the viewport to
    // keep their line, not the user arriving at the tail, so follow must stay
    // off. Deriving from the resulting event's position would flip it on and the
    // next pin would take the position over.
    scrollTo(el, 700); // at the bottom, following
    scrollTo(el, 650); // holding, 50px up
    expect(scroll.isUserScrolledUp()).toBe(true);
    scroll.adjustForContentShift(50); // content grew above: follow the line down
    el.dispatchEvent(new Event("scroll")); // the adjust's own event
    expect(el.scrollTop).toBe(700); // at the bottom now
    expect(scroll.isUserScrolledUp()).toBe(true); // and still holding
  });
});

// restoreView is the per-view seam that carries BOTH halves of a saved view.
// It exists because "holding at the bottom" became reachable once a content
// shrink stopped re-engaging follow, and position alone cannot express it.
describe("restoreView", () => {
  let el: HTMLElement;
  let changes: boolean[];

  beforeEach(() => {
    el = makeScrollEl(1000, 300);
    changes = [];
    scroll.init({ scrollEl: el, onUserScrollChange: (up) => changes.push(up) });
  });

  it("restores a mid position with follow off", () => {
    scroll.restoreView({ top: 100, following: false });
    el.dispatchEvent(new Event("scroll"));
    expect(el.scrollTop).toBe(100);
    expect(scroll.isUserScrolledUp()).toBe(true);
  });

  it("restores the bottom with follow on", () => {
    scrollTo(el, 200); // holding
    scroll.restoreView({ top: 700, following: true });
    el.dispatchEvent(new Event("scroll"));
    expect(el.scrollTop).toBe(700);
    expect(scroll.isUserScrolledUp()).toBe(false);
  });

  it("restores holding AT the bottom, which position alone cannot express", () => {
    scrollTo(el, 700); // at the bottom, following
    scroll.restoreView({ top: 700, following: false });
    el.dispatchEvent(new Event("scroll"));
    expect(el.scrollTop).toBe(700);
    expect(scroll.isUserScrolledUp()).toBe(true);
    // The pin must respect it, so the tail does not take the view back.
    scroll.stickToBottom();
    expect(el.scrollTop).toBe(700);
  });

  it("restores following at a mid position and lets the pin reconcile it", () => {
    scrollTo(el, 200); // holding
    scroll.restoreView({ top: 400, following: true });
    el.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(false);
    scroll.stickToBottom();
    expect(el.scrollTop).toBe(700); // the bottom: scrollHeight - clientHeight
  });

  it("does not arm the one-event pass-through when the position did not move", () => {
    // No move means no scroll event, so an arm would linger and swallow the
    // user's NEXT gesture. Restore the offset it already holds, then make a
    // genuine upward move and require it to register.
    scrollTo(el, 700); // at the bottom, following
    scroll.restoreView({ top: 700, following: true });
    expect(scroll.isUserScrolledUp()).toBe(false);
    scrollTo(el, 300); // a real upward gesture
    expect(scroll.isUserScrolledUp()).toBe(true);
  });

  it("ignores a non-finite offset but still applies the follow state", () => {
    scrollTo(el, 400); // holding
    scroll.restoreView({ top: Number.NaN, following: true });
    expect(el.scrollTop).toBe(400); // not coerced to 0 / jumped to the top
    expect(scroll.isUserScrolledUp()).toBe(false);
  });
});

// reconcileScrollRange is the third arithmetic state of distanceFromBottom:
// positive means content below (pin), zero means the tail (nothing to do), and
// NEGATIVE means the container is holding an offset past the end of its own
// content. Only two of the three were ever consumed, so a container that does
// not reconcile a shrink left the viewport parked over empty space with the
// content above it, and nothing in the library wrote the offset again.
//
// Every test here uses the DEFERRED-CLAMP fixture, because that is the whole
// subject: on a container that reconciles synchronously the negative state does
// not exist, which is exactly why the clamping fixtures could not fail for this.
describe("reconcileScrollRange (a container that does not reconcile a shrink)", () => {
  it("moves a FOLLOWING reader back onto the content after a big shrink", () => {
    const f = makeDeferredClampScrollEl(85000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(84400); // at the tail of a long session, following
    expect(scroll.isUserScrolledUp()).toBe(false);

    // The application erases its scrollback: only the live screen survives.
    const before = f.el.scrollTop;
    f.setScrollHeight(600);
    // The offset is untouched and illegal: 84400 into 600px of content.
    expect(f.el.scrollTop).toBe(84400);
    expect(f.maxTop()).toBe(0);

    scroll.noteContentShrink(before);
    scroll.reconcileScrollRange();
    expect(f.el.scrollTop).toBe(0); // the whole bug: this used to stay at 84400
  });

  it("keeps a following reader following across the correction", () => {
    const f = makeDeferredClampScrollEl(85000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(84400);

    const before = f.el.scrollTop;
    f.setScrollHeight(6000);
    scroll.noteContentShrink(before);
    scroll.reconcileScrollRange();
    expect(f.el.scrollTop).toBe(5400);
    // The correction produced a large UPWARD move, which the direction rule
    // would read as the user pulling away from the tail. It goes through
    // writePreservingFollow, so the event it causes passes through instead.
    f.el.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(false);
  });

  it("corrects a HOLDING reader too, and leaves them holding", () => {
    // The pin cannot own this correction: it is follow-gated, and a reader who
    // scrolled up to read is stranded over empty space by the same shrink. The
    // read anchor cannot own it either, because it deliberately stands down when
    // the lines it was holding were discarded rather than trimmed. Landing at
    // the tail with follow still OFF is the ratified degradation for a reading
    // position whose lines no longer exist.
    const f = makeDeferredClampScrollEl(85000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(84400); // following
    f.userScrollTo(20000); // scrolled up to read
    expect(scroll.isUserScrolledUp()).toBe(true);

    const before = f.el.scrollTop;
    f.setScrollHeight(600);
    scroll.noteContentShrink(before);
    scroll.reconcileScrollRange();
    expect(f.el.scrollTop).toBe(0);
    f.el.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(true); // still holding
  });

  it("leaves an out-of-range offset alone when no caller announced a removal", () => {
    // The overscroll bounce. Safari reports an offset past the maximum while a
    // rubber-band is in flight, with no content change at all, and correcting
    // that would cut the user's own gesture. Nothing announced a shrink, so
    // nothing is armed, so the call is a no-op.
    const f = makeDeferredClampScrollEl(6000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(5400); // at the bottom
    // The bounce, forced past the maximum the way the platform does it (a write
    // would be clamped, which is the point of not using one here).
    Object.defineProperty(f.el, "scrollTop", { value: 5600, configurable: true });

    scroll.reconcileScrollRange();
    expect(f.el.scrollTop).toBe(5600); // untouched
  });

  it("corrects a bounce that coincides with a removal, and that is the choice", () => {
    // The accepted overlap, pinned so it reads as a decision. A bounce during a
    // row-removing pass (cap eviction under heavy streaming) satisfies the gate,
    // and the correction snaps the offset to the maximum the bounce was settling
    // towards anyway. The alternative (refuse whenever the offset was ALREADY
    // out of range) skips the repair for the rest of the session whenever the
    // two coincide, and a cut animation is the cheaper loss.
    const f = makeDeferredClampScrollEl(6000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(5400);
    Object.defineProperty(f.el, "scrollTop", {
      get: () => bounced,
      set: (v: number) => {
        bounced = Math.max(0, Math.min(v, f.maxTop()));
      },
      configurable: true,
    });
    let bounced = 5600; // mid-bounce, past the maximum

    const before = f.el.scrollTop;
    f.setScrollHeight(5000); // a cap eviction lands in the same frame
    scroll.noteContentShrink(before);
    scroll.reconcileScrollRange();
    expect(f.el.scrollTop).toBe(4400);
  });

  it("does not correct a subpixel residual", () => {
    // scrollHeight and clientHeight are integers while scrollTop is fractional,
    // so a correctly reconciled offset can read a fraction past the end. The
    // epsilon keeps its original job; correcting this would write on every
    // shrink pass to move the viewport by half a pixel.
    const f = makeDeferredClampScrollEl(6000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(5400);
    Object.defineProperty(f.el, "scrollTop", { value: 5400.6, configurable: true });

    scroll.noteContentShrink(5400.6);
    scroll.reconcileScrollRange();
    expect(f.el.scrollTop).toBe(5400.6); // untouched
  });

  it("arms both questions when the container reconciles only partway", () => {
    // The two questions are independent, which is why neither test returns early
    // on the other: a partial reconciliation moved the offset (so the event it
    // produced is a clamp, not a gesture) AND left it out of range (so a
    // correction is still owed). Answering only the first is what shipped.
    const f = makeDeferredClampScrollEl(85000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(84400);

    const before = f.el.scrollTop;
    f.setScrollHeight(600);
    Object.defineProperty(f.el, "scrollTop", { value: 40000, configurable: true });
    scroll.noteContentShrink(before); // moved down, and still illegal

    // The clamp's own event must not disengage follow (question one)...
    f.el.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(false);
    // ...and the offset must still be corrected (question two). Restore a
    // writable property so the correction can land.
    let top = 40000;
    Object.defineProperty(f.el, "scrollTop", {
      get: () => top,
      set: (v: number) => {
        top = Math.max(0, Math.min(v, f.maxTop()));
      },
      configurable: true,
    });
    scroll.reconcileScrollRange();
    expect(f.el.scrollTop).toBe(0);
  });

  it("is a no-op when nothing armed it, so an out-of-band call cannot misfire", () => {
    const f = makeDeferredClampScrollEl(6000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(2000); // holding, well inside the range
    scroll.reconcileScrollRange();
    expect(f.el.scrollTop).toBe(2000);
    expect(scroll.isUserScrolledUp()).toBe(true);
  });

  it("consumes the arm, so a later pass that strands nothing writes nothing", () => {
    const f = makeDeferredClampScrollEl(85000, 600);
    scroll.init({ scrollEl: f.el });
    f.userScrollTo(84400);

    const before = f.el.scrollTop;
    f.setScrollHeight(600);
    scroll.noteContentShrink(before);
    scroll.reconcileScrollRange();
    expect(f.el.scrollTop).toBe(0);

    // Output resumes and the reader scrolls up to read it. A second call with
    // nothing armed must not drag them back to the tail.
    f.setScrollHeight(6000);
    f.userScrollTo(1000);
    scroll.reconcileScrollRange();
    expect(f.el.scrollTop).toBe(1000);
  });
});
