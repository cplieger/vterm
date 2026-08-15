package terminal

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestWindowTitleReachesClient is a regression test: the flush builder
// computes and encodes the window-title payload (set via OSC 0/1/2), but the
// fan-out previously wrote only the modes/screen/scroll payloads and dropped
// the title, so window-title updates never reached the browser. A child that
// emits OSC 2 must now produce a title wire frame (wireMsgTitle) carrying the
// title.
func TestWindowTitleReachesClient(t *testing.T) {
	// /bin/sh emits the OSC 2 "set window title" sequence, then idles so the
	// connection stays open long enough for the 50ms flush ticker to fire.
	ws, cleanup := dialHandler(t, []string{"/bin/sh", "-c", `printf '\033]2;vterm-title-ok\007'; sleep 2`})
	defer cleanup()

	// Resize starts the child at a real size and latches flushing on.
	sendControl(t, ws, map[string]any{"type": "resize", "cols": 80, "rows": 24})

	if !readFrameOfType(t, ws, wireMsgTitle, []byte("vterm-title-ok"), 3*time.Second) {
		t.Fatal("no title wire frame carrying the OSC-set window title was received (flush fan-out dropped it)")
	}
}

// readFrameOfType reads WebSocket frames one at a time until a frame arrives
// whose first byte is msgType and whose bytes contain want, or the timeout
// fires. The server writes each payload as its own binary frame, so the
// leading byte of a frame is its wire message type.
func readFrameOfType(t *testing.T, ws *websocket.Conn, msgType byte, want []byte, timeout time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			return false
		}
		if len(msg) > 0 && msg[0] == msgType && bytes.Contains(msg, want) {
			return true
		}
	}
}
