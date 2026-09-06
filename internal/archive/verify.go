package archive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Mode is how thoroughly Verify reads an archive (section 13.2).
type Mode string

const (
	// ModeShallow is the default and takes seconds: the whole-file sha256,
	// the gzip CRC, the manifest parsing, and the member count and names
	// matching the digest table. It does not re-hash member contents.
	ModeShallow Mode = "shallow"
	// ModeSpot extracts N members chosen by a PRNG seeded from the archive's
	// own sha256 -- always including the metadata members and the largest,
	// smallest and newest data members -- into a scratch directory, compares
	// them and deletes them. This is what runs automatically before every
	// destructive action.
	ModeSpot Mode = "spot"
	// ModeDeep re-hashes every member and takes minutes.
	ModeDeep Mode = "deep"
)

// DefaultSpot is the sample size of a spot check. Twenty-four members is
// enough that a truncated or partially-written archive is caught with near
// certainty while the check still costs one streaming pass and a few megabytes
// of scratch.
const DefaultSpot = 24

// VerifyOptions configures a check.
type VerifyOptions struct {
	Mode Mode
	// Spot is the sample size for ModeSpot; zero means DefaultSpot.
	Spot int
	// Scratch is where ModeSpot extracts to, normally
	// /var/lib/gvm/restore-scratch/<instance>. Empty means os.TempDir, which
	// is right for a test and wrong for a 2 GB member on a small VPS.
	Scratch string
	Now     func() time.Time
}

// Problem is one thing wrong with an archive. They are collected rather than
// returned one at a time: an operator deciding whether to trust a backup needs
// the shape of the damage, not its first instance.
type Problem struct {
	Member string `json:"member,omitempty"`
	Detail string `json:"detail"`
}

// Report is what a verification found.
type Report struct {
	Path     string        `json:"path"`
	Mode     Mode          `json:"mode"`
	SHA256   string        `json:"sha256"`
	Bytes    int64         `json:"bytes"`
	Members  int           `json:"members"`
	Checked  int           `json:"checked"`
	Legacy   bool          `json:"legacy,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	Problems []Problem     `json:"problems,omitempty"`
	Duration time.Duration `json:"duration"`
	Manifest *Manifest     `json:"-"`
}

// OK reports whether the archive may be trusted.
func (r *Report) OK() bool { return r != nil && len(r.Problems) == 0 }

func (r *Report) fail(member, format string, args ...any) {
	r.Problems = append(r.Problems, Problem{Member: member, Detail: fmt.Sprintf(format, args...)})
}

func (r *Report) warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// Err turns a report with problems into an error, or returns nil.
func (r *Report) Err() error {
	if r.OK() {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d problem(s) in %s", len(r.Problems), r.Path)
	for i, p := range r.Problems {
		if i == namedInError {
			fmt.Fprintf(&b, "; and %d more", len(r.Problems)-namedInError)
			break
		}
		b.WriteString("; ")
		if p.Member != "" {
			b.WriteString(p.Member)
			b.WriteString(": ")
		}
		b.WriteString(p.Detail)
	}
	return fmt.Errorf("%w: %s", ErrCorrupt, b.String())
}

// Verify reads an archive back and reports what it found.
//
// It never returns a Ref. Turning a verification into a receipt is VerifyRef's
// job, and the split is deliberate: `gvm backup verify` wants a report about
// an archive it has no intention of destroying anything with.
func Verify(ctx context.Context, path string, o VerifyOptions) (*Report, error) {
	now := clockOf(o.Now)
	start := now()

	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	rep := &Report{Path: path, Mode: o.mode(), Bytes: fi.Size()}

	// Pass one: the whole-file digest. Every mode needs it -- it is what
	// ModeShallow reports, and it is the seed ModeSpot's PRNG uses, which is
	// what makes a spot check reproducible against the same archive.
	sum, err := hashFile(ctx, path)
	if err != nil {
		return nil, err
	}
	rep.SHA256 = sum

	man, manErr := ReadArchiveManifest(path)
	switch {
	case manErr == nil:
		rep.Manifest = man
		rep.Members = len(man.Members)
		if verr := man.Validate(); verr != nil {
			rep.fail("", "%v", verr)
		}
	case errors.Is(manErr, ErrNoManifest):
		// A bash-suite tarball. It is not corrupt, it is old. Section 13.2's
		// integrity story simply does not exist for it, and saying so is more
		// useful than refusing to look at it.
		rep.Legacy = true
		rep.warn("no %s: this archive predates gvm (it looks like a bash-suite backup.sh "+
			"tarball). There are no per-member digests, so only the gzip CRC and the member "+
			"listing can be checked, and a restore from it cannot verify contents as they stream.",
			ManifestMember)
	default:
		// A manifest that is present and unreadable is a problem WITH the
		// archive, not a reason to refuse to answer the question. `gvm backup
		// verify` was asked whether this file can be trusted, and "no,
		// because its manifest is shredded" is the answer.
		rep.fail(ManifestMember, "could not be read: %v", manErr)
	}

	want := map[string]Member{}
	if man != nil {
		for _, m := range man.Members {
			want[m.Path] = m
		}
	}

	var pick map[string]bool
	if rep.Mode == ModeSpot && man != nil {
		pick = selectSpot(man.Members, o.spot(), sum)
	}

	scratch := ""
	if rep.Mode == ModeSpot {
		scratch, err = os.MkdirTemp(o.scratchRoot(), "gvm-verify-")
		if err != nil {
			return nil, fmt.Errorf("archive: verify scratch: %w", err)
		}
		defer os.RemoveAll(scratch)
	}

	// Pass two: stream the whole thing. Even a spot check has to decompress
	// past every member to reach the ones it sampled -- tar is sequential --
	// so what "spot" actually saves is the hashing and the scratch writes,
	// which is where the minutes are.
	seen := map[string]bool{}
	err = streamMembers(ctx, path, func(hdr *tar.Header, r io.Reader) error {
		name := strings.TrimSuffix(hdr.Name, "/")
		if strings.HasPrefix(name, MetaPrefix) {
			// The metadata members are already checked: manifest.json parsed,
			// and members.json was compared against the digest the head
			// records for it. Re-reading them here would prove nothing new.
			_, err := io.Copy(io.Discard, r)
			return err
		}
		seen[name] = true
		m, known := want[name]
		if man != nil && !known {
			rep.fail(name, "present in the tarball but absent from the digest table")
		}

		switch {
		case rep.Mode == ModeDeep && m.Kind == KindFile,
			rep.Mode == ModeSpot && pick[name] && m.Kind == KindFile:
			got, n, herr := hashInto(ctx, r, scratchPathFor(scratch, name))
			if herr != nil {
				return herr
			}
			rep.Checked++
			if known {
				if n != m.Size {
					rep.fail(name, "is %d bytes, the manifest records %d", n, m.Size)
				}
				if got != m.SHA256 {
					rep.fail(name, "hashes %s, the manifest records %s", got, m.SHA256)
				}
			}
		default:
			// Not sampled: the bytes still have to stream past to reach the
			// next header, but nothing is hashed and nothing is written.
			// Checked counts digests compared, not members walked, so it is
			// not incremented here.
			if _, err := io.Copy(io.Discard, r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// A gzip CRC failure, a truncated stream or an unreadable header all
		// land here, and all of them mean the same thing to a caller about to
		// delete a world: do not.
		rep.fail("", "%v", err)
		rep.Duration = now().Sub(start)
		return rep, nil
	}

	if man != nil {
		var missing []string
		for _, m := range man.Members {
			if !seen[m.Path] {
				missing = append(missing, m.Path)
			}
		}
		sort.Strings(missing)
		for i, p := range missing {
			if i == namedInError {
				rep.fail("", "and %d further members listed in the digest table are missing "+
					"from the tarball", len(missing)-namedInError)
				break
			}
			rep.fail(p, "listed in the digest table but missing from the tarball")
		}
	} else {
		rep.Members = len(seen)
	}

	rep.Duration = now().Sub(start)
	return rep, nil
}

// VerifyRef verifies an archive and, on success, stamps the Ref as a receipt.
//
// This is the only function that sets VerifiedAt, which is what makes "a Ref
// that authorizes a destroy has been read back" a property of the code rather
// than of everybody remembering.
func VerifyRef(ctx context.Context, ref *Ref, o VerifyOptions) (*Report, error) {
	if ref == nil {
		return nil, errors.New("archive: VerifyRef needs a Ref to verify")
	}
	rep, err := Verify(ctx, ref.Path, o)
	if err != nil {
		return nil, err
	}
	if err := rep.Err(); err != nil {
		return rep, err
	}
	// A receipt has to mean somebody compared the contents against something.
	// Verify is permissive about both of these on purpose -- a legacy tarball
	// is old rather than corrupt, and `gvm backup verify` should still report
	// on it -- but permissive is the wrong answer for the function whose stamp
	// is what Destroy, reconcile.Remove and Prune accept as authority to delete
	// a world. The check is on the digest table rather than on rep.Checked
	// because ModeShallow legitimately compares no member CONTENTS; what it
	// must never do is pass because there was nothing to compare against.
	if rep.Legacy {
		return rep, fmt.Errorf("%w: %s carries no manifest, so none of its contents were checked "+
			"against anything; it cannot authorise a destroy", ErrNoManifest, ref.Path)
	}
	if rep.Manifest == nil || len(rep.Manifest.Members) == 0 {
		return rep, fmt.Errorf("%w: %s has no member digest table, so there is nothing to check its "+
			"contents against; it cannot authorise a destroy", ErrCorrupt, ref.Path)
	}
	if ref.SHA256 != "" && ref.SHA256 != rep.SHA256 {
		return rep, fmt.Errorf("%w: %s hashes %s now and %s when it was written",
			ErrCorrupt, ref.Path, rep.SHA256, ref.SHA256)
	}
	ref.SHA256 = rep.SHA256
	ref.Bytes = rep.Bytes
	ref.VerifiedAt = clockOf(o.Now)().UTC()
	return rep, nil
}

func (o VerifyOptions) mode() Mode {
	switch o.Mode {
	case ModeSpot, ModeDeep, ModeShallow:
		return o.Mode
	default:
		return ModeShallow
	}
}

func (o VerifyOptions) spot() int {
	if o.Spot <= 0 {
		return DefaultSpot
	}
	return o.Spot
}

func (o VerifyOptions) scratchRoot() string {
	if o.Scratch == "" {
		return ""
	}
	// MkdirTemp with an empty dir means os.TempDir; creating the caller's
	// scratch root first means a first spot check on a fresh box works.
	if err := os.MkdirAll(o.Scratch, 0o700); err != nil {
		return ""
	}
	return o.Scratch
}

// selectSpot chooses which members a spot check reads.
//
// The largest, smallest and newest members are always in: the largest is where
// a truncated write shows up, the smallest catches an off-by-one in the tar
// padding, and the newest is the member most likely to have been written while
// the world was still moving. The rest are drawn by a PRNG seeded from the
// archive's own sha256, so the same archive is always sampled the same way and
// a "verify passed yesterday, failed today" is a fact about the bytes rather
// than about the dice.
func selectSpot(members []Member, n int, archiveSHA string) map[string]bool {
	pick := map[string]bool{}
	files := make([]Member, 0, len(members))
	for _, m := range members {
		if m.Kind == KindFile {
			files = append(files, m)
		}
	}
	if len(files) == 0 {
		return pick
	}

	largest, smallest, newest := files[0], files[0], files[0]
	for _, m := range files[1:] {
		if m.Size > largest.Size {
			largest = m
		}
		if m.Size < smallest.Size {
			smallest = m
		}
		if m.MTime.After(newest.MTime) {
			newest = m
		}
	}
	for _, m := range []Member{largest, smallest, newest} {
		pick[m.Path] = true
	}

	// The draw is bounded by the number of DISTINCT paths, because pick is
	// keyed by path. Bounding it by the number of table entries instead makes
	// the ceiling unreachable for a table that names one path twice, and the
	// loop below then spins forever -- with no context and no iteration cap, so
	// a deadlined verify cannot break out of it. ReadArchiveManifest refuses
	// such a table now, but this is the loop that must terminate for any input
	// it is given.
	//
	// First occurrence wins, so for any archive without duplicates cand is
	// files in the same order and the draw sequence is byte-identical: the
	// reproducibility this function exists for is unchanged, and an archive
	// verified by an older build still samples the same members.
	cand := make([]Member, 0, len(files))
	inCand := make(map[string]bool, len(files))
	for _, m := range files {
		if !inCand[m.Path] {
			inCand[m.Path] = true
			cand = append(cand, m)
		}
	}

	seed, err := hex.DecodeString(archiveSHA)
	if err != nil || len(seed) < 32 {
		// No usable seed means take the head of the list rather than nothing:
		// a spot check that silently degrades to zero members would report
		// success on a shredded archive.
		for i := 0; i < len(cand) && len(pick) < n; i++ {
			pick[cand[i].Path] = true
		}
		return pick
	}
	var key [32]byte
	copy(key[:], seed)
	rng := rand.New(rand.NewChaCha8(key))
	for len(pick) < n && len(pick) < len(cand) {
		pick[cand[rng.IntN(len(cand))].Path] = true
	}
	return pick
}

// streamMembers decompresses an archive once and hands each member to fn.
func streamMembers(ctx context.Context, path string, fn func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("archive: gunzip %s: %w", path, err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// Reaching the tar trailer is not the same as reaching the end of
			// the gzip stream, and the gzip CRC lives at the end of the gzip
			// stream. Draining to EOF is what makes ModeShallow's "gzip CRC"
			// claim true.
			if _, derr := io.Copy(io.Discard, zr); derr != nil {
				return fmt.Errorf("archive: %s: %w", path, derr)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("archive: %s: %w", path, err)
		}
		if err := fn(hdr, tr); err != nil {
			return err
		}
	}
}

// hashInto hashes a member, optionally writing it to scratch on the way past.
//
// The write is what makes a spot check a real read rather than a decompression
// benchmark: it proves the bytes can land on a filesystem, which is the thing
// a restore is about to ask of them.
func hashInto(ctx context.Context, r io.Reader, dest string) (string, int64, error) {
	h := sha256.New()
	var w io.Writer = h
	if dest != "" {
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return "", 0, fmt.Errorf("archive: verify scratch: %w", err)
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return "", 0, fmt.Errorf("archive: verify scratch: %w", err)
		}
		defer f.Close()
		w = io.MultiWriter(h, f)
	}
	counted := &countingWriter{w: w}
	if err := copyCtx(ctx, counted, r); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), counted.n, nil
}

// scratchPathFor maps a member name into the scratch directory, refusing
// anything that would escape it.
func scratchPathFor(scratch, member string) string {
	if scratch == "" {
		return ""
	}
	rel, err := safeMemberPath(member)
	if err != nil {
		return ""
	}
	return filepath.Join(scratch, filepath.FromSlash(rel))
}
