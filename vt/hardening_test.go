package vt

import "testing"

// --- Unbounded buffer growth (P1 findings) ---

func TestOSCBufferBounded(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b]2;"))
	chunk := make([]byte, 4096)
	for i := range chunk {
		chunk[i] = 'A'
	}
	for range 4 {
		s.Write(chunk)
	}
	if len(s.oscBuf) > maxOSCLen {
		t.Errorf("oscBuf grew to %d bytes, want <= %d", len(s.oscBuf), maxOSCLen)
	}
	s.Write([]byte("\x07"))
	if s.pState != stGround {
		t.Errorf("parser not back to ground after BEL")
	}
}

func TestCSIParamsBounded(t *testing.T) {
	s := New(24, 80)
	// more than maxParams (32) semicolon-separated values
	s.Write([]byte("\x1b["))
	for i := range 40 {
		if i > 0 {
			s.Write([]byte(";"))
		}
		s.Write([]byte("1"))
	}
	if !s.ignoring {
		t.Errorf("expected ignoring flag set after param count overflow")
	}
	if s.numParams > maxParams {
		t.Errorf("numParams grew to %d, want <= %d", s.numParams, maxParams)
	}
}

func TestCSIIntermedBounded(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b["))
	// intermediate bytes, 0x20-0x2F range
	chunk := make([]byte, 256)
	for i := range chunk {
		chunk[i] = 0x20 + byte(i%16)
	}
	s.Write(chunk)
	if s.numInterm > maxIntermed {
		t.Errorf("numInterm grew to %d, want <= %d", s.numInterm, maxIntermed)
	}
}

// --- CSI param clamping (P1 finding: DoS via huge loop counts) ---

func TestShiftLeftLargeN(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("Hello World"))
	s.Write([]byte("\x1b[65535 @")) // shift left, clamped
	for x := range s.Width {
		if s.Cells[0][x].Ch != ' ' {
			t.Errorf("cell[0][%d] = %q, want ' ' after large shift left", x, s.Cells[0][x].Ch)
			break
		}
	}
}

func TestShiftRightLargeN(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("Hello World"))
	s.Write([]byte("\x1b[65535 A")) // shift right, clamped
	for x := range s.Width {
		if s.Cells[0][x].Ch != ' ' {
			t.Errorf("cell[0][%d] = %q, want ' ' after large shift right", x, s.Cells[0][x].Ch)
			break
		}
	}
}

func TestDeleteCharsLargeN(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("Hello"))
	s.Write([]byte("\x1b[1G"))     // back to col 0
	s.Write([]byte("\x1b[65535P")) // more than exist: clears from cursor
	for x := range s.Width {
		if s.Cells[0][x].Ch != ' ' {
			t.Errorf("cell[0][%d] = %q, want ' '", x, s.Cells[0][x].Ch)
			break
		}
	}
}

func TestCSIArgClamped(t *testing.T) {
	s := New(24, 80)
	s.Write([]byte("\x1b[999999999A")) // CSI param values clamp to maxCSIArgValue
	row, _ := s.CursorPos()
	if row != 0 {
		t.Errorf("cursor row after huge CUU: got %d, want 0", row)
	}
}

func TestScrollUpLargeN(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("Line1\r\nLine2\r\nLine3"))
	s.Write([]byte("\x1b[65535S")) // large clamped scroll: not a timeout
	for y := range s.Height {
		for x := range s.Width {
			if s.Cells[y][x].Ch != ' ' {
				t.Errorf("cell[%d][%d] = %q, want ' '", y, x, s.Cells[y][x].Ch)
				return
			}
		}
	}
}

func TestScrollDownLargeN(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("Line1\r\nLine2\r\nLine3"))
	s.Write([]byte("\x1b[65535T")) // clamped to region height
	for y := range s.Height {
		for x := range s.Width {
			if s.Cells[y][x].Ch != ' ' {
				t.Errorf("cell[%d][%d] = %q, want ' '", y, x, s.Cells[y][x].Ch)
				return
			}
		}
	}
}

// TestWideCharNarrowScreenCursorClamp verifies that a width-2 character
// on a 1-column screen does not leave curX out of bounds. Regression
// test for a fuzz-discovered bug where put() failed to clamp curX after
// the wide-char spacer on screens narrower than 2 columns.
func TestWideCharNarrowScreenCursorClamp(t *testing.T) {
	s := New(5, 1)
	s.Write([]byte("漢"))
	row, col := s.CursorPos()
	if col < 0 || col >= s.Width {
		t.Errorf("cursor col %d out of bounds [0, %d) after wide char on 1-col screen", col, s.Width)
	}
	if row < 0 || row >= s.Height {
		t.Errorf("cursor row %d out of bounds [0, %d)", row, s.Height)
	}
	s.Write([]byte("漢漢漢ABCD"))
	_, col = s.CursorPos()
	if col < 0 || col >= s.Width {
		t.Errorf("cursor col %d out of bounds after multiple writes on 1-col screen", col)
	}
}

func TestInsertLinesLargeN(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("AAAA\r\nBBBB\r\nCCCC"))
	s.Write([]byte("\x1b[1;1H"))   // home
	s.Write([]byte("\x1b[65535L")) // more than the scroll region: clamped
	for y := range s.Height {
		for x := range s.Width {
			if s.Cells[y][x].Ch != ' ' {
				t.Errorf("cell[%d][%d] = %q, want ' '", y, x, s.Cells[y][x].Ch)
				return
			}
		}
	}
}

func TestDeleteLinesLargeN(t *testing.T) {
	s := New(5, 10)
	s.Write([]byte("AAAA\r\nBBBB\r\nCCCC"))
	s.Write([]byte("\x1b[1;1H"))   // home
	s.Write([]byte("\x1b[65535M")) // more than the scroll region: clamped
	for y := range s.Height {
		for x := range s.Width {
			if s.Cells[y][x].Ch != ' ' {
				t.Errorf("cell[%d][%d] = %q, want ' '", y, x, s.Cells[y][x].Ch)
				return
			}
		}
	}
}
