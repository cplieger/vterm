// Generator for the published, language-neutral wire-compatibility manifest
// (`web/wire-compatibility.json`).
//
// Why it exists: the wire revision and directional floor are published as
// TypeScript constants (wire-compatibility.ts) and as Go constants
// (terminal.WireProtocolVersion / MinSupportedClientWireVersion). A consumer
// that is neither — a Dockerfile, a shell release gate, a Python CI script —
// had no machine-readable surface to read them from and resorted to scraping
// the TypeScript source with `sed`, which breaks silently on any reformat.
//
// This module renders that surface FROM the existing WIRE_COMPATIBILITY export,
// so the numbers keep exactly one source of truth in the TypeScript half. The
// rendered text is checked into the repo (see the drift guard in
// wire-manifest.test.ts) because the publish pipeline runs no build step: npm
// and JSR publish the working tree as-is, so a manifest generated at publish
// time would never exist in the tarball.
//
// The manifest deliberately does NOT carry the package version. `npm pkg set
// version` injects that at publish time, after checkout, with no regeneration
// step — an embedded version would be frozen at the repo's placeholder (0.1.0)
// in every published artifact. Consumers read the package version from
// package.json / jsr.json, which the pipeline does rewrite.
//
// Public-artifact contract: see the "Wire compatibility manifest" section of
// web/README.md for what a consumer may rely on and what counts as a breaking
// change.

import { WIRE_COMPATIBILITY, type WireCompatibility } from "./wire-compatibility.js";

/**
 * Schema revision of the manifest FILE, independent of the wire protocol
 * revision it reports. A consumer must reject a `schemaVersion` it does not
 * understand rather than guess at the shape. Bumped only for a breaking
 * change to the manifest's own layout (a removed or repurposed field).
 */
export const WIRE_MANIFEST_SCHEMA_VERSION = 1;

/** Path of the checked-in artifact this module renders, relative to `web/`. */
export const WIRE_MANIFEST_PATH = "wire-compatibility.json";

/** Shape of the generated wire-compatibility manifest. */
export interface WireManifest {
  /** Schema revision of this file's layout (see WIRE_MANIFEST_SCHEMA_VERSION). */
  readonly schemaVersion: number;
  /** Informational provenance note; not part of the parsed contract. */
  readonly generatedBy: string;
  /** The same values the TypeScript `WIRE_COMPATIBILITY` export carries. */
  readonly wireCompatibility: WireCompatibility;
}

/**
 * Builds the manifest object from the live TypeScript constants. Nesting the
 * protocol values under `wireCompatibility` keeps file-level metadata
 * (`schemaVersion`) in a separate namespace from the payload, so a future
 * metadata field can never collide with a protocol field name.
 */
export function buildWireManifest(): WireManifest {
  return {
    schemaVersion: WIRE_MANIFEST_SCHEMA_VERSION,
    generatedBy: "web/src/wire-manifest.ts",
    wireCompatibility: {
      protocolVersion: WIRE_COMPATIBILITY.protocolVersion,
      minimumServerProtocolVersion: WIRE_COMPATIBILITY.minimumServerProtocolVersion,
      incompatibleCloseCode: WIRE_COMPATIBILITY.incompatibleCloseCode,
    },
  };
}

/**
 * Renders the exact bytes of the checked-in manifest: two-space indentation and
 * a trailing newline, which is what `prettier --check` requires of a JSON file
 * in this repo. Comparing rendered text (not parsed objects) is what makes the
 * drift guard able to fail on a hand edit that happens to parse the same.
 */
export function renderWireManifest(): string {
  return `${JSON.stringify(buildWireManifest(), null, 2)}\n`;
}
