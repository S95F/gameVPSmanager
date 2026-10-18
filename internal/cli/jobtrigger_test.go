package cli

import (
	"strings"
	"testing"
)

// TestTimerTriggerAtTerminal pins the pure decision so the refusal is tested
// without faking a terminal: refuse only a human (TTY, not under a unit) who
// claims a timer trigger.
func TestTimerTriggerAtTerminal(t *testing.T) {
	cases := []struct {
		trigger          string
		underUnit, isTTY bool
		want             bool
	}{
		{"timer", false, true, true},   // human at a terminal: refuse
		{"timer", true, true, false},   // under a unit: it really is a timer run
		{"timer", false, false, false}, // a script (no TTY): allowed
		{"cli", false, true, false},    // not a timer trigger: nothing to refuse
		{"manual", false, true, false},
	}
	for _, tc := range cases {
		if got := timerTriggerAtTerminal(tc.trigger, tc.underUnit, tc.isTTY); got != tc.want {
			t.Errorf("timerTriggerAtTerminal(%q, under=%v, tty=%v) = %v, want %v",
				tc.trigger, tc.underUnit, tc.isTTY, got, tc.want)
		}
	}
}

// TestJobRunTriggerTimerNotRefusedForAScript: the refusal is TTY-gated, so a
// run whose stdout is not a terminal (a test buffer, a pipe) with --trigger
// timer is NOT refused — it gets past the trigger check to slug resolution.
func TestJobRunTriggerTimerNotRefusedForAScript(t *testing.T) {
	h := newTestHost(t)
	_, out, errb := runWith(t, h, "", "job", "run", "no_such_slug", "--trigger", "timer")
	if strings.Contains(out+errb, "at a terminal") {
		t.Errorf("a non-TTY --trigger timer was refused as if at a terminal:\n%s", out+errb)
	}
}
