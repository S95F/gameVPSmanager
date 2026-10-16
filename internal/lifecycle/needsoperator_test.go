package lifecycle

import (
	"testing"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// TestNeedsOperatorMatchesRestartPreventExitStatus holds NeedsOperator and the
// packaged unit together: every exit code systemd will not restart (the
// exported RestartPreventExitStatus, which a packaging test pins to
// gvm@.service) means the unit needs a human, and so does systemd's own
// start-limit-hit. A retryable code, or a unit that is up, never does.
func TestNeedsOperatorMatchesRestartPreventExitStatus(t *testing.T) {
	failed := func(code int) Observation {
		return Observation{UnitChecked: true, Unit: platform.UnitState{ActiveState: "failed", ExecMainStatus: code}}
	}

	for _, code := range RestartPreventExitStatus {
		if !failed(code).NeedsOperator() {
			t.Errorf("exit %d is restart-prevented but NeedsOperator is false", code)
		}
	}

	slh := Observation{UnitChecked: true, Unit: platform.UnitState{ActiveState: "failed", Result: "start-limit-hit"}}
	if !slh.NeedsOperator() {
		t.Error("start-limit-hit should need an operator")
	}

	for _, code := range []int{0, 70, 75} {
		if failed(code).NeedsOperator() {
			t.Errorf("exit %d is retryable but NeedsOperator is true", code)
		}
	}

	// A unit that is currently active never needs an operator, even if its
	// ExecMainStatus still carries a code from a previous exec.
	up := Observation{UnitChecked: true, Unit: platform.UnitState{ActiveState: "active", ExecMainStatus: 65}}
	if up.NeedsOperator() {
		t.Error("an active unit must not need an operator")
	}

	// An unchecked manager is not evidence of anything.
	if (Observation{}).NeedsOperator() {
		t.Error("an unchecked unit must not need an operator")
	}
}

// TestNeedsOperatorStopsTheGuard: a unit that needs a human is not reported as
// desired-running-and-down, so the guard leaves it alone.
func TestNeedsOperatorStopsTheGuard(t *testing.T) {
	o := Observation{
		Enabled: true, UnitChecked: true,
		Unit:   platform.UnitState{ActiveState: "failed", ExecMainStatus: 65},
		DropIn: DropInHeader{SpecSHA256: "abc"},
	}
	if o.DesiredRunningAndDown() {
		t.Error("a unit that needs an operator must not be desired-running-and-down")
	}
	// The same unit with a retryable exit IS desired-running-and-down.
	o.Unit.ExecMainStatus = 70
	if !o.DesiredRunningAndDown() {
		t.Error("a retryable failure should still be desired-running-and-down")
	}
}
