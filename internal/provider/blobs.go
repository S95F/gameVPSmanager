package provider

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Digest identifies bytes, or identifies a build.
//
// Two shapes share the type on purpose, and telling them apart is the whole
// job of Verifiable:
//
//	sha256:1b40c9de…    bytes gvm downloaded and can hash
//	sha512:…  sha1:…    the same, in whatever the upstream source publishes
//	buildid:380870:18452901   Valve's identity for an install gvm did not fetch
//	tree-sha256:4b19c0…       a workshop tree the server downloaded itself
//
// Only the first three are hashes of a file, and only those three may enter
// the blob store. The other two are honest identities for bytes nobody can
// re-hash on demand, and the store refuses them by name rather than by falling
// back to "well, the file is there".
type Digest string

// Digest algorithms. The three byte algorithms are "sha256 / sha1 / sha512 as
// the source publishes" (section 18.3): CurseForge's API returns sha1 and
// Modrinth returns sha512+sha1, so a store that accepted only sha256 would
// force gvm to compute a digest nobody can check it against.
const (
	AlgoSHA256 = "sha256"
	AlgoSHA512 = "sha512"
	AlgoSHA1   = "sha1"
	// AlgoBuildID is a Steam app's build identity: "buildid:<appid>:<buildid>".
	// It names what SteamCMD installed, which SteamCMD itself wrote to disk --
	// there is no single file to hash.
	AlgoBuildID = "buildid"
	// AlgoTreeSHA256 is a content TREE digest (section 12.1), over a canonical
	// relpath/size/sha256 listing rather than over one file's bytes.
	AlgoTreeSHA256 = "tree-sha256"
)

// hexLen is the hex length each byte algorithm produces. The length check is
// not cosmetic: it is what stops "sha256:deadbeef" -- a truncated digest an
// attacker can brute-force -- from being accepted as a 256-bit commitment.
var hexLen = map[string]int{
	AlgoSHA256: sha256.Size * 2,
	AlgoSHA512: sha512.Size * 2,
	AlgoSHA1:   sha1.Size * 2,
}

// The blob store's refusals.
//
// ErrDigestMismatch and ErrNotByteDigest are TERMINAL: retrying cannot change
// the answer, and a retry ladder that treated them as transient would keep
// re-downloading bytes that will never match while reporting "retrying" to an
// operator whose actual problem is a substituted file. Terminal reports the
// classification so the ladder in section 13.5 asks one place.
var (
	// ErrDigestMismatch is the refusal this whole package exists for: the
	// bytes on disk are not the bytes that were approved.
	ErrDigestMismatch = errors.New("provider: digest mismatch")
	// ErrNotByteDigest is a digest the store cannot verify by hashing -- a
	// buildid or a tree digest. Terminal, because the right response is to
	// stop asking the blob store, not to ask again.
	ErrNotByteDigest = errors.New("provider: not a byte digest")
	// ErrBadDigest is a malformed digest string.
	ErrBadDigest = errors.New("provider: malformed digest")
	// ErrBlobNotFound is an address the store holds nothing at. NOT terminal:
	// the fix is to fetch it, which is exactly what the caller does next.
	ErrBlobNotFound = errors.New("provider: no blob at this digest")
	// ErrNotRegular is a blob address occupied by a directory, a device or a
	// socket.
	ErrNotRegular = errors.New("provider: blob path is not a regular file")
	// ErrNoKeepFunc is GC called with no keep predicate. A nil predicate reads
	// as "keep nothing", which would delete the entire cache on a caller's
	// forgotten assignment.
	ErrNoKeepFunc = errors.New("provider: GC needs a keep predicate")
)

// Terminal reports whether retrying err could ever change the answer.
func Terminal(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrDigestMismatch),
		errors.Is(err, ErrNotByteDigest),
		errors.Is(err, ErrBadDigest),
		errors.Is(err, ErrAdopted),
		errors.Is(err, ErrInvalidPlan),
		errors.Is(err, ErrUnauthorized):
		return true
	}
	return false
}

// ParseDigest validates a digest and returns it in canonical form.
//
// Canonicalisation is load-bearing, not tidiness: the store's path is derived
// from the digest text, so "SHA256:AB…" and "sha256:ab…" would otherwise be
// two addresses for one set of bytes -- two copies, and two chances for one of
// them to be stale.
func ParseDigest(d Digest) (Digest, error) {
	s := string(d)
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%w: empty", ErrBadDigest)
	}
	algo, value, found := strings.Cut(s, ":")
	if !found {
		return "", fmt.Errorf("%w: %q has no algorithm prefix (want \"sha256:…\")",
			ErrBadDigest, s)
	}
	algo = strings.ToLower(strings.TrimSpace(algo))
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%w: %q carries no value", ErrBadDigest, s)
	}

	switch algo {
	case AlgoSHA256, AlgoSHA512, AlgoSHA1:
		value = strings.ToLower(value)
		if len(value) != hexLen[algo] {
			return "", fmt.Errorf("%w: %s needs %d hex characters and %q has %d",
				ErrBadDigest, algo, hexLen[algo], value, len(value))
		}
		if _, err := hex.DecodeString(value); err != nil {
			return "", fmt.Errorf("%w: %q is not hex", ErrBadDigest, value)
		}
	case AlgoBuildID:
		// "buildid:<appid>:<buildid>" keeps its second colon, so only the
		// shape is checked here.
		if strings.Contains(value, "/") || strings.Contains(value, `\`) {
			return "", fmt.Errorf("%w: %q is not a build identity", ErrBadDigest, s)
		}
	case AlgoTreeSHA256:
		value = strings.ToLower(value)
		if len(value) != hexLen[AlgoSHA256] {
			return "", fmt.Errorf("%w: %s needs %d hex characters and %q has %d",
				ErrBadDigest, algo, hexLen[AlgoSHA256], value, len(value))
		}
		if _, err := hex.DecodeString(value); err != nil {
			return "", fmt.Errorf("%w: %q is not hex", ErrBadDigest, value)
		}
	default:
		return "", fmt.Errorf("%w: %q is not a digest algorithm (want %s, %s, %s, %s or %s)",
			ErrBadDigest, algo, AlgoSHA256, AlgoSHA512, AlgoSHA1, AlgoBuildID, AlgoTreeSHA256)
	}
	return Digest(algo + ":" + value), nil
}

// NewDigest builds a canonical digest from a raw hash sum.
func NewDigest(algo string, sum []byte) Digest {
	return Digest(algo + ":" + hex.EncodeToString(sum))
}

// Algo is the part before the first colon, lowercased, or "" for a digest that
// has none.
func (d Digest) Algo() string {
	algo, _, found := strings.Cut(string(d), ":")
	if !found {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(algo))
}

// Value is everything after the first colon.
func (d Digest) Value() string {
	_, v, found := strings.Cut(string(d), ":")
	if !found {
		return ""
	}
	return strings.TrimSpace(v)
}

// Verifiable reports whether these bytes can be re-hashed from a single file,
// which is the only thing the blob store will accept.
func (d Digest) Verifiable() bool {
	_, ok := hexLen[d.Algo()]
	return ok
}

func (d Digest) String() string { return string(d) }

// Short is the digest a log line or a dashboard row shows.
func (d Digest) Short() string {
	v := d.Value()
	if len(v) > 12 {
		v = v[:12] + "…"
	}
	if a := d.Algo(); a != "" {
		return a + ":" + v
	}
	return string(d)
}

// BlobStore hands out a path only on a digest match. There is no "trust me"
// path and there is deliberately no way to ask for one.
//
// gvm downloads third-party game and mod binaries and then executes them as a
// service account. The digest is the only thing standing between a corrupted
// or substituted download and code execution under that uid, so every method
// that could produce bytes for execution re-derives the digest first -- Get
// included, on every call, even though the file is stored under its own hash.
// A store that trusted its own filenames would be defeated by anything that
// can write one file.
type BlobStore interface {
	// Get returns the blob's path, or a terminal error and NO path.
	Get(ctx context.Context, d Digest) (instance.Jailed, error)
	// Put stores the stream iff it hashes to want, and stores nothing
	// otherwise.
	Put(ctx context.Context, r io.Reader, want Digest) (instance.Jailed, error)
	// Has answers "is there a file at this address" and vouches for nothing.
	Has(d Digest) bool
	// Link hardlinks a verified blob to dst, copying on EXDEV.
	Link(want Digest, dst instance.Jailed) error
	// GC removes every blob keep rejects.
	GC(keep func(Digest) bool) (freed int64, err error)
}

// The directory modes section 5 gives this tree. BlobMode, the mode of a
// published blob, is platform-split; see blobmode_linux.go.
const (
	BlobDirMode fs.FileMode = 0o755
	BlobTmpMode fs.FileMode = 0o750
)

// stagingGrace is how long an in-flight download may sit in tmp before GC
// treats it as the debris of a crashed fetch. It is generous because a 65 GB
// download over a slow link is not debris.
const stagingGrace = 24 * time.Hour

// stagingPrefix marks a staging file. The dot keeps it out of any glob, and the
// name is distinctive enough that an operator who finds one in a directory
// listing can tell what left it there.
const stagingPrefix = ".gvm-blob-"

// Store is the content-addressed blob store at /var/lib/gvm/blobs.
//
// Layout is section 5's: blobs/<algo>/<first two hex characters>/<hex>, with
// staging under blobs/tmp so the rename that publishes a verified blob is a
// same-filesystem rename.
type Store struct {
	root string
	tmp  string
	// jail is what lets Get return an instance.Jailed at all: Jailed's fields
	// are unexported, so the only way to produce one is to resolve a path
	// through a jail, and the only paths this jail will resolve are inside the
	// blob root. A symlink planted in the store pointing at /etc/shadow
	// resolves outside it and is refused.
	jail *instance.Jail
}

var _ BlobStore = (*Store)(nil)

// NewStore roots a store at dir, creating it if needed. An empty dir means the
// real one.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		dir = filepath.FromSlash(instance.BlobsDir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("provider: blob root %s: %w", dir, err)
	}
	tmp := filepath.Join(abs, "tmp")
	if err := os.MkdirAll(abs, BlobDirMode); err != nil {
		return nil, fmt.Errorf("provider: create blob root: %w", err)
	}
	if err := os.MkdirAll(tmp, BlobTmpMode); err != nil {
		return nil, fmt.Errorf("provider: create blob staging: %w", err)
	}

	// Not strict. The development machine's temp directory resolves through a
	// symlink, and containment is decided on the RESOLVED path regardless, so
	// strict mode would buy nothing here and would make every test on a
	// symlinked root fail for a reason that has nothing to do with blobs.
	j, err := instance.NewJail(instance.JailConfig{Allow: []string{abs}})
	if err != nil {
		return nil, fmt.Errorf("provider: blob jail: %w", err)
	}
	return &Store{root: abs, tmp: tmp, jail: j}, nil
}

// DefaultStore is the store of a real box.
func DefaultStore() (*Store, error) { return NewStore("") }

// Root is the store's directory, for doctor output and for tests.
func (s *Store) Root() string { return s.root }

// addr validates a digest and returns it in canonical form along with its
// store-relative, slash-separated address.
//
// The two are returned together because they must agree: the address is
// derived from the canonical text, and comparing a hash against the
// caller's spelling instead of the canonical one is how "sha256:AB…" would
// read a blob stored at "sha256:ab…" and then report a mismatch.
func (s *Store) addr(d Digest) (canon Digest, rel string, err error) {
	canon, err = ParseDigest(d)
	if err != nil {
		return "", "", err
	}
	if !canon.Verifiable() {
		return "", "", fmt.Errorf("%w: %s names bytes the store cannot re-hash, so it will not "+
			"store or serve them; the provider that produced them owns verifying them",
			ErrNotByteDigest, canon.Short())
	}
	v := canon.Value()
	return canon, path.Join(canon.Algo(), v[:2], v), nil
}

// rel is addr's address half, for the paths-only callers.
func (s *Store) rel(d Digest) (string, error) {
	_, rel, err := s.addr(d)
	return rel, err
}

// Get returns the blob's path iff the file at that address hashes to d.
//
// On any failure it returns the zero instance.Jailed, whose Path() is "" and
// whose Valid() is false. That is deliberate and it is the second half of the
// guarantee: a caller that ignores the error does not receive a usable path,
// because there is no path in the value either.
func (s *Store) Get(ctx context.Context, d Digest) (instance.Jailed, error) {
	var none instance.Jailed
	if ctx == nil {
		ctx = context.Background()
	}
	canon, rel, err := s.addr(d)
	if err != nil {
		return none, err
	}

	// Resolve first, then hash what resolution returned, so the bytes that
	// were hashed are the bytes at the path being handed out.
	jp, err := s.jail.Resolve(filepath.FromSlash(rel))
	if err != nil {
		return none, fmt.Errorf("provider: blob %s: %w", d.Short(), err)
	}
	fi, err := os.Lstat(jp.Path())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return none, fmt.Errorf("%w: %s", ErrBlobNotFound, d.Short())
	case err != nil:
		return none, fmt.Errorf("provider: stat blob %s: %w", d.Short(), err)
	case !fi.Mode().IsRegular():
		return none, fmt.Errorf("%w: %s is %s", ErrNotRegular, jp.Path(), fi.Mode().Type())
	}

	got, err := hashFile(ctx, jp.Path(), canon.Algo())
	if err != nil {
		return none, err
	}
	if got != canon {
		return none, fmt.Errorf("%w: %s holds %s; these bytes are not the bytes that were "+
			"approved and gvm will not hand out a path to them. Re-fetch the artifact; if the "+
			"mismatch survives a re-fetch, the upstream file changed under its own digest",
			ErrDigestMismatch, jp.Path(), got.Short())
	}
	return jp, nil
}

// Put stores r's bytes iff they hash to want.
//
// On a mismatch nothing is stored: the staging file is removed and the address
// is left exactly as it was, whether that was empty or held a previous good
// copy. A partially-written blob under its final name is the one state this
// store must never produce, because the name is the only claim anything makes
// about the content.
//
// A correct Put over an occupied address publishes anyway unless the bytes
// already there are the right ones, which is what makes a corrupted cache
// self-healing: the repair path is the ordinary fetch path, and there is no
// quarantine verb for anybody to remember. It deliberately does not
// short-circuit on Has -- Has knows nothing about content, and trusting it
// here is how a corrupt blob would survive every attempt to replace it.
func (s *Store) Put(ctx context.Context, r io.Reader, want Digest) (j instance.Jailed, err error) {
	var none instance.Jailed
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil {
		return none, fmt.Errorf("%w: no reader", ErrBadDigest)
	}
	canon, rel, err := s.addr(want)
	if err != nil {
		return none, err
	}
	h, err := hasherFor(canon.Algo())
	if err != nil {
		return none, err
	}

	if err = os.MkdirAll(s.tmp, BlobTmpMode); err != nil {
		return none, fmt.Errorf("provider: create blob staging: %w", err)
	}
	f, err := os.CreateTemp(s.tmp, stagingPrefix+"*")
	if err != nil {
		return none, fmt.Errorf("provider: stage blob: %w", err)
	}
	staged := f.Name()
	defer func() {
		if err == nil {
			return
		}
		f.Close()
		// The staging file goes on every failure path, including a digest
		// mismatch. "Put with a mismatched digest stores nothing" has to mean
		// nothing anywhere, not nothing at the final address: a .part file
		// holding substituted bytes is a file some future cleanup might
		// mistake for a half-finished good download.
		os.Remove(staged)
	}()

	if _, err = io.Copy(io.MultiWriter(f, h), &ctxReader{ctx: ctx, r: r}); err != nil {
		return none, fmt.Errorf("provider: download blob %s: %w", canon.Short(), err)
	}
	if err = f.Sync(); err != nil {
		return none, fmt.Errorf("provider: fsync %s: %w", staged, err)
	}
	if err = f.Close(); err != nil {
		return none, fmt.Errorf("provider: close %s: %w", staged, err)
	}

	if got := NewDigest(canon.Algo(), h.Sum(nil)); got != canon {
		err = fmt.Errorf("%w: downloaded bytes hash to %s, not %s; nothing was stored",
			ErrDigestMismatch, got.Short(), canon.Short())
		return none, err
	}

	// Chmod before the publish: a blob that appears at its final address with
	// the wrong mode -- even for a microsecond -- is a blob an instance user
	// could rewrite.
	if err = os.Chmod(staged, BlobMode); err != nil {
		return none, fmt.Errorf("provider: chmod %s: %w", staged, err)
	}

	full := filepath.Join(s.root, filepath.FromSlash(rel))
	if err = os.MkdirAll(filepath.Dir(full), BlobDirMode); err != nil {
		return none, fmt.Errorf("provider: create blob shard: %w", err)
	}

	// An address that already holds these exact bytes is left alone, and that
	// is not just an optimisation. Publishing creates a new inode, so a
	// republish silently orphans every hardlink an instance already has into
	// this blob: mods/x.jar would keep pointing at the old inode and the disk
	// would hold both copies forever. Replacing is for the corrupt case only,
	// which is why the check is a hash and not a stat.
	if s.holds(ctx, canon, full) {
		if err = os.Remove(staged); err != nil {
			return none, fmt.Errorf("provider: discard redundant staging file: %w", err)
		}
	} else {
		if err = publish(staged, full); err != nil {
			return none, fmt.Errorf("provider: publish blob %s: %w", canon.Short(), err)
		}
		if err = spec.SyncDir(filepath.Dir(full)); err != nil {
			return none, err
		}
	}

	jp, err := s.jail.Resolve(filepath.FromSlash(rel))
	if err != nil {
		return none, fmt.Errorf("provider: blob %s: %w", canon.Short(), err)
	}
	return jp, nil
}

// Has reports whether a file exists at a digest's address.
//
// It hashes nothing, so it is a cache-population hint and NOT evidence about
// content: "we probably do not need to download this again" is the only
// question it answers. That is why it returns a bool and not a path -- only
// Get hands out a path, and Get hashes first.
func (s *Store) Has(d Digest) bool {
	rel, err := s.rel(d)
	if err != nil {
		return false
	}
	fi, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(rel)))
	return err == nil && fi.Mode().IsRegular()
}

// Link puts a verified blob at dst, as a hardlink where the filesystem allows
// one and a re-verified copy where it does not.
//
// dst is an instance.Jailed, so the destination has already been proven to be
// inside the tree it claims to be in -- a modpack cannot name
// ../../../../etc/cron.d/root here, because it cannot produce the value.
//
// The copy fallback re-hashes what it wrote. A copy that failed halfway and a
// copy onto a full disk both produce a file at the destination, and a jar with
// the right name and the wrong bytes is executed exactly as eagerly as a
// correct one.
func (s *Store) Link(want Digest, dst instance.Jailed) error {
	if !dst.Valid() {
		return fmt.Errorf("%w: the destination did not come from a jail", ErrPaths)
	}
	canon, _, err := s.addr(want)
	if err != nil {
		return err
	}
	src, err := s.Get(context.Background(), canon)
	if err != nil {
		return err
	}

	dir := filepath.Dir(dst.Path())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("provider: create %s: %w", dir, err)
	}
	// Staged beside the destination and renamed over it, so a reader of
	// mods/x.jar sees either the old file or the new one and never a partial
	// link target. os.Link needs its new path absent, so the unique name is
	// created and then removed.
	tf, err := os.CreateTemp(dir, stagingPrefix+"link-*")
	if err != nil {
		return fmt.Errorf("provider: stage link in %s: %w", dir, err)
	}
	staged := tf.Name()
	tf.Close()
	if err := os.Remove(staged); err != nil {
		return fmt.Errorf("provider: stage link in %s: %w", dir, err)
	}
	defer os.Remove(staged) // no-op once the publish renames it away

	if linkErr := os.Link(src.Path(), staged); linkErr != nil {
		if !isCrossDevice(linkErr) {
			return fmt.Errorf("provider: hardlink %s: %w", canon.Short(), linkErr)
		}
		if err := copyVerified(src.Path(), staged, canon); err != nil {
			return err
		}
	}

	if err := publish(staged, dst.Path()); err != nil {
		return fmt.Errorf("provider: publish %s: %w", dst.Path(), err)
	}
	return spec.SyncDir(dir)
}

// holds reports whether full already contains exactly the bytes want names.
//
// Any failure answers false, because the only consequence of a false negative
// is a republish, while a false positive would leave corrupt bytes in place
// and call them verified.
func (s *Store) holds(ctx context.Context, want Digest, full string) bool {
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	got, err := hashFile(ctx, full, want.Algo())
	return err == nil && got == want
}

// publish renames a staged file over its final address.
//
// Replacing rather than refusing is what makes a corrupted cache self-healing:
// the repair path is the ordinary fetch path, with no quarantine verb for
// anyone to remember. The replace is atomic, so a reader sees the old bytes or
// the new ones and never a partial file.
//
// The retry is for Windows, where a file that has just appeared is a file a
// scanner has just opened without FILE_SHARE_DELETE, and MoveFileEx reports
// that as a permission error -- see spec.ReplaceFile, which retries for the
// same reason on a shorter budget. A blob store publishes files a scanner has
// watched being written, often several at once, so it needs the longer one. On
// Linux, where blobs are actually served, the first attempt always wins.
func publish(staged, full string) error {
	var err error
	for attempt := range 4 {
		if err = spec.ReplaceFile(staged, full); err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrPermission) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
	}
	return err
}

// GC removes every blob keep rejects, and the debris of crashed downloads.
//
// keep is the caller's live set, so GC is only as safe as the set is complete:
// a caller that computes it from a lockfile it failed to read would delete the
// whole cache. That is survivable -- this tree is derived state and
// re-downloadable (section 5) -- but it is not free, so a nil predicate is a
// refusal rather than "keep nothing".
//
// It reports every removal failure rather than the first: a GC that stops at
// the first EACCES has freed some unknown amount and told the operator a
// number that is wrong.
func (s *Store) GC(keep func(Digest) bool) (freed int64, err error) {
	if keep == nil {
		return 0, ErrNoKeepFunc
	}
	cutoff := time.Now().Add(-stagingGrace)
	var errs []error

	walkErr := filepath.WalkDir(s.root, func(p string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			errs = append(errs, walkErr)
			return nil
		}
		if e.IsDir() {
			if p == s.tmp {
				// Staging is swept by age below, not by the keep set: a file
				// in flight has no digest to ask about yet.
				freedTmp, tmpErrs := s.sweepStaging(cutoff)
				freed += freedTmp
				errs = append(errs, tmpErrs...)
				return filepath.SkipDir
			}
			return nil
		}
		d, ok := s.digestOf(p)
		if !ok || keep(d) {
			return nil
		}
		fi, statErr := e.Info()
		if statErr != nil {
			errs = append(errs, statErr)
			return nil
		}
		if rmErr := os.Remove(p); rmErr != nil {
			errs = append(errs, fmt.Errorf("provider: remove %s: %w", p, rmErr))
			return nil
		}
		freed += fi.Size()
		return nil
	})
	if walkErr != nil {
		errs = append(errs, walkErr)
	}
	return freed, errors.Join(errs...)
}

// sweepStaging removes staging files last written before cutoff.
func (s *Store) sweepStaging(cutoff time.Time) (freed int64, errs []error) {
	entries, err := os.ReadDir(s.tmp)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, []error{fmt.Errorf("provider: read blob staging: %w", err)}
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), stagingPrefix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !fi.ModTime().Before(cutoff) {
			continue
		}
		p := filepath.Join(s.tmp, e.Name())
		if err := os.Remove(p); err != nil {
			errs = append(errs, fmt.Errorf("provider: remove %s: %w", p, err))
			continue
		}
		freed += fi.Size()
	}
	return freed, errs
}

// digestOf reads a blob's address back out of its path, and reports false for
// anything that is not one.
//
// Refusing to guess matters here: GC's next move after this function is
// os.Remove, and a path that merely looks blob-shaped must not be handed to
// keep as a digest -- keep would correctly say "I have never heard of that"
// and GC would delete a file nobody could identify.
func (s *Store) digestOf(p string) (Digest, bool) {
	rel, err := filepath.Rel(s.root, p)
	if err != nil {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 {
		return "", false
	}
	algo, shard, name := parts[0], parts[1], parts[2]
	if _, ok := hexLen[algo]; !ok {
		return "", false
	}
	if len(name) < 2 || shard != name[:2] {
		return "", false
	}
	d, err := ParseDigest(Digest(algo + ":" + name))
	if err != nil || string(d) != algo+":"+name {
		return "", false
	}
	return d, true
}

func hasherFor(algo string) (hash.Hash, error) {
	switch algo {
	case AlgoSHA256:
		return sha256.New(), nil
	case AlgoSHA512:
		return sha512.New(), nil
	case AlgoSHA1:
		// Weak against a chosen-prefix collision and kept because CurseForge's
		// API publishes nothing else (section 18.3). It still proves the bytes
		// are the bytes the API named, which is the property the store needs,
		// and the alternative is no check at all.
		return sha1.New(), nil
	}
	return nil, fmt.Errorf("%w: %q cannot be hashed by this store", ErrNotByteDigest, algo)
}

// hashFile re-derives a file's digest.
//
// Every Get pays this, on bytes that are already stored under their own hash.
// That is the cost of the guarantee: the filename is a claim made by whatever
// wrote it, and these bytes are about to be executed as a service account.
// Bit rot, a half-finished copy from an older gvm, and a substituted file are
// the same observation from here, and all three must stop the install.
func hashFile(ctx context.Context, p, algo string) (Digest, error) {
	h, err := hasherFor(algo)
	if err != nil {
		return "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("provider: open %s: %w", p, err)
	}
	defer f.Close()
	if _, err := io.Copy(h, &ctxReader{ctx: ctx, r: f}); err != nil {
		return "", fmt.Errorf("provider: hash %s: %w", p, err)
	}
	return NewDigest(algo, h.Sum(nil)), nil
}

// copyVerified is Link's EXDEV fallback: copy, then hash what was written.
//
// The re-hash is the point. A copy that ran out of disk halfway, and a copy
// onto a filesystem that silently truncated, both leave a file at the
// destination with the right name, and a jar with the right name and the wrong
// bytes is executed exactly as eagerly as a correct one. On a mismatch the
// copy is removed here, so the caller's publish never happens and the
// destination keeps whatever it had.
func copyVerified(src, staged string, want Digest) error {
	if err := copyFile(src, staged); err != nil {
		return err
	}
	got, err := hashFile(context.Background(), staged, want.Algo())
	if err != nil {
		return err
	}
	if got != want {
		if rmErr := os.Remove(staged); rmErr != nil {
			return fmt.Errorf("%w: the copy of %s hashes to %s and could not be removed: %v",
				ErrDigestMismatch, want.Short(), got.Short(), rmErr)
		}
		return fmt.Errorf("%w: the copy of %s hashes to %s", ErrDigestMismatch,
			want.Short(), got.Short())
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("provider: open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, BlobMode)
	if err != nil {
		return fmt.Errorf("provider: create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("provider: copy %s: %w", src, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("provider: fsync %s: %w", dst, err)
	}
	return out.Close()
}

// isCrossDevice reports whether a link failed because the two paths are on
// different filesystems, which is the one link failure a copy can fix.
//
// Correct on Linux, which is where blobs are ever linked. Windows reports
// ERROR_NOT_SAME_DEVICE and does not map it onto syscall.EXDEV, so on the
// development machine the fallback does not engage and the link error
// surfaces; copyVerified is therefore tested directly rather than through this
// branch, so the half that could corrupt an install is covered on both
// platforms.
func isCrossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }

// ctxReader makes a multi-gigabyte hash or download cancellable.
//
// Without it, `gvm update --timeout` on a 65 GB app would return while the copy
// kept running, and the job lock would be held by work nobody is waiting for.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
