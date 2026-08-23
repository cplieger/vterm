package terminal

import (
	"path/filepath"
	"testing"

	"github.com/coder/websocket"
)

// TestWireManifestMatchesGoConstants is the Go leg of the manifest's three-way
// conformance guard. The TS leg (web/src/test-helpers/wire-manifest.node.test.ts) proves the
// manifest is regenerable from the TypeScript constants; this proves the same
// numbers match the Go constants a server actually enforces. Without it the
// manifest could stay perfectly in sync with a TypeScript half that had itself
// drifted from the server — the published artifact would then be confidently
// wrong, which is worse than the `sed`-scraping it replaces.
//
// The manifest reports the CLIENT half's directional values, so the mapping is:
//
//	protocolVersion               == WireProtocolVersion (both halves emit the
//	                                 same current revision, per CONTRIBUTING)
//	minimumServerProtocolVersion  != a Go constant by definition (it is the TS
//	                                 receiver's floor), but it must never exceed
//	                                 the Go server's own revision, or the
//	                                 published pair would refuse itself
//	incompatibleCloseCode         == WireIncompatibleCloseCode
func TestWireManifestMatchesGoConstants(t *testing.T) {
	// Read through the EXPORTED decoder rather than a local mirror of the struct.
	// That is what makes this drift guard cover the code consumers actually call:
	// a schema change now fails here in the producer, not one image build at a
	// time in three repos that each re-declared the shape.
	path := filepath.Join("..", "web", "wire-compatibility.json")
	m, err := ReadWireManifest(path)
	if err != nil {
		t.Fatalf("ReadWireManifest(%s) (regenerate with: cd web && UPDATE_GOLDEN=1 npx vitest --run src/test-helpers/wire-manifest.node.test.ts): %v", path, err)
	}

	if m.SchemaVersion != WireManifestSchemaVersion {
		t.Fatalf("manifest schemaVersion = %d, want %d; a consumer rejects an unknown shape, so a bump is a breaking change that must be release-noted", m.SchemaVersion, WireManifestSchemaVersion)
	}
	if m.ProtocolVersion != WireProtocolVersion {
		t.Errorf("manifest protocolVersion = %d, Go WireProtocolVersion = %d; the two halves must emit the same current revision (regenerate the manifest, or fix whichever constant is wrong)",
			m.ProtocolVersion, WireProtocolVersion)
	}
	if websocket.StatusCode(m.IncompatibleCloseCode) != WireIncompatibleCloseCode {
		t.Errorf("manifest incompatibleCloseCode = %d, Go WireIncompatibleCloseCode = %d",
			m.IncompatibleCloseCode, WireIncompatibleCloseCode)
	}
	if m.MinimumServerProtocolVersion > WireProtocolVersion {
		t.Errorf("manifest minimumServerProtocolVersion = %d exceeds the Go server's revision %d; the published pair would refuse itself",
			m.MinimumServerProtocolVersion, WireProtocolVersion)
	}

	// The manifest is exactly the input a consumer's release gate feeds to
	// WirePairIncompatibility: assert the published pair judges itself
	// compatible, which also exercises the within-side coherence invariant on
	// real published numbers rather than on synthetic ones.
	if got := WirePairIncompatibility(WirePair{
		Server: WireEnd{Rev: WireProtocolVersion, MinPeer: MinSupportedClientWireVersion},
		Client: WireEnd{Rev: m.ProtocolVersion, MinPeer: m.MinimumServerProtocolVersion},
	}); got != "" {
		t.Errorf("this server paired with the published manifest is incompatible: %s", got)
	}
}
