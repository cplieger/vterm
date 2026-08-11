package terminal

// Order tests for the shared display order and the two enumerations that serve
// it. Three properties, and they fail in different ways:
//
//   - Both enumerations report ONE order, and it is a function of manager state
//     rather than of the map iteration that built it. A consumer builds its tab
//     strip from whichever enumeration reaches it first, so a per-call order is a
//     per-load strip.
//   - m.order stays exactly the keys of m.sessions. A position only means
//     something while that holds, and four sites maintain it.
//   - A reorder reaches every OTHER client. The write touches no handler and no
//     title, so the sweep's position check is the only thing that emits it.

import (
	"bufio"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompareSessionOrderRankThenAgeThenID(t *testing.T) {
	early := time.Date(2026, time.August, 11, 9, 0, 0, 0, time.UTC)
	late := early.Add(time.Nanosecond)
	key := func(rank int, at time.Time, id string) sessionOrder {
		return sessionOrder{createdAt: at, id: id, rank: rank}
	}

	cases := map[string]struct {
		a    sessionOrder
		b    sessionOrder
		want int
	}{
		// Rank is the arrangement a viewer chose, so it outranks both other keys:
		// the younger session with the lower rank still comes first.
		"rank beats age":                {key(0, late, "zz"), key(1, early, "aa"), -1},
		"rank beats id":                 {key(0, early, "zz"), key(1, early, "aa"), -1},
		"higher rank sorts later":       {key(2, early, "aa"), key(1, late, "zz"), 1},
		"equal rank falls to age":       {key(0, early, "zz"), key(0, late, "aa"), -1},
		"equal rank and age fall to id": {key(0, early, "aa"), key(0, early, "zz"), -1},
		"same session compares equal":   {key(3, early, "aa"), key(3, early, "aa"), 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := compareSessionOrder(tc.a, tc.b)
			if sign(got) != tc.want {
				t.Errorf("compareSessionOrder = %d (sign %d), want sign %d", got, sign(got), tc.want)
			}
			// Total orders are antisymmetric. Without this the comparator could
			// report "before" in both directions for a tie and sort would be free
			// to produce either arrangement, which is the bug this file guards.
			rev := compareSessionOrder(tc.b, tc.a)
			if sign(rev) != -sign(got) {
				t.Errorf("reversed = %d (sign %d), want sign %d", rev, sign(rev), -sign(got))
			}
		})
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// TestRankOfPutsAStrayLast covers the case the createdAt and id keys exist for.
// A session missing from m.order is an invariant violation, and the zero value a
// bare map lookup returns is position 0 — a real session's position. Ranking it
// past the end instead keeps the strip stable and lets age settle the strays.
func TestRankOfPutsAStrayLast(t *testing.T) {
	rank := map[string]int{"a": 0, "b": 1}
	if got := rankOf(rank, "b", len(rank)); got != 1 {
		t.Errorf("rankOf(known) = %d, want 1", got)
	}
	if got := rankOf(rank, "stray", len(rank)); got != 2 {
		t.Errorf("rankOf(stray) = %d, want 2 (past the end, not 0)", got)
	}
}

// TestSetSessionOrderRequiresTheExactLiveSet is the validation table for the
// write side. The one set check is load-bearing three times over: it keeps
// positions dense and unique, it makes the write atomic, and it turns a caller
// whose view of the session set is stale into a refusal rather than a silent
// corruption of the order.
func TestSetSessionOrderRequiresTheExactLiveSet(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	ids := make([]string, 0, 3)
	for range 3 {
		id, err := m.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, id)
	}

	cases := map[string]struct {
		ids  []string
		want bool
	}{
		"the live set, reordered":     {[]string{ids[2], ids[0], ids[1]}, true},
		"the live set, unchanged":     {[]string{ids[0], ids[1], ids[2]}, true},
		"short a session":             {[]string{ids[0], ids[1]}, false},
		"an id that is not a session": {[]string{ids[0], ids[1], "deadbeef"}, false},
		"a duplicate for a real id":   {[]string{ids[0], ids[1], ids[1]}, false},
		"empty":                       {[]string{}, false},
		"nil":                         {nil, false},
		"one extra unknown id":        {[]string{ids[0], ids[1], ids[2], "extra"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			before := m.orderSnapshot()
			if got := m.SetSessionOrder(tc.ids); got != tc.want {
				t.Fatalf("SetSessionOrder(%d ids) = %v, want %v", len(tc.ids), got, tc.want)
			}
			after := m.orderSnapshot()
			if tc.want {
				if !slices.Equal(after, tc.ids) {
					t.Errorf("order = %v, want %v", short(after), short(tc.ids))
				}
				return
			}
			// A refused write must change nothing. A partial application would
			// leave positions neither client asked for.
			if !slices.Equal(after, before) {
				t.Errorf("refused write changed the order: %v -> %v", short(before), short(after))
			}
		})
	}
}

// TestSetSessionOrderDoesNotAliasTheCallerSlice pins the clone. The ids come from
// a decoded request body, so keeping the caller's backing array would let a
// later write through that same slice reorder the manager's state with no request
// at all.
func TestSetSessionOrderDoesNotAliasTheCallerSlice(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	first, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	caller := []string{second, first}
	if !m.SetSessionOrder(caller) {
		t.Fatal("SetSessionOrder = false, want true")
	}
	caller[0], caller[1] = first, second // the caller reuses its slice
	if got := m.orderSnapshot(); !slices.Equal(got, []string{second, first}) {
		t.Errorf("order followed the caller's slice: %v, want %v",
			short(got), short([]string{second, first}))
	}
}

// TestOrderTracksTheSessionSet pins the invariant every position depends on:
// m.order is exactly the keys of m.sessions, in a dense sequence, through each of
// the four sites that change the set. A position means nothing if a closed
// session can linger in the order or a live one can be missing from it.
func TestOrderTracksTheSessionSet(t *testing.T) {
	m := NewSessionManager(catFactory, WithIdleReaper(time.Hour))
	t.Cleanup(m.Shutdown)

	assertConsistent := func(t *testing.T, stage string) {
		t.Helper()
		m.mu.Lock()
		order := slices.Clone(m.order)
		live := slices.Collect(maps.Keys(m.sessions))
		m.mu.Unlock()
		if len(order) != len(live) {
			t.Fatalf("%s: order has %d ids, sessions has %d", stage, len(order), len(live))
		}
		slices.Sort(order)
		slices.Sort(live)
		if !slices.Equal(order, live) {
			t.Fatalf("%s: order %v != live set %v", stage, short(order), short(live))
		}
	}

	ids := make([]string, 0, 4)
	for i := range 4 {
		id, err := m.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, id)
		assertConsistent(t, "after create")
		// Newest last: a client that has arranged its tabs keeps that arrangement,
		// and only the new id moves.
		if got := m.orderSnapshot(); got[len(got)-1] != id {
			t.Errorf("create %d: newest id is at %d, want last", i, slices.Index(got, id))
		}
	}

	// A close from the middle must close the gap, or every later position shifts
	// by an amount nothing tracks.
	if !m.Close(ids[1]) {
		t.Fatal("Close = false, want true")
	}
	assertConsistent(t, "after close")
	if got := m.orderSnapshot(); !slices.Equal(got, []string{ids[0], ids[2], ids[3]}) {
		t.Errorf("order after close = %v, want the survivors in order", short(got))
	}

	// A reorder, then a close of a session that was moved: the survivors keep
	// their relative arrangement rather than snapping back to creation order.
	if !m.SetSessionOrder([]string{ids[3], ids[0], ids[2]}) {
		t.Fatal("SetSessionOrder = false, want true")
	}
	if !m.Close(ids[0]) {
		t.Fatal("Close = false, want true")
	}
	assertConsistent(t, "after close of a reordered session")
	if got := m.orderSnapshot(); !slices.Equal(got, []string{ids[3], ids[2]}) {
		t.Errorf("order = %v, want the arrangement minus the closed session", short(got))
	}

	// The reaper drops every session at once; the order must go with them.
	m.mu.Lock()
	m.idleSince = time.Now().Add(-2 * time.Hour)
	m.mu.Unlock()
	m.maybeReap()
	assertConsistent(t, "after reap")
	if got := m.orderSnapshot(); len(got) != 0 {
		t.Errorf("order after reap = %v, want empty", short(got))
	}
}

// orderSnapshot copies the display order for assertions.
func (m *SessionManager) orderSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.order)
}

// TestListAndSnapshotAgreeOnOrderEveryCall is the regression guard for the
// cross-device tab-order bug. snapshot() ranged m.sessions and returned
// unsorted, so the status stream pushed a different order on every connect (Go
// randomizes map iteration) while List sorted. A client adopting tabs in arrival
// order therefore drew a different strip on every load, and the two enumerations
// disagreed with each other by construction.
//
// Run against a REORDERED set, so the assertion is the shared order rather than
// creation order, which map iteration could match by luck. Repeated because one
// call of an unsorted enumeration can match any order by luck; the chance of that
// holding across every repeat is negligible.
func TestListAndSnapshotAgreeOnOrderEveryCall(t *testing.T) {
	const (
		sessions = 5
		repeats  = 20
	)
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	ids := make([]string, 0, sessions)
	for range sessions {
		id, err := m.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, id)
	}
	want := []string{ids[3], ids[0], ids[4], ids[1], ids[2]}
	if !m.SetSessionOrder(want) {
		t.Fatal("SetSessionOrder = false, want true")
	}

	for i := range repeats {
		listIDs := make([]string, 0, sessions)
		for pos, info := range m.List() {
			listIDs = append(listIDs, info.ID)
			// The field a client sorts by must agree with the sequence it is served
			// in, or the two halves of the contract disagree.
			if info.Order != pos {
				t.Fatalf("call %d: %s is at sequence position %d but reports order %d",
					i, LogID(info.ID), pos, info.Order)
			}
		}
		if !slices.Equal(listIDs, want) {
			t.Fatalf("call %d: List order = %v, want %v", i, short(listIDs), short(want))
		}
		snapIDs := make([]string, 0, sessions)
		for pos, ev := range m.snapshot() {
			snapIDs = append(snapIDs, ev.ID)
			if ev.Order == nil {
				t.Fatalf("call %d: %s carries no order on the initial sync", i, LogID(ev.ID))
			} else if *ev.Order != pos {
				t.Fatalf("call %d: %s is at snapshot position %d but reports order %d",
					i, LogID(ev.ID), pos, *ev.Order)
			}
		}
		if !slices.Equal(snapIDs, want) {
			t.Fatalf("call %d: snapshot order = %v, want %v", i, short(snapIDs), short(want))
		}
	}
}

// listIDs is the session ids GET /api/sessions serves, in order.
func listIDs(m *SessionManager) []string {
	list := m.List()
	out := make([]string, 0, len(list))
	for _, info := range list {
		out = append(out, info.ID)
	}
	return out
}

// short trims ids to their log prefix so a failure message is readable.
func short(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, LogID(id))
	}
	return out
}

// TestReorderBroadcastsToOtherClients is the read side of sync. SetSessionOrder
// touches no handler, no title and no status, so the sweep's position check is
// the ONLY thing that tells a second client about a reorder made by the first.
// Without it the device that dragged the tab is the only one that sees it move.
//
// Drives diffStatuses directly with the background sweep stopped, so nothing
// races the assertions and no sleep is needed.
func TestReorderBroadcastsToOtherClients(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	m.stopSweep()
	ids := make([]string, 0, 3)
	for range 3 {
		id, err := m.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, id)
	}

	// Drain the first sweep, where every session emits because its status moves
	// off "", then settle: a quiet manager must emit nothing.
	m.diffStatuses()
	if quiet := m.diffStatuses(); len(quiet) != 0 {
		t.Fatalf("a quiet manager emitted %d events, want 0", len(quiet))
	}

	// Move the last session to the front. Its position changes, and so does that
	// of everything it passed.
	if !m.SetSessionOrder([]string{ids[2], ids[0], ids[1]}) {
		t.Fatal("SetSessionOrder = false, want true")
	}
	got := make(map[string]int, 3)
	for _, ev := range m.diffStatuses() {
		if ev.Order == nil {
			t.Fatalf("%s carries no order on a live status event", LogID(ev.ID))
		}
		got[ev.ID] = *ev.Order
	}
	want := map[string]int{ids[2]: 0, ids[0]: 1, ids[1]: 2}
	if len(got) != len(want) {
		t.Fatalf("reorder emitted %d events, want %d (one per moved session)", len(got), len(want))
	}
	for id, pos := range want {
		if got[id] != pos {
			t.Errorf("%s reported order %d, want %d", LogID(id), got[id], pos)
		}
	}

	// And it settles again: a position that has been delivered is not re-emitted
	// every tick.
	if quiet := m.diffStatuses(); len(quiet) != 0 {
		t.Fatalf("the sweep after a reorder emitted %d events, want 0", len(quiet))
	}
}

// TestEventsHandlerInitialSyncIsOrdered pins the property END TO END, over real
// SSE frames: the wire order a client actually reads is the enumeration order,
// not whatever the map produced. The in-process test above cannot catch a future
// writer that re-orders frames between snapshot() and the socket.
//
// This one's ordering check is statistical rather than certain — an unsorted
// enumeration matches the sorted order once in sessions! attempts, so it takes
// repeated connects to be confident. Its deterministic value is the wire shape
// (one decodable frame per session, in enumeration order); the sibling test
// above is the guard against a randomized order.
func TestEventsHandlerInitialSyncIsOrdered(t *testing.T) {
	const (
		sessions = 6
		connects = 5
	)
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	ids := make([]string, 0, sessions)
	for range sessions {
		id, err := m.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, id)
	}
	// Reversed, so the assertion cannot pass on creation order.
	want := slices.Clone(ids)
	slices.Reverse(want)
	if !m.SetSessionOrder(want) {
		t.Fatal("SetSessionOrder = false, want true")
	}

	srv := httptest.NewServer(m.EventsHandler())
	t.Cleanup(srv.Close)
	for c := range connects {
		gotIDs := readInitialSync(t, srv.URL, len(want))
		if !slices.Equal(gotIDs, want) {
			t.Fatalf("connect %d: initial-sync order = %v, want %v", c, short(gotIDs), short(want))
		}
	}
}

// readInitialSync opens one status stream and returns the session ids of its
// first want frames, in wire order. Each frame is DECODED rather than
// substring-matched, so a reordered frame fails the caller's comparison instead
// of passing on "the id appears somewhere in the body".
func readInitialSync(t *testing.T, url string, want int) []string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(newSSEDataReader(t, resp.Body, want))
	ids := make([]string, 0, want)
	for range want {
		var ev statusEvent
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("decode initial-sync frame %d: %v", len(ids), err)
		}
		ids = append(ids, ev.ID)
	}
	return ids
}

// TestSetOrderRoute covers the HTTP surface of the write: the status codes a
// client branches on, and that a success is visible to the next reader.
func TestSetOrderRoute(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	ids := make([]string, 0, 2)
	for range 2 {
		id, err := m.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, id)
	}
	srv := httptest.NewServer(m.RESTHandler())
	t.Cleanup(srv.Close)

	reversed := []string{ids[1], ids[0]}
	// The accepted case has to be seen to CHANGE something, or every later row's
	// "the order still reads reversed" assertion would also pass against an
	// implementation that never applies an order at all.
	if listed := listIDs(m); !slices.Equal(listed, ids) {
		t.Fatalf("before any write, List = %v, want creation order %v",
			short(listed), short(ids))
	}
	cases := []struct {
		name string
		body string
		want int
	}{
		{"the live set reordered", `{"order":["` + ids[1] + `","` + ids[0] + `"]}`, http.StatusNoContent},
		{"a stale view of the set", `{"order":["` + ids[0] + `"]}`, http.StatusConflict},
		{"an id that is not a session", `{"order":["nope","alsonope"]}`, http.StatusConflict},
		// A body with no order array is a malformed request, not an empty reorder:
		// see TestSetOrderRouteRejectsAMalformedEnvelope for why that distinction
		// has to be visible.
		{"no order field", `{}`, http.StatusBadRequest},
		{"not an array", `{"order":"first"}`, http.StatusBadRequest},
		{"not JSON", `<order/>`, http.StatusBadRequest},
		{"empty body", ``, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
				srv.URL+SessionsPath+"/order", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("PUT: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			// Whatever the outcome, the one accepted arrangement is what a reader
			// sees: a refusal must not have applied a partial order.
			if listed := listIDs(m); !slices.Equal(listed, reversed) {
				t.Errorf("List order = %v, want %v", short(listed), short(reversed))
			}
		})
	}
}

// TestSetOrderRouteRejectsAnOversizedBody pins the decode cap. The set check
// refuses a wrong list anyway, so the cap exists to stop a flood being decoded
// before that check can run.
func TestSetOrderRouteRejectsAnOversizedBody(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	if _, err := m.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	srv := httptest.NewServer(m.RESTHandler())
	t.Cleanup(srv.Close)

	// One id per entry, well past maxOrderBodyBytes.
	flood := `{"order":["` + strings.Repeat("a", maxOrderBodyBytes+1) + `"]}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
		srv.URL+SessionsPath+"/order", strings.NewReader(flood))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestCreateEchoesTheStoredCreatedAt pins the 201 body against the value every
// later enumeration reports. handleCreate used to stamp a second time.Now(), so
// the timestamp a client recorded for its newest tab matched neither List nor
// the status stream — which a client that orders tabs by age reads as a
// different session age, and sorts to a different place, until the next
// reconcile replaced the value.
func TestCreateEchoesTheStoredCreatedAt(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	srv := httptest.NewServer(m.RESTHandler())
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+SessionsPath, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var created SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode 201 body: %v", err)
	}

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}
	if created.ID != list[0].ID {
		t.Fatalf("201 id = %s, List id = %s", LogID(created.ID), LogID(list[0].ID))
	}
	if !created.CreatedAt.Equal(list[0].CreatedAt) {
		t.Errorf("201 createdAt = %s, List createdAt = %s (must be the same instant)",
			created.CreatedAt.Format(time.RFC3339Nano), list[0].CreatedAt.Format(time.RFC3339Nano))
	}
	if created.Order != list[0].Order {
		t.Errorf("201 order = %d, List order = %d", created.Order, list[0].Order)
	}
}

// TestCreateEchoesThePositionItAppendedTo is the second-session case, and the one
// a single-session test cannot see: create appends, so the new session's position
// is the END of the order, while a zero value claims position 0 at the FRONT. A
// client that trusts the 201 body would put its new tab first and then watch it
// jump when the next status event corrected it.
func TestCreateEchoesThePositionItAppendedTo(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	srv := httptest.NewServer(m.RESTHandler())
	t.Cleanup(srv.Close)

	for want := range 3 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+SessionsPath, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		var created SessionInfo
		decodeErr := json.NewDecoder(resp.Body).Decode(&created)
		resp.Body.Close()
		if decodeErr != nil {
			t.Fatalf("decode 201 body: %v", decodeErr)
		}
		if created.Order != want {
			t.Errorf("session %d: 201 order = %d, want %d (appended last)", want, created.Order, want)
		}
		// And the enumeration agrees, so the client is not choosing between two
		// answers for the same session.
		list := m.List()
		if got := list[len(list)-1]; got.ID != created.ID || got.Order != want {
			t.Errorf("session %d: List has %s at order %d, want %s at %d",
				want, LogID(got.ID), got.Order, LogID(created.ID), want)
		}
	}
}

// newSSEDataReader adapts an SSE body to a reader of concatenated JSON payloads,
// stopping after want frames so the caller is never blocked on the live stream.
// Keepalive comments and the blank frame separators are dropped.
func newSSEDataReader(t *testing.T, body io.Reader, want int) io.Reader {
	t.Helper()
	var buf strings.Builder
	sc := bufio.NewScanner(body)
	for seen := 0; seen < want && sc.Scan(); {
		payload, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		buf.WriteString(payload)
		buf.WriteByte('\n')
		seen++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read SSE body: %v", err)
	}
	return strings.NewReader(buf.String())
}

// TestShutdownClearsTheOrder is the fourth maintenance site, and the one the
// invariant test above cannot reach: it asserts under the lock rather than
// through a public reader, because every public reader is empty after Shutdown
// whether or not the order was cleared. Without it, deleting `m.order = nil` from
// Shutdown is a silent mutant — and a manager reused after Shutdown then ranks
// live sessions behind the ghosts of dead ones.
func TestShutdownClearsTheOrder(t *testing.T) {
	m := NewSessionManager(catFactory)
	first, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !m.SetSessionOrder([]string{second, first}) {
		t.Fatal("SetSessionOrder = false, want true")
	}

	m.Shutdown()
	if got := m.orderSnapshot(); len(got) != 0 {
		t.Fatalf("order after Shutdown = %v, want empty", short(got))
	}

	// A session created after Shutdown must be position 0, not queued behind the
	// dead ids.
	revived, err := m.Create()
	if err != nil {
		t.Fatalf("Create after Shutdown: %v", err)
	}
	t.Cleanup(m.Shutdown)
	list := m.List()
	if len(list) != 1 || list[0].ID != revived || list[0].Order != 0 {
		t.Fatalf("List after Shutdown+Create = %+v, want one session at order 0", list)
	}
}

// TestSweepReadsTheOrderAfterPhaseTwo is the regression guard for the sweep's
// lock split. Phase 2 runs lock-free and can block on a wedged handler getter, so
// the display order can change while a sweep is in flight. Reading the order in
// phase 1 meant the sweep emitted a position the server had ALREADY replaced, and
// silently: the tracker recorded the stale value as delivered, so the next sweep
// saw no change and never corrected it. Clients were left on an arrangement no
// longer in force.
//
// Driven through the testDiffPhaseHold seam, which holds one real sweep open at
// exactly that instant, so this is deterministic rather than a race the test hopes
// to hit.
func TestSweepReadsTheOrderAfterPhaseTwo(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	m.stopSweep()
	ids := make([]string, 0, 3)
	for range 3 {
		id, err := m.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, id)
	}
	// Settle: drain the first sweep (every session emits, its status moving off
	// ""), then confirm a quiet manager emits nothing.
	m.diffStatuses()
	if quiet := m.diffStatuses(); len(quiet) != 0 {
		t.Fatalf("a quiet manager emitted %d events, want 0", len(quiet))
	}

	// Reorder from INSIDE the sweep, between phase 2 and phase 3, and fire once so
	// the hold cannot re-enter through the diffStatuses it triggers.
	want := []string{ids[2], ids[0], ids[1]}
	// Atomic, and a CAS rather than a plain bool: the seam is a package global, so
	// any other manager's 250ms sweep in this process can enter it. That cannot
	// happen today (every test manager gets a Shutdown, and this test is serial),
	// but the failure mode if it ever does is the worst kind — the reorder lands
	// outside the phase-2 window and the test passes while testing nothing.
	var fired atomic.Bool
	hold := func() {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		if !m.SetSessionOrder(want) {
			t.Error("SetSessionOrder from inside the sweep = false, want true")
		}
	}
	testDiffPhaseHold.Store(&hold)
	t.Cleanup(func() { testDiffPhaseHold.Store(nil) })

	got := make(map[string]int, len(want))
	for _, ev := range m.diffStatuses() {
		if ev.Order == nil {
			t.Fatalf("%s carries no order on a live status event", LogID(ev.ID))
		}
		got[ev.ID] = *ev.Order
	}
	if !fired.Load() {
		t.Fatal("the phase hold never ran; the seam is not wired")
	}
	for pos, id := range want {
		if got[id] != pos {
			t.Errorf("%s reported order %d, want %d (the order in force at phase 3)",
				LogID(id), got[id], pos)
		}
	}
	// And the arrangement is now settled: a stale emission would have recorded the
	// old position as delivered, so this second sweep would report the correction
	// the first one owed.
	if quiet := m.diffStatuses(); len(quiet) != 0 {
		t.Errorf("the sweep after the reorder emitted %d events, want 0 "+
			"(a stale position was delivered and is being corrected late)", len(quiet))
	}
}

// TestStatusEventOrderOnTheWire pins the JSON, not the struct. A live session at
// the front reports order 0, and a removed session reports no order at all: it has
// left the arrangement, and 0 is a real position, so a consumer that reads fields
// before it checks `removed` would otherwise be told the closing session just
// became the first tab.
func TestStatusEventOrderOnTheWire(t *testing.T) {
	front := 0
	live, err := json.Marshal(statusEvent{ID: "s1", Status: StatusIdle, Order: &front})
	if err != nil {
		t.Fatalf("marshal live: %v", err)
	}
	if !strings.Contains(string(live), `"order":0`) {
		t.Errorf("live front event = %s, want it to carry \"order\":0", live)
	}

	gone, err := json.Marshal(statusEvent{ID: "s1", Status: StatusExited, Removed: true})
	if err != nil {
		t.Fatalf("marshal removed: %v", err)
	}
	if strings.Contains(string(gone), `"order"`) {
		t.Errorf("removed event = %s, want no order field", gone)
	}
	// The removal marker itself must survive, or the test above would pass on an
	// event that says nothing at all.
	if !strings.Contains(string(gone), `"removed":true`) {
		t.Errorf("removed event = %s, want \"removed\":true", gone)
	}
}

// TestSetOrderRouteRejectsAMalformedEnvelope covers the shapes a client bug
// produces rather than a stale view. Each must be a 400 the caller can see, not a
// 204 that reports success for a request the server did not understand — which is
// what an absent list did against an empty session set, and what a second JSON
// value did against any set.
func TestSetOrderRouteRejectsAMalformedEnvelope(t *testing.T) {
	cases := []struct {
		name     string
		sessions int
		body     string
		want     int
	}{
		// No sessions live, so an absent list would match the empty set by accident
		// and answer 204. This is the case that makes the pointer necessary.
		{"no order field, no sessions", 0, `{}`, http.StatusBadRequest},
		{"explicit null, no sessions", 0, `{"order":null}`, http.StatusBadRequest},
		{"an honest empty reorder", 0, `{"order":[]}`, http.StatusNoContent},
		{"two bodies in one request", 1, `{"order":[]}{"order":[]}`, http.StatusBadRequest},
		{"trailing garbage", 1, `{"order":[]} nope`, http.StatusBadRequest},
		{"an array, not an object", 1, `["a"]`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewSessionManager(catFactory)
			t.Cleanup(m.Shutdown)
			for range tc.sessions {
				if _, err := m.Create(); err != nil {
					t.Fatalf("Create: %v", err)
				}
			}
			srv := httptest.NewServer(m.RESTHandler())
			t.Cleanup(srv.Close)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
				srv.URL+SessionsPath+"/order", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("PUT: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// TestListPublishesDensePositions pins the wire contract independently of the
// manager's internal state: whatever rank the order list yields, what a client
// reads is 0-based, dense and unique. Driven by forcing the invariant violation
// the fallback exists for — two live sessions the order does not name — which
// share a raw rank and would otherwise both claim the same position.
func TestListPublishesDensePositions(t *testing.T) {
	m := NewSessionManager(catFactory)
	t.Cleanup(m.Shutdown)
	ids := make([]string, 0, 3)
	for range 3 {
		id, err := m.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, id)
	}
	// Reorder so the session that KEEPS its position is the youngest, not the
	// oldest. That is what makes the fixture able to tell a stray ranked past the
	// end from a stray ranked at 0: with the survivor also being the oldest, both
	// rules produce the same sequence and the difference is invisible.
	if !m.SetSessionOrder([]string{ids[2], ids[0], ids[1]}) {
		t.Fatal("SetSessionOrder = false, want true")
	}
	// Strip two sessions out of the order, leaving them live. Only a bug could do
	// this; the point is that the published field survives it.
	m.mu.Lock()
	m.order = m.order[:1]
	ordered := m.order[0]
	m.mu.Unlock()
	if ordered != ids[2] {
		t.Fatalf("fixture kept %s in the order, want the youngest session %s",
			LogID(ordered), LogID(ids[2]))
	}

	// BOTH enumerations, because they renumber independently and each one is the
	// sole source for some client. Dropping snapshot's renumbering alone left the
	// whole suite green until this loop covered it.
	listIDs := make([]string, 0, 3)
	listPos := make(map[int]string, 3)
	for i, info := range m.List() {
		if info.Order != i {
			t.Errorf("List: %s is at sequence position %d but reports order %d",
				LogID(info.ID), i, info.Order)
		}
		if other, dup := listPos[info.Order]; dup {
			t.Errorf("List: %s and %s both report order %d",
				LogID(other), LogID(info.ID), info.Order)
		}
		listPos[info.Order] = info.ID
		listIDs = append(listIDs, info.ID)
	}
	if len(listPos) != 3 {
		t.Errorf("List reported %d distinct positions, want 3", len(listPos))
	}

	snapIDs := make([]string, 0, 3)
	snapPos := make(map[int]string, 3)
	for i, ev := range m.snapshot() {
		if ev.Order == nil {
			t.Fatalf("snapshot: %s carries no order", LogID(ev.ID))
		}
		if *ev.Order != i {
			t.Errorf("snapshot: %s is at sequence position %d but reports order %d",
				LogID(ev.ID), i, *ev.Order)
		}
		if other, dup := snapPos[*ev.Order]; dup {
			t.Errorf("snapshot: %s and %s both report order %d",
				LogID(other), LogID(ev.ID), *ev.Order)
		}
		snapPos[*ev.Order] = ev.ID
		snapIDs = append(snapIDs, ev.ID)
	}
	if len(snapPos) != 3 {
		t.Errorf("snapshot reported %d distinct positions, want 3", len(snapPos))
	}

	// The one session the order still names must lead both sequences. This is what
	// pins rankOf's "past the end" argument at its call sites: ranking a stray at 0
	// instead would let it tie with the ordered session, and the renumbering would
	// then hide the collision behind a clean-looking dense sequence.
	if len(listIDs) != 0 && listIDs[0] != ordered {
		t.Errorf("List leads with %s, want the still-ordered session %s",
			LogID(listIDs[0]), LogID(ordered))
	}
	if len(snapIDs) != 0 && snapIDs[0] != ordered {
		t.Errorf("snapshot leads with %s, want the still-ordered session %s",
			LogID(snapIDs[0]), LogID(ordered))
	}
	if !slices.Equal(listIDs, snapIDs) {
		t.Errorf("the two enumerations disagree under a desync: %v vs %v",
			short(listIDs), short(snapIDs))
	}
}
