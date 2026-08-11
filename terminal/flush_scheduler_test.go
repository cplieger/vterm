package terminal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/cplieger/web-terminal-engine/v3/vt"
)

// P4 (event-driven flush + zero-client render suspension) completion-criteria
// tests. The 50 ms ticker is gone: the scheduler sleeps until poked (PTY
// output, resize, attach/resume, reliable input), flushes the first wake
// immediately, batches sustained output at flushInterval, and arms a retry at
// the DEC 2026 hold-release deadline so a held final redraw still lands.

// dialSession dials a manager-routed WS for the session and returns the conn.
func dialSession(t *testing.T, srv *httptest.Server, id string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:bodyclose // coder/websocket Dial nils resp.Body on success
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws?session="+id, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	return ws
}

type wsReadResult struct {
	typ  websocket.MessageType
	data []byte
	err  error
}

// startWSReader owns all reads for ws. Tests observe its channel with timers
// instead of canceling an in-flight websocket.Read: coder/websocket documents
// read-context cancellation as connection-fatal, so using expiring contexts to
// assert quiet periods destroys the socket needed by the next assertion.
func startWSReader(t *testing.T, ws *websocket.Conn) <-chan wsReadResult {
	t.Helper()
	results := make(chan wsReadResult, 16)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		defer close(results)
		for {
			typ, data, err := ws.Read(context.Background())
			select {
			case results <- wsReadResult{typ: typ, data: data, err: err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return results
}

// readUntilQuiet drains frames until none arrives for the grace window,
// returning the number read. Used to consume the attach-time burst so a test
// measures only what its own stimulus produces.
func readUntilQuiet(t *testing.T, results <-chan wsReadResult, grace time.Duration) int {
	t.Helper()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	n := 0
	for {
		select {
		case result, ok := <-results:
			if !ok {
				t.Fatal("WebSocket reader stopped while draining attach frames")
			}
			if result.err != nil {
				t.Fatalf("WebSocket read while draining attach frames: %v", result.err)
			}
			n++
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(grace)
		case <-timer.C:
			return n
		}
	}
}

func readFrameWithin(t *testing.T, results <-chan wsReadResult, timeout time.Duration) wsReadResult {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result, ok := <-results:
		if !ok {
			t.Fatal("WebSocket reader stopped before the expected frame")
		}
		if result.err != nil {
			t.Fatalf("WebSocket read: %v", result.err)
		}
		return result
	case <-timer.C:
		t.Fatalf("no WebSocket frame within %v", timeout)
		return wsReadResult{}
	}
}

func assertNoFrame(t *testing.T, results <-chan wsReadResult, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case result, ok := <-results:
		if !ok {
			t.Fatal("WebSocket reader stopped during quiet-window assertion")
		}
		if result.err != nil {
			t.Fatalf("WebSocket read during quiet-window assertion: %v", result.err)
		}
		t.Fatalf("unexpected WebSocket frame during %v quiet window: type=%v bytes=%d", duration, result.typ, len(result.data))
	case <-timer.C:
	}
}

// TestFlushScheduler_isolatedEchoBeatsTickAlignment pins completion criterion
// (2): an isolated PTY write flushes without waiting out a fixed interval.
// The old ticker added 0-50 ms of tick alignment to every isolated echo; the
// event-driven scheduler flushes the first wake immediately, so the frame
// must arrive well inside flushInterval/2 (25 ms) plus scheduling slack.
func TestFlushScheduler_isolatedEchoBeatsTickAlignment(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	t.Cleanup(h.Shutdown)
	if err := h.StartEager(); err != nil {
		t.Fatalf("StartEager: %v", err)
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:bodyclose // coder/websocket Dial nils resp.Body on success
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "") // #nosec G104 -- test cleanup
	results := startWSReader(t, ws)

	// Let the attach burst + cat's startup settle (scheduler goes idle).
	readUntilQuiet(t, results, 200*time.Millisecond)

	// One isolated write straight into the screen path (bypassing the PTY
	// round-trip so the measurement is the scheduler's latency, not cat's).
	start := time.Now()
	h.handlePTYData([]byte("echo-probe"))
	readFrameWithin(t, results, 5*time.Second)
	elapsed := time.Since(start)

	// Generous slack over the criterion's flushInterval/2 for CI scheduling
	// noise: the point is "immediate, not tick-aligned" — the old ticker's
	// WORST case was a full flushInterval (50 ms) and its average 25 ms.
	if elapsed >= flushInterval {
		t.Fatalf("isolated echo took %v, want < flushInterval (%v): first-wake flush must not be tick-gated", elapsed, flushInterval)
	}
}

// TestFlushScheduler_sustainedOutputRetainsBatching pins the other half of
// completion criterion (2): once a burst is active, another dirty poke waits
// for the remainder of flushInterval instead of producing an unbounded frame
// rate. The tolerance covers delivery time between dispatch and this reader.
func TestFlushScheduler_sustainedOutputRetainsBatching(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	t.Cleanup(h.Shutdown)
	if err := h.StartEager(); err != nil {
		t.Fatalf("StartEager: %v", err)
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:bodyclose // coder/websocket Dial nils resp.Body on success
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "") // #nosec G104 -- test cleanup
	results := startWSReader(t, ws)
	readUntilQuiet(t, results, 200*time.Millisecond)

	h.handlePTYData([]byte("first"))
	readFrameWithin(t, results, 5*time.Second)
	secondDirtyAt := time.Now()
	h.handlePTYData([]byte("second"))
	readFrameWithin(t, results, 5*time.Second)

	const deliverySlack = 10 * time.Millisecond
	if elapsed := time.Since(secondDirtyAt); elapsed < flushInterval-deliverySlack {
		t.Fatalf("second sustained frame arrived after %v, want >= %v (flushInterval minus delivery slack)", elapsed, flushInterval-deliverySlack)
	}
}

// TestFlushScheduler_zeroClientSuspensionRetainsHistory pins completion
// criterion (3) at the contract level: with no client attached, passes do no
// render/diff work but STILL drain scrolled-off lines into the retained ring,
// and a later attach replays that history.
func TestFlushScheduler_zeroClientSuspensionRetainsHistory(t *testing.T) {
	h := NewHandler([]string{"/bin/true"}, WithLogger(nil))
	// Process-free: drive the screen directly (same pattern as the ED3 test).
	h.screen = vt.New(3, 20)
	h.sizeEstablished = true

	// Scroll lines off a tiny screen with NOBODY attached.
	for range 10 {
		h.handlePTYData([]byte("suspended line\r\n"))
	}
	frame, hold := h.buildFrame()
	if frame != nil {
		t.Fatalf("buildFrame with zero clients = %+v, want nil (suspension must skip render/diff)", frame)
	}
	if !hold.IsZero() {
		t.Fatalf("zero-client pass reported hold %v, want zero", hold)
	}
	if got := len(h.scrollback.Lines()); got == 0 {
		t.Fatal("zero-client pass retained no scrollback; history must survive suspension for the next attach")
	}

	// The suspension pass must not have consumed one-shot signals: stage a
	// clipboard copy while suspended and confirm it is still pending.
	h.handlePTYData([]byte("\x1b]52;c;aGVsbG8=\x07"))
	if _, _ = h.buildFrame(); len(h.pendingClipboard) == 0 {
		t.Fatal("zero-client pass consumed pendingClipboard; one-shot signals must stay pending for the next attach")
	}
}

// TestFlushScheduler_heldRedrawFlushesAtDeadline pins completion criterion
// (4): a DEC 2026 synchronized-output hold with NO subsequent PTY byte still
// flushes when the hold deadline passes — the scheduler arms a release timer
// rather than waiting for the next poke (which would never come).
func TestFlushScheduler_heldRedrawFlushesAtDeadline(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	srv := httptest.NewServer(m.WebSocketHandler())
	t.Cleanup(srv.Close)
	ws := dialSession(t, srv, id)
	defer ws.Close(websocket.StatusNormalClosure, "") // #nosec G104 -- test cleanup
	results := startWSReader(t, ws)
	readUntilQuiet(t, results, 200*time.Millisecond)

	h := handlerOf(t, m, id)
	// BSU (hold), content, and NOTHING after — no ESU, no further output.
	// vt caps the 2026 hold at 1s from the h; the redraw must flush at that
	// deadline via the scheduler's release timer.
	h.handlePTYData([]byte("\x1b[?2026h"))
	h.handlePTYData([]byte("held redraw content"))

	// No frame while held (a short quiet window well inside the hold).
	assertNoFrame(t, results, 300*time.Millisecond)

	// The release deadline (1s from the hold) must deliver the redraw with no
	// further PTY bytes. Allow generous slack for CI.
	readFrameWithin(t, results, 3*time.Second)
}

// TestFlushScheduler_resizeReprintHeldUntilSettled drives the full kiro-cli
// post-resize choreography through the real scheduler and a live WebSocket:
// a size change (SIGWINCH), then ED3 + the transcript reprinted in several
// small DEC 2026 BSU/ESU brackets. Pre-fix, the first bracket's ESU cleared
// the screen-level resize hold, so every inter-bracket flush pass streamed a
// mid-reprint window — the "whole history visibly churns through the screen"
// glitch on every phone keyboard/rotation resize. The settle hold must keep
// the wire QUIET across the whole reprint and deliver the settled result as
// one post-quiet batch.
func TestFlushScheduler_resizeReprintHeldUntilSettled(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	t.Cleanup(h.Shutdown)
	if err := h.StartEager(); err != nil {
		t.Fatalf("StartEager: %v", err)
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:bodyclose // coder/websocket Dial nils resp.Body on success
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "") // #nosec G104 -- test cleanup
	results := startWSReader(t, ws)

	// Pre-reprint content on screen; let the attach burst + echo settle.
	h.handlePTYData([]byte("old transcript line\r\n"))
	readUntilQuiet(t, results, 200*time.Millisecond)

	// The phone keyboard opens: a size CHANGE arms the settle hold.
	h.handleResize(&clientState{}, 100, 24)

	// kiro-cli's redraw: ED3, then the transcript in small 2026 brackets with
	// inter-bracket gaps well under redrawSettleQuiet. Each gap is asserted
	// frame-free: pre-fix the first ESU released the hold and the next
	// 50ms-cadence pass leaked a mid-reprint frame into exactly these windows.
	h.handlePTYData([]byte("\x1b[3J"))
	for range 3 {
		h.handlePTYData([]byte("\x1b[?2026h" + "reprint chunk\r\n" + "\x1b[?2026l"))
		assertNoFrame(t, results, 40*time.Millisecond)
	}

	// Reprint done: the settled batch must land once quiet elapses.
	start := time.Now()
	readFrameWithin(t, results, redrawSettleQuiet+2*time.Second)
	if elapsed := time.Since(start); elapsed > redrawSettleCap {
		t.Fatalf("settled frame took %v after the last reprint byte, want <= cap %v", elapsed, redrawSettleCap)
	}
}

// TestFlushScheduler_reprintLongerThanCapCoalesces drives the case the settle
// hold's cap used to handle badly: a redraw that keeps writing PAST
// redrawSettleCap. Releasing there streamed the rest of the reprint a partial
// screen at a time (the churn the hold exists to prevent), so a lapse now lets
// ONE full repaint through and re-arms instead. The scheduler-level assertion is
// that the wire carries few frames across a multi-cap reprint rather than one per
// flushInterval.
//
// Uses real time because the point is the scheduler's own timers. The budget is
// derived from the constants, so changing them cannot silently make this vacuous.
func TestFlushScheduler_reprintLongerThanCapCoalesces(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithLogger(nil))
	t.Cleanup(h.Shutdown)
	if err := h.StartEager(); err != nil {
		t.Fatalf("StartEager: %v", err)
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	//nolint:bodyclose // coder/websocket Dial nils resp.Body on success
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "") // #nosec G104 -- test cleanup
	results := startWSReader(t, ws)

	h.handlePTYData([]byte("pre-resize content\r\n"))
	readUntilQuiet(t, results, 200*time.Millisecond)

	// The phone keyboard opens: a size CHANGE arms the hold.
	h.handleResize(&clientState{}, 100, 24)

	// A reprint that outlasts the cap: small chunks with gaps well under
	// redrawSettleQuiet, so the child never goes quiet, for longer than one cap
	// interval.
	reprintFor := redrawSettleCap + redrawSettleCap/2
	deadline := time.Now().Add(reprintFor)
	for time.Now().Before(deadline) {
		h.handlePTYData([]byte("reprint line\r\n"))
		time.Sleep(20 * time.Millisecond)
	}

	// Drain everything the reprint window produced, then wait out the settle,
	// counting only SCREEN payloads. That is what coalescing is about: a screen
	// frame is a state the user sees, so shipping one mid-redraw is the churn.
	// Scroll frames are orthogonal — they carry history that lands above the
	// viewport — and streaming them is correct, so counting every websocket
	// message would conflate the two and assert the wrong thing.
	screens := readUntilQuietCounting(t, results, redrawSettleQuiet+500*time.Millisecond, wireMsgScreen)

	// Pre-fix the cap lapsed once and every flushInterval pass after it shipped a
	// mid-reprint screen, so this window carried tens of them. Coalescing ships
	// about one per cap interval plus the settled one. The bound asserts an order
	// of magnitude rather than an exact count, so scheduling jitter cannot make it
	// flaky, while still failing decisively against the old release-and-stream.
	maxScreens := int(reprintFor/redrawSettleCap) + 3
	if screens > maxScreens {
		t.Errorf("reprint of %v carried %d screen frames, want <= %d: the hold released mid-redraw instead of coalescing",
			reprintFor, screens, maxScreens)
	}
	if screens == 0 {
		t.Fatal("reprint carried no screen frame at all; the hold never released")
	}
}

// readUntilQuietCounting drains frames until none arrives for the grace window,
// returning how many carried the given wire message type. The type-filtering
// sibling of readUntilQuiet, for assertions about one payload kind.
func readUntilQuietCounting(
	t *testing.T,
	results <-chan wsReadResult,
	grace time.Duration,
	msgType byte,
) int {
	t.Helper()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	n := 0
	for {
		select {
		case result, ok := <-results:
			if !ok {
				t.Fatal("WebSocket reader stopped while draining frames")
			}
			if result.err != nil {
				t.Fatalf("WebSocket read while draining frames: %v", result.err)
			}
			if len(result.data) > 0 && result.data[0] == msgType {
				n++
			}
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(grace)
		case <-timer.C:
			return n
		}
	}
}
