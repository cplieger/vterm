package terminal

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/web-terminal-engine/v4/vt"
)

// Tests for demand-paged scrollback (docs/paged-scrollback.md): the bounded
// ring accessor, the per-row URI-strip ceiling, the `history` control's
// validation and intersection serve, the per-socket history bucket, the
// resumeAck capability bit, and the bounded resume replay.

// row builds a single-run row of plain text (local helper; the golden test's
// own `row` lives in wire_golden_test.go and is not shared to keep each file
// readable on its own).
func plainRow(text string) []vt.WireRun {
	return []vt.WireRun{{T: text, F: -1, B: -1, Uc: -1}}
}

// fillRing appends n lines to a handler's ring, each identifiable by index.
func fillRing(h *Handler, n int) {
	lines := make([][]vt.WireRun, n)
	for i := range lines {
		lines[i] = plainRow(fmt.Sprintf("line-%d", i))
	}
	h.scrollback.Append(lines)
}

// decodeScroll pulls firstIndex and numLines out of a scroll frame.
func decodeScroll(t *testing.T, frame []byte) (firstIndex uint64, numLines int) {
	t.Helper()
	if len(frame) < encodedScrollHeaderSize {
		t.Fatalf("scroll frame too short: %d bytes, want >= %d", len(frame), encodedScrollHeaderSize)
	}
	if frame[0] != wireMsgScroll {
		t.Fatalf("frame type %d, want scroll (%d)", frame[0], wireMsgScroll)
	}
	firstIndex = binary.LittleEndian.Uint64(frame[9:17])
	numLines = int(binary.LittleEndian.Uint16(frame[17:19]))
	return firstIndex, numLines
}

// TestLinesRange_clampBoundEmpty pins the bounded accessor's three behaviors
// against LinesFrom's: it clamps a too-old start up to the retained edge (so
// the caller can still detect an eviction gap), it never returns more than the
// requested count, and it returns nothing outside the retained range.
func TestLinesRange_clampBoundEmpty(t *testing.T) {
	h := NewHandler([]string{"/bin/true"}, WithScrollbackCapacity(10), WithLogger(nil))
	fillRing(h, 25) // committed 25, retains [15, 25)

	tests := []struct {
		name         string
		abs          uint64
		maxLines     int
		wantFirstAbs uint64
		wantCount    int
	}{
		{"exact window inside the retained range", 18, 3, 18, 3},
		{"bound is respected below availability", 15, 2, 15, 2},
		{"count clamps to what remains at the tail", 23, 10, 23, 2},
		{"start below the retained edge clamps up", 0, 4, 15, 4},
		{"at committed returns nothing", 25, 5, 25, 0},
		{"beyond committed returns nothing", 99, 5, 25, 0},
		{"zero maxLines returns nothing", 18, 0, 25, 0},
		{"negative maxLines returns nothing", 18, -3, 25, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			firstAbs, lines := h.scrollback.LinesRange(tc.abs, tc.maxLines)
			if firstAbs != tc.wantFirstAbs {
				t.Errorf("firstAbs = %d, want %d", firstAbs, tc.wantFirstAbs)
			}
			if len(lines) != tc.wantCount {
				t.Errorf("len(lines) = %d, want %d", len(lines), tc.wantCount)
			}
		})
	}
}

// TestLinesRange_matchesLinesFromWhenUnbounded pins the sibling relationship:
// with a bound at or above what LinesFrom would return, the two agree exactly.
// A divergence here means the bounded path drifted from the shipped one.
func TestLinesRange_matchesLinesFromWhenUnbounded(t *testing.T) {
	h := NewHandler([]string{"/bin/true"}, WithScrollbackCapacity(50), WithLogger(nil))
	fillRing(h, 40)

	for _, abs := range []uint64{0, 1, 17, 39, 40} {
		t.Run(fmt.Sprintf("abs=%d", abs), func(t *testing.T) {
			wantFirst, want := h.scrollback.LinesFrom(abs)
			gotFirst, got := h.scrollback.LinesRange(abs, 1000)
			if gotFirst != wantFirst || len(got) != len(want) {
				t.Errorf("LinesRange(%d, 1000) = (%d, %d lines), LinesFrom(%d) = (%d, %d lines)",
					abs, gotFirst, len(got), abs, wantFirst, len(want))
			}
		})
	}
}

// oversizedRow builds a row whose encoding exceeds rowByteCeiling by giving
// every run a maximal OSC 8 URI. It returns the row and its pre-strip size.
func oversizedRow(runs int) []vt.WireRun {
	uri := "https://example.com/" + strings.Repeat("u", 4073) // ~4093 bytes, the maxOSCLen cap
	out := make([]vt.WireRun, runs)
	for i := range out {
		out[i] = vt.WireRun{T: "x", U: uri, F: -1, B: -1, Uc: -1, A: vt.AttrAutolink}
	}
	return out
}

// TestRowCeiling_stripsOversizedRowsOnEveryPath is the ceiling's core contract:
// a row too big for a budgeted reply is re-encoded with URIs stripped, and the
// decision is made in the encoder every row-emitting path shares, so a page, a
// live scroll frame, and a screen frame all agree byte-for-byte on the same
// committed row. Without that agreement the client's idempotence breaks: the
// same absolute index would arrive with different bytes depending on path.
func TestRowCeiling_stripsOversizedRowsOnEveryPath(t *testing.T) {
	big := oversizedRow(100)
	if pre := encodedRowSize(big); pre <= rowByteCeiling {
		t.Fatalf("fixture is not oversized: %d bytes, want > %d", pre, rowByteCeiling)
	}

	scroll := encodeScrollMsg(0, 7, [][]vt.WireRun{big})
	screen := encodeScreenMsg(7, 1, 0, 0, 0, []int{0}, [][]vt.WireRun{big}, 0, false, false, false, false, false)

	// The row payload is the tail of each frame; both must carry the SAME
	// stripped row bytes.
	scrollRow := scroll[encodedScrollHeaderSize:]
	screenRow := screen[27+2:] // screen header (27) + the 2-byte row index
	if string(scrollRow) != string(screenRow) {
		t.Errorf("scroll and screen encoded the same row differently (%d vs %d bytes); the ceiling must live in the shared encoder",
			len(scrollRow), len(screenRow))
	}
	if len(scroll) > pageByteBudget {
		t.Errorf("one-row scroll reply is %d bytes, want <= pageByteBudget (%d)", len(scroll), pageByteBudget)
	}
}

// TestRowCeiling_headerInclusiveBoundary pins the ceiling's exact edge, in the
// units it is expressed in: encodedRowSize INCLUDES the 2-byte run count, so a
// row measuring exactly rowByteCeiling passes untouched and one byte larger is
// stripped. An off-by-one here is invisible on ordinary rows and only shows on
// the pathological one the ceiling exists for.
func TestRowCeiling_headerInclusiveBoundary(t *testing.T) {
	const uri = "https://example.com/x"
	// Several runs, because each text/URI field is capped at 0xFFFF bytes by
	// the encoder — one run cannot reach the ceiling at all.
	const runCount = 4
	rowOfSize := func(size int) []vt.WireRun {
		textTotal := size - encodedRowCountSize - runCount*(encodedRunFixedSize+len(uri))
		if textTotal < 0 {
			t.Fatalf("size %d is smaller than %d runs of overhead", size, runCount)
		}
		out := make([]vt.WireRun, runCount)
		per := textTotal / runCount
		for i := range out {
			n := per
			if i == runCount-1 {
				n = textTotal - per*(runCount-1) // the remainder lands on the last run
			}
			if n > 0xFFFF {
				t.Fatalf("run text %d exceeds the encoder's 0xFFFF field cap", n)
			}
			// Every run carries a URI, so stripping is observable on all of them.
			out[i] = vt.WireRun{T: strings.Repeat("a", n), U: uri, A: vt.AttrAutolink}
		}
		return out
	}

	atCeiling := rowOfSize(rowByteCeiling)
	if got := encodedRowSize(atCeiling); got != rowByteCeiling {
		t.Fatalf("fixture size = %d, want exactly %d", got, rowByteCeiling)
	}
	if capped := capRowRuns(atCeiling); capped[0].U == "" {
		t.Errorf("a row measuring exactly rowByteCeiling (%d) was stripped; the ceiling is inclusive", rowByteCeiling)
	}

	overCeiling := rowOfSize(rowByteCeiling + 1)
	if got := encodedRowSize(overCeiling); got != rowByteCeiling+1 {
		t.Fatalf("fixture size = %d, want exactly %d", got, rowByteCeiling+1)
	}
	capped := capRowRuns(overCeiling)
	for i, run := range capped {
		if run.U != "" {
			t.Errorf("run %d of a row one byte over the ceiling kept its URI", i)
		}
		if run.A&vt.AttrAutolink != 0 {
			t.Errorf("run %d of a stripped row kept the autolink bit", i)
		}
	}
	// And the stripped result fits the budget the ceiling was derived from.
	if size := encodedScrollHeaderSize + encodedRowSize(capped); size > pageByteBudget {
		t.Errorf("stripped one-row reply is %d bytes, want <= pageByteBudget (%d)", size, pageByteBudget)
	}
}

func TestStripRowURIs_clearsAutolinkBit(t *testing.T) {
	in := []vt.WireRun{
		{T: "a", U: "https://example.com", A: vt.AttrAutolink | 1},
		{T: "b", U: "https://other.example", A: 4},
	}
	got := stripRowURIs(in)

	for i, run := range got {
		if run.U != "" {
			t.Errorf("run %d: U = %q, want empty", i, run.U)
		}
		if run.A&vt.AttrAutolink != 0 {
			t.Errorf("run %d: autolink bit still set (A = %d)", i, run.A)
		}
	}
	// Styling other than the autolink bit survives, and the input is untouched.
	if got[0].A != 1 || got[1].A != 4 {
		t.Errorf("stripping altered SGR attrs: got %d, %d; want 1, 4", got[0].A, got[1].A)
	}
	if in[0].U == "" {
		t.Error("stripRowURIs mutated its input; it must return a copy")
	}
}

// TestEncodedRowSize_matchesEncoder pins the helper against the encoder it
// exists to predict. The page-packing arithmetic, the ceiling, and the tests
// all read this one helper precisely so they cannot drift; if the encoder gains
// a field and the helper does not, this fails.
func TestEncodedRowSize_matchesEncoder(t *testing.T) {
	tests := map[string][]vt.WireRun{
		"empty":            {},
		"one plain run":    plainRow("hello"),
		"multibyte text":   {{T: "héllo→", F: -1, B: -1, Uc: -1}},
		"with a hyperlink": {{T: "click", U: "https://example.com/x", F: 1, B: 2, Uc: 3, A: 4}},
		"many runs":        oversizedRow(3),
	}
	for name, runs := range tests {
		t.Run(name, func(t *testing.T) {
			want := len(appendRowRuns(nil, runs))
			if got := encodedRowSize(capRowRuns(runs)); got != want {
				t.Errorf("encodedRowSize = %d, encoder wrote %d bytes", got, want)
			}
		})
	}
}

// TestShrinkToBudget_keepsPrefixAndAtLeastOne pins both halves of the shrink
// rule. The direction is load-bearing: keeping a SUFFIX would move the reply's
// firstIndex above the request's fromAbs, which the protocol defines as the
// permanent-trim signal, so a merely-styled page would paint a false "history
// trimmed" marker on the client.
func TestShrinkToBudget_keepsPrefixAndAtLeastOne(t *testing.T) {
	small := plainRow("small")
	huge := oversizedRow(100) // stripped by the ceiling, still ~2 KB

	t.Run("everything fits", func(t *testing.T) {
		lines := [][]vt.WireRun{small, small, small}
		if got := shrinkToBudget(lines); got != 3 {
			t.Errorf("shrinkToBudget = %d, want 3 (all fit)", got)
		}
	})

	t.Run("a single oversized row still serves one line", func(t *testing.T) {
		if got := shrinkToBudget([][]vt.WireRun{huge}); got != 1 {
			t.Errorf("shrinkToBudget = %d, want 1; a reply must never be empty for want of budget", got)
		}
	})

	t.Run("the kept lines are a prefix", func(t *testing.T) {
		// Enough maximal rows that the budget must cut somewhere in the middle.
		wide := make([][]vt.WireRun, 0, 400)
		for range 400 {
			wide = append(wide, oversizedRow(60))
		}
		n := shrinkToBudget(wide)
		if n <= 0 || n >= len(wide) {
			t.Fatalf("shrinkToBudget = %d, want a partial cut in 1..%d", n, len(wide)-1)
		}
		if size := len(encodeScrollMsg(0, 0, wide[:n])); size > pageByteBudget {
			t.Errorf("kept %d lines encoding to %d bytes, over budget %d", n, size, pageByteBudget)
		}
		if size := len(encodeScrollMsg(0, 0, wide[:n+1])); size <= pageByteBudget {
			t.Errorf("kept only %d lines but %d also fit (%d bytes); the cut is too early", n, n+1, size)
		}
	})
}

// TestParseReplayMax pins the advisory contract. The field must never be able
// to cost a client its resume: the handler drops any control whose unmarshal
// errors, so a malformed value has to read as absent rather than propagating an
// error, and the clamp has to mirror the client's own pre-send clamp so the
// sent value and the honored value are the same number.
func TestParseReplayMax(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantValue int64 // meaningful only when wantBound is true
		wantBound bool
	}{
		{"absent", "", 0, false},
		{"null", "null", 0, false},
		{"zero is out of domain", "0", 0, false},
		{"negative is out of domain", "-5", 0, false},
		{"fractional is malformed", "3.5", 0, false},
		{"string is malformed", `"1500"`, 0, false},
		{"overflowing is malformed", "1e300", 0, false},
		{"valid passes through", "1463", 1463, true},
		{"at the clamp", fmt.Sprint(maxReplayLines), maxReplayLines, true},
		{"above the clamp is clamped down", "9963", maxReplayLines, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseReplayMax(json.RawMessage(tc.raw))
			switch {
			case !tc.wantBound && got != nil:
				t.Errorf("parseReplayMax(%q) = %d, want nil (absent/full replay)", tc.raw, *got)
			case tc.wantBound && got == nil:
				t.Errorf("parseReplayMax(%q) = nil, want %d", tc.raw, tc.wantValue)
			case tc.wantBound && *got != tc.wantValue:
				t.Errorf("parseReplayMax(%q) = %d, want %d", tc.raw, *got, tc.wantValue)
			}
		})
	}
}

// TestHistoryPagingDeclared_ringDepthGate pins the depth condition. The bit
// invites the client to shrink its resident tail, and what bounds that
// regression is client-side accumulation — so a ring shallower than the LEGACY
// CLIENT default must not invite the flip, or reachable history is cut with no
// paging to compensate. The boundary pair is asserted against the constant, not
// a literal.
func TestHistoryPagingDeclared_ringDepthGate(t *testing.T) {
	tests := []struct {
		capacity int
		want     bool
	}{
		{0, false},
		{1000, false}, // the engine default BEFORE 2026-08; now only a deliberate choice
		{paginationMinRing - 1, false},
		{paginationMinRing, true},
		{defaultScrollbackCapacity, true}, // the default: paging is declared unless a consumer opts down
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("capacity=%d", tc.capacity), func(t *testing.T) {
			h := NewHandler([]string{"/bin/true"}, WithScrollbackCapacity(tc.capacity), WithLogger(nil))
			if got := h.historyPagingDeclared(); got != tc.want {
				t.Errorf("historyPagingDeclared() = %v, want %v at capacity %d", got, tc.want, tc.capacity)
			}
		})
	}
}

// TestResumeAck_historyPagingBit pins the capability signal end to end: the
// resumeAck's ackFlags carry bit1 exactly when the ring is deep enough, and
// independently of bit0 (ledgerLost). A client that read the wrong bit would
// either page against a server that cannot serve it or never page at all.
func TestResumeAck_historyPagingBit(t *testing.T) {
	const flagsOffset = 34 // 1 type + 8 ack + 8 epoch + 8 committed + 8 oldest + 1 version

	tests := []struct {
		name     string
		capacity int
		wantBit  bool
	}{
		{"deep ring declares paging", 20000, true},
		{"default ring does not", 1000, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler([]string{"/bin/cat"}, WithScrollbackCapacity(tc.capacity), WithLogger(nil))
			h.screen = vt.New(3, 20)
			server, client, cleanup := dualConn(t)
			defer cleanup()

			h.handleResume(server, &clientState{}, "sid", -1, 0, nil)
			frames := readServerFrames(t, client, 300*time.Millisecond)

			var ack []byte
			for _, f := range frames {
				if len(f) > flagsOffset && f[0] == wireMsgResumeAck {
					ack = f
					break
				}
			}
			if ack == nil {
				t.Fatalf("no resumeAck frame with the length-gated tail in %d frames", len(frames))
			}
			gotBit := ack[flagsOffset]&resumeAckFlagHistoryPaging != 0
			if gotBit != tc.wantBit {
				t.Errorf("historyPaging bit = %v, want %v (ackFlags = %d)", gotBit, tc.wantBit, ack[flagsOffset])
			}
			if ack[flagsOffset]&resumeAckFlagLedgerLost != 0 {
				t.Errorf("ledgerLost bit set on an intact-ledger resume (ackFlags = %d)", ack[flagsOffset])
			}
		})
	}
}

// TestPagingDeclaredWhereverReplayTruncates pins the invariant that couples the
// two bounds: the resume replay is clamped UNCONDITIONALLY, so a ring the clamp
// can truncate must declare paging or the withheld rows are unreachable for the
// life of the session — the client has no other way to ask for them.
//
// The two constants were independent and left a window where that failed: the
// threshold was 5000 and the bound 2000, so a ring configured anywhere in
// 2001..4999 replayed its newest 2000 lines, cleared the paging bit, and stranded
// the rest in the authoritative ring. Deriving the threshold closes the window;
// this test is what keeps it closed if either number is edited again.
func TestPagingDeclaredWhereverReplayTruncates(t *testing.T) {
	// One line past the bound is the tightest case: the replay withholds exactly
	// one row, and that row must still be requestable.
	for _, capacity := range []int{maxReplayLines + 1, maxReplayLines + 999, 20000, 100000} {
		h := NewHandler([]string{"/bin/cat"}, WithScrollbackCapacity(capacity), WithLogger(nil))
		if !h.historyPagingDeclared() {
			t.Errorf("capacity %d: replay truncates at %d but paging is not declared, so the withheld rows are unreachable",
				capacity, maxReplayLines)
		}
	}
	// At or below the bound the replay carries the whole ring, so there is nothing
	// withheld and nothing to page for. Not declaring is then a free choice, and
	// the one the design makes (a shallow-ring server must not invite the client's
	// tail flip).
	for _, capacity := range []int{0, 1, 500, maxReplayLines} {
		h := NewHandler([]string{"/bin/cat"}, WithScrollbackCapacity(capacity), WithLogger(nil))
		if h.historyPagingDeclared() {
			t.Errorf("capacity %d: nothing is withheld at or below the replay bound, so paging should not be declared", capacity)
		}
	}
}

// TestHandleResume_boundedReplay pins the replay clamp. The point is that ring
// DEPTH costs the client nothing at attach: however deep the ring, a paging
// pairing downloads at most replayMax lines. The unsupported case is the
// safety half — a client that sent the field optimistically to a server that
// does not declare paging must still get its full backfill.
func TestHandleResume_boundedReplay(t *testing.T) {
	tests := []struct {
		name        string
		capacity    int
		haveThrough int64
		replayMax   int64 // 0 means the field is absent (full replay)
		wantLines   int
		wantFirst   uint64
	}{
		// The bound is UNCONDITIONAL: a client that asks for nothing still gets
		// at most maxReplayLines, because the ring is an operator-sized number
		// (100k by default) and streaming all of it is tens of megabytes written
		// under one lock. This case used to assert the opposite ("replays
		// everything retained"), which was safe only while rings were small.
		{"an absent bound is still bounded", 20000, -1, 0, maxReplayLines, 3000 - maxReplayLines},
		{"a client may ask for less", 20000, -1, 1463, 1463, 3000 - 1463},
		// Nor is the clamp gated on the server declaring paging: a shallow ring
		// bounds its replay too. Such a ring holds LESS than the bound, so nothing
		// is withheld and it needs no pages to backfill from — which is exactly
		// why paginationMinRing is derived from maxReplayLines rather than chosen
		// (see the invariant test below).
		{"the bound applies on a shallow ring too", 1000, -1, 500, 500, 2500},
		{"a shallow ring bounds an unbounded request at its own depth", 1000, -1, 0, 1000, 2000},
		{"bound never withholds what the client is missing anyway", 20000, 2990, 1463, 9, 2991},
		{"committed below the bound leaves the start alone", 20000, -1, maxReplayLines, 2000, 1000},
		// The half-open count: replayFrom is the FIRST index to send and
		// committed is one PAST the last, so a client that is exactly current
		// receives nothing rather than one line or a negative count.
		{"a client exactly current receives nothing", 20000, 2999, 0, 0, 0},
		{"a client claiming more than committed receives nothing", 20000, 5000, 0, 0, 0},
		{"a client claiming more than committed, with a bound, receives nothing", 20000, 5000, 1463, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler([]string{"/bin/cat"}, WithScrollbackCapacity(tc.capacity), WithLogger(nil))
			h.screen = vt.New(3, 20)
			fillRing(h, 3000)

			server, client, cleanup := dualConn(t)
			defer cleanup()
			var replayMax *int64
			if tc.replayMax != 0 {
				bound := tc.replayMax
				replayMax = &bound
			}
			h.handleResume(server, &clientState{}, "sid", tc.haveThrough, 0, replayMax)
			frames := readServerFrames(t, client, 400*time.Millisecond)

			total := 0
			var first uint64
			seen := false
			for _, f := range frames {
				if len(f) == 0 || f[0] != wireMsgScroll {
					continue
				}
				idx, n := decodeScroll(t, f)
				if !seen {
					first, seen = idx, true
				}
				total += n
			}
			if total != tc.wantLines {
				t.Errorf("replayed %d lines, want %d", total, tc.wantLines)
			}
			if seen && first != tc.wantFirst {
				t.Errorf("replay started at index %d, want %d", first, tc.wantFirst)
			}
		})
	}
}

// TestHistoryControl_servesIntersection is the read path's core contract: the
// reply is the intersection of the request window and the retained range —
// never lines the client did not ask for — so every non-empty reply's
// firstIndex lands inside the request window and the client's correlation
// always succeeds. The empty case is the other half: it carries the REQUEST's
// own fromAbs, because the alternative (the accessor's empty-case value,
// `committed`) is an index far outside the window and would fail correlation.
func TestHistoryControl_servesIntersection(t *testing.T) {
	const ringCap = 5000
	if ringCap < MinPagingCapacity {
		t.Fatalf("fixture needs a paging-declaring ring: %d < %d", ringCap, MinPagingCapacity)
	}
	// A ring of ringCap filled past capacity, so retention is the newest ringCap
	// of 8000 committed lines: [3000, 8000). Deliberately a LITERAL rather than
	// paginationMinRing — the expectations below are arithmetic on the retained
	// edge, so borrowing the threshold constant made them silently wrong the day
	// that constant moved (it did: it is now derived from maxReplayLines).
	// deepEnoughForPaging below keeps the fixture's intent checked instead.
	tests := []struct {
		name      string
		fromAbs   int64
		maxLines  int64
		wantFirst uint64
		wantLines int
	}{
		{"fully inside the retained range", 4000, 100, 4000, 100},
		{"clamped up to the retained edge", 2500, 1000, 3000, 500},
		{"truncated at committed", 7950, 100, 7950, 50},
		{"entirely below the retained range is empty", 0, 100, 0, 0},
		{"at committed is empty", 8000, 10, 8000, 0},
		{"beyond committed is empty", 9000, 10, 9000, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler([]string{"/bin/cat"}, WithScrollbackCapacity(ringCap), WithLogger(nil))
			h.screen = vt.New(3, 20)
			fillRing(h, 8000)

			server, client, cleanup := dualConn(t)
			defer cleanup()
			state := &clientState{}
			state.session.Store(&sessionState{})

			h.historyControl(server, state, &controlMsg{
				Type: ctlTypeHistory, FromAbs: tc.fromAbs, MaxLines: tc.maxLines,
			})
			frames := readServerFrames(t, client, 300*time.Millisecond)
			if len(frames) != 1 {
				t.Fatalf("got %d frames, want exactly 1 scroll reply", len(frames))
			}
			first, n := decodeScroll(t, frames[0])
			if first != tc.wantFirst {
				t.Errorf("firstIndex = %d, want %d", first, tc.wantFirst)
			}
			if n != tc.wantLines {
				t.Errorf("numLines = %d, want %d", n, tc.wantLines)
			}
			// Never lines outside the request window.
			if n > 0 {
				end := uint64(tc.fromAbs) + uint64(tc.maxLines)
				if first < uint64(tc.fromAbs) || first >= end {
					t.Errorf("firstIndex %d outside the request window [%d, %d)", first, tc.fromAbs, end)
				}
				if first+uint64(n) > end {
					t.Errorf("reply [%d, %d) crosses the request end %d", first, first+uint64(n), end)
				}
			}
		})
	}
}

// TestHistoryControl_rejectsBadRequests pins the validation gates. Each shape
// must produce NO reply at all: a malformed or unserviceable request is dropped
// rather than answered, so nothing can be mistaken for a correlated page.
func TestHistoryControl_rejectsBadRequests(t *testing.T) {
	tests := []struct {
		name     string
		fromAbs  int64
		maxLines int64
	}{
		{"zero maxLines", 100, 0},
		{"negative maxLines", 100, -1},
		{"maxLines above the page size", 100, historyPageSize + 1},
		{"negative fromAbs", -1, 10},
		{"fromAbs at the safe-integer ceiling overflows the window", maxSafeInteger, 10},
		{"fromAbs just above the subtraction bound", maxSafeInteger - 9, 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler([]string{"/bin/cat"}, WithScrollbackCapacity(paginationMinRing), WithLogger(nil))
			h.screen = vt.New(3, 20)
			fillRing(h, 100)

			server, client, cleanup := dualConn(t)
			defer cleanup()
			state := &clientState{}
			state.session.Store(&sessionState{})

			h.historyControl(server, state, &controlMsg{
				Type: ctlTypeHistory, FromAbs: tc.fromAbs, MaxLines: tc.maxLines,
			})
			if frames := readServerFrames(t, client, 200*time.Millisecond); len(frames) != 0 {
				t.Errorf("got %d frames for an invalid request; want none", len(frames))
			}
		})
	}

	t.Run("the exact subtraction boundary is accepted", func(t *testing.T) {
		h := NewHandler([]string{"/bin/cat"}, WithScrollbackCapacity(paginationMinRing), WithLogger(nil))
		h.screen = vt.New(3, 20)
		fillRing(h, 100)

		server, client, cleanup := dualConn(t)
		defer cleanup()
		state := &clientState{}
		state.session.Store(&sessionState{})

		// fromAbs + maxLines == maxSafeInteger exactly: the largest legal
		// request. It serves nothing (far above committed) but must be
		// ANSWERED, which is what distinguishes the boundary from an overflow.
		h.historyControl(server, state, &controlMsg{
			Type: ctlTypeHistory, FromAbs: maxSafeInteger - 10, MaxLines: 10,
		})
		if frames := readServerFrames(t, client, 200*time.Millisecond); len(frames) != 1 {
			t.Errorf("got %d frames at the exact boundary; want 1 (empty reply)", len(frames))
		}
	})
}

// TestHistoryControl_gates pins the two preconditions that are not about the
// request's own shape: a socket that has not resumed has no session to answer
// for, and a server whose ring is too shallow never advertised paging, so a
// request against it is a stale-bundle artifact rather than something to serve.
func TestHistoryControl_gates(t *testing.T) {
	t.Run("ignored before the socket's first resume", func(t *testing.T) {
		h := NewHandler([]string{"/bin/cat"}, WithScrollbackCapacity(paginationMinRing), WithLogger(nil))
		h.screen = vt.New(3, 20)
		fillRing(h, 100)

		server, client, cleanup := dualConn(t)
		defer cleanup()
		h.historyControl(server, &clientState{}, &controlMsg{ // no session attached
			Type: ctlTypeHistory, FromAbs: 10, MaxLines: 10,
		})
		if frames := readServerFrames(t, client, 200*time.Millisecond); len(frames) != 0 {
			t.Errorf("got %d frames pre-resume; want none", len(frames))
		}
	})

	t.Run("ignored when paging is not declared", func(t *testing.T) {
		h := NewHandler([]string{"/bin/cat"}, WithScrollbackCapacity(1000), WithLogger(nil))
		h.screen = vt.New(3, 20)
		fillRing(h, 100)

		server, client, cleanup := dualConn(t)
		defer cleanup()
		state := &clientState{}
		state.session.Store(&sessionState{})
		h.historyControl(server, state, &controlMsg{
			Type: ctlTypeHistory, FromAbs: 10, MaxLines: 10,
		})
		if frames := readServerFrames(t, client, 200*time.Millisecond); len(frames) != 0 {
			t.Errorf("got %d frames from a shallow-ring server; want none", len(frames))
		}
	})
}

// TestTakeHistoryToken pins the bucket: it starts full (so a fresh socket can
// burst), serves exactly historyBurst back-to-back requests, refuses the next,
// and refills over time. The gate exists to suppress accidental bursts and keep
// one socket from monopolizing the handler, not to meter a healthy client —
// which paces itself more slowly than this refills.
func TestTakeHistoryToken(t *testing.T) {
	st := &clientState{}

	for i := range int(historyBurst) {
		if !st.takeHistoryToken() {
			t.Fatalf("token %d/%d refused; the bucket must start full", i+1, int(historyBurst))
		}
	}
	if st.takeHistoryToken() {
		t.Errorf("token %d granted; want refusal past the burst", int(historyBurst)+1)
	}

	// Backdate the last refill by exactly one refill period: one token accrues,
	// so exactly one more request is served and the next is refused again.
	st.historyLast = time.Now().Add(-historyRefill)
	if !st.takeHistoryToken() {
		t.Error("no token after one refill period; the bucket does not refill")
	}
	if st.takeHistoryToken() {
		t.Error("two tokens after one refill period; the refill over-credits")
	}
}

// TestHistoryControl_throttled pins the bucket's effect on the wire: past the
// burst, a request produces no reply. A client cannot tell that from network
// loss, which is deliberate.
func TestHistoryControl_throttled(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithScrollbackCapacity(paginationMinRing), WithLogger(nil))
	h.screen = vt.New(3, 20)
	fillRing(h, 100)

	server, client, cleanup := dualConn(t)
	defer cleanup()
	state := &clientState{}
	state.session.Store(&sessionState{})
	req := &controlMsg{Type: ctlTypeHistory, FromAbs: 10, MaxLines: 10}

	for range int(historyBurst) {
		h.historyControl(server, state, req)
	}
	if frames := readServerFrames(t, client, 300*time.Millisecond); len(frames) != int(historyBurst) {
		t.Fatalf("served %d of %d burst requests", len(frames), int(historyBurst))
	}

	h.historyControl(server, state, req)
	if frames := readServerFrames(t, client, 200*time.Millisecond); len(frames) != 0 {
		t.Errorf("got %d frames past the burst; want none (throttled)", len(frames))
	}
}

// TestHandleControl_routesHistory pins the dispatch wiring: a `history` control
// is RECOGNIZED (so the read loop does not log it as unknown) and reaches the
// handler. An unrecognized control on an older server is exactly the
// "unsupported" answer a newer client infers from the missing capability bit,
// which is why recognition here is the whole difference.
func TestHandleControl_routesHistory(t *testing.T) {
	h := NewHandler([]string{"/bin/cat"}, WithScrollbackCapacity(paginationMinRing), WithLogger(nil))
	h.screen = vt.New(3, 20)
	fillRing(h, 100)

	server, client, cleanup := dualConn(t)
	defer cleanup()
	state := &clientState{}
	state.session.Store(&sessionState{})

	payload, err := json.Marshal(controlMsg{Type: ctlTypeHistory, FromAbs: 20, MaxLines: 5})
	if err != nil {
		t.Fatalf("marshal control: %v", err)
	}
	d := h.handleControl(server, state, payload, nil)
	if !d.parsed || !d.known {
		t.Errorf("disposition parsed=%v known=%v; want both true", d.parsed, d.known)
	}
	frames := readServerFrames(t, client, 300*time.Millisecond)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 scroll reply", len(frames))
	}
	if first, n := decodeScroll(t, frames[0]); first != 20 || n != 5 {
		t.Errorf("reply = (first %d, %d lines), want (20, 5)", first, n)
	}
}
