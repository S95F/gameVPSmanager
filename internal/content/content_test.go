package content

import (
	"errors"
	"strings"
	"testing"
)

func TestParseImpactRefusesRatherThanDefaulting(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Impact
	}{
		{"none", ImpactNone},
		{"restart", ImpactRestart},
		{"download_cycle", ImpactDownloadCycle},
		{"rebuild", ImpactRebuild},
		{"  rebuild  ", ImpactRebuild},
	} {
		got, err := ParseImpact(tc.in)
		if err != nil {
			t.Errorf("ParseImpact(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseImpact(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}

	// The case that matters. ImpactNone is the zero value, so a typo that
	// decoded to it would turn a world-affecting removal into a no-op that
	// takes no archive and asks for no confirmation.
	for _, bad := range []string{"", "rebuid", "REBUILD", "download-cycle", "restart ;"} {
		got, err := ParseImpact(bad)
		if err == nil {
			t.Errorf("ParseImpact(%q) = %s with no error; a typo must not decode to an impact",
				bad, got)
		}
		if !errors.Is(err, ErrInvalidSchema) {
			t.Errorf("ParseImpact(%q) error does not wrap ErrInvalidSchema: %v", bad, err)
		}
	}
}

// TestImpactOrderIsTheSeverityOrder pins the thing Max depends on. Reshuffling
// the constants would silently downgrade a rebuild to a restart.
func TestImpactOrderIsTheSeverityOrder(t *testing.T) {
	if !(ImpactNone < ImpactRestart && ImpactRestart < ImpactDownloadCycle &&
		ImpactDownloadCycle < ImpactRebuild) {
		t.Fatal("the Impact constants are no longer in severity order, so Max can downgrade")
	}
	if got := ImpactRestart.Max(ImpactRebuild); got != ImpactRebuild {
		t.Errorf("restart.Max(rebuild) = %s", got)
	}
	if got := ImpactRebuild.Max(ImpactRestart); got != ImpactRebuild {
		t.Errorf("rebuild.Max(restart) = %s", got)
	}
	for _, i := range Impacts() {
		if !i.Valid() {
			t.Errorf("%s is not Valid()", i)
		}
		round, err := ParseImpact(i.String())
		if err != nil || round != i {
			t.Errorf("%s does not round trip through its own string: %v, %v", i, round, err)
		}
	}
}

func TestChangeKindsAndResolutionPathsAreClosed(t *testing.T) {
	for _, k := range ChangeKinds() {
		if !k.Valid() {
			t.Errorf("%q is listed but not Valid()", k)
		}
	}
	if ChangeKind("remvoe").Valid() {
		t.Error("a misspelled change kind validates, so an impact map typo would go unnoticed")
	}
	for _, p := range ResolutionPaths() {
		if !p.Valid() {
			t.Errorf("%q is listed but not Valid()", p)
		}
	}
	if ResolutionPath("edge.forgecdn.net").Valid() {
		t.Error("an unknown resolution path validates")
	}
}

// TestDedupeActivateKeepsFirstOccurrence is load order, not tidiness: moving a
// repeated library id to its last mention puts it below a mod that needs it.
func TestDedupeActivateKeepsFirstOccurrence(t *testing.T) {
	got := dedupeActivate([]ActivationID{
		"ThatDamnLibrary", "NeatUIFramework", "Hilux88", "ThatDamnLibrary", "", "Hilux88",
	})
	want := []ActivationID{"ThatDamnLibrary", "NeatUIFramework", "Hilux88"}
	if len(got) != len(want) {
		t.Fatalf("dedupe gave %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupe gave %v, want %v", got, want)
		}
	}
}

// TestJoinUsesTheDeclaredSeparator covers the footgun the separator is data
// for: a comma breaks a PZ list with no error at all.
func TestJoinUsesTheDeclaredSeparator(t *testing.T) {
	if got := joinAcquire([]AcquireID{"1", "2", "3"}, ";"); got != "1;2;3" {
		t.Errorf("joinAcquire = %q", got)
	}
	if got := joinActivate([]ActivationID{"A", "B"}, ";"); got != "A;B" {
		t.Errorf("joinActivate = %q", got)
	}
}

func TestRefHelpersTrimAndNarrow(t *testing.T) {
	r := Ref{ID: "  3171167894 ", Requires: []string{"", " 3508537032 "}}
	if got := RefID(r); got != AcquireID("3171167894") {
		t.Errorf("RefID = %q", got)
	}
	req := RefRequires(r)
	if len(req) != 1 || req[0] != AcquireID("3508537032") {
		t.Errorf("RefRequires = %v; empty edges must be dropped and the rest trimmed", req)
	}
}

func TestUnaccountedStringNamesTheItemAndTheReason(t *testing.T) {
	u := Unaccounted{ID: "123", Reason: "the API returned result 9", Hint: "the collection is private"}
	s := u.String()
	for _, want := range []string{"123", "result 9", "private"} {
		if !strings.Contains(s, want) {
			t.Errorf("Unaccounted.String() = %q, missing %q; a refusal nobody can act on is "+
				"one operators route around", s, want)
		}
	}
}
