package vt

import "math"

// Minimum-contrast lift.
//
// A terminal program picks a palette SLOT, never an RGB value: kiro-cli writes
// SGR 34 for a link and cannot know what index 4 resolves to here, nor what the
// client paints behind it. The two emulators closest to this engine's shape
// answer that the same way. xterm.js exposes `minimumContrastRatio` (VS Code
// sets it to 4.5, WCAG AA, by default) and iTerm2 ships a "Minimum Contrast"
// control whose documentation states the same cause: because ANSI colors are
// configurable, apps cannot avoid the collision. Both shift the TEXT color
// toward white or black, and neither touches the background. WithMinimumContrast
// is this engine's equivalent.
//
// The lift runs in makeRunWithURL, on a run's resolved (fg, bg) pair. It is
// deliberately NOT in colorToWire, which also answers OSC 4 palette queries
// (osc.go): a query must report the palette entry the application set, not the
// value the renderer painted over it.

// contrastSearchSteps bounds the blend search. Each step halves the interval,
// so 8 steps resolve the blend factor to better than 1/256 — finer than an
// 8-bit channel can express.
const contrastSearchSteps = 8

// srgbToLinear converts one 8-bit sRGB channel to linear light, per the WCAG 2
// relative-luminance definition.
func srgbToLinear(c uint8) float64 {
	v := float64(c) / 255
	if v <= 0.03928 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

// relLuminance returns the WCAG 2 relative luminance of a 0xRRGGBB color.
func relLuminance(c int32) float64 {
	r := srgbToLinear(uint8(c >> 16)) // #nosec G115 -- channel byte
	g := srgbToLinear(uint8(c >> 8))  // #nosec G115 -- channel byte
	b := srgbToLinear(uint8(c))       // #nosec G115 -- channel byte
	return 0.2126*r + 0.7152*g + 0.0722*b
}

// contrastRatio returns the WCAG 2 contrast ratio between two 0xRRGGBB colors,
// from 1 (identical luminance) to 21 (black against white).
func contrastRatio(a, b int32) float64 {
	la, lb := relLuminance(a), relLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// blend mixes from toward to by t (0 returns from, 1 returns to), per channel in
// sRGB space. Blending toward pure white or pure black moves luminance
// monotonically, which is what makes the binary search below valid.
func blend(from, to int32, t float64) int32 {
	mix := func(shift int) int32 {
		a := float64((from >> shift) & 0xff)
		b := float64((to >> shift) & 0xff)
		return int32(math.Round(a + t*(b-a)))
	}
	return mix(16)<<16 | mix(8)<<8 | mix(0)
}

// ensureContrast returns fg lifted just far enough to reach ratio against bg, or
// fg unchanged when it already does. It blends fg toward white or black, away
// from bg, which is what iTerm2's Minimum Contrast and xterm.js's
// minimumContrastRatio both do. Hue washes out as the lift grows; that is the
// accepted trade for legibility, and a palette chosen for the background keeps
// the lift small. When neither extreme can reach the ratio (a mid-luminance
// background), it returns whichever extreme gets closest.
func ensureContrast(fg, bg int32, ratio float64) int32 {
	if contrastRatio(fg, bg) >= ratio {
		return fg
	}
	// Push away from the background: lighten when the text is already the
	// lighter of the pair (every dark theme), darken otherwise. A tie lightens,
	// because a tie means both sit at the same luminance and the direction is
	// then arbitrary.
	first, second := int32(0xffffff), int32(0x000000)
	if relLuminance(bg) > relLuminance(fg) {
		first, second = second, first
	}
	if c, ok := blendToContrast(fg, bg, first, ratio); ok {
		return c
	}
	if c, ok := blendToContrast(fg, bg, second, ratio); ok {
		return c
	}
	if contrastRatio(first, bg) >= contrastRatio(second, bg) {
		return first
	}
	return second
}

// blendToContrast binary-searches the smallest blend of fg toward target that
// reaches ratio against bg. ok is false when even target itself falls short, so
// the caller can try the other direction instead of settling for a blend that
// misses the floor anyway.
func blendToContrast(fg, bg, target int32, ratio float64) (int32, bool) {
	if contrastRatio(target, bg) < ratio {
		return fg, false
	}
	lo, hi := 0.0, 1.0
	best := target // the full blend is known to satisfy the ratio
	for range contrastSearchSteps {
		mid := (lo + hi) / 2
		c := blend(fg, target, mid)
		if contrastRatio(c, bg) >= ratio {
			best = c
			hi = mid
		} else {
			lo = mid
		}
	}
	return best, true
}

// liftForContrast applies the configured floor to a run's resolved foreground.
// It resolves a default background (-1) to the theme background, because that is
// what the client's CSS paints for it, and swaps the theme pair under DECSCNM
// reverse video, because the client's CSS swaps it too.
//
// A DEFAULT foreground is returned untouched. The client's CSS owns that color
// (--text) and swaps it with --bg under DECSCNM, so baking a lifted RGB here
// would freeze the pair and defeat a consumer's own theme override. A consumer
// whose own default pair is illegible fixes its CSS, not its terminal.
func (s *Screen) liftForContrast(fg, bg int32) int32 {
	if fg == wireDefaultColor {
		return fg
	}
	themeBG := s.theme.Background
	if s.ReverseVideo {
		themeBG = s.theme.Foreground
	}
	effBG := bg
	if effBG == wireDefaultColor {
		effBG = themeBG
	}
	return ensureContrast(fg, effBG, s.minContrast)
}
