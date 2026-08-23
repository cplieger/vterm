// bindMobileToolbar — the on-screen mobile keyboard toolbar widget
// (toolbar.ts, split out of keyboard.ts 2026-07). Covers button wiring,
// sticky-Ctrl semantics, dispose, the DECCKM/kitty-aware arrow + Escape
// encodings, and the equivalence fixtures pinning the toolbar's bytes to the
// physical-key encoder byte-for-byte (the anti-drift guard that motivated the
// shared logical-key homes in keyboard.ts).

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { bindMobileToolbar } from "./toolbar.js";
import { mapKeyboardEvent as mapKeyboardEventRaw, type KeyboardResult } from "./keyboard.js";
import * as modes from "./modes.js";

// These tests drive the module-singleton modes (set via modes.setModes),
// the same source the toolbar reads; mapKeyboardEvent takes modes explicitly.
const mapKeyboardEvent = (e: KeyboardEvent): KeyboardResult => mapKeyboardEventRaw(e, modes);

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

beforeEach(() => {
  // Defaults: bracketed paste on (irrelevant here), app cursor off, no kitty.
  // ALL nine params explicit: setModes leaves omitted optionals unchanged, and
  // vitest runs with isolate:false, so a stale kitty flag from another test
  // (or left FOR another test file sharing this worker) is a real hazard.
  modes.setModes(true, false, false, false, 0, false, false, false, 0);
  document.body.innerHTML = "";
});

afterEach(() => {
  // Leave the shared module-global modes clean for whichever test file runs
  // next on this worker (isolate:false): several fixtures here arm the kitty
  // disambiguate flag, and keypad/keyboard suites assume it is off.
  modes.setModes(true, false, false, false, 0, false, false, false, 0);
});
// ===========================================================================
// bindMobileToolbar — on-screen mobile keyboard toolbar wiring.
// (Not part of the input-encoding matrix, but exported by keyboard.ts and
// verified here; DECCKM-aware arrows share modes.ts with mapKeyboardEvent.)
// ===========================================================================

function makeToolbar(): {
  toolbar: HTMLElement;
  buttons: Record<string, HTMLElement>;
} {
  const toolbar = document.createElement("div");
  toolbar.className = "kb-toolbar";
  const buttons: Record<string, HTMLElement> = {};
  const ids = [
    "kb-toggle",
    "kb-ctrl",
    "kb-up",
    "kb-down",
    "kb-left",
    "kb-right",
    "kb-tab",
    "kb-enter",
    "kb-esc",
  ];
  for (const id of ids) {
    const b = document.createElement("button");
    b.id = id;
    toolbar.appendChild(b);
    buttons[id] = b;
  }
  document.body.appendChild(toolbar);
  return { toolbar, buttons };
}

function fireDown(el: HTMLElement): { defaultPrevented: boolean } {
  const ev = new Event("pointerdown", { cancelable: true, bubbles: true });
  el.dispatchEvent(ev);
  return { defaultPrevented: ev.defaultPrevented };
}

describe("bindMobileToolbar: happy path", () => {
  let send: ReturnType<typeof vi.fn<(bytes: string) => void>>;
  let fixture: ReturnType<typeof makeToolbar>;

  beforeEach(() => {
    document.body.innerHTML = "";
    send = vi.fn<(bytes: string) => void>();
    fixture = makeToolbar();
  });

  it("kb-up/down/right/left send arrow CSI sequences (default DECCKM off)", () => {
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });

    fireDown(fixture.buttons["kb-up"]!);
    fireDown(fixture.buttons["kb-down"]!);
    fireDown(fixture.buttons["kb-right"]!);
    fireDown(fixture.buttons["kb-left"]!);

    expect(send).toHaveBeenNthCalledWith(1, "\x1b[A");
    expect(send).toHaveBeenNthCalledWith(2, "\x1b[B");
    expect(send).toHaveBeenNthCalledWith(3, "\x1b[C");
    expect(send).toHaveBeenNthCalledWith(4, "\x1b[D");

    ctrl.dispose();
  });

  it("kb-tab/enter/esc send the canonical bytes", () => {
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });

    fireDown(fixture.buttons["kb-tab"]!);
    fireDown(fixture.buttons["kb-enter"]!);
    fireDown(fixture.buttons["kb-esc"]!);

    expect(send).toHaveBeenNthCalledWith(1, "\t");
    expect(send).toHaveBeenNthCalledWith(2, "\r");
    expect(send).toHaveBeenNthCalledWith(3, "\x1b");

    ctrl.dispose();
  });

  it("preventDefault is called on every toolbar press", () => {
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    for (const id of [
      "kb-toggle",
      "kb-ctrl",
      "kb-up",
      "kb-down",
      "kb-left",
      "kb-right",
      "kb-tab",
      "kb-enter",
      "kb-esc",
    ]) {
      const r = fireDown(fixture.buttons[id]!);
      expect(r.defaultPrevented).toBe(true);
    }
    ctrl.dispose();
  });

  it("kb-toggle toggles .collapsed on the toolbar; does NOT clear armed Ctrl", () => {
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    ctrl.setCtrlArmed(true);
    expect(fixture.toolbar.classList.contains("collapsed")).toBe(false);

    fireDown(fixture.buttons["kb-toggle"]!);
    expect(fixture.toolbar.classList.contains("collapsed")).toBe(true);
    expect(ctrl.isCtrlArmed()).toBe(true); // toggle doesn't disarm

    fireDown(fixture.buttons["kb-toggle"]!);
    expect(fixture.toolbar.classList.contains("collapsed")).toBe(false);
    expect(ctrl.isCtrlArmed()).toBe(true);

    ctrl.dispose();
  });

  it("kb-ctrl toggles armed state and does NOT call send", () => {
    const onChange = vi.fn();
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send, onCtrlChange: onChange });

    expect(ctrl.isCtrlArmed()).toBe(false);
    fireDown(fixture.buttons["kb-ctrl"]!);
    expect(ctrl.isCtrlArmed()).toBe(true);
    expect(fixture.buttons["kb-ctrl"]!.classList.contains("armed")).toBe(true);
    expect(fixture.buttons["kb-ctrl"]!.getAttribute("aria-pressed")).toBe("true");
    expect(onChange).toHaveBeenLastCalledWith(true);

    fireDown(fixture.buttons["kb-ctrl"]!);
    expect(ctrl.isCtrlArmed()).toBe(false);
    expect(fixture.buttons["kb-ctrl"]!.classList.contains("armed")).toBe(false);
    expect(fixture.buttons["kb-ctrl"]!.getAttribute("aria-pressed")).toBe("false");
    expect(onChange).toHaveBeenLastCalledWith(false);

    expect(send).not.toHaveBeenCalled();

    ctrl.dispose();
  });

  it("arrow / Tab / Enter / Esc presses clear armed Ctrl", () => {
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    for (const id of ["kb-up", "kb-down", "kb-left", "kb-right", "kb-tab", "kb-enter", "kb-esc"]) {
      ctrl.setCtrlArmed(true);
      fireDown(fixture.buttons[id]!);
      expect(ctrl.isCtrlArmed()).toBe(false);
    }
    ctrl.dispose();
  });

  it("missing toolbar buttons are skipped silently", () => {
    const empty = document.createElement("div");
    document.body.appendChild(empty);
    const ctrl = bindMobileToolbar({ toolbar: empty, send });
    expect(() => ctrl.applyStickyCtrl("a")).not.toThrow();
    ctrl.dispose();
    expect(send).not.toHaveBeenCalled();
  });
});

describe("bindMobileToolbar: applyStickyCtrl", () => {
  let send: ReturnType<typeof vi.fn<(bytes: string) => void>>;
  let fixture: ReturnType<typeof makeToolbar>;

  beforeEach(() => {
    document.body.innerHTML = "";
    send = vi.fn<(bytes: string) => void>();
    fixture = makeToolbar();
  });

  it("not armed → returns text unchanged", () => {
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    expect(ctrl.applyStickyCtrl("a")).toBe("a");
    expect(ctrl.applyStickyCtrl("hello")).toBe("hello");
    expect(ctrl.isCtrlArmed()).toBe(false);
    ctrl.dispose();
  });

  it("armed + 1-char input → ctrl byte and disarms", () => {
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    ctrl.setCtrlArmed(true);
    expect(ctrl.applyStickyCtrl("c")).toBe("\x03");
    expect(ctrl.isCtrlArmed()).toBe(false);
    ctrl.dispose();
  });

  it("armed + 1-char unmapped input → original char and disarms", () => {
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    ctrl.setCtrlArmed(true);
    expect(ctrl.applyStickyCtrl("é")).toBe("é");
    expect(ctrl.isCtrlArmed()).toBe(false);
    ctrl.dispose();
  });

  it("armed + paste (multi-char) → text unchanged and disarms", () => {
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    ctrl.setCtrlArmed(true);
    expect(ctrl.applyStickyCtrl("hello")).toBe("hello");
    expect(ctrl.isCtrlArmed()).toBe(false);
    ctrl.dispose();
  });
});

describe("bindMobileToolbar: dispose", () => {
  it("removes all listeners — pointerdown after dispose does NOT call send", () => {
    document.body.innerHTML = "";
    const send = vi.fn<(bytes: string) => void>();
    const fixture = makeToolbar();

    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    ctrl.dispose();

    for (const id of ["kb-up", "kb-down", "kb-left", "kb-right", "kb-tab", "kb-enter", "kb-esc"]) {
      fireDown(fixture.buttons[id]!);
    }
    expect(send).not.toHaveBeenCalled();
  });

  it("clears armed state and resets kb-ctrl visuals", () => {
    document.body.innerHTML = "";
    const send = vi.fn<(bytes: string) => void>();
    const fixture = makeToolbar();

    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    ctrl.setCtrlArmed(true);
    expect(fixture.buttons["kb-ctrl"]!.classList.contains("armed")).toBe(true);
    expect(fixture.buttons["kb-ctrl"]!.getAttribute("aria-pressed")).toBe("true");

    ctrl.dispose();
    expect(ctrl.isCtrlArmed()).toBe(false);
    expect(fixture.buttons["kb-ctrl"]!.classList.contains("armed")).toBe(false);
    expect(fixture.buttons["kb-ctrl"]!.getAttribute("aria-pressed")).toBe("false");
  });

  it("is idempotent", () => {
    document.body.innerHTML = "";
    const send = vi.fn<(bytes: string) => void>();
    const fixture = makeToolbar();

    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    expect(() => {
      ctrl.dispose();
      ctrl.dispose();
    }).not.toThrow();
  });
});

describe("bindMobileToolbar: DECCKM-aware arrows", () => {
  it("application-cursor mode → SS3 sequences (\\x1bOA..D)", () => {
    document.body.innerHTML = "";
    const send = vi.fn<(bytes: string) => void>();
    const fixture = makeToolbar();

    modes.setModes(true /* bracketed */, true /* app cursor */);
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });

    fireDown(fixture.buttons["kb-up"]!);
    fireDown(fixture.buttons["kb-down"]!);
    fireDown(fixture.buttons["kb-right"]!);
    fireDown(fixture.buttons["kb-left"]!);

    expect(send).toHaveBeenNthCalledWith(1, "\x1bOA");
    expect(send).toHaveBeenNthCalledWith(2, "\x1bOB");
    expect(send).toHaveBeenNthCalledWith(3, "\x1bOC");
    expect(send).toHaveBeenNthCalledWith(4, "\x1bOD");

    ctrl.dispose();
  });

  it("default mode → CSI sequences (\\x1b[A..D])", () => {
    document.body.innerHTML = "";
    const send = vi.fn<(bytes: string) => void>();
    const fixture = makeToolbar();

    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });

    fireDown(fixture.buttons["kb-up"]!);
    expect(send).toHaveBeenLastCalledWith("\x1b[A");

    ctrl.dispose();
  });

  it("mode is consulted on each press, not at bind time", () => {
    document.body.innerHTML = "";
    const send = vi.fn<(bytes: string) => void>();
    const fixture = makeToolbar();

    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    fireDown(fixture.buttons["kb-up"]!);
    expect(send).toHaveBeenLastCalledWith("\x1b[A");

    modes.setModes(true, true);
    fireDown(fixture.buttons["kb-up"]!);
    expect(send).toHaveBeenLastCalledWith("\x1bOA");

    ctrl.dispose();
  });
});

describe("bindMobileToolbar: applyStickyCtrl under kitty disambiguate", () => {
  // The toolbar path must be byte-identical with the physical-keyboard
  // encoder: the spec's unshifted-key rule folds a shifted glyph into its
  // base codepoint + the shift modifier bit (judgement finding: ':' encoded
  // 58;5u from the toolbar vs 59;6u from a real keyboard).
  let send: ReturnType<typeof vi.fn<(bytes: string) => void>>;
  let fixture: ReturnType<typeof makeToolbar>;

  beforeEach(() => {
    document.body.innerHTML = "";
    send = vi.fn<(bytes: string) => void>();
    fixture = makeToolbar();
    modes.setModes(false, false, false, false, 0, false, false, false, 1); // kitty 0x1
  });

  afterEach(() => {
    modes.setModes(false, false, false, false, 0, false, false, false, 0);
  });

  it("unshifted char → CSI cp;5u matching the physical path", () => {
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    ctrl.setCtrlArmed(true);
    expect(ctrl.applyStickyCtrl("s")).toBe("\x1b[115;5u");
    ctrl.dispose();
  });

  it("shifted symbol → unshifted base codepoint + shift modifier (':' = 59;6u, not 58;5u)", () => {
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    ctrl.setCtrlArmed(true);
    expect(ctrl.applyStickyCtrl(":")).toBe("\x1b[59;6u");
    ctrl.dispose();
  });

  it("uppercase letter → lowercase codepoint + shift modifier", () => {
    const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });
    ctrl.setCtrlArmed(true);
    expect(ctrl.applyStickyCtrl("A")).toBe("\x1b[97;6u");
    ctrl.dispose();
  });
});

// ===========================================================================
// Toolbar <-> physical-key equivalence fixtures.
// The mobile toolbar's arrow/ESC buttons and the physical-key encoder share
// one logical-key home (plainCursorKeySeq / plainEscapeSeq in keyboard.ts);
// these fixtures pin the two paths to IDENTICAL bytes across every mode state
// so an edit to either side cannot silently drift them apart (the pre-2026-07
// state: three independent decisions — csiLetter, the toolbar's arrowSeq, and
// a hand-inlined ESC[27u literal).
// ===========================================================================

describe("toolbar buttons emit exactly the physical-key bytes (equivalence fixtures)", () => {
  const modeStates: {
    name: string;
    appCursor: boolean;
    kittyFlags: number;
  }[] = [
    { name: "legacy defaults", appCursor: false, kittyFlags: 0 },
    { name: "DECCKM application cursor", appCursor: true, kittyFlags: 0 },
    { name: "kitty disambiguate (supersedes DECCKM)", appCursor: true, kittyFlags: 1 },
  ];
  const arrows: { btn: string; key: string }[] = [
    { btn: "kb-up", key: "ArrowUp" },
    { btn: "kb-down", key: "ArrowDown" },
    { btn: "kb-left", key: "ArrowLeft" },
    { btn: "kb-right", key: "ArrowRight" },
  ];

  for (const state of modeStates) {
    it(`arrows + Escape match under ${state.name}`, () => {
      modes.setModes(true, state.appCursor, false, false, 0, false, false, false, state.kittyFlags);
      document.body.innerHTML = "";
      const send = vi.fn<(bytes: string) => void>();
      const fixture = makeToolbar();
      const ctrl = bindMobileToolbar({ toolbar: fixture.toolbar, send });

      for (const { btn, key } of arrows) {
        send.mockClear();
        fireDown(fixture.buttons[btn]!);
        const physical = sent(mapKeyboardEvent(ev({ key })));
        expect(send, `${btn} vs ${key} (${state.name})`).toHaveBeenCalledWith(physical);
      }

      send.mockClear();
      fireDown(fixture.buttons["kb-esc"]!);
      const physicalEsc = sent(mapKeyboardEvent(ev({ key: "Escape" })));
      expect(send, `kb-esc vs Escape (${state.name})`).toHaveBeenCalledWith(physicalEsc);

      ctrl.dispose();
    });
  }
});
