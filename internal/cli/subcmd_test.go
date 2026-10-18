package cli

import (
	"strings"
	"testing"
)

// TestSubcommandDidYouMean: a near-miss subcommand suggests the real one and
// exits 2, instead of dumping full usage or guessing an instance name.
func TestSubcommandDidYouMean(t *testing.T) {
	h := newTestHost(t)
	code, _, errb := runWith(t, h, "", "db", "statuss")
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errb, `did you mean "status"`) {
		t.Errorf("no suggestion for a mistyped subcommand:\n%s", errb)
	}
}

// TestGroupHelpListsSubcommandsWithAvailability: a group's help prints its
// table, and an unbuilt subcommand is marked there rather than discovered one
// refusal at a time.
func TestGroupHelpListsSubcommandsWithAvailability(t *testing.T) {
	h := newTestHost(t)
	code, out, errb := runWith(t, h, "", "adopt", "--help")
	combined := out + errb
	if code != 0 {
		t.Fatalf("exit %d: %s", code, combined)
	}
	for _, want := range []string{"Usage: gvm adopt", "discover", "cutover", "finalize", "not in this build"} {
		if !strings.Contains(combined, want) {
			t.Errorf("adopt help missing %q:\n%s", want, combined)
		}
	}
}

// TestAdoptHelpCarriesSafetyLines: `gvm help adopt` gives each step a one-line
// safety statement and says what can be adopted (migration-adopt-14).
func TestAdoptHelpCarriesSafetyLines(t *testing.T) {
	h := newTestHost(t)
	code, out, errb := runWith(t, h, "", "help", "adopt")
	combined := out + errb
	if code != 0 {
		t.Fatalf("exit %d: %s", code, combined)
	}
	for _, want := range []string{
		"reads only; safe with players online",
		"refuses within 7 days without --force",
		"Only Project Zomboid bash-suite installs can be adopted",
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("adopt help missing %q:\n%s", want, combined)
		}
	}
}

// TestGroupSubcommandRequiredExitsTwo: a group with no subcommand is a usage
// error, and prints the table so the operator sees the choices.
func TestGroupSubcommandRequiredExitsTwo(t *testing.T) {
	h := newTestHost(t)
	code, _, errb := runWith(t, h, "", "user")
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errb, "revoke-sessions") {
		t.Errorf("the required-subcommand usage did not list the subcommands:\n%s", errb)
	}
}
