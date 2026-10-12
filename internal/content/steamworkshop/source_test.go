package steamworkshop

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/content/fetch"
	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func refs(in ...string) []content.Ref {
	out := make([]content.Ref, 0, len(in))
	for _, id := range in {
		out = append(out, content.Ref{ID: id})
	}
	return out
}

func activationStrings(in []content.ActivationID) []string {
	out := make([]string, 0, len(in))
	for _, id := range in {
		out = append(out, string(id))
	}
	return out
}

// liveFromDeclared renders the ini an already-synced server would have, so
// "nothing to do" is expressible.
func liveFromDeclared(acquire []string, activate []string) *content.Live {
	return &content.Live{
		Acquisition: strings.Join(acquire, ";"),
		Activation:  strings.Join(activate, ";"),
	}
}

// TestNothingToDoIsAFirstClassAnswer is the normal nightly result, and it must
// not page anyone (D-43). Everything declared is listed, in order, and on disk.
func TestNothingToDoIsAFirstClassAnswer(t *testing.T) {
	declared := []string{"3171167894", "3435796523", "2866258937"}
	in := input(t, spec.ContentList{}, liveFromDeclared(declared,
		[]string{"ThatDamnLibrary", "Hilux88", "MiniHealthPanel", "MiniHealthPanelB41"}),
		workshopFS())

	p, err := offlineSource(t).Resolve(context.Background(), in, refs(declared...))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !p.Empty() {
		t.Fatalf("changes = %v, want none: %s", p.Changes, p.Summary())
	}
	if p.Impact() != content.ImpactNone {
		t.Errorf("impact = %s, want none", p.Impact())
	}
	if p.Boot {
		t.Error("an acquisition boot was planned with every item already on disk")
	}
	if !slices.Equal(p.Steps, []string{
		"plan: read-only, safe with players online", "nothing to do"}) {
		t.Errorf("steps = %v", p.Steps)
	}
	if err := p.CheckAccounted(); err != nil {
		t.Fatalf("the gate closed on a clean plan: %v", err)
	}
	if n := len(p.Entries); n != 3 || p.Declared != 3 {
		t.Errorf("%d entries for %d declared", n, p.Declared)
	}
}

// TestResolveAccountsForEveryDeclaredRef is the fail-closed gate, from the
// source's side. A typo'd id, a doubled id and an empty entry all CLOSE THE
// GATE rather than shortening the list, because a partial list looks exactly
// like a deliberate one.
func TestResolveAccountsForEveryDeclaredRef(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{}, workshopFS())
	want := []content.Ref{
		{ID: "3171167894"},
		{ID: "ThatDamnLibrary"}, // an ACTIVATION id in the acquisition list
		{ID: "3171167894"},      // declared twice
		{ID: "  "},              // nothing at all
	}

	p, err := offlineSource(t).Resolve(context.Background(), in, want)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Declared != 4 {
		t.Errorf("declared = %d", p.Declared)
	}
	if len(p.Unaccounted) != 3 {
		t.Fatalf("unaccounted = %v, want three records", p.Unaccounted)
	}
	gate := p.CheckAccounted()
	if !errors.Is(gate, content.ErrUnaccounted) {
		t.Fatalf("CheckAccounted = %v", gate)
	}
	// The refusal has to be actionable: a gate that said only "refused" is a
	// gate operators disable.
	for _, want := range []string{"ThatDamnLibrary", "declared twice", "no id"} {
		if !strings.Contains(gate.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, gate)
		}
	}
	for _, u := range p.Unaccounted {
		if strings.TrimSpace(u.Reason) == "" {
			t.Errorf("unaccounted %v carries no reason", u)
		}
	}

	// And nothing downstream can proceed off it.
	if _, err := offlineSource(t).Materialize(context.Background(), in, p,
		content.Progress{}); !errors.Is(err, content.ErrUnaccounted) {
		t.Fatalf("Materialize off an unaccounted plan = %v", err)
	}
	if _, err := p.Lock(in.Schema); !errors.Is(err, content.ErrUnaccounted) {
		t.Fatalf("Plan.Lock off an unaccounted plan = %v", err)
	}
}

// TestResolveRequiresAReadingOfTheLiveConfig: a plan is a statement about what
// has to CHANGE. Computed without knowing what is live it would read an unread
// config as an empty one, which on an adopted server means every mod looks
// newly added and Mods= looks like it needs writing from scratch.
func TestResolveRequiresAReadingOfTheLiveConfig(t *testing.T) {
	in := input(t, spec.ContentList{}, nil, workshopFS())
	_, err := offlineSource(t).Resolve(context.Background(), in, refs("3171167894"))
	if !errors.Is(err, ErrInput) {
		t.Fatalf("error = %v, want ErrInput", err)
	}
	for _, want := range []string{"WorkshopItems", "Mods"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestAnUndownloadedItemPlansADownloadCycle is section 12.6's structural
// escalation: the bytes are not there yet and only the server can fetch them,
// so the plan grows an acquisition boot between the stops.
func TestAnUndownloadedItemPlansADownloadCycle(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{
		Acquisition: "3171167894",
		Activation:  "ThatDamnLibrary",
	}, workshopFS())

	p, err := offlineSource(t).Resolve(context.Background(), in,
		refs("3171167894", "3778511497"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Impact() != content.ImpactDownloadCycle {
		t.Fatalf("impact = %s, want download_cycle: %s", p.Impact(), p.Summary())
	}
	if !p.Boot {
		t.Fatal("no acquisition boot was planned for an item whose bytes are not on disk")
	}
	if !slices.Equal(p.Unmaterialized(), ids("3778511497")) {
		t.Errorf("unmaterialized = %v", p.Unmaterialized())
	}
	// The id STAYS LISTED so the server retries it: dropping it means it is
	// never requested again, so it is never downloaded, so the next resolve
	// fails identically.
	if !slices.Equal(p.AcquireIDs(in.Schema.Acquisition.KeepMissing),
		ids("3171167894", "3778511497")) {
		t.Errorf("acquisition line = %v", p.AcquireIDs(true))
	}
	var found bool
	for _, c := range p.Changes {
		if c.ID == "3778511497" && c.Unmaterialized {
			found = true
			if !strings.Contains(c.Why, "acquisition boot") {
				t.Errorf("the change does not explain the boot: %q", c.Why)
			}
		}
	}
	if !found {
		t.Errorf("no unmaterialized change for the new item: %v", p.Changes)
	}
	if !slices.Contains(p.Steps, "stop: clean ladder again") {
		t.Errorf("steps = %v, want the two stops around the acquisition boot", p.Steps)
	}
}

// TestAnItemListedButNeverDownloadedStillPlansWork: saying nothing here would
// make `gvm content sync` a no-op on an instance whose operator is waiting for
// a mod to appear.
func TestAnItemListedButNeverDownloadedStillPlansWork(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{
		Acquisition: "3171167894;3778511497",
		Activation:  "ThatDamnLibrary",
	}, workshopFS())

	p, err := offlineSource(t).Resolve(context.Background(), in,
		refs("3171167894", "3778511497"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Empty() {
		t.Fatal("a declared, listed, never-downloaded item planned nothing")
	}
	if p.Impact() != content.ImpactDownloadCycle || !p.Boot {
		t.Errorf("impact = %s, boot = %t", p.Impact(), p.Boot)
	}
}

// TestADelistedItemIsARemoval, and the reorder report does not drown in it:
// removing one item from the middle of a list shifts every later index by one,
// and 20 reorder lines for one removal is a report that gets skimmed.
func TestADelistedItemIsARemovalWithoutAReorderStorm(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{
		Acquisition: "2222222222;3171167894;3435796523;2866258937",
		Activation:  "ThatDamnLibrary;Hilux88;MiniHealthPanel;MiniHealthPanelB41",
	}, workshopFS())

	p, err := offlineSource(t).Resolve(context.Background(), in,
		refs("3171167894", "3435796523", "2866258937"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var removes, reorders []content.AcquireID
	for _, c := range p.Changes {
		switch c.Kind {
		case content.ChangeRemove:
			removes = append(removes, c.ID)
		case content.ChangeReorder:
			reorders = append(reorders, c.ID)
		}
	}
	if !slices.Equal(removes, ids("2222222222")) {
		t.Errorf("removes = %v", removes)
	}
	if len(reorders) != 0 {
		t.Errorf("reorders = %v; the three surviving items kept their relative order", reorders)
	}
	if p.Impact() != content.ImpactRestart {
		t.Errorf("impact = %s, want restart (the manifest's value for a removal)", p.Impact())
	}
}

// TestAReorderIsReportedAndNothingIsReordered: load order is hand-tuned, and
// the declared order is the one that gets written.
func TestAReorderIsReportedAgainstTheLiveLine(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{
		Acquisition: "3435796523;3171167894",
		Activation:  "Hilux88;ThatDamnLibrary",
	}, workshopFS())

	p, err := offlineSource(t).Resolve(context.Background(), in,
		refs("3171167894", "3435796523"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var reorders []content.Change
	for _, c := range p.Changes {
		if c.Kind == content.ChangeReorder {
			reorders = append(reorders, c)
		}
	}
	if len(reorders) == 0 {
		t.Fatalf("no reorder reported: %v", p.Changes)
	}
	if reorders[0].ID != "3171167894" {
		t.Errorf("reorder = %+v, want the library that moved to the front", reorders[0])
	}
	// The plan's own order is still the DECLARED order, untouched.
	if got := p.AcquireIDs(true); !slices.Equal(got, ids("3171167894", "3435796523")) {
		t.Errorf("acquisition line = %v, want the declared order", got)
	}
}

// TestActivationDriftIsAChange is section 12.6 step 10, as data: the SERVER
// pulled a new version of an already-listed item and its internal ids moved.
// update.sh's blind second restart, replaced by its data-driven descendant.
func TestActivationDriftIsAChange(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{
		Acquisition: "3171167894;3435796523",
		// Hilux88 is missing: the item updated and changed its internal id.
		Activation: "ThatDamnLibrary",
	}, workshopFS())

	p, err := offlineSource(t).Resolve(context.Background(), in,
		refs("3171167894", "3435796523"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var drift *content.Change
	for i, c := range p.Changes {
		if c.Kind == content.ChangeActivation {
			drift = &p.Changes[i]
		}
	}
	if drift == nil {
		t.Fatalf("no activation change: %v", p.Changes)
	}
	if drift.ID != "3435796523" || !strings.Contains(drift.To, "Hilux88") {
		t.Errorf("drift = %+v", drift)
	}
	if p.Impact() != content.ImpactRestart {
		t.Errorf("impact = %s, want restart", p.Impact())
	}
}

// TestVerbatimActivationPlansNoActivationDrift: under `activation_policy:
// preserve` the line is the operator's, it is EXPECTED to differ from what the
// entries render, and reporting that as drift would be gvm proposing to undo
// somebody's hand-pruned edit on every sync.
func TestVerbatimActivationPlansNoActivationDrift(t *testing.T) {
	preserve := spec.ContentList{ActivationPolicy: spec.ActivationPreserve}
	// Every declared item is live and on disk EXCEPT one, so the plan has
	// something to do -- and the live Mods= is missing two ids the entries
	// would render, which under a derive policy is exactly the drift the
	// previous test asserts.
	in := input(t, preserve, &content.Live{
		Acquisition: "3171167894;3435796523;2866258937",
		Activation:  "ThatDamnLibrary",
	}, workshopFS())

	p, err := offlineSource(t).Resolve(context.Background(), in,
		refs("3171167894", "3435796523", "2866258937", "3508537032"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, c := range p.Changes {
		if c.Kind == content.ChangeActivation {
			t.Fatalf("a verbatim block planned an activation change: %+v; the line is the "+
				"operator's and is EXPECTED to differ from what the entries render", c)
		}
	}
	if p.Empty() {
		t.Fatal("the plan has a newly declared item and reported no change")
	}
	if !slices.Contains(p.Steps, "activation: the recorded Mods line is preserved verbatim "+
		"and is NOT rewritten") {
		t.Errorf("steps = %v", p.Steps)
	}
	for _, step := range p.Steps {
		if strings.HasPrefix(step, "write activation") {
			t.Errorf("a verbatim plan proposes to write the activation key: %q", step)
		}
	}
}

// TestResolveRecordsAnArtifactWithNoDigest: gvm did not download these bytes
// and cannot claim to have verified them. Presence and non-emptiness, counted
// and sized; the tree digest is `verify --deep`.
func TestResolveRecordsAnArtifactWithNoDigest(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{}, workshopFS())
	p, err := offlineSource(t).Resolve(context.Background(), in, refs("3171167894"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	e := p.Entries[0]
	if e.Artifact == nil {
		t.Fatal("no artifact for an item that is on disk")
	}
	if e.Artifact.Kind != content.ArtifactTree {
		t.Errorf("kind = %q, want tree", e.Artifact.Kind)
	}
	if e.Artifact.Digest != "" {
		t.Errorf("digest = %q; recording a plausible-looking digest for bytes gvm never had "+
			"is how an unverifiable identity gets mistaken for a verified one", e.Artifact.Digest)
	}
	if e.Artifact.Members != 2 || e.Artifact.Size == 0 {
		t.Errorf("artifact = %+v, want the two members counted and sized", e.Artifact)
	}
	if !strings.HasPrefix(e.Artifact.Path, workshopRoot) {
		t.Errorf("path = %q", e.Artifact.Path)
	}
	if e.Resolution != content.ResolutionServerBoot {
		t.Errorf("resolution = %q, want server_boot: the SERVER downloads these", e.Resolution)
	}
	if p.Bytes != e.Artifact.Size {
		t.Errorf("plan bytes = %d, artifact %d", p.Bytes, e.Artifact.Size)
	}
}

func TestAVendoredRefRecordsItsOwnResolution(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{}, workshopFS())
	p, err := offlineSource(t).Resolve(context.Background(), in,
		[]content.Ref{{ID: "3171167894", Vendored: true}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := p.Entries[0].Resolution; got != content.ResolutionVendored {
		t.Errorf("resolution = %q, want vendored: operator bytes, never fetched, never pruned",
			got)
	}
}

// TestTheMetadataChangesEvidenceAndNeverTheIDs is section 12.5's rule at the
// source level: the same declared list, resolved with and without a reachable
// API, enables exactly the same mods.
func TestTheMetadataChangesEvidenceAndNeverTheIDs(t *testing.T) {
	declared := refs("3171167894", "3435796523", "2866258937", "3508537032")
	live := &content.Live{}

	s := newStub(t)
	online, err := New(Config{WorkshopAppID: pzWorkshopAppID, Policy: loopbackPolicy(),
		APIBase: s.srv.URL, CommunityBase: s.srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	hot, err := online.Resolve(context.Background(),
		input(t, spec.ContentList{}, live, workshopFS()), declared)
	if err != nil {
		t.Fatalf("online Resolve: %v", err)
	}
	cold, err := offlineSource(t).Resolve(context.Background(),
		input(t, spec.ContentList{}, live, workshopFS()), declared)
	if err != nil {
		t.Fatalf("offline Resolve: %v", err)
	}

	if !slices.Equal(activationStrings(hot.ActivationIDs()),
		activationStrings(cold.ActivationIDs())) {
		t.Fatalf("online enabled %v and offline enabled %v; Steam being down must not change "+
			"which mods a server enables", hot.ActivationIDs(), cold.ActivationIDs())
	}
	if !slices.Equal(hot.AcquireIDs(true), cold.AcquireIDs(true)) {
		t.Errorf("the acquisition lines differ: %v vs %v", hot.AcquireIDs(true),
			cold.AcquireIDs(true))
	}

	// What DOES change is the evidence, and one ambiguity resolved: 3508537032
	// carries Build 42 alone on the Workshop, so the item is promoted out of
	// `check` -- with every one of its ids already enabled either way.
	neat, _ := hot.Entry("3508537032")
	if neat.Upstream == nil || neat.Upstream.Title != "Neat UI Framework" {
		t.Errorf("no upstream recorded: %+v", neat.Upstream)
	}
	if !slices.Contains(neat.Evidence, "tags:Build 42") {
		t.Errorf("evidence = %v", neat.Evidence)
	}
	// And the banned flag is evidence plus a warning, never a removal.
	if !slices.Contains(neat.Evidence, EvidenceBanned) {
		t.Errorf("evidence = %v, want %s", neat.Evidence, EvidenceBanned)
	}
	if !slices.Contains(hot.AcquireIDs(true), "3508537032") {
		t.Error("a banned item was dropped from the acquisition line")
	}
	var warned bool
	for _, w := range hot.Warnings {
		if strings.Contains(w, "3508537032") && strings.Contains(w, "BANNED") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("warnings = %v, want the ban reported", hot.Warnings)
	}
	// The ambiguous item stays ambiguous: both builds tagged, no file marker.
	mini, _ := hot.Entry("2866258937")
	if !mini.Ambiguous {
		t.Error("2866258937 is tagged for both builds with no file marker: still a guess")
	}
	if len(cold.Warnings) == 0 {
		t.Error("the offline resolve did not say it had no metadata")
	}
}

// TestResolveRecordsAWrongAppIDAsEvidence: a hand-declared id from another
// game's collection is evidence and a warning, not a removal. The files on
// disk are what the server will actually load, and gvm does not delete a mod
// the operator asked for.
func TestResolveRecordsAWrongAppIDAsEvidence(t *testing.T) {
	s := newStub(t)
	src, err := New(Config{WorkshopAppID: 294420, Policy: loopbackPolicy(),
		APIBase: s.srv.URL, CommunityBase: s.srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p, err := src.Resolve(context.Background(),
		input(t, spec.ContentList{}, &content.Live{}, workshopFS()), refs("3171167894"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	e := p.Entries[0]
	if !slices.Contains(e.Evidence, EvidenceWrongApp) {
		t.Fatalf("evidence = %v, want %s", e.Evidence, EvidenceWrongApp)
	}
	if len(p.Unaccounted) != 0 {
		t.Errorf("unaccounted = %v; a hand-declared id is reported, not refused", p.Unaccounted)
	}
	if !slices.Contains(p.AcquireIDs(true), "3171167894") {
		t.Error("the item was dropped")
	}
}

// TestMaterializeWritesNoBytesAndRecordsIntent. On a server_boot acquisition
// that is the truth rather than a shortcut: the SERVER downloads its own items
// from the acquisition key, so there is nothing for the jail to bound and no
// byte that could arrive unverified.
func TestMaterializeWritesNoBytesAndRecordsIntent(t *testing.T) {
	declared := []string{"3171167894", "3435796523", "2866258937"}
	in := input(t, spec.ContentList{}, &content.Live{}, workshopFS())
	s := offlineSource(t)

	p, err := s.Resolve(context.Background(), in, refs(declared...))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var steps []string
	l, err := s.Materialize(context.Background(), in, p, provider.Progress{
		OnStep: func(step string) { steps = append(steps, step) },
	})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if len(steps) == 0 {
		t.Error("a long phase reported no progress at all")
	}

	if l.Version != content.LockVersion || l.Instance != "pz-main" {
		t.Errorf("lock header = %+v", l)
	}
	if !l.GeneratedAt.Equal(in.Now()) {
		t.Errorf("generated_at = %v, want the injected clock", l.GeneratedAt)
	}
	sl, err := l.SourceFor("workshop")
	if err != nil {
		t.Fatalf("SourceFor: %v", err)
	}
	if sl.Kind != content.KindSteamWorkshop || !sl.OrderSignificant {
		t.Errorf("source lock = %+v", sl)
	}
	if sl.AcquisitionLine != strings.Join(declared, ";") {
		t.Errorf("acquisition_line = %q", sl.AcquisitionLine)
	}
	if sl.ActivationLine != "ThatDamnLibrary;Hilux88;MiniHealthPanel;MiniHealthPanelB41" {
		t.Errorf("activation_line = %q", sl.ActivationLine)
	}
	if err := l.ValidateAgainst(in.Schema); err != nil {
		t.Fatalf("the lock gvm just wrote does not validate: %v", err)
	}
	// Ordinals are load order and are recorded, not implied by position.
	for i, e := range sl.Entries {
		if e.Ordinal != i {
			t.Errorf("entry %s has ordinal %d at position %d", e.Acquire, e.Ordinal, i)
		}
	}
	// `gvm content why`'s reverse map.
	owner, ok := sl.Owning("MiniHealthPanelB41")
	if !ok || owner.Acquire != "2866258937" {
		t.Errorf("Owning(MiniHealthPanelB41) = %+v, %t", owner, ok)
	}
}

// TestMaterializeRefusesWhenAnItemVanishedAfterThePlan: Resolve may have run
// minutes ago with players online. An item that was on disk then can be gone
// now, and recording it as acquired would put a mod in Mods= that is not there.
func TestMaterializeRefusesWhenAnItemVanishedAfterThePlan(t *testing.T) {
	fsys := workshopFS()
	in := input(t, spec.ContentList{}, &content.Live{}, fsys)
	s := offlineSource(t)

	p, err := s.Resolve(context.Background(), in, refs("3171167894", "3435796523"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The mod was unsubscribed in game, or the root was unmounted, between the
	// plan and the write.
	delete(fsys, "3435796523/mods/Hilux/41/mod.info")
	delete(fsys, "3435796523/mods/Hilux/42/mod.info")

	_, err = s.Materialize(context.Background(), in, p, content.Progress{})
	if !errors.Is(err, content.ErrUnaccounted) {
		t.Fatalf("error = %v, want content.ErrUnaccounted", err)
	}
	if !strings.Contains(err.Error(), "3435796523") {
		t.Errorf("the refusal does not name the item: %v", err)
	}
}

// TestMaterializeRecordsTheLiveLineUnderVerbatimActivation is the adoption
// switch. The live line is the ONLY copy of the operator's list -- the entries
// cannot reproduce a hand-pruned ordering -- so a lock that recorded nothing
// here would be a lock whose Activate refuses, or worse, writes an empty Mods=.
func TestMaterializeRecordsTheLiveLineUnderVerbatimActivation(t *testing.T) {
	preserve := spec.ContentList{ActivationPolicy: spec.ActivationPreserve}
	// Spacing, ordering and an id gvm would not have derived: all of it is
	// the operator's, and all of it survives.
	liveLine := "ThatDamnLibrary;  Hilux88;MiniHealthPanel;SomeHandAddedThing"
	in := input(t, preserve, &content.Live{
		Acquisition: "3171167894;3435796523;2866258937",
		Activation:  liveLine,
	}, workshopFS())
	s := offlineSource(t)

	p, err := s.Resolve(context.Background(), in,
		refs("3171167894", "3435796523", "2866258937"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	l, err := s.Materialize(context.Background(), in, p, content.Progress{})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	sl, _ := l.SourceFor("workshop")
	if !sl.VerbatimActivation {
		t.Fatal("the lock does not record the preserve policy")
	}
	if sl.ActivationLine != liveLine {
		t.Fatalf("activation_line = %q, want the live line byte for byte", sl.ActivationLine)
	}
	if err := l.ValidateAgainst(in.Schema); err != nil {
		t.Fatalf("ValidateAgainst: %v", err)
	}

	// And Activate hands it back untouched, while Derive refuses outright.
	act, err := s.Activate(context.Background(), in, l)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !act.Verbatim() || act.Activate.Value() != liveLine {
		t.Fatalf("Activate returned %q, verbatim=%t", act.Activate.Value(), act.Verbatim())
	}
	// The ACQUISITION half is still gvm's, always derived from the entries.
	if act.Acquisition.Value() != "3171167894;3435796523;2866258937" {
		t.Errorf("acquisition = %q", act.Acquisition.Value())
	}
	if _, err := content.Derive(in.Schema, l); !errors.Is(err, content.ErrVerbatim) {
		t.Errorf("Derive = %v, want ErrVerbatim", err)
	}
}

// TestMaterializeRefusesVerbatimWithNothingToPreserve: writing the empty line
// would disable every mod on an adopted server, and PZ rewrites the ini on
// shutdown, so it would become permanent.
func TestMaterializeRefusesVerbatimWithNothingToPreserve(t *testing.T) {
	preserve := spec.ContentList{ActivationPolicy: spec.ActivationPreserve}
	in := input(t, preserve, &content.Live{Acquisition: "3171167894"}, workshopFS())
	s := offlineSource(t)

	p, err := s.Resolve(context.Background(), in, refs("3171167894"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = s.Materialize(context.Background(), in, p, content.Progress{})
	if !errors.Is(err, content.ErrInvalidLock) {
		t.Fatalf("error = %v, want content.ErrInvalidLock", err)
	}
	if !strings.Contains(err.Error(), "gvm content import") {
		t.Errorf("the refusal does not name the way out: %v", err)
	}
}

func TestMaterializeRefusesAPlanForAnotherBlock(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{}, workshopFS())
	s := offlineSource(t)
	p, err := s.Resolve(context.Background(), in, refs("3171167894"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	p.Block = "someone-elses-block"
	if _, err := s.Materialize(context.Background(), in, p, content.Progress{}); !errors.Is(err,
		ErrInput) {
		t.Fatalf("error = %v, want ErrInput", err)
	}
}

func TestActivateDerivesTheTwoLinesFromTheLock(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{}, workshopFS())
	s := offlineSource(t)
	p, err := s.Resolve(context.Background(), in, refs("3171167894", "2866258937"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	l, err := s.Materialize(context.Background(), in, p, content.Progress{})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	act, err := s.Activate(context.Background(), in, l)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if act.Verbatim() {
		t.Error("a derive-policy block returned a verbatim line")
	}
	if act.Acquisition.Key != "WorkshopItems" || act.Activate.Key != "Mods" {
		t.Fatalf("the two namespaces write to %q and %q", act.Acquisition.Key, act.Activate.Key)
	}
	// Activation first when both land in one file, so a diff shows one
	// changed value rather than two moved lines.
	if got := act.Writes(); !slices.Equal(got, []string{
		"Mods=ThatDamnLibrary;MiniHealthPanel;MiniHealthPanelB41",
		"WorkshopItems=3171167894;2866258937",
	}) {
		t.Errorf("writes = %#v", got)
	}
}

func TestResolveHonoursCancellation(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{}, workshopFS())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := offlineSource(t).Resolve(ctx, in, refs("3171167894")); !errors.Is(err,
		context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// TestTheRootThatHeldTheItemIsTheOneThatSizesIt: the server writes workshop
// content to different locations depending on its version and both can exist
// on one box, so the artifact walk has to size the item under the SAME root the
// resolver found it in -- not under whichever root comes first.
func TestTheRootThatHeldTheItemIsTheOneThatSizesIt(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{}, workshopFS())
	// Two roots: the resolver reports the path of whichever one held the item,
	// and here the second root's name is a lie about where its handle points.
	in.Roots = []content.Root{
		{Name: "/elsewhere", FS: fstest.MapFS{}},
		{Name: workshopRoot, FS: workshopFS()},
	}
	p, err := offlineSource(t).Resolve(context.Background(), in, refs("3171167894"))
	if err != nil {
		t.Fatalf("Resolve with two roots: %v", err)
	}
	e := p.Entries[0]
	if !e.Materialized || e.Artifact == nil {
		t.Fatalf("the item was not found under the second root: %+v", e)
	}
	if !strings.HasPrefix(e.Artifact.Path, workshopRoot) {
		t.Errorf("path = %q, want the root that actually held it", e.Artifact.Path)
	}
	if e.Artifact.Members != 2 {
		t.Errorf("members = %d; sizing it under the wrong root would report 0",
			e.Artifact.Members)
	}
}

// TestPlanWarningsComeOutInDeclaredOrder.
//
// Details.Refused is a map, and the warning loop used to range over it
// directly. Go randomises map iteration, so two consecutive `gvm content sync
// --dry-run` runs over an UNCHANGED instance printed the same findings in
// different orders -- and the same slice is serialised into the --json payload,
// so a dashboard diffing two readings sees a change that is not one. The
// neighbouring det.Absent loop already walks a slice in request order, which is
// what made this line the outlier.
//
// Looped rather than run twice: a rotation reproduces often but not reliably,
// so this asserts the CONTRACT -- the order is the declared order -- instead of
// sampling for a disagreement.
func TestPlanWarningsComeOutInDeclaredOrder(t *testing.T) {
	// Five ids the fixture knows nothing about, so the stub answers result 9
	// for every one and all five land in Refused.
	declared := []string{"3900000001", "3900000002", "3900000003", "3900000004", "3900000005"}

	st := newStub(t)
	p := fetch.Loopback("127.0.0.1")
	src, err := New(Config{
		WorkshopAppID: pzWorkshopAppID,
		Policy:        &p,
		APIBase:       st.srv.URL,
		CommunityBase: st.srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var first []string
	for run := range 12 {
		in := input(t, spec.ContentList{}, liveFromDeclared(declared, nil), workshopFS())
		plan, err := src.Resolve(context.Background(), in, refs(declared...))
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		// The ids, in the order their warnings were emitted.
		var order []string
		for _, w := range plan.Warnings {
			for _, id := range declared {
				if strings.HasPrefix(w, id+":") {
					order = append(order, id)
				}
			}
		}
		if !slices.Equal(order, declared) {
			t.Fatalf("run %d: warning order = %v, want the declared order %v", run, order,
				declared)
		}
		if run == 0 {
			first = plan.Warnings
			continue
		}
		if !slices.Equal(plan.Warnings, first) {
			t.Fatalf("run %d: two resolves of one unchanged instance disagree.\n got %v\nwant %v",
				run, plan.Warnings, first)
		}
	}
	if len(first) < len(declared) {
		t.Fatalf("warnings = %v; the refused items were not warned about at all", first)
	}
}
