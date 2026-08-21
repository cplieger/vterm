// @vitest-environment happy-dom

// Mouse hit-testing and mode-gating tests: the parts of mouse.ts that decide
// WHETHER to report and WHICH coordinate pair to report, as opposed to the
// button-byte composition mouse.test.ts already pins.
//
// Covered here and nowhere else:
//   - SGR-pixels (DEC 1016): the whole pixel-coordinate branch, which had no
//     test at all even though the wire decoder plumbs the flag.
//   - The terminal element's own viewport offset, which every existing test
//     hides by leaving getBoundingClientRect all-zero (happy-dom does no
//     layout), so a sign error in the rect subtraction is invisible to them.
//   - The refusals: tracking off, no supported encoding enabled, a degenerate
//     cell size, a pointer outside the element.
//   - The disposer detaching EVERY listener, not just mousedown.
//
// Spec source is the same as mouse.test.ts: xterm "Control Sequences", Mouse
// Tracking, plus DEC 1016 (SGR-pixels — same CSI < grammar, pixel coordinates,
// 1-based).

import { describe, it, expect, beforeEach } from "vitest";
import { init as initMouse, type MouseInputHandler } from "./mouse.js";
import * as modes from "./modes.js";

const ESC = "\x1b";
const MOTION = 32;

// SGR-1006 grammar, mirrored from the spec rather than from encodeSGR.
const expectedSGR = (b: number, col: number, row: number, release: boolean): string =>
  `${ESC}[<${b};${col};${row}${release ? "m" : "M"}`;

beforeEach(() => {
  // modes is a module singleton and the suite runs with isolate:false: reset
  // every flag so nothing leaks in or out of this file.
  modes.setModes(true, false, false, false, 0, false, false, false);
});

interface Fixture {
  term: HTMLDivElement;
  sent: string[];
  dispose: () => void;
}

/**
 * A terminal element with an 8x16 cell and a settable viewport rect. happy-dom
 * has no layout, so getBoundingClientRect is defined here: with left/top 0 the
 * hit test is `floor(clientX / 8) + 1`, and a non-zero rect is how an element
 * that is not at the viewport origin is expressed.
 */
function setup(rect: { left: number; top: number } = { left: 0, top: 0 }): Fixture {
  const term = document.createElement("div");
  Object.defineProperty(term, "getBoundingClientRect", {
    value: () => ({ left: rect.left, top: rect.top, right: 0, bottom: 0, width: 0, height: 0 }),
    configurable: true,
  });
  const sent: string[] = [];
  const handler: MouseInputHandler = {
    send: (data) => sent.push(data),
    cellSize: () => ({ width: 8, height: 16 }),
    termElement: () => term,
  };
  const dispose = initMouse(handler);
  return { term, sent, dispose };
}

/** A terminal element whose reported cell size is degenerate. */
function setupWithCell(width: number, height: number): Fixture {
  const term = document.createElement("div");
  Object.defineProperty(term, "getBoundingClientRect", {
    value: () => ({ left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }),
    configurable: true,
  });
  const sent: string[] = [];
  const dispose = initMouse({
    send: (data) => sent.push(data),
    cellSize: () => ({ width, height }),
    termElement: () => term,
  });
  return { term, sent, dispose };
}

function mouseEvent(type: string, opts: Record<string, unknown>): MouseEvent {
  const e = new MouseEvent(type, { cancelable: true });
  for (const [key, value] of Object.entries(opts)) {
    Object.defineProperty(e, key, { value, configurable: true });
  }
  return e;
}

function wheelEvent(opts: Record<string, unknown>): WheelEvent {
  const e = new WheelEvent("wheel", { cancelable: true });
  for (const [key, value] of Object.entries(opts)) {
    Object.defineProperty(e, key, { value, configurable: true });
  }
  return e;
}

/** SGR 1006 + the given tracking mode; pixels and focus off. */
function enableSGR(mode: number): void {
  modes.setModes(true, false, true, false, mode, false, false, false);
}

/** DEC 1016 (SGR-pixels) + the given tracking mode; SGR 1006 off. */
function enablePixels(mode: number): void {
  modes.setModes(true, false, false, false, mode, false, false, true);
}

describe("SGR-pixels (DEC 1016): reports pixel offsets instead of cells", () => {
  it("maps the element's top-left pixel to 1;1", () => {
    // Same CSI < grammar as 1006, but Px/Py are 1-based PIXEL offsets within
    // the terminal element rather than cell coordinates.
    enablePixels(1002);
    const { term, sent } = setup();
    term.dispatchEvent(mouseEvent("mousedown", { clientX: 0, clientY: 0, button: 0, buttons: 0 }));
    expect(sent).toEqual([expectedSGR(0, 1, 1, false)]);
  });

  it("reports the pixel offset itself, not the cell it falls in", () => {
    enablePixels(1002);
    const { term, sent } = setup();
    term.dispatchEvent(
      mouseEvent("mousedown", { clientX: 37, clientY: 82, button: 0, buttons: 0 }),
    );
    expect(sent).toEqual([expectedSGR(0, 38, 83, false)]);
  });

  it("subtracts the element's own viewport offset", () => {
    // A terminal that is not at the viewport origin: the report is relative to
    // the element, so the rect is subtracted, never added.
    enablePixels(1002);
    const { term, sent } = setup({ left: 10, top: 20 });
    term.dispatchEvent(
      mouseEvent("mousedown", { clientX: 30, clientY: 50, button: 0, buttons: 0 }),
    );
    expect(sent).toEqual([expectedSGR(0, 21, 31, false)]);
  });

  it("reports nothing for a pointer left of the element", () => {
    enablePixels(1002);
    const { term, sent } = setup();
    term.dispatchEvent(
      mouseEvent("mousedown", { clientX: -1, clientY: 40, button: 0, buttons: 0 }),
    );
    expect(sent).toEqual([]);
  });

  it("reports nothing for a pointer above the element", () => {
    enablePixels(1002);
    const { term, sent } = setup();
    term.dispatchEvent(
      mouseEvent("mousedown", { clientX: 40, clientY: -1, button: 0, buttons: 0 }),
    );
    expect(sent).toEqual([]);
  });

  it("reports pixel offsets for a drag as well as a press", () => {
    enablePixels(1002);
    const { term, sent } = setup();
    term.dispatchEvent(mouseEvent("mousemove", { clientX: 5, clientY: 9, buttons: 1 }));
    expect(sent).toEqual([expectedSGR(0 + MOTION, 6, 10, false)]);
  });
});

describe("cell hit-testing: the element's viewport offset and its edges", () => {
  it("subtracts the element's own viewport offset before dividing by the cell", () => {
    // rect.left 10 with an 8px cell: clientX 26 is 16px into the element, i.e.
    // column 3. Adding the rect instead would report column 5.
    enableSGR(1002);
    const { term, sent } = setup({ left: 10, top: 20 });
    term.dispatchEvent(
      mouseEvent("mousedown", { clientX: 26, clientY: 52, button: 0, buttons: 0 }),
    );
    expect(sent).toEqual([expectedSGR(0, 3, 3, false)]);
  });

  it("reports nothing for a pointer left of the element", () => {
    enableSGR(1002);
    const { term, sent } = setup();
    term.dispatchEvent(
      mouseEvent("mousedown", { clientX: -8, clientY: 32, button: 0, buttons: 0 }),
    );
    expect(sent).toEqual([]);
  });

  it("reports nothing for a pointer above the element", () => {
    enableSGR(1002);
    const { term, sent } = setup();
    term.dispatchEvent(
      mouseEvent("mousedown", { clientX: 16, clientY: -16, button: 0, buttons: 0 }),
    );
    expect(sent).toEqual([]);
  });

  it("reports nothing when the cell width is not yet measured", () => {
    // cellSize() reads the renderer's font metrics, which are zero before the
    // first measurement; dividing by it would report an infinite column.
    enableSGR(1002);
    const { term, sent } = setupWithCell(0, 16);
    term.dispatchEvent(
      mouseEvent("mousedown", { clientX: 16, clientY: 32, button: 0, buttons: 0 }),
    );
    expect(sent).toEqual([]);
  });

  it("reports nothing when the cell height is not yet measured", () => {
    enableSGR(1002);
    const { term, sent } = setupWithCell(8, 0);
    term.dispatchEvent(
      mouseEvent("mousedown", { clientX: 16, clientY: 32, button: 0, buttons: 0 }),
    );
    expect(sent).toEqual([]);
  });
});

describe("mode gating applies to every event type, not just the press", () => {
  it("reports no release while tracking is off", () => {
    modes.setModes(true, false, true, false, 0, false, false, false); // SGR on, tracking off
    const { term, sent } = setup();
    term.dispatchEvent(mouseEvent("mouseup", { clientX: 16, clientY: 32, button: 0, buttons: 0 }));
    expect(sent).toEqual([]);
  });

  it("reports no motion while tracking is off", () => {
    modes.setModes(true, false, true, false, 0, false, false, false);
    const { term, sent } = setup();
    term.dispatchEvent(mouseEvent("mousemove", { clientX: 16, clientY: 32, buttons: 1 }));
    expect(sent).toEqual([]);
  });

  it("reports no wheel while tracking is off", () => {
    modes.setModes(true, false, true, false, 0, false, false, false);
    const { term, sent } = setup();
    term.dispatchEvent(wheelEvent({ deltaY: -1, clientX: 16, clientY: 32 }));
    expect(sent).toEqual([]);
  });
});

describe("no supported encoding enabled: tracking alone reports nothing", () => {
  // Tracking on but neither SGR 1006 nor SGR-pixels: the legacy X10 / urxvt
  // encodings are deliberately unimplemented, so the module stays silent rather
  // than sending a report in an encoding the app did not ask for.
  const legacyOnly = (): void => {
    modes.setModes(true, false, false, false, 1002, false, false, false);
  };

  it("reports no press", () => {
    legacyOnly();
    const { term, sent } = setup();
    term.dispatchEvent(
      mouseEvent("mousedown", { clientX: 16, clientY: 32, button: 0, buttons: 0 }),
    );
    expect(sent).toEqual([]);
  });

  it("reports no release", () => {
    legacyOnly();
    const { term, sent } = setup();
    term.dispatchEvent(mouseEvent("mouseup", { clientX: 16, clientY: 32, button: 0, buttons: 0 }));
    expect(sent).toEqual([]);
  });

  it("reports no motion", () => {
    legacyOnly();
    const { term, sent } = setup();
    term.dispatchEvent(mouseEvent("mousemove", { clientX: 16, clientY: 32, buttons: 1 }));
    expect(sent).toEqual([]);
  });

  it("reports no wheel", () => {
    legacyOnly();
    const { term, sent } = setup();
    term.dispatchEvent(wheelEvent({ deltaY: -1, clientX: 16, clientY: 32 }));
    expect(sent).toEqual([]);
  });
});

describe("reported events suppress the browser's own handling", () => {
  it("preventDefaults a press, a release and a wheel", () => {
    // Without this the browser also selects text under a TUI that is tracking
    // the mouse, and scrolls the page under one that handles the wheel.
    enableSGR(1002);
    const { term } = setup();
    const press = mouseEvent("mousedown", { clientX: 16, clientY: 32, button: 0, buttons: 0 });
    const release = mouseEvent("mouseup", { clientX: 16, clientY: 32, button: 0, buttons: 0 });
    const wheel = wheelEvent({ deltaY: -1, clientX: 16, clientY: 32 });
    term.dispatchEvent(press);
    term.dispatchEvent(release);
    term.dispatchEvent(wheel);
    expect(press.defaultPrevented).toBe(true);
    expect(release.defaultPrevented).toBe(true);
    expect(wheel.defaultPrevented).toBe(true);
  });

  it("swallows a gesture it reports nothing for, rather than letting the page scroll", () => {
    // A horizontal gesture (deltaY 0) is not reportable — there is no sideways
    // wheel button in SGR 1006 — but a terminal that is tracking the wheel owns
    // the gesture: passing it through would scroll the page out from under the
    // TUI. Guarding preventDefault along with the report would trade the wire
    // bug for that one, so the two halves are asserted apart.
    enableSGR(1002);
    const { term, sent } = setup();
    const sideways = wheelEvent({ deltaY: 0, deltaX: -40, clientX: 16, clientY: 32 });
    term.dispatchEvent(sideways);
    expect(sent).toEqual([]);
    expect(sideways.defaultPrevented).toBe(true);
  });
});

describe("the disposer detaches every listener it attached", () => {
  it("stops reporting releases, motion and wheel after dispose", () => {
    enableSGR(1003);
    const { term, sent, dispose } = setup();
    dispose();
    term.dispatchEvent(mouseEvent("mouseup", { clientX: 16, clientY: 32, button: 0, buttons: 0 }));
    term.dispatchEvent(mouseEvent("mousemove", { clientX: 16, clientY: 32, buttons: 1 }));
    term.dispatchEvent(wheelEvent({ deltaY: -1, clientX: 16, clientY: 32 }));
    expect(sent).toEqual([]);
  });

  it("stops reporting focus changes after dispose", () => {
    modes.setModes(true, false, true, true, 1002, false, false, false); // focus reporting on
    const { term, sent, dispose } = setup();
    dispose();
    term.dispatchEvent(new Event("focusin"));
    term.dispatchEvent(new Event("focusout"));
    expect(sent).toEqual([]);
  });

  it("detaches every event type from the element a re-init supersedes", () => {
    // A re-mount on a new element self-heals by detaching the old one. Any event
    // type left attached there keeps reporting through the NEW handler — the
    // stale element is still in the old DOM and still receives events, so a
    // half-detach sends input from a terminal the consumer has thrown away.
    modes.setModes(true, false, true, true, 1003, false, false, false); // tracking + focus on
    const sent: string[] = [];
    const elementWithRect = (): HTMLDivElement => {
      const el = document.createElement("div");
      Object.defineProperty(el, "getBoundingClientRect", {
        value: () => ({ left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }),
        configurable: true,
      });
      return el;
    };
    const first = elementWithRect();
    const second = elementWithRect();
    // One shared `sent`, so a report from a leaked listener on `first` — which
    // would go through the SECOND handler — is still visible here.
    const handlerFor = (el: HTMLElement): MouseInputHandler => ({
      send: (data) => sent.push(data),
      cellSize: () => ({ width: 8, height: 16 }),
      termElement: () => el,
    });
    initMouse(handlerFor(first));
    initMouse(handlerFor(second)); // supersedes: `first` must be fully detached

    first.dispatchEvent(
      mouseEvent("mousedown", { clientX: 16, clientY: 32, button: 0, buttons: 0 }),
    );
    first.dispatchEvent(mouseEvent("mouseup", { clientX: 16, clientY: 32, button: 0, buttons: 0 }));
    first.dispatchEvent(mouseEvent("mousemove", { clientX: 16, clientY: 32, buttons: 1 }));
    first.dispatchEvent(wheelEvent({ deltaY: -1, clientX: 16, clientY: 32 }));
    first.dispatchEvent(new Event("focusin"));
    first.dispatchEvent(new Event("focusout"));
    expect(sent).toEqual([]);
  });
});

describe("the Shift bypass does not outlive its own gesture", () => {
  it("reports a held-button drag that follows a bypassed release", () => {
    // Shift+press hands the gesture to the browser for native selection, and
    // the release ends it. A drag after that is a new gesture and must report,
    // so the bypass flag has to be cleared by the release it belongs to.
    enableSGR(1002);
    const { term, sent } = setup();
    term.dispatchEvent(
      mouseEvent("mousedown", { clientX: 16, clientY: 32, button: 0, buttons: 1, shiftKey: true }),
    );
    term.dispatchEvent(
      mouseEvent("mouseup", { clientX: 16, clientY: 32, button: 0, buttons: 0, shiftKey: true }),
    );
    expect(sent).toEqual([]);
    term.dispatchEvent(mouseEvent("mousemove", { clientX: 16, clientY: 32, buttons: 1 }));
    expect(sent).toEqual([expectedSGR(0 + MOTION, 3, 3, false)]);
  });
});
