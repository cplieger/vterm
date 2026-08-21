// Render layer: store-backed, absolute-index DOM rows.
//
// The renderer owns a LineStore (the authoritative client model) and reflects
// it to the DOM. Every terminal line is a `div.term-row` carrying a `data-abs`
// attribute equal to its absolute index; the rows sit in one natively-scrolled
// container in absolute order. There is no separate "live zone" and
// "scrollback": the live window is simply the last `height` absolute indices,
// and a line that scrolls into history just stops being updated. This is what
// removes the live/history reconciliation that caused the duplicate-rows and
// view-jumping bugs.
//
// Decode frames feed the store (handleScreen/handleScroll); a single
// requestAnimationFrame flush drains the store's change set and applies it:
// evicted indices drop their row, dirty indices build/update their row in
// place. The window block always has `height` rows, so scrollHeight only grows
// when real history commits — never oscillating mid-redraw.

import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";
import { LineStore, PAGE_SIZE, PREFETCH_THRESHOLD } from "./store.js";
import * as scroll from "./scroll.js";
import { isReverseVideo } from "./modes.js";
import { interiorGaps } from "./intervals.js";

// --- Width cache (two-tier, xterm.js style) ---
const WIDTH_FLAT_SIZE = 256;
const WIDTH_FLAT_UNSET = -9999;
const widthFlat = new Float32Array(WIDTH_FLAT_SIZE).fill(WIDTH_FLAT_UNSET);
const widthMap = new Map<string, number>();

const VARIANT_REGULAR = 0;
const VARIANT_BOLD = 1;
const VARIANT_ITALIC = 2;
// widthMap holds one entry per unique (bold, italic, glyph) measured — bounded
// by the rendered repertoire in practice, but a long CJK/emoji-heavy session
// can accumulate tens of thousands of keys that survive eviction, reset, and
// tab close (only a font change clears it). Cap it: a clear on overflow costs
// an occasional re-measure and changes no rendered output.
const WIDTH_MAP_MAX = 20_000;
const variantCtx: (CanvasRenderingContext2D | null)[] = [null, null, null, null];
let fontString = "";

function variantContext(variant: number): CanvasRenderingContext2D {
  let ctx = variantCtx[variant];
  if (ctx) {
    return ctx;
  }
  const canvas = document.createElement("canvas");
  canvas.width = 1;
  canvas.height = 1;
  // eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- 2d context always available on fresh canvas
  ctx = canvas.getContext("2d")!;
  let f = "";
  if (variant & VARIANT_ITALIC) {
    f += "italic ";
  }
  if (variant & VARIANT_BOLD) {
    f += "bold ";
  }
  f += fontString;
  ctx.font = f;
  variantCtx[variant] = ctx;
  return ctx;
}

function resetVariantContexts(): void {
  for (let i = 0; i < variantCtx.length; i++) {
    variantCtx[i] = null;
  }
}

function measureChar(ch: string, bold: boolean, italic: boolean): number {
  if (!bold && !italic && ch.length === 1) {
    const cp = ch.charCodeAt(0);
    if (cp < WIDTH_FLAT_SIZE) {
      // eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- bounds checked above
      const cached = widthFlat[cp]!;
      if (cached !== WIDTH_FLAT_UNSET) {
        return cached;
      }
      const w = variantContext(VARIANT_REGULAR).measureText(ch).width;
      widthFlat[cp] = w;
      return w;
    }
  }
  const key = (bold ? "B" : "") + (italic ? "I" : "") + ch;
  const cached = widthMap.get(key);
  if (cached !== undefined) {
    return cached;
  }
  let variant = 0;
  if (bold) {
    variant |= VARIANT_BOLD;
  }
  if (italic) {
    variant |= VARIANT_ITALIC;
  }
  const w = variantContext(variant).measureText(ch).width;
  if (widthMap.size >= WIDTH_MAP_MAX) {
    widthMap.clear();
  }
  widthMap.set(key, w);
  return w;
}

function measureCellWidth(): number {
  // Measure using a span appended to termWrap (which already has the
  // font applied via CSS). This ensures the web font is used if loaded.
  const span = document.createElement("span");
  span.style.visibility = "hidden";
  span.style.position = "absolute";
  span.style.whiteSpace = "pre";
  span.textContent = "MMMMMMMMMM";
  termWrap.appendChild(span);
  const width = span.getBoundingClientRect().width / 10;
  termWrap.removeChild(span);
  return width;
}

// --- State ---
let output: HTMLElement;
let termWrap: HTMLElement;

// The store the renderer reflects. Module-private by default (a consumer that
// never calls bind() gets one implicit store, the original single-terminal
// behavior). The tabs feature keeps one LineStore per tab and calls bind() on
// switch to point the renderer at the active tab's cache (design section 6).
let store = new LineStore();
// abs index -> its row element. The DOM children of `output` are these
// elements, kept in ascending data-abs order.
const rowEls = new Map<number, HTMLDivElement>();

// The "earlier output trimmed" marker (a non-data-abs first child of output,
// shown when the store reports history older than it holds was trimmed). Kept
// as a module ref so it is reused rather than recreated each flush.
let trimMarkerEl: HTMLDivElement | null = null;

/**
 * Per-gap "earlier output not loaded" markers, keyed by the gap's LOW index.
 *
 * Distinct from the singleton trim marker above in two ways that matter. Each
 * one carries its gap's low index as `data-abs`, so `insertRowInOrder` and the
 * read-anchor binary searches keep their monotonic-`data-abs` invariant with
 * the marker in place — the trim marker can be a `data-abs`-less first child
 * precisely because it only ever sits at the very top, and an INTERIOR marker
 * cannot. And a marker here is a PROJECTION of the store's gap geometry: it is
 * re-derived (value and position) whenever either edge of its gap moves, so a
 * gap healing from its low edge carries its marker up with it, and it is
 * removed when the gap closes (docs/paged-scrollback.md §5.4).
 */
const gapMarkerEls = new Map<number, HTMLDivElement>();

/** Transport seams for demand paging, wired by the consumer at init. */
let requestHistoryFn: ((fromAbs: number, maxLines: number) => boolean) | null = null;
let historyBudgetFn: (() => number) | null = null;

// Rows awaiting a DOM (re)build, processed in budgeted batches across frames.
// A session restore (kiro-cli's /chat), a post-resize transcript reprint, or a
// `cat bigfile` dumps thousands of lines in one wire frame; building them all
// in a single rAF janks or, on a constrained device, hangs the tab. The store
// still ingests the whole burst at once (it is cheap, pure data); the renderer
// drains this queue at most MAX_ROWS_PER_FRAME per frame and reschedules until
// it is empty, so each frame stays short. The drain is VIEWPORT-FIRST each
// frame (see flushRenderInner): live-window rows build before any scrollback
// backlog, and scrollback drains newest->oldest so the backlog fills upward,
// above the bottom-pinned viewport — offscreen — instead of visibly streaming
// through it. The cursor row is always built regardless of the budget so the
// caret never lags.
const renderQueue = new Set<number>();
const MAX_ROWS_PER_FRAME = 300;

// Cursor state, refreshed from the store window on each flush. The caret is
// painted by a dedicated overlay element (see positionCursorOverlay), NOT by
// restyling the span at the cursor cell: rows are pure content, so cursor
// motion never rewrites row DOM — which preserves any native selection in the
// old/new cursor rows and deletes the per-keystroke row rebuild entirely
// (judgement finding: changed-row replaceChildren collapsed a row-local
// selection).
let cursorAbs = -1; // absolute index of the row the cursor is on
let cursorCol = 0;
let cursorHidden = false;
let cursorStyleVal = 0; // 0-6: DECSCUSR

function cursorClassName(): string {
  // DECSCUSR: 0/1=blinking block, 2=steady block, 3=blinking underline,
  // 4=steady underline, 5=blinking bar, 6=steady bar
  if (cursorStyleVal === 3 || cursorStyleVal === 4) {
    return "term-cursor-underline";
  }
  if (cursorStyleVal === 5 || cursorStyleVal === 6) {
    return "term-cursor-bar";
  }
  return "term-cursor";
}
let cellWidth = 8;
let cellHeight = 17;
let defaultSpacing = 0;
let onCursorMove: (() => void) | null = null;
let pendingFrame: number | undefined;

// termWrap's padding, cached: the overlay positioners (caret, predicted
// cursor, getCursorPx for the IME view) need it EVERY flush, and a live
// getComputedStyle after the flush's DOM writes forces a style recalc —
// measured at real cost in the render bench (the caret positioner alone was
// ~11% of flush CPU under full-screen churn). Lazily read once per attach and
// refreshed by updateFontMetrics (the same staleness contract as
// cellWidth/cellHeight: a consumer that restyles the terminal calls
// updateFontMetrics, which re-reads both).
let padLeft = 0;
let padTop = 0;
let padValid = false;

function termPadding(): { padL: number; padT: number } {
  if (!padValid) {
    const cs = window.getComputedStyle(termWrap);
    padLeft = parseFloat(cs.paddingLeft);
    padTop = parseFloat(cs.paddingTop);
    padValid = true;
  }
  return { padL: padLeft, padT: padTop };
}

// Bounded error-path reschedule (l-f28 / d-u4-1). The drain loop deletes a
// queued row only AFTER upsertRow succeeds, so a row whose build throws stays
// queued. flushRender's catch reschedules to finish a partial drain and to
// retry a transient throw (a font/measureText race), but a row that throws
// deterministically would otherwise turn catch -> rAF -> throw into a ~60fps
// busy loop (CPU/battery burn + per-frame console spam) that never stops, even
// on an idle session. `flushDrainedThisPass` records forward progress (queued
// rows actually built this pass; reset at each flushRenderInner entry, and
// visible to the catch because a mid-drain throw leaves it at the count so
// far). `renderNoProgressStreak` counts consecutive passes that threw with zero
// progress; once it passes the cap the catch stops rescheduling and lets the
// next inbound frame retry (the pre-l-f28 behavior for a stuck row).
let flushDrainedThisPass = 0;
let renderNoProgressStreak = 0;
const MAX_RENDER_NO_PROGRESS_RETRIES = 3;
// The stall canary's latch (see the reschedule rule near flushRender). Reset by
// init, and by any flush that ends in a healthy state, so it suppresses a repeat
// of ONE stall rather than every stall for the life of the process.
let unnamedStallReported = false;
// Whether the CURRENT flush performed a full reset (server restart / epoch
// change). Read by restoreReadAnchor after the flush: a re-anchor across a
// reset would match unrelated content from the new index space (R2 review).
let fullResetThisPass = false;
// The ED3 base observed since the last flush, or -1. Set by handleScreen from
// the frame itself; consumed (and reset) by the pass that applies it. Renderer-
// local on purpose: the renderer is the caller of every path that discards a
// REGION rather than trimming the cap, so nothing in the store's change set has
// to grow a field for it (docs/scroll-position-fidelity.md §5).
let discardedBelowPending = -1;
let discardedBelowThisPass = -1;
// Whether THIS pass removed rows from the DOM. Only a pass that did can have
// caused a clamp, and announcing one that did not is unsound on Safari, which
// updates scrollTop PAST the maximum during an overscroll bounce: the settle back
// from a rubber-band is a downward move in value with no content change at all,
// and arming for it would hand the user's own gesture a pass-through.
let removedRowsThisPass = false;

/**
 * Initialize the renderer by attaching it to a pair of DOM elements: the
 * scrollable terminal wrapper and the inner output container that receives
 * row elements. Must be called once before any handleScreen/handleScroll call.
 *
 * @param opts.output      Inner element that holds row children.
 * @param opts.termWrap    Outer scroll container.
 * @param opts.onCursorMove Optional callback invoked when the cursor moves.
 * @param opts.maxLines    Optional retained-line cap for the implicit store
 *                         (default 5000) — a HISTORY budget floored at the
 *                         live screen (the store never evicts the current
 *                         window, so a cap at or below the terminal height
 *                         keeps the full screen with no scrollback). Governs
 *                         how many absolute-index lines — and therefore DOM
 *                         rows — the client keeps; a memory-constrained
 *                         consumer (a phone) passes a smaller budget. A
 *                         consumer that manages stores itself (a tabbed
 *                         shell) constructs each `LineStore` with its own cap
 *                         and `bind()`s it AFTER init; init always installs a
 *                         fresh implicit store, so call it before any bind.
 *                         For the batched eviction to help, choose a cap
 *                         comfortably above the terminal height (near or
 *                         below it, eviction degrades to per-line — the
 *                         screen stays correct, but the churn returns).
 *                         Non-positive or non-integer values warn and are
 *                         ignored (the default applies).
 */
export function init(opts: {
  output: HTMLElement;
  termWrap: HTMLElement;
  onCursorMove?: () => void;
  maxLines?: number;
  /** Ask the transport for a page of history. Wired by the consumer to
   *  `connection.requestHistory`; absent means paging is not available and the
   *  controller stays dormant (docs/paged-scrollback.md §5.4). */
  requestHistory?: (fromAbs: number, maxLines: number) => boolean;
  /** The transport's current adaptive request budget
   *  (`connection.historyBudget`). Read at FIRE time, and used for both the
   *  request's length and its anchor: a shrunken length with a full-size anchor
   *  serves a range that ends far from the reader. */
  historyBudget?: () => number;
}): void {
  output = opts.output;
  termWrap = opts.termWrap;
  onCursorMove = opts.onCursorMove ?? null;
  requestHistoryFn = opts.requestHistory ?? null;
  historyBudgetFn = opts.historyBudget ?? null;
  // New attach target: the cached padding belongs to the previous termWrap.
  padValid = false;
  // Fresh attach: ALWAYS install a fresh implicit store, so no cap — a
  // consumer-configured one, or a bound per-tab store's — leaks from a
  // previous attachment into this one (init is the attachment boundary;
  // vitest's non-isolated module reuse relies on the same reset).
  const validCap =
    opts.maxLines !== undefined && Number.isInteger(opts.maxLines) && opts.maxLines > 0;
  if (opts.maxLines !== undefined && !validCap) {
    console.warn(`vterm: ignoring invalid maxLines ${String(opts.maxLines)}`);
  }
  // An absent OR invalid cap is forwarded as absent, which is how the store
  // spells "engine's choice"; a value here would be indistinguishable from a
  // consumer decision and would pin the tail at the compatibility cap forever.
  store = new LineStore(validCap ? opts.maxLines : undefined);
  rowEls.clear();
  renderQueue.clear();
  trimMarkerEl = null;
  for (const el of gapMarkerEls.values()) {
    el.remove();
  }
  gapMarkerEls.clear();
  // Drop the predicted-cursor + caret overlays (re-created lazily against the
  // new termWrap) so re-init starts clean.
  if (predCursorEl) {
    predCursorEl.remove();
    predCursorEl = null;
  }
  if (cursorEl) {
    cursorEl.remove();
    cursorEl = null;
  }
  output.replaceChildren();
  cursorAbs = -1;
  if (pendingFrame !== undefined) {
    cancelAnimationFrame(pendingFrame);
    pendingFrame = undefined;
  }
  flushDrainedThisPass = 0;
  renderNoProgressStreak = 0;
  // The stall canary latches so it cannot spam, and init is the attachment
  // boundary, so the latch resets here with every other piece of pass state. A
  // latch that survived would make the canary a once-per-PROCESS report, silent
  // for every later attachment and for every genuinely new stall.
  unnamedStallReported = false;
  // Attachment boundary: a view restore armed against the PREVIOUS surface must
  // not survive it, or its timer outlives teardown and writes scrollTop on a
  // detached element. Same discipline scroll.init applies to its own arms.
  bindGen++;
  clearPendingRestore();
  discardedBelowPending = -1;
  discardedBelowThisPass = -1;
  lastInboundMs = 0;
  syncCursorBlink();
}

/**
 * Reset internal screen state so the next frame performs a full repaint.
 * With the store model this is a full reset (used on server restart): the
 * store clears and the next flush wipes and rebuilds the DOM.
 */
export function resetScreen(): void {
  store.reset();
  scheduleFlush();
}

/**
 * Clear all rows (history + window). Used on server restart alongside
 * resetScreen; both reset the store, so this is equivalent.
 */
export function resetScrollback(): void {
  store.reset();
  scheduleFlush();
}

/**
 * Bind the renderer to a different store and rebuild the surface from it. The
 * tabs feature calls this on every switch to point the one renderer at the
 * active tab's cached LineStore (design sections 5, 6, 8). The DOM is wiped and
 * repainted viewport-first from the new store; this is local, so the last-known
 * screen paints without a network round-trip.
 *
 * `opts.view` is the per-view scroll memory `captureViewMemory()` returned when
 * the consumer last left this store, and passing it makes the swap ATOMIC. Both
 * halves matter and they land at different times on purpose:
 *
 *   - the FOLLOW half is adopted SYNCHRONOUSLY, before the wipe, so the very
 *     first flush's bottom pin is gated on the INCOMING view's state. The
 *     follow flag is global (one per kernel), so without this the first frame
 *     after a switch is gated on the state of the tab the user just LEFT —
 *     binding a following tab while holding in another rendered the cached
 *     screen above the viewport and left a black gap until a touch re-engaged
 *     follow. It is applied through restoreView at the CURRENT offset, which
 *     sets the flag without moving the position.
 *   - the POSITION half is armed and re-asserted across the rebuild's frames
 *     (see applyPendingRestore), because the row it names is usually not built
 *     yet. A view left FOLLOWING arms nothing: the per-flush bottom pin already
 *     is the correct answer, which keeps the common path free of any restore.
 */
export function bind(next: LineStore, opts?: { view?: ViewMemory | null }): void {
  const view = opts?.view ?? null;
  // Passing `opts` at all is a statement that this caller owns the view. A NULL
  // view is therefore not "no opinion" but "no memory" — a tab never visited, or
  // one left on the alternate screen, where there is no absolute index worth
  // remembering — and the right follow state for no memory is the tail. Without
  // this, those tabs inherited the OUTGOING tab's follow flag, which is the
  // stale-global-flag bug this seam exists to close, just narrowed to the tabs
  // that have nothing saved. A caller that passes no `opts` keeps the pre-3.7
  // behavior and its follow state is left alone.
  if (opts !== undefined) {
    scroll.restoreView({
      top: scroll.currentScrollTop(),
      following: view === null ? true : view.following,
    });
  }
  // The OUTGOING store cannot have a request in flight: the socket that issued it
  // is being switched away with it. Closing its solicited window here is what
  // stops it stranding open, because the transport's own `clearSolicited` lands on
  // whichever store is bound WHEN IT FIRES — and the order of `bind` against the
  // consumer's `setSession`/reconnect teardown is the consumer's choice, which
  // neither module constrains. Bound after the switch, that clear closes the
  // INCOMING store's (empty) window and leaves the outgoing one permanently open:
  // a standing exemption from apply-line guard 2 for that index range, with no
  // socket and no timer left to close it, so any later frame in that range can
  // resurrect an evicted row and be classified as browse cache. The sibling
  // pendingRestore arm already had a generation guard for the same hazard; this
  // one had none.
  store.clearSolicited();
  store = next;
  // A new bind invalidates any restore armed for the previous one: cancel, then
  // arm, one slot, so a second switch mid-drain cannot land the first tab's
  // anchor into this store.
  bindGen++;
  clearPendingRestore();
  rebuild();
  if (view !== null && !view.following) {
    pendingRestore = {
      view,
      gen: bindGen,
      lastWrote: scroll.currentScrollTop(),
      deadline: Date.now() + RESTORE_MAX_MS,
    };
  }
}

/**
 * The store the renderer is currently bound to. Exposed so the shell can feed
 * decoded frames into the active tab's store and read its resume bounds.
 */
export function boundStore(): LineStore {
  return store;
}

/**
 * Queue every retained line for building, viewport-first: the live window rows
 * (what the user sees) in ascending order first, then scrollback newest->oldest
 * so rows adjacent to the viewport fill before deep history. Iterates the
 * retained key set (forEachLine), NOT the integer range [oldest, highest], so a
 * sparse store (a frame whose base jumped far from a retained index) never
 * freezes the drain. Shared by the two wipe-and-rebuild-from-store paths
 * (rebuild() and the alt-exit branch in flushRenderInner) so both order rows
 * identically; the per-frame budget then spreads a large backlog across frames
 * without janking.
 */
function queueRowsViewportFirst(): void {
  const winBase = store.getWindow().base;
  const inWindow: number[] = [];
  const belowWindow: number[] = [];
  store.forEachLine((abs) => {
    if (abs >= winBase) {
      inWindow.push(abs);
    } else {
      belowWindow.push(abs);
    }
  });
  for (const abs of inWindow) {
    renderQueue.add(abs);
  }
  for (let i = belowWindow.length - 1; i >= 0; i--) {
    // eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- index in range
    renderQueue.add(belowWindow[i]!);
  }
}

/**
 * Wipe the DOM and rebuild it from the current store, viewport-first and
 * budgeted. The live window (what the user sees) builds first, then scrollback
 * from newest to oldest so rows adjacent to the viewport fill in before deep
 * history; the existing per-frame budget spreads a large backlog across frames
 * so the switch never janks. Used by bind(); also safe to call directly to
 * force a full repaint of the current store.
 *
 * The wipe covers content space in full: the rows AND the four overlays anchored
 * into it (see collapseContentSpaceOverlays). Leaving the overlays behind left
 * the container a measured 81081px of scroll range over zero rows of content,
 * which is a black pane the bottom pin refuses to correct.
 */
export function rebuild(): void {
  // The wipe is the largest content shrink this module performs, and it happens
  // OUTSIDE a flush (bind calls it synchronously), so the flush's own
  // announcement cannot cover it. Unannounced, the clamp it causes falls back to
  // the epsilon — exactly the inference §4 exists to stop relying on.
  const scrollTopBeforeWipe = scroll.currentScrollTop();
  output.replaceChildren();
  rowEls.clear();
  renderQueue.clear();
  trimMarkerEl = null;
  for (const el of gapMarkerEls.values()) {
    el.remove();
  }
  gapMarkerEls.clear();
  cursorAbs = -1;
  altRendered = false;
  altPrevRows = [];
  // Fresh surface: a stale give-up streak from a prior store must not deny the
  // rebuilt surface its full transient-retry budget.
  renderNoProgressStreak = 0;
  // The rows are not the only thing in content space. Collapse the overlays
  // with them, BEFORE the offset below is read.
  collapseContentSpaceOverlays();
  // Alt screen paints from the ephemeral grid in the flush, so it needs no
  // absolute-index queueing here.
  if (!store.isAlt()) {
    queueRowsViewportFirst();
  }
  scroll.noteContentShrink(scrollTopBeforeWipe);
  scheduleFlush();
}

/**
 * Drop every CONTENT-SPACE overlay's hold on the scroll container, as part of a
 * wipe. Four elements sit INSIDE the container carrying a `top` in content
 * coordinates — the caret, the predicted cursor, the IME view and the consumer's
 * hidden textarea — so each one holds the container's scrollable overflow at the
 * offset it was last placed at. `output.replaceChildren()` removes the rows and
 * leaves all four.
 *
 * Measured on a 4769-row tab in Chromium: after the wipe the scroller had ZERO
 * rows of content and an 81081px scroll range, held by the caret and the
 * textarea still sitting at 81064px, with the viewport parked at 80281px over
 * nothing. Two consequences, and the second is why this is not cosmetic:
 *
 *   - The reader sees that empty region, which paints the terminal's background:
 *     a black pane, and then a partial row clipped to the top edge once the
 *     drain's growing block reaches the parked offset.
 *   - `stickToBottom` is DISARMED. It measures `distanceFromBottom()` against
 *     the phantom height, reads 0 — already at the bottom — and pins nothing, so
 *     the invariant that would otherwise rescue the view refuses to act for
 *     exactly as long as the phantom height survives.
 *
 * The renderer heals all four at the END of the first flush
 * (`positionCursorOverlay` plus `onCursorMove`), so the window is normally one
 * frame and invisible. It stops being one frame whenever that tail does not
 * run on time: a long frame (the wipe tears down thousands of rows), a throw
 * mid-drain (the catch skips the tail and the bounded give-up can stop
 * rescheduling), or a full reset arriving with the switch's reconnect. The
 * reported symptom is a scroll tick clearing it, which fits: a scroll re-enters
 * the built block, and the block sits at the TOP of the phantom range.
 *
 * Collapsing here also makes the wipe's clamp SYNCHRONOUS, which two callers
 * need. `noteContentShrink` arms only when it observes the position actually
 * move, so a deferred clamp left the wipe — the largest shrink this module
 * performs — with nothing to announce. And `bind` records
 * `pendingRestore.lastWrote` from the same offset, so a deferred clamp
 * (measured: 80281 -> 4346) then read as a 76000px foreign write and threw away
 * the reading position the switch was restoring.
 */
function collapseContentSpaceOverlays(): void {
  // `cursorAbs` is -1 by now, which is the state positionCursorOverlay already
  // hides for; go through it rather than restating the class name here.
  positionCursorOverlay(undefined);
  // The outgoing session's prediction cannot describe the incoming screen
  // either. The consumer re-pushes it from the `onCursorMove` below, which is
  // its call to make and cannot re-inflate anything: `rowEls` is empty, so
  // setPredictedCursor's own fallback places it at the content origin.
  predCursorEl?.classList.remove("visible");
  // The IME view and the hidden textarea are the CONSUMER's, reachable only
  // through the cursor seam — and after a wipe the cursor genuinely has no row,
  // so the seam is accurate rather than borrowed. `getCursorPx` reports the
  // content origin while `rowEls` is empty, so the handler moves them there.
  onCursorMove?.();
}

/**
 * Highest absolute line index the client holds, or -1 if empty. Exposed so a
 * consumer can ask how far this store's content reaches; it is NOT the resume
 * `haveThrough` (that is getReplayBoundary below, and the difference is a bug
 * when the two are confused).
 */
export function getHighestIndex(): number {
  return store.highestIndex();
}

/**
 * The resume `haveThrough`: the highest index this client will not ask the
 * server to re-send. Wire this to Callbacks.getHaveThrough, never
 * getHighestIndex.
 *
 * The two differ by whatever the store holds provisionally — screen rows the
 * application drew and the server has not committed at those indices — and
 * claiming those on the wire is what leaves a stale copy of the last screen
 * parked in scrollback after a reattach. See LineStore.replayBoundary.
 */
export function getReplayBoundary(): number {
  return store.replayBoundary();
}

/**
 * Record the server's retained-history bounds from a resumeAck (committed =
 * one past the newest retained, oldest = oldest retained absolute index). The
 * store uses these to tell a genuine history trim (the server evicted lines
 * the client was missing) from a still-loading state, which drives the
 * "earlier output trimmed" marker. Resync guard 8.2.2.
 */
export function noteResumeBounds(committed: number, oldest: number): void {
  store.noteResumeBounds(committed, oldest);
  scheduleFlush();
}

// --- demand-paged scrollback: the consumer's seams (docs/paged-scrollback.md) ---

/**
 * Apply a correlated history page and, when the reply proved history is gone,
 * raise the paging floor so nothing below it is requested again.
 *
 * Wired to `connection`'s `onHistoryReply`. The viewport index is supplied here
 * rather than by the caller because the renderer is the only layer that knows
 * it, and the store's eviction needs it to decide which end of the cache is
 * safe to drop.
 */
export function handleHistoryReply(msg: ScrollMessage, raiseFloorTo: number | null): void {
  if (raiseFloorTo !== null) {
    store.raisePagingFloor(raiseFloorTo);
  }
  store.applyHistoryScroll(msg, viewportAbs());
  scheduleFlush();
}

/**
 * Run the store's single resume-ack transition, supplying the viewport the
 * store cannot see. Wired to `connection`'s `onResumeTransition`.
 */
export function applyResumeTransition(ack: {
  epochChanged: boolean;
  committed: number | null;
  serverOldest: number | null;
  paging: boolean;
  sentHaveThrough: number;
  sentReplayMax: number | null;
}): void {
  // A tab switch RECONNECTS (connection.setSession -> reconnectNow), so this
  // transition routinely runs while the surface is mid-rebuild — at a clamped
  // offset, over rows that are still being built. The live measurement there is
  // a transient that belongs to neither the outgoing nor the incoming view, and
  // the store's reclassify pass uses it to decide which rows survive: reading
  // the transient can evict the very rows an armed restore is about to bring
  // back. An armed restore names the position the user is REGAINING, so it is
  // the answer to "which rows must survive" (docs/scroll-position-fidelity.md
  // §7.2, §7.3). Every other viewportAbs consumer asks about NOW and keeps the
  // live value.
  const pending = pendingRestoreAbs();
  // The store no longer infers whether the reader is following; this layer knows.
  // `isUserScrolledUp()` is the one authoritative source, and an ARMED RESTORE
  // overrides it outright: mid-rebuild the browser has clamped the offset, so the
  // live flag describes the transient rather than the view the reader is about to
  // regain — and a restore is armed only for a position in history.
  const pendingRestoreArmed = pending !== null;
  store.applyResumeAck({
    ...ack,
    viewportAbs: pending ?? viewportAbs(),
    following: !pendingRestoreArmed && !scroll.isUserScrolledUp(),
  });
  scheduleFlush();
}

/** Record the window of a request going out (the store port's half). */
export function noteSolicited(fromAbs: number, end: number): void {
  store.noteSolicited(fromAbs, end);
}

/** Release the in-flight window (reply applied, timed out, socket gone). */
export function clearSolicited(): void {
  store.clearSolicited();
}

/**
 * Drop the browse cache, supplying the viewport the skip rule needs. The
 * consumer owns the TTL and the visibility state; the engine has no notion of
 * tabs or page visibility.
 */
export function dropBrowseCache(pageVisible: boolean): void {
  store.dropBrowseCache(viewportAbs(), pageVisible);
  scheduleFlush();
}

/** When the browse cache was last created or refreshed, for a consumer's TTL. */
export function lastBrowseActivityMs(): number {
  return store.lastBrowseActivityMs();
}

/** Lines currently held as disposable browse cache (diagnostics, tests). */
export function browseCacheSize(): number {
  return store.browseCacheSize();
}

/**
 * The resume replay bound to send: the client's own residency minus the window
 * it is about to be sent anyway, so an attach does not download rows the cap
 * would immediately trim. Wired to `connection`'s `getReplayMax`.
 */
export function replayMaxForResume(): number {
  return Math.max(1, store.tailCap() - store.getWindow().height);
}

/**
 * How many rows are queued for a DOM (re)build but not yet built. Non-zero
 * means the surface is still materializing content the store already holds: a
 * resume replay, a wipe-and-rebuild after a store swap, or any burst larger
 * than one frame's build budget.
 *
 * Exposed so a consumer can surface a "still catching up" affordance during a
 * large restore. It is a RENDER-side measure, not a transport one: it reaches
 * zero between the server's replay chunks, so on its own it is not a
 * restore-complete signal. Pair it with the resumeAck bounds (the store's
 * highestIndex reaching the reported `committed`) to tell "this frame's backlog
 * is drained" from "the restore has finished arriving".
 */
export function pendingRowCount(): number {
  return renderQueue.size;
}

// --- Color helpers ---
function colorHex(c: number | undefined): string | null {
  if (c === undefined || c < 0) {
    return null;
  }
  return "#" + c.toString(16).padStart(6, "0");
}

// --- URL detection (xterm.js addon-web-links pattern) ---
const URL_RE = /(https?|HTTPS?):\/\/[^\s"'!*(){}|\\^<>`]*[^\s"':,.!?{}|\\^~[\]`()<>]/g;

function linkifySpans(
  spans: (HTMLSpanElement | HTMLAnchorElement)[],
): (HTMLSpanElement | HTMLAnchorElement)[] {
  const out: (HTMLSpanElement | HTMLAnchorElement)[] = [];
  for (const span of spans) {
    // Pass anchors through untouched. A span may already be an <a> from an
    // OSC 8 hyperlink emitted by the application (see buildRowSpans). The
    // app-provided href is authoritative and takes precedence over heuristic
    // autolinking — re-scanning it with URL_RE would rebuild the link from
    // the *visible* text, which for a URL that wraps across rows is only a
    // fragment. That truncates the href and defeats the terminal's
    // clickable-across-line-wraps behavior. Skip; only autolink plain text.
    if (span.tagName === "A") {
      out.push(span);
      continue;
    }
    const text = span.textContent;
    URL_RE.lastIndex = 0;
    let match: RegExpExecArray | null;
    let last = 0;
    let found = false;
    while ((match = URL_RE.exec(text)) !== null) {
      found = true;
      if (match.index > last) {
        const pre = span.cloneNode(false) as HTMLSpanElement;
        pre.textContent = text.slice(last, match.index);
        out.push(pre);
      }
      const a = document.createElement("a");
      a.href = match[0];
      a.target = "_blank";
      a.rel = "noopener noreferrer";
      // Auto-detected bare URLs are tightly scoped to the matched URL text
      // (never a padded/bordered region), so `.term-autolink` keeps a persistent
      // underline for discoverability. OSC 8 hyperlinks (buildRowSpans below) get
      // only `.term-link`, which the UI underlines on hover — an app can attach a
      // single OSC 8 link to a whole region (e.g. a URL wrapping inside a table
      // cell, where the link stays open across the cell padding/borders), and a
      // persistent underline would then bleed across the cell/row.
      a.className = "term-link term-autolink";
      a.textContent = match[0];
      // Copy inline styles from the source span property-by-property, never
      // via cssText: a cssText assignment is a string-PARSED style write, the
      // one kind a style-src CSP reasons about, and this was the renderer's
      // only such write. With it gone, every style write in the render path
      // is an individually-assigned property (unambiguously ungoverned by
      // style-src), which keeps the consumers' CSP-hardening story clean.
      for (let i = 0; i < span.style.length; i++) {
        const prop = span.style.item(i);
        a.style.setProperty(
          prop,
          span.style.getPropertyValue(prop),
          span.style.getPropertyPriority(prop),
        );
      }
      out.push(a);
      last = match.index + match[0].length;
    }
    if (!found) {
      out.push(span);
    } else if (last < text.length) {
      const post = span.cloneNode(false) as HTMLSpanElement;
      post.textContent = text.slice(last);
      out.push(post);
    }
  }
  return out;
}

// A hyperlink run is "link text" only if it has at least one glyph that is not
// whitespace and not a box-drawing (U+2500–U+257F) or block-element
// (U+2580–U+259F) character. Terminal table structure (borders `│`, padding,
// empty columns, margins) is made of exactly those, and an app may keep an OSC 8
// hyperlink open across it while a URL wraps; such decorative runs are not
// anchored, so the link decoration hugs the actual text instead of bleeding
// across the cell/row.
function runHasLinkText(spans: (HTMLSpanElement | HTMLAnchorElement)[]): boolean {
  for (const s of spans) {
    for (const ch of s.textContent) {
      if (/\s/.test(ch)) {
        continue;
      }
      const cp = ch.codePointAt(0) ?? 0;
      if (cp >= 0x2500 && cp <= 0x259f) {
        continue;
      }
      return true;
    }
  }
  return false;
}

// --- Build row DOM ---
function buildRowSpans(runs: readonly WireRun[]): (HTMLSpanElement | HTMLAnchorElement)[] {
  const out: (HTMLSpanElement | HTMLAnchorElement)[] = [];
  for (const run of runs) {
    if (!run.t) {
      continue;
    }
    const runStartIdx = out.length;
    const attrs = run.a ?? 0;
    const isBold = (attrs & 1) !== 0;
    const isItalic = (attrs & 2) !== 0;
    const isUnderline = (attrs & 4) !== 0;
    const isInverse = (attrs & 8) !== 0;
    const isStrike = (attrs & 16) !== 0;
    const isDim = (attrs & 32) !== 0;
    const isHidden = (attrs & 64) !== 0;
    const isBlink = (attrs & 128) !== 0;
    const isOverline = (attrs & 256) !== 0;
    const isDoubleUnderline = (attrs & 512) !== 0;

    // Server swaps FG/BG for inverse in wire.go, but when both are
    // default (-1) the swap is a no-op. Detect inverse + defaults and
    // apply theme-inverted colors so the inverted space is visible.
    let fg = colorHex(run.f);
    let bg = colorHex(run.b);
    if (isInverse && fg === null && bg === null) {
      fg = "var(--bg)";
      bg = "var(--text)";
    }
    const ucColor = colorHex(run.uc);

    const applyStyle = (span: HTMLSpanElement, spacing: number): void => {
      if (isHidden) {
        span.style.visibility = "hidden";
      }
      if (fg !== null) {
        span.style.color = fg;
      }
      if (bg !== null) {
        span.style.background = bg;
      }
      if (isBold) {
        span.style.fontWeight = "bold";
      }
      if (isItalic) {
        span.style.fontStyle = "italic";
      }
      if (isDim) {
        span.style.opacity = ".5";
      }
      // Build text-decoration combining all line types.
      const decoLines: string[] = [];
      if (isDoubleUnderline) {
        decoLines.push("underline");
      } else if (isUnderline) {
        decoLines.push("underline");
      }
      if (isOverline) {
        decoLines.push("overline");
      }
      if (isStrike) {
        decoLines.push("line-through");
      }
      if (decoLines.length > 0) {
        let deco = decoLines.join(" ");
        if (isDoubleUnderline) {
          deco += " double";
        }
        span.style.textDecoration = deco;
      }
      if (ucColor !== null) {
        span.style.textDecorationColor = ucColor;
      }
      if (spacing !== defaultSpacing) {
        span.style.letterSpacing = `${spacing}px`;
      }
      if (isBlink) {
        span.classList.add("term-blink");
      }
    };

    let prevSpacing: number | null = null;
    let buffer = "";
    const flush = (): void => {
      if (buffer.length === 0) {
        return;
      }
      const span = document.createElement("span");
      span.textContent = buffer;
      applyStyle(span, prevSpacing ?? 0);
      out.push(span);
      buffer = "";
    };
    for (const ch of run.t) {
      if (ch === "\uFFFF") {
        // Wide-char continuation placeholder: mark previous span as double-width.
        // Flush any buffered text first so the wide char is in its own span.
        flush();
        if (out.length > 0) {
          // eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- length checked above
          const prev = out[out.length - 1]!;

          const prevText = prev.textContent;
          if (prevText.length > 0) {
            // eslint-disable-next-line @typescript-eslint/no-non-null-assertion, @typescript-eslint/no-misused-spread -- terminal text is ASCII/CJK, safe to spread; .at(-1) guaranteed by length check
            const lastChar = [...prevText].at(-1)!;
            const w = measureChar(lastChar, isBold, isItalic);
            prev.style.letterSpacing = `${cellWidth * 2 - w}px`;
          }
        }
        // The spacer occupies the wide char's second cell. (Column arithmetic
        // for the caret lives in glyphAt, which mirrors this advance rule —
        // the engine reports cursor_col in true cell coordinates, a wide
        // glyph moving curX by 2.)
        continue;
      }
      const w = measureChar(ch, isBold, isItalic);
      const spacing = cellWidth - w;
      if (prevSpacing === null) {
        prevSpacing = spacing;
      } else if (spacing !== prevSpacing) {
        flush();
        prevSpacing = spacing;
      }
      buffer += ch;
    }
    flush();
    // Wrap this run's spans in an <a> when it has a hyperlink URL — but only if
    // the run actually contains link *text*. An OSC 8 hyperlink can stay open
    // across a whole region the app never meant as the clickable "link": a URL
    // that wraps inside a table cell keeps the link open across the cell padding,
    // the borders `│` and the empty adjacent column. Anchoring those decorative
    // cells makes the link's decoration (the underline) bleed across the cell and,
    // on wrap, the full row. Skip them so the anchor — and its underline, at rest
    // or on hover — hugs the visible link text (they stay as plain, non-clickable
    // spans; the wrapped URL's text runs each still carry the full href).
    const href = run.u && /^https?:\/\//i.test(run.u) ? run.u : null;
    const runSpans = out.splice(runStartIdx);
    if (href && runHasLinkText(runSpans)) {
      const a = document.createElement("a");
      a.href = href;
      a.target = "_blank";
      a.rel = "noopener noreferrer";
      // Server-stamped autolink (attr bit 1024, vt.AttrAutolink): a bare URL
      // the server detected — joined across soft-wrap continuations, so every
      // row segment carries the FULL href. Styled like the client's own
      // autolinks (persistent underline; the anchor hugs exactly the URL
      // text). An app-provided OSC 8 hyperlink keeps the hover-only base
      // `.term-link` (it may span decorative cells).
      a.className = (attrs & 1024) !== 0 ? "term-link term-autolink" : "term-link";
      for (const s of runSpans) {
        a.appendChild(s);
      }
      out.push(a);
    } else {
      for (const s of runSpans) {
        out.push(s);
      }
    }
  }
  if (out.length === 0) {
    const span = document.createElement("span");
    span.textContent = "\u00a0";
    out.push(span);
  }
  return linkifySpans(out);
}

// --- Frame handling: feed the store, then flush to DOM ---

/**
 * Apply a `ScreenMessage`: update the store's window + changed rows, then
 * schedule a render flush. The store handles merging changed rows by absolute
 * index, so no client-side frame coalescing is needed.
 */
export function handleScreen(msg: ScreenMessage): void {
  lastInboundMs = Date.now();
  if (msg.scrollbackCleared) {
    // ED3: the application discarded its saved lines, so everything the store
    // holds below this base is about to go. Recorded HERE, at the one place that
    // sees the frame before the store consumes it, so restoreReadAnchor can tell
    // a region DISCARD from an ordinary cap trim — the two need opposite
    // recoveries and only the frame knows which happened
    // (docs/scroll-position-fidelity.md §5). Max, because several frames can
    // land between flushes.
    discardedBelowPending = Math.max(discardedBelowPending, msg.base);
  }
  store.applyScreen(msg);
  scheduleFlush();
}

/**
 * Apply a `ScrollMessage`: commit history lines into the store by absolute
 * index, then schedule a render flush.
 */
export function handleScroll(msg: ScrollMessage): void {
  lastInboundMs = Date.now();
  store.applyScroll(msg);
  scheduleFlush();
}

function scheduleFlush(): void {
  if (pendingFrame !== undefined) {
    return;
  }
  pendingFrame = requestAnimationFrame(flushRender);
}

/**
 * The renderer's half of a scroll-position event. Wire this to
 * `scroll.init({ onScrollPosition })` and nothing else: it is the ONE hook the
 * renderer needs from a position change, so a consumer cannot half-wire it.
 *
 * Two jobs, in this order. Re-evaluate the demand-paging trigger, because the
 * reader may have approached a gap. Then finish a drain that stopped with rows
 * still queued, which is reachable on an idle session: the bounded error path in
 * `flushRender`'s catch deliberately stops rescheduling, and no inbound frame
 * arrives to restart it.
 *
 * It replaced a pair of exported calls (`maybeFetchHistory` plus a public
 * `resumeDrain`) because that pair was a silent-omission trap of exactly the
 * shape the paged-scrollback docs warn about: a consumer upgrading the engine and
 * keeping its old one-line wiring got paging and no drain recovery, with no type
 * error and no runtime complaint. `maybeFetchHistory` stays exported for the
 * transport's own retry path, which is a different event.
 */
export function handleScrollPosition(): void {
  maybeFetchHistory();
  resumeDrain();
}

/**
 * Resume a drain that still has rows queued. Private: reached only through
 * `handleScrollPosition`, so there is one wiring obligation rather than two.
 *
 * Three refusals, and each one is load-bearing rather than defensive:
 *
 * **An empty queue.** `flushRender` runs `applyPendingRestore`,
 * `restoreReadAnchor` and `stickToBottomIfFollowing` unconditionally; only the
 * drain is queue-gated. So a flush scheduled from a scroll handler would run the
 * position invariants at moments the renderer never previously flushed at, and
 * because `atBottom` tolerates `BOTTOM_TOLERANCE_PX` while `stickToBottom` pins
 * on any non-zero gap, a downward scroll landing just short of the tail on an
 * IDLE session would snap to the bottom one frame later. It would also expire an
 * armed view restore early through `RESTORE_OWN_WRITE_EPSILON_PX`.
 *
 * **Alt screen.** Alt is a NAMED suspension of the drain (see the reschedule rule
 * below), and a resumption path that ignores it is not a resumption path. The
 * drain would survive anyway, because `flushRenderInner`'s alt branch returns
 * before the drain, but the flush around it would still call `renderAlt` (which
 * replaces the whole output subtree, destroying an in-alt selection) and run all
 * three position invariants. Leaning on the body of a function to make a
 * scheduling decision safe is how the next edit to that body becomes a bug. Alt
 * exit re-queues everything through `queueRowsViewportFirst`, so refusing costs
 * nothing.
 *
 * **A give-up already retried.** The catch's give-up exists so a deterministically
 * throwing row cannot become a 60fps loop. Retrying it from a scroll event would
 * move that loop outside the module rather than remove it: a flick is one scroll
 * event per frame, and `scroll.ts`'s position callback fires for the browser's own
 * clamps too, so a stuck row would log two errors per frame for the length of a
 * gesture. One retry per give-up is enough to serve the case this function exists
 * for (the reader scrolls, the surface completes) without reopening the loop the
 * cap closed. The retry is re-earned by anything that clears the streak: real
 * forward progress, an empty queue, a rebuild, or a re-attach. NOT by the give-up
 * log — a retry that throws gives up again, and re-arming there would restore the
 * per-gesture loop one indirection further out.
 *
 * Residual, accepted: `scroll.ts` fires its position seam before its own
 * announced-shrink early return, so a programmatic clamp (a cap eviction) can spend
 * the retry with no user action. The exposure is small because any successful flush
 * re-arms it, and the alternative (a time bound) adds a clock to a path whose whole
 * purpose is to be cheap.
 */
function resumeDrain(): void {
  if (renderQueue.size === 0 || store.isAlt()) {
    return;
  }
  // A frame is already coming, so there is nothing to resume and nothing to spend.
  // Without this the retry is burned in a state where it buys NOTHING: the catch
  // reaches `streak === MAX` from its own progress-less branch and schedules a frame
  // on the way out, so for one frame the streak is at the cap with a flush pending.
  // A scroll landing there would set the streak past the cap while `scheduleFlush`
  // no-ops on the occupied slot, and the real give-up one frame later would then
  // find its one retry already gone. The spend must be gated on the give-up having
  // HAPPENED, and an empty frame slot is what distinguishes that.
  if (pendingFrame !== undefined) {
    return;
  }
  if (renderNoProgressStreak >= MAX_RENDER_NO_PROGRESS_RETRIES) {
    if (renderNoProgressStreak > MAX_RENDER_NO_PROGRESS_RETRIES) {
      return;
    }
    // Spend the one retry by carrying it in the streak itself, rather than in a
    // second flag beside it. A flag was tried and it leaked: it had to be cleared
    // at every site that clears the streak, it was cleared at three of five, and
    // the two misses (`rebuild`, and a clean pass) are the common ones — so one
    // give-up plus one scroll disabled the resume for the rest of the attachment,
    // invisibly, because that state presents to the canary as a plain give-up. With
    // the count carrying it there is nothing to keep in sync: every existing
    // `renderNoProgressStreak = 0` re-arms the retry by construction.
    renderNoProgressStreak = MAX_RENDER_NO_PROGRESS_RETRIES + 1;
  }
  scheduleFlush();
}

// --- Read-position anchoring (manual scroll anchoring) ---
//
// A flush can change the content height ABOVE the reading position: rows evicted
// from the top of history once the retention cap is reached, and the trim marker
// appearing. Every such change moves whatever the user is reading, unless the
// viewport is shifted by the same amount.
//
// Chrome and Firefox do that natively (scroll anchoring, `overflow-anchor`).
// WebKit does not implement it at all, so on Safari — including the iPad, where
// this UI mostly runs — a user scrolled up to read had their position dragged
// one line per evicted row for as long as output kept arriving.
//
// The anchor is the first row at or below the viewport top, found by binary
// search over the container's children: they are in document order, so their
// offsetTop is monotonic. ~13 reads for a full 5000-row buffer, and only while
// the user is actually scrolled up — a following viewport takes this path not at
// all, because the bottom pin owns its position.
interface ReadAnchor {
  el: HTMLElement;
  /** The anchor row's absolute index, so an eviction that removes the element
   *  itself (batched cap eviction frees up to a whole batch at once) can
   *  re-resolve to the nearest surviving row instead of giving up. */
  abs: number;
  /** Where the row sat ON SCREEN: offsetTop minus the scroll offset. Measuring
   *  the screen position rather than the container offset is what makes the
   *  correction idempotent — see restoreReadAnchor. */
  screenTop: number;
}

// rowAtViewportTop returns the first CONTENT row at or below the viewport top —
// the ONE row-selection primitive every "where is the reader" question resolves
// through: the read anchor, the paging trigger's viewportAbs, and the per-view
// memory a tabbed consumer saves. One definition because two would drift, and
// they would drift exactly during a rebuild, which is when the answer matters
// most (docs/scroll-position-fidelity.md §7.2).
//
// Children are in document order with monotonic offsetTop, so this is a binary
// search: ~13 reads for a full 5000-row buffer. Non-content children are NEVER a
// reading position and are skipped: a marker is an annotation ABOUT a hole, so
// holding one in place would pin the annotation rather than the text the reader
// is looking at.
//
// "Content row" is decided by IDENTITY in rowEls, not by the presence of a
// data-abs attribute. A gap marker carries one — updateGapMarkers sets it to the
// gap's LOW index so insertRowInOrder keeps the container monotonic — so an
// attribute test skips only the trim marker and happily returns a gap marker as
// a reading position. Its index is the first ABSENT line, which rowEls can never
// resolve, so a restore anchored there could never land and a viewportAbs built
// on it would name a line the store does not hold.
function isContentRow(el: HTMLElement): boolean {
  const abs = rowAbs(el);
  return abs >= 0 && rowEls.get(abs) === el;
}

function rowAtViewportTop(): HTMLElement | null {
  const kids = output.children;
  if (kids.length === 0) {
    return null;
  }
  const offset = scroll.currentScrollTop();
  let lo = 0;
  let hi = kids.length - 1;
  let found: HTMLElement | null = null;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    const el = kids[mid] as HTMLElement;
    if (el.offsetTop >= offset) {
      found = el;
      hi = mid - 1;
    } else {
      lo = mid + 1;
    }
  }
  // Walk past any markers the search landed on. The bound is O(gaps + 1), not 1:
  // two gap markers CAN be adjacent siblings, and can be the last two children —
  // measured once predictReplayJump retires the window, since emptyWindow()'s
  // base of 0 makes the drain ascending. Cheap either way (a handful of gaps at
  // most), but do not assume a single step.
  while (found !== null && !isContentRow(found)) {
    found = found.nextElementSibling as HTMLElement | null;
  }
  return found;
}

function captureReadAnchor(): ReadAnchor | null {
  if (!scroll.isUserScrolledUp()) {
    return null; // following: stickToBottom owns the position
  }
  const el = rowAtViewportTop();
  if (el === null) {
    // Nothing at or below the viewport top. Reaching this with a container that
    // CLAMPS scrollTop needs the viewport to be shorter than a single row, so it
    // is effectively unreachable in a browser; the previous last-row fallback
    // was reachable mainly under a test fixture whose scrollTop setter did not
    // clamp. Standing down is the right answer either way: using the TAIL as a
    // proxy reading position is what turned a large content shrink into a
    // tail-drag (docs/scroll-position-fidelity.md §1.2).
    return null;
  }
  return { el, abs: rowAbs(el), screenTop: el.offsetTop - scroll.currentScrollTop() };
}

// firstRowAtOrAfter returns the first output child whose absolute index is
// >= abs — the nearest surviving CONTENT at or below a lost reading position.
// Binary search over the children, mirroring rowAtViewportTop: they are kept in
// ascending data-abs order.
//
// Markers are skipped by identity (isContentRow), not by reading -1 from a
// missing data-abs. That older property held only while the trim marker was the
// single non-row child; a per-gap marker carries its gap's LOW index, so an
// attribute test would return the marker as the "nearest surviving row" and the
// anchor would pin an annotation about a hole to the reader's screen position.
// (~13 reads at the 5000-row cap; this runs inside the eviction flush, the
// frame's most expensive pass already.)
function firstRowAtOrAfter(abs: number): HTMLElement | null {
  const kids = output.children;
  let lo = 0;
  let hi = kids.length - 1;
  let found: HTMLElement | null = null;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    const el = kids[mid] as HTMLElement;
    if (rowAbs(el) >= abs) {
      found = el;
      hi = mid - 1;
    } else {
      lo = mid + 1;
    }
  }
  while (found !== null && !isContentRow(found)) {
    found = found.nextElementSibling as HTMLElement | null;
  }
  return found;
}

function restoreReadAnchor(anchor: ReadAnchor | null): void {
  if (anchor === null) {
    return; // following: stickToBottom owns the position
  }
  let el: HTMLElement | null = anchor.el;
  if (el.parentElement !== output) {
    if (fullResetThisPass) {
      // Server restart / epoch change: absolute indices restarted from 0, so
      // a child with a matching data-abs is UNRELATED content from the new
      // session — re-anchoring on it would pull an arbitrary row to the old
      // reading position. Stand down; the fresh session's own follow state
      // owns the viewport.
      return;
    }
    // The anchor row itself was evicted out from under the reader: batched
    // cap eviction frees up to a whole batch (256 at the default cap) in one
    // pass, so a reader parked within a batch of the buffer top loses the
    // anchored ELEMENT while rows they had not read yet survive below it.
    // Re-resolve to the first surviving row at or after the anchored index
    // and hold THAT at the anchor's screen position — the view resumes at
    // the nearest surviving content instead of silently jumping past up to
    // a batch of unread lines (WebKit has no native anchoring to catch it).
    el = firstRowAtOrAfter(anchor.abs);
    if (el === null) {
      return; // nothing survives (reset/clear): nothing to hold
    }
    // A region DISCARD, not a cap trim: the application erased its saved lines
    // (ED3), so nothing surviving is guaranteed ADJACENT to what the reader was
    // looking at. On an inline TUI that reprints on resize the same text comes
    // straight back at new indices, so the nearest surviving row is unrelated
    // content — and holding it at the reader's screen position is the "random jump
    // on resize" symptom. A cap trim is the opposite case and keeps the re-resolve
    // above: it removes the oldest contiguous run, so the survivor really is the
    // reader's neighbour (docs/scroll-position-fidelity.md §1.2, §5).
    //
    // The test is the ANCHOR's index. An earlier version also required the
    // SURVIVOR to sit at or above the discard base, which never held for the shape
    // this exists for: the reprint re-delivers lines below that base in the same
    // frame, so the survivor looked adjacent and the correction fired anyway.
    //
    // The remaining `anchor.abs <` clause is a GUARD, not a live path, and the
    // reachability argument is worth recording because it is not obvious. For a
    // single well-formed frame it cannot fire: `msg.base` is both the discard bound
    // and the new window base, so an anchor at or above it sits inside the live
    // window, which cap eviction protects and `truncateBelowWindow` only trims
    // above. Across several frames in one batch (ED3 at a low base, the window
    // settling high) the anchor's ELEMENT normally survives instead — the store
    // coalesces an evict-then-reapply of the same index, so `upsertRow` reuses the
    // element and this whole branch is skipped, leaving the ordinary drift
    // correction to handle the history the discard removed above the reader, which
    // is right. Measured: that path applies a legitimate -1700px correction for
    // 100 discarded rows, and it is NOT this branch.
    if (discardedBelowThisPass >= 0 && anchor.abs < discardedBelowThisPass) {
      return;
    }
  }
  // Correct by how far the row DRIFTED ON SCREEN, not by how much the content
  // above it changed. The two differ exactly when the browser already did this
  // itself: Chrome and Firefox have native scroll anchoring, so their offsetTop
  // change comes with a matching scrollTop change and the screen position is
  // already right — this then measures zero drift and does nothing. Correcting
  // the content delta instead would double-compensate there and throw the view
  // the other way. On Safari, which has no native anchoring, the drift is the
  // whole content delta and this is the only thing that fixes it.
  const drift = el.offsetTop - scroll.currentScrollTop() - anchor.screenTop;
  scroll.adjustForContentShift(drift);
}

function flushRender(): void {
  pendingFrame = undefined;
  // Read BEFORE any mutation, twice over: the anchor needs the pre-mutation
  // screen position, and noteContentShrink needs the pre-mutation offset to tell
  // whether this pass's row removals actually moved the viewport.
  const scrollTopBefore = scroll.currentScrollTop();
  const anchor = captureReadAnchor();
  try {
    flushRenderInner();
    // A clean pass means the error condition (if any) has cleared, so give a
    // later transient error its full retry budget again. Any backlog reschedule
    // was already issued from flushRenderInner's own end-of-body.
    renderNoProgressStreak = 0;
  } catch (err) {
    console.error("vterm: render error", err);
    // flushRenderInner threw mid-drain, skipping its own end-of-body
    // "if (renderQueue.size > 0) scheduleFlush()" reschedule, so rows still
    // queued would strand until the next external scheduleFlush() (a new
    // frame). Reschedule here to finish the drain and to retry a transient
    // throw (l-f28) -- but BOUND it. The drain loop deletes a row only after
    // upsertRow succeeds, so a row that throws deterministically stays queued;
    // an unconditional catch -> rAF -> throw reschedule is then a ~60fps busy
    // loop that never stops, even on an idle session. Reschedule while the pass
    // made forward progress (the backlog is shrinking) or the consecutive
    // no-progress streak is under the cap (covers a transient font/measureText
    // race that clears within a frame or two); once passes throw with zero
    // progress past the cap, stop and let the next inbound frame (drainChanges)
    // retry -- the pre-l-f28 behavior for a permanently stuck row.
    if (renderQueue.size === 0) {
      renderNoProgressStreak = 0;
    } else if (flushDrainedThisPass > 0) {
      renderNoProgressStreak = 0;
      scheduleFlush();
    } else if (renderNoProgressStreak < MAX_RENDER_NO_PROGRESS_RETRIES) {
      renderNoProgressStreak++;
      scheduleFlush();
    } else {
      console.error("vterm: giving up render retry after repeated no-progress errors");
    }
  }
  // Tell the scroll controller whether this pass's row removals moved the
  // viewport. Must run BEFORE any write of our own below, or the comparison
  // measures our write instead of the browser's clamp. Announcing the clamp is
  // what keeps a content shrink from reading as "the user scrolled up" and
  // silently switching auto-follow off (scroll.ts's header; §1.3).
  if (removedRowsThisPass) {
    scroll.noteContentShrink(scrollTopBefore);
    // And correct the offset when the container left it past the end of the
    // shrunken content instead of reconciling it (scroll.ts's header: the third
    // arithmetic state). Before the three invariants below, so they measure a
    // geometry the container agrees with rather than one it is still holding an
    // impossible offset into.
    scroll.reconcileScrollRange();
  }
  // Three position invariants, applied after every DOM mutation and in this
  // order. An armed view restore goes first because it OWNS the position while
  // it is armed; the read anchor then holds whatever line is on screen; the
  // bottom pin last, and only if following.
  //
  // The anchor is skipped ONLY in the frame the restore actually LANDED, and
  // that exception is load-bearing rather than tidy: the anchor was captured
  // BEFORE this frame's mutations, so once the restore has authoritatively moved
  // the viewport, the anchor's drift measures the restore's own write and
  // corrects it straight back out — the restore and the anchor cancel and the
  // reader stays at the top. Skipping it while merely ARMED would be wrong for
  // the opposite reason: a rebuild spans many frames, and suppressing the anchor
  // across all of them reintroduces the WebKit read-position slide for every one.
  const restoreLanded = applyPendingRestore();
  if (!restoreLanded) {
    restoreReadAnchor(anchor);
  }
  // Single auto-follow invariant, applied after every DOM mutation.
  stickToBottomIfFollowing();
  // Absorb every write this frame made — ours and the browser's native scroll
  // anchoring — into the restore's baseline, so only a move that happens BETWEEN
  // frames (a real gesture) reads as one.
  if (pendingRestore !== null) {
    pendingRestore.lastWrote = scroll.currentScrollTop();
  }
  // The post-flush trigger. The reader's position relative to the store's gaps
  // can change without any scroll event — a tail trim moves the frontier up
  // under a stationary reader, and a byte-short page leaves a fresh sub-gap
  // beside them — so the flush is the other place the trigger must run
  // (docs/paged-scrollback.md §5.4).
  maybeFetchHistory();
  reportUnnamedDrainStall();
}

// --- The reschedule rule ---
//
// After a flush, owed rows are in exactly one of four states. The set is
// exhaustive by construction here rather than by review of each return site,
// which is what a queue left non-empty with an empty `pendingFrame` slot costs:
// `scheduleFlush` is a single slot, so every later request from every source is
// silently dropped and nothing drains.
//
//  1. nothing owed          - the queue is empty.
//  2. scheduled             - a frame will drain it.
//  3. suspended, named edge - alt screen. `renderAlt` replaces the whole output
//                             subtree, so continuing the drain would inject
//                             main-buffer rows into the alt grid; the alt-exit
//                             branch re-queues everything via
//                             queueRowsViewportFirst, so nothing is lost. The
//                             resumption edge is alt exit. A future third
//                             suspension point must declare its edge here.
//  4. stopped and reported  - the bounded error path gave up after
//                             MAX_RENDER_NO_PROGRESS_RETRIES and logged. It is a
//                             guard on the process, so it refuses rather than
//                             looping; the next inbound frame retries, or one
//                             scroll does (resumeDrain, once per give-up).
//
// Anything else is a stall nobody owns. Reported once PER EPISODE: the latch
// clears as soon as a flush ends in a healthy state, so a repeat of one stall
// stays quiet while a genuinely new stall later is still reported. Log spam would
// be its own defect, and so would a canary that goes deaf after the first report.
function reportUnnamedDrainStall(): void {
  if (renderQueue.size === 0 || pendingFrame !== undefined || store.isAlt()) {
    // A healthy end: the episode (if any) is over, so re-arm the canary.
    unnamedStallReported = false;
    return;
  }
  if (renderNoProgressStreak >= MAX_RENDER_NO_PROGRESS_RETRIES) {
    // Named and already logged by the catch. Not a healthy state, so the latch is
    // left as it is rather than re-armed.
    return;
  }
  if (unnamedStallReported) {
    return;
  }
  unnamedStallReported = true;
  console.warn(
    `vterm: ${String(renderQueue.size)} rows queued with no scheduled frame and no named suspension`,
  );
}

function flushRenderInner(): void {
  // Forward-progress accounting for the bounded error-path reschedule. Reset at
  // entry, incremented per drained row below; a mid-drain throw leaves it at
  // the count-so-far for flushRender's catch to read.
  flushDrainedThisPass = 0;
  const ch = store.drainChanges();
  fullResetThisPass = ch.fullReset;
  discardedBelowThisPass = discardedBelowPending;
  discardedBelowPending = -1;
  removedRowsThisPass = false;

  if (ch.fullReset) {
    removedRowsThisPass = true;
    output.replaceChildren();
    rowEls.clear();
    renderQueue.clear();
    trimMarkerEl = null;
    for (const el of gapMarkerEls.values()) {
      el.remove();
    }
    gapMarkerEls.clear();
    cursorAbs = -1;
    // The rows are not the only thing in content space, and this wipe is as
    // total as rebuild()'s. Collapse the four overlays with them HERE rather
    // than relying on the tail's positionCursorOverlay/onCursorMove, so the
    // shrink this pass announces is the real one: while an overlay still holds
    // the container's scrollable overflow at its old offset, both the
    // announcement and the range correction measure a phantom height and read
    // "already at the bottom" over zero rows of content. See
    // collapseContentSpaceOverlays for the measurement, and note that the tail
    // is exactly what does not run on the three paths listed there.
    collapseContentSpaceOverlays();
  } else {
    for (const abs of ch.evictedLines) {
      const el = rowEls.get(abs);
      if (el) {
        el.remove();
        removedRowsThisPass = true;
      }
      rowEls.delete(abs);
      renderQueue.delete(abs);
    }
  }

  // Refresh cursor state from the window.
  const win = store.getWindow();
  const newCursorAbs = win.base + win.cursorRow;
  cursorAbs = newCursorAbs;
  cursorCol = win.cursorCol;
  cursorHidden = win.cursorHidden;
  cursorStyleVal = win.cursorStyle;
  blinkEnabled = win.cursorBlink;
  // Reconcile the blink interval with the fresh cursor state (blink mode and
  // DECTCM visibility may both have changed this frame); no-op when the
  // resulting mode is unchanged.
  syncCursorBlink();

  // Alt screen: render the ephemeral grid instead of the absolute buffer.
  // Returning here skips the dirtyLines queueing below — safe by invariant:
  // main-buffer rows never repaint while alt is active, and the alt-exit
  // branch below rebuilds EVERYTHING via queueRowsViewportFirst, so any line
  // dirtied during the alt session is repainted at exit.
  if (store.isAlt()) {
    const altRows = store.getAltRows();
    renderAlt(altRows);
    positionCursorOverlay(altRows[win.cursorRow]);
    if (onCursorMove) {
      onCursorMove();
    }
    return;
  }
  if (altRendered) {
    // Just exited alt: drop the ephemeral rows and rebuild from the store,
    // viewport-first (shared with rebuild()) so the visible viewport fills
    // before deep scrollback on a large-history alt-exit.
    altRendered = false;
    altPrevRows = [];
    removedRowsThisPass = true;
    output.replaceChildren();
    rowEls.clear();
    renderQueue.clear();
    trimMarkerEl = null;
    for (const el of gapMarkerEls.values()) {
      el.remove();
    }
    gapMarkerEls.clear();
    queueRowsViewportFirst();
  }

  // Queue this frame's changed rows for building.
  for (const abs of ch.dirtyLines) {
    renderQueue.add(abs);
  }

  // The cursor's row is built regardless of the budget — the caret overlay
  // positions off the row element's offsetTop, so a huge backlog must never
  // leave it floating over a not-yet-built row. Content-driven only: cursor
  // MOTION no longer touches row DOM (the overlay carries the caret), so a
  // selection anywhere — including on the cursor row — survives typing.
  if (renderQueue.has(newCursorAbs) || !rowEls.has(newCursorAbs)) {
    upsertRow(newCursorAbs);
    renderQueue.delete(newCursorAbs);
  }

  // Drain up to MAX_ROWS_PER_FRAME queued rows this frame, viewport-first;
  // the rest carry over to the next frame (scheduled below) so one big burst
  // never blocks paint. Live-window rows build first (ascending), then the
  // scrollback backlog newest->oldest, so the backlog fills upward above the
  // bottom-pinned viewport — offscreen. A Set's insertion order did the
  // opposite under load: a multi-thousand-row backlog (a resume replay,
  // kiro-cli's post-resize transcript reprint, `cat bigfile`) queued ahead of
  // freshly-dirtied window rows, so the visible screen either churned through
  // history or froze (only the force-built cursor row moving) for seconds on
  // a slow device while the backlog drained. flushDrainedThisPass doubles as
  // the per-frame budget counter and the forward-progress signal the
  // error-path reschedule reads (it was reset to 0 at entry and nothing
  // between there and here touches it).
  const inWindow: number[] = [];
  const belowWindow: number[] = [];
  for (const abs of renderQueue) {
    if (abs >= win.base) {
      inWindow.push(abs);
    } else {
      belowWindow.push(abs);
    }
  }
  inWindow.sort((a, b) => a - b);
  belowWindow.sort((a, b) => b - a);
  // Drain the two lists sequentially under one budget (no concatenated copy:
  // during a full-backlog drain the spread re-allocated a backlog-sized array
  // every frame — pure GC churn on the hot path).
  const drainRow = (abs: number): boolean => {
    if (flushDrainedThisPass >= MAX_ROWS_PER_FRAME) {
      return false;
    }
    upsertRow(abs);
    renderQueue.delete(abs);
    flushDrainedThisPass++;
    return true;
  };
  for (const abs of inWindow) {
    if (!drainRow(abs)) {
      break;
    }
  }
  for (const abs of belowWindow) {
    if (!drainRow(abs)) {
      break;
    }
  }

  updateTrimMarker();
  // The gap markers are a projection of the store's geometry, so they are
  // re-derived on every flush rather than maintained incrementally: a page
  // apply, a browse eviction and a tail trim all move gap edges, and none of
  // them should have to know a marker exists.
  updateGapMarkers();

  // More rows pending: keep draining on subsequent frames.
  if (renderQueue.size > 0) {
    scheduleFlush();
  }

  positionCursorOverlay(store.getLine(cursorAbs));

  if (onCursorMove) {
    onCursorMove();
  }
}

// --- Per-view scroll memory (docs/scroll-position-fidelity.md §3) ---
//
// A reading position is a LINE, not a pixel offset. A tabbed consumer that saved
// `scrollTop` and replayed it on re-entry had that write silently CLAMPED,
// because a rebuild has only built the live window plus one frame's budget when
// the replay lands (301 of up to 5000 rows, ~5100 of ~85,000 px), and the clamp
// was never re-attempted. The saved offset also stops meaning the same line as
// soon as the tab's content grows while it is backgrounded, which is every tab
// whose session kept working. So the memory is an absolute index plus the row's
// on-screen offset, and the restore is RE-ASSERTED until the row it names has
// actually been built.
//
// `screenTop` is deliberately a DIFFERENCE (offsetTop - scrollTop), the same
// form ReadAnchor uses: rows report offsets in `.term-output`'s space while
// scrollTop belongs to `.term`, so any ABSOLUTE pixel value would need the
// rowTopInTermWrap conversion and would be one padding off without it. A
// difference cancels the space out.
export interface ViewMemory {
  /** Absolute line index of the content row at the viewport top. */
  abs: number;
  /** That row's on-screen position: offsetTop minus the scroll offset. */
  screenTop: number;
  /** Whether auto-follow was engaged. */
  following: boolean;
}

// The armed restore. `gen` is the bind generation it belongs to, so a second
// switch mid-drain cannot land the first tab's anchor into the second tab's
// store. `lastWrote` is the offset this module last left behind: a position that
// does NOT match it was moved by the user, which cancels — the library never
// fights a gesture, and it establishes that by knowing its own writes rather
// than by listening to a signal (`onScrollPosition`) that also fires for the
// browser's clamps, including the one the rebuild itself causes.
interface PendingRestore {
  view: ViewMemory;
  gen: number;
  lastWrote: number;
  deadline: number;
}
let pendingRestore: PendingRestore | null = null;
let bindGen = 0;
// Wall-clock of the last inbound frame for the bound store. `renderQueue.size
// === 0` alone is NOT "the rebuild finished": it reaches zero BETWEEN a resume
// batch's replay chunks (see pendingRowCount's contract), so the settle needs
// transport quiet too. Both numbers mirror the tabs feature's already-tuned
// catch-up cue, which answers the same question for the same reason.
let lastInboundMs = 0;
const RESTORE_SETTLE_MS = 250;
const RESTORE_MAX_MS = 30000;
// A position within this many px of what we last wrote is still ours: a
// fractional-layout or subpixel-DPR readback is not a user gesture.
const RESTORE_OWN_WRITE_EPSILON_PX = 1;

/**
 * Capture the current view as per-view scroll memory a consumer can hand back
 * to `bind`. Returns null when there is nothing meaningful to remember: no
 * content rows yet, or the alternate screen is active (an alt grid has no
 * absolute indices worth restoring, and measuring one would overwrite a tab's
 * real saved position with an alt-screen row).
 *
 * Unlike the private read anchor this measures in BOTH follow states — the
 * follow half is saved alongside it, and a consumer needs the pair.
 */
export function captureViewMemory(): ViewMemory | null {
  if (store.isAlt()) {
    return null;
  }
  const el = rowAtViewportTop();
  if (el === null) {
    return null;
  }
  const abs = rowAbs(el);
  if (abs < 0) {
    return null;
  }
  return {
    abs,
    screenTop: el.offsetTop - scroll.currentScrollTop(),
    following: !scroll.isUserScrolledUp(),
  };
}

/** Drop any armed restore. Called on (re)init and on every bind. */
function clearPendingRestore(): void {
  pendingRestore = null;
}

// applyPendingRestore re-asserts an armed view restore, and is the reason the
// restore is not a one-shot: the anchored row may not be BUILT yet. A rebuild
// drains at MAX_ROWS_PER_FRAME, so a reader parked deep in history has their row
// materialize several frames after the switch, and a single write at frame 1
// would be clamped to the partial content and never retried.
//
// Returns true when it landed (and disarmed).
function applyPendingRestore(): boolean {
  const p = pendingRestore;
  if (p === null) {
    return false;
  }
  // A newer bind owns the surface, or indices restarted: the saved anchor
  // describes content that is no longer here.
  if (p.gen !== bindGen || fullResetThisPass) {
    clearPendingRestore();
    return false;
  }
  const now = scroll.currentScrollTop();
  if (Math.abs(now - p.lastWrote) > RESTORE_OWN_WRITE_EPSILON_PX) {
    clearPendingRestore(); // the user moved: never fight a gesture
    return false;
  }
  const el = rowEls.get(p.view.abs);
  if (el === undefined) {
    // Not built yet, or gone for good. Give up only once the surface has
    // genuinely settled (queue drained AND transport quiet) or the bound
    // lapses; until then stay armed and try again next frame.
    const quiet = renderQueue.size === 0 && Date.now() - lastInboundMs > RESTORE_SETTLE_MS;
    if (quiet || Date.now() > p.deadline) {
      clearPendingRestore();
    }
    return false;
  }
  // Same arithmetic as the read anchor's drift correction, for the same
  // space-cancelling reason, and idempotent: an already-satisfied restore
  // measures zero and writes nothing.
  const drift = el.offsetTop - now - p.view.screenTop;
  scroll.adjustForContentShift(drift);
  clearPendingRestore();
  return true;
}

/**
 * The viewport's absolute index for the paging layer's live questions: which
 * rows to exempt from eviction, whether a reader is sitting on cache, which gap
 * an interaction is approaching. All three ask about NOW, so they get the live
 * measurement.
 *
 * `pendingRestoreAbs` is the separate answer for the one caller that asks a
 * different question — the resume transition, which asks which rows must
 * SURVIVE a switch (docs/scroll-position-fidelity.md §7.2).
 */
function viewportAbs(): number {
  if (!scroll.isUserScrolledUp()) {
    return store.getWindow().base;
  }
  const el = rowAtViewportTop();
  const abs = el === null ? -1 : rowAbs(el);
  return abs >= 0 ? abs : store.getWindow().base;
}

/**
 * The absolute index of an ARMED view restore, or null. Only the resume
 * transition consults this: during a rebuild the live viewport is a transient —
 * mid-drain, at a clamped offset — and a reclassify pass that reads it can evict
 * the very rows the restore is about to bring back. The pending anchor is the
 * position the user is regaining, which is the one that must survive.
 */
export function pendingRestoreAbs(): number | null {
  const p = pendingRestore;
  if (p === null) {
    return null;
  }
  // Expire on READ as well as in the flush. The flush is the only other place
  // this state is examined, so an idle surface (queue drained, no inbound frames,
  // no flush scheduled) would otherwise hold an arm indefinitely and keep
  // answering with a line the reader has long left.
  if (Date.now() > p.deadline) {
    clearPendingRestore();
    return null;
  }
  return p.view.abs;
}

/**
 * The fetch trigger: decide whether to ask for a page, and for which range.
 *
 * Called from the post-flush anchor path and from `scroll.ts`'s position seam.
 * Both are cheap to over-call — every guard below is a pure read, and pacing
 * makes a spurious run free — which is deliberate: the alternative (trying to
 * fire it only when needed) is how a trigger ends up never firing on the idle
 * session that needs it most.
 */
export function maybeFetchHistory(): void {
  // ALT: no paging while the alternate screen is active. The event paths are
  // scrollback-UI-only, but the pending-demand timer fires from a CLOCK, so
  // this guard is the load-bearing one — without it a vim session would fetch
  // pages nobody can see, and each denial would re-arm the timer for the
  // session's whole duration (docs/paged-scrollback.md §5.5).
  if (store.isAlt()) {
    return;
  }
  if (requestHistoryFn === null) {
    return; // paging not wired by this consumer
  }
  const budget = historyBudgetFn === null ? PAGE_SIZE : historyBudgetFn();
  if (!Number.isInteger(budget) || budget < 1) {
    return;
  }
  const abs = viewportAbs();
  const edges = store.absentEdgesNear(abs, PREFETCH_THRESHOLD);
  const gap = edges[0];
  if (gap === undefined) {
    return;
  }
  // APPROACH-ANCHORED: always fetch the end NEAREST the reader, so a wide gap
  // heals from the side being read. Fetching a fixed end would land pages up to
  // (gapWidth - budget) lines away from the viewport, leaving the rows under the
  // reader blank while the far end filled in.
  const floor = store.pagingFloorIndex();
  const fromAbs =
    abs >= gap.hi
      ? Math.max(gap.lo, gap.hi - budget, floor) // approaching from BELOW the gap's top
      : Math.max(gap.lo, floor); // approaching from above: start at the gap's low edge
  const maxLines = Math.min(budget, gap.hi - fromAbs);
  if (maxLines < 1) {
    return;
  }
  requestHistoryFn(fromAbs, maxLines);
}

/**
 * Re-derive the gap markers from the store's geometry. A projection, not a
 * mutation log: every call recomputes which gaps exist and reconciles the DOM,
 * so a gap that healed from either edge moves or loses its marker without any
 * caller having to know which edge changed.
 */
function updateGapMarkers(): void {
  // Through intervals.ts, which declares itself the single source of gap
  // geometry — and the store's fetch trigger reads it too. This loop used to be
  // a second copy of `interiorGaps` with the same bounds and the same predicate,
  // so any correction to gap derivation (a touching-run case, an off-by-one at a
  // run edge) would have landed on one of them: the renderer marking a hole the
  // trigger will not fetch, or omitting one over a hole it will.
  const gaps = new Map<number, number>(); // low index -> high index
  for (const gap of interiorGaps(store.retainedRanges())) {
    gaps.set(gap.lo, gap.hi);
  }
  // Remove markers whose gap closed.
  for (const [lo, el] of [...gapMarkerEls]) {
    if (!gaps.has(lo)) {
      el.remove();
      gapMarkerEls.delete(lo);
    }
  }
  const floor = store.pagingFloorIndex();
  for (const [lo, hi] of gaps) {
    let el = gapMarkerEls.get(lo);
    if (el === undefined) {
      el = document.createElement("div");
      el.className = "term-gap-marker";
      el.setAttribute("role", "status");
      gapMarkerEls.set(lo, el);
    }
    // Three states, and the IDLE one is the default (§5.4). A gap marker whose
    // whole reason to exist is "do not read these two regions as contiguous"
    // cannot fall back to asserting nothing, or the splice it prevents comes
    // back silently.
    const condemned = floor >= hi;
    const label = condemned ? "earlier output trimmed" : "earlier output not loaded";
    if (el.textContent !== label) {
      el.textContent = label;
      el.setAttribute("aria-label", label);
    }
    el.dataset["abs"] = String(lo);
    el.classList.toggle("term-gap-trimmed", condemned);
    // Position by data-abs order, the same invariant the row inserts keep.
    const next = firstRowAtOrAfter(hi);
    if (el.parentElement !== output || el.nextElementSibling !== next) {
      output.insertBefore(el, next);
    }
  }
}

// updateTrimMarker shows, hides and LABELS the top-of-store marker as the first
// child of output, driven by the store. It carries no data-abs, so
// insertRowInOrder (which compares numeric data-abs) never places a row before
// it; it stays pinned at the top.
//
// Three states, because there are three honest things to say about the history
// above what is held (docs/paged-scrollback.md §5.4):
//
//  - NOTHING, when the store holds index 0: there is no history above it.
//  - "earlier output trimmed", the PERMANENT statement. Without paging declared
//    — including the pre-ack instant — it is the store's own trim bookkeeping,
//    the pre-paging behavior and the right one when no fetch exists: the client
//    evicted those rows itself, which is a fact whatever the server can serve.
//    With paging declared it is EARNED instead: the paging floor has reached the
//    oldest held index (the server proved nothing at or below it survives — an
//    empty frontier reply lands exactly here), or the server's own retained edge
//    is at or above it.
//  - "earlier output not loaded", the RECOVERABLE statement, with paging
//    declared and neither of those proofs in hand. This is the state that was
//    missing: a bounded resume replay routinely leaves a fetchable frontier, and
//    the old single predicate either said nothing at all (silently presenting a
//    partial transcript as the beginning of the session) or said "trimmed" about
//    history the server still holds and the trigger is about to fetch.
//
// The frontier is deliberately NOT sourced from gap geometry: its lower edge is
// a policy question (what is still worth requesting), so an exhausted frontier
// still renders a marker here rather than vanishing when the gap closes.
function updateTrimMarker(): void {
  const label = topMarkerLabel();
  if (label === null) {
    if (trimMarkerEl !== null && trimMarkerEl.parentElement === output) {
      trimMarkerEl.remove();
    }
    return;
  }
  if (trimMarkerEl === null) {
    trimMarkerEl = document.createElement("div");
    trimMarkerEl.className = "term-trim-marker";
    trimMarkerEl.setAttribute("role", "status");
  }
  if (trimMarkerEl.textContent !== label) {
    trimMarkerEl.textContent = label;
    trimMarkerEl.setAttribute("aria-label", label);
  }
  // Mirrors the interior gap marker's class, so one rule styles "gone" in both
  // places and a reader learns the distinction once.
  trimMarkerEl.classList.toggle("term-gap-trimmed", label === TRIM_LABEL_GONE);
  if (trimMarkerEl.parentElement !== output || output.firstChild !== trimMarkerEl) {
    output.insertBefore(trimMarkerEl, output.firstChild);
  }
}

/** The permanent label: nothing below the oldest held index can be recovered. */
const TRIM_LABEL_GONE = "earlier output trimmed";
/** The recoverable label: history above exists and a fetch can bring it back. */
const TRIM_LABEL_PENDING = "earlier output not loaded";

/** Which top-of-store statement is true, or null for no marker (§5.4). */
function topMarkerLabel(): string | null {
  const oldest = store.oldestIndex();
  if (oldest <= 0) {
    return null; // holds index 0, or holds nothing: no history above it
  }
  if (!store.pagingDeclared()) {
    return store.hasTrimmedHistory() ? TRIM_LABEL_GONE : null;
  }
  const serverOldest = store.serverOldestIndex();
  const condemned =
    store.pagingFloorIndex() >= oldest || (serverOldest >= 0 && serverOldest >= oldest);
  return condemned ? TRIM_LABEL_GONE : TRIM_LABEL_PENDING;
}

// upsertRow builds or updates the DOM row for an absolute index, or removes it
// if the store no longer holds it. New rows are inserted in ascending data-abs
// order.
function upsertRow(abs: number): void {
  const runs = store.getLine(abs);
  if (runs === undefined) {
    const stale = rowEls.get(abs);
    if (stale) {
      stale.remove();
      removedRowsThisPass = true;
      rowEls.delete(abs);
    }
    return;
  }
  const spans = buildRowSpans(runs);
  let el = rowEls.get(abs);
  if (el === undefined) {
    el = document.createElement("div");
    el.className = "term-row";
    el.dataset["abs"] = String(abs);
    el.replaceChildren(...spans);
    insertRowInOrder(el, abs);
    rowEls.set(abs, el);
  } else {
    el.replaceChildren(...spans);
  }
}

// insertRowInOrder places a freshly-created row element among output's
// children so they stay in ascending data-abs order. The common case (a new
// highest index) is an O(1) append; out-of-order inserts scan for the slot.
function insertRowInOrder(el: HTMLDivElement, abs: number): void {
  const last = output.lastElementChild as HTMLElement | null;
  if (last === null || rowAbs(last) < abs) {
    output.appendChild(el);
    return;
  }
  for (const child of output.children) {
    if (rowAbs(child as HTMLElement) > abs) {
      output.insertBefore(el, child);
      return;
    }
  }
  output.appendChild(el);
}

function rowAbs(el: HTMLElement): number {
  const v = el.dataset["abs"];
  return v === undefined ? -1 : Number(v);
}

// --- Alt screen (ephemeral grid; no history) ---
let altRendered = false;
// The alt row arrays rendered by the last flush, by grid index. Row identity
// is the store's change signal (applyScreen swaps exactly the changed rows'
// arrays; getAltRows returns the live references), so `prev[y] === rows[y]`
// means row y's DOM is already current. A separate MUTABLE container from the
// store's own array (which is mutated in place across frames): the full build
// snapshots it once, and the reconcile path updates only the entries it
// rebuilt — the previous per-frame `rows.slice()` allocated a fresh array on
// every alt flush (60fps GC churn under a full-screen TUI) for identical
// semantics. Cleared on alt exit and re-init.
let altPrevRows: (readonly WireRun[])[] = [];

function renderAlt(rows: readonly (readonly WireRun[])[]): void {
  rowEls.clear();
  // Full (re)build: first alt frame, a grid-height change (resize), or a
  // desynced DOM (defensive; e.g. a consumer poked the subtree).
  if (!altRendered || output.children.length !== rows.length) {
    altRendered = true;
    const els: HTMLDivElement[] = [];
    for (const runs of rows) {
      const div = document.createElement("div");
      div.className = "term-row";
      div.replaceChildren(...buildRowSpans(runs));
      els.push(div);
    }
    // This replaceChildren REMOVES rows, and on the first alt frame it removes
    // the entire main-buffer scrollback in favour of one screen of grid: the
    // largest shrink in this module after rebuild()'s wipe. The alt branch in
    // flushRenderInner returns before the shared bookkeeping, so the flag is set
    // here or not at all, and without it the pass reached neither
    // noteContentShrink (leaving the clamp to be inferred from its lossy
    // arithmetic signature, which can silently switch auto-follow off) nor
    // reconcileScrollRange (leaving the viewport parked past the grid).
    //
    // Over-announcing is harmless by mechanism: the resize and desynced-DOM
    // cases reach this branch too, and both seams are position-gated, so a
    // rebuild that moves nothing and strands nothing announces nothing.
    removedRowsThisPass = true;
    output.replaceChildren(...els);
    altPrevRows = rows.slice();
    return;
  }
  // Reconcile in place: rebuild only rows whose array identity changed. A
  // full-screen TUI that repaints everything (htop) rebuilds every row either
  // way; one that repaints a few lines (vim editing, a progress bar) now
  // touches only those rows' DOM — measured ~50x less flush CPU in the render
  // bench's partial-update scenario, and the browser skips layout for
  // untouched rows.
  for (let y = 0; y < rows.length; y++) {
    if (altPrevRows[y] === rows[y]) {
      continue;
    }
    const div = output.children[y] as HTMLDivElement;
    // eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- y < rows.length === children.length
    div.replaceChildren(...buildRowSpans(rows[y]!));
    // eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- same bound as above
    altPrevRows[y] = rows[y]!;
  }
}

/** Pin the viewport to the bottom iff the user is following. The scroll
 *  controller (scroll.ts) owns scrollTop and the follow/hold decision. */
function stickToBottomIfFollowing(): void {
  scroll.stickToBottom();
}

// --- Cursor blink ---
const CURSOR_BLINK_MS = 530;
// While the application hides the cursor (DECTCM — e.g. a full-screen TUI
// that paints its own cursor cell, like an agent front-end), the fast toggle
// would only restyle a display:none overlay, so the interval downshifts to
// this slow re-check: still polling for the cursor's return (a self-healing
// backstop even if a transition frame were somehow missed — the flush hook is
// the primary, instant restart), at a fraction of the 530ms blink's wakeups.
// For agent consumers the hidden-cursor state is the session's steady state.
const CURSOR_RECHECK_MS = 4000;
let blinkInterval: ReturnType<typeof setInterval> | null = null;
let blinkEnabled = true;
// What the running interval (if any) is configured as. syncCursorBlink()
// reconfigures only on mode change, so it is safe to call on every flush.
type BlinkMode = "off" | "idle" | "fast";
let blinkMode: BlinkMode = "off";

// hiddenDoc reports whether the page is currently background/hidden. The blink
// interval is gated on it: rAF-driven flushes already freeze in a hidden tab,
// but a plain setInterval keeps firing (throttled), so an idle hidden terminal
// would keep toggling a class — pointless wakeups that cost battery on mobile.
function hiddenDoc(): boolean {
  return typeof document !== "undefined" && document.visibilityState === "hidden";
}

// desiredBlinkMode folds the three gates into an interval mode: no timer at
// all when blinking is disabled or the page is hidden (both have event-driven
// restarts — the next mode frame's flush and visibilitychange), the fast
// phase toggle when the cursor is visible, and the slow re-check while the
// application hides it.
function desiredBlinkMode(): BlinkMode {
  if (!blinkEnabled || hiddenDoc()) {
    return "off";
  }
  return cursorHidden ? "idle" : "fast";
}

// syncCursorBlink reconciles the interval with the current gate state; every
// mode change resets the phase to solid. Called from the flush (blink mode +
// DECTCM state), the visibilitychange listener, init(), and the idle re-check
// itself. The blink class lives on termWrap: the caret overlay is a termWrap
// child (not inside output), and the `.cursor-blink-off .term-cursor`
// descendant selector must reach it.
function syncCursorBlink(): void {
  const mode = desiredBlinkMode();
  if (mode === blinkMode) {
    return;
  }
  blinkMode = mode;
  if (blinkInterval !== null) {
    clearInterval(blinkInterval);
    blinkInterval = null;
  }
  termWrap.classList.remove("cursor-blink-off");
  if (mode === "fast") {
    blinkInterval = setInterval(() => {
      termWrap.classList.toggle("cursor-blink-off");
    }, CURSOR_BLINK_MS);
  } else if (mode === "idle") {
    blinkInterval = setInterval(syncCursorBlink, CURSOR_RECHECK_MS);
  }
}

// Pause the interval while hidden; resume (cursor solid, phase reset) when the
// tab is foregrounded again. Registered once at module load — init() re-runs
// on re-attach, and a per-init registration would stack listeners.
if (typeof document !== "undefined") {
  document.addEventListener("visibilitychange", () => {
    // Pre-init (no termWrap yet) there is nothing to pause or resume.
    if ((termWrap as HTMLElement | undefined) === undefined) {
      return;
    }
    // blinkEnabled/cursorHidden keep their server-driven state across a
    // background stint, so foregrounding restores the right mode without
    // waiting for the next frame.
    syncCursorBlink();
  });
}

// --- Font metrics & sizing ---
/**
 * Re-measure the cell width/height from the rendered DOM. Call after any font
 * or zoom change so subsequent `computeSize()` and `getCursorPx()` use fresh
 * metrics.
 */
export function updateFontMetrics(): void {
  // Capture BEFORE the metrics change: a row-height change rescales every
  // offsetTop, so a holding reader's line moves under them with nothing to
  // correct it — the read anchor only measures drift across a FLUSH's own
  // mutations, and a pure restyle may not schedule one at all. `abs` (the line
  // identity, which is what the reader cares about) is exact regardless of when
  // this runs; `screenTop` can be up to one row stale if the layout changed
  // before this call, which is a sub-row error rather than a lost position.
  const before = captureViewMemory();
  const cs = window.getComputedStyle(termWrap);
  // Refresh the overlay-positioning padding cache from the same computed
  // style (one staleness contract for all cached box metrics).
  padLeft = parseFloat(cs.paddingLeft);
  padTop = parseFloat(cs.paddingTop);
  padValid = true;
  const fontSize = cs.fontSize;
  const family = cs.fontFamily;
  fontString = `${fontSize} ${family}`;
  widthFlat.fill(WIDTH_FLAT_UNSET);
  widthMap.clear();
  resetVariantContexts();
  const measuredW = measureCellWidth();
  cellWidth = Math.round(measuredW);
  cellHeight = parseFloat(cs.lineHeight) || 17;
  defaultSpacing = cellWidth - measuredW;
  output.style.letterSpacing = `${defaultSpacing}px`;
  document.documentElement.style.setProperty("--char-w", `${cellWidth}px`);
  // A holding reader's line is restored through the same machinery a tab switch
  // uses: arm it and let the next flush re-assert it. Idempotent when the metrics
  // did not actually change (the drift measures zero), and a no-op for a
  // following viewport, whose position the bottom pin already owns.
  // An armed restore SURVIVES this call, and its baseline has to be refreshed or
  // it does not: the reflow that provoked the call has already moved scrollTop,
  // so leaving lastWrote stale makes the next flush read that as a gesture and
  // cancel the very restore this branch is protecting. Declining to arm and
  // silently killing the existing arm would be the worst of both.
  if (pendingRestore !== null) {
    pendingRestore.lastWrote = scroll.currentScrollTop();
  }
  // NEVER over an armed restore. A resize settle calls this (through the
  // consumer's measurable-size check) and a tab switch reconnects, so the two
  // routinely overlap: replacing a bind's saved anchor with one measured
  // mid-rebuild would swap the reader's real position for a transient.
  if (before !== null && !before.following && pendingRestore === null) {
    pendingRestore = {
      view: before,
      gen: bindGen,
      lastWrote: scroll.currentScrollTop(),
      deadline: Date.now() + RESTORE_MAX_MS,
    };
    scheduleFlush();
  }
}

const MIN_COLS = 20;
const MIN_ROWS = 5;

/**
 * Compute the integer (cols, rows) the terminal element can fit at current
 * font metrics, clamped to a minimum of 20×5. Used to decide what dimensions
 * to send to the server in a `resize` control message.
 */
export function computeSize(): { cols: number; rows: number } {
  const cs = window.getComputedStyle(termWrap);
  const padX = parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight);
  const padY = parseFloat(cs.paddingTop) + parseFloat(cs.paddingBottom);
  const contentW = termWrap.clientWidth - padX;
  const contentH = termWrap.clientHeight - padY;
  const cols = Math.max(MIN_COLS, Math.floor(contentW / cellWidth));
  const rows = Math.max(MIN_ROWS, Math.floor(contentH / cellHeight));
  return { cols, rows };
}

// rowTopInTermWrap returns a row element's top in termWrap's coordinate space
// (the space every absolutely-positioned overlay — caret, predicted cursor,
// composition view — resolves against). A bare `offsetTop` is NOT that: it is
// relative to the element's offsetParent, and the UI package's CSS makes
// `.term-output` a positioned element (position: relative, for its own
// reasons), so rows report offsets in output-space,
// one padding/offset short of termWrap-space (the caret floated 4px above
// every glyph under the real stylesheet — invisible to the harness, whose
// unpositioned #out made both spaces coincide). Walk the offsetParent chain
// and accumulate until termWrap so the math is correct under EITHER
// stylesheet, and under any future wrapper the UI grows between the two.
function rowTopInTermWrap(el: HTMLElement): number {
  let top = 0;
  let node: Element | null = el;
  while (node instanceof HTMLElement && node !== termWrap) {
    top += node.offsetTop;
    node = node.offsetParent;
    // offsetTop is measured from the offsetParent's padding edge, but an
    // intermediate parent's own offsetTop locates its BORDER box — add its
    // top border so the accumulation stays exact if a wrapper ever grows one
    // (both current stylesheets use border: 0 here). termWrap itself is
    // excluded: absolute children resolve from its padding edge already.
    if (node instanceof HTMLElement && node !== termWrap) {
      top += node.clientTop;
    }
  }
  // Chain ended off termWrap (display:none, detached, or termWrap unpositioned
  // with the row offset-rooted elsewhere): fall back to rect delta, which is
  // space-independent. scrollTop re-bases the viewport-relative delta into
  // content space.
  if (node !== termWrap) {
    return (
      el.getBoundingClientRect().top - termWrap.getBoundingClientRect().top + termWrap.scrollTop
    );
  }
  return top;
}

// rowTopFor resolves the top of the row holding absolute line `abs`, in
// termWrap's coordinate space. Three positioners need that answer — the caret
// overlay, the predicted-cursor overlay, and `getCursorPx` for the consumer's
// IME view and hidden textarea — and they must not disagree, because they
// describe the same row. They did: `getCursorPx` fell back to a bare `padT`
// while the two that PAINT fell back to grid arithmetic, and `rowEls` is
// populated only by the main-screen row builder (`renderAlt` clears it), so for
// an ENTIRE alt-screen session the caret painted on the right row while the
// consumer was told the top of the terminal every frame.
//
// Three cases, in order:
//   - the map holds a built row: its own DOM offset, walked into termWrap space.
//   - no row for a real index (an alt-screen grid row, or a main row this flush
//     has not built yet): uniform-grid arithmetic from the window base.
//   - `abs < 0`: not a row at all. That is the state
//     `collapseContentSpaceOverlays` leaves behind, where the cursor genuinely
//     has no row and the content origin IS the accurate answer; the arithmetic
//     would put the overlay a cell above it.
function rowTopFor(abs: number): number {
  const el = rowEls.get(abs);
  if (el) {
    return rowTopInTermWrap(el);
  }
  const { padT } = termPadding();
  if (abs < 0) {
    return padT;
  }
  return padT + (abs - store.getWindow().base) * cellHeight;
}

/**
 * Returns the cursor's pixel position relative to the scroll container
 * (termWrap) — the coordinate space of the absolutely-positioned overlays
 * (predicted-cursor, IME composition view) — plus the current cell height.
 * Uses the cursor row's actual DOM offset, or the grid position of a row that
 * has none (see rowTopFor).
 */
export function getCursorPx(): { left: number; top: number; cellH: number } {
  const { padL } = termPadding();
  return {
    left: Math.round(padL + cursorCol * cellWidth),
    top: Math.round(rowTopFor(cursorAbs)),
    cellH: cellHeight,
  };
}

// --- Caret overlay ---
//
// The real cursor is a single absolutely-positioned element appended to
// termWrap (the positioned scroll container, same coordinate system as the
// predicted-cursor overlay), NOT a restyled span inside the cursor row. Rows
// stay pure content: cursor motion repositions this element and never
// rewrites row DOM, so a native selection survives typing — including on the
// cursor row itself — and the per-keystroke row rebuild is gone.
let cursorEl: HTMLElement | null = null;

function ensureCursorEl(): HTMLElement {
  if (cursorEl === null) {
    cursorEl = document.createElement("div");
    cursorEl.setAttribute("aria-hidden", "true");
    termWrap.appendChild(cursorEl);
  }
  return cursorEl;
}

// glyphAt returns the character occupying `col` in a row's runs, advancing
// columns exactly like buildRowSpans (each char one cell; the \uFFFF
// continuation placeholder occupies the wide char's second cell). A miss —
// cursor past end of text, empty row, or a continuation cell — reads as a
// space (the block cursor then paints an inverted blank, matching a real
// terminal).
function glyphAt(runs: readonly WireRun[] | undefined, col: number): string {
  if (!runs) {
    return " ";
  }
  let c = 0;
  for (const run of runs) {
    if (!run.t) {
      continue;
    }
    for (const ch of run.t) {
      if (c === col) {
        return ch === "\uFFFF" ? " " : ch;
      }
      c++;
    }
  }
  return " ";
}

// positionCursorOverlay moves/styles the caret overlay for the current cursor
// state. `runs` is the cursor row's content (main: store.getLine; alt: the
// grid row) — the block style copies the glyph under the cursor so the
// inverted cell looks exactly like the old inline-span cursor. Called at the
// end of every flush; hidden when the cursor is hidden or the screen is empty.
function positionCursorOverlay(runs: readonly WireRun[] | undefined): void {
  const el = ensureCursorEl();
  if (cursorHidden || cursorAbs < 0) {
    el.className = "term-cursor-overlay";
    return;
  }
  const { padL } = termPadding();
  // Alt-screen rows are not registered in rowEls; their grid is uniform, so the
  // row offset derives from the window-relative row index. Main-screen rows
  // exist by the time this runs (the flush force-builds the cursor row).
  // rowTopFor is shared with getCursorPx so the consumer's IME view cannot be
  // told a different row than the one the caret paints on.
  const top = rowTopFor(cursorAbs);
  const ch = glyphAt(runs, cursorCol);
  // A wide glyph (CJK, emoji) owns two cells; the overlay covers both, like
  // the old inline cursor span did via its continuation-spacer adjustment.
  const wide = measureChar(ch, false, false) > cellWidth * 1.5;
  el.textContent = ch === " " ? "\u00a0" : ch;
  el.className = `term-cursor-overlay visible ${cursorClassName()}`;
  el.style.left = `${Math.round(padL + cursorCol * cellWidth)}px`;
  el.style.top = `${Math.round(top)}px`;
  el.style.width = `${wide ? cellWidth * 2 : cellWidth}px`;
  el.style.height = `${cellHeight}px`;
  el.style.lineHeight = `${cellHeight}px`;
}

let predCursorEl: HTMLElement | null = null;

// Create the predicted-cursor overlay the renderer owns. Appended to termWrap
// (the positioned scroll container) so its absolute left/top math matches the
// row offsets. Styled by the `.pred-cursor` class from the UI CSS bundle. The
// renderer owning this means the engine never depends on a host-provided
// `#pred-cursor` scaffold element.
function createPredCursorEl(): HTMLElement {
  const el = document.createElement("div");
  el.className = "pred-cursor";
  el.setAttribute("aria-hidden", "true");
  termWrap.appendChild(el);
  return el;
}

/**
 * Show or hide a "predicted" cursor overlay at window-relative (row, col).
 * Useful for client-side echo of typed characters before the server
 * acknowledges them, over high-latency connections. The overlay element is
 * created lazily on first use (a consumer that never predicts never creates
 * it).
 */
export function setPredictedCursor(row: number, col: number, active: boolean): void {
  const el = predCursorEl ?? (predCursorEl = createPredCursorEl());
  const win = store.getWindow();
  const predAbs = win.base + row;
  if (!active || (predAbs === cursorAbs && col === cursorCol)) {
    el.classList.remove("visible");
    return;
  }
  const { padL } = termPadding();
  // Same resolver as the caret and getCursorPx. A row the flush has not built
  // yet answers from grid arithmetic, which for `predAbs >= 0` is exactly the
  // `padT + row * cellHeight` this used to compute inline. A prediction BELOW
  // the first line of the session (`predAbs < 0`) now lands on the content
  // origin instead of a negative top: that row does not exist, and an overlay
  // at a negative offset paints over the terminal's top padding.
  const top = rowTopFor(predAbs);
  el.style.left = `${Math.round(padL + col * cellWidth)}px`;
  el.style.top = `${Math.round(top)}px`;
  el.style.width = `${cellWidth}px`;
  el.style.height = `${cellHeight}px`;
  el.classList.add("visible");
}

/** Apply or remove the reverse-video class on the terminal output.
 *  When DECSCNM (mode 5) is active, default fg/bg are swapped via CSS. */
export function updateReverseVideo(): void {
  if (isReverseVideo()) {
    termWrap.classList.add("term-reverse-video");
  } else {
    termWrap.classList.remove("term-reverse-video");
  }
}
