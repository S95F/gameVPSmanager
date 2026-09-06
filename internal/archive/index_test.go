package archive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReindexIsADBFreeListing is section 14.6's requirement in one test:
// everything `gvm restore --list` needs comes off the disk, from the archives
// themselves, with no database configured at all.
func TestReindexIsADBFreeListing(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})

	routine := makeArchive(t, dir, src, ClassRoutine, "nightly", fixed)
	preserve := makeArchive(t, dir, src, ClassPreserve, "pre-rebuild", fixed.Add(time.Hour))

	// Two things that are not gvm archives, to prove the listing is not just
	// os.ReadDir with a suffix filter.
	legacyBytes, err := os.ReadFile(filepath.Join("testdata", "legacy-pz-backup.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "pz-servertest-20260101-030000.tar.gz")
	if err := os.WriteFile(legacy, legacyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// A legacy tarball's filename carries a stamp nothing can parse, so mtime
	// is the only honest ordering key it has. Backdating it here is what a
	// real one looks like: older than everything gvm has written since.
	old := fixed.AddDate(0, -8, 0)
	if err := os.Chtimes(legacy, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	ix, err := Reindex(context.Background(), dir, ReindexOptions{Now: func() time.Time { return fixed.Add(2 * time.Hour) }})
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if len(ix.Entries) != 3 {
		t.Fatalf("index holds %d entries, want the three tarballs and not notes.txt: %+v", len(ix.Entries), ix.Entries)
	}
	// Newest first, which is the order every listing wants and the order
	// retention walks.
	if ix.Entries[0].File != filepath.Base(preserve.Path) {
		t.Errorf("the index is not newest-first: %s came first", ix.Entries[0].File)
	}

	byName := map[string]Entry{}
	for _, e := range ix.Entries {
		byName[e.File] = e
	}
	if got := byName[filepath.Base(preserve.Path)]; got.Class != ClassPreserve || got.Reason != "pre-rebuild" {
		t.Errorf("preserve entry = %+v", got)
	}
	if got := byName[filepath.Base(routine.Path)]; got.Class != ClassRoutine || got.SHA256 != routine.SHA256 {
		t.Errorf("routine entry = %+v, want sha %s", got, routine.SHA256)
	}
	if got := byName[filepath.Base(legacy)]; !got.Legacy || got.Class != "" {
		t.Errorf("the bash-suite tarball was not marked legacy: %+v", got)
	}

	// The file on disk is what a second process reads.
	reloaded, err := LoadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Entries) != len(ix.Entries) {
		t.Fatalf("the saved index holds %d entries, the in-memory one %d", len(reloaded.Entries), len(ix.Entries))
	}
	fi, err := os.Stat(IndexPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	samePerm(t, IndexPath(dir), fi.Mode(), platformPerm(ArchiveFileMode))
	// No .bak: a second file in a directory retention globs is a file
	// somebody will eventually mistake for an archive.
	if _, err := os.Stat(IndexPath(dir) + ".bak"); err == nil {
		t.Error("the index left a .bak beside the archives")
	}
}

// TestReindexReusesADigestItAlreadyHas keeps a nightly reindex from re-reading
// every archive on the box.
func TestReindexReusesADigestItAlreadyHas(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})
	ref := makeArchive(t, dir, src, ClassRoutine, "nightly", fixed)

	first, err := Reindex(context.Background(), dir, ReindexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Entries[0].SHA256 != ref.SHA256 {
		t.Fatalf("index sha %s, archive sha %s", first.Entries[0].SHA256, ref.SHA256)
	}

	// Poison the cached digest without touching the file, then reindex. A
	// reindex that re-hashes unconditionally would repair it; the caching one
	// must not, which is exactly the behaviour being pinned.
	first.Entries[0].SHA256 = strings.Repeat("9", 64)
	if err := first.Save(dir); err != nil {
		t.Fatal(err)
	}
	cached, err := Reindex(context.Background(), dir, ReindexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cached.Entries[0].SHA256 != strings.Repeat("9", 64) {
		t.Error("Reindex re-hashed a file whose size and mtime had not changed; on a box with " +
			"twenty 2.4 GB nightlies that is 48 GB of reading per run")
	}

	// --rehash is the repair, and so is a file that actually changed.
	fresh, err := Reindex(context.Background(), dir, ReindexOptions{Rehash: true})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Entries[0].SHA256 != ref.SHA256 {
		t.Errorf("Rehash did not recompute: %s", fresh.Entries[0].SHA256)
	}
}

// TestReindexSurvivesACorruptIndex: rebuilding the cache is the repair, and
// refusing to would leave the operator with a broken listing and no way to
// replace it.
func TestReindexSurvivesACorruptIndex(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})
	makeArchive(t, dir, src, ClassRoutine, "nightly", fixed)

	if err := os.WriteFile(IndexPath(dir), []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var notes []string
	ix, err := Reindex(context.Background(), dir, ReindexOptions{OnNote: func(s string) { notes = append(notes, s) }})
	if err != nil {
		t.Fatalf("Reindex over a corrupt index = %v", err)
	}
	if len(ix.Entries) != 1 {
		t.Fatalf("index holds %d entries, want 1", len(ix.Entries))
	}
	if len(notes) == 0 {
		t.Error("the corrupt index was discarded silently")
	}
}

// TestEntryRefCannotVouchForBytesNobodyRead: the index is a cache, and a cache
// must not be able to authorize a destroy.
func TestEntryRefCannotVouchForBytesNobodyRead(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})
	ref := makeArchive(t, dir, src, ClassRoutine, "nightly", fixed)

	ix, err := Reindex(context.Background(), dir, ReindexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fromIndex := ix.Entries[0].Ref(dir)
	if fromIndex.Verified() {
		t.Fatal("a Ref rebuilt from the index claimed to be verified. Nothing has read those " +
			"bytes back, and that Ref would authorize a destroy")
	}

	// Once a verification happens, the index remembers it.
	mustVerify(t, ref, ModeShallow)
	ix.Note(ref)
	if err := ix.Save(dir); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Entries[0].Ref(dir).Verified() {
		t.Fatal("the index did not remember the verification")
	}
}
