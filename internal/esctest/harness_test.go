package esctest

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/web-terminal-engine/v5/vt"
)

// TestConformance runs the esctest2 suite against the engine's VT and asserts
// that the set of failing tests matches the checked-in known-deviations
// allowlist (known_failures.txt). It is skipped unless ESCTEST2_DIR points at
// an esctest2 checkout (run `bash scripts/esctest.sh` to fetch it and drive
// this test), so a normal `go test ./...` stays fast and dependency-free.
//
// When known_failures.txt is absent the test runs in triage mode: it logs the
// full failing set without asserting, so a first run can be inspected to seed
// the allowlist.
func TestConformance(t *testing.T) {
	dir := os.Getenv("ESCTEST2_DIR")
	if dir == "" {
		t.Skip("ESCTEST2_DIR not set; run scripts/esctest.sh to fetch esctest2 and drive this test")
	}

	level := 5 // aim at the highest VT level; genuine constraints go in the allowlist
	if v := os.Getenv("ESCTEST_LEVEL"); v != "" {
		if n, cerr := strconv.Atoi(v); cerr == nil {
			level = n
		}
	}

	// Maximum strictness: enable xtermWinopsEnabled so the tests gated behind
	// optionRequired(XTERM_WINOPS_ENABLED) — OSC 52 selection query, the
	// Set/Reset-Title-Mode hex/UTF-8 tests, and DECNCSM — become must-pass
	// rather than skipped-as-option-missing. ESCTEST_OPTIONS overrides the set
	// (space/comma separated); "none" disables all options.
	opts := []string{"xtermWinopsEnabled"}
	if v := os.Getenv("ESCTEST_OPTIONS"); v != "" {
		if v == "none" {
			opts = nil
		} else {
			opts = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
		}
	}

	res, err := Run(t.Context(), &Options{
		Dir:        dir,
		Timeout:    750 * time.Millisecond,
		Include:    os.Getenv("ESCTEST_INCLUDE"),
		MaxVTLevel: level,
		Options:    opts,
	})
	if err != nil {
		t.Fatalf("esctest harness failed to run: %v", err)
	}

	if dst := os.Getenv("ESCTEST_LOGCOPY"); dst != "" {
		if werr := os.WriteFile(dst, []byte(res.RawLog), 0o600); werr != nil {
			t.Logf("could not write ESCTEST_LOGCOPY %s: %v", dst, werr)
		}
	}

	slices.Sort(res.Failed)
	t.Logf("esctest: %d passed, %d skipped/known-bug, %d failed", res.Passed, res.Skipped, len(res.Failed))
	// "Full run" means no ESCTEST_INCLUDE subset filter; both the pass-count
	// floor and the stale-entry check below apply only to a full run. Derive
	// it once so the two sites cannot drift.
	inc := os.Getenv("ESCTEST_INCLUDE")
	fullRun := inc == "" || inc == ".*"
	// A pass->skip/error/timeout regression drops out of the FAILED set the
	// allowlist check below compares, so guard the pass count too. Full runs
	// only; an ESCTEST_INCLUDE subset legitimately passes few tests.
	if fullRun {
		const minExpectedPass = 480 // floor under the documented 498 tally, headroom for benign pin drift
		if res.Passed < minExpectedPass {
			t.Errorf("esctest pass count %d below floor %d: tests silently stopped running (regression the failing-set check misses)", res.Passed, minExpectedPass)
		}
	}
	for _, name := range res.Failed {
		t.Logf("  FAIL %s", name)
	}

	allow, ok := loadAllowlist(t)
	if !ok {
		if os.Getenv("ESCTEST_TRIAGE") == "" {
			t.Fatal("known_failures.txt missing during a real gate run: the conformance contract file is gone, so the gate would otherwise pass with zero assertions; set ESCTEST_TRIAGE=1 for a first seeding run")
		}
		t.Logf("ESCTEST_TRIAGE set and no known_failures.txt: triage mode, not asserting (seed the allowlist from the FAILs above)")
		return
	}

	regressions, stale := evaluateGate(res, allow, fullRun)
	for _, n := range regressions {
		t.Errorf("unexpected esctest failure (regression): %s", n)
	}
	// The "a listed test now passes" check is only valid for a FULL run. A
	// scoped ESCTEST_INCLUDE (documented in scripts/esctest.sh and steering as
	// `ESCTEST_INCLUDE='CUPTests' bash scripts/esctest.sh -v`) runs a subset,
	// so an allowlisted test merely not appearing means "not run", not "now
	// passes" -- asserting it emits a false stale-entry error for every
	// out-of-scope allowlisted test and drowns the real FAIL list.
	for _, n := range stale {
		t.Errorf("allowlisted test %s now PASSES; remove it from known_failures.txt", n)
	}
}

// evaluateGate classifies a conformance Result against the known-failures
// allowlist. regressions are failing tests not on the allowlist (a real
// regression). stale are allowlisted tests that no longer fail; the stale set
// is computed only when fullRun is true, because a scoped --include run does
// not exercise every allowlisted test, so absence then means "not run", not
// "now passes".
func evaluateGate(res *Result, allow map[string]bool, fullRun bool) (regressions, stale []string) {
	failed := make(map[string]bool, len(res.Failed))
	for _, n := range res.Failed {
		failed[n] = true
	}
	for _, n := range res.Failed {
		if !allow[n] {
			regressions = append(regressions, n)
		}
	}
	if fullRun {
		for n := range allow {
			if !failed[n] {
				stale = append(stale, n)
			}
		}
	}
	return regressions, stale
}

func TestEvaluateGate(t *testing.T) {
	res := &Result{Failed: []string{"A.regressed", "B.allowed"}}
	allow := map[string]bool{"B.allowed": true, "C.stale": true}

	reg, stale := evaluateGate(res, allow, true)
	if strings.Join(reg, ",") != "A.regressed" {
		t.Errorf("full-run regressions = %v, want [A.regressed] (failing test not on allowlist)", reg)
	}
	if strings.Join(stale, ",") != "C.stale" {
		t.Errorf("full-run stale = %v, want [C.stale] (allowlisted test no longer failing)", stale)
	}

	reg2, stale2 := evaluateGate(res, allow, false)
	if strings.Join(reg2, ",") != "A.regressed" {
		t.Errorf("scoped-run regressions = %v, want [A.regressed]", reg2)
	}
	if len(stale2) != 0 {
		t.Errorf("scoped-run stale = %v, want empty (stale check skipped on a scoped run)", stale2)
	}
}

// loadAllowlist reads known_failures.txt (one test name per line, # comments and
// blank lines ignored). The bool is false when the file does not exist.
func loadAllowlist(t *testing.T) (map[string]bool, bool) {
	t.Helper()
	f, err := os.Open("known_failures.txt")
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()

	allow := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		allow[line] = true
	}
	return allow, true
}

func TestParseLog(t *testing.T) {
	const log = "*** BEGIN ***\n" +
		"*** TEST BSTests.test_BS_InitialReverseWraparound FAILED\n" +
		"Passed.\n" +
		"*** TEST DECSETTests.test_DECSET_DECCOLM FAILED\n" +
		"Passed.\n" +
		"Passed.\n" +
		"Fails as expected: XtermWinopsTests.test_XtermWinops_MoveToXY\n" +
		"Skipped because insufficient vt level: FooTests.test_bar\n" +
		"*** 3 tests passed, 2 failed ***\n"
	got := parseLog(log)
	if joined := strings.Join(got.Failed, ","); joined != "BSTests.test_BS_InitialReverseWraparound,DECSETTests.test_DECSET_DECCOLM" {
		t.Errorf("parseLog Failed = %q, want the two FAILED test names", joined)
	}
	if got.Passed != 3 {
		t.Errorf("parseLog Passed = %d, want 3", got.Passed)
	}
	if got.Skipped != 2 {
		t.Errorf("parseLog Skipped = %d, want 2", got.Skipped)
	}
}

func TestCrossCheckSummary(t *testing.T) {
	// Consistent: esctest's summary tallies equal the counts parseLog scrapes
	// (3 "Passed." lines, 2 FAILED markers) -> no error. Uses the real esctest2
	// 664be3c summary shape -- a "K known bugs" middle clause and the failed
	// word upper-cased ("TESTS FAILED") because failures are present.
	const okLog = "*** BEGIN ***\n" +
		"*** TEST A.a FAILED\n" +
		"Passed.\n" +
		"*** TEST B.b FAILED\n" +
		"Passed.\n" +
		"Passed.\n" +
		"*** 3 tests passed, 0 known bugs, 2 TESTS FAILED ***\n"
	if err := crossCheckSummary(okLog, parseLog(okLog)); err != nil {
		t.Errorf("crossCheckSummary(consistent 3-clause log) = %v, want nil", err)
	}

	// Clean-run shape: everything passed, so the failed word stays lower-case
	// ("0 tests failed") and the known-bug clause is singular. The regex must
	// capture passed/failed across this form too (not the known-bug number).
	const cleanLog = "*** BEGIN ***\n" +
		"Passed.\n" +
		"Passed.\n" +
		"Passed.\n" +
		"*** 3 tests passed, 1 known bug, 0 tests failed ***\n"
	if err := crossCheckSummary(cleanLog, parseLog(cleanLog)); err != nil {
		t.Errorf("crossCheckSummary(clean-run 3-clause log) = %v, want nil", err)
	}

	// Mismatch: esctest's summary reports 2 failed, but only 1 clean FAILED
	// marker is scraped -- the other test ERRORED, which failedRe cannot see.
	// The cross-check must flag it so the gate does not go silently green.
	const mismatchLog = "*** BEGIN ***\n" +
		"*** TEST A.a FAILED\n" +
		"Passed.\n" +
		"Passed.\n" +
		"Passed.\n" +
		"*** 3 tests passed, 0 known bugs, 2 TESTS FAILED ***\n"
	err := crossCheckSummary(mismatchLog, parseLog(mismatchLog))
	if err == nil {
		t.Fatal("crossCheckSummary(summary=2 failed / scraped=1 failed) = nil, want a mismatch error")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("mismatch error = %q, want it to name a count mismatch", err)
	}

	// Fail-safe: a summary line the loose completion oracle matches but whose
	// numeric counts are unparseable (an unrecognized format) must NOT fail the
	// gate. parseLog's per-test-marker counts stay authoritative, so
	// crossCheckSummary skips with a warning and returns nil rather than turning
	// the whole gate red on a summary-wording drift.
	const garbledLog = "*** BEGIN ***\n" +
		"Passed.\n" +
		"*** all tests passed! ***\n"
	if !summaryRe.MatchString(garbledLog) {
		t.Fatal("test setup: garbledLog must still match the loose completion oracle")
	}
	if err := crossCheckSummary(garbledLog, parseLog(garbledLog)); err != nil {
		t.Errorf("crossCheckSummary(unparseable summary counts) = %v, want nil (fail-safe skip)", err)
	}
}

// TestPumpInMemory drives pump over an in-memory reader/writer (no PTY, no
// *os.File): it feeds a DSR query (CSI 5 n), which vt.Screen answers with
// "\x1b[0n" WITHOUT needing AllowScreenReport, and asserts the answer is
// written back verbatim and the response queue is drained afterward.
func TestPumpInMemory(t *testing.T) {
	screen := vt.New(25, 80)
	r := bytes.NewReader([]byte("\x1b[5n")) // DSR: report device status
	var w bytes.Buffer

	pump(r, &w, screen) // returns at EOF, no goroutine needed

	if got, want := w.String(), "\x1b[0n"; got != want {
		t.Errorf("pump wrote %q back to the child, want the DSR answer %q", got, want)
	}
	if resp := screen.TakeResponse(); len(resp) != 0 {
		t.Errorf("pump left a queued response = %q, want it drained to empty after writing", resp)
	}
}

// TestPumpOSPipe exercises pump over real *os.File descriptors (os.Pipe pairs),
// distinct from the in-memory TestPumpInMemory, without a live PTY child. A
// genuine bidirectional PTY is avoided on purpose: its echo/line-discipline
// makes the query-answer round trip non-deterministic and can block pump's
// read forever. Two pipes give clean, bounded, sleep-free semantics: the input
// pipe is pre-loaded with the query then closed (so pump reads the query, hits
// EOF, and returns), and the answer is read back off the output pipe.
func TestPumpOSPipe(t *testing.T) {
	screen := vt.New(25, 80)

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (input): %v", err)
	}
	defer func() { _ = inR.Close() }()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (output): %v", err)
	}
	defer func() { _ = outR.Close() }()

	// Pre-load the query and close the write end so pump reads the query, then
	// hits EOF and returns -- no goroutine, no read deadline, no sleep.
	if _, werr := inW.Write([]byte("\x1b[6n")); werr != nil { // CPR: cursor position report
		t.Fatalf("write query to input pipe: %v", werr)
	}
	_ = inW.Close()

	// pump's short answer fits the kernel pipe buffer, so the synchronous write
	// to outW never blocks even though nothing is draining outR yet.
	pump(inR, outW, screen)
	_ = outW.Close() // let io.ReadAll(outR) terminate at EOF

	got, rerr := io.ReadAll(outR)
	if rerr != nil {
		t.Fatalf("read answer from output pipe: %v", rerr)
	}
	if want := "\x1b[1;1R"; string(got) != want { // fresh screen: cursor at row 1, col 1
		t.Errorf("pump wrote %q back over the pipe, want the CPR answer %q", got, want)
	}
	if resp := screen.TakeResponse(); len(resp) != 0 {
		t.Errorf("pump left a queued response = %q, want it drained to empty", resp)
	}
}

func TestEsctestArgs(t *testing.T) {
	o := &Options{Python: "python3", MaxVTLevel: 5, Timeout: 750 * time.Millisecond, Include: "CUPTests", Options: []string{"xtermWinopsEnabled", "otherOpt"}}
	got := strings.Join(esctestArgs("/x/esctest.py", "/tmp/e.log", o), " ")
	want := "/x/esctest.py --expected-terminal=xterm --max-vt-level=5 --xterm-checksum=0 --logfile=/tmp/e.log --no-print-logs --timeout=0.75 --v=2 --include=CUPTests --options xtermWinopsEnabled otherOpt"
	if got != want {
		t.Errorf("esctestArgs = %q, want %q", got, want)
	}
}

func TestApplyDefaults(t *testing.T) {
	var o Options
	applyDefaults(&o)
	if o.Python != "python3" || o.Rows != 25 || o.Cols != 80 || o.MaxVTLevel != 4 || o.Timeout != time.Second || o.Include != ".*" {
		t.Errorf("applyDefaults(zero) = %+v, want python3/25x80/level4/1s/.*", o)
	}
	set := Options{Python: "py", Rows: 40, Cols: 100, MaxVTLevel: 5, Timeout: 2 * time.Second, Include: "X"}
	applyDefaults(&set)
	if set.Python != "py" || set.Rows != 40 || set.Cols != 100 || set.MaxVTLevel != 5 || set.Timeout != 2*time.Second || set.Include != "X" {
		t.Errorf("applyDefaults(set) mutated explicit values: %+v", set)
	}
}

func TestLoadAllowlist(t *testing.T) {
	t.Chdir(t.TempDir())
	const content = "# comment line\n\nChangeColorTests.test_ChangeColor_CIELab\n  DECSETTests.test_DECSET_DECCOLM  \n\n# another\n"
	if err := os.WriteFile("known_failures.txt", []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	allow, ok := loadAllowlist(t)
	if !ok {
		t.Fatal("loadAllowlist returned ok=false for an existing file")
	}
	if len(allow) != 2 {
		t.Errorf("loadAllowlist parsed %d entries, want 2 (comments and blanks skipped)", len(allow))
	}
	if !allow["ChangeColorTests.test_ChangeColor_CIELab"] || !allow["DECSETTests.test_DECSET_DECCOLM"] {
		t.Errorf("loadAllowlist did not trim whitespace or missing entries: %v", allow)
	}
}

func TestLoadAllowlist_missing(t *testing.T) {
	t.Chdir(t.TempDir())
	allow, ok := loadAllowlist(t)
	if ok {
		t.Error("loadAllowlist ok=true when known_failures.txt absent, want false")
	}
	if allow != nil {
		t.Errorf("loadAllowlist allow=%v when file absent, want nil", allow)
	}
}
