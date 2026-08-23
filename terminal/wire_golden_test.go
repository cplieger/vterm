package terminal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/cplieger/web-terminal-engine/v5/vt"
)

// The golden frames are the cross-language wire contract. This Go encoder MUST
// produce exactly these bytes, and the TypeScript decoder MUST decode them
// (web/src/wire-golden.node.test.ts reads the same files). Keeping the Go encoder
// and the TS decoder in one repo means a wire change is one PR; these fixtures
// make a drift between the two halves a test failure rather than a runtime
// mis-decode.
//
// After an INTENTIONAL wire change, regenerate the fixtures and update the TS
// decoder + its assertions in the same PR:
//
//	UPDATE_GOLDEN=1 go test ./terminal/ -run TestWireGolden
func goldenFrames() map[string][]byte {
	row := func(text string) []vt.WireRun {
		return []vt.WireRun{{T: text, F: -1, B: -1, Uc: -1}}
	}
	screenRows := [][]vt.WireRun{row("ab"), {}, row("cd")}
	return map[string][]byte{
		// screen: base=100, height=3, cursor=(1,2), changed rows 0 and 2,
		// cursorStyle=2, blink=true.
		"screen": encodeScreenMsg(100, 3, 1, 2, 0, []int{0, 2}, screenRows, 2, false, true, false, false, false),
		// scroll: two history lines starting at absolute index 50.
		"scroll": encodeScrollMsg(0, 50, [][]vt.WireRun{row("h0"), row("h1")}),
		// resumeAck: ack=7, epoch, committed=200, oldest=10, ledger intact,
		// paging NOT declared; carries the third length-gated tail
		// (serverWireVersion + ackFlags).
		"resumeack": encodeResumeAck(7, 1234567890, 200, 10, false, false),
		// resumeAck with the ledger-lost flag set (ackFlags bit0).
		"resumeack-ledgerlost": encodeResumeAck(7, 1234567890, 200, 10, true, false),
		// resumeAck with the history-paging capability declared (ackFlags bit1),
		// the demand-paged-scrollback signal. Independent of bit0, so the pair
		// pins that a client reads the bits separately.
		"resumeack-historypaging": encodeResumeAck(7, 1234567890, 200, 10, false, true),
		// ackOnly: bare ack (type + inputAck, no body), ack=42.
		"ackonly": encodeAckOnly(42),
		// clipboard (opcode 6): OSC 52 text for the system clipboard. Fixture
		// added 2026-07 (judgement finding): its cross-language layout was
		// previously pinned only by two independently hand-written tests.
		"clipboard": encodeClipboardMsg([]byte("copy me")),
		// modes: bracketed paste + SGR mouse + reverse video on, mouseMode 1002,
		// kitty disambiguate flag (1).
		"modes": encodeModesMsg(true, false, true, false, false, true, false, 1002, 1),
		"title": encodeTitleMsg("hello world"),
		"pong":  encodePongMsg(),
	}
}

func TestWireGolden(t *testing.T) {
	dir := filepath.Join("..", "wire-golden")
	update := os.Getenv("UPDATE_GOLDEN") != ""
	if update {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
	}
	for name, got := range goldenFrames() {
		path := filepath.Join(dir, name+".bin")
		if update {
			if err := os.WriteFile(path, got, 0o600); err != nil {
				t.Fatalf("write golden %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read golden %s (run UPDATE_GOLDEN=1 go test to generate): %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: Go encoding drifted from the golden fixture (%d bytes now, %d in fixture). "+
				"If this wire change is intentional, regenerate with UPDATE_GOLDEN=1 and update the TS decoder + web/src/wire-golden.node.test.ts in the same change.",
				name, len(got), len(want))
		}
	}
}
