package terminal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cplieger/web-terminal-engine/v5/vt"
)

// render_golden_test.go generates the cross-language DISPLAY-conformance
// fixture: real escape sequences are fed through the actual vt.Screen, the
// resulting styled runs are encoded as a binary screen frame, and the bytes are
// written to ../render-golden/attributes.bin. The TypeScript tier
// (web/src/render-e2e-golden.test.ts) decodes that frame with the real decoder,
// renders it with the real render.ts, and asserts the SPEC — so this fixture is
// the engine's ACTUAL wire output, not a hand-authored "expected". If the
// engine's color resolution or attribute encoding drifts from the spec, the TS
// test fails.
//
// The row→feature layout is the contract shared with the TS test; keep the two
// in lockstep. Regenerate after an intentional change:
//
//	UPDATE_GOLDEN=1 go test ./terminal/ -run TestRenderGolden
//
// renderGoldenRows is the ordered list of (feature, escape sequence) written one
// per screen row. Each sequence is self-contained (SGR reset appended) so an
// attribute never bleeds into the next row.
var renderGoldenRows = []struct {
	feature string
	seq     string
}{
	{"bold", "\x1b[1mB\x1b[0m"},
	{"italic", "\x1b[3mI\x1b[0m"},
	{"underline", "\x1b[4mU\x1b[0m"},
	{"strikethrough", "\x1b[9mS\x1b[0m"},
	{"overline", "\x1b[53mO\x1b[0m"},
	{"color256-cube-red", "\x1b[38;5;196mR\x1b[0m"},
	{"color256-cube-blue", "\x1b[38;5;21mB\x1b[0m"},
	{"color256-grayscale", "\x1b[38;5;244mG\x1b[0m"},
	{"truecolor-fg", "\x1b[38;2;255;128;0mT\x1b[0m"},
	{"truecolor-bg", "\x1b[48;2;0;0;255mK\x1b[0m"},
	{"underline-color", "\x1b[4;58;2;0;255;0mC\x1b[0m"},
	{"inverse-default", "\x1b[7mV\x1b[0m"},
	{"hyperlink", "\x1b]8;;https://example.com/x\x1b\\L\x1b]8;;\x1b\\"},
	// MUST stay last: this bare URL is longer than the 40-col row, so it
	// SOFT-WRAPS onto the row below (the only entry that occupies two rows).
	// The server's wrap-aware autolinker must stamp BOTH row segments with the
	// full URL + the autolink attr bit; the TS tier asserts each row renders an
	// anchor carrying the complete href (the phone-width wrapped-URL fix).
	{"autolink-wrap", "https://example.com/wrapped/path/abcdefghijkl"},
}

func TestRenderGolden(t *testing.T) {
	const cols = 40
	height := len(renderGoldenRows) + 2 // + 2 blank rows; cursor parked below.
	s := vt.New(height, cols)
	for i, r := range renderGoldenRows {
		// CUP to (row i+1, col 1), 1-indexed, then emit the feature sequence.
		s.Write(fmt.Appendf(nil, "\x1b[%d;1H", i+1))
		s.Write([]byte(r.seq))
	}

	rows := make([][]vt.WireRun, height)
	changed := make([]int, height)
	for y := range height {
		rows[y] = s.RenderRowWire(y)
		changed[y] = y
	}

	// base=0, cursor parked on the last (blank) row, hidden; no bell/alt/clear.
	frame := encodeScreenMsg(0, height, height-1, 0, 0, changed, rows, 0, true, false, false, false, false)

	dir := filepath.Join("..", "render-golden")
	path := filepath.Join(dir, "attributes.bin")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, frame, 0o600); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run UPDATE_GOLDEN=1 go test ./terminal/ -run TestRenderGolden): %v", path, err)
	}
	if !bytes.Equal(frame, want) {
		t.Errorf("render golden drifted (%d bytes now, %d in fixture). If intentional, regenerate with "+
			"UPDATE_GOLDEN=1 and re-run the TS render-e2e-golden test in the same change.", len(frame), len(want))
	}
}

// --- Comprehensive all-codes display fixture (tier-3 CDP dump) ---------------
//
// TestRenderGoldenAllCodes feeds EVERY SGR display code through the real
// vt.Screen — all attributes, all 16 basic fg/bg, the full 256-color palette
// (38;5;0..255), truecolor, and underline-color — one code per row, then writes
// the resulting wire frame (all-codes.bin) plus a layout MANIFEST
// (all-codes.manifest.json) that records which SGR each row carries. The
// Playwright tier (web/e2e/render-all-codes.e2e.test.ts) renders the frame in a
// real headless Chromium, dumps every cell's computed style over CDP, and
// asserts each against the SPEC derived independently from the manifest — the
// manifest is only "what was fed", never the expected output.
//
// Regenerate: UPDATE_GOLDEN=1 go test ./terminal/ -run TestRenderGoldenAllCodes

// allCodesEntry records one row's SGR for the manifest (what was fed, not the
// expected render).
type allCodesEntry struct {
	Row  int    `json:"row"`
	Kind string `json:"kind"` // attr | basicFg | basicBg | color256Fg | truecolorFg | truecolorBg | ulColor
	Code int    `json:"code"` // SGR code, or the 256-color index
	R    int    `json:"r"`
	G    int    `json:"g"`
	B    int    `json:"b"`
}

func buildAllCodesMatrix() (seqs []string, manifest []allCodesEntry) {
	add := func(kind string, code, r, g, b int, seq string) {
		manifest = append(manifest, allCodesEntry{Row: len(seqs), Kind: kind, Code: code, R: r, G: g, B: b})
		seqs = append(seqs, seq)
	}
	// Text attributes (6 = rapid blink, which the engine aliases to blink).
	for _, code := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 21, 53} {
		add("attr", code, 0, 0, 0, fmt.Sprintf("\x1b[%dmX\x1b[0m", code))
	}
	// Basic 16 foreground (30-37 normal, 90-97 bright).
	for _, code := range []int{30, 31, 32, 33, 34, 35, 36, 37, 90, 91, 92, 93, 94, 95, 96, 97} {
		add("basicFg", code, 0, 0, 0, fmt.Sprintf("\x1b[%dmX\x1b[0m", code))
	}
	// Basic 16 background (40-47 normal, 100-107 bright).
	for _, code := range []int{40, 41, 42, 43, 44, 45, 46, 47, 100, 101, 102, 103, 104, 105, 106, 107} {
		add("basicBg", code, 0, 0, 0, fmt.Sprintf("\x1b[%dmX\x1b[0m", code))
	}
	// Full 256-color palette as foreground.
	for i := range 256 {
		add("color256Fg", i, 0, 0, 0, fmt.Sprintf("\x1b[38;5;%dmX\x1b[0m", i))
	}
	// Truecolor foreground samples.
	for _, c := range [][3]int{{255, 0, 0}, {0, 255, 0}, {0, 0, 255}, {255, 128, 0}, {18, 52, 86}} {
		add("truecolorFg", 0, c[0], c[1], c[2], fmt.Sprintf("\x1b[38;2;%d;%d;%dmX\x1b[0m", c[0], c[1], c[2]))
	}
	// Truecolor background samples.
	for _, c := range [][3]int{{0, 0, 255}, {200, 100, 50}} {
		add("truecolorBg", 0, c[0], c[1], c[2], fmt.Sprintf("\x1b[48;2;%d;%d;%dmX\x1b[0m", c[0], c[1], c[2]))
	}
	// Underline color (SGR 58) with an underline (SGR 4) so the decoration shows.
	for _, c := range [][3]int{{0, 255, 0}, {255, 0, 255}} {
		add("ulColor", 0, c[0], c[1], c[2], fmt.Sprintf("\x1b[4;58;2;%d;%d;%dmX\x1b[0m", c[0], c[1], c[2]))
	}
	// Attribute-OFF codes: prove the OFF SGR actually CLEARS the ON attribute
	// end to end (a stuck-on attribute must fail). Each row writes an ON glyph
	// "A" then an OFF glyph "B"; the TS side asserts A carries the attribute and
	// B does NOT. Pairs per ECMA-48 / ANSI: 22 cancels bold(1) + faint(2), 23
	// italic(3), 24 underline(4), 25 blink(5), 27 inverse(7), 28 conceal(8), 29
	// strike(9), 55 overline(53).
	onFor := map[int]int{22: 1, 23: 3, 24: 4, 25: 5, 27: 7, 28: 8, 29: 9, 55: 53}
	for _, off := range []int{22, 23, 24, 25, 27, 28, 29, 55} {
		add("attrOff", off, 0, 0, 0, fmt.Sprintf("\x1b[%dmA\x1b[%dmB\x1b[0m", onFor[off], off))
	}
	// Default-color codes: prove the channel reverts to the terminal default.
	// Row writes a set-color glyph "A" then a default glyph "B"; the TS side
	// asserts B took the default (default fg = theme text color, default bg =
	// transparent, default underline color = currentColor). 39 fg, 49 bg, 59 ul.
	add("defaultColor", 39, 0, 0, 0, "\x1b[31mA\x1b[39mB\x1b[0m")
	add("defaultColor", 49, 0, 0, 0, "\x1b[41mA\x1b[49mB\x1b[0m")
	add("defaultColor", 59, 0, 0, 0, "\x1b[4;58;2;0;255;0mA\x1b[59mB\x1b[0m")
	// Underline sub-parameters (SGR 4:x colon form): 4:2 double underline (single
	// glyph X); 4:0 underline off (ON glyph "A" then OFF glyph "B").
	add("ulStyle", 2, 0, 0, 0, "\x1b[4:2mX\x1b[0m")
	add("ulStyle", 0, 0, 0, 0, "\x1b[4mA\x1b[4:0mB\x1b[0m")
	return seqs, manifest
}

func TestRenderGoldenAllCodes(t *testing.T) {
	const cols = 8
	seqs, manifest := buildAllCodesMatrix()
	height := len(seqs) + 1 // + 1 blank row; cursor parked there.
	s := vt.New(height, cols)
	for i, seq := range seqs {
		s.Write(fmt.Appendf(nil, "\x1b[%d;1H", i+1))
		s.Write([]byte(seq))
	}
	rows := make([][]vt.WireRun, height)
	changed := make([]int, height)
	for y := range height {
		rows[y] = s.RenderRowWire(y)
		changed[y] = y
	}
	frame := encodeScreenMsg(0, height, height-1, 0, 0, changed, rows, 0, true, false, false, false, false)

	dir := filepath.Join("..", "render-golden")
	binPath := filepath.Join(dir, "all-codes.bin")
	manPath := filepath.Join(dir, "all-codes.manifest.json")
	manJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(binPath, frame, 0o600); err != nil {
			t.Fatalf("write bin: %v", err)
		}
		if err := os.WriteFile(manPath, manJSON, 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		return
	}
	wantBin, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read %s (run UPDATE_GOLDEN=1 go test ./terminal/ -run TestRenderGoldenAllCodes): %v", binPath, err)
	}
	if !bytes.Equal(frame, wantBin) {
		t.Errorf("all-codes fixture drifted (%d vs %d bytes); regenerate with UPDATE_GOLDEN=1 and re-run the TS all-codes e2e test.", len(frame), len(wantBin))
	}
	wantMan, err := os.ReadFile(manPath)
	if err != nil {
		t.Fatalf("read manifest %s: %v", manPath, err)
	}
	if !bytes.Equal(manJSON, wantMan) {
		t.Errorf("all-codes manifest drifted; regenerate with UPDATE_GOLDEN=1.")
	}
}

// --- Behavioral display conformance (escape sequence -> engine -> wire -> DOM) ---
//
// esctest2 proves the engine's screen MODEL is correct after an operation
// (clear, erase, cursor move, insert/delete). It says nothing about whether the
// browser RENDERER turns that model into correct DOM. TestRenderGoldenBehavior
// closes that gap end to end: it runs a real escape SEQUENCE through the actual
// vt.Screen, asserts the engine's resulting grid equals a SPEC-AUTHORED expected
// grid (a spec-check of the engine, independent of esctest), then emits the real
// wire frame. The TS side (web/src/render-behavior.test.ts) decodes that frame
// with the real decoder, renders it with the real render.ts, and asserts the DOM
// text grid equals the SAME spec grid — so one hand-authored grid pins both the
// engine and the renderer, and they cannot silently disagree.
//
// Scope: in-place SCREEN operations (the grid the renderer paints). Scroll-to-
// history (which emits scroll frames into the scrollback ring) is covered by
// web/src/render-store.test.ts; this fixture asserts the on-screen grid only.
//
// Regenerate: UPDATE_GOLDEN=1 go test ./terminal/ -run TestRenderGoldenBehavior

// behaviorScenarios is the ordered scenario table. `want` is the SPEC-EXPECTED
// screen (H=4 rows x W=8 cols), each row trailing-trimmed like vt RowString. The
// grid is authored from what the sequence must produce per the VT/ANSI spec, not
// copied from the engine or the renderer.
var behaviorScenarios = []struct {
	name  string
	input string
	want  []string
}{
	// The canonical case: write text, then clear the screen (ED2). The DOM must
	// end up blank — no leftover glyphs.
	{"clear_screen_ed2", "\x1b[1;1HABC\x1b[2;1HDEF\x1b[2J", []string{"", "", "", ""}},
	// ED0 erases from the cursor to the end of the screen.
	{"erase_below_ed0", "\x1b[1;1HAAAA\x1b[2;1HBBBB\x1b[3;1HCCCC\x1b[2;3H\x1b[0J", []string{"AAAA", "BB", "", ""}},
	// ED1 erases from the start of the screen to the cursor (inclusive).
	{"erase_above_ed1", "\x1b[1;1HAAAA\x1b[2;1HBBBB\x1b[3;1HCCCC\x1b[2;3H\x1b[1J", []string{"", "   B", "CCCC", ""}},
	// EL0 erases from the cursor to the end of the line.
	{"erase_line_to_end_el0", "HELLOWOR\x1b[1;3H\x1b[0K", []string{"HE", "", "", ""}},
	// EL1 erases from the start of the line to the cursor (inclusive).
	{"erase_line_to_start_el1", "HELLOWOR\x1b[1;4H\x1b[1K", []string{"    OWOR", "", "", ""}},
	// EL2 erases the whole line.
	{"erase_whole_line_el2", "HELLOWOR\x1b[2K", []string{"", "", "", ""}},
	// CUP positions the cursor; the next glyph must land at that cell.
	{"cursor_position_write", "\x1b[2;4HX", []string{"", "   X", "", ""}},
	// Repositioning mid-line and writing overwrites in place.
	{"overwrite_middle", "AAAAAAAA\x1b[1;3HBB", []string{"AABBAAAA", "", "", ""}},
	// CR returns to column 0; the following text overwrites from the start.
	{"carriage_return", "ABCDEF\rXY", []string{"XYCDEF", "", "", ""}},
	// BS moves left one cell; the next glyph overwrites the char under it.
	{"backspace_overwrite", "ABC\bX", []string{"ABX", "", "", ""}},
	// HT advances to the next 8-column tab stop, clamped to the right margin.
	{"tab_stop", "A\tB", []string{"A      B", "", "", ""}},
	// ICH inserts blanks at the cursor and shifts the rest of the line right.
	{"insert_char_ich", "ABCDEF\x1b[1;3H\x1b[2@", []string{"AB  CDEF", "", "", ""}},
	// DCH deletes chars at the cursor and shifts the rest of the line left.
	{"delete_char_dch", "ABCDEFGH\x1b[1;3H\x1b[2P", []string{"ABEFGH", "", "", ""}},
	// ECH erases n chars at the cursor in place (no shift).
	{"erase_char_ech", "ABCDEFGH\x1b[1;3H\x1b[2X", []string{"AB  EFGH", "", "", ""}},
	// IL inserts a blank line at the cursor row, pushing lines below down.
	{"insert_line_il", "\x1b[1;1HR0\x1b[2;1HR1\x1b[3;1HR2\x1b[4;1HR3\x1b[2;1H\x1b[L", []string{"R0", "", "R1", "R2"}},
	// DL deletes the cursor row, pulling lines below up.
	{"delete_line_dl", "\x1b[1;1HR0\x1b[2;1HR1\x1b[3;1HR2\x1b[4;1HR3\x1b[2;1H\x1b[M", []string{"R0", "R2", "R3", ""}},
	// Autowrap: writing past the right margin continues on the next row.
	{"autowrap_to_next_row", "ABCDEFGHIJKL", []string{"ABCDEFGH", "IJKL", "", ""}},
	// LF at the bottom row scrolls the screen up; the top line leaves the screen
	// (into scrollback) and a blank line appears at the bottom.
	{"newline_scroll_up", "\x1b[1;1HR0\x1b[2;1HR1\x1b[3;1HR2\x1b[4;1HR3\x1b[4;1H\n", []string{"R1", "R2", "R3", ""}},
	// RI (reverse index) at the top row scrolls the screen down; the bottom line
	// leaves the screen and a blank line appears at the top.
	{"reverse_index_scroll_down", "\x1b[1;1HR0\x1b[2;1HR1\x1b[3;1HR2\x1b[4;1HR3\x1b[1;1H\x1bM", []string{"", "R0", "R1", "R2"}},

	// --- Cursor movement: each positions the cursor, moves it, then writes "X";
	// the resulting cell shows where it landed (a spec-check of the engine's
	// motion AND of the wire->DOM cell mapping in a real browser). ---
	// CUU (CSI A): up 2 rows (row4 -> row2).
	{"cursor_up_cuu", "\x1b[4;1H\x1b[2AX", []string{"", "X", "", ""}},
	// CUD (CSI B): down 2 rows (row1 -> row3).
	{"cursor_down_cud", "\x1b[1;1H\x1b[2BX", []string{"", "", "X", ""}},
	// CUF (CSI C): forward 3 columns (col1 -> col4).
	{"cursor_forward_cuf", "\x1b[1;1H\x1b[3CX", []string{"   X", "", "", ""}},
	// CUB (CSI D): back 4 columns (col8 -> col4).
	{"cursor_back_cub", "\x1b[1;8H\x1b[4DX", []string{"   X", "", "", ""}},
	// CHA (CSI G): cursor to absolute column 5.
	{"cursor_col_absolute_cha", "\x1b[1;1H\x1b[5GX", []string{"    X", "", "", ""}},
	// HPA (CSI `): horizontal position absolute, column 6.
	{"cursor_hpa", "\x1b[1;1H\x1b[6\x60X", []string{"     X", "", "", ""}},
	// HPR (CSI a): horizontal position relative +3 (col1 -> col4).
	{"cursor_hpr", "\x1b[1;1H\x1b[3aX", []string{"   X", "", "", ""}},
	// VPA (CSI d): vertical position absolute row3, keeping column 3.
	{"cursor_vpa", "\x1b[1;3H\x1b[3dX", []string{"", "", "  X", ""}},
	// VPR (CSI e): vertical position relative +2 (row1 -> row3), keeping column.
	{"cursor_vpr", "\x1b[1;1H\x1b[2eX", []string{"", "", "X", ""}},
	// CNL (CSI E): cursor next line x2, to column 1 (row1 -> row3).
	{"cursor_next_line_cnl", "\x1b[1;5H\x1b[2EX", []string{"", "", "X", ""}},
	// CPL (CSI F): cursor previous line x2, to column 1 (row4 -> row2).
	{"cursor_prev_line_cpl", "\x1b[4;5H\x1b[2FX", []string{"", "X", "", ""}},
	// CHT (CSI I): cursor forward tabulation from col1 -> right margin (col8;
	// col9 is the next default stop, clamped to the last column).
	{"cursor_forward_tab_cht", "\x1b[1;1H\x1b[IX", []string{"       X", "", "", ""}},
	// CBT (CSI Z): cursor backward tabulation from col8 -> col1.
	{"cursor_back_tab_cbt", "\x1b[1;8H\x1b[ZX", []string{"X", "", "", ""}},

	// --- Index / next-line (ESC D / ESC E); RI is above as reverse_index. ---
	// IND (ESC D): index down one line (row2 -> row3), keeping column.
	{"index_ind", "\x1b[2;1H\x1bDX", []string{"", "", "X", ""}},
	// NEL (ESC E): next line — down one row and to column 1 (row2 col5 -> row3).
	{"next_line_nel", "\x1b[2;5H\x1bEX", []string{"", "", "X", ""}},

	// --- Scroll region contents up/down (CSI S / CSI T) ---
	// SU 2: contents move up 2 rows; R2,R3 remain, two blank rows at the bottom.
	{"scroll_up_su", "\x1b[1;1HR0\x1b[2;1HR1\x1b[3;1HR2\x1b[4;1HR3\x1b[2S", []string{"R2", "R3", "", ""}},
	// SD 2: contents move down 2 rows; two blank rows at top, R0,R1 at bottom.
	{"scroll_down_sd", "\x1b[1;1HR0\x1b[2;1HR1\x1b[3;1HR2\x1b[4;1HR3\x1b[2T", []string{"", "", "R0", "R1"}},

	// --- REP (CSI b): repeat the last printed glyph. "X" + REP 5 => 6 X, then Y. ---
	{"repeat_rep", "X\x1b[5bY", []string{"XXXXXXY", "", "", ""}},

	// --- Tab stops (ESC H = HTS set, HT advance, CSI g = TBC clear) ---
	// HTS sets a stop at col4; HT from col1 then lands on it.
	{"set_tab_hts", "\x1b[1;4H\x1bH\x1b[1;1H\tX", []string{"   X", "", "", ""}},
	// TBC 3 clears every stop; HT from col1 then runs to the right margin (col8).
	{"clear_tabs_tbc", "\x1b[1;1H\x1b[3g\tX", []string{"       X", "", "", ""}},

	// --- Save / restore cursor (ESC 7 = DECSC, ESC 8 = DECRC) ---
	// Save at (2,3), move away to write Z, restore, then write X at the saved spot.
	{"save_restore_cursor_decsc", "\x1b[2;3H\x1b7\x1b[4;7HZ\x1b8X", []string{"", "  X", "", "      Z"}},

	// --- DECALN (ESC # 8): screen-alignment test fills every cell with 'E'. ---
	{"screen_alignment_decaln", "\x1b#8", []string{"EEEEEEEE", "EEEEEEEE", "EEEEEEEE", "EEEEEEEE"}},

	// --- DECSTBM (CSI Pt;Pb r): a scroll region confines LF scrolling. Region
	// rows 2-3; an LF at the bottom margin scrolls only those rows, leaving the
	// rows outside the region untouched, then "X" writes at the freed bottom row.
	{"scroll_region_decstbm", "\x1b[1;1HR0\x1b[2;1HR1\x1b[3;1HR2\x1b[4;1HR3\x1b[2;3r\x1b[3;1H\nX", []string{"R0", "R2", "X", "R3"}},

	// --- Origin mode + left/right margins (?69h + CSI Pl;Pr s + ?6h): CUP (1,1)
	// is relative to the top/left margin. Region rows2-3, cols3-6, origin on =>
	// (1,1) maps to (row2, col3). ---
	{"origin_mode_margins", "\x1b[2;3r\x1b[?69h\x1b[3;6s\x1b[?6h\x1b[1;1HX", []string{"", "  X", "", ""}},

	// --- Rectangular-area editing (DEC) ---
	// DECFRA (CSI Pch;Pt;Pl;Pb;Pr $ x): fill rows2-3 x cols3-5 with 'X' (88).
	{"fill_rect_decfra", "\x1b[88;2;3;3;5$x", []string{"", "  XXX", "  XXX", ""}},
	// DECERA (CSI Pt;Pl;Pb;Pr $ z): fill the screen with 'E' (DECALN), then erase
	// rows2-3 x cols3-5 back to blanks.
	{"erase_rect_decera", "\x1b#8\x1b[2;3;3;5$z", []string{"EEEEEEEE", "EE   EEE", "EE   EEE", "EEEEEEEE"}},
	// DECCRA (CSI Pts;Pls;Pbs;Prs;Pps;Ptd;Pld;Ppd $ v): copy rows1 cols1-2 ("AB")
	// to row3 col1.
	{"copy_rect_deccra", "\x1b[1;1HAB\x1b[1;1;1;2;1;3;1;1$v", []string{"AB", "", "AB", ""}},

	// --- Column shift + insert/delete (SL/SR, DECIC/DECDC) ---
	// SL (CSI Ps SP @): shift the whole region left 2 columns (A,B fall off).
	{"shift_left_sl", "\x1b[1;1HABCDEFGH\x1b[2 @", []string{"CDEFGH", "", "", ""}},
	// SR (CSI Ps SP A): shift the whole region right 2 columns (G,H fall off).
	{"shift_right_sr", "\x1b[1;1HABCDEFGH\x1b[2 A", []string{"  ABCDEF", "", "", ""}},
	// DECIC (CSI Ps ' }): insert 2 blank columns at the cursor column across the
	// region, pushing the rest of each row right.
	{"insert_col_decic", "\x1b[1;1HAAAAAAAA\x1b[2;1HBBBBBBBB\x1b[1;3H\x1b[2'}", []string{"AA  AAAA", "BB  BBBB", "", ""}},
	// DECDC (CSI Ps ' ~): delete 2 columns at the cursor column across the region,
	// pulling the rest of each row left.
	{"delete_col_decdc", "\x1b[1;1HAAAAAAAA\x1b[2;1HBBBBBBBB\x1b[1;3H\x1b[2'~", []string{"AAAAAA", "BBBBBB", "", ""}},

	// --- Back / forward index (ESC 6 / ESC 9): move one column; away from a
	// margin this is a plain move. ---
	// DECBI from col4 (not at the left margin) -> col3.
	{"back_index_decbi", "\x1b[1;4H\x1b6X", []string{"  X", "", "", ""}},
	// DECFI from col4 (not at the right margin) -> col5.
	{"forward_index_decfi", "\x1b[1;4H\x1b9X", []string{"    X", "", "", ""}},

	// --- Selective erase (DECSCA marks cells protected; DECSED/DECSEL spare
	// them). "AB" is protected (DECSCA 1), "CD" is not (DECSCA 0). ---
	// DECSED (CSI ? Ps J): selectively erase the display, sparing protected "AB".
	{"selective_erase_display_decsed", "\x1b[1;1H\x1b[1\"qAB\x1b[0\"qCD\x1b[?2J", []string{"AB", "", "", ""}},
	// DECSEL (CSI ? Ps K): selectively erase the line, sparing protected "AB".
	{"selective_erase_line_decsel", "\x1b[1;1H\x1b[1\"qAB\x1b[0\"qCD\x1b[1;1H\x1b[?2K", []string{"AB", "", "", ""}},
}

// behaviorEntry is one scenario's manifest record. Frame marshals to base64
// (encoding/json encodes []byte that way), so the TS side decodes it directly.
type behaviorEntry struct {
	Name   string   `json:"name"`
	Input  string   `json:"input"` // the escape sequence fed to the engine (documentation)
	Want   []string `json:"want"`  // spec-expected screen grid, trailing-trimmed rows
	CurRow int      `json:"curRow"`
	CurCol int      `json:"curCol"`
	Frame  []byte   `json:"frame"` // the engine's real wire frame for this scenario
}

func TestRenderGoldenBehavior(t *testing.T) {
	const rows, cols = 4, 8
	manifest := make([]behaviorEntry, 0, len(behaviorScenarios))
	for _, sc := range behaviorScenarios {
		s := vt.New(rows, cols)
		s.Write([]byte(sc.input))

		// Spec-check the ENGINE: its resulting grid must equal the authored grid.
		if len(sc.want) != rows {
			t.Fatalf("%s: want grid has %d rows, screen has %d", sc.name, len(sc.want), rows)
		}
		for y := range rows {
			if got := s.RowString(y); got != sc.want[y] {
				t.Errorf("%s: engine RowString(%d) = %q, want %q (spec grid)", sc.name, y, got, sc.want[y])
			}
		}

		curRow, curCol := s.CursorPos()
		wire := make([][]vt.WireRun, rows)
		changed := make([]int, rows)
		for y := range rows {
			wire[y] = s.RenderRowWire(y)
			changed[y] = y
		}
		// Cursor hidden so the grid-text assertion is not disturbed by a cursor
		// span; the renderer's cursor placement is covered separately.
		frame := encodeScreenMsg(0, rows, curRow, curCol, 0, changed, wire, 0, true, false, false, false, false)
		manifest = append(manifest, behaviorEntry{
			Name: sc.name, Input: sc.input, Want: sc.want, CurRow: curRow, CurCol: curCol, Frame: frame,
		})
	}

	manJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal behavior manifest: %v", err)
	}
	dir := filepath.Join("..", "render-golden")
	manPath := filepath.Join(dir, "behavior.manifest.json")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(manPath, manJSON, 0o600); err != nil {
			t.Fatalf("write behavior manifest: %v", err)
		}
		return
	}
	want, err := os.ReadFile(manPath)
	if err != nil {
		t.Fatalf("read %s (run UPDATE_GOLDEN=1 go test ./terminal/ -run TestRenderGoldenBehavior): %v", manPath, err)
	}
	if !bytes.Equal(manJSON, want) {
		t.Errorf("behavior fixture drifted; regenerate with UPDATE_GOLDEN=1 and re-run the TS render-behavior test.")
	}
}
