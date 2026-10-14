package curseforge

import (
	"context"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/content"
)

// resolveRef resolves one CurseForge acquisition ref through the 6-rung ladder.
//
// The ladder (section 12.8):
//
//  1. api    — fetch file metadata from the CurseForge API.
//  2. download_url — use the API's downloadUrl field (VERBATIM, never
//     synthesised).
//  3. cache  — check the blob store by murmur2 fingerprint.
//  4. manual — scan the operator's drop-box directory.
//  5. mirror_modrinth — fingerprint search on Modrinth (best-effort).
//  6. blocked — the author opted out; record and continue. NEVER aborts.
//
// embeddedIDs contains the project IDs of any dep_type==embedded dependencies
// declared by the resolved file. The caller must skip any subsequent ref whose
// project ID appears here: embedded files are bundled inside the parent JAR and
// must not be downloaded separately.
func (s *Source) resolveRef(ctx context.Context, in *content.Input, ref content.Ref, ordinal int) (entry *content.PlanEntry, embeddedIDs []int, err error) {
	projectID, fileID, err := parseRef(content.RefID(ref))
	if err != nil {
		return nil, nil, err
	}

	// Rung 1: API.
	var file *cfFile
	if fileID != 0 {
		file, err = s.client.getFile(ctx, projectID, fileID)
	} else {
		gameVersion := ""
		if in.Schema != nil {
			gameVersion = in.Schema.GameVersion
		}
		file, err = s.client.getLatestFile(ctx, projectID, gameVersion)
	}
	if err != nil {
		// API error: log and continue to rung 6 (record as blocked).
		in.Logger().Info("curseforge: API error, treating as blocked",
			"project", projectID, "file", fileID, "error", err.Error())
		file = nil
	}

	// Dependency checks (section 12.8 rules):
	//   - depRelationIncompatible aborts the plan immediately.
	//   - depRelationEmbedded deps are bundled inside this file; collect their
	//     project IDs so the caller can skip any separate ref for them.
	if file != nil {
		for _, dep := range file.Dependencies {
			switch dep.RelationType {
			case depRelationIncompatible:
				return nil, nil, fmt.Errorf("curseforge: %s declares incompatible dependency on project %d; aborting plan",
					file.FileName, dep.ModID)
			case depRelationEmbedded:
				embeddedIDs = append(embeddedIDs, dep.ModID)
			}
		}
	}

	// Rung 2: download_url (from API response, verbatim).
	if file != nil && !isBlocked(file) {
		// The URL came from the API; it is never constructed here.
		e, rungErr := s.rungDownloadURL(ctx, in, ref, ordinal, file)
		return e, embeddedIDs, rungErr
	}

	// Rung 3: cache (check blob store by fingerprint).
	if file != nil && file.Fingerprint != 0 && in.Blobs != nil {
		if e, ok := s.rungCache(in, ref, ordinal, file); ok {
			return e, embeddedIDs, nil
		}
	}

	// Rung 4: manual (drop-box).
	if s.dropBox != "" {
		fn := ""
		fp := uint32(0)
		if file != nil {
			fn, fp = file.FileName, file.Fingerprint
		}
		if e, ok := s.rungManual(in, ref, ordinal, fn, fp); ok {
			return e, embeddedIDs, nil
		}
	}

	// Rung 5: mirror_modrinth (fingerprint search).
	if file != nil && file.Fingerprint != 0 {
		if e, ok := s.rungMirrorModrinth(ctx, in, ref, ordinal, file); ok {
			return e, embeddedIDs, nil
		}
	}

	// Rung 6: blocked. Record the entry and continue (never abort).
	return s.rungBlocked(in, ref, ordinal, file), embeddedIDs, nil
}

// rungDownloadURL materializes using the verbatim API downloadUrl.
func (s *Source) rungDownloadURL(_ context.Context, in *content.Input, ref content.Ref, ordinal int, file *cfFile) (*content.PlanEntry, error) {
	dig := fileDigest(file)
	art := &content.Artifact{
		Kind:   content.ArtifactFile,
		Digest: dig,
		Size:   file.FileLength,
		Path:   file.FileName,
	}
	// Store the download URL for Materialize. The URL is from the API response,
	// never synthesised from project IDs.
	s.mu.Lock()
	s.downloadURLs[dig] = file.DownloadURL
	s.mu.Unlock()

	return &content.PlanEntry{
		Ordinal:    ordinal,
		Ref:        ref,
		Resolution: content.ResolutionDownloadURL,
		Artifact:   art,
		Upstream: &content.Upstream{
			API:      "curseforge",
			FileSize: file.FileLength,
		},
	}, nil
}

// rungCache checks whether the blob store already has this file by fingerprint.
func (s *Source) rungCache(in *content.Input, ref content.Ref, ordinal int, file *cfFile) (*content.PlanEntry, bool) {
	dig := fileDigest(file)
	if in.Blobs == nil || !in.Blobs.Has(dig) {
		return nil, false
	}
	art := &content.Artifact{
		Kind:   content.ArtifactFile,
		Digest: dig,
		Size:   file.FileLength,
		Path:   file.FileName,
	}
	return &content.PlanEntry{
		Ordinal:    ordinal,
		Ref:        ref,
		Resolution: content.ResolutionCache,
		Artifact:   art,
		Upstream:   &content.Upstream{API: "curseforge"},
	}, true
}

// rungManual checks the drop-box for a manually placed JAR.
func (s *Source) rungManual(in *content.Input, ref content.Ref, ordinal int, filename string, fp uint32) (*content.PlanEntry, bool) {
	match, err := ScanDropBox(s.dropBox, fp, filename)
	if err != nil || match == "" {
		return nil, false
	}

	// Read the file to get its size and actual fingerprint.
	// (We found it by name-stem fallback; re-verify the fingerprint.)
	data, err := readFile(match)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	actualFP := murmur2(data)
	dig := content.Digest(fmt.Sprintf("murmur2:%d", actualFP))

	art := &content.Artifact{
		Kind:   content.ArtifactFile,
		Digest: dig,
		Size:   int64(len(data)),
		Path:   filename,
	}
	s.mu.Lock()
	s.manualPaths[dig] = match
	s.mu.Unlock()

	return &content.PlanEntry{
		Ordinal:    ordinal,
		Ref:        ref,
		Resolution: content.ResolutionManual,
		Artifact:   art,
		Upstream:   &content.Upstream{API: "curseforge"},
	}, true
}

// rungMirrorModrinth attempts a fingerprint lookup on Modrinth.
// This is best-effort; failure falls through to rung 6.
func (s *Source) rungMirrorModrinth(_ context.Context, _ *content.Input, ref content.Ref, ordinal int, file *cfFile) (*content.PlanEntry, bool) {
	// NOTE: A real Modrinth fingerprint API call would go here.
	// The current implementation is intentionally minimal: Modrinth's hash-based
	// search endpoint can locate a file by SHA1 or SHA512. A future PR can wire
	// this up with the modrinth package when the shared hash format is stabilised.
	_ = file
	return nil, false
}

// rungBlocked records a blocked entry. It NEVER returns an error: a blocked
// mod is a known state, not a failure, and the caller continues with the rest
// of the plan.
func (s *Source) rungBlocked(_ *content.Input, ref content.Ref, ordinal int, file *cfFile) *content.PlanEntry {
	name := ""
	if file != nil {
		name = file.FileName
	}
	// Nil artifact: nothing to materialise. Materialize will skip this entry.
	return &content.PlanEntry{
		Ordinal:    ordinal,
		Ref:        ref,
		Resolution: content.ResolutionBlocked,
		Artifact:   nil, // explicitly nil: blocked mods have no materialisable bytes
		Upstream: &content.Upstream{
			API:   "curseforge",
			Title: name,
			// BanReason is the operator-readable explanation.
			BanReason: "mod author opted out of API distribution; drop the JAR into the " +
				"blocked-mods drop-box and run `gvm content resolve-blocked`",
		},
	}
}

// fileDigest builds a content.Digest from a cfFile's hashes.
// SHA1 is used when available (the only hash CurseForge provides in the API).
func fileDigest(file *cfFile) content.Digest {
	for _, h := range file.Hashes {
		if h.Algo == 1 { // SHA1
			return content.Digest("sha1:" + strings.ToLower(h.Value))
		}
	}
	// Fallback: murmur2 fingerprint. Not cryptographic, but it is what the API
	// returns and it is sufficient for change detection.
	return content.Digest(fmt.Sprintf("murmur2:%d", file.Fingerprint))
}

// parseRef splits a CurseForge acquisition id into projectID and optionally
// fileID. Accepted forms: "123456" or "123456:789012".
func parseRef(id content.AcquireID) (projectID, fileID int, err error) {
	s := strings.TrimSpace(string(id))
	if s == "" {
		return 0, 0, fmt.Errorf("curseforge: empty acquisition id")
	}
	sep := strings.Index(s, ":")
	if sep < 0 {
		n, err := parseInt(s)
		if err != nil {
			return 0, 0, fmt.Errorf("curseforge: invalid acquisition id %q: %w", id, err)
		}
		return n, 0, nil
	}
	pid, err := parseInt(s[:sep])
	if err != nil {
		return 0, 0, fmt.Errorf("curseforge: invalid project id in %q: %w", id, err)
	}
	fid, err := parseInt(s[sep+1:])
	if err != nil {
		return 0, 0, fmt.Errorf("curseforge: invalid file id in %q: %w", id, err)
	}
	return pid, fid, nil
}

func parseInt(s string) (int, error) {
	n := 0
	if len(s) == 0 {
		return 0, fmt.Errorf("empty integer")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not an integer: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
