// @vitest-environment happy-dom
//
// Brick-4 scroll controller. happy-dom has no real layout, so scrollHeight /
// clientHeight are overridden to drive the follow/hold state machine
// deterministically: following derives from scroll position plus movement
// direction (any upward move with content below holds; a shrink-clamp landing
// at the bottom keeps following), and stickToBottom only pins when following.

import { describe, it, expect, beforeEach } from "vitest";
import * as scroll from "./scroll.js";

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
    scrollTo(el, 0); // distance from bottom = 700 -> holding
    expect(scroll.isUserScrolledUp()).toBe(true);
    expect(changes).toEqual([true]);
    scrollTo(el, 700); // distance = 0 -> following
    expect(scroll.isUserScrolledUp()).toBe(false);
    expect(changes).toEqual([true, false]);
  });

  it("treats within-tolerance as following (24px)", () => {
    scrollTo(el, 680); // distance = 20 (<= 24) -> following
    expect(scroll.isUserScrolledUp()).toBe(false);
    scrollTo(el, 669); // distance = 31 (> 24) -> holding
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
    expect(el.scrollTop).toBe(1000); // pinned to scrollHeight
  });

  it("stickToBottom does nothing while holding (does not yank the reader)", () => {
    scrollTo(el, 100); // holding
    scroll.stickToBottom();
    expect(el.scrollTop).toBe(100); // unchanged
  });

  it("scrollToBottom forces the bottom and re-engages following", () => {
    scrollTo(el, 0); // holding
    expect(scroll.isUserScrolledUp()).toBe(true);
    scroll.scrollToBottom();
    expect(el.scrollTop).toBe(1000);
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
    el.dispatchEvent(new Event("scroll")); // happy-dom doesn't auto-fire it
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
});
