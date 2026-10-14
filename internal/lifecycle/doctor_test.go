package lifecycle

import (
	"context"
	"strings"
	"testing"
)

// fakeCheckEnv is a test double for CheckEnv.
type fakeCheckEnv struct{ pgReachable bool }

func (f fakeCheckEnv) PostgresReachable(_ context.Context) bool { return f.pgReachable }

// --- Registry and severity tests ---

func TestRegistryRunAllNoPanic(t *testing.T) {
	reg := &CheckRegistry{}
	reg.Register(DoctorCheck{
		ID:      "TEST-001",
		Summary: "always ok",
		Run: func(_ context.Context, _ CheckEnv) CheckFinding {
			return CheckFinding{ID: "TEST-001", Severity: CheckOK, Message: "ok"}
		},
	})
	reg.Register(DoctorCheck{
		ID:      "TEST-002",
		Summary: "always panics",
		Run: func(_ context.Context, _ CheckEnv) CheckFinding {
			panic("deliberate test panic")
		},
	})
	findings := reg.RunAll(context.Background(), fakeCheckEnv{})
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(findings))
	}
	if findings[1].Severity != CheckProblem {
		t.Errorf("panicking check must produce CheckProblem, got %v", findings[1].Severity)
	}
	if !strings.Contains(findings[1].Message, "deliberate test panic") {
		t.Errorf("problem message should contain panic value: %q", findings[1].Message)
	}
}

func TestCountCheckProblems(t *testing.T) {
	findings := []CheckFinding{
		{Severity: CheckOK},
		{Severity: CheckProblem},
		{Severity: CheckWarning},
		{Severity: CheckProblem},
	}
	if n := CountCheckProblems(findings); n != 2 {
		t.Errorf("expected 2, got %d", n)
	}
}

func TestFormatFindingGlyphs(t *testing.T) {
	cases := []struct {
		sev   CheckSeverity
		glyph string
	}{
		{CheckOK, "✓"},
		{CheckProblem, "✗"},
		{CheckWarning, "!"},
		{CheckNote, "·"},
		{CheckSkip, "-"},
	}
	for _, tc := range cases {
		f := CheckFinding{ID: "X-001", Severity: tc.sev, Message: "test"}
		out := FormatFinding(f)
		if !strings.Contains(out, tc.glyph) {
			t.Errorf("severity %s: expected glyph %q in %q", tc.sev, tc.glyph, out)
		}
	}
}

func TestFormatFindingWithFix(t *testing.T) {
	f := CheckFinding{
		ID:       "FW-001",
		Severity: CheckProblem,
		Message:  "firewall not active",
		Fix:      "gvm firewall enable",
	}
	out := FormatFinding(f)
	if !strings.Contains(out, "Fix:") {
		t.Errorf("expected Fix: line, got: %q", out)
	}
	if !strings.Contains(out, "gvm firewall enable") {
		t.Errorf("expected fix command in output, got: %q", out)
	}
}
