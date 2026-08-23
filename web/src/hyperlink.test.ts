// OSC 8 hyperlink rendering: a run carrying an OSC 8 URI is painted as an <a>.
//
// Spec: xterm ctlseqs OSC 8 (`OSC 8 ; params ; URI ST`) — the URI in the
// sequence is authoritative and is what the link points at. Anchors are opened
// safely (target=_blank, rel=noopener guards reverse-tabnabbing). render.ts
// only linkifies http/https URIs (a conservative allow-list); the adversarial
// scheme sweep proving javascript:/data:/etc. never become clickable lives in
// hyperlink-safety.fuzz.test.ts. Expectations here derive from the OSC 8 spec,
// not from reading render.ts.

import { describe, it, expect, beforeEach } from "vitest";
import * as render from "./render.js";
import type { ScreenMessage, WireRun } from "./types.js";

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

function frame(rowsByIdx: Record<number, WireRun[]>, cursor: [number, number]): ScreenMessage {
  const screenH = 5;
  const rows: WireRun[][] = new Array(screenH);
  const changed: number[] = [];
  for (const k of Object.keys(rowsByIdx)) {
    const idx = Number(k);
    rows[idx] = rowsByIdx[idx]!;
    changed.push(idx);
  }
  return {
    type: "screen",
    base: 0,
    rows,
    cursor,
    changed,
    cursorHidden: true,
    cursorStyle: 0,
    cursorBlink: true,
  };
}

async function flushFrame(msg: ScreenMessage): Promise<void> {
  render.handleScreen(msg);
  // Wait for the frame render.ts scheduled, not for a duration. This used to be
  // a 32ms sleep because the emulator implemented rAF as a ~16ms timer and
  // racing its queue ordering flaked on CI; against real frames the honest wait
  // is the frame itself. Two deep because the first callback can run in the
  // frame the flush was queued in.
  await new Promise<void>((resolve) => {
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        resolve();
      });
    });
  });
}

describe("OSC 8 hyperlink rendering", () => {
  let output: HTMLDivElement;
  let termWrap: HTMLDivElement;

  beforeEach(() => {
    output = document.createElement("div");
    output.id = "term-output";
    output.contentEditable = "true";
    termWrap = document.createElement("div");
    termWrap.id = "term-wrap";
    termWrap.appendChild(output);
    document.body.innerHTML = "";
    document.body.appendChild(termWrap);
    render.init({ output, termWrap });
    render.updateFontMetrics();
  });

  it("renders a run with URL as an <a> element with correct attributes", async () => {
    const runs: WireRun[] = [
      { t: "click ", f: -1, b: -1, a: 0, uc: -1 },
      { t: "here", f: -1, b: -1, a: 0, uc: -1, u: "http://example.com" },
      { t: " end", f: -1, b: -1, a: 0, uc: -1 },
    ];
    const msg = frame({ 0: runs }, [0, 10]);
    await flushFrame(msg);

    const anchors = output.querySelectorAll("a.term-link");
    expect(anchors.length).toBeGreaterThanOrEqual(1);
    const a = anchors[0] as HTMLAnchorElement;
    // The OSC 8 target is carried verbatim on the attribute (spec: the URI in
    // the sequence is authoritative), and resolves to the absolute URL.
    expect(a.getAttribute("href")).toBe("http://example.com");
    expect(a.href).toBe("http://example.com/");
    // Opened safely: a new context with no window.opener handle back, and
    // no Referer leak of the terminal URL to an attacker-controlled OSC 8 URI.
    expect(a.target).toBe("_blank");
    expect(a.rel).toBe("noopener noreferrer");
    expect(a.textContent).toBe("here");
  });

  it("does not render <a> for runs without URL", async () => {
    const runs: WireRun[] = [{ t: "plain text", f: -1, b: -1, a: 0, uc: -1 }];
    const msg = frame({ 0: runs }, [0, 10]);
    await flushFrame(msg);

    const anchors = output.querySelectorAll("a.term-link");
    // linkifySpans may detect URLs in text, but "plain text" has none
    expect(anchors.length).toBe(0);
  });

  it("keeps the OSC 8 href when the visible text is itself a URL fragment", async () => {
    // First row of a URL that wraps across lines: the visible text is only
    // a fragment, but the full target is carried in `u`. The regex
    // autolinker must NOT rebuild the link from the truncated visible text.
    const full = "http://example.com/very/long/path/that/wraps/here";
    const runs: WireRun[] = [
      { t: "http://example.com/very/long/pa", f: -1, b: -1, a: 0, uc: -1, u: full },
    ];
    const msg = frame({ 0: runs }, [0, 0]);
    await flushFrame(msg);

    const anchors = output.querySelectorAll("a.term-link");
    expect(anchors.length).toBe(1);
    const a = anchors[0] as HTMLAnchorElement;
    // Raw attribute is the OSC 8 target, not the truncated visible fragment.
    expect(a.getAttribute("href")).toBe(full);
    expect(a.textContent).toBe("http://example.com/very/long/pa");
  });

  it("renders a server-stamped autolink run (attr bit 1024) with the persistent-underline class", async () => {
    // The server's wrap-aware autolinker (vt.AttrAutolink) stamps bare URLs
    // with the full href and bit 1024. Unlike an app OSC 8 link (hover-only
    // underline), the anchor hugs exactly the URL text, so it keeps the
    // persistent `.term-autolink` underline — this is what makes the SECOND
    // line of a wrapped URL visibly a link on a phone.
    const full = "https://amzn.example/start/#/device?user_code=ABCD-EFGH";
    const runs: WireRun[] = [
      { t: "device?user_code=ABCD-EFGH", f: -1, b: -1, a: 1024, uc: -1, u: full },
    ];
    const msg = frame({ 0: runs }, [0, 0]);
    await flushFrame(msg);

    const anchors = output.querySelectorAll("a.term-link.term-autolink");
    expect(anchors.length).toBe(1);
    const a = anchors[0] as HTMLAnchorElement;
    // The tail row's anchor carries the FULL joined href, not the visible
    // fragment (the truncated-tap bug), and opens safely.
    expect(a.getAttribute("href")).toBe(full);
    expect(a.textContent).toBe("device?user_code=ABCD-EFGH");
    expect(a.target).toBe("_blank");
    expect(a.rel).toBe("noopener noreferrer");
  });

  it("renders both rows of a server-stamped wrapped URL as anchors with the same full href", async () => {
    const full = "https://ex.example/aaaaaaaaaa/bbbbbbbbbb";
    const row0: WireRun[] = [
      { t: "Open: ", f: -1, b: -1, a: 0, uc: -1 },
      { t: "https://ex.example/aaaa", f: -1, b: -1, a: 1024, uc: -1, u: full },
    ];
    const row1: WireRun[] = [
      { t: "aaaaaa/bbbbbbbbbb", f: -1, b: -1, a: 1024, uc: -1, u: full },
      { t: " done", f: -1, b: -1, a: 0, uc: -1 },
    ];
    const msg = frame({ 0: row0, 1: row1 }, [1, 20]);
    await flushFrame(msg);

    const anchors = [...output.querySelectorAll("a.term-link.term-autolink")];
    expect(anchors.length).toBe(2);
    for (const el of anchors) {
      expect(el.getAttribute("href")).toBe(full);
    }
  });

  it("does not linkify a non-http(s) OSC 8 scheme (renders as inert text)", async () => {
    // render.ts uses a conservative http/https-only allow-list, so even a
    // benign non-http scheme like mailto: is NOT turned into a live anchor —
    // the text renders inert. (Dangerous schemes are swept in the fuzz file.)
    const runs: WireRun[] = [{ t: "mail me", f: -1, b: -1, a: 0, uc: -1, u: "mailto:a@b.example" }];
    const msg = frame({ 0: runs }, [0, 0]);
    await flushFrame(msg);

    expect(output.querySelectorAll("a").length).toBe(0);
    expect(output.textContent).toContain("mail me");
  });

  it("autolinks a bare http(s) URL in plain text with the same safe-open attributes", async () => {
    // render.ts builds anchors on TWO paths: the OSC 8 `u`-field path
    // (asserted above) and linkifySpans, which detects a bare URL in the
    // visible text of a run that carries no `u`. Both must set the
    // reverse-tabnabbing guards (target=_blank, rel=noopener noreferrer);
    // the autolinker path is otherwise unasserted -- the safety fuzz file
    // uses a fixed non-URL text to isolate the OSC 8 gate, and the only
    // text-path test here uses URL-free text. A regression dropping
    // rel/target on the autolinker anchor would pass today.
    const runs: WireRun[] = [{ t: "see https://example.com/x now", f: -1, b: -1, a: 0, uc: -1 }];
    const msg = frame({ 0: runs }, [0, 0]);
    await flushFrame(msg);
    const anchors = output.querySelectorAll("a.term-link");
    expect(anchors.length).toBe(1);
    const a = anchors[0] as HTMLAnchorElement;
    expect(a.getAttribute("href")).toBe("https://example.com/x");
    expect(a.textContent).toBe("https://example.com/x");
    expect(a.target).toBe("_blank");
    expect(a.rel).toBe("noopener noreferrer");
  });

  it("distinguishes OSC 8 hyperlinks from auto-detected URLs by class", async () => {
    // Regression guard for the table-link underline bleed: an OSC 8 hyperlink
    // may cover a whole padded/bordered region (e.g. a URL wrapping inside a
    // table cell keeps the link open across cell padding + borders), so it must
    // carry ONLY `term-link` (the UI underlines it on hover, never persistently).
    // A heuristically auto-detected bare URL is tightly scoped to the matched
    // text and cannot bleed, so it additionally gets `term-autolink` (the UI
    // keeps a persistent underline). Both keep `term-link` so shared styling and
    // the safe-open attributes still apply.
    const osc8: WireRun[] = [
      { t: "click", f: -1, b: -1, a: 0, uc: -1, u: "https://example.com/x" },
    ];
    const bare: WireRun[] = [{ t: "see https://example.com/y here", f: -1, b: -1, a: 0, uc: -1 }];
    await flushFrame(frame({ 0: osc8, 1: bare }, [2, 0]));

    const anchors = [...output.querySelectorAll("a.term-link")] as HTMLAnchorElement[];
    const osc8Anchors = anchors.filter((a) => !a.classList.contains("term-autolink"));
    const autoAnchors = anchors.filter((a) => a.classList.contains("term-autolink"));

    expect(osc8Anchors.length).toBe(1);
    expect(osc8Anchors[0]?.getAttribute("href")).toBe("https://example.com/x");
    expect(autoAnchors.length).toBe(1);
    expect(autoAnchors[0]?.getAttribute("href")).toBe("https://example.com/y");
  });

  it("does not anchor whitespace/border cells an OSC 8 link stayed open across", async () => {
    // Reproduces a link wrapping inside a table cell: kiro-cli keeps the OSC 8
    // hyperlink open across the link text, then the trailing cell padding, the
    // right border `│` and the empty adjacent column — every cell shares the same
    // URI. Only the run with real text must become an <a>; the decorative runs
    // must render as plain (non-anchored) spans so the link's underline can only
    // ever hug the text, never bleed across the cell/row (at rest or on hover).
    const url = "https://example.com/wrapped";
    const runs: WireRun[] = [
      { t: "docs", f: -1, b: -1, a: 0, uc: -1, u: url }, // link text
      { t: "      ", f: -1, b: -1, a: 0, uc: -1, u: url }, // trailing cell padding
      { t: "│", f: -1, b: -1, a: 32, uc: -1, u: url }, // right border (dim), same link
      { t: "        ", f: -1, b: -1, a: 0, uc: -1, u: url }, // empty adjacent column
    ];
    await flushFrame(frame({ 0: runs }, [1, 0]));

    const anchors = [...output.querySelectorAll("a.term-link")] as HTMLAnchorElement[];
    expect(anchors.length).toBe(1);
    expect(anchors[0]?.textContent).toBe("docs");
    expect(anchors[0]?.getAttribute("href")).toBe(url);
    // the border still renders (as plain text) but is NOT inside any anchor
    expect(output.textContent).toContain("│");
    const anchoredText = anchors.map((a) => a.textContent ?? "").join("");
    expect(anchoredText).toBe("docs");
  });
});
