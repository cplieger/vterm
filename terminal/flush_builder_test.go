package terminal

import (
	"slices"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/cplieger/web-terminal-engine/v5/vt"
)

// noClients is an empty client snapshot for tests that only care
// about the frame contents, not its fan-out.
var noClients = map[*websocket.Conn]uint64{}

// TestBuild_CursorOnlyMoveAddsRowToChanged drives the bug from the
// "cursor visually doesn't move on left/right arrow or space-over-space"
// report:
//
//   - First Build is a full repaint (all rows in changed) — establishes
//     the prev cache.
//   - Second Build with no PTY input at all: must return nil.
//   - Third Build after CSI D (cursor back): row content unchanged but
//     cursor moved, so the cursor row must appear in changed so the
//     wire frame carries its payload and the client can repaint the
//     inline cursor span at the new column. Without this fix the
//     server emits a non-nil frame but flushLoop drops the screen
//     payload (it gates on len(changed) > 0) and the client never
//     sees the move.
func TestBuild_CursorOnlyMoveAddsRowToChanged(t *testing.T) {
	screen := vt.New(10, 40)
	// Establish some content so the cursor is on a row that has runs.
	if _, err := screen.Write([]byte("hello world")); err != nil {
		t.Fatalf("screen write: %v", err)
	}
	b := &flushFrameBuilder{}

	// First frame: full repaint baseline.
	frame := b.Build(screen, true, noClients, 0)
	if frame == nil {
		t.Fatalf("first Build returned nil; expected full repaint")
	}
	if len(frame.changed) != screen.Height {
		t.Fatalf("first Build: want all %d rows changed, got %d", screen.Height, len(frame.changed))
	}
	row, _ := screen.CursorPos()
	if !slices.Contains(frame.changed, row) {
		t.Fatalf("first Build: cursor row %d missing from changed %v", row, frame.changed)
	}

	// Second frame with no input: nothing to send.
	if frame := b.Build(screen, true, noClients, 0); frame != nil {
		t.Fatalf("idle Build returned non-nil frame: changed=%v", frame.changed)
	}

	// Move cursor left without changing any cell content (CSI D).
	prevRow, prevCol := screen.CursorPos()
	if _, err := screen.Write([]byte{0x1b, '[', 'D'}); err != nil {
		t.Fatalf("write CSI D: %v", err)
	}
	curRow, curCol := screen.CursorPos()
	if curRow == prevRow && curCol == prevCol {
		t.Fatalf("CSI D did not move cursor: still at row=%d col=%d", curRow, curCol)
	}

	frame = b.Build(screen, true, noClients, 0)
	if frame == nil {
		t.Fatalf("post-cursor-move Build returned nil; expected a frame so the client can repaint")
	}
	if !slices.Contains(frame.changed, curRow) {
		t.Fatalf("post-cursor-move Build: cursor row %d missing from changed %v", curRow, frame.changed)
	}
	// Ensure the cursor coordinates reported by the frame match the
	// post-move position (this is what the wire encoder reads).
	if frame.curRow != curRow || frame.curCol != curCol {
		t.Fatalf("frame cursor pos = (%d,%d); want (%d,%d)",
			frame.curRow, frame.curCol, curRow, curCol)
	}
}

// TestBuild_CursorBetweenRowsTouchesBothRows covers the other shape:
// when the cursor moves to a different row without altering content,
// both the previous and current cursor rows must be in `changed` so
// the inline cursor span is removed from the old row and inserted on
// the new row.
func TestBuild_CursorBetweenRowsTouchesBothRows(t *testing.T) {
	screen := vt.New(10, 40)
	// Land the cursor at (5, 5) with content on both rows 5 and 7.
	if _, err := screen.Write([]byte("\x1b[6;1Habcde\x1b[8;1Hxyz\x1b[6;6H")); err != nil {
		t.Fatalf("screen write: %v", err)
	}
	b := &flushFrameBuilder{}
	if frame := b.Build(screen, true, noClients, 0); frame == nil {
		t.Fatal("baseline Build returned nil")
	}
	prevRow, _ := screen.CursorPos()
	if prevRow != 5 {
		t.Fatalf("setup: cursor row = %d, want 5", prevRow)
	}

	// Move cursor to (7, 4) — different row, no cell content change.
	if _, err := screen.Write([]byte("\x1b[8;5H")); err != nil {
		t.Fatalf("write CUP: %v", err)
	}
	curRow, _ := screen.CursorPos()
	if curRow != 7 {
		t.Fatalf("post-CUP cursor row = %d, want 7", curRow)
	}

	frame := b.Build(screen, true, noClients, 0)
	if frame == nil {
		t.Fatal("inter-row cursor move Build returned nil")
	}
	if !slices.Contains(frame.changed, prevRow) {
		t.Fatalf("changed missing previous cursor row %d: got %v", prevRow, frame.changed)
	}
	if !slices.Contains(frame.changed, curRow) {
		t.Fatalf("changed missing current cursor row %d: got %v", curRow, frame.changed)
	}
}

// TestBuild_scrollbackProducesScrollLines verifies that when scrollback has
// drained from the screen and we are not in alt-screen, Build surfaces those
// lines as the frame's scrollLines.
func TestBuild_scrollbackProducesScrollLines(t *testing.T) {
	screen := vt.New(3, 20) // tiny screen so writing many lines scrolls
	for range 15 {
		if _, err := screen.Write([]byte("scrollback line\r\n")); err != nil {
			t.Fatalf("screen write: %v", err)
		}
	}

	b := &flushFrameBuilder{}
	frame := b.Build(screen, true, noClients, 0)
	if frame == nil {
		t.Fatalf("Build returned nil; expected a full-repaint baseline frame")
	}
	if len(frame.scrollLines) == 0 {
		t.Errorf("Build: scrollLines empty; want non-empty (scrollback drained, not alt-screen)")
	}
}

// TestBuildTitlePayload_changeDetection verifies buildTitlePayload emits a
// frame on first announce and whenever the title changes, and suppresses
// (returns nil) when the title is unchanged.
func TestBuildTitlePayload_changeDetection(t *testing.T) {
	screen := vt.New(5, 20)
	b := &flushFrameBuilder{}

	screen.Title = "first"
	// First call: not yet announced -> must emit a payload.
	if got := b.buildTitlePayload(screen); len(got) == 0 {
		t.Fatalf("first buildTitlePayload = empty, want a title frame")
	}
	// Same title, now announced -> suppressed (nil).
	if got := b.buildTitlePayload(screen); len(got) != 0 {
		t.Errorf("unchanged-title buildTitlePayload = %d bytes, want 0 (suppress when title unchanged)", len(got))
	}
	// Changed title -> emit again.
	screen.Title = "second"
	if got := b.buildTitlePayload(screen); len(got) == 0 {
		t.Errorf("changed-title buildTitlePayload = empty, want a title frame")
	}
}

// TestAppendRowIfMissing_rowCountBoundary verifies the [0, rowCount) range
// guard: an index equal to rowCount is rejected (out of range) while the last
// in-range index (rowCount-1) is appended.
func TestAppendRowIfMissing_rowCountBoundary(t *testing.T) {
	// y == rowCount is out of [0, rowCount): must NOT be appended.
	if got := appendRowIfMissing(nil, 5, 5); len(got) != 0 {
		t.Errorf("appendRowIfMissing(nil, 5, 5) = %v, want empty (y==rowCount is out of range)", got)
	}
	// y == rowCount-1 is in range: must be appended.
	got := appendRowIfMissing(nil, 4, 5)
	if len(got) != 1 || got[0] != 4 {
		t.Errorf("appendRowIfMissing(nil, 4, 5) = %v, want [4]", got)
	}
}

// TestBuild_AbsoluteIndexIntegrity drives a scrolling sequence through the
// builder + ring exactly as buildFrame does (Build, then Append the scroll
// lines), while a simulated client applies every server write into a map keyed
// by absolute index. It then asserts the client's reconstructed buffer is
// gap-free and correctly ordered: line N always lands at absolute index N,
// with no duplicates and no holes. This is the property that makes resume
// dedup/gap-free.
func TestBuild_AbsoluteIndexIntegrity(t *testing.T) {
	screen := vt.New(3, 20) // tiny screen so each printed line soon scrolls
	ring := newScrollbackRing(1000)
	b := &flushFrameBuilder{}
	client := map[uint64][]vt.WireRun{}

	apply := func() {
		committedBefore := ring.Committed()
		frame := b.Build(screen, true, noClients, committedBefore)
		if frame == nil {
			return
		}
		// Client applies committed history at firstIdx+i ...
		for i, line := range frame.scrollLines {
			client[frame.scrollFirstIdx+uint64(i)] = line
		}
		// ... and changed window rows at base+y. Idempotent by abs index.
		for _, y := range frame.changed {
			if y >= 0 && y < len(frame.rows) {
				client[frame.base+uint64(y)] = frame.rows[y]
			}
		}
		if len(frame.scrollLines) > 0 {
			ring.Append(frame.scrollLines)
		}
	}

	const n = 10
	for i := range n {
		line := []byte{'l', 'i', 'n', 'e', byte('0' + i/10), byte('0' + i%10), '\r', '\n'}
		if _, err := screen.Write(line); err != nil {
			t.Fatalf("write line %d: %v", i, err)
		}
		apply() // simulate a flush after each write
	}
	apply() // final settle

	if len(client) == 0 {
		t.Fatal("client received no lines")
	}
	// Keys must be contiguous from 0 (no gaps, no negative).
	var maxAbs uint64
	for k := range client {
		if k > maxAbs {
			maxAbs = k
		}
	}
	for k := uint64(0); k <= maxAbs; k++ {
		if _, ok := client[k]; !ok {
			t.Fatalf("gap in client buffer at absolute index %d (max %d)", k, maxAbs)
		}
	}
	// Each printed line N must appear at absolute index N exactly.
	for i := range n {
		want := string([]byte{'l', 'i', 'n', 'e', byte('0' + i/10), byte('0' + i%10)})
		got := runText(client[uint64(i)])
		if got != want {
			t.Errorf("absolute index %d = %q, want %q", i, got, want)
		}
	}
}

// runText joins the text of a row's runs, trimming the trailing blanks the
// renderer pads rows with.
func runText(runs []vt.WireRun) string {
	var b strings.Builder
	for _, r := range runs {
		b.WriteString(r.T)
	}
	return trimTrailingSpaces(b.String())
}

func trimTrailingSpaces(s string) string {
	end := len(s)
	for end > 0 && s[end-1] == ' ' {
		end--
	}
	return s[:end]
}

// TestBuild_bellOnlyStillEmitsFrame pins the bell fold in Build: a BEL changes
// no cell and moves no cursor, so diffWindow yields no changed rows and the
// cursor is unmoved -- frameEmpty would drop the frame and the bell would never
// reach the client. Build folds the cursor row into `changed` when the bell rang
// so the screen frame is emitted with its bell flag set. The first Build primes
// the previous-window cache so the second sees a genuinely idle (bell-only) screen.
func TestBuild_bellOnlyStillEmitsFrame(t *testing.T) {
	screen := vt.New(10, 40)
	if _, err := screen.Write([]byte("hello world")); err != nil {
		t.Fatalf("screen write: %v", err)
	}
	b := &flushFrameBuilder{}

	// Prime the prev-window cache with a full-repaint baseline.
	if frame := b.Build(screen, true, noClients, 0); frame == nil {
		t.Fatal("baseline Build returned nil; expected full repaint")
	}

	// Ring the bell only: no cell change, no cursor move.
	if _, err := screen.Write([]byte{0x07}); err != nil {
		t.Fatalf("write BEL: %v", err)
	}

	frame := b.Build(screen, true, noClients, 0)
	if frame == nil {
		t.Fatal("bell-only Build returned nil; the bell fold must emit a frame so the bell reaches the client")
	}
	if !frame.bell {
		t.Error("bell-only frame has bell=false; want true")
	}
	curRow, _ := screen.CursorPos()
	if !slices.Contains(frame.changed, curRow) {
		t.Errorf("bell-only frame changed=%v missing cursor row %d (needed so the screen payload, and its bell bit, is emitted)", frame.changed, curRow)
	}
}

// TestBuildModesPayload_kittyKeyboardFlag closes the flag->wire loop: enabling
// the kitty disambiguate flag on the screen (CSI > 1 u) must make
// buildModesPayload emit a modes frame whose trailing kbdFlags byte is 1, and an
// unchanged flag afterwards must be suppressed. (The wire->client half is proven
// by the cross-language golden test.)
func TestBuildModesPayload_kittyKeyboardFlag(t *testing.T) {
	screen := vt.New(5, 20)
	b := &flushFrameBuilder{}

	// First announce (modes not yet announced) -> emits; kbdFlags byte (index
	// 12 = type1 + ack8 + flags1 + mouseMode2) starts at 0.
	first := b.buildModesPayload(screen)
	if len(first) < 13 {
		t.Fatalf("first buildModesPayload = %d bytes, want >= 13 (incl. kbdFlags)", len(first))
	}
	if first[12] != 0 {
		t.Errorf("initial kbdFlags byte = %d, want 0", first[12])
	}

	// App enables the disambiguate flag; the next payload carries kbdFlags = 1.
	if _, err := screen.Write([]byte("\x1b[>1u")); err != nil {
		t.Fatalf("screen write: %v", err)
	}
	changed := b.buildModesPayload(screen)
	if len(changed) < 13 {
		t.Fatalf("after CSI >1u buildModesPayload = %d bytes, want a frame", len(changed))
	}
	if changed[12] != 1 {
		t.Errorf("kbdFlags byte after CSI >1u = %d, want 1", changed[12])
	}

	// Unchanged -> suppressed (nil).
	if got := b.buildModesPayload(screen); got != nil {
		t.Errorf("unchanged buildModesPayload = %d bytes, want nil (suppressed)", len(got))
	}
}

// TestReset_reAnnouncesTitleAndModes pins the resume / second-client
// redelivery contract of flushFrameBuilder.Reset: after the window title and
// DEC modes have been announced to one client, Reset (called on resize, a new
// client connect, a resume, or an alt-screen transition) must clear the
// announced flags so the NEXT Build re-emits both even though the screen state
// is unchanged. Without it, titleStable/modesStable suppress the re-announce
// and a resuming or second-tab client keeps a stale title and default modes
// (breaking mouse/arrow-key input encoding).
func TestReset_reAnnouncesTitleAndModes(t *testing.T) {
	screen := vt.New(5, 20)
	screen.Title = "session title"
	b := &flushFrameBuilder{}

	// Announce title and modes to the first client.
	if got := b.buildTitlePayload(screen); len(got) == 0 {
		t.Fatal("first buildTitlePayload = empty, want a title frame")
	}
	if got := b.buildModesPayload(screen); len(got) == 0 {
		t.Fatal("first buildModesPayload = empty, want a modes frame")
	}
	// Unchanged screen state: both are suppressed.
	if got := b.buildTitlePayload(screen); got != nil {
		t.Fatalf("unchanged buildTitlePayload = %d bytes, want nil", len(got))
	}
	if got := b.buildModesPayload(screen); got != nil {
		t.Fatalf("unchanged buildModesPayload = %d bytes, want nil", len(got))
	}

	// Resume / second client / resize: Reset must force a re-announce of both.
	b.Reset()
	if got := b.buildTitlePayload(screen); len(got) == 0 {
		t.Error("after Reset, buildTitlePayload = empty; a resuming client must be re-sent the current title")
	}
	if got := b.buildModesPayload(screen); len(got) == 0 {
		t.Error("after Reset, buildModesPayload = empty; a resuming client must be re-sent the current DEC modes")
	}
}

// TestBuild_WrapExtensionResendsFirstURLRow pins the streaming half of the
// wrapped-URL autolink fix: when a URL's continuation lands on the next row in
// a LATER flush (char-by-char echo of a long URL being typed, or any output
// burst that straddles a flush tick), the FIRST row's rendered runs change —
// its autolink stamp extends to the longer match — and diffWindow's
// runs-equality diff must re-send that row even though its CELLS never
// changed. If this regresses, the first row keeps a truncated href until an
// unrelated repaint.
func TestBuild_WrapExtensionResendsFirstURLRow(t *testing.T) {
	screen := vt.New(10, 20)
	// Fill row 0 to the wrap edge with the URL head. No wrap has occurred yet.
	if _, err := screen.Write([]byte("https://ex.com/aaaaa")); err != nil { // exactly 20 cols
		t.Fatalf("screen write: %v", err)
	}
	b := &flushFrameBuilder{}
	if frame := b.Build(screen, true, noClients, 0); frame == nil {
		t.Fatalf("baseline Build returned nil")
	}

	// The continuation arrives in a later tick: the deferred wrap fires, row 1
	// becomes a soft continuation, and the chain-joined match now extends.
	if _, err := screen.Write([]byte("bbb")); err != nil {
		t.Fatalf("screen write 2: %v", err)
	}
	frame := b.Build(screen, true, noClients, 0)
	if frame == nil {
		t.Fatalf("Build after wrap extension returned nil")
	}
	if !slices.Contains(frame.changed, 0) {
		t.Errorf("row 0 not re-sent after its autolink stamp extended; changed = %v", frame.changed)
	}
	if !slices.Contains(frame.changed, 1) {
		t.Errorf("continuation row 1 missing from changed = %v", frame.changed)
	}
	// And the re-sent row 0 must now carry the FULL joined href.
	full := "https://ex.com/aaaaabbb"
	var got string
	for _, r := range frame.rows[0] {
		if r.A&vt.AttrAutolink != 0 {
			got = r.U
		}
	}
	if got != full {
		t.Errorf("row 0 href after extension = %q, want %q", got, full)
	}
}
