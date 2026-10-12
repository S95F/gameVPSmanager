package blobstore

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/provider"
)

// KeepSet is the live set a garbage collection may not delete, plus a record of
// where it came from and what it could not read.
//
// The second half is the point. `gvm content gc` builds the live set by reading
// every instance's lockfile, and the lock is truth (section 12.10) -- so a run
// in which one lockfile failed to parse, or one instance directory could not be
// listed, has a set that is missing everything that instance holds. The set
// would still be a perfectly valid predicate; it would just quietly answer "no"
// for several gigabytes of mods that are very much in use.
//
// So a hole is recorded as a hole, and Check refuses. The zero value has no
// sources and is therefore refused too, which is what makes a forgotten
// assignment a no-op rather than a cache wipe: a KeepSet another package can
// write as a composite literal carries no digests, and the one thing it must
// not do is read as "keep nothing".
type KeepSet struct {
	keep map[Digest]struct{}
	// from is every lockfile that was read successfully, named in the refusal
	// so an operator can see which instances were accounted for.
	from []string
	// holes are the reads that failed. Any hole refuses the whole GC: there is
	// no safe way to delete from a cache whose live set is partly unknown.
	holes []error
	// trees counts the entries whose digest is not a byte digest -- workshop
	// items the server downloaded itself. They are not in this store and not
	// holes either, and counting them keeps "0 digests kept" from looking like
	// a failed read on a Project Zomboid box.
	trees int
}

// NewKeepSet starts an empty keep set. It is not usable until something has
// been added: see Check.
func NewKeepSet() *KeepSet {
	return &KeepSet{keep: make(map[Digest]struct{})}
}

// Add records one digest as live.
//
// A non-byte digest (a workshop tree, a buildid) is counted and not kept: there
// is nothing in this store at that address, so keeping it would be meaningless,
// and treating it as a hole would make every PZ instance refuse to GC.
func (k *KeepSet) Add(d Digest) {
	if k == nil {
		return
	}
	canon, err := provider.ParseDigest(d)
	if err != nil {
		// A digest the parser rejects is a hole, not a shrug: it came out of a
		// lockfile gvm wrote, so either the file was edited or an older gvm
		// wrote something this one cannot read. Either way the live set is not
		// fully known.
		k.holes = append(k.holes, fmt.Errorf("unparseable digest %q in a lockfile: %w", d, err))
		return
	}
	if !canon.Verifiable() {
		k.trees++
		return
	}
	if k.keep == nil {
		k.keep = make(map[Digest]struct{})
	}
	k.keep[canon] = struct{}{}
}

// AddLock records every digest one instance's lockfile holds live.
//
// from is the lockfile's path, for the refusal message. A nil lock is a hole:
// the caller asked for an instance to be accounted for and nothing was.
func (k *KeepSet) AddLock(from string, l *content.Lock) {
	if k == nil {
		return
	}
	if l == nil {
		k.Unreadable(from, errors.New("no lock"))
		return
	}
	for _, block := range l.Blocks() {
		sl, err := l.SourceFor(block)
		if err != nil {
			k.Unreadable(from, fmt.Errorf("block %q: %w", block, err))
			continue
		}
		for _, e := range sl.Entries {
			if e.Artifact == nil || strings.TrimSpace(string(e.Artifact.Digest)) == "" {
				// An entry with no artifact is an item that was never
				// materialised -- a server_boot item, or one the plan recorded
				// as blocked. It holds no blob, so it keeps none.
				continue
			}
			k.Add(e.Artifact.Digest)
		}
	}
	k.from = append(k.from, from)
}

// Unreadable records that one source of live digests could not be read.
//
// Calling it is how a caller says "I tried and failed" rather than "there was
// nothing there", and the two must not look alike to a GC.
func (k *KeepSet) Unreadable(from string, err error) {
	if k == nil {
		return
	}
	if err == nil {
		err = errors.New("unspecified failure")
	}
	k.holes = append(k.holes, fmt.Errorf("%s: %w", from, err))
}

// Predicate is the only way to obtain the predicate provider.Store.GC takes,
// and it runs Check first.
//
// A set with a hole cannot produce one AT ALL, which is what makes the
// completeness rule a property of this type rather than of one function in
// another package. The predicate used to be an exported method whose doc
// comment advertised the ungated spelling, and provider.Store.GC is exported
// and refuses only a nil predicate -- so `store.GC(ks.Keep)` compiled from
// anywhere and reached os.Remove holding a live set that answered "no" for
// every instance whose lockfile had failed to read. Same reasoning as Place's:
// there is no shape of this operation in which a caller gets a usable
// predicate without the completeness question having been asked.
//
// A nil func is returned alongside the error, never a usable one, so a caller
// that ignored the error deletes nothing rather than everything.
func (k *KeepSet) Predicate() (func(Digest) bool, error) {
	if err := k.Check(); err != nil {
		return nil, err
	}
	return k.holds, nil
}

// holds is the raw lookup. Unexported, so Predicate is the only door; named
// `holds` rather than `keep` because that collides with the field.
func (k *KeepSet) holds(d Digest) bool {
	if k == nil || k.keep == nil {
		return false
	}
	canon, err := provider.ParseDigest(d)
	if err != nil {
		// GC only ever asks about an address it read off its own tree, so an
		// unparseable one cannot be in the live set. Answering false deletes
		// it, which is correct: nothing can reference an address nothing can
		// name.
		return false
	}
	_, ok := k.keep[canon]
	return ok
}

// Check reports whether this set is complete enough to delete against.
//
// Two refusals, and they are different failures. No sources at all means the
// caller never read a lockfile -- the zero value, or a loop that found no
// instances because it looked in the wrong directory -- and running a GC
// against it would empty the cache. A hole means the caller read some and
// failed on others, which deletes exactly the instances it failed to read.
func (k *KeepSet) Check() error {
	if k == nil {
		return fmt.Errorf("%w: no keep set at all", ErrIncompleteKeepSet)
	}
	if len(k.holes) > 0 {
		return fmt.Errorf("%w: %d of %d source(s) could not be read, so the live set is "+
			"partly unknown and a collection would delete whatever those instances hold; "+
			"fix the reads or pass the instances explicitly: %w",
			ErrIncompleteKeepSet, len(k.holes), len(k.holes)+len(k.from), errors.Join(k.holes...))
	}
	if len(k.from) == 0 {
		return fmt.Errorf("%w: it was built from no lockfiles, which as a predicate means "+
			"\"keep nothing\" and as a garbage collection means deleting the whole cache; "+
			"a box with no instances has nothing to collect", ErrIncompleteKeepSet)
	}
	return nil
}

// Len is how many blob addresses are live.
func (k *KeepSet) Len() int {
	if k == nil {
		return 0
	}
	return len(k.keep)
}

// Trees is how many entries named something this store does not hold -- a
// workshop item the server downloaded itself.
func (k *KeepSet) Trees() int {
	if k == nil {
		return 0
	}
	return k.trees
}

// Sources are the lockfiles this set was built from, sorted for a stable
// report.
func (k *KeepSet) Sources() []string {
	if k == nil || len(k.from) == 0 {
		return nil
	}
	out := append([]string(nil), k.from...)
	sort.Strings(out)
	return out
}

// Digests are the live addresses, sorted, for `gvm content gc --dry-run`.
func (k *KeepSet) Digests() []Digest {
	if k == nil {
		return nil
	}
	out := make([]Digest, 0, len(k.keep))
	for d := range k.keep {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// String is the line a GC report prints before it deletes anything.
func (k *KeepSet) String() string {
	if k == nil {
		return "keep set: <none>"
	}
	s := fmt.Sprintf("keep set: %d blob(s) from %d lockfile(s)", len(k.keep), len(k.from))
	if k.trees > 0 {
		s += fmt.Sprintf(", %d tree artifact(s) not in this store", k.trees)
	}
	if len(k.holes) > 0 {
		s += fmt.Sprintf(", %d UNREADABLE", len(k.holes))
	}
	return s
}
