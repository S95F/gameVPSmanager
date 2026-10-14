package curseforge

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/S95F/gameVPSmanager/internal/content"
)

// Source is the CurseForge content backend.
//
// The x-api-key is supplied at construction time (operator-provided). It is
// never embedded in this package; see client.go.
type Source struct {
	client       *Client
	dropBox      string // path to operator's drop-box directory, may be empty
	mu           sync.Mutex
	downloadURLs map[content.Digest]string // populated by Resolve, consumed by Materialize
	manualPaths  map[content.Digest]string // drop-box file paths for manual-resolution entries
}

// New returns a CurseForge Source with the operator-supplied API key.
// dropBox is the path to the directory where operators drop blocked-mod JARs;
// it may be empty to disable manual rung.
func New(apiKey, dropBox string, hc *http.Client) *Source {
	return &Source{
		client:       NewClient(apiKey, hc),
		dropBox:      dropBox,
		downloadURLs: make(map[content.Digest]string),
		manualPaths:  make(map[content.Digest]string),
	}
}

func (s *Source) Kind() content.SourceKind { return content.KindCurseForge }

// Resolve fetches file metadata from the CurseForge API and classifies every
// Ref through the 6-rung resolution ladder.
//
// A blocked mod (downloadUrl empty) enters ResolutionBlocked and is NEVER an
// error: the plan continues, and the operator can provide the file manually.
func (s *Source) Resolve(ctx context.Context, in *content.Input, want []content.Ref) (*content.Plan, error) {
	plan := &content.Plan{
		Source:   content.KindCurseForge,
		Declared: len(want),
	}

	// embeddedProjects tracks project IDs declared as depRelationEmbedded by
	// already-resolved files. An embedded dep is physically bundled inside its
	// parent JAR; a separate ref for it would download the file twice.
	embeddedProjects := make(map[int]bool)

	for i, ref := range want {
		// Skip a ref whose project was declared as embedded by a previously
		// resolved file to prevent double-download.
		if pid, _, parseErr := parseRef(content.RefID(ref)); parseErr == nil && embeddedProjects[pid] {
			plan.Unaccounted = append(plan.Unaccounted, content.Unaccounted{
				ID: content.RefID(ref),
				Reason: fmt.Sprintf("project %d is embedded in an already-resolved file; "+
					"skipping to avoid double-download", pid),
			})
			continue
		}

		entry, embedded, err := s.resolveRef(ctx, in, ref, i)
		if err != nil {
			plan.Unaccounted = append(plan.Unaccounted, content.Unaccounted{
				ID:     content.RefID(ref),
				Reason: err.Error(),
			})
			continue
		}
		for _, eid := range embedded {
			embeddedProjects[eid] = true
		}
		plan.Entries = append(plan.Entries, *entry)
	}
	return plan, nil
}

// Materialize downloads JAR files for non-blocked entries.
//
// Blocked entries (Resolution == ResolutionBlocked) are silently skipped: they
// have no Artifact and nothing to download. The plan's CheckAccounted guard
// ensures this is explicit rather than hidden.
func (s *Source) Materialize(ctx context.Context, in *content.Input, p *content.Plan, pr content.Progress) (*content.Lock, error) {
	if err := p.CheckAccounted(); err != nil {
		return nil, err
	}

	lock := content.NewLock(in)
	sl := &content.SourceLock{Kind: content.KindCurseForge}

	for _, pe := range p.Entries {
		if pe.Artifact == nil {
			// Blocked entry: record in the lock but do not attempt to download.
			sl.Entries = append(sl.Entries, content.LockEntry{
				Ordinal:    pe.Ordinal,
				Acquire:    pe.AcquireID(),
				Resolution: pe.Resolution,
				Artifact:   nil,
			})
			continue
		}

		if err := s.materializeEntry(ctx, in, pe); err != nil {
			return nil, err
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

// materializeEntry downloads and links one entry.
func (s *Source) materializeEntry(ctx context.Context, in *content.Input, pe content.PlanEntry) error {
	if pe.Artifact == nil {
		return nil
	}
	dig := pe.Artifact.Digest
	if in.Blobs != nil && in.Blobs.Has(dig) {
		// Already in the blob store; just link.
		dst, err := in.Jail.Resolve(pe.Artifact.Path)
		if err != nil {
			return fmt.Errorf("curseforge: resolve path for %s: %w", pe.AcquireID(), err)
		}
		return in.Blobs.Link(dig, dst)
	}

	// Manual path (from drop-box)?
	s.mu.Lock()
	manualPath, hasManual := s.manualPaths[dig]
	s.mu.Unlock()
	if hasManual {
		return s.materializeFromFile(in, pe, manualPath)
	}

	// Download URL from API response.
	s.mu.Lock()
	dlURL, hasURL := s.downloadURLs[dig]
	s.mu.Unlock()
	if !hasURL || dlURL == "" {
		return fmt.Errorf("curseforge: no download URL for %s (digest %s)", pe.AcquireID(), dig)
	}

	return s.downloadAndLink(ctx, in, pe, dlURL)
}

// downloadAndLink fetches a URL and links the result into the jail.
func (s *Source) downloadAndLink(ctx context.Context, in *content.Input, pe content.PlanEntry, dlURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return fmt.Errorf("curseforge: prepare download for %s: %w", pe.AcquireID(), err)
	}
	req.Header.Set("x-api-key", s.client.apiKey)

	resp, err := s.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("curseforge: download %s: %w", pe.AcquireID(), err)
	}
	dig := pe.Artifact.Digest
	_, putErr := in.Blobs.Put(ctx, resp.Body, dig)
	resp.Body.Close()
	if putErr != nil {
		return fmt.Errorf("curseforge: store %s: %w", pe.AcquireID(), putErr)
	}

	dst, err := in.Jail.Resolve(pe.Artifact.Path)
	if err != nil {
		return fmt.Errorf("curseforge: resolve path for %s: %w", pe.AcquireID(), err)
	}
	return in.Blobs.Link(dig, dst)
}

// materializeFromFile copies a drop-box file into the blob store.
func (s *Source) materializeFromFile(in *content.Input, pe content.PlanEntry, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("curseforge: open drop-box file %s: %w", path, err)
	}
	defer f.Close()

	ctx := context.Background()
	_, putErr := in.Blobs.Put(ctx, f, pe.Artifact.Digest)
	if putErr != nil {
		return fmt.Errorf("curseforge: store drop-box file %s: %w", path, putErr)
	}

	dst, err := in.Jail.Resolve(pe.Artifact.Path)
	if err != nil {
		return fmt.Errorf("curseforge: resolve path for %s: %w", pe.AcquireID(), err)
	}
	return in.Blobs.Link(pe.Artifact.Digest, dst)
}

// Activate maps the lock to the game's activation keys.
func (s *Source) Activate(_ context.Context, in *content.Input, l *content.Lock) (*content.Activation, error) {
	return content.Activate(in.Schema, l)
}

// Observe reads local disk state.
func (s *Source) Observe(_ context.Context, in *content.Input) (*content.Observed, error) {
	obs := &content.Observed{
		ReadAt: in.Now(),
	}
	for _, root := range in.Roots {
		obs.Roots = append(obs.Roots, root.Name)
	}
	return obs, nil
}

// readFile reads a file's bytes. Small helper used by rungManual.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// compile-time assertion: Source implements content.Source.
var _ content.Source = (*Source)(nil)
