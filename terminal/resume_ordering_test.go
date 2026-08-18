package terminal

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/cplieger/web-terminal-engine/v5/vt"
)

// These tests pin the resume/live-flush write-ordering contract (the
// reconnect-during-output race, R4 adversarial finding): handleResume's
// snapshot+batch and dispatchFrame's per-client payload loop are serialized by
// clientState.writeMu; a frame built before a resume snapshot is stripped by
// the resumeGen check to its durable payloads (scroll chunks and clipboard,
// which the batch does not re-deliver) rather than written whole after the
// batch; and the batch takes a builder reset with its snapshot so the first
// frame built after it is a full repaint that supersedes anything stripped.
//
// Harness note: the client conn is read by ONE long-lived pump goroutine per
// test and every assertion consumes from its channel. A short-context Read
// directly on the conn cannot be used for "nothing arrives" checks — a
// coder/websocket Read whose context expires CLOSES the connection, poisoning
// every later assertion on the same socket.

// wsPair returns a connected server-side and client-side WebSocket pair, so a
// test can drive Handler internals against the server conn and observe what a
// browser would receive on the client conn.
func wsPair(t *testing.T) (sws, cws *websocket.Conn, cleanup func()) {
	t.Helper()
	connCh := make(chan *websocket.Conn, 1)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		connCh <- ws
		<-done // hold the handler (and with it the conn) open until cleanup
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	//nolint:bodyclose // library contract: Body is nil on success
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	cancel()
	if err != nil {
		srv.Close()
		t.Fatalf("ws dial: %v", err)
	}
	s := <-connCh
	return s, c, func() {
		close(done)
		_ = c.CloseNow()
		_ = s.CloseNow()
		srv.Close()
	}
}

// framePump drains binary frames from the client conn into a buffered channel
// for the test's whole life. The buffer is far larger than any frame count
// these tests produce, so the pump never blocks and never drops.
func framePump(t *testing.T, cws *websocket.Conn) <-chan []byte {
	t.Helper()
	frames := make(chan []byte, 8192)
	// context.Background() (not t.Context()): the pump must stay readable for the
	// test's whole life, so its ctx is cancelled by t.Cleanup below rather than at
	// t.Context() cancellation, which precedes the cleanups that close the conn.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		defer close(frames)
		for {
			_, msg, err := cws.Read(ctx)
			if err != nil {
				return
			}
			frames <- msg
		}
	}()
	return frames
}

// awaitFrame consumes frames until pred matches one (returning it) or the
// timeout fires (returning nil).
func awaitFrame(frames <-chan []byte, pred func([]byte) bool, timeout time.Duration) []byte {
	deadline := time.After(timeout)
	for {
		select {
		case msg, ok := <-frames:
			if !ok {
				return nil
			}
			if pred(msg) {
				return msg
			}
		case <-deadline:
			return nil
		}
	}
}

func typeIs(want byte) func([]byte) bool {
	return func(msg []byte) bool { return len(msg) > 0 && msg[0] == want }
}

func containsBytes(want []byte) func([]byte) bool {
	return func(msg []byte) bool { return bytes.Contains(msg, want) }
}

// isFullScreenFrame reports whether a wire frame is a screen frame whose
// changed-row count equals its height — a FULL repaint. Header layout
// (encodeScreenMsg): [type 1][ack 8][base 8][curRow 2][curCol 2][height 2]
// [numChanged 2]...
func isFullScreenFrame(msg []byte) bool {
	if len(msg) < 25 || msg[0] != wireMsgScreen {
		return false
	}
	height := binary.LittleEndian.Uint16(msg[21:23])
	numChanged := binary.LittleEndian.Uint16(msg[23:25])
	return height > 0 && numChanged == height
}

// assertNoContentFrames consumes frames for the whole window and fails on any
// screen or scroll frame. Transport frames (ackOnly, pong) are tolerated:
// sweepAcks deliberately stays outside the write lock (an interleaved bare
// ack is harmless and documented at the dispatchFrame comment).
func assertNoContentFrames(t *testing.T, frames <-chan []byte, window time.Duration, what string) {
	t.Helper()
	deadline := time.After(window)
	for {
		select {
		case msg, ok := <-frames:
			if !ok {
				return
			}
			if len(msg) > 0 && (msg[0] == wireMsgScreen || msg[0] == wireMsgScroll) {
				t.Fatalf("%s: content frame (type %d) delivered", what, msg[0])
			}
		case <-deadline:
			return
		}
	}
}

// writeBinary sends raw PTY input bytes (a binary frame with no 0x00 sentinel).
func writeBinary(t *testing.T, ws *websocket.Conn, b []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, b); err != nil {
		t.Fatalf("binary write: %v", err)
	}
}

func testModesPayload() []byte {
	return encodeModesMsg(false, false, false, false, false, false, false, 0, 0)
}

func TestDispatchFrame_DefersSocketWritesBehindResumeBatch(t *testing.T) {
	h := NewHandler([]string{"/bin/true"}, WithLogger(nil))
	sws, cws, cleanup := wsPair(t)
	defer cleanup()
	frames := framePump(t, cws)
	state := h.registry.Add(sws)
	mkFrame := func() *flushFrame {
		return &flushFrame{
			clients:      map[*websocket.Conn]uint64{sws: 0},
			writers:      map[*websocket.Conn]*clientState{sws: state},
			gens:         map[*websocket.Conn]uint64{sws: state.resumeGen.Load()},
			modesPayload: testModesPayload(),
		}
	}

	// Baseline: an ungated dispatch delivers the frame.
	h.dispatchFrame(mkFrame())
	if awaitFrame(frames, typeIs(wireMsgModes), 2*time.Second) == nil {
		t.Fatal("baseline dispatch: modes frame never arrived")
	}

	// A held write lock (handleResume's batch in flight) makes the dispatcher
	// WAIT: the frame's payloads are not regenerable (scroll lines already
	// committed, one-shots consumed), so it is delayed behind the batch and
	// delivered afterwards — never dropped, never interleaved into it.
	state.writeMu.Lock()
	done := make(chan struct{})
	go func() {
		h.dispatchFrame(mkFrame())
		close(done)
	}()
	// Nothing may arrive while the lock is held, and the dispatch pass must
	// still be waiting (delay-not-drop: a completed pass here would mean the
	// frame was thrown away).
	deadline := time.After(400 * time.Millisecond)
	for held := true; held; {
		select {
		case msg, ok := <-frames:
			if ok {
				state.writeMu.Unlock()
				t.Fatalf("frame (type %d) delivered while the resume write lock was held", msg[0])
			}
			held = false
		case <-done:
			state.writeMu.Unlock()
			t.Fatal("dispatch completed while the lock was held: the frame was dropped, not deferred")
		case <-deadline:
			held = false
		}
	}
	state.writeMu.Unlock()
	// Batch over: the deferred frame delivers.
	if awaitFrame(frames, typeIs(wireMsgModes), 2*time.Second) == nil {
		t.Fatal("deferred frame never delivered after the resume write lock released")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch pass never completed after the lock released")
	}
}

// nextFrame returns the next frame from the pump, or nil on timeout. Strict
// single-frame read (no consume-until): these direct-dispatch tests run no
// flushLoop, so every frame on the wire is one the test dispatched, and
// ORDER is part of the contract under test.
func nextFrame(frames <-chan []byte, timeout time.Duration) []byte {
	select {
	case msg, ok := <-frames:
		if !ok {
			return nil
		}
		return msg
	case <-time.After(timeout):
		return nil
	}
}

// dispatchProbe is one independent subject for the generation-gate cases: a
// handler, a socket registered in its client registry, a pump draining that
// socket, and a builder for frames captured against the CURRENT generation.
// Each case builds its own, so no case inherits another's wire position — the
// cases below were once ordered phases over one socket, which made every
// failure after the first a misattribution.
type dispatchProbe struct {
	h       *Handler
	sws     *websocket.Conn
	state   *clientState
	frames  <-chan []byte
	mkFrame func() *flushFrame
}

func newDispatchProbe(t *testing.T) *dispatchProbe {
	t.Helper()
	h := NewHandler([]string{"/bin/true"}, WithLogger(nil))
	sws, cws, cleanup := wsPair(t)
	t.Cleanup(cleanup)
	frames := framePump(t, cws)
	state := h.registry.Add(sws)
	p := &dispatchProbe{h: h, sws: sws, state: state, frames: frames}
	p.mkFrame = func() *flushFrame {
		return &flushFrame{
			clients:      map[*websocket.Conn]uint64{sws: 0},
			writers:      map[*websocket.Conn]*clientState{sws: state},
			gens:         map[*websocket.Conn]uint64{sws: state.resumeGen.Load()},
			modesPayload: testModesPayload(),
		}
	}
	return p
}

// staleGen bumps the socket's resume generation the way handleResume does
// (under h.mu, before snapshotting) and returns the generation a frame
// captured BEFORE it. That ordering is what makes such a frame stale, so the
// fixture models the only state the gate actually sees: a captured generation
// one BELOW current, never above.
func (p *dispatchProbe) staleGen() uint64 {
	gen := p.state.resumeGen.Load()
	p.state.resumeGen.Add(1)
	return gen
}

// staleFrame returns a frame carrying one payload of every class — state
// (screen, modes, title), durable one-shot (clipboard), durable history
// (scroll) — already made stale by a resume snapshot: handleResume bumps the
// generation under h.mu before snapshotting, so the frame's captured
// generation is one behind.
func (p *dispatchProbe) staleFrame() *flushFrame {
	f := p.mkFrame()
	f.changed = []int{0}
	f.rows = [][]vt.WireRun{{{T: "STALE-SCREEN", F: -1, B: -1, Uc: -1}}}
	f.screenHeight = 1
	f.titlePayload = encodeTitleMsg("STALE-TITLE")
	f.clipboardPayload = encodeClipboardMsg([]byte("STALE-CLIP"))
	f.scrollLines = [][]vt.WireRun{{{T: "STALE-SCROLL", F: -1, B: -1, Uc: -1}}}
	f.scrollFirstIdx = 41
	f.gens = map[*websocket.Conn]uint64{p.sws: p.staleGen()}
	return f
}

// TestDispatchFrame_StaleFrameDropsStateKeepsDurable covers the generation
// gate: a frame built before a resume snapshot must not write its STATE
// payloads (screen/modes/title) after the batch — they would regress the
// client to pre-snapshot state the batch superseded — while its DURABLE
// payloads must still be written, because the batch's replay starts above the
// client's haveThrough and does not re-deliver the frame's scroll lines (RACE
// round-2 finding, claude+gpt — under ring-cap pressure they may not even be
// in the ring anymore), and the clipboard is a consumed one-shot no later
// frame will carry.
func TestDispatchFrame_StaleFrameDropsStateKeepsDurable(t *testing.T) {
	t.Run("stale frame delivers durables in order, drops state", func(t *testing.T) {
		// Order is asserted strictly: first the frame's clipboard, then its
		// scroll. A write-everything-on-mismatch mutant delivers modes first
		// and fails here; a drop-everything mutant delivers nothing and fails
		// too.
		p := newDispatchProbe(t)
		p.h.dispatchFrame(p.staleFrame())

		first := nextFrame(p.frames, 2*time.Second)
		if first == nil {
			t.Fatal("stale frame's durable payloads never arrived (dropped whole: permanent scrollback hole)")
		}
		if first[0] != wireMsgClipboard {
			t.Fatalf("first delivered frame is type %d, want clipboard (%d): state payloads leaked past the generation check or order broke", first[0], wireMsgClipboard)
		}
		second := nextFrame(p.frames, 2*time.Second)
		if second == nil || second[0] != wireMsgScroll {
			t.Fatalf("second delivered frame %v, want the scroll chunk", second)
		}
		if !bytes.Contains(second, []byte("STALE-SCROLL")) {
			t.Error("scroll frame does not carry the stale frame's lines")
		}
		if bytes.Contains(second, []byte("STALE-SCREEN")) {
			t.Error("screen rows rode the durable subset")
		}
	})

	t.Run("post-resume frame delivers its modes first", func(t *testing.T) {
		// A frame built AFTER the snapshot (current generation) delivers its
		// modes — and it is the FIRST modes frame on the wire, proving the
		// stale frame's modes payload was dropped, not delayed. The trailing
		// check then also fails if the stale screen or title payload was
		// delivered. The stale frame's own two durables are consumed first, so
		// this case owns the whole exchange rather than inheriting a queue.
		p := newDispatchProbe(t)
		p.h.dispatchFrame(p.staleFrame())
		for range 2 {
			if got := nextFrame(p.frames, 2*time.Second); got == nil {
				t.Fatal("stale frame's durable payloads never arrived")
			}
		}

		p.h.dispatchFrame(p.mkFrame())
		got := nextFrame(p.frames, 2*time.Second)
		if got == nil || got[0] != wireMsgModes {
			t.Fatalf("post-resume dispatch: got %v, want the fresh modes frame", got)
		}
		if extra := nextFrame(p.frames, 300*time.Millisecond); extra != nil {
			t.Fatalf("an extra frame (type %d) arrived: the stale frame's state was delivered too", extra[0])
		}
	})

	t.Run("state-only stale frame writes nothing and records no ack", func(t *testing.T) {
		// A stale frame with NO durable payloads writes nothing — and is NOT
		// recorded as delivered: lastAckSent must keep its value, or the next
		// ack sweep would skip a client that was never actually told (the
		// "never records an ack as sent that no frame carried" contract).
		p := newDispatchProbe(t)
		p.state.lastAckSent.Store(3)
		stateOnly := p.mkFrame()
		stateOnly.clients = map[*websocket.Conn]uint64{p.sws: 7}
		stateOnly.gens = map[*websocket.Conn]uint64{p.sws: p.staleGen()}
		p.h.dispatchFrame(stateOnly)
		if msg := nextFrame(p.frames, 300*time.Millisecond); msg != nil {
			t.Fatalf("state-only stale frame delivered a payload (type %d)", msg[0])
		}
		if got := p.state.lastAckSent.Load(); got != 3 {
			t.Errorf("lastAckSent = %d after an empty-durable strip, want 3 (nothing was written, nothing may be recorded)", got)
		}
	})

	t.Run("a durable strip restamps the socket's freshest ack", func(t *testing.T) {
		// A strip RESTAMPS its durable payloads with the socket's freshest ack
		// (the batch's resumeAck), NOT the pre-resume ack captured at snapshot
		// time: after a ledger-loss resume the captured value can exceed the
		// client's reset outbox accounting, and applyAck's min(received,
		// bytesSent) would then trim input the server never acked
		// (connection.ts applyAck). The wire ack lives at bytes 1..9 of every
		// payload (withClientAck). And the delivered bookkeeping must record
		// exactly the stamped value — monotonic, so lastAckSent never regresses
		// below the batch's store and never latches a value no frame carried.
		p := newDispatchProbe(t)
		p.state.lastAckSent.Store(11) // the resumeAck the batch wrote
		restamp := p.mkFrame()
		restamp.clients = map[*websocket.Conn]uint64{p.sws: 9999} // stale captured ack
		restamp.gens = map[*websocket.Conn]uint64{p.sws: p.staleGen()}
		restamp.clipboardPayload = encodeClipboardMsg([]byte("RESTAMP"))
		p.h.dispatchFrame(restamp)
		got := nextFrame(p.frames, 2*time.Second)
		if got == nil || got[0] != wireMsgClipboard {
			t.Fatalf("restamp probe: got %v, want the clipboard payload", got)
		}
		if ack := binary.LittleEndian.Uint64(got[1:9]); ack != 11 {
			t.Errorf("durable payload carries ack %d, want 11 (the socket's freshest); the strip stamped the captured pre-resume ack", ack)
		}
		if got := p.state.lastAckSent.Load(); got != 11 {
			t.Errorf("lastAckSent = %d after a durable-only write, want 11 (the stamped value; neither the captured 9999 nor a regression)", got)
		}
	})

	t.Run("a clipboard-less strip still delivers and restamps its scroll chunk", func(t *testing.T) {
		// A strip whose only durable payload is SCROLL — no clipboard, which is
		// the ordinary shape: chunks ride every sustained-output flush while the
		// clipboard is a rare consumed one-shot — must still deliver the chunk.
		// durableSubset returns scrollPayloads as-is on that branch; dropping it
		// is the permanent scrollback hole the gate exists to prevent.
		p := newDispatchProbe(t)
		p.state.lastAckSent.Store(11)
		scrollOnly := p.mkFrame()
		scrollOnly.clients = map[*websocket.Conn]uint64{p.sws: 9999}
		scrollOnly.gens = map[*websocket.Conn]uint64{p.sws: p.staleGen()}
		scrollOnly.changed = []int{0}
		scrollOnly.rows = [][]vt.WireRun{{{T: "DROP-ME", F: -1, B: -1, Uc: -1}}}
		scrollOnly.screenHeight = 1
		scrollOnly.scrollLines = [][]vt.WireRun{{{T: "SCROLL-ONLY", F: -1, B: -1, Uc: -1}}}
		scrollOnly.scrollFirstIdx = 77
		p.h.dispatchFrame(scrollOnly)
		got := nextFrame(p.frames, 2*time.Second)
		if got == nil || got[0] != wireMsgScroll || !bytes.Contains(got, []byte("SCROLL-ONLY")) {
			t.Fatalf("clipboard-less strip: got %v, want the scroll chunk (durableSubset dropped the durable history)", got)
		}
		if ack := binary.LittleEndian.Uint64(got[1:9]); ack != 11 {
			t.Errorf("clipboard-less strip stamped ack %d, want 11 (the restamp must apply on this branch too)", ack)
		}
		if extra := nextFrame(p.frames, 300*time.Millisecond); extra != nil {
			t.Fatalf("clipboard-less strip delivered an extra frame (type %d): state leaked past the gate", extra[0])
		}
	})

	t.Run("a matching-generation frame carries its own captured ack", func(t *testing.T) {
		// The complement of the strip: a frame whose generation MATCHES carries
		// its own captured ack — the socket's fresh bytesReceived at snapshot
		// time — NOT lastAckSent. Stamping lastAckSent here would freeze the
		// client's ack for as long as frames keep flowing (NoteAcksSent records
		// what was stamped, so the value never advances) and its outbox would
		// stop trimming until the first no-frame pass swept it.
		p := newDispatchProbe(t)
		p.state.lastAckSent.Store(11)
		current := p.mkFrame()
		current.clients = map[*websocket.Conn]uint64{p.sws: 12} // fresh > lastAckSent (11)
		p.h.dispatchFrame(current)
		got := nextFrame(p.frames, 2*time.Second)
		if got == nil || got[0] != wireMsgModes {
			t.Fatalf("current-generation dispatch: got %v, want the modes frame", got)
		}
		if ack := binary.LittleEndian.Uint64(got[1:9]); ack != 12 {
			t.Errorf("matching-generation frame carries ack %d, want its captured 12 (the restamp must apply ONLY on a strip)", ack)
		}
		if got := p.state.lastAckSent.Load(); got != 12 {
			t.Errorf("lastAckSent = %d after a delivered current-generation frame, want 12", got)
		}
	})
}

// dialOwnHandler is dialHandler with the Handler kept, so a test can reach
// registry internals (the sole client's state) and the builder.
func dialOwnHandler(t *testing.T, cmd []string) (*Handler, *websocket.Conn, func()) {
	t.Helper()
	h := NewHandler(cmd, WithWorkDir("/"), WithLogger(nil))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	//nolint:bodyclose // library contract: Body is nil on success
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	cancel()
	if err != nil {
		srv.Close()
		t.Fatalf("ws dial: %v", err)
	}
	return h, ws, func() {
		_ = ws.Close(websocket.StatusNormalClosure, "")
		srv.Close()
	}
}

// soleClientState waits for the handler's registry to hold exactly one client
// and returns its state.
func soleClientState(t *testing.T, h *Handler) *clientState {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.registry.mu.Lock()
		if len(h.registry.clients) == 1 {
			for _, st := range h.registry.clients {
				h.registry.mu.Unlock()
				return st
			}
		}
		h.registry.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("client never registered")
	return nil
}

func TestResumeGate_LiveOutputDeferredThenFullRepaintFollows(t *testing.T) {
	// End-to-end through a REAL handler: while the resume write lock is held
	// (the batch stand-in — the very lock handleResume takes), live output
	// produces NO content frames on the socket; after a builder reset taken
	// under h.mu while the lock is still held (the invariant handleResume
	// provides by resetting inside its snapshot's h.mu section) and the
	// lock's release plus the dirty poke, the deferred output arrives —
	// delayed, never dropped — AND a FULL repaint follows (the builder reset
	// must not be removable: a plain diff also carries the text, so the
	// full-frame assertion is what pins the reset).
	h, ws, cleanup := dialOwnHandler(t, []string{"/bin/cat"})
	defer cleanup()
	frames := framePump(t, ws)
	sendControl(t, ws, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	bootstrapResume(t, ws, "s-gate")
	// Live path proof: echoed input flows to the client.
	writeBinary(t, ws, []byte("hello\r"))
	if awaitFrame(frames, containsBytes([]byte("hello")), 5*time.Second) == nil {
		t.Fatal("live echo never arrived; harness broken")
	}

	st := soleClientState(t, h)
	st.writeMu.Lock()
	writeBinary(t, ws, []byte("BLOCKED\r"))
	assertNoContentFrames(t, frames, 400*time.Millisecond, "resume write lock held")
	// Batch end: full-repaint reset under h.mu while the write lock is still
	// held (handleResume takes its reset even earlier, with the snapshot),
	// then release and poke.
	h.mu.Lock()
	h.builder.Reset()
	h.mu.Unlock()
	st.writeMu.Unlock()
	h.markDirty()
	if awaitFrame(frames, containsBytes([]byte("BLOCKED")), 5*time.Second) == nil {
		t.Fatal("output produced during the gate never arrived after the batch ended")
	}
	if awaitFrame(frames, isFullScreenFrame, 5*time.Second) == nil {
		t.Fatal("no FULL repaint followed the batch: the builder reset did not take")
	}
}

// wireFrameMaxCounter scans a screen or scroll frame's run texts for the
// emitter's monotonic T<n> counter and returns the highest found (-1 if
// none), plus whether a screen frame is a FULL repaint. Layouts per
// encodeScreenMsg / encodeScrollMsg (wire_binary.go): screen header is
// [type 1][ack 8][base 8][curRow 2][curCol 2][height 2][numChanged 2]
// [cursorStyle 1][cursorFlags 1], scroll header [type 1][ack 8][firstIdx 8]
// [numLines 2]; rows are [idx? 2][numRuns 2] then per run [textLen 2][text]
// [F 4][B 4][A 2][Uc 4][urlLen 2][url].
func wireFrameMaxCounter(b []byte) (maxCounter int, full bool) {
	maxCounter = -1
	var count int
	var off int
	perRowIdx := false
	switch {
	case len(b) >= 27 && b[0] == wireMsgScreen:
		height := int(binary.LittleEndian.Uint16(b[21:23]))
		count = int(binary.LittleEndian.Uint16(b[23:25]))
		full = height > 0 && count == height
		off = 27
		perRowIdx = true
	case len(b) >= 19 && b[0] == wireMsgScroll:
		count = int(binary.LittleEndian.Uint16(b[17:19]))
		off = 19
	default:
		return -1, false
	}
	for range count {
		if perRowIdx {
			off += 2 // changed-row index
		}
		if off+2 > len(b) {
			return maxCounter, full
		}
		numRuns := int(binary.LittleEndian.Uint16(b[off : off+2]))
		off += 2
		for range numRuns {
			if off+2 > len(b) {
				return maxCounter, full
			}
			textLen := int(binary.LittleEndian.Uint16(b[off : off+2]))
			off += 2
			if off+textLen > len(b) {
				return maxCounter, full
			}
			for _, m := range tickCounterRe.FindAllStringSubmatch(string(b[off:off+textLen]), -1) {
				if n, err := strconv.Atoi(m[1]); err == nil && n > maxCounter {
					maxCounter = n
				}
			}
			off += textLen + 14 // F4 + B4 + A2 + Uc4
			if off+2 > len(b) {
				return maxCounter, full
			}
			urlLen := int(binary.LittleEndian.Uint16(b[off : off+2]))
			off += 2 + urlLen
		}
	}
	return maxCounter, full
}

var tickCounterRe = regexp.MustCompile(`T(\d+)`)

func TestResumeBatch_AtomicOnTheSocketUnderLiveOutput(t *testing.T) {
	// The gpt-R4-shaped ordering contract, observed from the client, in the
	// backpressured shape the fable RACE review proved necessary: a loopback
	// reader gives the batch a sub-ms write window that no 50ms flush pass
	// ever lands in, so the un-backpressured version of this test passed
	// against the PRE-FIX code. Each round therefore sends the resume and
	// then STOPS READING under an unthrottled emitter — kernel socket
	// buffers fill, the batch's writes block mid-sequence, and live flushes
	// get a real window to misorder. Two oracles, both from the probe that
	// caught the revert:
	//   I1 — between the resumeAck and the batch's window frame, only
	//        modes/title (and tolerated transport acks) may appear;
	//   I4 — no FULL screen frame may ever carry a max T<n> counter BELOW
	//        the highest already delivered (a stale batch window written
	//        after newer live content: the regression the race caused).
	h, ws, cleanup := dialOwnHandler(t,
		[]string{"/bin/sh", "-c", "i=0; while :; do i=$((i+1)); echo T$i; done"})
	defer cleanup()
	_ = h
	// Huge scroll bursts under an unthrottled emitter exceed the client
	// library's default 32KB read limit, which CLOSES the conn (the 0.15s
	// instant-death failure shape; fable's probe hit it first).
	ws.SetReadLimit(1 << 26)
	sendControl(t, ws, map[string]any{"type": "resize", "cols": 80, "rows": 24})

	// ONE pump on an UNBUFFERED channel: when the test stops receiving, the
	// pump blocks after at most one in-flight frame and stops reading the
	// socket — real backpressure without touching the conn's read context.
	raw := make(chan []byte)
	pumpCtx := t.Context()
	go func() {
		defer close(raw)
		for {
			_, msg, err := ws.Read(pumpCtx)
			if err != nil {
				return
			}
			select {
			case raw <- msg:
			case <-pumpCtx.Done():
				return
			}
		}
	}()
	readOne := func(timeout time.Duration) []byte {
		select {
		case msg, ok := <-raw:
			if !ok {
				return nil
			}
			return msg
		case <-time.After(timeout):
			return nil
		}
	}

	highWater := -1
	fold := func(b []byte) {
		if n, _ := wireFrameMaxCounter(b); n > highWater {
			highWater = n
		}
	}

	// Bootstrap: resume and read through the first batch window.
	bootstrapResume(t, ws, "s-atomic")
	for {
		msg := readOne(10 * time.Second)
		if msg == nil {
			t.Fatal("bootstrap: emitter output never arrived; harness broken")
		}
		fold(msg)
		if len(msg) > 0 && msg[0] == wireMsgScreen {
			break
		}
	}

	for round := range 6 {
		// Let output accumulate briefly while reading (advances highWater).
		settle := time.After(150 * time.Millisecond)
	settleLoop:
		for {
			select {
			case <-settle:
				break settleLoop
			default:
				if msg := readOne(50 * time.Millisecond); msg != nil {
					fold(msg)
				}
			}
		}

		// Resume, then stop reading: buffers fill and the batch blocks
		// mid-write while live flushes keep building.
		bootstrapResume(t, ws, "s-atomic")
		time.Sleep(600 * time.Millisecond)

		// Drain: find the resumeAck, then apply I1 until the batch window
		// and I4 to every full screen frame until the stream quiets.
		for {
			msg := readOne(10 * time.Second)
			if msg == nil {
				t.Fatalf("round %d: resumeAck never arrived", round)
			}
			fold(msg)
			if len(msg) > 0 && msg[0] == wireMsgResumeAck {
				break
			}
		}
		sawWindow := false
		checkUntil := time.Now().Add(400 * time.Millisecond)
		for !sawWindow || time.Now().Before(checkUntil) {
			msg := readOne(2 * time.Second)
			if msg == nil {
				if sawWindow {
					continue
				}
				t.Fatalf("round %d: batch window never arrived after resumeAck", round)
			}
			if len(msg) == 0 {
				continue
			}
			switch msg[0] {
			case wireMsgScreen:
				n, full := wireFrameMaxCounter(msg)
				if !sawWindow && !full {
					t.Fatalf("round %d: DIFF screen frame between resumeAck and the batch window (live flush interleaved into the batch)", round)
				}
				if full && n >= 0 && n < highWater {
					t.Fatalf("round %d: full screen frame max T%d below delivered high water T%d (stale window written after newer content — the race)", round, n, highWater)
				}
				fold(msg)
				sawWindow = true
			case wireMsgScroll:
				if !sawWindow {
					t.Fatalf("round %d: scroll frame interleaved into the resume batch", round)
				}
				fold(msg)
			case wireMsgModes, wireMsgTitle, wireMsgAckOnly, wireMsgPong:
				// the batch's own frames / tolerated transport
			default:
				t.Fatalf("round %d: unexpected frame type %d inside the resume batch", round, msg[0])
			}
		}
	}
}

func TestResumeBatch_DeterministicBlockAdvanceRelease(t *testing.T) {
	// The gpt-prescribed contract test, made deterministic by the
	// testResumeBatchHold seam: block a REAL resume batch after its writes
	// (snapshot C delivered, write lock still held), COMMIT new history to
	// the scrollback ring while it is held (a live pass builds and must
	// block in dispatch), then release and assert from the client's stream
	// that (1) nothing interleaved into the held batch, (2) the lines
	// committed during the hold arrive as SCROLL frames after it — the
	// payload class the resume replay can never re-deliver, so this pins
	// delay-not-drop where it matters — and (3) a FULL repaint follows (the
	// builder reset taken with the snapshot is consumed by the deferred
	// pass).
	hold := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBatch := func() { releaseOnce.Do(func() { close(release) }) }
	// holdOnce mirrors releaseOnce: the seam is package-global, so a second
	// resume entering it (a future test edit, or an unexpected auto-resume)
	// must park on release, not panic the handler goroutine on a double
	// close of hold.
	var holdOnce sync.Once
	holdFn := func() {
		holdOnce.Do(func() { close(hold) })
		<-release
	}
	testResumeBatchHold.Store(&holdFn)
	// LIFO: releaseBatch unblocks a handler still parked in the seam (a
	// t.Fatal before the explicit release would otherwise leak it wedged),
	// THEN cleanup closes the conns, THEN the seam clears. The seam is an
	// atomic.Pointer because httptest does not wait for hijacked conns: a
	// straggler handler may load it after the test returns.
	defer testResumeBatchHold.Store(nil)
	h, ws, cleanup := dialOwnHandler(t, []string{"/bin/cat"})
	defer cleanup()
	defer releaseBatch()
	ws.SetReadLimit(1 << 26)
	frames := framePump(t, ws)
	sendControl(t, ws, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	// Settle: consume the attach/resize repaint and wait for the burst to go
	// idle, so the resume finds the flush loop PARKED. Without this the
	// repaint pass's dispatch can lose the writeMu race to the batch and
	// wedge the loop in wg.Wait before the injection below, making the
	// commit poll racy instead of deterministic.
	if awaitFrame(frames, isFullScreenFrame, 5*time.Second) == nil {
		t.Fatal("attach repaint never arrived; harness broken")
	}
	for nextFrame(frames, 250*time.Millisecond) != nil {
	}

	// The FIRST resume will block on the seam, so run it from a goroutine's
	// perspective: send it, then wait for the batch to reach the hold point.
	bootstrapResume(t, ws, "s-det")
	select {
	case <-hold:
	case <-time.After(5 * time.Second):
		t.Fatal("resume batch never reached the hold seam")
	}

	// The batch is now held with the write lock owned. Drive 30 lines of
	// output through the screen in ONE PTY chunk — enough to scroll history
	// off a 24-row window into the ring, and atomic under h.mu so the very
	// first flush pass (handlePTYData's own dirty poke wakes the idle loop
	// immediately) builds with every line, before it blocks in dispatch on
	// writeMu. Injected directly because this test's read loop is BLOCKED
	// inside handleResume on the seam. Committed advance is the proof the
	// pass's scroll payloads exist and are unregenerable.
	preCommitted, _ := h.ScrollbackBounds()
	var burst bytes.Buffer
	for i := range 30 {
		fmt.Fprintf(&burst, "SCROLLMARK-%d\r\n", i)
	}
	h.handlePTYData(burst.Bytes())
	commitDeadline := time.Now().Add(5 * time.Second)
	for {
		if committed, _ := h.ScrollbackBounds(); committed > preCommitted {
			break
		}
		if time.Now().After(commitDeadline) {
			t.Fatal("no flush pass committed the injected lines during the hold; harness broken")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Drain the batch's own prefix (resumeAck..window), which was written
	// BEFORE the seam. After the window — the batch's last write — nothing
	// may arrive while the hold is in place.
	var sawAck, sawWindow bool
	drainDeadline := time.After(2 * time.Second)
drain:
	for !sawWindow {
		select {
		case msg, ok := <-frames:
			if !ok {
				t.Fatal("conn closed during the held batch")
			}
			if len(msg) == 0 {
				continue
			}
			switch msg[0] {
			case wireMsgResumeAck:
				sawAck = true
			case wireMsgScreen:
				if !sawAck {
					t.Fatal("screen frame before the batch's resumeAck")
				}
				sawWindow = true
			case wireMsgScroll:
				t.Fatal("scroll frame interleaved into the held batch")
			case wireMsgModes, wireMsgTitle, wireMsgAckOnly, wireMsgPong:
			default:
				t.Fatalf("unexpected frame type %d inside the held batch", msg[0])
			}
		case <-drainDeadline:
			break drain
		}
	}
	if !sawAck || !sawWindow {
		t.Fatal("batch prefix (resumeAck..window) never fully arrived")
	}
	// With the hold still in place, the deferred live content must NOT leak.
	select {
	case msg := <-frames:
		t.Fatalf("frame (type %d) delivered while the batch held the write lock", msg[0])
	case <-time.After(300 * time.Millisecond):
	}

	// Release. The deferred pass delivers, in payload order (screen before
	// scroll chunks): a FULL screen frame — the builder reset the snapshot
	// took, consumed by the during-hold build — then SCROLL frames that must
	// carry the lines that scrolled off during the hold (SCROLLMARK-0 is ~7
	// lines below the window bottom, so no repaint can carry it — only the
	// ring lines the pass committed; if the gate had dropped that pass, this
	// history would be unreachable forever).
	releaseBatch()
	if awaitFrame(frames, isFullScreenFrame, 5*time.Second) == nil {
		t.Fatal("no FULL repaint delivered after the batch: the snapshot's builder reset did not take")
	}
	scroll := awaitFrame(frames, func(msg []byte) bool {
		return len(msg) > 0 && msg[0] == wireMsgScroll && bytes.Contains(msg, []byte("SCROLLMARK-0"))
	}, 5*time.Second)
	if scroll == nil {
		t.Fatal("scroll lines committed during the batch never arrived after release (dropped, not deferred: permanent scrollback hole)")
	}
}

func TestResume_SnapshotResetForcesFullPostBatchFrame(t *testing.T) {
	// Pins that handleResume itself takes the builder reset (fable round-3
	// F1: the line moved into the snapshot's h.mu section had no test — the
	// gate test resets the builder by hand, and the seam test's whole-window
	// burst makes any diff full vacuously). Oracle: on a QUIESCENT handler,
	// a resume is followed by a second FULL screen frame — the flush pass
	// handleResume's markDirty wakes, building against the reset cache, so
	// every window row re-emits with no new PTY output. Without the reset
	// that pass builds an empty diff and produces no screen frame at all:
	// deleting the Reset() call turns this test red deterministically.
	h, ws, cleanup := dialOwnHandler(t, []string{"/bin/cat"})
	defer cleanup()
	_ = h
	frames := framePump(t, ws)
	sendControl(t, ws, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	// Prime the builder's last-flushed cache: echo once, then settle to
	// idle, so the only thing that can make the post-resume pass emit a
	// full frame is the reset (not residual dirt from the attach burst).
	writeBinary(t, ws, []byte("prime\r"))
	if awaitFrame(frames, containsBytes([]byte("prime")), 5*time.Second) == nil {
		t.Fatal("prime echo never arrived; harness broken")
	}
	for nextFrame(frames, 300*time.Millisecond) != nil {
	}

	bootstrapResume(t, ws, "s-reset-pin")
	// The batch's own window frame arrives first (full by construction:
	// handleResume encodes every row itself, reset or no reset)...
	if awaitFrame(frames, isFullScreenFrame, 5*time.Second) == nil {
		t.Fatal("resume batch window never arrived")
	}
	// ...then the reset-consuming flush pass must deliver a SECOND full
	// frame with zero new output. Tolerates interleaved transport frames
	// (ackOnly/pong); fails on timeout if the pass built an empty diff.
	if awaitFrame(frames, isFullScreenFrame, 5*time.Second) == nil {
		t.Fatal("no full repaint followed the batch on a quiescent handler: handleResume's snapshot-section builder.Reset() did not take")
	}
}

func TestTakeResumeToken_BurstsThenThrottles(t *testing.T) {
	st := &clientState{}
	for i := range resumeBurst {
		if !st.takeResumeToken() {
			t.Fatalf("token %d refused within the burst", i)
		}
	}
	if st.takeResumeToken() {
		t.Fatal("token granted past the burst with no refill time")
	}
	// One refill interval restores exactly one token.
	st.resumeLast = st.resumeLast.Add(-resumeRefillEvery)
	if !st.takeResumeToken() {
		t.Fatal("token refused after one refill interval")
	}
	if st.takeResumeToken() {
		t.Fatal("second token granted after a single refill interval")
	}
	// A long idle stretch tops the bucket up to the burst cap, never beyond.
	st.resumeLast = st.resumeLast.Add(-time.Hour)
	for i := range resumeBurst {
		if !st.takeResumeToken() {
			t.Fatalf("token %d refused after a full idle top-up", i)
		}
	}
	if st.takeResumeToken() {
		t.Fatal("token granted beyond the burst cap after an idle top-up")
	}
}

func TestResume_SpamThrottleDropsExcessResumes(t *testing.T) {
	// The wiring proof for the per-socket resume throttle: one more resume
	// than the burst allows, fired back to back, yields exactly resumeBurst
	// resumeAcks — the over-limit resume is dropped ackless WITHOUT closing
	// the socket (later controls still work). Timing margin: the loop
	// completes in well under a second against a 2s/token refill, so a
	// stray refill cannot hand the over-limit resume a token.
	h, ws, cleanup := dialOwnHandler(t, []string{"/bin/cat"})
	defer cleanup()
	_ = h
	ws.SetReadLimit(1 << 26)
	frames := framePump(t, ws)
	sendControl(t, ws, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	for range resumeBurst + 1 {
		bootstrapResume(t, ws, "s-throttle")
	}
	for i := range resumeBurst {
		if awaitFrame(frames, typeIs(wireMsgResumeAck), 5*time.Second) == nil {
			t.Fatalf("resumeAck %d never arrived (throttle bit inside the burst)", i)
		}
	}
	if awaitFrame(frames, typeIs(wireMsgResumeAck), 500*time.Millisecond) != nil {
		t.Fatal("resumeAck for the over-burst resume arrived: the throttle is not wired")
	}
	sendControl(t, ws, map[string]any{"type": "ping"})
	if awaitFrame(frames, typeIs(wireMsgPong), 2*time.Second) == nil {
		t.Fatal("socket dead after a throttled resume")
	}
}

// TestHistoryControl_NotInterleavedIntoResumeBatch extends this file's ordering
// contract to the demand-paging read path (docs/paged-scrollback.md §7).
//
// A page reply is a scroll frame written outside the flush loop, on the socket's
// own goroutine, which makes it the one payload that could land BETWEEN the
// resumeAck and the batch's window frame — the client would then apply history
// as live scrollback mid-resume, above indices the batch is about to replay. The
// serialization is the same `clientState.writeMu` the dispatcher takes, so this
// asserts the same delay-not-drop shape: while the batch holds the lock nothing
// is written and the call is still in progress, and the reply lands afterwards.
//
// The goroutine model already serializes these (a socket's control reads and its
// resume run on one goroutine), so this is a regression guard on the lock rather
// than a fix for an observed interleave.
func TestHistoryControl_NotInterleavedIntoResumeBatch(t *testing.T) {
	h := NewHandler([]string{"/bin/true"}, WithScrollbackCapacity(paginationMinRing), WithLogger(nil))
	h.screen = vt.New(3, 20)
	fillRing(h, 100)

	sws, cws, cleanup := wsPair(t)
	defer cleanup()
	frames := framePump(t, cws)
	state := h.registry.Add(sws)
	state.session.Store(&sessionState{})

	pageRequest := func(fromAbs int64) *controlMsg {
		return &controlMsg{Type: ctlTypeHistory, FromAbs: fromAbs, MaxLines: 20}
	}

	// Baseline: with nothing holding the socket, the reply goes out.
	h.historyControl(sws, state, pageRequest(10))
	if awaitFrame(frames, typeIs(wireMsgScroll), 2*time.Second) == nil {
		t.Fatal("baseline: page reply never arrived")
	}

	// A held write lock stands in for handleResume's batch in flight.
	state.writeMu.Lock()
	done := make(chan struct{})
	go func() {
		h.historyControl(sws, state, pageRequest(40))
		close(done)
	}()
	deadline := time.After(400 * time.Millisecond)
	for held := true; held; {
		select {
		case msg, ok := <-frames:
			if ok {
				state.writeMu.Unlock()
				t.Fatalf("frame (type %d) written into the resume batch's window", msg[0])
			}
			held = false
		case <-done:
			state.writeMu.Unlock()
			t.Fatal("historyControl returned while the batch held the lock: it served without serializing")
		case <-deadline:
			held = false
		}
	}
	state.writeMu.Unlock()

	if awaitFrame(frames, typeIs(wireMsgScroll), 2*time.Second) == nil {
		t.Fatal("the deferred page reply never arrived after the batch released the lock")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("historyControl never completed after the lock released")
	}
}
