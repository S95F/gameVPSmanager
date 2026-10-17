package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/archive"
)

// TestRestoreToExtractsIntoAScratchDir is Phase 38 item 1: `restore --to <dir>`
// extracts an archive somewhere to inspect it, touching no live world.
func TestRestoreToExtractsIntoAScratchDir(t *testing.T) {
	h := newTestHost(t)
	_, arc := backupFixture(t, "valheim-1", archive.ClassRoutine, "nightly")
	dest := filepath.Join(t.TempDir(), "r1") // absent, as --to normally is

	code, out, errb := runWith(t, h, "", "restore", arc, "--to", dest)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errb)
	}
	// backupFixture archives its world under the "data/" prefix, so the member
	// lands at <dest>/data/world.db.
	if _, err := os.Stat(filepath.Join(dest, "data", "world.db")); err != nil {
		t.Errorf("the archived world was not extracted under %s: %v", dest, err)
	}
	if !strings.Contains(out, "nothing live was touched") {
		t.Errorf("output does not say the extract touched no world:\n%s", out)
	}
}

// TestRestoreToRefusesADirThatOverlapsAnInstance is the one hazard the
// inspection path still has: a `--to` aimed at an instance's own tree would
// overwrite the live world. gvm refuses before writing a byte and names the
// instance path it hit.
func TestRestoreToRefusesADirThatOverlapsAnInstance(t *testing.T) {
	h := newTestHost(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "world", "data")
	seedInstance(t, h, "valheim-1", "valheim",
		"--install", filepath.Join(root, "world", "install"),
		"--data", dataDir, "--var", "public_name=Test")
	_, arc := backupFixture(t, "other-1", archive.ClassRoutine, "nightly")

	code, _, errb := runWith(t, h, "", "restore", arc, "--to", dataDir)
	if code != exitRefused {
		t.Fatalf("exit %d, want %d (refused)\n%s", code, exitRefused, errb)
	}
	if !strings.Contains(errb, "valheim-1") || !strings.Contains(errb, "data") {
		t.Errorf("the refusal does not name the overlapping instance path:\n%s", errb)
	}
}

// TestRestoreToRefusesANonEmptyDir: --to needs an absent or empty directory, so
// an extract never mixes its members with what is already there.
func TestRestoreToRefusesANonEmptyDir(t *testing.T) {
	h := newTestHost(t)
	_, arc := backupFixture(t, "valheim-1", archive.ClassRoutine, "nightly")
	dest := t.TempDir() // exists
	if err := os.WriteFile(filepath.Join(dest, "already-here.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errb := runWith(t, h, "", "restore", arc, "--to", dest)
	if code != exitUsage {
		t.Fatalf("exit %d, want %d (usage)\n%s", code, exitUsage, errb)
	}
	if !strings.Contains(errb, "not empty") {
		t.Errorf("the refusal does not say the directory is not empty:\n%s", errb)
	}
}
