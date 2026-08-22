package vt

import (
	"math"
	"testing"
)

// black is the default theme background every case below renders against, which
// is also web-terminal-ui's default `--bg`.
const black = int32(0x000000)

// wcagAA is the WCAG 2 contrast floor for body text, and the value VS Code
// applies to its integrated terminal.
const wcagAA = 4.5

// TestContrastRatioAnchors pins the ratio function against the three values the
// WCAG definition fixes exactly, so a later refactor of the luminance math
// cannot drift silently.
func TestContrastRatioAnchors(t *testing.T) {
	cases := []struct {
		name string
		a, b int32
		want float64
	}{
		{"black on white is the maximum", 0x000000, 0xffffff, 21},
		{"white on white is the minimum", 0xffffff, 0xffffff, 1},
		{"mid grey #767676 on black is the AA boundary", 0x767676, black, 4.62},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contrastRatio(tc.a, tc.b)
			if math.Abs(got-tc.want) > 0.01 {
				t.Errorf("contrastRatio(%#06x, %#06x) = %.3f, want %.2f", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestContrastRatioIsSymmetric checks the argument order does not matter, which
// the caller relies on: liftForContrast passes (fg, bg) while the direction
// choice inside ensureContrast compares luminances the other way round.
func TestContrastRatioIsSymmetric(t *testing.T) {
	for _, c := range []int32{0x000000, 0x0d73cc, 0xcc0403, 0xffffff, 0x767676} {
		if fwd, rev := contrastRatio(c, black), contrastRatio(black, c); fwd != rev {
			t.Errorf("contrastRatio not symmetric for %#06x: %.4f vs %.4f", c, fwd, rev)
		}
	}
}

// TestEnsureContrastReachesTheFloor is the core guarantee: whatever the input,
// the returned color meets the requested ratio. The cases walk the palette slots
// that fail on black, the VGA blue this engine used to resolve SGR 34 to (the
// reported bug: 1.58:1, unreadable), and a light background so the darkening
// direction is covered too.
func TestEnsureContrastReachesTheFloor(t *testing.T) {
	cases := []struct {
		name   string
		fg, bg int32
		ratio  float64
	}{
		{"legacy VGA blue on black", 0x0000aa, black, wcagAA},
		{"palette blue on black", 0x0d73cc, black, wcagAA},
		{"palette red on black", 0xcc0403, black, wcagAA},
		{"palette bright-black on black", 0x767676, black, wcagAA},
		{"black on black", black, black, wcagAA},
		{"dark text on a white background darkens", 0x0000aa, 0xffffff, 7},
		{"light text on a light background darkens", 0xeeeeee, 0xffffff, wcagAA},
		{"a demanding ratio still lands", 0x0d73cc, black, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ensureContrast(tc.fg, tc.bg, tc.ratio)
			if r := contrastRatio(got, tc.bg); r < tc.ratio {
				t.Errorf("ensureContrast(%#06x, %#06x, %.1f) = %#06x at %.2f:1, below the floor",
					tc.fg, tc.bg, tc.ratio, got, r)
			}
		})
	}
}

// TestEnsureContrastLeavesLegibleColorsAlone is the other half of the contract:
// a color already above the floor must come back byte-identical, or the floor
// would recolor the whole screen instead of the illegible parts of it.
func TestEnsureContrastLeavesLegibleColorsAlone(t *testing.T) {
	// Every palette slot that already clears AA on black, plus the default text
	// color and a truecolor value an application picked itself.
	for _, fg := range []int32{
		0x19cb00, 0xcecb00, 0xcb1ed1, 0x0dcdcd, 0xdddddd,
		0xf2201f, 0x23fd00, 0xfffd00, 0x1a8fff, 0xfd28ff, 0x14ffff, 0xffffff,
		0xddddE1, 0x7f8fff,
	} {
		if got := ensureContrast(fg, black, wcagAA); got != fg {
			t.Errorf("ensureContrast(%#06x, black, %.1f) = %#06x, want it untouched (%.2f:1 already passes)",
				fg, wcagAA, got, contrastRatio(fg, black))
		}
	}
}

// TestEnsureContrastLiftIsMinimal checks the search returns the SMALLEST lift
// that clears the floor rather than jumping to white: the whole point of
// blending is that the color keeps as much of its hue as legibility allows. A
// lift that overshoots the floor by a wide margin is a washed-out color.
func TestEnsureContrastLiftIsMinimal(t *testing.T) {
	got := ensureContrast(0x0000aa, black, wcagAA)
	r := contrastRatio(got, black)
	if r > wcagAA+0.15 {
		t.Errorf("lift of VGA blue overshot: %#06x at %.2f:1, want just above %.1f", got, r, wcagAA)
	}
	if got == 0xffffff {
		t.Error("lift of VGA blue went all the way to white; the blend search did not run")
	}
	// Blue is the dominant channel of the input, so it must stay dominant.
	if b, rr := got&0xff, (got>>16)&0xff; b <= rr {
		t.Errorf("lift of VGA blue lost its hue: %#06x has blue %#02x <= red %#02x", got, b, rr)
	}
}

// TestEnsureContrastTreatsTheFloorAsInclusive covers the three places a color is
// compared against the requested ratio. WCAG states every threshold as "at
// least" a ratio, so a color sitting exactly ON the floor passes and nothing
// above it has to move.
//
// Each case asks for the ratio one specific color already achieves against the
// background, which is why the floors are derived here instead of written as
// decimals: the boundary is only reached when the requested ratio and the
// measured one are bit-identical, and no decimal literal expresses that.
func TestEnsureContrastTreatsTheFloorAsInclusive(t *testing.T) {
	const midGrey = int32(0x808080)
	cases := []struct {
		name   string
		fg, bg int32
		ratio  float64
		want   int32
	}{
		{
			// Pure red measures 1.0124:1 against mid grey. Asked for exactly
			// that, the pair already complies and the color is handed back
			// untouched rather than nudged one step toward black.
			name: "a pair already at the floor is untouched",
			fg:   0xff0000, bg: midGrey,
			ratio: contrastRatio(0xff0000, midGrey),
			want:  0xff0000,
		},
		{
			// White is the best the lightening direction can do here, 3.9494:1.
			// A floor exactly at that ceiling is still reachable, so the lift
			// keeps its direction instead of abandoning it and darkening.
			name: "an extreme exactly at the floor is still reachable",
			fg:   0xc0c0c0, bg: midGrey,
			ratio: contrastRatio(0xffffff, midGrey),
			want:  0xffffff,
		},
		{
			// 0xe0e0e0 measures 2.9918:1 against mid grey. Asked for exactly
			// that, the search settles on it instead of blending further and
			// washing out more of the color than legibility needs.
			name: "the lift stops at the blend that exactly meets the floor",
			fg:   0xc0c0c0, bg: midGrey,
			ratio: contrastRatio(0xe0e0e0, midGrey),
			want:  0xe0e0e0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ensureContrast(tc.fg, tc.bg, tc.ratio); got != tc.want {
				t.Errorf("ensureContrast(%#06x, %#06x, %.4f) = %#06x, want %#06x",
					tc.fg, tc.bg, tc.ratio, got, tc.want)
			}
		})
	}
}

// TestEnsureContrastLightensOnALuminanceTie pins the documented tie rule. When
// the text and its background sit at the same luminance there is no "away from
// the background" direction to pick, and the function lightens. Grey on grey is
// that case: it fails any floor above 1:1, and both extremes can reach a modest
// one, so the tie is what decides which way the color moves.
func TestEnsureContrastLightensOnALuminanceTie(t *testing.T) {
	const midGrey = int32(0x808080)
	got := ensureContrast(midGrey, midGrey, 3)
	if relLuminance(got) <= relLuminance(midGrey) {
		t.Errorf("ensureContrast(%#06x, %#06x, 3) = %#06x at luminance %.4f, want lighter than %.4f",
			midGrey, midGrey, got, relLuminance(got), relLuminance(midGrey))
	}
	if r := contrastRatio(got, midGrey); r < 3 {
		t.Errorf("ensureContrast(%#06x, %#06x, 3) = %#06x at %.2f:1, below the floor",
			midGrey, midGrey, got, r)
	}
}

// TestEnsureContrastImpossibleFloorPicksTheBestExtreme covers the mid-luminance
// background where NEITHER white nor black reaches the requested ratio. The
// function cannot satisfy the caller, so it must return the closest it can get
// instead of leaving the unreadable input in place.
func TestEnsureContrastImpossibleFloorPicksTheBestExtreme(t *testing.T) {
	const midGrey = int32(0x777777) // ~4.54:1 to black, ~4.48:1 to white
	got := ensureContrast(midGrey, midGrey, 21)
	if got != 0x000000 && got != 0xffffff {
		t.Fatalf("ensureContrast on an impossible floor = %#06x, want an extreme", got)
	}
	best := math.Max(contrastRatio(0x000000, midGrey), contrastRatio(0xffffff, midGrey))
	if r := contrastRatio(got, midGrey); math.Abs(r-best) > 0.001 {
		t.Errorf("picked %#06x at %.3f:1; the better extreme reaches %.3f:1", got, r, best)
	}
}

// TestBlendEndpoints checks the blend's two fixed points, which the binary
// search assumes: t=0 is the source and t=1 is the target.
func TestBlendEndpoints(t *testing.T) {
	const from, to = int32(0x0d73cc), int32(0xffffff)
	if got := blend(from, to, 0); got != from {
		t.Errorf("blend(t=0) = %#06x, want %#06x", got, from)
	}
	if got := blend(from, to, 1); got != to {
		t.Errorf("blend(t=1) = %#06x, want %#06x", got, to)
	}
}

// TestBlendTowardExtremesIsMonotonic checks the property the binary search
// depends on: blending further toward white never lowers luminance (and toward
// black never raises it). If this broke, the search could settle below the floor.
func TestBlendTowardExtremesIsMonotonic(t *testing.T) {
	for _, target := range []int32{0xffffff, 0x000000} {
		prev := relLuminance(0x0d73cc)
		for i := 1; i <= 32; i++ {
			l := relLuminance(blend(0x0d73cc, target, float64(i)/32))
			if target == 0xffffff && l < prev-1e-12 {
				t.Fatalf("luminance fell while blending toward white at step %d", i)
			}
			if target == 0x000000 && l > prev+1e-12 {
				t.Fatalf("luminance rose while blending toward black at step %d", i)
			}
			prev = l
		}
	}
}

// TestWithMinimumContrastClamps pins the accepted range. The clamp exists so a
// consumer cannot ask for a ratio no color pair can express, and so a garbage
// value degrades to "off" instead of poisoning every comparison with NaN.
func TestWithMinimumContrastClamps(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"below the range clamps to off", 0, MinimumContrastOff},
		{"negative clamps to off", -5, MinimumContrastOff},
		{"off passes through", MinimumContrastOff, MinimumContrastOff},
		{"AA passes through", wcagAA, wcagAA},
		{"above the range clamps to the maximum", 100, 21},
		{"NaN degrades to off", math.NaN(), MinimumContrastOff},
		{"+Inf clamps to the maximum", math.Inf(1), 21},
		{"-Inf clamps to off", math.Inf(-1), MinimumContrastOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(1, 4, WithMinimumContrast(tc.in))
			if s.minContrast != tc.want {
				t.Errorf("WithMinimumContrast(%v) → minContrast = %v, want %v", tc.in, s.minContrast, tc.want)
			}
		})
	}
}

// TestMinimumContrastOffByDefault is the compatibility guarantee: a Screen built
// without the option renders the palette entry the application selected, so
// upgrading the engine cannot silently recolor an existing consumer.
func TestMinimumContrastOffByDefault(t *testing.T) {
	s := New(1, 8)
	if s.minContrast != MinimumContrastOff {
		t.Errorf("default minContrast = %v, want %v", s.minContrast, MinimumContrastOff)
	}
	s.Write([]byte("\x1b[34mX")) // SGR 34: palette blue, 4.34:1 on black
	runs := s.RenderRowWire(0)
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	if runs[0].F != 0x0d73cc {
		t.Errorf("F = %#06x, want the raw palette entry 0x0d73cc", runs[0].F)
	}
}

// TestMinimumContrastLiftsSGR34 is the reported bug, end to end through the
// public write+render path: kiro-cli colors a link with SGR 34, and the run that
// reaches the client must be legible on the theme background.
func TestMinimumContrastLiftsSGR34(t *testing.T) {
	s := New(1, 8, WithMinimumContrast(wcagAA))
	s.Write([]byte("\x1b[34mlink"))
	runs := s.RenderRowWire(0)
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	if r := contrastRatio(runs[0].F, black); r < wcagAA {
		t.Errorf("SGR 34 reached the client as %#06x at %.2f:1 on black, below %.1f", runs[0].F, r, wcagAA)
	}
}

// TestMinimumContrastUsesTheRunBackground checks the floor measures against the
// run's OWN background, not the theme's. White text on a white background is the
// case a theme-only comparison would wave through.
func TestMinimumContrastUsesTheRunBackground(t *testing.T) {
	s := New(1, 8, WithMinimumContrast(wcagAA))
	// SGR 97 = bright white fg, SGR 107 = bright white bg.
	s.Write([]byte("\x1b[97;107mX"))
	runs := s.RenderRowWire(0)
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	if runs[0].B != 0xffffff {
		t.Fatalf("B = %#06x, want the untouched background 0xffffff", runs[0].B)
	}
	if r := contrastRatio(runs[0].F, runs[0].B); r < wcagAA {
		t.Errorf("white on white left at %#06x/%#06x, %.2f:1", runs[0].F, runs[0].B, r)
	}
}

// TestMinimumContrastNeverTouchesTheBackground pins iTerm2's stated rule, which
// this engine follows: the floor moves text, never the surface behind it.
// Recoloring a background would corrupt an application's own layout (a table
// fill, a selected row, a progress bar).
func TestMinimumContrastNeverTouchesTheBackground(t *testing.T) {
	// SGR 30 on 40: black on black, the worst case for the floor to be tempted.
	s := New(1, 8, WithMinimumContrast(21))
	s.Write([]byte("\x1b[30;40mX"))
	runs := s.RenderRowWire(0)
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	if runs[0].B != black {
		t.Errorf("B = %#06x, want the untouched background %#06x", runs[0].B, black)
	}
}

// TestMinimumContrastSkipsDefaultForeground protects the client's CSS contract:
// -1 means "paint --text", which the client swaps with --bg under DECSCNM and a
// consumer may override in its own stylesheet. Baking an RGB here would freeze
// both. A consumer whose default pair is illegible fixes its CSS.
func TestMinimumContrastSkipsDefaultForeground(t *testing.T) {
	s := New(1, 8, WithMinimumContrast(21))
	s.Write([]byte("plain"))
	runs := s.RenderRowWire(0)
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	if runs[0].F != wireDefaultColor {
		t.Errorf("F = %#06x, want the default marker %d", runs[0].F, wireDefaultColor)
	}
}

// TestMinimumContrastSkipsConcealed checks SGR 8 text is never pushed away from
// its background. The client hides it with `visibility`, but a client that
// implements conceal the classic way (paint the text in the background color)
// would otherwise be handed a foreground that reveals it.
func TestMinimumContrastSkipsConcealed(t *testing.T) {
	s := New(1, 8, WithMinimumContrast(21))
	s.Write([]byte("\x1b[8;34msecret"))
	runs := s.RenderRowWire(0)
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	if runs[0].A&64 == 0 {
		t.Fatalf("A = %d, expected the hidden bit (64) to be set", runs[0].A)
	}
	if runs[0].F != 0x0d73cc {
		t.Errorf("concealed F = %#06x, want the unlifted palette entry 0x0d73cc", runs[0].F)
	}
}

// TestMinimumContrastFollowsReverseVideo covers DECSCNM. The client swaps
// --text/--bg in CSS for the whole screen, so a run with a default background is
// actually sitting on the theme FOREGROUND, and the floor has to measure against
// that. Light-blue text is legible on black and illegible on near-white.
func TestMinimumContrastFollowsReverseVideo(t *testing.T) {
	const lightBlue = "\x1b[38;2;170;200;255m" // ~2.1:1 against the theme fg
	s := New(1, 8, WithMinimumContrast(wcagAA))
	s.Write([]byte("\x1b[?5h" + lightBlue + "X")) // DECSCNM on
	if !s.ReverseVideo {
		t.Fatal("DECSCNM did not take effect")
	}
	runs := s.RenderRowWire(0)
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	effectiveBG := s.theme.Foreground
	if r := contrastRatio(runs[0].F, effectiveBG); r < wcagAA {
		t.Errorf("under DECSCNM, F = %#06x is %.2f:1 against the swapped background %#06x",
			runs[0].F, r, effectiveBG)
	}
	// Same color with DECSCNM off needs no lift, which proves the swap is what
	// drove the case above rather than the color being illegible either way.
	plain := New(1, 8, WithMinimumContrast(wcagAA))
	plain.Write([]byte(lightBlue + "X"))
	if runs := plain.RenderRowWire(0); len(runs) == 0 || runs[0].F != 0xaac8ff {
		t.Errorf("without DECSCNM: F = %#06x, want the untouched 0xaac8ff", runs[0].F)
	}
}

// TestMinimumContrastRespectsPaletteOverride checks the floor runs AFTER an
// OSC 4 override, which is the case a palette choice alone cannot cover: an
// application is free to set index 4 to something unreadable at any time.
func TestMinimumContrastRespectsPaletteOverride(t *testing.T) {
	s := New(1, 8, WithMinimumContrast(wcagAA))
	s.Write([]byte("\x1b]4;4;rgb:00/00/20\x07")) // index 4 -> near-black blue
	s.Write([]byte("\x1b[34mX"))
	runs := s.RenderRowWire(0)
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	if r := contrastRatio(runs[0].F, black); r < wcagAA {
		t.Errorf("overridden index 4 reached the client as %#06x at %.2f:1", runs[0].F, r)
	}
}

// TestMinimumContrastLeavesOSC4QueryAlone is why the lift lives in
// makeRunWithURL and not in colorToWire: an OSC 4 query must report the palette
// entry the application set, not the value the renderer painted over it.
// Otherwise an application that sets a color and reads it back sees a different
// one, and a "restore my palette" round trip corrupts itself.
func TestMinimumContrastLeavesOSC4QueryAlone(t *testing.T) {
	s := New(1, 8, WithMinimumContrast(21))
	s.Write([]byte("\x1b]4;4;?\x07"))
	const want = "\x1b]4;4;rgb:0d0d/7373/cccc\x1b\\"
	if got := string(s.response); got != want {
		t.Errorf("OSC 4 query = %q, want %q (the raw palette entry)", got, want)
	}
}

// TestMinimumContrastLiftsAKiroCLILink replays the exact byte sequence kiro-cli
// writes for a markdown link, captured from a live PTY session: an OSC 8
// hyperlink wrapping SGR 34 link text, then the closing OSC 8. This is the
// reported bug in its real shape, so it pins that the run reaching the client
// keeps the href AND carries a legible color.
func TestMinimumContrastLiftsAKiroCLILink(t *testing.T) {
	const captured = "see \x1b]8;;https://example.com/x\x07\x1b[34mthe example page\x1b[39m\x1b]8;;\x07 here"
	s := New(1, 40, WithMinimumContrast(wcagAA))
	s.Write([]byte(captured))

	var link *WireRun
	for i, run := range s.RenderRowWire(0) {
		if run.U != "" {
			link = &s.RenderRowWire(0)[i]
			break
		}
	}
	if link == nil {
		t.Fatal("no run carried the OSC 8 hyperlink")
	}
	if link.U != "https://example.com/x" {
		t.Errorf("U = %q, want the app's href", link.U)
	}
	if link.T != "the example page" {
		t.Errorf("T = %q, want the link text", link.T)
	}
	if r := contrastRatio(link.F, black); r < wcagAA {
		t.Errorf("link text F = %#06x at %.2f:1 on black, below %.1f", link.F, r, wcagAA)
	}
}

// FuzzEnsureContrast asserts the two invariants over arbitrary color pairs and
// ratios: the result always meets the requested floor when the floor is
// reachable at all, and a pair that already passes is returned untouched.
func FuzzEnsureContrast(f *testing.F) {
	f.Add(int32(0x0000aa), black, 4.5)
	f.Add(int32(0x0d73cc), black, 4.5)
	f.Add(black, black, 21.0)
	f.Add(int32(0xffffff), int32(0xffffff), 4.5)
	f.Add(int32(0x777777), int32(0x777777), 21.0)
	f.Fuzz(func(t *testing.T, fg, bg int32, ratio float64) {
		if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
			t.Skip("the option clamps these before they reach ensureContrast")
		}
		fg &= 0xffffff
		bg &= 0xffffff
		ratio = math.Min(21, math.Max(1, ratio))

		got := ensureContrast(fg, bg, ratio)
		if got < 0 || got > 0xffffff {
			t.Fatalf("ensureContrast(%#06x, %#06x, %v) = %#x, outside 24-bit RGB", fg, bg, ratio, got)
		}
		if contrastRatio(fg, bg) >= ratio {
			if got != fg {
				t.Errorf("a passing color was changed: %#06x → %#06x", fg, got)
			}
			return
		}
		// The floor is unreachable only when neither extreme clears it; then the
		// contract is the best available extreme, checked in its own test.
		reachable := math.Max(contrastRatio(0xffffff, bg), contrastRatio(black, bg)) >= ratio
		if r := contrastRatio(got, bg); reachable && r < ratio {
			t.Errorf("ensureContrast(%#06x, %#06x, %v) = %#06x at %.4f:1, below a reachable floor",
				fg, bg, ratio, got, r)
		}
	})
}
