package reconcile

import (
	"strings"
	"testing"
)

// TestTheEnableStepIsUnchangedWhenTheUnitIsAlreadyEnabled.
//
// It is the last step that stood between `gvm apply` and honest idempotence.
// Everything else in a plan compares what it would write against what is on
// disk; the enablement had nothing to compare against, so it always read as
// work. A plan that always shows work is a plan an operator learns to skim,
// and the apply they skim past is the one that mattered.
//
// The observation is the multi-user.target.wants symlink, which is what
// `systemctl enable` creates from the packaged template's [Install] section.
func TestTheEnableStepIsUnchangedWhenTheUnitIsAlreadyEnabled(t *testing.T) {
	find := func(p *Plan) Step {
		t.Helper()
		for _, s := range p.Describe() {
			if s.Kind == KindUnit {
				return s
			}
		}
		t.Fatal("the plan has no unit step")
		return Step{}
	}

	before, _, _ := planFor(t, "valheim-1", "valheim", nil)
	if got := find(before); got.Change != Update {
		t.Errorf("a unit that is not enabled plans %q, want %q", got.Change, Update)
	}

	after, _, _ := planFor(t, "valheim-1", "valheim", func(pi *PlanInput) {
		ins := newInspect().account(pi.Instance.Spec.User)
		ins.mkdir(WantsLink(pi.Instance.Metadata.Name))
		pi.Inspect = ins
	})
	got := find(after)
	if got.Change != Unchanged {
		t.Errorf("an already-enabled unit plans %q, want %q", got.Change, Unchanged)
	}
	if !strings.Contains(got.Note, "already enabled") {
		t.Errorf("the note does not say why nothing changes: %q", got.Note)
	}
	// The verb is still issued: the symlink is evidence, not proof, and
	// re-asserting an enablement costs one dbus call.
	if !strings.Contains(got.Object, "unit.enable") {
		t.Errorf("the step stopped issuing the verb: %q", got.Object)
	}
}

// The disabled direction has to be symmetrical, or `enabled: false` reports a
// disable on every apply forever.
func TestTheDisableStepIsUnchangedWhenTheUnitIsNotEnabled(t *testing.T) {
	p, _, _ := planFor(t, "valheim-1", "valheim", func(pi *PlanInput) {
		pi.Instance.Spec.Enabled = false
	})
	for _, s := range p.Describe() {
		if s.Kind != KindUnit {
			continue
		}
		if s.Change != Unchanged {
			t.Errorf("disable on a unit that is not enabled plans %q", s.Change)
		}
		if !strings.Contains(s.Object, "unit.disable") {
			t.Errorf("step = %q", s.Object)
		}
		return
	}
	t.Fatal("the plan has no unit step")
}
