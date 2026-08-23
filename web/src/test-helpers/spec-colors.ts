// Display SPEC — color derivations, authored from the published standards, NOT
// from the engine's wire.go. The conformance tests assert the renderer/engine
// against THESE values; a mismatch is a finding, not something to reconcile by
// copying wire.go.
//
// Sources:
//   - 256-color cube + grayscale ramp: xterm's 256colres.pl / the widely
//     documented formula (see `man xterm`, "256color" and ISO-6429 SGR 38;5).
//   - Truecolor: SGR 38;2;R;G;B / 48;2;R;G;B is literally the RGB triple
//     (ITU / ISO-6429 direct-color).
//   - The 16 base colors (indices 0-15, SGR 30-37 / 90-97) are TERMINAL-DEFINED
//     in the abstract (xterm, VGA, and Windows consoles all pick different RGB).
//     They are a palette CHOICE, not a universal spec. This engine's documented
//     choice is kitty's published default palette, so `kitty16` below pins those
//     exact values from kitty's own `kitty/options/definition.py` —
//     independently of the engine's `wire.go` `basic16RGB`. The conformance test
//     asserts the engine's default basic-16 EQUALS that published palette (a
//     regression or an unintended palette change is a finding), and additionally
//     that the 16 slots are mutually distinct and addressed consistently across
//     SGR forms.

/**
 * cube256 returns the standard RGB for an xterm 256-color CUBE index (16-231).
 * The cube is 6×6×6: index i → n=i-16, (r,g,b) base-6 digits of n, each level
 * mapped 0→0 and k>0→55+40·k (so the six levels are 0,95,135,175,215,255).
 * Throws for out-of-range indices so a test can't silently pass on a bad index.
 */
function cube256(index: number): number {
  if (index < 16 || index > 231) {
    throw new RangeError(`cube256: index ${index} outside 16..231`);
  }
  const n = index - 16;
  const level = (k: number): number => (k === 0 ? 0 : 55 + 40 * k);
  const r = level(Math.floor(n / 36));
  const g = level(Math.floor(n / 6) % 6);
  const b = level(n % 6);
  return (r << 16) | (g << 8) | b;
}

/**
 * grayscale256 returns the standard RGB for an xterm 256-color GRAYSCALE index
 * (232-255): v = 8 + 10·(index-232), giving 24 grays from 8 to 238.
 */
function grayscale256(index: number): number {
  if (index < 232 || index > 255) {
    throw new RangeError(`grayscale256: index ${index} outside 232..255`);
  }
  const v = 8 + 10 * (index - 232);
  return (v << 16) | (v << 8) | v;
}

/**
 * standard256 returns the spec RGB for a 256-color index in the cube or
 * grayscale range (16-255). Indices 0-15 are terminal-defined (palette choice)
 * and intentionally NOT covered — call cube256/grayscale256 semantics via this
 * for 16-255 only.
 */
export function standard256(index: number): number {
  if (index >= 16 && index <= 231) {
    return cube256(index);
  }
  if (index >= 232 && index <= 255) {
    return grayscale256(index);
  }
  throw new RangeError(`standard256: index ${index} is terminal-defined (0-15) or invalid`);
}

/** rgb packs an (r,g,b) truecolor triple to 0xRRGGBB, the SGR direct-color value. */
export function rgb(r: number, g: number, b: number): number {
  return (r << 16) | (g << 8) | b;
}

/**
 * kitty16 is kitty's published default palette — the RGB the base 16 colors
 * (SGR 30-37 normal, 90-97 bright; and 40-47 / 100-107 as background) resolve
 * to. Index 0-7 are the normal colors, 8-15 the bright variants. Authored here
 * from kitty's own `kitty/options/definition.py` (`color0`..`color15`), NOT read
 * from the engine's wire.go. The engine documents this exact palette as its
 * default, so the conformance test pins each slot to `kitty16[i]`; a drift is a
 * finding.
 *
 * Why kitty and not the classic VGA table the engine used to resolve these to:
 * kitty is the only widely-used terminal whose default background is also pure
 * black, which is web-terminal-ui's default, so its slots are chosen against the
 * background this engine renders on. The VGA table reads at 1.58:1 for blue on
 * black, far under the WCAG AA floor of 4.5:1, and GNOME Terminal ships it as a
 * legacy preset named "Linux Console" rather than a default.
 */
export const kitty16: readonly number[] = [
  0x000000, // 0  black
  0xcc0403, // 1  red
  0x19cb00, // 2  green
  0xcecb00, // 3  yellow
  0x0d73cc, // 4  blue
  0xcb1ed1, // 5  magenta
  0x0dcdcd, // 6  cyan
  0xdddddd, // 7  white
  0x767676, // 8  bright black
  0xf2201f, // 9  bright red
  0x23fd00, // 10 bright green
  0xfffd00, // 11 bright yellow
  0x1a8fff, // 12 bright blue
  0xfd28ff, // 13 bright magenta
  0x14ffff, // 14 bright cyan
  0xffffff, // 15 bright white
];

/** hex formats a 0xRRGGBB value as the CSS "#rrggbb" hex notation. */
export function hex(value: number): string {
  return "#" + (value >>> 0).toString(16).padStart(6, "0");
}

/**
 * cssColor formats a 0xRRGGBB SPEC value the way a browser serializes that
 * color when a style declaration is read back — `rgb(r, g, b)`, not the hex
 * notation it was authored in.
 *
 * The DOM conformance tiers compare a rendered span's `style.color` against a
 * spec-derived value, and the renderer writes hex. CSSOM does not round-trip
 * the authored text: reading the property back yields the computed-value
 * serialization from CSS Color 4, so a literal "#ff0000" can never equal what
 * `style.color` reports even when the color is exactly right.
 *
 * Both sides are therefore put through the SAME serializer, the browser's own,
 * rather than the assertion being relaxed. The spec integer stays the
 * authority: `cssColor(standard256(196))` still fails if the renderer produces
 * any other color, and a format-only difference no longer registers as one.
 */
export function cssColor(value: number): string {
  const probe = document.createElement("span");
  probe.style.color = hex(value);
  return probe.style.color;
}
