// Scroll-container fixtures that behave like a REAL scroller.
//
// happy-dom has no layout, so every scroll test defines scrollHeight /
// clientHeight / scrollTop itself. The fixtures that predate this file define
// scrollTop as a plain setter, which stores whatever it is handed — and a real
// container CLAMPS to [0, scrollHeight - clientHeight]. That difference is not
// cosmetic: the clamp IS the mechanism behind the tab-switch position bug
// (docs/scroll-position-fidelity.md §1.1), and it is the mechanism the pin's
// deliberate over-scroll write (`scrollTop = scrollHeight`) relies on. A
// non-clamping double cannot express either, so a test written on one cannot
// fail for the reason it exists — which is why the bug shipped, and why
// docs/paged-scrollback.md §7's multi-frame-prepend anchor test needs this too.
//
// Both helpers clamp. Use makeClampingScrollEl for a standalone container with
// a settable height, and installRowGeometry when rows in a real DOM tree must
// report offsets and the container must derive its height from them.

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

/** Handle for a patched row/container geometry, restored by `restore()`. */
export interface RowGeometry {
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
}): RowGeometry {
  const { output, termWrap, clientHeight } = opts;
  let rowHeight = opts.rowHeight;
  const mode = opts.offsets ?? "documentOrder";
  const prevOffsetTop = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetTop");
  Object.defineProperty(HTMLElement.prototype, "offsetTop", {
    configurable: true,
    get(this: HTMLElement): number {
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
    get: () => settle(),
    set: (v: number) => {
      top = v;
      settle();
    },
  });

  return {
    restore(): void {
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
