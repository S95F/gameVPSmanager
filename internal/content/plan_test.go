package content

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// fakeSource is a Source whose Materialize and Activate are written the way
// section 12.2 says every source's must be: the gate first, the work second.
// It stands in for steamworkshop here so the GATE is what is under test rather
// than one source's plumbing.
type fakeSource struct {
	sc *Schema
	// materialized and activated record whether the body ran, which is the
	// assertion that matters: a refusal that happened after the writes would
	// be no refusal at all.
	materialized bool
	activated    bool
}

func (f *fakeSource) Kind() SourceKind { return KindSteamWorkshop }

func (f *fakeSource) Resolve(context.Context, *Input, []Ref) (*Plan, error) { return nil, nil }

func (f *fakeSource) Materialize(_ context.Context, _ *Input, p *Plan, _ Progress) (*Lock, error) {
	if err := Materializable(p); err != nil {
		return nil, err
	}
	f.materialized = true
	sl, err := p.Lock(f.sc)
	if err != nil {
		return nil, err
	}
	l := NewLock(&Input{Instance: "pz-main", Game: "projectzomboid"})
	if err := l.Put(f.sc.Block, sl); err != nil {
		return nil, err
	}
	return l, nil
}

func (f *fakeSource) Activate(_ context.Context, _ *Input, l *Lock) (*Activation, error) {
	a, err := Activate(f.sc, l)
	if err != nil {
		return nil, err
	}
	f.activated = true
	return a, nil
}

func (f *fakeSource) Observe(context.Context, *Input) (*Observed, error) { return nil, nil }

// goodPlan is three declared items, all resolved and all on disk.
func goodPlan() *Plan {
	refs := []Ref{
		{ID: "3171167894", Note: "that DAMN Library (required by all KI5 vehicle mods)"},
		{ID: "3508537032", Note: "NeatUI Framework"},
		{ID: "2866258937", Note: "Mini Health Panel [B41/B42.20]"},
	}
	p := &Plan{Block: "workshop", Source: KindSteamWorkshop, Declared: len(refs)}
	activation := [][]ActivationID{
		{"ThatDamnLibrary"},
		{"NeatUIFramework"},
		// The multi case: one workshop item, two mods. 2866258937 is the live
		// example and the reason ActivationIDs is a slice.
		{"MiniHealthPanel", "MiniHealthPanelB41"},
	}
	evidence := [][]string{
		{"modinfo:versionMin>=42"},
		{"modinfo:42-dir"},
		{"modinfo:no-build-marker"},
	}
	for i, r := range refs {
		e := PlanEntry{
			Ordinal:      i,
			Ref:          r,
			Activation:   activation[i],
			Resolution:   ResolutionServerBoot,
			Materialized: true,
			Ambiguous:    i == 2,
			Evidence:     evidence[i],
		}
		if i == 0 {
			// One artifact, so the golden lockfile pins the tree shape too: a
			// workshop item is a directory the server downloaded, not a blob
			// gvm can hand to the store.
			e.Artifact = &Artifact{
				Kind:    ArtifactTree,
				Path:    "/home/pzserver/Zomboid/steamapps/workshop/content/108600/3171167894",
				Digest:  Digest(AlgoTreeSHA256 + ":4b19c0" + strings.Repeat("0", 58)),
				Size:    48211934,
				Members: 412,
			}
			e.Upstream = &Upstream{
				API:           "steam_remote_storage",
				TimeUpdated:   time.Date(2026, 8, 21, 4, 11, 9, 0, time.UTC),
				Title:         "that DAMN Library",
				Tags:          []string{"Build 41", "Build 42"},
				ConsumerAppID: 108600,
			}
		}
		p.Entries = append(p.Entries, e)
	}
	return p
}

func TestAGoodPlanMaterializesAndActivates(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	f := &fakeSource{sc: sc}
	p := goodPlan()

	l, err := f.Materialize(context.Background(), nil, p, Progress{})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	a, err := f.Activate(context.Background(), nil, l)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got := a.Acquisition.Value(); got != "3171167894;3508537032;2866258937" {
		t.Errorf("acquisition line = %q", got)
	}
	if got := a.Activate.Value(); got != "ThatDamnLibrary;NeatUIFramework;MiniHealthPanel;MiniHealthPanelB41" {
		t.Errorf("activation line = %q", got)
	}
	if a.Acquisition.Key != "WorkshopItems" || a.Activate.Key != "Mods" {
		t.Errorf("the two lines must carry their OWN keys, got %q and %q",
			a.Acquisition.Key, a.Activate.Key)
	}
	// 3 acquisition ids, 4 activation ids: the "multi" case, which is why
	// ActivationIDs is a slice.
	if len(a.Acquisition.IDs) != 3 || len(a.Activate.IDs) != 4 {
		t.Errorf("got %d acquisition and %d activation ids; one item holding two mods is "+
			"normal and must not collapse", len(a.Acquisition.IDs), len(a.Activate.IDs))
	}
}

// TestUnaccountedRefusesMaterializeAndActivate is the fail-closed gate.
//
// rebuild.sh counted the ids it parsed against the ids in the raw file and
// refused to rewrite mods.txt when the two disagreed, leaving the file
// untouched. This is that check as a type.
func TestUnaccountedRefusesMaterializeAndActivate(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	f := &fakeSource{sc: sc}

	p := goodPlan()
	p.Unaccounted = []Unaccounted{{
		ID:     "3778511497",
		Reason: "the Steam API returned result 9 for this child",
		Hint:   "the collection may be private or the item removed",
	}}
	p.Declared++ // the unaccounted item was declared too

	_, err := f.Materialize(context.Background(), nil, p, Progress{})
	if err == nil {
		t.Fatal("Materialize accepted a plan with an unaccounted item; a partial list looks " +
			"exactly like a deliberate one")
	}
	if !errors.Is(err, ErrUnaccounted) {
		t.Errorf("Materialize error does not wrap ErrUnaccounted: %v", err)
	}
	if f.materialized {
		t.Fatal("Materialize ran its body before refusing")
	}
	for _, want := range []string{"3778511497", "result 9", "left as they are"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}

	// Activate refuses too, through the lock's copy of the same check: a lock
	// that recorded unaccounted items (a hand edit, or an older gvm) must not
	// be activated silently.
	l := NewLock(&Input{Instance: "pz-main"})
	sl, err := goodPlan().Lock(sc)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	sl.Unaccounted = p.Unaccounted
	if err := l.Put(sc.Block, sl); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := f.Activate(context.Background(), nil, l); !errors.Is(err, ErrUnaccounted) {
		t.Fatalf("Activate accepted a lock with an unaccounted item: %v", err)
	}
	if f.activated {
		t.Fatal("Activate ran its body before refusing")
	}
}

// TestTheCountsHaveToAddUp catches the resolver that loses track of its own
// input and records no reason for it. That is worse than a named unaccounted
// item: nothing else it reported can be trusted either.
func TestTheCountsHaveToAddUp(t *testing.T) {
	p := goodPlan()
	p.Declared = 33 // 3 entries, no unaccounted records

	err := p.CheckAccounted()
	if err == nil {
		t.Fatal("a plan that resolved 3 of 33 declared items with no recorded reason passed " +
			"the gate")
	}
	if !errors.Is(err, ErrUnaccounted) {
		t.Errorf("error does not wrap ErrUnaccounted: %v", err)
	}
	if !strings.Contains(err.Error(), "3") || !strings.Contains(err.Error(), "33") {
		t.Errorf("the refusal has to name both counts: %v", err)
	}
	if p.Accounted() {
		t.Error("Accounted() disagrees with CheckAccounted()")
	}
}

// TestAmbiguousIsNotUnaccounted is the distinction the whole report rests on.
// An ambiguous item is enabled WITH A WARNING -- today's behaviour, preserved
// exactly. An unaccounted one aborts.
func TestAmbiguousIsNotUnaccounted(t *testing.T) {
	p := goodPlan()
	if got := p.Ambiguous(); len(got) != 1 || got[0] != AcquireID("2866258937") {
		t.Fatalf("Ambiguous() = %v, want the one no-build-marker item", got)
	}
	if err := p.CheckAccounted(); err != nil {
		t.Fatalf("an ambiguous item must not close the gate: %v", err)
	}
	ids := p.ActivationIDs()
	var found bool
	for _, id := range ids {
		if id == "MiniHealthPanelB41" {
			found = true
		}
	}
	if !found {
		t.Error("the ambiguous item's ids must all be enabled; withholding them is a mod the " +
			"operator declared that silently never loads")
	}
}

func TestComputeImpactEscalatesForUnmaterializedOnServerBoot(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})

	// A reorder is `restart` in the shipped manifest, so without the
	// structural rule this plan would stop and start without ever fetching.
	p := goodPlan()
	p.Entries[1].Materialized = false
	p.Changes = []Change{
		{Kind: ChangeReorder, ID: "3171167894"},
		{Kind: ChangeAdd, ID: "3508537032", Unmaterialized: true},
	}
	if got := p.ComputeImpact(sc); got != ImpactDownloadCycle {
		t.Fatalf("impact = %s, want download_cycle: the item's bytes are not on disk and only "+
			"the server fetches them", got)
	}
	if !p.Boot {
		t.Error("Boot must be set: step 5 of the download cycle is the acquisition boot")
	}
	if !strings.Contains(p.Changes[1].Why, "not on disk") {
		t.Errorf("the escalation must say why: %q", p.Changes[1].Why)
	}
}

func TestComputeImpactEscalatesRemovingWorldData(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	p := goodPlan()
	p.Changes = []Change{{Kind: ChangeRemove, ID: "3171167894", WorldData: true}}

	if got := p.ComputeImpact(sc); got != ImpactRebuild {
		t.Fatalf("impact = %s, want rebuild: removing a world-affecting item is "+
			"archive-before-destroy", got)
	}
	// And the manifest cannot talk it back down.
	if err := p.SetImpact(ImpactRestart); err == nil {
		t.Fatal("SetImpact lowered a rebuild to a restart, which is an archive that never " +
			"gets taken")
	}
	if got := p.Impact(); got != ImpactRebuild {
		t.Errorf("impact changed to %s after a refused SetImpact", got)
	}
	if err := p.SetImpact(ImpactRebuild); err != nil {
		t.Errorf("SetImpact to the same level: %v", err)
	}
}

func TestEmptyPlanIsTheNormalNightlyAnswer(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	p := goodPlan()
	if !p.Empty() {
		t.Fatal("a plan with no changes is not Empty()")
	}
	if got := p.ComputeImpact(sc); got != ImpactNone {
		t.Errorf("impact of a no-change plan = %s; `skipped` is a first-class success (D-43)",
			got)
	}
	if p.Boot {
		t.Error("a no-change plan must not ask for an acquisition boot")
	}
}

// TestAcquireIDsKeepMissing covers the rule that keeps a failed download
// recoverable: the id stays listed so the server retries it.
func TestAcquireIDsKeepMissing(t *testing.T) {
	p := goodPlan()
	p.Entries[2].Materialized = false

	if got := p.AcquireIDs(true); len(got) != 3 {
		t.Errorf("keep_missing dropped an id: %v; the server would never request it again, so "+
			"it is never downloaded, so the next resolve fails identically", got)
	}
	if got := p.AcquireIDs(false); len(got) != 2 {
		t.Errorf("keep_missing false kept the missing id: %v", got)
	}
}

func TestPlanValidateRefusesDuplicatesAndUnexplainedRefusals(t *testing.T) {
	p := goodPlan()
	p.Entries = append(p.Entries, p.Entries[0])
	p.Declared++
	if err := p.Validate(); err == nil {
		t.Error("two entries for one acquisition id validated; the derived activation list " +
			"would depend on which one is read")
	}

	p = goodPlan()
	p.Unaccounted = []Unaccounted{{ID: "1"}}
	if err := p.Validate(); err == nil {
		t.Error("an unaccounted record with no reason validated")
	}

	p = goodPlan()
	p.Entries[0].Resolution = "edge.forgecdn.net"
	if err := p.Validate(); err == nil {
		t.Error("an unknown resolution path validated")
	}
}

func TestPlanLockRefusesAnUnaccountedPlan(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	p := goodPlan()
	p.Unaccounted = []Unaccounted{{ID: "9", Reason: "unreadable"}}
	p.Declared++

	if _, err := p.Lock(sc); !errors.Is(err, ErrUnaccounted) {
		t.Fatalf("Plan.Lock wrote a lock off an unaccounted plan: %v; that carries the partial "+
			"list forward as recorded intent, which is a step worse than writing it to the ini",
			err)
	}
}

func TestPlanSummaryNamesTheRefusal(t *testing.T) {
	p := goodPlan()
	p.Unaccounted = []Unaccounted{{ID: "9", Reason: "unreadable"}}
	p.Declared++
	if !strings.Contains(p.Summary(), "UNACCOUNTED") {
		t.Errorf("Summary() hides the refusal: %q", p.Summary())
	}
}
