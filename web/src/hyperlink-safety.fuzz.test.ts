// SECURITY invariant — OSC 8 hyperlink scheme allow-list.
//
// xterm OSC 8 (`OSC 8 ; params ; URI ST`) lets an application attach an
// arbitrary URI to a run of text. A terminal MUST NOT turn every scheme into a
// clickable link: a `javascript:`, `data:`, `vbscript:`, or `file:` href is a
// script-injection / local-file vector, so only http/https may become a live
// anchor. render.ts enforces this in buildRowSpans with `/^https?:\/\//i`.
//
// This file fuzzes that gate with adversarial URIs (case, whitespace, control
// chars, embedded NULs, nested schemes) and asserts the TWO-SIDED invariant on
// the REAL renderer:
//   - dangerous scheme  -> zero anchors, and the run text still renders (inert);
//   - safe http(s) URI  -> exactly one anchor carrying the verbatim href.
//
// Spec refs: xterm ctlseqs (OSC 8) and the OSC 8 hyperlink spec's security
// section (only a safe scheme subset should be actionable):
// https://gist.github.com/egmontkob/eb114294efbcd5adb1944c9f3cb5feda
// Content was rephrased for compliance with licensing restrictions.

import { describe, it, expect, beforeEach, afterEach } from "vitest";
import fc from "fast-check";
import * as render from "./render.js";
import type { ScreenMessage, WireRun } from "./types.js";

// A fixed cell metric: a real Canvas2D measures whatever font the machine has
// installed, and render's width cache needs a deterministic measureText.
HTMLCanvasElement.prototype.getContext = function (): unknown {
  return { font: "", measureText: (t: string) => ({ width: t.length * 8 }) };
} as typeof HTMLCanvasElement.prototype.getContext;

// The visible text of every fuzzed run: a fixed non-URL marker so the text
// itself is never auto-linkified — this isolates the OSC 8 href gate under test
// from render's separate plain-text autolinker.
const LINK_TEXT = "linktext";

let output: HTMLDivElement;
let realRAF: typeof globalThis.requestAnimationFrame;
let realCAF: typeof globalThis.cancelAnimationFrame;

beforeEach(() => {
  // Drive render's requestAnimationFrame-batched flush synchronously so each
  // property iteration renders and asserts without awaiting a timer — this keeps
  // a 1000-run fast-check property well within the per-test time budget.
  // Restored in afterEach so the stub never leaks to other files (the vitest
  // config runs with isolate:false, sharing globals across files in a worker).
  realRAF = globalThis.requestAnimationFrame;
  realCAF = globalThis.cancelAnimationFrame;
  globalThis.requestAnimationFrame = ((cb: FrameRequestCallback): number => {
    cb(0);
    // Return undefined (cast) so render's `pendingFrame` guard clears after the
    // synchronous flush and the NEXT render is not suppressed.
    return undefined as unknown as number;
  }) as typeof globalThis.requestAnimationFrame;
  globalThis.cancelAnimationFrame = (() => undefined) as typeof globalThis.cancelAnimationFrame;

  output = document.createElement("div");
  output.id = "term-output";
  const termWrap = document.createElement("div");
  termWrap.id = "term-wrap";
  termWrap.appendChild(output);
  document.body.innerHTML = "";
  document.body.appendChild(termWrap);
  render.init({ output, termWrap });
  render.updateFontMetrics();
});

afterEach(() => {
  globalThis.requestAnimationFrame = realRAF;
  globalThis.cancelAnimationFrame = realCAF;
});

// renderRuns paints one row of runs and returns the built row element. The
// stubbed rAF makes handleScreen flush synchronously, so the DOM is ready on
// return.
function renderRuns(runs: WireRun[]): HTMLElement {
  const msg: ScreenMessage = {
    type: "screen",
    base: 0,
    rows: [runs, [], [], [], []],
    cursor: [0, 0],
    changed: [0],
    cursorHidden: true,
    cursorStyle: 0,
    cursorBlink: false,
  };
  render.handleScreen(msg);
  return output.children[0] as HTMLElement;
}

function osc8Run(uri: string, text = LINK_TEXT): WireRun {
  return { t: text, f: -1, b: -1, a: 0, uc: -1, u: uri };
}

function anchorsIn(row: HTMLElement): HTMLAnchorElement[] {
  return Array.from(row.querySelectorAll("a"));
}

// --- Adversarial corpus (durable seeds) ---
// Specific attack strings that MUST never produce a clickable anchor. Kept as
// an explicit, human-readable list (DAMP) so a regression names the exact input.
const DANGEROUS_SEEDS: readonly string[] = [
  "javascript:alert(1)",
  "data:text/html,<script>alert(1)</script>",
  "JAVASCRIPT:void(0)",
  "Data:text/html;base64,PHNjcmlwdD4=",
  "jAvAsCrIpT:fetch('/steal')",
  "  javascript:x", // leading spaces
  "vbscript:run",
  "\tjavascript:x", // leading tab
  "\njavascript:x", // leading newline
  "javascript\n:alert(1)", // newline inside the scheme
  "java\tscript:alert(1)", // tab inside the scheme
  "file:///etc/passwd",
  "ftp://host/file",
  "mailto:a@b.example", // safe but non-http(s): render is http(s)-only by policy
  "://missing-scheme",
  "\u0000javascript:x", // leading NUL
  "https\u0000://evil", // NUL breaking the scheme
  "  data:text/html,x",
];

// Dangerous-scheme generator: a known-bad scheme prefix (with case + whitespace
// obfuscations) + an arbitrary tail. Constructed to NEVER start with a bare
// http(s)://, so the "dangerous" label is by construction — the test does not
// re-run render's regex to decide what is dangerous (that would be a tautology).
const dangerousScheme = fc.constantFrom(
  "javascript:",
  "JavaScript:",
  "  javascript:",
  "\tjavascript:",
  "data:text/html,",
  "DATA:text/plain,",
  "vbscript:",
  "VBScript:",
  "file:///",
  "ftp://",
  "mailto:",
  "tel:",
  "blob:",
  "about:",
);
const dangerousUri = fc
  .tuple(dangerousScheme, fc.string({ maxLength: 40 }))
  .map(([scheme, tail]) => scheme + tail);

// Safe-scheme generator: an http(s):// prefix (mixed case, exercising the /i
// flag) + a URL-ish tail. By construction every value is a live http(s) link
// and MUST anchor.
const safeScheme = fc.constantFrom("http://", "https://", "HTTP://", "HTTPS://", "HtTpS://");
const urlTail = fc.stringMatching(/^[a-z0-9/?=_.-]{0,40}$/);
const safeUri = fc.tuple(safeScheme, urlTail).map(([scheme, tail]) => scheme + tail);

describe("hyperlink safety: OSC 8 href scheme allow-list", () => {
  it("dangerous-scheme URIs never become anchors; the run text still renders", () => {
    fc.assert(
      fc.property(dangerousUri, (uri) => {
        const row = renderRuns([osc8Run(uri)]);
        // Security invariant: no clickable anchor for a non-http(s) scheme.
        expect(anchorsIn(row).length).toBe(0);
        // The content is not dropped — it renders as inert text.
        expect(row.textContent).toContain(LINK_TEXT);
      }),
    );
  });

  it("the explicit dangerous corpus never renders an anchor (text preserved)", () => {
    for (const uri of DANGEROUS_SEEDS) {
      const row = renderRuns([osc8Run(uri)]);
      expect(anchorsIn(row).length, `must not linkify: ${JSON.stringify(uri)}`).toBe(0);
      expect(row.textContent, `text must render: ${JSON.stringify(uri)}`).toContain(LINK_TEXT);
    }
  });

  it("safe http(s) URIs render exactly one anchor carrying the verbatim href", () => {
    fc.assert(
      fc.property(safeUri, (uri) => {
        const row = renderRuns([osc8Run(uri)]);
        const anchors = anchorsIn(row);
        // Non-vacuous: the safe branch must actually produce a link (guards
        // against a "reject everything" gate that would trivially pass the
        // dangerous-scheme property above).
        expect(anchors.length).toBe(1);
        const a = anchors[0]!;
        // The OSC 8 target is used verbatim (not rebuilt from the visible text).
        expect(a.getAttribute("href")).toBe(uri);
        // And it resolves to an http(s) URL — a genuinely clickable link.
        expect(a.href.toLowerCase()).toMatch(/^https?:\/\//);
        expect(a.textContent).toContain(LINK_TEXT);
      }),
    );
  });

  it("mixed safe + dangerous runs in one row: anchor count == safe count, all http(s)", () => {
    // Interleave dangerous and safe runs in a single frame; only the safe ones
    // may anchor. The count check is two-sided: no dangerous run leaked an
    // anchor (else the count exceeds safeCount) and no safe run was dropped
    // (else it falls short). safeCount is known by construction, not by
    // re-testing render's regex.
    const dangerous = fc.sample(dangerousUri, 120);
    const safe = fc.sample(safeUri, 120);
    const runs: WireRun[] = [];
    let safeCount = 0;
    const n = Math.max(dangerous.length, safe.length);
    for (let i = 0; i < n; i++) {
      if (i < dangerous.length) {
        runs.push(osc8Run(dangerous[i]!, "d"));
      }
      if (i < safe.length) {
        runs.push(osc8Run(safe[i]!, "s"));
        safeCount++;
      }
    }
    const row = renderRuns(runs);
    const anchors = anchorsIn(row);
    expect(anchors.length).toBe(safeCount);
    for (const a of anchors) {
      expect(a.href.toLowerCase()).toMatch(/^https?:\/\//);
    }
  });
});
