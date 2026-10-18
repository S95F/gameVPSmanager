package lifecycle

import (
	"errors"
	"slices"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
)

// Verb is a lifecycle action an operator asks for.
//
// There are three, and the omissions are the design.
//
// `enable` and `disable` are NOT here. Section 13.4 defines a durable stop as
// `gvm disable`, which sets `enabled: false` in the instance YAML and applies
// -- that is a change to INTENT, and intent has exactly one write path
// (internal/reconcile) for the same reason the privileged surface has exactly
// one (internal/hostops). A second package that could change what an instance
// IS would be the drift the design exists to prevent, and it would be able to
// do it without rendering the drop-in that the new value implies.
//
// `hold` and `unhold` are not here either: the hold is not an operation, it is
// what a stop LEAVES BEHIND so the guard timer does not undo it, and a verb
// that set it without stopping anything would let an operator disarm the guard
// for a running instance with no record of why the instance later stayed down.
type Verb string

const (
	// VerbStart brings the instance up and clears the operator hold.
	VerbStart Verb = "start"
	// VerbStop takes it down through the shutdown ladder and leaves the hold.
	VerbStop Verb = "stop"
	// VerbRestart is stop-then-start as one systemd job, so that
	// RuntimeDirectoryPreserve=restart keeps /run/gvm/<name> and the console
	// socket's path does not change under an attached operator.
	VerbRestart Verb = "restart"
)

// HoldEffect is what a verb does to /run/gvm/<name>/hold.
type HoldEffect uint8

const (
	// HoldKeep leaves it alone.
	HoldKeep HoldEffect = iota
	// HoldSet writes it, so `gvm-guard.timer` leaves the instance down.
	HoldSet
	// HoldClear removes it, so the guard resumes responsibility.
	HoldClear
)

// Decl is one verb's declaration.
//
// It exists for the same reason hostops' does: the set is enumerable, every
// row states what the verb touches, and a verb with no declaration is refused
// rather than dispatched. The difference is what it declares -- these are
// operator intents, and each one names the hostops verb it becomes, so the
// path from "gvm stop" to a privileged act is one table lookup wide.
type Decl struct {
	Verb Verb
	// Host is the privileged verb this becomes. Every lifecycle verb is
	// exactly one hostops verb plus a hold effect; a verb that needed two
	// would be a workflow and belongs in internal/jobs, where it gets a
	// job_runs row and a RunningGuard.
	Host hostops.Verb
	// Audit is the action the row is written under. It is hostops' to write,
	// and it is repeated here so that Params can be assembled against the
	// right allowlist without a second lookup at the call site.
	Audit audit.Action
	// Hold is what happens to the operator hold, and WHEN relative to the
	// host verb. See Manager.Do for the ordering and why stop writes it twice.
	Hold HoldEffect
	// Wants is the phase the verb is trying to reach. It is what
	// `--wait` waits for.
	Wants   Phase
	Summary string
}

var decls = map[Verb]Decl{
	VerbStart: {
		Verb: VerbStart, Host: hostops.VerbUnitStart, Audit: audit.InstanceStart,
		Hold: HoldClear, Wants: PhaseRunning,
		Summary: "start the instance and clear the operator hold",
	},
	VerbStop: {
		Verb: VerbStop, Host: hostops.VerbUnitStop, Audit: audit.InstanceStop,
		Hold: HoldSet, Wants: PhaseStopped,
		Summary: "run the shutdown ladder and hold the instance down",
	},
	VerbRestart: {
		Verb: VerbRestart, Host: hostops.VerbUnitRestart, Audit: audit.InstanceRestart,
		Hold: HoldClear, Wants: PhaseRunning,
		Summary: "restart the instance as one systemd job",
	},
}

// Verbs returns the declared verbs, sorted.
func Verbs() []Verb {
	out := make([]Verb, 0, len(decls))
	for v := range decls {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// Lookup returns a verb's declaration.
func Lookup(v Verb) (Decl, bool) { d, ok := decls[v]; return d, ok }

// ErrRefused is what every precondition failure wraps, so a caller can test
// for "gvm said no" without enumerating the reasons.
var ErrRefused = errors.New("lifecycle: refused")

// ErrUnknownVerb is a verb that is not declared.
var ErrUnknownVerb = errors.New("lifecycle: verb is not declared")

// Refusal is a precondition that did not hold.
//
// It carries the hint as a separate field rather than baked into the message
// because the CLI prints it on its own line and the API returns it as its own
// key: a refusal an operator cannot act on is a refusal they will work around
// with --force, and then --force becomes the way they always run the command.
type Refusal struct {
	Verb   Verb
	Phase  Phase
	Rule   Rule
	Reason string
	Hint   string
	// Force says --force may proceed anyway.
	//
	// It is false for every refusal where forcing would only move the failure
	// somewhere less useful: a manifest pin mismatch makes the runner exit 64
	// a second later, and a --force that turned a clear message into a
	// restart-prevented unit would be strictly worse for the operator.
	Force bool
}

func (r *Refusal) Error() string {
	s := "lifecycle: " + string(r.Verb) + " refused: " + r.Reason
	if r.Hint != "" {
		s += " -> " + r.Hint
	}
	return s
}

func (r *Refusal) Is(target error) bool { return target == ErrRefused }

// Allowed is the precondition table, and it is pure.
//
// It takes the observation and the verdict already derived from it, so that a
// refusal and the phase an operator was just shown cannot disagree. Every rule
// below answers one question: would doing this now make things worse, or only
// fail?
//
// Refusing a START is cheap and refusing a STOP is expensive -- an operator
// stopping a server is usually reacting to something, and gvm telling them
// "no, a job holds it" while a world burns is the wrong trade. So stop is
// refused in exactly two situations: there is no such instance, and a live job
// is mid-way through owning it (which the operator can override, because they
// may be stopping it precisely BECAUSE the job is wedged).
func Allowed(v Verb, o Observation, ver Verdict) error {
	if _, ok := decls[v]; !ok {
		return ErrUnknownVerb
	}
	no := func(reason, hint string, force bool) error {
		return &Refusal{Verb: v, Phase: ver.Phase, Rule: ver.Rule, Reason: reason, Hint: hint, Force: force}
	}

	// Nothing can be done to an instance gvm does not have a spec for, and
	// nothing can be done with one it cannot read: every path, unit name and
	// account below is derived from that file.
	switch ver.Rule {
	case RuleNoSpec:
		return no("there is no "+specPath(o.Name), "gvm list", false)
	case RuleSpecUnreadable:
		return no(specPath(o.Name)+" does not parse", "gvm apply --diff "+o.Name, false)
	}

	// A live job owns the install tree. Starting the game on top of a
	// half-written one, or stopping the server the job is about to restart,
	// both break the job's RunningGuard and leave the instance in a state
	// neither side expects. Overridable, because the operator may be stopping
	// it precisely because the job is wedged -- and then `gvm job clear` is
	// the follow-up the hint names.
	if o.LeaseState == LeaseLive && o.Lease != nil {
		return no("a "+string(o.Lease.Kind)+" job holds this instance: "+o.Lease.Describe(o.Now),
			"wait for it, or: gvm job clear "+o.Name, true)
	}

	if v == VerbStop {
		// Everything below is a reason not to bring an instance UP. None of
		// them is a reason to leave one running.
		return nil
	}

	if o.LeaseState == LeaseStale || o.LeaseUnreadable {
		return no("a job died holding this instance and the install tree may be half-written",
			"gvm job clear "+o.Name, true)
	}
	switch ver.Rule {
	case RulePinMismatch:
		// The runner compares the pin itself and exits 64 (D-17). Refusing
		// here is the same refusal, one step earlier, with a hint instead of
		// a unit stuck in "restart prevented". Never a command that moves the
		// pin: restoring a hand-edited pin is the fix, and moving to a newer
		// manifest revision is not available in this build.
		return no("the pinned manifest does not match what the instance records",
			"if the pin was edited by hand, restore it, then: sudo gvm apply "+o.Name+
				" --diff; moving to a newer manifest revision is not available in this build", false)
	case RulePinMissing:
		return no(manifestLockPath(o.Name)+" is missing", "gvm apply "+o.Name, false)
	case RuleNeverApplied:
		return no("no apply has converged this spec, so there is no drop-in to run under",
			"gvm apply "+o.Name, false)
	case RuleUnitMasked:
		return no(unitName(o.Name)+" is masked", "systemctl unmask "+unitName(o.Name), false)
	case RuleUnitNotFound:
		return no("systemd does not know "+unitName(o.Name), "gvm apply "+o.Name, false)
	}
	if o.Installed == No {
		// Overridable: the sentinel is the manifest's claim about the install,
		// and an operator who knows the claim is wrong (a renamed binary after
		// an engine update) should be able to try anyway. Not pointed back at
		// `gvm install`, which is not available in this build.
		return no("the provider's install sentinel is not on disk",
			"game files are not installed, and installing them is not available in this build", true)
	}
	return nil
}
