// @vitest-environment happy-dom
//
// Two edges of the toolbar's sticky-Ctrl state machine that toolbar.test.ts
// leaves open: the same-value write, and multi-char input under kitty
// disambiguate.
//
// setCtrlArmed is edge-triggered on purpose — a consumer wires onCtrlChange to
// its own UI, and a notification per call rather than per CHANGE makes that UI
// flicker and can loop if the consumer writes the state back. The same-value
// path still repaints, which is what normalises a scaffold whose kb-ctrl shipped
// with the wrong aria-pressed.

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { bindMobileToolbar } from "./toolbar.js";
import * as modes from "./modes.js";

/** A toolbar scaffold: the ctrl button is the only one these tests press. */
function makeToolbar(): { toolbar: HTMLElement; ctrl: HTMLElement } {
  const toolbar = document.createElement("div");
  toolbar.className = "kb-toolbar";
  const ctrl = document.createElement("button");
  ctrl.id = "kb-ctrl";
  ctrl.setAttribute("aria-pressed", "false");
  toolbar.appendChild(ctrl);
  document.body.appendChild(toolbar);
  return { toolbar, ctrl };
}

describe("bindMobileToolbar: sticky Ctrl at its edges", () => {
  beforeEach(() => {
    // All nine mode params explicit: setModes leaves omitted optionals alone and
    // the suite runs with isolate:false, so a kitty flag left by another file is
    // a real hazard.
    modes.setModes(true, false, false, false, 0, false, false, false, 0);
    document.body.innerHTML = "";
  });

  afterEach(() => {
    modes.setModes(true, false, false, false, 0, false, false, false, 0);
  });

  it("notifies only on a change, not on a same-value write", () => {
    const fixture = makeToolbar();
    const onCtrlChange = vi.fn();
    const ctrl = bindMobileToolbar({
      toolbar: fixture.toolbar,
      send: vi.fn(),
      onCtrlChange,
    });

    ctrl.setCtrlArmed(true);
    ctrl.setCtrlArmed(true);
    ctrl.setCtrlArmed(false);
    ctrl.setCtrlArmed(false);

    // Two writes each, one transition each: edge-triggered.
    expect(onCtrlChange).toHaveBeenCalledTimes(2);
    expect(onCtrlChange).toHaveBeenNthCalledWith(1, true);
    expect(onCtrlChange).toHaveBeenNthCalledWith(2, false);

    ctrl.dispose();
  });

  it("repaints the button on a same-value write, correcting a stale pressed state", () => {
    const fixture = makeToolbar();
    // A scaffold that shipped kb-ctrl marked pressed while the widget's state
    // says unarmed: assistive tech would announce a sticky Ctrl that is not
    // armed until the first real toggle.
    fixture.ctrl.setAttribute("aria-pressed", "true");
    fixture.ctrl.classList.add("armed");
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send: vi.fn() });

    ctrl.setCtrlArmed(false);

    expect(fixture.ctrl.getAttribute("aria-pressed")).toBe("false");
    expect(fixture.ctrl.classList.contains("armed")).toBe(false);

    ctrl.dispose();
  });

  it("passes a multi-char paste through verbatim under kitty disambiguate", () => {
    // Disambiguate escape codes on (kitty flags bit 0). Ctrl+char then encodes
    // as CSI-u from the FIRST codepoint, which for a paste would replace the
    // whole string with one control sequence — losing the paste and typing a
    // control character the user never pressed.
    modes.setModes(true, false, false, false, 0, false, false, false, 1);
    const fixture = makeToolbar();
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send: vi.fn() });
    ctrl.setCtrlArmed(true);

    const out = ctrl.applyStickyCtrl("hello");

    expect(out).toBe("hello");
    // The armed state is still consumed: the user asked for one Ctrl press.
    expect(ctrl.isCtrlArmed()).toBe(false);

    ctrl.dispose();
  });

  it("encodes a single char as CSI-u under kitty disambiguate", () => {
    modes.setModes(true, false, false, false, 0, false, false, false, 1);
    const fixture = makeToolbar();
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send: vi.fn() });
    ctrl.setCtrlArmed(true);

    const out = ctrl.applyStickyCtrl("s");

    // The single-char path is the one that MAY encode: 115;5u is Ctrl+s.
    expect(out).toBe("\x1b[115;5u");

    ctrl.dispose();
  });
});
