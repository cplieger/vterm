package terminal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/websocket"
)

// wireManifest mirrors web/wire-compatibility.json, the generated
// language-neutral publication of the wire-compatibility constants. The struct
// is deliberately written the way a THIRD-PARTY consumer would write it (parse
// the schema version, reject an unknown shape, then read the payload), so this
// test also demonstrates the documented consumption contract.
type wireManifest struct {
	WireCompatibility struct {
		ProtocolVersion              int `json:"protocolVersion"`
		MinimumServerProtocolVersion int `json:"minimumServerProtocolVersion"`
		IncompatibleCloseCode        int `json:"incompatibleCloseCode"`
	} `json:"wireCompatibility"`
	SchemaVersion int `json:"schemaVersion"`
}

// TestWireManifestMatchesGoConstants is the Go leg of the manifest's three-way
// conformance guard. The TS leg (web/src/wire-manifest.test.ts) proves the
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
	path := filepath.Join("..", "web", "wire-compatibility.json")
	data, err := os.ReadFile(path) // #nosec G304 -- fixed repo-relative test fixture
	if err != nil {
		t.Fatalf("read %s (regenerate with: cd web && UPDATE_GOLDEN=1 npx vitest --run src/wire-manifest.test.ts): %v", path, err)
	}
	var m wireManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	if m.SchemaVersion != 1 {
		t.Fatalf("manifest schemaVersion = %d, want 1; a consumer rejects an unknown shape, so a bump is a breaking change that must be release-noted", m.SchemaVersion)
	}
	if m.WireCompatibility.ProtocolVersion != WireProtocolVersion {
		t.Errorf("manifest protocolVersion = %d, Go WireProtocolVersion = %d; the two halves must emit the same current revision (regenerate the manifest, or fix whichever constant is wrong)",
			m.WireCompatibility.ProtocolVersion, WireProtocolVersion)
	}
	if websocket.StatusCode(m.WireCompatibility.IncompatibleCloseCode) != WireIncompatibleCloseCode {
		t.Errorf("manifest incompatibleCloseCode = %d, Go WireIncompatibleCloseCode = %d",
			m.WireCompatibility.IncompatibleCloseCode, WireIncompatibleCloseCode)
	}
	if m.WireCompatibility.MinimumServerProtocolVersion > WireProtocolVersion {
		t.Errorf("manifest minimumServerProtocolVersion = %d exceeds the Go server's revision %d; the published pair would refuse itself",
			m.WireCompatibility.MinimumServerProtocolVersion, WireProtocolVersion)
	}

	// The manifest is exactly the input a consumer's release gate feeds to
	// WirePairIncompatibility: assert the published pair judges itself
	// compatible, which also exercises the within-side coherence invariant on
	// real published numbers rather than on synthetic ones.
	if got := WirePairIncompatibility(
		WireProtocolVersion, MinSupportedClientWireVersion,
		m.WireCompatibility.ProtocolVersion, m.WireCompatibility.MinimumServerProtocolVersion,
	); got != "" {
		t.Errorf("this server paired with the published manifest is incompatible: %s", got)
	}
}
