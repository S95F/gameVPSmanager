package lifecycle_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// "The reconciler never actuates" is a TYPE in this package, and these two
// fixtures are what keeps it one.
//
// The go tool never sees them through ./... -- testdata is excluded from
// wildcard matching -- so these tests name them explicitly.
//
// TestReconcilerNeverActuates, in the package's own test file, proves that no
// start HAPPENED during one sweep over a host with four kinds of drift on it.
// That is a statement about one run. This is the statement an edit to the
// sweep cannot falsify: the reconciler's view of systemd is a UnitReader,
// which has State and ListUnits and no method that changes anything, so there
// is no Start to call from anywhere at all.
const (
	fixtureActuates = "reconciler-actuates"
	fixtureObserves = "reconciler-observes"
)

func TestAReconcilerThatStartsAUnitDoesNotCompile(t *testing.T) {
	out, err := buildFixture(t, fixtureActuates)
	if err == nil {
		t.Fatalf("testdata/%s compiled. The reconciler can now start a unit, and "+
			"\"it never actuates\" is a convention rather than a type", fixtureActuates)
	}
	// The message is asserted, not just the failure: a fixture that stopped
	// compiling for an unrelated reason -- a renamed import, a missing symbol
	// -- would otherwise report success for the wrong reason.
	for _, want := range []string{"Start", "UnitReader"} {
		if !strings.Contains(out, want) {
			t.Errorf("the compile error does not mention %q, so this test is no longer proving "+
				"that the seam has no Start on it:\n%s", want, out)
		}
	}
}

// TestTheCompileFailFixtureHasAWorkingControl is what stops the test above
// from passing because of a typo. If the control stops building, the "must not
// compile" assertion is worthless and this says so first.
func TestTheCompileFailFixtureHasAWorkingControl(t *testing.T) {
	out, err := buildFixture(t, fixtureObserves)
	if err != nil {
		t.Fatalf("testdata/%s must compile, or the compile-fail fixture proves nothing:\n%s\n%v",
			fixtureObserves, out, err)
	}
}

// buildFixture compiles one testdata program and returns the compiler's output.
func buildFixture(t *testing.T, name string) (string, error) {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH to compile the fixtures with")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A path under t.TempDir() rather than os.DevNull: a build that succeeds
	// has to be able to write its binary, or the success case fails for a
	// reason that has nothing to do with the code under test.
	bin := filepath.Join(t.TempDir(), "fixture.out")
	cmd := exec.CommandContext(ctx, goBin, "build", "-o", bin, "./testdata/"+name)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
