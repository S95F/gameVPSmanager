package cli

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/socket"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// loaderFor turns a fixed instance into the loader resolvePrepare takes, so the
// decision half is asserted with no /etc/gvm on the machine.
func loaderFor(in *spec.Instance, err error) func(string) (*spec.Instance, spec.Diags, error) {
	return func(string) (*spec.Instance, spec.Diags, error) { return in, nil, err }
}

func instanceWithUser(user string) *spec.Instance {
	in := &spec.Instance{}
	in.Spec.User = user
	return in
}

// The setgid bit is the entire mechanism of D-09: the runner then creates
// console.sock as an unprivileged user and it inherits group gvm-console with
// no further help. A plain 0750 here would leave gvm-web unable to reach any
// console, with nothing anywhere saying why.
func TestResolvePrepareIsSetgidAndGroupTraversableOnly(t *testing.T) {
	got, err := resolvePrepare("pz-main", loaderFor(instanceWithUser("pzserver"), nil))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Mode&fs.ModeSetgid == 0 {
		t.Errorf("mode %v has no setgid bit", got.Mode)
	}
	if got.Mode.Perm() != 0o750 {
		t.Errorf("perm = %04o, want 0750; anything world-readable breaks D-10", got.Mode.Perm())
	}
	if got.Group != socket.ConsoleGroup {
		t.Errorf("group = %q, want %q", got.Group, socket.ConsoleGroup)
	}
	if want := instance.PathsFor("pz-main").Run; got.Dir != want {
		t.Errorf("dir = %q, want %q", got.Dir, want)
	}
}

// An ADOPTED instance keeps the account it already had. Deriving gvm-<name>
// instead would chown the runtime directory to an account the game does not run
// as, and the runner would silently fail to write its own status file.
func TestResolvePrepareUsesTheInstancesOwnAccount(t *testing.T) {
	got, err := resolvePrepare("pz-main", loaderFor(instanceWithUser("pzserver"), nil))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.User != "pzserver" {
		t.Errorf("user = %q, want the adopted account", got.User)
	}
	if got.User == instance.UserName("pz-main") {
		t.Error("the account was derived from the name rather than read from the spec")
	}
}

// An instance file with no user is a spec bug, and guessing would produce two
// silent failures at once.
func TestResolvePrepareRefusesAnInstanceWithNoUser(t *testing.T) {
	_, err := resolvePrepare("pz-main", loaderFor(instanceWithUser(""), nil))
	if err == nil {
		t.Fatal("accepted an instance with no user")
	}
	if !strings.Contains(err.Error(), "gvm apply") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
}

func TestResolvePrepareRefusesABadName(t *testing.T) {
	_, err := resolvePrepare("../../etc", loaderFor(instanceWithUser("x"), nil))
	if !errors.Is(err, instance.ErrName) {
		t.Errorf("err = %v, want instance.ErrName", err)
	}
}

func TestResolvePrepareSurfacesALoadFailure(t *testing.T) {
	want := errors.New("permission denied")
	if _, err := resolvePrepare("pz-main", loaderFor(nil, want)); !errors.Is(err, want) {
		t.Errorf("err = %v, want the loader's own error", err)
	}
}
