package lifecycle

import (
	"context"
	"fmt"
)

// CheckSeverity indicates how serious a doctor check finding is.
type CheckSeverity string

const (
	// CheckProblem is a definite error — emitted as ✗. Non-zero exit from `gvm doctor`.
	CheckProblem CheckSeverity = "problem"
	// CheckWarning may cause trouble — emitted as !.
	CheckWarning CheckSeverity = "warning"
	// CheckNote is informational — emitted as ·.
	CheckNote CheckSeverity = "note"
	// CheckOK means the check passed — emitted as ✓.
	CheckOK CheckSeverity = "ok"
	// CheckSkip means the check could not run (prereq missing) — emitted as -.
	CheckSkip CheckSeverity = "skip"
)

// CheckFinding is one result from a doctor Check.
type CheckFinding struct {
	ID       string        // e.g. "HOST-009"
	Severity CheckSeverity // CheckProblem/CheckWarning/CheckNote/CheckOK/CheckSkip
	Message  string        // human text — INST-002 OOM sentence lives here verbatim
	Fix      string        // optional one-liner the operator can run
}

// DoctorCheck is one doctor check.
type DoctorCheck struct {
	// ID is the stable identifier (HOST-009, EDGE-001, …).
	ID string
	// Summary is one line describing what the check verifies.
	Summary string
	// Run executes the check and returns a CheckFinding.
	// Run must NEVER mutate system state.
	Run func(ctx context.Context, env CheckEnv) CheckFinding
}

// CheckEnv provides read-only access to system state.
// All methods must be safe to call with Postgres down.
type CheckEnv interface {
	// PostgresReachable reports whether Postgres is reachable via peer auth.
	// When false, DB-backed checks should return CheckSkip.
	PostgresReachable(ctx context.Context) bool
}

// CheckRegistry holds every doctor check.
type CheckRegistry struct {
	checks []DoctorCheck
}

// Register adds a check to the registry.
func (r *CheckRegistry) Register(c DoctorCheck) {
	r.checks = append(r.checks, c)
}

// All returns every registered check in registration order.
func (r *CheckRegistry) All() []DoctorCheck {
	out := make([]DoctorCheck, len(r.checks))
	copy(out, r.checks)
	return out
}

// RunAll executes every registered check and returns the findings.
// It never panics; a check that panics is caught and recorded as a CheckProblem.
func (r *CheckRegistry) RunAll(ctx context.Context, env CheckEnv) []CheckFinding {
	out := make([]CheckFinding, 0, len(r.checks))
	for _, c := range r.checks {
		f := safeDoctorRun(ctx, env, c)
		out = append(out, f)
	}
	return out
}

func safeDoctorRun(ctx context.Context, env CheckEnv, c DoctorCheck) (f CheckFinding) {
	defer func() {
		if p := recover(); p != nil {
			f = CheckFinding{
				ID:       c.ID,
				Severity: CheckProblem,
				Message:  fmt.Sprintf("check panicked: %v", p),
			}
		}
	}()
	return c.Run(ctx, env)
}

// CountCheckProblems returns the number of CheckProblem-severity findings.
func CountCheckProblems(findings []CheckFinding) int {
	n := 0
	for _, f := range findings {
		if f.Severity == CheckProblem {
			n++
		}
	}
	return n
}

// FormatFinding renders a CheckFinding as one line with the status glyph prefix.
//
//	✓ HOST-009  /run/gvm 0755 root:root; /run/gvm/web 2750 gvm:caddy
//	✗ HOST-006  /etc/gvm/master.key is mode 0640, want 0400
//	!  FW-004   rcon port bound to 0.0.0.0
//	·  INST-011 ambiguous Workshop item
//	-  HOST-001 skipped (postgres is not reachable)
func FormatFinding(f CheckFinding) string {
	var glyph string
	switch f.Severity {
	case CheckOK:
		glyph = "✓"
	case CheckProblem:
		glyph = "✗"
	case CheckWarning:
		glyph = "!"
	case CheckNote:
		glyph = "·"
	default:
		glyph = "-"
	}
	if f.Fix != "" {
		return fmt.Sprintf("  %s %-10s %s\n     Fix: %s", glyph, f.ID, f.Message, f.Fix)
	}
	return fmt.Sprintf("  %s %-10s %s", glyph, f.ID, f.Message)
}
