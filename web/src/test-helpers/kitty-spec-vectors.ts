// Spec-first conformance vectors for the kitty keyboard DISAMBIGUATE flag (0x1).
//
// These are transcribed from the OFFICIAL spec's normative tables
// (https://sw.kovidgoyal.net/kitty/keyboard-protocol/) as the SOURCE OF TRUTH —
// authored FROM THE SPEC, not from our encoder — so the implementation is
// asserted against what the protocol mandates rather than against itself. Each
// vector cites the spec section it enforces. Consumed by kitty-encoder.test.ts
// (Vitest Browser Mode) and mirrored by e2e/keyboard-kitty.e2e.test.ts
// (Playwright, against a served page).
//
// `kitty` is the expected result with the disambiguate flag active; `legacy` is
// the expected result with the flag OFF and is given only where our legacy
// encoding is well-defined and a meaningful regression guard (many keys encode
// identically in both modes; some legacy edge cases are pre-existing and out of
// this feature's scope, so their `legacy` is omitted rather than asserted).

export type Expect = { kind: "send"; bytes: string } | { kind: "ignore" };

export interface KittyVector {
  /** What this asserts + the spec table/section it is derived from. */
  spec: string;
  key: string;
  code?: string;
  shift?: boolean;
  ctrl?: boolean;
  alt?: boolean;
  meta?: boolean;
  kitty: Expect;
  legacy?: Expect;
}

const send = (bytes: string): Expect => ({ kind: "send", bytes });
const ignore: Expect = { kind: "ignore" };

const ESC = "\x1b";

export const KITTY_SPEC_VECTORS: KittyVector[] = [
  // --- Escape (Functional key codes: ESCAPE = 27 u). The headline fix: a lone
  // Esc byte is indistinguishable from the start of an escape sequence. ---
  {
    spec: "Functional keys: ESCAPE 27u",
    key: "Escape",
    kitty: send(`${ESC}[27u`),
    legacy: send(ESC),
  },
  {
    spec: "Functional keys: ESCAPE 27u + shift",
    key: "Escape",
    shift: true,
    kitty: send(`${ESC}[27;2u`),
  },

  // --- Text keys under Disambiguate. Plain/shift printables stay TEXT (the spec:
  // text-producing keys are sent as UTF-8; "Lock modifiers are not reported for
  // text producing keys"), so the encoder defers to the hidden textarea. Only
  // ctrl / alt / ctrl+alt / shift+alt / ctrl+shift move to CSI-u, using the
  // UNSHIFTED key code. Values cross-checked against the spec "Example
  // encodings" table (ctrl+shift column is given there explicitly). ---
  {
    spec: "Legacy text keys: plain letter stays text",
    key: "a",
    code: "KeyA",
    kitty: ignore,
    legacy: ignore,
  },
  {
    spec: "Legacy text keys: shift letter stays text",
    key: "A",
    code: "KeyA",
    shift: true,
    kitty: ignore,
  },
  {
    spec: "Example encodings: ctrl+i -> 105;5u (vs legacy 0x09 == Tab)",
    key: "i",
    code: "KeyI",
    ctrl: true,
    kitty: send(`${ESC}[105;5u`),
    legacy: send("\x09"),
  },
  {
    spec: "Example encodings: alt+a -> 97;3u",
    key: "a",
    code: "KeyA",
    alt: true,
    kitty: send(`${ESC}[97;3u`),
    legacy: send(`${ESC}a`),
  },
  {
    spec: "Disambiguate: ctrl+alt+a -> 97;7u",
    key: "a",
    code: "KeyA",
    ctrl: true,
    alt: true,
    kitty: send(`${ESC}[97;7u`),
  },
  {
    spec: "Disambiguate: shift+alt+a -> 97;4u (unshifted 97)",
    key: "A",
    code: "KeyA",
    shift: true,
    alt: true,
    kitty: send(`${ESC}[97;4u`),
  },
  {
    spec: "Example encodings: ctrl+shift+i -> 105;6u (unshifted 105)",
    key: "I",
    code: "KeyI",
    ctrl: true,
    shift: true,
    kitty: send(`${ESC}[105;6u`),
  },
  {
    spec: "Example encodings: ctrl+shift+3 -> 51;6u",
    key: "#",
    code: "Digit3",
    ctrl: true,
    shift: true,
    kitty: send(`${ESC}[51;6u`),
  },
  {
    spec: "Example encodings: ctrl+shift+; -> 59;6u (unshifted ';'=59, not ':'=58)",
    key: ":",
    code: "Semicolon",
    ctrl: true,
    shift: true,
    kitty: send(`${ESC}[59;6u`),
  },
  {
    // DELIBERATE PRODUCT DEVIATION from the spec table (which would send
    // 97;9u): meta/Cmd-ONLY printable combos are browser/OS chrome (Cmd+C,
    // Cmd+V, Cmd+R) and stay with the browser, exactly as the legacy path
    // carves them out — and as the kitty terminal itself consumes its own
    // emulator shortcuts instead of reporting them. Reporting these ate
    // copy-on-macOS inside kitty apps (judgement finding, fixed 2026-07).
    // Meta combined with ctrl/alt still reports (vector below).
    spec: "DEVIATION (browser chrome): meta(super)+a alone is NOT reported -> ignore (spec table: 97;9u)",
    key: "a",
    code: "KeyA",
    meta: true,
    kitty: ignore,
    legacy: ignore,
  },
  {
    spec: "Modifiers: ctrl+meta(super)+a -> 97;13u (meta reports when combined with ctrl/alt)",
    key: "a",
    code: "KeyA",
    ctrl: true,
    meta: true,
    kitty: send(`${ESC}[97;13u`),
  },
  {
    spec: "Legacy ctrl mapping: ctrl+space -> 32;5u (vs legacy NUL)",
    key: " ",
    code: "Space",
    ctrl: true,
    kitty: send(`${ESC}[32;5u`),
    legacy: send("\x00"),
  },

  // --- C0 controls (spec "C0 controls" table + the Disambiguate exception):
  // Enter / Tab / Backspace keep legacy bytes when UNMODIFIED so `reset` is
  // typeable after a crash; with modifiers they disambiguate to CSI-u. ---
  {
    spec: "C0 exception: Enter unmodified stays 0x0d",
    key: "Enter",
    kitty: send("\r"),
    legacy: send("\r"),
  },
  {
    spec: "C0 exception: shift+Enter -> 13;2u",
    key: "Enter",
    shift: true,
    kitty: send(`${ESC}[13;2u`),
  },
  {
    spec: "C0 exception: Tab unmodified stays 0x09",
    key: "Tab",
    kitty: send("\t"),
    legacy: send("\t"),
  },
  {
    spec: "C0 exception: shift+Tab -> 9;2u (vs legacy CSI Z)",
    key: "Tab",
    shift: true,
    kitty: send(`${ESC}[9;2u`),
    legacy: send(`${ESC}[Z`),
  },
  {
    spec: "C0 exception: Backspace unmodified stays 0x7f",
    key: "Backspace",
    kitty: send("\x7f"),
    legacy: send("\x7f"),
  },
  {
    spec: "C0 exception: ctrl+Backspace -> 127;5u (vs legacy 0x08)",
    key: "Backspace",
    ctrl: true,
    kitty: send(`${ESC}[127;5u`),
    legacy: send("\x08"),
  },

  // --- Functional keys with a letter trailer (arrows, Home, End, F1/F2/F4).
  // Under kitty these are CSI 1;{mod}{letter} (CSI form even for F1-F4, whose
  // legacy form is SS3). Unmodified arrows/Home/End encode identically to legacy
  // (no DECCKM). ---
  {
    spec: "Functional: Up = CSI A",
    key: "ArrowUp",
    kitty: send(`${ESC}[A`),
    legacy: send(`${ESC}[A`),
  },
  {
    spec: "Functional: shift+Up = CSI 1;2A",
    key: "ArrowUp",
    shift: true,
    kitty: send(`${ESC}[1;2A`),
  },
  {
    spec: "Functional: ctrl+Left = CSI 1;5D",
    key: "ArrowLeft",
    ctrl: true,
    kitty: send(`${ESC}[1;5D`),
  },
  { spec: "Functional: Home = CSI H", key: "Home", kitty: send(`${ESC}[H`) },
  { spec: "Functional: ctrl+End = CSI 1;5F", key: "End", ctrl: true, kitty: send(`${ESC}[1;5F`) },
  {
    spec: "Functional: F1 = CSI P (legacy SS3 P)",
    key: "F1",
    kitty: send(`${ESC}[P`),
    legacy: send(`${ESC}OP`),
  },
  { spec: "Functional: F2 = CSI Q", key: "F2", kitty: send(`${ESC}[Q`) },
  { spec: "Functional: F4 = CSI S", key: "F4", kitty: send(`${ESC}[S`) },
  { spec: "Functional: ctrl+F1 = CSI 1;5P", key: "F1", ctrl: true, kitty: send(`${ESC}[1;5P`) },

  // --- Functional keys with a tilde trailer (Insert, Delete, PageUp/Down, F3,
  // F5-F12). F3 is 13~ specifically — it has NO letter form because CSI R
  // collides with the Cursor Position Report (spec note). ---
  { spec: "Functional: Insert = CSI 2~", key: "Insert", kitty: send(`${ESC}[2~`) },
  { spec: "Functional: Delete = CSI 3~", key: "Delete", kitty: send(`${ESC}[3~`) },
  {
    spec: "Functional: shift+Delete = CSI 3;2~",
    key: "Delete",
    shift: true,
    kitty: send(`${ESC}[3;2~`),
  },
  { spec: "Functional: PageUp = CSI 5~", key: "PageUp", kitty: send(`${ESC}[5~`) },
  {
    spec: "Functional: ctrl+PageDown = CSI 6;5~",
    key: "PageDown",
    ctrl: true,
    kitty: send(`${ESC}[6;5~`),
  },
  {
    spec: "Functional: F3 = CSI 13~ (NOT CSI R; legacy SS3 R)",
    key: "F3",
    kitty: send(`${ESC}[13~`),
    legacy: send(`${ESC}OR`),
  },
  { spec: "Functional: shift+F3 = CSI 13;2~", key: "F3", shift: true, kitty: send(`${ESC}[13;2~`) },
  {
    spec: "Functional: F5 = CSI 15~",
    key: "F5",
    kitty: send(`${ESC}[15~`),
    legacy: send(`${ESC}[15~`),
  },
  { spec: "Functional: F12 = CSI 24~", key: "F12", kitty: send(`${ESC}[24~`) },

  // --- F13-F24 (Functional key definitions: F13 57376 .. F24 57387, CSI u
  // form). Legacy covers F13-F20 via xterm's extended tilde codes (25-34, 27
  // and 30 skipped); F21-F24 have NO legacy encoding (xterm stops at F20) and
  // must stay silent there — under kitty all twelve report. ---
  {
    spec: "Functional: F13 = CSI 57376 u (legacy xterm tilde 25~)",
    key: "F13",
    kitty: send(`${ESC}[57376u`),
    legacy: send(`${ESC}[25~`),
  },
  {
    spec: "Functional: ctrl+F13 = CSI 57376;5 u",
    key: "F13",
    ctrl: true,
    kitty: send(`${ESC}[57376;5u`),
    legacy: send(`${ESC}[25;5~`),
  },
  {
    spec: "Functional: F20 = CSI 57383 u (legacy xterm tilde 34~)",
    key: "F20",
    kitty: send(`${ESC}[57383u`),
    legacy: send(`${ESC}[34~`),
  },
  {
    spec: "Functional: F21 = CSI 57384 u (no legacy encoding — silent)",
    key: "F21",
    kitty: send(`${ESC}[57384u`),
    legacy: ignore,
  },
  {
    spec: "Functional: F24 = CSI 57387 u (no legacy encoding — silent)",
    key: "F24",
    kitty: send(`${ESC}[57387u`),
    legacy: ignore,
  },
  {
    spec: "Functional: shift+F24 = CSI 57387;2 u",
    key: "F24",
    shift: true,
    kitty: send(`${ESC}[57387;2u`),
  },

  // --- Keypad (Functional key codes KP_* 57414-57427). Under 0x1 only the
  // NON-TEXT keypad keys (NumLock-off navigation + NumpadEnter) get dedicated
  // KP_ codes; NumLock-on digits stay TEXT (KP_ digit codes 57399-57416 are a
  // report-all 0x8 feature). ev.code identifies the physical keypad key; ev.key
  // its current function. Legacy reports them as their non-keypad equivalents. ---
  {
    spec: "Keypad: KP_UP 57419u (Numpad8, NumLock off) vs legacy CSI A",
    key: "ArrowUp",
    code: "Numpad8",
    kitty: send(`${ESC}[57419u`),
    legacy: send(`${ESC}[A`),
  },
  {
    spec: "Keypad: KP_DOWN 57420u",
    key: "ArrowDown",
    code: "Numpad2",
    kitty: send(`${ESC}[57420u`),
  },
  {
    spec: "Keypad: KP_LEFT 57417u",
    key: "ArrowLeft",
    code: "Numpad4",
    kitty: send(`${ESC}[57417u`),
  },
  {
    spec: "Keypad: KP_RIGHT 57418u",
    key: "ArrowRight",
    code: "Numpad6",
    kitty: send(`${ESC}[57418u`),
  },
  { spec: "Keypad: KP_HOME 57423u", key: "Home", code: "Numpad7", kitty: send(`${ESC}[57423u`) },
  { spec: "Keypad: KP_END 57424u", key: "End", code: "Numpad1", kitty: send(`${ESC}[57424u`) },
  {
    spec: "Keypad: KP_PAGE_UP 57421u",
    key: "PageUp",
    code: "Numpad9",
    kitty: send(`${ESC}[57421u`),
  },
  {
    spec: "Keypad: KP_PAGE_DOWN 57422u",
    key: "PageDown",
    code: "Numpad3",
    kitty: send(`${ESC}[57422u`),
  },
  {
    spec: "Keypad: KP_INSERT 57425u",
    key: "Insert",
    code: "Numpad0",
    kitty: send(`${ESC}[57425u`),
  },
  {
    spec: "Keypad: KP_DELETE 57426u",
    key: "Delete",
    code: "NumpadDecimal",
    kitty: send(`${ESC}[57426u`),
  },
  {
    spec: "Keypad: KP_BEGIN 57427u (Numpad5, NumLock off)",
    key: "Clear",
    code: "Numpad5",
    kitty: send(`${ESC}[57427u`),
  },
  {
    spec: "Keypad: KP_ENTER 57414u (distinct from main Enter) vs legacy 0x0d",
    key: "Enter",
    code: "NumpadEnter",
    kitty: send(`${ESC}[57414u`),
    legacy: send("\r"),
  },
  {
    spec: "Keypad: ctrl+KP_UP 57419;5u",
    key: "ArrowUp",
    code: "Numpad8",
    ctrl: true,
    kitty: send(`${ESC}[57419;5u`),
  },
  {
    spec: "Keypad: NumLock-on digit stays text (KP_5 is 0x8-only)",
    key: "5",
    code: "Numpad5",
    kitty: ignore,
  },
  {
    spec: "Keypad: ctrl+numpad-digit disambiguates via ASCII codepoint 53;5u (not KP_5)",
    key: "5",
    code: "Numpad5",
    ctrl: true,
    kitty: send(`${ESC}[53;5u`),
  },
];
