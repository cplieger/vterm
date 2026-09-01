package vt

import "testing"

// BenchmarkScreenWrite measures throughput of Screen.Write processing raw
// VT output containing mixed printable text and escape sequences.
func BenchmarkScreenWrite(b *testing.B) {
	// Simulate a typical terminal frame: colored text with cursor movement.
	frame := []byte("\x1b[H\x1b[2J")
	for i := range 24 {
		frame = append(frame, []byte("\x1b[38;5;"+string(rune('0'+i%10))+"m")...)
		frame = append(frame, []byte("Hello, terminal world! Line content here.\r\n")...)
	}

	s := New(24, 80)
	b.ReportAllocs()
	for b.Loop() {
		s.Write(frame)
	}
}

// BenchmarkRenderRowWire measures the cost of converting a populated row
// into wire-format style runs.
func BenchmarkRenderRowWire(b *testing.B) {
	s := New(24, 80)
	// Fill row 0 with styled content.
	frame := []byte("\x1b[1;31mHello \x1b[0;32mWorld \x1b[4;34mTest content padding.")
	s.Write(frame)

	b.ReportAllocs()
	for b.Loop() {
		_ = s.RenderRowWire(0)
	}
}
