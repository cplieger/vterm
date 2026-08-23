// Observable getter-contract tests for modes.ts — the DEC private-mode state
// the server syncs via a ModesMessage. These assert that each getter
// (isBracketedPaste, isApplicationCursor, getMouseMode, ...) reflects the
// last-synced mode, that a later sync overrides an earlier one, and that a
// partial sync from an older server preserves the modes it omits.
//
// The DEC mode NUMBERS are spec: ?2004 bracketed paste, ?1 DECCKM cursor keys,
// ?1000/1002/1003 mouse tracking, ?1006 SGR mouse, ?1016 SGR-pixels mouse,
// ?1004 focus reporting, ?5 reverse video (DECSCNM), DECKPAM application
// keypad. The getters are the observable contract consumers read; we assert
// them, not the module's internal flag storage.
//
// modes.ts is module-singleton state and vitest runs with isolate:false, so
// every test sets the modes it depends on explicitly — the pristine power-on
// default cannot be observed once another test file has synced a mode.
//
// The arrow-key CSI/SS3 encoding these modes drive is covered spec-first in
// keyboard.test.ts / keyboard.property.test.ts and is not duplicated here.

import { describe, it, expect, beforeEach } from "vitest";
import { bracketTextForPaste } from "./keyboard.js";
import * as modes from "./modes.js";

// Read every getter into one plain object so a full sync can be asserted with a
// single toEqual (full diff on failure, no assertion-roulette).
interface ModeSnapshot {
  bracketed: boolean;
  appCursor: boolean;
  mouseSGR: boolean;
  focus: boolean;
  mouseMode: number;
  appKeypad: boolean;
  reverse: boolean;
  pixels: boolean;
}

function readModes(): ModeSnapshot {
  return {
    bracketed: modes.isBracketedPaste(),
    appCursor: modes.isApplicationCursor(),
    mouseSGR: modes.isMouseSGR(),
    focus: modes.isFocusReporting(),
    mouseMode: modes.getMouseMode(),
    appKeypad: modes.isApplicationKeypad(),
    reverse: modes.isReverseVideo(),
    pixels: modes.isMousePixels(),
  };
}

beforeEach(() => {
  // Known baseline before each test (shared module singleton under isolate:false).
  modes.setModes(true, false, false, false, 0, false, false, false);
});

describe("modes: getters mirror the DEC private-mode state synced from the server", () => {
  it("each getter returns its own synced field (mixed snapshot)", () => {
    // Mixed values so a getter that reads a neighbouring flag is caught:
    // ?2004 on, DECCKM off, ?1006 on, ?1004 off, mouse=button-event(1002),
    // DECKPAM off, ?5 on, ?1016 off.
    modes.setModes(true, false, true, false, 1002, false, true, false);
    expect(readModes()).toEqual({
      bracketed: true,
      appCursor: false,
      mouseSGR: true,
      focus: false,
      mouseMode: 1002,
      appKeypad: false,
      reverse: true,
      pixels: false,
    });
  });

  it("a later sync overrides the earlier one (getters track the latest ModesMessage)", () => {
    modes.setModes(true, true, true, true, 1003, true, true, true);
    modes.setModes(false, false, false, false, 0, false, false, false);
    expect(readModes()).toEqual({
      bracketed: false,
      appCursor: false,
      mouseSGR: false,
      focus: false,
      mouseMode: 0,
      appKeypad: false,
      reverse: false,
      pixels: false,
    });
  });
});

describe("modes: getMouseMode reports the synced mouse-tracking mode", () => {
  // Spec mouse-tracking modes: 0 = off, ?1000 normal, ?1002 button-event,
  // ?1003 any-event.
  for (const mode of [0, 1000, 1002, 1003]) {
    it(`reflects mouse tracking mode ${mode}`, () => {
      modes.setModes(true, false, false, false, mode);
      expect(modes.getMouseMode()).toBe(mode);
    });
  }
});

describe("modes: a partial sync preserves omitted optional modes (older-server back-compat)", () => {
  it("updates the two required fields but leaves omitted optional modes untouched", () => {
    // Full sync turns every optional mode on.
    modes.setModes(true, true, true, true, 1003, true, true, true);
    // An older server build sends only the two required fields. setModes must
    // update those two but NOT reset the optional modes it does not carry.
    modes.setModes(false, false);
    expect(readModes()).toEqual({
      bracketed: false, // required field: updated
      appCursor: false, // required field: updated
      mouseSGR: true, // optional, omitted -> preserved
      focus: true,
      mouseMode: 1003,
      appKeypad: true,
      reverse: true,
      pixels: true,
    });
  });
});

describe("modes: isBracketedPaste gates the observable paste-bracketing behavior", () => {
  // The getter's downstream effect through its consumer (keyboard.ts). DEC
  // ?2004 on -> paste wrapped in ESC[200~..ESC[201~ with any embedded ESC
  // sanitised; off -> the helper is a pure pass-through.
  it("wraps and sanitises paste text when bracketed paste is enabled", () => {
    modes.setModes(true, false); // ?2004 on
    expect(modes.isBracketedPaste()).toBe(true);
    expect(bracketTextForPaste("a\x1b[201~b")).toBe(`\x1b[200~a\u241B[201~b\x1b[201~`);
  });

  it("passes paste text through unchanged when bracketed paste is disabled", () => {
    modes.setModes(false, false); // ?2004 off
    expect(modes.isBracketedPaste()).toBe(false);
    // No sentinels, and the embedded ESC is left intact (sanitising only
    // happens while bracketing).
    expect(bracketTextForPaste("a\x1b[201~b")).toBe("a\x1b[201~b");
  });
});
