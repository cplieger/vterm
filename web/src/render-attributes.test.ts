// Tier 1 display conformance: does render.ts turn each wire attribute into the
// on-screen result the SPEC requires? Each test STATES the spec (what the
// attribute must produce) and asserts the real renderer complies. Expectations
// are the standard CSS representation of each SGR effect — NOT copied from
// render.ts. A failure means the renderer deviates from the spec.
//
// Wire attribute bitmask (the engine→UI contract, per vt/wire.go doc):
//   1 bold · 2 italic · 4 underline · 8 inverse · 16 strikethrough · 32 dim
//   64 hidden · 128 blink · 256 overline · 512 double-underline
import { describe, it, expect, beforeEach } from "vitest";
import { initHarness, renderRow, firstTextSpan } from "./test-helpers/render-harness.js";
import { rgb, cssColor } from "./test-helpers/spec-colors.js";
import type { WireRun } from "./types.js";

const A = {
  bold: 1,
  italic: 2,
  underline: 4,
  inverse: 8,
  strike: 16,
  dim: 32,
  hidden: 64,
  blink: 128,
  overline: 256,
  doubleUnderline: 512,
} as const;

function run(text: string, over: Partial<WireRun> = {}): WireRun {
  return { t: text, f: -1, b: -1, uc: -1, a: 0, ...over };
}

async function styleOf(over: Partial<WireRun>): Promise<CSSStyleDeclaration> {
  const spans = await renderRow([run("M", over)]);
  const span = firstTextSpan(spans);
  expect(span, "a span with the glyph must be rendered").toBeDefined();
  return span!.style;
}

beforeEach(() => {
  initHarness();
});

describe("SGR text attributes → on-screen effect (spec)", () => {
  it("SGR 1 bold: MUST render at a bold font weight", async () => {
    const style = await styleOf({ a: A.bold });
    // Spec: bold text is heavier. Standard CSS: font-weight bold (== 700).
    expect(["bold", "700"]).toContain(style.fontWeight);
  });

  it("SGR 3 italic: MUST render in an italic font style", async () => {
    const style = await styleOf({ a: A.italic });
    expect(style.fontStyle).toBe("italic");
  });

  it("SGR 4 underline: MUST render a text underline", async () => {
    const style = await styleOf({ a: A.underline });
    expect(style.textDecoration).toContain("underline");
  });

  it("SGR 21 double-underline: MUST render a doubled underline", async () => {
    const style = await styleOf({ a: A.doubleUnderline });
    // Spec: a double line under the text. Standard CSS: underline + double style.
    expect(style.textDecoration).toContain("underline");
    expect(style.textDecoration).toContain("double");
  });

  it("SGR 9 strikethrough: MUST render a line through the text", async () => {
    const style = await styleOf({ a: A.strike });
    expect(style.textDecoration).toContain("line-through");
  });

  it("SGR 53 overline: MUST render a line above the text", async () => {
    const style = await styleOf({ a: A.overline });
    expect(style.textDecoration).toContain("overline");
  });

  it("SGR 2 dim: MUST render at reduced intensity", async () => {
    const style = await styleOf({ a: A.dim });
    // Spec: faint / reduced intensity. Observable at the DOM tier as an opacity
    // below 1 (the pixel tier verifies it is actually lighter on screen).
    const opacity = parseFloat(style.opacity);
    expect(opacity).toBeGreaterThan(0);
    expect(opacity).toBeLessThan(1);
  });

  it("SGR 8 hidden: MUST NOT paint the glyph", async () => {
    const style = await styleOf({ a: A.hidden });
    // Spec: concealed text is invisible. Standard CSS: visibility hidden.
    expect(style.visibility).toBe("hidden");
  });

  it("SGR 5 blink: MUST mark the text as blinking", async () => {
    const spans = await renderRow([run("M", { a: A.blink })]);
    const span = firstTextSpan(spans);
    expect(span, "a span with the glyph must be rendered").toBeDefined();
    // Spec: the text blinks. The renderer drives a CSS animation via a class.
    expect(span!.className).toContain("term-blink");
  });

  it("combined bold+italic+underline: MUST render all three at once", async () => {
    const style = await styleOf({ a: A.bold | A.italic | A.underline });
    expect(["bold", "700"]).toContain(style.fontWeight);
    expect(style.fontStyle).toBe("italic");
    expect(style.textDecoration).toContain("underline");
  });
});

describe("colors → on-screen effect (spec)", () => {
  it("truecolor foreground: MUST render the exact RGB", async () => {
    const style = await styleOf({ f: rgb(255, 0, 0) });
    expect(style.color).toBe(cssColor(rgb(255, 0, 0))); // #ff0000
  });

  it("truecolor background: MUST render the exact RGB", async () => {
    const style = await styleOf({ b: rgb(0, 0, 255) });
    expect(style.background).toBe(cssColor(rgb(0, 0, 255))); // #0000ff
  });

  it("SGR 58 underline color: MUST color the underline decoration", async () => {
    const style = await styleOf({ a: A.underline, uc: rgb(0, 255, 0) });
    expect(style.textDecoration).toContain("underline");
    expect(style.textDecorationColor).toBe(cssColor(rgb(0, 255, 0))); // #00ff00
  });

  it("default foreground: MUST NOT pin an explicit color (inherits the theme)", async () => {
    const style = await styleOf({ f: -1 });
    expect(style.color).toBe("");
  });

  it("SGR 7 inverse over default colors: MUST swap to theme bg-on-fg", async () => {
    // Spec: reverse video paints the background color as ink on a foreground-
    // colored cell. With both colors default the swap must use the theme vars
    // (else an inverse blank would be invisible).
    const style = await styleOf({ a: A.inverse });
    expect(style.color).toBe("var(--bg)");
    expect(style.background).toBe("var(--text)");
  });
});

describe("OSC 8 hyperlink → on-screen effect (spec)", () => {
  it("a run carrying a URI MUST render as an anchor to that URI", async () => {
    const spans = await renderRow([run("click", { u: "https://example.com/x" })]);
    const anchor = spans.find((s) => s.tagName === "A") as HTMLAnchorElement | undefined;
    expect(anchor, "an <a> element must be rendered").toBeDefined();
    expect(anchor!.getAttribute("href")).toBe("https://example.com/x");
    expect(anchor!.textContent).toContain("click");
  });
});
