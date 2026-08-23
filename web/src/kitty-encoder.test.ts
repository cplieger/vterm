// Spec-FIRST conformance for the kitty keyboard disambiguate encoder. The
// expectations come from test-helpers/kitty-spec-vectors.ts, which is
// transcribed from the official spec's normative tables — NOT from this
// encoder's behavior — so a deviation from the spec fails here rather than being
// blessed by a test written against the implementation. Each `it` is named after
// the spec rule it enforces.
import { describe, it, expect } from "vitest";
import { mapKeyboardEvent, type KeyboardModes } from "./keyboard.js";
import { KITTY_SPEC_VECTORS, type KittyVector } from "./test-helpers/kitty-spec-vectors.js";

function modesWith(kbdFlags: number): KeyboardModes {
  return {
    isApplicationCursor: () => false,
    isApplicationKeypad: () => false,
    getKeyboardFlags: () => kbdFlags,
  };
}
const kittyModes = modesWith(1); // disambiguate flag active
const legacyModes = modesWith(0); // protocol disabled

function eventFor(v: KittyVector): KeyboardEvent {
  const init: KeyboardEventInit = {
    key: v.key,
    shiftKey: v.shift ?? false,
    ctrlKey: v.ctrl ?? false,
    altKey: v.alt ?? false,
    metaKey: v.meta ?? false,
  };
  if (v.code !== undefined) {
    init.code = v.code;
  }
  return new KeyboardEvent("keydown", init);
}

describe("kitty disambiguate encoding is spec-conformant (flag 0x1 active)", () => {
  for (const v of KITTY_SPEC_VECTORS) {
    it(v.spec, () => {
      expect(mapKeyboardEvent(eventFor(v), kittyModes)).toEqual(v.kitty);
    });
  }
});

describe("legacy encoding is unchanged with the flag OFF (regression)", () => {
  for (const v of KITTY_SPEC_VECTORS) {
    if (!v.legacy) {
      continue;
    }
    it(v.spec, () => {
      expect(mapKeyboardEvent(eventFor(v), legacyModes)).toEqual(v.legacy);
    });
  }
});
