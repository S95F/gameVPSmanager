// Package blobstore is the content subsystem's view of the content-addressed
// blob store.
//
// It declares no store of its own. internal/provider already owns the digest
// discipline these bytes need, and it is not a policy that survives being
// written twice:
//
//   - Get returns a path only on a digest match, re-hashing the file on every
//     call even though it is stored under its own hash, because the filename is
//     a claim made by whatever wrote it and these bytes are about to be executed
//     as a service account.
//   - Put stores NOTHING on a mismatch -- not at the final address and not as a
//     staging file -- so a substituted or truncated download leaves the store
//     exactly as it was.
//   - A buildid or a tree digest is refused by name rather than falling back to
//     "well, the file is there".
//
// So Store here is provider.BlobStore, an ALIAS, and this package adds the two
// operations the content subsystem needs that the install seam does not:
//
//  1. Place -- verify a blob and put it at a JAILED content destination, in one
//     call, so no code path exists that resolves a destination and then forgets
//     to verify, or verifies and then writes somewhere else.
//  2. KeepSet -- a garbage-collection keep set that refuses to be used when it
//     was built from an incomplete read of the lockfiles. The lock is truth
//     (section 12.10); a keep set assembled while one lockfile failed to parse
//     would delete that instance's blobs, and a GC that is "only as safe as the
//     set is complete" needs completeness to be a question the set can answer.
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/S95F/gameVPSmanager/internal/content/jail"
	"github.com/S95F/gameVPSmanager/internal/provider"
)

// Store is provider.BlobStore, reused rather than redeclared.
//
// An alias, not a second interface: the same store serves the install seam and
// the content seam, and a content.BlobStore that had to be adapted at the
// boundary would be two digest policies one release apart from disagreeing
// about what counts as verified.
type Store = provider.BlobStore

// Digest and its algorithms come from the same place for the same reason: the
// parser that rejects a truncated, brute-forceable digest is the parser, and
// there is exactly one of it.
type Digest = provider.Digest

// The refusals, re-exported rather than redeclared so a caller matches one
// package's sentinels.
var (
	// ErrDigestMismatch is the refusal the whole store exists for: the bytes
	// on disk are not the bytes that were approved. TERMINAL.
	ErrDigestMismatch = provider.ErrDigestMismatch
	// ErrBlobNotFound is an address the store holds nothing at. NOT terminal:
	// the fix is to fetch it, which is what the caller does next.
	ErrBlobNotFound = provider.ErrBlobNotFound
	// ErrNotByteDigest is a digest the store cannot verify by hashing -- a
	// tree digest or a buildid. A workshop item never enters the store.
	ErrNotByteDigest = provider.ErrNotByteDigest
	// ErrBadDigest is a malformed digest string.
	ErrBadDigest = provider.ErrBadDigest

	// ErrIncompleteKeepSet is a GC asked to run against a keep set that does
	// not know the whole live set. See KeepSet.
	ErrIncompleteKeepSet = errors.New("blobstore: the garbage-collection keep set is incomplete")
	// ErrNoStore is a nil store.
	ErrNoStore = errors.New("blobstore: no blob store")
)

// Terminal reports whether retrying err could ever change the answer. It is
// provider's classification, asked in one place so the fetch ladder and the
// store agree.
func Terminal(err error) bool { return provider.Terminal(err) }

// Open returns the blob store rooted at dir, creating it if needed. An empty
// dir means the real one, /var/lib/gvm/blobs.
//
// The store is shared across instances on purpose: it is addressed by content,
// so two instances running the same mod hold one copy and one hardlink each.
func Open(dir string) (Store, error) {
	s, err := provider.NewStore(dir)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Place verifies a blob and puts it at a content destination, in one call.
//
// The destination is resolved through the content jail here rather than being
// passed in, so there is no shape of this operation in which a caller resolves
// a path and then forgets to verify the bytes, or verifies the bytes and then
// writes them somewhere the jail never saw. provider.Store.Link re-hashes the
// source before linking and re-hashes the copy on the cross-device fallback, so
// the bytes that arrive are the bytes rel was approved for.
//
// It takes no context, and that is a statement rather than an omission:
// BlobStore.Link takes none either, so the hash it performs is not
// interruptible, and accepting a context here to ignore it would advertise a
// cancellation that does not happen. A Place of a 400 MB artifact runs to
// completion; the budget that bounds it is the job's, one level up.
func Place(s Store, want Digest, j *jail.Jail, rel string) (jail.Path, error) {
	var none jail.Path
	if s == nil {
		return none, fmt.Errorf("%w: cannot place %s", ErrNoStore, want.Short())
	}
	// The jail is asked first, and the order is deliberate: the store's job is
	// to decide what the bytes are and the jail's is to decide where they may
	// land, and asking the other way round pays for a full hash before
	// discovering the destination was /etc.
	dst, err := j.Target(rel)
	if err != nil {
		return none, err
	}
	// Link re-derives the digest itself (and re-derives it again on the
	// cross-device copy), so there is no Get here. A pre-check would read
	// every byte of the artifact a second time for a better-ordered error
	// message, which on a 250-file modpack is the whole pack hashed twice.
	if err := s.Link(want, dst); err != nil {
		return none, fmt.Errorf("place %s at %s: %w", want.Short(), rel, err)
	}
	return dst, nil
}

// Ingest stores bytes a fetcher produced, iff they hash to want.
//
// It exists so that the fetch side never holds the store's Put: open is handed
// the writer, so a client that streams a hostile response into it cannot also
// decide what the digest was. A short or substituted stream fails the hash and
// Put stores nothing, which is also what makes a truncated download
// indistinguishable from a tampered one -- correctly, because the response to
// both is to fetch it again.
func Ingest(ctx context.Context, s Store, want Digest, open func(io.Writer) error) (jail.Path, error) {
	var none jail.Path
	if s == nil {
		return none, fmt.Errorf("%w: cannot ingest %s", ErrNoStore, want.Short())
	}
	if open == nil {
		return none, fmt.Errorf("%w: no producer for %s", ErrBadDigest, want.Short())
	}

	pr, pw := io.Pipe()
	go func() {
		err := open(pw)
		// CloseWithError(nil) is Close, so the single call covers both exits
		// and the reader always sees an end.
		_ = pw.CloseWithError(err)
	}()
	defer pr.Close()

	p, err := s.Put(ctx, pr, want)
	if err != nil {
		return none, fmt.Errorf("ingest %s: %w", want.Short(), err)
	}
	return p, nil
}

// Need reports which of wants the store does not already hold.
//
// It is a HINT and nothing more: Has hashes nothing, so a corrupted blob counts
// as held here and the mismatch surfaces at Place, where Link re-hashes. That is
// the right division -- this answers "how much is there left to download" for a
// plan, and Place answers "are these the approved bytes" for a write. A caller
// that treated this as verification would be trusting a filename.
//
// Order is preserved and duplicates collapse, so the answer reads as a download
// list rather than as a set.
func Need(s Store, wants []Digest) []Digest {
	out := make([]Digest, 0, len(wants))
	seen := make(map[Digest]bool, len(wants))
	for _, d := range wants {
		canon, err := provider.ParseDigest(d)
		if err != nil || !canon.Verifiable() {
			// Not a byte digest: a workshop tree, which the server downloads
			// itself. Not this store's to fetch, and not a hole either.
			continue
		}
		if seen[canon] {
			continue
		}
		seen[canon] = true
		if s == nil || !s.Has(canon) {
			out = append(out, canon)
		}
	}
	return out
}

// Collect is GC with a keep set that cannot be half-built.
//
// provider.Store.GC already refuses a nil predicate, because a nil predicate
// reads as "keep nothing" and would delete the whole cache on a forgotten
// assignment. The other half of the same hole -- a predicate built correctly
// from an INCOMPLETE reading of the lockfiles -- is closed by KeepSet.Predicate
// and not here. The blobs are derived state and re-downloadable, but a GC that
// silently deleted a 6 GB modpack because one lockfile had a syntax error would
// be paid for in the next download cycle, with the server stopped.
//
// So this is a convenience over Predicate plus GC and is NOT the sole guard:
// there is no exported way to get a usable predicate out of a holed set, which
// is what stops the next caller writing `store.GC(...)` directly and skipping
// the question.
func Collect(s Store, ks *KeepSet) (freed int64, err error) {
	if s == nil {
		return 0, ErrNoStore
	}
	p, err := ks.Predicate()
	if err != nil {
		return 0, err
	}
	return s.GC(p)
}
