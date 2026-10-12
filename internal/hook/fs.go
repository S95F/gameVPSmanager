package hook

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// ErrPath is a path a hook may not address.
var ErrPath = errors.New("hook: path refused")

// dirModeFor is the directory mode that goes with a file mode.
//
// A 0640 config file wants a 0750 directory: the owner traverses and writes,
// the group (gvm-console, gvm-web) traverses and reads, and nothing else sees
// it. Derived rather than passed, because every caller would pass the same pair
// and the one that got it wrong would be a world directory other accounts can
// list.
func dirModeFor(m fs.FileMode) fs.FileMode {
	d := m.Perm()
	if d&0o400 != 0 {
		d |= 0o100
	}
	if d&0o040 != 0 {
		d |= 0o010
	}
	if d&0o004 != 0 {
		d |= 0o001
	}
	return d
}

// FSOption configures the hook filesystem.
type FSOption func(*osFS)

// WithChown replaces the ownership call. Tests use it; nothing in production
// should.
func WithChown(ch func(path string, uid, gid int) error) FSOption {
	return func(f *osFS) {
		if ch != nil {
			f.chown = ch
		}
	}
}

// NewFS builds the filesystem a hook writes through.
//
// owner is the account that owns the instance tree, and everything this FS
// creates is handed to it. That is not tidiness: post_install and post_content
// hooks run in the JOB process, which is root, while pre_start hooks run in the
// runner as the instance user. A PalWorldSettings.ini seeded root-owned is a
// file the server cannot rewrite when it shuts down, and the symptom is a world
// whose settings silently revert -- a bug whose cause is three layers from
// where it shows up.
//
// The zero UnixIdent means "do not chown", which is what a hook already running
// as the instance user wants, and what the Windows development machine can do.
func NewFS(p provider.Paths, owner provider.UnixIdent, opts ...FSOption) (JailedFS, error) {
	roots := map[Root]string{}
	for r, j := range map[Root]instance.Jailed{
		RootInstall: p.Install,
		RootData:    p.Data,
		RootHome:    p.Home,
		RootWork:    p.Work,
	} {
		if j.Valid() {
			roots[r] = j.Path()
		}
	}
	// Work is legitimately absent -- it is SteamCMD's per-instance HOME (D-30)
	// and a non-Steam game has none -- so a hook that needs it finds out by
	// asking rather than by writing into the wrong tree. The other three are
	// required, and provider.NewPaths has already refused a Paths without them.
	for _, r := range []Root{RootInstall, RootData, RootHome} {
		if roots[r] == "" {
			return nil, fmt.Errorf("%w: these Paths carry no %s root, so they did not come "+
				"from provider.NewPaths", ErrNoSeam, r)
		}
	}

	allow := make([]string, 0, len(roots))
	for _, r := range Roots() {
		if roots[r] != "" {
			allow = append(allow, roots[r])
		}
	}
	// Not strict, and no protected paths. Not strict because ~/.steam/root and
	// ~/.steam/sdk64 are symlinks the steam shims put there on purpose, and a
	// strict jail refuses a path that reaches its target through one. No
	// protected paths because this jail answers containment -- is this inside a
	// tree this instance owns -- and not world safety: the destroy jail is
	// instance.NewJail's job with backup.world_paths protected, and protecting
	// the world here would refuse to resolve Data itself, since Data is the
	// world's parent.
	j, err := instance.NewJail(instance.JailConfig{Allow: allow})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSeam, err)
	}

	// os.Lchown and NOT os.Chown. instance.Jail.Resolve proves a fact about the
	// PATH at check time and holds no descriptor, so between the resolve and
	// the chown the instance account -- which owns the directory a created
	// ancestor goes into, by hostops' OwnedByInstance -- can rename it away and
	// leave a symlink in its place. os.Chown follows that link and hands root's
	// chown a target the attacker picked; §18.1 says a compromised workshop
	// item must not yield root, /etc/gvm/secrets or another instance's tree.
	// Every legitimate target here was symlink-evaluated by resolve() -- a
	// directory mkdirOwned just created, or the file WriteFileAtomic just
	// renamed into place -- so Lchown is identical on every real path and
	// differs only when the inode was swapped. A symlink is never something a
	// hook meant to chown.
	f := &osFS{roots: roots, jail: j, owner: owner, chown: os.Lchown}
	for _, o := range opts {
		o(f)
	}
	return f, nil
}

type osFS struct {
	roots map[Root]string
	jail  *instance.Jail
	owner provider.UnixIdent
	chown func(path string, uid, gid int) error
}

func (f *osFS) Path(r Root) (string, error) {
	p, ok := f.roots[r]
	if !ok || p == "" {
		return "", fmt.Errorf("%w: this instance has no %s root", ErrPath, r)
	}
	return p, nil
}

// resolve turns a (root, relative path) pair into an absolute path, or refuses.
//
// Three checks, in the order the messages should arrive: the root exists for
// this instance, the relative path is relative and contains no "..", and the
// resolved result is still under THAT root. The third one is the reason this
// goes through a jail at all: install/mods may be a symlink to another
// instance's tree, and a containment check on the written-down path would
// approve it.
func (f *osFS) resolve(r Root, rel string) (string, error) {
	root, err := f.Path(r)
	if err != nil {
		return "", err
	}
	clean, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	jp, err := f.jail.Resolve(filepath.Join(root, filepath.FromSlash(clean)))
	if err != nil {
		return "", fmt.Errorf("%w: %s/%s: %v", ErrPath, r, clean, err)
	}
	// The jail proves "inside one of this instance's four roots". This proves
	// "inside the one that was named", so that a write addressed to install
	// cannot land in the world tree through a link.
	if !under(jp.Path(), root) {
		return "", fmt.Errorf("%w: %s/%s resolves to %s, which is outside the %s root (%s)",
			ErrPath, r, clean, jp.Path(), r, root)
	}
	return jp.Path(), nil
}

// cleanRel refuses anything that is not a relative path inside a root.
//
// ".." is rejected as an ELEMENT rather than cleaned away, for the reason
// instance.Jail gives: filepath.Clean absorbs "..", rewriting a path into a
// sibling that may well be inside a different root, and the file that then gets
// written is not the file anybody wrote down.
func cleanRel(rel string) (string, error) {
	s := strings.TrimSpace(rel)
	if s == "" || s == "." {
		return ".", nil
	}
	if filepath.IsAbs(s) || strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\`) {
		return "", fmt.Errorf("%w: %q is absolute; a hook addresses a file as a root plus a "+
			"relative path, so that there is no spelling of another instance's tree", ErrPath, s)
	}
	slashed := filepath.ToSlash(s)
	for _, el := range strings.Split(slashed, "/") {
		if el == ".." {
			return "", fmt.Errorf("%w: %q contains a \"..\" element", ErrPath, rel)
		}
	}
	return path.Clean(slashed), nil
}

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

func (f *osFS) Stat(r Root, rel string) (fs.FileInfo, error) {
	p, err := f.resolve(r, rel)
	if err != nil {
		return nil, err
	}
	return os.Stat(p)
}

func (f *osFS) ReadFile(r Root, rel string) ([]byte, error) {
	p, err := f.resolve(r, rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func (f *osFS) ReadDir(r Root, rel string) ([]fs.DirEntry, error) {
	p, err := f.resolve(r, rel)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(p)
}

// Glob matches a pattern under one root.
//
// Through fs.Glob over an os.DirFS rather than filepath.Glob, so the pattern
// cannot escape the root even before the jail sees the matches: a pattern is
// manifest-supplied, and "../../*" is a glob a careless manifest could contain.
// Matches are re-resolved individually, so a symlinked match is refused the same
// way a written-down one would be.
func (f *osFS) Glob(r Root, pattern string) ([]string, error) {
	root, err := f.Path(r)
	if err != nil {
		return nil, err
	}
	pat, err := cleanRel(pattern)
	if err != nil {
		return nil, err
	}
	matches, err := fs.Glob(os.DirFS(root), pat)
	if err != nil {
		return nil, fmt.Errorf("%w: %s/%s: %v", ErrPath, r, pattern, err)
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if _, err := f.resolve(r, m); err != nil {
			// A match gvm will not address is skipped rather than fatal: a
			// stray symlink in an install tree must not stop a preset list
			// from being read, and the hook reports what it used.
			continue
		}
		out = append(out, m)
	}
	sort.Strings(out)
	return out, nil
}

func (f *osFS) Sub(r Root, rel string) (fs.FS, error) {
	p, err := f.resolve(r, rel)
	if err != nil {
		return nil, err
	}
	// os.DirFS and not fs.Sub of a parent: this is handed to internal/pz's
	// resolver, which walks it, and ".." cannot walk a DirFS out of its own
	// root.
	//
	// A SYMLINK inside the subtree still can, and that is accepted rather than
	// refused. Two reasons. The subtree was already symlink-evaluated and
	// contained by instance.Jail in resolve() above, so the root itself is not
	// an escape. And an operator who moved workshop content to a second disk
	// and left `content/108600/<id> -> /mnt/big/<id>` is ordinary on a VPS with
	// a small root disk; refusing that link would report the item missing and
	// leave its ids out of the activation line -- a mod the operator declared
	// that silently never loads, which is the one failure section 12.5 exists
	// to prevent, this time caused by gvm's own strictness. The write jail can
	// afford to be strict because a refused write fails loudly; a refused READ
	// degrades into a shorter Mods=. What the reader does enforce is the dirent
	// TYPE and a size bound on each mod.info (internal/pz), which closes the
	// hang and the read-through without that regression.
	return os.DirFS(p), nil
}

func (f *osFS) MkdirAll(r Root, rel string, perm fs.FileMode) error {
	p, err := f.resolve(r, rel)
	if err != nil {
		return err
	}
	if perm == 0 {
		perm = 0o750
	}
	return f.mkdirOwned(p, perm)
}

// mkdirOwned creates the missing ancestors and hands each one to the instance
// account.
//
// Each one, not just the leaf: a root-owned Pal/Saved left between an owned Pal
// and an owned Config/ is a directory the server cannot write into. The
// existing ones are left alone, because an adopted instance's tree already has
// an owner and re-chowning somebody's world is not a hook's business.
//
// One level at a time, outermost first, each handed over as soon as it exists,
// rather than one os.MkdirAll followed by a sweep of chowns. The sweep is a
// window: this runs as root in the job process (see NewFS), the directory the
// outermost new ancestor goes into is owned BY THE INSTANCE ACCOUNT, and a
// single MkdirAll can create four levels through a symlink planted after the
// first one. Per-level reduces the create-to-own gap to one syscall and leaves
// nothing for a swap to reach; os.Lchown is what makes the remaining window
// harmless. An already-existing level is left alone, which is contract rule 2.
func (f *osFS) mkdirOwned(abs string, perm fs.FileMode) error {
	if fi, err := os.Stat(abs); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("hook: create %s: a file is already there", abs)
		}
		return nil
	}
	var missing []string
	for p := abs; ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			break
		}
		missing = append(missing, p)
		if parent := filepath.Dir(p); parent == p {
			break
		}
	}
	// Outermost first, so a failure halfway leaves the shallower directories
	// correct rather than the deeper ones.
	for i := len(missing) - 1; i >= 0; i-- {
		p := missing[i]
		if err := os.Mkdir(p, perm); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("hook: create %s: %w", p, err)
			}
			// Something else created it between the stat and the mkdir. It is
			// then an existing directory, and this call does not own it.
			continue
		}
		if err := f.give(p); err != nil {
			return err
		}
	}
	return nil
}

// WriteFile replaces a file atomically and hands it to the instance account.
//
// Atomically, with the previous contents kept beside it: a game server owns its
// config files and rewrites them on its own schedule, so anything a hook
// replaces may contain a setting an admin made in-game or a fix applied at 3am,
// and a truncate-then-fail would leave a server that cannot boot.
//
// Missing parent directories are created. A hook that had to MkdirAll first is
// a hook where somebody eventually does not, and the failure (an ENOENT in the
// middle of a post_install phase) reads as a broken install rather than as a
// missing mkdir.
func (f *osFS) WriteFile(r Root, rel string, data []byte, perm fs.FileMode) error {
	p, err := f.resolve(r, rel)
	if err != nil {
		return err
	}
	if perm == 0 {
		perm = 0o640
	}
	if err := f.mkdirOwned(filepath.Dir(p), dirModeFor(perm)); err != nil {
		return err
	}
	if err := spec.WriteFileAtomic(p, data, perm); err != nil {
		return err
	}
	return f.give(p)
}

// give hands a path to the instance account, if there is one to hand it to.
func (f *osFS) give(abs string) error {
	if !f.owner.Valid() || f.chown == nil {
		return nil
	}
	if err := f.chown(abs, f.owner.UID(), f.owner.GID()); err != nil {
		return fmt.Errorf("hook: chown %s to %s: %w", abs, f.owner, err)
	}
	return nil
}

// Locate maps an absolute path back to the pair that addresses it.
//
// The manifest's own fields are templates that render to absolute paths --
// "{{ .Data }}/Server/{{ .Var.server_name }}.ini" -- so a hook handed one has
// to get back to a (root, rel) pair to touch it at all. The longest matching
// root wins, which is what makes an adopted instance whose data tree lives
// INSIDE its install tree resolve to the data root rather than to a path
// relative to install.
func (f *osFS) Locate(abs string) (Root, string, error) {
	in := filepath.Clean(abs)
	if !filepath.IsAbs(in) {
		return "", "", fmt.Errorf("%w: %q is not absolute, so there is nothing to locate",
			ErrPath, abs)
	}
	var (
		best    Root
		bestLen = -1
		rel     string
	)
	for _, r := range Roots() {
		root := f.roots[r]
		if root == "" || !under(in, root) {
			continue
		}
		if len(root) > bestLen {
			sub, err := filepath.Rel(root, in)
			if err != nil {
				continue
			}
			best, bestLen, rel = r, len(root), filepath.ToSlash(sub)
		}
	}
	if bestLen < 0 {
		return "", "", fmt.Errorf("%w: %s is not under any of this instance's roots; a hook "+
			"cannot address a path outside them", ErrPath, in)
	}
	return best, path.Clean(rel), nil
}
