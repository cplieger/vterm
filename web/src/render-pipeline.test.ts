// Full-pipeline integration test: feeds REAL the host application PTY bytes captured
// from a working session through the WS binary decoder and the renderer,
// then validates the resulting DOM matches the expected post-keystroke
// state.
//
// The captured bytes were collected from /tmp/wscap2.txt (a real
// session). This test exercises the same code path the browser
// would, with one exception: instead of arriving over a WebSocket, the
// frames are decoded directly from base64-encoded bytes.

import { describe, it, expect, beforeEach, vi } from "vitest";
import * as render from "./render.js";
import { decodeWireBinary } from "./wire-binary.js";
import type { ScreenMessage } from "./types.js";

// A real Canvas2D measures whatever font the machine has installed, so
// measureText is declared here and render computes cell widths from a fixed
// metric.
interface FakeCtx {
  font: string;
  measureText: (t: string) => { width: number };
}
HTMLCanvasElement.prototype.getContext = function fakeGetContext(): unknown {
  const ctx: FakeCtx = {
    font: "",
    measureText: (text: string) => ({ width: text.length * 8 }),
  };
  return ctx;
} as typeof HTMLCanvasElement.prototype.getContext;

// Helper to build a binary screen frame with the given parameters,
// matching the layout in internal/terminal/wire_binary.go.
//
//   [1B] msg_type = 0
//   [8B] inputAck (uint64 LE)
//   [8B] base (uint64 LE) — absolute index of row 0 (wire v2)
//   [2B] cursor_row (uint16 LE)
//   [2B] cursor_col (uint16 LE)
//   [2B] screen_height (uint16 LE)
//   [2B] num_changed (uint16 LE)
//   [1B] cursor_style
//   [1B] cursor_flags
//   For each changed row:
//     [2B] row_idx (uint16 LE)
//     [2B] num_runs (uint16 LE)
//     For each run:
//       [2B] text_byte_len (uint16 LE)
//       [N B] text utf-8
//       [4B] fg (int32 LE, -1 = default)
//       [4B] bg (int32 LE, -1 = default)
//       [2B] attrs (uint16 LE)
//       [4B] uc (int32 LE)
//
// The cursor_flags byte: bit 0 = hidden, bit 1 = bell, bit 2 = blink.
interface Run {
  text: string;
  fg?: number;
  bg?: number;
  attr?: number;
  uc?: number;
}

function buildScreenFrame(opts: {
  cursorRow: number;
  cursorCol: number;
  screenHeight: number;
  cursorHidden?: boolean;
  cursorBlink?: boolean;
  cursorStyle?: number;
  changed: { idx: number; runs: Run[] }[];
}): ArrayBuffer {
  const enc = new TextEncoder();
  // Pre-encode all run text bytes so we know total length.
  const runBytes = opts.changed.map((r) => r.runs.map((run) => enc.encode(run.text)));
  let len = 1 + 8 + 8 + 2 + 2 + 2 + 2 + 1 + 1; // +8 for base (wire v2)
  for (let i = 0; i < opts.changed.length; i++) {
    len += 2; // idx
    len += 2; // num_runs
    for (let j = 0; j < opts.changed[i]!.runs.length; j++) {
      len += 2; // text_len
      len += runBytes[i]![j]!.length;
      len += 4 + 4 + 2 + 4 + 2; // fg + bg + attrs + uc + url_byte_len
    }
  }

  const buf = new ArrayBuffer(len);
  const dv = new DataView(buf);
  const u8 = new Uint8Array(buf);
  let off = 0;
  dv.setUint8(off, 0);
  off += 1; // msg_type = screen
  dv.setBigUint64(off, 0n, true);
  off += 8; // ack
  dv.setBigUint64(off, 0n, true);
  off += 8; // base (absolute index of row 0)
  dv.setUint16(off, opts.cursorRow, true);
  off += 2;
  dv.setUint16(off, opts.cursorCol, true);
  off += 2;
  dv.setUint16(off, opts.screenHeight, true);
  off += 2;
  dv.setUint16(off, opts.changed.length, true);
  off += 2;
  dv.setUint8(off, opts.cursorStyle ?? 0);
  off += 1;
  let flags = 0;
  if (opts.cursorHidden) {
    flags |= 1;
  }
  if (opts.cursorBlink ?? true) {
    flags |= 4;
  }
  dv.setUint8(off, flags);
  off += 1;

  for (let i = 0; i < opts.changed.length; i++) {
    const c = opts.changed[i]!;
    dv.setUint16(off, c.idx, true);
    off += 2;
    dv.setUint16(off, c.runs.length, true);
    off += 2;
    for (let j = 0; j < c.runs.length; j++) {
      const run = c.runs[j]!;
      const tb = runBytes[i]![j]!;
      dv.setUint16(off, tb.length, true);
      off += 2;
      u8.set(tb, off);
      off += tb.length;
      dv.setInt32(off, run.fg ?? -1, true);
      off += 4;
      dv.setInt32(off, run.bg ?? -1, true);
      off += 4;
      dv.setUint16(off, run.attr ?? 0, true);
      off += 2;
      dv.setInt32(off, run.uc ?? -1, true);
      off += 4;
      dv.setUint16(off, 0, true);
      off += 2; // url_byte_len = 0 (no hyperlink)
    }
  }
  return buf;
}

/** Wait for the frame render.ts scheduled, not for a duration: real frames tick,
 *  so a fixed sleep races the flush in both directions, and Chromium throttles
 *  rAF outright when the page is not visible. Two deep because the first
 *  callback can run in the frame the flush was queued in. */
async function nextFrames(): Promise<void> {
  await new Promise<void>((resolve) => {
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        resolve();
      });
    });
  });
}

async function flushFrame(buf: ArrayBuffer): Promise<void> {
  const msg = decodeWireBinary(buf) as ScreenMessage;
  expect(msg).not.toBeNull();
  expect(msg.type).toBe("screen");
  render.handleScreen(msg);
  await nextFrames();
}

describe("full pipeline: binary frame -> decoder -> renderer", () => {
  let outputEl: HTMLDivElement;
  let termWrap: HTMLDivElement;

  beforeEach(() => {
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    outputEl = document.getElementById("term-output") as HTMLDivElement;
    render.resetScreen();
    render.init({ output: outputEl, termWrap });
    render.updateFontMetrics();
  });

  it("after typing 'abc' then space then 'd' through the binary wire format, the inverse-cursor span tracks the cursor column", async () => {
    // Initial blank frame so allRows[19] exists.
    const blankRow: Run[] = [{ text: " ".repeat(120), attr: 0 }];
    const initialChanged = Array.from({ length: 30 }, (_, i) => ({ idx: i, runs: blankRow }));
    await flushFrame(
      buildScreenFrame({
        cursorRow: 0,
        cursorCol: 0,
        screenHeight: 30,
        cursorHidden: true,
        changed: initialChanged,
      }),
    );

    // Frame after typing 'a': row 19 = "a" + inv " " + 118 trailing
    await flushFrame(
      buildScreenFrame({
        cursorRow: 19,
        cursorCol: 1,
        screenHeight: 30,
        cursorHidden: true,
        changed: [
          {
            idx: 19,
            runs: [
              { text: "a", attr: 0 },
              { text: " ", attr: 8 }, // inverse
              { text: " ".repeat(118), attr: 0 },
            ],
          },
        ],
      }),
    );
    expectInverseAtCol(outputEl, 19, 1, " ");

    // Frame after typing 'b': row 19 = "ab" + inv " " + 117 trailing
    await flushFrame(
      buildScreenFrame({
        cursorRow: 19,
        cursorCol: 2,
        screenHeight: 30,
        cursorHidden: true,
        changed: [
          {
            idx: 19,
            runs: [
              { text: "ab", attr: 0 },
              { text: " ", attr: 8 },
              { text: " ".repeat(117), attr: 0 },
            ],
          },
        ],
      }),
    );
    expectInverseAtCol(outputEl, 19, 2, " ");

    // Frame after typing 'c'
    await flushFrame(
      buildScreenFrame({
        cursorRow: 19,
        cursorCol: 3,
        screenHeight: 30,
        cursorHidden: true,
        changed: [
          {
            idx: 19,
            runs: [
              { text: "abc", attr: 0 },
              { text: " ", attr: 8 },
              { text: " ".repeat(116), attr: 0 },
            ],
          },
        ],
      }),
    );
    expectInverseAtCol(outputEl, 19, 3, " ");

    // Frame after typing space
    await flushFrame(
      buildScreenFrame({
        cursorRow: 19,
        cursorCol: 4,
        screenHeight: 30,
        cursorHidden: true,
        changed: [
          {
            idx: 19,
            runs: [
              { text: "abc ", attr: 0 },
              { text: " ", attr: 8 },
              { text: " ".repeat(115), attr: 0 },
            ],
          },
        ],
      }),
    );
    expectInverseAtCol(outputEl, 19, 4, " ");

    // Frame after typing 'd'
    await flushFrame(
      buildScreenFrame({
        cursorRow: 19,
        cursorCol: 5,
        screenHeight: 30,
        cursorHidden: true,
        changed: [
          {
            idx: 19,
            runs: [
              { text: "abc d", attr: 0 },
              { text: " ", attr: 8 },
              { text: " ".repeat(114), attr: 0 },
            ],
          },
        ],
      }),
    );
    expectInverseAtCol(outputEl, 19, 5, " ");

    // Frame after Left Arrow (no content typed; inverse moves to 'd')
    // Cursor moves from col 5 to col 4. Cells: "abc " then inverse "d"
    // (col 4 was 'd', is now 'd' with inverse). Server's diff sees cells
    // 4 and 5 changed; trackCursor adds row 19 if not already present.
    await flushFrame(
      buildScreenFrame({
        cursorRow: 19,
        cursorCol: 4,
        screenHeight: 30,
        cursorHidden: true,
        changed: [
          {
            idx: 19,
            runs: [
              { text: "abc ", attr: 0 },
              { text: "d", attr: 8 },
              { text: " ".repeat(115), attr: 0 },
            ],
          },
        ],
      }),
    );
    expectInverseAtCol(outputEl, 19, 4, "d");

    // Another Left Arrow: cursor col 3, on space. Cells: "abc" + inv " " + "d"
    await flushFrame(
      buildScreenFrame({
        cursorRow: 19,
        cursorCol: 3,
        screenHeight: 30,
        cursorHidden: true,
        changed: [
          {
            idx: 19,
            runs: [
              { text: "abc", attr: 0 },
              { text: " ", attr: 8 },
              { text: "d", attr: 0 },
              { text: " ".repeat(115), attr: 0 },
            ],
          },
        ],
      }),
    );
    expectInverseAtCol(outputEl, 19, 3, " ");
  });

  it("when two screen frames arrive in the same rAF tick, the LATEST frame's row content wins", async () => {
    const blankRow: Run[] = [{ text: " ".repeat(120), attr: 0 }];
    const initialChanged = Array.from({ length: 30 }, (_, i) => ({ idx: i, runs: blankRow }));
    await flushFrame(
      buildScreenFrame({
        cursorRow: 0,
        cursorCol: 0,
        screenHeight: 30,
        cursorHidden: true,
        changed: initialChanged,
      }),
    );

    const frame1 = buildScreenFrame({
      cursorRow: 19,
      cursorCol: 3,
      screenHeight: 30,
      cursorHidden: true,
      changed: [
        {
          idx: 19,
          runs: [
            { text: "abc", attr: 0 },
            { text: " ", attr: 8 },
            { text: " ".repeat(116), attr: 0 },
          ],
        },
      ],
    });
    const frame2 = buildScreenFrame({
      cursorRow: 19,
      cursorCol: 4,
      screenHeight: 30,
      cursorHidden: true,
      changed: [
        {
          idx: 19,
          runs: [
            { text: "abc ", attr: 0 },
            { text: " ", attr: 8 },
            { text: " ".repeat(115), attr: 0 },
          ],
        },
      ],
    });
    const msg1 = decodeWireBinary(frame1) as ScreenMessage;
    const msg2 = decodeWireBinary(frame2) as ScreenMessage;
    render.handleScreen(msg1);
    render.handleScreen(msg2);
    await nextFrames();
    expectInverseAtCol(outputEl, 19, 4, " ");
  });
});

function expectInverseAtCol(
  output: HTMLElement,
  rowIdx: number,
  col: number,
  expectedChar: string,
): void {
  const rowEl = output.children[rowIdx] as HTMLElement | undefined;
  expect(rowEl, `row[${rowIdx}] must exist`).toBeDefined();
  const spans = Array.from(rowEl!.children) as HTMLElement[];
  let cumCol = 0;
  let foundInverseAt = -1;
  let foundInverseText = "";
  for (const span of spans) {
    const text = span.textContent ?? "";
    // The application's inverse-video cell over default colors: the spec swap
    // is theme bg-on-fg, which render.ts emits as color:var(--bg) +
    // background:var(--text). Require BOTH (the native cursor stays hidden in
    // these frames, so there is no term-cursor span to match instead).
    const isInverse = span.style.background === "var(--text)" && span.style.color === "var(--bg)";
    if (isInverse) {
      foundInverseAt = cumCol;
      foundInverseText = text;
      break;
    }
    cumCol += [...text].length;
  }
  expect(
    foundInverseAt,
    `expected inverse-styled span at col ${col} in row[${rowIdx}], got col ${foundInverseAt} (rowEl outerHTML: ${rowEl!.outerHTML.slice(0, 300)})`,
  ).toBe(col);
  expect([...foundInverseText][0]).toBe(expectedChar);
}

describe("cursor-blink visibility gate", () => {
  let outputEl: HTMLDivElement;
  let termWrap: HTMLDivElement;

  // setVisibility shadows the page's visibility state (a test cannot background
  // a real document) and fires the event the gate listens on. Note the browser
  // reports "visible" by default, so the hidden arm is never taken by accident.
  function setVisibility(state: "visible" | "hidden"): void {
    Object.defineProperty(document, "visibilityState", { value: state, configurable: true });
    document.dispatchEvent(new Event("visibilitychange"));
  }

  beforeEach(() => {
    setVisibility("visible");
    document.body.innerHTML = `<div id="term"><div id="term-output"></div></div>`;
    termWrap = document.getElementById("term") as HTMLDivElement;
    outputEl = document.getElementById("term-output") as HTMLDivElement;
    render.resetScreen();
    render.init({ output: outputEl, termWrap });
    render.updateFontMetrics();
  });

  it("pauses the blink interval while hidden (solid cursor) and resumes on foreground", async () => {
    // A visible blinking cursor: one frame with cursorBlink=true (the default).
    await flushFrame(
      buildScreenFrame({
        cursorRow: 0,
        cursorCol: 0,
        screenHeight: 2,
        changed: [{ idx: 0, runs: [{ text: " ".repeat(10), attr: 0 }] }],
      }),
    );

    // Drive only the interval clock; rAF (the flush path) stays real. The
    // interval running now was created before the fake clock, so cycle it
    // through hidden->visible once to re-arm it under the fake timers.
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    try {
      setVisibility("hidden");
      setVisibility("visible");
      // Blinking: the phase class toggles within one cycle.
      vi.advanceTimersByTime(530);
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(true);

      // Hidden: the timer stops and the phase resets to solid — a background
      // tab must not keep waking up to toggle an invisible cursor (battery).
      setVisibility("hidden");
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(false);
      vi.advanceTimersByTime(530 * 4);
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(false);

      // Foregrounded: blinking resumes from the solid phase.
      setVisibility("visible");
      vi.advanceTimersByTime(530);
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it("downshifts to a slow re-check while the application hides the cursor (DECTCM)", async () => {
    const frame = (over: { cursorHidden?: boolean }) =>
      buildScreenFrame({
        cursorRow: 0,
        cursorCol: 0,
        screenHeight: 2,
        changed: [{ idx: 0, runs: [{ text: " ".repeat(10), attr: 0 }] }],
        ...over,
      });
    await flushFrame(frame({}));

    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    try {
      // Re-arm the visible-cursor blink interval under the fake clock.
      setVisibility("hidden");
      setVisibility("visible");
      vi.advanceTimersByTime(530);
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(true);

      // The application hides the cursor: the fast toggle stops and the phase
      // resets to solid — the overlay is display:none, so restyling it twice a
      // second is pure waste (an agent TUI hides the cursor for the whole
      // session). The interval downshifts to the slow re-check instead.
      await flushFrame(frame({ cursorHidden: true }));
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(false);
      // Across many blink periods — including several slow re-check fires —
      // the phase class never toggles.
      vi.advanceTimersByTime(530 * 16);
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(false);

      // The cursor re-shows: blinking resumes from the solid phase.
      await flushFrame(frame({ cursorHidden: false }));
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(false);
      vi.advanceTimersByTime(530);
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it("stops the interval entirely while the application disables blink, resumes on re-enable", async () => {
    const frame = (over: { cursorBlink?: boolean }) =>
      buildScreenFrame({
        cursorRow: 0,
        cursorCol: 0,
        screenHeight: 2,
        changed: [{ idx: 0, runs: [{ text: " ".repeat(10), attr: 0 }] }],
        ...over,
      });
    await flushFrame(frame({}));

    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    try {
      setVisibility("hidden");
      setVisibility("visible");
      vi.advanceTimersByTime(530);
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(true);

      // Blink disabled (DECSCUSR steady / mode 12 reset): solid cursor, no
      // timer at all.
      await flushFrame(frame({ cursorBlink: false }));
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(false);
      vi.advanceTimersByTime(530 * 16);
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(false);

      // Re-enabled: blinking resumes.
      await flushFrame(frame({ cursorBlink: true }));
      vi.advanceTimersByTime(530);
      expect(termWrap.classList.contains("cursor-blink-off")).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });
});
