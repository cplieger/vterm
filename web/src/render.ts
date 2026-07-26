// Render layer: store-backed, absolute-index DOM rows.
//
// The renderer owns a LineStore (the authoritative client model) and reflects
// it to the DOM. Every terminal line is a `div.term-row` carrying a `data-abs`
// attribute equal to its absolute index; the rows sit in one natively-scrolled
// container in absolute order. There is no separate "live zone" and
// "scrollback": the live window is simply the last `height` absolute indices,
// and a line that scrolls into history just stops being updated. This is what
// removes the live/history reconciliation that caused the duplicate-rows and
// view-jumping bugs (see the #web-terminal-engine steering doc, "Design rationale").
//
// Decode frames feed the store (handleScreen/handleScroll); a single
// requestAnimationFrame flush drains the store's change set and applies it:
// evicted indices drop their row, dirty indices build/update their row in
// place. The window block always has `height` rows, so scrollHeight only grows
// when real history commits — never oscillating mid-redraw.

import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";
import { LineStore } from "./store.js";
import * as scroll from "./scroll.js";
import { isReverseVideo } from "./modes.js";

// --- Width cache (two-tier, xterm.js style) ---
const WIDTH_FLAT_SIZE = 256;
const WIDTH_FLAT_UNSET = -9999;
const widthFlat = new Float32Array(WIDTH_FLAT_SIZE).fill(WIDTH_FLAT_UNSET);
const widthMap = new Map<string, number>();

const VARIANT_REGULAR = 0;
const VARIANT_BOLD = 1;
const VARIANT_ITALIC = 2;
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

/**
 * Initialize the renderer by attaching it to a pair of DOM elements: the
 * scrollable terminal wrapper and the inner output container that receives
 * row elements. Must be called once before any handleScreen/handleScroll call.
 *
 * @param opts.output      Inner element that holds row children.
 * @param opts.termWrap    Outer scroll container.
 * @param opts.onCursorMove Optional callback invoked when the cursor moves.
 */
export function init(opts: {
  output: HTMLElement;
  termWrap: HTMLElement;
  onCursorMove?: () => void;
}): void {
  output = opts.output;
  termWrap = opts.termWrap;
  onCursorMove = opts.onCursorMove ?? null;
  // New attach target: the cached padding belongs to the previous termWrap.
  padValid = false;
  // Fresh attach: drop any prior model + DOM so re-init (and vitest's
  // non-isolated module reuse) starts clean.
  store.reset();
  rowEls.clear();
  renderQueue.clear();
  trimMarkerEl = null;
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
 */
export function bind(next: LineStore): void {
  store = next;
  rebuild();
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
 */
export function rebuild(): void {
  output.replaceChildren();
  rowEls.clear();
  renderQueue.clear();
  trimMarkerEl = null;
  cursorAbs = -1;
  altRendered = false;
  altPrevRows = [];
  // Fresh surface: a stale give-up streak from a prior store must not deny the
  // rebuilt surface its full transient-retry budget.
  renderNoProgressStreak = 0;
  // Alt screen paints from the ephemeral grid in the flush, so it needs no
  // absolute-index queueing here.
  if (!store.isAlt()) {
    queueRowsViewportFirst();
  }
  scheduleFlush();
}

/**
 * Highest absolute line index the client holds, or -1 if empty. This is the
 * resume `haveThrough` value (it replaces the old DOM-row count). Exposed so
 * the connection layer can request only the lines the client is missing.
 */
export function getHighestIndex(): number {
  return store.highestIndex();
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
  store.applyScreen(msg);
  scheduleFlush();
}

/**
 * Apply a `ScrollMessage`: commit history lines into the store by absolute
 * index, then schedule a render flush.
 */
export function handleScroll(msg: ScrollMessage): void {
  store.applyScroll(msg);
  scheduleFlush();
}

function scheduleFlush(): void {
  if (pendingFrame !== undefined) {
    return;
  }
  pendingFrame = requestAnimationFrame(flushRender);
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
  /** Where the row sat ON SCREEN: offsetTop minus the scroll offset. Measuring
   *  the screen position rather than the container offset is what makes the
   *  correction idempotent — see restoreReadAnchor. */
  screenTop: number;
}

function captureReadAnchor(): ReadAnchor | null {
  if (!scroll.isUserScrolledUp()) {
    return null; // following: stickToBottom owns the position
  }
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
  // Everything is above the viewport top (a shrink already clamped past the
  // last row): anchor on the last row rather than giving up.
  const el = found ?? (kids[kids.length - 1] as HTMLElement);
  return { el, screenTop: el.offsetTop - offset };
}

function restoreReadAnchor(anchor: ReadAnchor | null): void {
  if (anchor?.el.parentElement !== output) {
    return; // not holding, or the anchor row was itself evicted (mass trim)
  }
  // Correct by how far the row DRIFTED ON SCREEN, not by how much the content
  // above it changed. The two differ exactly when the browser already did this
  // itself: Chrome and Firefox have native scroll anchoring, so their offsetTop
  // change comes with a matching scrollTop change and the screen position is
  // already right — this then measures zero drift and does nothing. Correcting
  // the content delta instead would double-compensate there and throw the view
  // the other way. On Safari, which has no native anchoring, the drift is the
  // whole content delta and this is the only thing that fixes it.
  const drift = anchor.el.offsetTop - scroll.currentScrollTop() - anchor.screenTop;
  scroll.adjustForContentShift(drift);
}

function flushRender(): void {
  pendingFrame = undefined;
  // Measured BEFORE any DOM mutation, so the post-mutation read below reveals
  // exactly how much the content above the reading position moved.
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
  // Two position invariants, applied after every DOM mutation and in this order:
  // hold the reader's line against content that shifted above it, then (only if
  // following) pin to the new bottom. They are mutually exclusive by
  // construction — the anchor is null while following — so the order is for
  // readability, not correctness.
  restoreReadAnchor(anchor);
  // Single auto-follow invariant, applied after every DOM mutation.
  stickToBottomIfFollowing();
}

function flushRenderInner(): void {
  // Forward-progress accounting for the bounded error-path reschedule. Reset at
  // entry, incremented per drained row below; a mid-drain throw leaves it at
  // the count-so-far for flushRender's catch to read.
  flushDrainedThisPass = 0;
  const ch = store.drainChanges();

  if (ch.fullReset) {
    output.replaceChildren();
    rowEls.clear();
    renderQueue.clear();
    trimMarkerEl = null;
    cursorAbs = -1;
  } else {
    for (const abs of ch.evictedLines) {
      const el = rowEls.get(abs);
      if (el) {
        el.remove();
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
    output.replaceChildren();
    rowEls.clear();
    renderQueue.clear();
    trimMarkerEl = null;
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
  for (const abs of [...inWindow, ...belowWindow]) {
    if (flushDrainedThisPass >= MAX_ROWS_PER_FRAME) {
      break;
    }
    upsertRow(abs);
    renderQueue.delete(abs);
    flushDrainedThisPass++;
  }

  updateTrimMarker();

  // More rows pending: keep draining on subsequent frames.
  if (renderQueue.size > 0) {
    scheduleFlush();
  }

  positionCursorOverlay(store.getLine(cursorAbs));

  if (onCursorMove) {
    onCursorMove();
  }
}

// updateTrimMarker shows or hides the "earlier output trimmed" marker as the
// first child of output, driven by the store. It appears when history older
// than the store holds was trimmed — either the client evicted its oldest
// lines at the cap, or the server reported (via resumeAck bounds) that it no
// longer retains history the client was missing on resume. The marker carries
// no data-abs, so insertRowInOrder (which compares numeric data-abs) never
// places a row before it; it stays pinned at the top.
function updateTrimMarker(): void {
  if (store.hasTrimmedHistory()) {
    if (trimMarkerEl === null) {
      trimMarkerEl = document.createElement("div");
      trimMarkerEl.className = "term-trim-marker";
      trimMarkerEl.setAttribute("role", "status");
      trimMarkerEl.setAttribute("aria-label", "earlier output trimmed");
      trimMarkerEl.textContent = "earlier output trimmed";
    }
    if (trimMarkerEl.parentElement !== output || output.firstChild !== trimMarkerEl) {
      output.insertBefore(trimMarkerEl, output.firstChild);
    }
  } else if (trimMarkerEl !== null && trimMarkerEl.parentElement === output) {
    trimMarkerEl.remove();
  }
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
// means row y's DOM is already current. Cleared on alt exit and re-init.
let altPrevRows: readonly (readonly WireRun[])[] = [];

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
  }
  altPrevRows = rows.slice();
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

/**
 * Returns the cursor's pixel position relative to the scroll container
 * (termWrap) — the coordinate space of the absolutely-positioned overlays
 * (predicted-cursor, IME composition view) — plus the current cell height.
 * Uses the cursor row's actual DOM offset.
 */
export function getCursorPx(): { left: number; top: number; cellH: number } {
  const { padL, padT } = termPadding();
  const el = rowEls.get(cursorAbs);
  const top = el ? rowTopInTermWrap(el) : padT;
  return {
    left: Math.round(padL + cursorCol * cellWidth),
    top: Math.round(top),
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
  const win = store.getWindow();
  const { padL, padT } = termPadding();
  const rowEl = rowEls.get(cursorAbs);
  // Alt-screen rows are not registered in rowEls; their grid is uniform, so
  // the row offset derives from the window-relative row index. Main-screen
  // rows exist by the time this runs (the flush force-builds the cursor row).
  // rowTopInTermWrap (not bare offsetTop) so the top is in termWrap space
  // regardless of which ancestor is the rows' offsetParent.
  const top = rowEl ? rowTopInTermWrap(rowEl) : padT + (cursorAbs - win.base) * cellHeight;
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
  const { padL, padT } = termPadding();
  const rowEl = rowEls.get(predAbs);
  const top = rowEl ? rowTopInTermWrap(rowEl) : padT + row * cellHeight;
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
