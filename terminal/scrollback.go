package terminal

import "github.com/cplieger/web-terminal-engine/v5/vt"

// scrollbackRing is a capacity-bounded ring buffer of scrollback lines,
// addressed by absolute line index.
//
// Every line that scrolls off the top of the VT screen is appended here
// and assigned a monotonic absolute index: the first line ever committed
// is index 0, the next is 1, and so on, growing without bound for the
// life of the session. The ring retains only the most recent `capacity`
// lines; older lines are evicted, but their indices are never reused. The
// absolute index of the current top screen row equals Committed().
//
// The buffer GROWS to `capacity` rather than being allocated at it, so a
// deep capacity costs a session nothing until it has actually produced that
// much history. This matters because capacity is an operator-set number
// (SCROLLBACK) whose whole point is that it can be set absurdly high:
// preallocating charged every session 24 bytes per configured line up front
// — 2.3 MB at the 100k default before a single line was printed, and 240 MB
// for an operator who asked for 10 million.
//
// Absolute indexing is the backbone of the engine. It makes resume
// alignment exact (the client asks for
// "everything after index H") and makes duplicate delivery structurally
// impossible (writing the same index twice overwrites the same row),
// replacing the old count-based scheme whose two independently-capped
// buffers drifted into overlaps and gaps.
type scrollbackRing struct {
	buf       [][]vt.WireRun
	capacity  int    // retained-line ceiling; buf grows up to this
	start     int    // ring index of the oldest retained line
	count     int    // number of retained lines (<= len(buf))
	committed uint64 // total lines ever appended = absolute index of the next line
}

func newScrollbackRing(capacity int) *scrollbackRing {
	return &scrollbackRing{capacity: max(capacity, 0)}
}

// Append adds lines to the ring in order, assigning each the next
// absolute index and evicting the oldest retained line when at capacity.
// committed advances by len(lines) regardless of capacity, so absolute
// indices stay monotonic even after eviction.
func (r *scrollbackRing) Append(lines [][]vt.WireRun) {
	if r.capacity == 0 {
		// Scrollback disabled: still advance committed so the screen
		// window's absolute base stays correct. Lines are unrecoverable
		// on resume, which is the documented behavior of capacity 0.
		r.committed += uint64(len(lines))
		return
	}
	for _, line := range lines {
		if len(r.buf) < r.capacity {
			// Growing. start is 0 and count == len(buf) in this phase, so the
			// append position IS the ring index and no modulo is needed.
			r.buf = append(r.buf, line)
			r.count++
		} else {
			// At capacity: the slot holding the oldest line is also the next
			// slot to write, so one store plus one start advance is the whole
			// eviction. count stays at capacity.
			r.buf[r.start] = line
			r.start = (r.start + 1) % r.capacity
		}
		r.committed++
	}
}

// Committed returns the total number of lines ever committed to history,
// which equals the absolute index of the current top screen row (the
// next line to be appended).
func (r *scrollbackRing) Committed() uint64 {
	return r.committed
}

// OldestIndex returns the absolute index of the oldest line still
// retained in the ring. Lines below this index have been evicted and
// cannot be replayed; a resuming client that needs them is shown a
// history-trimmed marker rather than a misaligned stitch.
func (r *scrollbackRing) OldestIndex() uint64 {
	return r.committed - uint64(r.count) // #nosec G115 -- count is non-negative and bounded by len(buf)
}

// copyLine returns a fresh copy of one retained line. The rings hand lines to
// replay and paging paths whose callers serialize or transform them; without
// the copy the returned inner []vt.WireRun still aliases ring history, so one
// mutating caller would rewrite what every LATER replay sees. The outer slices
// were always fresh; this closes the inner layer (go-rulebook C21).
func copyLine(line []vt.WireRun) []vt.WireRun {
	if line == nil {
		return nil
	}
	out := make([]vt.WireRun, len(line))
	copy(out, line)
	return out
}

// LinesFrom returns the retained lines with absolute index >= abs, in
// order, along with the absolute index of the first returned line.
// When abs is older than what the ring retains, it clamps up to
// OldestIndex (the caller compares the returned firstAbs against the
// requested abs to detect an eviction gap). When abs is at or beyond
// Committed, it returns no lines.
func (r *scrollbackRing) LinesFrom(abs uint64) (firstAbs uint64, lines [][]vt.WireRun) {
	if r.count == 0 || abs >= r.committed {
		return r.committed, nil
	}
	oldest := r.OldestIndex()
	start := max(abs, oldest)
	skip := int(start - oldest) // #nosec G115 -- bounded by count
	out := make([][]vt.WireRun, 0, r.count-skip)
	n := len(r.buf)
	for i := skip; i < r.count; i++ {
		out = append(out, copyLine(r.buf[(r.start+i)%n]))
	}
	return start, out
}

// LinesRange returns at most maxLines retained lines starting at absolute
// index abs, in order, along with the absolute index of the first returned
// line. It is LinesFrom with a count bound: LinesFrom returns everything
// from the clamp up to the tail, which is an O(ring) copy per call and far
// more than a paged history request asks for (see docs/paged-scrollback.md
// §4.2). Clamping behavior is identical — abs older than the retained range
// clamps up to OldestIndex, so the caller compares the returned firstAbs
// against the requested abs to detect an eviction gap — and abs at or beyond
// Committed returns no lines. maxLines <= 0 returns no lines.
func (r *scrollbackRing) LinesRange(abs uint64, maxLines int) (firstAbs uint64, lines [][]vt.WireRun) {
	if r.count == 0 || abs >= r.committed || maxLines <= 0 {
		return r.committed, nil
	}
	oldest := r.OldestIndex()
	start := max(abs, oldest)
	skip := int(start - oldest) // #nosec G115 -- bounded by count
	avail := r.count - skip
	take := min(avail, maxLines)
	out := make([][]vt.WireRun, 0, take)
	n := len(r.buf)
	for i := skip; i < skip+take; i++ {
		out = append(out, copyLine(r.buf[(r.start+i)%n]))
	}
	return start, out
}

// Lines returns all retained lines in order (oldest first). Retained for
// tests; the live and resume paths use LinesFrom.
func (r *scrollbackRing) Lines() [][]vt.WireRun {
	if r.count == 0 {
		return nil
	}
	out := make([][]vt.WireRun, r.count)
	n := len(r.buf)
	for i := range r.count {
		out[i] = copyLine(r.buf[(r.start+i)%n])
	}
	return out
}

// Clear discards all retained lines. committed is preserved so absolute
// indices never repeat within a session.
//
// The buffer is RELEASED, not logically emptied, and both halves matter on a
// growing ring. Leaving it untouched would put the ring in an impossible state
// — length intact, count zero — where Append's growth branch writes past index
// 0 while the readers still index from 0, so the next line committed would read
// back as a pre-Clear row. And dropping the array (rather than reslicing it to
// zero length) frees the rows it holds, which is what an application clearing
// its scrollback is asking for; at the 100k default that array is 2.3 MB of
// pointers keeping every retained row alive.
func (r *scrollbackRing) Clear() {
	r.buf = nil
	r.start = 0
	r.count = 0
}

// Len returns the number of lines currently retained.
func (r *scrollbackRing) Len() int {
	return r.count
}
