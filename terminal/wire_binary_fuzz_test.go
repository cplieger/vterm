package terminal

import (
	"encoding/binary"
	"testing"

	"github.com/cplieger/web-terminal-engine/v4/vt"
)

// FuzzEncodeScreenMsg_structuralIntegrity generates random screen states,
// encodes them via encodeScreenMsg, and walks the output bytes to confirm the
// frame round-trips and is self-framing: the header counts match, each changed
// row's decoded index and run text equal what was encoded (decode(encode(x))
// == x for those fields), and the payload consumes exactly the encoded length
// with no trailing bytes. There is no Go decoder (the TS side owns decode), so
// the walk decodes inline against the documented frame layout.
func FuzzEncodeScreenMsg_structuralIntegrity(f *testing.F) {
	f.Add(uint8(3), uint8(4), uint8(1), uint8(0), []byte("hi"))
	f.Add(uint8(1), uint8(1), uint8(0), uint8(0), []byte(""))
	f.Add(uint8(10), uint8(20), uint8(5), uint8(3), []byte("abc"))

	f.Fuzz(func(t *testing.T, height, width, numChanged, curStyle uint8, text []byte) {
		h := int(height)%50 + 1
		nc := int(numChanged) % h
		if nc == 0 {
			nc = 1
		}

		// width drives the run text length so that dimension of the fuzz input
		// actually influences the encoded payload (screen_height, not width, is
		// on the wire, so width has no field of its own to map to).
		maxLen := int(width)%20 + 1
		txt := "A"
		if len(text) > 0 {
			txt = string(text[:min(len(text), maxLen)])
		}

		// Build rows and changed indices; every changed row carries exactly one
		// run whose text is txt, so the walk below can assert that text round-trips.
		changed := make([]int, nc)
		rows := make([][]vt.WireRun, h)
		for i := range nc {
			idx := i % h
			changed[i] = idx
			rows[idx] = []vt.WireRun{{T: txt, F: -1, B: -1, A: 0, Uc: -1}}
		}

		buf := encodeScreenMsg(0, h, 0, 0, 0, changed, rows, curStyle, false, false, false, false, false)

		// Structural validation. Header is now:
		//   [0] type, [1:9] ack, [9:17] base, [17:19] curRow,
		//   [19:21] curCol, [21:23] screenHeight, [23:25] numChanged,
		//   [25] cursorStyle, [26] cursorFlags, [27:] changed rows.
		if len(buf) < 27 {
			t.Fatalf("encoded too short: %d bytes", len(buf))
		}
		if buf[0] != wireMsgScreen {
			t.Fatalf("msg type = %d, want %d", buf[0], wireMsgScreen)
		}
		screenH := binary.LittleEndian.Uint16(buf[21:23])
		if int(screenH) != h {
			t.Fatalf("screenHeight = %d, want %d", screenH, h)
		}
		numCh := binary.LittleEndian.Uint16(buf[23:25])
		if int(numCh) != nc {
			t.Fatalf("numChanged = %d, want %d", numCh, nc)
		}

		// Walk each changed row and verify the frame round-trips the fields we
		// set: the decoded row index matches changed[i], the row carries exactly
		// the one run we encoded, and that run's text decodes back to txt. The
		// walk also proves the payload is self-framing (every length field lands
		// in bounds and the bytes are consumed exactly, with no trailing slack).
		off := 27
		for i := range int(numCh) {
			if off+2 > len(buf) {
				t.Fatalf("truncated at row_idx %d", i)
			}
			rowIdx := int(binary.LittleEndian.Uint16(buf[off:]))
			off += 2
			if rowIdx >= h {
				t.Fatalf("row_idx %d >= screenHeight %d", rowIdx, h)
			}
			if rowIdx != changed[i] {
				t.Fatalf("changed row %d: decoded row_idx %d, want %d (index must round-trip)", i, rowIdx, changed[i])
			}
			if off+2 > len(buf) {
				t.Fatalf("truncated at num_runs for row %d", i)
			}
			numRuns := int(binary.LittleEndian.Uint16(buf[off:]))
			off += 2
			if numRuns != 1 {
				t.Fatalf("changed row %d: num_runs = %d, want 1 (one run was encoded)", i, numRuns)
			}
			if off+2 > len(buf) {
				t.Fatalf("truncated at text_len for row %d", i)
			}
			tlen := int(binary.LittleEndian.Uint16(buf[off:]))
			off += 2
			if off+tlen > len(buf) {
				t.Fatalf("truncated text (len %d) for row %d", tlen, i)
			}
			if got := string(buf[off : off+tlen]); got != txt {
				t.Fatalf("changed row %d: decoded text %q, want %q (text must round-trip)", i, got, txt)
			}
			off += tlen
			off += 4 + 4 + 2 + 4 // fg + bg + attrs + uc
			if off+2 > len(buf) {
				t.Fatalf("truncated at url_len for row %d", i)
			}
			ulen := int(binary.LittleEndian.Uint16(buf[off:]))
			off += 2 + ulen
		}
		if off != len(buf) {
			t.Fatalf("trailing bytes: consumed %d of %d", off, len(buf))
		}
	})
}
