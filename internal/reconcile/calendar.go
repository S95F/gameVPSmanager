package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrCalendar is the sentinel every schedule refusal wraps.
var ErrCalendar = errors.New("reconcile: invalid OnCalendar expression")

// maxCalendar bounds the expression.
//
// systemd's own grammar has no length limit worth quoting, but every legal
// expression an operator writes -- "*-*-* 05:00:00 America/Denver", "Mon..Fri
// 04:00" -- is under forty characters. A 200-character schedule is not a
// schedule; it is something else that got into the field.
const maxCalendar = 200

// ValidateCalendar is the check that runs EVERYWHERE, including on the
// development machine and inside `gvm apply --diff`.
//
// It is deliberately not a reimplementation of systemd's calendar grammar.
// That grammar is large, quietly extended between releases, and has no Go
// implementation worth trusting; reimplementing it would be a bug farm whose
// failures are silent timers. What this function does is the part that is not
// about calendars at all: the schedule comes out of an operator-editable YAML
// file and is written verbatim into a systemd unit, so a value containing a
// newline is not a malformed schedule, it is a SECOND DIRECTIVE the operator
// did not write -- and the unit runs as root.
//
// The grammar itself is checked by CheckCalendar, which asks systemd.
func ValidateCalendar(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return fmt.Errorf("%w: the schedule is empty; a timer with no OnCalendar loads and "+
			"never fires", ErrCalendar)
	}
	if len(expr) > maxCalendar {
		return fmt.Errorf("%w: %d bytes, over the %d-byte ceiling", ErrCalendar, len(expr), maxCalendar)
	}
	if err := checkUnitValue("jobs.schedule", expr); err != nil {
		// The wrapped error already says a directive is literal to end of
		// line. Naming the schedule here is what tells an operator WHICH of
		// their jobs to look at.
		return fmt.Errorf("%w: %w", ErrCalendar, err)
	}
	if expr != strings.TrimSpace(expr) {
		// systemd tolerates it; a diff does not. Leading or trailing space
		// makes an unchanged schedule compare as changed forever, which
		// restarts the timer on every apply.
		return fmt.Errorf("%w: %q has leading or trailing whitespace", ErrCalendar, expr)
	}
	return nil
}

// CalendarChecker asks whether systemd itself accepts an expression.
//
// It is an interface so a plan can be built on a machine that has no systemd:
// the pure half (ValidateCalendar) always runs, and this one is skipped with a
// note rather than faked. A checker that answered "fine" without asking would
// be worse than none, because it would move the failure from apply time to the
// first missed nightly.
type CalendarChecker interface {
	// CheckCalendar returns nil when the expression is valid, an error
	// wrapping ErrCalendar when it is not, and platform.ErrNotSupported when
	// it could not ask.
	CheckCalendar(ctx context.Context, expr string) error
}

// SystemdCalendar is the production checker: it runs `systemd-analyze
// calendar`, exactly as nightly.sh did.
//
// This is the ONE place in this subsystem where running a program is correct,
// and the reason is stated in section 7.5: systemd's calendar grammar has no
// Go implementation worth trusting, and the authority on whether systemd
// accepts a string is systemd. noshell_test.go allows os/exec in
// calendar_linux.go alone, for this.
type SystemdCalendar struct{}

var _ CalendarChecker = SystemdCalendar{}

// NoCalendarCheck is the explicit "did not ask" checker.
//
// It exists so that a caller on a machine without systemd names that choice in
// its own construction rather than passing nil and hoping the callee treats it
// as optional.
type NoCalendarCheck struct{}

// CheckCalendar reports that nothing was checked. The error wraps
// ErrCalendarUnchecked rather than ErrCalendar, because "we could not ask" and
// "systemd said no" must not lead an operator to the same conclusion.
func (NoCalendarCheck) CheckCalendar(context.Context, string) error { return ErrCalendarUnchecked }

// ErrCalendarUnchecked means the grammar was not verified against systemd.
//
// A caller may proceed on it -- the development machine has no systemd and
// `gvm apply --diff` still has to work there -- but it must say so in its
// output. A plan that silently skipped the check and a plan that passed it
// must not look the same.
var ErrCalendarUnchecked = errors.New("reconcile: OnCalendar not checked against systemd on this host")

// CheckSchedules validates every job's schedule, returning one error naming
// all the broken ones.
//
// All of them, not the first: an operator editing three jobs wants three
// answers from one `gvm apply`, and reporting one per run is how a five-minute
// edit becomes fifteen.
func CheckSchedules(ctx context.Context, c CalendarChecker, schedules map[string]string) (unchecked bool, err error) {
	var bad []string
	names := sortedKeys(schedules)
	for _, job := range names {
		expr := schedules[job]
		if e := ValidateCalendar(expr); e != nil {
			bad = append(bad, fmt.Sprintf("jobs.%s.schedule: %v", job, e))
			continue
		}
		if c == nil {
			unchecked = true
			continue
		}
		switch e := c.CheckCalendar(ctx, expr); {
		case e == nil:
		case errors.Is(e, ErrCalendarUnchecked):
			unchecked = true
		default:
			bad = append(bad, fmt.Sprintf("jobs.%s.schedule: %v", job, e))
		}
	}
	if len(bad) > 0 {
		return unchecked, fmt.Errorf("%w:\n  %s", ErrCalendar, strings.Join(bad, "\n  "))
	}
	return unchecked, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// A stable order so two runs of `gvm apply --diff` on an unchanged host
	// produce byte-identical output; map order would make the diff itself the
	// thing that changed.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
