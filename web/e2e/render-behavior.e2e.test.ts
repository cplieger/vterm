// Tier 3 — behavioral display conformance through a SERVED page. The Vitest
// sibling (src/render-behavior.test.ts) asserts the DOM grid after each escape
// operation inside the test runner's page; this runs the SAME Go-generated
// fixtures through the real render.ts in a Playwright-driven chromium loading the
// real harness page, so the grid is asserted against a full page load and the
// engine's own bundle rather than the runner's module graph.
//
// Each scenario's frame is the engine's real wire output for a real escape
// SEQUENCE (clear, erase, cursor-move, insert/delete, scroll, ...); the expected
// grid is spec-authored in terminal/render_golden_test.go TestRenderGoldenBehavior
// and asserted there against the engine too. Run with `npm run test:e2e`.
import { test, expect } from "@playwright/test";
import { bundleEngine, HARNESS, readGolden, waitForRows } from "./e2e-harness.js";

interface BehaviorEntry {
  name: string;
  input: string;
  want: string[];
  frame: string; // base64 of the engine's real wire frame
}

const scenarios = JSON.parse(
  readGolden("behavior.manifest.json").toString("utf8"),
) as BehaviorEntry[];

test.describe("behavioral display conformance in a real browser (escape seq -> engine -> wire -> DOM)", () => {
  let bundle = "";
  test.beforeAll(async () => {
    bundle = await bundleEngine();
  });

  for (const sc of scenarios) {
    test(`renders the on-screen grid after ${sc.name}`, async ({ page }) => {
      const frameBytes = Array.from(Buffer.from(sc.frame, "base64"));
      await page.setContent(HARNESS);
      await page.addScriptTag({ content: bundle });
      await page.evaluate((bytes: number[]) => {
        const msg = WTE.decodeWireBinary(new Uint8Array(bytes).buffer);
        if (!msg || msg.type !== "screen") {
          throw new Error("fixture did not decode as a screen frame");
        }
        const out = document.getElementById("out")!;
        const wrap = document.getElementById("wrap")!;
        WTE.render.init({ output: out, termWrap: wrap });
        WTE.render.updateFontMetrics();
        WTE.render.handleScreen(msg);
      }, frameBytes);
      // Deterministic flush wait: every spec grid's rows must exist as divs
      // (the fixture frame is a full repaint, so at least want.length rows
      // materialize; rows build in ascending order).
      await waitForRows(page, sc.want.length);

      const got = await page.evaluate(() => {
        const out = document.getElementById("out")!;
        // Each row's visible text: normalize the nbsp filler an empty row uses
        // and trim trailing blanks, matching the vt RowString spec-grid convention.
        return Array.from(out.children).map((rowEl) =>
          (rowEl.textContent ?? "").replace(/\u00a0/g, " ").replace(/[ ]+$/, ""),
        );
      });
      // The DOM grid rendered with REAL layout must equal the spec grid.
      expect(got.slice(0, sc.want.length), `input ${JSON.stringify(sc.input)}`).toEqual(sc.want);
    });
  }
});

// WTE is the esbuild IIFE global injected via addScriptTag.
declare const WTE: {
  render: {
    init: (opts: { output: unknown; termWrap: unknown }) => void;
    updateFontMetrics: () => void;
    handleScreen: (msg: unknown) => void;
  };
  decodeWireBinary: (buf: ArrayBuffer) => { type: string } | null;
};
