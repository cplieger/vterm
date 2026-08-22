// @vitest-environment happy-dom
//
// The renderer's half of the shrink-range correction: a flush that removes rows
// on a container that does NOT reconcile the offset must leave the viewport on
// the content, not parked past the end of it.
//
// This is the test that would have caught the shipped defect, and the two things
// that made it unwritable before are both fixtures rather than logic:
//
//   - every scroll fixture in this repo clamped in the scrollTop GETTER, so an
//     out-of-range offset could not be observed; and
//   - the assertion has to be made at the RENDER level, because the seam being
//     guarded is a call in flushRender's tail. A unit test on scroll.ts proves
//     reconcileScrollRange works; only this proves the renderer calls it.
//
// So it is deliberately written to fail if a future refactor drops the call,
// reorders it after the position invariants, or stops setting removedRowsThisPass
// on a path that removes rows.

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import * as render from "./render.js";
import * as scroll from "./scroll.js";
import { installRowGeometry, type RowGeometry } from "./test-helpers/scroll-fixture.js";
import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";

interface FakeCtx {
  font: string;
  measureText: (t: string) => { width: number };
}
HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
  const ctx: FakeCtx = { font: "", measureText: (t: string) => ({ width: t.length * 8 }) };
  return ctx;
} as typeof HTMLCanvasElement.prototype.getContext;

const ROW_H = 17;
const VIEWPORT_H = 170; // 10 rows visible
const WINDOW_H = 4;

function row(text: string): WireRun[] {
  return [{ t: text, f: -1, b: -1, a: 0, uc: -1 }];
}
function screenMsg(
  base: number,
  rows: WireRun[][],
  changed: number[],
  extra: Partial<ScreenMessage> = {},
): ScreenMessage {
  return {
    type: "screen",
    base,
    rows,
    changed,
    cursor: [0, 0],
    cursorHidden: false,
    cursorStyle: 0,
    cursorBlink: false,
    ...extra,
  };
}
function scrollMsg(firstIndex: number, texts: string[]): ScrollMessage {
  return { type: "scroll", firstIndex, lines: texts.map(row) };
}
function windowRows(): WireRun[][] {
  return [row("w0"), row("w1"), row("w2"), row("w3")];
}

describe("a shrink on a container that does not reconcile its offset", () => {
  let output: HTMLDivElement;
  let termWrap: HTMLDivElement;
  let geom: RowGeometry;
  let frames: FrameRequestCallback[];

  function pumpUntilIdle(limit = 200): void {
    let n = 0;
    while (frames.length > 0 && n < limit) {
      const due = frames;
      frames = [];
      for (const cb of due) {
        cb(performance.now());
      }
      n++;
    }
  }

  /** The offset the geometry allows: what "the bottom" means right now. */
  function maxTop(): number {
    return Math.max(0, termWrap.scrollHeight - termWrap.clientHeight);
  }

  /** Fill the surface with `count` history lines plus a live window. */
  function fill(count: number): void {
    const texts: string[] = [];
    for (let i = 0; i < count; i++) {
      texts.push(`line ${String(i)}`);
    }
    render.handleScroll(scrollMsg(0, texts));
    render.handleScreen(screenMsg(count, windowRows(), [0, 1, 2, WINDOW_H - 1]));
    pumpUntilIdle();
  }

  beforeEach(() => {
    frames = [];
    vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback): number => {
      frames.push(cb);
      return frames.length;
    });
    vi.stubGlobal("cancelAnimationFrame", (): void => {
      /* the pump drops un-run callbacks with the array */
    });
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    output = document.getElementById("term-output") as HTMLDivElement;
    geom = installRowGeometry({
      output,
      termWrap,
      rowHeight: ROW_H,
      clientHeight: VIEWPORT_H,
      reconcile: "deferred",
    });
    render.init({ output, termWrap });
    render.updateFontMetrics();
    scroll.init({ scrollEl: termWrap });
  });

  afterEach(() => {
    geom.restore();
    vi.unstubAllGlobals();
    render.resetScrollback();
    document.body.innerHTML = "";
  });

  it("leaves the viewport on the content after the app erases its scrollback", () => {
    fill(400);
    // Following, so the flush pinned the viewport to the tail of 404 rows.
    expect(scroll.isUserScrolledUp()).toBe(false);
    const strandedFrom = termWrap.scrollTop;
    expect(strandedFrom).toBeGreaterThan(6000);

    // ED3: the server reports the scrollback gone, and the store forgets every
    // line below the new base. The container holds its offset regardless.
    render.handleScreen(
      screenMsg(400, windowRows(), [0, 1, 2, WINDOW_H - 1], { scrollbackCleared: true }),
    );
    pumpUntilIdle();

    // The surface is now one window, so the bottom is the top.
    expect(output.children.length).toBeLessThanOrEqual(WINDOW_H);
    expect(termWrap.scrollTop).toBe(maxTop());
    // And explicitly: not where it was left. Without the correction this stayed
    // at the old offset and the viewport showed background with the content
    // thousands of pixels above it.
    expect(termWrap.scrollTop).toBeLessThan(strandedFrom);
  });

  it("keeps auto-follow engaged across that correction", () => {
    fill(400);
    render.handleScreen(
      screenMsg(400, windowRows(), [0, 1, 2, WINDOW_H - 1], { scrollbackCleared: true }),
    );
    pumpUntilIdle();
    // The correction is a large upward move. Classified as a gesture it would
    // switch follow off and leave the next output line unpinned.
    termWrap.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(false);

    // The proof that follow is real: more output arrives and lands in view.
    render.handleScroll(scrollMsg(404, ["after A", "after B"]));
    render.handleScreen(screenMsg(406, windowRows(), [0, 1, 2, WINDOW_H - 1]));
    pumpUntilIdle();
    expect(termWrap.scrollTop).toBe(maxTop());
  });

  it("corrects a reader who had scrolled up, and leaves them holding", () => {
    fill(400);
    termWrap.scrollTop = 2000;
    termWrap.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(true);

    render.handleScreen(
      screenMsg(400, windowRows(), [0, 1, 2, WINDOW_H - 1], { scrollbackCleared: true }),
    );
    pumpUntilIdle();

    // The lines they were reading no longer exist, so the anchor stands down and
    // the tail with follow OFF is the designed landing. What must not happen is
    // being left over empty space.
    expect(termWrap.scrollTop).toBe(maxTop());
    termWrap.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(true);
  });

  it("corrects the offset on a full reset too", () => {
    fill(400);
    const strandedFrom = termWrap.scrollTop;
    render.resetScrollback();
    pumpUntilIdle();
    expect(termWrap.scrollTop).toBeLessThan(strandedFrom);
    expect(termWrap.scrollTop).toBe(maxTop());
  });

  it("collapses the content-space overlays inside the full-reset wipe", () => {
    // The overlays live in the scroll container carrying a top in CONTENT
    // coordinates, so one left at the old cursor row holds the container's
    // scrollable overflow above the built content: the shrink then measures a
    // phantom height, reads "already at the bottom" over zero rows, and both
    // seams correctly decline to act on a geometry that is a lie. So the
    // full-reset branch collapses them with the rows rather than leaving it to
    // the flush tail, which is exactly what does not run on the paths where this
    // bites (a throw mid-drain, the bounded give-up, a long frame).
    //
    // The consumer's textarea and IME view are reachable only through the cursor
    // seam, so that seam firing with the caret already hidden IS the collapse.
    const seen: boolean[] = [];
    render.init({
      output,
      termWrap,
      onCursorMove: () => {
        const c = termWrap.querySelector(".term-cursor-overlay");
        seen.push(c?.classList.contains("visible") === true);
      },
    });
    render.updateFontMetrics();
    fill(400);
    seen.length = 0;

    render.resetScrollback();
    pumpUntilIdle();

    // The first notification of the reset pass is the collapse, and it must
    // report a caret that is already hidden (class-gated to display: none, so it
    // contributes no height). Without the collapse the first notification is the
    // tail's, one full drain later.
    expect(seen.length).toBeGreaterThan(0);
    expect(seen[0]).toBe(false);
  });

  it("announces and corrects an alt-screen entry that replaces the scrollback", () => {
    // renderAlt's full rebuild swaps every main-buffer row for one screen of
    // grid, and the alt branch returns before the shared bookkeeping, so this
    // path used to reach neither seam.
    fill(400);
    const strandedFrom = termWrap.scrollTop;
    render.handleScreen(
      screenMsg(400, [row("alt0"), row("alt1"), row("alt2")], [0, 1, 2], { altActive: true }),
    );
    pumpUntilIdle();

    expect(output.children.length).toBe(3);
    expect(termWrap.scrollTop).toBeLessThan(strandedFrom);
    expect(termWrap.scrollTop).toBe(maxTop());
  });
});
