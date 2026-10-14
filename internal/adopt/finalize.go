package adopt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ErrFinalizeNotReady is returned when finalize is called before the 7-day
// review window has elapsed and --force was not given.
var ErrFinalizeNotReady = errors.New("finalize: 7-day review window has not elapsed")

// FinalizeOptions controls gvm adopt finalize behaviour.
type FinalizeOptions struct {
	Force bool // bypass the 7-day wait
	Now   time.Time
}

// FinalizePZ archives the legacy unit files and removes them, completing the
// adoption. It refuses to run within 7 days of adoption unless Force is set.
//
// Legacy unit files are copied to /var/lib/gvm/adopt/<name>-legacy-units/
// before removal so they can be consulted for reference.
func FinalizePZ(serverName, legacyUnitPath, adoptedFrom string, opts FinalizeOptions) error {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	// 7-day guard.
	if !opts.Force {
		adoptedAt := adoptedAtTime(adoptedFrom)
		if !adoptedAt.IsZero() {
			elapsed := opts.Now.Sub(adoptedAt)
			if elapsed < 7*24*time.Hour {
				remaining := 7*24*time.Hour - elapsed
				return fmt.Errorf("%w (%.1f hours remaining; use --force to override)",
					ErrFinalizeNotReady, remaining.Hours())
			}
		}
	}

	// Archive legacy unit to /var/lib/gvm/adopt/<name>-legacy-units/.
	archiveDir := filepath.Join("/var/lib/gvm/adopt", serverName+"-legacy-units")
	if err := os.MkdirAll(archiveDir, 0750); err != nil {
		return fmt.Errorf("finalize: mkdir archive: %w", err)
	}

	unitBase := filepath.Base(legacyUnitPath)
	archivePath := filepath.Join(archiveDir, unitBase)

	data, err := os.ReadFile(legacyUnitPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Already removed — idempotent.
			return nil
		}
		return fmt.Errorf("finalize: read legacy unit: %w", err)
	}
	if err := os.WriteFile(archivePath, data, 0640); err != nil {
		return fmt.Errorf("finalize: archive legacy unit: %w", err)
	}

	// Remove legacy unit file.
	if err := os.Remove(legacyUnitPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("finalize: remove legacy unit: %w", err)
	}

	return nil
}
