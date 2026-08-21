// @vitest-environment happy-dom
//
// Contract test for the scroll-container fixtures.
//
// A test double normally needs no test of its own, and this one is the
// exception the file's own header argues for: its clamp IS the mechanism behind
// the tab-switch position bug (docs/scroll-position-fidelity.md §1.1), and a
// double that stores whatever it is handed makes a scroll test "cannot fail for
// the reason it exists". The clamp is therefore load-bearing evidence, not
// convenience — but nothing pinned it: the fixture could stop clamping
// altogether, or start remembering the offset a shrink destroyed, and 91 tests
// across four suites would still pass while proving nothing.
//
// So the three properties every one of those suites silently depends on are
// asserted here directly: the offset is clamped to [0, scrollHeight -
// clientHeight]; the clamp is DESTRUCTIVE (a browser does not restore the old
// offset when the content grows back); and a shrink that moves the offset
// announces it with exactly one scroll event, which is the event the library
// must not mistake for a user scrolling up. Precedent for testing a helper
// here: wire-manifest.test.ts, beside this file.

import { describe, it, expect, afterEach } from "vitest";
import { makeClampingScrollEl, installRowGeometry } from "./scroll-fixture.js";

/** Count scroll events the fixture dispatches on its element. */
function countScrolls(el: HTMLElement): { n: () => number } {
  let n = 0;
  el.addEventListener("scroll", () => {
    n++;
  });
  return { n: () => n };
}

describe("makeClampingScrollEl clamps like a real scroller", () => {
  it("refuses an offset past the bottom, settling at scrollHeight - clientHeight", () => {
    // The pin writes `scrollTop = scrollHeight` deliberately: a real container
    // takes the maximum, not the number it was handed.
    const s = makeClampingScrollEl(1000, 200);
    s.el.scrollTop = 1000;
    expect(s.el.scrollTop).toBe(800);
  });

  it("refuses a negative offset, settling at 0", () => {
    const s = makeClampingScrollEl(1000, 200);
    s.el.scrollTop = -50;
    expect(s.el.scrollTop).toBe(0);
  });

  it("reports 0 as the maximum when the content is shorter than the viewport", () => {
    const s = makeClampingScrollEl(120, 200);
    s.el.scrollTop = 90;
    expect(s.el.scrollTop).toBe(0);
  });

  it("clampedTo answers with the offset the container will actually settle on", () => {
    // Tests compute their expectations from this, so it has to agree with the
    // setter rather than be a second opinion about the same arithmetic.
    const s = makeClampingScrollEl(1000, 200);
    expect(s.clampedTo(5000)).toBe(800);
    expect(s.clampedTo(-1)).toBe(0);
    expect(s.clampedTo(250)).toBe(250);
  });

  it("loses the position a shrink destroyed instead of restoring it on regrowth", () => {
    // The whole design under test exists because a browser does NOT remember
    // the larger offset. A fixture that clamps for display while retaining the
    // old number models a container that never loses a position — the fiction
    // that let the tab-switch bug through review.
    const s = makeClampingScrollEl(1000, 200);
    s.el.scrollTop = 800;
    s.setScrollHeight(300);
    expect(s.el.scrollTop).toBe(100);
    s.setScrollHeight(1000);
    expect(s.el.scrollTop).toBe(100);
  });

  it("forgets an over-scroll write, so later growth cannot resurrect it", () => {
    // The other half of destructiveness, and the half a shrink cannot show: the
    // pin writes `scrollTop = scrollHeight` on purpose, so a fixture that stored
    // the raw request and clamped only on READ would jump to that stale 5000 the
    // moment a drain made the content taller. The offset must stay where the
    // container settled at write time.
    const s = makeClampingScrollEl(1000, 200);
    s.el.scrollTop = 5000;
    expect(s.el.scrollTop).toBe(800);
    s.setScrollHeight(6000);
    expect(s.el.scrollTop).toBe(800);
  });

  it("announces a shrink that moved the offset with exactly one scroll event", () => {
    const s = makeClampingScrollEl(1000, 200);
    s.el.scrollTop = 800;
    const scrolls = countScrolls(s.el);
    s.setScrollHeight(300);
    expect(scrolls.n()).toBe(1);
  });

  it("stays silent on a shrink that left the offset where it was", () => {
    // A shrink above the reader's position moves nothing, so there is no event
    // to mistake for anything: the arming discipline must not be triggered by
    // content changes alone.
    const s = makeClampingScrollEl(1000, 200);
    s.el.scrollTop = 50;
    const scrolls = countScrolls(s.el);
    s.setScrollHeight(300);
    expect(s.el.scrollTop).toBe(50);
    expect(scrolls.n()).toBe(0);
  });

  it("moves and announces a user scroll, clamped the same way", () => {
    const s = makeClampingScrollEl(1000, 200);
    const scrolls = countScrolls(s.el);
    s.userScrollTo(5000);
    expect(s.el.scrollTop).toBe(800);
    expect(scrolls.n()).toBe(1);
  });
});

describe("installRowGeometry derives the scroller from the rows actually built", () => {
  let geom: { restore(): void; setRowHeight(next: number): void } | null = null;

  afterEach(() => {
    geom?.restore();
    geom = null;
  });

  function tree(rows: number): { output: HTMLElement; termWrap: HTMLElement } {
    const termWrap = document.createElement("div");
    const output = document.createElement("div");
    termWrap.appendChild(output);
    for (let i = 0; i < rows; i++) {
      output.appendChild(document.createElement("div"));
    }
    return { output, termWrap };
  }

  it("positions rows in document order and heights the container from them", () => {
    const { output, termWrap } = tree(10);
    geom = installRowGeometry({ output, termWrap, rowHeight: 20, clientHeight: 100 });
    expect((output.children[0] as HTMLElement).offsetTop).toBe(0);
    expect((output.children[3] as HTMLElement).offsetTop).toBe(60);
    expect(termWrap.scrollHeight).toBe(200);
    expect(termWrap.clientHeight).toBe(100);
  });

  it("never reports a content height below the viewport", () => {
    const { output, termWrap } = tree(2);
    geom = installRowGeometry({ output, termWrap, rowHeight: 20, clientHeight: 100 });
    expect(termWrap.scrollHeight).toBe(100);
  });

  it("clamps the offset to the rows present, and destructively", () => {
    // A row-height change is what makes a saved PIXEL offset meaningless: every
    // offset and the content height move together. Shrinking the rows takes the
    // reading position away for good, exactly like a reflow.
    const { output, termWrap } = tree(10);
    geom = installRowGeometry({ output, termWrap, rowHeight: 20, clientHeight: 100 });
    termWrap.scrollTop = 5000;
    expect(termWrap.scrollTop).toBe(100);
    geom.setRowHeight(5);
    expect(termWrap.scrollTop).toBe(0);
    geom.setRowHeight(20);
    expect(termWrap.scrollTop).toBe(0);
  });

  it("positions a row by its absolute index only when asked to", () => {
    // `byAbs` makes pixel space and line space isomorphic, which is a trap for
    // any per-view scroll-memory test — so it must be opt-in, and the default
    // must be document order even for rows that carry data-abs.
    const { output, termWrap } = tree(0);
    const row = document.createElement("div");
    row.dataset["abs"] = "40";
    output.appendChild(row);
    geom = installRowGeometry({
      output,
      termWrap,
      rowHeight: 20,
      clientHeight: 100,
      offsets: "byAbs",
    });
    expect(row.offsetTop).toBe(800);
    expect(termWrap.scrollHeight).toBe(820);
  });

  it("restores the real offsetTop so the patch cannot leak into the next file", () => {
    // The base vitest config runs with isolate: false, so a prototype patch that
    // outlives its test poisons every suite that follows it in the worker.
    const { output, termWrap } = tree(3);
    const handle = installRowGeometry({ output, termWrap, rowHeight: 20, clientHeight: 100 });
    expect((output.children[2] as HTMLElement).offsetTop).toBe(40);
    handle.restore();
    expect((output.children[2] as HTMLElement).offsetTop).toBe(0);
  });
});
