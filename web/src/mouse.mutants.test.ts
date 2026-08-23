// The mouse module's two DOM-contract obligations, which no behavioural test can
// see: the wheel listener's passive flag and the disposer's detach. Neither is
// observable from the outcome of a dispatched event — a passive listener's
// preventDefault is ignored with only a console warning, and a leaked listener
// looks identical until something else is listening too.
//
// Both are asserted at the registration, and both are stated that way on
// purpose. `passive: false` is what makes the unconditional preventDefault in
// the wheel handler legal — a passive listener's preventDefault is ignored with a
// console warning, and the page scrolls behind the terminal, which is the exact
// regression the handler's own comment says it exists to prevent (xterm.js does
// the same: swallow the gesture even when there is nothing to report). And
// `detach` is the disposer's stated contract: the module-level `handler = null`
// already stops every report, so a dropped detach leaves six listeners on an
// element the consumer believes it has released, with no report anywhere to show
// it.

import { describe, it, expect, beforeEach, vi } from "vitest";

import { init as initMouse, type MouseInputHandler } from "./mouse.js";
import * as modes from "./modes.js";

/** Every listener init() attaches, in the order the module attaches them. */
const ATTACHED = ["mousedown", "mouseup", "mousemove", "wheel", "focusin", "focusout"] as const;

beforeEach(() => {
  modes.setModes(true, false, false, false, 0, false, false, false);
});

function setup(): { term: HTMLDivElement; sent: string[]; handler: MouseInputHandler } {
  const term = document.createElement("div");
  const sent: string[] = [];
  return {
    term,
    sent,
    handler: {
      send: (data) => sent.push(data),
      cellSize: () => ({ width: 8, height: 16 }),
      termElement: () => term,
    },
  };
}

describe("mouse: the wheel listener's passive flag", () => {
  it("registers the wheel listener as NON-passive so preventDefault holds", () => {
    const { term, handler } = setup();
    const spy = vi.spyOn(term, "addEventListener");

    const dispose = initMouse(handler);

    expect(spy).toHaveBeenCalledWith("wheel", expect.any(Function), { passive: false });
    dispose();
    spy.mockRestore();
  });

  it("registers the other five listeners with no options at all", () => {
    // The control for the case above: the flag is on the wheel listener
    // specifically, because the wheel is the only gesture this module cancels.
    const { term, handler } = setup();
    const spy = vi.spyOn(term, "addEventListener");

    const dispose = initMouse(handler);

    for (const type of ATTACHED) {
      if (type === "wheel") {
        continue;
      }
      expect(spy).toHaveBeenCalledWith(type, expect.any(Function));
    }
    dispose();
    spy.mockRestore();
  });
});

describe("mouse: the disposer's detach", () => {
  it("removes every listener it attached from the element", () => {
    // Silence is not detachment. The module nulls its handler on dispose, so
    // every listener that stays behind reports nothing and looks correct; what
    // it does instead is keep the element (and the closure holding the previous
    // consumer) alive for as long as the node is referenced.
    const { term, handler } = setup();
    const removed = vi.spyOn(term, "removeEventListener");

    const dispose = initMouse(handler);
    dispose();

    for (const type of ATTACHED) {
      expect(removed).toHaveBeenCalledWith(type, expect.any(Function));
    }
    expect(removed).toHaveBeenCalledTimes(ATTACHED.length);
    removed.mockRestore();
  });

  it("detaches nothing twice when the disposer is called again", () => {
    // The disposer is documented idempotent, and detach() is what has to be
    // idempotent for that to hold.
    const { term, handler } = setup();
    const dispose = initMouse(handler);
    const removed = vi.spyOn(term, "removeEventListener");

    dispose();
    dispose();

    expect(removed).toHaveBeenCalledTimes(ATTACHED.length);
    removed.mockRestore();
  });
});
