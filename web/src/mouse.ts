// Mouse event encoding for terminal input (SGR 1006 protocol).
//
// Encodes mouse events as SGR sequences: ESC[<code;col;rowM (press/move)
// or ESC[<code;col;rowm (release). Coordinates are 1-based.
//
// Button encoding (matches xterm/xterm.js):
//   0=left, 1=middle, 2=right, 64=wheel-up, 65=wheel-down
//   +4=shift, +8=alt, +16=ctrl, +32=motion
//
// Focus events: ESC[I (focus in), ESC[O (focus out).
//
// Out-of-scope (consumed but not implemented):
//   - X10 mouse mode (mode 9): not supported.
//   - urxvt encoding (mode 1015): not supported.
//   - DEFAULT encoding (raw bytes): not supported; only SGR 1006.

import { getMouseMode, isMouseSGR, isMousePixels, isFocusReporting } from "./modes.js";

const ESC = "\x1b";

/** Encode a mouse button code for SGR protocol. */
function buttonCode(
  button: number,
  motion: boolean,
  shift: boolean,
  alt: boolean,
  ctrl: boolean,
): number {
  let code = button;
  if (shift) {
    code |= 4;
  }
  if (alt) {
    code |= 8;
  }
  if (ctrl) {
    code |= 16;
  }
  if (motion) {
    code |= 32;
  }
  return code;
}

/**
 * Encode a mouse event into an SGR 1006 escape sequence ready to send to the
 * terminal as input. The returned string starts with ESC[< and ends with `M`
 * for press/move or `m` for release.
 *
 * @param code  Button code returned from `buttonCode` (button + modifier bits).
 * @param col   1-based column.
 * @param row   1-based row.
 * @param release `true` for release events (terminator `m`), `false` otherwise.
 */
export function encodeSGR(code: number, col: number, row: number, release: boolean): string {
  const final = release ? "m" : "M";
  return `${ESC}[<${code};${col};${row}${final}`;
}

/** Map DOM button index to xterm button number. */
function domButtonToXterm(button: number): number {
  // DOM MouseEvent.button: 0=left, 1=middle, 2=right, 3=back, 4=forward.
  // xterm: 0/1/2 for left/middle/right; the "additional buttons" (X11 8-11,
  // reached via DOM back/forward) use the +128 extended-button encoding, so
  // back (DOM 3 = X11 8) → 128 and forward (DOM 4 = X11 9) → 129.
  if (button <= 2) {
    return button;
  }
  return 128 + (button - 3);
}

/**
 * Adapter the consumer wires into `init` so the mouse module can: send encoded
 * input bytes to the server, query current cell pixel size for hit-testing,
 * and access the terminal DOM element to attach listeners to.
 */
export interface MouseInputHandler {
  /** Sends raw input bytes (or escape sequences) to the server. */
  send: (data: string) => void;
  /** Returns the current cell pixel size for hit-testing pointer coordinates. */
  cellSize: () => { width: number; height: number };
  /** Returns the terminal DOM element to attach mouse listeners to. */
  termElement: () => HTMLElement;
}

let handler: MouseInputHandler | null = null;
// True while a Shift-initiated press is in flight. Shift+click/drag is the
// xterm convention for "bypass application mouse tracking": the gesture is
// reserved for the browser's native text selection instead of being reported
// to (and preventDefault-ed away from) the TUI. Tracked from press to release
// so the move/up of a bypassed drag stay bypassed even if Shift is lifted
// mid-drag.
let shiftBypass = false;
// Last motion report sent, for same-cell dedup: a drag within one cell fires
// many DOM mousemove events that would all encode to the identical SGR
// sequence and flood the PTY (xterm.js applies the same suppression). Cleared
// on press/release so the first motion of a new gesture always reports.
let lastMotion = "";
// The element the current init() attached to, so a re-init or dispose can
// detach the exact listener set (addEventListener dedups identical
// registrations on the SAME element, but a re-init on a NEW element would
// otherwise leak the old one's listeners).
let attachedEl: HTMLElement | null = null;

function detach(): void {
  if (!attachedEl) {
    return;
  }
  attachedEl.removeEventListener("mousedown", onMouseDown);
  attachedEl.removeEventListener("mouseup", onMouseUp);
  attachedEl.removeEventListener("mousemove", onMouseMove);
  attachedEl.removeEventListener("wheel", onWheel);
  attachedEl.removeEventListener("focusin", onFocusIn);
  attachedEl.removeEventListener("focusout", onFocusOut);
  attachedEl = null;
}

/**
 * Initialize the mouse module by attaching pointer/wheel/focus listeners to
 * the terminal element. Listeners gate on the active mouse mode + SGR 1006
 * encoding; they are no-ops when the server hasn't enabled mouse tracking.
 *
 * Returns an idempotent disposer that detaches the listeners and resets the
 * module state, so a consumer can tear the terminal down (or re-init on a
 * different element) without leaking listeners. Re-initializing without
 * disposing first detaches the previous element automatically.
 */
export function init(h: MouseInputHandler): () => void {
  detach(); // self-heal a re-init on a different element
  handler = h;
  shiftBypass = false;
  lastMotion = "";
  const el = h.termElement();
  attachedEl = el;
  el.addEventListener("mousedown", onMouseDown);
  el.addEventListener("mouseup", onMouseUp);
  el.addEventListener("mousemove", onMouseMove);
  el.addEventListener("wheel", onWheel, { passive: false });
  el.addEventListener("focusin", onFocusIn);
  el.addEventListener("focusout", onFocusOut);
  return function dispose(): void {
    if (attachedEl !== el) {
      return; // superseded by a later init — nothing of ours left to detach
    }
    detach();
    handler = null;
    shiftBypass = false;
    lastMotion = "";
  };
}

// pixelToCell returns the coordinate pair to report for a mouse event. Normally
// that's the 1-based cell column/row; under DEC 1016 (SGR-pixels) it's instead
// the 1-based pixel offset within the terminal element (same SGR grammar, so
// encodeSGR is reused unchanged). The {col,row} field names carry whichever the
// active mode reports.
function pixelToCell(e: MouseEvent): { col: number; row: number } | null {
  if (!handler) {
    return null;
  }
  const el = handler.termElement();
  const rect = el.getBoundingClientRect();
  if (isMousePixels()) {
    const px = Math.round(e.clientX - rect.left);
    const py = Math.round(e.clientY - rect.top);
    if (px < 0 || py < 0) {
      return null;
    }
    return { col: px + 1, row: py + 1 }; // 1-based pixel offsets
  }
  const { width, height } = handler.cellSize();
  if (width <= 0 || height <= 0) {
    return null;
  }
  const col = Math.floor((e.clientX - rect.left) / width) + 1; // 1-based
  const row = Math.floor((e.clientY - rect.top) / height) + 1; // 1-based
  if (col < 1 || row < 1) {
    return null;
  }
  return { col, row };
}

function onMouseDown(e: MouseEvent): void {
  const mode = getMouseMode();
  if (mode === 0 || !(isMouseSGR() || isMousePixels()) || !handler) {
    return;
  }
  if (e.shiftKey) {
    // Shift+press: leave the whole gesture to the browser (native selection).
    shiftBypass = true;
    return;
  }
  shiftBypass = false;
  lastMotion = ""; // new gesture: next motion always reports
  const pos = pixelToCell(e);
  if (!pos) {
    return;
  }
  const code = buttonCode(domButtonToXterm(e.button), false, e.shiftKey, e.altKey, e.ctrlKey);
  handler.send(encodeSGR(code, pos.col, pos.row, false));
  e.preventDefault();
}

function onMouseUp(e: MouseEvent): void {
  const mode = getMouseMode();
  if (mode === 0 || !(isMouseSGR() || isMousePixels()) || !handler) {
    return;
  }
  if (shiftBypass) {
    // Release of a Shift-initiated (native-selection) gesture: report nothing.
    shiftBypass = false;
    return;
  }
  const pos = pixelToCell(e);
  if (!pos) {
    return;
  }
  const btn = domButtonToXterm(e.button);
  const code = buttonCode(btn, false, e.shiftKey, e.altKey, e.ctrlKey);
  handler.send(encodeSGR(code, pos.col, pos.row, true));
  lastMotion = ""; // gesture over: next motion always reports
  e.preventDefault();
}

function onMouseMove(e: MouseEvent): void {
  const mode = getMouseMode();
  if (mode === 0 || !(isMouseSGR() || isMousePixels()) || !handler) {
    return;
  }
  if (shiftBypass && e.buttons) {
    return; // a Shift-initiated drag is a native selection; don't report it
  }
  // mode 1000: no motion events
  // mode 1002: motion only while button held (drag)
  // mode 1003: all motion events
  if (mode === 1000) {
    return;
  }
  if (mode === 1002 && !e.buttons) {
    return;
  }
  const pos = pixelToCell(e);
  if (!pos) {
    return;
  }
  // No button held (bare motion in mode 1003) reports xterm's "no button"
  // code 3, not 0 — code 0 would be read as a left-button drag during hover.
  const btn = e.buttons ? (e.buttons & 1 ? 0 : e.buttons & 4 ? 1 : e.buttons & 2 ? 2 : 0) : 3;
  const code = buttonCode(btn, true, e.shiftKey, e.altKey, e.ctrlKey);
  const seq = encodeSGR(code, pos.col, pos.row, false);
  if (seq === lastMotion) {
    return; // same cell, same buttons/modifiers: suppress the duplicate report
  }
  lastMotion = seq;
  handler.send(seq);
}

function onWheel(e: WheelEvent): void {
  const mode = getMouseMode();
  if (mode === 0 || !(isMouseSGR() || isMousePixels()) || !handler) {
    return;
  }
  const pos = pixelToCell(e);
  if (!pos) {
    return;
  }
  // Wheel up = button 64, wheel down = button 65. A gesture with NO vertical
  // component — a horizontal trackpad swipe, or shift-wheel where the browser
  // routes the delta to deltaX — has no wheel button to report: xterm.js's
  // reference encoder returns before encoding when the vertical delta is zero,
  // while its wheel handler still calls preventDefault unconditionally. So
  // parity is "report nothing, still swallow the gesture": a terminal owns the
  // wheel, and letting a sideways swipe through would scroll the page instead.
  // Reporting 65 here (the pre-fix behavior, `deltaY < 0 ? 64 : 65` with no
  // zero case) scrolled the TUI DOWN on a sideways gesture.
  if (e.deltaY !== 0) {
    const btn = e.deltaY < 0 ? 64 : 65;
    const code = buttonCode(btn, false, e.shiftKey, e.altKey, e.ctrlKey);
    handler.send(encodeSGR(code, pos.col, pos.row, false));
  }
  e.preventDefault();
}

function onFocusIn(): void {
  if (!isFocusReporting() || !handler) {
    return;
  }
  handler.send(`${ESC}[I`);
}

function onFocusOut(): void {
  if (!isFocusReporting() || !handler) {
    return;
  }
  handler.send(`${ESC}[O`);
}
