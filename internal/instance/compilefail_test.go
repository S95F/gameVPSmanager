package instance_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixtures under testdata/ are documentation that executes. Two of them
// must fail to compile and one must succeed; the go tool never sees any of
// them through ./... because testdata is excluded from wildcard matching, so
// these tests name them explicitly.
const (
	fixtureStringPath = "destroy-string"
	fixtureForged     = "forge-jailed"
	fixtureControl    = "destroy-jailed"
)

func TestDestroyDoesNotCompileWithAStringPath(t *testing.T) {
	out, err := buildFixture(t, fixtureStringPath)
	if err == nil {
		t.Fatalf("testdata/%s compiled. instance.Destroy now accepts a raw path, "+
			"and the destroy jail is a naming convention rather than a type", fixtureStringPath)
	}
	// The message is asserted, not just the failure: a fixture that stopped
	// compiling for an unrelated reason (a renamed import, a missing symbol)
	// would otherwise report success for the wrong reason.
	wantAll := []string{"cannot use", "[]string", "Jailed"}
	for _, want := range wantAll {
		if !strings.Contains(out, want) {
			t.Errorf("compile error does not mention %q, so this test is no longer proving a "+
				"type mismatch:\n%s", want, out)
		}
	}
}

func TestForgingAJailedDoesNotCompile(t *testing.T) {
	out, err := buildFixture(t, fixtureForged)
	if err == nil {
		t.Fatalf("testdata/%s compiled. Jailed's fields can be set from another package, "+
			"so a caller can assert a path is safe instead of proving it", fixtureForged)
	}
	// gc says "cannot refer to unexported field root in struct literal of type
	// instance.Jailed". Asserting on the field name as well as the phrase
	// keeps this honest if only one of them changes.
	for _, want := range []string{"unexported field", "root", "instance.Jailed"} {
		if !strings.Contains(out, want) {
			t.Errorf("compile error does not mention %q; the fixture may be failing for an "+
				"unrelated reason:\n%s", want, out)
		}
	}
}

// TestTheCompileFailFixturesHaveAWorkingControl is what stops the two tests
// above from passing because of a typo. If the control stops building, every
// "must not compile" assertion above is worthless and this says so first.
func TestTheCompileFailFixturesHaveAWorkingControl(t *testing.T) {
	out, err := buildFixture(t, fixtureControl)
	if err != nil {
		t.Fatalf("testdata/%s must compile, or the compile-fail fixtures prove nothing:\n%s\n%v",
			fixtureControl, out, err)
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
