import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    environment: "node",
    pool: "threads",
    isolate: false,
    include: ["src/**/*.test.ts"],
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
    testTimeout: 2000,
    hookTimeout: 5000,
    slowTestThreshold: 100,
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
