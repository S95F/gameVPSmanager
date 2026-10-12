package update

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/provider"
)

// TestBuildDiffReadsThePlanRatherThanItsProse.
//
// Four plan shapes, and the two EMPTY ones are the reason this function exists.
// A provider's Reason carries both build ids inside an English sentence, and a
// pipeline that had to read that sentence to tell "current, nothing to do" from
// "stale, and this instance is adopted" would be a pipeline whose correctness
// depended on wording.
func TestBuildDiffReadsThePlanRatherThanItsProse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		plan    *provider.Plan
		changed bool
		from    string
		to      string
	}{
		{
			// The two ids are carried even on a no-op night, so the job log
			// says which pair produced it. "Nothing to do" with no numbers
			// beside it for a month is how a broken check hides.
			name: "current",
			plan: &provider.Plan{
				Intent:  provider.IntentUpdate,
				Reason:  "build 18452901 is current on branch public",
				Current: provider.Version{ID: "18452901"},
				Target:  provider.Version{ID: "18452901"},
			},
			from: "18452901", to: "18452901",
		},
		{
			name: "stale and managed",
			plan: &provider.Plan{
				Intent:    provider.IntentUpdate,
				Current:   provider.Version{ID: "18452901"},
				Target:    provider.Version{ID: "18461233"},
				Artifacts: []provider.Artifact{{Digest: "buildid:380870:18461233", DestRel: "."}},
			},
			changed: true, from: "18452901", to: "18461233",
		},
		{
			// The adopted case: an EMPTY plan that must still read as "this
			// axis wants work". Whether gvm is ALLOWED to do that work is
			// jobs.Decide's question, not this one's.
			name: "stale and adopted",
			plan: &provider.Plan{
				Intent:          provider.IntentUpdate,
				Current:         provider.Version{ID: "18452901"},
				Target:          provider.Version{ID: "18461233"},
				UpdateAvailable: true,
			},
			changed: true, from: "18452901", to: "18461233",
		},
		{
			name: "no plan at all",
			plan: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := BuildDiff(tc.plan)
			if d.Changed != tc.changed {
				t.Errorf("changed = %v, want %v", d.Changed, tc.changed)
			}
			if d.From != tc.from || d.To != tc.to {
				t.Errorf("from/to = %q/%q, want %q/%q", d.From, d.To, tc.from, tc.to)
			}
			if !d.Changed && tc.plan != nil && d.Note == "" {
				t.Error("a no-op night carries no reason; `nothing to do` with nothing beside " +
					"it is indistinguishable from a check that broke")
			}
		})
	}
}

// TestContentDiffFailsClosedOnAnUnaccountedItem.
//
// rebuild.sh lines 77-80, generalised. That script counted the ids it parsed
// out of a Steam collection page against a count taken from the raw HTML and
// refused to rewrite mods.txt when the two disagreed, because a changed markup
// silently DROPS mods -- and a server that boots normally with four mods
// missing is worse than one that did not sync.
func TestContentDiffFailsClosedOnAnUnaccountedItem(t *testing.T) {
	_, err := ContentDiff(jobs.ContentPlan{
		Changed:     true,
		Unaccounted: []string{"2169435993", "2460154811"},
	})
	if !errors.Is(err, ErrUnaccounted) {
		t.Fatalf("err = %v, want ErrUnaccounted", err)
	}
	for _, want := range []string{"2169435993", "2460154811", "nothing was stopped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestABlockedFileIsNotAContentDiff.
//
// Section 13.5's CurseForge paragraph: a modpack whose author revoked
// third-party download rights must not cost downtime. So the axis reports no
// work and carries the sentence. A build diff on the same night still proceeds
// on its own merits -- the binary update is real work and has nothing to do
// with a file a human has to fetch by hand.
func TestABlockedFileIsNotAContentDiff(t *testing.T) {
	d, err := ContentDiff(jobs.ContentPlan{Changed: true, Blocked: []string{"238222"}})
	if err != nil {
		t.Fatalf("a blocked file is not an error: %v", err)
	}
	if d.Changed {
		t.Error("a blocked file was reported as content work, which would stop the server")
	}
	if !strings.Contains(d.Note, "238222") {
		t.Errorf("the note does not name the blocked item: %q", d.Note)
	}

	// And it does not suppress the build axis.
	dec := jobs.Decide(testInstance, jobs.Diff{Changed: true, From: "1", To: "2"}, d, true)
	if dec.Skip || !dec.ApplyBuild || !dec.Stop {
		t.Errorf("decision = %+v; a blocked mod must not cancel a real binary update", dec)
	}
}

// TestContentDiffCarriesTheItemsByName, so a degraded run says WHICH mod.
func TestContentDiffCarriesTheItemsByName(t *testing.T) {
	d, err := ContentDiff(jobs.ContentPlan{
		Changed: true,
		Added:   []string{"2169435993"},
		Removed: []string{"1299328280"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Changed {
		t.Fatal("a changed list was not reported as changed")
	}
	if len(d.Items) != 2 {
		t.Errorf("items = %v, want both the added and the removed id", d.Items)
	}
}

// TestTheTwoDiffsFeedTheDecisionTheJobLayerMakes.
//
// This package produces the INPUT to D-43's gate and never the gate. The table
// here is the round trip -- provider plan and content plan in, jobs.Decision
// out -- so that a change to either derivation that broke the decision breaks a
// test in the package that made the change.
func TestTheTwoDiffsFeedTheDecisionTheJobLayerMakes(t *testing.T) {
	cur := &provider.Plan{
		Intent: provider.IntentUpdate, Reason: "current",
		Current: provider.Version{ID: "18452901"}, Target: provider.Version{ID: "18452901"},
	}
	stale := &provider.Plan{
		Intent:    provider.IntentUpdate,
		Current:   provider.Version{ID: "18452901"},
		Target:    provider.Version{ID: "18461233"},
		Artifacts: []provider.Artifact{{Digest: "buildid:380870:18461233", DestRel: "."}},
	}
	adopted := &provider.Plan{
		Intent:          provider.IntentUpdate,
		Current:         provider.Version{ID: "18452901"},
		Target:          provider.Version{ID: "18461233"},
		UpdateAvailable: true,
	}
	quiet := jobs.ContentPlan{}
	moved := jobs.ContentPlan{Changed: true, Added: []string{"2169435993"}}

	for _, tc := range []struct {
		name    string
		plan    *provider.Plan
		content jobs.ContentPlan
		managed bool
		skip    bool
		stop    bool
		avail   bool
	}{
		{name: "quiet night", plan: cur, content: quiet, managed: true, skip: true},
		{name: "build moved", plan: stale, content: quiet, managed: true, stop: true},
		{name: "content moved", plan: cur, content: moved, managed: true, stop: true},
		{
			// The one that matters: never stopped, and reported.
			name: "adopted, build moved", plan: adopted, content: quiet, managed: false,
			skip: true, avail: true,
		},
		{
			// An adopted instance whose CONTENT moved still does content
			// work, and still is not stopped for it: Project Zomboid
			// downloads its own workshop items on boot.
			name: "adopted, content moved", plan: adopted, content: moved, managed: false,
			avail: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cd, err := ContentDiff(tc.content)
			if err != nil {
				t.Fatal(err)
			}
			dec := jobs.Decide(testInstance, BuildDiff(tc.plan), cd, tc.managed)
			if dec.Skip != tc.skip {
				t.Errorf("skip = %v, want %v (%s)", dec.Skip, tc.skip, dec.Reason)
			}
			if dec.Stop != tc.stop {
				t.Errorf("stop = %v, want %v (%s)", dec.Stop, tc.stop, dec.Reason)
			}
			if dec.UpdateAvailable != tc.avail {
				t.Errorf("update_available = %v, want %v", dec.UpdateAvailable, tc.avail)
			}
			if !tc.managed && dec.ApplyBuild {
				t.Error("an adopted instance was authorized to apply a binary update")
			}
		})
	}
}

// TestTheUpdatePipelineHoldsNothingThatCanStopAServer.
//
// The structural half of D-43. The comments in pipeline.go claim the pipeline
// cannot stop a server; this is what makes the claim checkable. Every member of
// Config is examined for a method that could actuate the unit, and for
// assignability to jobs.Supervisor -- so a future seam added here with a Stop
// on it fails this test by name rather than quietly moving the stop out from
// behind the running guard.
func TestTheUpdatePipelineHoldsNothingThatCanStopAServer(t *testing.T) {
	forbidden := []string{
		"Stop", "Start", "Restart", "Reload", "Kill", "Signal",
		"Mask", "Unmask", "Enable", "Disable",
	}
	super := reflect.TypeOf((*jobs.Supervisor)(nil)).Elem()

	ct := reflect.TypeOf(Config{})
	for i := range ct.NumField() {
		f := ct.Field(i)
		for _, typ := range []reflect.Type{f.Type, reflect.PointerTo(f.Type)} {
			if typ.Kind() == reflect.Interface && typ.Implements(super) {
				t.Errorf("Config.%s (%s) is a jobs.Supervisor; the stop belongs to the job "+
					"layer's step table, behind the running guard", f.Name, f.Type)
			}
			if typ.Kind() != reflect.Interface && typ.Implements(super) {
				t.Errorf("Config.%s (%s) implements jobs.Supervisor", f.Name, f.Type)
			}
			for _, name := range forbidden {
				if _, ok := typ.MethodByName(name); ok {
					t.Errorf("Config.%s (%s) has a %s method; internal/update executes phases "+
						"and must not be able to actuate the unit", f.Name, f.Type, name)
				}
			}
		}
	}

	// And the pipeline itself exposes none either, so a caller cannot reach a
	// stop through it.
	pt := reflect.TypeOf(&Pipeline{})
	for _, name := range forbidden {
		if _, ok := pt.MethodByName(name); ok {
			t.Errorf("(*Pipeline).%s exists", name)
		}
	}
}

// TestThePipelineIsWhatTheJobRunnerAsksFor, restated as a test so the failure
// has a sentence attached rather than being a compile error in a var block.
func TestThePipelineIsWhatTheJobRunnerAsksFor(t *testing.T) {
	var p any = New(Config{Instance: testInstance})
	if _, ok := p.(jobs.Updates); !ok {
		t.Fatal("*Pipeline no longer satisfies jobs.Updates, so the `update` runner cannot " +
			"drive it")
	}
}
