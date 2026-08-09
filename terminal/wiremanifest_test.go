package terminal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The decoder's whole contract is that NO failure is a compatibility verdict: a
// consumer that turned an unreadable or unknown-schema manifest into "the wire
// pair is incompatible" would send a maintainer to move exactly the wrong thing.
// So every case below asserts an error, and the schema case asserts a
// distinguishable one.

func TestDecodeWireManifestReadsThePublishedShape(t *testing.T) {
	t.Parallel()
	m, err := DecodeWireManifest([]byte(`{
	  "schemaVersion": 1,
	  "generatedBy": "web/src/wire-manifest.ts",
	  "wireCompatibility": {
	    "protocolVersion": 4,
	    "minimumServerProtocolVersion": 3,
	    "incompatibleCloseCode": 4002
	  }
	}`))
	if err != nil {
		t.Fatalf("DecodeWireManifest: %v", err)
	}
	if m.SchemaVersion != 1 || m.ProtocolVersion != 4 || m.MinimumServerProtocolVersion != 3 || m.IncompatibleCloseCode != 4002 {
		t.Errorf("decoded %+v, want schema 1 / rev 4 / floor 3 / close 4002", m)
	}
}

// An unknown schema must be TELLABLE from every other failure, because its remedy
// is the opposite one: bump the reader, never move a wire pin.
func TestDecodeWireManifestRejectsAnUnknownSchema(t *testing.T) {
	t.Parallel()
	_, err := DecodeWireManifest([]byte(`{"schemaVersion":99,"wireCompatibility":{"protocolVersion":4,"minimumServerProtocolVersion":3}}`))
	if !errors.Is(err, ErrWireManifestSchema) {
		t.Fatalf("err = %v, want ErrWireManifestSchema so a gate can say 'bump the reader' instead of 'move a pin'", err)
	}
}

// A future ADDITIVE field must not break existing readers — that is what the
// schemaVersion gate is for, so tolerating unknown fields is the deliberate
// counterpart to refusing an unknown version.
func TestDecodeWireManifestToleratesAnUnknownField(t *testing.T) {
	t.Parallel()
	m, err := DecodeWireManifest([]byte(`{"schemaVersion":1,"somethingNew":"x","wireCompatibility":{"protocolVersion":4,"minimumServerProtocolVersion":3,"incompatibleCloseCode":4002,"alsoNew":1}}`))
	if err != nil {
		t.Fatalf("an additive field broke the decoder: %v", err)
	}
	if m.ProtocolVersion != 4 {
		t.Errorf("ProtocolVersion = %d, want 4", m.ProtocolVersion)
	}
}

func TestDecodeWireManifestRejectsUnusableRevisions(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"missing wireCompatibility": `{"schemaVersion":1}`,
		"zero protocolVersion":      `{"schemaVersion":1,"wireCompatibility":{"protocolVersion":0,"minimumServerProtocolVersion":3}}`,
		"zero floor":                `{"schemaVersion":1,"wireCompatibility":{"protocolVersion":4,"minimumServerProtocolVersion":0}}`,
		"negative floor":            `{"schemaVersion":1,"wireCompatibility":{"protocolVersion":4,"minimumServerProtocolVersion":-1}}`,
		"malformed json":            `{"schemaVersion":1,`,
		"empty":                     ``,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeWireManifest([]byte(body)); err == nil {
				t.Error("decoded successfully; a gate would then compare a value it never read")
			}
		})
	}
}

func TestReadWireManifestReportsAnUnreadablePath(t *testing.T) {
	t.Parallel()
	if _, err := ReadWireManifest(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("an absent manifest read successfully")
	}
}

func TestReadWireManifestReadsAFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wire-compatibility.json")
	body := `{"schemaVersion":1,"wireCompatibility":{"protocolVersion":4,"minimumServerProtocolVersion":3,"incompatibleCloseCode":4002}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	m, err := ReadWireManifest(path)
	if err != nil {
		t.Fatalf("ReadWireManifest: %v", err)
	}
	if m.ProtocolVersion != 4 || m.MinimumServerProtocolVersion != 3 {
		t.Errorf("decoded %+v, want rev 4 / floor 3", m)
	}
}

// The decoded manifest is exactly the input a release gate feeds the comparator,
// so pin that the two fit together without the caller reshaping anything.
func TestDecodedManifestFeedsWirePairIncompatibility(t *testing.T) {
	t.Parallel()
	m, err := ReadWireManifest(filepath.Join("..", "web", "wire-compatibility.json"))
	if err != nil {
		t.Fatalf("ReadWireManifest: %v", err)
	}
	if got := WirePairIncompatibility(
		WireProtocolVersion, MinSupportedClientWireVersion,
		m.ProtocolVersion, m.MinimumServerProtocolVersion,
	); got != "" {
		t.Errorf("the published manifest paired with this server is incompatible: %s", got)
	}
}
