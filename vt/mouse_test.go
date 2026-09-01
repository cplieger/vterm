package vt

import "testing"

func TestMouseModeTracking(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantMode  uint16
		wantSGR   bool
		wantFocus bool
	}{
		{"mode 1000 enables normal tracking", "\x1b[?1000h", 1000, false, false},
		{"mode 1002 enables button-event tracking", "\x1b[?1002h", 1002, false, false},
		{"mode 1003 enables any-event tracking", "\x1b[?1003h", 1003, false, false},
		{"mode 1006 enables SGR encoding", "\x1b[?1000;1006h", 1000, true, false},
		{"mode 1004 enables focus reporting", "\x1b[?1004h", 0, false, true},
		{"combined modes", "\x1b[?1003;1006;1004h", 1003, true, true},
		{"disable mode 1000", "\x1b[?1000h\x1b[?1000l", 0, false, false},
		{"disable mode 1003", "\x1b[?1003h\x1b[?1003l", 0, false, false},
		{"disable SGR", "\x1b[?1006h\x1b[?1006l", 0, false, false},
		{"disable focus", "\x1b[?1004h\x1b[?1004l", 0, false, false},
		{"upgrade 1000 to 1003", "\x1b[?1000h\x1b[?1003h", 1003, false, false},
		{"soft reset clears mouse", "\x1b[?1003;1006;1004h\x1b[!p", 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(24, 80)
			if _, err := s.Write([]byte(tt.input)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if s.MouseMode != tt.wantMode {
				t.Errorf("MouseMode = %d, want %d", s.MouseMode, tt.wantMode)
			}
			if s.MouseSGR != tt.wantSGR {
				t.Errorf("MouseSGR = %v, want %v", s.MouseSGR, tt.wantSGR)
			}
			if s.FocusReporting != tt.wantFocus {
				t.Errorf("FocusReporting = %v, want %v", s.FocusReporting, tt.wantFocus)
			}
		})
	}
}

// TestMousePixelsModeTracked verifies SGR-pixels mouse (DEC private mode 1016)
// sets and clears the MousePixels flag and that the flag survives a resize (the
// mode is independent of screen dimensions). 1016 is the pixel-coordinate
// variant of SGR mouse (1006); the engine tracks only the flag while the client
// encodes the pixel coordinates.
func TestMousePixelsModeTracked(t *testing.T) {
	s := New(5, 5)
	if s.MousePixels {
		t.Fatal("default MousePixels should be false")
	}
	s.Write([]byte("\x1b[?1016h"))
	if !s.MousePixels {
		t.Error("after ?1016h, MousePixels = false, want true")
	}
	s.Resize(1, 1)
	if !s.MousePixels {
		t.Error("MousePixels must survive a resize")
	}
	s.Write([]byte("\x1b[?1016l"))
	if s.MousePixels {
		t.Error("after ?1016l, MousePixels = true, want false")
	}
}

// TestMouseModeBoundaryResize verifies mouse mode survives a resize to the
// minimum dimensions and the cursor stays in bounds.
func TestMouseModeBoundaryResize(t *testing.T) {
	s := New(10, 80)
	s.Write([]byte("\x1b[?1003h\x1b[?1006h"))
	s.Write([]byte("漢字テスト"))
	s.Resize(1, 1)
	if s.MouseMode != 1003 {
		t.Fatalf("mouse mode lost on resize: %d", s.MouseMode)
	}
	row, col := s.CursorPos()
	if col < 0 || col >= s.Width {
		t.Fatalf("cursor col %d out of bounds after resize with mouse mode", col)
	}
	if row < 0 || row >= s.Height {
		t.Fatalf("cursor row %d out of bounds", row)
	}
}

// TestMouseCoords223Boundary verifies cursor positioning works past the X10
// 223-column encoding limit on a wide screen and SGR mode can be enabled.
func TestMouseCoords223Boundary(t *testing.T) {
	s := New(5, 250)
	s.Write([]byte("\x1b[?1000h"))
	if s.MouseMode != 1000 {
		t.Fatalf("expected mouse mode 1000")
	}
	s.Write([]byte("\x1b[1;224H")) // col 224 (0-indexed 223)
	if row, col := s.CursorPos(); col != 223 || row != 0 {
		t.Fatalf("expected (0,223), got (%d,%d)", row, col)
	}
	s.Write([]byte("\x1b[1;250H"))
	if _, col := s.CursorPos(); col != 249 {
		t.Fatalf("expected col 249, got %d", col)
	}
	s.Write([]byte("\x1b[?1006h"))
	if !s.MouseSGR {
		t.Fatal("expected SGR mode")
	}
}
