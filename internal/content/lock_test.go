package content

import (
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// updateGolden rewrites the golden lockfile instead of comparing against it.
// Deliberately a flag and not an env var: regenerating a pinned on-disk shape
// is something a person should have typed on purpose.
var updateGolden = flag.Bool("update", false, "rewrite the golden lockfile")

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// lockedPZ is a lock as Materialize would have written it.
func lockedPZ(t *testing.T, sc *Schema) *Lock {
	t.Helper()
	l := NewLock(&Input{
		Instance:       "pz-main",
		Game:           "projectzomboid",
		ManifestSHA256: "1b40c9de5f4a2c8e0b6d7f1a3c5e7b9d1f3a5c7e9b1d3f5a7c9e1b3d5f7a9c1e",
		Clock:          fixedClock(time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC)),
	})
	sl, err := goodPlan().Lock(sc)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := l.Put(sc.Block, sl); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return l
}

// TestVerbatimActivationReturnsTheRecordedLineUntouched is the adoption
// switch, and the operator-edit switch. Both are the same property: the
// recorded line is the only copy of a decision gvm cannot reconstruct.
func TestVerbatimActivationReturnsTheRecordedLineUntouched(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{
		ActivationPolicy: spec.ActivationPreserve,
	})
	l := lockedPZ(t, sc)
	sl, err := l.SourceFor(sc.Block)
	if err != nil {
		t.Fatal(err)
	}

	// A hand-pruned list: the operator dropped MiniHealthPanelB41 themselves,
	// kept an id gvm no longer resolves, and left their own spacing in.
	recorded := "ThatDamnLibrary;NeatUIFramework;MiniHealthPanel;SomeOldModGvmCannotSee"
	sl.VerbatimActivation = true
	sl.ActivationLine = recorded

	a, err := Activate(sc, l)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got := a.Activate.Value(); got != recorded {
		t.Fatalf("Activate re-derived the activation line.\n got %q\nwant %q", got, recorded)
	}
	if !a.Verbatim() {
		t.Error("the returned line does not report itself as verbatim")
	}
	if !strings.Contains(a.Note, "untouched") {
		t.Errorf("the note does not say the line was preserved: %q", a.Note)
	}
	// The acquisition half is still gvm's: it owns which items the server
	// fetches, including the ones not downloaded yet.
	if got := a.Acquisition.Value(); got != "3171167894;3508537032;2866258937" {
		t.Errorf("the acquisition line should still be derived, got %q", got)
	}

	// And Derive REFUSES, rather than silently producing the resolved list.
	_, derr := Derive(sc, l)
	if !errors.Is(derr, ErrVerbatim) {
		t.Fatalf("Derive re-derived a verbatim list: %v", derr)
	}
	if !strings.Contains(derr.Error(), "promote") {
		t.Errorf("the refusal does not name the way out (`gvm content promote`): %v", derr)
	}
}

// TestVerbatimWithNothingRecordedRefuses is the worst thing this package could
// do, closed off: "preserve" with an empty recorded line is not permission to
// write an empty Mods= onto an adopted server.
func TestVerbatimWithNothingRecordedRefuses(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{
		ActivationPolicy: spec.ActivationPreserve,
	})
	l := lockedPZ(t, sc)
	sl, _ := l.SourceFor(sc.Block)
	sl.VerbatimActivation = true
	sl.ActivationLine = "   "

	_, err := Activate(sc, l)
	if err == nil {
		t.Fatal("Activate returned an empty verbatim line; writing it disables every mod, and " +
			"PZ rewrites the ini on shutdown so it becomes permanent")
	}
	if !errors.Is(err, ErrInvalidLock) {
		t.Errorf("error does not wrap ErrInvalidLock: %v", err)
	}
	if !strings.Contains(err.Error(), "import") {
		t.Errorf("the refusal does not name the fix: %v", err)
	}
}

func TestDeriveProducesTheResolvedLine(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	l := lockedPZ(t, sc)

	line, err := Derive(sc, l)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if line.Verbatim() {
		t.Error("a derived line reports itself as verbatim")
	}
	want := "ThatDamnLibrary;NeatUIFramework;MiniHealthPanel;MiniHealthPanelB41"
	if got := line.Value(); got != want {
		t.Errorf("derived line = %q, want %q", got, want)
	}
	if line.Key != "Mods" {
		t.Errorf("derived line carries key %q", line.Key)
	}
}

func TestLockRoundTripsThroughJSON(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	l := lockedPZ(t, sc)

	data, err := Marshal(l)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	again, err := Marshal(back)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if string(again) != string(data) {
		t.Errorf("the lock does not round trip byte-identically:\n%s\n---\n%s", data, again)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("the lock has no trailing newline, so every later diff shows a modified last line")
	}

	sl, err := back.SourceFor("workshop")
	if err != nil {
		t.Fatal(err)
	}
	if len(sl.Entries) != 3 {
		t.Fatalf("decoded %d entries", len(sl.Entries))
	}
	if got := sl.Entries[2].ActivationIDs; len(got) != 2 {
		t.Errorf("activation_ids decoded as %v; one item holding two mods must survive the "+
			"round trip", got)
	}
}

// TestLockJSONUsesTheDocumentedFieldNames pins the on-disk shape. The lockfile
// goes inside every backup tarball's manifest.json and is meant to be pasted
// into a support thread, so a renamed field is a compatibility break with
// archives already on a shelf.
func TestLockJSONUsesTheDocumentedFieldNames(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	data, err := Marshal(lockedPZ(t, sc))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		`"version": 1`,
		`"instance": "pz-main"`,
		`"game": "projectzomboid"`,
		`"manifest_sha256"`,
		`"generated_at"`,
		`"gvm_version"`,
		`"sources"`,
		`"kind": "steam_workshop"`,
		`"order_significant": true`,
		`"verbatim_activation": false`,
		`"acquisition_line"`,
		`"activation_line"`,
		`"entries"`,
		`"ordinal"`,
		`"acquire_id"`,
		`"activation_ids"`,
		`"resolution"`,
		`"evidence"`,
		`"ambiguous": true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the lock JSON is missing %s:\n%s", want, got)
		}
	}
	// A note is documentation carried as data: "required by all KI5 vehicle
	// mods" is the only record of why an item sits at ordinal 0.
	if !strings.Contains(got, "KI5 vehicle mods") {
		t.Error("the operator's note did not survive into the lock")
	}
}

// TestLockGoldenFile keeps the whole document stable, not just its field
// names. Run with -update to refresh it after a deliberate change.
func TestLockGoldenFile(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	l := lockedPZ(t, sc)
	// The build stamp is a link-time value; pinning it would make this test
	// fail on every release rather than on a schema change.
	l.GVMVersion = "0.1.0"

	data, err := Marshal(l)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	golden := filepath.Join("testdata", "lock", "pz-main.golden.json")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read %s: %v", golden, err)
		}
		t.Fatalf("%s is missing; the lock shape is unpinned (run with -update)", golden)
	}
	if string(data) != string(want) {
		t.Errorf("the lock no longer matches %s.\n got:\n%s\nwant:\n%s", golden, data, want)
	}
}

func TestCheckManifestFailsWhenTheManifestMoved(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	l := lockedPZ(t, sc)

	if err := l.CheckManifest(l.ManifestSHA256); err != nil {
		t.Fatalf("CheckManifest against its own digest: %v", err)
	}
	if err := l.CheckManifest(strings.ToUpper(l.ManifestSHA256)); err != nil {
		t.Errorf("CheckManifest is case sensitive on a hex digest: %v", err)
	}
	err := l.CheckManifest("0000000000000000000000000000000000000000000000000000000000000000")
	if !errors.Is(err, ErrManifestMoved) {
		t.Fatalf("CheckManifest accepted a different manifest: %v", err)
	}
	if !strings.Contains(err.Error(), "lock --refresh") {
		t.Errorf("the refusal does not name the fix: %v", err)
	}

	l.ManifestSHA256 = ""
	if err := l.CheckManifest("abc"); !errors.Is(err, ErrManifestMoved) {
		t.Errorf("a lock with no recorded digest passed the check: %v", err)
	}
}

func TestValidateRefusesADuplicateIDOrOrdinal(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})

	t.Run("duplicate id", func(t *testing.T) {
		l := lockedPZ(t, sc)
		sl, _ := l.SourceFor(sc.Block)
		sl.Entries = append(sl.Entries, sl.Entries[0])
		sl.Entries[len(sl.Entries)-1].Ordinal = 99
		if err := sl.Validate(sc.Block, "", "", sc.Acquisition.KeepMissing); err == nil {
			t.Error("a lock recording one id twice validated")
		}
	})

	t.Run("duplicate ordinal", func(t *testing.T) {
		l := lockedPZ(t, sc)
		sl, _ := l.SourceFor(sc.Block)
		sl.Entries[1].Ordinal = sl.Entries[0].Ordinal
		err := sl.Validate(sc.Block, "", "", sc.Acquisition.KeepMissing)
		if err == nil {
			t.Fatal("two entries at one ordinal validated; load order is no longer a total " +
				"order, so \"libraries first\" is not expressible")
		}
		if !strings.Contains(err.Error(), "ordinal") {
			t.Errorf("error does not mention the ordinal: %v", err)
		}
	})
}

// TestValidateCrossChecksTheRecordedLines is the asymmetry that makes verbatim
// activation safe: gvm owns the acquisition line, the operator may own the
// activation one.
func TestValidateCrossChecksTheRecordedLines(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})

	t.Run("acquisition line is always gvm's", func(t *testing.T) {
		l := lockedPZ(t, sc)
		sl, _ := l.SourceFor(sc.Block)
		sl.AcquisitionLine = "3171167894"
		if err := sl.Validate(sc.Block, ";", ";", sc.Acquisition.KeepMissing); err == nil {
			t.Error("an acquisition line its own entries do not render validated")
		}
	})

	t.Run("activation line must match when deriving", func(t *testing.T) {
		l := lockedPZ(t, sc)
		sl, _ := l.SourceFor(sc.Block)
		sl.ActivationLine = "ThatDamnLibrary"
		err := sl.Validate(sc.Block, ";", ";", sc.Acquisition.KeepMissing)
		if err == nil {
			t.Fatal("a non-verbatim activation line that disagrees with the entries validated")
		}
		if !strings.Contains(err.Error(), "preserve") {
			t.Errorf("the refusal does not name the switch that would make it legal: %v", err)
		}
	})

	t.Run("activation line may differ when verbatim", func(t *testing.T) {
		l := lockedPZ(t, sc)
		sl, _ := l.SourceFor(sc.Block)
		sl.VerbatimActivation = true
		sl.ActivationLine = "ThatDamnLibrary;SomethingOnlyTheOperatorKnows"
		if err := sl.Validate(sc.Block, ";", ";", sc.Acquisition.KeepMissing); err != nil {
			t.Errorf("a verbatim line is EXPECTED to differ from the entries; validating it "+
				"against them would destroy the edit the switch protects: %v", err)
		}
	})
}

func TestValidateAgainstRefusesAPolicyMismatch(t *testing.T) {
	derive := mustCompile(t, pzBlock(), spec.ContentList{})
	l := lockedPZ(t, derive)
	sl, _ := l.SourceFor(derive.Block)
	sl.VerbatimActivation = true
	sl.ActivationLine = "ThatDamnLibrary"

	err := l.ValidateAgainst(derive)
	if err == nil {
		t.Fatal("a lock marked verbatim validated against an instance that derives; one of " +
			"the two has to be corrected deliberately")
	}
	if !strings.Contains(err.Error(), "promote") {
		t.Errorf("the refusal does not name `gvm content promote`: %v", err)
	}
}

func TestLockRefusesAFutureVersion(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	l := lockedPZ(t, sc)
	l.Version = LockVersion + 1

	data, err := Marshal(l)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := Unmarshal(data); !errors.Is(err, ErrInvalidLock) {
		t.Fatalf("a lock from a newer gvm decoded: %v; a field this build would ignore could "+
			"be the one that says not to re-derive a list", err)
	}
}

func TestSourceForNamesWhatIsMissing(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	l := lockedPZ(t, sc)

	_, err := l.SourceFor("mods")
	if err == nil {
		t.Fatal("SourceFor invented a block")
	}
	if !strings.Contains(err.Error(), "workshop") {
		t.Errorf("the error does not say which blocks the lock DOES have: %v", err)
	}
	if _, err := (*Lock)(nil).SourceFor("workshop"); err == nil {
		t.Error("SourceFor on a nil lock returned a source; a nil source lock read as \"no " +
			"mods\" would write an empty list")
	}
}

// TestOwningIsTheReverseMap is what `gvm content why` is built on: an operator
// holding a string id out of a live Mods= needs to know which item put it there.
func TestOwningIsTheReverseMap(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	sl, err := lockedPZ(t, sc).SourceFor(sc.Block)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := sl.Owning("MiniHealthPanelB41")
	if !ok {
		t.Fatal("Owning could not find the item behind MiniHealthPanelB41")
	}
	if e.Acquire != AcquireID("2866258937") {
		t.Errorf("Owning returned %s", e.Acquire)
	}
	if !e.Ambiguous {
		t.Error("the entry does not record that it was ambiguous, so `why` cannot explain it")
	}
	if _, ok := sl.Owning("NotAThing"); ok {
		t.Error("Owning invented an entry")
	}
}

func TestLoadReportsAMissingFileAsNotExist(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.lock.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load of a missing lock = %v; \"this instance has no content lock yet\" is a "+
			"normal state and callers distinguish it with errors.Is", err)
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pz-main.lock.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); !errors.Is(err, ErrInvalidLock) {
		t.Fatalf("Load accepted garbage: %v", err)
	}
}

func TestLockPathIsTheDecidedLocation(t *testing.T) {
	// D-03: intent lives under /etc/gvm, observed state does not.
	if got := LockPathFor("pz-main"); got != "/etc/gvm/instances/pz-main.lock.json" {
		t.Errorf("LockPathFor = %q, want the D-03 path", got)
	}
}

// TestAKeepMissingFalseLockValidatesAndRendersOneLine is the writer-validator
// agreement, parameterised over the one setting that broke it.
//
// Plan.Lock renders acquisition_line through the schema's keep_missing;
// SourceLock.Validate used to render its expectation with keepMissing hardcoded
// true. With keep_missing: false and anything unmaterialized the two disagree
// by construction, and Materialize calls ValidateAgainst on the lock it has
// just built -- so gvm refuses its own output and the message blames the
// operator for a hand edit that never happened. The setting defaults to false
// (a plain bool in the manifest grammar) so an operator-authored
// /etc/gvm/games/<id>.yaml that omits the key reaches this with no code change.
//
// The third assertion is the invariant that matters beyond the signature: the
// line Activate actually writes must equal the line the lock recorded.
func TestAKeepMissingFalseLockValidatesAndRendersOneLine(t *testing.T) {
	for name, tc := range map[string]struct {
		keepMissing  bool
		materialized []bool
		wantIDs      string
	}{
		"keep_missing true, one not downloaded": {
			true, []bool{true, true, false}, "3171167894;3508537032;2866258937",
		},
		"keep_missing false, all downloaded": {
			false, []bool{true, true, true}, "3171167894;3508537032;2866258937",
		},
		"keep_missing false, one not downloaded": {
			false, []bool{true, true, false}, "3171167894;3508537032",
		},
		// The fresh-instance case: nothing has downloaded yet.
		"keep_missing false, none downloaded": {
			false, []bool{false, false, false}, "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := pzBlock()
			b.Acquisition.KeepMissing = tc.keepMissing
			sc := mustCompile(t, b, spec.ContentList{})

			p := goodPlan()
			for i := range p.Entries {
				p.Entries[i].Materialized = tc.materialized[i]
			}
			sl, err := p.Lock(sc)
			if err != nil {
				t.Fatalf("Lock: %v", err)
			}
			if sl.AcquisitionLine != tc.wantIDs {
				t.Fatalf("recorded acquisition_line = %q, want %q", sl.AcquisitionLine,
					tc.wantIDs)
			}

			l := NewLock(&Input{Instance: "pz-main", Game: "projectzomboid"})
			if err := l.Put(sc.Block, sl); err != nil {
				t.Fatal(err)
			}
			if err := l.ValidateAgainst(sc); err != nil {
				t.Fatalf("gvm refuses the lock it just wrote: %v", err)
			}

			a, err := Activate(sc, l)
			if err != nil {
				t.Fatalf("Activate: %v", err)
			}
			if got := a.Acquisition.Value(); got != sl.AcquisitionLine {
				t.Errorf("Activate writes %q and the lock recorded %q; one line must have one "+
					"rendering", got, sl.AcquisitionLine)
			}
		})
	}
}

// TestAcquireIDsDropsOnTheRecordedMaterializedFlag pins the predicate
// unification.
//
// Plan.AcquireIDs drops on !Materialized. The lock's copy used to drop on
// "this entry resolved to no activation ids", which is a different question in
// both directions: a downloaded item whose every variant was Dropped has bytes
// and no ids, and an undownloaded item listed in mods.txt can carry ids and no
// bytes. So the lock records the fact rather than a proxy for it.
func TestAcquireIDsDropsOnTheRecordedMaterializedFlag(t *testing.T) {
	sl := &SourceLock{Kind: KindSteamWorkshop, Entries: []LockEntry{
		{Ordinal: 0, Acquire: "1", Materialized: true, ActivationIDs: []ActivationID{"A"}},
		// Downloaded, and every variant inside it lost the score comparison.
		// It has bytes, so it stays on the acquisition line.
		{Ordinal: 1, Acquire: "2", Materialized: true},
		// Not downloaded, but something recorded activation ids for it.
		{Ordinal: 2, Acquire: "3", ActivationIDs: []ActivationID{"C"}},
	}}
	if got := joinAcquire(sl.AcquireIDs(true), ";"); got != "1;2;3" {
		t.Errorf("keep_missing true = %q, want every id", got)
	}
	if got := joinAcquire(sl.AcquireIDs(false), ";"); got != "1;2" {
		t.Errorf("keep_missing false = %q, want the two with bytes on disk", got)
	}
}

// TestActivateRefusesALockWhoseEntriesDoNotRenderItsRecordedLines is the
// lock's half of the fail-closed gate, at the chokepoint.
//
// SourceLock.CheckAccounted carries only Unaccounted across the process
// boundary -- not the arithmetic half that Plan.CheckAccounted and
// Collection.CheckAccounted both have. A lock whose entries array was truncated
// while its recorded lines were left intact passes Unmarshal, passes
// CheckAccounted, and activates into a SHORT Mods=. The evidence that it used
// to be longer is sitting in the same file, in the two recorded lines, so
// Activate asks.
func TestActivateRefusesALockWhoseEntriesDoNotRenderItsRecordedLines(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	l := lockedPZ(t, sc)
	sl, err := l.SourceFor(sc.Block)
	if err != nil {
		t.Fatal(err)
	}

	// A hand edit, or a lock written by a build with a different entry shape:
	// the last entry goes and the recorded lines stay exactly as gvm wrote
	// them.
	sl.Entries = sl.Entries[:len(sl.Entries)-1]

	data, err := Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal refuses it already, so this test is no longer about Activate: %v",
			err)
	}
	if _, err := Activate(sc, reloaded); !errors.Is(err, ErrInvalidLock) {
		t.Fatalf("Activate = %v; a truncated lock activated into a short %s",
			err, sc.Activation.Key)
	}
	if _, err := Derive(sc, reloaded); !errors.Is(err, ErrInvalidLock) {
		t.Errorf("Derive = %v; it has the same one-condition gate", err)
	}
}
