package terminal

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestResolveSession_GCsIdleSession verifies the opportunistic GC sweep in
// ResolveSession removes a session idle longer than the 60-minute window.
// Resolving an unknown session id triggers the sweep.
func TestResolveSession_GCsIdleSession(t *testing.T) {
	r := newClientRegistry(slog.Default())
	r.sessions["idle"] = &sessionState{
		lastSeen:      time.Now().Add(-61 * time.Minute),
		bytesReceived: 7,
	}

	// Resolving an unknown session id triggers the opportunistic GC sweep.
	r.ResolveSession(&clientState{}, "fresh")

	r.mu.Lock()
	_, present := r.sessions["idle"]
	r.mu.Unlock()
	if present {
		t.Errorf("ResolveSession: 61-minute-idle session still present; want it GC'd (idle > 60 min)")
	}
}

// TestResolveSession_retainsRecentSession verifies the GC sweep keeps a
// session whose last activity is well within the 60-minute window.
func TestResolveSession_retainsRecentSession(t *testing.T) {
	r := newClientRegistry(slog.Default())
	r.sessions["recent"] = &sessionState{
		lastSeen:      time.Now().Add(-1 * time.Minute),
		bytesReceived: 3,
	}

	r.ResolveSession(&clientState{}, "fresh")

	r.mu.Lock()
	_, present := r.sessions["recent"]
	r.mu.Unlock()
	if !present {
		t.Errorf("ResolveSession: 1-minute-idle session was removed; want it retained (threshold is 60 min)")
	}
}

// TestResolveSession_GCLogsOnlyWhenSessionHadBytes verifies the GC sweep emits
// the "gc'd idle session with received bytes" info log only when the evicted
// session actually received input; a zero-byte session is GC'd silently. The
// log lets operators correlate user-visible "my input vanished" reports with
// the eviction.
func TestResolveSession_GCLogsOnlyWhenSessionHadBytes(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	const logMsg = "gc'd idle session with received bytes"

	// bytesReceived > 0 -> the GC must log.
	r := newClientRegistry(slog.Default())
	r.sessions["had-bytes"] = &sessionState{
		lastSeen:      time.Now().Add(-61 * time.Minute),
		bytesReceived: 5,
	}
	r.ResolveSession(&clientState{}, "fresh1")
	if !strings.Contains(buf.String(), logMsg) {
		t.Errorf("GC of idle session with bytesReceived>0 did not emit %q", logMsg)
	}

	// bytesReceived == 0 -> the GC must NOT log.
	buf.Reset()
	r2 := newClientRegistry(slog.Default())
	r2.sessions["no-bytes"] = &sessionState{
		lastSeen:      time.Now().Add(-61 * time.Minute),
		bytesReceived: 0,
	}
	r2.ResolveSession(&clientState{}, "fresh2")
	if strings.Contains(buf.String(), logMsg) {
		t.Errorf("GC of idle session with bytesReceived==0 emitted %q; want silent", logMsg)
	}
}

// TestIncrementReceived_addsPositiveCount verifies a positive byte count is
// added to the session's running total.
func TestIncrementReceived_addsPositiveCount(t *testing.T) {
	r := newClientRegistry(slog.Default())
	st := &clientState{}
	sess := &sessionState{}
	st.session.Store(sess)

	r.IncrementReceived(st, 5)

	if sess.bytesReceived != 5 {
		t.Errorf("IncrementReceived(st, 5) = %d, want 5", sess.bytesReceived)
	}
}

// TestIncrementReceived_zeroIsNoOp verifies a non-positive count returns early
// without touching the session: neither bytesReceived nor lastSeen changes.
// lastSeen is the discriminating observable since += 0 is a no-op on the
// counter regardless.
func TestIncrementReceived_zeroIsNoOp(t *testing.T) {
	r := newClientRegistry(slog.Default())
	st := &clientState{}
	sentinel := time.Unix(1_000_000, 0)
	sess := &sessionState{lastSeen: sentinel}
	st.session.Store(sess)

	r.IncrementReceived(st, 0)

	if sess.bytesReceived != 0 {
		t.Errorf("IncrementReceived(st, 0): bytesReceived = %d, want 0", sess.bytesReceived)
	}
	if !sess.lastSeen.Equal(sentinel) {
		t.Errorf("IncrementReceived(st, 0) modified lastSeen to %v; want unchanged %v", sess.lastSeen, sentinel)
	}
}

// TestRegistry_ConcurrentResolveIncrementSnapshot stresses the registry's own
// lock: many goroutines resolve sessions, increment received bytes, and
// snapshot concurrently. Run under -race to surface data races on the
// sessions map. Real *websocket.Conn values aren't needed — the contention
// under test is on session state, not the client map keys.
func TestRegistry_ConcurrentResolveIncrementSnapshot(t *testing.T) {
	r := newClientRegistry(slog.Default())
	const goroutines = 20
	const iters = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(id int) {
			defer wg.Done()
			for i := range iters {
				state := &clientState{}
				sessionID := "session-" + string(rune('A'+id)) + "-" + string(rune('0'+i%10))
				_, _ = r.ResolveSession(state, sessionID)
				r.IncrementReceived(state, 42)
				_ = r.Snapshot()
			}
		}(g)
	}
	wg.Wait()
}

// TestRegistry_ConcurrentResolveSharedSession stresses contention on the same
// sessionState: many goroutines resolve a small set of shared session ids and
// increment their counters concurrently. Run under -race.
func TestRegistry_ConcurrentResolveSharedSession(t *testing.T) {
	r := newClientRegistry(slog.Default())
	const goroutines = 50
	const iters = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for i := range iters {
				state := &clientState{}
				sid := "shared-session"
				if i%3 == 0 {
					sid = "alt-session"
				}
				_, _ = r.ResolveSession(state, sid)
				r.IncrementReceived(state, 10)
			}
		}()
	}
	wg.Wait()
}

// TestResolveSession_evictsOldestWhenOverCap pins the maxResumeSessions cap backstop
// (CWE-770) that ResolveSession enforces via evictOldestSession: when a new
// session pushes the retained count past maxResumeSessions, the single oldest-lastSeen
// entry is evicted (not the newcomer) and the count returns to maxResumeSessions. The
// eviction body (find-oldest loop + delete) was unexercised after extraction
// into evictOldestSession (only the early-return ran, 22.2% coverage), so a
// mutant in the `sx.lastSeen.Before(oldest)` comparison or the delete survived.
func TestResolveSession_evictsOldestWhenOverCap(t *testing.T) {
	r := newClientRegistry(slog.Default())
	now := time.Now()

	// One session is distinctly the oldest, but still inside the 60-min
	// retention window so the idle GC does NOT remove it -- forcing the cap
	// eviction (not the GC) to be the remover we assert on.
	const oldestID = "oldest"
	r.sessions[oldestID] = &sessionState{lastSeen: now.Add(-30 * time.Minute)}
	// Fill the rest to exactly maxResumeSessions with recent entries. Distinct 2-byte
	// keys avoid an fmt/strconv import (none collide with the longer ASCII ids).
	for i := 1; i < maxResumeSessions; i++ {
		r.sessions[string([]byte{byte(i), byte(i >> 8)})] = &sessionState{lastSeen: now.Add(-time.Minute)}
	}
	if len(r.sessions) != maxResumeSessions {
		t.Fatalf("setup: %d sessions, want exactly maxResumeSessions=%d", len(r.sessions), maxResumeSessions)
	}

	// Resolving a new, unknown session id pushes the count to maxResumeSessions+1 and
	// triggers the cap eviction.
	r.ResolveSession(&clientState{}, "newcomer")

	r.mu.Lock()
	defer r.mu.Unlock()
	if got := len(r.sessions); got != maxResumeSessions {
		t.Errorf("after over-cap resolve: %d sessions retained, want %d (cap eviction must drop exactly one)", got, maxResumeSessions)
	}
	if _, ok := r.sessions[oldestID]; ok {
		t.Errorf("cap eviction kept the oldest session %q; want the oldest-lastSeen entry evicted", oldestID)
	}
	if _, ok := r.sessions["newcomer"]; !ok {
		t.Error("cap eviction removed the just-added newcomer; want the OLDEST entry removed, not the newest")
	}
}

// TestMinLiveSize_minsEachDimensionAndSkipsSizeless verifies MinLiveSize
// returns the per-dimension minimum across connected clients that reported a
// size, skipping any that never sent one, so the result fits inside every
// connected client's viewport.
func TestMinLiveSize_minsEachDimensionAndSkipsSizeless(t *testing.T) {
	r := newClientRegistry(slog.Default())
	r.clients[new(websocket.Conn)] = &clientState{cols: 120, rows: 40}
	r.clients[new(websocket.Conn)] = &clientState{cols: 80, rows: 24}
	r.clients[new(websocket.Conn)] = &clientState{} // never reported a size -> skipped

	cols, rows, ok := r.MinLiveSize()
	if !ok || cols != 80 || rows != 24 {
		t.Errorf("MinLiveSize() = (%d, %d, %v), want (80, 24, true) [min per dimension]", cols, rows, ok)
	}
}

// TestMinLiveSize_falseWhenNoSizedClient verifies MinLiveSize reports ok=false
// when no connected client has a known size, so the heal becomes a no-op.
func TestMinLiveSize_falseWhenNoSizedClient(t *testing.T) {
	r := newClientRegistry(slog.Default())
	r.clients[new(websocket.Conn)] = &clientState{} // connected but no size yet
	if _, _, ok := r.MinLiveSize(); ok {
		t.Errorf("MinLiveSize() ok=true with no sized client; want false")
	}
}

// TestRemove_returnsDepartedSize verifies Remove returns the size the removed
// socket had reported (so the caller can decide whether the departure should
// heal the shared screen) and drops it from the registry.
func TestRemove_returnsDepartedSize(t *testing.T) {
	r := newClientRegistry(slog.Default())
	ws := new(websocket.Conn)
	st := r.Add(ws)
	r.RecordSize(st, 100, 30)

	cols, rows := r.Remove(ws)
	if cols != 100 || rows != 30 {
		t.Errorf("Remove() = (%d, %d), want (100, 30) [the departed socket's recorded size]", cols, rows)
	}
	r.mu.Lock()
	_, present := r.clients[ws]
	r.mu.Unlock()
	if present {
		t.Errorf("Remove: connection still registered after removal")
	}
}

// TestRemove_stampsLastSeenOnDetach pins the retention semantics the iOS-sleep
// resume depends on: the 60-minute window measures time since a client last
// HELD the ledger, not time since the last keystroke. A session whose last
// input was hours ago (a tab reading agent output) must survive a detach plus a
// sub-window sleep — before the detach stamp it was reclaimed on the very next
// key miss, and the returning client got a false ledger-loss / "server
// restarted" banner.
func TestRemove_stampsLastSeenOnDetach(t *testing.T) {
	r := newClientRegistry(slog.Default())
	ws := &websocket.Conn{}
	state := r.Add(ws)
	r.ResolveSession(state, "sid")
	r.IncrementReceived(state, 10)

	// Two hours of reading output with no typing: nothing refreshes lastSeen
	// while the socket stays up, so the ledger ages past the GC window.
	r.mu.Lock()
	r.sessions["sid"].lastSeen = time.Now().Add(-2 * time.Hour)
	r.mu.Unlock()

	r.Remove(ws) // the screen goes to sleep and the socket drops

	// The detach stamp is the whole fix: the clock restarts here, so the ledger
	// now reads as freshly idle rather than two hours stale.
	r.mu.Lock()
	aged := time.Since(r.sessions["sid"].lastSeen)
	r.mu.Unlock()
	if aged > time.Minute {
		t.Fatalf("detach left lastSeen %v old; want stamped to ~now", aged.Round(time.Second))
	}

	// A key miss elsewhere (another tab reconnecting) triggers the sweep 30
	// minutes later — inside the window, measured from the detach.
	r.mu.Lock()
	r.sessions["sid"].lastSeen = time.Now().Add(-30 * time.Minute)
	r.mu.Unlock()

	r.ResolveSession(&clientState{}, "other") // opportunistic GC sweep

	r.mu.Lock()
	_, present := r.sessions["sid"]
	r.mu.Unlock()
	if !present {
		t.Errorf("GC reclaimed a ledger detached only 30m ago; want it retained until 60m past the detach")
	}

	// And it still expires: past the window, measured from the detach.
	r.mu.Lock()
	r.sessions["sid"].lastSeen = time.Now().Add(-61 * time.Minute)
	r.mu.Unlock()
	r.ResolveSession(&clientState{}, "other2")
	r.mu.Lock()
	_, present = r.sessions["sid"]
	r.mu.Unlock()
	if present {
		t.Errorf("GC retained a ledger detached 61m ago; want it reclaimed (the window must still bind)")
	}
}

// TestResolveSession_createdFlagAndLastSeenRefresh pins the two ResolveSession
// behaviors the ledger-loss protocol depends on: a key miss reports
// created=true (handleResume turns that plus claimed sentBytes into the
// resumeAck ledgerLost flag), and a key HIT refreshes lastSeen so an attached
// but input-idle client (a pure viewer reconnecting) never ages into the GC
// window merely because IncrementReceived never ran for it.
func TestResolveSession_createdFlagAndLastSeenRefresh(t *testing.T) {
	r := newClientRegistry(slog.Default())

	_, created := r.ResolveSession(&clientState{}, "sid")
	if !created {
		t.Errorf("first ResolveSession(sid): created=false, want true (key miss)")
	}

	// Age the session, then hit it again: created must be false and lastSeen
	// must be refreshed to ~now.
	r.mu.Lock()
	r.sessions["sid"].lastSeen = time.Now().Add(-59 * time.Minute)
	r.mu.Unlock()

	_, created = r.ResolveSession(&clientState{}, "sid")
	if created {
		t.Errorf("second ResolveSession(sid): created=true, want false (key hit)")
	}
	r.mu.Lock()
	age := time.Since(r.sessions["sid"].lastSeen)
	r.mu.Unlock()
	if age > time.Minute {
		t.Errorf("key hit left lastSeen %v old; want refreshed to ~now", age.Round(time.Second))
	}
}

// TestGCSkipsAttachedSessions verifies gcIdleSessions never reclaims a ledger
// attached to a live client: two sessions idle past the 60-minute window, one
// attached to a registered client — the attached one survives the sweep, the
// orphan is deleted.
func TestGCSkipsAttachedSessions(t *testing.T) {
	r := newClientRegistry(slog.Default())
	attachedSess := &sessionState{lastSeen: time.Now().Add(-61 * time.Minute), bytesReceived: 3}
	orphanSess := &sessionState{lastSeen: time.Now().Add(-61 * time.Minute), bytesReceived: 5}
	r.sessions["attached"] = attachedSess
	r.sessions["orphan"] = orphanSess
	ws := &websocket.Conn{}
	state := r.Add(ws)
	state.session.Store(attachedSess)

	// Resolving an unknown session id triggers the opportunistic GC sweep.
	r.ResolveSession(&clientState{}, "fresh")

	r.mu.Lock()
	_, attachedPresent := r.sessions["attached"]
	_, orphanPresent := r.sessions["orphan"]
	r.mu.Unlock()
	if !attachedPresent {
		t.Errorf("GC reclaimed a session attached to a live client; want it retained")
	}
	if orphanPresent {
		t.Errorf("GC retained an unattached idle session; want it reclaimed")
	}
}

// TestEvictOldestSession_prefersUnattached verifies the cap eviction picks the
// oldest UNATTACHED victim when the globally-oldest session is attached to a
// live client: an abuser minting ids can then only evict abandoned ledgers,
// never a connected client's resume state.
func TestEvictOldestSession_prefersUnattached(t *testing.T) {
	r := newClientRegistry(slog.Default())

	// Oldest overall: attached to a live client.
	attachedSess := &sessionState{lastSeen: time.Now().Add(-50 * time.Minute)}
	r.sessions["attached-oldest"] = attachedSess
	ws := &websocket.Conn{}
	state := r.Add(ws)
	state.session.Store(attachedSess)

	// Second-oldest: unattached — the expected victim.
	r.sessions["unattached-victim"] = &sessionState{lastSeen: time.Now().Add(-40 * time.Minute)}

	// Fill to the cap so the next create must evict.
	for i := 0; len(r.sessions) < maxResumeSessions; i++ {
		r.sessions[fmt.Sprintf("filler-%d", i)] = &sessionState{lastSeen: time.Now()}
	}

	r.ResolveSession(&clientState{}, "overflow") // cap+1 → eviction

	r.mu.Lock()
	_, attachedPresent := r.sessions["attached-oldest"]
	_, victimPresent := r.sessions["unattached-victim"]
	total := len(r.sessions)
	r.mu.Unlock()
	if !attachedPresent {
		t.Errorf("cap eviction removed the attached session; want the oldest unattached victim instead")
	}
	if victimPresent {
		t.Errorf("cap eviction kept the oldest unattached session; want it evicted")
	}
	if total > maxResumeSessions {
		t.Errorf("session count %d exceeds cap %d after eviction", total, maxResumeSessions)
	}
}

// TestAckSweepTargets_recordsOptimisticallyAndHonorsNoteAcksSent pins the ack
// sweep's bookkeeping: a session whose bytesReceived advanced past lastAckSent
// is a target exactly once (optimistic record), a session already covered by a
// dispatched content frame (NoteAcksSent) is skipped, and a session-less
// client is never a target.
func TestAckSweepTargets_recordsOptimisticallyAndHonorsNoteAcksSent(t *testing.T) {
	r := newClientRegistry(slog.Default())
	ws := &websocket.Conn{}
	state := r.Add(ws)
	r.ResolveSession(state, "sid")
	r.Add(&websocket.Conn{}) // session-less client: never a target

	r.IncrementReceived(state, 5)
	targets := r.AckSweepTargets()
	if got := targets[ws]; got != 5 || len(targets) != 1 {
		t.Fatalf("AckSweepTargets after +5 input = %v, want map[%p:5] with exactly one entry", targets, ws)
	}
	if again := r.AckSweepTargets(); len(again) != 0 {
		t.Errorf("second AckSweepTargets = %v, want empty (optimistic record must stick)", again)
	}

	// A content frame carried the next value: NoteAcksSent must suppress the sweep.
	r.IncrementReceived(state, 4) // bytesReceived now 9
	r.NoteAcksSent(map[*websocket.Conn]uint64{ws: 9})
	if after := r.AckSweepTargets(); len(after) != 0 {
		t.Errorf("AckSweepTargets after NoteAcksSent(9) = %v, want empty", after)
	}
}

// TestPerSenderResumeKeysKeepIndependentLedgers pins the server half of the
// P1 per-sender resume-key contract: two clients attached to ONE managed
// session resume with distinct keys (`<sid>#<instanceA>` / `<sid>#<instanceB>`,
// minted client-side), and the registry — which keys ledgers by the resume
// string verbatim — must give each its own bytesReceived. The pre-P1 shared
// key acked the COMBINED total to both devices, so device A's applyAck
// trimmed unacked bytes the server had only received from B (silent input
// loss on A's next resume). Trace from the judgement (A: 120 sent / 100
// received; B: 50): A's ack must stay 100 — leaving its 20 unacked bytes in
// its outbox for retransmission — no matter how much B sends.
func TestPerSenderResumeKeysKeepIndependentLedgers(t *testing.T) {
	r := newClientRegistry(slog.Default())
	wsA, wsB := &websocket.Conn{}, &websocket.Conn{}
	stateA := r.Add(wsA)
	stateB := r.Add(wsB)
	defer r.Remove(wsA)
	defer r.Remove(wsB)

	ackA, createdA := r.ResolveSession(stateA, "sess-1#instance-A")
	ackB, createdB := r.ResolveSession(stateB, "sess-1#instance-B")
	if !createdA || !createdB {
		t.Fatalf("fresh per-sender keys must create distinct ledgers (createdA=%v createdB=%v)", createdA, createdB)
	}
	if ackA != 0 || ackB != 0 {
		t.Fatalf("fresh ledgers must start at zero (ackA=%d ackB=%d)", ackA, ackB)
	}

	// A sends 120 bytes but the server receives only 100 before the drop;
	// B sends 50. Each increments ITS OWN ledger.
	r.IncrementReceived(stateA, 100)
	r.IncrementReceived(stateB, 50)

	// A's resume acks A's ledger (100), not the combined 150: its 20 unacked
	// bytes stay in its outbox and retransmit. B's resume acks 50.
	if ack, created := r.ResolveSession(stateA, "sess-1#instance-A"); created || ack != 100 {
		t.Errorf("A's resume = (ack %d, created %v), want (100, false): B's input must not advance A's ledger", ack, created)
	}
	if ack, created := r.ResolveSession(stateB, "sess-1#instance-B"); created || ack != 50 {
		t.Errorf("B's resume = (ack %d, created %v), want (50, false)", ack, created)
	}
}
