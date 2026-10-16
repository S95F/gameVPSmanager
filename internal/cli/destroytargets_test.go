package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// destroyFixture seeds an instance whose install and data are real directories
// on this platform, and returns everything destroyTargetsIn needs.
func destroyFixture(t *testing.T, game string, vars ...string) (*spec.Instance, *spec.Manifest, string, string, string) {
	t.Helper()
	h := newTestHost(t)
	root := t.TempDir()
	install := filepath.Join(root, "install")
	data := filepath.Join(root, "data")
	secrets := filepath.Join(root, "secrets", "x-1")
	for _, d := range []string{install, data, secrets} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	args := append([]string{"--install", install, "--data", data}, vars...)
	seedInstance(t, h, "x-1", game, args...)
	in := mustReadInstance(t, h, "x-1")
	m, _, err := h.catalog().Load(game, overrideAuth(false))
	if err != nil {
		t.Fatal(err)
	}
	return in, m, install, data, secrets
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

// TestDestroyTargetsResolvesForEveryShippedGame is the test that would have
// caught `gvm destroy` being unable to destroy anything at all.
//
// destroyTargets hands the jail its own allow roots, and every shipped manifest
// puts its rebuild survivors INSIDE install or data -- so a jail whose
// Protected set is those survivors refuses the very roots it was asked to
// resolve, in one of two ways: NewJail refuses at construction when a protected
// path IS an allow root (generic-steam's world_paths is exactly {{ .Data }}),
// and Resolve refuses afterwards because deleting a root would take a protected
// descendant with it. Both are a total failure of the command for every shipped
// game, and both are invisible to a test that only checks the archive gates.
func TestDestroyTargetsResolvesForEveryShippedGame(t *testing.T) {
	for _, tc := range []struct {
		game string
		vars []string
	}{
		{"valheim", []string{"--var", "public_name=Test"}},
		{"projectzomboid", []string{"--var", "public_name=Test"}},
		{"7daystodie", []string{"--var", "server_name=Test"}},
		{"palworld", []string{"--var", "server_name=Test"}},
		{"corekeeper", nil},
		// generic-steam is a template (`gvm create` refuses it now), so no
		// instance of it can exist to destroy; its world_paths={{ .Data }}
		// shape is exercised through the jail's own tests, not a create fixture.
	} {
		t.Run(tc.game, func(t *testing.T) {
			in, m, install, data, secrets := destroyFixture(t, tc.game, tc.vars...)

			targets, err := destroyTargets(in, m, secrets)
			if err != nil {
				t.Fatalf("destroyTargets: %v\n\n`gvm destroy` cannot remove anything at all for %s: "+
					"the jail is built with the manifest's REBUILD survivors as its protected set, "+
					"and those live inside the install and data roots the destroy is resolving",
					err, tc.game)
			}
			got := map[string]bool{}
			for _, tg := range targets {
				got[tg.Path()] = true
			}
			for _, want := range []string{install, data} {
				if !got[realPath(t, want)] {
					t.Errorf("the destroy set does not contain %s; it is %v", want, got)
				}
			}
			if len(targets) != 2 {
				t.Errorf("the destroy set is %v, want exactly the install and data roots", got)
			}
		})
	}
}

// TestDestroyTargetsRefusesARootThatResolvesIntoTheSecrets keeps the one
// protection that survives into a full destroy meaningful.
//
// The secrets directory is outside both roots, so it normally blocks nothing.
// It is in the jail for the case where data is a symlink into it, where the
// right answer is to refuse the whole destroy rather than to take D-02's
// credentials with the world.
func TestDestroyTargetsRefusesARootThatResolvesIntoTheSecrets(t *testing.T) {
	in, m, _, data, secrets := destroyFixture(t, "valheim", "--var", "public_name=Test")

	if err := os.RemoveAll(data); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secrets, data); err != nil {
		t.Skipf("this box cannot create symlinks (%v); the refusal is asserted by the Linux run", err)
	}

	_, err := destroyTargets(in, m, secrets)
	if !errors.Is(err, instance.ErrProtected) {
		t.Fatalf("destroyTargets = %v, want ErrProtected: the data root resolves into the instance's "+
			"secrets directory, and a destroy through it would take every credential with it", err)
	}
}
