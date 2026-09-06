package reconcile

import (
	"context"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// UserAction ensures the instance's own system account exists.
//
// It is issued only for a MANAGED instance. An adopted one runs as an account
// that pre-dates gvm -- pzserver, which may own other things on this box --
// and section 6.4 is explicit that gvm creates no user and chowns nothing for
// it. The account still has to exist, so the adopted case is an OBSERVATION
// reported in the plan rather than a verb: `= user pzserver (pre-existing, not
// managed by gvm)`.
type UserAction struct {
	Instance string
	Account  string
	Actor    audit.Actor
	Host     Host

	// Present is what the planner saw. It decides the sigil; hostops decides
	// the truth, and its EnsureSystemUser never modifies an account that
	// exists.
	Present bool
}

func (a *UserAction) Describe() Step {
	ch, note := Add, "system account and its own group, in no shared group at all (section 6.4)"
	if a.Present {
		ch = Unchanged
		note = "account already exists; gvm never modifies one it did not create"
	}
	return Step{
		Kind: KindUser, Change: ch, Instance: a.Instance,
		Object: a.Account, Note: note,
	}
}

func (a *UserAction) Destructive() bool { return false }

func (a *UserAction) Do(ctx context.Context, _ *archive.Ref) error {
	_, err := a.Host.Do(ctx, hostops.Request{
		Verb: hostops.VerbUserEnsure, Actor: a.Actor, Instance: a.Instance,
	})
	return err
}

// AdoptedUserNote is the plan line for an instance gvm does not own the
// account of.
//
// It is a step rather than a silence because the account is load-bearing and
// invisible: an operator reading an apply needs to see that gvm knows the
// server runs as pzserver and has decided not to touch it. A missing line
// would read as "gvm did not consider the account", which is the state that
// precedes an adoption chowning a live world.
type AdoptedUserNote struct {
	Instance string
	Account  string
	// Present is false when the account named in the spec does not exist on
	// this box, which is a REFUSAL rather than a note: gvm will not create an
	// adopted instance's account (that would be inventing the identity a live
	// world's files belong to), and a unit with User= naming a missing account
	// fails to start.
	Present bool
}

func (a *AdoptedUserNote) Describe() Step {
	if !a.Present {
		return Step{
			Kind: KindUser, Change: Warn, Instance: a.Instance, Object: a.Account,
			Note: "adopted instance names an account that does not exist on this host. gvm will " +
				"not create it: for an adopted server the account is the identity the world's " +
				"files already belong to, and inventing one produces a server that starts and " +
				"cannot read its own save. Create it, or fix spec.user.",
		}
	}
	return Step{
		Kind: KindUser, Change: Unchanged, Instance: a.Instance, Object: a.Account,
		Note: "pre-existing, not managed by gvm (managed_install: false)",
	}
}

func (a *AdoptedUserNote) Destructive() bool { return false }

// Do performs nothing, and that is the whole content of the step.
func (a *AdoptedUserNote) Do(context.Context, *archive.Ref) error {
	if !a.Present {
		return errAdoptedAccountMissing(a.Instance, a.Account)
	}
	return nil
}

func errAdoptedAccountMissing(name, account string) error {
	return &missingAccountError{Instance: name, Account: account}
}

type missingAccountError struct{ Instance, Account string }

func (e *missingAccountError) Error() string {
	return "reconcile: " + e.Instance + " is adopted and runs as " + e.Account +
		", which does not exist on this host; gvm does not create an adopted instance's account"
}

// MismatchedUserRefusal is a managed instance whose spec.user is not the
// account a managed instance runs as.
//
// The two halves of this plan read spec.user and the derived gvm-<name> from
// different places -- the account step and hostops both derive from the NAME,
// while the directory chowns and the drop-in's User= take spec.user -- so a
// spec where they disagree produces an apply that creates one account, chowns
// to a second and starts as a third, dying part-way through with half an
// instance on the host. `gvm create` refuses to write one, but nothing between
// a hand-edited YAML and here does: spec.ValidateInstance has no rule for
// spec.user. A foreign account means managed_install: false.
type MismatchedUserRefusal struct {
	Instance string
	Declared string
	Derived  string
}

func (a *MismatchedUserRefusal) Describe() Step {
	return Step{
		Kind: KindUser, Change: Warn, Instance: a.Instance, Object: a.Declared,
		Note: "spec.user is " + a.Declared + ", but a managed instance runs as " + a.Derived +
			". The account gvm would create, the account it would chown to and the account the " +
			"unit would start as are three different names. Set managed_install: false if this " +
			"server predates gvm and owns its files, or remove spec.user.",
	}
}

func (a *MismatchedUserRefusal) Destructive() bool { return false }

func (a *MismatchedUserRefusal) Do(context.Context, *archive.Ref) error {
	return fmt.Errorf("reconcile: %s declares managed_install: true and spec.user %q, but a managed "+
		"instance runs as %q; gvm will not apply a spec whose account it cannot agree with itself on",
		a.Instance, a.Declared, a.Derived)
}

// accountAction decides which of the shapes above an instance gets.
func accountAction(in *spec.Instance, actor audit.Actor, host Host, present func(string) bool) Action {
	name := in.Metadata.Name
	if !in.Spec.ManagedInstall {
		account := in.Spec.User
		return &AdoptedUserNote{Instance: name, Account: account, Present: present(account)}
	}
	account := instance.UserName(name)
	if in.Spec.User != "" && in.Spec.User != account {
		return &MismatchedUserRefusal{Instance: name, Declared: in.Spec.User, Derived: account}
	}
	return &UserAction{
		Instance: name, Account: account, Actor: actor, Host: host,
		Present: present(account),
	}
}
