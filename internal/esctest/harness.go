// Package esctest runs the esctest2 VT conformance suite
// (github.com/ThomasDickey/esctest2, George Nachman's esctest with Thomas
// Dickey's fixes) against the engine's vt.Screen.
//
// esctest2 is GPL-2.0; the engine is MPL-2.0. Those cannot be COMBINED into one
// work in this direction: MPL-2.0 §3.3 lets MPL files enter a GPL work, not GPL
// code enter an MPL one. To keep the licenses at arm's length, esctest2 is NEVER
// vendored into this repo. It is fetched separately (see scripts/esctest.sh) and
// located at run time via the ESCTEST2_DIR environment variable. The suite is
// executed as an ordinary subprocess and communicated with over a PTY — it is
// never linked into the engine — so this is mere aggregation, not a derived work.
//
// The model: esctest runs as the child process of a terminal. It writes escape
// sequences to its stdout (which the terminal renders) and reads state back
// through queries (cursor-position reports, and rectangular-area checksums via
// DECRQCRA). It never inspects pixels, so it validates the Go VT state machine
// directly with no browser involved. Run drives a real PTY: the child's stdout
// feeds vt.Screen, and vt.Screen's Response bytes (query answers) are written
// back to the child's stdin.
package esctest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/web-terminal-engine/v5/vt"
	"github.com/creack/pty"
)

// Options configures a conformance run.
type Options struct {
	// Dir is the esctest2 checkout (the directory containing esctest/esctest.py).
	Dir string
	// Python is the interpreter to run esctest with (default "python3").
	Python string
	// Include is a regexp restricting which tests run (esctest --include; default ".*").
	Include string
	// Options are esctest --options flags (e.g. "xtermWinopsEnabled"). Enabling
	// xtermWinopsEnabled is the maximum-strictness setting: it turns the tests
	// gated behind optionRequired(XTERM_WINOPS_ENABLED) — OSC 52 selection
	// query, the Set/Reset-Title-Mode hex/UTF-8 tests, and DECNCSM — from
	// "expected to fail (option missing)" into MUST-PASS. Do NOT add
	// disableWideChars (it would force the 8-bit C1 control tests, which are
	// incompatible with a UTF-8 terminal, into hard failures).
	Options []string
	// Timeout is how long esctest waits for each query response (default 1s).
	Timeout time.Duration
	// Rows, Cols size the PTY and the screen. esctest resets to 25x80 per test
	// via a programmatic resize the engine treats as a no-op, so these MUST be
	// 25x80 for the suite's assumptions to hold (default 25x80).
	Rows int
	Cols int
	// MaxVTLevel is esctest --max-vt-level. Must be >=4 or every screen-content
	// assertion is skipped (DECRQCRA is a level-4 feature). Default 4.
	MaxVTLevel int
}

// Result is the parsed outcome of a conformance run.
type Result struct {
	// RawLog is the full esctest logfile, kept for diagnosis.
	RawLog string
	// Failed is the list of test names that failed (excludes known bugs/skips).
	Failed []string
	// Passed is the number of tests that passed.
	Passed int
	// Skipped counts tests esctest skipped (known bugs + insufficient VT level).
	Skipped int
}

var (
	// esctest logs one such line per failing test.
	failedRe = regexp.MustCompile(`\*{3} TEST (\S+) FAILED`)
	// summaryRe is the loose completion oracle: esctest emits a summary line on
	// every clean run and never on a runner-level crash, so its mere presence
	// signals the run finished (see Run).
	summaryRe = regexp.MustCompile(`\*{3} .*passed.*\*{3}`)
	// summaryCountRe captures esctest's own passed/failed tallies from that same
	// summary line so crossCheckSummary can compare them against the counts
	// parseLog scrapes from per-test markers. esctest2 (pinned commit 664be3c)
	// emits a three-clause summary via RunTests, e.g.
	// "*** 498 tests passed, 34 known bugs, 0 tests failed ***", and upper-cases
	// the failed clause to "N TESTS FAILED" whenever any test fails. The pattern
	// therefore matches case-insensitively and tolerates the interposed
	// "K known bug(s)," clause while still capturing passed (group 1) and failed
	// (group 2).
	summaryCountRe = regexp.MustCompile(`(?i)\*{3}\s+(\d+)\s+tests?\s+passed,\s+(?:\d+\s+known\s+bugs?,\s+)?(\d+)\s+tests?\s+failed\s+\*{3}`)
)

// Run executes the esctest suite against a fresh vt.Screen and returns the
// parsed result. ctx cancels the child process (e.g. a test timeout). A non-nil
// error means the harness itself failed to run the suite (missing interpreter,
// PTY error); test failures reported by esctest are carried in Result, not the
// error.
func Run(ctx context.Context, opts *Options) (*Result, error) {
	cfg := *opts
	applyDefaults(&cfg)

	script := filepath.Join(cfg.Dir, "esctest", "esctest.py")
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Errorf("esctest.py not found at %s: %w", script, err)
	}

	logFile, err := os.CreateTemp("", "esctest-*.log")
	if err != nil {
		return nil, fmt.Errorf("create logfile: %w", err)
	}
	logPath := logFile.Name()
	_ = logFile.Close()
	defer func() { _ = os.Remove(logPath) }()

	cmd := exec.CommandContext(ctx, cfg.Python, esctestArgs(script, logPath, &cfg)...) // #nosec G204 -- fixed esctest invocation
	cmd.Dir = filepath.Join(cfg.Dir, "esctest")

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(cfg.Rows), Cols: uint16(cfg.Cols)}) // #nosec G115 -- terminal dims are small
	if err != nil {
		return nil, fmt.Errorf("start esctest under pty: %w", err)
	}
	defer func() { _ = ptmx.Close() }()

	// esctest runs with --expected-terminal=xterm and its per-test reset()
	// establishes xterm's stock black-on-white resources (OSC 10 #000, OSC 11
	// #fff). Configure the screen with that same theme so dynamic-color reset
	// (OSC 110/111/112) restores the xterm default esctest asserts, rather than
	// the engine's real dark default (which is correct for the browser but not
	// what the xterm-conformance suite expects). This is test-harness config,
	// like AllowScreenReport below; the shipped engine default stays dark.
	screen := vt.New(cfg.Rows, cfg.Cols, vt.WithTheme(vt.Theme{
		Foreground: vt.RGB(0, 0, 0),
		Background: vt.RGB(0xff, 0xff, 0xff),
		Cursor:     vt.RGB(0, 0, 0),
	}))
	screen.AllowScreenReport = true // enable DECRQCRA so esctest can read the screen back

	done := make(chan struct{})
	go func() {
		pump(ptmx, ptmx, screen)
		close(done)
	}()

	// esctest exits 0 both when tests fail AND when its own top-level runner
	// aborts (RunTests raises -> main() logs a traceback and still exits 0), so
	// the exit code cannot signal completion. The summary line is the reliable
	// completion oracle: esctest emits it on every clean run (even zero matched
	// tests) and never on a runner-level crash, so gate on the summary alone.
	waitErr := cmd.Wait()
	_ = ptmx.Close()
	<-done

	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		return nil, fmt.Errorf("read esctest logfile %s: %w", logPath, readErr)
	}
	log := string(data)
	if !summaryRe.MatchString(log) {
		return nil, fmt.Errorf("esctest did not complete: no summary line in log (wait err: %v); log tail:\n%s", waitErr, tail(log, 2000))
	}

	res := parseLog(log)
	// Cross-check esctest's own summary tallies against the counts parseLog
	// scraped from per-test markers. A divergence means the scraper is blind to
	// some result (a test that ERRORED rather than cleanly FAILED, or a renamed
	// marker), which would otherwise leave the regression gate silently green.
	// Fail through the same nil,error path as the completion oracle above so
	// TestConformance/evaluateGate treat a mismatch as a hard failure.
	if err := crossCheckSummary(log, res); err != nil {
		return nil, err
	}

	return res, nil
}

// applyDefaults fills in the zero-value Options fields in place.
func applyDefaults(o *Options) {
	if o.Python == "" {
		o.Python = "python3"
	}
	if o.Rows <= 0 {
		o.Rows = 25
	}
	if o.Cols <= 0 {
		o.Cols = 80
	}
	if o.MaxVTLevel <= 0 {
		o.MaxVTLevel = 4
	}
	if o.Timeout <= 0 {
		o.Timeout = time.Second
	}
	if o.Include == "" {
		o.Include = ".*"
	}
}

// esctestArgs builds the esctest.py command line. --xterm-checksum=0 selects the
// pre-279 xterm checksum (the negated ordinal sum vt.reportRectChecksum emits);
// --no-print-logs keeps the trailing log dump out of the PTY (the logfile still
// gets it); --v=2 (LOG_INFO) puts per-test pass/fail + the summary in the log.
func esctestArgs(script, logPath string, o *Options) []string {
	args := []string{
		script,
		"--expected-terminal=xterm",
		fmt.Sprintf("--max-vt-level=%d", o.MaxVTLevel),
		"--xterm-checksum=0",
		"--logfile=" + logPath,
		"--no-print-logs",
		fmt.Sprintf("--timeout=%g", o.Timeout.Seconds()),
		"--v=2",
		"--include=" + o.Include,
	}
	// esctest's --options takes nargs="+", so the flag is followed by each
	// option token as a separate argument.
	if len(o.Options) > 0 {
		args = append(args, "--options")
		args = append(args, o.Options...)
	}
	return args
}

// pump feeds the child's escape output (read from r) into the screen and writes
// the screen's query answers back to the child's stdin (w), until r reaches EOF
// or errors. In production r and w are the same PTY master (*os.File, which
// satisfies both interfaces); the io.Reader/io.Writer seam lets tests drive it
// over pipes without a live child. A single goroutine owns the screen, so no
// synchronization is needed.
func pump(r io.Reader, w io.Writer, screen *vt.Screen) {
	buf := make([]byte, 8192)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			_, _ = screen.Write(buf[:n])
			if resp := screen.TakeResponse(); len(resp) > 0 {
				_, _ = w.Write(resp)
			}
		}
		if readErr != nil {
			return
		}
	}
}

// parseLog derives pass/skip counts and the failing-test list from the esctest
// logfile. Counts come from the per-test markers esclog emits, which are more
// robust than the human-readable summary line.
func parseLog(log string) *Result {
	res := &Result{RawLog: log}
	for _, m := range failedRe.FindAllStringSubmatch(log, -1) {
		res.Failed = append(res.Failed, m[1])
	}
	for line := range strings.SplitSeq(log, "\n") {
		switch {
		case line == "Passed.":
			res.Passed++
		case strings.HasPrefix(line, "Fails as expected:"),
			strings.HasPrefix(line, "Skipped because"):
			res.Skipped++
		}
	}
	return res
}

// crossCheckSummary verifies esctest's own summary tallies match the counts
// parseLog scraped from per-test markers. parseLog derives Passed by counting
// "Passed." lines and Failed from "*** TEST x FAILED" markers; esctest's
// summary ("*** N tests passed, K known bugs, M tests failed ***", with the
// failed clause upper-cased to "M TESTS FAILED" on any failure) is an
// independent count of the same outcomes, so the two must agree. When they
// diverge the per-test scraper is blind to some result — a test that ERRORED
// instead of cleanly FAILING, or a renamed marker — so this returns an error to
// fail the gate loudly rather than report a false green. If the summary counts
// cannot be parsed at all (an unrecognized format), the cross-check is skipped
// with a warning rather than failed: parseLog's per-test-marker counts remain
// the authoritative result, so a summary-wording drift must not turn the whole
// gate red on its own.
func crossCheckSummary(log string, res *Result) error {
	m := summaryCountRe.FindStringSubmatch(log)
	if m == nil {
		slog.Warn("esctest: could not parse summary counts for cross-check; skipping")
		return nil
	}
	sumPassed, _ := strconv.Atoi(m[1]) // m[1]/m[2] are \d+ captures, so Atoi cannot fail
	sumFailed, _ := strconv.Atoi(m[2])
	if sumPassed != res.Passed || sumFailed != len(res.Failed) {
		return fmt.Errorf("esctest summary/scraped count mismatch: summary=%d passed/%d failed, scraped=%d passed/%d failed (a test errored or a marker wording changed; the per-test scraper is silently blind); log tail:\n%s", sumPassed, sumFailed, res.Passed, len(res.Failed), tail(log, 500))
	}
	return nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
