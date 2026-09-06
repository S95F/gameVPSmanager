package lifecycle

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStatusIsAProjectionAndNotASecondOpinion(t *testing.T) {
	// Every row of the phase table, projected. The status view must agree
	// with Derive on every one: two functions that disagreed would give
	// `gvm status` and `gvm doctor` two stories about one server.
	for _, tc := range deriveCases() {
		o := base()
		tc.setup(&o)
		v := Derive(o)
		s := NewStatus(o, v)
		if s.Phase != v.Phase || s.Rule != v.Rule || s.Reason != v.Reason || s.Hint != v.Hint {
			t.Errorf("%s: Status %s/%s does not match the verdict %s/%s",
				tc.name, s.Phase, s.Rule, v.Phase, v.Rule)
		}
		if s.Ready && s.Phase != PhaseRunning {
			t.Errorf("%s: Ready is true in phase %s", tc.name, s.Phase)
		}
	}
}

// TestDriftIsOnlyMeaningfulOnceSomethingHasBeenApplied.
//
// An unapplied spec is "not applied", not "drifted". Conflating them makes the
// drift count on a freshly created box equal to the number of instances, and a
// count that is always wrong is a count nobody reads.
func TestDriftIsOnlyMeaningfulOnceSomethingHasBeenApplied(t *testing.T) {
	o := base()
	o.DropInPresent = false
	o.DropIn = DropInHeader{}
	if s := NewStatus(o, Derive(o)); s.Drift {
		t.Fatal("an instance that was never applied is reported as drifted")
	}

	o = base()
	o.SpecSHA256 = "1111111111111111"
	if s := NewStatus(o, Derive(o)); !s.Drift {
		t.Fatal("a spec edited since the last apply is not reported as drifted")
	}
}

// TestARunningInstanceStillShowsItsDrift. The one an operator will not
// otherwise notice: it is working, so nothing draws the eye, until a restart
// applies a config they forgot they wrote.
func TestARunningInstanceStillShowsItsDrift(t *testing.T) {
	o := base()
	o.SpecSHA256 = "1111111111111111"
	s := NewStatus(o, Derive(o))
	if s.Phase != PhaseRunning {
		t.Fatalf("phase = %s", s.Phase)
	}
	if !strings.Contains(s.Line(), "spec edited, not applied") {
		t.Fatalf("the list row hides the drift: %q", s.Line())
	}
}

func TestReportSaysWhenTheUnitStateWasNotChecked(t *testing.T) {
	o := base()
	o.UnitChecked = false
	rep := NewStatus(o, Derive(o)).Report()
	if !strings.Contains(rep, "not checked on this host") {
		t.Fatalf("an unchecked unit is not reported as unchecked:\n%s", rep)
	}
	// It must not be reported as inactive either: a stopped server and an
	// unreachable service manager send an operator to two different places.
	if strings.Contains(rep, "inactive") {
		t.Fatalf("an unchecked unit was reported as inactive:\n%s", rep)
	}
}

func TestReportPrintsTheHintAndTheFindings(t *testing.T) {
	o := base()
	o.Unit.ActiveState = "inactive"
	o.Status = nil
	o.LocalDropIn = true
	v := Derive(o)
	rep := NewStatus(o, v).Report()
	if !strings.Contains(rep, v.Hint) {
		t.Errorf("the report does not print the hint:\n%s", rep)
	}
	if !strings.Contains(rep, "90-local.conf") {
		t.Errorf("the report does not mention the operator's own drop-in:\n%s", rep)
	}
	// The phase finding is already the reason line; printing it twice under a
	// second heading reads like two separate problems.
	if strings.Count(rep, v.Reason) != 1 {
		t.Errorf("the reason is printed %d times:\n%s", strings.Count(rep, v.Reason), rep)
	}
}

func TestSummariseCountsRunningStoppedAndProblems(t *testing.T) {
	mk := func(p Phase) Status { return Status{Phase: p} }
	got := Summarise([]Status{
		mk(PhaseRunning), mk(PhaseRunning), mk(PhaseStopped),
		mk(PhaseFailed), mk(PhaseDegraded), mk(PhaseDeclared),
	})
	if got.Total != 6 || got.Running != 2 || got.Problems != 2 || got.Stopped != 2 {
		t.Fatalf("Summarise = %+v", got)
	}
}

// TestStatusMarshalsToTheMirrorsKeys. A dashboard reading this JSON and one
// reading instance_status must see the same names, or the two views of one
// server are two schemas.
func TestStatusMarshalsToTheMirrorsKeys(t *testing.T) {
	o := base()
	b, err := json.Marshal(NewStatus(o, Derive(o)))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"instance", "phase", "phase_reason", "active_state", "sub_state",
		"unit_result", "main_pid", "players_online", "control_kind", "ready_at",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("the JSON has no %q key", key)
		}
	}
}

func TestStatusesAreSortedByName(t *testing.T) {
	rep := SweepReport{Instances: []InstanceReport{
		{Name: "valheim-1", Observation: Observation{Name: "valheim-1"}},
		{Name: "ck-1", Observation: Observation{Name: "ck-1"}},
		{Name: "pz-main", Observation: Observation{Name: "pz-main"}},
	}}
	got := Statuses(rep)
	want := []string{"ck-1", "pz-main", "valheim-1"}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("Statuses = %v, want %v", got, want)
		}
	}
}

func TestPhaseSeverityKeepsTheProblemCountMeaningful(t *testing.T) {
	// Section 20's note: accepted and informational states are notices, so
	// "6 problems" stays a number worth reacting to. A stopped instance is
	// doing what it was told and is not a problem.
	for _, p := range []Phase{PhaseStopped, PhaseDeclared, PhaseAbsent, PhaseUpdating} {
		if p.Severity() == SevProblem {
			t.Errorf("%s counts as a problem", p)
		}
	}
	for _, p := range []Phase{PhaseFailed, PhaseDegraded, PhaseAwaitingManualFiles} {
		if p.Severity() != SevProblem {
			t.Errorf("%s does not count as a problem", p)
		}
	}
	// A degraded instance may well be serving players, and hiding it from the
	// "up" list hides the one that most needs looking at.
	if !PhaseDegraded.Serving() {
		t.Error("degraded is not counted as serving")
	}
	if PhaseFailed.Transitional() {
		t.Error("failed is transitional; a caller would poll it forever")
	}
}
