//go:build linux

package reconcile

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// systemdAnalyzePaths are the absolute paths systemd-analyze ships at.
//
// Absolute, and resolved by stat rather than through PATH: this runs inside a
// root process, and letting an inherited PATH decide which binary answers
// "is this schedule valid" is a decision nobody made.
var systemdAnalyzePaths = []string{
	"/usr/bin/systemd-analyze",
	"/bin/systemd-analyze",
}

// calendarTimeout bounds the check.
//
// systemd-analyze parses a string and exits; it does not talk to the manager
// and cannot block on dbus. The timeout exists because a root daemon must have
// one for every subprocess it starts, not because this one is expected to
// need it.
const calendarTimeout = 10 * time.Second

// CheckCalendar asks systemd whether it accepts the expression.
//
// The expression is passed as ONE argv element to a program resolved from an
// absolute path, with an emptied environment. There is no shell, so the
// quoting question -- what happens to a schedule containing a quote, a
// semicolon or a backtick -- does not arise: the bytes reach systemd-analyze
// exactly as the operator wrote them, and systemd-analyze either accepts them
// or does not.
func (SystemdCalendar) CheckCalendar(ctx context.Context, expr string) error {
	// The pure checks first. They refuse the values that must never reach a
	// subprocess argv at all, and they run on every platform, so a schedule
	// carrying a newline is refused identically on the development machine
	// and on the target.
	if err := ValidateCalendar(expr); err != nil {
		return err
	}
	bin, err := findTool(systemdAnalyzePaths)
	if err != nil {
		return ErrCalendarUnchecked
	}

	ctx, cancel := context.WithTimeout(ctx, calendarTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "calendar", expr)
	// An emptied environment rather than the inherited one: this process is
	// root and the child needs nothing from it. LANG in particular can change
	// how systemd-analyze formats its output.
	cmd.Env = []string{}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w: systemd-analyze did not answer within %s", ErrCalendar, calendarTimeout)
	}
	// systemd-analyze prints "Failed to parse calendar expression: Invalid
	// argument" and exits non-zero. Its own sentence is more useful than any
	// paraphrase, so it is surfaced verbatim and trimmed only of blank lines.
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("%w: %q: %s", ErrCalendar, expr, msg)
}

// findTool returns the first candidate that exists and is a regular file.
//
// A directory or a dangling symlink at one of these paths is skipped rather
// than run, so a half-finished install does not turn into an exec failure the
// caller reads as "this schedule is invalid".
func findTool(candidates []string) (string, error) {
	for _, p := range candidates {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		return p, nil
	}
	return "", fmt.Errorf("none of %s is present", strings.Join(candidates, ", "))
}
