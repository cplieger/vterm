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
// flips holding before the next pin can fire.
//
// ENGAGING is direction-based too, and asymmetrically so: only a move DOWNWARD
// that lands at the bottom re-engages. An upward move never engages, and a
// scroll event that did not move the position infers nothing. This asymmetry is
// what makes a content SHRINK safe. A shrink (top-row eviction at the retention
// cap, ED3 erasing scrollback, a wipe-and-rebuild) lowers scrollHeight, so the
// browser clamps scrollTop DOWN to the new maximum and delivers that clamp as an
// UPWARD move that lands at the bottom. The clamp is indistinguishable by
// position from a user arriving at the tail, but not by direction: a user
// returning to the tail always moves down, and a clamp always moves up. Before
// the asymmetry, deriving the state from position alone meant any shrink
// silently re-engaged auto-follow under a user who had deliberately scrolled up
// to read, and every subsequent line then pinned them to the bottom. Measured on
// an inline TUI that erases scrollback on each redraw: scrolled up 20000px, an
// ED3 collapsed the content, and the viewport was pinned to the tail from there
// on. The rule now: follow may be engaged by init, by an explicit
// scrollToBottom/restoreView, or by a downward user scroll reaching the bottom.
// Nothing else.
//
// The two library writes that happen WHILE HOLDING and must not change the
// state (adjustForContentShift's anchor correction, restoreView's explicit
// restore) say so directly, by arming a one-event pass-through for the single
// scroll event their own write produces. That is deliberately not a general
// "was this event ours?" mechanism: this module does not own every write to the
// container (a consumer may page the viewport or animate a smooth jump through
// the DOM), and per CSSOM View an element enqueues at most one scroll event per
// frame, so a same-frame library write and user gesture are indistinguishable
// anyway. The arm is set only when the write actually moved the position, so it
// can never linger and swallow a later real gesture; every write from outside
// this module is classified by direction like any user scroll, which is correct
// for all of them (paging up holds, paging down or a smooth jump to the bottom
// re-engages on landing).

// A content SHRINK is ANNOUNCED, not inferred. The classification above works
// off the clamp's arithmetic signature — an upward move that lands within
// CLAMP_EPSILON_PX of the bottom — and that signature is lossy: scrollHeight
// and clientHeight are integer-rounded while scrollTop is fractional, so under
// browser zoom or a fractional device pixel ratio the residual can exceed the
// epsilon and a clamp reads as a user scrolling up. The reader is then left
// "holding at the bottom" — visually at the tail with auto-follow silently off,
// a state only a keystroke or the jump button clears, and one that survives a
// tab switch (restoreView carries it deliberately). See
// docs/scroll-position-fidelity.md §1.3.
//
// The layer that REMOVES the rows knows a shrink happened, so it says so:
// noteContentShrink() arms a one-event pass-through for the clamp its own
// mutation caused. The epsilon keeps its original and only job, absorbing
// subpixel residual, and stops being the thing that IDENTIFIES a clamp.
//
// The arm carries the same discipline as preserveFollowOnce, for the same
// reason: it is set only when the position ACTUALLY moved (observed after the
// mutation, never predicted from a row count), so an event is guaranteed to
// follow and a lingering arm cannot swallow a later real gesture. It is also
// cleared unconditionally by the first event that arrives, so it cannot outlive
// one frame even if that guarantee were ever broken.
const BOTTOM_TOLERANCE_PX = 24;
// An upward move only disengages follow when a real gap is left below it.
// Bigger than 0 to absorb fractional-layout rounding in the shrink-clamp case
// (a clamp lands at the bottom, but scrollTop can be subpixel-off); far below
// any real one-frame user scroll increment.
const CLAMP_EPSILON_PX = 1;

let scrollEl: HTMLElement | null = null;
let following = true;
let lastScrollTop = 0;
// Armed by a library write that must PRESERVE the current follow state across
// the single scroll event it produces (see the header). Consumed by that one
// event; never armed when the write did not move the position, because then no
// event fires and a lingering arm would swallow the next real gesture.
let preserveFollowOnce = false;
// Armed by noteContentShrink when a caller's own row removal moved the
// position: the next event is that clamp, not a gesture. Consumed (and in all
// cases cleared) by the first event to arrive.
let shrinkArmed = false;
let onFollowChange: ((scrolledUp: boolean) => void) | null = null;
let onPosition: (() => void) | null = null;
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
 * callbacks fire whenever the follow state toggles (its argument is true when
 * the user has scrolled up / disengaged auto-follow), and — separately — on
 * every scroll event that reflects a real position change.
 *
 * @param opts.scrollEl            Element whose scroll position is observed and owned.
 * @param opts.onUserScrollChange  Optional callback fired on follow/hold toggle.
 * @param opts.onScrollPosition    Optional callback fired on every real scroll move.
 */
export function init(opts: {
  scrollEl: HTMLElement;
  onUserScrollChange?: (scrolledUp: boolean) => void;
  onScrollPosition?: () => void;
}): void {
  // Detach any prior listener (re-init in tests / re-mount).
  if (scrollEl && scrollHandler) {
    scrollEl.removeEventListener("scroll", scrollHandler);
  }
  scrollEl = opts.scrollEl;
  onFollowChange = opts.onUserScrollChange ?? null;
  onPosition = opts.onScrollPosition ?? null;
  following = true;
  lastScrollTop = scrollEl.scrollTop;
  preserveFollowOnce = false;
  shrinkArmed = false;
  scrollHandler = () => {
    if (!scrollEl) {
      return;
    }
    const top = scrollEl.scrollTop;
    const prev = lastScrollTop;
    lastScrollTop = top;
    // Read-and-clear: whatever this event turns out to be, the announcement
    // does not survive it. An arm that could outlive one event is the bug
    // preserveFollowOnce's "only when it moved" rule exists to prevent, and a
    // coalesced event that nets out downward must not leave one behind.
    const wasShrink = shrinkArmed;
    shrinkArmed = false;
    if (preserveFollowOnce) {
      // This event is the echo of a library write that carries its own follow
      // intent; the state is already correct.
      preserveFollowOnce = false;
      return;
    }
    // The POSITION seam, fired after the early-return above and before the
    // follow decision below. Both halves of that placement are load-bearing
    // (docs/paged-scrollback.md §5.4): after, because the swallowed event is
    // the echo of the library's OWN write — a paged-in prepend goes through
    // adjustForContentShift, and firing the seam for it would turn every
    // prepend into a fresh fetch trigger, a self-feeding loop; and separate
    // from onFollowChange, because that one fires only on a follow/hold TOGGLE,
    // so an idle session where the reader scrolls within history would never
    // notify at all — and idle browsing is exactly when paging must work.
    //
    // Deliberately a BARE notification: no index, no transport. scroll.ts stays
    // index-free and transport-free; the renderer owns the mapping from scroll
    // position to absolute index and decides whether to fetch.
    //
    // NOT "the user scrolled". This sits before the direction branch below, so
    // it also fires for the browser's own clamps — an eviction, an ED3 wipe, a
    // consumer restyling the surface — i.e. every position change that was not
    // the library's own arming write. That is harmless because the fetch
    // trigger re-evaluates its full guard set on each call and asks for nothing
    // when the geometry says nothing is missing, but a reader who mistakes this
    // for a gesture signal will wire the wrong thing to it.
    if (onPosition) {
      onPosition();
    }
    if (top < prev) {
      // Upward: either the user pulling away from the live tail, or the browser
      // clamping after a content shrink. Neither may ENGAGE follow (see the
      // header). Only the former leaves a real gap below, and only that
      // disengages.
      //
      // An ANNOUNCED shrink is the clamp, told to us by the layer that removed
      // the rows, so the state is preserved without consulting the epsilon at
      // all. The epsilon still backs the un-announced case (a shrink from
      // outside this library's mutation paths, e.g. a consumer restyling the
      // surface).
      if (wasShrink) {
        return;
      }
      if (distanceFromBottom() > CLAMP_EPSILON_PX) {
        setFollowing(false);
      }
      return;
    }
    if (top > prev) {
      // Downward: position decides. Reaching the bottom re-engages follow;
      // stopping short of it holds.
      setFollowing(atBottom());
    }
    // Unmoved: a scroll event that changed nothing implies no intent, and the
    // position it reports may be one a shrink clamp already put us at.
  };
  scrollEl.addEventListener("scroll", scrollHandler, { passive: true });
}

/**
 * Write the container's scroll offset on the library's own behalf, preserving
 * the current follow state across the resulting scroll event. `lastScrollTop` is
 * synced to the POST-clamp value so the next event's direction is computed from
 * where the container actually landed, and the one-event pass-through is armed
 * only when the position really moved (no move means no event to consume).
 */
function writePreservingFollow(next: number): void {
  if (!scrollEl) {
    return;
  }
  const before = scrollEl.scrollTop;
  scrollEl.scrollTop = next;
  const after = scrollEl.scrollTop;
  lastScrollTop = after;
  if (after !== before) {
    preserveFollowOnce = true;
  }
}

/**
 * Announce that the caller's own mutation just REMOVED content, so the scroll
 * event it produced is the browser's clamp rather than a user gesture, and the
 * follow state must survive it.
 *
 * Call it AFTER the mutation, passing the offset read BEFORE it. The arm is set
 * only when the position actually moved, which is what makes it safe: a clamp
 * that moved the position guarantees an event to consume the arm, and a
 * mutation that did not move the position arms nothing. Predicting the movement
 * from a row count instead would arm on every removal — including the many that
 * cannot clamp (rows removed below the viewport, a removal smaller than the
 * remaining bottom gap, a wipe whose scrollHeight is held up by an
 * absolutely-positioned overlay) — and a lingering arm swallows the next real
 * gesture.
 *
 * That last case used to be live here rather than illustrative, and it is why
 * the caller's ORDER matters: the caret, the predicted cursor, the IME view and
 * the consumer's hidden textarea all sit inside the scroll container carrying a
 * `top` in CONTENT coordinates, so any one of them left at the old offset holds
 * scrollHeight above the built content and the clamp never happens. `rebuild`
 * now collapses all four as part of its wipe, before it calls this
 * (`render.ts` collapseContentSpaceOverlays), so the clamp it announces is real.
 * A caller that removes content without collapsing whatever else is anchored in
 * that space arms nothing and gets no announcement. See
 * `docs/tab-switch-repaint.md` §6.2.
 *
 * A native-scroll-anchoring adjustment (Chrome/Firefox lowering scrollTop as
 * rows are removed above the viewport) also moves the position and also arms
 * this; that is correct rather than incidental, since it is likewise the
 * library's own mutation moving the viewport and not the user.
 *
 * @param scrollTopBefore  The container's offset read before the mutation.
 */
export function noteContentShrink(scrollTopBefore: number): void {
  if (!scrollEl) {
    return;
  }
  if (scrollEl.scrollTop < scrollTopBefore) {
    shrinkArmed = true;
  }
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
 * `following` is deliberately NOT changed: this is a library correction, not a
 * user gesture, so it must leave the reader holding exactly as they were. The
 * write goes through writePreservingFollow, which syncs the direction baseline
 * and lets the resulting scroll event pass through without re-deriving the
 * state. Deriving it instead would let a correction that happens to land the
 * reader at the bottom silently re-engage auto-follow.
 */
export function adjustForContentShift(deltaPx: number): void {
  if (!scrollEl || following || deltaPx === 0) {
    return;
  }
  writePreservingFollow(scrollEl.scrollTop + deltaPx);
}

/**
 * Restore a saved view: both the scroll offset and the follow state, together
 * and explicitly. For a consumer keeping per-view scroll memory (a tabbed shell
 * re-entering a tab), this is the correct call. `restoreScrollTop` writes only
 * the position and lets the state re-derive, which cannot express a view that
 * was holding AT the bottom — a state that is reachable, because a content
 * shrink under a scrolled-up reader clamps them to the bottom without engaging
 * follow.
 *
 * A non-finite offset is ignored rather than assigned (the DOM would coerce NaN
 * to 0 and silently jump to the top of history); the follow state is still
 * applied, since the caller's intent for it is unambiguous.
 */
export function restoreView(view: { top: number; following: boolean }): void {
  if (!scrollEl) {
    return;
  }
  if (Number.isFinite(view.top)) {
    writePreservingFollow(view.top);
  }
  setFollowing(view.following);
}

/**
 * Restore a previously saved scroll offset (per-view scroll memory: a tabbed
 * shell re-entering a tab whose user had scrolled up to read). Position only:
 * the follow/hold state then re-derives from the resulting scroll event exactly
 * as it does for a user scroll, so a restored read position holds and a
 * restored at-bottom position re-engages following ONLY if the restore moved
 * downward to reach it. Prefer restoreView when the saved view carries a follow
 * state, which is the only way to express "holding at the bottom".
 *
 * @deprecated A pixel offset cannot identify a reading position: replayed into
 * a surface whose rows are still being built it is silently clamped, and
 * replayed into one whose content grew it points at a different line. Use
 * `render.captureViewMemory()` + `render.bind(store, { view })`, which restore
 * a LINE (see docs/scroll-position-fidelity.md §3). Kept for one release for
 * any external consumer holding pixel-space view memory; removed in the next
 * major.
 */
export function restoreScrollTop(top: number): void {
  if (!scrollEl) {
    return;
  }
  scrollEl.scrollTop = top;
}
