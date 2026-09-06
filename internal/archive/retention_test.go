package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// makeArchive writes one archive at a nominated instant.
func makeArchive(t *testing.T, dir, src string, class Class, reason string, at time.Time) *Ref {
	t.Helper()
	ref, err := Create(context.Background(), Options{
		Dir: dir, Instance: "pz-main", Class: class, Reason: reason,
		Now:     func() time.Time { return at },
		Sources: []Source{{Prefix: "data/", Base: src}},
	})
	if err != nil {
		t.Fatalf("Create(%s at %s): %v", class, at, err)
	}
	return ref
}

func archivesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), Ext) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// TestPruneNeverTouchesPreserve is the one retention rule with no exceptions.
//
// rebuild.sh promised "kept until you delete it -- not pruned by backup
// retention" in a log line. Here it is a property of the code, and --force is
// tested explicitly because --force is precisely the moment somebody would
// otherwise talk themselves into it.
func TestPruneNeverTouchesPreserve(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})

	preserve := makeArchive(t, dir, src, ClassPreserve, "pre-rebuild", fixed)
	old1 := makeArchive(t, dir, src, ClassRoutine, "nightly", fixed.Add(1*time.Hour))
	old2 := makeArchive(t, dir, src, ClassRoutine, "nightly", fixed.Add(2*time.Hour))
	fresh := makeArchive(t, dir, src, ClassRoutine, "nightly", fixed.Add(3*time.Hour))
	mustVerify(t, fresh, ModeShallow)

	for _, tc := range []struct {
		name   string
		policy Policy
	}{
		{"keep 1", Policy{Keep: 1, Now: func() time.Time { return fixed.Add(4 * time.Hour) }}},
		{"force", Policy{Keep: 1, Force: true, Now: func() time.Time { return fixed.Add(4 * time.Hour) }}},
		{"keep_days 0 and force", Policy{Force: true, Now: func() time.Time { return fixed.Add(4000 * time.Hour) }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Prune(context.Background(), dir, tc.policy, fresh)
			if err != nil {
				t.Fatalf("Prune: %v", err)
			}
			if _, serr := os.Stat(preserve.Path); serr != nil {
				t.Fatalf("the preserve archive was pruned by %q: %v. It is the world as it stood "+
					"the moment before a rebuild deleted it, and no code path may reclaim it",
					tc.name, serr)
			}
			if len(res.Preserved) != 1 || res.PreservedBytes == 0 {
				t.Errorf("Preserved = %v (%d bytes); doctor reports this so an operator notices "+
					"before the disk fills", res.Preserved, res.PreservedBytes)
			}
			if _, serr := os.Stat(fresh.Path); serr != nil {
				t.Fatalf("the archive that authorised the prune was pruned by it: %v", serr)
			}
		})
	}

	// The routine ones did go, which is what makes the test above mean
	// something: prune is not simply broken.
	for _, gone := range []*Ref{old1, old2} {
		if _, err := os.Stat(gone.Path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived --force; retention is not running at all", filepath.Base(gone.Path))
		}
	}
}

// TestPruneWithNoWindowKeepsEverything pins the reading of a zero Policy.
//
// Policy documents Keep:0 as "no count limit" and KeepDays:0 as "no age
// limit", and nothing upstream fills either in: spec.BackupPolicy's fields are
// plain ints with no loader default and no validation, so an adopted or
// hand-written /etc/gvm/instances/<name>.yaml with no backup block reaches
// retention with both at zero. Two absent windows have to mean "nothing has
// been crossed", because the alternative reading makes a zero policy identical
// to --force on precisely the instance whose operator believed they had
// configured no retention at all.
func TestPruneWithNoWindowKeepsEverything(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})

	var refs []*Ref
	for i := range 4 {
		refs = append(refs, makeArchive(t, dir, src, ClassRoutine, "nightly", fixed.Add(time.Duration(i)*time.Hour)))
	}
	fresh := refs[3]
	mustVerify(t, fresh, ModeShallow)

	res, err := Prune(context.Background(), dir, Policy{
		Now: func() time.Time { return fixed.AddDate(0, 0, 400) },
	}, fresh)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v for a policy declaring no count limit and no age limit; "+
			"an unset backup block must not act as --force", res.Removed)
	}
	if got := archivesIn(t, dir); len(got) != 4 {
		t.Fatalf("archives = %v, want all four still on disk", got)
	}
	for _, r := range refs {
		if _, err := os.Stat(r.Path); err != nil {
			t.Errorf("%s was pruned by an empty policy: %v", filepath.Base(r.Path), err)
		}
	}
}

// TestPruneOnlyAfterSuccess is the invariant backup.sh had by accident.
//
// Its prune ran on a schedule after its tar; the moment the tar failed -- disk
// full, world locked, steamcmd holding a file -- the prune would still run and
// eat the good archives. Making the fresh receipt an argument means a failing
// create returns nil and the caller has nothing to pass.
func TestPruneOnlyAfterSuccess(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})

	makeArchive(t, dir, src, ClassRoutine, "nightly", fixed)
	makeArchive(t, dir, src, ClassRoutine, "nightly", fixed.Add(time.Hour))
	makeArchive(t, dir, src, ClassRoutine, "nightly", fixed.Add(2*time.Hour))
	before := archivesIn(t, dir)
	if len(before) != 3 {
		t.Fatalf("setup wrote %d archives, want 3", len(before))
	}

	// Tonight's backup fails: the include names a path that is not there.
	failed, err := Create(context.Background(), Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed.Add(3 * time.Hour) },
		Sources: []Source{{Prefix: "data/", Base: src, Include: []string{"Saves/Multiplayer/servertest"}}},
	})
	if err == nil {
		t.Fatal("Create over a missing include reported success")
	}
	if failed != nil {
		t.Fatal("a failed Create returned a Ref, which is exactly the value that would then " +
			"authorise a prune")
	}

	res, err := Prune(context.Background(), dir, Policy{Keep: 1}, failed)
	if !errors.Is(err, ErrNoFreshArchive) {
		t.Fatalf("Prune after a failed create = %v, want ErrNoFreshArchive", err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("a refused prune still removed %v", res.Removed)
	}
	if got := archivesIn(t, dir); len(got) != 3 {
		t.Fatalf("the archive set is now %v, want the three that were there; a failing backup "+
			"must never erode the history it failed to add to", got)
	}

	// And the same refusal for an archive that was written but never read back.
	written := makeArchive(t, dir, src, ClassRoutine, "nightly", fixed.Add(4*time.Hour))
	if written.Verified() {
		t.Fatal("Create returned a verified Ref")
	}
	if _, err := Prune(context.Background(), dir, Policy{Keep: 1}, written); !errors.Is(err, ErrNoFreshArchive) {
		t.Fatalf("Prune after an unverified create = %v, want ErrNoFreshArchive: deleting "+
			"yesterday's world to make room for a truncated one is the failure this ordering "+
			"exists to prevent", err)
	}
	if got := archivesIn(t, dir); len(got) != 4 {
		t.Fatalf("the archive set is now %v, want all four", got)
	}
}

// TestPruneRefusesAnArchiveFromAnotherDirectory closes the obvious way around
// the receipt: passing last week's verified Ref from a different instance.
func TestPruneRefusesAnArchiveFromAnotherDirectory(t *testing.T) {
	src, dirA, dirB := t.TempDir(), t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})

	makeArchive(t, dirA, src, ClassRoutine, "nightly", fixed)
	makeArchive(t, dirA, src, ClassRoutine, "nightly", fixed.Add(time.Hour))
	elsewhere := makeArchive(t, dirB, src, ClassRoutine, "nightly", fixed.Add(2*time.Hour))
	mustVerify(t, elsewhere, ModeShallow)

	if _, err := Prune(context.Background(), dirA, Policy{Keep: 1}, elsewhere); !errors.Is(err, ErrNoFreshArchive) {
		t.Fatalf("Prune = %v, want ErrNoFreshArchive for a receipt from another directory", err)
	}
	if got := archivesIn(t, dirA); len(got) != 2 {
		t.Fatalf("archives in dirA = %v, want both untouched", got)
	}
}

// TestPruneKeepAndKeepDays checks the two windows compose the way section 13.2
// describes: keep the N newest, then let age decide about the rest.
func TestPruneKeepAndKeepDays(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})

	// Five nightlies, one a day.
	var refs []*Ref
	for i := range 5 {
		refs = append(refs, makeArchive(t, dir, src, ClassRoutine, "nightly", fixed.AddDate(0, 0, i)))
	}
	fresh := refs[4]
	mustVerify(t, fresh, ModeShallow)

	// keep:2 holds the fresh one plus the next newest; keep_days:3, measured
	// from the day after the last archive, spares one more.
	res, err := Prune(context.Background(), dir, Policy{
		Keep: 2, KeepDays: 3,
		Now: func() time.Time { return fixed.AddDate(0, 0, 5) },
	}, fresh)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Removed) != 2 {
		t.Fatalf("Removed = %v, want the two oldest: keep:2 spares the newest two and "+
			"keep_days:3 spares the third", res.Removed)
	}
	for _, gone := range refs[:2] {
		if _, err := os.Stat(gone.Path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived", filepath.Base(gone.Path))
		}
	}
	for _, kept := range refs[2:] {
		if _, err := os.Stat(kept.Path); err != nil {
			t.Errorf("%s was pruned: %v", filepath.Base(kept.Path), err)
		}
	}
}

// TestPruneLeavesArchivesGvmDidNotWrite: a bash-suite tarball in the backup
// directory is somebody else's, and deleting somebody else's backups on a
// schedule is not a feature.
func TestPruneLeavesArchivesGvmDidNotWrite(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})

	legacy := filepath.Join(dir, "pz-servertest-20260101-030000.tar.gz")
	b, err := os.ReadFile(filepath.Join("testdata", "legacy-pz-backup.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, b, 0o600); err != nil {
		t.Fatal(err)
	}

	makeArchive(t, dir, src, ClassRoutine, "nightly", fixed)
	fresh := makeArchive(t, dir, src, ClassRoutine, "nightly", fixed.Add(time.Hour))
	mustVerify(t, fresh, ModeShallow)

	res, err := Prune(context.Background(), dir, Policy{Force: true}, fresh)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("--force deleted a bash-suite tarball gvm did not write: %v", err)
	}
	if len(res.Unmanaged) != 1 || res.Unmanaged[0] != filepath.Base(legacy) {
		t.Errorf("Unmanaged = %v, want the legacy tarball named so the operator can decide", res.Unmanaged)
	}
}

// TestDeleteArchiveRefusesPreserve keeps the rule true for the single-file
// path too, so a future caller reaching for os.Remove has somewhere correct to
// reach instead.
func TestDeleteArchiveRefusesPreserve(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})
	preserve := makeArchive(t, dir, src, ClassPreserve, "pre-rebuild", fixed)
	routine := makeArchive(t, dir, src, ClassRoutine, "nightly", fixed.Add(time.Hour))

	if err := DeleteArchive(preserve.Path); !errors.Is(err, ErrPreserveIsNeverPruned) {
		t.Fatalf("DeleteArchive(preserve) = %v, want ErrPreserveIsNeverPruned", err)
	}
	if _, err := os.Stat(preserve.Path); err != nil {
		t.Fatalf("the preserve archive was removed anyway: %v", err)
	}
	if err := DeleteArchive(routine.Path); err != nil {
		t.Fatalf("DeleteArchive(routine) = %v, want it to work", err)
	}
}

// TestDeleteArchiveRefusesAPreserveItCannotRead.
//
// The guard only fires when the head manifest PARSES, so every peek failure --
// a truncation, garbage gzip, ErrNoManifest, ErrHeadTooLarge -- falls through
// to an unconditional os.Remove, with the class sitting in plain sight in the
// filename gvm itself wrote. That inverts the rest of the package, where an
// unreadable archive is reported (index sets Unreadable) or routed to Unmanaged
// rather than deleted, and it inverts the one rule with no exceptions: a
// preserve archive is the world as it stood the moment before a rebuild deleted
// it, and only a human with rm may reclaim one.
func TestDeleteArchiveRefusesAPreserveItCannotRead(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})

	truncate := func(t *testing.T, path string, n int64) {
		t.Helper()
		if err := os.Truncate(path, n); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a truncated preserve is refused", func(t *testing.T) {
		preserve := makeArchive(t, dir, src, ClassPreserve, "pre-rebuild", fixed)
		truncate(t, preserve.Path, 100)
		if err := DeleteArchive(preserve.Path); !errors.Is(err, ErrPreserveIsNeverPruned) {
			t.Fatalf("DeleteArchive = %v, want ErrPreserveIsNeverPruned", err)
		}
		if _, err := os.Stat(preserve.Path); err != nil {
			t.Fatalf("the preserve archive was removed anyway: %v", err)
		}
	})

	t.Run("a name gvm did not write is refused", func(t *testing.T) {
		// A bash-suite tarball or a hand-renamed file: gvm cannot establish its
		// class at all, and Prune already leaves this shape in Unmanaged.
		foreign := filepath.Join(dir, "pz-servertest-20260101-030000.tar.gz")
		if err := os.WriteFile(foreign, []byte("not a gvm archive"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := DeleteArchive(foreign); !errors.Is(err, ErrClassUnknown) {
			t.Fatalf("DeleteArchive = %v, want ErrClassUnknown", err)
		}
		if _, err := os.Stat(foreign); err != nil {
			t.Fatalf("a file gvm did not write was removed: %v", err)
		}
	})

	t.Run("a truncated ROUTINE archive still deletes", func(t *testing.T) {
		// The refusal must not widen into "refuse everything unreadable": a
		// corrupt nightly is something retention would reclaim anyway, and
		// stranding it would leave no in-gvm way to clear it.
		routine := makeArchive(t, dir, src, ClassRoutine, "nightly", fixed.Add(2*time.Hour))
		truncate(t, routine.Path, 100)
		if err := DeleteArchive(routine.Path); err != nil {
			t.Fatalf("DeleteArchive(corrupt routine) = %v, want it to work", err)
		}
	})

	t.Run("deletion stays idempotent", func(t *testing.T) {
		// An absent file returns the natural fs error, not a class refusal, so
		// a caller retrying a partly-completed prune is not told it is
		// forbidden from something already done.
		err := DeleteArchive(filepath.Join(dir, "pz-main-20260101-000000-routine.tar.gz"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("DeleteArchive(absent) = %v, want fs.ErrNotExist", err)
		}
	})
}

// TestDryRunPrunesNothing: the plan an operator is shown must not be the act.
func TestDryRunPrunesNothing(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})
	makeArchive(t, dir, src, ClassRoutine, "nightly", fixed)
	fresh := makeArchive(t, dir, src, ClassRoutine, "nightly", fixed.Add(time.Hour))
	mustVerify(t, fresh, ModeShallow)

	before := archivesIn(t, dir)
	res, err := Prune(context.Background(), dir, Policy{Keep: 1, DryRun: true}, fresh)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Removed) != 1 {
		t.Fatalf("dry run planned %v, want one removal", res.Removed)
	}
	if got := archivesIn(t, dir); len(got) != len(before) {
		t.Fatalf("the dry run deleted something: %v -> %v", before, got)
	}
}
