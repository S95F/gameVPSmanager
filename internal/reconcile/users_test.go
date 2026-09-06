package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/instance"
)

// TestAManagedInstanceWithAForeignUserIsRefused.
//
// `gvm create` no longer writes such a spec, but a hand-edited YAML can and
// spec.ValidateInstance has no rule about spec.user. The plan reads the account
// from two places -- accountAction and hostops derive gvm-<name>, while the
// directory chowns and the drop-in's User= take spec.user -- so an apply over a
// disagreeing spec creates one account, chowns to a second and starts as a
// third, and fails somewhere in the middle with the instance half on the host.
func TestAManagedInstanceWithAForeignUserIsRefused(t *testing.T) {
	in, _ := loadInstance(t, "valheim-1")
	if !in.Spec.ManagedInstall {
		t.Fatal("the fixture is adopted, so this test would assert nothing")
	}
	in.Spec.User = "pzserver"

	a := accountAction(in, rootActor(), &fakeHost{}, func(string) bool { return true })
	step := a.Describe()
	if step.Change != Warn {
		t.Errorf("the plan reports %q for a managed instance running as somebody else's account; "+
			"an operator reading --diff has to see this before the apply", step.Change)
	}
	if !strings.Contains(step.Note, instance.UserName("valheim-1")) {
		t.Errorf("the note does not name the account gvm would actually use: %q", step.Note)
	}
	if err := a.Do(context.Background(), nil); err == nil {
		t.Fatal("the apply proceeded over a spec whose account gvm cannot agree with itself on")
	}

	// The control: the same instance with its own derived account still plans a
	// plain user step, so the refusal is about the mismatch and nothing else.
	in.Spec.User = instance.UserName("valheim-1")
	if _, ok := accountAction(in, rootActor(), &fakeHost{}, func(string) bool { return true }).(*UserAction); !ok {
		t.Error("a managed instance with its own account no longer gets a UserAction")
	}
}
