package adopt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrFinalizeNotReady is the 7-day review window not having elapsed (or not
// being provable), overridable with --force.
var ErrFinalizeNotReady = errors.New("finalize: the 7-day review window has not elapsed")

// ErrFinalizeRefused is a refusal that --force does NOT override: the instance
// is not adopted, the legacy unit cannot be identified from the host's own
// drop-in, or --unit-path names a different unit. These are about getting the
// target right, not about waiting, so --force has no business clearing them.
var ErrFinalizeRefused = errors.New("finalize: refused")

// FinalizeInputs is everything the fail-closed decision reads. Each field comes
// from the host, not from an operator flag the decision could be talked past:
// AdoptedFrom and ManagedInstall from the instance YAML, LegacyUnit from the
// instance's own 20-adopt.conf, and UnitPathFlag only to be checked AGAINST
// LegacyUnit, never trusted in its place.
type FinalizeInputs struct {
	Name           string
	AdoptedFrom    string // metadata.adopted_from from the instance YAML
	ManagedInstall bool   // spec.managed_install from the instance YAML
	LegacyUnit     string // the Conflicts= unit from 20-adopt.conf
	HaveDropin     bool   // whether a 20-adopt.conf naming a unit was found
	UnitPathFlag   string // --unit-path, checked against LegacyUnit
	Force          bool
	Now            time.Time
}

// ParseAdoptGuardUnit extracts the legacy unit name from a 20-adopt.conf
// drop-in's Conflicts= line. ok is false when the drop-in is absent or names no
// unit — in which case finalize cannot tell what it would be removing, and
// refuses. The legacy unit's name lives only here: `gvm adopt cutover` wrote it
// into the guard, and it is NOT in the instance YAML (see the adopt-guard
// template), so this is the one trustworthy source.
func ParseAdoptGuardUnit(dropin []byte) (string, bool) {
	for _, line := range strings.Split(string(dropin), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if v, ok := strings.CutPrefix(line, "Conflicts="); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v, true
			}
		}
	}
	return "", false
}

// CheckFinalize returns nil when finalize may proceed, or a refusal naming why.
//
// It fails CLOSED. The old guard only waited when it could parse a timestamp,
// so a missing, unparseable or timestamp-less adopted_from skipped the 7-day
// wait entirely and finalize deleted whatever --unit-path named
// (migration-adopt-2). Here a timestamp that cannot be shown to be 7 days old
// is a refusal, not a pass, and the unit to remove comes from the host's own
// drop-in with --unit-path only validated against it.
func CheckFinalize(in FinalizeInputs) error {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	// Never adopted — these hold even under --force.
	if in.ManagedInstall || strings.TrimSpace(in.AdoptedFrom) == "" {
		return fmt.Errorf("%w: %s is not an adopted instance (managed_install is true, or it "+
			"records no adopted_from), so there is nothing to finalize; nothing was changed",
			ErrFinalizeRefused, in.Name)
	}
	// The legacy unit must come from the instance's own adopt guard, not a flag.
	if !in.HaveDropin || strings.TrimSpace(in.LegacyUnit) == "" {
		return fmt.Errorf("%w: cannot tell which legacy unit %s replaced (its 20-adopt.conf names "+
			"no Conflicts= unit); nothing was changed", ErrFinalizeRefused, in.Name)
	}
	// --unit-path, when given, must BE the unit the guard names. This is the
	// check that stops finalize deleting an arbitrary unit path, and --force
	// does not clear it.
	if p := strings.TrimSpace(in.UnitPathFlag); p != "" && filepath.Base(p) != in.LegacyUnit {
		return fmt.Errorf("%w: --unit-path %s is not %s, the unit this instance's adopt guard "+
			"names; nothing was changed", ErrFinalizeRefused, p, in.LegacyUnit)
	}

	// The 7-day guard, overridable with --force. A timestamp that cannot be
	// parsed (the bash-suite form, or a corrupt value) is treated as NOT ready.
	if !in.Force {
		at := adoptedAtTime(in.AdoptedFrom)
		if at.IsZero() {
			return fmt.Errorf("%w: %s records no parseable adoption timestamp (adopted_from=%q), "+
				"so the 7-day window cannot be shown to have elapsed; use --force to finalize anyway",
				ErrFinalizeNotReady, in.Name, in.AdoptedFrom)
		}
		if elapsed := now.Sub(at); elapsed < 7*24*time.Hour {
			return fmt.Errorf("%w (%.1f hours remaining; use --force to override)",
				ErrFinalizeNotReady, (7*24*time.Hour - elapsed).Hours())
		}
	}
	return nil
}

// LegacyUnitsArchiveDir is where finalize copies the legacy units before
// removing them, which is the archive-first half of rule 5. It moved from
// /var/lib/gvm/adopt to /var/backups/gvm so the copies sit beside the world
// archives an operator already knows to look for.
func LegacyUnitsArchiveDir(name string) string {
	return filepath.Join("/var/backups/gvm", name, "legacy-units")
}

// ArchiveAndRemoveLegacyUnit copies the legacy unit file into the archive
// directory, then removes it. It is called ONLY after CheckFinalize has passed
// and the operator has typed the instance name, so by here the decision to
// remove has been made and audited; this is the IO that carries it out.
//
// A missing unit file is idempotent success: a re-run of a finalize whose
// removal already happened must not error.
func ArchiveAndRemoveLegacyUnit(name, legacyUnitPath string) error {
	data, err := os.ReadFile(legacyUnitPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("finalize: read legacy unit %s: %w", legacyUnitPath, err)
	}

	dir := LegacyUnitsArchiveDir(name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("finalize: make archive dir: %w", err)
	}
	dst := filepath.Join(dir, filepath.Base(legacyUnitPath))
	if err := os.WriteFile(dst, data, 0o640); err != nil {
		return fmt.Errorf("finalize: archive legacy unit to %s: %w", dst, err)
	}
	if err := os.Remove(legacyUnitPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("finalize: remove legacy unit %s: %w", legacyUnitPath, err)
	}
	return nil
}
