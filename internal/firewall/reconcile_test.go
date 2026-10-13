package firewall

import (
	"errors"
	"testing"
)

func TestReconcileGuardRunsFirst(t *testing.T) {
	// A desired set that doesn't admit SSH must fail before any steps are planned.
	g := Guard{SSHPorts: []int{22}, Confident: true, LiveParseTrusted: true}
	desired := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 80, Comment: "gvm:host:http"},
	}
	_, err := Reconcile(nil, desired, g)
	if !errors.Is(err, ErrWouldLockOut) {
		t.Fatalf("expected ErrWouldLockOut, got %v", err)
	}
}

func TestReconcileAddsSSHFirst(t *testing.T) {
	g := Guard{SSHPorts: []int{22}, Confident: true, LiveParseTrusted: true}
	desired := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 27015, Comment: "gvm:pz1:game"},
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
	}
	plan, err := Reconcile(nil, desired, g)
	if err != nil {
		t.Fatal(err)
	}
	var adds []Step
	for _, s := range plan.Steps {
		if s.Kind == StepAdd {
			adds = append(adds, s)
		}
	}
	if len(adds) < 2 {
		t.Fatalf("expected 2 add steps, got %d", len(adds))
	}
	if adds[0].Rule.Port != 22 {
		t.Errorf("first add should be SSH port 22, got %d", adds[0].Rule.Port)
	}
}

func TestReconcileAddsBeforeDeletes(t *testing.T) {
	g := Guard{SSHPorts: []int{22}, Confident: true, LiveParseTrusted: true}
	live := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 27015, Comment: "gvm:pz1:old-game"},
	}
	desired := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 8080, Comment: "gvm:pz1:new-game"},
	}
	plan, err := Reconcile(live, desired, g)
	if err != nil {
		t.Fatal(err)
	}
	sawAdd := false
	for _, s := range plan.Steps {
		if s.Kind == StepDelete && !sawAdd {
			t.Error("delete appeared before any add")
		}
		if s.Kind == StepAdd {
			sawAdd = true
		}
	}
}

func TestReconcileNeverDeletesSSHEvenIfTagged(t *testing.T) {
	g := Guard{SSHPorts: []int{22}, Confident: true, LiveParseTrusted: true}
	live := RuleSet{
		// This rule is gvm-tagged but points at SSH — must never be deleted.
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
	}
	desired := RuleSet{
		// Desired still admits SSH (same rule), but let's say the comment changed.
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh-v2"},
	}
	plan, err := Reconcile(live, desired, g)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range plan.Steps {
		if s.Kind == StepDelete && s.Rule.Port == 22 {
			t.Error("Reconcile produced a delete step for an SSH port")
		}
	}
}

func TestReconcileDeletesSuppressedWhenNotTrusted(t *testing.T) {
	// D-42: when LiveParseTrusted=false, zero deletions regardless of the live set.
	g := Guard{SSHPorts: []int{22}, Confident: true, LiveParseTrusted: false}
	live := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 27015, Comment: "gvm:pz1:stale"},
	}
	desired := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
	}
	plan, err := Reconcile(live, desired, g)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range plan.Steps {
		if s.Kind == StepDelete {
			t.Errorf("delete produced with LiveParseTrusted=false: %+v", s)
		}
	}
}

func TestReconcileForeignRulesReported(t *testing.T) {
	g := Guard{SSHPorts: []int{22}, Confident: true, LiveParseTrusted: true}
	live := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 9090, Comment: "operator-added"},
	}
	desired := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
	}
	plan, err := Reconcile(live, desired, g)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ForeignRules) != 1 || plan.ForeignRules[0].Port != 9090 {
		t.Errorf("expected foreign rule on 9090, got %+v", plan.ForeignRules)
	}
	for _, s := range plan.Steps {
		if s.Kind == StepDelete && s.Rule.Port == 9090 {
			t.Error("foreign rule must never be deleted")
		}
	}
}

func TestReconcileAdoptionNotAddDelete(t *testing.T) {
	// A foreign rule occupying the exact desired slot → adopt, not add+delete.
	g := Guard{SSHPorts: []int{22}, Confident: true, LiveParseTrusted: true}
	live := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 7777, Comment: "ufw-before-rules"},
	}
	desired := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 7777, Comment: "gvm:pz1:game"},
	}
	plan, err := Reconcile(live, desired, g)
	if err != nil {
		t.Fatal(err)
	}
	var adopted, added, deleted int
	for _, s := range plan.Steps {
		switch s.Kind {
		case StepAdopt:
			adopted++
		case StepAdd:
			if s.Rule.Port == 7777 {
				added++
			}
		case StepDelete:
			if s.Rule.Port == 7777 {
				deleted++
			}
		}
	}
	if adopted != 1 {
		t.Errorf("expected 1 adopt step, got %d", adopted)
	}
	if added != 0 || deleted != 0 {
		t.Errorf("expected 0 add/delete for 7777, got add=%d del=%d", added, deleted)
	}
}

func TestReconcileShadowWarning(t *testing.T) {
	g := Guard{SSHPorts: []int{22}, Confident: true, LiveParseTrusted: true}
	live := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
		{Verb: VerbDeny, Proto: ProtoTCP, Port: 7777, Comment: ""},
	}
	desired := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 7777, Comment: "gvm:pz1:game"},
	}
	plan, err := Reconcile(live, desired, g)
	if err != nil {
		t.Fatal(err)
	}
	var shadows int
	for _, s := range plan.Steps {
		if s.Kind == StepShadow {
			shadows++
		}
	}
	if shadows != 1 {
		t.Errorf("expected 1 shadow warning, got %d", shadows)
	}
}

func TestReconcileAlreadyCorrectNotDuplicated(t *testing.T) {
	// A rule already present with the correct tag must not appear in Steps.
	g := Guard{SSHPorts: []int{22}, Confident: true, LiveParseTrusted: true}
	live := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
	}
	desired := RuleSet{
		{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, Comment: "gvm:host:ssh"},
	}
	plan, err := Reconcile(live, desired, g)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 0 {
		t.Errorf("expected zero steps for idempotent set, got %d: %+v", len(plan.Steps), plan.Steps)
	}
	if len(plan.AlreadyCorrect) != 1 {
		t.Errorf("expected 1 already-correct entry, got %d", len(plan.AlreadyCorrect))
	}
}
