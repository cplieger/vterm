package terminal

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/cplieger/web-terminal-engine/v3/vt"
)

// dialHandler stands up the handler on an httptest server and opens
// a WebSocket client against it. Returns the open connection and a
// cleanup func. Uses /bin/cat so tests don't depend on dtach being
// installed in the workspace.
func dialHandler(t *testing.T, cmd []string) (*websocket.Conn, func()) {
	t.Helper()
	h := NewHandler(cmd, WithWorkDir("/"))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	// coder/websocket's Dial nils out resp.Body on success; its
	// godoc is explicit: "You never need to close resp.Body
	// yourself." The bodyclose linter is stdlib-oriented and
	// doesn't know about that contract.
	//
	//nolint:bodyclose // library contract: Body is nil on success
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	cancel()
	if err != nil {
		srv.Close()
		t.Fatalf("ws dial: %v", err)
	}
	cleanup := func() {
		_ = ws.Close(websocket.StatusNormalClosure, "")
		srv.Close()
	}
	return ws, cleanup
}

// readUntil reads WS frames until the accumulated bytes contain
// want, or the timeout fires. Returns the concatenated bytes seen so
// far on timeout to aid debugging.
func readUntil(t *testing.T, ws *websocket.Conn, want []byte, timeout time.Duration) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	var got bytes.Buffer
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (got so far: %q)", err, got.Bytes())
		}
		got.Write(msg)
		if bytes.Contains(got.Bytes(), want) {
			return got.Bytes()
		}
	}
}

// sendControl writes a 0x00-prefixed JSON control frame.
func sendControl(t *testing.T, ws *websocket.Conn, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	frame := make([]byte, len(body)+1)
	frame[0] = 0
	copy(frame[1:], body)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("control write: %v", err)
	}
}

// TestEchoThroughPTY: /bin/cat reflects input back over the PTY.
// When the PTY is in cooked mode (default), cat echoes every byte
// back once for terminal echo; we only need to confirm "hello"
// appears in the output stream to prove the bidirectional pipe is
// wired correctly.
func TestEchoThroughPTY(t *testing.T) {
	ws, cleanup := dialHandler(t, []string{"/bin/cat"})
	defer cleanup()

	sendControl(t, ws, map[string]any{
		"type": "resize", "cols": 100, "rows": 40,
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, []byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, ws, []byte("hello"), 2*time.Second)
}

// TestResizeControlIsAccepted: a well-formed resize frame must not
// close the WS. We send resize, then raw input, and confirm the
// pipe still works. We can't directly assert the child's window
// size without shelling out, but the happy-path ioctl is internally
// exercised.
func TestResizeControlIsAccepted(t *testing.T) {
	ws, cleanup := dialHandler(t, []string{"/bin/cat"})
	defer cleanup()

	sendControl(t, ws, map[string]any{
		"type": "resize", "cols": 100, "rows": 40,
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, []byte("after-resize\n")); err != nil {
		t.Fatalf("post-resize write: %v", err)
	}
	readUntil(t, ws, []byte("after-resize"), 2*time.Second)
}

// TestBadControlMessageIgnored: a malformed JSON control frame
// must not tear down the connection — we keep the pipe open so a
// buggy client can recover.
func TestBadControlMessageIgnored(t *testing.T) {
	ws, cleanup := dialHandler(t, []string{"/bin/cat"})
	defer cleanup()

	sendControl(t, ws, map[string]any{
		"type": "resize", "cols": 100, "rows": 40,
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	// 0x00 prefix + garbage JSON.
	if err := ws.Write(ctx, websocket.MessageBinary, []byte{0x00, '{', 'x'}); err != nil {
		t.Fatalf("bad control write: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageBinary, []byte("still-alive\n")); err != nil {
		t.Fatalf("post-bad write: %v", err)
	}
	readUntil(t, ws, []byte("still-alive"), 2*time.Second)
}

// TestEmptyCommandFails: starting the handler with no command is a
// misconfiguration; the first WS must close cleanly with
// InternalError rather than hang or panic.
func TestEmptyCommandFails(t *testing.T) {
	h := NewHandler(nil, WithWorkDir("/"))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	//nolint:bodyclose // library contract: Body is nil on success
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		// An upgrade failure is also acceptable (some WS stacks
		// surface the server-side error at dial time). The
		// important property is the test doesn't hang.
		return
	}
	defer ws.Close(websocket.StatusNormalClosure, "")
	// Read should return an error as the server closes.
	_, _, readErr := ws.Read(ctx)
	if readErr == nil {
		t.Fatalf("expected read error after handler rejects empty command")
	}
}

// mustJSON marshals v to JSON for use as a handleControl payload (the bytes
// after the 0x00 control prefix).
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("mustJSON: %v", err)
	}
	return b
}

// serverSideConn stands up a throwaway WebSocket server, dials it, and returns
// the SERVER-side *websocket.Conn so a test can hand a real, non-nil
// connection to handleControl/handleResume without running the full handleWS
// read loop. The server goroutine drains client reads so cleanup unblocks the
// httptest server.
func serverSideConn(t *testing.T) (*websocket.Conn, func()) {
	t.Helper()
	ch := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ch <- c
		for {
			if _, _, rerr := c.Read(r.Context()); rerr != nil {
				return
			}
		}
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	dctx, dcancel := context.WithTimeout(t.Context(), 5*time.Second)
	//nolint:bodyclose // coder/websocket Dial nils resp.Body on success
	client, _, err := websocket.Dial(dctx, wsURL, nil)
	dcancel()
	if err != nil {
		srv.Close()
		t.Fatalf("serverSideConn dial: %v", err)
	}
	var server *websocket.Conn
	select {
	case server = <-ch:
	case <-time.After(3 * time.Second):
		_ = client.Close(websocket.StatusNormalClosure, "")
		srv.Close()
		t.Fatalf("serverSideConn: server side never accepted")
	}
	cleanup := func() {
		_ = client.Close(websocket.StatusNormalClosure, "")
		_ = server.CloseNow()
		srv.Close()
	}
	return server, cleanup
}

// TestShutdown_callsCancel verifies Shutdown invokes the stored cancel func
// when the handler has been started.
func TestShutdown_callsCancel(t *testing.T) {
	h := NewHandler([]string{"/bin/sh"})
	called := false
	h.cancel = func() { called = true }
	h.started.Store(true) // Shutdown returns early unless started

	h.Shutdown()

	if !called {
		t.Errorf("Shutdown did not invoke h.cancel; want it called")
	}
}

// TestShutdown_closesPtmx verifies Shutdown closes the PTY file. The close is
// observed by a second Close returning an already-closed error.
func TestShutdown_closesPtmx(t *testing.T) {
	h := NewHandler([]string{"/bin/sh"})
	f, err := os.CreateTemp(t.TempDir(), "ptmx")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	h.ptmx = f
	h.cancel = func() {} // non-nil no-op so the cancel branch never panics
	h.started.Store(true)

	h.Shutdown()

	if err := h.ptmx.Close(); err == nil {
		t.Errorf("re-Close after Shutdown returned nil; Shutdown did not close ptmx")
	}
}

// TestEnsureStarted_colsNotDefaultedWhenPositive verifies a positive cols value
// below the default is kept (only cols < 1 falls back to defaultCols).
func TestEnsureStarted_colsNotDefaultedWhenPositive(t *testing.T) {
	h := NewHandler([]string{"/bin/sh", "-c", "true"}, WithWorkDir("/"))
	if err := h.ensureStarted(1, 24); err != nil {
		t.Fatalf("ensureStarted(1, 24): %v", err)
	}
	defer h.Shutdown()

	h.mu.Lock()
	w := h.screen.Width
	h.mu.Unlock()
	if w != 1 {
		t.Errorf("ensureStarted(cols=1): screen.Width = %d, want 1 (cols==1 must not default to %d)", w, defaultCols)
	}
}

// TestEnsureStarted_rowsNotDefaultedWhenPositive verifies a positive rows value
// below the default is kept (only rows < 1 falls back to defaultRows).
func TestEnsureStarted_rowsNotDefaultedWhenPositive(t *testing.T) {
	h := NewHandler([]string{"/bin/sh", "-c", "true"}, WithWorkDir("/"))
	if err := h.ensureStarted(24, 1); err != nil {
		t.Fatalf("ensureStarted(24, 1): %v", err)
	}
	defer h.Shutdown()

	h.mu.Lock()
	ht := h.screen.Height
	h.mu.Unlock()
	if ht != 1 {
		t.Errorf("ensureStarted(rows=1): screen.Height = %d, want 1 (rows==1 must not default to %d)", ht, defaultRows)
	}
}

// TestBuildFrame_appendsScrollLinesToRing verifies buildFrame appends drained
// scroll lines to the scrollback ring so they can be replayed to reconnecting
// clients.
func TestBuildFrame_appendsScrollLinesToRing(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()

	// Tiny screen + many newlines forces lines to scroll off into the drain.
	h.screen = vt.New(3, 20)
	for range 20 {
		if _, err := h.screen.Write([]byte("scroll line\r\n")); err != nil {
			t.Fatalf("screen write: %v", err)
		}
	}
	h.sizeEstablished = true
	h.registry.Add(&websocket.Conn{}) // attached client: the render path, not zero-client suspension

	frame, _ := h.buildFrame()
	if frame == nil {
		t.Fatalf("buildFrame returned nil; expected a frame with scroll lines")
	}
	if len(frame.scrollLines) == 0 {
		t.Fatalf("precondition failed: frame has no scroll lines to append")
	}
	if got := len(h.scrollback.Lines()); got == 0 {
		t.Errorf("buildFrame appended %d scrollback lines, want > 0", got)
	}
}

// TestHandleControl_resizeStartsProcess verifies a well-formed resize control
// message starts the child process and sizes the screen.
func TestHandleControl_resizeStartsProcess(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()

	payload := mustJSON(t, controlMsg{Type: ctlTypeResize, Cols: 100, Rows: 40})
	h.handleControl(nil, &clientState{}, payload, nil)

	if !h.started.Load() {
		t.Fatalf("handleControl(valid resize): process not started")
	}
	h.mu.Lock()
	w := h.screen.Width
	h.mu.Unlock()
	if w != 100 {
		t.Errorf("handleControl(resize cols=100): screen.Width = %d, want 100", w)
	}
}

// TestHandleControl_unknownTypeDoesNotStart verifies a control message whose
// type is neither resume nor resize is ignored and does not start the process.
func TestHandleControl_unknownTypeDoesNotStart(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()

	payload := mustJSON(t, controlMsg{Type: "bogus"})
	h.handleControl(nil, &clientState{}, payload, nil)

	if h.started.Load() {
		t.Errorf("handleControl(unknown type): process started; only a resize may start it")
	}
}

// TestHandleControl_resumeResolvesSession verifies a resume control message
// carrying a session id dispatches handleResume, which resolves and registers
// the session on the client state.
func TestHandleControl_resumeResolvesSession(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()
	ws, cleanup := serverSideConn(t)
	defer cleanup()

	state := &clientState{}
	payload := mustJSON(t, controlMsg{Type: ctlTypeResume, SessionID: "sid"})
	resumeServed := false
	h.handleControl(ws, state, payload, func() { resumeServed = true })

	if state.session.Load() == nil {
		t.Errorf("handleControl(resume): clientState.session is nil; resume must resolve a session")
	}
	if !resumeServed {
		t.Errorf("handleControl(resume): onResumeServed not invoked; the deferred exited-close would never release")
	}
	h.registry.mu.Lock()
	_, ok := h.registry.sessions["sid"]
	h.registry.mu.Unlock()
	if !ok {
		t.Errorf("handleControl(resume): registry has no session %q; resume was not dispatched", "sid")
	}
}

// TestHandleControl_pingElicitsPong verifies a client liveness ping draws a
// pong frame back. The client probes with a ping after a stretch of inbound
// silence; the returning pong (or any frame) lets it tell an idle-but-healthy
// socket from one iOS froze during sleep, so a quiet terminal is not
// reconnect-flapped (bug 2 defense-in-depth).
func TestHandleControl_pingElicitsPong(t *testing.T) {
	ws, cleanup := dialHandler(t, []string{"/bin/cat"})
	defer cleanup()

	// Deliberately send no resize: the child never starts, so no screen or
	// scroll frames race the pong. The only frame the server sends is the pong.
	sendControl(t, ws, map[string]any{"type": ctlTypePing})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("never received a pong after ping: %v", err)
		}
		if len(msg) > 0 && msg[0] == wireMsgPong {
			return // got the liveness reply
		}
	}
}

// TestHandleResize_colsAboveMinNotFloored verifies a cols value above
// minResizeCols is applied unchanged.
func TestHandleResize_colsAboveMinNotFloored(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()

	h.handleResize(&clientState{}, 100, 40)

	h.mu.Lock()
	w := h.screen.Width
	h.mu.Unlock()
	if w != 100 {
		t.Errorf("handleResize(cols=100): screen.Width = %d, want 100 (above minResizeCols, not floored)", w)
	}
}

// TestHandleResize_rowsAboveMinNotFloored verifies a rows value above
// minResizeRows is applied unchanged.
func TestHandleResize_rowsAboveMinNotFloored(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()

	h.handleResize(&clientState{}, 100, 40)

	h.mu.Lock()
	ht := h.screen.Height
	h.mu.Unlock()
	if ht != 40 {
		t.Errorf("handleResize(rows=40): screen.Height = %d, want 40 (above minResizeRows, not floored)", ht)
	}
}

// TestApplySize_sizeChangeArmsRedrawSettle verifies the redraw-settle hold
// arms exactly when the dimensions change: the first start seeds the screen
// size (ensureStarted), so the first handleResize is a same-size applySize and
// must NOT arm (no SIGWINCH fires for an unchanged winsize, so there is no
// redraw to hide and live output must not stall); a second resize to a
// different size must arm.
func TestApplySize_sizeChangeArmsRedrawSettle(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()

	h.handleResize(&clientState{}, 100, 40)
	h.mu.Lock()
	afterFirst := h.redrawHoldUntil(time.Now())
	h.mu.Unlock()
	if !afterFirst.IsZero() {
		t.Errorf("first resize (size seeded by ensureStarted): redrawHoldUntil = %v, want zero (same-size applySize must not arm)", afterFirst)
	}

	h.handleResize(&clientState{}, 90, 30)
	h.mu.Lock()
	afterChange := h.redrawHoldUntil(time.Now())
	h.mu.Unlock()
	if afterChange.IsZero() {
		t.Error("size-changing resize: redrawHoldUntil = zero, want a future deadline (the SIGWINCH redraw window must be held)")
	}

	// Re-applying the SAME size while armed must not matter either way, and a
	// later same-size resize after settle must not re-arm.
	h.mu.Lock()
	h.redrawSettleUntil = time.Time{} // settle
	h.mu.Unlock()
	h.handleResize(&clientState{}, 90, 30)
	h.mu.Lock()
	afterSame := h.redrawHoldUntil(time.Now())
	h.mu.Unlock()
	if !afterSame.IsZero() {
		t.Errorf("same-size resize after settle: redrawHoldUntil = %v, want zero", afterSame)
	}
}

// TestRedrawSettle_ESUDoesNotRelease pins the regression that motivated the
// settle hold: kiro-cli brackets its post-resize transcript reprint in many
// small DEC 2026 BSU/ESU chunks, and the first chunk's ESU used to clear the
// screen-level resize hold (vt.Screen.ReleaseFlush zeroes FlushHoldUntil), so
// every flush pass between brackets streamed a mid-reprint window — visible
// history churn on every phone keyboard/rotation resize. The handler-level
// hold must survive the ESU.
func TestRedrawSettle_ESUDoesNotRelease(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()

	h.handleResize(&clientState{}, 100, 40)
	h.handleResize(&clientState{}, 90, 30) // size change: arms the settle hold

	// Child starts its redraw: one small synchronized bracket, then more
	// output between brackets.
	h.handlePTYData([]byte("\x1b[?2026h" + "chunk 1 of transcript\r\n" + "\x1b[?2026l"))

	h.mu.Lock()
	screenHeld := h.screen.IsFlushHeld()
	redrawHold := h.redrawHoldUntil(time.Now())
	h.mu.Unlock()
	if screenHeld {
		t.Error("screen-level hold survived ESU; expected ReleaseFlush to clear it (2026 semantics)")
	}
	if redrawHold.IsZero() {
		t.Fatal("redraw-settle hold released by the child's ESU; mid-reprint frames would stream to clients again")
	}
}

// TestRedrawSettle_releasesOnQuietAndExtendsOnOutput verifies the two release
// dynamics: continued PTY output extends the quiet deadline, and quiet for
// redrawSettleQuiet releases (and disarms) the hold.
func TestRedrawSettle_releasesOnQuietAndExtendsOnOutput(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()

	h.handleResize(&clientState{}, 100, 40)
	h.handleResize(&clientState{}, 90, 30)

	h.handlePTYData([]byte("redraw output"))
	h.mu.Lock()
	d1 := h.redrawHoldUntil(time.Now())
	h.mu.Unlock()
	if d1.IsZero() {
		t.Fatal("hold must be active right after redraw output")
	}

	time.Sleep(redrawSettleQuiet / 2)
	h.handlePTYData([]byte("more redraw output"))
	h.mu.Lock()
	d2 := h.redrawHoldUntil(time.Now())
	h.mu.Unlock()
	if !d2.After(d1) {
		t.Errorf("continued output must extend the quiet deadline: first %v, after more output %v", d1, d2)
	}

	time.Sleep(redrawSettleQuiet + 30*time.Millisecond)
	h.mu.Lock()
	d3 := h.redrawHoldUntil(time.Now())
	armed := !h.redrawSettleUntil.IsZero()
	h.mu.Unlock()
	if !d3.IsZero() {
		t.Errorf("quiet for > redrawSettleQuiet must release the hold; got deadline %v", d3)
	}
	if armed {
		t.Error("a lapsed hold must disarm (redrawSettleUntil cleared), not report zero while staying armed")
	}
}

// TestRedrawSettle_capBoundsContinuousOutput verifies the hard ceiling: output
// that never goes quiet (a resize landing mid-stream) is held at most until
// the cap, never until the stream pauses.
func TestRedrawSettle_capBoundsContinuousOutput(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()

	now := time.Now()
	h.mu.Lock()
	h.armRedrawSettle(now)
	// Simulate a stream still writing at the cap: last data is fresh, but the
	// cap deadline has passed.
	h.redrawSettleUntil = now.Add(-time.Millisecond)
	h.redrawLastData = now
	d := h.redrawHoldUntil(now)
	h.mu.Unlock()
	if !d.IsZero() {
		t.Errorf("cap passed with fresh output: redrawHoldUntil = %v, want zero (the cap must win over the quiet window)", d)
	}

	// And below the cap, the earlier of (quiet deadline, cap) governs.
	h.mu.Lock()
	h.armRedrawSettle(now)
	h.redrawSettleUntil = now.Add(10 * time.Millisecond) // cap sooner than quiet
	h.redrawLastData = now
	d = h.redrawHoldUntil(now)
	h.mu.Unlock()
	if want := now.Add(10 * time.Millisecond); !d.Equal(want) {
		t.Errorf("cap sooner than quiet: redrawHoldUntil = %v, want the cap %v", d, want)
	}
}

// TestRedrawSettle_buildFrameHeldThenSettles drives the observable contract
// end to end on the real handlePTYData -> buildFrame path: while the settle
// hold is active buildFrame emits nothing and returns the retry deadline; once
// the redraw goes quiet the next pass emits one frame carrying the settled
// window and, when the redraw began with ED3 (kiro-cli's reprint signature),
// the scrollbackCleared signal — the client swaps old content for new in one
// atomic repaint instead of watching the reprint stream through.
func TestRedrawSettle_buildFrameHeldThenSettles(t *testing.T) {
	h := NewHandler([]string{"/bin/true"}, WithLogger(nil))
	h.sizeEstablished = true
	h.registry.Add(&websocket.Conn{}) // attached client: the render path, not zero-client suspension

	h.mu.Lock()
	h.armRedrawSettle(time.Now())
	h.mu.Unlock()

	// The redraw: ED3 (discard scrollback) then reprinted content.
	h.handlePTYData([]byte("\x1b[3J"))
	h.handlePTYData([]byte("reprinted transcript line"))

	frame, holdUntil := h.buildFrame()
	if frame != nil {
		t.Fatalf("buildFrame emitted a mid-redraw frame while the settle hold is active (changed=%d)", len(frame.changed))
	}
	if holdUntil.IsZero() || !holdUntil.After(time.Now()) {
		t.Fatalf("held pass must return a future retry deadline; got %v", holdUntil)
	}

	time.Sleep(redrawSettleQuiet + 30*time.Millisecond)
	frame, holdUntil = h.buildFrame()
	if !holdUntil.IsZero() {
		t.Fatalf("hold must have lapsed after quiet; got retry deadline %v", holdUntil)
	}
	if frame == nil {
		t.Fatal("settled pass must emit the accumulated redraw as one frame")
	}
	if !frame.scrollbackCleared {
		t.Error("the settled frame must carry the pending ED3 scrollbackCleared signal (one atomic old-for-new swap on the client)")
	}
}

// TestRedrawSettle_capLapseCoalescesThenDisarms pins the lapse ladder, which is
// what stops a redraw LONGER than the cap from streaming its remainder. A cap
// lapse while the child is still writing must not fall back to per-flush
// streaming (the churn the hold exists to prevent); it must let one FULL repaint
// through and re-arm, a bounded number of times, and only then give up so a
// genuinely streaming program is not throttled forever.
//
// Driven through redrawHoldUntil's explicit `now` rather than sleeps: the cap is
// a whole second, and a real-time version of this test would be both slow and
// flaky under load.
func TestRedrawSettle_capLapseCoalescesThenDisarms(t *testing.T) {
	h := NewHandler([]string{"/bin/true"}, WithLogger(nil))
	defer h.Shutdown()

	t0 := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.armRedrawSettle(t0)

	// Each lapse: the cap deadline has passed but the child wrote just now, so
	// it is still redrawing.
	for i := 1; i <= redrawSettleMaxRearms; i++ {
		lapse := h.redrawSettleUntil.Add(time.Millisecond)
		h.redrawLastData = lapse // still writing
		d := h.redrawHoldUntil(lapse)
		if !d.IsZero() {
			t.Fatalf("lapse %d: redrawHoldUntil = %v, want zero so this pass can flush", i, d)
		}
		if !h.redrawCoalesceNow {
			t.Fatalf("lapse %d: redrawCoalesceNow = false, want true so the pass builds a FULL repaint rather than a mid-redraw diff", i)
		}
		if h.redrawSettleUntil.IsZero() {
			t.Fatalf("lapse %d: hold disarmed, want a re-arm (budget %d of %d used)", i, i, redrawSettleMaxRearms)
		}
		if got, want := h.redrawSettleUntil, lapse.Add(redrawSettleCap); !got.Equal(want) {
			t.Errorf("lapse %d: re-armed until %v, want %v", i, got, want)
		}
		if h.redrawRearms != i {
			t.Errorf("lapse %d: redrawRearms = %d, want %d", i, h.redrawRearms, i)
		}
		h.redrawCoalesceNow = false // buildFrame consumes it on the pass that builds
	}

	// Budget spent: a program still writing at this point is streaming, not
	// redrawing, so the hold must release for good.
	lapse := h.redrawSettleUntil.Add(time.Millisecond)
	h.redrawLastData = lapse
	if d := h.redrawHoldUntil(lapse); !d.IsZero() {
		t.Errorf("budget spent: redrawHoldUntil = %v, want zero", d)
	}
	if !h.redrawSettleUntil.IsZero() {
		t.Error("budget spent: hold must disarm for good, not re-arm again")
	}
	if h.redrawCoalesceNow {
		t.Error("budget spent: must not request another full repaint; normal streaming resumes")
	}
}

// TestRedrawSettle_quietReleaseDoesNotCoalesce keeps the two exits distinct: a
// redraw that ended (the child went quiet) needs no forced full repaint, and
// requesting one would cost every settled resize an extra whole-screen frame.
func TestRedrawSettle_quietReleaseDoesNotCoalesce(t *testing.T) {
	h := NewHandler([]string{"/bin/true"}, WithLogger(nil))
	defer h.Shutdown()

	t0 := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.armRedrawSettle(t0)
	quiet := t0.Add(redrawSettleQuiet + time.Millisecond)
	if d := h.redrawHoldUntil(quiet); !d.IsZero() {
		t.Fatalf("quiet for > redrawSettleQuiet must release; got %v", d)
	}
	if !h.redrawSettleUntil.IsZero() {
		t.Error("a settled hold must disarm")
	}
	if h.redrawCoalesceNow {
		t.Error("a settled release must not request a full repaint")
	}
}

// TestRedrawSettle_freshArmRestoresBudget verifies the budget is per-redraw, not
// per-session: a later geometry change gets the full coalescing allowance even
// if an earlier one spent it.
func TestRedrawSettle_freshArmRestoresBudget(t *testing.T) {
	h := NewHandler([]string{"/bin/true"}, WithLogger(nil))
	defer h.Shutdown()

	h.mu.Lock()
	defer h.mu.Unlock()
	h.redrawRearms = redrawSettleMaxRearms
	h.redrawCoalesceNow = true
	h.armRedrawSettle(time.Now())
	if h.redrawRearms != 0 {
		t.Errorf("armRedrawSettle: redrawRearms = %d, want 0 (a new redraw gets the full budget)", h.redrawRearms)
	}
	if h.redrawCoalesceNow {
		t.Error("armRedrawSettle must clear a stale coalesce request")
	}
}

// TestBuildFrame_coalescedLapseEmitsFullRepaint is the observable half of the
// ladder: the pass following a coalescing lapse must emit a frame describing the
// WHOLE screen, not a diff against the client's cache. A diff at that instant
// describes a screen caught mid-redraw, which is exactly the partial state the
// hold exists to hide.
func TestBuildFrame_coalescedLapseEmitsFullRepaint(t *testing.T) {
	h := NewHandler([]string{"/bin/true"}, WithLogger(nil))
	h.sizeEstablished = true
	h.registry.Add(&websocket.Conn{})

	// Establish a client-side cache so a diff would be SMALL, making the
	// full-repaint assertion meaningful rather than vacuous.
	h.handlePTYData([]byte("first line\r\n"))
	first, _ := h.buildFrame()
	if first == nil {
		t.Fatal("setup: expected an initial frame to prime the builder's diff cache")
	}

	// A geometry change arms the hold; the child starts redrawing and does not
	// stop, so the cap lapses.
	h.mu.Lock()
	h.armRedrawSettle(time.Now())
	h.mu.Unlock()
	h.handlePTYData([]byte("second line\r\n"))
	h.mu.Lock()
	h.redrawSettleUntil = time.Now().Add(-time.Millisecond) // cap lapsed
	h.redrawLastData = time.Now()                           // still writing
	h.mu.Unlock()

	frame, holdUntil := h.buildFrame()
	if !holdUntil.IsZero() {
		t.Fatalf("a coalescing lapse must let the pass flush; got retry deadline %v", holdUntil)
	}
	if frame == nil {
		t.Fatal("a coalescing lapse must emit the coalesced update")
	}
	if len(frame.changed) != len(frame.rows) {
		t.Errorf("coalesced frame carries %d changed rows of %d: want every row (a full repaint, not a mid-redraw diff)",
			len(frame.changed), len(frame.rows))
	}
	h.mu.Lock()
	coalesceStillPending := h.redrawCoalesceNow
	h.mu.Unlock()
	if coalesceStillPending {
		t.Error("buildFrame must consume the coalesce request once it has built the repaint")
	}
}

// dualConn stands up a throwaway WebSocket server, dials it, and returns BOTH
// the SERVER-side conn (to hand to dispatchFrame/handleResume) and the
// CLIENT-side conn (to read back the server→client frames a test wants to
// assert on). The server goroutine drains client→server reads so the
// connection stays healthy; nothing is sent that way in these tests.
func dualConn(t *testing.T) (server, client *websocket.Conn, cleanup func()) {
	t.Helper()
	ch := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ch <- c
		for {
			if _, _, rerr := c.Read(r.Context()); rerr != nil {
				return
			}
		}
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	dctx, dcancel := context.WithTimeout(t.Context(), 5*time.Second)
	//nolint:bodyclose // coder/websocket Dial nils resp.Body on success
	client, _, err := websocket.Dial(dctx, wsURL, nil)
	dcancel()
	if err != nil {
		srv.Close()
		t.Fatalf("dualConn dial: %v", err)
	}
	select {
	case server = <-ch:
	case <-time.After(3 * time.Second):
		_ = client.Close(websocket.StatusNormalClosure, "")
		srv.Close()
		t.Fatalf("dualConn: server side never accepted")
	}
	cleanup = func() {
		_ = client.Close(websocket.StatusNormalClosure, "")
		_ = server.CloseNow()
		srv.Close()
	}
	return server, client, cleanup
}

// readServerFrames reads every binary frame the server sent until no new frame
// arrives within idle. Frames are already buffered by the time a synchronous
// dispatchFrame/handleResume call returns, so only the final (no-more-frames)
// read waits out idle.
func readServerFrames(t *testing.T, client *websocket.Conn, idle time.Duration) [][]byte {
	t.Helper()
	var frames [][]byte
	for {
		ctx, cancel := context.WithTimeout(t.Context(), idle)
		_, msg, err := client.Read(ctx)
		cancel()
		if err != nil {
			return frames
		}
		cp := make([]byte, len(msg))
		copy(cp, msg)
		frames = append(frames, cp)
	}
}

// countFramesByType tallies frames by their leading msg_type byte, ignoring
// any zero-length frame.
func countFramesByType(frames [][]byte) map[byte]int {
	m := make(map[byte]int)
	for _, f := range frames {
		if len(f) > 0 {
			m[f[0]]++
		}
	}
	return m
}

// TestDispatchFrame_scrollOnlyEmitsScrollNotScreen verifies a frame carrying
// scroll lines but no changed rows is sent as exactly one scroll frame and no
// screen frame: the screen payload must be gated on len(changed) > 0 and the
// scroll payload must be both built (len(scrollLines) > 0) and written.
func TestDispatchFrame_scrollOnlyEmitsScrollNotScreen(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	server, client, cleanup := dualConn(t)
	defer cleanup()

	frame := &flushFrame{
		clients:        map[*websocket.Conn]uint64{server: 0},
		scrollLines:    [][]vt.WireRun{makeLine("scrolled")},
		scrollFirstIdx: 0,
		screenHeight:   3,
	}
	h.dispatchFrame(frame)

	types := countFramesByType(readServerFrames(t, client, 300*time.Millisecond))
	if types[wireMsgScreen] != 0 {
		t.Errorf("scroll-only frame emitted %d screen frame(s); want 0 (no changed rows ⇒ no screen payload)", types[wireMsgScreen])
	}
	if types[wireMsgScroll] != 1 {
		t.Errorf("scroll-only frame emitted %d scroll frame(s); want 1", types[wireMsgScroll])
	}
}

// TestDispatchFrame_changedOnlyEmitsScreenNotScroll verifies a frame carrying
// changed rows but no scroll lines is sent as exactly one screen frame and no
// scroll frame: the scroll payload must be gated on len(scrollLines) > 0.
func TestDispatchFrame_changedOnlyEmitsScreenNotScroll(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	server, client, cleanup := dualConn(t)
	defer cleanup()

	frame := &flushFrame{
		clients:      map[*websocket.Conn]uint64{server: 0},
		rows:         [][]vt.WireRun{makeLine("row0")},
		changed:      []int{0},
		screenHeight: 1,
	}
	h.dispatchFrame(frame)

	types := countFramesByType(readServerFrames(t, client, 300*time.Millisecond))
	if types[wireMsgScroll] != 0 {
		t.Errorf("changed-only frame emitted %d scroll frame(s); want 0 (no scroll lines ⇒ no scroll payload)", types[wireMsgScroll])
	}
	if types[wireMsgScreen] != 1 {
		t.Errorf("changed-only frame emitted %d screen frame(s); want 1", types[wireMsgScreen])
	}
}

// TestDispatchFrame_modesPayloadIsWritten verifies a non-nil modes payload is
// actually written to the client (the nil-guard must send when the payload is
// present).
func TestDispatchFrame_modesPayloadIsWritten(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	server, client, cleanup := dualConn(t)
	defer cleanup()

	frame := &flushFrame{
		clients:      map[*websocket.Conn]uint64{server: 0},
		modesPayload: encodeModesMsg(true, false, false, false, false, false, false, 0, 0),
		screenHeight: 1,
	}
	h.dispatchFrame(frame)

	types := countFramesByType(readServerFrames(t, client, 300*time.Millisecond))
	if types[wireMsgModes] != 1 {
		t.Errorf("frame with a modes payload emitted %d modes frame(s); want 1", types[wireMsgModes])
	}
}

// TestDispatchFrame_largeScrollBurstSplitsIntoChunks pins the chunking loop in
// dispatchFrame: a drained burst larger than maxScrollLinesPerFrame is split into
// several scroll frames, each tagged with its own absolute first index
// (scrollFirstIdx + i). The other TestDispatchFrame_* cases pass a single scroll
// line, so the loop only ever iterated once and the split plus the per-chunk index
// offset were unverified. A mutant collapsing the split into one frame, or dropping
// the +i so every chunk reuses scrollFirstIdx, would survive.
func TestDispatchFrame_largeScrollBurstSplitsIntoChunks(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	server, client, cleanup := dualConn(t)
	defer cleanup()

	const firstIdx uint64 = 7
	lines := make([][]vt.WireRun, maxScrollLinesPerFrame+500)
	for i := range lines {
		lines[i] = makeLine("x")
	}
	frame := &flushFrame{
		clients:        map[*websocket.Conn]uint64{server: 0},
		scrollLines:    lines,
		scrollFirstIdx: firstIdx,
		screenHeight:   3,
	}
	h.dispatchFrame(frame)

	var firstIdxs []uint64
	var lineCounts []uint16
	for _, f := range readServerFrames(t, client, 300*time.Millisecond) {
		if len(f) >= 19 && f[0] == wireMsgScroll {
			firstIdxs = append(firstIdxs, binary.LittleEndian.Uint64(f[9:17]))
			lineCounts = append(lineCounts, binary.LittleEndian.Uint16(f[17:19]))
		}
	}
	want := []uint64{firstIdx, firstIdx + maxScrollLinesPerFrame}
	if len(firstIdxs) != len(want) {
		t.Fatalf("burst of %d lines emitted %d scroll frame(s) (first indices %v); want %d at %v (split at maxScrollLinesPerFrame=%d)",
			len(lines), len(firstIdxs), firstIdxs, len(want), want, maxScrollLinesPerFrame)
	}
	for i, w := range want {
		if firstIdxs[i] != w {
			t.Errorf("scroll chunk %d first index = %d, want %d (chunk i must start at scrollFirstIdx+i)", i, firstIdxs[i], w)
		}
	}
	// Each chunk must be bounded by maxScrollLinesPerFrame and the chunks must
	// together carry every input line exactly once: a mutant that stops bounding
	// the slice end (shipping all lines in the first chunk and duplicating the
	// tail) keeps the frame count and first indices but breaks these counts.
	if lineCounts[0] != maxScrollLinesPerFrame || lineCounts[1] != uint16(len(lines)-maxScrollLinesPerFrame) {
		t.Errorf("scroll chunk line counts = %v, want [%d %d] (each chunk bounded by maxScrollLinesPerFrame; no dropped or duplicated lines)",
			lineCounts, maxScrollLinesPerFrame, len(lines)-maxScrollLinesPerFrame)
	}
}

// TestHandleResume_commitsScrolledLinesToHistory verifies handleResume commits
// the lines that scrolled off the screen while the client was away into the
// scrollback ring, so they can be replayed. With the commit skipped, the ring
// stays empty and that history is lost.
func TestHandleResume_commitsScrolledLinesToHistory(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	server, cleanup := serverSideConn(t)
	defer cleanup()

	// Tiny screen + many newlines pushes lines off the top into the pending
	// drain (not yet committed to the ring).
	h.screen = vt.New(3, 20)
	for range 20 {
		if _, err := h.screen.Write([]byte("scrolled\r\n")); err != nil {
			t.Fatalf("screen write: %v", err)
		}
	}

	if before := h.scrollback.Committed(); before != 0 {
		t.Fatalf("precondition: committed=%d, want 0 before resume", before)
	}
	h.handleResume(server, &clientState{}, "sid", -1, 0, nil)

	if got := h.scrollback.Committed(); got == 0 {
		t.Errorf("handleResume committed %d lines to history; want > 0 (scrolled lines must be retained)", got)
	}
}

// TestHandleResume_altStraddleDrainNotCommitted pins the alt-straddle guard in
// handleResume (the !altTransitionPending term, the resume-side twin of Build's
// guard): drain that straddles an alt-screen transition must NOT be committed to
// main history on resume. TestHandleResume_commitsScrolledLinesToHistory covers
// only the no-transition path (fresh builder => altTransitionPending false =>
// commit). The two cases below share identical pending drain on the main screen
// and differ ONLY in the builder's last-observed alt state, so the flipped commit
// outcome proves the guard (not an empty drain) does the gating; a mutant dropping
// the !altTransitionPending term commits alt lines as main history and is caught.
func TestHandleResume_altStraddleDrainNotCommitted(t *testing.T) {
	tests := []struct {
		name          string
		prevAltValid  bool
		prevAlt       bool
		wantCommitted bool
	}{
		{"transition pending (just left alt) drops straddling drain", true, true, false},
		{"no pending transition commits drain", false, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
			server, cleanup := serverSideConn(t)
			defer cleanup()

			// Tiny screen + many newlines leaves lines in the pending (uncommitted)
			// drain; the screen stays on the MAIN buffer (InAltScreen == false).
			h.screen = vt.New(3, 20)
			for range 20 {
				if _, err := h.screen.Write([]byte("scrolled\r\n")); err != nil {
					t.Fatalf("screen write: %v", err)
				}
			}
			// With prevAlt=true and the screen now on main, altTransitionPending
			// reports a not-yet-folded alt->main exit (Reset does not clear it).
			h.builder.prevAltValid = tc.prevAltValid
			h.builder.prevAlt = tc.prevAlt

			if before := h.scrollback.Committed(); before != 0 {
				t.Fatalf("precondition: committed=%d, want 0 before resume", before)
			}
			h.handleResume(server, &clientState{}, "sid", -1, 0, nil)

			if gotCommitted := h.scrollback.Committed() > 0; gotCommitted != tc.wantCommitted {
				t.Errorf("after resume: committed-history-present=%v, want %v (alt-straddle drain must be dropped; plain main-screen drain must commit)",
					gotCommitted, tc.wantCommitted)
			}
		})
	}
}

// TestHandleResume_replayStartsAfterHaveThrough verifies the server replays
// only the lines the client is missing: with haveThrough=0 the first replayed
// line is absolute index 1, not 0. Pins from = haveThrough + 1 (so flipping the
// +, or the haveThrough >= 0 guard, is caught — both would replay from 0 or
// replay nothing).
func TestHandleResume_replayStartsAfterHaveThrough(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	server, client, cleanup := dualConn(t)
	defer cleanup()

	h.scrollback.Append([][]vt.WireRun{makeLine("L0"), makeLine("L1"), makeLine("L2")})

	h.handleResume(server, &clientState{}, "sid", 0, 0, nil) // client already holds index 0

	frames := readServerFrames(t, client, 300*time.Millisecond)
	var scroll []byte
	for _, f := range frames {
		if len(f) > 0 && f[0] == wireMsgScroll {
			scroll = f
			break
		}
	}
	if scroll == nil {
		t.Fatalf("handleResume(haveThrough=0): no replay scroll frame; want one starting at index 1")
	}
	if len(scroll) < 17 {
		t.Fatalf("scroll frame too short (%d bytes) to carry a first-index field", len(scroll))
	}
	if firstIdx := binary.LittleEndian.Uint64(scroll[9:17]); firstIdx != 1 {
		t.Errorf("replay first index = %d, want 1 (client holds index 0, replay starts at haveThrough+1)", firstIdx)
	}
}

// TestHandleResume_replayChunkBoundaryEmitsSingleFrame verifies that when the
// replay length is an exact multiple of the chunk size, the server sends one
// scroll frame and no trailing empty frame. With the loop bound off by one, an
// extra zero-line frame is emitted.
func TestHandleResume_replayChunkBoundaryEmitsSingleFrame(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	server, client, cleanup := dualConn(t)
	defer cleanup()

	// 50 lines == replayChunk, so the chunk loop lands exactly on len(replay).
	lines := make([][]vt.WireRun, 50)
	for i := range lines {
		lines[i] = makeLine("x")
	}
	h.scrollback.Append(lines)

	h.handleResume(server, &clientState{}, "sid", -1, 0, nil) // -1 ⇒ replay everything from index 0

	types := countFramesByType(readServerFrames(t, client, 300*time.Millisecond))
	if types[wireMsgScroll] != 1 {
		t.Errorf("replay of exactly replayChunk lines emitted %d scroll frame(s); want 1 (no trailing empty frame)", types[wireMsgScroll])
	}
}

// TestHandleResume_replayChunksCarryAscendingAbsoluteIndices verifies a replay
// longer than one chunk tags each chunk with the correct absolute first index
// (firstAbs + i), so the client stores every line at its true index. With the
// offset computed the wrong way, later chunks carry a bogus index.
func TestHandleResume_replayChunksCarryAscendingAbsoluteIndices(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	server, client, cleanup := dualConn(t)
	defer cleanup()

	// 120 lines ⇒ three chunks of 50/50/20 at absolute indices 0, 50, 100.
	lines := make([][]vt.WireRun, 120)
	for i := range lines {
		lines[i] = makeLine("x")
	}
	h.scrollback.Append(lines)

	h.handleResume(server, &clientState{}, "sid", -1, 0, nil) // replay all from index 0

	var firstIdxs []uint64
	for _, f := range readServerFrames(t, client, 300*time.Millisecond) {
		if len(f) >= 17 && f[0] == wireMsgScroll {
			firstIdxs = append(firstIdxs, binary.LittleEndian.Uint64(f[9:17]))
		}
	}
	want := []uint64{0, 50, 100}
	if len(firstIdxs) != len(want) {
		t.Fatalf("replay sent %d scroll frames (first indices %v); want %d at %v", len(firstIdxs), firstIdxs, len(want), want)
	}
	for i, w := range want {
		if firstIdxs[i] != w {
			t.Errorf("scroll frame %d first index = %d, want %d (chunk %d starts at absolute index %d)", i, firstIdxs[i], w, i, w)
		}
	}
}

// TestHandleResize_belowMinimumIsFlooredUp pins the documented iPad
// keyboard-slide behavior: a near-zero resize is floored UP to the minimum
// (not dropped) and still starts the child. Existing resize tests only pass
// dimensions above the minimum, so the `cols < minResizeCols` /
// `rows < minResizeRows` floor branches were unexercised.
func TestHandleResize_belowMinimumIsFlooredUp(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()

	h.handleResize(&clientState{}, 1, 1)

	if !h.started.Load() {
		t.Fatalf("handleResize(1,1): process not started; a floored resize must still start the child")
	}
	h.mu.Lock()
	w, ht := h.screen.Width, h.screen.Height
	h.mu.Unlock()
	if w != minResizeCols {
		t.Errorf("handleResize(cols=1): screen.Width = %d, want %d (floored up to minResizeCols)", w, minResizeCols)
	}
	if ht != minResizeRows {
		t.Errorf("handleResize(rows=1): screen.Height = %d, want %d (floored up to minResizeRows)", ht, minResizeRows)
	}
}

// TestEnsureStarted_reaperInvokesOnProcessExitWithStatus exercises the
// process-reaper goroutine added in cycle 1 (the `go func(){ cmd.Wait();
// onProcessExit(werr); cancel() }` in ensureStarted). TestNewHandler_WithOnProcessExit
// only calls the stored callback directly and never spawns a child, so the real
// reaper path -- reap the child, forward cmd.Wait's exit status to onProcessExit --
// was unasserted. Here a real child exits non-zero and the reaper must invoke
// onProcessExit with the *exec.ExitError carrying the status. A mutant that drops
// the callback invocation, or passes nil instead of cmd.Wait's result, is caught.
func TestEnsureStarted_reaperInvokesOnProcessExitWithStatus(t *testing.T) {
	exitErr := make(chan error, 1)
	h := NewHandler([]string{"/bin/sh", "-c", "exit 7"},
		WithWorkDir("/"),
		WithLogger(nil),
		WithOnProcessExit(func(err error) { exitErr <- err }),
	)
	defer h.Shutdown()

	if err := h.ensureStarted(80, 24); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}

	select {
	case err := <-exitErr:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("onProcessExit err = %v (%T), want *exec.ExitError from cmd.Wait", err, err)
		}
		if ee.ExitCode() != 7 {
			t.Errorf("child exit code = %d, want 7 (reaper must forward cmd.Wait's status)", ee.ExitCode())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("onProcessExit not called within 10s; the reaper goroutine must invoke it on child exit")
	}
}

// TestHandlePTYData_writesDeviceQueryResponseToPTY pins the writeback half of
// the handlePTYData refactor: a device-status query in the PTY output makes the
// VT screen produce a Response, which handlePTYData must write back to the PTY
// (outside h.mu). CSI 6 n (DSR cursor-position) on a fresh screen with the cursor
// at home yields ESC[1;1R. No terminal-level test drove this path (handlePTYData
// was 66.7%), so a mutant dropping the `resp = h.screen.TakeResponse()` capture or the
// `h.ptmx.Write(resp)` writeback survived. The read deadline is required so the
// red case fails instead of blocking forever.
func TestHandlePTYData_writesDeviceQueryResponseToPTY(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
	h.ptmx = pw

	// DSR cursor-position query; the fresh 30x120 screen's cursor is at home.
	h.handlePTYData([]byte("\x1b[6n"))

	if err := pr.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 64)
	n, err := pr.Read(buf)
	if err != nil {
		t.Fatalf("reading device-query response from PTY: %v (handlePTYData must write the screen's taken response back)", err)
	}
	if got, want := string(buf[:n]), "\x1b[1;1R"; got != want {
		t.Errorf("device-query response written to PTY = %q, want %q", got, want)
	}
}

// TestHandleResume_frameOrderByAltState pins the resume frame-ordering invariant
// behind findings l-f38 and h-f1: on resume with committed history beyond the
// client's haveThrough, the server delivers the current-window screen frame and
// the scroll replay in an order that depends on the live alt state, because the
// window frame is what sets the client's alt flag and the client drops scroll
// frames while that flag is set (store.ts applyScroll).
//   - MAIN screen (InAltScreen=false): the window frame must PRECEDE the replay,
//     so a client with a stale alt flag (disconnected in alt, app left alt while
//     away) leaves alt before the replayed history lands; otherwise it is
//     silently dropped (finding l-f38). The window must also be a full repaint at
//     the committed base (num_changed == screen_height, base == committed).
//   - ALT screen (InAltScreen=true): the replay must PRECEDE the window frame, so
//     a client not yet in alt (fresh load / second tab on an in-alt session)
//     stores the main-screen history before the window frame flips it into alt;
//     otherwise that history is dropped (the h-f1 regression).
//
// In both arms the window frame must carry the live alt state (offset-26 bit3),
// so a mutant hardcoding it false is caught.
func TestHandleResume_frameOrderByAltState(t *testing.T) {
	tests := []struct {
		name        string
		inAlt       bool
		windowFirst bool // expected: window screen frame precedes the scroll replay
	}{
		{"main screen: window frame precedes replay", false, true},
		{"in alt: replay precedes window frame", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
			server, client, cleanup := dualConn(t)
			defer cleanup()

			// Commit history the resuming client does not yet hold, so a scroll
			// replay is generated alongside the window frame.
			h.scrollback.Append([][]vt.WireRun{makeLine("L0"), makeLine("L1"), makeLine("L2")})
			// The window frame must carry this live alt state in both arms.
			h.screen.InAltScreen = tc.inAlt

			h.handleResume(server, &clientState{}, "sid", -1, 0, nil) // -1 ⇒ client holds nothing, replay all

			frames := readServerFrames(t, client, 300*time.Millisecond)

			firstScreen, firstScroll := -1, -1
			for i, f := range frames {
				if len(f) == 0 {
					continue
				}
				switch f[0] {
				case wireMsgScreen:
					if firstScreen == -1 {
						firstScreen = i
					}
				case wireMsgScroll:
					if firstScroll == -1 {
						firstScroll = i
					}
				}
			}

			if firstScreen == -1 {
				t.Fatalf("resume sent no screen frame; the current window must be delivered on resume")
			}
			if firstScroll == -1 {
				t.Fatalf("resume sent no scroll frame; committed history beyond haveThrough must be replayed")
			}
			if tc.windowFirst && firstScreen >= firstScroll {
				t.Errorf("window screen frame at index %d, scroll replay at index %d; on the main screen the window frame must precede the replay so the client leaves alt before the history lands",
					firstScreen, firstScroll)
			}
			if !tc.windowFirst && firstScroll >= firstScreen {
				t.Errorf("scroll replay at index %d, window screen frame at index %d; in alt the replay must precede the window frame so a not-yet-alt client stores history before the window flips it into alt",
					firstScroll, firstScreen)
			}

			// The window frame must carry the server's LIVE alt state. cursor_flags
			// is the byte at offset 26 (1 type + 8 ack + 8 base + 2 row + 2 col + 2
			// height + 2 num_changed + 1 style); bit3 (0x08) is altActive.
			screenFrame := frames[firstScreen]
			if len(screenFrame) < 27 {
				t.Fatalf("screen frame too short (%d bytes) to carry the cursor_flags byte", len(screenFrame))
			}
			if gotAlt := screenFrame[26]&0x08 != 0; gotAlt != tc.inAlt {
				t.Errorf("window frame altActive bit = %v, want %v (frame must reflect the server's live screen state)", gotAlt, tc.inAlt)
			}

			// On the main screen the resume window frame must additionally be a
			// FULL repaint at the committed base: every screen row is present
			// (num_changed == screen_height) and base equals the committed history
			// length (3 lines appended above). The ordering and alt-bit checks read
			// only the frame header, so neither pins the window's row set.
			if !tc.inAlt {
				gotHeight := binary.LittleEndian.Uint16(screenFrame[21:23])
				gotChanged := binary.LittleEndian.Uint16(screenFrame[23:25])
				if gotChanged != gotHeight {
					t.Errorf("window frame num_changed = %d, want %d (== screen_height; resume window must repaint every row)", gotChanged, gotHeight)
				}
				if gotBase := binary.LittleEndian.Uint64(screenFrame[9:17]); gotBase != 3 {
					t.Errorf("window frame base = %d, want 3 (committed history length; window sits just past committed)", gotBase)
				}
			}
		})
	}
}

// TestHealSize_growsSurvivorAfterBindingClientLeaves verifies the disconnect
// heal: when the client whose size the shared screen currently holds departs,
// healSize relaxes the screen to the smallest size the remaining clients need
// (here the lone desktop), so a survivor is not left clamped to a departed
// phone's size. healSize is called directly (the debounce timer is a stdlib
// wrapper over it, exercised via maybeHealSize in the test below).
func TestHealSize_growsSurvivorAfterBindingClientLeaves(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()

	// Real (throwaway-server) conns, not zero-value fakes: the handler is
	// started, so its flush scheduler is live and may dispatch to every
	// registered conn at any moment — a write to a zero-value websocket.Conn
	// segfaults. (The old unconditional 1s resize hold merely masked this.)
	phoneConn, _, cleanupPhone := dualConn(t)
	defer cleanupPhone()
	deskConn, _, cleanupDesk := dualConn(t)
	defer cleanupDesk()
	phone := h.registry.Add(phoneConn)
	desk := h.registry.Add(deskConn)

	// Phone resizes last (last-writer-wins): starts the child at 40x20 and makes
	// the shared screen 40x20. Desktop's larger size is recorded but not applied.
	h.handleResize(phone, 40, 20)
	h.registry.RecordSize(desk, 120, 40)

	h.mu.Lock()
	w, ht := h.screen.Width, h.screen.Height
	h.mu.Unlock()
	if w != 40 || ht != 20 {
		t.Fatalf("setup: screen = %dx%d, want 40x20 (phone last-wrote)", w, ht)
	}

	// Phone departs; the desktop is the sole survivor.
	h.registry.Remove(phoneConn)
	h.healSize()

	h.mu.Lock()
	w, ht = h.screen.Width, h.screen.Height
	h.mu.Unlock()
	if w != 120 || ht != 40 {
		t.Errorf("healSize after phone left: screen = %dx%d, want 120x40 (relaxed to the surviving desktop)", w, ht)
	}
}

// TestMaybeHealSize_onlyArmsForTheBindingClient verifies the heal is armed only
// when the departed client was holding the current screen size; a client at a
// different size leaving arms nothing (some other client / a live resize still
// holds the current size).
func TestMaybeHealSize_onlyArmsForTheBindingClient(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	defer h.Shutdown()

	// No registered client: the started handler's flush scheduler dispatches
	// to every registered conn, and a write to a zero-value websocket.Conn
	// segfaults (the old unconditional 1s resize hold merely masked this).
	// maybeHealSize needs none — it compares the departed size (its args)
	// against the screen; handleResize accepts an unregistered state.
	h.handleResize(&clientState{}, 40, 20) // shared screen is now 40x20

	// A non-binding departure (size != current screen) arms nothing.
	h.maybeHealSize(120, 40)
	h.mu.Lock()
	armed := h.healTimer != nil
	h.mu.Unlock()
	if armed {
		t.Errorf("maybeHealSize(120x40) armed a heal, but the screen is 40x20 (departed client was not binding)")
	}

	// The binding departure (size == current screen) arms the debounced heal.
	h.maybeHealSize(40, 20)
	h.mu.Lock()
	armed = h.healTimer != nil
	if h.healTimer != nil {
		h.healTimer.Stop() // don't let it fire during later tests
	}
	h.mu.Unlock()
	if !armed {
		t.Errorf("maybeHealSize(40x20) did not arm a heal, but the departed client held the current 40x20 screen")
	}
}

// TestHandleResume_ledgerLostFlag pins the resumeAck ledger-loss signal: the
// ackFlags bit fires exactly when the resume key missed the registry while the
// client claimed sent bytes (its ledger was GC'd/evicted — the server cannot
// vouch for that input), and stays clear for a genuine first connect
// (sentBytes=0) or a key hit. Also pins the tail layout: byte 33 carries the
// server's wireProtocolVersion, byte 34 the flags.
func TestHandleResume_ledgerLostFlag(t *testing.T) {
	tests := []struct {
		name           string
		preSeedSession bool
		sentBytes      uint64
		wantLost       bool
	}{
		{"key miss with claimed bytes signals loss", false, 120, true},
		{"genuine first connect stays clear", false, 0, false},
		{"key hit with claimed bytes stays clear", true, 120, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
			server, client, cleanup := dualConn(t)
			defer cleanup()

			if tc.preSeedSession {
				h.registry.sessions["sid"] = &sessionState{lastSeen: time.Now(), bytesReceived: 120}
			}
			h.handleResume(server, &clientState{}, "sid", -1, tc.sentBytes, nil)

			var resumeAck []byte
			for _, f := range readServerFrames(t, client, 300*time.Millisecond) {
				if len(f) > 0 && f[0] == wireMsgResumeAck {
					resumeAck = f
					break
				}
			}
			if resumeAck == nil {
				t.Fatalf("no resumeAck frame received")
			}
			if len(resumeAck) < 35 {
				t.Fatalf("resumeAck is %d bytes; want >= 35 (serverWireVersion + ackFlags tail)", len(resumeAck))
			}
			if v := resumeAck[33]; v != wireProtocolVersion {
				t.Errorf("resumeAck serverWireVersion byte = %d, want %d", v, wireProtocolVersion)
			}
			if gotLost := resumeAck[34]&resumeAckFlagLedgerLost != 0; gotLost != tc.wantLost {
				t.Errorf("resumeAck ledgerLost flag = %v, want %v", gotLost, tc.wantLost)
			}
		})
	}
}

// TestSweepAcks_acksQuietInputOnce pins the ackOnly sweep: input applied with
// no content frame in the same tick (a silent app — `read -s`) must produce
// exactly one ackOnly frame carrying the advanced count, and a second sweep
// with no further input must send nothing (lastAckSent recorded). This is the
// path that keeps outbox trimming independent of app output.
func TestSweepAcks_acksQuietInputOnce(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	server, client, cleanup := dualConn(t)
	defer cleanup()

	state := h.registry.Add(server)
	h.registry.ResolveSession(state, "sid")
	h.registry.IncrementReceived(state, 42)

	h.sweepAcks()
	frames := framePump(t, client)
	var acks []uint64
	deadline := time.After(2 * time.Second)
collect:
	for len(acks) == 0 {
		select {
		case f := <-frames:
			if len(f) == 9 && f[0] == wireMsgAckOnly {
				acks = append(acks, binary.LittleEndian.Uint64(f[1:9]))
			}
		case <-deadline:
			break collect
		}
	}
	if len(acks) != 1 || acks[0] != 42 {
		t.Fatalf("sweep after quiet input sent acks %v; want exactly [42]", acks)
	}

	h.sweepAcks()
	// One pump for the whole test (a readServerFrames re-read is vacuous:
	// its idle timeout closed the conn, so a second call always returns
	// nothing — the assertion could never fail).
	if extra := nextFrame(frames, 300*time.Millisecond); extra != nil {
		t.Errorf("second sweep with no new input sent a frame (type %d); want none", extra[0])
	}
}

// TestDispatchFrame_suppressesRedundantAckSweep verifies the NoteAcksSent hook:
// when a content frame already carried a client's current ack (stamped by
// withClientAck in dispatchFrame), the following no-frame tick's sweep must
// not resend it as an ackOnly.
func TestDispatchFrame_suppressesRedundantAckSweep(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	server, client, cleanup := dualConn(t)
	defer cleanup()

	state := h.registry.Add(server)
	h.registry.ResolveSession(state, "sid")
	h.registry.IncrementReceived(state, 7)

	frame := &flushFrame{
		clients:      map[*websocket.Conn]uint64{server: 7},
		modesPayload: encodeModesMsg(true, false, false, false, false, false, false, 0, 0),
		screenHeight: 1,
	}
	h.dispatchFrame(frame)
	// One pump for the whole test: readServerFrames' idle timeout closes the
	// conn, so a second call on the same socket returns nothing
	// unconditionally and the suppression assertion below could never fail.
	frames := framePump(t, client)
	if got := nextFrame(frames, 2*time.Second); got == nil || got[0] != wireMsgModes {
		t.Fatalf("dispatch emitted %v; want one modes frame", got)
	}

	h.sweepAcks()
	if extra := nextFrame(frames, 300*time.Millisecond); extra != nil {
		t.Errorf("sweep after a content frame carried ack=7 sent a frame (type %d); want none (NoteAcksSent must suppress)", extra[0])
	}
}

// sendText writes a raw TEXT WebSocket message (the v4 control transport).
func sendText(t *testing.T, ws *websocket.Conn, payload []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("text write: %v", err)
	}
}

// bootstrapResume sends the v3-encoded binary resume that ARMS a connection
// for the typed-framing upgrade (protocolVersion >= 4), mirroring the client's
// bootstrap (design §4 phase 1).
func bootstrapResume(t *testing.T, ws *websocket.Conn, sessionID string) {
	t.Helper()
	sendControl(t, ws, map[string]any{
		"type": "resume", "sessionId": sessionID, "sentBytes": 0,
		"haveThrough": -1, "protocolVersion": wireProtocolVersion,
	})
}

// readUntilClose drains frames until the server closes the connection,
// returning the close status (or failing on timeout / non-close errors).
func readUntilClose(t *testing.T, ws *websocket.Conn, timeout time.Duration) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	for {
		_, _, err := ws.Read(ctx)
		if err == nil {
			continue
		}
		status := websocket.CloseStatus(err)
		if status == -1 {
			t.Fatalf("connection ended without a close status: %v", err)
		}
		return status
	}
}

// TestTypedFraming_latchSequence pins the v4 happy path end to end, including
// the adversarial-review F1 payload: after bootstrap (binary resume, arms) +
// text upgrade (latches), a binary frame that IS a byte-exact v3 control frame
// (0x00 + {"type":"ping"}) must reach the PTY as literal input — the latch
// retired the sentinel, so nothing is consumed as control anymore. Delivery is
// proven three ways (impl-review finding 2): the echo carries the JSON, NO
// pong frame is ever sent (a sentinel mis-parse would have answered the ping),
// and the session ledger advances by EXACTLY len(payload) — every byte,
// leading NUL included, counted as input.
func TestTypedFraming_latchSequence(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithWorkDir("/"))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	dctx, dcancel := context.WithTimeout(t.Context(), 5*time.Second)
	//nolint:bodyclose // library contract: Body is nil on success
	ws, _, err := websocket.Dial(dctx, wsURL, nil)
	dcancel()
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer func() { _ = ws.Close(websocket.StatusNormalClosure, "") }()

	sendControl(t, ws, map[string]any{"type": "resize", "cols": 100, "rows": 40})
	bootstrapResume(t, ws, "sid-latch")

	sendText(t, ws, []byte(`{"type":"upgrade"}`))
	// Post-latch text controls still dispatch (resize via text keeps the pipe healthy).
	sendText(t, ws, []byte(`{"type":"resize","cols":90,"rows":30}`))

	// The F1 payload: valid v3 control bytes sent as post-latch binary input.
	payload := append([]byte{0x00}, []byte(`{"type":"ping"}`)...)
	wctx, wcancel := context.WithTimeout(t.Context(), time.Second)
	defer wcancel()
	if err := ws.Write(wctx, websocket.MessageBinary, payload); err != nil {
		t.Fatalf("binary write: %v", err)
	}

	// Drain frames until cat's echo carries the JSON — and assert no pong
	// (wireMsgPong) ever arrives: the decisive negative that the payload was
	// NOT consumed as a control ping.
	rctx, rcancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer rcancel()
	var echoed bytes.Buffer
	for !bytes.Contains(echoed.Bytes(), []byte(`{"type":"ping"}`)) {
		_, msg, err := ws.Read(rctx)
		if err != nil {
			t.Fatalf("read: %v (echo so far: %q)", err, echoed.Bytes())
		}
		if len(msg) > 0 && msg[0] == wireMsgPong {
			t.Fatalf("server answered a pong: the post-latch binary payload was parsed as a control ping")
		}
		echoed.Write(msg)
	}

	// Ledger proof: exactly len(payload) input bytes were counted for the
	// session (IncrementReceived runs only on the input path, with the whole
	// frame). The resize/upgrade controls contribute nothing.
	want := uint64(len(payload))
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.registry.mu.Lock()
		sess := h.registry.sessions["sid-latch"]
		var got uint64
		if sess != nil {
			got = sess.bytesReceived
		}
		h.registry.mu.Unlock()
		if got == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session bytesReceived = %d, want %d (all payload bytes incl. the leading NUL must count as input)", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTypedFraming_textSizeLimits pins the wsReadLimit boundary for text
// controls empirically (impl-review finding 5: verify coder/websocket's
// boundary semantics rather than assuming them): a valid control padded to
// EXACTLY wsReadLimit bytes must still dispatch (and latch), while one byte
// over must close the connection with 1009 StatusMessageTooBig — enforced by
// the transport before handler code runs.
func TestTypedFraming_textSizeLimits(t *testing.T) {
	pad := func(total int) []byte {
		// {"type":"upgrade","pad":"<a...>"} padded to exactly total bytes.
		const overhead = len(`{"type":"upgrade","pad":""}`)
		return []byte(`{"type":"upgrade","pad":"` + strings.Repeat("a", total-overhead) + `"}`)
	}

	t.Run("exactly at the limit dispatches", func(t *testing.T) {
		ws, cleanup := dialHandler(t, []string{"/bin/cat"})
		defer cleanup()
		bootstrapResume(t, ws, "sid-limit")

		msg := pad(wsReadLimit)
		if len(msg) != wsReadLimit {
			t.Fatalf("test bug: padded control is %d bytes, want %d", len(msg), wsReadLimit)
		}
		sendText(t, ws, msg) // latches; a close here would fail the next step
		// Prove the connection survived and latched: post-latch binary input
		// with a leading NUL reaches the PTY.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := ws.Write(ctx, websocket.MessageBinary, []byte{0x00, 'O', 'K', '9'}); err != nil {
			t.Fatalf("post-limit write: %v", err)
		}
		readUntil(t, ws, []byte("OK9"), 2*time.Second)
	})

	t.Run("one byte over closes 1009", func(t *testing.T) {
		ws, cleanup := dialHandler(t, []string{"/bin/cat"})
		defer cleanup()
		bootstrapResume(t, ws, "sid-limit-over")

		sendText(t, ws, pad(wsReadLimit+1))
		if got := readUntilClose(t, ws, 2*time.Second); got != websocket.StatusMessageTooBig {
			t.Errorf("close status = %v, want %v (transport-enforced read limit)", got, websocket.StatusMessageTooBig)
		}
	})
}

// TestTypedFraming_textPolicyCloses pins the server's text-frame policy: text
// before the arm, unparseable text after the arm, empty text, and invalid
// UTF-8 all close the connection with the documented status codes — never
// latching, never guessing, never reaching the PTY (review F3/F7).
func TestTypedFraming_textPolicyCloses(t *testing.T) {
	tests := []struct {
		name       string
		arm        bool
		payload    []byte
		wantStatus websocket.StatusCode
	}{
		{"text before arm", false, []byte(`{"type":"upgrade"}`), websocket.StatusUnsupportedData},
		{"unparseable text after arm", true, []byte("not json"), websocket.StatusUnsupportedData},
		{"empty text", true, []byte{}, websocket.StatusUnsupportedData},
		{"invalid utf-8 text", true, []byte{0xff, 0xfe, '{'}, websocket.StatusInvalidFramePayloadData},
		{"unrecognized control before latch", true, []byte(`{"type":"mystery"}`), websocket.StatusUnsupportedData},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, cleanup := dialHandler(t, []string{"/bin/cat"})
			defer cleanup()
			if tc.arm {
				bootstrapResume(t, ws, "sid-policy")
			}
			sendText(t, ws, tc.payload)
			if got := readUntilClose(t, ws, 2*time.Second); got != tc.wantStatus {
				t.Errorf("close status = %v, want %v", got, tc.wantStatus)
			}
		})
	}
}

// TestTypedFraming_preLatchKeepsV3Semantics verifies that an ARMED (but not
// latched) connection still runs the exact v3 binary path: sentinel controls
// dispatch, and the parse-fallback delivers a 0x00-leading non-JSON frame to
// the PTY whole (the P2 closure) — so a v4-capable client that never upgrades
// (v3 server pairing) loses nothing.
func TestTypedFraming_preLatchKeepsV3Semantics(t *testing.T) {
	ws, cleanup := dialHandler(t, []string{"/bin/cat"})
	defer cleanup()

	sendControl(t, ws, map[string]any{"type": "resize", "cols": 100, "rows": 40})
	bootstrapResume(t, ws, "sid-prelatch")

	// Sentinel control still consumed as control (resize does not reach the PTY).
	sendControl(t, ws, map[string]any{"type": "resize", "cols": 90, "rows": 30})

	// P2 parse-fallback: 0x00-leading non-JSON binary is INPUT, delivered whole.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, []byte{0x00, 'Z', 'Q', '7'}); err != nil {
		t.Fatalf("fallback write: %v", err)
	}
	readUntil(t, ws, []byte("ZQ7"), 2*time.Second)
}

// TestParseFallback_countsBytes pins the P2 accounting contract: a fallback
// frame increments the session ledger by the FULL frame length (leading NUL
// included), so client and server byte counts stay aligned and acks land on
// frame boundaries.
func TestParseFallback_countsBytes(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithWorkDir("/"))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	dctx, dcancel := context.WithTimeout(t.Context(), 5*time.Second)
	//nolint:bodyclose // library contract: Body is nil on success
	ws, _, err := websocket.Dial(dctx, wsURL, nil)
	dcancel()
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer func() { _ = ws.Close(websocket.StatusNormalClosure, "") }()

	sendControl(t, ws, map[string]any{"type": "resize", "cols": 100, "rows": 40})
	bootstrapResume(t, ws, "sid-count")

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	// A solitary NUL and a NUL-leading frame — both fallback input.
	if err := ws.Write(ctx, websocket.MessageBinary, []byte{0x00}); err != nil {
		t.Fatalf("solitary NUL write: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageBinary, []byte{0x00, 'A'}); err != nil {
		t.Fatalf("NUL-leading write: %v", err)
	}
	readUntil(t, ws, []byte("A"), 2*time.Second) // input observed at the PTY

	deadline := time.Now().Add(2 * time.Second)
	for {
		h.registry.mu.Lock()
		sess := h.registry.sessions["sid-count"]
		var got uint64
		if sess != nil {
			got = sess.bytesReceived
		}
		h.registry.mu.Unlock()
		if got == 3 {
			break // 1 (solitary NUL) + 2 (NUL + 'A')
		}
		if time.Now().After(deadline) {
			t.Fatalf("session bytesReceived = %d, want 3 (fallback frames must count their full length)", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWireCompatibility_declaredClientVersionPolicy(t *testing.T) {
	tests := map[string]struct {
		version      int
		wantRejected bool
	}{
		"version silent":  {version: 0},
		"supported floor": {version: MinSupportedClientWireVersion},
		"current":         {version: WireProtocolVersion},
		"future":          {version: WireProtocolVersion + 1},
		"below floor": {
			version:      MinSupportedClientWireVersion - 1,
			wantRejected: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ws, cleanup := dialHandler(t, []string{"/bin/cat"})
			defer cleanup()

			sendControl(t, ws, map[string]any{
				"type":            ctlTypeResume,
				"protocolVersion": tc.version,
			})

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if tc.wantRejected {
				for {
					_, _, err := ws.Read(ctx)
					if err == nil {
						continue
					}
					if got := websocket.CloseStatus(err); got != WireIncompatibleCloseCode {
						t.Errorf("close status = %d, want %d", got, WireIncompatibleCloseCode)
					}
					if !strings.Contains(err.Error(), "reload or upgrade") {
						t.Errorf("close error %q lacks actionable reload/upgrade reason", err)
					}
					return
				}
			}

			// A supported, current, future, or version-silent declaration keeps
			// the socket usable. A binary-sentinel ping proves the read loop did
			// not exit; future revisions deliberately retain the v4 baseline.
			sendControl(t, ws, map[string]any{"type": ctlTypePing})
			for {
				_, msg, err := ws.Read(ctx)
				if err != nil {
					t.Fatalf("accepted version %d closed before pong: %v", tc.version, err)
				}
				if len(msg) > 0 && msg[0] == wireMsgPong {
					return
				}
			}
		})
	}
}
