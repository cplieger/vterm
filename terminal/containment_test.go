package terminal

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// TestSanitizeCgroupName pins the whitelist. A sanitized id becomes a directory
// name under a root the server owns, so a traversal or an absolute path must be
// impossible rather than unlikely; these cases are the ones that would matter.
func TestSanitizeCgroupName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "abc123", "abc123"},
		{"dash and underscore kept", "a-b_c", "a-b_c"},
		{"parent traversal stripped", "../../etc/passwd", "etcpasswd"},
		{"absolute path stripped", "/sys/fs/cgroup/evil", "sysfscgroupevil"},
		{"dot segments stripped", "..", ""},
		{"single slash", "/", ""},
		{"empty", "", ""},
		{"nul byte", "ab\x00cd", "abcd"},
		{"newline injection", "id\nother", "idother"},
		{"space", "a b", "ab"},
		{"unicode dropped", "sessioné☃", "session"},
		{"only separators", "///...///", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeCgroupName(tc.in); got != tc.want {
				t.Fatalf("sanitizeCgroupName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeCgroupNameBounded checks the length cap, so a hostile id cannot
// produce an unbounded path component.
func TestSanitizeCgroupNameBounded(t *testing.T) {
	t.Parallel()
	got := sanitizeCgroupName(strings.Repeat("a", containMaxIDLen*4))
	if len(got) != containMaxIDLen {
		t.Fatalf("length %d, want %d", len(got), containMaxIDLen)
	}
}

// TestNewContainmentRejectsNonCgroupRoot is the degradation contract: a host that
// cannot support containment must produce a typed error the consumer can log and
// continue past, never a panic and never a partial setup.
func TestNewContainmentRejectsNonCgroupRoot(t *testing.T) {
	t.Parallel()
	c, err := NewContainment(t.TempDir(), "wt-", slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected an error for a non-cgroup2 root")
	}
	if !errors.Is(err, errContainmentUnsupported) {
		t.Fatalf("error %v does not wrap errContainmentUnsupported", err)
	}
	if c != nil {
		t.Fatal("expected a nil Containment alongside the error")
	}
}

// TestNewContainmentRejectsEmptyArgs guards the two arguments that would other-
// wise produce directories at a path nobody intended.
func TestNewContainmentRejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ root, prefix string }{
		{"", "wt-"},
		{t.TempDir(), ""},
	} {
		if _, err := NewContainment(tc.root, tc.prefix, nil); err == nil {
			t.Fatalf("NewContainment(%q, %q) succeeded, want an error", tc.root, tc.prefix)
		}
	}
}

// TestContainmentOptionsDefaultOff pins the default: a handler built without the
// options is not contained, which is what makes this landable without moving any
// consumer.
func TestContainmentOptionsDefaultOff(t *testing.T) {
	t.Parallel()
	h := NewHandler([]string{"/bin/true"})
	if h.cfg.containment != nil || h.cfg.containmentID != "" || h.cfg.containSample != 0 {
		t.Fatalf("containment defaults are not off: %+v", h.cfg)
	}
	if h.contain != nil {
		t.Fatal("handler has a session cgroup before start")
	}
}

// TestWithContainmentNilIsNoop covers the shape a consumer actually writes: pass
// the result of NewContainment straight through without branching on whether the
// host supported it.
func TestWithContainmentNilIsNoop(t *testing.T) {
	t.Parallel()
	h := NewHandler([]string{"/bin/true"}, WithContainment(nil, "sess-1"))
	if h.cfg.containment != nil {
		t.Fatal("nil Containment should stay nil")
	}
	// The nil handle's methods must be safe, since ensureStarted calls
	// releaseFD/teardown unconditionally on the failure path.
	h.contain.releaseFD()
	h.contain.teardown()
	if mem, pids := h.contain.peaks(); mem != 0 || pids != 0 {
		t.Fatalf("nil peaks = (%d, %d), want (0, 0)", mem, pids)
	}
}
