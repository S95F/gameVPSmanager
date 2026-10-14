package adopt

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CutoverEnv abstracts systemd, backup, and file operations for the cutover
// so tests can inject failures at each post-step-6 step.
type CutoverEnv interface {
	WriteFile(path string, data []byte, perm fs.FileMode) error
	MkdirAll(path string, perm fs.FileMode) error
	SystemctlStop(unit string) error
	SystemctlDaemonReload() error
	SystemctlEnable(unit string) error
	SystemctlDisable(unit string) error
	SystemctlStart(unit string) error
	// TarCreate archives src into a .tar.gz at dst.
	TarCreate(src, dst string) error
	// WorldFiles lists paths of world save files under dataDir.
	WorldFiles(dataDir string) ([]string, error)
}

// CutoverResult records what happened and where the rollback script was written.
type CutoverResult struct {
	RollbackScriptPath string
	BackupPath         string
	StartedAt          time.Time
	CompletedAt        time.Time
}

// ErrCutoverRolledBack is returned when cutover fails at a post-step-6 stage
// and rollback was executed automatically.
type ErrCutoverRolledBack struct {
	Step int
	Err  error
}

func (e *ErrCutoverRolledBack) Error() string {
	return fmt.Sprintf("cutover step %d failed (%v); rollback executed", e.Step, e.Err)
}

func (e *ErrCutoverRolledBack) Unwrap() error { return e.Err }

// worldFingerprint is the set of world save paths that must survive cutover.
type worldFingerprint map[string]struct{}

// CutoverPZ executes the 11-step cutover for a Project Zomboid server.
// Steps 1–5 are pre-commit: a failure there returns an error without rollback.
// Steps 6–11 have automatic rollback (ErrCutoverRolledBack).
func CutoverPZ(ctx context.Context, env CutoverEnv, disc *DiscoverResult, instanceYAML []byte) (*CutoverResult, error) {
	res := &CutoverResult{StartedAt: time.Now()}
	legacyUnit := disc.UnitName

	// ------ Steps 1–5: pre-commit (no automatic rollback) ------

	// Step 1: Verify discovery is complete enough to proceed.
	if disc.InstallDir == "" || disc.DataDir == "" || disc.ServerName == "" {
		return nil, fmt.Errorf("cutover: discovery incomplete (install=%q data=%q server=%q)",
			disc.InstallDir, disc.DataDir, disc.ServerName)
	}

	// Step 2: Stop the legacy server.
	if err := env.SystemctlStop(legacyUnit); err != nil {
		return nil, fmt.Errorf("cutover step 2: stop %s: %w", legacyUnit, err)
	}

	// Step 3: Record world fingerprint before backup.
	preFiles, err := env.WorldFiles(disc.DataDir)
	if err != nil {
		return nil, fmt.Errorf("cutover step 3: fingerprint world: %w", err)
	}
	fingerprint := make(worldFingerprint, len(preFiles))
	for _, p := range preFiles {
		fingerprint[p] = struct{}{}
	}

	// Step 4: Archive world data.
	backupDir := filepath.Join("/var/backups/pzserver")
	ts := time.Now().UTC().Format("20060102T150405Z")
	backupPath := filepath.Join(backupDir, "adopt-"+disc.ServerName+"-"+ts+".tar.gz")
	if err := env.MkdirAll(backupDir, 0750); err != nil {
		return nil, fmt.Errorf("cutover step 4: mkdir backup: %w", err)
	}
	if err := env.TarCreate(filepath.Join(disc.DataDir, "Saves"), backupPath); err != nil {
		return nil, fmt.Errorf("cutover step 4: backup: %w", err)
	}
	res.BackupPath = backupPath

	// Step 5: Write rollback script BEFORE any gvm state is created.
	rollbackDir := "/var/lib/gvm/adopt"
	if err := env.MkdirAll(rollbackDir, 0750); err != nil {
		return nil, fmt.Errorf("cutover step 5: mkdir rollback: %w", err)
	}
	rollbackPath := filepath.Join(rollbackDir, disc.ServerName+"-rollback.sh")
	rollbackScript := GenerateRollbackScript(disc.ServerName, legacyUnit, backupPath, disc.DataDir, ts)
	if err := env.WriteFile(rollbackPath, []byte(rollbackScript), 0750); err != nil {
		return nil, fmt.Errorf("cutover step 5: write rollback script: %w", err)
	}
	res.RollbackScriptPath = rollbackPath

	// ------ Steps 6–11: post-commit (automatic rollback on failure) ------

	rollback := func(step int, cause error) (*CutoverResult, error) {
		_ = env.SystemctlStop("gvm@" + disc.ServerName + ".service")
		_ = env.SystemctlEnable(legacyUnit)
		_ = env.SystemctlStart(legacyUnit)
		return nil, &ErrCutoverRolledBack{Step: step, Err: cause}
	}

	// Step 6: Write gvm instance YAML.
	instDir := "/etc/gvm/instances"
	if err := env.MkdirAll(instDir, 0750); err != nil {
		return rollback(6, fmt.Errorf("mkdir instances: %w", err))
	}
	instPath := filepath.Join(instDir, disc.ServerName+".yaml")
	if err := env.WriteFile(instPath, instanceYAML, 0640); err != nil {
		return rollback(6, fmt.Errorf("write instance yaml: %w", err))
	}

	// Step 7: Write systemd drop-in directory.
	dropinDir := "/etc/systemd/system/gvm@" + disc.ServerName + ".service.d"
	if err := env.MkdirAll(dropinDir, 0750); err != nil {
		return rollback(7, fmt.Errorf("mkdir dropin: %w", err))
	}
	dropinContent := generateDropin(disc)
	if err := env.WriteFile(filepath.Join(dropinDir, "gvm-adopt.conf"), []byte(dropinContent), 0640); err != nil {
		return rollback(7, fmt.Errorf("write dropin: %w", err))
	}

	// Step 8: Reload systemd.
	if err := env.SystemctlDaemonReload(); err != nil {
		return rollback(8, fmt.Errorf("daemon-reload: %w", err))
	}

	// Step 9: Start gvm unit.
	if err := env.SystemctlStart("gvm@" + disc.ServerName + ".service"); err != nil {
		return rollback(9, fmt.Errorf("start gvm unit: %w", err))
	}

	// Step 10: Verify world fingerprint (superset semantics: no pre-cutover file may vanish).
	postFiles, err := env.WorldFiles(disc.DataDir)
	if err != nil {
		return rollback(10, fmt.Errorf("post-cutover fingerprint: %w", err))
	}
	postSet := make(map[string]struct{}, len(postFiles))
	for _, p := range postFiles {
		postSet[p] = struct{}{}
	}
	var missing []string
	for p := range fingerprint {
		if _, ok := postSet[p]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return rollback(10, fmt.Errorf("world fingerprint: %d file(s) missing after cutover: %s",
			len(missing), strings.Join(missing, ", ")))
	}

	// Step 11: Disable legacy unit, enable gvm unit on boot.
	if err := env.SystemctlDisable(legacyUnit); err != nil {
		return rollback(11, fmt.Errorf("disable legacy unit: %w", err))
	}
	if err := env.SystemctlEnable("gvm@" + disc.ServerName + ".service"); err != nil {
		return rollback(11, fmt.Errorf("enable gvm unit: %w", err))
	}

	res.CompletedAt = time.Now()
	return res, nil
}

// OsCutoverEnv returns a CutoverEnv backed by the real OS.
func OsCutoverEnv() CutoverEnv { return &osCutoverEnv{} }

type osCutoverEnv struct{}

func (e *osCutoverEnv) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}

func (e *osCutoverEnv) MkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (e *osCutoverEnv) SystemctlStop(unit string) error {
	return runSystemctl("stop", unit)
}

func (e *osCutoverEnv) SystemctlDaemonReload() error {
	return runSystemctl("daemon-reload")
}

func (e *osCutoverEnv) SystemctlEnable(unit string) error {
	return runSystemctl("enable", unit)
}

func (e *osCutoverEnv) SystemctlDisable(unit string) error {
	return runSystemctl("disable", unit)
}

func (e *osCutoverEnv) SystemctlStart(unit string) error {
	return runSystemctl("start", unit)
}

func (e *osCutoverEnv) TarCreate(src, dst string) error {
	return runTar(src, dst)
}

func (e *osCutoverEnv) WorldFiles(dataDir string) ([]string, error) {
	return listSaveFiles(dataDir)
}

// generateDropin produces a minimal systemd drop-in for the adopted instance.
func generateDropin(disc *DiscoverResult) string {
	var b strings.Builder
	b.WriteString("[Service]\n")
	b.WriteString(fmt.Sprintf("# Adopted from %s on %s\n",
		disc.UnitName, time.Now().UTC().Format("2006-01-02")))
	return b.String()
}
