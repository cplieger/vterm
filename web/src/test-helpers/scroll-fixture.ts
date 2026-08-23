// Scroll-container fixtures that behave like a REAL scroller.
//
// A real browser gives a container real layout, and these fixtures still fake
// it, deliberately: what they model is the CLAMP, and the clamp is not something
// a test can arrange by giving an element overflow. The fixtures that predate
// this file define scrollTop as a plain setter, which stores whatever it is
// handed — and a real container CLAMPS to [0, scrollHeight - clientHeight]. That
// difference is not cosmetic: the clamp IS the mechanism behind the tab-switch
// position bug (docs/scroll-position-fidelity.md §1.1), and it is the mechanism
// the pin's deliberate over-scroll write (`scrollTop = scrollHeight`) relies on.
// A non-clamping double cannot express either, so a test written on one cannot
// fail for the reason it exists — which is why the bug shipped, and why
// docs/paged-scrollback.md §7's multi-frame-prepend anchor test needs this too.
//
// Every fixture here fakes scrollHeight, clientHeight and scrollTop TOGETHER,
// from one derivation. That matters more in a browser than it did under an
// emulator: a faked scrollTop beside a real scrollHeight is a container whose
// two halves disagree, and the arithmetic under test is exactly the difference
// between them.
//
// Both helpers clamp. Use makeClampingScrollEl for a standalone container with
// a settable height, and installRowGeometry when rows in a real DOM tree must
// report offsets and the container must derive its height from them.
//
// makeDeferredClampScrollEl is the third, and the odd one: a container that
// clamps a WRITE but does not reconcile an offset the content shrank out from
// under. That is the WebKit shape — so no browser this suite runs in can supply
// it, whatever layout it is given — it is the state reconcileScrollRange exists
// for, and neither clamping fixture can express it. A position invariant is
// worth testing against both shapes.

/** A standalone clamping scroll container whose content height is settable. */
export interface ClampingScrollEl {
  el: HTMLElement;
  /** Set the content height; re-clamps the current offset exactly as a browser
   *  does when content is removed. */
  setScrollHeight(next: number): void;
  /** Move the offset the way a USER does: clamped, then a scroll event. */
  userScrollTo(top: number): void;
  /** The offset a browser would settle on for this request. */
  clampedTo(top: number): number;
}

export function makeClampingScrollEl(scrollHeight: number, clientHeight: number): ClampingScrollEl {
  const el = document.createElement("div");
  let height = scrollHeight;
  let top = 0;
  const maxTop = (): number => Math.max(0, height - clientHeight);
  const clamp = (v: number): number => Math.max(0, Math.min(v, maxTop()));
  Object.defineProperty(el, "scrollHeight", { get: () => height, configurable: true });
  Object.defineProperty(el, "clientHeight", { get: () => clientHeight, configurable: true });
  Object.defineProperty(el, "scrollTop", {
    get: () => top,
    set: (v: number) => {
      top = clamp(v);
    },
    configurable: true,
  });
  return {
    el,
    setScrollHeight(next: number): void {
      height = next;
      // A shrink below the current offset clamps it, and the browser delivers
      // that as a scroll event — the event this library must not mistake for a
      // user scrolling up.
      const before = top;
      top = clamp(top);
      if (top !== before) {
        el.dispatchEvent(new Event("scroll"));
      }
    },
    userScrollTo(top_: number): void {
      el.scrollTop = top_;
      el.dispatchEvent(new Event("scroll"));
    },
    clampedTo: clamp,
  };
}

/** A container that does NOT reconcile its offset when the content shrinks. */
export interface DeferredClampScrollEl {
  el: HTMLElement;
  /** Shrink or grow the content, leaving the stored offset exactly where it is
   *  even when that is now past the end, and firing no event. */
  setScrollHeight(next: number): void;
  /** Move the offset the way a USER does: clamped, then a scroll event. This is
   *  also what makes a real container of this shape reconcile, which is why the
   *  bug it models "fixes itself when I scroll". */
  userScrollTo(top: number): void;
  /** The largest offset the geometry allows right now. */
  maxTop(): number;
}

/**
 * A scroll container shaped like WebKit rather than like Blink: a write is
 * clamped, but an offset already established is NOT reconciled when the content
 * shrinks under it. The offset simply stays where it was, out of range, and
 * reports itself that way, until something writes it or the user scrolls.
 *
 * This is the container `makeClampingScrollEl` cannot express, and the reason
 * this file exists a second time. Both older fixtures reconcile synchronously
 * (`setScrollHeight` re-clamps and fires the event in one call, and
 * `installRowGeometry` clamps inside the `scrollTop` GETTER), so
 * `distanceFromBottom()` is never negative in a test written on them, and the
 * whole third arithmetic state that `reconcileScrollRange` exists for is
 * unreachable. That is why the defect shipped: not one test could fail for it.
 *
 * Which of the two doubles is honest depends on the engine, and nothing in the
 * specs picks a winner. CSSOM View clamps a programmatic scroll at write time,
 * which both fixtures model; neither it nor CSS Overflow 3 says when a UA must
 * reconcile an offset the content has shrunk out from under. So a library that
 * only works on the reconciling double works only on some browsers, and every
 * position invariant deserves a test against both.
 *
 * Deliberately models ONE divergence. Writes still clamp here, because that part
 * IS specified; a fixture that broke both at once would stop being evidence
 * about either.
 */
export function makeDeferredClampScrollEl(
  scrollHeight: number,
  clientHeight: number,
): DeferredClampScrollEl {
  const el = document.createElement("div");
  let height = scrollHeight;
  let top = 0;
  const maxTop = (): number => Math.max(0, height - clientHeight);
  Object.defineProperty(el, "scrollHeight", { get: () => height, configurable: true });
  Object.defineProperty(el, "clientHeight", { get: () => clientHeight, configurable: true });
  Object.defineProperty(el, "scrollTop", {
    get: () => top,
    // A write is clamped (CSSOM View), and is the supported way back into range.
    set: (v: number) => {
      top = Math.max(0, Math.min(v, maxTop()));
    },
    configurable: true,
  });
  return {
    el,
    setScrollHeight(next: number): void {
      // The whole point: the offset is not touched and no event fires, so a
      // shrink below it leaves the container reporting an offset it cannot
      // legally hold, silently.
      height = next;
    },
    userScrollTo(top_: number): void {
      el.scrollTop = top_;
      el.dispatchEvent(new Event("scroll"));
    },
    maxTop,
  };
}

/** Handle for a patched row/container geometry, restored by `restore()`. */ export interface RowGeometry {
  restore(): void;
  /** Change the row height, the way a font load, a zoom, or a CSS change does.
   *  Every row's offset and the container's height move together, which is what
   *  makes a saved PIXEL offset meaningless and a saved LINE survive — and what
   *  a fixture with a fixed row height cannot express at all. */
  setRowHeight(next: number): void;
}

/**
 * Make rows in a real DOM tree report offsets, and make `termWrap` a clamping
 * scroller whose content height derives from the rows actually built.
 *
 * offsetTop comes from the row's `data-abs` when present so a SPARSE store (a
 * paged-in region far above the tail, or a gap) keeps monotonic offsets, and
 * from document order otherwise; children with neither read 0, which is what a
 * marker pinned at the top of the container really has.
 */
export function installRowGeometry(opts: {
  output: HTMLElement;
  termWrap: HTMLElement;
  rowHeight: number;
  clientHeight: number;
  /** How a row's offsetTop is derived.
   *
   *  `"documentOrder"` (the DEFAULT, and the honest one) positions a row by its
   *  index among the container's children, which is what a real surface does:
   *  rows occupy space in the order they are built, and a row's pixel offset says
   *  nothing about its absolute line number.
   *
   *  `"byAbs"` positions it at `data-abs * rowHeight`, making pixel space and line
   *  space ISOMORPHIC. That is convenient for a sparse store, and it is a trap for
   *  any test about per-view scroll memory: under it a pixel-offset restore and a
   *  line restore are indistinguishable, so a test written to prove the line
   *  restore works passes on the pixel implementation it was written to condemn.
   *  Use it only when a test's subject is genuinely sparse geometry. */
  offsets?: "documentOrder" | "byAbs";
  /** Rows above the first built one still occupy space in a real surface; this
   *  keeps the container's height honest for a sparse store. */
  heightOf?: (output: HTMLElement) => number;
  /** How the container answers for an offset the content has shrunk out from
   *  under.
   *
   *  `"synchronous"` (the DEFAULT) clamps in the GETTER, so an out-of-range
   *  offset is never observable. That is Blink and Gecko, and it is the model
   *  every test in this repo was written on.
   *
   *  `"deferred"` clamps a WRITE but leaves an established offset exactly where
   *  it was, reporting it out of range until something writes it. That is
   *  WebKit, and it is the shape `reconcileScrollRange` exists for. Use it for
   *  any test about a position invariant surviving a content shrink. */
  reconcile?: "synchronous" | "deferred";
}): RowGeometry {
  const { output, termWrap, clientHeight } = opts;
  let rowHeight = opts.rowHeight;
  const mode = opts.offsets ?? "documentOrder";
  const reconcile = opts.reconcile ?? "synchronous";
  const prevOffsetTop = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetTop");
  // A prototype patch reaches every element in the page for as long as it is
  // installed, the runner's own DOM included, and rows are built by the renderer
  // AFTER this call so a per-element assignment cannot cover them. Containment
  // is the next best thing: answer only for descendants of the fixture's own
  // container and let everything else fall through to the browser's real
  // offsetTop.
  Object.defineProperty(HTMLElement.prototype, "offsetTop", {
    configurable: true,
    get(this: HTMLElement): number {
      if (this !== output && !output.contains(this)) {
        return Number(prevOffsetTop?.get?.call(this) ?? 0);
      }
      if (mode === "byAbs") {
        const abs = Number(this.dataset["abs"] ?? "");
        if (Number.isFinite(abs)) {
          return abs * rowHeight;
        }
      }
      const parent = this.parentElement;
      if (!parent) {
        return 0;
      }
      return Array.prototype.indexOf.call(parent.children, this) * rowHeight;
    },
  });

  const contentHeight =
    opts.heightOf ??
    ((out: HTMLElement): number => {
      if (mode === "byAbs") {
        const last = out.lastElementChild as HTMLElement | null;
        return last === null ? 0 : last.offsetTop + rowHeight;
      }
      return out.children.length * rowHeight;
    });

  let top = 0;
  const maxTop = (): number => Math.max(0, contentHeight(output) - clientHeight);
  // DESTRUCTIVE clamping, which is the whole point. A browser clamps the stored
  // offset when the content shrinks and does NOT remember the larger value: the
  // reading position is GONE, which is the property this design exists to work
  // around. A getter that clamps for display while retaining the old number
  // silently restores the position when the content grows back — modelling a
  // container that never loses a position, i.e. exactly the fiction that let the
  // tab-switch bug through review.
  const settle = (): number => {
    top = Math.max(0, Math.min(top, maxTop()));
    return top;
  };
  Object.defineProperty(termWrap, "scrollHeight", {
    configurable: true,
    get: () => Math.max(contentHeight(output), clientHeight),
  });
  Object.defineProperty(termWrap, "clientHeight", { configurable: true, get: () => clientHeight });
  Object.defineProperty(termWrap, "scrollTop", {
    configurable: true,
    // The getter settles only in the synchronous mode. In the deferred mode it
    // reports the stored offset verbatim, which is how an offset past the end of
    // the content becomes observable at all.
    get: () => (reconcile === "synchronous" ? settle() : top),
    set: (v: number) => {
      top = v;
      settle();
    },
  });

  return {
    restore(): void {
      // RESTORE the descriptor, never `delete`. `offsetTop` is defined on
      // HTMLElement.prototype by the platform, so deleting the patched property
      // removes the real accessor with it and every later read returns
      // `undefined`. The else branch is therefore unreachable in a browser and
      // exists only so a missing descriptor cannot leave the patch installed.
      if (prevOffsetTop) {
        Object.defineProperty(HTMLElement.prototype, "offsetTop", prevOffsetTop);
      } else {
        delete (HTMLElement.prototype as unknown as Record<string, unknown>)["offsetTop"];
      }
    },
    setRowHeight(next: number): void {
      rowHeight = next;
      // Re-settle: taller rows can push the current offset past the new maximum
      // exactly as a real reflow does, and the clamp is destructive.
      settle();
    },
  };
}
