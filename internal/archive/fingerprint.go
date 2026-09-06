package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Fingerprint is a cheap description of a world tree, taken before and after
// the one supervised downtime window of an adoption cutover.
//
// It is not a checksum of the world and must never be used as one. See
// Contains.
type Fingerprint struct {
	// Paths are the roots that were walked, relative to Base.
	Paths []string `json:"paths"`
	Files int      `json:"files"`
	Dirs  int      `json:"dirs"`
	Bytes int64    `json:"bytes"`
	// NewestMod is the most recent mtime anywhere under the roots. This is
	// the half of D-34 that proves the server actually wrote.
	NewestMod time.Time `json:"newest_mod"`
	// Digest is over the sorted relative path set. It is recorded for
	// forensics -- "is this the same shape of world I saw yesterday" -- and
	// is NOT the comparison Contains makes.
	Digest string `json:"digest"`

	// Entries is the relative path set. It is what makes the superset test
	// possible, and it is deliberately dropped when a fingerprint is embedded
	// in an archive manifest: 8412 paths do not fit in the head budget, and
	// the check that needs them is a live pre/post comparison during a
	// cutover, never a read-back from a tarball.
	Entries []string `json:"entries,omitempty"`
}

// MissingPathsError is what a failed superset test returns.
//
// It names the paths rather than only counting them because the operator's
// next question after "the world verification failed" is always "which files",
// and an answer of "412 of them" sends them to a shell instead of to the fix.
type MissingPathsError struct {
	// Missing is every path present before and absent after.
	Missing []string
	// Before and After are the set sizes, so the shape of the failure --
	// "412 gone" versus "everything gone" -- is visible without counting.
	Before, After int
}

// namedInError caps how many paths an error message spells out. Beyond this
// the count is the information; a 8412-line error is a scroll, not a message.
const namedInError = 12

func (e *MissingPathsError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "world verification failed: %d of %d paths present before the cutover are "+
		"absent after it (the world now has %d)", len(e.Missing), e.Before, e.After)
	if e.After < e.Before/2 {
		b.WriteString("; a world this much smaller is a FRESHLY GENERATED one, which is what " +
			"a wrong servername or -cachedir produces")
	}
	b.WriteString(": ")
	for i, p := range e.Missing {
		if i == namedInError {
			fmt.Fprintf(&b, " and %d more", len(e.Missing)-namedInError)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p)
	}
	return b.String()
}

// ErrWorldNotWritten is the other half of D-34: the file set is intact but
// nothing has been written since the fingerprint was taken.
var ErrWorldNotWritten = errors.New("world verification failed: the newest mtime has not advanced")

// ErrNoEntries is a fingerprint that carries only its summary.
var ErrNoEntries = errors.New("fingerprint: no path set recorded, so the superset test cannot run")

// Contains reports whether other -- the post-start fingerprint -- still holds
// everything f held, and that the server has written since.
//
// This is D-34 in code. Digest equality is impossible: Project Zomboid writes
// to the world on BOTH a clean stop and a start, so the digest differs on every
// successful cutover and a digest check would false-alarm every time and be
// switched off within a week. The subset test still catches the only
// catastrophe that matters -- the server generating a fresh empty world beside
// the real one because the servername or -cachedir was wrong -- and the
// advancing mtime catches the opposite failure, a server that came up pointing
// at a read-only or stale copy and never wrote at all.
//
// Files being ADDED is normal and passes. Files disappearing never is.
func (f *Fingerprint) Contains(other *Fingerprint) error {
	if f == nil || other == nil {
		return fmt.Errorf("%w: comparing against a nil fingerprint", ErrNoEntries)
	}
	if len(f.Entries) == 0 || len(other.Entries) == 0 {
		return fmt.Errorf("%w: before has %d entries, after has %d",
			ErrNoEntries, len(f.Entries), len(other.Entries))
	}

	have := make(map[string]struct{}, len(other.Entries))
	for _, p := range other.Entries {
		have[p] = struct{}{}
	}
	var missing []string
	for _, p := range f.Entries {
		if _, ok := have[p]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return &MissingPathsError{Missing: missing, Before: len(f.Entries), After: len(other.Entries)}
	}

	// Strictly after, not "not before". Equal mtimes mean the server was
	// started and stopped without touching the world, which for every game
	// gvm supports means it was not really running against this directory.
	if !other.NewestMod.After(f.NewestMod) {
		return fmt.Errorf("%w: it was %s before the cutover and %s after, so nothing under %s "+
			"has been written; the server is very likely running against a different directory",
			ErrWorldNotWritten, f.NewestMod.Format(time.RFC3339), other.NewestMod.Format(time.RFC3339),
			strings.Join(f.Paths, ", "))
	}
	return nil
}

// summary returns the form that goes into an archive manifest: everything
// except the path set.
func (f *Fingerprint) summary() *Fingerprint {
	if f == nil {
		return nil
	}
	c := *f
	c.Entries = nil
	return &c
}

// TakeFingerprint walks the named roots under base and describes what it finds.
//
// Symlinks are recorded by name and never followed: following one would let a
// link inside the world point at /usr and make the fingerprint of every
// cutover identical and enormous.
func TakeFingerprint(ctx context.Context, base string, roots []string) (*Fingerprint, error) {
	if strings.TrimSpace(base) == "" {
		return nil, errors.New("fingerprint: no base directory")
	}
	if len(roots) == 0 {
		return nil, errors.New("fingerprint: no world paths; a fingerprint of nothing would " +
			"pass every check it is asked")
	}

	f := &Fingerprint{Paths: make([]string, 0, len(roots))}
	seen := map[string]bool{}

	for _, r := range roots {
		rel := cleanRel(r)
		f.Paths = append(f.Paths, rel)
		abs := filepath.Join(base, filepath.FromSlash(rel))

		err := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				// A world path that does not exist is a fact worth recording,
				// not an error: before a cutover it may legitimately be
				// absent, and afterwards its absence is exactly what Contains
				// is being asked about.
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("fingerprint: walk %s: %w", p, err)
			}
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			rp, rerr := filepath.Rel(base, p)
			if rerr != nil {
				return fmt.Errorf("fingerprint: relativise %s: %w", p, rerr)
			}
			slash := filepath.ToSlash(rp)
			if seen[slash] {
				return nil
			}
			seen[slash] = true

			fi, ferr := d.Info()
			if ferr != nil {
				if errors.Is(ferr, fs.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("fingerprint: stat %s: %w", p, ferr)
			}
			f.Entries = append(f.Entries, slash)
			if fi.ModTime().After(f.NewestMod) {
				f.NewestMod = fi.ModTime().UTC()
			}
			switch {
			case fi.IsDir():
				f.Dirs++
			default:
				f.Files++
				if fi.Mode().IsRegular() {
					f.Bytes += fi.Size()
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	sort.Strings(f.Entries)
	f.NewestMod = f.NewestMod.UTC().Truncate(time.Second)
	f.Digest = "sha256:" + digestOfSet(f.Entries)
	return f, nil
}

// digestOfSet hashes the sorted path set, newline separated.
func digestOfSet(entries []string) string {
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
