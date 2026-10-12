package jobs

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
)

// dirFiles is a filesystem of directories only, which is all ArchiveStore
// reads for itself: the archives are reached through the injected archive
// calls, and the only question this seam answers is "is there a directory
// here, and what instances does the root hold".
type dirFiles map[string][]string

func (d dirFiles) Stat(name string) (fs.FileInfo, error) {
	if _, ok := d[name]; !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return nil, nil
}

func (d dirFiles) ReadFile(name string) ([]byte, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (d dirFiles) ReadDir(name string) ([]fs.DirEntry, error) {
	ents, ok := d[name]
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	out := make([]fs.DirEntry, 0, len(ents))
	for _, e := range ents {
		out = append(out, dirEntry(e))
	}
	return out, nil
}

type dirEntry string

func (e dirEntry) Name() string               { return string(e) }
func (e dirEntry) IsDir() bool                { return !strings.Contains(string(e), ".") }
func (e dirEntry) Type() fs.FileMode          { return fs.ModeDir }
func (e dirEntry) Info() (fs.FileInfo, error) { return nil, nil }

func at(day int) time.Time { return time.Date(2026, 9, day, 4, 0, 0, 0, time.UTC) }

// storeRig is an ArchiveStore whose four archive calls are recorded.
type storeRig struct {
	store *ArchiveStore
	// verified are the paths VerifyRef was asked about, in order.
	verified []string
	// checked are the paths Verify was asked about, with the mode.
	checked []string
	// pruned is the ref Prune was handed, or nil if it was never called.
	pruned  *archive.Ref
	prunes  int
	notes   []string
	badRefs map[string]error
	badOnes map[string]error
}

func newStoreRig(entries ...archive.Entry) *storeRig {
	r := &storeRig{badRefs: map[string]error{}, badOnes: map[string]error{}}
	dir := "/var/backups/gvm/" + testInstance
	ix := &archive.Index{Schema: archive.SchemaVersion, Instance: testInstance, Entries: entries}
	r.store = &ArchiveStore{
		Root:  "/var/backups/gvm",
		Files: dirFiles{"/var/backups/gvm": {testInstance, "not a name!"}, dir: nil},
		Notes: func(s string) { r.notes = append(r.notes, s) },
		Now:   func() time.Time { return at(9) },
		Reindexer: func(context.Context, string, archive.ReindexOptions) (*archive.Index, error) {
			return ix, nil
		},
		Verifier: func(_ context.Context, p string, o archive.VerifyOptions) (*archive.Report, error) {
			r.checked = append(r.checked, p+":"+string(o.Mode))
			if err := r.badOnes[p]; err != nil {
				return nil, err
			}
			return &archive.Report{Path: p, Mode: o.Mode, Members: 3}, nil
		},
		Receipter: func(_ context.Context, ref *archive.Ref, o archive.VerifyOptions) (*archive.Report, error) {
			r.verified = append(r.verified, ref.Path)
			if err := r.badRefs[ref.Path]; err != nil {
				return nil, err
			}
			ref.VerifiedAt = at(9)
			return &archive.Report{Path: ref.Path, Mode: o.Mode}, nil
		},
		Retainer: func(_ context.Context, d string, p archive.Policy, after *archive.Ref) (*archive.PruneResult, error) {
			r.prunes++
			r.pruned = after
			return &archive.PruneResult{Dir: d, Removed: []string{"old-1", "old-2"}}, nil
		},
	}
	r.store.Dir = func(string) string { return dir }
	return r
}

func routine(file string, day int) archive.Entry {
	return archive.Entry{File: file, Class: archive.ClassRoutine, CreatedAt: at(day), Bytes: 1 << 20}
}

// TestAStandalonePruneVerifiesTheArchiveThatAuthorisesIt.
//
// archive.Prune refuses without a fresh, verified Ref, and a nightly `prune`
// job has written nothing. The receipt is therefore the NEWEST routine archive
// in the directory, read back first -- which is what makes a scheduled prune
// safe rather than a scheduled deletion.
func TestAStandalonePruneVerifiesTheArchiveThatAuthorisesIt(t *testing.T) {
	r := newStoreRig(routine("a-20260902.tar.gz", 2), routine("c-20260904.tar.gz", 4),
		routine("b-20260903.tar.gz", 3))

	removed, err := r.store.Prune(context.Background(), testInstance)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if len(r.verified) != 1 || !strings.HasSuffix(r.verified[0], "c-20260904.tar.gz") {
		t.Fatalf("verified %v; the receipt must be the NEWEST archive", r.verified)
	}
	if r.pruned == nil || !r.pruned.Verified() {
		t.Fatalf("prune ran with %+v; retention never runs without a verified receipt", r.pruned)
	}
}

// TestATruncatedNewestArchivePrunesNothing is the invariant backup.sh had by
// accident and this adapter has on purpose.
//
// If the newest archive does not read back, the older ones are the only intact
// history on the box -- and deleting them to honour a retention count would be
// the single worst thing this subsystem could do.
func TestATruncatedNewestArchivePrunesNothing(t *testing.T) {
	r := newStoreRig(routine("a-20260902.tar.gz", 2), routine("c-20260904.tar.gz", 4))
	r.badRefs[filepath.Join("/var/backups/gvm", testInstance, "c-20260904.tar.gz")] =
		errors.New("gzip: invalid checksum")

	removed, err := r.store.Prune(context.Background(), testInstance)
	if err == nil {
		t.Fatal("pruned on the strength of an archive that did not verify")
	}
	if !errors.Is(err, ErrNoReceipt) {
		t.Errorf("err = %v, want ErrNoReceipt", err)
	}
	if removed != 0 || r.prunes != 0 {
		t.Errorf("removed = %d after %d prune call(s); nothing may be deleted", removed, r.prunes)
	}
	if !strings.Contains(err.Error(), "c-20260904.tar.gz") {
		t.Errorf("err = %q; it does not name the archive that failed", err)
	}
}

// TestADirectoryWithOnlyPreserveArchivesPrunesNothing.
//
// A preserve archive is not part of the history retention manages (section
// 13.2: never pruned, by any code path), so it cannot be the receipt that
// authorises pruning either -- using one would let a rebuild's archive license
// the deletion of the nightlies.
func TestADirectoryWithOnlyPreserveArchivesPrunesNothing(t *testing.T) {
	r := newStoreRig(archive.Entry{
		File: "pre-rebuild.tar.gz", Class: archive.ClassPreserve, CreatedAt: at(4),
	})
	if _, err := r.store.Prune(context.Background(), testInstance); !errors.Is(err, ErrNoReceipt) {
		t.Fatalf("err = %v, want ErrNoReceipt", err)
	}
	if len(r.verified) != 0 {
		t.Errorf("verified %v; a preserve archive is not a receipt", r.verified)
	}
}

// TestTheIndexsMemoryOfAVerificationIsNotAReceipt.
//
// archive/index.go says the index is a CACHE. An entry carrying VerifiedAt
// from a check that ran a month ago must not stand in for reading the bytes
// now, because the failure being guarded against -- a file that has rotted
// since -- is precisely one the cache cannot see.
func TestTheIndexsMemoryOfAVerificationIsNotAReceipt(t *testing.T) {
	e := routine("c-20260904.tar.gz", 4)
	e.VerifiedAt = at(4)
	r := newStoreRig(e)
	r.badRefs[filepath.Join("/var/backups/gvm", testInstance, "c-20260904.tar.gz")] =
		errors.New("gzip: invalid checksum")

	if _, err := r.store.Prune(context.Background(), testInstance); err == nil {
		t.Fatal("the index's remembered verification authorised a prune")
	}
	if len(r.verified) != 1 {
		t.Errorf("verified %v; the archive was not read back", r.verified)
	}
}

// TestAnInstanceWithNoArchiveDirectoryIsNotAFailure: the state between
// `gvm create` and the first backup, which the nightly verify meets on every
// new instance.
func TestAnInstanceWithNoArchiveDirectoryIsNotAFailure(t *testing.T) {
	r := newStoreRig()
	r.store.Files = dirFiles{"/var/backups/gvm": {testInstance}}

	n, err := r.store.VerifyShallow(context.Background(), testInstance)
	if err != nil || n != 0 {
		t.Fatalf("VerifyShallow() = %d, %v; a directory that is not there yet is not damage", n, err)
	}
	if len(r.notes) == 0 {
		t.Error("nothing was recorded; a sweep that skipped an instance has to say so")
	}
}

// TestVerifyUsesShallowForEverythingAndDeepForTheNewestPreserve, which is
// section 13.2's cost decision: seconds per archive over all of them, minutes
// over the one a restore is most likely to need.
func TestVerifyUsesShallowForEverythingAndDeepForTheNewestPreserve(t *testing.T) {
	r := newStoreRig(
		routine("a-20260902.tar.gz", 2),
		archive.Entry{File: "pre-rebuild-20260903.tar.gz", Class: archive.ClassPreserve, CreatedAt: at(3)},
		archive.Entry{File: "pre-restore-20260905.tar.gz", Class: archive.ClassPreserve, CreatedAt: at(5)},
	)
	n, err := r.store.VerifyShallow(context.Background(), testInstance)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("checked %d, want all 3", n)
	}
	for _, c := range r.checked {
		if !strings.HasSuffix(c, ":"+string(archive.ModeShallow)) {
			t.Errorf("%s was not checked shallow", c)
		}
	}

	r.checked = nil
	n, err = r.store.VerifyNewestPreserve(context.Background(), testInstance)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("checked %d, want exactly the newest preserve", n)
	}
	if len(r.checked) != 1 || !strings.Contains(r.checked[0], "pre-restore-20260905") {
		t.Fatalf("checked %v, want the newest preserve archive", r.checked)
	}
	if !strings.HasSuffix(r.checked[0], ":"+string(archive.ModeDeep)) {
		t.Errorf("%s was not checked deep", r.checked[0])
	}
}

// TestOneCorruptArchiveDoesNotHideTheOthers: a nightly verification's value is
// the SHAPE of the damage, so the scan continues and every failure is named.
func TestOneCorruptArchiveDoesNotHideTheOthers(t *testing.T) {
	r := newStoreRig(routine("a.tar.gz", 2), routine("b.tar.gz", 3), routine("c.tar.gz", 4))
	dir := "/var/backups/gvm/" + testInstance
	r.badOnes[filepath.Join(dir, "a.tar.gz")] = errors.New("unexpected EOF")
	r.badOnes[filepath.Join(dir, "c.tar.gz")] = errors.New("gzip: invalid header")

	n, err := r.store.VerifyShallow(context.Background(), testInstance)
	if err == nil {
		t.Fatal("two corrupt archives verified clean")
	}
	if n != 1 {
		t.Errorf("checked = %d; the scan stopped at the first failure", n)
	}
	for _, want := range []string{"a.tar.gz", "c.tar.gz"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q; it does not name %s", err, want)
		}
	}
}

// TestInstancesComesFromTheFilesystemAndSkipsWhatIsNotOne.
//
// From /var/backups/gvm and never from a database: section 14.6 promises the
// nightly verify works with Postgres down. A stray directory is skipped with a
// note rather than refused, because /var/backups is somewhere operators put
// things and one of them must not stop the sweep.
func TestInstancesComesFromTheFilesystemAndSkipsWhatIsNotOne(t *testing.T) {
	r := newStoreRig()
	got, err := r.store.Instances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != testInstance {
		t.Errorf("instances = %v, want [%s]", got, testInstance)
	}
	if len(r.notes) == 0 {
		t.Error("the skipped directory was not reported")
	}
}
