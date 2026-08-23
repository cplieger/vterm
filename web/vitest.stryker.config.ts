// Vitest config for Stryker mutation runs ONLY (stryker.config.json points
// here; plain `npx vitest run` keeps using vitest.config.ts).
//
// Why a separate config: wire-golden.node.test.ts reads its golden fixtures
// from ../../wire-golden/ — Go-engine output that lives OUTSIDE this npm
// package at the repo root — with node:fs, addressed by name. Stryker sandboxes
// the package into .stryker-tmp/ and cannot see files above it, so that suite
// fails with ENOENT before any mutant runs. It still executes in regular CI
// (ts-ci vitest) and locally; mutation testing simply scores against the rest.
//
// The two DOM conformance suites used to be excluded here for the same reason
// and no longer are: render-behavior and render-e2e-golden reach their fixtures
// through Vite imports (?raw and ?url), so the bytes are served by the dev
// server rather than read off a path the sandbox cannot resolve.
import { defineConfig, mergeConfig } from "vitest/config";
import base from "./vitest.config.js";

export default mergeConfig(
  base,
  defineConfig({
    test: {
      exclude: ["node_modules/**", "src/wire-golden.node.test.ts"],
    },
  }),
);
