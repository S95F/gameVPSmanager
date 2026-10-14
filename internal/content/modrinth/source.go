// Package modrinth implements the Modrinth content source (section 12.8).
//
// Architecture rules enforced by this package:
//
//   - dependency_type == "embedded" is NEVER downloaded. Embedded deps are
//     bundled inside the parent JAR; downloading them separately would install
//     the mod twice and break the version the parent ships.
//
//   - dependency_type == "incompatible" aborts the plan. An incompatible dep
//     is an explicit "do not install these together" statement.
//
//   - filter parameters are JSON-encoded (Modrinth's API requires it).
//
//   - game_versions and loaders in the API response are re-verified before
//     any JAR is written.
//
//   - SHA512 is used when available (Modrinth provides both SHA512 and SHA1).
package modrinth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/content"
)

const modrinthAPIBase = "https://api.modrinth.com/v2"

// Source is the Modrinth content backend.
type Source struct {
	http         *http.Client
	mu           sync.Mutex
	downloadURLs map[content.Digest]string // digest → download URL, populated by Resolve
}

// New returns a Modrinth Source. nil http client uses http.DefaultClient.
func New(client *http.Client) *Source {
	if client == nil {
		client = http.DefaultClient
	}
	return &Source{
		http:         client,
		downloadURLs: make(map[content.Digest]string),
	}
}

func (s *Source) Kind() content.SourceKind { return content.KindModrinth }

// Resolve fetches version metadata from Modrinth and classifies every Ref.
func (s *Source) Resolve(ctx context.Context, in *content.Input, want []content.Ref) (*content.Plan, error) {
	mcVersion := modrinthMCVersion(in)
	loader := modrinthLoader(in)

	plan := &content.Plan{
		Source:   content.KindModrinth,
		Declared: len(want),
	}

	for i, ref := range want {
		id := content.RefID(ref)
		entry, err := s.resolveOne(ctx, in, ref, i, mcVersion, loader)
		if err != nil {
			plan.Unaccounted = append(plan.Unaccounted, content.Unaccounted{
				ID:     content.RefID(ref),
				Reason: err.Error(),
			})
			continue
		}
		plan.Entries = append(plan.Entries, *entry)
		_ = id
	}
	return plan, nil
}

// Materialize downloads the planned jars via the blob store.
func (s *Source) Materialize(ctx context.Context, in *content.Input, p *content.Plan, pr content.Progress) (*content.Lock, error) {
	if err := p.CheckAccounted(); err != nil {
		return nil, err
	}

	lock := content.NewLock(in)
	sl := &content.SourceLock{Kind: content.KindModrinth}

	for _, pe := range p.Entries {
		if pe.Artifact == nil {
			continue
		}
		// Download the jar via the blob store.
		if !in.Blobs.Has(pe.Artifact.Digest) {
			artURL := s.modrinthArtifactURL(pe)
			if artURL == "" {
				// No URL available; skip (should not happen in production).
				continue
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, artURL, nil)
			if err != nil {
				return nil, fmt.Errorf("modrinth: fetch %s: %w", pe.AcquireID(), err)
			}
			req.Header.Set("User-Agent", "gvm/"+time.Now().Format("2006"))
			resp, err := s.http.Do(req)
			if err != nil {
				return nil, fmt.Errorf("modrinth: download %s: %w", pe.AcquireID(), err)
			}
			_, putErr := in.Blobs.Put(ctx, resp.Body, pe.Artifact.Digest)
			resp.Body.Close()
			if putErr != nil {
				return nil, fmt.Errorf("modrinth: store %s: %w", pe.AcquireID(), putErr)
			}
		}

		dst, err := in.Jail.Resolve(jarPath(in, pe))
		if err != nil {
			return nil, fmt.Errorf("modrinth: resolve path for %s: %w", pe.AcquireID(), err)
		}
		if err := in.Blobs.Link(pe.Artifact.Digest, dst); err != nil {
			return nil, fmt.Errorf("modrinth: link %s: %w", pe.AcquireID(), err)
		}

		sl.Entries = append(sl.Entries, content.LockEntry{
			Ordinal:       pe.Ordinal,
			Acquire:       pe.AcquireID(),
			Materialized:  true,
			ActivationIDs: pe.Activation,
			Resolution:    pe.Resolution,
			Artifact:      pe.Artifact,
			Upstream:      pe.Upstream,
		})
	}

	if in.Schema != nil {
		lock.Sources[in.Schema.Block] = sl
	}
	return lock, nil
}

// Activate maps the lock to the game's activation keys.
func (s *Source) Activate(ctx context.Context, in *content.Input, l *content.Lock) (*content.Activation, error) {
	return content.Activate(in.Schema, l)
}

// Observe reads local disk state.
func (s *Source) Observe(ctx context.Context, in *content.Input) (*content.Observed, error) {
	obs := &content.Observed{
		ReadAt: in.Now(),
	}
	for _, root := range in.Roots {
		obs.Roots = append(obs.Roots, root.Name)
	}
	return obs, nil
}

// modrinthVersion is one version returned by the Modrinth v2 API.
type modrinthVersion struct {
	ID            string         `json:"id"`
	ProjectID     string         `json:"project_id"`
	Name          string         `json:"name"`
	VersionNumber string         `json:"version_number"`
	GameVersions  []string       `json:"game_versions"`
	Loaders       []string       `json:"loaders"`
	Files         []modrinthFile `json:"files"`
	Dependencies  []modrinthDep  `json:"dependencies"`
	VersionType   string         `json:"version_type"` // "release" | "beta" | "alpha"
}

type modrinthFile struct {
	URL      string            `json:"url"`
	Filename string            `json:"filename"`
	Primary  bool              `json:"primary"`
	Size     int64             `json:"size"`
	Hashes   map[string]string `json:"hashes"`
}

type modrinthDep struct {
	ProjectID      string `json:"project_id"`
	VersionID      string `json:"version_id"`
	DependencyType string `json:"dependency_type"` // "required"|"optional"|"incompatible"|"embedded"
}

// resolveOne fetches and classifies one Modrinth project/version ref.
func (s *Source) resolveOne(ctx context.Context, in *content.Input, ref content.Ref, ordinal int, mcVersion, loader string) (*content.PlanEntry, error) {
	// Parse the ref ID as "project_slug" or "project_slug:version_id".
	projectID, versionID := parseModrinthRef(string(content.RefID(ref)))

	var ver *modrinthVersion
	var err error
	if versionID != "" {
		ver, err = s.fetchVersion(ctx, versionID)
	} else {
		ver, err = s.fetchLatestVersion(ctx, projectID, mcVersion, loader)
	}
	if err != nil {
		return nil, err
	}

	// Re-verify game_versions and loaders (section 12.8 rule).
	if !containsString(ver.GameVersions, mcVersion) {
		return nil, fmt.Errorf("version %s does not support Minecraft %s (got %v)",
			ver.VersionNumber, mcVersion, ver.GameVersions)
	}
	if loader != "" && !containsString(ver.Loaders, loader) {
		return nil, fmt.Errorf("version %s does not support loader %s (got %v)",
			ver.VersionNumber, loader, ver.Loaders)
	}

	// Check for incompatible dependencies — abort the plan.
	for _, dep := range ver.Dependencies {
		if dep.DependencyType == "incompatible" {
			return nil, fmt.Errorf("version %s has incompatible dependency %s; aborting plan",
				ver.VersionNumber, dep.ProjectID)
		}
		// embedded deps are NEVER downloaded.
	}

	// Find the primary file (prefer primary flag, fall back to first).
	var primary *modrinthFile
	for i := range ver.Files {
		if ver.Files[i].Primary {
			primary = &ver.Files[i]
			break
		}
	}
	if primary == nil && len(ver.Files) > 0 {
		primary = &ver.Files[0]
	}
	if primary == nil {
		return nil, fmt.Errorf("modrinth version %s has no files", ver.VersionNumber)
	}

	// Prefer SHA512 digest, fall back to SHA1.
	var digest content.Digest
	if sha512 := primary.Hashes["sha512"]; sha512 != "" {
		digest = content.Digest("sha512:" + sha512)
	} else if sha1 := primary.Hashes["sha1"]; sha1 != "" {
		digest = content.Digest("sha1:" + sha1)
	} else {
		return nil, fmt.Errorf("modrinth version %s has no sha512 or sha1 hash", ver.VersionNumber)
	}

	art := &content.Artifact{
		Kind:   content.ArtifactFile,
		Digest: digest,
		Size:   primary.Size,
		Path:   primary.Filename,
	}
	// Record the download URL for Materialize to use.
	s.mu.Lock()
	s.downloadURLs[digest] = primary.URL
	s.mu.Unlock()

	return &content.PlanEntry{
		Ordinal:    ordinal,
		Ref:        ref,
		Resolution: content.ResolutionAPI,
		Artifact:   art,
		Upstream: &content.Upstream{
			API:   "modrinth",
			Title: ver.Name,
		},
	}, nil
}

// fetchVersion fetches a specific version by ID.
func (s *Source) fetchVersion(ctx context.Context, versionID string) (*modrinthVersion, error) {
	u := fmt.Sprintf("%s/version/%s", modrinthAPIBase, url.PathEscape(versionID))
	var ver modrinthVersion
	if err := getJSON(ctx, s.http, u, &ver); err != nil {
		return nil, fmt.Errorf("modrinth API version %s: %w", versionID, err)
	}
	return &ver, nil
}

// fetchLatestVersion fetches the latest version of a project for a given MC version.
func (s *Source) fetchLatestVersion(ctx context.Context, projectID, mcVersion, loader string) (*modrinthVersion, error) {
	// Build filter params as JSON arrays (Modrinth API requirement).
	params := url.Values{}
	if mcVersion != "" {
		gv, _ := json.Marshal([]string{mcVersion})
		params.Set("game_versions", string(gv))
	}
	if loader != "" {
		ld, _ := json.Marshal([]string{loader})
		params.Set("loaders", string(ld))
	}

	u := fmt.Sprintf("%s/project/%s/version?%s", modrinthAPIBase, url.PathEscape(projectID), params.Encode())
	var versions []modrinthVersion
	if err := getJSON(ctx, s.http, u, &versions); err != nil {
		return nil, fmt.Errorf("modrinth project %s versions: %w", projectID, err)
	}
	// Return the latest release, or the first if no releases.
	for i := range versions {
		if versions[i].VersionType == "release" {
			return &versions[i], nil
		}
	}
	if len(versions) > 0 {
		return &versions[0], nil
	}
	return nil, fmt.Errorf("no Modrinth versions for project %s (mc=%s, loader=%s)", projectID, mcVersion, loader)
}

// parseModrinthRef splits "project_slug:version_id" or returns ("project_slug", "").
func parseModrinthRef(ref string) (projectID, versionID string) {
	parts := strings.SplitN(ref, ":", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return ref, ""
}

// modrinthMCVersion extracts the Minecraft version from the content schema.
func modrinthMCVersion(in *content.Input) string {
	if in.Schema == nil {
		return ""
	}
	// Try to get the Minecraft version from the schema's game version field.
	// This comes from the manifest's minecraft spec.
	return in.Schema.GameVersion
}

// modrinthLoader extracts the loader kind from the content schema.
func modrinthLoader(in *content.Input) string {
	if in.Schema == nil {
		return ""
	}
	return in.Schema.Loader
}

// modrinthArtifactURL looks up the download URL for a plan entry's artifact digest.
func (s *Source) modrinthArtifactURL(pe content.PlanEntry) string {
	if pe.Artifact == nil {
		return ""
	}
	s.mu.Lock()
	u := s.downloadURLs[pe.Artifact.Digest]
	s.mu.Unlock()
	return u
}

// jarPath returns the destination path for a mod JAR.
func jarPath(in *content.Input, pe content.PlanEntry) string {
	if pe.Artifact != nil && pe.Artifact.Path != "" {
		return pe.Artifact.Path
	}
	return string(pe.AcquireID()) + ".jar"
}

// containsString reports whether ss contains s.
func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// getJSON fetches url and decodes the response into v.
func getJSON(ctx context.Context, client *http.Client, rawURL string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
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
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

var _ content.Source = (*Source)(nil)
