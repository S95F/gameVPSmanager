package reconcile

import (
	"errors"
	"strings"
	"testing"
)

const sampleSpec = `apiVersion: gvm/v1
kind: Instance
metadata:
  name: pz-main
spec:
  game: projectzomboid
  vars:
    # the sweet spot for an 8GB box; 24 OOMed last winter
    max_players: 18
    public_name: "Friends"  # shown in the server browser
  # the ladder was hand-tuned after the save-stall incident
  shutdown:
    budget: 360s
`

// TestEditPreservesCommentsAndSetsTheValue is the whole point of editing the
// file as a document: the value changes and the reasoning beside it survives.
func TestEditPreservesCommentsAndSetsTheValue(t *testing.T) {
	out, err := EditInstanceDoc([]byte(sampleSpec), []SpecEdit{
		{Path: []string{"spec", "vars", "max_players"}, Value: "20"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "max_players: 20") {
		t.Errorf("the value was not set:\n%s", s)
	}
	for _, comment := range []string{
		"the sweet spot for an 8GB box",
		"shown in the server browser",
		"hand-tuned after the save-stall incident",
	} {
		if !strings.Contains(s, comment) {
			t.Errorf("a comment was lost (%q):\n%s", comment, s)
		}
	}
	if !strings.Contains(s, "budget: 360s") {
		t.Errorf("an untouched value changed:\n%s", s)
	}
}

// TestEditGrowsAMissingPath: `gvm create` writes no vars block when none has a
// value, so the first `set` has to grow spec.vars rather than fall over.
func TestEditGrowsAMissingPath(t *testing.T) {
	out, err := EditInstanceDoc([]byte("spec:\n  game: valheim\n"), []SpecEdit{
		{Path: []string{"spec", "vars", "difficulty"}, Value: "hard"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "vars:") || !strings.Contains(s, "difficulty: hard") {
		t.Errorf("the missing vars block was not grown:\n%s", s)
	}
}

// TestEditRefusesToClobberABlock: a path that names a mapping (spec) is not a
// value, and setting it as one would delete the whole block. Refuse instead.
func TestEditRefusesToClobberABlock(t *testing.T) {
	if _, err := EditInstanceDoc([]byte(sampleSpec), []SpecEdit{
		{Path: []string{"spec"}, Value: "x"},
	}); err == nil {
		t.Error("setting a mapping as a scalar was allowed")
	}
}

// TestEditRefusesANonMappingPath: descending THROUGH a scalar (spec.game) would
// mean reinterpreting a value as a block. Refuse.
func TestEditRefusesANonMappingPath(t *testing.T) {
	if _, err := EditInstanceDoc([]byte(sampleSpec), []SpecEdit{
		{Path: []string{"spec", "game", "nested"}, Value: "x"},
	}); err == nil {
		t.Error("descending through a scalar was allowed")
	}
}

// TestEditInfersScalarTags writes the value a human would have typed: a bare
// number as an int, true/false as a bool, everything else as a string, and a
// forced !!str for a numeric-looking value that is really a name.
func TestEditInfersScalarTags(t *testing.T) {
	out, err := EditInstanceDoc([]byte("spec:\n  game: valheim\n"), []SpecEdit{
		{Path: []string{"spec", "vars", "count"}, Value: "20"},
		{Path: []string{"spec", "vars", "flag"}, Value: "true"},
		{Path: []string{"spec", "vars", "name"}, Value: "Dedicated"},
		{Path: []string{"spec", "vars", "numeric_name"}, Value: "2024", Tag: "!!str"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"count: 20", "flag: true", "name: Dedicated"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	if !strings.Contains(s, `numeric_name: "2024"`) {
		t.Errorf("a forced-string numeric was written as an int rather than quoted:\n%s", s)
	}
}

// TestIfMatchCatchesAConcurrentEdit is the guard against a hand edit landing
// between a verb reading the file and writing it back.
func TestIfMatchCatchesAConcurrentEdit(t *testing.T) {
	raw := []byte(sampleSpec)
	d := SpecDigest(raw)
	if err := CheckIfMatch(raw, d); err != nil {
		t.Errorf("unchanged bytes failed the If-Match: %v", err)
	}
	if err := CheckIfMatch([]byte("spec: {}\n"), d); !errors.Is(err, ErrSpecChanged) {
		t.Errorf("a changed file passed the If-Match: %v", err)
	}
	if err := CheckIfMatch(raw, ""); err != nil {
		t.Errorf("an empty baseline should opt out, not fail: %v", err)
	}
}
