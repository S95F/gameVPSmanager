package content_test

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
//
// They exist because the two-namespace rule is the single hardest-won lesson in
// the bash suite, and a rule enforced by review is a rule that holds until the
// reviewer is busy. These make the compiler the reviewer.
const (
	fixtureSwap    = "namespace-swap"
	fixtureForge   = "forge-verbatim"
	fixtureControl = "namespace-ok"
)

func TestAnActivationIDCannotBeWrittenToTheAcquisitionKey(t *testing.T) {
	out, err := buildFixture(t, fixtureSwap)
	if err == nil {
		t.Fatalf("testdata/%s compiled. An activation id can now be written to the "+
			"acquisition key: the server would be told to download \"ThatDamnLibrary\", "+
			"download nothing, enable nothing, and log nothing", fixtureSwap)
	}
	// The message is asserted, not just the failure: a fixture that stopped
	// compiling for an unrelated reason (a renamed method, a moved import)
	// would otherwise report success for the wrong reason.
	for _, want := range []string{"cannot use", "ActivationID", "AcquireID"} {
		if !strings.Contains(out, want) {
			t.Errorf("the compile error does not mention %q, so this test is no longer "+
				"proving a namespace mismatch:\n%s", want, out)
		}
	}
}

func TestForgingAVerbatimActivationLineDoesNotCompile(t *testing.T) {
	out, err := buildFixture(t, fixtureForge)
	if err == nil {
		t.Fatalf("testdata/%s compiled. A caller outside this package can manufacture a "+
			"\"verbatim\" activation line, so Verbatim() is a flag rather than a statement "+
			"about where the value came from", fixtureForge)
	}
	for _, want := range []string{"unexported field", "verbatim"} {
		if !strings.Contains(out, want) {
			t.Errorf("the compile error does not mention %q; the fixture may be failing for "+
				"an unrelated reason:\n%s", want, out)
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
