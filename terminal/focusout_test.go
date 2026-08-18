package terminal

import (
	"bytes"
	"testing"
)

// TestFocusOutOnEnable pins the keep-unfocused edge logic: the server writes a
// focus-out (ESC [ O) once each time the process enables DEC 1004 focus
// reporting, and only when WithKeepUnfocused is set. This is what keeps a
// focus-gated notifier (kiro-cli's OSC 9) emitting while the client sends no
// focus bytes of its own. The PTY write itself is thin wiring around this
// decision; the decision is what is exercised here.
func TestFocusOutOnEnable(t *testing.T) {
	t.Run("disabled by default: never injects even when reporting is on", func(t *testing.T) {
		h := NewHandler([]string{"true"})
		h.screen.FocusReporting = true
		if got := h.focusOutOnEnable(); got != nil {
			t.Errorf("focusOutOnEnable() = %q without WithKeepUnfocused, want nil", got)
		}
	})

	t.Run("explicit false reproduces the no-option default", func(t *testing.T) {
		h := NewHandler([]string{"true"}, WithKeepUnfocused(false))
		h.screen.FocusReporting = true
		if got := h.focusOutOnEnable(); got != nil {
			t.Errorf("focusOutOnEnable() with WithKeepUnfocused(false) = %q, want nil", got)
		}
	})

	t.Run("the last WithKeepUnfocused wins", func(t *testing.T) {
		h := NewHandler([]string{"true"}, WithKeepUnfocused(true), WithKeepUnfocused(false))
		h.screen.FocusReporting = true
		if got := h.focusOutOnEnable(); got != nil {
			t.Errorf("focusOutOnEnable() after true-then-false = %q, want nil (last option wins)", got)
		}
		h2 := NewHandler([]string{"true"}, WithKeepUnfocused(false), WithKeepUnfocused(true))
		h2.screen.FocusReporting = true
		if got := h2.focusOutOnEnable(); !bytes.Equal(got, focusOutSeq) {
			t.Errorf("focusOutOnEnable() after false-then-true = %q, want ESC[O (last option wins)", got)
		}
	})

	t.Run("injects on the enable edge, not on the steady level", func(t *testing.T) {
		h := NewHandler([]string{"true"}, WithKeepUnfocused(true))

		// Reporting still off: nothing to inject.
		if got := h.focusOutOnEnable(); got != nil {
			t.Errorf("focusOutOnEnable() with reporting off = %q, want nil", got)
		}

		// Rising edge (the process issued CSI ?1004h): inject focus-out.
		h.screen.FocusReporting = true
		if got := h.focusOutOnEnable(); !bytes.Equal(got, focusOutSeq) {
			t.Errorf("focusOutOnEnable() on enable edge = %q, want ESC[O", got)
		}

		// Steady enabled (no new edge): do not repeat.
		if got := h.focusOutOnEnable(); got != nil {
			t.Errorf("focusOutOnEnable() on steady enabled = %q, want nil", got)
		}

		// Disable then re-enable: a fresh edge re-pins the process to unfocused.
		h.screen.FocusReporting = false
		if got := h.focusOutOnEnable(); got != nil {
			t.Errorf("focusOutOnEnable() on disable = %q, want nil", got)
		}
		h.screen.FocusReporting = true
		if got := h.focusOutOnEnable(); !bytes.Equal(got, focusOutSeq) {
			t.Errorf("focusOutOnEnable() on re-enable edge = %q, want ESC[O", got)
		}
	})
}
