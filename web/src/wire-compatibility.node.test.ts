import { describe, expect, it, vi } from "vitest";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { decodeWireBinary as decodePublishedV3 } from "@cplieger/web-terminal-engine-v3";
import { decodeWireBinary } from "./wire-binary.js";
import {
  MIN_SUPPORTED_SERVER_WIRE_VERSION,
  WIRE_COMPATIBILITY,
  WIRE_INCOMPATIBLE_CLOSE_CODE,
  WIRE_PROTOCOL_VERSION,
} from "./wire-compatibility.js";

interface PublishedFixtures {
  schemaVersion: number;
  source: {
    tag: string;
    commit: string;
    wireProtocolVersion: number;
  };
  encoding: "base64";
  fixtures: Record<string, string>;
}

const root = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const published = JSON.parse(
  readFileSync(join(root, "wire-golden", "v3-published.json"), "utf8"),
) as PublishedFixtures;

function fromBase64(encoded: string): ArrayBuffer {
  const buf = Buffer.from(encoded, "base64");
  return buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.byteLength);
}

function loadCurrent(name: string): ArrayBuffer {
  const buf = readFileSync(join(root, "wire-golden", `${name}.bin`));
  return buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.byteLength);
}

const previousExpected = new Map<string, string | null>([
  ["modes", "modes"],
  ["pong", null],
  ["resumeack", "resumeAck"],
  ["screen", "screen"],
  ["scroll", "scroll"],
  ["title", "title"],
]);

const currentExpectedForV3 = new Map<string, string | null>([
  ["ackonly", null],
  ["clipboard", "clipboard"],
  ["modes", "modes"],
  ["pong", null],
  ["resumeack", "resumeAck"],
  ["resumeack-ledgerlost", "resumeAck"],
  ["screen", "screen"],
  ["scroll", "scroll"],
  ["title", "title"],
]);

describe("wire compatibility release metadata", () => {
  it("exports the current revision, directional floor, and definitive close code", () => {
    expect(WIRE_COMPATIBILITY).toEqual({
      protocolVersion: WIRE_PROTOCOL_VERSION,
      minimumServerProtocolVersion: MIN_SUPPORTED_SERVER_WIRE_VERSION,
      incompatibleCloseCode: WIRE_INCOMPATIBLE_CLOSE_CODE,
    });
    expect(WIRE_PROTOCOL_VERSION).toBe(4);
    expect(MIN_SUPPORTED_SERVER_WIRE_VERSION).toBe(3);
    expect(WIRE_INCOMPATIBLE_CLOSE_CODE).toBe(4002);
  });

  it("pins the previous published fixture revision at the supported floor", () => {
    expect(published.schemaVersion).toBe(1);
    expect(published.source.tag).toBe("v2.8.0");
    expect(published.source.commit).toBe("92aa8d4ef83482289eadae35b4055fc410d67f18");
    expect(published.source.wireProtocolVersion).toBe(MIN_SUPPORTED_SERVER_WIRE_VERSION);
  });
});

describe("current decoder against previous published v3 fixtures", () => {
  for (const [name, encoded] of Object.entries(published.fixtures)) {
    it(`decodes ${name}`, () => {
      const decoded = decodeWireBinary(fromBase64(encoded));
      expect(decoded?.type ?? null).toBe(previousExpected.get(name));
    });
  }
});

describe("previous published v3 decoder against current fixtures", () => {
  for (const [name, expectedType] of currentExpectedForV3) {
    it(`tolerates ${name}`, () => {
      const warn = vi.spyOn(console, "warn").mockImplementation(() => {
        /* expected for a current opcode unknown to the previous decoder */
      });
      const decoded = decodePublishedV3(loadCurrent(name));
      expect(decoded?.type ?? null).toBe(expectedType);
      warn.mockRestore();
    });
  }
});
