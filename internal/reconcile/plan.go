package reconcile

import (
	"context"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/instance"
)

// Kind says what a step is, for the two consumers that have to reason about a
// plan without running it: Validate, which enforces the write-probe ordering,
// and the printer, which groups the output.
//
// It is an enum rather than a free string because Validate's rule -- "no
// drop-in write without a probe of the same instance before it" -- is only
// enforceable if "this step is a drop-in write" is a fact and not a guess
// about a description.
type Kind string

const (
	// KindProbe is the D-15 write-probe. It changes nothing.
	KindProbe Kind = "probe"
	// KindDir is one dir.ensure.
	KindDir Kind = "dir"
	// KindUser is the instance's system account.
	KindUser Kind = "user"
	// KindDropIn is the generated 10-gvm.conf. Validate treats it specially.
	KindDropIn Kind = "dropin"
	// KindFile is any other control-plane file gvm owns.
	KindFile Kind = "file"
	// KindTimer is a job timer, which carries the RestartUnit subtlety.
	KindTimer Kind = "timer"
	// KindReload is the single daemon-reload an apply performs.
	KindReload Kind = "reload"
	// KindUnit is a supervision verb: enable, disable, start, restart.
	KindUnit Kind = "unit"
	// KindRemove deletes data. It is the only kind for which
	// Action.Destructive may be true.
	KindRemove Kind = "remove"
)

// Change is the sigil `gvm apply --diff` prints in front of a step.
//
// "unchanged" is a first-class outcome rather than an omission, because the
// most common apply is one that changes nothing and an operator needs to see
// that it looked.
type Change string

const (
	Add       Change = "+"
	Update    Change = "~"
	Unchanged Change = "="
	Delete    Change = "-"
	Warn      Change = "!"
)

// Step is one line of a plan, and the whole of what a caller may know about an
// Action without executing it.
type Step struct {
	Kind     Kind
	Change   Change
	Instance string

	// Object is what the step acts on, as the host will name it: a path, a
	// unit name, an account. It comes from the same derivation hostops uses,
	// so the plan and the audit row name the same object.
	Object string

	// Note is one line of WHY, shown indented under the step. It carries the
	// reasons an operator cannot derive: "data is under /home, which the
	// template's ProtectHome=read-only would hide", "the schedule changed, so
	// the timer is restarted rather than started".
	Note string

	// Diff is the unified diff of a file's contents, already masked. It is
	// empty for steps that write no file. It is carried on the Step rather
	// than recomputed by the printer so that whatever produced it -- which is
	// the only thing that knows which values were tainted -- is the thing
	// that masked it.
	Diff string
}

// String renders the step the way `gvm apply --diff` prints its first line.
func (s Step) String() string {
	out := string(s.Change) + " " + string(s.Kind) + " " + s.Object
	if s.Note != "" {
		out += "\n      " + s.Note
	}
	return out
}

// Action is one step of an apply: a description that can be printed and an
// effect that can be run.
//
// Do takes a *archive.Ref, and that parameter is archive-before-destroy
// expressed as a type rather than as a review comment. A non-destructive
// action is handed nil and ignores it. A destructive one cannot do its work
// without it: the only destructive primitive in this package is Remove, which
// forwards to instance.Destroy, whose signature takes the Ref -- so an
// implementation that "ignores" the parameter has no way to reach a deletion,
// and one that drops the parameter from its signature does not satisfy this
// interface. Both are compile errors, and testdata/ holds the fixtures that
// prove they still are.
type Action interface {
	// Describe returns the plan line. It must be pure: `gvm apply --diff`
	// calls it and executes nothing.
	Describe() Step

	// Destructive reports whether Do removes data. Apply refuses to run a
	// destructive action without a verified Ref, which is the runtime half of
	// the same rule.
	Destructive() bool

	// Do performs the step.
	Do(ctx context.Context, ref *archive.Ref) error
}

// Remove is the only destructive primitive an Action may call.
//
// It exists so that "delete something" has exactly one spelling inside a plan,
// and that spelling takes the Ref. instance.Destroy re-asserts the receipt and
// every target's proof independently, so this function adds no authority of
// its own -- it adds the property that the authority is a PARAMETER, which is
// what makes a Do that ignored its Ref fail to compile rather than fail to
// protect anything.
func Remove(ctx context.Context, ref *archive.Ref, targets []instance.Jailed) error {
	if !ref.Verified() {
		return fmt.Errorf("%w: the ref is nil or has never been verified", ErrNoArchive)
	}
	return instance.Destroy(ctx, ref, targets)
}

// Plan is an ordered list of actions and the complete description of an apply.
//
// It is a value, it is built without touching the host, and it is what
// `--diff` prints. Apply executes exactly this list in exactly this order.
type Plan struct {
	Instance string
	Steps    []Action
}

// Add appends actions, skipping nils so a builder can write conditional steps
// as `p.Add(maybeNil())` without a guard at every call site.
func (p *Plan) Add(as ...Action) {
	for _, a := range as {
		if a != nil {
			p.Steps = append(p.Steps, a)
		}
	}
}

// Describe returns every step's line, in execution order.
func (p *Plan) Describe() []Step {
	out := make([]Step, 0, len(p.Steps))
	for _, a := range p.Steps {
		out = append(out, a.Describe())
	}
	return out
}

// Destructive reports whether any step removes data, which is what decides
// whether `gvm apply` demands a confirmation and an archive.
func (p *Plan) Destructive() bool {
	for _, a := range p.Steps {
		if a.Destructive() {
			return true
		}
	}
	return false
}

// Changes returns the steps that would actually change the host.
func (p *Plan) Changes() []Step {
	var out []Step
	for _, s := range p.Describe() {
		if s.Change != Unchanged {
			out = append(out, s)
		}
	}
	return out
}

// Validate enforces the orderings the plan's meaning depends on.
//
// The write-probe rule is the one with teeth (D-15). A drop-in carries
// ProtectHome= and ReadWritePaths=, and the failure it can cause -- a game
// that starts, runs, and cannot write its own save directory -- is invisible
// until the world is lost. So the probe is not "recommended before" the write;
// a plan that writes a drop-in with no preceding probe of that instance is
// refused here, and Apply calls this before executing anything.
//
// It is checked on the PLAN rather than inside the drop-in action because the
// action cannot see what ran before it, and a probe that lived inside the
// drop-in action would be a probe whose absence nobody could observe in
// `--diff`.
func (p *Plan) Validate() error {
	probed := map[string]bool{}
	for i, a := range p.Steps {
		s := a.Describe()
		if s.Kind != KindProbe && s.Kind != KindDropIn && s.Instance == "" && p.Instance == "" {
			// A step with no instance in a plan with no instance cannot be
			// attributed in an audit row, which is the one thing every
			// mutating hostops verb requires.
			return fmt.Errorf("%w: step %d (%s %s) belongs to no instance", ErrPlan, i, s.Kind, s.Object)
		}
		switch s.Kind {
		case KindProbe:
			probed[p.instanceOf(s)] = true
		case KindDropIn:
			if !probed[p.instanceOf(s)] {
				return fmt.Errorf("%w: step %d writes %s with no write-probe before it; "+
					"a ProtectHome/ReadWritePaths combination that blocks the game's own "+
					"save directory would then be discovered at 3am instead of now (D-15)",
					ErrPlan, i, s.Object)
			}
		case KindRemove:
			if !a.Destructive() {
				return fmt.Errorf("%w: step %d removes %s but reports Destructive()==false, "+
					"so Apply would run it without an archive", ErrPlan, i, s.Object)
			}
		}
		if a.Destructive() && s.Kind != KindRemove {
			return fmt.Errorf("%w: step %d (%s %s) is destructive but is not a %q step; "+
				"deletion has one kind so that a plan can be read for it",
				ErrPlan, i, s.Kind, s.Object, KindRemove)
		}
	}
	return nil
}

// instanceOf resolves a step's instance, defaulting to the plan's.
func (p *Plan) instanceOf(s Step) string {
	if s.Instance != "" {
		return s.Instance
	}
	return p.Instance
}

// String renders the whole plan the way `gvm apply --diff` prints it.
func (p *Plan) String() string {
	var b strings.Builder
	for _, s := range p.Describe() {
		b.WriteString("  " + s.String() + "\n")
		if s.Diff != "" {
			for _, line := range strings.Split(strings.TrimRight(s.Diff, "\n"), "\n") {
				b.WriteString("      " + line + "\n")
			}
		}
	}
	return b.String()
}
