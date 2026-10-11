package steamcmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The three shims a SteamCMD-installed server needs, and the per-instance HOME
// that keeps two of them from corrupting each other.
//
// Each one has the same shape of failure: the server starts, systemctl calls it
// active, and it is either invisible to players or dead with no useful error.
// They are bite-sized to fix and painful to diagnose from a dashboard, which is
// why they are modelled rather than left to a per-game script.
//
//  1. steamclient.so. Servers that call SteamAPI_Init dlopen
//     "$HOME/.steam/sdk64/steamclient.so" (32-bit engines: sdk32). SteamCMD
//     never puts it there. Symptom: "[S_API FAIL] SteamAPI_Init() failed" or
//     "Failed to load module '…/.steam/sdk64/steamclient.so'".
//
//  2. SteamAppId. Several servers are INSTALLED under a server appid and must
//     ANNOUNCE a different game appid at runtime (Valheim 896660 -> 892970,
//     Core Keeper 1963720 -> 1621690). Symptom: the server does not start, or
//     starts and never appears in the server browser.
//
//  3. The HOME layout. steamcmd writes both "$HOME/.steam" and
//     "$XDG_DATA_HOME/Steam", and ValveSoftware/steam-for-linux#6976 documents
//     it using the two inconsistently; LinuxGSM pre-creates
//     ~/.local/share/Steam and symlinks ~/.steam/root and ~/.steam/steam at it.
//
// And the HOME itself: two steamcmd processes under one HOME fight over
// ~/.steam and race on appmanifest files, which is the single best way to
// corrupt an install. Each instance gets its own (D-30, section 5.2); the
// host-wide flock stays as belt-and-braces.
//
// Everything here is split into a pure plan and an apply against a narrow
// filesystem seam, so the derivation -- which paths, which targets, which
// modes -- is asserted on the Windows development machine, where os.Symlink
// needs a privilege gvm does not have and must not ask for.

// ErrShim is a shim gvm will not apply.
var ErrShim = errors.New("steamcmd: shim refused")

// ShimKind is what a shim puts on disk.
type ShimKind uint8

const (
	ShimDir ShimKind = iota
	ShimSymlink
	ShimFile
)

func (k ShimKind) String() string {
	switch k {
	case ShimDir:
		return "directory"
	case ShimSymlink:
		return "symlink"
	case ShimFile:
		return "file"
	}
	return "shim(" + strconv.Itoa(int(k)) + ")"
}

// Shim is one filesystem fact a server needs.
type Shim struct {
	Kind    ShimKind
	Path    string // absolute
	Target  string // ShimSymlink only
	Content []byte // ShimFile only
	Mode    fs.FileMode
	// Why is rendered by `gvm doctor` and by the install log. A symlink whose
	// reason nobody recorded is a symlink a future cleanup deletes.
	Why string
}

func (s Shim) String() string {
	if s.Kind == ShimSymlink {
		return fmt.Sprintf("%s %s -> %s", s.Kind, s.Path, s.Target)
	}
	return fmt.Sprintf("%s %s", s.Kind, s.Path)
}

// ShimInput is everything the plan is derived from.
type ShimInput struct {
	// Home is the GAME's HOME (spec paths.home): /srv/gvm/<name> for a new
	// instance, /home/pzserver for the adopted one. This is where a server
	// dlopens steamclient.so from, so it must be where link_sdk writes.
	Home string
	// Work is SteamCMD's HOME (spec paths.work),
	// /var/lib/gvm/cache/steam/homes/<name>. Empty when this game has no
	// steamcmd step.
	Work string
	// SteamCMDDir is where the steamcmd tarball was extracted. linux64/ and
	// linux32/ hold the steamclient.so the sdk symlinks point at.
	SteamCMDDir string
	// Arch is spec.runtime.arch, which decides whether the engine will look in
	// sdk64, sdk32 or both.
	Arch string
	// RuntimeAppID is the appid the server must announce, or 0 when it is the
	// install appid and no shim is needed.
	RuntimeAppID int
	// AppIDFileRel is where steam_appid.txt goes, relative to the install tree.
	// Empty means the generic fallback is not wanted for this game.
	AppIDFileRel string
}

// steamDirMode is 0755: the game's own user owns these and nothing else needs
// to write them.
const steamDirMode fs.FileMode = 0o755

// HomeShims is the plan for the two HOME-side shims: the layout and the sdk
// symlinks.
//
// It covers both HOMEs when they differ, because steamcmd's confusion about
// ~/.steam affects its own HOME and the dlopen affects the game's, and a shim
// applied to only one of them is the version of this bug that takes an evening
// to find.
//
// Nothing here is written through the install tree's jail, and that is
// deliberate: provider.Paths.Dest is a STRICT jail rooted at the install path,
// so it refuses any path that reaches its target through a symlink -- which is
// exactly what ~/.steam/root and ~/.steam/sdk64 are. These paths are under
// Home and Work, which the install jail does not own at all.
func HomeShims(in ShimInput) ([]Shim, error) {
	home := strings.TrimSpace(in.Home)
	if home == "" {
		return nil, fmt.Errorf("%w: no game HOME; a server with no stable HOME cannot be given a "+
			"steamclient.so to dlopen", ErrShim)
	}
	if !filepath.IsAbs(home) && !strings.HasPrefix(home, "/") {
		return nil, fmt.Errorf("%w: HOME %q is not absolute", ErrShim, home)
	}

	var out []Shim
	out = append(out, layoutShims(home, "the game's HOME")...)
	if w := strings.TrimSpace(in.Work); w != "" && w != home {
		out = append(out, layoutShims(w, "SteamCMD's own HOME (D-30)")...)
	}

	if dir := strings.TrimSpace(in.SteamCMDDir); dir != "" {
		for _, bits := range SDKBits(in.Arch) {
			sdk := path.Join(home, ".steam", "sdk"+strconv.Itoa(bits))
			out = append(out, Shim{
				Kind: ShimDir, Path: sdk, Mode: steamDirMode,
				Why: fmt.Sprintf("servers dlopen $HOME/.steam/sdk%d/steamclient.so", bits),
			}, Shim{
				Kind:   ShimSymlink,
				Path:   path.Join(sdk, "steamclient.so"),
				Target: path.Join(dir, "linux"+strconv.Itoa(bits), "steamclient.so"),
				Why: "SteamCMD does not place steamclient.so where SteamAPI_Init looks for it; " +
					"without this the server prints [S_API FAIL] and is invisible to the " +
					"server browser",
			})
		}
	}
	return out, nil
}

// layoutShims is the ~/.steam/{root,steam} -> ~/.local/share/Steam layout.
func layoutShims(home, which string) []Shim {
	data := path.Join(home, ".local", "share", "Steam")
	dotSteam := path.Join(home, ".steam")
	why := "steamcmd uses ~/.steam and ~/.local/share/Steam inconsistently " +
		"(steam-for-linux#6976); this is " + which
	return []Shim{
		{Kind: ShimDir, Path: data, Mode: steamDirMode, Why: why},
		{Kind: ShimDir, Path: dotSteam, Mode: steamDirMode, Why: why},
		{Kind: ShimSymlink, Path: path.Join(dotSteam, "root"), Target: data, Why: why},
		{Kind: ShimSymlink, Path: path.Join(dotSteam, "steam"), Target: data, Why: why},
	}
}

// SDKBits is which steamclient.so a game's engine will look for.
//
// A 32-bit engine (srcds, GoldSrc, Garry's Mod, Left 4 Dead 2) looks in sdk32
// and never in sdk64. Linking both would be harmless on disk and untrue in
// `gvm doctor`, which lists these shims as facts about the instance.
func SDKBits(arch string) []int {
	switch strings.TrimSpace(strings.ToLower(arch)) {
	case "i386", "x86":
		return []int{32}
	case "any":
		return []int{64, 32}
	default:
		return []int{64}
	}
}

// AppIDShim is the steam_appid.txt fallback, as a path RELATIVE to the install
// tree so it can be resolved through provider.Paths.Dest.
//
// It is the generic answer for a server that reports "no appID found" and does
// not read the environment: a file containing the appid, next to the binary.
// The env pair is the first answer and is produced by RuntimeEnv.
func AppIDShim(in ShimInput) (rel string, content []byte, ok bool) {
	rel = strings.TrimSpace(in.AppIDFileRel)
	if rel == "" || in.RuntimeAppID <= 0 {
		return "", nil, false
	}
	return rel, []byte(strconv.Itoa(in.RuntimeAppID) + "\n"), true
}

// RuntimeEnv is the environment the GAME needs, as facts for the unit
// generator and for doctor.
//
// It does not render the manifest's runtime.env -- that is the reconciler's job
// and the manifest is its source of truth. This is the provider's answer to
// "which of those values exist because of how Steam installed this game", so
// that a missing SteamAppId can be reported as a shim problem rather than
// discovered as a server that will not boot.
func RuntimeEnv(in ShimInput) map[string]string {
	env := map[string]string{}
	if h := strings.TrimSpace(in.Home); h != "" {
		env["HOME"] = h
	}
	if in.RuntimeAppID > 0 {
		env["SteamAppId"] = strconv.Itoa(in.RuntimeAppID)
	}
	return env
}

// SteamCMDEnv is the environment the steamcmd RUN needs.
//
// HOME is the per-instance one, which is the whole of D-30: steamcmd derives
// XDG_DATA_HOME from HOME, so one variable moves ~/.steam and
// ~/.local/share/Steam together and two instances can never share either.
//
// The UTF-8 locale is not cosmetic. steamcmd misbehaves without one -- the
// existing install.sh sets it before doing anything else -- and the failure is
// a mangled progress line at best and a refused download at worst.
func SteamCMDEnv(in ShimInput) map[string]string {
	home := strings.TrimSpace(in.Work)
	if home == "" {
		home = strings.TrimSpace(in.Home)
	}
	return map[string]string{
		"HOME":   home,
		"LANG":   "C.UTF-8",
		"LC_ALL": "C.UTF-8",
	}
}

// RuntimeAppID reads the appid a manifest says the server must announce.
//
// It reads spec.runtime.env.SteamAppId, which is where the shipped manifests
// declare it (Valheim: SteamAppId: "892970" beside install_appid 896660). A
// value that is not a plain number is reported as absent rather than guessed
// at: it means the manifest is computing it from a template, and the renderer
// owns that, not this.
func RuntimeAppID(m *spec.Manifest) (int, bool) {
	if m == nil {
		return 0, false
	}
	raw := strings.TrimSpace(m.Spec.Runtime.Env["SteamAppId"])
	if raw == "" {
		return 0, false
	}
	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// ShimFS is every filesystem effect applying a shim has.
//
// A seam, for the same reason hostops.FS is one: the interesting half of a
// symlink is which target, and os.Symlink on the Windows development machine
// needs a privilege that gvm must not teach anyone to grant. A fake
// implementing this runs the whole apply path, including the three cases that
// matter -- already correct, pointing somewhere else, and occupied by a real
// file.
type ShimFS interface {
	MkdirAll(path string, perm fs.FileMode) error
	// Lstat does NOT follow symlinks; the apply path depends on that.
	Lstat(path string) (fs.FileInfo, error)
	Stat(path string) (fs.FileInfo, error)
	Readlink(path string) (string, error)
	Symlink(target, link string) error
	Remove(path string) error
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm fs.FileMode) error
}

// OSShimFS is the real filesystem.
var OSShimFS ShimFS = osShimFS{}

type osShimFS struct{}

func (osShimFS) MkdirAll(p string, perm fs.FileMode) error { return os.MkdirAll(p, perm) }
func (osShimFS) Lstat(p string) (fs.FileInfo, error)       { return os.Lstat(p) }
func (osShimFS) Stat(p string) (fs.FileInfo, error)        { return os.Stat(p) }
func (osShimFS) Readlink(p string) (string, error)         { return os.Readlink(p) }
func (osShimFS) Symlink(target, link string) error         { return os.Symlink(target, link) }
func (osShimFS) Remove(p string) error                     { return os.Remove(p) }
func (osShimFS) ReadFile(p string) ([]byte, error)         { return os.ReadFile(p) }
func (osShimFS) WriteFile(p string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(p, data, perm)
}

// ShimResult is what applying a shim plan did.
type ShimResult struct {
	Created []string
	Already []string
	// Problems are shims gvm declined to force. They are NOT errors: a real
	// steamclient.so that an operator copied into ~/.steam/sdk64 by hand works
	// perfectly, and replacing a file gvm did not create is not this code's
	// call to make. They are reported so doctor can show them.
	Problems []string
}

// ApplyShims makes the plan true, idempotently.
//
// The error it returns is for failures that mean the shim could not be
// attempted at all -- a mkdir that was refused, a symlink that failed for a
// reason other than already existing. A shim gvm declines to force is a
// Problem, not an error, because the install is usually still correct and the
// hook phase treats an error as fatal.
func ApplyShims(shims []Shim, fsys ShimFS) (*ShimResult, error) {
	if fsys == nil {
		return nil, fmt.Errorf("%w: no filesystem", ErrShim)
	}
	res := &ShimResult{}
	for _, s := range shims {
		if !strings.HasPrefix(s.Path, "/") && !filepath.IsAbs(s.Path) {
			return res, fmt.Errorf("%w: %q is not an absolute path", ErrShim, s.Path)
		}
		var err error
		switch s.Kind {
		case ShimDir:
			err = applyShimDir(s, fsys, res)
		case ShimSymlink:
			err = applyShimLink(s, fsys, res)
		case ShimFile:
			err = applyShimFile(s, fsys, res)
		default:
			err = fmt.Errorf("%w: unknown shim kind %d", ErrShim, s.Kind)
		}
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

func applyShimDir(s Shim, fsys ShimFS, res *ShimResult) error {
	if fi, err := fsys.Lstat(s.Path); err == nil {
		if fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
			// A symlinked directory is the layout shim's own output: ~/.steam
			// is a directory and ~/.steam/root is a link into it.
			res.Already = append(res.Already, s.String())
			return nil
		}
		res.Problems = append(res.Problems, fmt.Sprintf("%s exists and is not a directory, so "+
			"%s cannot be created there", s.Path, s.Why))
		return nil
	}
	mode := s.Mode
	if mode == 0 {
		mode = steamDirMode
	}
	if err := fsys.MkdirAll(s.Path, mode); err != nil {
		return fmt.Errorf("steamcmd: create %s: %w", s.Path, err)
	}
	res.Created = append(res.Created, s.String())
	return nil
}

func applyShimLink(s Shim, fsys ShimFS, res *ShimResult) error {
	if strings.TrimSpace(s.Target) == "" {
		return fmt.Errorf("%w: %s has no target", ErrShim, s.Path)
	}
	// A link to a target that is not there is worse than no link: the dlopen
	// failure it produces looks identical to the missing-shim failure it was
	// supposed to fix, and the real problem (steamcmd is not installed where
	// gvm thinks) is one directory away.
	if _, err := fsys.Stat(s.Target); err != nil {
		res.Problems = append(res.Problems, fmt.Sprintf("%s is not there, so %s was not created; "+
			"%s", s.Target, s.Path, s.Why))
		return nil
	}

	fi, err := fsys.Lstat(s.Path)
	switch {
	case err != nil:
		// Absent: create it.
	case fi.Mode()&fs.ModeSymlink != 0:
		if cur, rerr := fsys.Readlink(s.Path); rerr == nil && cur == s.Target {
			res.Already = append(res.Already, s.String())
			return nil
		}
		// Pointing somewhere else: steamcmd moved, or this is a stale link from
		// a previous layout. Replaced, because a link gvm owns pointing at a
		// path that no longer exists is the same failure as no link at all.
		if rmErr := fsys.Remove(s.Path); rmErr != nil {
			return fmt.Errorf("steamcmd: replace %s: %w", s.Path, rmErr)
		}
	default:
		res.Problems = append(res.Problems, fmt.Sprintf("%s is a real %s and not a symlink; gvm "+
			"will not replace a file it did not create. If the server cannot find steamclient.so, "+
			"remove it and re-run the install", s.Path, fi.Mode().Type()))
		return nil
	}

	if err := fsys.Symlink(s.Target, s.Path); err != nil {
		return fmt.Errorf("steamcmd: link %s -> %s: %w", s.Path, s.Target, err)
	}
	res.Created = append(res.Created, s.String())
	return nil
}

func applyShimFile(s Shim, fsys ShimFS, res *ShimResult) error {
	if fi, err := fsys.Lstat(s.Path); err == nil && fi.IsDir() {
		res.Problems = append(res.Problems, s.Path+" is a directory, so the shim file was not written")
		return nil
	}
	if cur, err := fsys.ReadFile(s.Path); err == nil && string(cur) == string(s.Content) {
		res.Already = append(res.Already, s.String())
		return nil
	}
	mode := s.Mode
	if mode == 0 {
		mode = 0o644
	}
	if err := fsys.WriteFile(s.Path, s.Content, mode); err != nil {
		return fmt.Errorf("steamcmd: write %s: %w", s.Path, err)
	}
	res.Created = append(res.Created, s.String())
	return nil
}
