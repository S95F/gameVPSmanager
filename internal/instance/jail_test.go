package instance

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tree builds a real instance-shaped directory under t.TempDir() and returns
// the root. Real directories, not a fake filesystem: EvalSymlinks is the thing
// under test, and it only tells the truth about a filesystem that exists.
//
//	<root>/install/steamapps/
//	<root>/data/Server/
//	<root>/data/Saves/Multiplayer/servertest/
//	<root>/data/db/
//	<neighbour>/data/            a second instance's tree, beside this one
//	<outside>/                   somewhere the jail must never reach
func tree(t *testing.T) (root, neighbour, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "srv", "pz-main")
	neighbour = filepath.Join(base, "srv", "pz-main-old")
	outside = filepath.Join(base, "elsewhere")
	for _, d := range []string{
		filepath.Join(root, "install", "steamapps"),
		filepath.Join(root, "data", "Server"),
		filepath.Join(root, "data", "Saves", "Multiplayer", "servertest"),
		filepath.Join(root, "data", "db"),
		filepath.Join(neighbour, "data"),
		outside,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, neighbour, outside
}

// pzJail is the jail an adopted Project Zomboid instance gets: the whole
// instance tree is destroyable except the world and the survivors rebuild.sh
// kept (accounts, settings).
func pzJail(t *testing.T, root string) *Jail {
	t.Helper()
	j, err := NewJail(JailConfig{
		Allow: []string{root},
		Protected: []string{
			filepath.Join(root, "data", "Saves", "Multiplayer", "servertest"),
			filepath.Join(root, "data", "Server"),
			filepath.Join(root, "data", "db"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// symlinkOrSkip skips only the subtest that needs a symlink. Creating one on
// Windows needs SeCreateSymbolicLinkPrivilege or developer mode, and the rest
// of the jail's behaviour is testable without it.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks need privilege on this Windows host: %v", err)
		}
		t.Fatal(err)
	}
}

func TestResolveAcceptsWhatTheJailIsFor(t *testing.T) {
	root, _, _ := tree(t)
	j := pzJail(t, root)

	for _, rel := range []string{
		"install",
		"install/steamapps",
		"install/does-not-exist",       // a manifest glob may name a path this install never made
		filepath.Join(root, "install"), // absolute, inside
	} {
		got, err := j.Resolve(rel)
		if err != nil {
			t.Errorf("Resolve(%q) = %v, want a Jailed", rel, err)
			continue
		}
		if !got.Valid() {
			t.Errorf("Resolve(%q) returned an invalid Jailed", rel)
		}
		if !strings.HasPrefix(got.Path(), j.allow[0].real) {
			t.Errorf("Resolve(%q).Path() = %q, which is not under %q", rel, got.Path(), root)
		}
	}
}

// join builds a path WITHOUT cleaning it. filepath.Join cleans its result, so
// a fixture built with it has already had its ".." absorbed before the code
// under test sees the string -- which would make every case below pass for the
// wrong reason. A manifest glob is a raw string; so is this.
func join(elem ...string) string { return strings.Join(elem, string(filepath.Separator)) }

func TestResolveRejectsDotDotThatSurvivesClean(t *testing.T) {
	root, _, _ := tree(t)
	j := pzJail(t, root)

	cases := []struct {
		name string
		in   string
	}{
		{"leading, relative", "../pz-main-old/data"},
		{"buried, relative", "install/../../pz-main-old/data"},
		// filepath.Clean turns this into the neighbour's tree outright, so
		// the path that would be deleted is not the path that was written.
		{"absolute, cleaned into a sibling", join(root, "..", "pz-main-old", "data")},
		// At the filesystem root Clean DELETES the "..", which is how
		// "/../../etc" quietly becomes "/etc" -- the same trap the platform
		// package's cgroup path check was built around.
		{"absorbed at the root", join("", "..", "..", "etc")},
		{"bare", ".."},
		{"trailing", "install/.."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := j.Resolve(tc.in)
			if err == nil {
				t.Fatalf("Resolve(%q) succeeded", tc.in)
			}
			if !errors.Is(err, ErrTraversal) {
				t.Fatalf("Resolve(%q) = %v, want ErrTraversal", tc.in, err)
			}
		})
	}
}

// The rejection above is not theoretical: Clean really does rewrite these into
// paths inside a neighbouring instance's tree, and a jail that cleaned first
// would hand back a Jailed for somebody else's world.
func TestCleanWouldRewriteTraversalIntoANeighbourTree(t *testing.T) {
	root, neighbour, _ := tree(t)
	rewritten := filepath.Clean(join(root, "..", "pz-main-old", "data"))
	if rewritten != filepath.Join(neighbour, "data") {
		t.Fatalf("the fixture no longer exercises the trap: Clean gave %q, want %q",
			rewritten, filepath.Join(neighbour, "data"))
	}
	// And Clean at the filesystem root does not merely rewrite the path, it
	// deletes the traversal outright -- there is nothing left to detect after
	// the fact.
	if got := filepath.Clean(join("", "..", "..", "etc")); got != filepath.Clean(join("", "etc")) {
		t.Fatalf("Clean(/../../etc) = %q; the absorbed-at-the-root trap is no longer real", got)
	}
}

func TestResolveRejectsASymlinkWhoseTargetEscapes(t *testing.T) {
	root, _, outside := tree(t)
	link := filepath.Join(root, "install", "escape")
	symlinkOrSkip(t, outside, link)

	j := pzJail(t, root)
	_, err := j.Resolve("install/escape")
	if err == nil {
		t.Fatal("Resolve followed a symlink out of the jail and returned a Jailed")
	}
	if !errors.Is(err, ErrOutside) {
		t.Fatalf("Resolve = %v, want ErrOutside", err)
	}
	if !strings.Contains(err.Error(), "resolves to") {
		t.Errorf("the message does not say where the link went: %v", err)
	}
}

// A symlink that stays inside the jail is fine -- unless the jail is strict,
// which is the mode for paths that came from outside gvm.
func TestStrictModeRejectsEvenAnInwardSymlink(t *testing.T) {
	root, _, _ := tree(t)
	link := filepath.Join(root, "install", "inward")
	symlinkOrSkip(t, filepath.Join(root, "install", "steamapps"), link)

	lenient := pzJail(t, root)
	if _, err := lenient.Resolve("install/inward"); err != nil {
		t.Fatalf("a symlink that stays inside the jail was rejected by a lenient jail: %v", err)
	}

	strict, err := NewJail(JailConfig{Allow: []string{root}, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Resolve("install/inward"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("strict jail = %v, want ErrSymlink", err)
	}
}

func TestResolveRejectsAProtectedWorldPath(t *testing.T) {
	root, _, _ := tree(t)
	j := pzJail(t, root)

	world := "data/Saves/Multiplayer/servertest"
	for _, rel := range []string{
		world,                // the world itself
		world + "/map_p.bin", // and everything in it
		"data/Server",        // rebuild.sh's survivors
		"data/db",            // accounts and whitelist
	} {
		_, err := j.Resolve(rel)
		if !errors.Is(err, ErrProtected) {
			t.Errorf("Resolve(%q) = %v, want ErrProtected", rel, err)
		}
	}
}

// Deleting the parent of a world deletes the world. The prefix test has to run
// in both directions or "remove the data directory" quietly means "remove the
// saves inside it" -- which is the whole catastrophe the jail exists to stop.
func TestResolveRejectsAnAncestorOfAProtectedPath(t *testing.T) {
	root, _, _ := tree(t)
	j := pzJail(t, root)

	for _, rel := range []string{"data", "data/Saves", "data/Saves/Multiplayer", "."} {
		got, err := j.Resolve(rel)
		if err == nil {
			t.Errorf("Resolve(%q) = %s, but deleting it would take a protected path with it",
				rel, got)
			continue
		}
		if !errors.Is(err, ErrProtected) {
			t.Errorf("Resolve(%q) = %v, want ErrProtected", rel, err)
		}
	}
}

func TestResolveRejectsAnAbsolutePathOutsideEveryAllowRoot(t *testing.T) {
	root, neighbour, outside := tree(t)
	j := pzJail(t, root)

	for _, abs := range []string{
		outside,
		filepath.Join(neighbour, "data"),
		// The neighbour is a string prefix of nothing here, but the reverse
		// case is the one a naive HasPrefix gets wrong, so it is asserted in
		// TestUnderComparesWholePathElements below.
		filepath.Dir(root),
	} {
		_, err := j.Resolve(abs)
		if !errors.Is(err, ErrOutside) {
			t.Errorf("Resolve(%q) = %v, want ErrOutside", abs, err)
		}
	}
}

// The device assertion cannot be made off Linux -- st_dev has no portable
// equivalent -- so this test asserts the code path instead of the outcome:
// every other check still runs, and the skip is explicit rather than an
// accidental nil error.
func TestCrossDeviceCheckIsSkippedOffLinuxAndKeptOnIt(t *testing.T) {
	root, _, _ := tree(t)
	j := pzJail(t, root)

	got, err := j.Resolve("install")
	if err != nil {
		t.Fatalf("Resolve failed before reaching the device check: %v", err)
	}
	if got.Path() == "" {
		t.Fatal("Resolve returned an empty path")
	}

	// checkSameDevice is what Resolve calls; on Linux it answers, elsewhere it
	// reports the measurement as unavailable and the jail carries on.
	if err := checkSameDevice(j.allow[0].real, got.Path()); err != nil {
		t.Fatalf("a path inside its own allow root failed the device check: %v", err)
	}
	if runtime.GOOS == "linux" {
		// Two paths that are definitely on different filesystems on any Linux
		// box: /proc is a pseudo-filesystem, and the temp tree is not.
		if err := checkSameDevice(got.Path(), "/proc"); !errors.Is(err, ErrCrossDevice) {
			t.Fatalf("checkSameDevice(tmp, /proc) = %v, want ErrCrossDevice", err)
		}
	}
}

func TestResolveAllIsAllOrNothing(t *testing.T) {
	root, _, _ := tree(t)
	j := pzJail(t, root)

	good := []string{"install", "install/steamapps"}
	got, err := j.ResolveAll(good)
	if err != nil {
		t.Fatalf("ResolveAll(%v) = %v", good, err)
	}
	if len(got) != len(good) {
		t.Fatalf("ResolveAll returned %d paths, want %d", len(got), len(good))
	}

	mixed := []string{"install", "data/Server", "../escape", "install/steamapps"}
	got, err = j.ResolveAll(mixed)
	if err == nil {
		t.Fatal("ResolveAll accepted a set containing a protected path and a traversal")
	}
	if got != nil {
		t.Fatalf("ResolveAll returned %d paths alongside an error; a destroy over half a plan "+
			"is a destroy nobody approved", len(got))
	}
	// Both failures, not just the first: an operator reviewing a destroy plan
	// should not have to discover its problems one run at a time.
	if !errors.Is(err, ErrProtected) || !errors.Is(err, ErrTraversal) {
		t.Fatalf("ResolveAll reported %v; want both the protected path and the traversal", err)
	}
}

func TestRebuildIsTheOnlyWayToDestroyAWorld(t *testing.T) {
	root, _, _ := tree(t)
	j := pzJail(t, root)
	world := filepath.Join(root, "data", "Saves", "Multiplayer", "servertest")

	if _, err := j.Resolve(world); !errors.Is(err, ErrProtected) {
		t.Fatalf("the world is destroyable without a rebuild: %v", err)
	}

	rb, err := j.Rebuild([]string{world})
	if err != nil {
		t.Fatalf("Rebuild = %v", err)
	}
	got, err := rb.Resolve(world)
	if err != nil {
		t.Fatalf("the rebuilt jail still refuses the world it was built to destroy: %v", err)
	}
	if got.Path() != world {
		t.Errorf("Resolve = %q, want %q", got.Path(), world)
	}

	// The original is untouched. Rebuild constructs; it does not widen the
	// jail its caller was handed.
	if _, err := j.Resolve(world); !errors.Is(err, ErrProtected) {
		t.Fatal("Rebuild mutated the jail it was called on")
	}
	// And the survivors still survive: a rebuild takes the world, not the
	// accounts and settings.
	if _, err := rb.Resolve("data/Server"); !errors.Is(err, ErrProtected) {
		t.Fatalf("the rebuilt jail would destroy data/Server: %v", err)
	}
}

func TestRebuildRefusesToPromoteWhatTheJailDoesNotProtect(t *testing.T) {
	root, neighbour, outside := tree(t)
	j := pzJail(t, root)

	cases := []struct{ name, path string }{
		{"never protected", filepath.Join(root, "install")},
		{"another instance", filepath.Join(neighbour, "data")},
		{"outside entirely", outside},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := j.Rebuild([]string{tc.path}); !errors.Is(err, ErrJail) {
				t.Fatalf("Rebuild(%q) = %v; a rebuild that can name any path is not a jail",
					tc.path, err)
			}
		})
	}
	if _, err := j.Rebuild(nil); !errors.Is(err, ErrJail) {
		t.Fatalf("Rebuild(nil) = %v, want a refusal", err)
	}
}

func TestNewJailRefusesConfigurationsThatCannotBeSafe(t *testing.T) {
	root, _, _ := tree(t)

	cases := []struct {
		name string
		cfg  JailConfig
		want string
	}{
		{"no allow root", JailConfig{}, "no allow root"},
		{"relative allow root", JailConfig{Allow: []string{"srv/gvm/pz"}}, "not absolute"},
		{"traversal in a root", JailConfig{Allow: []string{join(root, "..", "x")}}, ".."},
		{"empty root", JailConfig{Allow: []string{"  "}}, "empty"},
		{
			"allow root inside a protected path",
			JailConfig{
				Allow:     []string{filepath.Join(root, "data", "Server")},
				Protected: []string{filepath.Join(root, "data")},
			},
			"could ever be destroyed",
		},
		{
			"allow root equal to a protected path",
			JailConfig{Allow: []string{root}, Protected: []string{root}},
			"could ever be destroyed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewJail(tc.cfg)
			if err == nil {
				t.Fatal("NewJail accepted it")
			}
			if !errors.Is(err, ErrJail) {
				t.Errorf("error does not wrap ErrJail: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NewJail = %q, which does not explain %q", err, tc.want)
			}
		})
	}
}

// The system-root rule is the one check that cannot be exercised with a temp
// directory on Windows, so the predicate is tested directly, with the
// slash-shaped paths a Linux host would hand it.
func TestIsSystemRootRefusesEveryRootAnInstanceCouldRuin(t *testing.T) {
	refuse := []string{
		"/", "/etc", "/etc/", "/home", "/opt", "/srv", "/usr", "/var", "/root",
		"/run", "/tmp", "/proc", "/sys", "/boot", "/dev", "/bin", "/sbin",
		"/lib", "/lib64", "/mnt", "/media",
		"/pzserver", // one element deep: somebody's whole install root
	}
	for _, p := range refuse {
		if !isSystemRoot(p) {
			t.Errorf("isSystemRoot(%q) = false; that directory is not one instance's to delete", p)
		}
	}
	accept := []string{
		"/srv/gvm/pz-main", "/opt/pzserver", "/home/pzserver/Zomboid",
		"/var/lib/gvm/instances/pz-main", "/etc/gvm/instances",
	}
	for _, p := range accept {
		if isSystemRoot(p) {
			t.Errorf("isSystemRoot(%q) = true; that is a legitimate instance path", p)
		}
	}
}

func TestUnderComparesWholePathElements(t *testing.T) {
	sep := string(filepath.Separator)
	base := sep + filepath.Join("srv", "gvm", "pz-main")
	cases := []struct {
		p, dir string
		want   bool
	}{
		{base, base, true},
		{filepath.Join(base, "data"), base, true},
		{filepath.Join(base, "data", "db"), base, true},
		// The one a bare HasPrefix gets wrong, and it costs a neighbouring
		// instance its entire tree.
		{base + "-old", base, false},
		{base + "-old" + sep + "data", base, false},
		{sep + filepath.Join("srv", "gvm"), base, false},
		{sep, base, false},
	}
	for _, tc := range cases {
		if got := under(tc.p, tc.dir); got != tc.want {
			t.Errorf("under(%q, %q) = %v, want %v", tc.p, tc.dir, got, tc.want)
		}
	}
}

func TestHasDotDotJudgesElementsNotSubstrings(t *testing.T) {
	yes := []string{"..", "../a", "a/..", "a/../b", `a\..\b`, "/a/../../etc", "..\\a"}
	for _, p := range yes {
		if !hasDotDot(p) {
			t.Errorf("hasDotDot(%q) = false", p)
		}
	}
	// A filename may contain dots without being a traversal, and refusing
	// "..bashrc" or "map..bin" would reject real files.
	no := []string{"a/..b", "..bashrc", "map..bin", "a/b..c/d", "...", "a/.../b", "."}
	for _, p := range no {
		if hasDotDot(p) {
			t.Errorf("hasDotDot(%q) = true, which would reject a real filename", p)
		}
	}
}

func TestResolveExistingToleratesAMissingTail(t *testing.T) {
	root, _, _ := tree(t)
	missing := filepath.Join(root, "install", "nope", "deeper", "still")

	resolved, existing, err := resolveExisting(missing)
	if err != nil {
		t.Fatalf("resolveExisting(%q) = %v; a jail that fails on a path the install never made "+
			"pushes its caller into skipping the check", missing, err)
	}
	if resolved != missing {
		t.Errorf("resolved = %q, want %q", resolved, missing)
	}
	wantExisting := filepath.Join(root, "install")
	if existing != wantExisting {
		t.Errorf("existing ancestor = %q, want %q (the deepest one that can be stat'ed)",
			existing, wantExisting)
	}
}

// The tolerant resolve must not lose the symlink hop that happens above the
// missing part of the path.
func TestResolveExistingResolvesTheExistingPrefixOfAMissingPath(t *testing.T) {
	root, _, outside := tree(t)
	link := filepath.Join(root, "install", "escape")
	symlinkOrSkip(t, outside, link)

	j := pzJail(t, root)
	_, err := j.Resolve("install/escape/not-created-yet")
	if !errors.Is(err, ErrOutside) {
		t.Fatalf("Resolve = %v, want ErrOutside: the missing tail hangs off a link that leaves "+
			"the jail", err)
	}
}

func TestJailedZeroValueCarriesNothing(t *testing.T) {
	var z Jailed
	if z.Valid() {
		t.Fatal("the zero Jailed reports itself as valid; a composite literal from another " +
			"package would be a forged proof")
	}
	if z.Path() != "" || z.Root() != "" || z.Rel() != "" {
		t.Errorf("the zero Jailed carries a path: %q %q %q", z.Path(), z.Root(), z.Rel())
	}
	if z.String() != "<unjailed>" {
		t.Errorf("String() = %q; a zero Jailed must not print as a plausible path", z.String())
	}
}

func TestJailedRelIsRelativeToItsRoot(t *testing.T) {
	root, _, _ := tree(t)
	j := pzJail(t, root)
	got, err := j.Resolve("install/steamapps")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("install", "steamapps"); got.Rel() != want {
		t.Errorf("Rel() = %q, want %q", got.Rel(), want)
	}
	if got.Root() != j.allow[0].real {
		t.Errorf("Root() = %q, want %q", got.Root(), j.allow[0].real)
	}
}

func TestAccessorsReturnCopies(t *testing.T) {
	root, _, _ := tree(t)
	j := pzJail(t, root)

	roots := j.AllowRoots()
	roots[0] = "/etc"
	if j.AllowRoots()[0] == "/etc" {
		t.Fatal("AllowRoots hands out the jail's own slice, so a caller can move a root")
	}
	prot := j.ProtectedPaths()
	if len(prot) != 3 {
		t.Fatalf("ProtectedPaths returned %d paths, want 3", len(prot))
	}
	prot[0] = "/etc"
	if j.ProtectedPaths()[0] == "/etc" {
		t.Fatal("ProtectedPaths hands out the jail's own slice")
	}
}

func TestANilJailResolvesNothing(t *testing.T) {
	var j *Jail
	if _, err := j.Resolve("install"); !errors.Is(err, ErrJail) {
		t.Fatalf("(*Jail)(nil).Resolve = %v, want ErrJail", err)
	}
	if j.Strict() {
		t.Error("a nil jail claims to be strict")
	}
	if _, err := j.Rebuild([]string{"x"}); !errors.Is(err, ErrJail) {
		t.Fatalf("(*Jail)(nil).Rebuild = %v, want ErrJail", err)
	}
	// The accessors are called from plan rendering and from doctor, which run
	// against whatever a caller has; a nil dereference there would turn a
	// missing jail into a crash instead of a refusal.
	if j.AllowRoots() != nil || j.ProtectedPaths() != nil {
		t.Error("a nil jail claims to have roots")
	}
}

func TestResolveRejectsAnEmptyPath(t *testing.T) {
	root, _, _ := tree(t)
	j := pzJail(t, root)
	for _, in := range []string{"", "   "} {
		if _, err := j.Resolve(in); !errors.Is(err, ErrOutside) {
			t.Errorf("Resolve(%q) = %v, want a refusal", in, err)
		}
	}
}

// Duplicated roots are a configuration accident (the same path rendered twice
// by two manifest fields), not an error worth failing an apply over.
func TestDuplicateRootsCollapse(t *testing.T) {
	root, _, _ := tree(t)
	j, err := NewJail(JailConfig{
		Allow:     []string{root, root, filepath.Join(root, "")},
		Protected: []string{filepath.Join(root, "data", "db"), filepath.Join(root, "data", "db")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(j.AllowRoots()) != 1 {
		t.Errorf("AllowRoots = %v, want one", j.AllowRoots())
	}
	if len(j.ProtectedPaths()) != 1 {
		t.Errorf("ProtectedPaths = %v, want one", j.ProtectedPaths())
	}
}

// Space, tab, CR and LF are all legal in a Linux filename, so "mods" and
// "mods " name two directories that can coexist. A Resolve that trims its
// argument hands back a proof aimed at an object the caller never named, and
// strict, containment and protection then all answer truthfully about the wrong
// one -- the single thing Jailed's doc comment promises cannot happen.
//
// The names here do not exist, so the whole resolution is lexical and the
// assertion holds identically on Windows, whose filesystem would otherwise eat
// the trailing space before Resolve could be blamed for it.
func TestResolveDoesNotTrimThePathItWasGiven(t *testing.T) {
	root, _, _ := tree(t)
	j := pzJail(t, root)

	for _, rel := range []string{"nope ", " nope", "\tnope", "nope\n"} {
		got, err := j.Resolve(rel)
		if err != nil {
			continue // refusing a padded name is defensible; retargeting is not
		}
		if want := filepath.Join(root, rel); got.Path() != want {
			t.Errorf("Resolve(%q).Path() = %q, want %q: the caller named one path and was "+
				"handed a proof for another", rel, got.Path(), want)
		}
	}
}

// The same defect where it costs data rather than only accuracy: two real
// directories whose names differ by one trailing space. The destroy set
// rendered from an archive's member list is exactly what the strict jail is
// for (section 12.8), and a trailing space survives both tar and zip.
func TestResolveDoesNotRewriteAWhitespacePaddedNameOntoItsNeighbour(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("NTFS cannot hold a directory whose name ends in a space")
	}
	root, _, _ := tree(t)
	for _, d := range []string{"mods", "mods "} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	j := pzJail(t, root)

	got, err := j.Resolve("mods ")
	if err != nil {
		return
	}
	if !strings.HasSuffix(got.Path(), "mods ") {
		t.Fatalf("Resolve(%q).Path() = %q: the caller named one directory and was handed a "+
			"proof for another, which still exists and is still full", "mods ", got.Path())
	}
}

// The shallow-root predicate has to be applied to the resolved spelling,
// because the resolved spelling is the one every later containment test uses.
func TestAnAllowRootIsJudgedShallowInBothSpellings(t *testing.T) {
	cases := []struct {
		name, given, real string
		wantRefusal       bool
	}{
		{"both deep", "/srv/gvm/pz-main", "/srv/gvm/pz-main", false},
		{"typed shallow", "/opt", "/opt", true},
		{"deep but resolves onto a system root", "/srv/gvm/pz-main", "/opt", true},
		{"resolves onto the filesystem root", "/srv/gvm/pz-main", "/", true},
		{"resolves one element deep", "/srv/gvm/pz-main", "/data", true},
		{"resolves deeper", "/opt/pzserver", "/var/opt/pzserver", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseShallowAllowRoot(tc.given, tc.real)
			if (err != nil) != tc.wantRefusal {
				t.Fatalf("refuseShallowAllowRoot(%q, %q) = %v, want refusal=%v",
					tc.given, tc.real, err, tc.wantRefusal)
			}
			if err == nil {
				return
			}
			if !errors.Is(err, ErrJail) {
				t.Errorf("error does not wrap ErrJail: %v", err)
			}
			if !strings.Contains(err.Error(), tc.given) {
				t.Errorf("refusal %q does not name the configured path %q", err, tc.given)
			}
			if tc.real != tc.given && !strings.Contains(err.Error(), tc.real) {
				t.Errorf("refusal %q does not name the resolved path %q", err, tc.real)
			}
		})
	}
}

// And the same rule through the constructor, against a real link: an allow root
// spelled three elements deep but symlinked at the filesystem root roots the
// jail at every application on the box, and every check after it passes.
func TestNewJailRefusesAnAllowRootThatSymlinksSomewhereShallow(t *testing.T) {
	base := t.TempDir()
	// The volume root rather than "/opt": it is the one target guaranteed to
	// exist on both platforms, and a dangling link resolves back to itself and
	// quietly tests nothing.
	shallow := filepath.VolumeName(base) + string(filepath.Separator)
	link := filepath.Join(base, "pz-main")
	symlinkOrSkip(t, shallow, link)

	if _, err := NewJail(JailConfig{Allow: []string{link}}); !errors.Is(err, ErrJail) {
		t.Fatalf("NewJail(%q -> %q) = %v; the jail is rooted at the filesystem root",
			link, shallow, err)
	}
}
