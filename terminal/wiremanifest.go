package terminal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// The wire-compatibility manifest is this engine's own published data format:
// web/wire-compatibility.json, rendered from the TypeScript constants by
// web/src/wire-manifest.ts and checked in, so it ships inside every npm and JSR
// release. It exists because a consumer that is neither TypeScript nor Go — a
// Dockerfile, a shell release gate — had no machine-readable surface for the
// wire revisions and resorted to scraping the TypeScript source with sed, which
// breaks on any reformat.
//
// A GO consumer needs it for a less obvious reason, and that is why this decoder
// exists. The Go module carries the SERVER half as constants, but a release gate
// compares server against CLIENT, and the client half ships in the npm package —
// pinned independently of the Go module, by design. So the gate has to read the
// vendored artifact's manifest to learn the half its own binary cannot know.
//
// All three family consumers were hand-declaring this struct to do that (and two
// were still scraping TypeScript instead). The schema is the engine's, so the
// decoder is too: a format's producer exporting its own reader beats N consumers
// mirroring it, and this engine already maintained the mirror in a test.
// Remediation, exit codes and refusal wording stay with each gate, which is the
// documented split — the engine says what the numbers are, the consumer says
// which pin to move.

// WireManifestSchemaVersion is the only manifest schema this decoder reads. The
// manifest declares its own version first precisely so a consumer refuses an
// unknown one instead of guessing at a moved field.
//
// It is bumped only for a breaking change to the manifest's own LAYOUT (a removed
// or repurposed field), never for a wire-protocol revision. Its TypeScript twin
// is WIRE_MANIFEST_SCHEMA_VERSION; the in-repo conformance test pins the two
// together.
const WireManifestSchemaVersion = 1

// ErrWireManifestSchema reports a manifest whose schemaVersion this decoder does
// not read.
//
// Worth distinguishing from every other read failure because the REMEDY differs
// and consumers act on it: an unknown schema means the reader is behind the
// artifact, so the fix is to bump the consumer, never to move a wire pin. A gate
// that reported it as a wire incompatibility would send a maintainer to change
// exactly the wrong thing.
var ErrWireManifestSchema = errors.New("terminal: unsupported wire-compatibility manifest schema")

// WireManifest is the decoded manifest.
//
// Flattened relative to the file, deliberately: the nesting under
// `wireCompatibility` exists so file-level metadata can never collide with a
// protocol field name, which is a property of the FILE's layout and not of the
// values a caller wants.
type WireManifest struct {
	// SchemaVersion is the layout revision, already validated against
	// WireManifestSchemaVersion by the time a caller sees it.
	SchemaVersion int
	// ProtocolVersion is the client's current wire revision.
	ProtocolVersion int
	// MinimumServerProtocolVersion is the client's directional floor: the oldest
	// server revision it will still decode.
	MinimumServerProtocolVersion int
	// IncompatibleCloseCode is the close code the runtime handshake uses to
	// refuse a declared-incompatible peer. Informational for a build gate.
	IncompatibleCloseCode int
}

// ReadWireManifest reads and validates the wire-compatibility manifest at path,
// which is the `wire-compatibility.json` at the root of a vendored engine npm
// package.
//
// Every failure is an error rather than a zero value, and none of them is a
// compatibility verdict: an unreadable, malformed, unknown-schema or
// non-positive manifest means the caller cannot SEE the client's declaration,
// which is a broken gate. A caller must not turn any of these into "bump a pin".
//
// The non-positive check belongs here rather than in the comparator because it
// is a property of the file: WirePairIncompatibility treats a non-positive input
// as a caller error, and a manifest that carries one is malformed at the source.
func ReadWireManifest(path string) (WireManifest, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path is the caller's own build argument
	if err != nil {
		return WireManifest{}, fmt.Errorf("terminal: read wire-compatibility manifest: %w", err)
	}
	return DecodeWireManifest(data)
}

// DecodeWireManifest is ReadWireManifest over bytes already in hand, for a caller
// that obtained the artifact some other way (an embedded copy, a registry fetch).
func DecodeWireManifest(data []byte) (WireManifest, error) {
	// Only the published fields are declared; an unknown field is tolerated so an
	// additive manifest revision does not have to break every consumer at once,
	// which is what the schemaVersion gate is for.
	var raw struct {
		WireCompatibility struct {
			ProtocolVersion              int `json:"protocolVersion"`
			MinimumServerProtocolVersion int `json:"minimumServerProtocolVersion"`
			IncompatibleCloseCode        int `json:"incompatibleCloseCode"`
		} `json:"wireCompatibility"`
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return WireManifest{}, fmt.Errorf("terminal: parse wire-compatibility manifest: %w", err)
	}
	if raw.SchemaVersion != WireManifestSchemaVersion {
		return WireManifest{}, fmt.Errorf("%w: manifest declares %d, this build reads %d",
			ErrWireManifestSchema, raw.SchemaVersion, WireManifestSchemaVersion)
	}
	m := WireManifest{
		SchemaVersion:                raw.SchemaVersion,
		ProtocolVersion:              raw.WireCompatibility.ProtocolVersion,
		MinimumServerProtocolVersion: raw.WireCompatibility.MinimumServerProtocolVersion,
		IncompatibleCloseCode:        raw.WireCompatibility.IncompatibleCloseCode,
	}
	if m.ProtocolVersion <= 0 || m.MinimumServerProtocolVersion <= 0 {
		return WireManifest{}, fmt.Errorf(
			"terminal: wire-compatibility manifest carries no usable revisions (protocolVersion %d, minimumServerProtocolVersion %d)",
			m.ProtocolVersion, m.MinimumServerProtocolVersion)
	}
	return m, nil
}
