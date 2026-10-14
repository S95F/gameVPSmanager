package minecraft

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// TestMinecraftProviderKind verifies the provider is registered correctly.
func TestMinecraftProviderKind(t *testing.T) {
	p := New(nil)
	if p.Kind() != spec.ProviderMinecraft {
		t.Errorf("Kind() = %q, want %q", p.Kind(), spec.ProviderMinecraft)
	}
}

// TestMinecraftProviderIsRegistered verifies init() ran and registered the driver.
func TestMinecraftProviderIsRegistered(t *testing.T) {
	_, ok := provider.Lookup(spec.ProviderMinecraft)
	if !ok {
		t.Errorf("minecraft provider not registered; check init() in provider.go")
	}
}

// TestMinecraftProviderPlanAdoptedIsEmpty verifies that an adopted instance
// gets an empty plan.
func TestMinecraftProviderPlanAdoptedIsEmpty(t *testing.T) {
	p := New(nil)
	c := vanillaCtx(t, false)
	pl, err := p.Plan(context.Background(), c, provider.IntentInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if pl.Writes() {
		t.Error("adopted instance should produce an empty (non-writing) plan")
	}
}

// TestMinecraftVanillaPlanFetchesJAR verifies that a vanilla Plan
// fetches the server.jar URL from piston-meta.
func TestMinecraftVanillaPlanFetchesJAR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "version_manifest"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"latest":{"release":"1.21.4","snapshot":""},"versions":[{"id":"1.21.4","type":"release","url":"http://%s/version/1.21.4"}]}`, r.Host)
		case strings.Contains(r.URL.Path, "version/1.21.4"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"1.21.4","javaVersion":{"majorVersion":21,"component":"java-runtime-delta"},"downloads":{"server":{"url":"http://%s/server.jar","sha1":"aabbccddee","size":1024}}}`, r.Host)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Override the piston manifest URL for this test.
	origURL := pistonManifestURL
	t.Cleanup(func() { pistonManifestURL = origURL })
	pistonManifestURL = srv.URL + "/version_manifest_v2.json"

	p := New(srv.Client())
	c := vanillaCtx(t, true)
	pl, err := p.Plan(context.Background(), c, provider.IntentInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !pl.Writes() {
		t.Error("managed install should produce a writing plan")
	}
	if len(pl.Artifacts) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(pl.Artifacts))
	}
	if pl.Artifacts[0].DestRel != "server.jar" {
		t.Errorf("DestRel = %q, want server.jar", pl.Artifacts[0].DestRel)
	}
	if pl.Opaque == nil {
		t.Error("Opaque should carry the java major version")
	}
}

// vanillaCtx builds a minimal Ctx for a vanilla Minecraft manifest.
func vanillaCtx(t *testing.T, managed bool) *provider.Ctx {
	t.Helper()
	dir := t.TempDir()
	install := filepath.Join(dir, "install")
	data := filepath.Join(dir, "data")
	home := filepath.Join(dir, "home")
	for _, d := range []string{install, data, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := provider.NewPaths(spec.Paths{Install: install, Data: data, Home: home})
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	m := &spec.Manifest{
		Spec: spec.GameSpec{
			Provider: spec.ProviderSpec{
				Kind: spec.ProviderMinecraft,
				Minecraft: &spec.MinecraftSpec{
					MCVersion: "1.21.4",
					Loader:    spec.LoaderSpec{Kind: "vanilla"},
					Java:      spec.JavaSpec{From: "piston_meta"},
					Sentinel:  spec.Sentinel{Path: "server.jar"},
				},
			},
		},
	}
	return &provider.Ctx{
		Name:           "test-mc",
		Manifest:       m,
		Paths:          paths,
		ManagedInstall: managed,
	}
}
