package terminal

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/cplieger/web-terminal-engine/v3/vt"
)

// Functional-options API: verify NewHandler applies defaults, each WithX
// threads its value independently, option order doesn't matter, and nil/zero
// inputs are handled safely.

// TestNewHandler_appliesDefaults verifies that calling NewHandler with only a
// command produces the documented defaults: scrollbackCapacity=1000,
// logger=slog.Default(), no origin policy, no onProcessExit, no workDir, no
// extra env (TERM/COLORTERM are injected at spawn time, not stored in cfg).
func TestNewHandler_appliesDefaults(t *testing.T) {
	h := NewHandler([]string{"/bin/sh"})

	if h.cfg.scrollbackCapacity != 1000 {
		t.Fatalf("default scrollbackCapacity = %d, want 1000", h.cfg.scrollbackCapacity)
	}
	if h.cfg.logger != slog.Default() {
		t.Fatal("default logger is not slog.Default()")
	}
	if h.cfg.originPolicy != nil {
		t.Fatal("default originPolicy should be nil (same-origin only)")
	}
	if h.cfg.onProcessExit != nil {
		t.Fatal("default onProcessExit should be nil")
	}
	if h.cfg.workDir != "" {
		t.Fatalf("default workDir = %q, want empty", h.cfg.workDir)
	}
	if h.cfg.env != nil {
		t.Fatalf("default env = %v, want nil", h.cfg.env)
	}
	if h.screen.Height != defaultRows || h.screen.Width != defaultCols {
		t.Fatalf("screen %dx%d, want %dx%d", h.screen.Height, h.screen.Width, defaultRows, defaultCols)
	}
	if cap(h.scrollback.buf) != 1000 {
		t.Fatalf("scrollback ring cap = %d, want 1000", cap(h.scrollback.buf))
	}
	if h.registry == nil {
		t.Fatal("registry is nil")
	}
	if h.builder == nil {
		t.Fatal("builder is nil")
	}
	if len(h.command) != 1 || h.command[0] != "/bin/sh" {
		t.Fatalf("command = %v, want [/bin/sh]", h.command)
	}
	if h.started.Load() {
		t.Fatal("started should be false before any connection")
	}
}

// TestNewHandler_WithScrollbackCapacity verifies custom scrollback capacity.
func TestNewHandler_WithScrollbackCapacity(t *testing.T) {
	h := NewHandler([]string{"/bin/sh"}, WithScrollbackCapacity(500))
	if h.cfg.scrollbackCapacity != 500 {
		t.Fatalf("scrollbackCapacity = %d, want 500", h.cfg.scrollbackCapacity)
	}
	if cap(h.scrollback.buf) != 500 {
		t.Fatalf("scrollback ring cap = %d, want 500", cap(h.scrollback.buf))
	}
}

// TestNewHandler_WithLogger verifies custom logger injection.
func TestNewHandler_WithLogger(t *testing.T) {
	custom := slog.New(slog.NewTextHandler(os.Stderr, nil))
	h := NewHandler([]string{"/bin/sh"}, WithLogger(custom))
	if h.cfg.logger != custom {
		t.Fatal("WithLogger did not inject custom logger")
	}
}

// TestNewHandler_WithLogger_Nil verifies a nil logger stores a discard logger
// (not nil), since a nil *slog.Logger would panic on use.
func TestNewHandler_WithLogger_Nil(t *testing.T) {
	h := NewHandler([]string{"/bin/sh"}, WithLogger(nil))
	if h.cfg.logger == nil {
		t.Fatal("WithLogger(nil) should store a discard logger, not nil")
	}
}

// TestNewHandler_WithWorkDir verifies the working directory is threaded.
func TestNewHandler_WithWorkDir(t *testing.T) {
	h := NewHandler([]string{"/bin/sh"}, WithWorkDir("/tmp"))
	if h.cfg.workDir != "/tmp" {
		t.Fatalf("workDir = %q, want /tmp", h.cfg.workDir)
	}
}

// TestNewHandler_WithEnv verifies additional env vars are stored.
func TestNewHandler_WithEnv(t *testing.T) {
	env := []string{"FOO=bar", "BAZ=qux"}
	h := NewHandler([]string{"/bin/sh"}, WithEnv(env))
	if len(h.cfg.env) != 2 || h.cfg.env[0] != "FOO=bar" || h.cfg.env[1] != "BAZ=qux" {
		t.Fatalf("env = %v, want [FOO=bar BAZ=qux]", h.cfg.env)
	}
}

// TestNewHandler_WithOriginPolicy verifies the origin policy threads.
func TestNewHandler_WithOriginPolicy(t *testing.T) {
	p, invalid := NewOriginPolicy("https://embed.example.com")
	if len(invalid) != 0 {
		t.Fatalf("invalid = %v, want none", invalid)
	}
	h := NewHandler([]string{"/bin/sh"}, WithOriginPolicy(p))
	if h.cfg.originPolicy != p {
		t.Fatal("WithOriginPolicy did not thread the policy")
	}
}

// TestNewHandler_WithOnProcessExit verifies the exit callback is stored and
// invokable.
func TestNewHandler_WithOnProcessExit(t *testing.T) {
	called := false
	fn := func(err error) { called = true }
	h := NewHandler([]string{"/bin/sh"}, WithOnProcessExit(fn))
	if h.cfg.onProcessExit == nil {
		t.Fatal("WithOnProcessExit did not store callback")
	}
	h.cfg.onProcessExit(nil)
	if !called {
		t.Fatal("onProcessExit callback not invoked")
	}
}

// TestNewHandler_OptionOrderIndependent verifies that applying options in
// different orders produces the same result.
func TestNewHandler_OptionOrderIndependent(t *testing.T) {
	env := []string{"X=1"}
	custom := slog.New(slog.NewTextHandler(os.Stderr, nil))

	h1 := NewHandler([]string{"/bin/sh"},
		WithScrollbackCapacity(200),
		WithLogger(custom),
		WithWorkDir("/home"),
		WithEnv(env),
	)
	h2 := NewHandler([]string{"/bin/sh"},
		WithEnv(env),
		WithWorkDir("/home"),
		WithLogger(custom),
		WithScrollbackCapacity(200),
	)

	if h1.cfg.scrollbackCapacity != h2.cfg.scrollbackCapacity {
		t.Fatal("scrollbackCapacity differs with option order")
	}
	if h1.cfg.logger != h2.cfg.logger {
		t.Fatal("logger differs with option order")
	}
	if h1.cfg.workDir != h2.cfg.workDir {
		t.Fatal("workDir differs with option order")
	}
	if len(h1.cfg.env) != len(h2.cfg.env) || h1.cfg.env[0] != h2.cfg.env[0] {
		t.Fatal("env differs with option order")
	}
}

// TestNewHandler_NoOptions_NilSlice verifies a nil Option in the variadic
// slice is skipped rather than called (a nil func pointer would panic).
func TestNewHandler_NoOptions_NilSlice(t *testing.T) {
	h := NewHandler([]string{"/bin/sh"}, nil)
	if h.cfg.scrollbackCapacity != 1000 {
		t.Fatalf("scrollbackCapacity = %d after nil option", h.cfg.scrollbackCapacity)
	}
}

// TestNewHandler_EmptyCommand verifies an empty command makes ensureStarted
// fail with a clear error (nil and empty-slice commands both have length 0).
func TestNewHandler_EmptyCommand(t *testing.T) {
	for _, cmd := range [][]string{nil, {}} {
		h := NewHandler(cmd)
		err := h.ensureStarted(80, 24)
		if err == nil {
			t.Fatalf("ensureStarted(%v): expected error for empty command", cmd)
		}
		if !strings.Contains(err.Error(), "empty command") {
			t.Fatalf("ensureStarted(%v): unexpected error: %v", cmd, err)
		}
	}
}

// TestNewHandler_ENVInjection verifies the advertised terminal identity — TERM,
// COLORTERM, and TERM_PROGRAM(+_VERSION) — is injected into the spawned process
// environment at spawn time (not stored in cfg.env). TERM_PROGRAM=iTerm.app
// (>= 3.6.6) is the identity that unlocks OSC 9;4 progress for kiro-cli + Claude
// Code and DEC 2026 synchronized output; see ensureStarted.
func TestNewHandler_ENVInjection(t *testing.T) {
	h := NewHandler([]string{"/bin/sh", "-c", "true"}, WithWorkDir("/"))
	if err := h.ensureStarted(80, 24); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}
	defer h.Shutdown()

	var hasTerm, hasColorterm, hasTermProgram, hasTermProgramVersion bool
	for _, e := range h.cmd.Env {
		switch {
		case strings.HasPrefix(e, "TERM=xterm-256color"):
			hasTerm = true
		case strings.HasPrefix(e, "COLORTERM=truecolor"):
			hasColorterm = true
		case strings.HasPrefix(e, "TERM_PROGRAM=iTerm.app"):
			hasTermProgram = true
		case strings.HasPrefix(e, "TERM_PROGRAM_VERSION="):
			hasTermProgramVersion = true
		}
	}
	if !hasTerm {
		t.Fatal("TERM=xterm-256color not found in cmd.Env")
	}
	if !hasColorterm {
		t.Fatal("COLORTERM=truecolor not found in cmd.Env")
	}
	if !hasTermProgram {
		t.Fatal("TERM_PROGRAM=iTerm.app not found in cmd.Env")
	}
	if !hasTermProgramVersion {
		t.Fatal("TERM_PROGRAM_VERSION not found in cmd.Env")
	}
}

// TestNewHandler_ENVCustomOverride verifies custom env is appended AFTER the
// standard env so a user-provided TERM wins (execve uses the last value).
func TestNewHandler_ENVCustomOverride(t *testing.T) {
	h := NewHandler([]string{"/bin/sh", "-c", "true"},
		WithWorkDir("/"),
		WithEnv([]string{"TERM=dumb"}),
	)
	if err := h.ensureStarted(80, 24); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}
	defer h.Shutdown()

	lastTerm := ""
	for _, e := range h.cmd.Env {
		if strings.HasPrefix(e, "TERM=") {
			lastTerm = e
		}
	}
	if lastTerm != "TERM=dumb" {
		t.Fatalf("last TERM = %q, want TERM=dumb (custom should override)", lastTerm)
	}
}

// TestNewHandler_WithScrollbackCapacity_Zero verifies zero capacity is safe:
// the ring accepts an Append without panicking.
func TestNewHandler_WithScrollbackCapacity_Zero(t *testing.T) {
	h := NewHandler([]string{"/bin/sh"}, WithScrollbackCapacity(0))
	if h.cfg.scrollbackCapacity != 0 {
		t.Fatalf("scrollbackCapacity = %d, want 0", h.cfg.scrollbackCapacity)
	}
	// Append must not panic on a zero-capacity ring.
	h.scrollback.Append([][]vt.WireRun{{{T: "hello"}}})
}

// TestNewHandler_WithScrollbackCapacity_Negative verifies a negative capacity
// is clamped to 0 and the resulting ring is still safe to Append to.
func TestNewHandler_WithScrollbackCapacity_Negative(t *testing.T) {
	h := NewHandler([]string{"/bin/sh"}, WithScrollbackCapacity(-1))
	if h.cfg.scrollbackCapacity != 0 {
		t.Fatalf("scrollbackCapacity = %d, want 0 (clamped from -1)", h.cfg.scrollbackCapacity)
	}
	h.scrollback.Append([][]vt.WireRun{{{T: "hello"}}})
}

// TestNewHandler_LastOptionWins verifies duplicate options use last-wins
// semantics.
func TestNewHandler_LastOptionWins(t *testing.T) {
	h := NewHandler([]string{"/bin/sh"},
		WithScrollbackCapacity(100),
		WithScrollbackCapacity(200),
		WithScrollbackCapacity(300),
	)
	if h.cfg.scrollbackCapacity != 300 {
		t.Fatalf("scrollbackCapacity = %d, want 300 (last option)", h.cfg.scrollbackCapacity)
	}
}

// TestNewHandler_NilLoggerDoesNotPanic verifies that WithLogger(nil)
// ("disables logging") does not cause a nil-pointer panic when the handler
// logs internally during a real start.
func TestNewHandler_NilLoggerDoesNotPanic(t *testing.T) {
	h := NewHandler([]string{"/bin/sh", "-c", "true"},
		WithWorkDir("/"),
		WithLogger(nil),
	)
	if err := h.ensureStarted(80, 24); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}
	defer h.Shutdown()
}

// commandCaptureHandler records every slog record for command-attr assertions.
type commandCaptureHandler struct {
	records *[]slog.Record
}

func (h commandCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h commandCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r.Clone())
	return nil
}
func (h commandCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h commandCaptureHandler) WithGroup(string) slog.Handler      { return h }

// startAndCollectRecords starts a real short-lived process under a capturing
// logger built from the given options and returns every record emitted through
// process start.
func startAndCollectRecords(t *testing.T, opts ...Option) []slog.Record {
	t.Helper()
	var records []slog.Record
	logger := slog.New(commandCaptureHandler{records: &records})
	h := NewHandler([]string{"/bin/sh", "-c", "true"},
		append([]Option{WithWorkDir("/"), WithLogger(logger)}, opts...)...)
	if err := h.ensureStarted(24, 80); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}
	defer h.Shutdown()
	return records
}

// commandAttrOf returns the string form of the "command" attribute of the
// process-start record, failing the test when the record or attr is missing.
func commandAttrOf(t *testing.T, records []slog.Record) string {
	t.Helper()
	for _, r := range records {
		if r.Message != "terminal: process started" {
			continue
		}
		var got string
		var found bool
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "command" {
				got = a.Value.String()
				found = true
				return false
			}
			return true
		})
		if !found {
			t.Fatal("process-start record carries no command attribute")
		}
		return got
	}
	t.Fatal("no process-start record emitted")
	return ""
}

// TestNewHandler_WithCommandLogValue verifies the process-start line records
// the fixed marker instead of the argv, and that no record leaks the argv.
func TestNewHandler_WithCommandLogValue(t *testing.T) {
	records := startAndCollectRecords(t, WithCommandLogValue("[redacted]"))

	if got := commandAttrOf(t, records); got != "[redacted]" {
		t.Errorf("command attr = %q, want the fixed marker", got)
	}
	for _, r := range records {
		r.Attrs(func(a slog.Attr) bool {
			if s := a.Value.String(); strings.Contains(s, "/bin/sh") {
				t.Errorf("argv leaked through %q attr %q = %q", r.Message, a.Key, s)
			}
			return true
		})
	}
}

// TestNewHandler_WithCommandLogValue_defaultLogsArgv pins the default: without
// the option the process-start line records the full argv.
func TestNewHandler_WithCommandLogValue_defaultLogsArgv(t *testing.T) {
	records := startAndCollectRecords(t)

	if got := commandAttrOf(t, records); !strings.Contains(got, "/bin/sh") {
		t.Errorf("command attr = %q, want the argv by default", got)
	}
}

// TestNewHandler_WithCommandLogValue_emptyIgnored pins the skip-zero option
// convention: an empty value keeps the default argv logging.
func TestNewHandler_WithCommandLogValue_emptyIgnored(t *testing.T) {
	records := startAndCollectRecords(t, WithCommandLogValue(""))

	if got := commandAttrOf(t, records); !strings.Contains(got, "/bin/sh") {
		t.Errorf("command attr = %q, want the argv when the option value is empty", got)
	}
}
