// SPEC-FIRST keyboard input-encoding tests for keyboard.ts.
//
// Every expected byte sequence in this file is derived from the xterm
// PC-style keyboard-encoding SPECIFICATION and transcribed from the two
// authoritative references, NOT from reading keyboard.ts's implementation:
//
//   - Control sequences (cursor keys, Home/End, application keypad):
//     https://invisible-island.net/xterm/ctlseqs/ctlseqs.html
//     ("PC-Style Function Keys" and "VT220-Style Function Keys" tables)
//   - Per-key / per-modifier sequences (the xterm-new terminfo column):
//     https://invisible-island.net/xterm/xterm-function-keys.html
//
// Modifier parameter = 1 + bitmask, bitmask = 1:Shift 2:Alt 4:Ctrl 8:Meta
// (ctlseqs.html: shift-F5 => CSI 15;2~, params 2-8, Meta extends to 9-16).
// So Shift=2, Alt=3, Ctrl=5, Ctrl+Shift=6 — matching the function-keys
// table columns kXX (=2), kXX3 (=3), kXX5 (=5), kXX6 (=6).
//
// A failing assertion here is a real finding: a deviation of our encoder
// from the xterm spec. Deviations are NOT "fixed" by editing the expected
// value or the source; they are recorded in the "spec deviations" block
// via it.skip and enumerated in the accompanying report.
//
// Content transcribed/rephrased from invisible-island.net xterm docs for
// compliance with licensing restrictions.

import { describe, it, expect, beforeEach } from "vitest";
import {
  bracketTextForPaste,
  ctrlByteFor,
  kittyCtrlCharSeq,
  mapKeyboardEvent as mapKeyboardEventRaw,
  prepareTextForTerminal,
  type KeyboardResult,
} from "./keyboard.js";
import * as modes from "./modes.js";

// These tests drive the module-singleton modes (set via modes.setModes in
// beforeEach), so bind it here; mapKeyboardEvent now takes modes explicitly.
const mapKeyboardEvent = (e: KeyboardEvent): KeyboardResult => mapKeyboardEventRaw(e, modes);

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function ev(init: KeyboardEventInit & { key: string; code?: string }): KeyboardEvent {
  return new KeyboardEvent("keydown", init);
}

/** Assert the result is a `send` and return its bytes (throws otherwise). */
function sent(result: KeyboardResult): string {
  if (result.kind !== "send") {
    throw new Error(`expected send, got ${result.kind}`);
  }
  return result.bytes;
}

/** The five modifier states in the spec matrix. */
type Mod = "none" | "shift" | "ctrl" | "alt" | "ctrlShift";

/** Browser KeyboardEvent modifier flags for each matrix column. */
const MOD_INIT: Record<Mod, KeyboardEventInit> = {
  none: {},
  shift: { shiftKey: true },
  ctrl: { ctrlKey: true },
  alt: { altKey: true },
  ctrlShift: { ctrlKey: true, shiftKey: true },
};

// xterm modifier parameter (1 + bitmask), transcribed from ctlseqs.html's
// modifier list and the function-keys table columns.
const MOD_PARAM: Record<Exclude<Mod, "none">, number> = {
  shift: 2, // 1 + 1(Shift)
  alt: 3, // 1 + 2(Alt)
  ctrl: 5, // 1 + 4(Ctrl)
  ctrlShift: 6, // 1 + 1(Shift) + 4(Ctrl)
};

const ALL_MODS: Mod[] = ["none", "shift", "ctrl", "alt", "ctrlShift"];
// PageUp/PageDown reserve Shift for local scrollback (see deviations block).
const NON_SHIFT_MODS: Mod[] = ["none", "ctrl", "alt", "ctrlShift"];

// -- Spec sequence templates (parameterised by the transcribed MOD_PARAM) ----

/** Cursor / Home / End in NORMAL mode: bare `CSI L`, modified `CSI 1;{p}L`. */
function letterFormNormal(letter: string, mod: Mod): string {
  return mod === "none" ? `\x1b[${letter}` : `\x1b[1;${MOD_PARAM[mod]}${letter}`;
}

/** Cursor / Home / End in APPLICATION cursor mode: bare `SS3 L`, modified stays `CSI 1;{p}L`. */
function letterFormApp(letter: string, mod: Mod): string {
  return mod === "none" ? `\x1bO${letter}` : `\x1b[1;${MOD_PARAM[mod]}${letter}`;
}

/** F1-F4: bare `SS3 L`, modified `CSI 1;{p}L` (PC-style function keys). */
function ss3FunctionKey(letter: string, mod: Mod): string {
  return mod === "none" ? `\x1bO${letter}` : `\x1b[1;${MOD_PARAM[mod]}${letter}`;
}

/** Editing keypad / F5-F12: bare `CSI n~`, modified `CSI n;{p}~`. */
function tildeForm(num: number, mod: Mod): string {
  return mod === "none" ? `\x1b[${num}~` : `\x1b[${num};${MOD_PARAM[mod]}~`;
}

beforeEach(() => {
  // modes.ts is module-singleton state shared across test files
  // (vitest isolate:false). Reset to the VT100 power-on default:
  // bracketed-paste on, cursor keys NORMAL, application keypad OFF.
  modes.setModes(true /* bracketed */, false /* app cursor */, false, false, 0, false);
});

// ===========================================================================
// Cursor keys + Home/End — NORMAL mode (DECCKM reset)
// Spec (ctlseqs): Up/Down/Right/Left = CSI A/B/C/D; Home = CSI H; End = CSI F.
// Modified forms from function-keys table: kUP=\E[1;2A, kUP5=\E[1;5A, etc.
// ===========================================================================

describe("cursor & Home/End keys — normal mode [spec: CSI form]", () => {
  const letterKeys: { key: string; letter: string }[] = [
    { key: "ArrowUp", letter: "A" },
    { key: "ArrowDown", letter: "B" },
    { key: "ArrowRight", letter: "C" },
    { key: "ArrowLeft", letter: "D" },
    { key: "Home", letter: "H" },
    { key: "End", letter: "F" },
  ];

  for (const { key, letter } of letterKeys) {
    for (const mod of ALL_MODS) {
      it(`${key} + ${mod} → ${JSON.stringify(letterFormNormal(letter, mod))}`, () => {
        expect(sent(mapKeyboardEvent(ev({ key, ...MOD_INIT[mod] })))).toBe(
          letterFormNormal(letter, mod),
        );
      });
    }
  }
});

// ===========================================================================
// Cursor keys + Home/End — APPLICATION cursor mode (DECCKM set)
// Spec (ctlseqs): bare form switches to SS3 (ESC O L); modified forms keep
// the CSI 1;{p}L form (SS3 has no modifier encoding).
// ===========================================================================

describe("cursor & Home/End keys — application cursor mode / DECCKM [spec: SS3 bare form]", () => {
  beforeEach(() => {
    modes.setModes(true /* bracketed */, true /* app cursor */, false, false, 0, false);
  });

  const letterKeys: { key: string; letter: string }[] = [
    { key: "ArrowUp", letter: "A" },
    { key: "ArrowDown", letter: "B" },
    { key: "ArrowRight", letter: "C" },
    { key: "ArrowLeft", letter: "D" },
    { key: "Home", letter: "H" },
    { key: "End", letter: "F" },
  ];

  for (const { key, letter } of letterKeys) {
    it(`${key} (no mod) → SS3 ${letter}`, () => {
      expect(sent(mapKeyboardEvent(ev({ key })))).toBe(letterFormApp(letter, "none"));
    });
  }

  // With any modifier the encoding must stay CSI even under DECCKM.
  for (const { key, letter } of letterKeys) {
    for (const mod of ["shift", "ctrl", "alt", "ctrlShift"] as Mod[]) {
      it(`${key} + ${mod} stays CSI under DECCKM → ${JSON.stringify(letterFormApp(letter, mod))}`, () => {
        expect(sent(mapKeyboardEvent(ev({ key, ...MOD_INIT[mod] })))).toBe(
          letterFormApp(letter, mod),
        );
      });
    }
  }
});

// ===========================================================================
// Editing keypad — Insert / Delete / PageUp / PageDown
// Spec (function-keys table): Insert=CSI 2~, Delete=CSI 3~, PageUp=CSI 5~,
// PageDown=CSI 6~; modified => CSI n;{p}~ (kIC=\E[2;2~, kDC5=\E[3;5~, ...).
// PageUp/PageDown reserve Shift for local scrollback — see deviations block.
// ===========================================================================

describe("editing keypad — Insert/Delete/PageUp/PageDown [spec: CSI n~ tilde form]", () => {
  const tildeKeys: { key: string; num: number; mods: Mod[] }[] = [
    { key: "Insert", num: 2, mods: ALL_MODS },
    { key: "Delete", num: 3, mods: ALL_MODS },
    { key: "PageUp", num: 5, mods: NON_SHIFT_MODS },
    { key: "PageDown", num: 6, mods: NON_SHIFT_MODS },
  ];

  for (const { key, num, mods } of tildeKeys) {
    for (const mod of mods) {
      it(`${key} + ${mod} → ${JSON.stringify(tildeForm(num, mod))}`, () => {
        expect(sent(mapKeyboardEvent(ev({ key, ...MOD_INIT[mod] })))).toBe(tildeForm(num, mod));
      });
    }
  }
});

// ===========================================================================
// Function keys F1-F4 — SS3 bare form, CSI 1;{p}L modified
// Spec (function-keys table, xterm-new): kf1=\EOP..kf4=\EOS;
// kf13(shift)=\E[1;2P, kf25(ctrl)=\E[1;5P, kf37(ctrl+shift)=\E[1;6P,
// kf49(alt)=\E[1;3P.
// ===========================================================================

describe("function keys F1-F4 [spec: SS3 P/Q/R/S bare, CSI 1;{p} modified]", () => {
  const fnKeys: { key: string; letter: string }[] = [
    { key: "F1", letter: "P" },
    { key: "F2", letter: "Q" },
    { key: "F3", letter: "R" },
    { key: "F4", letter: "S" },
  ];

  for (const { key, letter } of fnKeys) {
    for (const mod of ALL_MODS) {
      it(`${key} + ${mod} → ${JSON.stringify(ss3FunctionKey(letter, mod))}`, () => {
        expect(sent(mapKeyboardEvent(ev({ key, ...MOD_INIT[mod] })))).toBe(
          ss3FunctionKey(letter, mod),
        );
      });
    }
  }
});

// ===========================================================================
// Function keys F5-F12 — CSI tilde form
// Spec (function-keys table, xterm-new): kf5=\E[15~, kf6=\E[17~, kf7=\E[18~,
// kf8=\E[19~, kf9=\E[20~, kf10=\E[21~, kf11=\E[23~, kf12=\E[24~;
// modified => CSI n;{p}~ (kf17 shift-F5 = \E[15;2~, kf29 ctrl-F5 = \E[15;5~).
// ===========================================================================

describe("function keys F5-F12 [spec: CSI {15,17,18,19,20,21,23,24}~]", () => {
  const fnKeys: { key: string; num: number }[] = [
    { key: "F5", num: 15 },
    { key: "F6", num: 17 },
    { key: "F7", num: 18 },
    { key: "F8", num: 19 },
    { key: "F9", num: 20 },
    { key: "F10", num: 21 },
    { key: "F11", num: 23 },
    { key: "F12", num: 24 },
  ];

  for (const { key, num } of fnKeys) {
    for (const mod of ALL_MODS) {
      it(`${key} + ${mod} → ${JSON.stringify(tildeForm(num, mod))}`, () => {
        expect(sent(mapKeyboardEvent(ev({ key, ...MOD_INIT[mod] })))).toBe(tildeForm(num, mod));
      });
    }
  }
});

// ===========================================================================
// Function keys F13-F20 — xterm's extended tilde codes (25-34; 27 and 30
// skipped historically). F21-F24 have NO standard legacy encoding — xterm
// itself stops at F20 — so they must stay silent on the legacy path (the
// kitty path covers all twelve with dedicated codepoints; see the kitty
// suite). xterm.js supports none of F13+ (xtermjs/xterm.js#1426).
// ===========================================================================

describe("function keys F13-F20 [spec: CSI {25,26,28,29,31,32,33,34}~] and F21-F24 silence", () => {
  const fnKeys: { key: string; num: number }[] = [
    { key: "F13", num: 25 },
    { key: "F14", num: 26 },
    { key: "F15", num: 28 },
    { key: "F16", num: 29 },
    { key: "F17", num: 31 },
    { key: "F18", num: 32 },
    { key: "F19", num: 33 },
    { key: "F20", num: 34 },
  ];

  for (const { key, num } of fnKeys) {
    for (const mod of ALL_MODS) {
      it(`${key} + ${mod} → ${JSON.stringify(tildeForm(num, mod))}`, () => {
        expect(sent(mapKeyboardEvent(ev({ key, ...MOD_INIT[mod] })))).toBe(tildeForm(num, mod));
      });
    }
  }

  for (const key of ["F21", "F22", "F23", "F24"]) {
    it(`${key} → ignore (no standard legacy encoding; kitty-only)`, () => {
      expect(mapKeyboardEvent(ev({ key }))).toEqual({ kind: "ignore" });
    });
  }
});

// ===========================================================================
// Tab / Enter / Escape / Backspace / Space
// ===========================================================================

describe("Tab and Shift+Tab [spec: HT and CSI Z]", () => {
  it("Tab → HT (0x09)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "Tab" })))).toBe("\x09");
  });
  it("Shift+Tab → CSI Z (back-tab, kcbt=\\E[Z)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "Tab", shiftKey: true })))).toBe("\x1b[Z");
  });
});

describe("Enter [spec: CR; Alt prefixes ESC (metaSendsEscape)]", () => {
  it("Enter → CR (0x0d)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "Enter" })))).toBe("\x0d");
  });
  it("Alt+Enter → ESC CR", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "Enter", altKey: true })))).toBe("\x1b\x0d");
  });
});

describe("Escape [spec: ESC; Alt prefixes ESC]", () => {
  it("Escape → ESC (0x1b)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "Escape" })))).toBe("\x1b");
  });
  it("Alt+Escape → ESC ESC", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "Escape", altKey: true })))).toBe("\x1b\x1b");
  });
});

describe("Backspace [spec: xterm.js parity — DEL default, Ctrl→BS, Alt prefixes ESC]", () => {
  // Reference: xterm.js evaluateKeyboardEvent case 8 (verified against
  // src/common/input/Keyboard.ts): key = ev.ctrlKey ? '\b'(BS) : DEL(^?);
  // then, if ev.altKey, prefix ESC. Shift is not consulted. The given spec's
  // "Backspace → DEL" is the unmodified backarrow default (backarrowKey off).
  it("Backspace → DEL (0x7f) — xterm-new kbs = ^?", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "Backspace" })))).toBe("\x7f");
  });
  it("Shift+Backspace → DEL (Shift not consulted)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "Backspace", shiftKey: true })))).toBe("\x7f");
  });
  it("Ctrl+Backspace → BS / ^H (0x08)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "Backspace", ctrlKey: true })))).toBe("\x08");
  });
  it("Alt+Backspace → ESC DEL (meta prefix over the DEL byte)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "Backspace", altKey: true })))).toBe("\x1b\x7f");
  });
});

describe("Space [spec: Ctrl→NUL; Alt→ESC SP]", () => {
  it("Ctrl+Space → NUL (0x00)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: " ", ctrlKey: true })))).toBe("\x00");
  });
  it("Alt+Space → ESC SP", () => {
    expect(sent(mapKeyboardEvent(ev({ key: " ", altKey: true })))).toBe("\x1b ");
  });
});

// ===========================================================================
// Ctrl + printable → C0 control bytes (via mapKeyboardEvent)
// Spec: Ctrl+A..Z = 0x01..0x1a; Ctrl+@/Space = NUL; Ctrl+[ \ ] ^ _ =
// 0x1b..0x1f; Ctrl+? = DEL. (US-ASCII control-key convention.)
// ===========================================================================

describe("Ctrl+printable → C0 controls [spec: ASCII control convention]", () => {
  const letterCases: { key: string; byte: number }[] = [
    { key: "a", byte: 0x01 },
    { key: "A", byte: 0x01 }, // case-folded
    { key: "c", byte: 0x03 },
    { key: "m", byte: 0x0d },
    { key: "z", byte: 0x1a },
  ];
  for (const { key, byte } of letterCases) {
    it(`Ctrl+${key} → 0x${byte.toString(16).padStart(2, "0")}`, () => {
      expect(sent(mapKeyboardEvent(ev({ key, ctrlKey: true })))).toBe(String.fromCharCode(byte));
    });
  }

  const symbolCases: { key: string; byte: number }[] = [
    { key: "@", byte: 0x00 },
    { key: "[", byte: 0x1b },
    { key: "\\", byte: 0x1c },
    { key: "]", byte: 0x1d },
    { key: "^", byte: 0x1e },
    { key: "_", byte: 0x1f },
  ];
  for (const { key, byte } of symbolCases) {
    it(`Ctrl+${key} → 0x${byte.toString(16).padStart(2, "0")}`, () => {
      expect(sent(mapKeyboardEvent(ev({ key, ctrlKey: true })))).toBe(String.fromCharCode(byte));
    });
  }

  it("Ctrl+Shift+letter still folds to the C0 byte (Shift ignored)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "A", ctrlKey: true, shiftKey: true })))).toBe("\x01");
  });
});

// ===========================================================================
// Alt + printable → ESC prefix + character (metaSendsEscape default)
// ===========================================================================

describe("Alt+printable → ESC + char [spec: meta prefix]", () => {
  for (const key of ["a", "f", "z", "1", "."]) {
    it(`Alt+${key} → ESC ${key}`, () => {
      expect(sent(mapKeyboardEvent(ev({ key, altKey: true })))).toBe(`\x1b${key}`);
    });
  }
});

// ===========================================================================
// Modifier-only presses are ignored.
// ===========================================================================

describe("modifier-only presses are ignored", () => {
  for (const key of ["Shift", "Control", "Alt", "Meta"]) {
    it(`${key} alone → ignore`, () => {
      expect(mapKeyboardEvent(ev({ key })).kind).toBe("ignore");
    });
  }
});

// ===========================================================================
// DESIGN-CHOICE behaviours (green — these lock down deliberate deviations
// from a naive PTY-encoding reading; each also appears in the deviations
// block below so the report can enumerate them).
// ===========================================================================

describe("printable text is deferred to the DOM input event (design choice)", () => {
  it("bare printable keys return kind='ignore' (browser input event emits the char)", () => {
    // Splitting keydown (control/navigation) from the input event (printable
    // text) is required for IME / dead-key composition correctness. The
    // char in the spec is emitted by the input-event layer, not here.
    for (const key of ["a", "Z", "1", "9", "!", "@", "é", " "]) {
      expect(mapKeyboardEvent(ev({ key })).kind).toBe("ignore");
    }
  });
});

describe("Shift+PageUp/PageDown route to local scrollback (design choice)", () => {
  it("Shift+PageUp → scroll-up; Shift+PageDown → scroll-down", () => {
    // Real xterm scrolls its own buffer on Shift+PageUp rather than sending
    // to the PTY; we mirror that instead of emitting CSI 5;2~ / CSI 6;2~.
    expect(mapKeyboardEvent(ev({ key: "PageUp", shiftKey: true })).kind).toBe("scroll-up");
    expect(mapKeyboardEvent(ev({ key: "PageDown", shiftKey: true })).kind).toBe("scroll-down");
  });
});

// ===========================================================================
// SPEC DEVIATIONS — recorded, NOT fixed. Each it.skip states the spec value
// and the observed encoder behaviour. See the accompanying report.
// ===========================================================================

describe("spec deviations (documented — NOT fixed)", () => {
  it.skip("DEVIATION: bare printable key — spec: emits the character; got: kind='ignore' (deferred to DOM input event; DESIGN CHOICE)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "a" })))).toBe("a");
  });

  it.skip("DEVIATION: bare Space — spec: emits SP (0x20); got: kind='ignore' (deferred to DOM input event; DESIGN CHOICE)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: " " })))).toBe(" ");
  });

  it.skip("DEVIATION: Shift+PageUp — spec: CSI 5;2~; got: kind='scroll-up' (local scrollback; DESIGN CHOICE, matches real xterm)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "PageUp", shiftKey: true })))).toBe("\x1b[5;2~");
  });

  it.skip("DEVIATION: Shift+PageDown — spec: CSI 6;2~; got: kind='scroll-down' (local scrollback; DESIGN CHOICE, matches real xterm)", () => {
    expect(sent(mapKeyboardEvent(ev({ key: "PageDown", shiftKey: true })))).toBe("\x1b[6;2~");
  });
});

// ===========================================================================
// ctrlByteFor — the C0 lookup table (used by mapKeyboardEvent + sticky-Ctrl).
// Spec: ASCII control-key convention (see the Ctrl+printable block above).
// ===========================================================================

describe("ctrlByteFor", () => {
  it("a-z (case-folded) → 0x01..0x1a", () => {
    const letters = "abcdefghijklmnopqrstuvwxyz";
    for (let i = 0; i < letters.length; i++) {
      const lower = letters.charAt(i);
      const upper = lower.toUpperCase();
      expect(ctrlByteFor(lower)).toBe(String.fromCharCode(i + 1));
      expect(ctrlByteFor(upper)).toBe(String.fromCharCode(i + 1));
    }
  });

  it("space and @ → NUL (0x00)", () => {
    expect(ctrlByteFor(" ")).toBe("\x00");
    expect(ctrlByteFor("@")).toBe("\x00");
  });

  it("C0 symbol set [ \\ ] ^ _ → 0x1b..0x1f, ? → DEL", () => {
    expect(ctrlByteFor("[")).toBe("\x1b");
    expect(ctrlByteFor("\\")).toBe("\x1c");
    expect(ctrlByteFor("]")).toBe("\x1d");
    expect(ctrlByteFor("^")).toBe("\x1e");
    expect(ctrlByteFor("_")).toBe("\x1f");
    expect(ctrlByteFor("?")).toBe("\x7f");
  });

  it("returns null for unmapped single chars and non-1-length strings", () => {
    expect(ctrlByteFor("0")).toBeNull();
    expect(ctrlByteFor("!")).toBeNull();
    expect(ctrlByteFor("é")).toBeNull();
    expect(ctrlByteFor("")).toBeNull();
    expect(ctrlByteFor("ab")).toBeNull();
    expect(ctrlByteFor("hello")).toBeNull();
  });
});

// ===========================================================================
// Bracketed paste + CR/LF normalisation (keyboard.ts paste helpers).
// ===========================================================================

describe("bracketed paste", () => {
  it("wraps with DEC 2004 sentinels and sanitises embedded ESC", () => {
    expect(bracketTextForPaste("hello")).toBe("\x1b[200~hello\x1b[201~");
    expect(bracketTextForPaste("a\x1b[201~b")).toBe(`\x1b[200~a\u241B[201~b\x1b[201~`);
  });

  it("normalises CR/LF to CR", () => {
    expect(prepareTextForTerminal("a\r\nb\nc\r")).toBe("a\rb\rc\r");
    // Paste NUL hygiene (P2): NUL bytes are stripped wherever they appear —
    // never meaningful paste content, and a leading NUL would otherwise
    // interact with the v3 wire framing.
    expect(prepareTextForTerminal("\x00lead\x00mid\x00")).toBe("leadmid");
  });
});

describe("kittyCtrlCharSeq: character-level unshifted-key rule", () => {
  it("maps shifted US-layout symbols to base codepoint + ctrl+shift", () => {
    expect(kittyCtrlCharSeq(":")).toBe("\x1b[59;6u"); // ';' + shift
    expect(kittyCtrlCharSeq("{")).toBe("\x1b[91;6u"); // '[' + shift
    expect(kittyCtrlCharSeq("!")).toBe("\x1b[49;6u"); // '1' + shift
  });

  it("encodes unshifted characters with plain ctrl", () => {
    expect(kittyCtrlCharSeq("s")).toBe("\x1b[115;5u");
    expect(kittyCtrlCharSeq(";")).toBe("\x1b[59;5u");
    expect(kittyCtrlCharSeq("1")).toBe("\x1b[49;5u");
  });
});
