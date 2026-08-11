package terminal

import "testing"

// The minimum-contrast floor is configured on the Handler but enforced by the
// screen, so the only thing that can break between them is the plumb-through in
// NewHandler. Assert it behaviourally, through the screen's own wire output,
// rather than by reaching for vt's unexported field.
//
// SGR 34 is the case that motivated the option: a program picks palette slot 4
// and cannot know what RGB the terminal resolves it to. The engine's default for
// that slot clears 4.5:1 on black only just, so a floor of 4.5 must nudge it —
// which is precisely why this asserts "changed" rather than a literal RGB, and
// why the paired default case asserts the raw entry survives untouched.
func TestWithMinimumContrastReachesTheScreen(t *testing.T) {
	t.Parallel()

	// slot4 is the engine's default basic-16 blue (vt's basic16RGB index 4).
	const slot4 = int32(0x0d73cc)

	firstFG := func(t *testing.T, h *Handler) int32 {
		t.Helper()
		if _, err := h.screen.Write([]byte("\x1b[34mlink")); err != nil {
			t.Fatalf("screen write: %v", err)
		}
		runs := h.screen.RenderRowWire(0)
		if len(runs) == 0 {
			t.Fatal("no wire runs")
		}
		return runs[0].F
	}

	t.Run("off by default, so the raw palette entry reaches the client", func(t *testing.T) {
		t.Parallel()
		got := firstFG(t, NewHandler([]string{"/bin/true"}))
		if got != slot4 {
			t.Errorf("F = %#06x, want the untouched palette entry %#06x", got, slot4)
		}
	})

	t.Run("the option reaches the screen and lifts the foreground", func(t *testing.T) {
		t.Parallel()
		got := firstFG(t, NewHandler([]string{"/bin/true"}, WithMinimumContrast(4.5)))
		if got == slot4 {
			t.Errorf("F = %#06x is the unlifted palette entry; the option never reached the screen", got)
		}
	})

	t.Run("a ratio at or below the off value changes nothing", func(t *testing.T) {
		t.Parallel()
		got := firstFG(t, NewHandler([]string{"/bin/true"}, WithMinimumContrast(1)))
		if got != slot4 {
			t.Errorf("F = %#06x, want the untouched palette entry %#06x", got, slot4)
		}
	})
}
