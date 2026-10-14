// Package minecraft is the install driver for Minecraft-based game servers
// (section 10). It supports vanilla, Paper, Purpur, Fabric, NeoForge and Forge
// loaders and uses piston-meta to select the Java version that matches the
// requested MC version.
//
// Architecture notes:
//
//	Plan() is network-reads-only: it fetches version metadata and returns an
//	Artifact list for Apply to download. No writes, no process spawning.
//
//	Apply() downloads each artifact via the BlobStore, hardlinks it into the
//	install tree, and writes a .gvm-java-major sentinel so mc.select_jdk can
//	find the right JRE.
//
//	Inspect()/Observe() read the manifest-declared sentinel file from disk.
//
// The mc.select_jdk hook (internal/hook/mc.go) uses the sentinel file to find
// and symlink the right Java binary before each start.
package minecraft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Provider is the Minecraft install driver.
type Provider struct {
	http *http.Client
}

// New returns a new Provider with the given HTTP client (nil → http.DefaultClient).
func New(httpClient *http.Client) Provider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return Provider{http: httpClient}
}

func init() { provider.Register(New(nil)) }

func (Provider) Kind() string { return spec.ProviderMinecraft }

// Inspect reads the sentinel file from disk.
func (p Provider) Inspect(ctx context.Context, c *provider.Ctx) (*provider.Inspection, error) {
	mc := mcSpec(c)
	if mc == nil {
		return nil, fmt.Errorf("minecraft provider: manifest has no minecraft spec")
	}
	sent := sentinelResult(c, mc.Sentinel)
	present, err := provider.Populated(c.Paths.Install)
	if err != nil {
		return nil, err
	}
	return &provider.Inspection{
		Present:  present,
		Sentinel: sent,
		Integrity: provider.IntegrityResult{
			Checked: false,
			Reason:  "Minecraft installs are not byte-verified; re-download with `gvm update --repair`",
		},
	}, nil
}

// Plan fetches version metadata and returns the server JAR artifact.
func (p Provider) Plan(ctx context.Context, c *provider.Ctx, in provider.Intent) (*provider.Plan, error) {
	mc := mcSpec(c)
	if mc == nil {
		return nil, fmt.Errorf("minecraft provider: manifest has no minecraft spec")
	}

	// Verify is read-only: Minecraft installs are not byte-verified in this build.
	if in == provider.IntentVerify {
		return provider.EmptyPlan(in, fmt.Sprintf(
			"verify %s: sentinel %s present check only; "+
				"Minecraft installs are not byte-verified in this build (no appmanifest). "+
				"Nothing is downloaded and nothing stops.",
			c.Name, mc.Sentinel.Path)), nil
	}

	// Adopted instances must not be overwritten.
	if !c.ManagedInstall {
		return provider.EmptyPlan(in,
			fmt.Sprintf("%s is an adopted install (managed_install:false): gvm will not overwrite "+
				"Minecraft files it did not place. Run `gvm instance promote %s` first.", c.Name, c.Name)), nil
	}

	resolver, err := resolverFor(p.http, mc)
	if err != nil {
		return nil, err
	}
	pl, err := resolver.plan(ctx, c, in)
	if err != nil {
		return nil, err
	}
	return pl, nil
}

// Apply downloads the planned artifacts and writes the sentinel files.
func (p Provider) Apply(ctx context.Context, c *provider.Ctx, pl *provider.Plan, pr provider.Progress) (*provider.Applied, error) {
	managed := c != nil && c.ManagedInstall
	if err := pl.CheckAuthorized(managed); err != nil {
		return nil, err
	}
	if !pl.Writes() {
		return provider.ApplyEmptyPlan(c, pl, "nothing to write")
	}

	mc := mcSpec(c)
	if mc == nil {
		return nil, fmt.Errorf("minecraft provider: manifest has no minecraft spec")
	}
	start := c.Now()

	// Download and link each artifact.
	var wrote []string
	var totalBytes int64
	for _, art := range pl.Artifacts {
		pr.Step("downloading " + art.DestRel)
		dst, err := c.Paths.Dest(art.DestRel)
		if err != nil {
			return nil, err
		}
		// Check if already in blob store.
		if !c.Blobs.Has(art.Digest) {
			pr.Step("fetching " + art.Source)
			if err := fetchToBlobs(ctx, p.http, art.Source, art.Digest, c.Blobs); err != nil {
				return nil, fmt.Errorf("fetch %s: %w", art.DestRel, err)
			}
		}
		if err := c.Blobs.Link(art.Digest, dst); err != nil {
			return nil, fmt.Errorf("link %s: %w", art.DestRel, err)
		}
		wrote = append(wrote, art.DestRel)
		totalBytes += art.Size
		pr.Bytes(totalBytes, pl.Bytes)
	}

	// Write the java-major sentinel so mc.select_jdk knows what to look for.
	if opq, ok := pl.Opaque.(*opaque); ok && opq.JavaMajor > 0 {
		sentPath := filepath.Join(c.Paths.Install.String(), ".gvm-java-major")
		data := fmt.Appendf(nil, "%d\n", opq.JavaMajor)
		if err := os.WriteFile(sentPath, data, 0o644); err != nil {
			c.Logger().Warn("could not write java major file", "err", err)
		}
	}

	now := c.Now()
	return &provider.Applied{
		Intent:    pl.Intent,
		Version:   pl.Target,
		Changed:   true,
		Wrote:     wrote,
		Bytes:     totalBytes,
		Steps:     pl.Steps,
		StartedAt: start,
		EndedAt:   now,
		Note:      pl.Reason,
	}, nil
}

// Observe reads the sentinel from disk.
func (p Provider) Observe(ctx context.Context, c *provider.Ctx) (*provider.Observation, error) {
	mc := mcSpec(c)
	if mc == nil {
		return &provider.Observation{ReadAt: c.Now()}, nil
	}
	sent := sentinelResult(c, mc.Sentinel)
	healthy := sent.Declared && sent.Present
	return &provider.Observation{
		Version: installedVersion(c, mc),
		Healthy: healthy,
		Detail:  sentinelDetail(sent),
		ReadAt:  c.Now(),
	}, nil
}

func sentinelDetail(s provider.SentinelResult) string {
	if !s.Declared {
		return "no sentinel declared"
	}
	if !s.Present {
		return "sentinel missing: " + s.Rel
	}
	return ""
}

// mcSpec returns the manifest's MinecraftSpec, or nil.
func mcSpec(c *provider.Ctx) *spec.MinecraftSpec {
	if c == nil || c.Manifest == nil {
		return nil
	}
	return c.Manifest.Spec.Provider.Minecraft
}

// sentinelResult reads the manifest-declared sentinel path.
func sentinelResult(c *provider.Ctx, s spec.Sentinel) provider.SentinelResult {
	if s.Path == "" {
		return provider.SentinelResult{Declared: false}
	}
	abs := filepath.Join(c.Paths.Install.String(), s.Path)
	fi, err := os.Lstat(abs)
	r := provider.SentinelResult{
		Declared:       true,
		Rel:            s.Path,
		WantExecutable: s.Executable,
		MinBytes:       s.MinBytes,
	}
	if err == nil {
		r.Present = true
		r.Size = fi.Size()
		r.IsDir = fi.IsDir()
		r.Executable = fi.Mode()&0o111 != 0
	}
	return r
}

// installedVersion tries to read the version from the server JAR manifest.
func installedVersion(c *provider.Ctx, mc *spec.MinecraftSpec) provider.Version {
	_ = mc
	// Read version from .gvm-java-major file as a proxy for "what was installed".
	data, err := os.ReadFile(filepath.Join(c.Paths.Install.String(), ".gvm-mc-version"))
	if err != nil {
		return provider.Version{}
	}
	v := string(data)
	if len(v) > 0 && v[len(v)-1] == '\n' {
		v = v[:len(v)-1]
	}
	return provider.Version{ID: v}
}

// opaque carries Plan's network-fetched state into Apply without a second fetch.
type opaque struct {
	JavaMajor int
}

// fetchToBlobs downloads url and stores it in the blob store under digest.
func fetchToBlobs(ctx context.Context, client *http.Client, url string, digest provider.Digest, blobs provider.BlobStore) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gvm/"+time.Now().Format("2006"))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	_, err = blobs.Put(ctx, resp.Body, digest)
	return err
}

// getJSON fetches url and decodes the response body into v.
func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gvm/"+time.Now().Format("2006"))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
