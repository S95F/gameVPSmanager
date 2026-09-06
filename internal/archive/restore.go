package archive

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// stagingPrefix and replacedPrefix name the two directories restore creates
// inside a destination base.
//
// They are inside the destination on purpose. rename(2) is only atomic within
// one filesystem, and the whole ordering guarantee of section 13.2 step 4 --
// verify every byte, THEN rename, so a mismatch aborts before anything moves
// -- depends on the staged tree and the live tree being on the same one. A
// staging directory under /tmp would silently degrade the rename into a copy.
const (
	stagingPrefix  = ".gvm-restore-"
	replacedPrefix = ".gvm-replaced-"
)

// RestoreOptions configures an extraction.
type RestoreOptions struct {
	// Source is the archive to read.
	Source string

	// Dest maps a manifest root prefix to the directory it is restored into.
	// A prefix absent from the map restores to the base the manifest records,
	// which is the DB-free path: the archive knows where it came from.
	Dest map[string]string

	// To is `restore --to <dir>`: every root is restored under <dir>/<prefix>
	// instead of over the live tree, and Dest is ignored. This is the
	// inspection path, and it skips nothing else -- the same stream
	// verification runs.
	To string

	// ExpectGameManifestSHA256 is the pinned game manifest of the instance
	// being restored onto. A mismatch is refused unless
	// AcceptManifestDrift, because restoring a 2026 world onto a 2027
	// manifest whose shutdown ladder changed is a decision, not a default.
	ExpectGameManifestSHA256 string
	AcceptManifestDrift      bool

	// PreserveOwnership applies the recorded uid/gid instead of leaving the
	// files owned by whoever ran the restore.
	PreserveOwnership bool

	// Chown is the seam ownership goes through, so the logic is testable off
	// Linux. Nil uses the platform's.
	Chown func(path string, uid, gid int) error

	// AllowLegacy permits an archive with no manifest. It defaults to false
	// so that a caller who has not thought about reduced verification does
	// not get it silently.
	AllowLegacy bool

	Now    func() time.Time
	OnNote func(string)
}

// RestoreReport is what happened.
type RestoreReport struct {
	Source   string    `json:"source"`
	Legacy   bool      `json:"legacy,omitempty"`
	Members  int       `json:"members"`
	Bytes    int64     `json:"bytes"`
	Warnings []string  `json:"warnings,omitempty"`
	Manifest *Manifest `json:"-"`

	// Restored maps each root prefix to the directory its members landed in.
	Restored map[string]string `json:"restored"`

	// Replaced names the holding directories that the previous contents were
	// moved into, per destination. Restore does NOT delete them: this package
	// is the only thing in gvm that removes a world, and it does that only
	// through Destroy, holding a verified receipt. The caller reports these
	// paths and reclaims them with a Ref in hand.
	Replaced []string `json:"replaced,omitempty"`
}

func (r *RestoreReport) warn(o RestoreOptions, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.Warnings = append(r.Warnings, msg)
	if o.OnNote != nil {
		o.OnNote(msg)
	}
}

// Restore extracts an archive, verifying every byte before anything moves.
//
// The order is section 13.2 step 4 and it has no way around it: members stream
// into a staging directory on the destination filesystem with their digests
// checked as the bytes go past, and only once the whole stream has verified is
// anything renamed into place. A mismatch aborts with the live tree untouched.
func Restore(ctx context.Context, o RestoreOptions) (rep *RestoreReport, err error) {
	now := clockOf(o.Now)
	stamp := now().UTC().Format(Stamp)

	if strings.TrimSpace(o.Source) == "" {
		return nil, errors.New("archive: restore needs a source archive")
	}
	rep = &RestoreReport{Source: o.Source, Restored: map[string]string{}}

	man, manErr := ReadArchiveManifest(o.Source)
	switch {
	case manErr == nil:
		rep.Manifest = man
		if verr := man.Validate(); verr != nil {
			return nil, verr
		}
		if derr := checkDrift(man, o); derr != nil {
			return nil, derr
		}
	case errors.Is(manErr, ErrNoManifest):
		if !o.AllowLegacy {
			return nil, fmt.Errorf("%w: %s. Pass AllowLegacy (the CLI's --legacy) to restore it "+
				"anyway, accepting that contents cannot be verified as they stream",
				manErr, o.Source)
		}
		rep.Legacy = true
		rep.warn(o, "%s carries no %s: it predates gvm and looks like a bash-suite backup.sh "+
			"tarball. There are no per-member digests, so this restore checks only the gzip CRC "+
			"and the path safety of each member. Modes and mtimes are taken from the tar headers; "+
			"ownership is not recorded at all.", o.Source, ManifestMember)
	default:
		return nil, manErr
	}

	roots, err := destinations(man, o)
	if err != nil {
		return nil, err
	}

	// One staging directory per destination, so every rename at the end is
	// within a filesystem.
	stages := map[string]string{}
	// roots is the same set of staging directories held open as os.Root
	// handles, and EVERY member is created through one. os.Root refuses to
	// traverse a symlink that leaves its root, which is the only containment
	// check that does not have to reason about what a chain of links will
	// resolve to -- see linkAncestors below for why lexical reasoning is not
	// enough on its own.
	stageRoots := map[string]*os.Root{}
	defer func() {
		for _, r := range stageRoots {
			r.Close()
		}
		for _, s := range stages {
			os.RemoveAll(s)
		}
	}()
	for prefix, dest := range roots {
		if mkErr := os.MkdirAll(dest, 0o755); mkErr != nil {
			return nil, fmt.Errorf("archive: create %s: %w", dest, mkErr)
		}
		stage, serr := os.MkdirTemp(dest, stagingPrefix+stamp+"-")
		if serr != nil {
			return nil, fmt.Errorf("archive: staging under %s: %w", dest, serr)
		}
		stages[prefix] = stage
		root, rerr := os.OpenRoot(stage)
		if rerr != nil {
			return nil, fmt.Errorf("archive: opening the staging root %s: %w", stage, rerr)
		}
		stageRoots[prefix] = root
		rep.Restored[prefix] = dest
	}

	want := map[string]Member{}
	if man != nil {
		for _, m := range man.Members {
			want[m.Path] = m
		}
	}

	var dirs []Member
	seen := map[string]bool{}
	// links records every symlink member this archive has declared, keyed by
	// its safe path within its own root. Nothing may be written THROUGH one.
	//
	// Judging each link on its own is not enough, and checkLinkTarget's
	// path.Join is the reason: it collapses ".." lexically, so a link at "d1/s"
	// pointing at "../d0/s/.." joins to "d0" and reads as an ordinary sibling.
	// The kernel collapses nothing -- it walks d1, then "..", then d0, then s,
	// which is itself a link to ".." -- so each hop climbs one real level and a
	// chain climbs as far as it likes. A following member whose own path holds
	// no ".." then satisfies safeMemberPath and is written through the chain,
	// outside the stage, before its digest is compared and where the deferred
	// cleanup cannot reach it. No legitimate archive needs to write through its
	// own symlink: whatever the link points at is archived as a member in its
	// own right.
	links := map[string]map[string]bool{}
	err = streamMembers(ctx, o.Source, func(hdr *tar.Header, r io.Reader) error {
		name := strings.TrimSuffix(hdr.Name, "/")
		if strings.HasPrefix(name, MetaPrefix) {
			_, cerr := io.Copy(io.Discard, r)
			return cerr
		}
		prefix, rel, ok := splitPrefix(roots, name)
		if !ok {
			return fmt.Errorf("%w: member %q belongs to no root of this archive", ErrUnsafeMember, name)
		}
		safe, serr := safeMemberPath(rel)
		if serr != nil {
			return fmt.Errorf("%w: %q: %v", ErrUnsafeMember, name, serr)
		}
		seen[name] = true

		m, known := want[name]
		if !known {
			// A legacy tarball has no table; a gvm one that is missing an
			// entry is corrupt, and extracting an unlisted member would be
			// extracting something nothing vouches for.
			if man != nil {
				return fmt.Errorf("%w: %s is in the tarball but not in the digest table",
					ErrCorrupt, name)
			}
			m = memberFromHeader(hdr, name)
		}

		if lerr := refuseThroughLink(links[prefix], safe); lerr != nil {
			return fmt.Errorf("%w: %q: %v", ErrUnsafeMember, name, lerr)
		}
		root := stageRoots[prefix]
		target := filepath.Join(stages[prefix], filepath.FromSlash(safe))
		switch m.Kind {
		case KindDir:
			if mkErr := root.MkdirAll(safe, 0o700); mkErr != nil {
				return mkErr
			}
			// Directory modes and mtimes are applied after everything inside
			// them has been written; setting them now would have every child
			// write bump the mtime straight back.
			dirs = append(dirs, m)
			return nil
		case KindSymlink:
			if lerr := checkLinkTarget(safe, m.Link); lerr != nil {
				return lerr
			}
			if mkErr := root.MkdirAll(path.Dir(safe), 0o700); mkErr != nil {
				return mkErr
			}
			if links[prefix] == nil {
				links[prefix] = map[string]bool{}
			}
			links[prefix][safe] = true
			if lerr := root.Symlink(filepath.FromSlash(m.Link), safe); lerr != nil {
				// Creating a symlink needs a privilege the development
				// machine usually withholds. Warning rather than failing
				// keeps `restore --to` usable there; on the deploy target it
				// never fires.
				rep.warn(o, "could not recreate symlink %s -> %s: %v", name, m.Link, lerr)
			}
			return nil
		}

		if mkErr := root.MkdirAll(path.Dir(safe), 0o700); mkErr != nil {
			return mkErr
		}
		sum, n, werr := writeVerified(ctx, r, root, safe, target, m.Perm())
		if werr != nil {
			return werr
		}
		rep.Members++
		rep.Bytes += n
		if known {
			if n != m.Size {
				return fmt.Errorf("%w: %s extracted %d bytes, the manifest records %d",
					ErrCorrupt, name, n, m.Size)
			}
			if sum != m.SHA256 {
				return fmt.Errorf("%w: %s hashes %s, the manifest records %s",
					ErrCorrupt, name, sum, m.SHA256)
			}
		}
		if !m.MTime.IsZero() {
			_ = root.Chtimes(safe, m.MTime, m.MTime)
		}
		if o.PreserveOwnership {
			if cerr := chownWith(o, target, m.UID, m.GID); cerr != nil {
				rep.warn(o, "could not set ownership on %s: %v", name, cerr)
			}
		}
		return nil
	})
	if err != nil {
		// Nothing has been renamed. The deferred cleanup removes the staged
		// tree and the live world is exactly as it was, which is the entire
		// point of staging first.
		return nil, err
	}

	if man != nil {
		var missing []string
		for _, m := range man.Members {
			if !seen[m.Path] {
				missing = append(missing, m.Path)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, fmt.Errorf("%w: %d member(s) listed in the digest table are missing from "+
				"%s, starting with %s", ErrCorrupt, len(missing), o.Source, missing[0])
		}
	}

	// Deepest first, so a parent's mtime is not bumped by a child being
	// given its own a moment later.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].Path) > len(dirs[j].Path) })
	for _, d := range dirs {
		prefix, rel, ok := splitPrefix(roots, d.Path)
		if !ok {
			continue
		}
		safe, serr := safeMemberPath(rel)
		if serr != nil {
			continue
		}
		root := stageRoots[prefix]
		target := filepath.Join(stages[prefix], filepath.FromSlash(safe))
		if p := d.Perm(); p != 0 {
			_ = root.Chmod(safe, p)
		}
		if !d.MTime.IsZero() {
			_ = root.Chtimes(safe, d.MTime, d.MTime)
		}
		if o.PreserveOwnership {
			if cerr := chownWith(o, target, d.UID, d.GID); cerr != nil {
				rep.warn(o, "could not set ownership on %s: %v", d.Path, cerr)
			}
		}
	}

	// Everything verified. Only now does anything move.
	for prefix, dest := range roots {
		replaced, rerr := promote(stages[prefix], dest, stamp)
		if rerr != nil {
			return nil, rerr
		}
		if replaced != "" {
			rep.Replaced = append(rep.Replaced, replaced)
		}
	}
	sort.Strings(rep.Replaced)
	if len(rep.Replaced) > 0 {
		rep.warn(o, "the previous contents were moved aside into %s and were NOT deleted: "+
			"removing a world is Destroy's job and needs a verified archive receipt",
			strings.Join(rep.Replaced, ", "))
	}
	return rep, nil
}

// checkDrift is the game-manifest comparison of section 13.2.
func checkDrift(man *Manifest, o RestoreOptions) error {
	if o.ExpectGameManifestSHA256 == "" || man.Instance.GameManifestSHA256 == "" {
		return nil
	}
	if o.ExpectGameManifestSHA256 == man.Instance.GameManifestSHA256 {
		return nil
	}
	if o.AcceptManifestDrift {
		return nil
	}
	return fmt.Errorf("%w: the archive was taken against game manifest %s revision %d, and this "+
		"instance is pinned to %s. argv, the shutdown ladder and the destroy globs may all have "+
		"changed; re-run with --accept-manifest-drift once you have read the diff",
		ErrManifestDrift, short(man.Instance.GameManifestSHA256), man.Instance.GameManifestRevision,
		short(o.ExpectGameManifestSHA256))
}

// destinations resolves each root prefix to the directory it restores into.
func destinations(man *Manifest, o RestoreOptions) (map[string]string, error) {
	out := map[string]string{}

	if man == nil {
		// A legacy tarball has no roots. Its members are stored at the top
		// level, so exactly one destination is needed and the caller must
		// name it: guessing $DATA_DIR from a filename would be the adoption
		// failure this project exists to avoid.
		dest := o.To
		if dest == "" {
			dest = o.Dest[""]
		}
		if dest == "" {
			return nil, errors.New("archive: this archive records no roots, so restore must be " +
				"told where to put it (--to <dir>, or an explicit destination)")
		}
		abs, err := filepath.Abs(dest)
		if err != nil {
			return nil, err
		}
		out[""] = abs
		return out, nil
	}

	taken := map[string]string{}
	for _, r := range man.Roots {
		var dest string
		switch {
		case o.To != "":
			dest = filepath.Join(o.To, filepath.FromSlash(strings.TrimSuffix(r.Prefix, "/")))
		case o.Dest[r.Prefix] != "":
			dest = o.Dest[r.Prefix]
		default:
			dest = filepath.FromSlash(r.Base)
		}
		abs, err := filepath.Abs(dest)
		if err != nil {
			return nil, err
		}
		// Two roots landing in one directory would each promote their staged
		// tree into it, and the second would move the first's freshly restored
		// entries into a holding directory -- a restore that silently half
		// worked. Refusing costs nothing and the caller chose these paths.
		if other, clash := taken[filepath.Clean(abs)]; clash {
			return nil, fmt.Errorf("archive: roots %q and %q both restore into %s; they would "+
				"overwrite each other's members", other, r.Prefix, abs)
		}
		taken[filepath.Clean(abs)] = r.Prefix
		out[r.Prefix] = abs
	}
	if len(out) == 0 {
		return nil, errors.New("archive: the manifest records no roots")
	}
	return out, nil
}

// splitPrefix finds which root a member belongs to, longest prefix first.
func splitPrefix(roots map[string]string, member string) (prefix, rel string, ok bool) {
	best := ""
	found := false
	for p := range roots {
		if p == "" {
			if !found {
				best, found = p, true
			}
			continue
		}
		if strings.HasPrefix(member, p) && (!found || len(p) > len(best)) {
			best, found = p, true
		}
	}
	if !found {
		return "", "", false
	}
	return best, strings.TrimPrefix(member, best), true
}

// safeMemberPath refuses a member name that would extract outside its root.
//
// The archive is data from outside gvm -- it may have been produced by another
// box, edited, or handed over by somebody else -- so this is zip-slip's front
// door and it is closed lexically, before any filesystem call. A backslash is
// refused outright: it is a legal character in a Linux filename and a path
// separator on the development machine, so a member called "a\b" would extract
// as one file on the host and two on Windows, and an archive that restores
// differently depending on where it is read is not a backup.
func safeMemberPath(rel string) (string, error) {
	if rel == "" || rel == "." {
		return "", errors.New("empty member path")
	}
	if strings.ContainsRune(rel, '\\') {
		return "", errors.New("contains a backslash, which is a separator on some platforms and " +
			"a filename character on others")
	}
	if strings.ContainsRune(rel, 0) {
		return "", errors.New("contains a NUL byte")
	}
	if path.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", errors.New("is absolute")
	}
	if vol := filepath.VolumeName(rel); vol != "" {
		return "", fmt.Errorf("names volume %q", vol)
	}
	for _, el := range strings.Split(rel, "/") {
		if el == ".." {
			return "", errors.New("contains a \"..\" element")
		}
	}
	clean := path.Clean(rel)
	if clean == "." || strings.HasPrefix(clean, "../") {
		return "", errors.New("does not stay inside its root")
	}
	return clean, nil
}

// checkLinkTarget refuses a symlink that points out of the restored tree.
//
// Zip-slip's second act: a member that is a symlink to /etc, followed by a
// member written "through" it. Refusing the link is simpler and safer than
// trying to detect the second half.
func checkLinkTarget(member, target string) error {
	if target == "" {
		return fmt.Errorf("%w: %s is a symlink with an empty target", ErrUnsafeMember, member)
	}
	if path.IsAbs(target) || strings.HasPrefix(target, "/") || filepath.VolumeName(target) != "" {
		return fmt.Errorf("%w: %s points at the absolute path %s", ErrUnsafeMember, member, target)
	}
	joined := path.Join(path.Dir(member), target)
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return fmt.Errorf("%w: %s points at %s, which resolves outside the archive root",
			ErrUnsafeMember, member, target)
	}
	return nil
}

// writeVerified streams one member to disk, hashing as it goes.
//
// It opens through the staging *os.Root rather than by absolute path, so a
// member cannot be written through a symlink that leaves the stage however that
// symlink came to exist. target is carried only for error messages, which have
// to name the path an operator would go and look at.
func writeVerified(ctx context.Context, r io.Reader, root *os.Root, name, target string, perm fs.FileMode) (string, int64, error) {
	if perm == 0 {
		perm = 0o600
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return "", 0, fmt.Errorf("archive: create %s: %w", target, err)
	}
	defer f.Close()

	h := sha256.New()
	counted := &countingWriter{w: io.MultiWriter(h, f)}
	if err := copyCtx(ctx, counted, r); err != nil {
		return "", 0, err
	}
	if err := f.Sync(); err != nil {
		return "", 0, fmt.Errorf("archive: fsync %s: %w", target, err)
	}
	// The mode is set explicitly rather than left to the open: umask would
	// otherwise silently narrow a 0644 config file into 0640 and a game that
	// reads its own config as a non-root user would break on restore only.
	if err := root.Chmod(name, perm); err != nil {
		return "", 0, fmt.Errorf("archive: chmod %s: %w", target, err)
	}
	return hex.EncodeToString(h.Sum(nil)), counted.n, nil
}

// refuseThroughLink rejects a member whose path runs through a symlink this
// same archive declared.
//
// It is the platform-independent half of the containment story: os.Root refuses
// the traversal on the filesystem, but only where symlinks can actually be
// created, and the development machine withholds that privilege. This rule is
// decided on the member names alone, so the refusal is identical everywhere and
// is asserted by a test that runs on Windows.
func refuseThroughLink(links map[string]bool, safe string) error {
	if len(links) == 0 {
		return nil
	}
	for dir := path.Dir(safe); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
		if links[dir] {
			return fmt.Errorf("is written through %q, which this archive declares as a symlink; "+
				"whatever a link points at is archived as a member in its own right, so a member "+
				"written through one is an attempt to land outside the tree", dir)
		}
	}
	return nil
}

// promote moves a verified staged tree into place, one top-level entry at a
// time, and reports where any previous contents were put.
//
// It renames rather than copies, so the window in which the destination is
// half-old and half-new is a sequence of rename(2) calls rather than the
// length of a 2.4 GB copy.
func promote(stage, dest, stamp string) (replaced string, err error) {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		from := filepath.Join(stage, e.Name())
		to := filepath.Join(dest, e.Name())
		if _, serr := os.Lstat(to); serr == nil {
			if replaced == "" {
				replaced = filepath.Join(dest, replacedPrefix+stamp)
				if mkErr := os.MkdirAll(replaced, 0o700); mkErr != nil {
					return "", mkErr
				}
			}
			if mvErr := os.Rename(to, filepath.Join(replaced, e.Name())); mvErr != nil {
				return "", fmt.Errorf("archive: move %s aside: %w", to, mvErr)
			}
		} else if !errors.Is(serr, fs.ErrNotExist) {
			return "", serr
		}
		if mvErr := os.Rename(from, to); mvErr != nil {
			return "", fmt.Errorf("archive: promote %s -> %s: %w", from, to, mvErr)
		}
	}
	syncDir(dest)
	return replaced, nil
}

// memberFromHeader is the legacy path's substitute for a digest table entry.
func memberFromHeader(hdr *tar.Header, name string) Member {
	m := Member{
		Path:  name,
		Mode:  octalMode(uint32(hdr.FileInfo().Mode().Perm())),
		UID:   hdr.Uid,
		GID:   hdr.Gid,
		MTime: hdr.ModTime.UTC().Truncate(time.Second),
		Size:  hdr.Size,
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		m.Kind = KindDir
	case tar.TypeSymlink:
		m.Kind = KindSymlink
		m.Link = filepath.ToSlash(hdr.Linkname)
	default:
		m.Kind = KindFile
	}
	return m
}

func chownWith(o RestoreOptions, path string, uid, gid int) error {
	if o.Chown != nil {
		return o.Chown(path, uid, gid)
	}
	return chownPath(path, uid, gid)
}

func short(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12] + "…"
}
