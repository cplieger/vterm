// Absolute-index line store: the client's authoritative model of the
// terminal. One buffer keyed by absolute line index, with the live screen
// window sliding along it. This is the data model that resolves the
// live/history split: there is no separate
// "live zone" and "scrollback" here, only lines addressed by absolute index,
// the last `height` of which happen to still be changing.
//
// The store is pure (no DOM). The renderer (render.ts) reads from it and
// reflects changes to the DOM; the connection layer feeds decoded wire
// messages into it. Applying a line is idempotent by absolute index, which is
// what makes re-delivery (resume replay, fast-burst re-send, a doubled frame
// from a zombie socket) incapable of duplicating a row.

import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";
import { interiorGaps, retainedIntervals, type Interval } from "./intervals.js";

/**
 * The tail cap held while paging is unconfirmed — the legacy default, so a
 * pairing with a server that cannot serve history back never has its reachable
 * history cut.
 *
 * This is the SOURCE of the default cap, not a second name for it: `MAX_LINES`
 * derives from it below. The two were independent literals, so editing this one
 * — the exported, documented, obviously-intended knob — changed nothing except
 * which assertion failed.
 */
export const COMPATIBILITY_TAIL_CAP = 5000;

/**
 * Maximum lines retained client-side. Older lines are evicted from the top.
 * Identical to the compatibility cap by construction: the depth a client holds
 * when nothing has told it to hold less IS the pre-flip cap.
 */
const MAX_LINES = COMPATIBILITY_TAIL_CAP;

/**
 * SNAPSHOT_VERSION is the shape version of a persisted store snapshot. Bump it
 * on any change to StoreSnapshot's fields or their meaning; fromSnapshot rejects
 * anything that does not match, so an older snapshot is discarded rather than
 * misread. There is deliberately no migration path: a discarded snapshot costs
 * one full resume, which is exactly the behavior of not having persisted at all.
 *
 * One carve-out, because the rule above exists to prevent MISREADING and this
 * cannot be misread: adding an OPTIONAL field whose absence is defined to mean
 * exactly the pre-existing behaviour needs no bump. `fromSnapshot` reads fields
 * individually off an `unknown` record and validates each, so an older entry
 * simply lacks the key and takes the documented absent path. The test is whether
 * a v-matching entry written by the previous release still hydrates to the same
 * store it would have before; if it does not, bump. `unconfirmedFrom` is the
 * first field added under this carve-out.
 */
const SNAPSHOT_VERSION = 1;

/**
 * A persisted line store, as plain data: no class instances, no functions, no
 * cycles, so it survives structuredClone and can go straight into IndexedDB
 * without a bespoke serializer (WireRun is primitives only — see types.ts).
 *
 * What is deliberately NOT here, and must not be added without re-reading why:
 *
 * - The live window (`WindowState`). It is last session's geometry, and it is
 *   load-bearing in two places that would silently misbehave under a stale
 *   value: `inWindow()` is what exempts live-window rows from the staleness
 *   guard and from cap eviction. The first screen frame re-establishes it.
 * - `serverOldest`. It is the server's retained-history bound from a resumeAck,
 *   and the next resumeAck supplies the current one. A stale value would make
 *   the "earlier output trimmed" marker report on a range that no longer exists.
 * - The alternate screen (`alt`, `altRows`). Ephemeral by definition: an alt
 *   grid is not history and is never addressed by absolute index.
 */
export interface StoreSnapshot {
  /** Shape version; must equal SNAPSHOT_VERSION or the snapshot is discarded. */
  v: number;
  /**
   * The server boot epoch these lines belong to, as last reported by a
   * resumeAck, or 0 when the client never learned one.
   *
   * This is the field that makes persistence safe rather than actively harmful.
   * Absolute line indices are only meaningful within one server process: a
   * restarted server begins again at 0. Restart detection is otherwise
   * in-memory — the first resumeAck of a page load records the epoch with
   * nothing to compare against — so a hydrated store plus a restarted server
   * would present stale content as live AND then silently drop the new
   * session's output, whose low indices fall at or below the hydrated
   * everEvictedThrough and are refused by apply-line guard 2.
   *
   * The consumer must therefore hand this to the connection layer before
   * connecting (see connection.adoptPersistedEpoch), so the first resumeAck
   * has something to compare against and the existing restart path fires.
   */
  serverEpoch: number;
  /** Lowest absolute index in `lines`.
   *
   *  Write-side metadata: `fromSnapshot` derives the bounds from the pairs it
   *  actually accepted rather than trusting these, so a corrupt value here changes
   *  nothing on the way back in. They are carried because a consumer's storage
   *  layer wants them without parsing the payload (the keeper records `highest` as
   *  its save watermark), and they are deliberately NOT validated against the pairs
   *  — a check with no failure mode behind it is worse than none. */
  oldest: number;
  /** Highest absolute index in `lines`. Write-side metadata; see `oldest`. */
  highest: number;
  /**
   * The saving store's provisional floor: the lowest index at or above which its
   * content had not been confirmed as committed history by the server. ABSENT
   * when everything it held was confirmed, and absent in every snapshot written
   * before this field existed, which is the same state and takes the same path.
   *
   * Carried because the alternative is a reloaded page telling the server "no
   * need to re-send" about screen rows it only ever saw the application draw.
   * The window descriptor cannot serve here (see above: a stale one is
   * load-bearing in two places), and this is not geometry — it is one index,
   * meaningless to interpret as anything else.
   *
   * `fromSnapshot` treats a malformed value as absent rather than rejecting the
   * snapshot. Being wrong in that direction costs a replayed screenful; rejecting
   * would throw away a whole session's history over a field that is an
   * optimisation of correctness, not a correctness precondition.
   */
  unconfirmedFrom?: number;
  /**
   * The retained lines as [absoluteIndex, runs] pairs, ascending. Pairs rather
   * than an object keyed by index, because object keys stringify and absolute
   * indices are numbers; pairs clone as-is and read plainly in a debugger.
   */
  lines: [number, WireRun[]][];
}

/**
 * isWireRunArray reports whether a value decoded from OUTSIDE this program is a
 * usable run array.
 *
 * This is the only path into a store that admits externally-sourced run objects:
 * the wire decoder constructs each WireRun field by field from typed cursor reads,
 * so `t` is a string by construction and applyLine's own `Array.isArray(runs)`
 * check inherits that guarantee. A snapshot inherits nothing, and the difference
 * is not academic — a run whose `t` is not a string reaches the renderer's
 * per-character loop and throws, the row stays queued because the drain only
 * clears a row after a successful build, and every row above it is blocked. That
 * failure is permanent and self-perpetuating (the store hydrated, so the entry is
 * never dropped, so the next reload breaks the same way), where every other
 * rejection here degrades to "nothing restored".
 *
 * Optional numeric fields are checked when present rather than defaulted: a
 * string in `f` would flow into a colour lookup, and silently coercing it would
 * hide a producer bug instead of rejecting a payload this store cannot trust.
 */
function isWireRunArray(value: unknown): value is WireRun[] {
  if (!Array.isArray(value)) {
    return false;
  }
  for (const run of value) {
    if (typeof run !== "object" || run === null) {
      return false;
    }
    const r = run as Record<string, unknown>;
    if (typeof r["t"] !== "string") {
      return false;
    }
    for (const field of ["f", "b", "uc", "a"]) {
      const v = r[field];
      if (v !== undefined && (typeof v !== "number" || !Number.isFinite(v))) {
        return false;
      }
    }
    if (r["u"] !== undefined && typeof r["u"] !== "string") {
      return false;
    }
  }
  return true;
}

/**
 * evictionBatch returns how many lines an at-cap eviction pass NOMINALLY frees
 * (the hysteresis band), derived from the cap so tiny test stores keep
 * one-at-a-time eviction (a batch of 1 IS the pre-batching behavior) while the
 * real 5000-line store frees 256 rows per pass. A pass can free fewer when the
 * retained history above the cap is smaller than the batch: headroom is taken
 * from evictable HISTORY only, never from the protected live window, so a cap
 * close to the screen height degrades toward per-line eviction (see the
 * operating-range note on the consumer option docs).
 *
 * Why evictions are batched at all: every evicted line removes a DOM row from
 * the TOP of the scroller, which shifts every row below it and invalidates the
 * whole scrolled-contents layer. At the cap, one-at-a-time eviction turns a
 * streaming session into one whole-layer invalidation PER LINE — on WebKit
 * (which re-rasterizes the resident tiles each time) that is the single largest
 * compositor cost this client can trigger, and on a memory-pressured iPhone it
 * is churn the page cannot afford. Batching keeps appends eviction-free between passes, so the
 * layer shifts once per batch instead of once per line. The retained count is
 * hard-bounded at `max(maxLines, live-window height)` after every apply — the
 * window is never evicted (a cap at or below the screen height keeps the full
 * screen with zero scrollback), and everything else stays under the cap
 * whatever shape the frames arrive in (enforceCap's skip-over-window walk
 * also bounds content committed above a stale window, e.g. a resume replay).
 */
function evictionBatch(maxLines: number): number {
  return Math.min(256, Math.max(1, Math.floor(maxLines / 16)));
}

/**
 * Demand-paged-scrollback residency constants (docs/paged-scrollback.md §3).
 *
 * The store holds a small RESIDENT TAIL plus a disposable BROWSE CACHE of
 * pages fetched on demand. The two budgets are enforced by separate mechanisms
 * that can never touch each other's lines: live output cannot evict browse
 * cache, and browse pressure cannot evict the tail.
 */

/**
 * The resident-tail target once the server has DECLARED it serves paging. The
 * compatibility value below is what the tail holds until then.
 */
export const RESIDENT_TAIL_CAP = 1500;

/** Lines of paged-in history retained as disposable cache. Engine-internal. */
export const BROWSE_CACHE_CAP = 2500;

/**
 * How close the viewport must come to an absent range's edge before a fetch is
 * triggered, and the radius of the eviction exemption around the reader. It is
 * both because they are the same idea: the band worth prefetching is the band
 * worth not evicting.
 */
export const PREFETCH_THRESHOLD = 500;

/** Lines requested per page, and the ceiling on one eviction pass's removals. */
export const PAGE_SIZE = 1000;

/**
 * The viewport exemption can never pin browse membership above its cap. Checked
 * statically here rather than left as a comment, because the eviction loop's
 * termination argument at the page-apply target rests on it: if the exemption
 * could cover the whole cache, a pass with work to do could find no victim.
 */
if (2 * PREFETCH_THRESHOLD + 1 >= BROWSE_CACHE_CAP) {
  throw new Error(
    `paged-scrollback invariant violated: the viewport exemption (${2 * PREFETCH_THRESHOLD + 1} lines) ` +
      `must stay below browseCacheCap (${BROWSE_CACHE_CAP})`,
  );
}

/** The live screen window: a fixed `height`-row block at the tail of the buffer.
 *  Describes the MAIN screen even while the alternate screen is active: the alt
 *  grid's geometry lives in the store's altRows, so during an alt session that
 *  outlived a resize, `base`/`height` report the main region alt exit restores,
 *  not the current terminal size. */
export interface WindowState {
  /** Absolute index of window row 0 (the MAIN screen's; frozen during alt). */
  base: number;
  /** Number of rows in the window — the MAIN screen's height, which equals
   *  the terminal height except during an alt session that resized (then the
   *  alt grid's height lives in altRows and this keeps the restore target). */
  height: number;
  /** Cursor row within the window (0..height-1). */
  cursorRow: number;
  /** Cursor column within the window. */
  cursorCol: number;
  /** DECSCUSR cursor style (0-6). */
  cursorStyle: number;
  /** Cursor hidden (DECTCEM off). */
  cursorHidden: boolean;
  /** Cursor blinking. */
  cursorBlink: boolean;
}

/** What changed since the last drain, for the renderer to apply to the DOM. */
export interface StoreChanges {
  /** Absolute indices whose content changed (need a DOM row build/update). */
  dirtyLines: number[];
  /** Absolute indices removed from the store (need their DOM row dropped). */
  evictedLines: number[];
  /** The window descriptor or cursor changed. */
  windowChanged: boolean;
  /** The alternate-screen grid or its active state changed. */
  altChanged: boolean;
  /** A full reset happened (server restart): the renderer must wipe all rows. */
  fullReset: boolean;
}

function emptyWindow(): WindowState {
  return {
    base: 0,
    height: 0,
    cursorRow: 0,
    cursorCol: 0,
    cursorStyle: 0,
    cursorHidden: false,
    cursorBlink: false,
  };
}

/** Deep-equality for two style runs (all wire fields). */
function runEqual(a: WireRun, b: WireRun): boolean {
  return a.t === b.t && a.f === b.f && a.b === b.b && a.a === b.a && a.uc === b.uc && a.u === b.u;
}

/** Deep-equality for two rows of runs. */
function rowEqual(a: WireRun[], b: WireRun[]): boolean {
  if (a.length !== b.length) {
    return false;
  }
  for (let i = 0; i < a.length; i++) {
    // eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- index < length
    if (!runEqual(a[i]!, b[i]!)) {
      return false;
    }
  }
  return true;
}

export class LineStore {
  private lines = new Map<number, WireRun[]>();
  private oldest = -1; // lowest retained absolute index (-1 = empty)
  private highest = -1; // highest retained absolute index (-1 = empty)
  private everEvictedThrough = -1; // highest absolute index ever evicted; lines <= this are stale
  private serverOldest = -1; // oldest index the server still retains (from resumeAck); for trim marker

  private win: WindowState = emptyWindow();
  private alt = false;
  private altRows: WireRun[][] = [];

  // Change tracking, drained by the renderer each frame.
  private dirty = new Set<number>();
  private evicted = new Set<number>();
  private windowDirty = false;
  private altDirty = false;
  private resetPending = false;

  // --- demand-paged scrollback (docs/paged-scrollback.md §5.1, §5.3) ---

  /**
   * The RESIDENT TAIL cap, and the one deliberately mutable piece of residency
   * policy: the capability the server declares in its resumeAck decides it at
   * runtime. It starts at the compatibility value and flips ONE-WAY to the
   * supported target through `applyResumeAck`. Every residency reader uses this
   * value — the tail-budget gate, `evictionBatch`, and `snapshot`'s default
   * bound — so a flip moves all of them together.
   */
  private effectiveTailCap: number;

  /** The post-flip target: what the tail shrinks to once paging is declared. */
  private readonly supportedTarget: number;

  /** The pre-flip cap: what the tail holds while paging is unconfirmed. */
  private readonly compatibilityCap: number;

  /**
   * The keys currently held as BROWSE CACHE — a subset of `lines`' keys, and
   * the cache's whole model: membership, size and eviction candidates all read
   * it directly. It drives cap accounting, eviction and snapshot exclusion. It
   * is NOT a guard-2 bypass — re-delivery into a retained index passes only
   * through applyLine's idempotence, or arrives under a fresh
   * `solicitedPending`.
   *
   * A SET of keys rather than a set of intervals, and no companion counter.
   * Both alternatives were built first and both leaked defects that this shape
   * cannot express:
   *
   *  - INTERVALS described ranges the store need not hold, so every question
   *    ("is the reader on cache?", "how many cache lines?", "which to evict?")
   *    became a walk over a numeric SPAN. The replay-jump band is a hull from
   *    `oldest` through the client's `haveThrough`, so that span is as wide as
   *    the client is behind: a 500k hull measured 2.3 s of main-thread work for
   *    ten real rows, and 80M measured 1.2 s in the one query that still walked
   *    it. Keys are bounded by residency, so every question here is O(cache).
   *  - A COMPANION COUNT (`browseCount`) had to be maintained by arithmetic at
   *    every mutation, and was wrong at four of them — it reached -96 on a third
   *    successive jump ack. `browse.size` cannot drift because it is not
   *    maintained at all.
   *
   * The one invariant is `browse ⊆ lines.keys()`, and it holds because `forget`
   * is the sole deletion path and deletes from both.
   */
  private browse = new Set<number>();

  /**
   * The window of the request currently in flight, or null. Lines inside it are
   * applyable even below prior evictions — paging is exactly a legitimate
   * re-fetch below the stale-re-send watermark, which is why the watermark
   * doctrine needed a solicited-range exception rather than a weaker guard.
   * Exactly one slot, matching single-flight: there is nothing to expire.
   */
  private solicitedPending: Interval | null = null;

  /**
   * The lowest index still worth requesting. Raised by a clamped or empty
   * reply (the server proving nothing at or below it survives), lowered only by
   * a resumeAck reporting an older retained edge — which within one epoch is
   * the repair path for a mis-correlated clamp.
   */
  private pagingFloor = 0;

  /** When browse cache was last created or read, for the consumer's TTL. */
  private browseActivityMs = 0;

  /**
   * Whether the last resumeAck declared paging. Kept because the RENDERER needs
   * it and cannot derive it: §5.4's top-of-store marker says something different
   * about the history above what is held depending on whether a fetch can bring
   * it back.
   *
   * A BOOLEAN, false until an ack says otherwise, not a tri-state with a
   * pre-ack "unknown". The pre-ack instant and a declared `unsupported` want the
   * same answer — assert only what the client's own bookkeeping proves — so a
   * third state would be a distinction nothing reads.
   */
  private paging = false;

  /**
   * The highest index the APPLICATION erased (ED3), or -1. A second watermark
   * beside `everEvictedThrough` because the two answer different questions and
   * only one of them is a trim.
   *
   * `everEvictedThrough` means "we dropped this to stay inside a memory budget",
   * and it drives the "earlier output trimmed" affordance. ED3 is not that: the
   * app discarded its own scrollback, so a marker would be permanent noise on
   * kiro-cli (which clears on every resize redraw), which is why ED3 deliberately
   * leaves it alone. That left NOTHING refusing an erased index, and a page reply
   * whose data timeout had already released single-flight comes back
   * UNCORRELATED — through the ordinary scroll path, classified TAIL, so outside
   * the cache budget and the TTL both. It never leaves.
   *
   * Refusing an erased index is unconditionally safe within an epoch: the server's
   * ring `Clear()` preserves `committed`, so an erased absolute index is never
   * reused, and no legitimate later frame can carry one. The companion reprint an
   * app emits in the same tick lands at NEW indices above everything the client
   * held, which is why this watermark tracks what was actually DROPPED rather than
   * the cleared bound — the bound would also refuse that reprint.
   */
  private erasedThrough = -1;

  /**
   * Memo for `retainedRanges()`, invalidated by every key-set mutation.
   *
   * Coalescing the keys costs a copy and a sort of the whole residency, and the
   * scroll seam asks for it on EVERY scroll event (the fetch trigger reads the
   * gaps near the viewport) while the renderer asks again on every flush (the
   * gap markers are re-derived, not tracked). Measured at 4500 retained rows:
   * 95 us per call, so 5.7 ms per 60-event scroll frame — a third of a frame
   * budget, on the phone this whole feature exists for, during the one
   * interaction it exists to serve. The key set changes far less often than it
   * is asked about, so the answer is computed once per change instead of once
   * per question.
   *
   * Invalidation is sound for the same reason the browse set is: insertion has
   * one owner (`applyLine`) and deletion has one owner (`forget`), so there are
   * exactly two places to clear it, plus `reset`. The snapshot hydrate writes
   * keys directly and deliberately clears NOTHING: it builds a store whose memo
   * is still unset, and an invalidation there is unreachable by construction
   * (verified by mutation — the test that covers it fails only if a future edit
   * reads the geometry before the keys land).
   */
  private rangesMemo: Interval[] | null = null;

  /**
   * @param maxLines  retained-line cap, or OMITTED for the engine's choice.
   *                  Optional rather than defaulted, because the two are not
   *                  the same statement: "omitted" selects the engine's own
   *                  post-flip target, and expressing that as a default VALUE
   *                  made the consumer's explicit `5000` indistinguishable from
   *                  silence — a UI passing `scrollbackLines: 5000` had its cap
   *                  silently replaced by the small target on the first paging
   *                  ack. It also made every call site spell the distinction
   *                  itself (`x !== undefined ? new LineStore(x) : new
   *                  LineStore()`), which is the shape a sentinel forces. A
   *                  HISTORY
   *                  budget floored at the live screen: the current window's
   *                  rows are never evicted, so a cap at or below the
   *                  terminal height keeps the full screen and simply
   *                  retains no scrollback. Injectable so eviction is
   *                  testable without allocating thousands of rows, and
   *                  consumer-tunable through the renderer/UI plumb.
   */
  constructor(maxLines?: number) {
    // Constructor-knob semantics (docs/paged-scrollback.md §5.3): an OMITTED
    // cap means "engine's choice", so it holds the legacy value until paging is
    // declared and then flips to the small target. A SUPPLIED cap is an
    // explicit memory decision by the consumer and holds in EVERY capability
    // state — the flip is then a no-op. Either way the tail never exceeds what
    // the consumer asked for, and never drops below what a non-paging server
    // can serve back.
    this.compatibilityCap = maxLines ?? MAX_LINES;
    this.supportedTarget = maxLines ?? RESIDENT_TAIL_CAP;
    this.effectiveTailCap = this.compatibilityCap;
  }

  /** Lines held in the resident tail (everything not classified browse). */
  private get tailCount(): number {
    return this.lines.size - this.browse.size;
  }

  /** The effective resident-tail cap, floored at the live window's height. */
  private get tailBound(): number {
    return Math.max(this.effectiveTailCap, this.win.height);
  }

  /** Highest absolute index held, or -1 if empty. */
  highestIndex(): number {
    return this.highest;
  }

  /**
   * The lowest absolute index at or above which this store's content has NOT
   * been confirmed as committed history by the server, or +Infinity when
   * everything held is confirmed.
   *
   * The distinction this tracks is real and the map cannot express it. A row
   * arriving in a SCREEN frame lands at `base + y`, which is a screen POSITION
   * the application repaints in place, so the content held at that absolute
   * index is what the app most recently drew there — provisional until the row
   * scrolls off and the server commits it. A row arriving in a SCROLL or history
   * frame is the opposite: the server has committed it, at that index, for good.
   *
   * One number rather than a per-row flag or a second key set. The rows above
   * this floor are always a contiguous suffix of the store, because a screen
   * frame's window is contiguous and lowers the floor to its base, while a
   * durable frame that reaches the floor from below raises it past the range it
   * delivered. `paged-scrollback.md` §5.3 records why a parallel structure over
   * the same map is the shape to avoid: the one that was tried, a maintained
   * companion count, was wrong at four mutation sites and reached -96.
   *
   * Movement is deliberately asymmetric. Lowering is unconditional, because a
   * screen frame is proof its window is provisional. Raising happens only when a
   * durable frame reaches the floor from at or below it, so a chunk landing
   * above a still-unconfirmed gap leaves the floor where it is. That direction
   * is the safe one to be wrong in: too low costs a few replayed rows, too high
   * is the defect this exists to prevent.
   */
  private unconfirmedFrom = Number.POSITIVE_INFINITY;

  /**
   * The resume REPLAY-EXCLUSION BOUNDARY: the highest index this client will not
   * ask the server to re-send. Send this as `haveThrough`, never `highestIndex()`.
   *
   * The two differ by whatever the store holds provisionally, and that
   * difference is a correctness bug once it reaches the wire. While a store has
   * no socket (a background tab) it observes no base advance, so its window rows
   * keep the content the app drew there, and claiming down to the window's
   * bottom row tells the server "no need to re-send those" about rows whose
   * committed content this client has never seen. The server replays strictly
   * above `haveThrough`, so the provisional copies are never corrected and
   * surface as scrollback under the new window: measured as a frozen kiro-cli
   * composer box parked above the live region after almost every tab switch.
   *
   * NOT a residency fact and NOT "the highest confirmed row": the index this
   * returns may be one the store never held or has since evicted. The only
   * guarantee is `replayBoundary() <= highestIndex()`, with equality whenever
   * nothing held is provisional.
   */
  replayBoundary(): number {
    if (this.highest < 0) {
      return this.highest;
    }
    // unconfirmedFrom is +Infinity when nothing is provisional, so the min is
    // `highest` and this is a no-op for a store fed only by durable frames.
    return Math.min(this.highest, this.unconfirmedFrom - 1);
  }

  /** Lowest absolute index held, or -1 if empty. */
  oldestIndex(): number {
    return this.oldest;
  }

  /**
   * True if history older than what the store holds was trimmed (evicted
   * client-side, or the server reported it no longer retains it). The
   * renderer shows a "history trimmed" marker at the top in this case.
   */
  hasTrimmedHistory(): boolean {
    if (this.oldest > 0 && this.everEvictedThrough >= 0) {
      return true; // we evicted the oldest lines ourselves
    }
    return this.serverOldest > 0 && this.oldest >= this.serverOldest;
  }

  /** Current live-window descriptor (cursor, base, height). */
  getWindow(): WindowState {
    return { ...this.win };
  }

  /** Whether the alternate screen is active. */
  isAlt(): boolean {
    return this.alt;
  }

  /**
   * The ephemeral alt-screen grid rows, as a read-only view of the store's
   * internal arrays (no copy: the pre-2026-07 deep copy allocated the whole
   * grid on every flush while a full-screen TUI was active — measured at real
   * cost in the render bench). Row identity is meaningful: applyScreen
   * replaces exactly the changed rows' arrays and leaves unchanged rows
   * reference-identical, which is what lets the renderer reconcile the alt
   * grid row-by-row instead of rebuilding it (renderAlt).
   */
  getAltRows(): readonly (readonly WireRun[])[] {
    return this.altRows;
  }

  /**
   * Iterate retained lines from oldest to highest in absolute-index order,
   * skipping holes. The renderer uses this to build and order DOM rows; a hole
   * (a jump in abs between consecutive lines) is a trimmed-history gap the
   * renderer marks.
   */
  forEachLine(cb: (abs: number, runs: WireRun[]) => void): void {
    if (this.oldest < 0) {
      return;
    }
    // Iterate the retained keys (bounded by maxLines), sorted ascending, not the
    // integer range [oldest, highest]: a frame whose base jumps far from a
    // retained index makes that range ~2^53 wide and freezes the tab (the DoS
    // enforceCap is written to avoid).
    const keys = [...this.lines.keys()].sort((a, b) => a - b);
    for (const abs of keys) {
      const runs = this.lines.get(abs);
      if (runs !== undefined) {
        cb(abs, runs);
      }
    }
  }

  /** Read a single retained line by absolute index, or undefined. */
  getLine(abs: number): WireRun[] | undefined {
    return this.lines.get(abs);
  }

  /**
   * Apply a decoded screen frame: update the window descriptor and cursor,
   * route to the alt grid when the alternate screen is active, and apply each
   * changed window row at its absolute index (base + y).
   */
  applyScreen(msg: ScreenMessage): void {
    // ED3 FIRST, BEFORE the screen-mode dispatch. The signal is a property of the
    // SESSION, not of the buffer the frame happens to describe. The server raises
    // a pending flag in the PTY path and attaches it to whatever frame it builds
    // next, with NO alt gate — and it force-appends a row so the flag can never be
    // dropped, which on an alt tick forces an ALT payload carrying it.
    //
    // Handling it inside the main-screen branch made every alt-active ED3 a total
    // no-op: not one of §5.5's three effects ran. `clear; vim foo` typed as one
    // line is enough (clear's terminfo emits CSI 3 J, vim's smcup follows in the
    // same PTY burst, both inside one flush interval), as is `reset` inside any
    // full-screen app. The erased transcript stayed resident and — because the
    // server's ring Clear() preserves `committed`, so post-ED3 lines keep
    // numbering from where the erased ones stopped — it spliced CONTIGUOUSLY into
    // later output with no index gap, no gap marker and no trim marker. The
    // in-flight window also stayed open, so a reply already on the wire put the
    // erased rows back through the one path exempt from guard 2.
    //
    // `msg.base` is the correct bound on either buffer: in alt it is
    // `committedBefore` (alt accrues no history, so the base stays frozen), which
    // is one past the newest committed line, so "below base" is the whole
    // scrollback either way.
    if (msg.scrollbackCleared) {
      this.applyScrollbackCleared(msg.base);
    }
    // A ROWS-LESS screen frame is a SIGNAL, not geometry, and must not redefine
    // any buffer's shape. The connection layer forwards a pre-ack ED3 exactly that
    // way (its rows are suppressed because the resume batch will repaint them),
    // and no real terminal has a zero-height screen. On the MAIN path, taking the
    // window from it set `height = 0`, which put the window bottom at `base - 1`
    // and handed truncateBelowWindow every retained row at or above `base` as
    // stranded: measured wiping a 3024-line store to 0 rows, and silently, because
    // neither that path nor the ED3 drop advances the eviction watermark that
    // would show a trim marker. On the ALT path it reached `enterAltIfNeeded(0)`
    // and collapsed the alt grid to zero rows until the next full frame. The
    // signal above is the whole payload of such a frame.
    if (msg.rows.length === 0) {
      return;
    }
    if (msg.altActive) {
      this.enterAltIfNeeded(msg.rows.length);
      for (const y of msg.changed) {
        const row = msg.rows[y];
        if (y >= 0 && y < this.altRows.length && row !== undefined) {
          this.altRows[y] = row;
          this.altDirty = true;
        }
      }
      this.updateWindowCursor(msg);
      return;
    }
    this.exitAltIfNeeded();
    this.updateWindow(msg);
    // Every index in this window is provisional: the app repaints these screen
    // positions in place, so what the store holds there is the last thing drawn,
    // not what the server will commit at that absolute index. Unconditional and
    // independent of `changed`, because an unchanged row still holds provisional
    // content from an earlier frame.
    this.markUnconfirmedFrom(msg.base);
    for (const y of msg.changed) {
      const row = msg.rows[y];
      if (row !== undefined) {
        this.applyLine(msg.base + y, row);
      }
    }
    this.truncateBelowWindow();
    // Re-check the cap AFTER the window settled: a screen frame can LOWER the
    // retention bound without applying a single line — a height shrink whose
    // rows are byte-identical short-circuits applyLine on the idempotency
    // guard, and applyLine is otherwise the only enforceCap caller. The R3
    // adversarial rounds' interleaved property test failed on exactly this
    // shape (~60% of seeds) until the bound was re-checked per FRAME, not
    // only per content change.
    this.enforceCap();
  }

  /** Apply a decoded scroll/history frame: each line at firstIndex + i. */
  applyScroll(msg: ScrollMessage): void {
    if (this.alt) {
      // History strictly below the frozen MAIN window is safe to store while
      // the alternate screen is active: the alt display renders from altRows,
      // never from the abs store, and lines below win.base can only be
      // main-screen scrollback — a resume batch's replay, or a post-resume
      // durable chunk that raced an alt flip (the 2026-08 write-ordering
      // race's last residual, closed here). They surface at alt exit's store
      // rebuild. Anything AT or ABOVE the frozen base still violates the
      // protocol invariant (the server never emits live scroll during alt)
      // and is dropped rather than corrupting the frozen window region. With
      // no main window ever seen (a fresh tab attaching straight into an
      // in-alt session) there is no region to protect, so everything is
      // accepted exactly as on the main-screen path — the alt-exit full
      // frame overwrites the window region either way. applyLine's guard
      // set applies to whatever passes.
      const base = this.win.height > 0 ? this.win.base : Number.POSITIVE_INFINITY;
      for (let i = 0; i < msg.lines.length; i++) {
        const abs = msg.firstIndex + i;
        const row = msg.lines[i];
        if (row !== undefined && abs < base) {
          this.applyLine(abs, row);
        }
      }
      // Confirm only the prefix the alt gate actually ACCEPTED. The rows at or
      // above the frozen base were dropped, so they are not committed history as
      // far as this store is concerned and must not raise the floor over
      // themselves.
      this.confirmRange(msg.firstIndex, Math.min(msg.lines.length, base - msg.firstIndex));
      return;
    }
    for (let i = 0; i < msg.lines.length; i++) {
      const row = msg.lines[i];
      if (row !== undefined) {
        this.applyLine(msg.firstIndex + i, row);
      }
    }
    this.confirmRange(msg.firstIndex, msg.lines.length);
  }

  /**
   * Lower the provisional floor to `base`: everything from there up is content
   * the application drew, not content the server committed. See
   * `unconfirmedFrom` for why the floor moves in only one direction here.
   */
  private markUnconfirmedFrom(base: number): void {
    if (!Number.isInteger(base) || base < 0) {
      return; // a malformed frame must not move the floor
    }
    if (base < this.unconfirmedFrom) {
      this.unconfirmedFrom = base;
    }
  }

  /**
   * Raise the provisional floor past a range the server just committed, and heal
   * it when the range proves the old floor is unreachable.
   *
   * The reach test is the correctness argument for the ordinary case. A chunk
   * landing above the floor leaves rows between the floor and the chunk still
   * provisional, so raising past it would claim exactly the rows this mechanism
   * exists to exclude. Under the normal dispatch order the test passes: a screen
   * frame lowers the floor to its new base and the scroll chunks carrying the rows
   * that just scrolled off start at or below that floor. When those chunks do NOT
   * arrive — the server writes each payload as its own message and ignores write
   * errors, so a socket that dies mid-dispatch is ordinary — the floor stays put
   * and the next resume asks for the range again.
   *
   * The HEAL is the second clause, and without it the floor wedges permanently.
   * Three ordinary events deliver content far above the floor and leave nothing
   * that can ever reach it: an ED3 (which drops every line below the new base,
   * taking the floor's own neighbourhood with it), a clamped replay, and the ring
   * eviction every server eventually performs. Measured before the heal existed: a
   * store whose floor sat at 99 still reported 99 after fifteen healthy frames had
   * carried `highest` to 297, and after a clamped replay took it to 5223. Every
   * attach then asked for a maximal replay forever, and `snapshot()` persisted the
   * wedge across reloads — strictly worse than the defect the floor exists to fix.
   *
   * So a range arriving entirely above `oldest` while the floor sits BELOW
   * `oldest` heals it: nothing at or above the floor is held any more, so there is
   * nothing left to protect, and holding the claim down protects only rows that no
   * longer exist. This is safe in the one direction that matters — it never raises
   * the floor over a row the store still holds provisionally, because such a row
   * would be at or above `oldest` by definition.
   *
   * `applyHistoryScroll` deliberately does not call this. A demand-paged reply
   * fetches history strictly below the resident tail; it can neither reach the
   * floor nor witness the floor being orphaned.
   */
  private confirmRange(firstIndex: number, count: number): void {
    if (!Number.isInteger(firstIndex) || firstIndex < 0 || count <= 0) {
      return;
    }
    const end = firstIndex + count;
    if (firstIndex > this.unconfirmedFrom) {
      // The heal: the floor names an index the store no longer holds, so it is
      // protecting nothing. Recorded through the same path as an ordinary raise so
      // there is one writer, not two.
      if (this.oldest >= 0 && this.unconfirmedFrom < this.oldest) {
        this.unconfirmedFrom = Math.max(end, this.oldest);
      }
      return; // otherwise a gap below stays provisional
    }
    if (end > this.unconfirmedFrom) {
      this.unconfirmedFrom = end;
    }
  }

  /**
   * Record the server's retained-history bounds from a resumeAck so the
   * renderer can tell a genuine trim from a still-loading state.
   *
   * `committed` is deliberately NOT stored. The replay-jump prediction needs
   * it, but it takes it as an argument from the ack being processed — reading a
   * stored copy would risk predicting from a previous ack's value, which is the
   * same class of defect as predicting from a mutated `highest`.
   */
  noteResumeBounds(_committed: number, oldestIndex: number): void {
    if (Number.isInteger(oldestIndex) && oldestIndex >= 0) {
      this.serverOldest = oldestIndex;
      // Within one boot epoch a ring's oldest only RISES, so a report below the
      // floor cannot be ordinary progress — it is the repair path for a floor
      // raised by a mis-correlated clamp, and lowering it back is how a
      // session that wrongly condemned its own history recovers.
      if (oldestIndex < this.pagingFloor) {
        this.pagingFloor = oldestIndex;
      }
    }
  }

  // --- demand-paged scrollback: public surface (docs/paged-scrollback.md) ---

  /** Whether the last resumeAck declared demand paging (§5.4's `supported`). */
  pagingDeclared(): boolean {
    return this.paging;
  }

  /** The oldest index the server reported retaining, or -1 if unknown. */
  serverOldestIndex(): number {
    return this.serverOldest;
  }

  /** The lowest index still worth requesting (§4.5). */
  pagingFloorIndex(): number {
    return this.pagingFloor;
  }

  /** Lines currently classified as browse cache. */
  browseCacheSize(): number {
    return this.browse.size;
  }

  /** The effective resident-tail cap (post-flip once paging is declared). */
  tailCap(): number {
    return this.effectiveTailCap;
  }

  /** When browse cache was last created or refreshed, for the consumer's TTL. */
  lastBrowseActivityMs(): number {
    return this.browseActivityMs;
  }

  /**
   * Record the window of a request about to go out, so its reply can apply
   * below the stale-re-send watermark. Exactly one slot, matching
   * single-flight — a second call replaces the first rather than accumulating.
   */
  noteSolicited(fromAbs: number, end: number): void {
    if (!Number.isSafeInteger(fromAbs) || !Number.isSafeInteger(end) || end <= fromAbs) {
      return;
    }
    this.solicitedPending = { lo: fromAbs, hi: end };
  }

  /** Release the in-flight window (reply applied, timed out, or socket gone). */
  clearSolicited(): void {
    this.solicitedPending = null;
  }

  /**
   * The retained ranges, coalesced from the KEY SET. This is the authority for
   * gap geometry — see intervals.ts for why it cannot be the browse set.
   */
  retainedRanges(): Interval[] {
    this.rangesMemo ??= retainedIntervals(this.lines.keys());
    // A COPY of the runs, not the memo itself: this is a public accessor and a
    // caller that mutated the returned list would corrupt every later answer.
    // The run count is small (one tail plus a cache run or two) where the key
    // count is thousands, which is the whole reason the memo pays.
    return this.rangesMemo.map((iv) => ({ ...iv }));
  }

  /**
   * The absent edges within `threshold` lines of `abs`, nearest first: the
   * trigger's input. Each entry is a gap the viewport is approaching, already
   * clamped to what is worth requesting — an interior hole between two retained
   * runs, or the bottom FRONTIER pseudo-gap `[pagingFloor, oldestHeld)`.
   *
   * The frontier is included only when there is something below to ask for:
   * `oldestHeld > 0`, the floor does not already cover it, and the server still
   * reports retaining something older. A gap whose high edge is at or below the
   * floor is omitted, because the floor means the server has PROVEN nothing at
   * or below it survives.
   */
  absentEdgesNear(abs: number, threshold: number): Interval[] {
    const runs = this.retainedRanges();
    if (runs.length === 0) {
      return [];
    }
    const candidates = interiorGaps(runs);
    const lowest = runs[0]?.lo ?? 0;
    if (
      lowest > 0 &&
      lowest > this.pagingFloor &&
      (this.serverOldest < 0 || lowest > this.serverOldest)
    ) {
      candidates.push({ lo: this.pagingFloor, hi: lowest });
    }
    // `gapHigh > pagingFloor`, deliberately NOT gapLow: after a ring exhaustion
    // followed by a later tail trim, the frontier's low edge EQUALS the floor,
    // and that reopened frontier must stay fetchable (§5.4).
    const live = candidates.filter((g) => g.hi > this.pagingFloor && g.hi > g.lo);
    const near = live.filter((g) => abs >= g.lo - threshold && abs <= g.hi + threshold);
    near.sort((a, b) => Math.abs(abs - a.hi) - Math.abs(abs - b.hi));
    return near;
  }

  /**
   * Apply a correlated history page. The bulk entry point for paged-in lines,
   * distinct from `applyScroll` in three ways that all matter: it classifies
   * what it stores as BROWSE cache, it suppresses per-line cap enforcement in
   * favour of one budget pass at the end, and it clips to the solicited window.
   *
   * `viewportAbs` comes from the renderer — the only layer that knows where the
   * reader is — and decides which end of the cache is safe to evict. The store
   * never guesses it.
   */
  applyHistoryScroll(msg: ScrollMessage, viewportAbs: number): void {
    // The ALT GATE first, identical to applyScroll's prologue: the fetch
    // controller is scrollback-UI-only, but a reply can still land during an
    // alt flip. History strictly below the frozen main base is safe to store
    // (it surfaces at alt exit's rebuild); at or above it would corrupt the
    // frozen window region.
    const altBase = this.alt
      ? this.win.height > 0
        ? this.win.base
        : Number.POSITIVE_INFINITY
      : Number.POSITIVE_INFINITY;

    // Only the INTERSECTION with the in-flight window gets solicited treatment.
    // A correlated frame wider than the current slot — a timed-out larger reply
    // racing a shrunken retry — has its out-of-window lines routed through the
    // ordinary guards instead, where they are typically refused below the
    // watermark (§5.1). Without the clip, a stale oversized reply would apply
    // content the client did not ask for on this attempt.
    const s = this.solicitedPending;
    if (s === null) {
      // The question was RETRACTED, so the answer is dropped whole. Every one of
      // the bulk path's concessions — admission below the stale-re-send
      // watermark, suppressed per-line cap enforcement, disposable-cache
      // classification — is paid for by a request the client currently has out.
      //
      // Downgrading it to the ordinary scroll path instead is NOT equivalent and
      // was tried first: at the store level a page reply is indistinguishable
      // from legitimate new content at the same indices (after ED3 an app
      // reprints its transcript below the new window base, and that must apply),
      // so the ordinary path admits the stale rows too. Only the CALLER knows
      // this frame was correlated against a window that no longer exists, so
      // only here can it be refused. The three cancellers are ED3, an epoch
      // reset, and a closed socket; if the range still matters the trigger asks
      // again, subject to the floor.
      return;
    }
    const applied: number[] = [];
    for (let i = 0; i < msg.lines.length; i++) {
      const abs = msg.firstIndex + i;
      const row = msg.lines[i];
      if (row === undefined || abs >= altBase) {
        continue;
      }
      if (abs < s.lo || abs >= s.hi) {
        // Outside the solicited window: ordinary guards, ordinary
        // classification (it is not this request's content).
        this.applyLine(abs, row);
        continue;
      }
      this.applyLine(abs, row, true);
      if (this.lines.has(abs)) {
        applied.push(abs);
      }
    }
    if (applied.length === 0) {
      // An empty or fully-refused reply changes no classification. The caller
      // still owns the floor raise for an empty reply (§4.3).
      this.enforceCap();
      return;
    }
    // Exactly the keys this reply put in the store join the cache. Re-fetching a
    // range already cached is a no-op per key, so overlap cannot inflate the
    // cache size the way a reply-length increment would.
    this.classifyAsBrowse(applied);
    // ONE budget pass, at the page-apply target. The transient between the
    // apply above and this pass is the reason the invariants are documented as
    // holding at OPERATION boundaries, not inside a bulk apply.
    this.enforceBrowseBudget(BROWSE_CACHE_CAP, viewportAbs);
    this.enforceCap();
  }

  /**
   * Move the given retained keys into the browse classification without
   * touching a single line. This is a RECLASSIFICATION, not a trim: nothing is
   * deleted, no DOM changes, and `everEvictedThrough` is untouched — which is
   * what lets the cap flip and the replay jump hand a band to the cache budget
   * instead of evicting it out from under the reader.
   *
   * It takes KEYS, not a range, and that is the point: every caller already
   * knows exactly which indices it is reclassifying (the page's applied list,
   * the flip's excess tail keys, the jump's keys below the send watermark), so
   * no span loop exists anywhere in the cache model. The range-taking version
   * this replaced had to walk `[lo, hi)` to find the retained members, and its
   * band is routinely a hull tens of millions of indices wide.
   *
   * Unheld keys are skipped, so the subset invariant survives a caller passing
   * an index the store does not have.
   */
  private classifyAsBrowse(keys: Iterable<number>): number {
    let added = 0;
    for (const abs of keys) {
      if (this.lines.has(abs) && !this.browse.has(abs)) {
        this.browse.add(abs);
        added++;
      }
    }
    if (added > 0) {
      this.browseActivityMs = Date.now();
    }
    return added;
  }

  /**
   * Evict browse cache down to `target`, far edge first.
   *
   * Removal is EXACTLY the overflow, capped at one page per step: a one-line
   * overflow removes one line, not a page, because anything larger thrashes
   * fetch/evict cycles at the cap. Victims are taken from whichever END of the
   * cache is FARTHER from `viewportAbs`, and every line within
   * `PREFETCH_THRESHOLD` of it is EXEMPT — an eviction must never create a hole
   * the trigger immediately re-fetches, nor blank the rows under the reader.
   *
   * Direction is computed from the cache's own extremes, never from a range's
   * edges. The version this replaced asked whether an interval began at or above
   * the viewport to find its far side, which is false whenever the reader is
   * INSIDE the cache — the normal deep-scroll shape — so it always took the low
   * end: the direction an up-scrolling reader is heading, while the pages behind
   * them were never freed.
   *
   * Termination: the exemption band is one contiguous range, so if any victim is
   * available at the far end the pass makes progress, and every removal shrinks
   * `browse`. When the reader's own neighbourhood is all that remains the pass
   * stops and accepts the bounded overshoot rather than spinning — that stop
   * rule is target-agnostic, which is what makes a smaller reclassify target
   * safe too.
   */
  private enforceBrowseBudget(target: number, viewportAbs: number): void {
    // §5.3's viewport exemption, and it is REACHABLE — an earlier comment here
    // claimed arithmetic made it dead, which was measurably wrong.
    //
    // The shape that reaches it: the resume-ack pass uses the small
    // PREFETCH_THRESHOLD target, and the band is 2 * PREFETCH_THRESHOLD + 1 wide,
    // so a cache SPANNING the reader but smaller than the band is entirely
    // exempt while still exceeding the target. Two retained runs either side of
    // the reader's position do it: 400 rows at [0,400), 400 at [600,1000), the
    // reader in the hole at 500, and a jump ack classifies all 800 against a
    // target of 500. Nothing is evictable, the pass stops, and the store keeps
    // 800 — the bounded overshoot the design prefers to blanking the rows under
    // the reader. Under BROWSE_CACHE_CAP the same arithmetic never bites, which
    // is what the dead-code claim was really about.
    const exemptLo = viewportAbs - PREFETCH_THRESHOLD;
    const exemptHi = viewportAbs + PREFETCH_THRESHOLD + 1;
    const exempt = (abs: number) => abs >= exemptLo && abs < exemptHi;

    while (this.browse.size > target) {
      // The victim pool IS the browse set: bounded by the cache, not by
      // residency and not by any numeric span. `size` is the count the loop
      // tests, so the gate and the pool cannot disagree.
      const keys = [...this.browse].sort((a, b) => a - b);
      const budget = Math.min(this.browse.size - target, PAGE_SIZE);
      // Distance from the reader decides which end goes; on a tie the older
      // (lower) end goes, matching the tail cap's oldest-first bias. `keys` is
      // non-empty because `size > target >= 0`.
      const lowest = keys[0] ?? 0;
      const highest = keys[keys.length - 1] ?? 0;
      const fromTop = Math.abs(highest - viewportAbs) > Math.abs(viewportAbs - lowest);
      const victims: number[] = [];
      const walk = fromTop ? keys.reverse() : keys;
      for (const abs of walk) {
        if (victims.length >= budget || exempt(abs)) {
          break; // the exemption is contiguous, so the rest of this end is too
        }
        victims.push(abs);
      }
      if (victims.length === 0) {
        break; // the reader's own neighbourhood is all that is left: overshoot
      }
      for (const abs of victims) {
        // forget() drops the line AND its classification. A cache drop is NOT a
        // permanent trim, so everEvictedThrough deliberately stays where it is
        // and the same index can be re-fetched later.
        this.forget(abs);
      }
      this.recomputeBounds();
    }
  }

  /**
   * The resume-ack transition: ONE store operation carrying every decision the
   * ack implies, in a fixed order (docs/paged-scrollback.md §4.5).
   *
   * It is one call rather than four because the order is load-bearing and the
   * inputs live in three different layers — the connection decodes the ack, the
   * store owns residency, the renderer owns the viewport. Split across
   * callbacks, an implementer can interleave them; here they cannot.
   *
   * `committed`/`serverOldest` are NULLABLE AS A PAIR: a server too old to
   * carry the ack's length-gated bounds tail sends neither, and inventing zeros
   * would lower the floor and forge a jump. Such an ack still runs the epoch
   * reset and the capability read, and skips the two steps that have no inputs.
   */
  applyResumeAck(ack: {
    /** The boot epoch changed: everything retained is from another server. */
    epochChanged: boolean;
    /** One past the server's newest committed line, or null on an old ack. */
    committed: number | null;
    /** The server's oldest retained index, or null on an old ack. */
    serverOldest: number | null;
    /** The server DECLARED demand paging (ackFlags bit1). */
    paging: boolean;
    /** The `haveThrough` this socket SENT, which the reply is a function of. */
    sentHaveThrough: number;
    /** The `replayMax` this socket SENT (already clamped), or null. */
    sentReplayMax: number | null;
    /** Where the reader is, from the renderer. */
    viewportAbs: number;
    /**
     * Whether the reader is at the live tail, from the renderer — which KNOWS,
     * where the store could only ever guess.
     *
     * The store used to derive it from `viewportAbs >= win.base`, and every
     * failure of that derivation was a real defect. It read a window descriptor
     * that step 4 of this very transition RETIRES, so the answer was wrong for
     * every predicted replay jump (r2's confirmed high, patched by capturing the
     * value earlier — a fix that left the hazard in place). With no descriptor at
     * all, which is the DOMINANT shape here (this ack is the first frame of its
     * batch, so it precedes the window frame, and a hydrated store persists no
     * window), the fallback was wrong in both directions: "not following" strands
     * ~2000 lines over budget on the first attach over a snapshot, and
     * "following" would drain a reader who is demonstrably deep in history.
     *
     * The renderer has one unambiguous source (`scroll.isUserScrolledUp()`, plus
     * an armed restore, which means "in history at the anchor"), so it answers and
     * the store stops inferring. Retiring the window descriptor mid-transition can
     * no longer corrupt anything.
     */
    following: boolean;
  }): void {
    // (1) An epoch change resets everything, cap included: a different server
    // process shares no absolute indices with the old one.
    if (ack.epochChanged) {
      this.reset();
    }
    // (2) The capability, recorded for the renderer before anything reads it, and
    // stated in BOTH directions: an ack is the only thing that knows, so an
    // unsupported pairing must actively restore the compatibility cap rather than
    // relying on some earlier reset to have cleared it. (When paging IS declared,
    // step 3's flip owns the cap.)
    this.paging = ack.paging;
    if (!ack.paging) {
      this.effectiveTailCap = this.compatibilityCap;
    }
    // (2b) Bounds, when the ack carries them.
    if (ack.committed !== null && ack.serverOldest !== null) {
      this.noteResumeBounds(ack.committed, ack.serverOldest);
    }
    // (3) Capability: the cap flip, bookkeeping only.
    let reclassified = false;
    if (ack.paging) {
      reclassified = this.confirmPaging();
    }
    // (4) The replay JUMP, predicted from what this socket SENT — never from
    // the store's current `highest`, which a frame arriving between send and
    // ack could have moved, masking the jump and leaving the stranded band for
    // enforceCap to eat. Gated on `paging`: under an unsupported pairing the
    // band stays TAIL, which is today's behavior and bounded by the
    // compatibility cap. Reclassifying it there would hand disposable-cache
    // semantics to history no fetch can restore.
    if (ack.paging && ack.committed !== null && ack.serverOldest !== null) {
      const jumped = this.predictReplayJump(
        ack.committed,
        ack.serverOldest,
        ack.sentHaveThrough,
        ack.sentReplayMax,
      );
      reclassified = reclassified || jumped;
    }
    // (5) ONE budget pass, if either step moved anything into the cache. The two
    // steps used to be compared as bands to pick the wider one; with membership
    // as a key set there is nothing to compare — the containment test below
    // reads the set itself and has one answer regardless of which step grew it.
    if (reclassified) {
      // CONTAINMENT: a FOLLOWING viewport is outside every reclassified band by
      // definition — it is looking at the live tail, not at cache — so the
      // disposable band drains to the small target. A reader in HISTORY keeps the
      // full cache instead, and the TTL cleans up later.
      //
      // Following is the WHOLE question, and the store asked a second one twice,
      // in two different structures. It tested whether the row UNDER the reader
      // was cache — a proxy that fails in exactly the shape
      // paging exists for: a reader whose position is an index the store does not
      // currently hold. Both doors to that shape are ordinary — a hole inside the
      // cache, and an ARMED RESTORE anchor (§7.2/§7.3 of
      // scroll-position-fidelity.md — the renderer deliberately passes the position
      // the reader is REGAINING, whose row may since have been dropped). Either
      // way the reader is unambiguously reading history and the proxy answered
      // "no", draining their depth to the small target: measured 801 rows kept
      // instead of 2420. The range version got that case right by accident (a hull
      // spans holes) and the key version got it wrong by accident; asking only
      // what the design means removes both.
      this.enforceBrowseBudget(
        ack.following ? PREFETCH_THRESHOLD : BROWSE_CACHE_CAP,
        ack.viewportAbs,
      );
      this.enforceCap();
    }
  }

  /**
   * The cap flip: set the effective tail cap to the supported target and
   * reclassify the excess tail as browse cache. Returns whether anything moved.
   *
   * The band keeps the newest `max(supportedTarget, windowHeight)` tail lines
   * and NEVER crosses `win.base` — a LIVE window row is never reclassified,
   * because the browse budget may evict it and the window must not be
   * evictable. (§5.2's replay-jump band deliberately DOES include the old
   * window rows, but only because it retires the descriptor first.)
   */
  private confirmPaging(): boolean {
    this.effectiveTailCap = this.supportedTarget;
    const keep = Math.max(this.supportedTarget, this.win.height);
    if (this.tailCount <= keep || this.oldest < 0) {
      return false;
    }
    // Walk the tail keys oldest-first and take all but the newest `keep`.
    const tailKeys = [...this.lines.keys()]
      .filter((k) => !this.browse.has(k))
      .sort((a, b) => a - b);
    const excess = tailKeys.length - keep;
    if (excess <= 0) {
      return false;
    }
    const winFloor = this.win.height > 0 ? this.win.base : Number.POSITIVE_INFINITY;
    return this.classifyAsBrowse(tailKeys.slice(0, excess).filter((k) => k < winFloor)) > 0;
  }

  /**
   * Predict where the incoming replay will START, and if it lands above what
   * this socket told the server it had, reclassify the stranded band as browse
   * cache before any frame of the batch applies.
   *
   * Two causes produce the same shape and both are covered: the new bounded
   * replay's clamp, and the plain eviction gap EVERY server produces when its
   * ring has moved past the client's `haveThrough`. Scoping detection to the
   * clamp alone would miss the shape that already ships.
   *
   * The band deliberately includes the OLD window rows and RETIRES the window
   * descriptor, because the batch's own window frame re-establishes it at the
   * new base — those rows stop being window rows the moment it lands.
   *
   * The band top is `sentHaveThrough` and deliberately does NOT follow the wire
   * value down now that `replayBoundary()` supplies it. The two questions are
   * different: the wire value asks "what will I not ask for", the band asks "what
   * did I claim to hold before the server answered". Rows above the sent value
   * arrived from the server as committed content, so they are real history and
   * must not be reclassified as disposable cache; rows this store holds only
   * provisionally sit at or above the boundary and are covered by the replay,
   * which now starts at `boundary + 1` precisely because of it.
   *
   * One residual, deliberately not addressed here. When the replay start is
   * CLAMPED above the boundary (`committed - replayMax`, reachable when a
   * background tab printed more than roughly `tailCap` lines), the provisional
   * rows are neither replayed nor banded, so they stay tail-classified and lose
   * the browse budget's TTL sweep. Widening the band to cover them would change a
   * pinned invariant that two existing cases assert, so it is recorded in
   * docs/resume-watermark.md rather than changed in passing.
   *
   * The retirement leaves the same transition's later steps reading a window of
   * height 0, so state precisely what that costs rather than forbidding it: the
   * only window-derived bound they touch is `enforceCap`'s, which loses the live
   * screen's eviction protection for that one pass. It is harmless HERE and not
   * by luck — after the jump the tail holds only the keys above
   * `sentHaveThrough`, so `tailCount` is far below the bound and the pass returns
   * at its first gate; and were it to run, the walk is oldest-first while the
   * screen rows are the newest. A future step that evaluates a window bound with
   * consequences must re-establish the descriptor first.
   */
  private predictReplayJump(
    committed: number,
    serverOldest: number,
    sentHaveThrough: number,
    sentReplayMax: number | null,
  ): boolean {
    const base = sentHaveThrough + 1;
    let predicted = Math.max(base, serverOldest);
    if (sentReplayMax !== null) {
      predicted = Math.max(predicted, committed - sentReplayMax);
    }
    if (predicted <= base || this.oldest < 0 || sentHaveThrough < this.oldest) {
      // No jump, or nothing this socket claimed is still held, so the stranded
      // band is empty and there is nothing to reclassify.
      //
      // The third clause used to mean only "an empty or fresh store", because the
      // wire value was the highest held index and so never sat below `oldest`.
      // The replay boundary can: a post-ED3 store has `oldest === win.base` and
      // claims `win.base - 1`. That is still the right answer here, for the
      // reason above rather than the original one — the band is `keys <=
      // sentHaveThrough`, which is empty exactly when this fires, so both the
      // reclassify and the budget pass it gates would be no-ops. The descriptor
      // is then not retired either, which is equally harmless: retirement exists
      // so a band containing old window rows survives the batch's own window
      // frame, and there is no band.
      return false;
    }
    // The stranded band is every retained key at or below what this socket told
    // the server it had. Taken from the KEY SET, so a client reconnecting far
    // behind costs its residency, not the width of the range it is missing.
    const stranded: number[] = [];
    for (const abs of this.lines.keys()) {
      if (abs <= sentHaveThrough) {
        stranded.push(abs);
      }
    }
    this.classifyAsBrowse(stranded);
    this.win = emptyWindow();
    this.windowDirty = true;
    return true;
  }

  /**
   * Raise the paging floor: the server proved nothing at or below `end`
   * survives, so no further request below it can succeed. Called for a clamped
   * reply (`firstIndex > fromAbs`) and for an empty one.
   */
  raisePagingFloor(end: number): void {
    if (Number.isSafeInteger(end) && end > this.pagingFloor) {
      this.pagingFloor = end;
    }
  }

  /**
   * Drop the browse cache. The consumer owns the TTL that calls this (the
   * engine has no notion of tabs or visibility) and supplies both inputs.
   *
   * A VISIBLE page whose reader sits on browse rows SKIPS the drop: the TTL is
   * an inactivity signal, and a reader parked on a long stack trace is inactive
   * while looking straight at cache content — wiping it and re-fetching one RTT
   * later serves nobody. A HIDDEN page has no reader, so it drops
   * unconditionally; without that condition the skip would retain exactly the
   * deep-scrolled cache the hidden-page TTL exists to free.
   */
  dropBrowseCache(viewportAbs: number, pageVisible: boolean): void {
    if (this.browse.size === 0) {
      return;
    }
    if (pageVisible && this.browse.has(viewportAbs)) {
      return; // skip and re-arm; the consumer's timer retries
    }
    for (const abs of [...this.browse]) {
      this.forget(abs);
    }
    this.recomputeBounds();
  }

  /**
   * Remove one retained line and its browse classification. The single owner of
   * every deletion in this store.
   *
   * It exists because classification lives in a second structure keyed by the
   * same indices, and a second structure is only as correct as its
   * least-maintained mutation path. Two paths did not maintain it — ED3's
   * `applyScrollbackCleared` and the resize `truncateBelowWindow`, the latter firing
   * on every soft-keyboard open — so deleting a browse-classified line left the
   * cache accounting too high, `tailCount` understated, and the tail cap
   * under-enforcing for the rest of the epoch. Funnelling every removal through
   * here removes the CLASS rather than those two instances, and is what makes
   * `browse ⊆ lines.keys()` a fact rather than a hope.
   *
   * Returns whether a line was actually held (mirroring Map.delete), so callers
   * can count real removals.
   */
  private forget(abs: number): boolean {
    const held = this.lines.delete(abs);
    if (held) {
      this.rangesMemo = null;
      this.evicted.add(abs);
      this.dirty.delete(abs);
    }
    this.browse.delete(abs);
    return held;
  }

  /** Recompute oldest/highest after a bulk removal. Bounded by map size. */
  private recomputeBounds(): void {
    let min = -1;
    let max = -1;
    for (const k of this.lines.keys()) {
      if (min < 0 || k < min) {
        min = k;
      }
      if (k > max) {
        max = k;
      }
    }
    this.oldest = min;
    this.highest = max;
  }

  /**
   * Full reset: drop all lines and window state. Used on server restart (a new
   * boot epoch), where absolute indices start over from 0 and any retained
   * content is stale. The renderer wipes all DOM on the next drain.
   */
  reset(): void {
    this.lines.clear();
    this.rangesMemo = null;
    this.oldest = -1;
    this.highest = -1;
    this.everEvictedThrough = -1;
    this.erasedThrough = -1;
    this.unconfirmedFrom = Number.POSITIVE_INFINITY;
    this.serverOldest = -1;
    this.win = emptyWindow();
    this.alt = false;
    this.altRows = [];
    this.dirty.clear();
    this.evicted.clear();
    this.windowDirty = true;
    this.altDirty = true;
    this.resetPending = true;
    // CONTENT-derived paging state goes with the content: a new boot epoch shares
    // no absolute indices with the old one, so the cache, the in-flight window and
    // the floor are all meaningless at once (docs/paged-scrollback.md §5.5).
    this.browse.clear();
    this.solicitedPending = null;
    this.pagingFloor = 0;
    this.browseActivityMs = 0;
    // CAPABILITY is deliberately NOT reset here. It describes the SERVER on the
    // other end of the socket, not the content, and only a resumeAck can restate
    // it — so a reset that cleared it left the store waiting for an event that
    // may never come. That was safe for the documented caller (applyResumeAck's
    // epoch step, whose very next step re-reads the ack), and wrong for the two
    // public ones that predate paging: `render.resetScreen()` and
    // `render.resetScrollback()`, which a consumer is explicitly invited to call
    // on alt-screen entry, nowhere near a resume. Until the next reconnect that
    // left `pagingDeclared()` false while the transport was still perfectly able
    // to page, so §5.4's top marker asserted "earlier output trimmed" about
    // history the trigger was about to fetch, and the tail cap reverted to the
    // compatibility value — discarding the residency reduction that is the whole
    // point of the feature on the device it exists for. `applyResumeAck` now
    // states the capability explicitly instead of relying on a reset to clear it.
  }

  /**
   * Serialize the newest retained lines as plain, structuredClone-safe data, for
   * a consumer that persists scrollback across a page discard (see
   * StoreSnapshot for what is deliberately excluded and why).
   *
   * `serverEpoch` is the boot epoch the caller last saw in a resumeAck; pass 0
   * if none is known. It is required rather than optional because a snapshot
   * without it cannot be hydrated safely, and an optional parameter is a
   * corruption bug waiting for the one caller who omits it.
   *
   * `maxLines` bounds the tail, defaulting to the store's own cap. A smaller
   * bound is the normal choice: the screen plus recent history is what a
   * returning user needs, and the cost of persisting is a repeated serialize on
   * a device that may already be under memory pressure. The depth that is not
   * kept is reported honestly after hydration, via the trim marker.
   *
   * Returns null when there is nothing worth persisting (an empty store), so a
   * caller cannot accidentally store an empty snapshot over a good one.
   */
  snapshot(serverEpoch: number, maxLines: number = this.effectiveTailCap): StoreSnapshot | null {
    if (this.oldest < 0 || this.highest < 0) {
      return null;
    }
    const cap = Number.isInteger(maxLines) && maxLines > 0 ? maxLines : this.effectiveTailCap;
    // Persist the LIVE TAIL only, excluding browse cache BY CLASSIFICATION
    // rather than by contiguity (docs/paged-scrollback.md §5.2). Walk down from
    // `highest` and stop at the first index that is absent OR a browse member:
    // a fetched page can sit FLUSH against the tail with no numeric gap, so a
    // contiguity test alone would serialize it. Excluding it is deliberate —
    // the cache is disposable by construction (recovery is one page fetch), and
    // persisting it would spend storage writes on data the design defines as
    // throwaway and hydrate interior holes into a fresh store.
    //
    // The result is that a hydrated store is always ONE contiguous tail, which
    // is what keeps its derived `everEvictedThrough = oldest - 1` honest.
    const tail: number[] = [];
    for (let abs = this.highest; abs >= 0 && tail.length < cap; abs--) {
      if (!this.lines.has(abs) || this.browse.has(abs)) {
        break;
      }
      tail.push(abs);
    }
    tail.reverse();
    const lines: [number, WireRun[]][] = [];
    let first = -1;
    let last = -1;
    for (const abs of tail) {
      const runs = this.lines.get(abs);
      if (runs !== undefined) {
        lines.push([abs, runs]);
        if (first < 0) {
          first = abs;
        }
        last = abs;
      }
    }
    if (lines.length === 0) {
      return null;
    }
    return {
      v: SNAPSHOT_VERSION,
      serverEpoch: Number.isFinite(serverEpoch) ? serverEpoch : 0,
      // Derived from what is actually being written, not from this.oldest: a
      // bounded tail starts above it.
      oldest: first,
      highest: last,
      lines,
      // OMITTED when nothing held is provisional, which is also what an older
      // snapshot looks like, so absent and "all confirmed" are deliberately the
      // same state on the way back in. JSON has no Infinity, so a sentinel value
      // would have to be invented and then defended; absence needs neither.
      ...(Number.isFinite(this.unconfirmedFrom) ? { unconfirmedFrom: this.unconfirmedFrom } : {}),
    };
  }

  /**
   * Rehydrate a store from a snapshot, or return null when the snapshot is
   * missing, malformed, or of a different shape version.
   *
   * Returning null rather than throwing (or returning a half-built store) is the
   * point: every failure mode here — a truncated write, a hand-edited value, a
   * snapshot from an older release — has the same correct handling, which is to
   * start empty and take a full resume.
   *
   * The hydrated store reports the depth it does NOT have.
   * `everEvictedThrough` is set to `oldest - 1`, which is what the eviction
   * bookkeeping would say had this client trimmed those lines itself: the
   * staleness guard then refuses a late re-delivery of a line below the tail
   * (it is not wanted; the tail is deliberate), and `hasTrimmedHistory()`
   * reports true so the renderer shows its "earlier output trimmed" affordance
   * instead of implying the history is complete.
   *
   * `maxLines` is the hydrated store's retained-line cap, and it also BOUNDS the
   * restore: a snapshot larger than the cap is trimmed to its newest `maxLines`
   * lines, because the cap is a memory budget (the renderer builds one DOM row
   * per retained line) and a restore that ignored it would blow the budget until
   * eviction caught up. The trimmed depth is reported through the usual channel:
   * `hasTrimmedHistory()`.
   *
   * The caller MUST pass `snap.serverEpoch` to the connection layer before
   * connecting; see StoreSnapshot.serverEpoch for what goes wrong otherwise.
   */
  static fromSnapshot(snap: unknown, maxLines?: number): LineStore | null {
    if (typeof snap !== "object" || snap === null) {
      return null;
    }
    const rec = snap as Record<string, unknown>;
    // Read the payload as unknown[] rather than through StoreSnapshot's typed
    // field: this data has been outside the program's memory, so its shape is an
    // assumption until checked, and typing it first would make the checks below
    // statically dead (the compiler would "know" each entry is a valid pair).
    if (rec["v"] !== SNAPSHOT_VERSION || !Array.isArray(rec["lines"])) {
      return null;
    }
    const raw = rec["lines"] as unknown[];
    if (raw.length === 0) {
      return null;
    }
    // Mirrors the constructor: an absent or invalid cap means "engine's choice"
    // and is FORWARDED as absent, not as a value. Resolved BEFORE the store is
    // constructed, because it has to be the store's own cap as well as the trim
    // bound — passing the raw value here gave `fromSnapshot(snap, 0)` a store
    // that trimmed at 5000 and then evicted almost everything on the next append.
    const cap =
      maxLines !== undefined && Number.isInteger(maxLines) && maxLines > 0 ? maxLines : undefined;
    const bound = cap ?? MAX_LINES;
    const store = new LineStore(cap);
    const pairs: [number, WireRun[]][] = [];
    let highest = -1;
    for (const entry of raw) {
      // One bad row must not poison the whole restore, and a partially applied
      // one is worse than none: bail out entirely and take a full resume.
      if (!Array.isArray(entry) || entry.length !== 2) {
        return null;
      }
      const abs: unknown = entry[0];
      const runs: unknown = entry[1];
      if (typeof abs !== "number" || !Number.isInteger(abs) || abs < 0) {
        return null;
      }
      if (!isWireRunArray(runs)) {
        return null;
      }
      if (abs <= highest) {
        return null; // not ascending, or duplicated: the index space is broken
      }
      pairs.push([abs, runs]);
      highest = abs;
    }
    // Bound the restore to the store's own cap. A snapshot may legitimately be
    // larger than the cap this client runs with (the same snapshot outlives a
    // consumer lowering its budget, and the persisted bound is a separate knob),
    // and hydrating over the cap would blow the memory budget the cap exists to
    // set — the renderer builds one DOM row per retained line. The whole payload
    // is validated first, so a corrupt head still rejects the restore rather
    // than being silently sliced off.
    const kept = pairs.length > bound ? pairs.slice(pairs.length - bound) : pairs;
    const oldest = kept[0]?.[0] ?? -1;
    if (oldest < 0) {
      return null;
    }
    for (const [abs, runs] of kept) {
      store.lines.set(abs, runs);
    }
    store.oldest = oldest;
    store.highest = highest;
    // Everything below the tail is gone as far as this store is concerned.
    store.everEvictedThrough = oldest - 1;
    // Restore the saving store's provisional floor. A malformed or absent value
    // leaves it at +Infinity, which is the pre-existing behaviour: the whole
    // hydrated tail reads as confirmed and the first resume claims all of it.
    // Read off the unknown record like every other field, and clamped into the
    // kept range because the tail may have been trimmed above the saved floor.
    const savedFloor: unknown = rec["unconfirmedFrom"];
    if (typeof savedFloor === "number" && Number.isInteger(savedFloor) && savedFloor >= 0) {
      store.unconfirmedFrom = savedFloor;
    }
    // The renderer must build every hydrated row: nothing is on screen yet.
    for (const abs of store.lines.keys()) {
      store.dirty.add(abs);
    }
    return store;
  }

  /** Drain accumulated changes for the renderer and clear the tracking sets. */
  drainChanges(): StoreChanges {
    const out: StoreChanges = {
      dirtyLines: [...this.dirty],
      evictedLines: [...this.evicted],
      windowChanged: this.windowDirty,
      altChanged: this.altDirty,
      fullReset: this.resetPending,
    };
    this.dirty.clear();
    this.evicted.clear();
    this.windowDirty = false;
    this.altDirty = false;
    this.resetPending = false;
    return out;
  }

  // --- internals ---

  /** Whether an absolute index falls inside the current live window. */
  private inWindow(abs: number): boolean {
    return this.win.height > 0 && abs >= this.win.base && abs < this.win.base + this.win.height;
  }

  /**
   * applyLine is the guarded core. It enforces the apply-line guard set
   * valid index, not stale, idempotent, and
   * cap-bounded. Returns nothing; effects are recorded in the dirty/evicted
   * sets for the next drain.
   */
  private applyLine(abs: number, runs: WireRun[], bulk = false): void {
    // Guard 1: a valid, non-negative integer index.
    if (!Number.isInteger(abs) || abs < 0) {
      return;
    }
    // Guard 2: not below what we have permanently evicted (stale re-send) —
    // EXCEPT inside the current live window, and EXCEPT inside the window of a
    // request we currently have in flight. The window is the terminal's
    // writable region and is never stale: under the real protocol its base
    // only advances, so a window index at or below everEvictedThrough is
    // unreachable — but a malformed or hostile frame whose base RETREATS
    // must degrade to a drawn screen, not to window rows silently dropped
    // forever (the property test drives exactly that shape). Same doctrine
    // as enforceCap's "the live window is never evictable".
    //
    // The SOLICITED exception is what makes demand paging possible at all:
    // a page is by definition a re-fetch of history below the watermark, so
    // the watermark alone cannot decide staleness any more. What keeps it
    // safe is that the exception is scoped to one in-flight request window —
    // an UNSOLICITED line below the watermark is still refused, exactly as
    // before (docs/paged-scrollback.md §5.1).
    if (abs <= this.everEvictedThrough && !this.inWindow(abs) && !this.isSolicited(abs)) {
      return;
    }
    // Guard 2b: not an index the APPLICATION erased. Same two exemptions, and
    // `erasedThrough` is why it is separable from guard 2 above — see its
    // declaration for why ED3 cannot advance the trim watermark, and why
    // refusing an erased index can never refuse legitimate content.
    if (abs <= this.erasedThrough && !this.inWindow(abs) && !this.isSolicited(abs)) {
      return;
    }
    // Guard 3: a well-formed run array.
    if (!Array.isArray(runs)) {
      return;
    }
    // Guard 5: idempotent — identical content is a no-op (no DOM churn, no
    // selection disturbance). Guards 4/6/8/9 (gap, alt-consistency, cell
    // width, row-element integrity) live at the callers and the renderer.
    const existing = this.lines.get(abs);
    if (existing !== undefined && rowEqual(existing, runs)) {
      return;
    }
    this.lines.set(abs, runs);
    this.rangesMemo = null;
    this.evicted.delete(abs);
    this.dirty.add(abs);
    if (this.oldest < 0 || abs < this.oldest) {
      this.oldest = abs;
    }
    if (abs > this.highest) {
      this.highest = abs;
    }
    // Guard 10: enforce the cap by evicting from the oldest end. SUPPRESSED
    // during a bulk page apply, which runs one budget pass of its own at the
    // end: per-line enforcement there would trim the tail against a count the
    // apply is still in the middle of changing.
    if (!bulk) {
      this.enforceCap();
    }
  }

  /** Whether `abs` lies inside the request currently in flight. */
  private isSolicited(abs: number): boolean {
    const s = this.solicitedPending;
    return s !== null && abs >= s.lo && abs < s.hi;
  }

  // applyScrollbackCleared handles ED3 (erase scrollback) in full: the app told
  // the terminal to discard its saved lines, and an inline TUI (kiro-cli) does it
  // on every resize redraw. Window rows (>= base) are kept and refreshed by the
  // frame carrying the signal. everEvictedThrough is left untouched: the app
  // discarded the lines deliberately (not a cap trim), so no "earlier output
  // trimmed" marker fits.
  //
  // Dropping the lines is only ONE of the three effects §5.5 requires, and the
  // other two are what stop the erased history coming back:
  //
  //  - The in-flight request window is CANCELLED. It is the store's standing
  //    permission to admit lines below its stale-re-send watermark, so a reply
  //    already in flight when the app erased its scrollback would otherwise
  //    re-apply the very rows the app just discarded — through the one path that
  //    is exempt from the staleness guard.
  //  - The paging FLOOR snaps to the cleared bound: nothing at or below it is
  //    worth requesting any more. Without it the trigger re-fetches the erased
  //    range from a server that still holds it, and the user watches erased
  //    output scroll back into view.
  //
  // Both run even when this client holds no line below `base`: an in-flight
  // request and a stale floor are not conditional on local residency.
  private applyScrollbackCleared(base: number): void {
    this.solicitedPending = null;
    this.raisePagingFloor(base);
    // The provisional floor moves up with the erase. Everything below `base` is
    // about to be dropped, so a floor down there would name rows the store no
    // longer holds and hold the resume claim below them forever: measured as a
    // floor stuck at 99 while `highest` climbed to 297 over fifteen healthy
    // frames, which turned every attach into a maximal replay. Never LOWERS the
    // floor, so a floor already above the erase bound keeps protecting its rows.
    if (base > this.unconfirmedFrom) {
      this.unconfirmedFrom = base;
    }
    if (this.oldest < 0 || this.oldest >= base) {
      return;
    }
    // Bounded key-scan (like enforceCap), not an integer walk over [oldest, base).
    // Through forget() so a browse-classified line takes its classification with
    // it: an application clearing its scrollback deletes cache as readily as tail.
    for (const abs of [...this.lines.keys()]) {
      if (abs < base) {
        this.forget(abs);
        // The watermark tracks what was actually DROPPED, never the cleared
        // bound: the gap between them is where the app's own reprint lands.
        this.erasedThrough = Math.max(this.erasedThrough, abs);
      }
    }
    this.recomputeBounds();
  }

  // truncateBelowWindow evicts every retained line past the window's bottom row.
  // The window's bottom row is the most recent line in the terminal, so no line
  // can exist at a higher absolute index. A resize that SHRINKS the screen (the
  // iOS soft keyboard opening) moves the window bottom up while the taller
  // screen's former bottom rows stay in the store at higher indices — stranded
  // below the live window. Cap eviction only trims the top/oldest, so nothing
  // removes them: they linger as phantom blank rows beneath the real content, an
  // "empty" region the user can scroll into and the reason the content never
  // appears to shrink to fit on a short viewport. Evicting them keeps the tail
  // invariant (highest === window bottom); it is the tail-side complement to the
  // top-side cap eviction.
  private truncateBelowWindow(): void {
    if (this.highest < 0) {
      return;
    }
    const windowBottom = this.win.base + this.win.height - 1;
    if (this.highest <= windowBottom) {
      return;
    }
    // Bounded key-scan (like enforceCap), NOT an integer walk over
    // [windowBottom+1, highest]: a frame whose base drops far below a high
    // retained index would otherwise loop up to ~2^53 times and freeze the tab.
    let newHighest = -1;
    for (const abs of [...this.lines.keys()]) {
      if (abs > windowBottom) {
        this.forget(abs);
      } else if (abs > newHighest) {
        newHighest = abs;
      }
    }
    if (newHighest >= 0) {
      this.highest = newHighest;
    } else {
      this.highest = -1;
      this.oldest = -1;
    }
  }

  private enforceCap(): void {
    // AMENDMENT 1 (docs/paged-scrollback.md §5.3): the gate and the victim
    // target read `tailCount`, not `lines.size`. Browse cache lives under its
    // OWN budget, enforced at page-apply time, so live output must not be able
    // to evict a page the reader is browsing — at the default batch that would
    // be up to 93 cache lines per applied line.
    if (this.tailCount <= this.tailBound) {
      return;
    }
    const target = this.tailBound - evictionBatch(this.tailBound) + 1;
    // ONE oldest-first eviction walk that SKIPS the live window. The window
    // is never evictable — the screen is documented as a fixed,
    // always-present block, so the cap is a HISTORY budget floored at the
    // live screen (a cap at or below the terminal height keeps the full
    // screen with zero scrollback). Content ABOVE the window (a resume
    // replay delivered before its window frame; a malformed stream that
    // never advances the base) is ordinary evictable history too: the
    // cursor hops over the window and keeps walking, so the bound
    // size <= max(maxLines, window height) holds whatever shape the frames
    // arrive in. Evicting that band OLDEST-first keeps the NEWEST lines —
    // the ones adjacent to the incoming window — so after the window
    // advances the retained set converges back to one contiguous block
    // (R3 adversarial finding: the earlier newest-first tail trim parked a
    // permanent interior hole under the live screen that resume, which
    // replays only above haveThrough, could never backfill).
    // everEvictedThrough advances over every TAIL eviction, including the
    // hopped band — safe for the window frame that follows a replay because
    // apply-line guard 2 exempts the live window (see applyLine).
    const winBottom = this.win.height > 0 ? this.win.base + this.win.height - 1 : -1;
    const hasWindow = this.win.height > 0;
    let cursor = this.oldest;
    let hopped = false;
    // The `cursor < highest` half of the guard keeps the NEWEST line alive in
    // a scroll-only store (a cap-1 store must still show something). With a
    // protected window retained there is always content to show, so a line
    // ABOVE the window bottom — the tail invariant broken by a scroll frame,
    // a shape only a malformed stream produces — is evictable even when it is
    // the highest key; otherwise one such line parks the store at bound+1
    // (found by the interleaved property test).
    while (
      this.tailCount > target &&
      cursor >= 0 &&
      (cursor < this.highest || (hasWindow && cursor > winBottom))
    ) {
      if (hasWindow && cursor >= this.win.base && cursor <= winBottom) {
        // Hop the cursor over the protected window to the first retained key
        // above it. O(1) contiguous probe first; the scan fallback is
        // bounded by map size (the DoS note below applies here too).
        if (winBottom >= this.highest) {
          break; // the window is the tail: nothing evictable above it
        }
        cursor = this.lines.has(winBottom + 1) ? winBottom + 1 : this.minKeyAbove(winBottom);
        hopped = true;
        if (cursor < 0) {
          break;
        }
        continue;
      }
      // AMENDMENT 2: the hop predicate extends to BROWSE intervals. The walk
      // steps over them exactly the way it steps over the window, which is
      // what keeps `this.oldest` correct: it stays the GLOBAL minimum retained
      // key (browse included), and hopping arms the once-per-pass bounds
      // recompute below. Without this the cursor would delete cache lines as
      // if they were tail, and `oldestIndex()` — which the frontier and the
      // gap geometry both read — would go stale on the first trim of any
      // browse episode.
      if (this.browse.has(cursor)) {
        const next = this.minKeyAboveNonBrowse(cursor);
        hopped = true;
        if (next < 0) {
          break;
        }
        cursor = next;
        continue;
      }
      const victim = cursor;
      if (this.forget(victim)) {
        // AMENDMENT 3: only TAIL victims advance everEvictedThrough. A browse
        // eviction is a cache drop, not a permanent trim, and must not teach
        // guard 2 to refuse a later re-fetch of the same index. (Browse lines
        // never reach here anyway — amendment 2 hops them — so this is the
        // invariant stated where a future edit would break it, and forget()
        // keeps the classification consistent if one ever does.)
        this.everEvictedThrough = Math.max(this.everEvictedThrough, victim);
      }
      if (this.lines.size === 0) {
        this.oldest = -1;
        this.highest = -1;
        return;
      }
      // Advance to the lowest remaining key above the victim. Contiguous
      // history (the common case: live scroll-off, resume replay, a
      // cat-bigfile burst) leaves victim + 1 present -- an O(1) probe. Only a
      // hole at the boundary falls back to a scan, and that scan is bounded
      // by map size (<= the retention bound), NOT by the integer gap to the
      // next retained line, so a malformed/compromised frame whose base jumps
      // far ahead of a retained low index cannot make eviction walk billions
      // of indices (an algorithmic-complexity DoS that freezes the tab).
      cursor = this.lines.has(victim + 1) ? victim + 1 : this.minKeyAbove(victim);
      if (!hopped) {
        // Classic head eviction: the cursor IS the minimum retained key.
        this.oldest = cursor;
      }
      if (cursor < 0) {
        break;
      }
    }
    if (hopped) {
      // The cursor hopped the window or a browse interval, so the minimum
      // retained key is not the cursor: recompute both bounds once per pass
      // (bounded scan; once per PASS, never per victim — R3 perf finding).
      let min = -1;
      let max = -1;
      for (const k of this.lines.keys()) {
        if (min < 0 || k < min) {
          min = k;
        }
        if (k > max) {
          max = k;
        }
      }
      this.oldest = min;
      this.highest = max;
    }
  }

  /**
   * The lowest retained NON-BROWSE key strictly above k, or -1. The eviction
   * walk uses it to step past a whole browse interval in one move rather than
   * key by key: a 2500-line cache would otherwise cost 2500 iterations of the
   * hop branch on every tail trim taken while the reader is deep in history.
   * Bounded by map size, like minKeyAbove.
   */
  private minKeyAboveNonBrowse(k: number): number {
    let min = -1;
    for (const key of this.lines.keys()) {
      if (key > k && !this.browse.has(key) && (min < 0 || key < min)) {
        min = key;
      }
    }
    return min;
  }

  /** The lowest retained key strictly above k, or -1. Bounded by map size. */
  private minKeyAbove(k: number): number {
    let min = -1;
    for (const key of this.lines.keys()) {
      if (key > k && (min < 0 || key < min)) {
        min = key;
      }
    }
    return min;
  }

  private updateWindow(msg: ScreenMessage): void {
    const next: WindowState = {
      base: msg.base,
      height: msg.rows.length,
      cursorRow: msg.cursor[0],
      cursorCol: msg.cursor[1],
      cursorStyle: msg.cursorStyle ?? 0,
      cursorHidden: msg.cursorHidden ?? false,
      cursorBlink: msg.cursorBlink ?? false,
    };
    if (!windowEqual(this.win, next)) {
      this.win = next;
      this.windowDirty = true;
    }
  }

  private updateWindowCursor(msg: ScreenMessage): void {
    // In the alt screen the base AND height keep the MAIN window's values —
    // the alt grid's own height lives in altRows — and only the cursor tracks
    // the alt frame. This keeps the retention bound (max(maxLines, window
    // height)) and the guard-2 window exemption defined over the MAIN screen,
    // the region alt exit must restore, rather than over a phantom range
    // mixing the main base with the alt height (R4 review, all three models:
    // a smaller alt frame otherwise appeared to lower the bound below the
    // protected main rows, and the exemption described rows that were not the
    // writable screen).
    const next: WindowState = {
      ...this.win,
      cursorRow: msg.cursor[0],
      cursorCol: msg.cursor[1],
      cursorStyle: msg.cursorStyle ?? 0,
      cursorHidden: msg.cursorHidden ?? false,
      cursorBlink: msg.cursorBlink ?? false,
    };
    if (!windowEqual(this.win, next)) {
      this.win = next;
      this.windowDirty = true;
    }
  }

  private enterAltIfNeeded(height: number): void {
    if (!this.alt) {
      this.alt = true;
      this.altDirty = true;
    }
    if (this.altRows.length !== height) {
      const next: WireRun[][] = new Array<WireRun[]>(height);
      for (let i = 0; i < height; i++) {
        next[i] = this.altRows[i] ?? [];
      }
      this.altRows = next;
      this.altDirty = true;
    }
  }

  private exitAltIfNeeded(): void {
    if (this.alt) {
      this.alt = false;
      this.altRows = [];
      this.altDirty = true;
    }
  }
}

function windowEqual(a: WindowState, b: WindowState): boolean {
  return (
    a.base === b.base &&
    a.height === b.height &&
    a.cursorRow === b.cursorRow &&
    a.cursorCol === b.cursorCol &&
    a.cursorStyle === b.cursorStyle &&
    a.cursorHidden === b.cursorHidden &&
    a.cursorBlink === b.cursorBlink
  );
}
