package terminal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type publishedWireFixtures struct {
	Source struct {
		Tag                 string `json:"tag"`
		Commit              string `json:"commit"`
		WireProtocolVersion int    `json:"wireProtocolVersion"`
	} `json:"source"`
	SchemaVersion int `json:"schemaVersion"`
}

func TestWireCompatibilityMetadata(t *testing.T) {
	if WireProtocolVersion < MinSupportedClientWireVersion {
		t.Errorf("current wire version %d is below client floor %d", WireProtocolVersion, MinSupportedClientWireVersion)
	}
	if WireIncompatibleCloseCode < 4000 || WireIncompatibleCloseCode > 4999 {
		t.Errorf("incompatible close code %d is outside the private application range", WireIncompatibleCloseCode)
	}

	path := filepath.Join("..", "wire-golden", "v3-published.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read previous published fixture manifest: %v", err)
	}
	var fixtures publishedWireFixtures
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("decode previous published fixture manifest: %v", err)
	}
	if fixtures.SchemaVersion != 1 {
		t.Errorf("fixture manifest schema = %d, want 1", fixtures.SchemaVersion)
	}
	if fixtures.Source.Tag != "v2.8.0" {
		t.Errorf("fixture source tag = %q, want v2.8.0", fixtures.Source.Tag)
	}
	if fixtures.Source.Commit == "" {
		t.Error("fixture source commit is empty")
	}
	if fixtures.Source.WireProtocolVersion != MinSupportedClientWireVersion {
		t.Errorf("published fixture revision = %d, server client floor = %d", fixtures.Source.WireProtocolVersion, MinSupportedClientWireVersion)
	}
}

// TestWirePairIncompatibility pins the exported build-time pair comparator:
// which direction each floor bounds, that both bounds are EXCLUSIVE (a
// revision exactly at the peer's floor is compatible), that a higher-than-
// known revision is compatible (matching runtime's warn-but-continue), and
// that a non-positive input is reported as a caller error rather than
// tolerated as runtime's version-silent 0.
func TestWirePairIncompatibility(t *testing.T) {
	cases := map[string]struct {
		serverRev, serverMinClient, clientRev, clientMinServer int
		wantCompatible                                         bool
		wantSubstr                                             string
	}{
		"current pairing is compatible": {
			WireProtocolVersion, MinSupportedClientWireVersion,
			WireProtocolVersion, MinSupportedClientWireVersion, true, "",
		},
		"client exactly at the server floor is compatible": {
			WireProtocolVersion, MinSupportedClientWireVersion,
			MinSupportedClientWireVersion, MinSupportedClientWireVersion, true, "",
		},
		"server exactly at the client floor is compatible": {
			MinSupportedClientWireVersion, MinSupportedClientWireVersion,
			WireProtocolVersion, MinSupportedClientWireVersion, true, "",
		},
		"client one below the server floor is refused": {
			WireProtocolVersion, MinSupportedClientWireVersion,
			MinSupportedClientWireVersion - 1, MinSupportedClientWireVersion, false, "TS half is behind",
		},
		"server one below the client floor is refused": {
			MinSupportedClientWireVersion - 1, MinSupportedClientWireVersion,
			WireProtocolVersion, MinSupportedClientWireVersion, false, "Go half is behind",
		},
		"future client revision is compatible": {
			WireProtocolVersion, MinSupportedClientWireVersion,
			WireProtocolVersion + 5, MinSupportedClientWireVersion, true, "",
		},
		"future server revision is compatible": {
			WireProtocolVersion + 5, MinSupportedClientWireVersion,
			WireProtocolVersion, MinSupportedClientWireVersion, true, "",
		},
		"zero client revision is a caller error, not version-silent": {
			WireProtocolVersion, MinSupportedClientWireVersion,
			0, MinSupportedClientWireVersion, false, "client wire revisions must be positive",
		},
		"zero client floor is a caller error": {
			WireProtocolVersion, MinSupportedClientWireVersion,
			WireProtocolVersion, 0, false, "client wire revisions must be positive",
		},
		"negative server revision is a caller error": {
			-1, MinSupportedClientWireVersion,
			WireProtocolVersion, MinSupportedClientWireVersion, false, "server wire revisions must be positive",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := WirePairIncompatibility(tc.serverRev, tc.serverMinClient, tc.clientRev, tc.clientMinServer)
			if tc.wantCompatible {
				if got != "" {
					t.Errorf("WirePairIncompatibility(%d,%d,%d,%d) = %q, want compatible (empty)",
						tc.serverRev, tc.serverMinClient, tc.clientRev, tc.clientMinServer, got)
				}
				return
			}
			if got == "" {
				t.Fatalf("WirePairIncompatibility(%d,%d,%d,%d) = compatible, want a reason containing %q",
					tc.serverRev, tc.serverMinClient, tc.clientRev, tc.clientMinServer, tc.wantSubstr)
			}
			if !strings.Contains(got, tc.wantSubstr) {
				t.Errorf("reason = %q, want it to contain %q", got, tc.wantSubstr)
			}
		})
	}
}

// TestWirePairIncompatibility_agreesWithRuntimeFloor guards the drift class the
// build gate exists to catch: the exported comparator's client-side verdict
// must match the runtime resume check's refusal boundary (terminal.go's
// ProtocolVersion < minSupportedClientWireVersion), so a consumer's gate can
// never pass a pairing the server would close with WireIncompatibleCloseCode.
func TestWirePairIncompatibility_agreesWithRuntimeFloor(t *testing.T) {
	for clientRev := 1; clientRev <= WireProtocolVersion+2; clientRev++ {
		runtimeRefuses := clientRev < minSupportedClientWireVersion
		gateRefuses := WirePairIncompatibility(
			WireProtocolVersion, MinSupportedClientWireVersion,
			clientRev, MinSupportedClientWireVersion,
		) != ""
		if runtimeRefuses != gateRefuses {
			t.Errorf("client rev %d: runtime refuses=%v but build gate refuses=%v; the gate's rule drifted from the handshake",
				clientRev, runtimeRefuses, gateRefuses)
		}
	}
}

// TestLogID pins the session-token log-truncation contract. The full session
// id is a WS resume capability token, so this is a confidentiality boundary
// (CWE-532), not cosmetics: the prefix length caps how much token entropy
// reaches aggregated logs, and the ellipsis marks the value as truncated so a
// reader never mistakes it for a whole id. Every consumer logs session ids
// through this function; without this test a length change or a dropped call
// would leak silently.
func TestLogID(t *testing.T) {
	cases := map[string]struct {
		in, want string
	}{
		"32-hex session id keeps 8 bytes plus the ellipsis": {
			"0123456789abcdef0123456789abcdef", "01234567\u2026",
		},
		"nine bytes truncate":                       {"123456789", "12345678\u2026"},
		"exactly eight bytes pass through unmarked": {"12345678", "12345678"},
		"short id passes through":                   {"abc", "abc"},
		"empty stays empty":                         {"", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := LogID(tc.in); got != tc.want {
				t.Errorf("LogID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestLogID_neverEmitsAFullToken is the invariant that matters operationally:
// for any id longer than the prefix, the logged form must not contain the
// whole id, must not exceed the prefix plus marker, and must carry the marker.
// A future "improvement" that widens the prefix or drops the ellipsis fails
// here rather than in a log aggregator.
func TestLogID_neverEmitsAFullToken(t *testing.T) {
	const prefix = 8
	for _, id := range []string{
		"0123456789abcdef0123456789abcdef",
		"aaaaaaaaa",
		strings.Repeat("f", 128),
	} {
		got := LogID(id)
		if strings.Contains(got, id) {
			t.Errorf("LogID(%q) = %q, which still contains the full id", id, got)
		}
		if !strings.HasSuffix(got, "\u2026") {
			t.Errorf("LogID(%q) = %q, want the truncation marker", id, got)
		}
		if trimmed := strings.TrimSuffix(got, "\u2026"); len(trimmed) != prefix {
			t.Errorf("LogID(%q) kept %d bytes, want %d", id, len(trimmed), prefix)
		}
		if !strings.HasPrefix(id, strings.TrimSuffix(got, "\u2026")) {
			t.Errorf("LogID(%q) = %q, which is not a prefix of the input", id, got)
		}
	}
}
