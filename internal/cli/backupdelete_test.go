package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
)

// backupFixture writes a real backups directory holding one archive of name,
// and returns the directory and the archive's full path.
//
// The archive is real -- archive.Create writes a verifiable tarball -- because
// `gvm backup delete` reads the on-disk index exactly as the running tool does,
// and a hand-built index.json would test a different code path from the one an
// operator reaches.
func backupFixture(t *testing.T, name string, class archive.Class, reason string) (backups, archivePath string) {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "world")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "world.db"), []byte("world bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	backups = filepath.Join(root, "backups")
	if err := os.MkdirAll(backups, 0o755); err != nil {
		t.Fatal(err)
	}
	ref, err := archive.Create(context.Background(), archive.Options{
		Dir: backups, Instance: name, Class: class, Reason: reason,
		Now:     func() time.Time { return testTime },
		Sources: []archive.Source{{Prefix: "data/", Base: src}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return backups, ref.Path
}

// verifyArchiveInIndex reads the archive back and records the verification in
// the directory's index, which is what sets VerifiedAt -- the signal the
// archive-first guard keys on.
func verifyArchiveInIndex(t *testing.T, backups, archivePath string) {
	t.Helper()
	ctx := context.Background()
	now := func() time.Time { return testTime }
	ix, err := archive.Reindex(ctx, backups, archive.ReindexOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	e, ok := ix.Find(filepath.Base(archivePath))
	if !ok {
		t.Fatalf("archive %s is not in the index", archivePath)
	}
	ref := e.Ref(backups)
	if _, err := archive.VerifyRef(ctx, ref, archive.VerifyOptions{
		Mode: archive.ModeSpot, Scratch: t.TempDir(), Now: now,
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	ix.Note(ref)
	if err := ix.Save(backups); err != nil {
		t.Fatal(err)
	}
}

// TestBackupDeleteRefusesAWrongTypedName: the typed-name gate is the whole
// safety of the verb, and a wrong name is a refusal (exit 5), not a failure --
// the operator was asked and did not confirm. Nothing is removed.
func TestBackupDeleteRefusesAWrongTypedName(t *testing.T) {
	h := newTestHost(t)
	// The instance is gone, so the archive-first guard does not pre-empt the
	// confirmation we are trying to exercise.
	_, arc := backupFixture(t, "valheim-1", archive.ClassPreserve, "pre-rebuild")

	code, _, errb := runWith(t, h, "", "backup", "delete", arc, "--confirm", "not-the-name")
	if code != exitRefused {
		t.Fatalf("exit %d, want %d (refused)\n%s", code, exitRefused, errb)
	}
	if _, err := os.Stat(arc); err != nil {
		t.Errorf("a refused delete removed the archive anyway: %v", err)
	}
}

// TestBackupDeleteRemovesAPreserveArchiveWithTheTypedName is the DoD line: the
// one explicit way to reclaim a preserve archive. It removes the tarball and its
// pin sidecar, and for an instance that is gone it says this was the last copy.
func TestBackupDeleteRemovesAPreserveArchiveWithTheTypedName(t *testing.T) {
	h := newTestHost(t)
	backups, arc := backupFixture(t, "valheim-1", archive.ClassRoutine, "pre-rebuild")
	// A durable preserve pin, as a destroy writes: retention will never touch
	// this, so `backup delete` is the only way it ever goes.
	if err := archive.WritePreservePin(arc, archive.PreservePin{
		Reason: "destroy-authorised", PinnedAt: testTime, Instance: "valheim-1",
	}); err != nil {
		t.Fatal(err)
	}

	code, out, errb := runWith(t, h, "", "backup", "delete", arc, "--confirm", "valheim-1")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errb)
	}
	if _, err := os.Stat(arc); !os.IsNotExist(err) {
		t.Errorf("the archive was not removed: %v", err)
	}
	if _, err := os.Stat(archive.PreserveSidecarPath(arc)); !os.IsNotExist(err) {
		t.Errorf("the preserve pin survived, so a reindex would resurrect it: %v", err)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("output does not confirm the removal:\n%s", out)
	}
	if !strings.Contains(out, "last copy of valheim-1") {
		t.Errorf("a gone instance's last archive did not say so:\n%s", out)
	}
	// And `backup list` no longer shows it.
	code, out, _ = runWith(t, h, "", "backup", "list", "--from", backups)
	if code != 0 {
		t.Fatalf("backup list exit %d", code)
	}
	if strings.Contains(out, filepath.Base(arc)) {
		t.Errorf("backup list still shows the deleted archive:\n%s", out)
	}
}

// TestBackupDeleteRefusesTheNewestVerifiedCopyOfALiveInstance is archive-first:
// a world that still exists must keep at least one verified copy, so the newest
// verified archive is refused (exit 5) and removed BEFORE the operator is even
// asked to type the name -- a correct name does not get past it.
func TestBackupDeleteRefusesTheNewestVerifiedCopyOfALiveInstance(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")
	backups, arc := backupFixture(t, "valheim-1", archive.ClassRoutine, "nightly")
	verifyArchiveInIndex(t, backups, arc)

	code, out, errb := runWith(t, h, "", "backup", "delete", arc, "--confirm", "valheim-1")
	if code != exitRefused {
		t.Fatalf("exit %d, want %d (refused)\n%s\n%s", code, exitRefused, out, errb)
	}
	if !strings.Contains(errb, "newest verified") {
		t.Errorf("the refusal does not explain the archive-first guard:\n%s", errb)
	}
	if _, err := os.Stat(arc); err != nil {
		t.Errorf("a refused delete removed the only verified copy of a live world: %v", err)
	}
}

// TestBackupDeleteDryRunRemovesNothing: --dry-run measures and reports, like
// every other dry run in gvm, and the archive is untouched.
func TestBackupDeleteDryRunRemovesNothing(t *testing.T) {
	h := newTestHost(t)
	_, arc := backupFixture(t, "valheim-1", archive.ClassPreserve, "pre-rebuild")

	code, out, errb := runWith(t, h, "", "backup", "delete", arc, "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errb)
	}
	if !strings.Contains(out, "nothing was removed") {
		t.Errorf("the dry run does not say it removed nothing:\n%s", out)
	}
	if _, err := os.Stat(arc); err != nil {
		t.Errorf("--dry-run removed the archive: %v", err)
	}
}
