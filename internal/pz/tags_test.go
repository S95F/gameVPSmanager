package pz

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// item builds the file-scan half of a verdict: the result resolve.go would
// have produced for one workshop item, before any tag was considered.
func item(id string, score int, status ItemStatus, modIDs ...string) ItemResult {
	return ItemResult{
		WorkshopID: id,
		Status:     status,
		Score:      score,
		ModIDs:     modIDs,
		Markers:    []string{"marker"},
	}
}

// TestTheZeroTagSetIsAnUnansweredAPI is the safety property of the type.
//
// A composite literal written in a hurry, a map lookup that missed, a fetch
// that failed: all of them produce this value, and all of them have to mean
// "no help from Steam" rather than "Steam says this item has no tags". The
// second reading is a strictly more confident claim about an item nobody
// looked up.
func TestTheZeroTagSetIsAnUnansweredAPI(t *testing.T) {
	var zero TagSet
	if zero.Known() {
		t.Error("the zero TagSet claims the API answered")
	}
	if zero.Has(BuildB42) || zero.Has(BuildB41) {
		t.Error("the zero TagSet carries a build opinion")
	}
	if got := zero.Evidence(); !slices.Equal(got, []string{EvidenceUnavailable}) {
		t.Errorf("evidence = %v, want [%s]", got, EvidenceUnavailable)
	}
	if !reflect.DeepEqual(zero, TagsUnavailable()) {
		t.Error("TagsUnavailable() is not the zero value, so the dangerous default is the " +
			"one a forgotten field produces")
	}

	// An API that answered with no tags is a DIFFERENT value: same resolution
	// today, different conversation with the operator.
	answered := TagsFrom(nil)
	if !answered.Known() {
		t.Error("TagsFrom(nil) reads as an unanswered API")
	}
	if got := answered.Evidence(); !slices.Equal(got, []string{EvidenceNoBuildTag}) {
		t.Errorf("evidence = %v, want [%s]", got, EvidenceNoBuildTag)
	}
}

// TestBuildTagsMatchExactly: the tag field is free text an author typed into a
// web form. A substring rule would read "Build 41 compatible, Build 42 soon"
// as a declaration of both.
func TestBuildTagsMatchExactly(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		want Build
		ok   bool
	}{
		{"Build 41", BuildB41, true},
		{"Build 42", BuildB42, true},
		{"build 42", BuildB42, true},     // case folded: Steam's own casing has moved before
		{"  Build 42  ", BuildB42, true}, // trimmed
		{"Build 42 Compatible", 0, false},
		{"Pre-Build 42", 0, false},
		{"Build 4", 0, false},
		{"Build42", 0, false},
		{"Vehicles", 0, false},
		{"", 0, false},
	} {
		tags := TagsFrom([]string{tc.tag})
		if got := tags.Has(BuildB41) || tags.Has(BuildB42); got != tc.ok {
			t.Errorf("TagsFrom([%q]) carries a build opinion = %t, want %t", tc.tag, got, tc.ok)
			continue
		}
		if tc.ok && !tags.Has(tc.want) {
			t.Errorf("TagsFrom([%q]) does not have Build %s", tc.tag, tc.want)
		}
	}
}

// TestNonBuildTagsStayOutOfTheEvidence: a lockfile entry carrying
// "tags:Vehicles" is noise in a file an operator is meant to read, but the
// tag is still worth having for a report.
func TestNonBuildTagsStayOutOfTheEvidence(t *testing.T) {
	tags := TagsFrom([]string{"Vehicles", "Build 42", "Build 42", "Balance"})

	if got := tags.Evidence(); !slices.Equal(got, []string{"tags:Build 42"}) {
		t.Errorf("evidence = %v, want only the build tag", got)
	}
	if got := tags.Names(); !slices.Equal(got, []string{"Vehicles", "Build 42", "Build 42",
		"Balance"}) {
		t.Errorf("names = %v, want every tag in page order", got)
	}
	if got := tags.Builds(); !slices.Equal(got, []Build{BuildB42}) {
		t.Errorf("builds = %v, want one entry: a repeated tag is one opinion", got)
	}
}

// TestNamesAndBuildsAreCopies: a TagSet is handed around by value and read
// from a report and a resolver in the same call graph. An accessor that
// returned the backing array would let one of them reorder the other's view.
func TestNamesAndBuildsAreCopies(t *testing.T) {
	tags := TagsFrom([]string{"Build 41", "Build 42"})
	names := tags.Names()
	names[0] = "clobbered"
	builds := tags.Builds()
	builds[0] = Build(99)

	if got := tags.Names(); got[0] != "Build 41" {
		t.Errorf("Names() handed out its backing array: %v", got)
	}
	if got := tags.Builds(); got[0] != BuildB41 {
		t.Errorf("Builds() handed out its backing array: %v", got)
	}
}

// TestSection125Table walks the evidence table in section 12.5 row by row.
//
// It is the whole contract of this file, so it is one table and not six tests:
// a row that disappears in a refactor is visible here in a way it would not be
// scattered across functions.
func TestSection125Table(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       ItemResult
		tags     TagSet
		target   Build
		status   ItemStatus
		conflict bool
		promoted bool
		warns    bool
		evidence []string
	}{
		{
			name:     "score 3, page agrees",
			in:       item("1", 3, StatusOK, "A"),
			tags:     TagsFrom([]string{"Build 42"}),
			target:   BuildB42,
			status:   StatusOK,
			evidence: []string{"tags:Build 42"},
		},
		{
			name:     "score 3, page tagged Build 41 only: conflict recorded, not acted on",
			in:       item("2", 3, StatusOK, "A"),
			tags:     TagsFrom([]string{"Build 41"}),
			target:   BuildB42,
			status:   StatusOK,
			conflict: true,
			warns:    true,
			evidence: []string{"tags:Build 41", EvidenceConflict},
		},
		{
			name:     "score 1, page agrees",
			in:       item("3", 1, StatusOK, "A"),
			tags:     TagsFrom([]string{"Build 42"}),
			target:   BuildB42,
			status:   StatusOK,
			evidence: []string{"tags:Build 42"},
		},
		{
			name:     "score 0, Build 42 only: promoted",
			in:       item("4", 0, StatusCheck, "A"),
			tags:     TagsFrom([]string{"Build 42"}),
			target:   BuildB42,
			status:   StatusOK,
			promoted: true,
			evidence: []string{"tags:Build 42"},
		},
		{
			name:     "score 0, Build 42 only, several mods: promoted to multi",
			in:       item("5", 0, StatusCheck, "A", "B"),
			tags:     TagsFrom([]string{"Build 42"}),
			target:   BuildB42,
			status:   StatusMulti,
			promoted: true,
			evidence: []string{"tags:Build 42"},
		},
		{
			name:     "score 0, both builds: check, all ids enabled, warned (2866258937)",
			in:       item("2866258937", 0, StatusCheck, "MiniHealthPanel", "MiniHealthPanelB41"),
			tags:     TagsFrom([]string{"Build 41", "Build 42"}),
			target:   BuildB42,
			status:   StatusCheck,
			warns:    true,
			evidence: []string{"tags:Build 41", "tags:Build 42"},
		},
		{
			name:     "score 0, Build 41 only on a Build 42 server: check plus a hard warning",
			in:       item("6", 0, StatusCheck, "A"),
			tags:     TagsFrom([]string{"Build 41"}),
			target:   BuildB42,
			status:   StatusCheck,
			warns:    true,
			evidence: []string{"tags:Build 41"},
		},
		{
			name:     "score 0, API unreachable: exactly today's behaviour",
			in:       item("7", 0, StatusCheck, "A"),
			tags:     TagsUnavailable(),
			target:   BuildB42,
			status:   StatusCheck,
			evidence: []string{EvidenceUnavailable},
		},
		{
			name:     "score 0, API answered with no build tag",
			in:       item("8", 0, StatusCheck, "A"),
			tags:     TagsFrom([]string{"Vehicles"}),
			target:   BuildB42,
			status:   StatusCheck,
			evidence: []string{EvidenceNoBuildTag},
		},
		// The same table from a Build 41 server's point of view, because the
		// scoring is parameterised and a rule that only held for B42 would be
		// a rule that silently inverted on the other target.
		{
			name:     "B41 target, score 0, Build 41 only: promoted",
			in:       item("9", 0, StatusCheck, "A"),
			tags:     TagsFrom([]string{"Build 41"}),
			target:   BuildB41,
			status:   StatusOK,
			promoted: true,
			evidence: []string{"tags:Build 41"},
		},
		{
			name:     "B41 target, score 3, Build 42 only: conflict",
			in:       item("10", 3, StatusOK, "A"),
			tags:     TagsFrom([]string{"Build 42"}),
			target:   BuildB41,
			status:   StatusOK,
			conflict: true,
			warns:    true,
			evidence: []string{"tags:Build 42", EvidenceConflict},
		},
		// Section 12.5's row 1 is "3 | has Build 42 | ok" and its conflict row
		// is "3 | Build 41 ONLY". The distinguishing word is "has" versus
		// "only", and no single-tag row can tell the two apart: with one tag on
		// the page, hasOther alone decides, so the !hasTarget term carries no
		// weight and dropping it changes nothing. These rows are the dual-tag
		// shape that gives it weight.
		//
		// The shape is the common one, not an edge case: a maintained
		// dual-build mod's page carries both tags while its files declare
		// versionMin 42. Recording `tags:conflict` and "the Workshop page is
		// tagged Build 41 only" against those is precisely the false evidence
		// 12.5's table exists to prevent.
		{
			name:     "score 3, page tagged for BOTH builds: agreement, not conflict",
			in:       item("11", 3, StatusOK, "A"),
			tags:     TagsFrom([]string{"Build 41", "Build 42"}),
			target:   BuildB42,
			status:   StatusOK,
			evidence: []string{"tags:Build 41", "tags:Build 42"},
		},
		{
			name:     "score 1, page tagged for both builds: agreement, not conflict",
			in:       item("12", 1, StatusOK, "A"),
			tags:     TagsFrom([]string{"Build 41", "Build 42"}),
			target:   BuildB42,
			status:   StatusOK,
			evidence: []string{"tags:Build 41", "tags:Build 42"},
		},
		// And the mirror, so the rule cannot silently invert on the other
		// target.
		{
			name:     "B41 target, score 3, page tagged for both builds: no conflict",
			in:       item("13", 3, StatusOK, "A"),
			tags:     TagsFrom([]string{"Build 41", "Build 42"}),
			target:   BuildB41,
			status:   StatusOK,
			evidence: []string{"tags:Build 41", "tags:Build 42"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := reflect.ValueOf(tc.in).Interface()
			v := LayerTags(tc.in, tc.tags, tc.target)

			if v.Status != tc.status {
				t.Errorf("status = %q, want %q", v.Status, tc.status)
			}
			if v.Conflict != tc.conflict {
				t.Errorf("conflict = %t, want %t", v.Conflict, tc.conflict)
			}
			if v.Promoted != tc.promoted {
				t.Errorf("promoted = %t, want %t", v.Promoted, tc.promoted)
			}
			if got := v.Warning != ""; got != tc.warns {
				t.Errorf("warning = %q, want a warning: %t", v.Warning, tc.warns)
			}
			if !slices.Equal(v.Evidence, tc.evidence) {
				t.Errorf("evidence = %v, want %v", v.Evidence, tc.evidence)
			}
			if v.Ambiguous() != (tc.status == StatusCheck) {
				t.Errorf("Ambiguous() = %t for status %q", v.Ambiguous(), v.Status)
			}
			// The item went in by value and must come out untouched: a
			// function that could edit ModIDs is a function that could drop
			// one.
			if !reflect.DeepEqual(before, tc.in) {
				t.Errorf("LayerTags modified the item it was given: %+v", tc.in)
			}
		})
	}
}

// TestTagsNeverDropAnItem is the rule, asserted against every tag combination
// rather than against the rows that happened to be written above.
//
// A verdict has no id list in it at all -- that is checked here too -- so the
// only thing a caller could act on is Status, and Status is only ever the file
// status or a promotion of it.
func TestTagsNeverDropAnItem(t *testing.T) {
	combos := []TagSet{
		TagsUnavailable(),
		TagsFrom(nil),
		TagsFrom([]string{"Build 41"}),
		TagsFrom([]string{"Build 42"}),
		TagsFrom([]string{"Build 41", "Build 42"}),
		TagsFrom([]string{"Vehicles"}),
	}
	items := []ItemResult{
		item("a", 3, StatusOK, "One"),
		item("b", 1, StatusOK, "One"),
		item("c", 0, StatusCheck, "One", "Two"),
		item("d", 0, StatusMulti, "One", "Two"),
		item("e", 0, StatusMissing),
		item("f", 0, StatusEmpty),
	}
	for _, in := range items {
		for _, tags := range combos {
			for _, target := range []Build{BuildB41, BuildB42, Build(99)} {
				v := LayerTags(in, tags, target)

				// Conflict is "the files scored for the target AND the page is
				// tagged for the other build ONLY". Pinned for all 108
				// combinations rather than only for the table rows above,
				// because dropping the !Has(target) term costs nothing in the
				// table and floods the evidence of every dual-build-tagged mod
				// with a conflict that is not one.
				//
				// The Score > 0 precondition is load-bearing: a score-0 item
				// never reaches the conflict branch at all, so an expression
				// without it predicts a conflict for item "d".
				resolvable := in.Status != StatusMissing && in.Status != StatusEmpty
				other := otherBuild(target)
				wantConflict := resolvable && in.Score > 0 && tags.Known() &&
					!tags.Has(target) && other != 0 && tags.Has(other)
				if v.Conflict != wantConflict {
					t.Errorf("%s: conflict = %t, want %t for score %d, tags %v, target %s",
						in.WorkshopID, v.Conflict, wantConflict, in.Score, tags.Names(), target)
				}
				// The score-0 switch's other half, equally unasserted across
				// the sweep: promotion is "no marker in the files, and the page
				// says this build and not the other".
				wantPromoted := resolvable && in.Score == 0 && tags.Known() &&
					tags.Has(target) && !(other != 0 && tags.Has(other))
				if v.Promoted != wantPromoted {
					t.Errorf("%s: promoted = %t, want %t for score %d, tags %v, target %s",
						in.WorkshopID, v.Promoted, wantPromoted, in.Score, tags.Names(), target)
				}

				// Never demoted: an item that resolved stays resolved, and an
				// item that is missing stays missing.
				switch in.Status {
				case StatusMissing, StatusEmpty:
					if v.Status != in.Status {
						t.Errorf("%s: a %q item became %q on tags %v; tags cannot conjure a "+
							"download", in.WorkshopID, in.Status, v.Status, tags.Names())
					}
					if v.Promoted {
						t.Errorf("%s: a %q item was promoted", in.WorkshopID, in.Status)
					}
				case StatusOK, StatusMulti:
					if v.Status == StatusCheck {
						t.Errorf("%s: a resolved item was demoted to check on tags %v",
							in.WorkshopID, tags.Names())
					}
				}
				if len(v.Evidence) == 0 {
					t.Errorf("%s: no evidence recorded for tags %v", in.WorkshopID,
						tags.Names())
				}
			}
		}
	}

	// Structural: there is no field on a verdict through which an id could
	// travel, so "tags never drop an item" cannot be undone by a caller
	// reading the wrong field.
	rt := reflect.TypeOf(TagVerdict{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.Name == "Evidence" {
			continue
		}
		if f.Type.Kind() == reflect.Slice || f.Type.Kind() == reflect.Map {
			t.Errorf("TagVerdict.%s is a %s; the only collection on a verdict is Evidence, "+
				"because a verdict that could carry ids could replace them", f.Name, f.Type)
		}
	}
}

// TestAnUnknownTargetNeverConflicts: resolve.go already scores everything zero
// for an unknown target, which makes every item loudly ambiguous. Telling the
// operator it also conflicts with Build 41 would be noise on top of a
// misconfiguration.
func TestAnUnknownTargetNeverConflicts(t *testing.T) {
	v := LayerTags(item("1", 0, StatusCheck, "A"), TagsFrom([]string{"Build 41", "Build 42"}),
		Build(99))

	if v.Conflict || v.Promoted {
		t.Errorf("conflict = %t, promoted = %t for an unknown target", v.Conflict, v.Promoted)
	}
	if v.Status != StatusCheck {
		t.Errorf("status = %q, want check", v.Status)
	}
}

// TestTheConflictWarningNamesBothSides: "gvm reports it and lets a human
// decide" is only true if the report says what each side claimed.
func TestTheConflictWarningNamesBothSides(t *testing.T) {
	in := ItemResult{
		WorkshopID: "3171167894",
		Status:     StatusOK,
		Score:      3,
		ModIDs:     []string{"ThatDamnLibrary"},
		Markers:    []string{"versionMin>=42"},
	}
	v := LayerTags(in, TagsFrom([]string{"Build 41"}), BuildB42)

	for _, want := range []string{"3171167894", "versionMin>=42", "Build 41", "Build 42"} {
		if !strings.Contains(v.Warning, want) {
			t.Errorf("the conflict warning does not mention %q: %s", want, v.Warning)
		}
	}
	if !slices.Contains(v.Evidence, EvidenceConflict) {
		t.Errorf("evidence = %v, want %s recorded", v.Evidence, EvidenceConflict)
	}
}

// TestTheB41OnlyWarningIsTheHardOne: a Build 41-only item on a Build 42 server
// is the row section 12.5 calls a hard doctor warning, and the message has to
// say that gvm is not going to remove it.
func TestTheB41OnlyWarningIsTheHardOne(t *testing.T) {
	v := LayerTags(item("2822286426", 0, StatusCheck, "OldMod"),
		TagsFrom([]string{"Build 41"}), BuildB42)

	if v.Warning == "" {
		t.Fatal("no warning for a Build 41-only item on a Build 42 server")
	}
	for _, want := range []string{"2822286426", "ONLY", "OldMod"} {
		if !strings.Contains(v.Warning, want) {
			t.Errorf("the warning does not mention %q: %s", want, v.Warning)
		}
	}
}
