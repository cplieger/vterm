package terminal

import (
	"testing"

	"github.com/coder/websocket"
	"github.com/cplieger/web-terminal-engine/v5/vt"
)

// TestED3ClearsScrollbackRing verifies that ED3 (CSI 3 J) emitted by the child
// clears the server's scrollback ring and raises the client-signal flag, while
// preserving committed so absolute indices stay monotonic. Inline TUIs
// (kiro-cli) emit ED3 on every resize redraw to discard the previous frame;
// without honoring it the old frames pile up in the ring and are replayed to
// clients, producing the resize-duplication bug.
func TestED3ClearsScrollbackRing(t *testing.T) {
	h := NewHandler([]string{"/bin/true"})

	// Populate the ring as if lines had scrolled into history.
	h.scrollback.Append([][]vt.WireRun{{{T: "old1"}}, {{T: "old2"}}})
	if h.scrollback.Len() == 0 {
		t.Fatal("setup: ring should be non-empty")
	}
	committedBefore := h.scrollback.Committed()

	// Child emits ED3 (erase scrollback). ED3 generates no reply, so
	// handlePTYData never touches the (unstarted) PTY.
	h.handlePTYData([]byte("\x1b[3J"))

	if h.scrollback.Len() != 0 {
		t.Fatalf("ED3 must clear the scrollback ring; Len = %d", h.scrollback.Len())
	}
	if !h.scrollbackClearedPending {
		t.Fatal("ED3 must set scrollbackClearedPending so the next frame signals the client")
	}
	if h.scrollback.Committed() != committedBefore {
		t.Fatalf("ED3 must preserve committed (monotonic indices); got %d want %d",
			h.scrollback.Committed(), committedBefore)
	}
}

// TestED3_nextFrameSignalsClientToDropHistory verifies the observable half of
// the ED3 contract: after the child erases scrollback, the very next outbound
// frame must carry the scrollbackCleared signal so the client drops its own
// history (indices below base). TestED3ClearsScrollbackRing only checks the
// internal scrollbackClearedPending flag; this drives the real
// handlePTYData -> buildFrame path and asserts the flag rides a frame and is
// then consumed. A mutant that clears the ring but never propagates the signal
// to a frame (leaving stale client history on screen) is caught here.
func TestED3_nextFrameSignalsClientToDropHistory(t *testing.T) {
	h := NewHandler([]string{"/bin/true"}, WithLogger(nil))
	// Latch flushing on without spawning a PTY (buildFrame returns nil until a
	// resize sets resized; we set it directly to keep the test process-free).
	h.sizeEstablished = true
	h.registry.Add(&websocket.Conn{}) // attached client: the render path, not zero-client suspension

	// Visible content so the repaint frame is non-empty, then ED3 to erase
	// scrollback. Neither sequence produces a PTY reply, so the nil ptmx is
	// never touched.
	h.handlePTYData([]byte("visible content"))
	h.handlePTYData([]byte("\x1b[3J"))

	frame, _ := h.buildFrame()
	if frame == nil {
		t.Fatal("buildFrame returned nil; expected a repaint frame carrying the scrollback-cleared signal")
	}
	if !frame.scrollbackCleared {
		t.Error("frame.scrollbackCleared=false after ED3; buildFrame must propagate the pending clear so the client drops history")
	}
	if h.scrollbackClearedPending {
		t.Error("buildFrame emitted the clear on a frame but did not consume scrollbackClearedPending; it would re-signal on every subsequent frame")
	}
}
