package instance

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// Jailed is a path that has been PROVEN safe to delete.
//
// Its fields are unexported, so no other package can write one: a Jailed can
// only come out of Jail.Resolve, which is the function that does the proving.
// That is the whole point of the type. rebuild.sh got this right by being
// careful, once, in one script; a Go program with a dozen callers cannot rely
// on care, so the guarantee is moved into the compiler. instance.Destroy takes
// []Jailed and therefore does not compile against []string, and there is no
// literal, cast or conversion that produces a populated Jailed from outside
// this package.
//
// The zero value is the one forgery another package can write --
// []Jailed{{}} compiles anywhere -- so it carries no path and every consumer
// rejects it (see Destroy).
type Jailed struct {
	root string // the allow root this path was proven to live under
	abs  string // the fully resolved absolute path
}

// Path is the absolute, symlink-resolved path.
func (j Jailed) Path() string { return j.abs }

// Root is the allow root the path was proven to live under.
func (j Jailed) Root() string { return j.root }

// Rel is the path relative to its allow root, which is what a plan or an audit
// record should print: the root is repeated once at the top, not on every line.
func (j Jailed) Rel() string {
	if j.abs == "" || j.root == "" {
		return ""
	}
	rel, err := filepath.Rel(j.root, j.abs)
	if err != nil {
		return j.abs
	}
	return rel
}

// Valid reports whether this value came from Resolve rather than from a zero
// composite literal.
func (j Jailed) Valid() bool { return j.abs != "" && j.root != "" }

func (j Jailed) String() string {
	if !j.Valid() {
		return "<unjailed>"
	}
	return j.abs
}

// The jail's refusals. They are separate sentinels because the operator's next
// move differs for each: a protected path needs --rebuild, an outside path
// needs a corrected manifest glob, and a cross-device path needs someone to
// look at what is mounted where.
var (
	// ErrProtected is the world-safety refusal: the path is a world path or
	// an ancestor of one.
	ErrProtected = errors.New("instance: path is a protected world path")
	// ErrOutside is the containment refusal: the path is not under any root
	// this jail may touch.
	ErrOutside = errors.New("instance: path is outside every allow root")
	// ErrTraversal is the lexical refusal: the path contains a ".." element.
	ErrTraversal = errors.New("instance: path contains a \"..\" element")
	// ErrCrossDevice is the mount refusal: the path resolves onto a different
	// filesystem than its allow root.
	ErrCrossDevice = errors.New("instance: path resolves onto a different filesystem")
	// ErrSymlink is the strict-mode refusal: the path reaches its target
	// through a symlink.
	ErrSymlink = errors.New("instance: path resolves through a symlink")
	// ErrJail is a malformed jail: bad configuration, not a bad path.
	ErrJail = errors.New("instance: invalid jail")
)

// JailConfig describes what a jail may and may not touch.
type JailConfig struct {
	// Allow are the absolute roots destruction may reach into. Allow[0] is
	// the primary: a relative path handed to Resolve is interpreted against
	// it. At least one is required, and none of them may be a system
	// directory or one level below the filesystem root.
	Allow []string
	// Protected are absolute paths that must survive, rendered from the
	// manifest's backup.world_paths and backup.destroy_protected, plus the
	// secrets directory. A protected path is refused, and so is any ancestor
	// of one.
	Protected []string
	// Strict requires a path's lexical and resolved forms to be identical,
	// which rejects any path reaching its target through a symlink at all --
	// even one that stays inside the jail. Used where the jail is applied to
	// paths that came from outside gvm (an archive's member list, a
	// modpack's file list): zip-slip's second act is a symlink member.
	Strict bool
}

// Jail turns a path into a Jailed, or refuses.
//
// A Jail is immutable once built. Rebuild -- the only operation that widens
// one -- returns a new Jail rather than mutating this one, so a widened jail
// cannot leak back to a caller that was handed the narrow one.
type Jail struct {
	allow     []jailRoot
	protected []jailRoot
	strict    bool
}

// jailRoot keeps both spellings of a root. The resolved form is what prefix
// math must use; the configured form is what an error message must say, because
// an operator who wrote {{ .Data }}/Saves needs to recognise the answer.
type jailRoot struct {
	given string
	real  string
}

// NewJail validates the configuration and resolves every root.
func NewJail(cfg JailConfig) (*Jail, error) {
	if len(cfg.Allow) == 0 {
		return nil, fmt.Errorf("%w: no allow root; a jail that allows nothing is a bug, "+
			"not a safe default, because the caller will read the error as \"nothing to delete\"", ErrJail)
	}
	j := &Jail{strict: cfg.Strict}

	for _, a := range cfg.Allow {
		r, err := newAllowRoot(a)
		if err != nil {
			return nil, fmt.Errorf("allow root: %w", err)
		}
		if !containsRoot(j.allow, r) {
			j.allow = append(j.allow, r)
		}
	}
	for _, p := range cfg.Protected {
		r, err := newProtectedPath(p)
		if err != nil {
			return nil, fmt.Errorf("protected path: %w", err)
		}
		if !containsRoot(j.protected, r) {
			j.protected = append(j.protected, r)
		}
	}

	// A root that is itself protected, or that sits inside something
	// protected, describes a jail in which nothing is destroyable. That is
	// not a safe configuration, it is an unnoticed one: every destroy would
	// report "protected" and the operator would go looking at the wrong list.
	for _, a := range j.allow {
		for _, p := range j.protected {
			if under(a.real, p.real) {
				return nil, fmt.Errorf("%w: allow root %s is at or under protected path %s, "+
					"so nothing under it could ever be destroyed", ErrJail, a.given, p.given)
			}
		}
	}
	return j, nil
}

// newAllowRoot is newRoot for a root destruction may reach into, so it also
// refuses a system directory.
func newAllowRoot(p string) (jailRoot, error) { return newRoot(p, true) }

// newProtectedPath is newRoot for a path that must survive. Protecting a
// system directory is not a mistake worth refusing -- it is belt and braces.
func newProtectedPath(p string) (jailRoot, error) { return newRoot(p, false) }

// newRoot cleans, checks and resolves one configured path.
func newRoot(p string, isAllow bool) (jailRoot, error) {
	if strings.TrimSpace(p) == "" {
		return jailRoot{}, fmt.Errorf("%w: empty path", ErrJail)
	}
	if hasDotDot(p) {
		return jailRoot{}, fmt.Errorf("%w: %q contains a \"..\" element", ErrJail, p)
	}
	if !filepath.IsAbs(p) {
		return jailRoot{}, fmt.Errorf("%w: %q is not absolute; a jail root resolved against a "+
			"working directory would move when the caller does", ErrJail, p)
	}
	clean := filepath.Clean(p)
	if isAllow {
		if err := refuseShallowAllowRoot(clean, clean); err != nil {
			return jailRoot{}, err
		}
	}
	real, _, err := resolveExisting(clean)
	if err != nil {
		return jailRoot{}, fmt.Errorf("%w: %v", ErrJail, err)
	}
	if isAllow {
		if err := refuseShallowAllowRoot(clean, real); err != nil {
			return jailRoot{}, err
		}
	}
	return jailRoot{given: clean, real: real}, nil
}

func containsRoot(rs []jailRoot, r jailRoot) bool {
	for _, have := range rs {
		if have.real == r.real {
			return true
		}
	}
	return false
}

// AllowRoots returns the configured allow roots, primary first.
func (j *Jail) AllowRoots() []string {
	if j == nil {
		return nil
	}
	return givens(j.allow)
}

// ProtectedPaths returns the configured protected paths.
func (j *Jail) ProtectedPaths() []string {
	if j == nil {
		return nil
	}
	return givens(j.protected)
}

// Strict reports whether this jail rejects symlinked paths outright.
func (j *Jail) Strict() bool { return j != nil && j.strict }

func givens(rs []jailRoot) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.given)
	}
	return out
}

// Rebuild returns a jail identical to j except that the named world paths move
// from protected into allow. It is the ONLY way to make a world destroyable,
// it is called from exactly one place (rebuild.Run, section 13.6 step 8), and
// it CONSTRUCTS A NEW JAIL rather than mutating one.
//
// Every named path must currently be protected by j. A path this jail does not
// protect is not the caller's to promote: accepting one would let `gvm rebuild`
// widen the jail to anything at all by naming it, which is precisely the
// property the jail exists to deny.
func (j *Jail) Rebuild(worldPaths []string) (*Jail, error) {
	if j == nil {
		return nil, fmt.Errorf("%w: no jail to rebuild", ErrJail)
	}
	if len(worldPaths) == 0 {
		return nil, fmt.Errorf("%w: rebuild names no world paths; there is nothing to promote, "+
			"and a rebuild that destroys nothing would report success having done nothing", ErrJail)
	}

	promote := make(map[string]bool, len(worldPaths))
	next := JailConfig{Allow: append([]string(nil), givens(j.allow)...), Strict: j.strict}

	for _, w := range worldPaths {
		r, err := newAllowRoot(w)
		if err != nil {
			return nil, fmt.Errorf("world path: %w", err)
		}
		found := false
		for _, p := range j.protected {
			if p.real == r.real {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: %s is not a protected path of this jail, so rebuild "+
				"cannot promote it; the destroy set is backup.world_paths and nothing else",
				ErrJail, r.given)
		}
		promote[r.real] = true
		next.Allow = append(next.Allow, r.given)
	}

	for _, p := range j.protected {
		if !promote[p.real] {
			next.Protected = append(next.Protected, p.given)
		}
	}
	return NewJail(next)
}

// Resolve turns one path into a Jailed, or explains why it will not.
//
// The order of the checks is the order of the messages an operator should get.
// Containment is answered before protection so that "/etc/passwd" is reported
// as outside the jail rather than as somebody's world.
//
// A relative path is interpreted against the primary allow root. An absolute
// path is taken as given and must still land inside one.
//
// What this does NOT promise: that the tree cannot change afterwards. The
// resolution is a fact about the filesystem at the moment it ran. Destroy is
// reached with the instance stopped and the per-instance job lock held, which
// is what closes the window; the type is the guarantee that the check happened
// at all, not that it is eternally fresh.
func (j *Jail) Resolve(rel string) (Jailed, error) {
	if j == nil || len(j.allow) == 0 {
		return Jailed{}, fmt.Errorf("%w: nil jail", ErrJail)
	}
	if strings.TrimSpace(rel) == "" {
		return Jailed{}, fmt.Errorf("%w: empty path", ErrOutside)
	}
	// rel is used exactly as written from here on. Space, tab, CR and LF are
	// all legal in a Linux filename, so "mods" and "mods " are two directories
	// that coexist; trimming would return a Jailed naming a path the caller
	// never asked about, while strict, containment and protection all answered
	// truthfully about the other one. That is the single thing this type
	// promises cannot happen.
	in := rel

	// Checked BEFORE Clean, not after. filepath.Clean absorbs "..": at the
	// filesystem root it deletes it outright ("/a/../../etc" becomes "/etc"),
	// and elsewhere it rewrites the path into a sibling that may well be
	// inside a different allow root. Either way the path that gets deleted is
	// not the path that was written down, and the whole check has been passed
	// by a string the author did not intend. Rejecting the element is the only
	// version of this test that cannot be argued with.
	if hasDotDot(in) {
		return Jailed{}, fmt.Errorf("%w: %q", ErrTraversal, rel)
	}

	lexical := in
	if !filepath.IsAbs(lexical) {
		lexical = filepath.Join(j.allow[0].given, lexical)
	}
	lexical = filepath.Clean(lexical)

	resolved, existing, err := resolveExisting(lexical)
	if err != nil {
		return Jailed{}, err
	}

	if j.strict && resolved != lexical {
		return Jailed{}, fmt.Errorf("%w: %s resolves to %s, and this jail is strict",
			ErrSymlink, lexical, resolved)
	}

	// Containment is checked against the RESOLVED path, which is what makes a
	// symlink escape a refusal: /srv/gvm/pz/data may be spelled inside the
	// jail and point at /home/someone-else.
	var match *jailRoot
	for i := range j.allow {
		if under(resolved, j.allow[i].real) {
			match = &j.allow[i]
			break
		}
	}
	if match == nil {
		if resolved != lexical {
			return Jailed{}, fmt.Errorf("%w: %s resolves to %s, which is outside %s",
				ErrOutside, lexical, resolved, strings.Join(j.AllowRoots(), ", "))
		}
		return Jailed{}, fmt.Errorf("%w: %s is not under %s",
			ErrOutside, resolved, strings.Join(j.AllowRoots(), ", "))
	}

	for _, p := range j.protected {
		if under(resolved, p.real) {
			return Jailed{}, fmt.Errorf("%w: %s is at or under %s; only `gvm rebuild` may "+
				"destroy a world path", ErrProtected, resolved, p.given)
		}
		// An ancestor of a protected path is as destructive as the protected
		// path itself: rm -rf on the parent takes the world with it. The
		// prefix test has to run in both directions or "delete the install
		// tree" quietly means "delete the saves inside it".
		if under(p.real, resolved) {
			return Jailed{}, fmt.Errorf("%w: deleting %s would take %s with it",
				ErrProtected, resolved, p.given)
		}
	}

	if err := checkSameDevice(match.real, existing); err != nil {
		return Jailed{}, err
	}
	return Jailed{root: match.real, abs: resolved}, nil
}

// ResolveAll resolves every path or returns no paths at all.
//
// It reports every failure rather than the first, because the caller is about
// to show an operator a destroy plan, and a plan that reveals its problems one
// run at a time turns a review into a guessing game. Returning a partial set on
// error would be worse than either: a destroy over half a plan is a destroy
// nobody approved.
func (j *Jail) ResolveAll(paths []string) ([]Jailed, error) {
	out := make([]Jailed, 0, len(paths))
	var errs []error
	for _, p := range paths {
		jp, err := j.Resolve(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, jp)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// checkSameDevice refuses a target that lives on a different filesystem than
// its allow root.
//
// A bind mount, an NFS mount or a separate volume mounted inside the instance
// tree passes every lexical test and is still not the jail's to delete. Off
// Linux the measurement does not exist; the check is skipped and every other
// one still applies, which is correct because gvm destroys nothing anywhere
// but Linux.
func checkSameDevice(root, target string) error {
	same, err := platform.SameDevice(root, target)
	if err != nil {
		if errors.Is(err, platform.ErrNotSupported) {
			return nil
		}
		return fmt.Errorf("%w: %v", ErrCrossDevice, err)
	}
	if !same {
		return fmt.Errorf("%w: %s is on a different filesystem than %s; something is mounted "+
			"inside the instance tree and deleting through it would take it with you",
			ErrCrossDevice, target, root)
	}
	return nil
}

// resolveExisting resolves symlinks in p, tolerating a path that does not exist
// yet, and also reports the deepest ancestor that does exist.
//
// The tolerance matters because a jail is used to check plans, not only
// completed facts: a destroy set rendered from a manifest glob can name a path
// this install never created, and a Resolve that failed on it would push the
// caller into "if the check errors, skip it" -- which is how the check stops
// happening. The existing ancestor is what the device test can actually stat.
func resolveExisting(p string) (resolved, existing string, err error) {
	cur, tail := p, ""
	for {
		real, evalErr := filepath.EvalSymlinks(cur)
		if evalErr == nil {
			if tail == "" {
				return real, real, nil
			}
			return filepath.Join(real, tail), real, nil
		}
		if !errors.Is(evalErr, fs.ErrNotExist) {
			return "", "", fmt.Errorf("instance: resolve %s: %w", p, evalErr)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", "", fmt.Errorf("instance: resolve %s: no existing ancestor", p)
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// hasDotDot reports whether any element of p is "..".
//
// It splits on both separators on every platform, so a path is judged the same
// way on the development machine as on the host. A file whose name contains a
// literal backslash is legal on Linux and would be refused here; no manifest
// writes one, and the alternative -- a safety check that behaves differently on
// the machine where it is tested -- is not a trade worth making.
func hasDotDot(p string) bool {
	for _, el := range strings.FieldsFunc(p, isAnySeparator) {
		if el == ".." {
			return true
		}
	}
	return false
}

func isAnySeparator(r rune) bool { return r == '/' || r == '\\' }

// under reports whether p is dir or lives inside it, comparing whole path
// elements. strings.HasPrefix alone would say "/srv/gvm/pz-main-old" is inside
// "/srv/gvm/pz-main", which is a neighbouring instance's entire world.
//
// Both arguments must already be cleaned. The comparison is case-sensitive
// because Linux is; a Windows-style case fold here would be a rule the
// production host does not share.
func under(p, dir string) bool {
	if p == dir {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(dir, sep) {
		dir += sep
	}
	return strings.HasPrefix(p, dir)
}

// systemRoots are directories that are never one instance's to delete, listed
// by name so the refusal does not depend on depth alone: "/opt" and "/srv" are
// two elements deep counted from nothing and still hold everybody's data.
var systemRoots = map[string]bool{
	"/bin": true, "/boot": true, "/dev": true, "/etc": true, "/home": true,
	"/lib": true, "/lib32": true, "/lib64": true, "/libx32": true, "/media": true,
	"/mnt": true, "/opt": true, "/proc": true, "/root": true, "/run": true,
	"/sbin": true, "/srv": true, "/sys": true, "/tmp": true, "/usr": true,
	"/var": true,
}

// refuseShallowAllowRoot judges one spelling of an allow root. It runs twice,
// before and after symlink resolution, because containment is decided on the
// resolved form: a root spelled three elements deep and symlinked at /opt would
// otherwise pass the test on the string and root the jail at every application
// on the box. It names both spellings for the same reason jailRoot keeps given
// alongside real -- the operator wrote one and has to recognise the other.
func refuseShallowAllowRoot(given, real string) error {
	if !shallowRoot(real) {
		return nil
	}
	if real == given {
		return fmt.Errorf("%w: %q is a system directory or one level below the "+
			"filesystem root; no instance owns a path that shallow", ErrJail, given)
	}
	return fmt.Errorf("%w: %q resolves to %q, which is a system directory or one level "+
		"below the filesystem root; no instance owns a path that shallow",
		ErrJail, given, real)
}

// shallowRoot answers systemRoot for a real, cleaned, platform-shaped path.
func shallowRoot(clean string) bool {
	rest := strings.TrimPrefix(clean, filepath.VolumeName(clean))
	return isSystemRoot(filepath.ToSlash(rest))
}

// isSystemRoot reports whether a slash-separated absolute path is too shallow
// or too well-known to be a destroy root. It takes slashes so that the rule is
// identical, and identically testable, on Linux and on Windows.
func isSystemRoot(slash string) bool {
	trimmed := strings.TrimSuffix(slash, "/")
	if trimmed == "" {
		return true // the filesystem root itself
	}
	if systemRoots[trimmed] {
		return true
	}
	n := 0
	for _, el := range strings.Split(trimmed, "/") {
		if el != "" {
			n++
		}
	}
	return n < 2
}
