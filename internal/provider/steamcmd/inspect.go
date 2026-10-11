package steamcmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The two read-only halves of the seam.
//
// Inspect is the thorough one: it walks the tree for stray appmanifests and
// runs the whole D-32 check, so it is what `gvm doctor`, adoption and drift
// detection ask. Observe is the every-thirty-seconds one and reads exactly two
// paths, because a status poll that walked a 65 GB CS2 tree would be the most
// expensive thing on the box.
//
// Both are safe against a live, running server: nothing here opens a file for
// writing, and nothing consults the network or the database. That is what makes
// "what is installed" answerable with Postgres down.

// Inspect reports what is on disk, without judgement, plus the D-32 verdict.
//
// HandEdits and Unmanaged stay empty, and the reason is recorded in
// Integrity.Facts rather than left as an absence: SteamCMD publishes no
// per-file digests, so the only way to find a hand-edited file is `validate` --
// which repairs it by overwriting it, destroying the evidence and the operator's
// change in the same step. Guessing from mtimes would report every file Steam
// touched during the last update as a hand edit.
func (p *Provider) Inspect(ctx context.Context, c *provider.Ctx) (*provider.Inspection, error) {
	sc, err := steamSpec(c)
	if err != nil {
		return nil, err
	}
	// Populated, not "installed": a directory holding 40 GB of a 65 GB download
	// is neither empty nor installed, and the sentinel plus the integrity
	// record are what answer the second question.
	present, err := provider.Populated(c.Paths.Install)
	if err != nil {
		return nil, err
	}

	report, verifyErr := p.verify(c, sc, "")
	if verifyErr != nil {
		// A failed verification is a FINDING, not an Inspect failure: the whole
		// point of Inspect is to return it. The caller branches on
		// Integrity.Failed and Sentinel.OK.
		c.Logger().Debug("steamcmd verification found problems",
			"instance", c.Name, "problems", strings.Join(report.Problems, "; "))
	}

	return &provider.Inspection{
		Present:   present,
		Version:   report.Version(),
		Sentinel:  report.Sentinel,
		Integrity: report.Integrity(),
	}, nil
}

// Observe is the cheap, disk-only poll.
//
// It reads the canonical appmanifest path directly instead of searching for it,
// which is the one difference from Inspect that matters: Inspect's search is
// what catches two appmanifests fighting over one tree, and Observe's job is to
// answer "is this still the build we think it is" in single-digit milliseconds.
//
// An absent or unreadable appmanifest is not an error here. It is the state of
// every instance that has not been installed yet, and an Observe that returned
// an error for it would make the dashboard's thirty-second poll fail for a
// perfectly ordinary condition.
func (p *Provider) Observe(ctx context.Context, c *provider.Ctx) (*provider.Observation, error) {
	sc, err := steamSpec(c)
	if err != nil {
		return nil, err
	}
	obs := &provider.Observation{ReadAt: c.Now()}

	tree, err := p.installTree(c)
	if err != nil {
		return nil, err
	}
	sentinel := sentinelResult(tree, sc.Sentinel)

	m, acfErr := p.readACF(c, sc)
	if acfErr != nil {
		obs.Detail = fmt.Sprintf("%s is absent or unreadable, so Steam has no record of app %d "+
			"in %s: nothing has been installed here yet (%v)",
			CanonicalACF(sc.InstallAppID), sc.InstallAppID, c.Paths.Install.Path(), acfErr)
		return obs, nil
	}

	obs.Version = provider.Version{ID: m.BuildID, Label: m.Name, At: m.LastUpdated}
	sentinelErr := sentinel.OK()
	// Complete and not Installed: the byte pairs are already parsed, and a poll
	// that judged StateFlags alone would report green for exactly the tree a
	// failed update leaves behind -- D-32 forbids auto-remediation, so that
	// state persists until an operator acts, and Inspect calls it a failure.
	obs.Healthy = m.Complete() && sentinelErr == nil

	switch {
	case m.BuildIDSuspect:
		// A fixed sentence. The refused bytes are what would redraw the
		// operator's terminal, so they stay in the file.
		obs.Detail = fmt.Sprintf("%s records a buildid that is not a plausible build id, so "+
			"something other than Steam wrote it; `gvm doctor %s` reports the whole finding",
			CanonicalACF(sc.InstallAppID), c.Name)
	case !m.Installed():
		obs.Detail = fmt.Sprintf("build %s, and StateFlags is %s: Steam does not call this "+
			"install complete", m.BuildLabel(), StateDescription(m.StateFlags))
	case !m.HasDownloadPair || m.BytesDownloaded != m.BytesToDownload:
		// The same sentence Verify.checkBytes gives, because the operator
		// should not have to tell the two readings apart.
		obs.Detail = fmt.Sprintf("build %s is installed and the download is short: %s of %s "+
			"arrived (%d of %d bytes)", m.BuildID, spec.FormatBytes(m.BytesDownloaded),
			spec.FormatBytes(m.BytesToDownload), m.BytesDownloaded, m.BytesToDownload)
	case !m.HasStagePair || m.BytesStaged != m.BytesToStage:
		obs.Detail = fmt.Sprintf("build %s is installed and the staging step is short: %s of %s "+
			"was committed into place (%d of %d bytes)", m.BuildID,
			spec.FormatBytes(m.BytesStaged), spec.FormatBytes(m.BytesToStage),
			m.BytesStaged, m.BytesToStage)
	case !m.TargetSettled():
		obs.Detail = fmt.Sprintf("build %s is installed and Steam still records %s as the build "+
			"it was moving to: an update was started and did not land",
			m.BuildID, m.TargetBuildID)
	case sentinelErr != nil:
		obs.Detail = fmt.Sprintf("build %s is installed and %v", m.BuildID, sentinelErr)
	default:
		obs.Detail = fmt.Sprintf("build %s, %s on disk, %s present",
			m.BuildID, spec.FormatBytes(m.SizeOnDisk), sentinel.Rel)
	}
	return obs, nil
}
