package steamworkshop

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// pzBlock is the shipped Project Zomboid content block, as
// games/projectzomboid.yaml declares it.
//
// A copy of the manifest's values rather than a read of the file, because the
// point of these tests is the SOURCE's behaviour against a known schema; a
// manifest edit should break the manifest's own tests, not every test here.
func pzBlock() spec.ContentSpec {
	return spec.ContentSpec{
		Name:             "workshop",
		Source:           spec.SourceSteamWorkshop,
		WorkshopAppID:    pzWorkshopAppID,
		OrderSignificant: true,
		Acquisition: spec.ContentAcquisition{
			Mechanism:   spec.AcquireServerBoot,
			WriteKey:    "WorkshopItems",
			WriteFile:   "server_ini",
			Separator:   ";",
			KeepMissing: true,
			BootTimeout: spec.Duration(3600 * time.Second),
			Verify:      spec.AcquireVerify{DirNonEmpty: true},
		},
		Activation: spec.ContentActivation{
			WriteKey:     "Mods",
			WriteFile:    "server_ini",
			Separator:    ";",
			Resolver:     "pz.resolve_mod_ids",
			ResolverVars: map[string]string{"target_build": "42"},
		},
		MetadataAPI:    APIName,
		CollectionSync: true,
		OwnedKeys:      []string{"WorkshopItems", "Mods"},
		Impact: map[string]string{
			"add": "download_cycle", "remove": "restart", "reorder": "restart",
			"update": "download_cycle", "repin": "download_cycle",
		},
	}
}

const workshopRoot = "/home/pzserver/Zomboid/steamapps/workshop/content/108600"

func mustSchema(t *testing.T, list spec.ContentList) *content.Schema {
	t.Helper()
	sc, err := content.Compile(pzBlock(), list, []string{workshopRoot})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return sc
}

// workshopFS is a workshop root as the server leaves it: one directory per
// numeric id, each holding whatever layout its author shipped.
func workshopFS() fstest.MapFS {
	return fstest.MapFS{
		// Explicit by declaration: versionMin 42+.
		"3171167894/mods/ThatDamnLibrary/mod.info": &fstest.MapFile{Data: []byte(
			"name=that DAMN Library\nid=ThatDamnLibrary\nversionMin=42.0.1\n")},
		"3171167894/mods/ThatDamnLibrary/media/x.txt": &fstest.MapFile{Data: []byte("bytes")},

		// The dual-build item: one workshop id, two variants, two internal
		// ids. Enabling the wrong one downloads the item and never loads it.
		"3435796523/mods/Hilux/41/mod.info": &fstest.MapFile{Data: []byte(
			"name=Hilux (B41)\nid=Hilux88B41\n")},
		"3435796523/mods/Hilux/42/mod.info": &fstest.MapFile{Data: []byte(
			"name=Hilux (B42)\nid=Hilux88\nversionMin=42\n")},

		// No build marker anywhere: every id inside it is enabled and the item
		// is flagged. 2866258937 is the live example.
		"2866258937/mods/MiniHealthPanel/mod.info": &fstest.MapFile{Data: []byte(
			"name=Mini Health Panel\nid=MiniHealthPanel\n")},
		"2866258937/mods/MiniHealthPanelB41/mod.info": &fstest.MapFile{Data: []byte(
			"name=Mini Health Panel B41\nid=MiniHealthPanelB41\n")},

		// An item nobody declared, sitting on disk. Adoption's finding.
		"3508537032/mods/NeatUIFramework/mod.info": &fstest.MapFile{Data: []byte(
			"name=Neat UI Framework\nid=NeatUIFramework\nversionMin=42\n")},

		// On disk and useless: a download that started and did not finish.
		"4000000001/.keep": &fstest.MapFile{Data: []byte("")},
	}
}

func rootsOf(fsys fs.FS) []content.Root {
	return []content.Root{{Name: workshopRoot, FS: fsys}}
}

func input(t *testing.T, list spec.ContentList, live *content.Live, fsys fs.FS) *content.Input {
	t.Helper()
	return &content.Input{
		Instance:       "pz-main",
		Game:           "projectzomboid",
		ManifestSHA256: strings.Repeat("ab", 32),
		Schema:         mustSchema(t, list),
		Roots:          rootsOf(fsys),
		Clock:          func() time.Time { return time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC) },
		Live:           live,
	}
}

func offlineSource(t *testing.T) *Source {
	t.Helper()
	s, err := New(Config{WorkshopAppID: pzWorkshopAppID, Offline: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestObserveJoinsTheConfigAndTheDisk is the reading adoption is built on, and
// the one that answers the discovery finding section 12.9 names: WorkshopItems=
// has 32 entries and Mods= has 33, so WHICH id is the extra one.
func TestObserveJoinsTheConfigAndTheDisk(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{
		Acquisition: "3171167894;3435796523;2866258937;9999999999",
		Activation: "ThatDamnLibrary;Hilux88;MiniHealthPanel;MiniHealthPanelB41;" +
			"SomethingNobodyHasHeardOf",
		ReadAt: time.Now(),
	}, workshopFS())

	obs, err := offlineSource(t).Observe(context.Background(), in)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}

	if !obs.ConfigRead {
		t.Fatal("the config was read and the reading does not say so")
	}
	if !slices.Equal(obs.Acquire,
		ids("3171167894", "3435796523", "2866258937", "9999999999")) {
		t.Errorf("acquire = %v", obs.Acquire)
	}
	if obs.AcquisitionRaw != in.Live.Acquisition {
		t.Error("the raw value was normalised; a hand edit has to survive the reading")
	}
	if !slices.Equal(obs.Roots, []string{workshopRoot}) {
		t.Errorf("roots = %v", obs.Roots)
	}

	// THE finding: one activation id live in the config that no observed item
	// accounts for. The operator's next question is which, and this is where
	// `gvm content why` answers it from.
	if !slices.Equal(obs.Unlisted,
		[]content.ActivationID{"SomethingNobodyHasHeardOf"}) {
		t.Fatalf("unlisted = %v, want the one id nothing on this box accounts for", obs.Unlisted)
	}

	// An item on disk that nobody declared is still observed: that is the
	// other half of the same discovery.
	if _, ok := obs.Item("3508537032"); !ok {
		t.Error("an item sitting on disk that the config does not list was not observed")
	}

	lib, _ := obs.Item("3171167894")
	if !lib.Materialized() {
		t.Errorf("3171167894 = %+v", lib)
	}
	if !slices.Contains(lib.Activation, "ThatDamnLibrary") {
		t.Errorf("activation = %v", lib.Activation)
	}

	// Absent, and "there but empty", are different problems with different
	// fixes, and neither counts as acquired.
	absent, _ := obs.Item("9999999999")
	if absent.Present || absent.Materialized() {
		t.Errorf("9999999999 = %+v", absent)
	}
	empty, _ := obs.Item("4000000001")
	if !empty.Present || !empty.Empty || empty.Materialized() {
		t.Errorf("4000000001 = %+v; a folder with no usable mod.info is not acquired", empty)
	}
}

// TestADroppedVariantLiveInModsIsNotUnlisted: a stale id from before a variant
// switch is a different finding from an id nothing on this box has ever heard
// of, and lumping the two together is how the second one gets ignored.
func TestADroppedVariantLiveInModsIsNotUnlisted(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{
		Acquisition: "3435796523",
		// Hilux88B41 is the LOSING variant inside 3435796523 on a B42 target.
		Activation: "Hilux88B41",
	}, workshopFS())

	obs, err := offlineSource(t).Observe(context.Background(), in)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(obs.Unlisted) != 0 {
		t.Fatalf("unlisted = %v; a dropped variant is accounted for by the item that ships it",
			obs.Unlisted)
	}
	it, _ := obs.Item("3435796523")
	if !slices.Contains(it.Dropped, "Hilux88B41") {
		t.Errorf("dropped = %v", it.Dropped)
	}
}

// TestObserveToleratesAnUnreadConfig: reading state is something an operator
// may ask for on an instance whose config does not exist yet.
func TestObserveToleratesAnUnreadConfig(t *testing.T) {
	obs, err := offlineSource(t).Observe(context.Background(),
		input(t, spec.ContentList{}, nil, workshopFS()))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.ConfigRead {
		t.Fatal("nobody read the config and the reading claims otherwise; an unread config " +
			"and an empty one are the same bytes, and only one of them is a reason to write")
	}
	if len(obs.Acquire) != 0 || len(obs.Activate) != 0 || len(obs.Unlisted) != 0 {
		t.Errorf("an unread config produced lists: %+v", obs)
	}
	// The disk is still read: that is the half that does not need the config.
	if len(obs.Items) != 5 {
		t.Errorf("observed %d item(s) on disk, want the 5 directories in the root",
			len(obs.Items))
	}
}

// errorFS fails ReadDir, which is what a permissions problem looks like.
type errorFS struct{ fs.FS }

func (errorFS) Open(string) (fs.File, error) { return nil, fs.ErrPermission }

// TestObserveRefusesARootItCannotList: "no items are downloaded" and "this
// directory could not be read" differ by everything that matters. The first
// sends a server_boot instance into an hour-long acquisition boot it does not
// need and reports every one of the operator's mods as missing; the second is
// a permissions problem with a one-line fix.
func TestObserveRefusesARootItCannotList(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{}, errorFS{})
	_, err := offlineSource(t).Observe(context.Background(), in)
	if !errors.Is(err, ErrInput) {
		t.Fatalf("error = %v, want ErrInput", err)
	}
	if !strings.Contains(err.Error(), workshopRoot) {
		t.Errorf("the refusal does not name the root: %v", err)
	}
}

func TestObserveRefusesARootWithNoHandle(t *testing.T) {
	in := input(t, spec.ContentList{}, &content.Live{}, workshopFS())
	in.Roots = append(in.Roots, content.Root{Name: "/declared/but/unopened"})
	if _, err := offlineSource(t).Observe(context.Background(), in); !errors.Is(err, ErrInput) {
		t.Fatalf("error = %v; scanning the roots that happen to have handles quietly produces "+
			"a shorter answer", err)
	}
}

// TestSplitRawKeepsOrderAndDropsEmptyFields: PZ writes "a;b;c;" about as often
// as "a;b;c", and an empty id in the middle of Mods= is a typo rather than an
// instruction. Order is load order, so nothing is sorted and nothing is
// de-duplicated.
func TestSplitRawKeepsOrderAndDropsEmptyFields(t *testing.T) {
	got := splitAcquire(" 3171167894; 3435796523 ;;2866258937; ", ";")
	if !slices.Equal(got, ids("3171167894", "3435796523", "2866258937")) {
		t.Errorf("acquire = %v", got)
	}
	// A repeat is evidence of an edit and survives the reading.
	if g := splitActivate("A;B;A", ";"); !slices.Equal(g,
		[]content.ActivationID{"A", "B", "A"}) {
		t.Errorf("activate = %v", g)
	}
	if g := splitAcquire("", ";"); g != nil {
		t.Errorf("an empty value = %v, want nothing", g)
	}
	// No separator means no list, and guessing one is how a comma ends up
	// disabling every mod at once.
	if g := splitActivate("A;B", ""); !slices.Equal(g, []content.ActivationID{"A;B"}) {
		t.Errorf("no separator = %v", g)
	}
}

func TestObserveRefusesTheWrongSchema(t *testing.T) {
	s := offlineSource(t)
	if _, err := s.Observe(context.Background(), nil); !errors.Is(err, ErrInput) {
		t.Errorf("Observe(nil) = %v", err)
	}
	if _, err := s.Observe(context.Background(), &content.Input{}); !errors.Is(err, ErrInput) {
		t.Errorf("Observe with no schema = %v", err)
	}

	block := pzBlock()
	block.Source = spec.SourceModrinth
	block.Activation.Resolver = ""
	sc, err := content.Compile(block, spec.ContentList{}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, err := s.Observe(context.Background(), &content.Input{Schema: sc}); !errors.Is(err,
		ErrInput) {
		t.Errorf("a modrinth block was observed by the workshop source: %v", err)
	}
}

// TestSteamCMDItemIsRefusedByName: workshop_download_item needs the host-wide
// steamcmd lock, the anonymous-UGC opt-in check and the "succeeded but produced
// nothing" guard, none of which this build has. A plan that claimed to acquire
// and did not would be worse than a refusal that names the gap.
func TestSteamCMDItemIsRefusedByName(t *testing.T) {
	block := pzBlock()
	block.Acquisition.Mechanism = spec.AcquireSteamCMDItem
	sc, err := content.Compile(block, spec.ContentList{}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	_, err = offlineSource(t).Observe(context.Background(), &content.Input{Schema: sc})
	if !errors.Is(err, ErrMechanism) {
		t.Fatalf("error = %v, want ErrMechanism", err)
	}
	for _, want := range []string{spec.AcquireSteamCMDItem, spec.AcquireServerBoot} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestObserveCarriesAnAbsentConfigAsItsOwnState.
//
// Three states, not two. ConfigRead stays TRUE for an absent file on purpose:
// planChanges early-returns nil on !ConfigRead, so collapsing the two would
// give a brand-new instance zero changes, sync would see nothing to do, and the
// first WorkshopItems= would never be written at all. ConfigAbsent is the flag
// a report consults before it says anything about what the live config enables.
func TestObserveCarriesAnAbsentConfigAsItsOwnState(t *testing.T) {
	for name, tc := range map[string]struct {
		live       *content.Live
		wantRead   bool
		wantAbsent bool
	}{
		"nobody looked": {nil, false, false},
		"looked and the file is not there": {
			&content.Live{Absent: true}, true, true,
		},
		"looked and the list is empty": {
			&content.Live{}, true, false,
		},
		"looked and the list has entries": {
			&content.Live{Acquisition: "3171167894", Activation: "ThatDamnLibrary"}, true, false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := input(t, spec.ContentList{}, tc.live, workshopFS())
			obs, err := offlineSource(t).Observe(context.Background(), in)
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if obs.ConfigRead != tc.wantRead {
				t.Errorf("ConfigRead = %t, want %t", obs.ConfigRead, tc.wantRead)
			}
			if obs.ConfigAbsent != tc.wantAbsent {
				t.Errorf("ConfigAbsent = %t, want %t", obs.ConfigAbsent, tc.wantAbsent)
			}
		})
	}
}
