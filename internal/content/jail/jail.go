// Package jail bounds where materialised content may be written.
//
// It is deliberately THIN. internal/instance already owns a jail with exactly
// this shape -- unexported fields so a proven path cannot be forged, ".."
// rejected before Clean can absorb it, containment decided on the
// symlink-RESOLVED path, a cross-device target refused -- and two jails that
// disagree about what is safe is strictly worse than one. So this package
// re-derives none of that: Path IS instance.Jailed, and every containment,
// traversal, symlink and device decision is made by instance.Jail.Resolve.
//
// What a WRITE jail needs that a destroy jail does not is three things, and
// they are all this package contains:
//
//  1. RELATIVE-ONLY targets. A destroy set is rendered from a manifest and may
//     legitimately be spelled absolutely. A content target is a member name out
//     of a third-party archive or API response, where "/etc/cron.d/root" and
//     "C:mods" are not paths to be contained, they are attempts to be refused.
//     There is one door -- Target -- and it takes a path relative to a root.
//
//  2. PRESERVE, the section 12.8 list: world/, server.properties, ops.json,
//     whitelist.json, banned-*.json, usercache.json. These differ from
//     Protected in exactly one way -- `--force` may promote them -- and that
//     promotion goes through instance.Jail.Rebuild, which constructs a new jail
//     rather than mutating one and refuses to promote anything this jail does
//     not already hold back.
//
//  3. OWNERSHIP OF THE WRITE. A Path is proof that a path was checked, not proof
//     that this jail checked it: instance.Jailed values exist in this program
//     that belong to other jails -- the blob store's, the install tree's -- so
//     the write helpers live here and re-ask.
//
// What is deliberately NOT here: a way to stream arbitrary bytes into the jail.
// Mod bytes reach disk through the blob store, which hands out a path only on a
// digest match, and an io.Writer here would be the shortest route around that.
package jail

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Path is a content destination that has been PROVEN to be inside this
// instance's content roots.
//
// It is an ALIAS of instance.Jailed, not a second type wrapping it. That is the
// point of this package: there is one proven-path type in the program, so the
// blob store's Link, the provider's Dest and this package's writes all speak
// it, and no conversion exists at any seam where two definitions of "safe"
// could drift apart. Its fields are unexported in internal/instance, so the
// only way to obtain a populated one is still to resolve a path through a jail.
type Path = instance.Jailed

// The refusals. The containment family is re-exported rather than redeclared,
// so a caller matching on jail.ErrOutside and a caller matching on
// instance.ErrOutside are matching the same sentinel.
var (
	// ErrOutside is the containment refusal: the target is not under any
	// content root.
	ErrOutside = instance.ErrOutside
	// ErrTraversal is the lexical refusal: the target contains a ".."
	// element. This is zip-slip's first act.
	ErrTraversal = instance.ErrTraversal
	// ErrProtected is the world-safety refusal. A content jail protects the
	// world and the secrets directory, and nothing here promotes them:
	// `--force` reaches the preserve list and stops.
	ErrProtected = instance.ErrProtected
	// ErrSymlink is the strict-mode refusal: the target reaches its place
	// through a symlink. This is zip-slip's second act, and it is why a
	// content jail is always strict.
	ErrSymlink = instance.ErrSymlink
	// ErrCrossDevice is the mount refusal: the target resolves onto a
	// different filesystem than its root.
	ErrCrossDevice = instance.ErrCrossDevice
	// ErrJail is a malformed jail: bad configuration, not a bad path.
	ErrJail = instance.ErrJail

	// ErrAbsolute is a content target that is not relative. An archive member
	// or an API-supplied filename is always relative to a declared root; one
	// that is not is not a path that needs containing, it is an attempt.
	ErrAbsolute = errors.New("jail: content target is not relative")
	// ErrBadTarget is a target string that cannot be checked meaningfully at
	// all: empty, or carrying a NUL.
	ErrBadTarget = errors.New("jail: unusable content target")
	// ErrPreserved is the section 12.8 refusal: the target is on the preserve
	// list. The operator's next move is `--force`, which names paths.
	ErrPreserved = errors.New("jail: target is a preserved path")
	// ErrNotOwned is a Path that did not come from THIS jail -- a blob store
	// path, an install-tree path, or the zero value.
	ErrNotOwned = errors.New("jail: path was not proven by this jail")
)

// Directory and file modes for content gvm writes itself. Mod bytes get
// provider.BlobMode through the blob store's Link; these are for the small
// files a source writes beside them (blocked-mods.json, a rendered config).
const (
	DirMode  fs.FileMode = 0o755
	FileMode fs.FileMode = 0o644
)

// Config describes the three lists a content jail is built from.
type Config struct {
	// Allow are the absolute content roots materialisation may write into,
	// already rendered: the mods directory, the workshop root, the drop-box.
	// Allow[0] is the primary and is what a relative target resolves against.
	Allow []string

	// Protected are paths that must survive and that nothing here promotes:
	// the rendered backup.world_paths and the secrets directory. A content
	// operation has no `--force` for a world -- that is `gvm rebuild`, which
	// takes an archive first.
	Protected []string

	// Preserve is section 12.8's list: paths never overwritten without
	// `--force`. They are refused like a protected path, and Force names them
	// one by one to lift the refusal.
	Preserve []string
}

// Jail turns a relative content path into a Path, or refuses.
//
// A Jail is immutable. Force -- the only operation that widens one -- returns a
// new Jail, so a widened jail cannot leak back to a caller that was handed the
// narrow one.
type Jail struct {
	// inner holds BOTH lists in its protected set, because the refusal is the
	// same refusal; only whether Force may lift it differs.
	inner *instance.Jail
	// preserveJail holds the preserve list ALONE, and exists so that
	// classifying a refusal is a question put to instance.Jail's own
	// containment rather than a substring search of an error message. A
	// message-sniffing classifier would mislabel the day somebody rewords a
	// refusal, and the two labels send the operator to two different commands.
	preserveJail *instance.Jail

	roots     []string // resolved content roots, primary first
	protected []string // resolved, never promotable
	preserve  []string // resolved, promotable by Force
	forced    []string // resolved, promoted by Force; for the report
}

// New validates the configuration and resolves every root.
//
// The content roots are resolved BEFORE the strict jail is built, and the
// resolved spellings are what the strict jail is rooted at. Strict mode compares
// a path's lexical and resolved forms, so a jail rooted at a configured path
// whose parent is a symlink -- /srv on a box where /srv is a link, or a
// developer's temp directory -- would refuse every single target for a reason
// that has nothing to do with the target. internal/provider does the same thing
// for the install tree's dest jail and for the same reason.
func New(cfg Config) (*Jail, error) {
	if len(cfg.Allow) == 0 {
		return nil, fmt.Errorf("%w: no content root; a jail that allows nothing would make "+
			"every materialise read as \"nothing to write\" rather than as a misconfiguration",
			ErrJail)
	}

	// The probe jail answers two questions with internal/instance's own code:
	// what does each configured path resolve to, and is each preserved path
	// actually inside a content root. It carries no protected set, because a
	// jail that protected the world here could not resolve the world path it
	// was asked about.
	probe, err := instance.NewJail(instance.JailConfig{Allow: cfg.Allow})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrJail, err)
	}

	roots := make([]string, 0, len(cfg.Allow))
	for _, a := range cfg.Allow {
		p, err := probe.Resolve(a)
		if err != nil {
			return nil, fmt.Errorf("%w: content root %s: %v", ErrJail, a, err)
		}
		if !contains(roots, p.Path()) {
			roots = append(roots, p.Path())
		}
	}

	protected, err := cleanList("protected path", cfg.Protected)
	if err != nil {
		return nil, err
	}
	preserve, err := cleanList("preserved path", cfg.Preserve)
	if err != nil {
		return nil, err
	}

	// A path on both lists is a configuration whose two readings differ on
	// whether `--force` may write it. The dangerous reading must not win by
	// being the one the loop happened to see last, so neither does.
	for _, p := range preserve {
		if contains(protected, p) {
			return nil, fmt.Errorf("%w: %s is listed as both protected and preserved, so it is "+
				"ambiguous whether --force may write it; protected means no", ErrJail, p)
		}
	}
	return build(roots, protected, preserve, nil)
}

// build assembles the two inner jails from three resolved lists. It is the one
// place either is constructed, so a Jail produced by New and a Jail produced by
// Force are the same kind of object.
func build(roots, protected, preserve, forced []string) (*Jail, error) {
	// Strict. Every path this jail judges came from a third-party archive, a
	// mod API or a collection page, and zip-slip's second act is a symlink
	// member: a tree containing "mods -> /etc" writes into /etc through a
	// member name with no ".." in it at all.
	inner, err := instance.NewJail(instance.JailConfig{
		Allow:     roots,
		Protected: append(append([]string(nil), protected...), preserve...),
		Strict:    true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrJail, err)
	}
	// Not strict: this jail is only ever asked to CLASSIFY a refusal the
	// strict one already made, and a second strict refusal on the way to
	// classifying the first would answer the wrong question.
	preserveJail, err := instance.NewJail(instance.JailConfig{
		Allow:     roots,
		Protected: preserve,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrJail, err)
	}
	return &Jail{
		inner:        inner,
		preserveJail: preserveJail,
		roots:        roots,
		protected:    protected,
		preserve:     preserve,
		forced:       forced,
	}, nil
}

// cleanList normalises one configured list of paths that must survive.
//
// It keeps the CONFIGURED spelling, cleaned, rather than the symlink-resolved
// one, for two reasons. Resolution is instance.NewJail's job and doing it twice
// is how two answers appear. And Force compares what the CLI passes -- which is
// this list, straight back out of Preserved() -- so comparing configured
// spellings makes that match exact instead of dependent on a second resolve
// agreeing with the first.
//
// A path outside every content root is NOT refused. The world lives in the data
// tree and the content roots are the mods directories, so the real
// configuration has every protected path outside every root; refusing it would
// force the caller to pre-filter backup.world_paths, and that filter is exactly
// where a world path that IS inside a content root would get dropped.
func cleanList(what string, in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("%w: an empty %s", ErrJail, what)
		}
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("%w: %s %q is not absolute; a path that must survive, "+
				"resolved against a working directory, would move when the caller does",
				ErrJail, what, p)
		}
		if clean := filepath.Clean(p); !contains(out, clean) {
			out = append(out, clean)
		}
	}
	return out, nil
}

// Roots are the resolved content roots, primary first.
func (j *Jail) Roots() []string {
	if j == nil {
		return nil
	}
	return clone(j.roots)
}

// Primary is the root a relative target resolves against.
func (j *Jail) Primary() string {
	if j == nil || len(j.roots) == 0 {
		return ""
	}
	return j.roots[0]
}

// Protected are the paths nothing here promotes.
func (j *Jail) Protected() []string {
	if j == nil {
		return nil
	}
	return clone(j.protected)
}

// Preserved are the paths `--force` may promote, in their current state: a path
// already promoted by Force is no longer on this list. It is also what the CLI
// hands back to Force for a blanket `--force`, which is why this and Force
// compare the same spellings.
func (j *Jail) Preserved() []string {
	if j == nil {
		return nil
	}
	return clone(j.preserve)
}

// Forced are the paths this jail had promoted into it. Non-empty means somebody
// passed `--force`, which is a sentence the plan output owes the operator.
func (j *Jail) Forced() []string {
	if j == nil {
		return nil
	}
	return clone(j.forced)
}

// Strict reports whether this jail rejects symlinked targets outright. It is
// always true, and it is exposed so a doctor check can assert that rather than
// trust this comment.
func (j *Jail) Strict() bool { return j != nil && j.inner.Strict() }

func clone(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return append([]string(nil), s...)
}

func contains(s []string, v string) bool {
	for _, have := range s {
		if have == v {
			return true
		}
	}
	return false
}

// Force returns a jail identical to j except that the named preserved paths
// become writable.
//
// It names paths; it is not a mode. `gvm content sync --force` passes the whole
// preserve list and gets the blanket behaviour, but the widening still happens
// one path at a time against a list this jail already holds, so a caller cannot
// turn `--force` into "write anywhere" by naming something else. A protected
// path is refused here, which is what keeps `--force` away from a world:
// destroying a world is `gvm rebuild`, and that takes an archive first.
//
// The widening itself is instance.Jail.Rebuild, so the new jail is built by the
// same code that built the old one and the two cannot disagree.
func (j *Jail) Force(paths []string) (*Jail, error) {
	if j == nil || j.inner == nil {
		return nil, fmt.Errorf("%w: no jail to force", ErrJail)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%w: --force names no paths; a force that promotes nothing would "+
			"report success and then refuse the same write", ErrJail)
	}

	named, err := cleanList("--force path", paths)
	if err != nil {
		return nil, err
	}

	promote := make([]string, 0, len(named))
	for _, p := range named {
		switch {
		case contains(j.protected, p):
			return nil, fmt.Errorf("%w: %s is protected, not preserved; --force does not reach "+
				"a world path, because destroying one is `gvm rebuild` and that takes an "+
				"archive first", ErrProtected, p)
		case contains(j.forced, p):
			continue // already promoted; promoting twice is not an error
		case !contains(j.preserve, p):
			return nil, fmt.Errorf("%w: %s is not a preserved path of this jail, so --force "+
				"cannot promote it; the force set is the preserve list (%s) and nothing else",
				ErrJail, p, strings.Join(j.preserve, ", "))
		}
		promote = append(promote, p)
	}
	if len(promote) == 0 {
		return j, nil
	}

	// Rebuild is asked to do the widening even though build() below could do
	// it directly, because Rebuild is the function internal/instance documents
	// as the only way to widen a jail and it enforces that every promoted path
	// was genuinely held back. Calling it keeps that enforcement on the path
	// rather than reimplementing its precondition here.
	if _, err := j.inner.Rebuild(promote); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrJail, err)
	}

	var preserve []string
	for _, p := range j.preserve {
		if !contains(promote, p) {
			preserve = append(preserve, p)
		}
	}
	return build(clone(j.roots), clone(j.protected), preserve, append(clone(j.forced), promote...))
}

// ForceAll promotes the whole preserve list. It is `--force` with no argument,
// written once here so the CLI cannot spell it as "skip the checks".
func (j *Jail) ForceAll() (*Jail, error) {
	if j == nil {
		return nil, fmt.Errorf("%w: no jail to force", ErrJail)
	}
	if len(j.preserve) == 0 {
		return j, nil
	}
	return j.Force(j.preserve)
}

// Target resolves one content destination, relative to the primary root.
//
// The path must be relative. Everything else -- "..", a symlinked parent, a
// protected or preserved ancestor, a target on another filesystem -- is
// instance.Jail.Resolve's answer, unchanged and with its own sentinel, so the
// refusal an operator reads is the same refusal the destroy path would have
// given for the same path.
func (j *Jail) Target(rel string) (Path, error) {
	if j == nil || j.inner == nil {
		return Path{}, fmt.Errorf("%w: nil jail", ErrJail)
	}
	if err := checkRelative(rel); err != nil {
		return Path{}, err
	}
	return j.resolve(rel, rel)
}

// TargetIn resolves one content destination against a named root rather than
// the primary one.
//
// A manifest declares several content roots -- two workshop locations, a mods
// directory and a drop-box -- and a source that had to concatenate strings to
// aim at the second one would be building paths outside the type that proves
// them. root must be one of this jail's roots.
func (j *Jail) TargetIn(root, rel string) (Path, error) {
	if j == nil || j.inner == nil {
		return Path{}, fmt.Errorf("%w: nil jail", ErrJail)
	}
	if err := checkRelative(rel); err != nil {
		return Path{}, err
	}
	probe, err := instance.NewJail(instance.JailConfig{Allow: j.roots})
	if err != nil {
		return Path{}, fmt.Errorf("%w: %v", ErrJail, err)
	}
	rp, err := probe.Resolve(root)
	if err != nil || !contains(j.roots, rp.Path()) {
		return Path{}, fmt.Errorf("%w: %s is not a content root of this jail (roots: %s)",
			ErrOutside, root, strings.Join(j.roots, ", "))
	}
	// Joined lexically and then resolved, not resolved-and-trusted: the jail
	// still decides, so a rel that climbs out of this root is answered by
	// containment rather than by arithmetic.
	return j.resolve(filepath.Join(rp.Path(), rel), rel)
}

// Targets resolves every path or returns none of them.
//
// Every failure is reported rather than the first. The caller is about to show
// an operator an extraction plan, and a plan that reveals its problems one run
// at a time turns a review into a guessing game -- while a partial set would be
// worse than either, because half an extracted modpack is a modpack nobody
// approved.
func (j *Jail) Targets(rels []string) ([]Path, error) {
	out := make([]Path, 0, len(rels))
	var errs []error
	for _, rel := range rels {
		p, err := j.Target(rel)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, p)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// resolve asks the strict jail, then labels a protected refusal that is really
// a preserve refusal.
//
// Both lists live in the strict jail's protected set because the refusal is the
// same; the operator's next move is not, so neither are the message and the
// sentinel. ErrPreserved says `--force`; ErrProtected says `gvm rebuild`.
func (j *Jail) resolve(arg, display string) (Path, error) {
	p, err := j.inner.Resolve(arg)
	if err == nil {
		return p, nil
	}
	if errors.Is(err, instance.ErrProtected) && len(j.preserve) > 0 {
		if _, perr := j.preserveJail.Resolve(arg); errors.Is(perr, instance.ErrProtected) {
			return Path{}, fmt.Errorf("%w: content target %q: %v; pass --force to take "+
				"responsibility for it", ErrPreserved, display, perr)
		}
	}
	return Path{}, fmt.Errorf("content target %q: %w", display, err)
}

// checkRelative is the one check this package makes for itself, because a
// destroy jail has no reason to make it.
//
// The absolute test is spelled for both platforms rather than left to
// filepath.IsAbs, which answers false for "/etc/passwd" on Windows: a tar
// member must be refused identically on the machine where this is tested and on
// the machine where it runs, or the test proves nothing about production. A
// volume name catches Windows' drive-relative "C:mods", which IsAbs also calls
// relative and which names a different directory per process.
func checkRelative(rel string) error {
	if strings.TrimSpace(rel) == "" {
		return fmt.Errorf("%w: empty content target", ErrBadTarget)
	}
	if strings.ContainsRune(rel, 0) {
		// A NUL truncates the path at the syscall boundary, so the path that
		// gets written would be a prefix of the path that got checked. That is
		// the one thing this type promises cannot happen.
		return fmt.Errorf("%w: %q contains a NUL byte, which would truncate the path the "+
			"kernel sees to a prefix of the path that was checked", ErrBadTarget, rel)
	}
	// filepath.VolumeName is a no-op off Windows, but a content target is the
	// same hostile string on every platform (an archive is unpacked wherever
	// gvm runs), so the drive-letter prefix is spelled out by hand.
	if filepath.VolumeName(rel) != "" ||
		(len(rel) >= 2 && rel[1] == ':' &&
			('a' <= rel[0] && rel[0] <= 'z' || 'A' <= rel[0] && rel[0] <= 'Z')) {
		return fmt.Errorf("%w: %q names a volume", ErrAbsolute, rel)
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return fmt.Errorf("%w: %q; a content target is a member name relative to a declared "+
			"root, and an absolute one is not a path to be contained", ErrAbsolute, rel)
	}
	// ".." is checked HERE and not left to instance.Jail.Resolve, even though
	// Resolve checks it too, because TargetIn joins rel onto a root before
	// handing it over and filepath.Join cleans: "../install/world" joined onto
	// downloads/ arrives as install/world with no ".." left in it, inside a
	// DIFFERENT content root. The element has to be refused before any join,
	// which means it has to be refused in this package.
	for _, el := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' }) {
		if el == ".." {
			return fmt.Errorf("%w: %q", ErrTraversal, rel)
		}
	}
	return nil
}

// Owns reports whether p was proven by THIS jail, and re-proves it.
//
// instance.Jailed values exist in this program that belong to other jails: the
// blob store hands one out for every verified blob, and the provider hands one
// out for every install destination. A write helper that accepted any of them
// would be a jail that bounds nothing. The zero value -- the one forgery a
// composite literal in another package can write -- carries no path and is
// refused by name, so the message says what went wrong rather than reporting a
// failure to create "".
//
// The re-resolve is cheap and it is not redundant: it is what makes "this Path
// is inside this jail" a question the filesystem answers now, rather than a
// claim inherited from whichever jail produced the value.
func (j *Jail) Owns(p Path) error {
	if j == nil || j.inner == nil {
		return fmt.Errorf("%w: nil jail", ErrJail)
	}
	if !p.Valid() {
		return fmt.Errorf("%w: the destination did not come from a jail at all (zero value)",
			ErrNotOwned)
	}
	got, err := j.inner.Resolve(p.Path())
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrNotOwned, p.Path(), err)
	}
	if got.Path() != p.Path() {
		return fmt.Errorf("%w: %s now resolves to %s", ErrNotOwned, p.Path(), got.Path())
	}
	return nil
}

// MkdirAll creates p as a directory, and its parents.
func (j *Jail) MkdirAll(p Path) error {
	if err := j.Owns(p); err != nil {
		return err
	}
	if err := os.MkdirAll(p.Path(), DirMode); err != nil {
		return fmt.Errorf("jail: create %s: %w", p.Path(), err)
	}
	return nil
}

// MkdirFor creates p's parent directory, which is what a file write needs.
func (j *Jail) MkdirFor(p Path) error {
	if err := j.Owns(p); err != nil {
		return err
	}
	dir := filepath.Dir(p.Path())
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("jail: create %s: %w", dir, err)
	}
	return nil
}

// WriteFile writes one small file gvm generated itself: blocked-mods.json, a
// rendered mod config. It is NOT how mod bytes reach disk -- those come out of
// the blob store, which hands out a path only on a digest match, and a writer
// here would be the shortest route around that.
//
// Atomic, and with no .bak. internal/spec's backup policy is right for a
// hand-edited control-plane file an operator may need to recover; a .bak beside
// a generated file in a directory the game scans is a second copy of something
// the game may try to load.
func (j *Jail) WriteFile(p Path, data []byte, perm fs.FileMode) error {
	if err := j.MkdirFor(p); err != nil {
		return err
	}
	return spec.WriteFileAtomicNoBackup(p.Path(), data, perm)
}

// String is the one-line description a plan header prints.
func (j *Jail) String() string {
	if j == nil || j.inner == nil {
		return "<no content jail>"
	}
	s := "content jail: " + strings.Join(j.roots, ", ")
	if len(j.protected) > 0 {
		s += fmt.Sprintf("; protected %d", len(j.protected))
	}
	if len(j.preserve) > 0 {
		s += fmt.Sprintf("; preserved %d", len(j.preserve))
	}
	if len(j.forced) > 0 {
		s += "; FORCED " + strings.Join(j.forced, ", ")
	}
	return s
}
