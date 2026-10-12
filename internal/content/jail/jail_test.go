package jail

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/instance"
)

// contentTree builds a real Minecraft-shaped instance tree under t.TempDir().
// Real directories, not a fake filesystem: EvalSymlinks and the device check
// only tell the truth about a filesystem that exists.
//
//	<base>/srv/atm10/install/mods/        the content root
//	<base>/srv/atm10/install/config/
//	<base>/srv/atm10/install/world/       PRESERVED (section 12.8)
//	<base>/srv/atm10/install/server.properties   PRESERVED
//	<base>/srv/atm10/data/Saves/world/    PROTECTED (backup.world_paths)
//	<base>/srv/atm10/downloads/           a second content root, the drop-box
//	<base>/elsewhere/                     somewhere no target may reach
type fixture struct {
	base, root, install, downloads, world, props, saves, outside string
}

func contentTree(t *testing.T) fixture {
	t.Helper()
	f := fixture{base: t.TempDir()}
	f.root = filepath.Join(f.base, "srv", "atm10")
	f.install = filepath.Join(f.root, "install")
	f.downloads = filepath.Join(f.root, "downloads")
	f.world = filepath.Join(f.install, "world")
	f.props = filepath.Join(f.install, "server.properties")
	f.saves = filepath.Join(f.root, "data", "Saves", "world")
	f.outside = filepath.Join(f.base, "elsewhere")

	for _, d := range []string{
		filepath.Join(f.install, "mods"),
		filepath.Join(f.install, "config"),
		f.world,
		f.downloads,
		f.saves,
		f.outside,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.props, []byte("level-name=world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// modpackJail is the jail a modpack extraction gets: the install tree and the
// drop-box are writable, the world under data/ is protected outright, and
// section 12.8's list is preserved behind --force.
func modpackJail(t *testing.T, f fixture) *Jail {
	t.Helper()
	j, err := New(Config{
		Allow:     []string{f.install, f.downloads},
		Protected: []string{f.saves},
		Preserve:  []string{f.world, f.props},
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks need privilege on this Windows host: %v", err)
		}
		t.Fatal(err)
	}
}

func TestTargetAcceptsWhatTheJailIsFor(t *testing.T) {
	f := contentTree(t)
	j := modpackJail(t, f)

	for _, rel := range []string{
		"mods/jei-1.20.1.jar",
		"mods",
		"config/jei/jei-client.ini",
		"kubejs/server_scripts/new.js", // a directory that does not exist yet
	} {
		p, err := j.Target(rel)
		if err != nil {
			t.Fatalf("Target(%q) = %v, want a path", rel, err)
		}
		if !p.Valid() {
			t.Fatalf("Target(%q) returned an invalid Path", rel)
		}
		if want := filepath.Join(f.install, filepath.FromSlash(rel)); p.Path() != want {
			t.Errorf("Target(%q) = %s, want %s", rel, p.Path(), want)
		}
	}
}

// TestTargetRefusesEveryShapeOfEscape is the zip-slip table. Each row is a real
// member name from a real archive exploit, and the sentinel is asserted rather
// than just the failure: the four refusals send an operator to four different
// places.
func TestTargetRefusesEveryShapeOfEscape(t *testing.T) {
	f := contentTree(t)
	j := modpackJail(t, f)

	cases := []struct {
		name string
		rel  string
		want error
	}{
		{"parent traversal", "../../etc/cron.d/root", ErrTraversal},
		{"traversal mid-path", "mods/../../../etc/passwd", ErrTraversal},
		{"traversal that Clean would absorb", "../../../../etc/passwd", ErrTraversal},
		{"backslash traversal", `..\..\windows\system32\evil.dll`, ErrTraversal},
		{"absolute posix", "/etc/cron.d/root", ErrAbsolute},
		{"absolute windows", `\windows\system32\evil.dll`, ErrAbsolute},
		{"drive relative", "C:mods/evil.jar", ErrAbsolute},
		{"drive absolute", `C:\windows\evil.dll`, ErrAbsolute},
		{"empty", "   ", ErrBadTarget},
		{"nul byte", "mods/ok.jar\x00/../../etc/passwd", ErrBadTarget},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := j.Target(tc.rel)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Target(%q) = %v, want %v", tc.rel, err, tc.want)
			}
			// The refusal has to come with no usable path, because a caller
			// that ignores the error must not receive one anyway.
			if p.Valid() {
				t.Errorf("Target(%q) refused and still returned %s", tc.rel, p.Path())
			}
		})
	}
}

// TestTargetRefusesASymlinkedParent is zip-slip's second act: a member name
// with no ".." in it that lands outside the root because a directory along the
// way is a link. This is why a content jail is always strict.
func TestTargetRefusesASymlinkedParent(t *testing.T) {
	f := contentTree(t)
	symlinkOrSkip(t, f.outside, filepath.Join(f.install, "escape"))
	j := modpackJail(t, f)

	if _, err := j.Target("escape/evil.jar"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("Target through a symlink = %v, want ErrSymlink", err)
	}
}

// TestTargetRefusesAnInwardSymlinkToo pins strict mode. A link that stays
// inside the jail is still refused, because deciding per-link which escapes
// means reading a tar member's intent.
func TestTargetRefusesAnInwardSymlinkToo(t *testing.T) {
	f := contentTree(t)
	symlinkOrSkip(t, filepath.Join(f.install, "config"), filepath.Join(f.install, "cfg"))
	j := modpackJail(t, f)

	if !j.Strict() {
		t.Fatal("a content jail must be strict")
	}
	if _, err := j.Target("cfg/jei.ini"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("Target through an inward symlink = %v, want ErrSymlink", err)
	}
}

// TestAContentRootReachedThroughASymlinkStillWorks is the trap strict mode sets
// for its own author. If the jail were rooted at the CONFIGURED spelling rather
// than the resolved one, every target on a box whose /srv is a link -- or on
// any developer's temp directory -- would be refused for a reason that has
// nothing to do with the target, and the fix someone reaches for is to turn
// strict off.
func TestAContentRootReachedThroughASymlinkStillWorks(t *testing.T) {
	f := contentTree(t)
	link := filepath.Join(f.base, "link-to-install")
	symlinkOrSkip(t, f.install, link)

	j, err := New(Config{Allow: []string{link}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := j.Target("mods/jei.jar")
	if err != nil {
		t.Fatalf("Target under a symlinked content root = %v, want a path", err)
	}
	if want := filepath.Join(f.install, "mods", "jei.jar"); p.Path() != want {
		t.Errorf("Target = %s, want the resolved %s", p.Path(), want)
	}
}

func TestAProtectedWorldIsRefusedAndNeverForceable(t *testing.T) {
	f := contentTree(t)
	// The protected world is inside the content root here, which is the
	// arrangement that actually happens: install/world for Minecraft, and the
	// data tree for an adopted PZ server.
	j, err := New(Config{
		Allow:     []string{f.root},
		Protected: []string{f.saves},
		Preserve:  []string{f.props},
	})
	if err != nil {
		t.Fatal(err)
	}

	rel := filepath.Join("data", "Saves", "world", "map_p.bin")
	if _, err := j.Target(rel); !errors.Is(err, ErrProtected) {
		t.Fatalf("Target(%q) = %v, want ErrProtected", rel, err)
	}
	// And a protected path is not the force list's to promote. If it were,
	// `gvm content sync --force` would be a world delete with no archive.
	if _, err := j.Force([]string{f.saves}); !errors.Is(err, ErrProtected) {
		t.Fatalf("Force(world) = %v, want ErrProtected", err)
	}
}

// TestWritingAFileWhereAProtectedWorldLivesIsRefused covers the other
// direction of the containment test: the target is an ANCESTOR of a protected
// path, so writing a file there would take the world with it.
func TestWritingAFileWhereAProtectedWorldLivesIsRefused(t *testing.T) {
	f := contentTree(t)
	j, err := New(Config{Allow: []string{f.root}, Protected: []string{f.saves}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Target(filepath.Join("data", "Saves")); !errors.Is(err, ErrProtected) {
		t.Fatalf("Target(data/Saves) = %v, want ErrProtected", err)
	}
}

// TestAProtectedPathOutsideTheContentRootsIsAcceptedAndUnreachable documents
// the real configuration: the content roots are the mods directories and
// backup.world_paths lives in the data tree, so every protected path is normally
// outside every root. Refusing that would force the caller to pre-filter
// world_paths, and that filter is exactly where a world path that IS inside a
// content root would get dropped.
func TestAProtectedPathOutsideTheContentRootsIsAcceptedAndUnreachable(t *testing.T) {
	f := contentTree(t)
	j := modpackJail(t, f) // Protected: f.saves, which is outside install/ and downloads/

	if got := j.Protected(); len(got) != 1 || got[0] != f.saves {
		t.Fatalf("Protected() = %v, want [%s]", got, f.saves)
	}
	// Unreachable by containment, which is the belt the protection is braces
	// for: a target cannot name it at all.
	rel, err := filepath.Rel(f.install, f.saves)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Target(filepath.ToSlash(rel)); !errors.Is(err, ErrTraversal) {
		t.Fatalf("Target(%q) = %v, want ErrTraversal", rel, err)
	}

	// And the braces, in a subtest so that the half above still reports a
	// result on a host where symlinks need privilege.
	t.Run("a symlink into the world is not followed", func(t *testing.T) {
		symlinkOrSkip(t, f.saves, filepath.Join(f.install, "world-link"))
		if _, err := j.Target("world-link/map_p.bin"); err == nil {
			t.Fatal("a symlink from inside the content root into the world was followed")
		}
	})
}

func TestAPreservedPathIsRefusedUntilForced(t *testing.T) {
	f := contentTree(t)
	narrow := modpackJail(t, f)

	for _, rel := range []string{"server.properties", "world/level.dat", "world"} {
		if _, err := narrow.Target(rel); !errors.Is(err, ErrPreserved) {
			t.Fatalf("Target(%q) = %v, want ErrPreserved", rel, err)
		}
	}
	// The message has to name the way out, because the operator's next move is
	// a flag they have to be told about.
	_, err := narrow.Target("server.properties")
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the preserve refusal does not name --force: %v", err)
	}

	forced, err := narrow.Force([]string{f.props})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := forced.Target("server.properties"); err != nil {
		t.Fatalf("after --force, Target(server.properties) = %v, want a path", err)
	}
	// Forcing one preserved path does not free the others.
	if _, err := forced.Target("world/level.dat"); !errors.Is(err, ErrPreserved) {
		t.Fatalf("after forcing server.properties, Target(world/level.dat) = %v, "+
			"want ErrPreserved", err)
	}
	if got := forced.Forced(); len(got) != 1 || got[0] != f.props {
		t.Errorf("Forced() = %v, want [%s]", got, f.props)
	}
}

// TestForceReturnsANewJailAndLeavesTheNarrowOneNarrow is the immutability
// property. A widened jail that could leak back to a caller holding the narrow
// one would make every refusal above conditional on call order.
func TestForceReturnsANewJailAndLeavesTheNarrowOneNarrow(t *testing.T) {
	f := contentTree(t)
	narrow := modpackJail(t, f)

	forced, err := narrow.ForceAll()
	if err != nil {
		t.Fatal(err)
	}
	if forced == narrow {
		t.Fatal("ForceAll returned the same jail; widening mutated it")
	}
	if _, err := narrow.Target("server.properties"); !errors.Is(err, ErrPreserved) {
		t.Fatalf("the narrow jail was widened in place: %v", err)
	}
	for _, rel := range []string{"server.properties", "world/level.dat"} {
		if _, err := forced.Target(rel); err != nil {
			t.Errorf("after ForceAll, Target(%q) = %v, want a path", rel, err)
		}
	}
	if len(forced.Preserved()) != 0 {
		t.Errorf("after ForceAll, Preserved() = %v, want nothing left", forced.Preserved())
	}
	// Protection survives --force in every form.
	if _, err := forced.Target("world/../../elsewhere/evil"); err == nil {
		t.Error("a forced jail followed a traversal out of the tree")
	}
}

func TestForceRefusesAPathThisJailNeverHeldBack(t *testing.T) {
	f := contentTree(t)
	j := modpackJail(t, f)

	// Inside the jail, but not on the preserve list: if --force could name it,
	// --force would mean "widen to anything you can spell".
	cases := []string{
		filepath.Join(f.install, "mods"),
		f.outside,
	}
	for _, p := range cases {
		if _, err := j.Force([]string{p}); err == nil {
			t.Errorf("Force(%s) was accepted; the force set is the preserve list and "+
				"nothing else", p)
		}
	}
	if _, err := j.Force(nil); !errors.Is(err, ErrJail) {
		t.Error("Force(nil) must refuse: a force that promotes nothing would report success " +
			"and then refuse the same write")
	}
}

func TestTargetInAimsAtASecondRoot(t *testing.T) {
	f := contentTree(t)
	j := modpackJail(t, f)

	p, err := j.TargetIn(f.downloads, "oculus-1.20.1.jar")
	if err != nil {
		t.Fatalf("TargetIn(downloads) = %v", err)
	}
	if want := filepath.Join(f.downloads, "oculus-1.20.1.jar"); p.Path() != want {
		t.Errorf("TargetIn = %s, want %s", p.Path(), want)
	}
	// A root this jail does not have is refused, not silently joined.
	if _, err := j.TargetIn(f.outside, "evil.jar"); !errors.Is(err, ErrOutside) {
		t.Errorf("TargetIn(outside) = %v, want ErrOutside", err)
	}
	// A traversal out of a named root is refused BEFORE the join, because
	// filepath.Join cleans: "../install/world/level.dat" joined onto
	// downloads/ arrives as install/world/level.dat with no ".." left in it,
	// sitting inside a different content root. A jail that checked ".." only
	// after joining would have let this through as an ordinary target.
	if _, err := j.TargetIn(f.downloads, "../install/world/level.dat"); !errors.Is(err, ErrTraversal) {
		t.Errorf("TargetIn with a traversal into a sibling root = %v, want ErrTraversal", err)
	}
	if _, err := j.TargetIn(f.downloads, "../../../elsewhere/evil.jar"); !errors.Is(err, ErrTraversal) {
		t.Errorf("TargetIn with a traversal out of the tree = %v, want ErrTraversal", err)
	}
}

// TestTargetsIsAllOrNothingAndReportsEveryFailure: half an extracted modpack is
// a modpack nobody approved, and a plan that reveals one problem per run turns a
// review into a guessing game.
func TestTargetsIsAllOrNothingAndReportsEveryFailure(t *testing.T) {
	f := contentTree(t)
	j := modpackJail(t, f)

	got, err := j.Targets([]string{
		"mods/ok.jar",
		"../../etc/passwd",
		"/etc/shadow",
		"server.properties",
	})
	if err == nil {
		t.Fatal("Targets accepted a set containing three refusals")
	}
	if got != nil {
		t.Fatalf("Targets returned %d paths alongside an error; a partial set is worse than "+
			"either answer", len(got))
	}
	for _, want := range []error{ErrTraversal, ErrAbsolute, ErrPreserved} {
		if !errors.Is(err, want) {
			t.Errorf("the joined error does not carry %v, so one failure was swallowed: %v",
				want, err)
		}
	}

	// The all-good case still returns everything.
	ok, err := j.Targets([]string{"mods/a.jar", "mods/b.jar"})
	if err != nil || len(ok) != 2 {
		t.Fatalf("Targets(good) = %d paths, %v", len(ok), err)
	}
}

func TestNewRefusesAConfigurationThatProtectsNothing(t *testing.T) {
	f := contentTree(t)

	t.Run("no content root", func(t *testing.T) {
		if _, err := New(Config{}); !errors.Is(err, ErrJail) {
			t.Fatalf("New(nothing) = %v, want ErrJail", err)
		}
	})

	t.Run("a relative path on a survival list", func(t *testing.T) {
		// Resolved against a working directory, "world" names a different
		// place per process, which is the one spelling a survival list must
		// not accept.
		for _, cfg := range []Config{
			{Allow: []string{f.install}, Protected: []string{"world"}},
			{Allow: []string{f.install}, Preserve: []string{"server.properties"}},
			{Allow: []string{f.install}, Preserve: []string{"  "}},
		} {
			if _, err := New(cfg); !errors.Is(err, ErrJail) {
				t.Fatalf("New(%+v) = %v, want ErrJail", cfg, err)
			}
		}
	})

	t.Run("one path on both lists", func(t *testing.T) {
		_, err := New(Config{
			Allow:     []string{f.install},
			Protected: []string{f.world},
			Preserve:  []string{f.world},
		})
		if !errors.Is(err, ErrJail) {
			t.Fatalf("New(both lists) = %v, want ErrJail; the two readings disagree about "+
				"whether --force may write it", err)
		}
	})

	t.Run("a shallow content root", func(t *testing.T) {
		// internal/instance's rule, inherited rather than re-stated: no
		// instance owns a path that shallow.
		if _, err := New(Config{Allow: []string{filepath.FromSlash("/opt")}}); err == nil {
			t.Fatal("New(/opt) was accepted as a content root")
		}
	})
}

// TestOwnsRejectsAPathFromAnotherJail is the hole a write helper would open if
// it trusted any instance.Jailed it was handed. The blob store hands one out for
// every verified blob; writing a mod into the blob store because the type
// matched would be a jail that bounds nothing.
func TestOwnsRejectsAPathFromAnotherJail(t *testing.T) {
	f := contentTree(t)
	j := modpackJail(t, f)

	t.Run("the zero value", func(t *testing.T) {
		// The one forgery a composite literal in another package can write.
		if err := j.Owns(Path{}); !errors.Is(err, ErrNotOwned) {
			t.Fatalf("Owns(zero) = %v, want ErrNotOwned", err)
		}
		if err := j.WriteFile(Path{}, []byte("x"), FileMode); !errors.Is(err, ErrNotOwned) {
			t.Fatalf("WriteFile(zero) = %v, want ErrNotOwned", err)
		}
	})

	t.Run("another jail's path", func(t *testing.T) {
		other, err := instance.NewJail(instance.JailConfig{Allow: []string{f.outside}})
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := other.Resolve("blob")
		if err != nil {
			t.Fatal(err)
		}
		if err := j.Owns(theirs); !errors.Is(err, ErrNotOwned) {
			t.Fatalf("Owns(another jail's path) = %v, want ErrNotOwned", err)
		}
		if err := j.WriteFile(theirs, []byte("x"), FileMode); !errors.Is(err, ErrNotOwned) {
			t.Fatalf("WriteFile(another jail's path) = %v, want ErrNotOwned", err)
		}
		if _, err := os.Stat(theirs.Path()); err == nil {
			t.Fatal("the refused write created the file anyway")
		}
	})

	t.Run("a preserved path resolved before the jail narrowed", func(t *testing.T) {
		// A Path obtained from a forced jail must not be writable through the
		// narrow one: --force is per-jail, and a value that outlived its jail
		// would make it per-process.
		forced, err := modpackJail(t, f).ForceAll()
		if err != nil {
			t.Fatal(err)
		}
		p, err := forced.Target("server.properties")
		if err != nil {
			t.Fatal(err)
		}
		if err := j.Owns(p); !errors.Is(err, ErrNotOwned) {
			t.Fatalf("the narrow jail accepted a forced jail's path: %v", err)
		}
	})
}

func TestWriteFileIsAtomicAndLeavesNoBackup(t *testing.T) {
	f := contentTree(t)
	j := modpackJail(t, f)

	p, err := j.Target("config/gvm/blocked-mods.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.WriteFile(p, []byte(`{"blocked":3}`), FileMode); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"blocked":3}` {
		t.Errorf("WriteFile wrote %q", got)
	}

	// Rewritten, and still exactly one file: a .bak beside a generated file in
	// a directory the game scans is a second copy of something it may load.
	if err := j.WriteFile(p, []byte(`{"blocked":0}`), FileMode); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(p.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("after two writes the directory holds %v, want one file", names)
	}
}

func TestMkdirAllAndMkdirForStayInsideTheJail(t *testing.T) {
	f := contentTree(t)
	j := modpackJail(t, f)

	dir, err := j.Target("kubejs/server_scripts")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir.Path()); err != nil || !st.IsDir() {
		t.Fatalf("MkdirAll did not create a directory: %v", err)
	}

	file, err := j.Target("kubejs/client_scripts/x.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.MkdirFor(file); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Dir(file.Path())); err != nil || !st.IsDir() {
		t.Fatalf("MkdirFor did not create the parent: %v", err)
	}
	if _, err := os.Stat(file.Path()); err == nil {
		t.Error("MkdirFor created the file itself")
	}
}

// TestPathIsInstanceJailedItself is the "share it, do not clone it" assertion.
// If Path ever becomes its own struct, there are two definitions of a proven
// path in the program and a conversion between them at every seam -- which is
// the precise shape of the bug this package exists not to have.
func TestPathIsInstanceJailedItself(t *testing.T) {
	f := contentTree(t)
	j := modpackJail(t, f)
	p, err := j.Target("mods/a.jar")
	if err != nil {
		t.Fatal(err)
	}

	// Assignable in both directions with no conversion: that is what makes it
	// one type rather than two that happen to agree today.
	var asInstance instance.Jailed = p
	var asPath Path = asInstance
	if asPath.Path() != p.Path() {
		t.Fatal("Path and instance.Jailed disagree")
	}
	if rel := p.Rel(); rel != filepath.Join("mods", "a.jar") {
		t.Errorf("Rel() = %q", rel)
	}
	if p.Root() != f.install {
		t.Errorf("Root() = %q, want %q", p.Root(), f.install)
	}
}

func TestNilJailRefusesEverythingRatherThanPanicking(t *testing.T) {
	var j *Jail
	if _, err := j.Target("mods/a.jar"); !errors.Is(err, ErrJail) {
		t.Errorf("nil Target = %v", err)
	}
	if _, err := j.TargetIn("x", "y"); !errors.Is(err, ErrJail) {
		t.Errorf("nil TargetIn = %v", err)
	}
	if err := j.Owns(Path{}); !errors.Is(err, ErrJail) {
		t.Errorf("nil Owns = %v", err)
	}
	if _, err := j.Force([]string{"x"}); !errors.Is(err, ErrJail) {
		t.Errorf("nil Force = %v", err)
	}
	if _, err := j.ForceAll(); !errors.Is(err, ErrJail) {
		t.Errorf("nil ForceAll = %v", err)
	}
	if j.Primary() != "" || j.Roots() != nil || j.Strict() {
		t.Error("a nil jail claimed to have roots")
	}
	if !strings.Contains(j.String(), "no content jail") {
		t.Errorf("nil String() = %q", j.String())
	}
}
