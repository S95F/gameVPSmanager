package blobstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/content/blobstore"
	"github.com/S95F/gameVPSmanager/internal/content/jail"
	"github.com/S95F/gameVPSmanager/internal/provider"
)

// TestStoreIsProviderBlobStoreItself is the "share it, do not clone it"
// assertion. If this package ever grows its own store type, there are two digest
// policies in the program and a conversion between them at the seam -- which is
// how "Get only on a digest match" becomes true of one of them.
func TestStoreIsProviderBlobStoreItself(t *testing.T) {
	s := open(t)

	// Assignable in both directions with no conversion.
	var asProvider provider.BlobStore = s
	var asStore blobstore.Store = asProvider
	if asStore == nil {
		t.Fatal("blobstore.Store and provider.BlobStore are not the same interface")
	}
	// And the digest type is one type, not two that agree today.
	var d blobstore.Digest = provider.Digest("sha256:" + hex64())
	var back provider.Digest = d
	if string(back) != string(d) {
		t.Fatal("blobstore.Digest and provider.Digest disagree")
	}
}

func hex64() string {
	sum := sha256.Sum256([]byte("x"))
	return fmt.Sprintf("%x", sum)
}

func open(t *testing.T) blobstore.Store {
	t.Helper()
	s, err := blobstore.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// digestOf is the digest a source would have recorded in its plan.
func digestOf(b []byte) blobstore.Digest {
	sum := sha256.Sum256(b)
	return provider.NewDigest(provider.AlgoSHA256, sum[:])
}

func put(t *testing.T, s blobstore.Store, b []byte) blobstore.Digest {
	t.Helper()
	d := digestOf(b)
	if _, err := s.Put(context.Background(), bytes.NewReader(b), d); err != nil {
		t.Fatal(err)
	}
	return d
}

// modsJail is the content jail a Minecraft mods directory gets, with section
// 12.8's preserve list.
// realTempDir is t.TempDir() with 8.3 short names resolved. On a host whose
// TEMP is spelled with one (the GitHub Windows runner's RUNNER~1), fixture
// paths would otherwise disagree with what EvalSymlinks returns.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return dir
}

func modsJail(t *testing.T) (*jail.Jail, string) {
	t.Helper()
	base := realTempDir(t)
	root := filepath.Join(base, "srv", "atm10", "install")
	if err := os.MkdirAll(filepath.Join(root, "mods"), 0o755); err != nil {
		t.Fatal(err)
	}
	props := filepath.Join(root, "server.properties")
	if err := os.WriteFile(props, []byte("level-name=world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := jail.New(jail.Config{Allow: []string{root}, Preserve: []string{props}})
	if err != nil {
		t.Fatal(err)
	}
	return j, root
}

func TestPlacePutsAVerifiedBlobInsideTheJail(t *testing.T) {
	s := open(t)
	j, root := modsJail(t)
	jar := []byte("PK\x03\x04 jei-1.20.1-15.2.0.27.jar")
	d := put(t, s, jar)

	p, err := blobstore.Place(s, d, j, "mods/jei.jar")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "mods", "jei.jar"); p.Path() != want {
		t.Errorf("Place = %s, want %s", p.Path(), want)
	}
	got, err := os.ReadFile(p.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, jar) {
		t.Errorf("placed bytes = %q", got)
	}
}

// TestPlaceRefusesEveryWayTheBytesCouldBeWrong is the digest discipline, asked
// through this package's one write call. Each row leaves the destination
// absent, because a jar with the right name and the wrong bytes is executed
// exactly as eagerly as a correct one.
func TestPlaceRefusesEveryWayTheBytesCouldBeWrong(t *testing.T) {
	jar := []byte("PK\x03\x04 a mod")

	t.Run("the store does not hold it", func(t *testing.T) {
		s := open(t)
		j, root := modsJail(t)
		_, err := blobstore.Place(s, digestOf(jar), j, "mods/jei.jar")
		if !errors.Is(err, blobstore.ErrBlobNotFound) {
			t.Fatalf("Place = %v, want ErrBlobNotFound", err)
		}
		// NOT terminal: the fix is to fetch it, which is what the caller does
		// next, and a ladder that gave up here would turn a cold cache into a
		// failed sync.
		if blobstore.Terminal(err) {
			t.Error("a missing blob was classified terminal; the fix is to fetch it")
		}
		assertAbsent(t, filepath.Join(root, "mods", "jei.jar"))
	})

	t.Run("the stored bytes were substituted", func(t *testing.T) {
		s := open(t)
		j, root := modsJail(t)
		d := put(t, s, jar)
		corrupt(t, s, d, []byte("PK\x03\x04 something else entirely"))

		_, err := blobstore.Place(s, d, j, "mods/jei.jar")
		if !errors.Is(err, blobstore.ErrDigestMismatch) {
			t.Fatalf("Place over a corrupted blob = %v, want ErrDigestMismatch", err)
		}
		if !blobstore.Terminal(err) {
			t.Error("a digest mismatch must be terminal: retrying cannot change the answer, " +
				"and a ladder that retried would re-download bytes that will never match")
		}
		assertAbsent(t, filepath.Join(root, "mods", "jei.jar"))
	})

	t.Run("a tree digest", func(t *testing.T) {
		// A workshop item is a directory the SERVER downloaded. There is no
		// file to hash, so it never enters this store, and the refusal is by
		// name rather than a fallback to "well, the file is there".
		s := open(t)
		j, root := modsJail(t)
		tree := provider.Digest(provider.AlgoTreeSHA256 + ":" + hex64())
		_, err := blobstore.Place(s, tree, j, "mods/item.jar")
		if !errors.Is(err, blobstore.ErrNotByteDigest) {
			t.Fatalf("Place(tree digest) = %v, want ErrNotByteDigest", err)
		}
		assertAbsent(t, filepath.Join(root, "mods", "item.jar"))
	})

	t.Run("a truncated digest", func(t *testing.T) {
		s := open(t)
		j, _ := modsJail(t)
		if _, err := blobstore.Place(s, "sha256:deadbeef", j, "mods/x.jar"); !errors.Is(err, blobstore.ErrBadDigest) {
			t.Fatalf("Place(short digest) = %v, want ErrBadDigest", err)
		}
	})
}

// TestPlaceAsksTheJailBeforeItAsksTheStore: a destination the jail refuses must
// never reach the store, because the store's job is to decide what the bytes
// are and the jail's is to decide where they may land. Checking in the other
// order would hash a 400 MB artifact to discover it was going to /etc.
func TestPlaceAsksTheJailBeforeItAsksTheStore(t *testing.T) {
	s := open(t)
	j, root := modsJail(t)
	jar := []byte("PK\x03\x04 a mod")
	d := put(t, s, jar)

	cases := []struct {
		name string
		rel  string
		want error
	}{
		{"traversal", "../../../etc/cron.d/root", jail.ErrTraversal},
		{"absolute", "/etc/cron.d/root", jail.ErrAbsolute},
		{"preserved", "server.properties", jail.ErrPreserved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := blobstore.Place(s, d, j, tc.rel)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Place(%q) = %v, want %v", tc.rel, err, tc.want)
			}
			if p.Valid() {
				t.Errorf("Place(%q) refused and still returned %s", tc.rel, p.Path())
			}
		})
	}
	// The one legitimate destination still works, so the refusals above are
	// about the paths and not about a broken fixture.
	if _, err := blobstore.Place(s, d, j, "mods/ok.jar"); err != nil {
		t.Fatalf("the control case failed: %v", err)
	}
	assertAbsent(t, filepath.Join(root, "server.properties.new"))
}

func TestIngestStoresNothingUnlessTheBytesHash(t *testing.T) {
	ctx := context.Background()
	jar := []byte("PK\x03\x04 jei")
	want := digestOf(jar)

	t.Run("the producer writes the approved bytes", func(t *testing.T) {
		s := open(t)
		p, err := blobstore.Ingest(ctx, s, want, func(w io.Writer) error {
			_, err := w.Write(jar)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if !p.Valid() {
			t.Fatal("Ingest returned no path")
		}
		if !s.Has(want) {
			t.Error("Ingest reported success and stored nothing")
		}
	})

	t.Run("the producer writes different bytes", func(t *testing.T) {
		s := open(t)
		_, err := blobstore.Ingest(ctx, s, want, func(w io.Writer) error {
			_, err := w.Write([]byte("PK\x03\x04 a mod nobody approved"))
			return err
		})
		if !errors.Is(err, blobstore.ErrDigestMismatch) {
			t.Fatalf("Ingest = %v, want ErrDigestMismatch", err)
		}
		if s.Has(want) {
			t.Error("a mismatched ingest stored bytes at the approved address")
		}
	})

	t.Run("the producer fails halfway", func(t *testing.T) {
		// A connection reset mid-download. The bytes written so far are a
		// prefix of a real file, which is exactly the state that must not end
		// up under a name that claims to be the whole thing.
		s := open(t)
		boom := errors.New("connection reset by peer")
		_, err := blobstore.Ingest(ctx, s, want, func(w io.Writer) error {
			if _, err := w.Write(jar[:3]); err != nil {
				return err
			}
			return boom
		})
		if err == nil {
			t.Fatal("a half-written ingest reported success")
		}
		if !errors.Is(err, boom) {
			t.Errorf("the ingest error lost the producer's cause: %v", err)
		}
		if s.Has(want) {
			t.Error("a half-written ingest stored bytes at the approved address")
		}
	})

	t.Run("no producer", func(t *testing.T) {
		s := open(t)
		if _, err := blobstore.Ingest(ctx, s, want, nil); !errors.Is(err, blobstore.ErrBadDigest) {
			t.Fatalf("Ingest(nil producer) = %v", err)
		}
	})
}

func TestNeedIsADownloadListAndNotAVerification(t *testing.T) {
	s := open(t)
	have := put(t, s, []byte("PK\x03\x04 cached"))
	missing := digestOf([]byte("PK\x03\x04 not here"))
	tree := provider.Digest(provider.AlgoTreeSHA256 + ":" + hex64())

	got := blobstore.Need(s, []blobstore.Digest{missing, have, missing, tree, "sha256:nope"})
	if len(got) != 1 || got[0] != missing {
		t.Fatalf("Need = %v, want exactly [%s]", got, missing.Short())
	}

	// A corrupted blob still counts as held, because Has hashes nothing. That
	// is the division of labour: this answers "how much is left to download"
	// and Place answers "are these the approved bytes".
	corrupt(t, s, have, []byte("substituted"))
	if got := blobstore.Need(s, []blobstore.Digest{have}); len(got) != 0 {
		t.Errorf("Need reported %v for a present-but-corrupt blob; Has is a hint and must not "+
			"pretend to verify", got)
	}
}

func TestCollectRefusesAKeepSetThatDoesNotKnowTheWholeLiveSet(t *testing.T) {
	live := []byte("PK\x03\x04 in use")
	dead := []byte("PK\x03\x04 nobody references this")

	t.Run("the zero keep set", func(t *testing.T) {
		s := open(t)
		d := put(t, s, live)
		// The forgery another package can write: a composite literal with no
		// digests in it. As a predicate it reads "keep nothing", and as a
		// garbage collection that is the whole cache.
		freed, err := blobstore.Collect(s, &blobstore.KeepSet{})
		if !errors.Is(err, blobstore.ErrIncompleteKeepSet) {
			t.Fatalf("Collect(zero) = %v, want ErrIncompleteKeepSet", err)
		}
		if freed != 0 || !s.Has(d) {
			t.Fatal("a refused collection deleted blobs anyway")
		}
	})

	t.Run("a nil keep set", func(t *testing.T) {
		s := open(t)
		d := put(t, s, live)
		if _, err := blobstore.Collect(s, nil); !errors.Is(err, blobstore.ErrIncompleteKeepSet) {
			t.Fatalf("Collect(nil) = %v, want ErrIncompleteKeepSet", err)
		}
		if !s.Has(d) {
			t.Fatal("a refused collection deleted blobs anyway")
		}
	})

	t.Run("one lockfile could not be read", func(t *testing.T) {
		// The case this type exists for. The set is a perfectly valid
		// predicate; it is just missing everything the unreadable instance
		// holds, and a GC against it deletes exactly that.
		s := open(t)
		keep := put(t, s, live)
		gone := put(t, s, dead)

		ks := blobstore.NewKeepSet()
		ks.AddLock("/etc/gvm/instances/atm10.lock.json", lockWith(keep))
		ks.Unreadable("/etc/gvm/instances/pz-main.lock.json",
			errors.New("unexpected end of JSON input"))

		_, err := blobstore.Collect(s, ks)
		if !errors.Is(err, blobstore.ErrIncompleteKeepSet) {
			t.Fatalf("Collect(with a hole) = %v, want ErrIncompleteKeepSet", err)
		}
		// The refusal names the instance that could not be read, because that
		// is the file the operator has to go and fix.
		if !bytes.Contains([]byte(err.Error()), []byte("pz-main.lock.json")) {
			t.Errorf("the refusal does not name the unreadable lockfile: %v", err)
		}
		if !s.Has(keep) || !s.Has(gone) {
			t.Fatal("a refused collection deleted blobs anyway")
		}
	})
}

func TestCollectFreesWhatNoLockReferences(t *testing.T) {
	s := open(t)
	keep := put(t, s, []byte("PK\x03\x04 in use"))
	dead := []byte("PK\x03\x04 nobody references this")
	gone := put(t, s, dead)

	ks := blobstore.NewKeepSet()
	ks.AddLock("/etc/gvm/instances/atm10.lock.json", lockWith(keep))

	freed, err := blobstore.Collect(s, ks)
	if err != nil {
		t.Fatal(err)
	}
	if freed != int64(len(dead)) {
		t.Errorf("freed = %d, want %d", freed, len(dead))
	}
	if !s.Has(keep) {
		t.Error("a referenced blob was deleted")
	}
	if s.Has(gone) {
		t.Error("an unreferenced blob survived")
	}
}

func TestKeepSetAccountsForTreeArtifactsWithoutCallingThemHoles(t *testing.T) {
	// A Project Zomboid box's lock is all tree artifacts: the server
	// downloaded every item itself and this store holds none of them. If a
	// tree counted as a hole, a PZ host could never collect at all; if it
	// counted as nothing, "0 blobs kept" would look like a failed read.
	tree := provider.Digest(provider.AlgoTreeSHA256 + ":" + hex64())
	l := &content.Lock{
		Version:  1,
		Instance: "pz-main",
		Sources: map[string]*content.SourceLock{
			"workshop": {
				Kind: content.KindSteamWorkshop,
				Entries: []content.LockEntry{{
					Ordinal:    0,
					Acquire:    "3171167894",
					Resolution: content.ResolutionServerBoot,
					Artifact:   &content.Artifact{Kind: content.ArtifactTree, Digest: tree},
				}, {
					// An item with no artifact at all: declared, not yet
					// acquired. It holds no blob, so it keeps none, and it is
					// not a hole either.
					Ordinal:    1,
					Acquire:    "3778511497",
					Resolution: content.ResolutionServerBoot,
				}},
			},
		},
	}

	ks := blobstore.NewKeepSet()
	ks.AddLock("/etc/gvm/instances/pz-main.lock.json", l)
	if err := ks.Check(); err != nil {
		t.Fatalf("a lock of tree artifacts must be collectable: %v", err)
	}
	if ks.Len() != 0 {
		t.Errorf("Len = %d, want 0 blobs", ks.Len())
	}
	if ks.Trees() != 1 {
		t.Errorf("Trees = %d, want 1", ks.Trees())
	}
	if got := ks.Sources(); len(got) != 1 {
		t.Errorf("Sources = %v", got)
	}
}

func TestKeepSetTreatsAnUnreadableDigestAsAHole(t *testing.T) {
	// It came out of a lockfile gvm wrote, so either the file was hand-edited
	// or an older gvm wrote something this one cannot parse. Either way the
	// live set is not fully known, and deleting against it is a guess.
	l := lockWith("sha256:deadbeef")
	ks := blobstore.NewKeepSet()
	ks.AddLock("/etc/gvm/instances/atm10.lock.json", l)
	if err := ks.Check(); !errors.Is(err, blobstore.ErrIncompleteKeepSet) {
		t.Fatalf("Check = %v, want ErrIncompleteKeepSet", err)
	}
}

func TestKeepSetTreatsANilLockAsAHole(t *testing.T) {
	ks := blobstore.NewKeepSet()
	ks.AddLock("/etc/gvm/instances/atm10.lock.json", nil)
	if err := ks.Check(); !errors.Is(err, blobstore.ErrIncompleteKeepSet) {
		t.Fatalf("Check(nil lock) = %v, want ErrIncompleteKeepSet", err)
	}
}

func TestKeepSetKeepsAndReports(t *testing.T) {
	a := digestOf([]byte("a"))
	b := digestOf([]byte("b"))
	ks := blobstore.NewKeepSet()
	ks.AddLock("/etc/gvm/instances/atm10.lock.json", lockWith(a, b))

	if err := ks.Check(); err != nil {
		t.Fatal(err)
	}
	// Through Predicate, which is the only door: it runs Check itself, so a
	// complete set is the only kind that can produce a live-set predicate.
	keep, err := ks.Predicate()
	if err != nil {
		t.Fatalf("Predicate on a complete set refused: %v", err)
	}
	if !keep(a) || !keep(b) {
		t.Error("a locked digest is not kept")
	}
	if keep(digestOf([]byte("c"))) {
		t.Error("an unlocked digest is kept")
	}
	// Canonicalisation has to happen on the way in AND on the way out, or
	// "SHA256:AB…" and "sha256:ab…" are two addresses for one set of bytes and
	// GC deletes the one it did not hear about.
	if !keep(provider.Digest("SHA256:" + upperHex(a.Value()))) {
		t.Error("the predicate is case-sensitive about a digest's spelling")
	}
	if got := ks.Digests(); len(got) != 2 || got[0] > got[1] {
		t.Errorf("Digests = %v, want two sorted", got)
	}
	if ks.Len() != 2 {
		t.Errorf("Len = %d", ks.Len())
	}
}

func TestNilKeepSetAndNilStoreRefuseRatherThanPanic(t *testing.T) {
	var ks *blobstore.KeepSet
	if err := ks.Check(); !errors.Is(err, blobstore.ErrIncompleteKeepSet) {
		t.Errorf("nil Check = %v", err)
	}
	if p, err := ks.Predicate(); p != nil || !errors.Is(err, blobstore.ErrIncompleteKeepSet) {
		t.Errorf("nil Predicate returned a predicate (%t) and error %v; want no predicate and "+
			"ErrIncompleteKeepSet", p != nil, err)
	}
	if ks.Len() != 0 || ks.Trees() != 0 {
		t.Error("a nil keep set claimed to hold something")
	}
	ks.Add(digestOf([]byte("a"))) // must not panic
	ks.AddLock("x", nil)
	ks.Unreadable("x", nil)
	if ks.Sources() != nil || ks.Digests() != nil {
		t.Error("a nil keep set returned sources")
	}
	if ks.String() == "" {
		t.Error("nil String() is empty")
	}

	if _, err := blobstore.Collect(nil, blobstore.NewKeepSet()); !errors.Is(err, blobstore.ErrNoStore) {
		t.Errorf("Collect(nil store) = %v", err)
	}
	j, _ := modsJail(t)
	if _, err := blobstore.Place(nil, digestOf([]byte("a")), j, "mods/a.jar"); !errors.Is(err, blobstore.ErrNoStore) {
		t.Errorf("Place(nil store) = %v", err)
	}
	if _, err := blobstore.Ingest(context.Background(), nil, digestOf([]byte("a")), func(io.Writer) error { return nil }); !errors.Is(err, blobstore.ErrNoStore) {
		t.Errorf("Ingest(nil store) = %v", err)
	}
	if blobstore.Need(nil, []blobstore.Digest{digestOf([]byte("a"))}) == nil {
		t.Error("Need(nil store) must report everything as needed")
	}
}

// lockWith builds a minimal lock holding one file artifact per digest.
func lockWith(ds ...blobstore.Digest) *content.Lock {
	sl := &content.SourceLock{Kind: content.KindModrinth}
	for i, d := range ds {
		sl.Entries = append(sl.Entries, content.LockEntry{
			Ordinal:       i,
			Acquire:       content.AcquireID(fmt.Sprintf("mod-%d", i)),
			ActivationIDs: []content.ActivationID{content.ActivationID(fmt.Sprintf("mod-%d.jar", i))},
			Resolution:    content.ResolutionAPI,
			Artifact:      &content.Artifact{Kind: content.ArtifactFile, Digest: d},
		})
	}
	return &content.Lock{
		Version:  1,
		Instance: "atm10",
		Game:     "minecraft-neoforge",
		Sources:  map[string]*content.SourceLock{"mods": sl},
	}
}

// corrupt substitutes the bytes at a digest's address, which is what bit rot,
// a half-finished copy from an older gvm and a deliberate substitution all look
// like from the store's side.
func corrupt(t *testing.T, s blobstore.Store, d blobstore.Digest, with []byte) {
	t.Helper()
	p, err := s.Get(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Path(), with, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s exists; a refused place wrote bytes anyway", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func upperHex(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// TestAHoledKeepSetCannotProduceAPredicate makes the completeness rule a
// property of the TYPE rather than of one function in another package.
//
// KeepSet's own doc comment states the invariant as a property of the type --
// "a keep set that refuses to be used when it was built from an incomplete read
// of the lockfiles" -- but the refusal lived only inside Collect, while the
// predicate was exported and its doc comment advertised the ungated spelling
// ("the predicate provider.Store.GC takes"). provider.Store.GC is exported and
// refuses only a NIL predicate, so `store.GC(ks.Keep)` compiled from any
// package and reached os.Remove with a live set that silently answered "no" for
// every instance whose lockfile had failed to parse.
//
// This is the same reasoning as internal/content's compile-fail fixtures and
// blobstore.Place: a rule enforced by review is a rule that holds until the
// reviewer is busy.
func TestAHoledKeepSetCannotProduceAPredicate(t *testing.T) {
	holed := blobstore.NewKeepSet()
	holed.AddLock("/etc/gvm/instances/atm10.lock.json", lockWith(digestOf([]byte("a"))))
	holed.Unreadable("/etc/gvm/instances/pz-main.lock.json", errors.New("syntax error"))

	unparseable := blobstore.NewKeepSet()
	unparseable.AddLock("/etc/gvm/instances/atm10.lock.json", lockWith("sha256:not-hex"))

	var nilSet *blobstore.KeepSet
	for name, ks := range map[string]*blobstore.KeepSet{
		"the zero value":        {},
		"a nil receiver":        nilSet,
		"an unreadable source":  holed,
		"an unparseable digest": unparseable,
	} {
		t.Run(name, func(t *testing.T) {
			p, err := ks.Predicate()
			if !errors.Is(err, blobstore.ErrIncompleteKeepSet) {
				t.Fatalf("Predicate() error = %v, want ErrIncompleteKeepSet", err)
			}
			if p != nil {
				t.Error("a predicate came back alongside the refusal, so a caller that " +
					"ignored the error would still delete against an incomplete live set")
			}
		})
	}
}
