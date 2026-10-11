package hook

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func TestFSRefusesEverySpellingOfEscape(t *testing.T) {
	e := newEnv(t, "generic-steam.yaml", nil)
	fsys := e.ctx.Fs

	for name, rel := range map[string]string{
		"parent":            "../data/secret",
		"deep parent":       "mods/../../../etc/passwd",
		"absolute unix":     "/etc/passwd",
		"absolute windows":  `\windows\system32`,
		"a bare parent":     "..",
		"a parent at depth": "a/b/../../..",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fsys.ReadFile(RootInstall, rel); err == nil {
				t.Errorf("ReadFile(%q) was allowed", rel)
			}
			err := fsys.WriteFile(RootInstall, rel, []byte("x"), 0o640)
			if err == nil {
				t.Fatalf("WriteFile(%q) was allowed", rel)
			}
			if !errors.Is(err, ErrPath) {
				t.Errorf("error does not wrap ErrPath: %v", err)
			}
			if _, err := fsys.Stat(RootInstall, rel); err == nil {
				t.Errorf("Stat(%q) was allowed", rel)
			}
			if _, err := fsys.Sub(RootInstall, rel); err == nil {
				t.Errorf("Sub(%q) was allowed", rel)
			}
		})
	}

	// A root this instance does not have is refused by name rather than
	// silently resolving to the first one.
	if _, err := fsys.Path("elsewhere"); err == nil {
		t.Error("Path() answered for a root that does not exist")
	}
}

func TestFSWritesAtomicallyAndHandsTheFileOver(t *testing.T) {
	e := newEnv(t, "generic-steam.yaml", nil)
	if err := e.ctx.Fs.WriteFile(RootData, "Server/conf.ini", []byte("a=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := e.read("data/Server/conf.ini"); got != "a=1\n" {
		t.Errorf("content = %q", got)
	}

	// The missing parent was created, and both it and the file were handed to
	// the instance account: a post_install hook runs as root, and a config file
	// the server cannot rewrite is a world whose settings silently revert.
	want := []string{"data/Server 998:998", "data/Server/conf.ini 998:998"}
	for _, w := range want {
		if !containsString(e.chowns, w) {
			t.Errorf("chowns = %v, missing %q", e.chowns, w)
		}
	}

	// A second write keeps the previous contents beside it, because the file it
	// replaces may hold a setting an admin made in-game.
	if err := e.ctx.Fs.WriteFile(RootData, "Server/conf.ini", []byte("a=2\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if !e.exists("data/Server/conf.ini.bak") {
		t.Error("no .bak was kept; a hook's overwrite of a server-owned file is unrecoverable")
	}
	if got := e.read("data/Server/conf.ini.bak"); got != "a=1\n" {
		t.Errorf(".bak = %q, want the previous contents", got)
	}
}

// TestFSCreatesEveryMissingAncestorOwned: os.MkdirAll creates intermediate
// directories too, and a root-owned Pal/Saved left between an owned Pal and an
// owned Config/ is a directory the server cannot write into.
func TestFSCreatesEveryMissingAncestorOwned(t *testing.T) {
	e := newEnv(t, "palworld.yaml", nil)
	rel := "Pal/Saved/Config/LinuxServer"
	if err := e.ctx.Fs.MkdirAll(RootInstall, rel, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{
		"install/Pal", "install/Pal/Saved", "install/Pal/Saved/Config",
		"install/Pal/Saved/Config/LinuxServer",
	} {
		if !containsString(e.chowns, d+" 998:998") {
			t.Errorf("%s was created but not handed to the instance account: %v", d, e.chowns)
		}
	}
}

// TestFSLeavesExistingDirectoriesAlone: an adopted instance's tree already has
// an owner, and re-chowning somebody's world is not a hook's business.
func TestFSLeavesExistingDirectoriesAlone(t *testing.T) {
	e := newEnv(t, "generic-steam.yaml", nil)
	e.write("data/existing/keep.txt", "x")
	e.chowns = nil
	if err := e.ctx.Fs.MkdirAll(RootData, "existing", 0o750); err != nil {
		t.Fatal(err)
	}
	if len(e.chowns) != 0 {
		t.Errorf("an existing directory was re-chowned: %v", e.chowns)
	}
}

func TestFSLocate(t *testing.T) {
	e := newEnv(t, "generic-steam.yaml", nil)
	install, err := e.ctx.Fs.Path(RootInstall)
	if err != nil {
		t.Fatal(err)
	}
	root, rel, err := e.ctx.Fs.Locate(filepath.Join(install, "media", "x.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if root != RootInstall || rel != "media/x.lua" {
		t.Errorf("Locate = %s/%s", root, rel)
	}

	// The root itself maps to ".", which a Glob or a Sub can use.
	if _, rel, err := e.ctx.Fs.Locate(install); err != nil || rel != "." {
		t.Errorf("Locate(root) = %q, %v", rel, err)
	}

	if _, _, err := e.ctx.Fs.Locate(filepath.Join(e.root, "outside")); err == nil {
		t.Error("Locate answered for a path outside every root")
	}
	if _, _, err := e.ctx.Fs.Locate("relative/path"); err == nil {
		t.Error("Locate answered for a relative path")
	}
}

// TestLocatePrefersTheLongestRoot is the adopted-instance case: a data tree
// that lives INSIDE the install tree has to resolve to the data root, or every
// hook addressing it would be writing through the install jail instead.
func TestLocatePrefersTheLongestRoot(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "pzserver")
	data := filepath.Join(install, "Zomboid")
	home := filepath.Join(root, "home")
	for _, d := range []string{install, data, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := provider.NewPaths(spec.Paths{Install: install, Data: data, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := NewFS(paths, provider.UnixIdent{})
	if err != nil {
		t.Fatal(err)
	}
	gotRoot, rel, err := fsys.Locate(filepath.Join(data, "Saves"))
	if err != nil {
		t.Fatal(err)
	}
	if gotRoot != RootData || rel != "Saves" {
		t.Errorf("Locate = %s/%s, want data/Saves", gotRoot, rel)
	}
}

func TestFSGlobStaysInsideItsRoot(t *testing.T) {
	e := newEnv(t, "projectzomboid.yaml", spec.VarValues{"server_name": "servertest"})
	for _, n := range []string{"Apocalypse", "Survivor", "SandboxVars"} {
		e.write("install/media/lua/shared/Sandbox/"+n+".lua", "return {\n}\n")
	}
	e.write("data/leak.lua", "return {\n}\n")

	got, err := e.ctx.Fs.Glob(RootInstall, "media/lua/shared/Sandbox/*.lua")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("Glob returned %v", got)
	}
	// Sorted, because a preset list an operator picks from must not reorder
	// between runs.
	if got[0] != "media/lua/shared/Sandbox/Apocalypse.lua" {
		t.Errorf("Glob is not sorted: %v", got)
	}

	// A pattern that tries to climb out is refused rather than matching
	// something in another tree.
	if _, err := e.ctx.Fs.Glob(RootInstall, "../data/*.lua"); err == nil {
		t.Error("Glob accepted a pattern that leaves its root")
	}
}

func TestFSSubIsReadOnlyAndRooted(t *testing.T) {
	e := newEnv(t, "projectzomboid.yaml", spec.VarValues{"server_name": "servertest"})
	e.write("data/steamapps/workshop/content/108600/2392709985/mods/Brita/mod.info",
		"name=Brita\nid=Brita\n")

	sub, err := e.ctx.Fs.Sub(RootData, "steamapps/workshop/content/108600")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(sub, "2392709985"); err != nil {
		t.Fatalf("the subtree does not contain the item: %v", err)
	}
	// A DirFS rooted at the subtree cannot be walked upwards out of it, which
	// is what makes it safe to hand to internal/pz's resolver.
	if _, err := fs.Stat(sub, "../../.."); err == nil {
		t.Error("the subtree can be walked out of")
	}
}

func TestNewFSRefusesIncompletePaths(t *testing.T) {
	// A Paths composite literal from another package can set the four public
	// fields and cannot set the jail, so "built by provider.NewPaths" has to be
	// checkable rather than assumed.
	if _, err := NewFS(provider.Paths{}, provider.UnixIdent{}); err == nil {
		t.Fatal("NewFS accepted a zero Paths")
	}
}

// TestWorkIsOptionalAndSaysSo: Work is SteamCMD's per-instance HOME and a
// non-Steam game has none, so a hook that needs it has to find out by asking
// rather than by writing into the wrong tree.
func TestWorkIsOptionalAndSaysSo(t *testing.T) {
	root := t.TempDir()
	dirs := map[string]string{}
	for _, n := range []string{"install", "data", "home"} {
		p := filepath.Join(root, n)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		dirs[n] = p
	}
	paths, err := provider.NewPaths(spec.Paths{
		Install: dirs["install"], Data: dirs["data"], Home: dirs["home"],
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := NewFS(paths, provider.UnixIdent{})
	if err != nil {
		t.Fatalf("NewFS refused a game with no steam home: %v", err)
	}
	_, err = fsys.Path(RootWork)
	if err == nil {
		t.Fatal("Path(work) answered for an instance that has no work tree")
	}
	if !strings.Contains(err.Error(), "work") {
		t.Errorf("the error does not name the root: %v", err)
	}
}

func TestDirModeForAFileMode(t *testing.T) {
	for in, want := range map[fs.FileMode]fs.FileMode{
		0o640: 0o750,
		0o644: 0o755,
		0o600: 0o700,
		0o660: 0o770,
	} {
		if got := dirModeFor(in); got != want {
			t.Errorf("dirModeFor(%04o) = %04o, want %04o", in, got, want)
		}
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// The production seam is os.Lchown, and that is load-bearing rather than a
// style choice.
//
// instance.Jail.Resolve proves a fact about the PATH and holds no descriptor,
// so between the resolve and the chown the instance account can rename the
// directory away and drop a symlink in its place -- and it owns the directory
// a created ancestor goes into, by hostops' DirInstall/DirData
// OwnedByInstance. os.Chown follows that link and hands root's chown an
// arbitrary target, which is the escalation §18.1 names. Lchown moves the
// damage to an inode the attacker already owns.
//
// Asserted by comparing the function the constructor installed, because the
// behavioural difference needs a symlink and os.Symlink on Windows needs a
// privilege this project must not teach anyone to grant.
func TestTheChownSeamDefaultsToLchown(t *testing.T) {
	e := newEnv(t, "generic-steam.yaml", nil)
	fsys, err := NewFS(e.ctx.Paths, provider.UnixIdent{})
	if err != nil {
		t.Fatal(err)
	}
	impl, ok := fsys.(*osFS)
	if !ok {
		t.Fatalf("NewFS returned %T", fsys)
	}
	want := reflect.ValueOf(os.Lchown).Pointer()
	if got := reflect.ValueOf(impl.chown).Pointer(); got != want {
		t.Error("the chown seam does not default to os.Lchown; os.Chown follows a symlink, and " +
			"this call runs as root against a path the instance account can swap")
	}
}

// Each level is created and handed over before the next one is created.
//
// One os.MkdirAll followed by a sweep of chowns is a window in which a symlink
// planted after the first level redirects the rest of the creation; per-level
// leaves one syscall between create and own. Stated portably: at the moment a
// directory is handed to the instance account, nothing has been created inside
// it yet.
func TestFSOwnsEachAncestorBeforeCreatingTheNext(t *testing.T) {
	e := newEnv(t, "palworld.yaml", nil)
	owner, err := provider.NewUnixIdent("gvm-test", "gvm-test", 998, 998)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	fsys, err := NewFS(e.ctx.Paths, owner, WithChown(func(p string, uid, gid int) error {
		fi, serr := os.Lstat(p)
		if serr != nil {
			t.Errorf("chown %s: the target does not exist: %v", p, serr)
			return nil
		}
		if fi.IsDir() {
			ents, rerr := os.ReadDir(p)
			if rerr != nil {
				t.Errorf("read %s: %v", p, rerr)
			}
			if len(ents) != 0 {
				t.Errorf("%s was handed over only after %d entries were created inside it, so "+
					"the deeper levels were created before this one was owned", p, len(ents))
			}
		}
		order = append(order, filepath.ToSlash(p))
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	if err := fsys.MkdirAll(RootInstall, "Pal/Saved/Config/LinuxServer", 0o750); err != nil {
		t.Fatal(err)
	}
	if len(order) != 4 {
		t.Fatalf("chowns = %v, want one per created level", order)
	}
	// Outermost first, so a failure halfway leaves the shallower directories
	// correct rather than the deeper ones.
	for i := 1; i < len(order); i++ {
		if !strings.HasPrefix(order[i], order[i-1]+"/") {
			t.Errorf("chown order is not outermost-first: %v", order)
		}
	}
}
