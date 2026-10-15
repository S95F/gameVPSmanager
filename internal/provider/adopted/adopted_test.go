package adopted

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

var at = time.Unix(1762000000, 0).UTC()

// ctxFor builds the live box's shape: an install tree under /opt and a world
// under /home, neither of them created by gvm.
func ctxFor(t *testing.T, populated bool) *provider.Ctx {
	t.Helper()
	base := t.TempDir()
	in := spec.Paths{
		Install: filepath.Join(base, "opt", "pzserver"),
		Data:    filepath.Join(base, "home", "pzserver", "Zomboid"),
		Home:    filepath.Join(base, "home", "pzserver"),
	}
	for _, d := range []string{in.Install, in.Data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if populated {
		f := filepath.Join(in.Install, "start-server.sh")
		if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := provider.NewPaths(in)
	if err != nil {
		t.Fatal(err)
	}
	return &provider.Ctx{
		Name:           "pz-main",
		Paths:          paths,
		ManagedInstall: false,
		Clock:          func() time.Time { return at },
	}
}

// Every intent, including verify. A read-only integrity check still needs a
// record of the expected bytes and this driver has none, so reporting
// "verified" would be a verdict off the absence of evidence -- the exact
// failure D-32 exists to prevent.
func TestPlanIsAlwaysEmptyAndNeverDestructive(t *testing.T) {
	p := New()
	c := ctxFor(t, true)
	for _, in := range provider.Intents() {
		t.Run(string(in), func(t *testing.T) {
			pl, err := p.Plan(context.Background(), c, in)
			if err != nil {
				t.Fatalf("Plan refused outright: %v", err)
			}
			if pl.Writes() {
				t.Errorf("Plan carries %d artifact(s)", len(pl.Artifacts))
			}
			if pl.Destructive {
				t.Error("Plan is destructive")
			}
			if pl.Bytes != 0 {
				t.Errorf("Plan asks for %d bytes of headroom", pl.Bytes)
			}
			if pl.Intent != in {
				t.Errorf("Plan reports intent %q for a %q request", pl.Intent, in)
			}
			// The operator's next move has to be in the sentence: adoption is
			// a state they can leave, and nothing else in the output says how.
			if !strings.Contains(pl.Reason, "not available in this build") {
				t.Errorf("the reason does not say how to change it: %q", pl.Reason)
			}
			if err := pl.Authorize(false); err != nil {
				t.Errorf("an empty plan was refused for an adopted instance: %v", err)
			}
		})
	}
}

// There is no field in an empty plan that could carry an app_update, and this
// is the other half: a plan that somehow carries one does not execute. That is
// what "refuses app_update and validate at the type level" means in practice.
func TestApplyRefusesAnythingThatWouldWrite(t *testing.T) {
	p := New()
	c := ctxFor(t, true)

	pl := &provider.Plan{
		Intent: provider.IntentUpdate,
		Reason: "buildid 18452901 -> 18461233",
		Artifacts: []provider.Artifact{{
			Digest:  "buildid:380870:18461233",
			Size:    1 << 30,
			Source:  "steamcmd:app_update 380870",
			DestRel: ".",
		}},
	}
	// Authorized against a managed instance, so the refusal under test is the
	// driver's own and not the adopted gate's.
	if err := pl.Authorize(true); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	managed := ctxFor(t, true)
	managed.ManagedInstall = true
	if _, err := p.Apply(context.Background(), managed, pl, provider.Progress{}); !errors.Is(err, provider.ErrRefused) {
		t.Fatalf("want ErrRefused, got %v", err)
	}

	// And the ordinary nightly: an authorized empty plan is a no-op that
	// reports no change, which is state `skipped` (D-43).
	empty := provider.EmptyPlan(provider.IntentUpdate, "pz-main is adopted")
	if err := empty.Authorize(false); err != nil {
		t.Fatal(err)
	}
	applied, err := p.Apply(context.Background(), c, empty, provider.Progress{})
	if err != nil {
		t.Fatalf("Apply of an empty plan: %v", err)
	}
	if applied.Changed {
		t.Error("Apply reported a change")
	}
	if applied.StartedAt != at || applied.EndedAt != at {
		t.Error("Apply did not use the injected clock")
	}
}

func TestInspectReportsPresenceWithoutClaimingAVerdict(t *testing.T) {
	p := New()

	got, err := p.Inspect(context.Background(), ctxFor(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Present {
		t.Error("a populated install tree reported absent")
	}
	// A sentinel verdict it cannot support would make an adopted install look
	// either broken or verified, and it is neither: it is somebody else's, and
	// working.
	if got.Sentinel.Declared {
		t.Error("the adopted driver claims a sentinel")
	}
	if got.Sentinel.OK() == nil {
		t.Error("an undeclared sentinel reported a pass")
	}
	if got.Integrity.Checked {
		t.Error("the adopted driver claims to have checked integrity")
	}
	if strings.TrimSpace(got.Integrity.Reason) == "" {
		t.Error("an unchecked integrity result must say why, or it reads as a clean one")
	}
	// Every file here is unmanaged; answering "all 48,000 of them" would bury
	// the case the field exists to surface.
	if len(got.Unmanaged) != 0 || len(got.HandEdits) != 0 {
		t.Error("the adopted driver listed files it has no record of")
	}
	// Nil, not a zero value: a zero Fingerprint reads as "zero files, zero
	// bytes", which is the catastrophe D-34 exists to detect.
	if got.Fingerprint != nil {
		t.Error("the adopted driver reported a fingerprint it did not take")
	}

	empty, err := p.Inspect(context.Background(), ctxFor(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Present {
		t.Error("an empty install tree reported present")
	}
}

func TestObserveNamesNoVersionAndSaysWhatItSees(t *testing.T) {
	p := New()

	got, err := p.Observe(context.Background(), ctxFor(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Healthy {
		t.Error("a populated adopted install reported unhealthy")
	}
	if got.Version.Known() {
		t.Error("the adopted driver invented a version")
	}
	if got.ReadAt != at {
		t.Errorf("ReadAt is %v, want the injected clock", got.ReadAt)
	}
	if strings.TrimSpace(got.Detail) == "" {
		t.Error("Observe said nothing")
	}

	empty, err := p.Observe(context.Background(), ctxFor(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Healthy {
		t.Error("an empty install reported healthy")
	}
	if !strings.Contains(empty.Detail, "will not fill it") {
		t.Errorf("the detail does not explain the empty tree: %q", empty.Detail)
	}
}

func TestKindIsTheSchemasSpelling(t *testing.T) {
	if got := New().Kind(); got != spec.ProviderAdopted {
		t.Errorf("Kind() is %q, want %q", got, spec.ProviderAdopted)
	}
	if _, ok := provider.Lookup(spec.ProviderAdopted); !ok {
		t.Error("the driver did not register itself")
	}
}
