// mapKeyboardEvent takes its mode state explicitly (design section 8), so a
// tabbed shell can map a keystroke against the active tab's modes rather than a
// process-global singleton. These tests pass ad-hoc KeyboardModes objects and
// assert the mapping follows the argument, independent of any module state.

import { describe, it, expect } from "vitest";
import { mapKeyboardEvent, type KeyboardModes, type KeyboardResult } from "./keyboard.js";

function ev(init: KeyboardEventInit & { key: string; code?: string }): KeyboardEvent {
  return new KeyboardEvent("keydown", init);
}
function sent(result: KeyboardResult): string {
  if (result.kind !== "send") {
    throw new Error(`expected send, got ${result.kind}`);
  }
  return result.bytes;
}
const normal: KeyboardModes = {
  isApplicationCursor: () => false,
  isApplicationKeypad: () => false,
  getKeyboardFlags: () => 0,
};
const appCursor: KeyboardModes = {
  isApplicationCursor: () => true,
  isApplicationKeypad: () => false,
  getKeyboardFlags: () => 0,
};
const appKeypad: KeyboardModes = {
  isApplicationCursor: () => false,
  isApplicationKeypad: () => true,
  getKeyboardFlags: () => 0,
};
// Kitty disambiguate flag (0x1) active.
const kitty: KeyboardModes = {
  isApplicationCursor: () => false,
  isApplicationKeypad: () => false,
  getKeyboardFlags: () => 1,
};

describe("mapKeyboardEvent honors the passed modes", () => {
  it("arrow keys use CSI form under the normal-modes argument", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "ArrowUp" }), normal))).toBe("\x1b[A");
  });

  it("arrow keys switch to SS3 form under the application-cursor argument", () => {
    // Same event, different modes object -> different encoding. This is the
    // per-tab behavior: the argument decides, not any shared state.
    expect(sent(mapKeyboardEvent(ev({ key: "ArrowUp" }), appCursor))).toBe("\x1bOA");
  });

  it("numpad keys stay bare under normal modes but emit SS3 under application keypad", () => {
    expect(mapKeyboardEvent(ev({ key: "1", code: "Numpad1" }), normal).kind).toBe("ignore");
    expect(sent(mapKeyboardEvent(ev({ key: "1", code: "Numpad1" }), appKeypad))).toBe("\x1bOq");
  });

  it("two modes objects passed in sequence do not bleed into each other", () => {
    // A tabbed shell alternates modes objects on switch; each call is isolated.
    expect(sent(mapKeyboardEvent(ev({ key: "ArrowLeft" }), appCursor))).toBe("\x1bOD");
    expect(sent(mapKeyboardEvent(ev({ key: "ArrowLeft" }), normal))).toBe("\x1b[D");
    expect(sent(mapKeyboardEvent(ev({ key: "ArrowLeft" }), appCursor))).toBe("\x1bOD");
  });
});

describe("mapKeyboardEvent reads the kitty disambiguate flag from the passed modes", () => {
  // The COMPREHENSIVE, spec-first kitty encoding conformance lives in
  // kitty-encoder.test.ts (driven by test-helpers/kitty-spec-vectors.ts). Here we
  // only assert that the flag is taken from the modes ARGUMENT — the same per-tab
  // property this file exists to guard for the other modes.
  it("the same Escape event encodes per the flag in the passed modes object", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "Escape" }), kitty))).toBe("\x1b[27u");
    expect(sent(mapKeyboardEvent(ev({ key: "Escape" }), normal))).toBe("\x1b");
  });

  it("two modes objects with different flags do not bleed into each other", () => {
    const ctrlI = (): KeyboardEvent => ev({ key: "i", code: "KeyI", ctrlKey: true });
    expect(sent(mapKeyboardEvent(ctrlI(), kitty))).toBe("\x1b[105;5u");
    expect(sent(mapKeyboardEvent(ctrlI(), normal))).toBe("\x09");
    expect(sent(mapKeyboardEvent(ctrlI(), kitty))).toBe("\x1b[105;5u");
  });
});
