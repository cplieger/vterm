// Vitest configuration for @cplieger/web-terminal-engine unit tests.
//
// Two projects, and the DEFAULT is the browser. A test file runs in a real
// headless Chromium unless its name opts out, because the browser is the
// environment this package actually ships into: it renders DOM, measures text
// through a real Canvas2D, and reads real layout.
//
// The opt-out is the `.node.test.ts` suffix, and it is load-bearing rather than
// decorative — placement has to be readable off the filename, because one of
// the two reasons a file needs Node fails SILENTLY when it is misplaced. Here
// all three node files need genuine Node CAPABILITIES: they read golden
// fixtures from ../../wire-golden/ by name, and wire-manifest WRITES a
// regenerated manifest back into the repo under UPDATE_GOLDEN=1. A write has no
// Vite-import equivalent at all, so it is the irreducible case.
//
// The reason lives in the stem (`wire-golden`, `wire-compatibility`,
// `wire-manifest`) and the placement in the suffix (`.node`). Fuzz keeps its own
// axis: `*.fuzz.test.ts` is how ts-ci selects fuzz targets, and a DOM fuzz test
// (hyperlink-safety) needs no marker here at all.
//
// `extends: true` on both projects is REQUIRED, not decorative: a project
// inherits NOTHING from the root `test` block without it, so the root's
// `expect.requireAssertions`, `allowOnly`, `mockReset`, `clearMocks`,
// `restoreMocks`, `unstubGlobals`, `sequence` and timeouts would be silently
// dropped — and losing a strictness option never fails a test, so the suite
// would stay green while the guarantees went away.
//
// `channel: "chromium"` opts into Chromium's newer headless mode, the real
// browser rather than the separate headless-shell build. CI installs it with
// `npx playwright install --with-deps chromium`; locally it is a one-time
// `npx --no-install playwright install chromium`.
//
// web/e2e/** stays outside every include here: it is Playwright's namespace
// (playwright.config.ts testDir "./e2e"), it spawns server child processes, and
// it is run by `npm run test:e2e`. Both projects therefore glob under src/.
//
// Run: vitest --run (single pass) or vitest (watch mode).
import { playwright } from "@vitest/browser-playwright";
import { defineConfig } from "vitest/config";

export default defineConfig({
  // The cross-language golden fixtures live in ../render-golden/ at the REPO
  // root, above this Vite root, and two browser tests now import them
  // (?raw / ?url) instead of reading them with node:fs. Browser Mode serves
  // them over HTTP, and Vite's dev-server file guard refuses anything outside
  // the root until it is allowed. Narrowest entry that covers both: the repo
  // root itself.
  server: {
    fs: {
      allow: [".", ".."],
    },
  },
  test: {
    projects: [
      {
        extends: true,
        test: {
          name: "node",
          environment: "node",
          include: ["src/**/*.node.test.ts"],
        },
      },
      {
        extends: true,
        test: {
          name: "browser",
          include: ["src/**/*.test.ts"],
          exclude: ["src/**/*.node.test.ts", "node_modules/**"],
          browser: {
            enabled: true,
            headless: true,
            provider: playwright({
              launchOptions: {
                channel: "chromium",
              },
            }),
            instances: [{ browser: "chromium" }],
            // Fixed viewport so layout-dependent assertions are reproducible;
            // a real browser computes real boxes.
            viewport: { width: 1280, height: 720 },
            // A failure screenshot per failing test is noise in CI and cannot
            // be read from a job log; the assertion diff is the artifact.
            screenshotFailures: false,
          },
        },
      },
    ],
    // No `include` at this level: `extends: true` MERGES array options into
    // each project, so a root include glob would be appended to the node
    // project's and pull every browser test into it. Both projects declare
    // their own; collection is theirs alone.
    exclude: ["node_modules/**"],
    passWithNoTests: false,
    allowOnly: false,
    globals: false,
    expect: {
      requireAssertions: true,
    },
    clearMocks: true,
    mockReset: true,
    restoreMocks: true,
    unstubEnvs: true,
    unstubGlobals: true,
    bail: process.env["CI"] ? 1 : 0,
    testTimeout: 5000,
    hookTimeout: 10000,
    slowTestThreshold: 300,
    sequence: {
      shuffle: { files: false, tests: false },
      concurrent: false,
      hooks: "stack",
    },
    setupFiles: ["./src/fc-strict-setup.ts"],
    printConsoleTrace: true,
    expandSnapshotDiff: true,
    coverage: {
      provider: "v8",
      include: ["src/**/*.ts"],
      exclude: ["src/**/*.test.ts", "src/**/*.d.ts", "src/**/*-setup.ts"],
      reportOnFailure: true,
      reporter: ["text", "text-summary", "lcov"],
    },
  },
});
