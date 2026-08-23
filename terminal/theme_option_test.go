package terminal

import (
	"testing"

	"github.com/cplieger/web-terminal-engine/v5/vt"
)

// The theme is configured on the Handler but reported by the screen, so the only
// thing that can break between them is the plumb-through in NewHandler. Assert
// it behaviourally, through the OSC 11 query reply the screen queues for the
// application, rather than by reaching for vt's unexported field.
//
// The reply is what a color-probing program actually reads, so a broken
// plumb-through means light/dark detection answers with the built-in dark
// default instead of the consumer's real colors -- silently, since a wrong
// answer looks exactly like a right one.
func TestWithThemeReachesTheScreen(t *testing.T) {
	t.Parallel()

	// queryBG writes an OSC 11 background query and returns the screen's reply.
	queryBG := func(t *testing.T, h *Handler) string {
		t.Helper()
		if _, err := h.screen.Write([]byte("\x1b]11;?\x1b\\")); err != nil {
			t.Fatalf("screen write: %v", err)
		}
		return string(h.screen.TakeResponse())
	}

	t.Run("default reports the built-in dark background", func(t *testing.T) {
		t.Parallel()
		const want = "\x1b]11;rgb:0000/0000/0000\x1b\\"
		got := queryBG(t, NewHandler([]string{"/bin/true"}))
		if got != want {
			t.Errorf("OSC 11 query reply = %q, want vt.DefaultTheme's background %q", got, want)
		}
	})

	t.Run("the option reaches the screen", func(t *testing.T) {
		t.Parallel()
		const want = "\x1b]11;rgb:4444/5555/6666\x1b\\"
		h := NewHandler([]string{"/bin/true"}, WithTheme(vt.Theme{
			Foreground: vt.RGB(0x11, 0x22, 0x33),
			Background: vt.RGB(0x44, 0x55, 0x66),
			Cursor:     vt.RGB(0x77, 0x88, 0x99),
		}))
		if got := queryBG(t, h); got != want {
			t.Errorf("OSC 11 query reply = %q, want the configured background %q; the option never reached the screen", got, want)
		}
	})

	// The theme background is also what resolves a DEFAULT background for the
	// minimum-contrast floor, so a consumer that passes both options gets the
	// lift computed against the color it actually paints. Both real consumers
	// pass WithMinimumContrast(4.5) today, so this is the composition that
	// matters and the one a dropped plumb-through would mis-render.
	t.Run("the theme background resolves the contrast floor", func(t *testing.T) {
		t.Parallel()
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
		onBlack := firstFG(t, NewHandler([]string{"/bin/true"}, WithMinimumContrast(4.5)))
		onWhite := firstFG(t, NewHandler([]string{"/bin/true"},
			WithMinimumContrast(4.5),
			WithTheme(vt.Theme{
				Foreground: vt.RGB(0x00, 0x00, 0x00),
				Background: vt.RGB(0xff, 0xff, 0xff),
				Cursor:     vt.RGB(0x00, 0x00, 0x00),
			})))
		if onWhite == onBlack {
			t.Errorf("SGR 34 lifted to %#06x against both a black and a white theme background; the theme never reached liftForContrast", onWhite)
		}
	})
}
