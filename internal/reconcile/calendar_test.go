package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestTheScheduleValidatorRefusesWhatSystemdWouldExecute.
//
// This validator is not a calendar parser and does not try to be: systemd's
// grammar is large and quietly extended, and reimplementing it would be a bug
// farm whose failures are silent timers. What it refuses is the part that is
// not about calendars at all -- a value that would stop being a value and
// start being a directive.
func TestTheScheduleValidatorRefusesWhatSystemdWouldExecute(t *testing.T) {
	bad := map[string]string{
		"a newline makes a second directive": "*-*-* 05:00:00\nExecStart=/bin/sh -c id",
		"a carriage return does too":         "*-*-* 05:00:00\rExecStart=/bin/false",
		"a NUL truncates the line":           "*-*-* 05:00:00\x00",
		"empty never fires":                  "",
		"whitespace only never fires":        "   ",
		"leading space breaks every diff":    " *-*-* 05:00:00",
		"trailing space too":                 "*-*-* 05:00:00 ",
	}
	for why, expr := range bad {
		if err := ValidateCalendar(expr); err == nil {
			t.Errorf("%s: ValidateCalendar(%q) accepted it", why, expr)
		} else if !errors.Is(err, ErrCalendar) {
			t.Errorf("%s: error does not wrap ErrCalendar: %v", why, err)
		}
	}

	// The expressions the shipped manifests and section 6.1 actually use must
	// pass, or every one of them would have to be special-cased.
	for _, expr := range []string{
		"*-*-* 05:00:00 America/Denver",
		"*-*-* 04:00:00 America/Denver",
		"Mon..Fri 04:00",
		"daily",
		"weekly",
		"*-*-01 03:00:00 UTC",
	} {
		if err := ValidateCalendar(expr); err != nil {
			t.Errorf("ValidateCalendar(%q) = %v, want it accepted", expr, err)
		}
	}
}

// TestAnUncheckedScheduleIsReportedAsUncheckedAndNotAsValid.
//
// There is no systemd on the development machine, so the grammar cannot be
// verified there. Answering "valid" would move a broken nightly from `gvm
// apply` on the box to the first night it did not fire, which is the least
// observable failure in this subsystem.
func TestAnUncheckedScheduleIsReportedAsUncheckedAndNotAsValid(t *testing.T) {
	unchecked, err := CheckSchedules(context.Background(), NoCalendarCheck{}, map[string]string{
		"nightly": "*-*-* 05:00:00 America/Denver",
	})
	if err != nil {
		t.Fatalf("a well-formed schedule was rejected: %v", err)
	}
	if !unchecked {
		t.Error("CheckSchedules did not report that it never asked systemd")
	}
}

// TestEveryBrokenScheduleIsReportedInOnePass.
//
// An operator editing three jobs wants three answers from one `gvm apply`.
// Reporting one per run is how a five-minute edit becomes fifteen.
func TestEveryBrokenScheduleIsReportedInOnePass(t *testing.T) {
	_, err := CheckSchedules(context.Background(), NoCalendarCheck{}, map[string]string{
		"nightly": "",
		"backup":  "*-*-* 04:00:00\nExecStart=/bin/false",
		"weekly":  "*-*-* 03:00:00 UTC",
	})
	if err == nil {
		t.Fatal("two broken schedules produced no error")
	}
	for _, want := range []string{"jobs.backup.schedule", "jobs.nightly.schedule"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report omits %s:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "jobs.weekly.schedule") {
		t.Errorf("a valid schedule was reported as broken:\n%v", err)
	}
}

// TestTheCheckerRefusesTheDangerousValuesEvenWhereItCannotAskSystemd.
//
// The pure half runs on every platform, so a schedule carrying a newline is
// refused identically on the development machine and on the target. That is a
// unit-file syntax question and is not systemd's to answer.
func TestTheCheckerRefusesTheDangerousValuesEvenWhereItCannotAskSystemd(t *testing.T) {
	err := SystemdCalendar{}.CheckCalendar(context.Background(), "*-*-* 05:00:00\nExecStart=/bin/sh")
	if err == nil {
		t.Fatal("a schedule carrying a newline reached the subprocess check")
	}
	if errors.Is(err, ErrCalendarUnchecked) {
		t.Error("a newline was reported as merely unchecked rather than refused")
	}
	if !errors.Is(err, ErrCalendar) {
		t.Errorf("the error does not wrap ErrCalendar: %v", err)
	}
}

// TestCheckSchedulesWithNoCheckerStillValidatesTheSyntax.
//
// A nil checker means "no systemd here", not "skip the checks".
func TestCheckSchedulesWithNoCheckerStillValidatesTheSyntax(t *testing.T) {
	unchecked, err := CheckSchedules(context.Background(), nil, map[string]string{
		"nightly": "*-*-* 05:00:00\nWants=evil.service",
	})
	if err == nil {
		t.Fatal("a nil checker skipped the syntax refusal too")
	}
	_ = unchecked
}
