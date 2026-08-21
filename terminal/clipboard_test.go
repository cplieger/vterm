package terminal

import (
	"encoding/binary"
	"testing"

	"github.com/coder/websocket"
)

// TestOSC52ClipboardFrame verifies the handler drains an OSC 52 clipboard copy
// from the VT into a clipboard wire frame, even when the copy arrives with no
// accompanying screen change (Build returns nil, buildFrame synthesizes a
// frame). OSC 52 SET generates no PTY reply, so handlePTYData never touches the
// (unstarted) PTY.
func TestOSC52ClipboardFrame(t *testing.T) {
	h := NewHandler([]string{"/bin/true"})

	h.registry.Add(&websocket.Conn{})                 // attached client: the render path, not zero-client suspension
	h.handlePTYData([]byte("\x1b]52;c;aGVsbG8=\x07")) // base64("hello")
	if got := string(h.pendingClipboard); got != "hello" {
		t.Fatalf("handlePTYData did not stage clipboard: got %q, want hello", got)
	}

	frame, _ := h.buildFrame()
	if frame == nil {
		t.Fatal("buildFrame returned nil; a clipboard event must synthesize a frame")
	}
	if frame.clipboardPayload == nil {
		t.Fatal("frame is missing clipboardPayload")
	}
	// Decode the payload: [1B type=6][8B ack][2B len][text].
	p := frame.clipboardPayload
	if p[0] != wireMsgClipboard {
		t.Errorf("payload opcode = %d, want %d", p[0], wireMsgClipboard)
	}
	textLen := binary.LittleEndian.Uint16(p[9:11])
	if got := string(p[11 : 11+textLen]); got != "hello" {
		t.Errorf("payload text = %q, want hello", got)
	}
	if h.pendingClipboard != nil {
		t.Error("buildFrame should have drained pendingClipboard")
	}
}

// TestOSC52ClipboardSurvivesLaterOutput pins that a staged copy is held until a
// frame carries it. Output keeps arriving between flushes, and each arrival
// re-reads the screen's clipboard slot — which is empty by then, because the copy
// was already taken. Letting that empty read overwrite the staged copy would drop
// the user's clipboard whenever the program printed anything after the OSC 52.
func TestOSC52ClipboardSurvivesLaterOutput(t *testing.T) {
	h := NewHandler([]string{"/bin/true"})

	h.registry.Add(&websocket.Conn{})
	h.handlePTYData([]byte("\x1b]52;c;aGVsbG8=\x07")) // base64("hello")
	h.handlePTYData([]byte("more output\r\n"))        // no copy in this chunk

	if got := string(h.pendingClipboard); got != "hello" {
		t.Fatalf("pendingClipboard = %q after later output, want hello", got)
	}
	frame, _ := h.buildFrame()
	if frame == nil || frame.clipboardPayload == nil {
		t.Fatal("no clipboard payload in the frame; the staged copy was lost")
	}
	p := frame.clipboardPayload
	textLen := binary.LittleEndian.Uint16(p[9:11])
	if got := string(p[11 : 11+textLen]); got != "hello" {
		t.Errorf("payload text = %q, want hello", got)
	}
}

// TestOSC4ForcesRepaintPending verifies an OSC 4 palette change raises the
// handler's repaint-pending flag, which buildFrame consumes (calling
// builder.Reset() so already-drawn cells re-resolve to the new palette).
func TestOSC4ForcesRepaintPending(t *testing.T) {
	h := NewHandler([]string{"/bin/true"})

	h.handlePTYData([]byte("\x1b]4;1;rgb:00/ff/00\x07"))
	if !h.paletteChangedPending {
		t.Fatal("OSC 4 must set paletteChangedPending")
	}

	h.registry.Add(&websocket.Conn{}) // attached client: the render path, not zero-client suspension
	_, _ = h.buildFrame()
	if h.paletteChangedPending {
		t.Error("buildFrame should have consumed paletteChangedPending")
	}
}
