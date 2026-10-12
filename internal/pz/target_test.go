package pz

import (
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"
)

// dualBuildItem is the layout that makes the scoring necessary: one workshop
// item, a Build 41 variant and a Build 42 variant side by side, each with its
// own mod.info and its own internal id. Enabling the wrong one downloads the
// item and never loads the mod.
func dualBuildItem() fs.FS {
	return fstest.MapFS{
		"3435796523/mods/Hilux/41/mod.info": &fstest.MapFile{Data: []byte(
			"name=Hilux (B41)\nid=Hilux88B41\n")},
		"3435796523/mods/Hilux/42/mod.info": &fstest.MapFile{Data: []byte(
			"name=Hilux (B42)\nid=Hilux88\nversionMin=42\n")},
	}
}

func TestParseBuildRefusesRatherThanDefaulting(t *testing.T) {
	for in, want := range map[string]Build{"41": BuildB41, "42": BuildB42, " 42 ": BuildB42} {
		got, err := ParseBuild(in)
		if err != nil || got != want {
			t.Errorf("ParseBuild(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	// A typo'd target that fell through to B42 on a B41 server would enable
	// the B42 variant of every dual-build item -- the same silent omission as
	// the other way round.
	for _, bad := range []string{"", "B42", "4.2", "40", "43"} {
		if got, err := ParseBuild(bad); err == nil {
			t.Errorf("ParseBuild(%q) = %v with no error", bad, got)
		}
	}
	if got := BuildB42.String(); got != "42" {
		t.Errorf("BuildB42.String() = %q", got)
	}
}

// TestResolveForPicksTheTargetsVariant is what makes a Build 41 instance
// expressible. Variant selection is still purely file-based; the target only
// decides which build's markers count.
func TestResolveForPicksTheTargetsVariant(t *testing.T) {
	root := WorkshopRoot{Name: "/workshop", FS: dualBuildItem()}

	for _, tc := range []struct {
		target        Build
		want, dropped string
		marker        string
	}{
		{BuildB42, "Hilux88", "Hilux88B41", "42-dir"},
		{BuildB41, "Hilux88B41", "Hilux88", "41-dir"},
	} {
		t.Run(tc.target.String(), func(t *testing.T) {
			res := ResolveFor([]WorkshopRoot{root}, []string{"3435796523"}, tc.target)
			if res.Target != tc.target {
				t.Errorf("recorded target = %v", res.Target)
			}
			if !slices.Equal(res.ModIDs, []string{tc.want}) {
				t.Fatalf("Mods= would be %v, want [%s]", res.ModIDs, tc.want)
			}
			it := res.Items[0]
			if !slices.Equal(it.Dropped, []string{tc.dropped}) {
				t.Errorf("dropped = %v, want [%s]", it.Dropped, tc.dropped)
			}
			if it.Status != StatusOK {
				t.Errorf("status = %q, want ok: the item declares its builds explicitly",
					it.Status)
			}
			if it.Score != 3 {
				t.Errorf("score = %d, want 3 (explicit)", it.Score)
			}
			if !slices.Contains(it.Markers, tc.marker) {
				t.Errorf("markers = %v, want %s", it.Markers, tc.marker)
			}
		})
	}
}

// TestResolveTargetsB42 pins the existing callers' meaning: Resolve's
// behaviour is unchanged by the parameterisation.
func TestResolveTargetsB42(t *testing.T) {
	root := WorkshopRoot{Name: "/workshop", FS: dualBuildItem()}
	plain := Resolve([]WorkshopRoot{root}, []string{"3435796523"})
	explicit := ResolveFor([]WorkshopRoot{root}, []string{"3435796523"}, BuildB42)

	if !slices.Equal(plain.ModIDs, explicit.ModIDs) {
		t.Fatalf("Resolve gave %v and ResolveFor(42) gave %v", plain.ModIDs, explicit.ModIDs)
	}
	if plain.Target != BuildB42 {
		t.Errorf("Resolve targets %v", plain.Target)
	}
}

func TestBuildMarkersAreReportedPerItem(t *testing.T) {
	root := WorkshopRoot{Name: "/workshop", FS: fstest.MapFS{
		// Explicit by declaration rather than by directory.
		"1/mods/A/mod.info": &fstest.MapFile{Data: []byte("id=A\nversionMin=42.0.1\n")},
		// The weaker layout hint: a common/ media split.
		"2/mods/B/mod.info": &fstest.MapFile{Data: []byte("id=B\n")},
		"2/mods/B/common/x": &fstest.MapFile{Data: []byte("x")},
		// No evidence either way.
		"3/mods/C/mod.info": &fstest.MapFile{Data: []byte("id=C\n")},
	}}
	res := ResolveFor([]WorkshopRoot{root}, []string{"1", "2", "3"}, BuildB42)

	want := []struct {
		score  int
		marker string
		status ItemStatus
	}{
		{3, "versionMin>=42", StatusOK},
		{1, "common-layout", StatusOK},
		{0, "no-build-marker", StatusCheck},
	}
	for i, w := range want {
		it := res.Items[i]
		if it.Score != w.score {
			t.Errorf("item %s score = %d, want %d", it.WorkshopID, it.Score, w.score)
		}
		if !slices.Contains(it.Markers, w.marker) {
			t.Errorf("item %s markers = %v, want %s", it.WorkshopID, it.Markers, w.marker)
		}
		if it.Status != w.status {
			t.Errorf("item %s status = %q, want %q", it.WorkshopID, it.Status, w.status)
		}
	}
}

// TestB41TargetOnAnUnmarkedRootVariantDegradesLoudly documents a known limit
// of a 3/1/0 scale that has no negative rung.
//
// The layout is the common one: the B41 variant at the item root with no
// marker, the B42 one under 42/. On a B41 target neither scores, so both ids
// are enabled and the item is flagged `check` -- the same visible warning
// sync-mods.sh printed. That is the right degradation: it is loud, and the
// alternative (demoting the variant that declares versionMin 42+) is scoring
// an item by what it is NOT, which no rung of the published scale expresses.
func TestB41TargetOnAnUnmarkedRootVariantDegradesLoudly(t *testing.T) {
	root := WorkshopRoot{Name: "/workshop", FS: fstest.MapFS{
		"1/mods/Thing/mod.info":    &fstest.MapFile{Data: []byte("id=ThingB41\n")},
		"1/mods/Thing/42/mod.info": &fstest.MapFile{Data: []byte("id=Thing\nversionMin=42\n")},
	}}
	res := ResolveFor([]WorkshopRoot{root}, []string{"1"}, BuildB41)

	if res.Items[0].Status != StatusCheck {
		t.Fatalf("status = %q, want check: a B41 target with no B41 marker has to warn",
			res.Items[0].Status)
	}
	if len(res.Ambiguous) != 1 {
		t.Errorf("the item was not reported as ambiguous: %v", res.Ambiguous)
	}
	if len(res.ModIDs) != 2 {
		t.Errorf("Mods= would be %v; with nothing to choose between, every id is enabled and "+
			"warned about rather than one being dropped silently", res.ModIDs)
	}
}

// TestAnUnknownTargetMakesEverythingAmbiguous: silently falling back to B42
// would be the one outcome nobody could see.
func TestAnUnknownTargetMakesEverythingAmbiguous(t *testing.T) {
	root := WorkshopRoot{Name: "/workshop", FS: dualBuildItem()}
	res := ResolveFor([]WorkshopRoot{root}, []string{"3435796523"}, Build(99))

	if res.Items[0].Status != StatusCheck {
		t.Fatalf("status = %q, want check", res.Items[0].Status)
	}
	if len(res.ModIDs) != 2 {
		t.Errorf("Mods= would be %v; with no scoring question to answer, every id inside the "+
			"item is enabled and warned about", res.ModIDs)
	}
}
