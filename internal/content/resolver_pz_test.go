package content

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// workshopFS is a workshop root as the server leaves it: one directory per
// numeric id, each holding whatever layout its author shipped.
//
// fstest.MapFS rather than files on disk, because the interesting variation is
// the LAYOUT -- a 42/ folder, a 41/ folder, a versionMin line, nothing at all
// -- and a map makes each case one line to read.
func workshopFS() fstest.MapFS {
	return fstest.MapFS{
		// Explicit by declaration: versionMin 42+.
		"3171167894/mods/ThatDamnLibrary/mod.info": &fstest.MapFile{Data: []byte(
			"name=that DAMN Library\nid=ThatDamnLibrary\nversionMin=42.0.1\n")},

		// The dual-build item: one workshop id, two variants, two internal
		// ids. Enabling the wrong one is the silent failure the scoring
		// exists to prevent.
		"3435796523/mods/Hilux/41/mod.info": &fstest.MapFile{Data: []byte(
			"name=Hilux (B41)\nid=Hilux88B41\n")},
		"3435796523/mods/Hilux/42/mod.info": &fstest.MapFile{Data: []byte(
			"name=Hilux (B42)\nid=Hilux88\nversionMin=42\n")},

		// No build marker anywhere: both ids are enabled and the item is
		// flagged. 2866258937 is the live example.
		"2866258937/mods/MiniHealthPanel/mod.info": &fstest.MapFile{Data: []byte(
			"name=Mini Health Panel\nid=MiniHealthPanel\n")},
		"2866258937/mods/MiniHealthPanelB41/mod.info": &fstest.MapFile{Data: []byte(
			"name=Mini Health Panel B41\nid=MiniHealthPanelB41\n")},

		// On disk and useless: a download that started and did not finish.
		"4000000001/readme.txt": &fstest.MapFile{Data: []byte("nothing usable here\n")},
	}
}

func pzResolve(t *testing.T, targetBuild string, ids ...AcquireID) *ResolverResult {
	t.Helper()
	block := pzBlock()
	if targetBuild == "" {
		delete(block.Activation.ResolverVars, "target_build")
	} else {
		block.Activation.ResolverVars["target_build"] = targetBuild
	}
	sc := mustCompile(t, block, spec.ContentList{})

	items := make([]ResolverItem, 0, len(ids))
	for i, id := range ids {
		items = append(items, ResolverItem{Ordinal: i, ID: id})
	}
	res, err := Resolve(context.Background(), sc, items,
		[]Root{{Name: "/home/pzserver/Zomboid/steamapps/workshop/content/108600", FS: workshopFS()}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return res
}

func itemByID(t *testing.T, res *ResolverResult, id AcquireID) ResolvedItem {
	t.Helper()
	for _, it := range res.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no result for %s; the resolver dropped a declared item", id)
	return ResolvedItem{}
}

func activationStrings(ids []ActivationID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

func TestPZResolverMapsTheTwoNamespaces(t *testing.T) {
	res := pzResolve(t, "42", "3171167894")
	it := itemByID(t, res, "3171167894")

	if got := activationStrings(it.Activation); !slices.Equal(got, []string{"ThatDamnLibrary"}) {
		t.Fatalf("activation ids = %v", got)
	}
	if it.Ambiguous || it.Missing {
		t.Errorf("an explicit item is neither ambiguous nor missing: %+v", it)
	}
	// Evidence, so a surprising inclusion is explainable months later without
	// re-running the scan.
	if !slices.Contains(it.Evidence, "modinfo:versionMin>=42") {
		t.Errorf("evidence = %v, want the versionMin marker", it.Evidence)
	}
	if it.Path == "" {
		t.Error("the resolved item records no path, so the lock cannot name the artifact")
	}
	if len(res.Unaccounted) != 0 {
		t.Errorf("unaccounted = %v", res.Unaccounted)
	}
}

// TestPZResolverPicksTheVariantForTheTargetBuild is the parameterisation.
// The same workshop item resolves to a DIFFERENT internal id on a B41 server,
// and the loser is recorded rather than forgotten.
func TestPZResolverPicksTheVariantForTheTargetBuild(t *testing.T) {
	for _, tc := range []struct {
		target        string
		want, dropped string
		marker        string
	}{
		{"42", "Hilux88", "Hilux88B41", "modinfo:42-dir"},
		{"41", "Hilux88B41", "Hilux88", "modinfo:41-dir"},
		{"", "Hilux88", "Hilux88B41", "modinfo:42-dir"}, // absent means 42
	} {
		name := tc.target
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			it := itemByID(t, pzResolve(t, tc.target, "3435796523"), "3435796523")
			if got := activationStrings(it.Activation); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("target %q enabled %v, want [%s]; enabling the other build's id "+
					"downloads the item and never loads the mod", tc.target, got, tc.want)
			}
			if got := activationStrings(it.Dropped); !slices.Equal(got, []string{tc.dropped}) {
				t.Errorf("dropped = %v, want [%s]; a surprising exclusion has to be "+
					"explainable", got, tc.dropped)
			}
			if !slices.Contains(it.Evidence, tc.marker) {
				t.Errorf("evidence = %v, want %s", it.Evidence, tc.marker)
			}
		})
	}
}

// TestPZResolverRefusesAnUnparseableTargetBuild: "B42" silently becoming 42 is
// a declaration nobody checked.
func TestPZResolverRefusesAnUnparseableTargetBuild(t *testing.T) {
	block := pzBlock()
	block.Activation.ResolverVars["target_build"] = "B42"
	sc := mustCompile(t, block, spec.ContentList{})

	_, err := Resolve(context.Background(), sc, []ResolverItem{{ID: "3171167894"}},
		[]Root{{Name: "root", FS: workshopFS()}})
	if err == nil {
		t.Fatal("an unparseable target_build resolved anyway")
	}
	if !errors.Is(err, ErrInvalidSchema) {
		t.Errorf("error does not wrap ErrInvalidSchema: %v", err)
	}
	if !strings.Contains(err.Error(), "target_build") {
		t.Errorf("error does not name the var: %v", err)
	}
}

// TestPZResolverEnablesAnAmbiguousItemWithAWarning is today's behaviour,
// preserved exactly. Withholding the ids would be a mod the operator declared
// that silently never loads -- strictly worse than enabling one variant too
// many and saying so.
func TestPZResolverEnablesAnAmbiguousItemWithAWarning(t *testing.T) {
	it := itemByID(t, pzResolve(t, "42", "2866258937"), "2866258937")

	if !it.Ambiguous {
		t.Fatal("an item with no build marker anywhere was not flagged ambiguous")
	}
	got := activationStrings(it.Activation)
	slices.Sort(got)
	if !slices.Equal(got, []string{"MiniHealthPanel", "MiniHealthPanelB41"}) {
		t.Fatalf("activation ids = %v; every id inside an unmarked item is enabled", got)
	}
	if len(it.Dropped) != 0 {
		t.Errorf("dropped = %v; nothing is dropped when no variant scored higher", it.Dropped)
	}
	if !slices.Contains(it.Evidence, "modinfo:no-build-marker") {
		t.Errorf("evidence = %v", it.Evidence)
	}
	// Ambiguous is NOT unaccounted: this item must not close the gate.
	if len(pzResolve(t, "42", "2866258937").Unaccounted) != 0 {
		t.Error("an ambiguous item produced an unaccounted record, which would abort the sync")
	}
}

func TestPZResolverReportsMissingAndEmptyItemsWithoutRefusing(t *testing.T) {
	res := pzResolve(t, "42", "3171167894", "9999999999", "4000000001")

	absent := itemByID(t, res, "9999999999")
	if !absent.Missing || len(absent.Activation) != 0 {
		t.Errorf("an id with no directory anywhere: %+v", absent)
	}
	if !slices.Contains(absent.Evidence, "modinfo:not-downloaded") {
		t.Errorf("evidence = %v", absent.Evidence)
	}

	empty := itemByID(t, res, "4000000001")
	if !empty.Missing {
		t.Error("a directory with no usable mod.info must not count as acquired")
	}
	if !slices.Contains(empty.Evidence, "modinfo:no-usable-mod-info") {
		t.Errorf("evidence = %v; \"on disk but useless\" and \"not downloaded\" are different "+
			"problems with different fixes", empty.Evidence)
	}

	// Neither is unaccounted: a server_boot source fetches them on its next
	// boot, and the ids have to stay listed for that to happen.
	if len(res.Unaccounted) != 0 {
		t.Fatalf("unaccounted = %v; a download that has not happened yet is not a parse "+
			"failure", res.Unaccounted)
	}
	// Every declared item still produced a result.
	if len(res.Items) != 3 {
		t.Errorf("got %d results for 3 declared items", len(res.Items))
	}
}

func TestPZResolverRefusesARootWithNoHandle(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	_, err := Resolve(context.Background(), sc, []ResolverItem{{ID: "3171167894"}},
		[]Root{{Name: "/declared/but/unopened"}})
	if err == nil {
		t.Fatal("a declared root with no filesystem handle was skipped; resolving against the " +
			"roots that happen to have handles quietly produces a shorter mod list")
	}
	if !strings.Contains(err.Error(), "/declared/but/unopened") {
		t.Errorf("error does not name the root: %v", err)
	}
}

func TestPZResolverHonoursCancellation(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Resolve(ctx, sc, []ResolverItem{{ID: "3171167894"}},
		[]Root{{Name: "root", FS: workshopFS()}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled resolve returned %v; walking a 6 GB tree has to be "+
			"interruptible", err)
	}
}

// TestPZResolverSearchesRootsInOrder: the server writes workshop content to
// different locations depending on its version, and both can exist on one box.
// The first root holding an item wins.
func TestPZResolverSearchesRootsInOrder(t *testing.T) {
	first := fstest.MapFS{"111/mods/Winner/mod.info": &fstest.MapFile{Data: []byte("id=Winner\n")}}
	second := fstest.MapFS{"111/mods/Loser/mod.info": &fstest.MapFile{Data: []byte("id=Loser\n")}}
	sc := mustCompile(t, pzBlock(), spec.ContentList{})

	res, err := Resolve(context.Background(), sc, []ResolverItem{{ID: "111"}}, []Root{
		{Name: "/data/steamapps/workshop/content/108600", FS: first},
		{Name: "/install/steamapps/workshop/content/108600", FS: second},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	it := itemByID(t, res, "111")
	if got := activationStrings(it.Activation); !slices.Equal(got, []string{"Winner"}) {
		t.Fatalf("activation ids = %v, want the first root's", got)
	}
	if !strings.HasPrefix(it.Path, "/data/") {
		t.Errorf("path = %q, want the first root", it.Path)
	}
}

// pzResolveWithUpstream is pzResolve with pre-fetched metadata attached, which
// is the seam section 12.5's evidence table arrives through.
func pzResolveWithUpstream(t *testing.T, items []ResolverItem) *ResolverResult {
	t.Helper()
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	res, err := Resolve(context.Background(), sc, items,
		[]Root{{Name: "/home/pzserver/Zomboid/steamapps/workshop/content/108600", FS: workshopFS()}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return res
}

// TestTagsAreLayeredAsEvidenceAndNeverAsAuthority walks the three outcomes
// that matter at this seam: a promotion, a conflict, and the degradation.
//
// The id lists are asserted in every case, because the whole rule is that none
// of these three changes them.
func TestTagsAreLayeredAsEvidenceAndNeverAsAuthority(t *testing.T) {
	t.Run("a single-build tag promotes an unmarked item", func(t *testing.T) {
		res := pzResolveWithUpstream(t, []ResolverItem{{
			ID:       "2866258937",
			Upstream: &Upstream{API: "steam_remote_storage", Tags: []string{"Build 42"}},
		}})
		it := itemByID(t, res, "2866258937")

		if it.Ambiguous {
			t.Error("an item whose Workshop page is tagged for one build only is not ambiguous; " +
				"a single-build item's ids are all that build's")
		}
		got := activationStrings(it.Activation)
		slices.Sort(got)
		if !slices.Equal(got, []string{"MiniHealthPanel", "MiniHealthPanelB41"}) {
			t.Fatalf("activation ids = %v; a promotion removes a warning, never an id", got)
		}
		if !slices.Contains(it.Evidence, "tags:Build 42") {
			t.Errorf("evidence = %v, want the tag recorded", it.Evidence)
		}
		if !slices.Contains(it.Evidence, "modinfo:no-build-marker") {
			t.Errorf("evidence = %v, want the file marker kept: the promotion is explainable "+
				"only if both halves are recorded", it.Evidence)
		}
	})

	t.Run("both builds tagged stays ambiguous", func(t *testing.T) {
		res := pzResolveWithUpstream(t, []ResolverItem{{
			ID:       "2866258937",
			Upstream: &Upstream{Tags: []string{"Build 41", "Build 42"}},
		}})
		it := itemByID(t, res, "2866258937")

		if !it.Ambiguous {
			t.Fatal("an item tagged for both builds with no file marker is still a guess")
		}
		for _, want := range []string{"tags:Build 41", "tags:Build 42"} {
			if !slices.Contains(it.Evidence, want) {
				t.Errorf("evidence = %v, want %s", it.Evidence, want)
			}
		}
		if len(res.Warnings) == 0 {
			t.Error("no warning for an item enabled on a guess")
		}
	})

	t.Run("a conflict is recorded, not acted on", func(t *testing.T) {
		res := pzResolveWithUpstream(t, []ResolverItem{{
			ID:       "3171167894",
			Upstream: &Upstream{Tags: []string{"Build 41"}},
		}})
		it := itemByID(t, res, "3171167894")

		if got := activationStrings(it.Activation); !slices.Equal(got, []string{"ThatDamnLibrary"}) {
			t.Fatalf("activation ids = %v; the files declare versionMin 42 and they win", got)
		}
		if it.Ambiguous {
			t.Error("a conflict does not make an explicitly marked item ambiguous")
		}
		if !slices.Contains(it.Evidence, "tags:conflict") {
			t.Errorf("evidence = %v, want tags:conflict", it.Evidence)
		}
		if len(res.Warnings) != 1 {
			t.Fatalf("warnings = %v, want one sentence a human can act on", res.Warnings)
		}
		if !strings.Contains(res.Warnings[0], "3171167894") {
			t.Errorf("the warning does not name the item: %s", res.Warnings[0])
		}
	})

	t.Run("an unreachable API degrades to exactly the file heuristic", func(t *testing.T) {
		ids := []AcquireID{"3171167894", "3435796523", "2866258937", "4000000001"}

		withTags := make([]ResolverItem, 0, len(ids))
		withoutTags := make([]ResolverItem, 0, len(ids))
		for i, id := range ids {
			// Tags that, if they were authority, would change every answer:
			// Build 41 only, on a Build 42 target.
			withTags = append(withTags, ResolverItem{Ordinal: i, ID: id,
				Upstream: &Upstream{Tags: []string{"Build 41"}}})
			withoutTags = append(withoutTags, ResolverItem{Ordinal: i, ID: id})
		}

		hot := pzResolveWithUpstream(t, withTags)
		cold := pzResolveWithUpstream(t, withoutTags)

		for i := range ids {
			a, b := hot.Items[i], cold.Items[i]
			if !slices.Equal(activationStrings(a.Activation), activationStrings(b.Activation)) {
				t.Errorf("%s enabled %v with tags and %v without; Steam being down must not "+
					"change which mods a server enables", a.ID, a.Activation, b.Activation)
			}
			if !slices.Equal(activationStrings(a.Dropped), activationStrings(b.Dropped)) {
				t.Errorf("%s dropped %v with tags and %v without", a.ID, a.Dropped, b.Dropped)
			}
		}
		// The degradation is recorded rather than silent.
		if !slices.Contains(itemByID(t, cold, "2866258937").Evidence, "tags:unavailable") {
			t.Errorf("evidence = %v, want tags:unavailable",
				itemByID(t, cold, "2866258937").Evidence)
		}
	})
}
