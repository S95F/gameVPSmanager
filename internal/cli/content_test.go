package cli

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/content/fetch"
	"github.com/S95F/gameVPSmanager/internal/content/steamworkshop"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The content fixture is one Project Zomboid instance in the three states that
// make the three-way diff worth having at once:
//
//   - 3171167894 is declared, locked, downloaded and live. The healthy case.
//   - 2866258937 is the live ambiguous item: no build marker anywhere in it, so
//     both of its ids are enabled and flagged. This is the id section 12.5's
//     table names, and its evidence is what `gvm content why` prints.
//   - 3778511497 is declared and NEVER ACQUIRED: not in the lock, not on disk,
//     not in the live config.
//   - 3435796523 is in the LOCK and no longer declared: somebody removed it
//     from the list and has not synced.
//   - 3536052310 is declared and locked at a DIFFERENT ORDINAL, with a
//     requires: edge, which is the "the operator changed their mind" half that
//     a two-way diff cannot tell from drift.
//
// Everything is in the memFS and in an injected content root, so the whole
// thing runs on Windows: the only Linux-only thing a content verb touches is
// the host, and that arrives through HostEnv.

const (
	fxLibrary   = "3171167894" // that DAMN Library -- ok
	fxAmbiguous = "2866258937" // Mini Health Panel -- check, two ids
	fxNeverGot  = "3778511497" // declared, never acquired
	fxDelisted  = "3435796523" // locked, no longer declared
	fxMoved     = "3536052310" // declared at a different ordinal
)

type contentFixture struct {
	*testHost
	Data string
}

// newContentFixture builds the instance through `gvm create` rather than
// hand-writing its YAML, so the manifest pin is the one create actually writes
// and the test cannot drift from it.
func newContentFixture(t *testing.T) *contentFixture {
	t.Helper()
	h := newTestHost(t)
	seedInstance(t, h, "pz-main", "projectzomboid", "--var", "public_name=Test")

	in := mustReadInstance(t, h, "pz-main")
	in.Spec.Content = map[string]spec.ContentList{
		"workshop": {
			Source:     "steam_workshop",
			Collection: "3374983339",
			Acquire: []content.Ref{
				{ID: fxLibrary, Note: "that DAMN Library (required by all KI5 vehicle mods)"},
				{ID: fxAmbiguous, Note: "Mini Health Panel [B41/B42.20]"},
				{ID: fxMoved, Note: "KI5 Hilux", Requires: []string{fxLibrary}},
				{ID: fxNeverGot, Note: "Skully's Zombie Collision"},
			},
		},
	}
	body, err := spec.MarshalYAML(in)
	if err != nil {
		t.Fatal(err)
	}
	h.FS.put(h.HostEnv.paths("pz-main").Spec, body, 0o640)

	f := &contentFixture{testHost: h, Data: in.Spec.Paths.Data}

	// The server's own ini, as the game would have written it. The live Mods=
	// enables both halves of the ambiguous item and the library, and lists one
	// id (StaleLeftover) that nothing on this box produces -- which is the
	// "WorkshopItems= has 32 entries and Mods= has 33" discovery finding.
	h.FS.put(path.Join(f.Data, "Server", "servertest.ini"), []byte(
		"PublicName=Test\n"+
			"Mods=ThatDamnLibrary;MiniHealthPanel;MiniHealthPanelB41;StaleLeftover\n"+
			"WorkshopItems="+fxLibrary+";"+fxAmbiguous+"\n"), 0o640)

	// The workshop root, injected rather than read off disk: these are
	// absolute Linux paths and os.DirFS would resolve them against C: here.
	root := path.Join(f.Data, "steamapps/workshop/content/108600")
	tree := fstest.MapFS{
		fxLibrary + "/mods/ThatDamnLibrary/mod.info": &fstest.MapFile{
			Data: []byte("name=that DAMN Library\nid=ThatDamnLibrary\nversionMin=42.0.1\n")},
		// No build marker anywhere: scores 0 for both builds, so every id
		// inside it is enabled and the item is flagged `check`.
		fxAmbiguous + "/mods/MiniHealthPanel/mod.info": &fstest.MapFile{
			Data: []byte("name=Mini Health Panel\nid=MiniHealthPanel\n")},
		fxAmbiguous + "/mods/MiniHealthPanelB41/mod.info": &fstest.MapFile{
			Data: []byte("name=Mini Health Panel B41\nid=MiniHealthPanelB41\n")},
		fxMoved + "/mods/42/mod.info": &fstest.MapFile{
			Data: []byte("name=KI5 Hilux\nid=Hilux88\n")},
		fxMoved + "/mods/Hilux88B41/mod.info": &fstest.MapFile{
			Data: []byte("name=KI5 Hilux B41\nid=Hilux88B41\n")},
	}
	h.HostEnv.ContentRoot = func(abs string) (fs.FS, error) {
		if abs == root {
			return tree, nil
		}
		return nil, &fs.PathError{Op: "open", Path: abs, Err: fs.ErrNotExist}
	}
	return f
}

// TestFirstContentAddKeepsTopComment: the first `gvm content add` on a freshly
// created instance has no content block to edit, which used to trigger a full
// re-marshal that discarded every hand-written comment while reporting success
// (content-mods-4). The block is now GROWN inside the parsed document, so a
// top-of-file comment survives the first add.
func TestFirstContentAddKeepsTopComment(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "pz-main", "projectzomboid", "--var", "public_name=Test")
	// Roots do not exist yet; a missing one is recorded, not fatal, for an edit.
	h.HostEnv.ContentRoot = func(abs string) (fs.FS, error) {
		return nil, &fs.PathError{Op: "open", Path: abs, Err: fs.ErrNotExist}
	}

	p := h.HostEnv.paths("pz-main").Spec
	raw, err := h.FS.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	const note = "operator notes: load order is hand-tuned; do not reorder"
	h.FS.put(p, append([]byte("# "+note+"\n"), raw...), 0o640)

	code, _, errb := runWith(t, h, "", "content", "add", "pz-main", "3374983339")
	if code != 0 {
		t.Fatalf("content add exit %d: %s", code, errb)
	}

	got, err := h.FS.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), note) {
		t.Errorf("the top comment did not survive the first content add:\n%s", got)
	}
	if !strings.Contains(string(got), "3374983339") {
		t.Errorf("the added item is missing after the first add:\n%s", got)
	}
}

// writeLock lays down the recorded intent. It is built through the same
// structs the sync path writes, so a field renamed in internal/content breaks
// this fixture rather than silently producing a lock the CLI reads as empty.
func (f *contentFixture) writeLock(t *testing.T) {
	t.Helper()
	root := path.Join(f.Data, "steamapps/workshop/content/108600")
	sl := &content.SourceLock{
		Kind:             content.KindSteamWorkshop,
		OrderSignificant: true,
		Entries: []content.LockEntry{
			{
				Ordinal: 0, Source: content.KindSteamWorkshop, Acquire: fxLibrary,
				Note:          "that DAMN Library (required by all KI5 vehicle mods)",
				ActivationIDs: []content.ActivationID{"ThatDamnLibrary"},
				Resolution:    content.ResolutionServerBoot,
				Evidence:      []string{"modinfo:versionMin>=42", "tags:Build 42"},
				Upstream:      &content.Upstream{Title: "that DAMN Library"},
				Artifact: &content.Artifact{
					Kind: content.ArtifactTree, Path: path.Join(root, fxLibrary),
					Size: 48211934, Members: 412,
				},
			},
			{
				Ordinal: 1, Source: content.KindSteamWorkshop, Acquire: fxAmbiguous,
				Note:          "Mini Health Panel [B41/B42.20]",
				ActivationIDs: []content.ActivationID{"MiniHealthPanel", "MiniHealthPanelB41"},
				Resolution:    content.ResolutionServerBoot,
				Ambiguous:     true,
				Evidence: []string{
					"modinfo:no-build-marker", "tags:Build 41", "tags:Build 42",
				},
				Upstream: &content.Upstream{Title: "Mini Health Panel [B41/B42.20]"},
			},
			{
				Ordinal: 2, Source: content.KindSteamWorkshop, Acquire: fxDelisted,
				Note:          "KI5 Chevrolet",
				ActivationIDs: []content.ActivationID{"Chevy88"},
				Resolution:    content.ResolutionServerBoot,
			},
			{
				// Locked at ordinal 3 and declared at ordinal 2: the move a
				// two-way diff would read as drift.
				Ordinal: 3, Source: content.KindSteamWorkshop, Acquire: fxMoved,
				Note:          "KI5 Hilux",
				Requires:      []content.AcquireID{fxLibrary},
				ActivationIDs: []content.ActivationID{"Hilux88"},
				Dropped:       []content.ActivationID{"Hilux88B41"},
				Resolution:    content.ResolutionServerBoot,
				Evidence:      []string{"modinfo:42-dir"},
			},
		},
	}
	sl.AcquisitionLine = strings.Join(
		[]string{fxLibrary, fxAmbiguous, fxDelisted, fxMoved}, ";")
	sl.ActivationLine = "ThatDamnLibrary;MiniHealthPanel;MiniHealthPanelB41;Chevy88;Hilux88"

	l := &content.Lock{
		Version:        content.LockVersion,
		Instance:       "pz-main",
		Game:           "projectzomboid",
		ManifestSHA256: mustReadInstance(t, f.testHost, "pz-main").Spec.Manifest.SHA256,
		// Three days old, so a re-stamp is observable: the fixture and the
		// test clock being the same instant would make the assertion vacuous.
		GeneratedAt: testTime.Add(-72 * time.Hour),
		GVMVersion:  "0.1.0",
		Sources:     map[string]*content.SourceLock{"workshop": sl},
	}
	raw, err := content.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	f.FS.put(f.HostEnv.paths("pz-main").ContentLock, raw, 0o644)
}

func (f *contentFixture) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runWith(t, f.testHost, "", args...)
}

func TestContentDiffIsThreeWay(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	code, out, errb := f.run(t, "content", "diff", "pz-main")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}

	// The header: three documents, three counts. A diff that did not say how
	// many items each side has cannot be checked by the operator reading it.
	for _, want := range []string{
		"declared  /etc/gvm/instances/pz-main.yaml",
		"locked    /etc/gvm/instances/pz-main.lock.json",
		"observed  live config",
		"DECLARED vs LOCKED",
		"LOCKED vs OBSERVED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}

	// Declared vs locked: the two findings that are about INTENT.
	if !strings.Contains(out, "+ "+fxNeverGot) || !strings.Contains(out, "never acquired") {
		t.Errorf("the declared-but-never-acquired item is missing:\n%s", out)
	}
	if !strings.Contains(out, "- "+fxDelisted) || !strings.Contains(out, "no longer declared") {
		t.Errorf("the locked-but-undeclared item is missing:\n%s", out)
	}
	if !strings.Contains(out, "~ "+fxMoved) || !strings.Contains(out, "ordinal 3 -> 2") {
		t.Errorf("the ordinal move is missing:\n%s", out)
	}
	// And the move carries its requires: edge and whether the new position
	// honours it, because that is the only reason to care that it moved.
	if !strings.Contains(out, "requires "+fxLibrary+", satisfied") {
		t.Errorf("the moved item's requires: edge was not reported:\n%s", out)
	}

	// Locked vs observed: the findings that are about THE BOX.
	if !strings.Contains(out, "! "+fxDelisted) ||
		!strings.Contains(out, "does not enable Chevy88") {
		t.Errorf("the activation id missing from the live Mods= was not reported:\n%s", out)
	}
	if !strings.Contains(out, "ambiguous: MiniHealthPanel, MiniHealthPanelB41") {
		t.Errorf("the ambiguous item was not reported with both ids:\n%s", out)
	}
	if !strings.Contains(out, "evidence: modinfo:no-build-marker, tags:Build 41, tags:Build 42") {
		t.Errorf("the ambiguous item's evidence was not printed:\n%s", out)
	}
	if !strings.Contains(out, "StaleLeftover") {
		t.Errorf("the unlisted live id was not reported:\n%s", out)
	}
	if !strings.Contains(out, "impact: download_cycle") {
		t.Errorf("the impact line is wrong; one item is declared and not on disk:\n%s", out)
	}
	if !strings.Contains(out, "applying mod changes is not available in this build") {
		t.Errorf("the diff does not say applying is unavailable in this build:\n%s", out)
	}
}

// The three-way view's whole purpose, as one assertion: with NO lock, the
// declared half cannot be compared and the command says so instead of
// reporting every declared item as drift.
func TestContentDiffWithoutALockSaysSoRatherThanGuessing(t *testing.T) {
	f := newContentFixture(t)
	code, out, errb := f.run(t, "content", "diff", "pz-main")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "not written yet") {
		t.Errorf("the missing lock was not named:\n%s", out)
	}
	if !strings.Contains(out, "nothing is recorded as intent yet") {
		t.Errorf("the declared-vs-locked section guessed instead of refusing:\n%s", out)
	}
}

// TestContentWhyExplainsTheAmbiguousItem is section 12.9's `why` output: the
// answer to "why is this mod not loading", which is the most common Project
// Zomboid support question in the legacy README.
func TestContentWhyExplainsTheAmbiguousItem(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	code, out, errb := f.run(t, "content", "why", "pz-main", "MiniHealthPanelB41")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	for _, want := range []string{
		"MiniHealthPanelB41",
		"from steam_workshop item " + fxAmbiguous,
		`"Mini Health Panel [B41/B42.20]"`,
		"ordinal 1",
		"no build marker",
		"(status: check)",
		"sibling in the same item: MiniHealthPanel",
		"evidence: modinfo:no-build-marker  tags:Build 41  tags:Build 42",
		"live in Mods: yes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("why output does not contain %q:\n%s", want, out)
		}
	}
}

// The other half of the same question, and the one an operator actually hits:
// the id is NOT enabled, because it lost the build-variant comparison inside
// its own item. A command that only explained enabled ids would answer every
// question except this one.
func TestContentWhyExplainsADroppedVariant(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	code, out, errb := f.run(t, "content", "why", "pz-main", "Hilux88B41")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "dropped as a lower-scoring build variant") {
		t.Errorf("the dropped variant was not explained:\n%s", out)
	}
	if !strings.Contains(out, "NOT enabled") {
		t.Errorf("the output does not say the id is not enabled:\n%s", out)
	}
	if !strings.Contains(out, "the variant that won is Hilux88") {
		t.Errorf("the winning variant was not named:\n%s", out)
	}
	if !strings.Contains(out, "live in Mods: no") {
		t.Errorf("the live-config answer is wrong:\n%s", out)
	}
}

// An id live in Mods= that nothing on the box produces: the discovery finding,
// for one id. It exits non-zero because a script asking "what is this" needs an
// answer it can branch on, and the message names the namespace mistake that
// causes most of these.
func TestContentWhyRefusesAnIDNothingAccountsFor(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	code, out, _ := f.run(t, "content", "why", "pz-main", "StaleLeftover")
	if code == 0 {
		t.Fatal("an id nothing accounts for was reported as explained")
	}
	if !strings.Contains(out, "nothing on this box accounts for it") {
		t.Errorf("output = %s", out)
	}
	if !strings.Contains(out, "IS live in Mods") {
		t.Errorf("the output does not say the server is being told to enable it:\n%s", out)
	}
	if !strings.Contains(out, "ACTIVATION id") {
		t.Errorf("the output does not name the namespace mistake:\n%s", out)
	}
}

// `why` works with no lock at all, which is the state adoption is in: the
// observed reading answers from the files alone.
func TestContentWhyWorksBeforeAnythingIsLocked(t *testing.T) {
	f := newContentFixture(t)
	code, out, errb := f.run(t, "content", "why", "pz-main", "ThatDamnLibrary")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "from steam_workshop item "+fxLibrary) {
		t.Errorf("output = %s", out)
	}
	if !strings.Contains(out, "modinfo:versionMin>=42") {
		t.Errorf("the file evidence is missing:\n%s", out)
	}
}

func TestContentListShowsBothNamespaces(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	code, out, errb := f.run(t, "content", "list", "pz-main", "--observed")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	// The column headers are the manifest's own write keys, so the table says
	// which key each column lands in rather than inventing two words for them.
	if !strings.Contains(out, "WorkshopItems") || !strings.Contains(out, "Mods") {
		t.Errorf("the columns are not named after the write keys:\n%s", out)
	}
	if !strings.Contains(out, fxNeverGot) || !strings.Contains(out, "absent") {
		t.Errorf("the item that is not on disk is not reported as absent:\n%s", out)
	}
	if !strings.Contains(out, "StaleLeftover") {
		t.Errorf("the unlisted live id was not reported:\n%s", out)
	}
}

func TestContentListJSONCarriesTheEvidence(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	code, out, errb := f.run(t, "content", "list", "pz-main", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var rep struct {
		Declared int `json:"declared"`
		Items    []struct {
			AcquireID     string   `json:"acquire_id"`
			ActivationIDs []string `json:"activation_ids"`
			Ambiguous     bool     `json:"ambiguous"`
			Evidence      []string `json:"evidence"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("--json did not produce JSON: %v\n%s", err, out)
	}
	if rep.Declared != 4 {
		t.Errorf("declared = %d, want 4", rep.Declared)
	}
	for _, it := range rep.Items {
		if it.AcquireID != fxAmbiguous {
			continue
		}
		if !it.Ambiguous || len(it.ActivationIDs) != 2 || len(it.Evidence) != 3 {
			t.Errorf("the ambiguous item lost its flag, its ids or its evidence: %+v", it)
		}
		return
	}
	t.Errorf("the ambiguous item is not in the JSON:\n%s", out)
}

// `sync --dry-run` is the whole decision half: it resolves, prints section
// 12.6's ladder and writes nothing. Without --dry-run the command refuses and
// names the job, because the applying half holds the instance lock across a
// stop and a start.
func TestContentSyncDryRunDecidesAndWritesNothing(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)
	before := len(f.FS.writes)

	code, out, errb := f.run(t, "content", "sync", "pz-main", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if len(f.FS.writes) != before {
		t.Errorf("--dry-run wrote %v", f.FS.writes[before:])
	}
	if !strings.Contains(out, "nothing has been applied") {
		t.Errorf("output = %s", out)
	}
	for _, want := range []string{
		"write acquisition: WorkshopItems",
		"acquisition boot",
		"write activation: Mods",
		"post-start observe",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the ladder does not mention %q:\n%s", want, out)
		}
	}
}

func TestContentSyncWithoutDryRunRefusesAndNamesTheJob(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)
	before := len(f.FS.writes)

	code, _, errb := f.run(t, "content", "sync", "pz-main")
	if code == 0 {
		t.Fatal("the applying half of a sync reported success")
	}
	if len(f.FS.writes) != before {
		t.Errorf("a refused sync wrote %v", f.FS.writes[before:])
	}
	if !strings.Contains(errb, "--dry-run") {
		t.Errorf("the refusal does not name the half that works: %s", errb)
	}
}

func TestContentAddWritesTheListAndNothingElse(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	code, out, errb := f.run(t, "content", "add", "pz-main", "2392709985",
		"--after", fxLibrary, "--note", "Brita's Weapon Pack")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	in := mustReadInstance(t, f.testHost, "pz-main")
	got := in.Spec.Content["workshop"].Acquire
	if len(got) != 5 || got[1].ID != "2392709985" {
		t.Fatalf("the item did not land after %s: %+v", fxLibrary, got)
	}
	if got[1].Note != "Brita's Weapon Pack" {
		t.Errorf("the note was not recorded: %+v", got[1])
	}
	// Intent and nothing else: the ini is untouched and the output says so.
	for _, w := range f.FS.writes {
		if strings.HasSuffix(w, "servertest.ini") {
			t.Errorf("`content add` wrote the game's config: %v", f.FS.writes)
		}
	}
	if !strings.Contains(out, "Nothing was applied") {
		t.Errorf("output = %s", out)
	}
}

func TestContentAddRefusesAnIDAlreadyDeclared(t *testing.T) {
	f := newContentFixture(t)
	code, _, errb := f.run(t, "content", "add", "pz-main", fxLibrary)
	if code == 0 {
		t.Fatal("a duplicate acquisition id was accepted")
	}
	if !strings.Contains(errb, "already declared at ordinal 0") {
		t.Errorf("stderr = %s", errb)
	}
	if !strings.Contains(errb, "gvm content move") {
		t.Errorf("the refusal does not name the command that changes a position: %s", errb)
	}
}

func TestContentRemoveSaysTheActivationKeyStillEnablesIt(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	code, out, errb := f.run(t, "content", "remove", "pz-main", fxAmbiguous)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	in := mustReadInstance(t, f.testHost, "pz-main")
	if len(in.Spec.Content["workshop"].Acquire) != 3 {
		t.Fatalf("the item was not removed: %+v", in.Spec.Content["workshop"].Acquire)
	}
	// The gap between undeclaring and unloading is the thing an operator gets
	// wrong, so the command states it.
	if !strings.Contains(out, "Mods key still enables") {
		t.Errorf("output = %s", out)
	}
}

// `move` is the command CheckOrder's violations name as their fix, which is
// only true if it exists. It changes a position and nothing else.
func TestContentMoveChangesOnlyThePosition(t *testing.T) {
	f := newContentFixture(t)
	code, out, errb := f.run(t, "content", "move", "pz-main", fxMoved, "--first")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	in := mustReadInstance(t, f.testHost, "pz-main")
	got := in.Spec.Content["workshop"].Acquire
	if got[0].ID != fxMoved {
		t.Fatalf("the item did not move to the head: %+v", got)
	}
	if len(got) != 4 || got[0].Note != "KI5 Hilux" {
		t.Errorf("move changed more than the position: %+v", got)
	}
	if !strings.Contains(out, "ordinal 2 -> 0") {
		t.Errorf("output = %s", out)
	}
	// And the move broke its own requires: edge, which is reported rather than
	// prevented -- gvm never reorders on its own, in either direction.
	if !strings.Contains(out, "load-order violation") {
		t.Errorf("moving a dependent above its dependency was not reported:\n%s", out)
	}
	if !strings.Contains(out, "gvm content move pz-main "+fxLibrary+" --before "+fxMoved) {
		t.Errorf("the violation's fix is not a runnable command:\n%s", out)
	}
}

func TestContentLockCheckValidatesTheRecordedIntent(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	code, out, errb := f.run(t, "content", "lock", "pz-main", "--check")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errb, out)
	}
	if !strings.Contains(out, "the recorded lines render from the recorded entries") {
		t.Errorf("output = %s", out)
	}
	if !strings.Contains(out, "verbatim_activation: false") {
		t.Errorf("the activation policy was not stated:\n%s", out)
	}
}

// A lock whose recorded acquisition line does not render from its own entries
// has been edited by something other than gvm, and --check has to fail: the
// line is what gets written into the config, and the entries are what the
// report is computed from.
func TestContentLockCheckFailsOnAnEditedLine(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	p := f.HostEnv.paths("pz-main").ContentLock
	raw, err := f.FS.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw),
		`"acquisition_line": "`+fxLibrary+";", `"acquisition_line": "`, 1)
	if edited == string(raw) {
		t.Fatal("the fixture lock no longer contains the acquisition line")
	}
	f.FS.put(p, []byte(edited), 0o644)

	code, _, errb := f.run(t, "content", "lock", "pz-main", "--check")
	if code == 0 {
		t.Fatal("a hand-edited lock passed --check")
	}
	if !strings.Contains(errb, "edited by something other than gvm") {
		t.Errorf("stderr = %s", errb)
	}
}

func TestContentLockRefreshRecordsTheManifestAndNoList(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	before, err := f.FS.ReadFile(f.HostEnv.paths("pz-main").ContentLock)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errb := f.run(t, "content", "lock", "pz-main", "--refresh")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	after, err := f.FS.ReadFile(f.HostEnv.paths("pz-main").ContentLock)
	if err != nil {
		t.Fatal(err)
	}
	l, err := content.Unmarshal(after)
	if err != nil {
		t.Fatalf("the re-stamped lock does not parse: %v", err)
	}
	sl, err := l.SourceFor("workshop")
	if err != nil {
		t.Fatal(err)
	}
	if sl.ActivationLine != "ThatDamnLibrary;MiniHealthPanel;MiniHealthPanelB41;Chevy88;Hilux88" {
		t.Errorf("--refresh changed a list: %q", sl.ActivationLine)
	}
	if l.ManifestSHA256 != mustReadInstance(t, f.testHost, "pz-main").Spec.Manifest.SHA256 {
		t.Errorf("the pinned manifest's digest was not recorded: %q", l.ManifestSHA256)
	}
	if l.GeneratedAt.Equal(mustLockOf(t, before).GeneratedAt) {
		t.Errorf("--refresh did not re-stamp generated_at")
	}
	if !strings.Contains(out, "the two lists are unchanged") {
		t.Errorf("output = %s", out)
	}
}

func mustLockOf(t *testing.T, raw []byte) *content.Lock {
	t.Helper()
	l, err := content.Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestContentBlockedIsHonestAboutASteamWorkshopBlock(t *testing.T) {
	f := newContentFixture(t)
	code, out, errb := f.run(t, "content", "blocked", "pz-main")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "0 files require a manual download") {
		t.Errorf("output = %s", out)
	}
	// The distinction that matters: this is a fact about the SOURCE, not a
	// reading of a file that happened to be empty.
	if !strings.Contains(out, "no distribution opt-out") {
		t.Errorf("the reason is not stated:\n%s", out)
	}
}

func TestContentUpdateReportsIDsThatMoved(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	code, out, errb := f.run(t, "content", "update", "pz-main")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	// The lock records Hilux88 for the moved item and the files still produce
	// Hilux88, so nothing moved -- but the never-acquired item is not in the
	// lock at all, and that is reported.
	if !strings.Contains(out, "+ "+fxNeverGot) {
		t.Errorf("output = %s", out)
	}
	if !strings.Contains(out, "nothing has been applied") {
		t.Errorf("update implied it changed something:\n%s", out)
	}
}

func TestContentUpdateRefusesToPinAWorkshopItem(t *testing.T) {
	f := newContentFixture(t)
	code, _, errb := f.run(t, "content", "update", "pz-main", fxLibrary, "--to", "v2")
	if code == 0 {
		t.Fatal("--to was accepted for a Steam Workshop item")
	}
	if !strings.Contains(errb, "no per-version id") {
		t.Errorf("stderr = %s", errb)
	}
}

func TestContentCollectionRefusesWithoutADeclaredCollection(t *testing.T) {
	f := newContentFixture(t)
	in := mustReadInstance(t, f.testHost, "pz-main")
	list := in.Spec.Content["workshop"]
	list.Collection = ""
	in.Spec.Content["workshop"] = list
	body, err := spec.MarshalYAML(in)
	if err != nil {
		t.Fatal(err)
	}
	f.FS.put(f.HostEnv.paths("pz-main").Spec, body, 0o640)

	code, _, errb := f.run(t, "content", "collection", "sync", "pz-main")
	if code == 0 {
		t.Fatal("a collection sync ran with no collection declared")
	}
	if !strings.Contains(errb, "declares no collection") {
		t.Errorf("stderr = %s", errb)
	}
}

// --no-fetch makes no request at all, which is the answer on a box with no
// egress. It is a first-class mode and not an error path.
func TestContentCollectionNoFetchMakesNoRequest(t *testing.T) {
	f := newContentFixture(t)
	f.HostEnv.Workshop = func(steamworkshop.Config) (*steamworkshop.API, error) {
		t.Fatal("--no-fetch built an API client, which is a request waiting to happen")
		return nil, nil
	}
	code, out, errb := f.run(t, "content", "collection", "sync", "pz-main", "--no-fetch")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "no request was made") {
		t.Errorf("output = %s", out)
	}
}

// Off Linux, a content verb that needs the host fails with the project's one
// answer for that, and never with a half-done read of C:\etc.
func TestContentNeedsALinuxHost(t *testing.T) {
	if isLinux() {
		t.Skip("this asserts the refusal the development machine produces")
	}
	code, _, errb := runWith(t, nil, "", "content", "diff", "pz-main")
	if code == 0 {
		t.Fatal("a content verb reported success with no host")
	}
	if !strings.Contains(errb, "not supported") {
		t.Errorf("stderr = %s", errb)
	}
}

// TestContentOutputMatchesItsGolden is the whole rendered block, byte for byte.
//
// The Contains assertions above each pin one FACT the output has to carry; this
// pins the SHAPE. Section 12.9 specifies the diff and the why output as a
// layout -- the sigil, the id, the label, the evidence indented under it -- and
// a layout is the thing that silently rots when a report grows a field.
// Together they fail differently: a changed fact breaks one named assertion, a
// changed shape breaks this one and prints both versions.
//
// Nothing in these fixtures is clock-, path- or host-dependent, which is what
// lets them be golden on Windows: the instance paths are the ones `gvm create`
// computes, the lock carries a fixed timestamp, and the workshop root is an
// injected fstest.MapFS.
func TestContentOutputMatchesItsGolden(t *testing.T) {
	cases := []struct {
		golden string
		args   []string
	}{
		{"diff.golden", []string{"content", "diff", "pz-main"}},
		{"why-ambiguous.golden", []string{"content", "why", "pz-main", "MiniHealthPanelB41"}},
		{"why-dropped.golden", []string{"content", "why", "pz-main", "Hilux88B41"}},
		{"list.golden", []string{"content", "list", "pz-main", "--observed"}},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			f := newContentFixture(t)
			f.writeLock(t)
			code, out, errb := f.run(t, tc.args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errb)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "content", tc.golden))
			if err != nil {
				t.Fatal(err)
			}
			if out != string(want) {
				t.Errorf("output does not match testdata/content/%s\n--- got ---\n%s\n"+
					"--- want ---\n%s", tc.golden, out, want)
			}
		})
	}
}

// steamStub serves the two keyless endpoints on a loopback httptest server.
//
// NO TEST HERE TOUCHES THE REAL NETWORK, and the construction is what
// guarantees it rather than a convention: the client is built from
// fetch.Loopback("127.0.0.1"), which is the only policy constructor that
// permits plain HTTP and which refuses any host that is not a loopback literal
// or a reserved never-resolvable name. Reaching Steam would mean replacing the
// policy as well as the base URL.
type steamStub struct {
	srv   *httptest.Server
	calls int
}

func newSteamStub(t *testing.T, children []string, meta map[string]string) *steamStub {
	t.Helper()
	s := &steamStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ISteamRemoteStorage/GetCollectionDetails/v1/",
		func(w http.ResponseWriter, _ *http.Request) {
			s.calls++
			var kids []string
			for i, id := range children {
				kids = append(kids, fmt.Sprintf(
					`{"publishedfileid":%q,"sortorder":%d,"filetype":0}`, id, i))
			}
			writeStubJSON(w, fmt.Sprintf(
				`{"response":{"result":1,"resultcount":1,"collectiondetails":[`+
					`{"publishedfileid":"3374983339","result":1,"children":[%s]}]}}`,
				strings.Join(kids, ",")))
		})
	mux.HandleFunc("/ISteamRemoteStorage/GetPublishedFileDetails/v1/",
		func(w http.ResponseWriter, r *http.Request) {
			s.calls++
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			var out []string
			for i := 0; ; i++ {
				id := r.PostForm.Get(fmt.Sprintf("publishedfileids[%d]", i))
				if id == "" {
					break
				}
				// consumer_app_id is Project Zomboid's GAME appid. A child from
				// another game is ErrWrongGame, which the source's own tests
				// cover; here every child belongs.
				out = append(out, fmt.Sprintf(
					`{"publishedfileid":%q,"result":1,"consumer_app_id":108600,`+
						`"title":%q,"time_updated":1756000000,"file_size":"1024",`+
						`"tags":[{"tag":"Build 42"}]}`, id, meta[id]))
			}
			writeStubJSON(w, fmt.Sprintf(
				`{"response":{"result":1,"resultcount":%d,"publishedfiledetails":[%s]}}`,
				len(out), strings.Join(out, ",")))
		})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func writeStubJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write([]byte(body))
}

// seam points the fixture's Workshop client at the stub.
func (s *steamStub) seam(t *testing.T) func(steamworkshop.Config) (*steamworkshop.API, error) {
	t.Helper()
	return func(cfg steamworkshop.Config) (*steamworkshop.API, error) {
		p := fetch.Loopback("127.0.0.1")
		cfg.Policy = &p
		cfg.APIBase = s.srv.URL
		cfg.CommunityBase = s.srv.URL
		return steamworkshop.NewAPI(cfg)
	}
}

// TestContentCollectionSyncMergesOntoTheDeclaredSequence is rebuild.sh step 1,
// with the semantics internal/pz.ModsFile.SyncCollection taught: known items
// KEEP THEIR POSITION and their note, delisted items go, new items are
// APPENDED because a new item's load position is unknowable from a collection.
func TestContentCollectionSyncMergesOntoTheDeclaredSequence(t *testing.T) {
	f := newContentFixture(t)
	// The collection holds three of the four declared items plus one new one,
	// and drops fxNeverGot. The declared ORDER is deliberately not the
	// collection's, so "known items keep their position" is observable.
	stub := newSteamStub(t,
		[]string{fxMoved, fxLibrary, fxAmbiguous, "2392709985"},
		map[string]string{
			fxLibrary:    "that DAMN Library",
			fxAmbiguous:  "Mini Health Panel [B41/B42.20]",
			fxMoved:      "KI5 Hilux",
			"2392709985": "Brita's Weapon Pack",
		})
	f.HostEnv.Workshop = stub.seam(t)

	code, out, errb := f.run(t, "content", "collection", "sync", "pz-main")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errb, out)
	}
	got := mustReadInstance(t, f.testHost, "pz-main").Spec.Content["workshop"].Acquire
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	want := []string{fxLibrary, fxAmbiguous, fxMoved, "2392709985"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("merged list = %v, want %v", ids, want)
	}
	// The note on a known item is the operator's documentation and survives.
	if got[0].Note != "that DAMN Library (required by all KI5 vehicle mods)" {
		t.Errorf("a known item's note was replaced by the collection's title: %q", got[0].Note)
	}
	if got[3].Note != "Brita's Weapon Pack" {
		t.Errorf("the new item did not carry its title in as a note: %q", got[3].Note)
	}
	if !strings.Contains(out, "+ 2392709985") || !strings.Contains(out, "- "+fxNeverGot) {
		t.Errorf("the report does not show what changed:\n%s", out)
	}
	if !strings.Contains(out, "appended") {
		t.Errorf("the report does not say new items are appended rather than placed:\n%s", out)
	}
}

// --dry-run fetches and merges and writes nothing. The fetch is the point: a
// dry run that skipped it could not show what the collection says.
func TestContentCollectionSyncDryRunWritesNothing(t *testing.T) {
	f := newContentFixture(t)
	stub := newSteamStub(t, []string{fxLibrary}, map[string]string{fxLibrary: "that DAMN Library"})
	f.HostEnv.Workshop = stub.seam(t)
	before := len(f.FS.writes)

	code, out, errb := f.run(t, "content", "collection", "sync", "pz-main", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if len(f.FS.writes) != before {
		t.Errorf("--dry-run wrote %v", f.FS.writes[before:])
	}
	if stub.calls == 0 {
		t.Error("--dry-run made no request, so it cannot have compared anything")
	}
	if !strings.Contains(out, "nothing has been written (--dry-run)") {
		t.Errorf("output = %s", out)
	}
}

// result==9 on the collection is private-or-removed, and it is an ERROR rather
// than a fall-through to an empty list: an empty list is a valid thing for a
// collection to be, which is exactly why it cannot be the error value. The
// refusal names the flag that scrapes the page instead.
func TestContentCollectionSyncRefusesAPrivateCollection(t *testing.T) {
	f := newContentFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeStubJSON(w, `{"response":{"result":1,"resultcount":1,"collectiondetails":[`+
			`{"publishedfileid":"3374983339","result":9}]}}`)
	}))
	t.Cleanup(srv.Close)
	f.HostEnv.Workshop = func(cfg steamworkshop.Config) (*steamworkshop.API, error) {
		p := fetch.Loopback("127.0.0.1")
		cfg.Policy, cfg.APIBase, cfg.CommunityBase = &p, srv.URL, srv.URL
		return steamworkshop.NewAPI(cfg)
	}
	before := len(f.FS.writes)

	code, _, errb := f.run(t, "content", "collection", "sync", "pz-main")
	if code == 0 {
		t.Fatal("a private collection produced a rewrite")
	}
	if len(f.FS.writes) != before {
		t.Errorf("a refused collection sync wrote %v", f.FS.writes[before:])
	}
	if !strings.Contains(errb, "allow-html-fallback") {
		t.Errorf("the refusal does not name the fallback: %s", errb)
	}
}

// The list editors are pure, and these are the cases an operator hits: the
// position flags, a placement that names an id that is not there, and the
// refusal to accept two positions at once.
//
// Last-flag-wins is deliberately NOT the behaviour: `--first --after X` is
// somebody changing their mind mid-command, and guessing which half they meant
// is how a library ends up below the mod that needs it.
func TestContentListEditorsArePure(t *testing.T) {
	base := []content.Ref{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	cases := []struct {
		name  string
		place placement
		want  string
		err   string
	}{
		{name: "default is the tail", want: "a,b,c,new"},
		{name: "first", place: placement{First: true}, want: "new,a,b,c"},
		{name: "last", place: placement{Last: true}, want: "a,b,c,new"},
		{name: "after", place: placement{After: "a"}, want: "a,new,b,c"},
		{name: "before", place: placement{Before: "c"}, want: "a,b,new,c"},
		{name: "after an unknown id", place: placement{After: "zz"},
			err: "that id is not declared"},
		{name: "two positions", place: placement{First: true, Before: "c"},
			err: "name one position"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := addRefs(base, []content.Ref{{ID: "new"}}, tc.place)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if joined := refIDs(got); joined != tc.want {
				t.Errorf("list = %s, want %s", joined, tc.want)
			}
			// The input is never modified: every caller of these holds the
			// list it read off disk and writes it back on failure.
			if joined := refIDs(base); joined != "a,b,c" {
				t.Errorf("addRefs mutated its input: %s", joined)
			}
		})
	}

	if _, err := removeRefs(base, []string{"zz"}); err == nil {
		t.Error("removing an undeclared id was accepted")
	}
	got, err := removeRefs(base, []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	if joined := refIDs(got); joined != "a,c" {
		t.Errorf("remove = %s", joined)
	}

	if _, err := moveRef(base, "a", placement{}); err == nil {
		t.Error("a move with no position was accepted")
	}
	if _, err := moveRef(base, "a", placement{After: "a"}); err == nil {
		t.Error("an item was moved relative to itself")
	}
	moved, err := moveRef(base, "c", placement{First: true})
	if err != nil {
		t.Fatal(err)
	}
	if joined := refIDs(moved); joined != "c,a,b" {
		t.Errorf("move = %s", joined)
	}
}

func refIDs(refs []content.Ref) string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return strings.Join(out, ",")
}

// A game with no content block at all is refused by name rather than producing
// an empty report: `gvm content list valheim-1` has to say that Valheim
// declares no content sources, not "0 items".
func TestContentRefusesAGameWithNoContentBlock(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")

	code, _, errb := runWith(t, h, "", "content", "list", "valheim-1")
	if code == 0 {
		t.Fatal("a game with no content block produced a listing")
	}
	if !strings.Contains(errb, "declares no content sources") {
		t.Errorf("stderr = %s", errb)
	}
}

// An unread config and an empty one are the same bytes, and only one of them is
// a reason to write anything. Project Zomboid generates its own ini on first
// run, so "the file is not there yet" is the ordinary state of a fresh
// instance -- and reporting it as an empty Mods= is how adoption turns into an
// empty mod list.
func TestContentDiffDistinguishesAnAbsentConfigFromAnEmptyOne(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)
	if err := f.FS.Remove(path.Join(f.Data, "Server", "servertest.ini")); err != nil {
		t.Fatal(err)
	}

	code, out, errb := f.run(t, "content", "diff", "pz-main")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "has not generated its own config yet") {
		t.Errorf("the absent config was reported as an empty list:\n%s", out)
	}
	if !strings.Contains(out, "absent rather than empty") {
		t.Errorf("the distinction is not stated:\n%s", out)
	}
}

// The three flags the applying half honours are accepted (section 12.9
// documents them) and a decision-only run says it did not act on them. A flag
// silently accepted and ignored is worse than one that does not exist.
func TestContentSyncSaysWhichFlagsItDidNotActOn(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)
	code, out, errb := f.run(t, "content", "sync", "pz-main", "--dry-run", "--no-boot", "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "--no-boot and --yes") {
		t.Errorf("output = %s", out)
	}
	if !strings.Contains(out, "does not act on them") {
		t.Errorf("the note does not say the flags were inert:\n%s", out)
	}
}

// The alias and the section-selector, which are both surface an operator types
// and neither of which has a second implementation to drift from.
func TestContentAliasAndSectionSelector(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	// `gvm mods` is `gvm content`, which only means anything if the whole
	// subcommand tree is reachable through it.
	code, out, errb := f.run(t, "mods", "list", "pz-main")
	if code != 0 {
		t.Fatalf("the alias failed: exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "block workshop (steam_workshop)") {
		t.Errorf("mods list = %s", out)
	}

	// --against observed prints the drift half and not the intent half, which
	// is what an operator asking "what is the box doing" wants.
	code, out, errb = f.run(t, "content", "diff", "pz-main", "--against", "observed")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if strings.Contains(out, "DECLARED vs LOCKED") {
		t.Errorf("--against observed printed the declared section:\n%s", out)
	}
	if !strings.Contains(out, "LOCKED vs OBSERVED") {
		t.Errorf("--against observed dropped the section it names:\n%s", out)
	}

	if code, _, errb = f.run(t, "content", "diff", "pz-main", "--against", "sideways"); code == 0 {
		t.Error("an unknown --against value was accepted")
	} else if !strings.Contains(errb, "want declared, lock or observed") {
		t.Errorf("stderr = %s", errb)
	}

	// why --json carries the same finding the text output renders, so a
	// dashboard and a terminal cannot tell two stories about one mod.
	code, out, errb = f.run(t, "content", "why", "pz-main", "Hilux88B41", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var ans struct {
		Found    bool     `json:"found"`
		Enabled  bool     `json:"enabled"`
		Status   string   `json:"status"`
		Evidence []string `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("--json did not produce JSON: %v\n%s", err, out)
	}
	if !ans.Found || ans.Enabled || ans.Status != "dropped" || len(ans.Evidence) == 0 {
		t.Errorf("why --json = %+v", ans)
	}
}

// TestContentDiffRendersACompleteFixCommand covers the VIOLATED branch of
// requiresDetail, which had no test anywhere.
//
// OrderViolation.Fix() baked a literal `<inst>` placeholder, and exactly ONE of
// its two call sites substituted the instance name: printOrderFindings did a
// ReplaceAll over the whole report, and requiresDetail -- which feeds both the
// printed diff and the `--json` payload -- called Fix() bare. So an operator
// copy-pasted `gvm content move <inst> ...` and a dashboard got the same
// unusable string. One edge away from the committed golden: the healthy fixture
// already renders this row with "requires ..., satisfied".
func TestContentDiffRendersACompleteFixCommand(t *testing.T) {
	f := newContentFixture(t)
	f.writeLock(t)

	// Put the library AFTER the mod that requires it, which is the violation
	// this row exists to report, and keep the ordinal change that makes
	// requiresDetail run at all.
	in := mustReadInstance(t, f.testHost, "pz-main")
	list := in.Spec.Content["workshop"]
	list.Acquire = []content.Ref{
		{ID: fxAmbiguous, Note: "Mini Health Panel [B41/B42.20]"},
		{ID: fxMoved, Note: "KI5 Hilux", Requires: []string{fxLibrary}},
		{ID: fxLibrary, Note: "that DAMN Library (required by all KI5 vehicle mods)"},
		{ID: fxNeverGot, Note: "Skully's Zombie Collision"},
	}
	in.Spec.Content["workshop"] = list
	body, err := spec.MarshalYAML(in)
	if err != nil {
		t.Fatal(err)
	}
	f.FS.put(f.HostEnv.paths("pz-main").Spec, body, 0o640)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"text", []string{"content", "diff", "pz-main"}},
		{"json", []string{"content", "diff", "pz-main", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := f.run(t, tc.args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errb)
			}
			if !strings.Contains(out, "VIOLATED") {
				t.Fatalf("the violated branch did not render, so this test proves nothing:\n%s",
					out)
			}
			if strings.Contains(out, "<inst>") {
				t.Errorf("the fix carries a placeholder an operator has to edit before "+
					"running:\n%s", out)
			}
			want := fmt.Sprintf("gvm content move pz-main %s --before %s", fxLibrary, fxMoved)
			if !strings.Contains(out, want) {
				t.Errorf("output does not carry the complete command %q:\n%s", want, out)
			}
		})
	}
}

// TestNoGoldenCarriesAPlaceholder: the goldens are what stop the `<inst>`
// placeholder coming back, and the reason it survived is that no golden
// exercised the branch that emitted it.
func TestNoGoldenCarriesAPlaceholder(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "content"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join("testdata", "content", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "<inst>") {
			t.Errorf("%s contains a <inst> placeholder", e.Name())
		}
	}
}

// TestContentDiffJSONDistinguishesAnAbsentConfigFromAnEmptyOne is the machine
// half of the distinction its sibling above pins for human output, and it was
// the half that lied.
//
// contentView.live() returned a non-nil *content.Live whether or not the file
// existed, so Observe set ConfigRead = (Live != nil) = true unconditionally.
// Three consequences, all of them wrong: `config_absent` is `omitempty`, so it
// vanished from the payload and a dashboard read false; the !ConfigRead guard
// in lockedVsObserved -- whose own text says "That is NOT an empty mod list" --
// was dead code; and the section emitted four affirmative "Mods does not enable
// X" rows about a file that does not exist.
//
// Both directions are asserted, because the two states have to be pinned APART:
// an absent ini and an ini with an empty Mods= must not report the same thing.
func TestContentDiffJSONDistinguishesAnAbsentConfigFromAnEmptyOne(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		f := newContentFixture(t)
		f.writeLock(t)
		if err := f.FS.Remove(path.Join(f.Data, "Server", "servertest.ini")); err != nil {
			t.Fatal(err)
		}
		code, out, errb := f.run(t, "content", "diff", "pz-main", "--json")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errb)
		}
		var got struct {
			ConfigAbsent     bool `json:"config_absent"`
			LockedVsObserved []struct {
				Sigil string `json:"sigil"`
				What  string `json:"what"`
			} `json:"locked_vs_observed"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, out)
		}
		if !got.ConfigAbsent {
			t.Error("config_absent is false or missing for a config that is not there")
		}
		if len(got.LockedVsObserved) != 1 {
			t.Fatalf("locked_vs_observed has %d row(s), want the one honest row: %+v",
				len(got.LockedVsObserved), got.LockedVsObserved)
		}
		row := got.LockedVsObserved[0]
		if row.Sigil != "!" || !strings.Contains(row.What, "NOT an empty mod list") {
			t.Errorf("row = %+v", row)
		}
		for _, r := range got.LockedVsObserved {
			if strings.Contains(r.What, "does not enable") {
				t.Errorf("a drift row asserts a fact about a file that does not exist: %+v", r)
			}
		}
	})

	// The inverse: the file exists and its Mods= is empty. That IS an empty
	// mod list, config_absent stays false, and the per-id drift rows are the
	// correct answer.
	t.Run("present and empty", func(t *testing.T) {
		f := newContentFixture(t)
		f.writeLock(t)
		f.FS.put(path.Join(f.Data, "Server", "servertest.ini"), []byte(
			"PublicName=Test\nMods=\nWorkshopItems=\n"), 0o640)

		code, out, errb := f.run(t, "content", "diff", "pz-main", "--json")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errb)
		}
		var got struct {
			ConfigAbsent     bool `json:"config_absent"`
			LockedVsObserved []struct {
				What string `json:"what"`
			} `json:"locked_vs_observed"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, out)
		}
		if got.ConfigAbsent {
			t.Error("config_absent is true for a config that exists and says nothing is enabled")
		}
		var drift int
		for _, r := range got.LockedVsObserved {
			if strings.Contains(r.What, "does not enable") {
				drift++
			}
		}
		if drift == 0 {
			t.Errorf("an empty Mods= produced no drift rows, so a real empty list now reads "+
				"like an unread one: %+v", got.LockedVsObserved)
		}
	})
}

// commentTheList inserts the kind of head comments ARCHITECTURE 6.1's example
// instance YAML carries, so a write path can be asked whether it keeps them.
//
// Done by editing the fixture's own bytes rather than by shipping a second
// YAML fixture: newContentFixture builds the file through spec.MarshalYAML, so
// the file has no comments for any write path to lose, which is exactly why
// four verbs could destroy them with the suite green.
func (f *contentFixture) commentTheList(t *testing.T) {
	t.Helper()
	p := f.HostEnv.paths("pz-main").Spec
	raw, err := f.FS.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	var out []string
	for _, line := range lines {
		switch {
		case strings.Contains(line, `- id: "`+fxLibrary+`"`):
			out = append(out, "        # --- Libraries / frameworks (must load first) ---")
		case strings.Contains(line, `- id: "`+fxMoved+`"`):
			out = append(out, "        # --- Vehicles ---")
		}
		out = append(out, line)
	}
	body := strings.Join(out, "\n")
	for _, want := range []string{"Libraries / frameworks", "--- Vehicles ---"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the fixture's YAML no longer has the line this helper anchors on (%q)",
				want)
		}
	}
	f.FS.put(p, []byte(body), 0o640)
}

// TestAListEditKeepsTheOperatorsSectionComments.
//
// ARCHITECTURE 6.1 says the instance YAML "is read and written through
// gopkg.in/yaml.v3's Node API so head comments survive an automated collection
// sync, exactly as mods.txt's section headers do today", and the example it
// says that under carries exactly these `# --- Libraries ... ---` headers.
// §12.5 adds that a collection sync appends new items under a dated head
// comment, and Merge's own doc comment defers that to "the writer's job".
//
// What the writer did instead was re-marshal the decoded struct, so all four
// list verbs silently destroyed every comment in the file -- the only place in
// the tree that re-serialises a document gvm did not author (`gvm apply` writes
// the file's own bytes back, and D-04 writes the pinned manifest verbatim).
// The one `.bak` hostops keeps meant the comments survived exactly one further
// edit, and collection sync is both the verb most likely to run twice and the
// verb the spec specifically names.
func TestAListEditKeepsTheOperatorsSectionComments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"add", []string{"content", "add", "pz-main", "2392709985", "--note", "Brita"}},
		{"remove", []string{"content", "remove", "pz-main", fxNeverGot}},
		{"move", []string{"content", "move", "pz-main", fxMoved, "--before", fxAmbiguous}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContentFixture(t)
			f.commentTheList(t)
			before, err := f.FS.ReadFile(f.HostEnv.paths("pz-main").Spec)
			if err != nil {
				t.Fatal(err)
			}

			code, _, errb := f.run(t, tc.args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errb)
			}
			after, err := f.FS.ReadFile(f.HostEnv.paths("pz-main").Spec)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"# --- Libraries / frameworks (must load first) ---",
				"# --- Vehicles ---",
			} {
				if !strings.Contains(string(after), want) {
					t.Errorf("%q is gone from the written file:\n%s", want, after)
				}
			}
			// Everything outside the acquire sequence is untouched, byte for
			// byte: a surgical edit that reflowed the rest of the document
			// would make every later `gvm diff` unreadable.
			if gotHead, wantHead := head(string(after)), head(string(before)); gotHead != wantHead {
				t.Errorf("the document above the content block changed.\n got %q\nwant %q",
					gotHead, wantHead)
			}
		})
	}
}

// head is everything up to the content block, which no list edit may touch.
func head(s string) string {
	if i := strings.Index(s, "  content:"); i >= 0 {
		return s[:i]
	}
	return s
}

// TestACollectionSyncAppendsUnderADatedHeadComment is §12.5's other half, and
// the one Merge's doc comment names as the writer's job: "the dated head
// comment the bash wrote above the new block is the writer's job ...
// MergeResult.Added is what that comment is rendered from".
func TestACollectionSyncAppendsUnderADatedHeadComment(t *testing.T) {
	f := newContentFixture(t)
	f.commentTheList(t)
	// The collection drops nothing and adds one item, so there is an appended
	// block for the header to sit above.
	stub := newSteamStub(t, []string{fxLibrary, fxAmbiguous, fxMoved, fxNeverGot, "2392709985"},
		map[string]string{"2392709985": "Brita's Weapon Pack"})
	f.HostEnv.Workshop = stub.seam(t)

	code, out, errb := f.run(t, "content", "collection", "sync", "pz-main")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errb, out)
	}
	after, err := f.FS.ReadFile(f.HostEnv.paths("pz-main").Spec)
	if err != nil {
		t.Fatal(err)
	}
	body := string(after)
	for _, want := range []string{
		"# --- Libraries / frameworks (must load first) ---",
		"# --- Vehicles ---",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%q is gone after a collection sync, which is the verb the spec names:\n%s",
				want, body)
		}
	}
	if !strings.Contains(body, "collection 3374983339") {
		t.Errorf("the appended block carries no head comment naming the collection:\n%s", body)
	}
	if !strings.Contains(body, f.HostEnv.now().UTC().Format("2006-01-02")) {
		t.Errorf("the appended block's head comment is not dated:\n%s", body)
	}
}

// TestARemovedItemTakesItsOwnCommentWithIt.
//
// The comments are re-attached by acquisition id and not by position, which is
// what makes a removal and a move both read correctly: a removed item's header
// goes with it instead of being left stranded above whatever moved up into its
// place, and a moved item carries its own comment along with the line.
func TestARemovedItemTakesItsOwnCommentWithIt(t *testing.T) {
	f := newContentFixture(t)
	f.commentTheList(t)

	// fxMoved is the item the "--- Vehicles ---" header sits above.
	code, _, errb := f.run(t, "content", "remove", "pz-main", fxMoved)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	body, err := f.FS.ReadFile(f.HostEnv.paths("pz-main").Spec)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if strings.Contains(got, "# --- Vehicles ---") {
		t.Errorf("the removed item's header was left behind, now labelling a mod it does not "+
			"describe:\n%s", got)
	}
	// The other header, on an item that survived, is still there.
	if !strings.Contains(got, "# --- Libraries / frameworks (must load first) ---") {
		t.Errorf("a surviving item lost its header:\n%s", got)
	}
}
