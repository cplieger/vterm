// @vitest-environment happy-dom

// The four decisions in mapKeyboardEvent that no scenario in the keyboard
// suites reaches from the side that distinguishes them.
//
// Three are precedence questions the existing tables cannot ask, because each
// table walks ONE key with ONE modifier: which rule wins when Ctrl and Alt are
// held together over Space, what happens to Alt over a key with no legacy
// encoding at all, and what the kitty encoder does with a key that is not a
// character. The fourth is the encoder's floor: the codepoint 0 that the kitty
// grammar has no room for.
//
// Sources: xterm's ctlseqs (Ctrl+Space is NUL, Alt+<char> is the ESC prefix)
// and the kitty keyboard protocol's key-codes section
// (https://sw.kovidgoyal.net/kitty/keyboard-protocol/), which defines
// unicode-key-code as the codepoint of the key's unshifted value — a value that
// only exists for keys that produce text.

import { describe, it, expect, beforeEach } from "vitest";

import { mapKeyboardEvent, type KeyboardResult } from "./keyboard.js";
import * as modes from "./modes.js";

const ESC = "\x1b";
const KITTY_DISAMBIGUATE = 1;

function ev(init: KeyboardEventInit & { key: string; code?: string }): KeyboardEvent {
  return new KeyboardEvent("keydown", init);
}

/** Map under the LEGACY encodings (no kitty flag). */
function legacy(init: KeyboardEventInit & { key: string; code?: string }): KeyboardResult {
  modes.setModes(true, false, false, false, 0, false, false, false, 0);
  return mapKeyboardEvent(ev(init), modes);
}

/** Map under the kitty disambiguate flag, via the injected-modes seam. */
function underKitty(init: KeyboardEventInit & { key: string; code?: string }): KeyboardResult {
  modes.setModes(true, false, false, false, 0, false, false, false, KITTY_DISAMBIGUATE);
  return mapKeyboardEvent(ev(init), modes);
}

beforeEach(() => {
  modes.setModes(true, false, false, false, 0, false, false, false, 0);
});

describe("legacy encoding: Ctrl and Alt held together", () => {
  it("sends NUL for Ctrl+Alt+Space, the Ctrl rule winning over the Alt prefix", () => {
    // Space has its own rule ahead of the generic printable path, and that rule
    // reads Ctrl alone: xterm sends NUL for Ctrl+Space whatever else is held
    // (ctlseqs, "PC-Style Function Keys"). The generic printable path is the
    // opposite — its Ctrl arm requires Alt to be absent — so Space's precedence
    // is only visible with both modifiers down, and dropping the Space rule
    // silently turns this into an ignored keypress.
    expect(legacy({ key: " ", code: "Space", ctrlKey: true, altKey: true })).toEqual({
      kind: "send",
      bytes: "\x00",
    });
  });

  it("keeps the single-modifier Space rules: Ctrl+Space NUL, Alt+Space ESC SP", () => {
    // The neighbours of the case above, so a change to the precedence shows up
    // as a difference between these three and not as a silent re-routing.
    expect(legacy({ key: " ", code: "Space", ctrlKey: true })).toEqual({
      kind: "send",
      bytes: "\x00",
    });
    expect(legacy({ key: " ", code: "Space", altKey: true })).toEqual({
      kind: "send",
      bytes: `${ESC} `,
    });
    expect(legacy({ key: " ", code: "Space" })).toEqual({ kind: "ignore" });
  });
});

describe("legacy encoding: Alt over a key with no legacy encoding", () => {
  it("ignores Alt+F21 rather than sending ESC followed by the key NAME", () => {
    // F21-F24 have no legacy encoding (xterm stops at F20), so they fall past
    // every table to the deferred-to-`input` return. The Alt-prefix rule is
    // gated on a SINGLE-character key for exactly this reason: without the
    // gate the prefix would splice the key's name into the stream and the PTY
    // would receive ESC F 2 1 — three stray printable bytes.
    expect(legacy({ key: "F21", code: "F21", altKey: true })).toEqual({ kind: "ignore" });
  });

  it("ignores Alt+Dead and Alt over a media key on the same rule", () => {
    // The other two shapes that reach the same fall-through: a dead key
    // mid-composition (the browser reports the name "Dead", and the composed
    // character arrives later on `input`) and a key that is not text at all.
    expect(legacy({ key: "Dead", code: "Backquote", altKey: true })).toEqual({ kind: "ignore" });
    expect(legacy({ key: "AudioVolumeUp", code: "AudioVolumeUp", altKey: true })).toEqual({
      kind: "ignore",
    });
  });
});

describe("the modifier-only preamble runs before the application-keypad rule", () => {
  // The keypad rule keys off ev.code (the PHYSICAL key) while the modifier-only
  // rule keys off ev.key (what the key currently does), and the two disagree on
  // a remapped keyboard: an xkb layout or a QMK firmware that maps the KP0
  // scancode to Shift_L makes a browser report code "Numpad0" with key "Shift".
  // The press produced no character, so under DECKPAM it must stay silent —
  // encoding it as the keypad's SS3 sequence would send ESC O p for a shift.
  // The ordering of the preamble is what decides that, and it is deliberate
  // (mapKeyboardEvent's own comment: the shared preamble runs first so both the
  // legacy and the kitty path behave identically).
  function underKeypad(init: KeyboardEventInit & { key: string; code?: string }): KeyboardResult {
    modes.setModes(true, false, false, false, 0, true, false, false, 0);
    return mapKeyboardEvent(ev(init), modes);
  }

  it("ignores a modifier press whose physical code is a keypad key", () => {
    expect(underKeypad({ key: "Shift", code: "Numpad0", shiftKey: true })).toEqual({
      kind: "ignore",
    });
    // The other three, with the modifier state a real keydown of each reports.
    expect(underKeypad({ key: "Control", code: "Numpad0", ctrlKey: true })).toEqual({
      kind: "ignore",
    });
    expect(underKeypad({ key: "Alt", code: "Numpad0", altKey: true })).toEqual({ kind: "ignore" });
    expect(underKeypad({ key: "Meta", code: "Numpad0", metaKey: true })).toEqual({
      kind: "ignore",
    });
  });

  it("still SS3-encodes the same physical key when it acts as the keypad zero", () => {
    // The control: same code, same mode, and the only difference is that the key
    // does what its label says. VT100 User Guide table 3-8: keypad 0 is ESC O p.
    expect(underKeypad({ key: "0", code: "Numpad0" })).toEqual({
      kind: "send",
      bytes: `${ESC}Op`,
    });
  });
});

describe("kitty disambiguate: keys that are not characters", () => {
  it("ignores Alt+Dead instead of reporting the physical key under it", () => {
    // A dead key carries a physical code with a perfectly good unshifted
    // codepoint (Backquote -> 96), so the codepoint derivation would happily
    // encode CSI 96;3u here. It must not: the key produced no character, the
    // composition it started will arrive on `input`, and reporting the base key
    // would send the app a backtick the user never typed.
    expect(underKitty({ key: "Dead", code: "Backquote", altKey: true })).toEqual({
      kind: "ignore",
    });
  });

  it("still reports Alt+` when the key IS the character", () => {
    // The control for the case above: same physical key, same modifier, and the
    // only difference is that this press produced a character. 3 = 1 + Alt(2).
    expect(underKitty({ key: "`", code: "Backquote", altKey: true })).toEqual({
      kind: "send",
      bytes: `${ESC}[96;3u`,
    });
  });

  it("never encodes a zero unicode-key-code", () => {
    // An event whose key is a single NUL character (and whose code the browser
    // did not report) leaves the codepoint derivation with nothing: it answers
    // 0, which the kitty grammar has no room for — `CSI 0 u` names no key, and
    // a decoder that reads it gets a key event for U+0000. The guard is what
    // keeps the encoder from emitting one, so the press is dropped instead.
    expect(underKitty({ key: "\u0000", ctrlKey: true })).toEqual({ kind: "ignore" });
  });
});
