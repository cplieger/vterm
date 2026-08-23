// Drift guard + TypeScript half of the three-way conformance check for the
// generated wire-compatibility manifest (web/wire-compatibility.json).
//
// The manifest is a GENERATED artifact that is checked in, because the publish
// pipeline has no build step. A generated-and-committed file with no guard goes
// stale the first time someone edits the source constants — which is precisely
// the "second source of truth" failure the manifest exists to remove — so this
// test regenerates it and fails on any byte of difference.
//
// Regenerate (same code path that compares, so the two can never disagree):
//
//   cd web && UPDATE_GOLDEN=1 npx vitest --run src/test-helpers/wire-manifest.node.test.ts
//
// The third surface, the Go constants, is pinned by
// terminal/wire_manifest_test.go; between the two, the manifest, the TS
// constants and the Go constants cannot diverge without a red test.
import { describe, expect, it } from "vitest";
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  WIRE_MANIFEST_PATH,
  WIRE_MANIFEST_SCHEMA_VERSION,
  buildWireManifest,
  renderWireManifest,
} from "./wire-manifest.js";
import {
  MIN_SUPPORTED_SERVER_WIRE_VERSION,
  WIRE_INCOMPATIBLE_CLOSE_CODE,
  WIRE_PROTOCOL_VERSION,
} from "../wire-compatibility.js";

const manifestPath = join(dirname(fileURLToPath(import.meta.url)), "..", "..", WIRE_MANIFEST_PATH);

describe("generated wire-compatibility manifest", () => {
  it("matches the checked-in artifact byte for byte", () => {
    const rendered = renderWireManifest();
    if (process.env["UPDATE_GOLDEN"]) {
      writeFileSync(manifestPath, rendered, { encoding: "utf8", mode: 0o600 });
    }
    const onDisk = readFileSync(manifestPath, "utf8");
    expect(
      onDisk,
      `web/${WIRE_MANIFEST_PATH} drifted from web/src/wire-compatibility.ts. ` +
        "Regenerate with: cd web && UPDATE_GOLDEN=1 npx vitest --run src/test-helpers/wire-manifest.node.test.ts " +
        "(and re-run `go test ./terminal/ -run TestWireManifest` so the Go constants are re-checked).",
    ).toBe(rendered);
  });

  it("pins the manifest values to the TypeScript constants", () => {
    const manifest = JSON.parse(readFileSync(manifestPath, "utf8")) as unknown as {
      schemaVersion: number;
      wireCompatibility: {
        protocolVersion: number;
        minimumServerProtocolVersion: number;
        incompatibleCloseCode: number;
      };
    };
    expect(manifest.schemaVersion).toBe(WIRE_MANIFEST_SCHEMA_VERSION);
    expect(manifest.wireCompatibility.protocolVersion).toBe(WIRE_PROTOCOL_VERSION);
    expect(manifest.wireCompatibility.minimumServerProtocolVersion).toBe(
      MIN_SUPPORTED_SERVER_WIRE_VERSION,
    );
    expect(manifest.wireCompatibility.incompatibleCloseCode).toBe(WIRE_INCOMPATIBLE_CLOSE_CODE);
  });

  it("pins the schema revision, so a shape change is a deliberate edit", () => {
    // A consumer rejects an unknown schemaVersion, so bumping this is a
    // breaking change to a published artifact: it must be a considered edit
    // here (and a release note), never a side effect of another change.
    expect(WIRE_MANIFEST_SCHEMA_VERSION).toBe(1);
    expect(buildWireManifest().schemaVersion).toBe(1);
  });

  it("carries no package version, which the publish step would leave stale", () => {
    // `npm pkg set version` rewrites package.json at publish time with no
    // regeneration of this artifact, so any version embedded here would ship
    // frozen at the repo's 0.1.0 placeholder. Keep the manifest version-free.
    const raw = readFileSync(manifestPath, "utf8");
    expect(raw).not.toContain("0.1.0");
    expect(Object.keys(buildWireManifest())).toEqual([
      "schemaVersion",
      "generatedBy",
      "wireCompatibility",
    ]);
  });
});
