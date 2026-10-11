package provider

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sha256Of(b []byte) Digest {
	sum := sha256.Sum256(b)
	return NewDigest(AlgoSHA256, sum[:])
}

func mustPut(t *testing.T, s *Store, b []byte) (Digest, instance.Jailed) {
	t.Helper()
	d := sha256Of(b)
	j, err := s.Put(context.Background(), bytes.NewReader(b), d)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !j.Valid() {
		t.Fatal("Put returned an unjailed path")
	}
	return d, j
}

// overwrite replaces a published blob's bytes in place, which is what bit rot,
// a half-finished copy from an older gvm, and a substituted file all look like
// from the store's side.
func overwrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, BlobMode); err != nil {
		t.Fatal(err)
	}
}

// wantNoPath asserts the half of the guarantee a careless caller depends on:
// the error says no, AND the value carries nothing usable. A caller that
// ignores err must not come away holding a path.
func wantNoPath(t *testing.T, j instance.Jailed, err error, target error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %v, got no error", target)
	}
	if !errors.Is(err, target) {
		t.Fatalf("expected %v, got %v", target, err)
	}
	if j.Valid() {
		t.Error("the refusal returned a valid Jailed; a caller ignoring the error would use it")
	}
	if j.Path() != "" {
		t.Errorf("the refusal returned path %q", j.Path())
	}
}

// TestBlobStoreRejectsMismatch is section 18.2's named gate: unverified bytes
// are never used.
func TestBlobStoreRejectsMismatch(t *testing.T) {
	s := newStore(t)
	d, j := mustPut(t, s, []byte("the real mod jar"))
	overwrite(t, j.Path(), []byte("a substituted mod jar"))

	got, err := s.Get(context.Background(), d)
	wantNoPath(t, got, err, ErrDigestMismatch)
	if !Terminal(err) {
		t.Error("a digest mismatch must be terminal: retrying cannot make these bytes correct")
	}
	// The message has to name what was found, or the operator cannot tell a
	// corrupted download from a changed upstream file.
	if !strings.Contains(err.Error(), "holds sha256:") {
		t.Errorf("mismatch error does not report the digest found: %v", err)
	}
}

func TestPutWithAMismatchedDigestStoresNothing(t *testing.T) {
	s := newStore(t)
	want := sha256Of([]byte("what the api promised"))

	j, err := s.Put(context.Background(), strings.NewReader("what the cdn actually sent"), want)
	wantNoPath(t, j, err, ErrDigestMismatch)

	if s.Has(want) {
		t.Error("Has reports the blob is present after a refused Put")
	}
	if n := countFiles(t, s.root); n != 0 {
		t.Errorf("a refused Put left %d file(s) under %s, staging included", n, s.root)
	}
}

func TestPutStoresNothingWhenTheDownloadFailsPartway(t *testing.T) {
	s := newStore(t)
	body := bytes.Repeat([]byte("a"), 4096)
	want := sha256Of(body)

	r := io.MultiReader(bytes.NewReader(body[:1000]), errReader{errors.New("connection reset")})
	j, err := s.Put(context.Background(), r, want)
	if err == nil {
		t.Fatal("a truncated download was accepted")
	}
	if j.Valid() {
		t.Error("a truncated download returned a path")
	}
	if n := countFiles(t, s.root); n != 0 {
		t.Errorf("a failed Put left %d file(s) behind", n)
	}
}

func TestPutReplacesACorruptBlobSoTheCacheHeals(t *testing.T) {
	s := newStore(t)
	body := []byte("mod jar v1")
	d, j := mustPut(t, s, body)
	overwrite(t, j.Path(), []byte("rot"))

	// The repair path is the ordinary fetch path: no separate "quarantine"
	// verb to remember, and Put does not short-circuit on Has.
	if _, err := s.Put(context.Background(), bytes.NewReader(body), d); err != nil {
		t.Fatalf("re-Put of a corrupt blob: %v", err)
	}
	if _, err := s.Get(context.Background(), d); err != nil {
		t.Fatalf("Get after repair: %v", err)
	}
}

// Publishing creates a new inode. Re-putting bytes that are already stored
// would therefore orphan every hardlink an instance holds into that blob --
// mods/x.jar would keep pointing at the old inode, and the disk would hold
// both copies until a GC that can no longer see either of them.
func TestPutOfBytesAlreadyStoredKeepsExistingHardlinks(t *testing.T) {
	s := newStore(t)
	body := []byte("jei-1.20.1-15.2.0.27.jar")
	d, blob := mustPut(t, s, body)

	dst := destIn(t, "mods/jei.jar")
	if err := s.Link(d, dst); err != nil {
		t.Fatalf("Link: %v", err)
	}
	linked, err := os.Stat(dst.Path())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Put(context.Background(), bytes.NewReader(body), d); err != nil {
		t.Fatalf("re-Put of identical bytes: %v", err)
	}
	republished, err := os.Stat(blob.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(linked, republished) {
		t.Error("re-Put replaced the blob's inode, orphaning the hardlink into it")
	}
}

func TestGetOnAnAbsentBlobIsNotAMismatch(t *testing.T) {
	s := newStore(t)
	j, err := s.Get(context.Background(), sha256Of([]byte("never stored")))
	wantNoPath(t, j, err, ErrBlobNotFound)
	// The two have different fixes: fetch it, versus stop trusting it.
	if Terminal(err) {
		t.Error("an absent blob must not be terminal: fetching it is the whole point")
	}
}

func TestRoundTripAcrossEveryAlgorithmTheSourcesPublish(t *testing.T) {
	s := newStore(t)
	body := []byte("fabric-api-0.92.2.jar")

	s256 := sha256.Sum256(body)
	s512 := sha512.Sum512(body)
	s1 := sha1.Sum(body)

	for _, d := range []Digest{
		NewDigest(AlgoSHA256, s256[:]),
		NewDigest(AlgoSHA512, s512[:]),
		NewDigest(AlgoSHA1, s1[:]),
	} {
		t.Run(d.Algo(), func(t *testing.T) {
			if _, err := s.Put(context.Background(), bytes.NewReader(body), d); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if !s.Has(d) {
				t.Error("Has is false after a successful Put")
			}
			j, err := s.Get(context.Background(), d)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			got, err := os.ReadFile(j.Path())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, body) {
				t.Error("the stored bytes are not the bytes that were put")
			}
			// The address is derived from the digest, so one set of bytes has
			// exactly one home per algorithm.
			if rel, _ := filepath.Rel(s.root, j.Path()); filepath.ToSlash(rel) !=
				d.Algo()+"/"+d.Value()[:2]+"/"+d.Value() {
				t.Errorf("unexpected layout %s", rel)
			}
		})
	}
}

// A buildid names an install SteamCMD wrote itself; a tree digest names a
// directory the game downloaded. Neither is a file this store can hash, and
// the refusal has to be explicit: falling back to "the file is there" is how
// an unverifiable identity becomes a verified one.
func TestTheStoreRefusesIdentitiesItCannotHash(t *testing.T) {
	s := newStore(t)
	tree := "tree-sha256:" + strings.Repeat("ab", 32)
	for _, d := range []Digest{"buildid:380870:18452901", Digest(tree)} {
		t.Run(d.Algo(), func(t *testing.T) {
			if _, err := ParseDigest(d); err != nil {
				t.Fatalf("%s is a legitimate digest shape: %v", d, err)
			}
			if d.Verifiable() {
				t.Fatal("this digest claims to be hashable")
			}
			j, err := s.Get(context.Background(), d)
			wantNoPath(t, j, err, ErrNotByteDigest)
			if !Terminal(err) {
				t.Error("asking the blob store again will not make it hashable")
			}
			j, err = s.Put(context.Background(), strings.NewReader("x"), d)
			wantNoPath(t, j, err, ErrNotByteDigest)
			if s.Has(d) {
				t.Error("Has is true for a digest the store will not serve")
			}
		})
	}
}

func TestDigestParsing(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	cases := []struct {
		name string
		in   Digest
		want Digest // "" means an error is expected
	}{
		{"sha256", Digest("sha256:" + hex64), Digest("sha256:" + hex64)},
		{"uppercase is canonicalised", Digest("SHA256:" + strings.ToUpper(hex64)),
			Digest("sha256:" + hex64)},
		{"padded", Digest("  sha256:" + hex64 + "  "), Digest("sha256:" + hex64)},
		{"buildid keeps its second colon", "buildid:380870:18452901", "buildid:380870:18452901"},
		{"no prefix", Digest(hex64), ""},
		{"unknown algorithm", Digest("md5:" + strings.Repeat("a", 32)), ""},
		{"empty", "", ""},
		{"no value", "sha256:", ""},
		// A truncated digest is a digest an attacker can brute-force, so the
		// length is checked rather than "whatever hex we were given".
		{"truncated", "sha256:deadbeef", ""},
		{"too long", Digest("sha256:" + hex64 + "ab"), ""},
		{"not hex", Digest("sha256:" + strings.Repeat("z", 64)), ""},
		// The store's path is derived from the digest text, so a separator in
		// it is a traversal attempt and not a digest.
		{"traversal", "sha256:../../../../etc/shadow", ""},
		{"traversal in a buildid", `buildid:380870:..\..\windows`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDigest(tc.in)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("%q was accepted as %q", tc.in, got)
				}
				if !errors.Is(err, ErrBadDigest) {
					t.Fatalf("want ErrBadDigest, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Two spellings of one digest would be two addresses, two copies, and two
// chances for one of them to be the stale one.
func TestOneSetOfBytesHasOneAddressWhateverTheSpelling(t *testing.T) {
	s := newStore(t)
	body := []byte("jei-1.20.1.jar")
	d, _ := mustPut(t, s, body)

	shouty := Digest(strings.ToUpper(string(d)))
	if !s.Has(shouty) {
		t.Fatal("an uppercase spelling of a stored digest is not found")
	}
	if _, err := s.Get(context.Background(), shouty); err != nil {
		t.Fatalf("Get with an uppercase digest: %v", err)
	}
	if n := countFiles(t, s.root); n != 1 {
		t.Errorf("%d files stored for one set of bytes", n)
	}
}

func TestLinkHardlinksAndReplaces(t *testing.T) {
	s := newStore(t)
	body := []byte("oculus-1.20.1-1.7.0.jar")
	d, _ := mustPut(t, s, body)
	dst := destIn(t, "mods/oculus.jar")

	if err := s.Link(d, dst); err != nil {
		t.Fatalf("Link: %v", err)
	}
	got, err := os.ReadFile(dst.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Error("the linked file is not the blob")
	}
	// Re-linking is the ordinary case: content apply runs every time.
	if err := s.Link(d, dst); err != nil {
		t.Fatalf("re-Link: %v", err)
	}
	if left := stagingLeftovers(t, filepath.Dir(dst.Path())); len(left) != 0 {
		t.Errorf("Link left staging files behind: %v", left)
	}
}

func TestLinkRefusesACorruptBlobAndLeavesNoDestination(t *testing.T) {
	s := newStore(t)
	d, j := mustPut(t, s, []byte("good jar"))
	overwrite(t, j.Path(), []byte("evil jar"))
	dst := destIn(t, "mods/evil.jar")

	err := s.Link(d, dst)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
	// This is the consequence that matters: a caller that ignored the error
	// would have a jar at the path it asked for, and the game would load it.
	if _, statErr := os.Lstat(dst.Path()); statErr == nil {
		t.Fatal("a refused Link created the destination anyway")
	}
}

func TestLinkRefusesAnUnjailedDestination(t *testing.T) {
	s := newStore(t)
	d, _ := mustPut(t, s, []byte("x"))
	if err := s.Link(d, instance.Jailed{}); !errors.Is(err, ErrPaths) {
		t.Fatalf("want ErrPaths for a zero Jailed, got %v", err)
	}
}

// copyVerified is Link's EXDEV fallback. It is exercised directly because
// Windows does not report a cross-device link as EXDEV, and the half that
// could put the wrong bytes in mods/ has to be covered on the machine the code
// is written on.
func TestTheCopyFallbackReVerifiesWhatItWrote(t *testing.T) {
	dir := t.TempDir()
	body := []byte("embeddium-0.3.18.jar")
	src := filepath.Join(dir, "src.jar")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "staged.jar")

	if err := copyVerified(src, staged, sha256Of(body)); err != nil {
		t.Fatalf("a correct copy was refused: %v", err)
	}
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("the copy is not the source")
	}

	if err := os.Chmod(staged, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(staged); err != nil {
		t.Fatal(err)
	}
	err = copyVerified(src, staged, sha256Of([]byte("some other jar")))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
	if _, statErr := os.Lstat(staged); statErr == nil {
		t.Error("a refused copy was left on disk for the publish step to rename into place")
	}
}

func TestGCRemovesWhatKeepRejects(t *testing.T) {
	s := newStore(t)
	keepD, _ := mustPut(t, s, []byte("still in the lockfile"))
	dropD, dropJ := mustPut(t, s, bytes.Repeat([]byte("b"), 2048))
	dropSize := int64(2048)

	freed, err := s.GC(func(d Digest) bool { return d == keepD })
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if freed != dropSize {
		t.Errorf("freed %d, want %d", freed, dropSize)
	}
	if _, statErr := os.Lstat(dropJ.Path()); statErr == nil {
		t.Error("the rejected blob survived")
	}
	if !s.Has(keepD) {
		t.Error("the kept blob was removed")
	}
	if s.Has(dropD) {
		t.Error("Has still reports the removed blob")
	}
}

// A nil predicate reads as "keep nothing", which on a forgotten assignment
// deletes the whole cache. The cache is rebuildable, which is why this is a
// refusal and not a panic -- and not a silent wipe either.
func TestGCRefusesANilKeepPredicate(t *testing.T) {
	s := newStore(t)
	d, _ := mustPut(t, s, []byte("x"))
	if _, err := s.GC(nil); !errors.Is(err, ErrNoKeepFunc) {
		t.Fatalf("want ErrNoKeepFunc, got %v", err)
	}
	if !s.Has(d) {
		t.Error("GC removed something before refusing")
	}
}

func TestGCSweepsTheDebrisOfACrashedDownload(t *testing.T) {
	s := newStore(t)
	keepD, _ := mustPut(t, s, []byte("a published blob"))

	old := filepath.Join(s.tmp, stagingPrefix+"crashed")
	fresh := filepath.Join(s.tmp, stagingPrefix+"inflight")
	foreign := filepath.Join(s.tmp, "operator-put-this-here")
	for _, p := range []string{old, fresh, foreign} {
		if err := os.WriteFile(p, bytes.Repeat([]byte("c"), 512), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	freed, err := s.GC(func(Digest) bool { return true })
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if freed != 512 {
		t.Errorf("freed %d, want the one abandoned staging file's 512", freed)
	}
	if _, statErr := os.Lstat(old); statErr == nil {
		t.Error("the abandoned staging file survived")
	}
	// A 65 GB download in flight is not debris, and a file gvm did not stage
	// is not gvm's to delete.
	for _, p := range []string{fresh, foreign} {
		if _, statErr := os.Lstat(p); statErr != nil {
			t.Errorf("%s was removed: %v", filepath.Base(p), statErr)
		}
	}
	if !s.Has(keepD) {
		t.Error("GC removed a kept blob while sweeping staging")
	}
}

// GC's next move after identifying an address is os.Remove, so a path that
// merely looks blob-shaped must not be reported as a digest: keep would say "I
// have never heard of that" and a file nobody can identify would be deleted.
func TestGCLeavesWhatIsNotABlobAddressAlone(t *testing.T) {
	s := newStore(t)
	hex64 := strings.Repeat("ab", 32)
	notBlobs := []string{
		filepath.Join("sha256", "ab", "short"),     // not a full digest
		filepath.Join("sha256", "zz", hex64),       // shard does not match the name
		filepath.Join("md5", hex64[:2], hex64),     // not an algorithm this store uses
		filepath.Join("sha256", hex64),             // missing the shard level
		filepath.Join("sha256", "ab", "ab", hex64), // too deep
		filepath.Join("archives.index.json"),       // somebody's state file
	}
	for _, rel := range notBlobs {
		p := filepath.Join(s.root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	freed, err := s.GC(func(Digest) bool { return false })
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if freed != 0 {
		t.Errorf("GC freed %d bytes out of files that are not blob addresses", freed)
	}
	for _, rel := range notBlobs {
		if _, statErr := os.Lstat(filepath.Join(s.root, rel)); statErr != nil {
			t.Errorf("%s was deleted: %v", rel, statErr)
		}
	}
}

// A blob address occupied by a directory, or by a dangling symlink, is not a
// blob. It must refuse rather than hash whatever it finds.
func TestGetRefusesAnAddressThatIsNotARegularFile(t *testing.T) {
	s := newStore(t)
	d := sha256Of([]byte("would-be blob"))
	rel, err := s.rel(d)
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(s.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}

	j, getErr := s.Get(context.Background(), d)
	wantNoPath(t, j, getErr, ErrNotRegular)
	if s.Has(d) {
		t.Error("Has reports a directory as a blob")
	}
}

func TestGetIsCancellable(t *testing.T) {
	s := newStore(t)
	// Large enough that the hash takes more than one Read, so the cancelled
	// context is observed mid-stream rather than only at the top.
	d, _ := mustPut(t, s, bytes.Repeat([]byte("d"), 1<<20))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	j, err := s.Get(ctx, d)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if j.Valid() {
		t.Error("a cancelled Get returned a path")
	}
}

// Two jobs resolving the same mod at once is the ordinary case on a box with
// several instances. Both Puts must succeed and the published blob must be
// whole.
func TestConcurrentPutsOfTheSameDigest(t *testing.T) {
	s := newStore(t)
	body := bytes.Repeat([]byte("e"), 64*1024)
	d := sha256Of(body)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Put(context.Background(), bytes.NewReader(body), d)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Put %d: %v", i, err)
		}
	}
	if _, err := s.Get(context.Background(), d); err != nil {
		t.Fatalf("Get after concurrent Puts: %v", err)
	}
	if n := countFiles(t, s.root); n != 1 {
		t.Errorf("%d files after 8 Puts of one digest", n)
	}
}

func TestStoreSatisfiesTheInterfaceItDocuments(t *testing.T) {
	var bs BlobStore = newStore(t)
	if bs.Has("sha256:" + Digest(strings.Repeat("ab", 32))) {
		t.Error("an empty store has something")
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// destIn builds a jailed destination inside a throwaway instance tree, the way
// the content linker gets one.
func destIn(t *testing.T, rel string) instance.Jailed {
	t.Helper()
	root := filepath.Join(t.TempDir(), "srv", "pz-main", "install")
	if err := os.MkdirAll(filepath.Join(root, "mods"), 0o755); err != nil {
		t.Fatal(err)
	}
	j, err := instance.NewJail(instance.JailConfig{Allow: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := j.Resolve(filepath.FromSlash(rel))
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(_ string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func stagingLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagingPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}
