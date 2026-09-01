package vt

import (
	"slices"
	"strings"
	"testing"
)

// TestDECRQSS_SGRDefault verifies a DECRQSS SGR query on a default style
// returns the SGR-0 status string.
func TestDECRQSS_SGRDefault(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1bP$qm\x1b\\"))
	want := "\x1bP1$r0m\x1b\\"
	if got := string(s.response); got != want {
		t.Errorf("DECRQSS SGR default = %q, want %q", got, want)
	}
}

// TestDECRQSS_SGRReflectsStyle verifies the SGR status string reflects the
// current style (bold + red).
func TestDECRQSS_SGRReflectsStyle(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[1;31m")) // bold + red
	s.Write([]byte("\x1bP$qm\x1b\\"))
	want := "\x1bP1$r0;1;31m\x1b\\"
	if got := string(s.response); got != want {
		t.Errorf("DECRQSS SGR = %q, want %q", got, want)
	}
}

// TestDECRQSS_SGRAllAttributesPresent verifies every set attribute appears in
// the SGR status string when many attributes are enabled at once.
func TestDECRQSS_SGRAllAttributesPresent(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[1;2;3;4;5;7;8;9;53m"))
	s.response = nil
	s.Write([]byte("\x1bP$qm\x1b\\"))
	got := string(s.response)
	if !strings.HasPrefix(got, "\x1bP1$r") || !strings.HasSuffix(got, "m\x1b\\") {
		t.Fatalf("response format: %q", got)
	}
	params := got[len("\x1bP1$r") : len(got)-len("m\x1b\\")]
	// Split into the actual ';'-separated SGR tokens and check membership, not
	// substring: a naive strings.Contains(params, "3") would false-match the "3"
	// inside "53" (overline) and never catch a dropped italic (3).
	tokens := strings.Split(params, ";")
	for _, want := range []string{"1", "2", "3", "4", "5", "7", "8", "9", "53"} {
		if !slices.Contains(tokens, want) {
			t.Errorf("missing attribute %q in %q", want, params)
		}
	}
}

// TestDECRQSS_DECSTBMDefault verifies a DECRQSS scroll-region query returns the
// full-screen default region.
func TestDECRQSS_DECSTBMDefault(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1bP$qr\x1b\\"))
	want := "\x1bP1$r1;24r\x1b\\"
	if got := string(s.response); got != want {
		t.Errorf("DECRQSS DECSTBM default = %q, want %q", got, want)
	}
}

// TestDECRQSS_DECSTBMCustom verifies the scroll-region query reflects a custom
// region set via DECSTBM.
func TestDECRQSS_DECSTBMCustom(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[5;20r"))
	s.Write([]byte("\x1bP$qr\x1b\\"))
	want := "\x1bP1$r5;20r\x1b\\"
	if got := string(s.response); got != want {
		t.Errorf("DECRQSS DECSTBM = %q, want %q", got, want)
	}
}

// TestDECRQSS_DECSCUSRStyles verifies the cursor-style query echoes each style
// value set via DECSCUSR.
func TestDECRQSS_DECSCUSRStyles(t *testing.T) {
	cases := []struct {
		name string
		set  string
		want string
	}{
		{"0 blinking block", "\x1b[0 q", "\x1bP1$r0 q\x1b\\"},
		{"1 blinking block", "\x1b[1 q", "\x1bP1$r1 q\x1b\\"},
		{"2 steady block", "\x1b[2 q", "\x1bP1$r2 q\x1b\\"},
		{"3 blinking underline", "\x1b[3 q", "\x1bP1$r3 q\x1b\\"},
		{"4 steady underline", "\x1b[4 q", "\x1bP1$r4 q\x1b\\"},
		{"5 blinking bar", "\x1b[5 q", "\x1bP1$r5 q\x1b\\"},
		{"6 steady bar", "\x1b[6 q", "\x1bP1$r6 q\x1b\\"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(24, 80)
			s.Write([]byte(tc.set))
			s.Write([]byte("\x1bP$q q\x1b\\"))
			if got := string(s.response); got != tc.want {
				t.Errorf("DECRQSS DECSCUSR after %q = %q, want %q", tc.set, got, tc.want)
			}
		})
	}
}

// TestDECRQSS_DECSCA verifies the character-protection status query (" q)
// reflects the current DECSCA attribute.
func TestDECRQSS_DECSCA(t *testing.T) {
	s := New(24, 80)
	// Default: not protected -> Ps 0.
	s.Write([]byte("\x1bP$q\"q\x1b\\"))
	if got, want := string(s.response), "\x1bP1$r0\"q\x1b\\"; got != want {
		t.Errorf("DECRQSS DECSCA default = %q, want %q", got, want)
	}
	// After DECSCA marks cells protected -> Ps 1.
	s.response = nil
	s.Write([]byte("\x1b[1\"q"))        // DECSCA: protect
	s.Write([]byte("\x1bP$q\"q\x1b\\")) // DECRQSS DECSCA
	if got, want := string(s.response), "\x1bP1$r1\"q\x1b\\"; got != want {
		t.Errorf("DECRQSS DECSCA after protect = %q, want %q", got, want)
	}
}

// TestDECRQSS_DECSCL verifies the conformance-level query (" p) reports the
// tracked level (default 65 = VT500 level 5) and reflects a DECSCL set.
func TestDECRQSS_DECSCL(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1bP$q\"p\x1b\\"))
	if got, want := string(s.response), "\x1bP1$r65;1\"p\x1b\\"; got != want {
		t.Errorf("DECRQSS DECSCL (default) = %q, want %q", got, want)
	}
	// After DECSCL sets level 4 (63), the query reports it back.
	s.response = nil
	s.Write([]byte("\x1b[63;1\"p"))
	s.Write([]byte("\x1bP$q\"p\x1b\\"))
	if got, want := string(s.response), "\x1bP1$r63;1\"p\x1b\\"; got != want {
		t.Errorf("DECRQSS DECSCL (after set 63) = %q, want %q", got, want)
	}
}

// TestDECRQSS_UnsupportedQueries verifies every unrecognized or malformed query
// returns the invalid-status response (0$r).
func TestDECRQSS_UnsupportedQueries(t *testing.T) {
	bad := []string{"z", "Z", "!p", "mm", "rr", " qq", "123", "", strings.Repeat("x", 200)}
	want := "\x1bP0$r\x1b\\"
	for _, q := range bad {
		t.Run(q, func(t *testing.T) {
			s := New(24, 80)
			s.Write([]byte("\x1bP$q" + q + "\x1b\\"))
			if got := string(s.response); got != want {
				t.Errorf("DECRQSS unsupported %q = %q, want %q", q, got, want)
			}
		})
	}
}

// TestDECRQSSBufferBoundedAtMax verifies the DECRQSS data buffer is capped at
// maxDCSLen, and an oversized (garbage) query still yields an invalid response
// and returns the parser to Ground after ST.
func TestDECRQSSBufferBoundedAtMax(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1bP$q"))
	data := make([]byte, maxDCSLen+10)
	for i := range data {
		data[i] = 'A'
	}
	s.Write(data)
	if len(s.dcsBuf) != maxDCSLen {
		t.Errorf("dcsBuf = %d, want %d (capped)", len(s.dcsBuf), maxDCSLen)
	}
	s.Write([]byte("\x1b\\"))
	if !strings.HasPrefix(string(s.response), "\x1bP0$r") {
		t.Errorf("oversized DECRQSS response = %q, want invalid (0$r) prefix", s.response)
	}
	if s.pState != stGround {
		t.Errorf("state after ST = %d, want Ground", s.pState)
	}
}

// TestUnknownDCSNotBuffered verifies an unknown DCS (unrecognized final byte)
// buffers no data and returns to Ground on ST.
func TestUnknownDCSNotBuffered(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1bPz")) // unknown DCS final 'z'
	s.Write([]byte("this should not be buffered at all"))
	if len(s.dcsBuf) != 0 {
		t.Errorf("unknown DCS buffered %d bytes, want 0", len(s.dcsBuf))
	}
	s.Write([]byte("\x1b\\"))
	if s.pState != stGround {
		t.Errorf("state after unknown DCS ST = %d, want Ground", s.pState)
	}
	// Observable outcome: an unknown DCS never replies.
	if len(s.response) != 0 {
		t.Errorf("unknown DCS produced a response %q, want none", s.response)
	}
}

// TestDCS8BitSTTerminates verifies the 8-bit ST (0x9C) terminates a DECRQSS and
// produces a response.
func TestDCS8BitSTTerminates(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1bP$qm")) // DECRQSS for SGR
	s.Write([]byte{0x9C})       // 8-bit ST
	if s.pState != stGround {
		t.Errorf("8-bit ST did not terminate DCS: state=%d", s.pState)
	}
	if len(s.response) == 0 {
		t.Error("DECRQSS with 8-bit ST produced no response")
	}
}

// TestMultipleSequentialDECRQSS verifies two DECRQSS queries against different
// state produce independent (different) responses.
func TestMultipleSequentialDECRQSS(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1bP$qm\x1b\\"))
	first := string(s.response)
	s.response = nil
	s.Write([]byte("\x1b[1m"))
	s.Write([]byte("\x1bP$qm\x1b\\"))
	second := string(s.response)
	if first == second {
		t.Error("two DECRQSS with different state gave same response")
	}
}

// TestDCSAbortedByCAN verifies CAN aborts an in-progress DCS without dispatching
// (no response).
func TestDCSAbortedByCAN(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1bP$qm")) // DECRQSS for SGR
	s.Write([]byte{0x18})       // CAN
	if s.pState != stGround {
		t.Errorf("CAN did not abort DCS: state=%d", s.pState)
	}
	if len(s.response) != 0 {
		t.Errorf("CAN in DCS produced response: %q", s.response)
	}
}

// TestDCSAbortedBySUB verifies SUB aborts an in-progress DCS without dispatching.
func TestDCSAbortedBySUB(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1bP$qm")) // DECRQSS for SGR
	s.Write([]byte{0x1A})       // SUB
	if s.pState != stGround {
		t.Errorf("SUB did not abort DCS: state=%d", s.pState)
	}
	if len(s.response) != 0 {
		t.Errorf("SUB in DCS produced response: %q", s.response)
	}
}

// TestXTGETTCAP verifies XTGETTCAP (DCS + q <hexname> ST): the color-count
// capability ("Co"/"colors") is answered with the hex-encoded value 256 in the
// valid form (DCS 1 + r name=value ST), while every other terminfo name is
// reported invalid (DCS 0 + r name ST) so the app falls back to its terminfo
// database. Names and the "256" value are hex per the XTGETTCAP spec: "Co" =
// 436F, "colors" = 636F6C6F7273, "TN" = 544E, "256" = 323536.
func TestXTGETTCAP(t *testing.T) {
	cases := []struct {
		name string
		hex  string // hex-encoded capability name(s) sent in the query
		want string
	}{
		{"Co color count", "436F", "\x1bP1+r436F=323536\x1b\\"},
		{"colors alias", "636F6C6F7273", "\x1bP1+r636F6C6F7273=323536\x1b\\"},
		{"unknown TN invalid", "544E", "\x1bP0+r544E\x1b\\"},
		// A multi-name query answers each capability in order.
		{"mixed Co;TN", "436F;544E", "\x1bP1+r436F=323536\x1b\\\x1bP0+r544E\x1b\\"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(24, 80)
			s.Write([]byte("\x1bP+q" + tc.hex + "\x1b\\"))
			if got := string(s.response); got != tc.want {
				t.Errorf("XTGETTCAP %q = %q, want %q", tc.hex, got, tc.want)
			}
		})
	}
}

// TestXTGETTCAPMalformedNameNotEchoed verifies a malformed (non-hex or
// odd-length) XTGETTCAP capability name is skipped with NO reply, not echoed
// back. Echoing the raw token would inject attacker-controlled bytes (e.g.
// CR/LF) into the PTY as input. Covers decodeHexString's error return and
// handleXTGetTcap's name=="" skip branch, which the valid-hex-only XTGETTCAP
// test never exercises.
func TestXTGETTCAPMalformedNameNotEchoed(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1bP+qZZ\x1b\\")) // "ZZ" is not valid hex
	if len(s.response) != 0 {
		t.Errorf("non-hex XTGETTCAP name produced reply %q, want none (skipped, not echoed)", s.response)
	}
	s.response = nil
	s.Write([]byte("\x1bP+qabc\x1b\\")) // 3 chars: odd-length hex
	if len(s.response) != 0 {
		t.Errorf("odd-length XTGETTCAP name produced reply %q, want none (skipped, not echoed)", s.response)
	}
}

func TestDECRQSS_AdditionalSelectors(t *testing.T) {
	cases := []struct {
		name, query, want string
	}{
		{"DECSLRM margins default", "s", "\x1bP1$r1;80s\x1b\\"},
		{"DECSLPP lines-per-page default", "t", "\x1bP1$r24t\x1b\\"},
		{"DECSNLS lines-per-screen default", "*|", "\x1bP1$r24*|\x1b\\"},
		{"DECSACE change-extent default", "*x", "\x1bP1$r0*x\x1b\\"},
		{"DECSASD active-status-display", "$}", "\x1bP1$r0$}\x1b\\"},
		{"DECSSDT status-display-type", "$~", "\x1bP1$r0$~\x1b\\"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(24, 80)
			s.Write([]byte("\x1bP$q" + tc.query + "\x1b\\"))
			if got := string(s.response); got != tc.want {
				t.Errorf("DECRQSS %q = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}
