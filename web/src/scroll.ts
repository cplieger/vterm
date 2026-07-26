// Scroll controller: the single owner of the scroll container's scrollTop.
//
// One piece of state: `following`. The user is "following" when the viewport is
// at (or within a small tolerance of) the bottom; otherwise they have scrolled
// up to read and are "holding". The state is derived purely from the scroll
// events — position plus movement direction — with no debounce window, no
// suppress timer, and no programmatic-vs-user flag. That heuristic soup (a
// 100px tolerance, a 150ms debounce, a 60-second touch window) was the source
// of the view-jumping and scroll-interruption bugs (the legacy heuristic-soup
// failure mode; see the #web-terminal-engine steering doc, "Design rationale").
//
// The renderer calls stickToBottom() once after each flush: if following, pin
// to the new bottom; if holding, do nothing. Holding the reading position when
// content ABOVE it changes height (top-of-history eviction at the retention cap,
// the trim marker appearing) is adjustForContentShift's job, called by the
// renderer around its DOM mutations. That used to be left to native scroll
// anchoring (overflow-anchor), which WebKit has never shipped — so on Safari and
// iPadOS the read position slid up one line per evicted row while the user was
// scrolled up reading.
// Appending content at the bottom does not fire a scroll event, so following
// stays true across new output and the post-flush pin lands correctly. Pinning
// to the bottom produces a scroll event whose recomputation yields
// following=true again (no churn; the pin only ever moves DOWN). Scrolling
// back to within the tolerance of the bottom re-engages following.
//
// Disengaging is direction-based, not tolerance-based: ANY upward movement
// that leaves a real gap below flips to holding at once. A tolerance-only
// rule lost a race under heavy streaming — the renderer flushes (and pins)
// every frame, so each frame's upward scroll increment restarted from the
// bottom the previous pin reset it to, and unless a single frame's delta
// exceeded the tolerance the user was yanked back down every few milliseconds
// (the "scroll up fights me during output" bug). Per the HTML event loop the
// scroll steps run before that frame's rAF flush, so the first upward tick
// flips holding before the next pin can fire. The nothing-below guard keeps
// one non-user case following: when content SHRINKS (top-row eviction, a
// clear), the browser clamps scrollTop DOWN to the new maximum — an upward
// move that lands exactly at the bottom, which must not break auto-follow.

const BOTTOM_TOLERANCE_PX = 24;
// An upward move only disengages follow when a real gap is left below it.
// Bigger than 0 to absorb fractional-layout rounding in the shrink-clamp case
// (a clamp lands at the bottom, but scrollTop can be subpixel-off); far below
// any real one-frame user scroll increment.
const CLAMP_EPSILON_PX = 1;

let scrollEl: HTMLElement | null = null;
let following = true;
let lastScrollTop = 0;
let onFollowChange: ((scrolledUp: boolean) => void) | null = null;
let scrollHandler: (() => void) | null = null;

function distanceFromBottom(): number {
  if (!scrollEl) {
    return 0;
  }
  return scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight;
}

function atBottom(): boolean {
  return distanceFromBottom() <= BOTTOM_TOLERANCE_PX;
}

function setFollowing(next: boolean): void {
  if (next === following) {
    return;
  }
  following = next;
  if (onFollowChange) {
    onFollowChange(!following);
  }
}

/**
 * Initialize the scroll controller on the scroll container. The optional
 * callback fires whenever the follow state toggles (its argument is true when
 * the user has scrolled up / disengaged auto-follow).
 *
 * @param opts.scrollEl            Element whose scroll position is observed and owned.
 * @param opts.onUserScrollChange  Optional callback fired on follow/hold toggle.
 */
export function init(opts: {
  scrollEl: HTMLElement;
  onUserScrollChange?: (scrolledUp: boolean) => void;
}): void {
  // Detach any prior listener (re-init in tests / re-mount).
  if (scrollEl && scrollHandler) {
    scrollEl.removeEventListener("scroll", scrollHandler);
  }
  scrollEl = opts.scrollEl;
  onFollowChange = opts.onUserScrollChange ?? null;
  following = true;
  lastScrollTop = scrollEl.scrollTop;
  scrollHandler = () => {
    if (!scrollEl) {
      return;
    }
    const top = scrollEl.scrollTop;
    const movedUp = top < lastScrollTop;
    lastScrollTop = top;
    // Upward movement with content still below = the user pulling away from
    // the live tail: hold immediately, however small the move (see the header
    // comment for why the tolerance alone lost the streaming race). An upward
    // move that lands AT the bottom is the browser clamping after a content
    // shrink, not the user; everything else derives from position as before.
    if (movedUp && distanceFromBottom() > CLAMP_EPSILON_PX) {
      setFollowing(false);
    } else {
      setFollowing(atBottom());
    }
  };
  scrollEl.addEventListener("scroll", scrollHandler, { passive: true });
}

/**
 * Pin the viewport to the bottom iff the user is following. Called by the
 * renderer after each flush. A no-op when holding (scrolled up) or already at
 * the bottom, so it never fights the user and never scrolls redundantly.
 */
export function stickToBottom(): void {
  if (!scrollEl || !following) {
    return;
  }
  if (distanceFromBottom() > 0) {
    scrollEl.scrollTop = scrollEl.scrollHeight;
  }
}

/**
 * Force scroll to the bottom and re-engage following. Used by the explicit
 * jump-to-bottom control.
 */
export function scrollToBottom(): void {
  if (!scrollEl) {
    return;
  }
  scrollEl.scrollTop = scrollEl.scrollHeight;
  setFollowing(true);
}

/** Whether the user has scrolled away from the bottom (auto-follow disengaged). */
export function isUserScrolledUp(): boolean {
  return !following;
}

/**
 * The viewport's current scroll offset, for a consumer keeping per-view
 * scroll memory (a tabbed shell saving the position of the tab it is
 * leaving). Pairs with restoreScrollTop. This module owns the container's
 * scrollTop; consumers read it through this seam rather than the DOM element
 * so an engine-side change to the scroll geometry cannot silently break them.
 */
export function currentScrollTop(): number {
  return scrollEl ? scrollEl.scrollTop : 0;
}

/**
 * Shift the viewport by a content-height change that happened ABOVE the reading
 * position, so the user keeps looking at the same line. A no-op while following
 * (the bottom pin owns the position then) and when the delta is zero.
 *
 * This is scroll anchoring, done by hand. Chrome and Firefox do it natively
 * (`overflow-anchor`), and this module's design leaned on that: "if holding, do
 * nothing and let native scroll anchoring hold the reading position when history
 * is inserted above". WebKit has never shipped scroll anchoring, so on Safari
 * — including iPadOS, where this UI mostly lives — nothing compensated, and
 * every row evicted from the top of history while the user was scrolled up
 * reading slid their reading position one line further up. Over a streaming
 * agent session at the retention cap that reads as "the view scrolls itself
 * while I am trying to read".
 *
 * `following` is deliberately NOT touched, and neither is `lastScrollTop`: the
 * state stays derived purely from the resulting scroll event, exactly as it is
 * for the bottom pin. Both directions recompute to holding (an upward shift
 * leaves a gap below; a downward one is not at the bottom), so there is no churn
 * and no follow flip.
 */
export function adjustForContentShift(deltaPx: number): void {
  if (!scrollEl || following || deltaPx === 0) {
    return;
  }
  scrollEl.scrollTop += deltaPx;
}

/**
 * Restore a previously saved scroll offset (per-view scroll memory: a tabbed
 * shell re-entering a tab whose user had scrolled up to read). The offset is
 * applied verbatim; the follow/hold state then re-derives from the resulting
 * scroll event exactly as it does for a user scroll, so a restored read
 * position holds and a restored at-bottom position re-engages following.
 */
export function restoreScrollTop(top: number): void {
  if (!scrollEl) {
    return;
  }
  scrollEl.scrollTop = top;
}
