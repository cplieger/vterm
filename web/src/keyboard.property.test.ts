// SPEC-FIRST property tests for keyboard.ts.
//
// These assert xterm-encoding INVARIANTS over arbitrary modifier
// combinations, complementing the example matrix in keyboard.test.ts:
//
//   1. Modified cursor keys encode the modifier as param = 1 + bitmask
//      (Shift=1, Alt=2, Ctrl=4, Meta=8) — ctlseqs.html modifier list.
//   2. In normal (DECCKM reset) mode every cursor key is CSI-form.
//   3. A modifier always forces CSI, even under application cursor mode
//      (SS3 has no modifier encoding).
//   4. ctrlByteFor folds A-Z/a-z to the C0 range 0x01..0x1a.
//
// It also retains the paste-injection-safety property for bracketTextForPaste.
//
// Content transcribed/rephrased from invisible-island.net xterm docs for
// compliance with licensing restrictions.

import { describe, it, expect, beforeEach } from "vitest";
import fc from "fast-check";
import {
  bracketTextForPaste,
  ctrlByteFor,
  mapKeyboardEvent as mapKeyboardEventRaw,
  type KeyboardResult,
} from "./keyboard.js";
import * as modes from "./modes.js";

// These tests drive the module-singleton modes (set via modes.setModes in
// beforeEach), so bind it here; mapKeyboardEvent now takes modes explicitly.
const mapKeyboardEvent = (e: KeyboardEvent): KeyboardResult => mapKeyboardEventRaw(e, modes);

// ---------------------------------------------------------------------------
// Keyboard-encoding invariants
// ---------------------------------------------------------------------------

function ev(init: KeyboardEventInit & { key: string }): KeyboardEvent {
  return new KeyboardEvent("keydown", init);
}

function sent(result: KeyboardResult): string {
  if (result.kind !== "send") {
    throw new Error(`expected send, got ${result.kind}`);
  }
  return result.bytes;
}

interface Mods {
  shiftKey: boolean;
  altKey: boolean;
  ctrlKey: boolean;
  metaKey: boolean;
}

const modsArb: fc.Arbitrary<Mods> = fc.record({
  shiftKey: fc.boolean(),
  altKey: fc.boolean(),
  ctrlKey: fc.boolean(),
  metaKey: fc.boolean(),
});

/** The SPEC bitmask (Shift=1, Alt=2, Ctrl=4, Meta=8). */
function bitmask(m: Mods): number {
  return (m.shiftKey ? 1 : 0) + (m.altKey ? 2 : 0) + (m.ctrlKey ? 4 : 0) + (m.metaKey ? 8 : 0);
}

const ARROW_LETTER: Record<string, string> = {
  ArrowUp: "A",
  ArrowDown: "B",
  ArrowRight: "C",
  ArrowLeft: "D",
};
const arrowArb = fc.constantFrom("ArrowUp", "ArrowDown", "ArrowRight", "ArrowLeft");
const modifiedArb = modsArb.filter((m) => bitmask(m) > 0);

describe("mapKeyboardEvent: cursor-key encoding invariants (property)", () => {
  beforeEach(() => {
    // Normal (DECCKM reset), application keypad off.
    modes.setModes(true, false, false, false, 0, false);
  });

  it("modified cursor key → CSI 1;{1+bitmask}{letter}", () => {
    // Structure: ESC [ 1 ; <param> <letter>. Parsed by slicing (rather than a
    // control-char regex) so the modifier param can be checked against 1+bitmask.
    const prefix = "\x1b[1;";
    fc.assert(
      fc.property(arrowArb, modifiedArb, (key, m) => {
        const bytes = sent(mapKeyboardEvent(ev({ key, ...m })));
        expect(bytes.startsWith(prefix)).toBe(true);
        const letter = bytes.slice(-1);
        const param = bytes.slice(prefix.length, -1);
        expect(letter).toBe(ARROW_LETTER[key]);
        expect(Number(param)).toBe(1 + bitmask(m));
      }),
      { numRuns: 400 },
    );
  });

  it("every cursor key in normal mode is CSI-form (never SS3)", () => {
    fc.assert(
      fc.property(arrowArb, modsArb, (key, m) => {
        const bytes = sent(mapKeyboardEvent(ev({ key, ...m })));
        expect(bytes.startsWith("\x1b[")).toBe(true);
        expect(bytes.startsWith("\x1bO")).toBe(false);
      }),
      { numRuns: 400 },
    );
  });

  it("a modifier forces CSI even under application cursor mode (DECCKM)", () => {
    modes.setModes(true, true, false, false, 0, false); // DECCKM on
    fc.assert(
      fc.property(arrowArb, modifiedArb, (key, m) => {
        const bytes = sent(mapKeyboardEvent(ev({ key, ...m })));
        expect(bytes.startsWith("\x1b[1;")).toBe(true);
      }),
      { numRuns: 400 },
    );
  });
});

describe("ctrlByteFor: C0 folding invariant (property)", () => {
  it("A-Z / a-z fold to 0x01..0x1a", () => {
    fc.assert(
      fc.property(fc.integer({ min: 0, max: 25 }), (i) => {
        const expected = String.fromCharCode(1 + i);
        expect(ctrlByteFor(String.fromCharCode(97 + i))).toBe(expected); // lower
        expect(ctrlByteFor(String.fromCharCode(65 + i))).toBe(expected); // upper
      }),
    );
  });
});

// ---------------------------------------------------------------------------
// Paste-injection safety (retained). When bracketed-paste mode is on,
// bracketTextForPaste must sanitise every ESC byte so an attacker-controlled
// paste cannot smuggle a premature paste-end (ESC [ 201 ~) and have the
// trailing bytes interpreted as typed commands.
// ---------------------------------------------------------------------------

const OPEN = "\x1b[200~";
const CLOSE = "\x1b[201~";

const pasteText = fc
  .array(
    fc.oneof(
      fc.constantFrom("\x1b", "\x1b[201~", "\x1b[200~", "[", "201~", "x", "\n", "\t"),
      fc.string({ maxLength: 5 }),
    ),
    { maxLength: 20 },
  )
  .map((parts) => parts.join(""));

describe("bracketTextForPaste: paste-injection safety (property)", () => {
  beforeEach(() => {
    modes.setModes(true, false);
  });

  it("wraps in sentinels and the body never contains a raw ESC or a literal paste-end", () => {
    fc.assert(
      fc.property(pasteText, (text) => {
        const out = bracketTextForPaste(text);
        expect(out.startsWith(OPEN)).toBe(true);
        expect(out.endsWith(CLOSE)).toBe(true);
        const body = out.slice(OPEN.length, out.length - CLOSE.length);
        expect(body.includes("\x1b")).toBe(false);
        expect(body.includes(CLOSE)).toBe(false);
      }),
    );
  });

  it("returns text unchanged when bracketed-paste mode is off", () => {
    modes.setModes(false, false);
    fc.assert(
      fc.property(pasteText, (text) => {
        expect(bracketTextForPaste(text)).toBe(text);
      }),
    );
  });
});
