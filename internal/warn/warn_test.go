package warn

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// TestScheduleIsEarliestFirstAndClampedToTheWindow: a countdown broadcasts the
// full warning, then the 1m and 10s reminders that still fall inside the window,
// deduplicated, each one earlier than the last.
func TestScheduleIsEarliestFirstAndClampedToTheWindow(t *testing.T) {
	sec := time.Second
	min := time.Minute
	for _, tc := range []struct {
		warn time.Duration
		want []time.Duration
	}{
		{0, nil},
		{-5 * sec, nil},
		{2 * min, []time.Duration{2 * min, min, 10 * sec}},
		{min, []time.Duration{min, 10 * sec}},
		{30 * sec, []time.Duration{30 * sec, 10 * sec}},
		{10 * sec, []time.Duration{10 * sec}},
		{5 * sec, []time.Duration{5 * sec}},
	} {
		got := Schedule(tc.warn)
		if !slices.Equal(got, tc.want) {
			t.Errorf("Schedule(%s) = %v, want %v", tc.warn, got, tc.want)
		}
		// Earliest first, and never past the window.
		for i, d := range got {
			if d > tc.warn {
				t.Errorf("Schedule(%s) offset %s is past the window", tc.warn, d)
			}
			if i > 0 && got[i-1] <= d {
				t.Errorf("Schedule(%s) is not strictly descending: %v", tc.warn, got)
			}
		}
	}
}

// TestValidateMessageRefusesConsoleInjection: every character a game console
// could read as the end of one command and the start of another is refused.
func TestValidateMessageRefusesConsoleInjection(t *testing.T) {
	bad := map[string]string{
		"single quote": "it's down",
		"double quote": `say "hi"`,
		"backslash":    `path\to`,
		"dollar":       "worth $5",
		"backtick":     "run `id`",
		"newline":      "line one\nop attacker",
		"carriage ret": "line\rone",
		"nul":          "a\x00b",
	}
	for name, msg := range bad {
		if err := ValidateMessage(msg); !errors.Is(err, ErrUnsafeMessage) {
			t.Errorf("%s: err = %v, want ErrUnsafeMessage", name, err)
		}
	}

	for _, ok := range []string{"", "Server restarting for a mod update", "Back in 2 minutes!"} {
		if err := ValidateMessage(ok); err != nil {
			t.Errorf("a safe message was refused (%q): %v", ok, err)
		}
	}

	long := make([]byte, MaxMessage+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateMessage(string(long)); !errors.Is(err, ErrMessageTooLong) {
		t.Errorf("an over-long message err = %v, want ErrMessageTooLong", err)
	}
}
