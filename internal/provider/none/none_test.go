package none

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

// ctxFor builds a MANAGED instance: unlike `adopted`, this driver's instances
// are gvm's, and the only reason nothing is fetched is that the game has no
// installer.
func ctxFor(t *testing.T, populated bool) *provider.Ctx {
	t.Helper()
	base := t.TempDir()
	in := spec.Paths{
		Install: filepath.Join(base, "srv", "gvm", "custom-1", "install"),
		Data:    filepath.Join(base, "srv", "gvm", "custom-1", "data"),
		Home:    filepath.Join(base, "srv", "gvm", "custom-1"),
	}
	for _, d := range []string{in.Install, in.Data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if populated {
		if err := os.WriteFile(filepath.Join(in.Install, "server"), []byte("ELF"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := provider.NewPaths(in)
	if err != nil {
		t.Fatal(err)
	}
	return &provider.Ctx{
		Name:           "custom-1",
		Paths:          paths,
		ManagedInstall: true,
		Clock:          func() time.Time { return at },
	}
}

func TestPlanIsAlwaysEmptyAndSaysWhoseFilesTheseAre(t *testing.T) {
	p := New()
	c := ctxFor(t, true)
	for _, in := range provider.Intents() {
		t.Run(string(in), func(t *testing.T) {
			pl, err := p.Plan(context.Background(), c, in)
			if err != nil {
				t.Fatalf("Plan refused outright: %v", err)
			}
			if pl.Writes() || pl.Destructive || pl.Bytes != 0 {
				t.Errorf("a driver with no installer planned work: %+v", pl)
			}
			// The difference from `adopted` has to be visible: there the fix
			// is to promote the instance, here it is to put the files there.
			if !strings.Contains(pl.Reason, "no installer") {
				t.Errorf("the reason does not explain why nothing happens: %q", pl.Reason)
			}
			if !strings.Contains(pl.Reason, c.Paths.Install.Path()) {
				t.Errorf("the reason does not say where the files belong: %q", pl.Reason)
			}
			if err := pl.Authorize(true); err != nil {
				t.Errorf("an empty plan was refused: %v", err)
			}
		})
	}
}

func TestApplyRefusesArtifactsAndIsOtherwiseANoOp(t *testing.T) {
	p := New()
	c := ctxFor(t, true)

	pl := &provider.Plan{
		Intent: provider.IntentInstall,
		Reason: "install",
		Artifacts: []provider.Artifact{{
			Digest:  provider.Digest("sha256:" + strings.Repeat("ab", 32)),
			Size:    12,
			Source:  "https://example.invalid/server.tar.gz",
			DestRel: "server",
		}},
	}
	if err := pl.Authorize(true); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if _, err := p.Apply(context.Background(), c, pl, provider.Progress{}); !errors.Is(err, provider.ErrRefused) {
		t.Fatalf("want ErrRefused, got %v", err)
	}

	empty := provider.EmptyPlan(provider.IntentInstall, "nothing to fetch")
	if err := empty.Authorize(true); err != nil {
		t.Fatal(err)
	}
	applied, err := p.Apply(context.Background(), c, empty, provider.Progress{})
	if err != nil {
		t.Fatalf("Apply of an empty plan: %v", err)
	}
	if applied.Changed {
		t.Error("Apply reported a change")
	}
	if applied.EndedAt != at {
		t.Error("Apply did not use the injected clock")
	}
}

func TestInspectAndObserveReportTheOperatorsFiles(t *testing.T) {
	p := New()

	got, err := p.Inspect(context.Background(), ctxFor(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Present {
		t.Error("a populated install tree reported absent")
	}
	if got.Sentinel.Declared || got.Integrity.Checked {
		t.Error("a driver with no manifest of expected bytes claimed a verdict")
	}

	obs, err := p.Observe(context.Background(), ctxFor(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if obs.Healthy {
		t.Error("an empty install reported healthy")
	}
	if !strings.Contains(obs.Detail, "put the server files there") {
		t.Errorf("the detail does not say what to do: %q", obs.Detail)
	}
	if obs.ReadAt != at {
		t.Errorf("ReadAt is %v, want the injected clock", obs.ReadAt)
	}
}

func TestKindIsTheSchemasSpelling(t *testing.T) {
	if got := New().Kind(); got != spec.ProviderNone {
		t.Errorf("Kind() is %q, want %q", got, spec.ProviderNone)
	}
	if _, ok := provider.Lookup(spec.ProviderNone); !ok {
		t.Error("the driver did not register itself")
	}
}
