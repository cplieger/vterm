// Keyboard event → terminal byte sequence mapping.
//
// Mirrors xterm.js's evaluateKeyboardEvent (src/common/input/Keyboard.ts):
// every browser KeyboardEvent maps to either a sequence to send to the
// PTY or a local action (page-up/down for local scrollback nav). The
// mapping is exhaustive over xterm.js's coverage so vim/readline/Ink/
// the host application get the keys they expect.
//
// Modifier encoding follows xterm.js / VT520 convention:
//   1=Shift, 2=Alt, 4=Ctrl, 8=Meta. Sum then +1 for CSI 1;{n}{letter}.
//
// Application cursor mode (DECCKM, CSI ?1h/l) is tracked client-side
// via modes.ts (announced by server in wireMsgModes). Server-side, the
// vt screen tracks both bracketed paste (?2004) and DECCKM (?1) and
// emits a modes frame whenever they change.

import { getKeyboardFlags, isBracketedPaste } from "./modes.js";

/** Result of mapping a keyboard event. */
export type KeyboardResult =
  | { kind: "send"; bytes: string }
  | { kind: "scroll-up" } // Shift+PageUp — handled locally
  | { kind: "scroll-down" } // Shift+PageDown — handled locally
  | { kind: "ignore" }; // Modifier-only press, etc.

/**
 * KeyboardModes is the mode state `mapKeyboardEvent` reads: DECCKM (application
 * cursor keys), DECKPAM (application keypad), and the kitty keyboard
 * progressive-enhancement flags. Passed explicitly so the shared input maps
 * against the active tab's modes and so the mapping is testable without mutating
 * global state. The `modes` module namespace satisfies this structurally; a
 * tabbed shell passes its active session's modes.
 */
export interface KeyboardModes {
  isApplicationCursor: () => boolean;
  isApplicationKeypad: () => boolean;
  /**
   * Active kitty keyboard flags (bit0 disambiguate; higher bits reserved). When
   * the disambiguate bit is set, non-text keys are encoded as kitty CSI-u
   * sequences instead of the legacy encodings.
   */
  getKeyboardFlags: () => number;
}

const ESC = "\x1b";
const DEL = "\x7f";

/** Compute the xterm modifier digit (used in CSI 1;{n}letter sequences). */
function modifiersDigit(ev: KeyboardEvent): number {
  return (
    1 + (ev.shiftKey ? 1 : 0) + (ev.altKey ? 2 : 0) + (ev.ctrlKey ? 4 : 0) + (ev.metaKey ? 8 : 0)
  );
}

// -- Cursor / navigation keys -----------------------------------------------

/** The cursor-key trailing letters that honor DECCKM (arrows + Home/End). */
type CursorKeyLetter = "A" | "B" | "C" | "D" | "H" | "F";

/**
 * plainCursorKeySeq is THE encoding of an unmodified logical cursor-key press
 * (arrows, Home, End) — the one home for the mode decision, shared by the
 * physical-key path (csiLetter's modifier-less branch) and the mobile
 * toolbar's arrow buttons so the two encoders cannot drift:
 *   - kitty disambiguate active: the CSI form regardless of DECCKM (the
 *     protocol supersedes cursor-key mode);
 *   - DECCKM (application cursor mode): the SS3 form (ESC O letter);
 *   - otherwise: the bare CSI form.
 *
 * Both mode bits are PARAMETERS (not read from the module-global mode state):
 * mapKeyboardEvent honors an injected KeyboardModes, and a helper silently
 * consulting the global would diverge from it in a tabbed shell.
 */
export function plainCursorKeySeq(
  letter: CursorKeyLetter,
  kittyActive: boolean,
  appCursor: boolean,
): string {
  if (kittyActive) {
    return `${ESC}[${letter}`;
  }
  return appCursor ? `${ESC}O${letter}` : `${ESC}[${letter}`;
}

/**
 * plainEscapeSeq is THE encoding of an unmodified logical Escape press: CSI
 * 27 u under kitty disambiguate (the protocol's headline unambiguous-Esc
 * feature — the app never has to distinguish a lone ESC byte from the start
 * of an escape sequence), else the bare ESC byte. Shared by the physical-key
 * path and the toolbar's ESC button. The flag is a parameter for the same
 * injected-modes honesty as plainCursorKeySeq.
 */
export function plainEscapeSeq(kittyActive: boolean): string {
  return kittyActive ? `${ESC}[27u` : ESC;
}

/** kittyActiveIn reads the disambiguate flag from an INJECTED KeyboardModes
 *  (the seam mapKeyboardEvent honors), as opposed to kittyDisambiguateActive,
 *  which reads the module-global active-session facade used by the toolbar.
 *  connection.setSession restores that facade from the target session's
 *  snapshot synchronously before switch-window input can be encoded. */
function kittyActiveIn(modes: KeyboardModes): boolean {
  return (modes.getKeyboardFlags() & KITTY_DISAMBIGUATE) !== 0;
}

// Letter is the xterm trailing letter for the keys that honor DECCKM: the four
// arrows plus Home (H) and End (F). Without modifiers we send the bare CSI form;
// with modifiers we send CSI 1;{mod}{letter}. xterm.js Keyboard.ts pattern.
// (F1-F4's SS3/CSI pair is encoded inline at its own call site, not here.)
//
// The parameter type is `CursorKeyLetter` rather than `string`, so "every
// letter reaching here honors DECCKM" is a compiler guarantee instead of a
// runtime net: all three call sites pass A/B/C/D from ARROW_LETTER, or the H/F
// literals.
//
// Application cursor mode (DECCKM, CSI ?1) is plumbed via modes.ts.
// When the application has set DECCKM, the modifier-less form switches
// from CSI to SS3 (ESC O letter) via plainCursorKeySeq; modifier-bearing
// forms stay on CSI because they have no SS3 equivalent.
function csiLetter(letter: CursorKeyLetter, ev: KeyboardEvent, modes: KeyboardModes): string {
  const m = modifiersDigit(ev);
  if (m === 1) {
    // `kittyActiveIn(modes)` is always FALSE as reached from today's only
    // caller: mapKeyboardEvent returns through encodeKittyDisambiguate before
    // any cursor key gets here. It is passed anyway rather than hard-coded,
    // because plainCursorKeySeq is the one home for the mode decision and a
    // literal `false` would make this function structurally unable to express
    // the kitty form — a later change to that early return would then
    // mis-encode silently instead of failing to compile or reading wrong.
    return plainCursorKeySeq(letter, kittyActiveIn(modes), modes.isApplicationCursor());
  }
  return `${ESC}[1;${m}${letter}`;
}

// Tilde-form keys (Insert=2, Delete=3, PageUp=5, PageDown=6, F5+=15..24).
function csiTilde(num: number, ev: KeyboardEvent): string {
  const m = modifiersDigit(ev);
  return m === 1 ? `${ESC}[${num}~` : `${ESC}[${num};${m}~`;
}

const FN_LETTER: Record<string, string | undefined> = {
  F1: "P",
  F2: "Q",
  F3: "R",
  F4: "S",
};
const FN_TILDE: Record<string, number | undefined> = {
  F5: 15,
  F6: 17,
  F7: 18,
  F8: 19,
  F9: 20,
  F10: 21,
  F11: 23,
  F12: 24,
  // F13-F20 use xterm's own extended tilde codes (25-34, with 27 and 30
  // skipped historically). F21-F24 have NO standard legacy encoding — xterm
  // itself stops at F20 — so they stay silent here and are covered only by
  // the kitty path (dedicated codepoints; see KITTY_FN_HIGH). Note xterm.js
  // supports none of F13+ (xtermjs/xterm.js#1426, open since 2018).
  F13: 25,
  F14: 26,
  F15: 28,
  F16: 29,
  F17: 31,
  F18: 32,
  F19: 33,
  F20: 34,
};

const ARROW_LETTER: Record<string, CursorKeyLetter | undefined> = {
  ArrowUp: "A",
  ArrowDown: "B",
  ArrowRight: "C",
  ArrowLeft: "D",
};

// Application keypad mode (DECKPAM) SS3 mappings. When the server has
// sent ESC = (DECKPAM), numeric keypad keys send ESC O <letter> instead
// of their normal character. Mapping from VT100 User Guide Table 3-8
// (ANSI mode, application keypad). xterm extensions for *, +, / included.
const KEYPAD_SS3: Record<string, string | undefined> = {
  "0": "p",
  "1": "q",
  "2": "r",
  "3": "s",
  "4": "t",
  "5": "u",
  "6": "v",
  "7": "w",
  "8": "x",
  "9": "y",
  ".": "n",
  "-": "m",
  "+": "k",
  "*": "j",
  "/": "o",
  Enter: "M",
};

// -- Kitty keyboard protocol (progressive enhancement) ----------------------
// When the server reports an active kitty flag (modes.getKeyboardFlags), keys
// that do NOT produce text are encoded per the kitty protocol instead of the
// legacy encodings, so an app that enabled the protocol (e.g. Codex via
// crossterm) gets unambiguous key events. We honor the disambiguate flag (0x1)
// — report-event-types (0x2) / report-alternate-keys (0x4) / report-all (0x8) /
// text (0x10) are masked off server-side, so the CSI ?u query reports only 0x1
// and the encoder never has to emit them.
//
// Under disambiguate, text-producing keys still flow through the hidden
// textarea as text (this encoder returns "ignore" for them), matching both the
// spec (text keys stay text under 0x1) and our IME/composition model. Only
// Escape, ctrl/alt/meta combinations, and functional keys are re-encoded.

/** Kitty progressive-enhancement flag bits (mirror vt/kitty.go). */
const KITTY_DISAMBIGUATE = 1;

// Functional keys encoded as CSI 1;{mod}{letter} (the 1;{mod} is omitted when
// unmodified). Under kitty these use the CSI form (legacy uses SS3 for F1-F4)
// and even under DECCKM. F3 is deliberately absent — it has no letter form in
// the kitty table (CSI R collides with the Cursor Position Report), so it goes
// in KITTY_TILDE as 13 instead. Do not add F3 here.
const KITTY_LETTER: Record<string, string | undefined> = {
  ArrowUp: "A",
  ArrowDown: "B",
  ArrowRight: "C",
  ArrowLeft: "D",
  Home: "H",
  End: "F",
  F1: "P",
  F2: "Q",
  F4: "S",
};
// Functional keys encoded as CSI {num};{mod}~ (the ;{mod} omitted when unmodified).
const KITTY_TILDE: Record<string, number | undefined> = {
  Insert: 2,
  Delete: 3,
  PageUp: 5,
  PageDown: 6,
  F3: 13, // no letter form — CSI R conflicts with the cursor-position report
  F5: 15,
  F6: 17,
  F7: 18,
  F8: 19,
  F9: 20,
  F10: 21,
  F11: 23,
  F12: 24,
};

// F13-F24 under kitty: the spec assigns them dedicated functional-key
// codepoints (57376-57387, contiguous), encoded as CSI {code};{mod} u like
// the keypad keys. The legacy path covers only F13-F20 (xterm tilde codes;
// see FN_TILDE) — these twelve make the kitty path the complete one, which
// exceeds xterm.js (no F13+ support at all, xtermjs/xterm.js#1426).
const KITTY_FN_HIGH: Record<string, number | undefined> = {
  F13: 57376,
  F14: 57377,
  F15: 57378,
  F16: 57379,
  F17: 57380,
  F18: 57381,
  F19: 57382,
  F20: 57383,
  F21: 57384,
  F22: 57385,
  F23: 57386,
  F24: 57387,
};

// US-layout base (unshifted) codepoints for the non-alphanumeric physical keys,
// keyed by ev.code. Used so a ctrl/alt+shift+symbol event reports the UNSHIFTED
// key code the spec mandates (e.g. Ctrl+Shift+; is CSI 59;6u using ';'=59, not
// ':'=58). Letters/digits are handled separately from ev.code; other keys fall
// back to the case-folded ev.key.
const KITTY_CODE_BASE: Record<string, number | undefined> = {
  Semicolon: 59, // ;
  Equal: 61, // =
  Comma: 44, // ,
  Minus: 45, // -
  Period: 46, // .
  Slash: 47, // /
  Backquote: 96, // `
  BracketLeft: 91, // [
  Backslash: 92, // \
  BracketRight: 93, // ]
  Quote: 39, // '
};

// US-layout shifted character -> unshifted base codepoint, for input paths
// that only have the produced CHARACTER (the mobile toolbar's sticky-Ctrl),
// not a KeyboardEvent with ev.code. The character-level twin of
// KITTY_CODE_BASE: Ctrl+':' must encode as CSI 59;6u (unshifted ';' + the
// shift modifier), matching what the physical-keyboard path derives from
// ev.code — not 58;5u from the shifted glyph.
const KITTY_SHIFTED_CHAR_BASE: Record<string, number | undefined> = {
  ":": 59, // ;
  "<": 44, // ,
  ">": 46, // .
  "?": 47, // /
  '"': 39, // '
  "{": 91, // [
  "}": 93, // ]
  "|": 92, // \
  "~": 96, // `
  "+": 61, // =
  _: 45, // -
  "!": 49, // 1
  "@": 50, // 2
  "#": 51, // 3
  $: 52, // 4
  "%": 53, // 5
  "^": 54, // 6
  "&": 55, // 7
  "*": 56, // 8
  "(": 57, // 9
  ")": 48, // 0
};

/**
 * kittyCtrlCharSeq encodes Ctrl+<typed character> in the kitty CSI-u form
 * using the spec's unshifted-key rule: a shifted glyph (':', 'A', '{') maps to
 * its US-layout base codepoint with the shift bit added to the modifier
 * (Ctrl+':' -> 59;6u), an unshifted one encodes directly (Ctrl+'s' -> 115;5u).
 * Shared by every path that has only the character (sticky-Ctrl toolbar),
 * keeping it byte-identical with the physical-keyboard encoder.
 */
export function kittyCtrlCharSeq(ch: string): string | null {
  let cp = KITTY_SHIFTED_CHAR_BASE[ch];
  let shifted = cp !== undefined;
  if (cp === undefined && ch >= "A" && ch <= "Z") {
    cp = ch.toLowerCase().codePointAt(0);
    shifted = true;
  }
  cp ??= ch.toLowerCase().codePointAt(0);
  if (cp === undefined) {
    return null;
  }
  return `${ESC}[${cp};${shifted ? 6 : 5}u`; // 5 = ctrl, 6 = ctrl+shift
}

// Keypad NON-TEXT keys under disambiguate, keyed by ev.key (the key's current
// function — NumLock flips a numpad key between its text digit, which stays text
// under 0x1, and these navigation functions). ev.code identifies it as a keypad
// key. Per the spec these get dedicated KP_* codes (57414-57427) so an app can
// tell them apart from the main navigation keys. NumpadEnter is included as
// KP_ENTER (57414) — it is a distinct key, NOT the legacy-byte main Enter.
// KP_BEGIN (Numpad5, NumLock off -> "Clear") is emitted as 57427 in the `u`
// form: both kitty and crossterm decode `CSI 57427 u`, whereas the spec's
// alternate `CSI E` letter form is kitty-only (crossterm/ratatui can't parse it).
// The KP_ DIGIT/operator codes (57399-57416) are deliberately absent — they are
// a report-all (0x8) feature; under 0x1 a NumLock-on numpad digit is plain text.
const KITTY_KEYPAD: Record<string, number | undefined> = {
  ArrowLeft: 57417, // KP_LEFT
  ArrowRight: 57418, // KP_RIGHT
  ArrowUp: 57419, // KP_UP
  ArrowDown: 57420, // KP_DOWN
  PageUp: 57421, // KP_PAGE_UP
  PageDown: 57422, // KP_PAGE_DOWN
  Home: 57423, // KP_HOME
  End: 57424, // KP_END
  Insert: 57425, // KP_INSERT
  Delete: 57426, // KP_DELETE
  Clear: 57427, // KP_BEGIN
  Enter: 57414, // KP_ENTER
};

/** CSI 1;{mod}{letter}, omitting `1;{mod}` when there are no modifiers. */
function kittyLetterSeq(letter: string, ev: KeyboardEvent): string {
  const m = modifiersDigit(ev);
  return m === 1 ? `${ESC}[${letter}` : `${ESC}[1;${m}${letter}`;
}
/** CSI {num};{mod}~, omitting `;{mod}` when there are no modifiers. */
function kittyTildeSeq(num: number, ev: KeyboardEvent): string {
  const m = modifiersDigit(ev);
  return m === 1 ? `${ESC}[${num}~` : `${ESC}[${num};${m}~`;
}
/** CSI {num};{mod}u, omitting `;{mod}` when there are no modifiers. */
function kittyUSeq(num: number, ev: KeyboardEvent): string {
  const m = modifiersDigit(ev);
  return m === 1 ? `${ESC}[${num}u` : `${ESC}[${num};${m}u`;
}

/** True when any modifier (ctrl / alt / meta / shift) is held. */
function anyModifier(ev: KeyboardEvent): boolean {
  return ev.ctrlKey || ev.altKey || ev.metaKey || ev.shiftKey;
}

/**
 * The kitty "unicode-key-code": the UNSHIFTED (lower-case / base-layout)
 * codepoint of the key. Letters and digits are read from ev.code (the physical
 * key), so a composed Alt/Option character on macOS still maps to its base key;
 * other single-character keys fall back to the case-folded ev.key.
 */
function kittyKeyCodepoint(ev: KeyboardEvent): number {
  const code = ev.code;
  if (code.length === 4 && code.startsWith("Key")) {
    return code.charCodeAt(3) + 32; // "KeyA".."KeyZ" -> 'a'..'z'
  }
  if (code.length === 6 && code.startsWith("Digit")) {
    return code.charCodeAt(5); // "Digit0".."Digit9" -> '0'..'9'
  }
  const base = KITTY_CODE_BASE[code];
  if (base !== undefined) {
    return base; // symbol key -> unshifted base codepoint (spec: use the un-shifted key)
  }
  const k = ev.key;
  return k.length === 1 ? (k.toLowerCase().codePointAt(0) ?? 0) : 0;
}

/**
 * Encode a keydown under the kitty disambiguate flag (0x1). Returns "ignore"
 * for text-producing keys so the hidden textarea still handles typing/IME.
 * Called only when the flag is active; the modifier-only and local-scroll cases
 * are handled by the shared preamble in mapKeyboardEvent (which skips its
 * application-keypad SS3 branch under this flag, so keypad keys reach here).
 */
function encodeKittyDisambiguate(ev: KeyboardEvent): KeyboardResult {
  const key = ev.key;

  // Keypad NON-TEXT keys (NumLock-off navigation + NumpadEnter) get dedicated
  // KP_* codes so an app can distinguish them from the main keys. Detected by
  // ev.code (Numpad*); the specific code is chosen by ev.key. Text keypad keys
  // (NumLock-on digits/operators) are NOT intercepted — they fall through to the
  // printable path below (text when unmodified; the ASCII-codepoint CSI-u form
  // when a ctrl/alt/meta modifier suppresses the text), because KP_ digit codes
  // are a report-all (0x8) feature we don't honor.
  if (ev.code.startsWith("Numpad")) {
    const kp = KITTY_KEYPAD[key];
    if (kp !== undefined) {
      return { kind: "send", bytes: kittyUSeq(kp, ev) };
    }
  }

  const letter = KITTY_LETTER[key];
  if (letter !== undefined) {
    return { kind: "send", bytes: kittyLetterSeq(letter, ev) };
  }
  const tnum = KITTY_TILDE[key];
  if (tnum !== undefined) {
    return { kind: "send", bytes: kittyTildeSeq(tnum, ev) };
  }
  const fnHigh = KITTY_FN_HIGH[key];
  if (fnHigh !== undefined) {
    return { kind: "send", bytes: kittyUSeq(fnHigh, ev) };
  }

  // Enter / Tab / Backspace keep their legacy bytes when UNMODIFIED (so a user
  // can still type `reset` if a program leaves the mode on after crashing);
  // with modifiers they take the disambiguated CSI-u form.
  if (key === "Enter") {
    return { kind: "send", bytes: anyModifier(ev) ? kittyUSeq(13, ev) : "\r" };
  }
  if (key === "Tab") {
    return { kind: "send", bytes: anyModifier(ev) ? kittyUSeq(9, ev) : "\t" };
  }
  if (key === "Backspace") {
    return { kind: "send", bytes: anyModifier(ev) ? kittyUSeq(127, ev) : DEL };
  }
  if (key === "Escape") {
    // Unmodified Escape shares the logical-Escape home with the toolbar's ESC
    // button; a modified Escape carries its modifier digit (no toolbar analog).
    // Kitty is definitionally active on this path (mapKeyboardEvent routes
    // here only when the injected modes carry the disambiguate flag).
    return { kind: "send", bytes: anyModifier(ev) ? kittyUSeq(27, ev) : plainEscapeSeq(true) };
  }

  // Single-character keys: text stays text (deferred to the textarea) UNLESS a
  // ctrl/alt/meta modifier is held, in which case the legacy encoding is
  // ambiguous (Ctrl+I == Tab, Alt+x == ESC x, …) so we send the CSI-u form with
  // the unshifted codepoint.
  if (key.length === 1) {
    // Meta/Cmd-ONLY combos stay with the browser even under kitty
    // disambiguate, mirroring the legacy path's deliberate carve-out: Cmd+C /
    // Cmd+V / Cmd+R are browser/OS chrome, and the protocol's reference
    // implementation (the kitty terminal itself) likewise consumes
    // emulator-owned shortcuts rather than reporting them to the app. Without
    // this, enabling the protocol silently broke copy-on-macOS.
    if (ev.metaKey && !ev.ctrlKey && !ev.altKey) {
      return { kind: "ignore" };
    }
    if (ev.ctrlKey || ev.altKey || ev.metaKey) {
      const cp = kittyKeyCodepoint(ev);
      if (cp > 0) {
        return { kind: "send", bytes: kittyUSeq(cp, ev) };
      }
    }
    return { kind: "ignore" };
  }
  return { kind: "ignore" };
}

/** True when the kitty disambiguate flag is active in the GLOBAL
 *  active-session mode facade. Exported for the toolbar widget (toolbar.ts);
 *  connection.setSession synchronously restores the target session's cached
 *  snapshot before input resumes. Encoder paths in this module use
 *  kittyActiveIn (the injected-modes read) instead. */
export function kittyDisambiguateActive(): boolean {
  return (getKeyboardFlags() & KITTY_DISAMBIGUATE) !== 0;
}

/**
 * mapKeyboardEvent converts a KeyboardEvent into the terminal action
 * to take. Returns "ignore" when the event is purely a modifier press
 * or when the browser should be allowed to handle it (e.g. browser
 * shortcuts like Cmd+R).
 *
 * Caller is responsible for ev.preventDefault() when the result is
 * "send" or "scroll-*"; we don't call it here so the function stays
 * pure and testable.
 *
 * `modes` supplies the active session's DECCKM/DECKPAM state; pass the `modes`
 * module namespace for the single-terminal case or the active tab's modes in a
 * tabbed shell.
 */
export function mapKeyboardEvent(ev: KeyboardEvent, modes: KeyboardModes): KeyboardResult {
  // Modifier-only presses (Shift, Ctrl, Alt, Meta) — no-op.
  if (ev.key === "Shift" || ev.key === "Control" || ev.key === "Alt" || ev.key === "Meta") {
    return { kind: "ignore" };
  }

  // Composition-in-progress: caller ignores keydowns while
  // CompositionHelper.isComposing is true. We don't see that state
  // here. The caller filters at its layer.

  // Application keypad mode (DECKPAM): when active, numpad keys send
  // SS3 sequences (ESC O <letter>) instead of their normal characters.
  // We detect numpad keys via ev.code (Numpad0-9, NumpadDecimal,
  // NumpadSubtract, NumpadAdd, NumpadMultiply, NumpadDivide, NumpadEnter).
  if (
    modes.isApplicationKeypad() &&
    (modes.getKeyboardFlags() & KITTY_DISAMBIGUATE) === 0 &&
    !ev.ctrlKey &&
    !ev.altKey &&
    !ev.metaKey &&
    ev.code.startsWith("Numpad")
  ) {
    const ss3Key = numpadCodeToKey(ev.code);
    if (ss3Key !== undefined) {
      const ss3Char = KEYPAD_SS3[ss3Key];
      if (ss3Char !== undefined) {
        return { kind: "send", bytes: `${ESC}O${ss3Char}` };
      }
    }
  }

  // Browser-reserved meta combos (Cmd+R, Cmd+T, Cmd+Q, Cmd+W on Mac;
  // Ctrl+R, Ctrl+T, Ctrl+W with no Shift on others). Let the browser
  // handle them. Heuristic: only take Ctrl+letter when Shift is NOT
  // held AND the letter is one of the ones xterm consumes (a-z),
  // skipping the few that browsers commonly reserve.
  // (We deliberately pass Ctrl+R / Ctrl+T / Ctrl+W through to the PTY
  // — the host application's Ctrl+T transcript view depends on it. Users on macOS
  // who want browser-reserved combos can use Cmd instead.)

  // Shift+PageUp / Shift+PageDown — local scrollback navigation
  // (matches xterm.js KeyboardResultType.PAGE_UP/PAGE_DOWN).
  if (ev.key === "PageUp" && ev.shiftKey && !ev.ctrlKey && !ev.altKey && !ev.metaKey) {
    return { kind: "scroll-up" };
  }
  if (ev.key === "PageDown" && ev.shiftKey && !ev.ctrlKey && !ev.altKey && !ev.metaKey) {
    return { kind: "scroll-down" };
  }

  // Kitty keyboard protocol: once the app enables the disambiguate flag, encode
  // non-text keys as CSI-u sequences (Esc, ctrl/alt combos, functional keys);
  // text keys still defer to the textarea. Placed after the shared preamble
  // (modifier-only, application keypad, local scroll) so those behave
  // identically in both modes.
  if ((modes.getKeyboardFlags() & KITTY_DISAMBIGUATE) !== 0) {
    return encodeKittyDisambiguate(ev);
  }

  // Cursor keys — CSI form with optional modifiers (CSI 1;{m}{letter}).
  const arrow = ARROW_LETTER[ev.key];
  if (arrow !== undefined) {
    return { kind: "send", bytes: csiLetter(arrow, ev, modes) };
  }

  // Home / End — CSI {H,F} with optional modifiers.
  if (ev.key === "Home") {
    return { kind: "send", bytes: csiLetter("H", ev, modes) };
  }
  if (ev.key === "End") {
    return { kind: "send", bytes: csiLetter("F", ev, modes) };
  }

  // Insert / Delete / PageUp / PageDown (no Shift) — CSI tilde forms.
  if (ev.key === "Insert") {
    return { kind: "send", bytes: csiTilde(2, ev) };
  }
  if (ev.key === "Delete") {
    return { kind: "send", bytes: csiTilde(3, ev) };
  }
  if (ev.key === "PageUp") {
    return { kind: "send", bytes: csiTilde(5, ev) };
  }
  if (ev.key === "PageDown") {
    return { kind: "send", bytes: csiTilde(6, ev) };
  }

  // F1-F4 — SS3 with optional modifier-CSI, like xterm.
  const fnLetter = FN_LETTER[ev.key];
  if (fnLetter !== undefined) {
    const m = modifiersDigit(ev);
    return {
      kind: "send",
      bytes: m === 1 ? `${ESC}O${fnLetter}` : `${ESC}[1;${m}${fnLetter}`,
    };
  }
  // F5-F12 — CSI tilde form with modifiers.
  const fnTilde = FN_TILDE[ev.key];
  if (fnTilde !== undefined) {
    return { kind: "send", bytes: csiTilde(fnTilde, ev) };
  }

  // Tab / Shift+Tab.
  if (ev.key === "Tab") {
    return { kind: "send", bytes: ev.shiftKey ? `${ESC}[Z` : "\t" };
  }

  // Enter — \r. Alt+Enter prefixes ESC.
  if (ev.key === "Enter") {
    return { kind: "send", bytes: ev.altKey ? `${ESC}\r` : "\r" };
  }

  // Backspace — \x7f (DEL). Alt+Backspace = ESC + DEL (delete-prev-word
  // in readline). Ctrl+Backspace = \b (^H).
  if (ev.key === "Backspace") {
    if (ev.altKey) {
      return { kind: "send", bytes: ESC + DEL };
    }
    if (ev.ctrlKey) {
      return { kind: "send", bytes: "\b" };
    }
    return { kind: "send", bytes: DEL };
  }

  // Escape — ESC (via the shared logical-Escape home; kitty is definitionally
  // inactive on this legacy path, so it yields the bare ESC byte).
  // Alt+Escape = ESC ESC (xterm.js convention).
  if (ev.key === "Escape") {
    return { kind: "send", bytes: ev.altKey ? `${ESC}${ESC}` : plainEscapeSeq(false) };
  }

  // Space — \x00 with Ctrl (per xterm). Alt+Space = ESC ' '.
  if (ev.key === " ") {
    if (ev.ctrlKey) {
      return { kind: "send", bytes: "\x00" };
    }
    if (ev.altKey) {
      return { kind: "send", bytes: ESC + " " };
    }
    return { kind: "ignore" }; // let `input` event handle plain space
  }

  // Single printable character with modifiers.
  if (ev.key.length === 1) {
    const ch = ev.key;

    // Ctrl+printable → C0 control byte (a-z → \x01..\x1a, plus the
    // C0 set @[\]^_? → \x00..\x1f, \x7f). Single branch via
    // `ctrlByteFor`; same table also drives bindMobileToolbar's
    // sticky-Ctrl applier.
    if (ev.ctrlKey && !ev.altKey && !ev.metaKey) {
      const c0 = ctrlByteFor(ch);
      if (c0 !== null) {
        return { kind: "send", bytes: c0 };
      }
    }
    // Alt+printable → ESC + char (meta prefix). Plain `input` event
    // would still fire with the char, so we'd duplicate. Caller must
    // preventDefault when we return "send" here to suppress `input`.
    if (ev.altKey && !ev.ctrlKey && !ev.metaKey) {
      return { kind: "send", bytes: ESC + ch };
    }
  }

  // Everything else: defer to the `input` event. The browser will
  // produce the printable character (including IME / dead-key
  // composition output) via input, where we send it.
  return { kind: "ignore" };
}

/**
 * ctrlByteFor returns the C0 control byte produced by Ctrl+`ch`, or
 * `null` when the character has no Ctrl mapping. The full table:
 *
 *   a-z (case-folded)  → \x01..\x1a (Ctrl+A=SOH .. Ctrl+Z=SUB)
 *   ' ' (space)        → \x00 (NUL — same as Ctrl+@)
 *   '@'                → \x00 (NUL)
 *   '['                → \x1b (ESC)
 *   '\\'               → \x1c (FS)
 *   ']'                → \x1d (GS)
 *   '^'                → \x1e (RS)
 *   '_'                → \x1f (US)
 *   '?'                → \x7f (DEL — also Ctrl+8 on US layouts via Shift+/)
 *
 * Anything else (multi-char strings, unmapped single chars) returns
 * `null`. Used by `mapKeyboardEvent` for Ctrl+printable handling and
 * by `bindMobileToolbar`'s sticky-Ctrl applier.
 */
export function ctrlByteFor(ch: string): string | null {
  if (ch.length !== 1) {
    return null;
  }
  // Letters a-z (case-folded) → \x01..\x1a.
  const code = ch.toLowerCase().charCodeAt(0);
  if (code >= 97 && code <= 122) {
    return String.fromCharCode(code - 96);
  }
  switch (ch) {
    case " ":
    case "@":
      return "\x00";
    case "[":
      return "\x1b";
    case "\\":
      return "\x1c";
    case "]":
      return "\x1d";
    case "^":
      return "\x1e";
    case "_":
      return "\x1f";
    case "?":
      return "\x7f";
    default:
      return null;
  }
}

// -- Application keypad helpers -----------------------------------------------

/** Map a KeyboardEvent.code starting with "Numpad" to the KEYPAD_SS3 lookup key. */
function numpadCodeToKey(code: string): string | undefined {
  switch (code) {
    case "Numpad0":
      return "0";
    case "Numpad1":
      return "1";
    case "Numpad2":
      return "2";
    case "Numpad3":
      return "3";
    case "Numpad4":
      return "4";
    case "Numpad5":
      return "5";
    case "Numpad6":
      return "6";
    case "Numpad7":
      return "7";
    case "Numpad8":
      return "8";
    case "Numpad9":
      return "9";
    case "NumpadDecimal":
      return ".";
    case "NumpadSubtract":
      return "-";
    case "NumpadAdd":
      return "+";
    case "NumpadMultiply":
      return "*";
    case "NumpadDivide":
      return "/";
    case "NumpadEnter":
      return "Enter";
    default:
      return undefined;
  }
}

// -- Bracketed paste --------------------------------------------------------

/**
 * bracketTextForPaste wraps text in DEC 2004 bracketed-paste sentinels
 * after sanitising any embedded ESC bytes to U+241B (visible escape
 * symbol), but only when the application has currently enabled
 * bracketed-paste mode (CSI ?2004h). When disabled, returns the text
 * unchanged. The current mode state is owned by modes.ts, kept in
 * sync by the server's wireMsgModes wire frame.
 *
 * The ESC sanitisation defends against an attacker-controlled paste
 * containing \x1b[201~ that would prematurely close the paste region
 * and let the rest be interpreted as a command — only relevant when
 * we are bracketing.
 */
export function bracketTextForPaste(text: string): string {
  if (!isBracketedPaste()) {
    return text;
  }
  // eslint-disable-next-line no-control-regex -- intentional: sanitising ESC bytes in pasted text
  const sanitised = text.replace(/\x1b/g, "\u241B");
  return `\x1b[200~${sanitised}\x1b[201~`;
}

/**
 * Normalise CR/LF to a single CR and strip NUL bytes before bracketing.
 * xterm.js convention on both counts: NUL in pasted text is never meaningful
 * terminal input, and under the v3 wire framing a NUL-leading paste would
 * otherwise be split or (on old servers) misread as a control frame.
 */
export function prepareTextForTerminal(text: string): string {
  // eslint-disable-next-line no-control-regex -- intentional: stripping NUL from pasted text
  return text.replace(/\r?\n/g, "\r").replace(/\x00/g, "");
}

// -- Mobile toolbar ---------------------------------------------------------
