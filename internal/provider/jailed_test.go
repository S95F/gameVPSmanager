package provider

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// adoptedTree is the shape the live Project Zomboid box has: an install tree
// under /opt, a world under /home, and the two on different branches of the
// filesystem rather than inside one instance directory.
func adoptedTree(t *testing.T) spec.Paths {
	t.Helper()
	base := t.TempDir()
	p := spec.Paths{
		Install: filepath.Join(base, "opt", "pzserver"),
		Data:    filepath.Join(base, "home", "pzserver", "Zomboid"),
		Home:    filepath.Join(base, "home", "pzserver"),
		Work:    filepath.Join(base, "var", "lib", "gvm", "cache", "steam", "homes", "pz-main"),
	}
	for _, d := range []string{
		filepath.Join(p.Install, "steamapps"),
		filepath.Join(p.Install, "media"),
		filepath.Join(p.Data, "Saves", "Multiplayer", "servertest"),
		p.Work,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestNewPathsResolvesTheFourRoots(t *testing.T) {
	in := adoptedTree(t)
	p, err := NewPaths(in)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]instance.Jailed{
		"install": p.Install, "data": p.Data, "home": p.Home, "work": p.Work,
	} {
		if !got.Valid() {
			t.Errorf("%s did not resolve", name)
		}
	}
}

// Work is SteamCMD's per-instance HOME (D-30) and a non-Steam game has none.
// It is left invalid rather than defaulted, so a driver that needs it finds
// out by asking instead of writing into whatever the default happened to be.
func TestNewPathsLeavesAnAbsentWorkInvalid(t *testing.T) {
	in := adoptedTree(t)
	in.Work = ""
	p, err := NewPaths(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Work.Valid() {
		t.Error("an unset work path resolved to something")
	}
	if p.Work.Path() != "" {
		t.Errorf("an unset work path carries %q", p.Work.Path())
	}
}

func TestNewPathsRefusesTheRootsItCannotDoWithout(t *testing.T) {
	for _, missing := range []string{"install", "data", "home"} {
		t.Run(missing, func(t *testing.T) {
			in := adoptedTree(t)
			switch missing {
			case "install":
				in.Install = ""
			case "data":
				in.Data = ""
			case "home":
				in.Home = ""
			}
			if _, err := NewPaths(in); !errors.Is(err, ErrPaths) {
				t.Fatalf("want ErrPaths, got %v", err)
			}
		})
	}
}

func TestNewPathsRefusesAShallowOrRelativeRoot(t *testing.T) {
	cases := map[string]string{
		// No instance owns a path that shallow; /opt holds everybody's data.
		"a system directory": "/opt",
		// A root resolved against a working directory moves when the caller
		// does, and gvmd's working directory is not the operator's.
		"a relative path": "pzserver",
		"a traversal":     "/srv/gvm/../../etc",
	}
	for name, install := range cases {
		t.Run(name, func(t *testing.T) {
			in := adoptedTree(t)
			in.Install = install
			if _, err := NewPaths(in); !errors.Is(err, ErrPaths) {
				t.Fatalf("want ErrPaths, got %v", err)
			}
		})
	}
}

// Dest is zip-slip's second door. Artifact.Validate closed the lexical one at
// plan time; this one asks the filesystem.
func TestDestRefusesEveryEscapeFromTheInstallTree(t *testing.T) {
	in := adoptedTree(t)
	p, err := NewPaths(in)
	if err != nil {
		t.Fatal(err)
	}

	if got, err := p.Dest(filepath.Join("media", "lua", "shared", "x.lua")); err != nil {
		t.Fatalf("a legitimate destination was refused: %v", err)
	} else if !got.Valid() {
		t.Fatal("a resolved destination is invalid")
	}

	for name, rel := range map[string]string{
		"traversal":          filepath.Join("..", "..", "etc", "cron.d", "root"),
		"traversal mid-path": filepath.Join("media", "..", "..", "pzserver-old", "x"),
		"absolute":           filepath.FromSlash("/etc/shadow"),
		"empty":              "",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := p.Dest(rel)
			if err == nil {
				t.Fatalf("%q resolved to %s", rel, got.Path())
			}
			if got.Valid() || got.Path() != "" {
				t.Error("a refused destination still carried a path")
			}
		})
	}
}

// A symlink inside the install tree is refused even when it stays inside it:
// the paths this jail judges come out of third-party archives, and zip-slip's
// second act is a symlink member.
func TestDestRefusesAPathThroughASymlink(t *testing.T) {
	in := adoptedTree(t)
	link := filepath.Join(in.Install, "mods")
	if err := os.Symlink(filepath.Join(in.Install, "media"), link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("creating a symlink needs developer mode or admin on Windows")
		}
		t.Fatal(err)
	}
	p, err := NewPaths(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Dest(filepath.Join("mods", "x.jar"))
	if err == nil {
		t.Fatalf("a path through a symlink resolved to %s", got.Path())
	}
	if !errors.Is(err, instance.ErrSymlink) {
		t.Errorf("want instance.ErrSymlink, got %v", err)
	}
}

// The zero Paths is the one value another package can write, since the install
// jail is unexported. It must refuse rather than resolve against nothing.
func TestTheZeroPathsRefusesToResolve(t *testing.T) {
	var p Paths
	if _, err := p.Dest("x"); !errors.Is(err, ErrPaths) {
		t.Fatalf("want ErrPaths, got %v", err)
	}
}

func TestPopulatedAnswersHasAnythingBeenInstalledHere(t *testing.T) {
	in := adoptedTree(t)
	p, err := NewPaths(in)
	if err != nil {
		t.Fatal(err)
	}

	// install/ holds subdirectories but no files; "has an install started
	// here" is still yes, and it is deliberately NOT the same question as
	// "is this installed", which is the manifest's sentinel.
	got, err := Populated(p.Install)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("a tree with subdirectories reported empty")
	}

	empty := filepath.Join(t.TempDir(), "srv", "valheim-1", "install")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	j, err := NewPaths(spec.Paths{Install: empty, Data: empty, Home: empty})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Populated(j.Install); err != nil || got {
		t.Errorf("an empty tree reported populated=%v err=%v", got, err)
	}

	// A tree that does not exist yet is empty, not an error: a plan is checked
	// before the directory is created.
	absent, err := instance.NewJail(instance.JailConfig{Allow: []string{p.Install.Path()}})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := absent.Resolve("not-created-yet")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Populated(gone); err != nil || got {
		t.Errorf("an absent tree reported populated=%v err=%v", got, err)
	}

	if _, err := Populated(instance.Jailed{}); !errors.Is(err, ErrPaths) {
		t.Errorf("Populated accepted an unresolved path: %v", err)
	}
}

// The zero value of a struct of ints is uid 0, which is the one identity
// third-party code must never run as (D-16). "Nobody filled this in" has to be
// a refusal, not a privilege escalation.
func TestUnixIdentRefusesRootAndTheUnsetValue(t *testing.T) {
	if (UnixIdent{}).Valid() {
		t.Fatal("the zero UnixIdent is valid")
	}
	if got := (UnixIdent{}).String(); got != "<unset>" {
		t.Errorf("the zero UnixIdent prints as %q", got)
	}

	refusals := []struct {
		name        string
		user, group string
		uid, gid    int
	}{
		{"root uid", "root", "root", 0, 0},
		{"root gid", "gvm-pz-main", "root", 2001, 0},
		{"no user", "", "gvm-pz-main", 2001, 2001},
		{"no group", "gvm-pz-main", "", 2001, 2001},
		{"blank user", "   ", "gvm-pz-main", 2001, 2001},
		{"negative id", "gvm-pz-main", "gvm-pz-main", -1, 2001},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewUnixIdent(tc.user, tc.group, tc.uid, tc.gid); !errors.Is(err, ErrIdent) {
				t.Fatalf("want ErrIdent, got %v", err)
			}
		})
	}

	u, err := NewUnixIdent("gvm-pz-main", "gvm-pz-main", 2001, 2002)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Valid() || u.User() != "gvm-pz-main" || u.UID() != 2001 || u.GID() != 2002 {
		t.Errorf("round trip lost something: %+v", u)
	}
	if u.String() != "gvm-pz-main:gvm-pz-main (2001:2002)" {
		t.Errorf("unexpected rendering %q", u.String())
	}
}

func TestSentinelResultSaysWhichHalfFailed(t *testing.T) {
	cases := []struct {
		name string
		in   SentinelResult
		want string // substring; "" means no error
	}{
		{"undeclared", SentinelResult{}, "declares no install sentinel"},
		{"absent", SentinelResult{Declared: true, Rel: "start-server.sh"}, "is absent"},
		{
			"a directory where a file was declared",
			SentinelResult{Declared: true, Rel: "start-server.sh", Present: true, IsDir: true},
			"is a directory",
		},
		{
			// 40 GB of a 65 GB download is neither empty nor installed, and
			// the difference is a server that starts and immediately dies.
			"under the byte floor",
			SentinelResult{Declared: true, Rel: "ProjectZomboid64", Present: true,
				Size: 1024, MinBytes: 1 << 20},
			"a partial download",
		},
		{
			"present but not executable",
			SentinelResult{Declared: true, Rel: "start-server.sh", Present: true,
				Size: 2048, WantExecutable: true},
			"not executable",
		},
		{
			"satisfied",
			SentinelResult{Declared: true, Rel: "start-server.sh", Present: true, Size: 2048,
				WantExecutable: true, Executable: true, MinBytes: 1024},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.OK()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want satisfied, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want a refusal, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message %q does not mention %q", err, tc.want)
			}
		})
	}
}

// "Nobody looked" and "nothing was wrong" must never be the same value: that
// is the regression D-32 exists to prevent.
func TestIntegrityResultDistinguishesCleanFromUnchecked(t *testing.T) {
	unchecked := IntegrityResult{Reason: "this driver keeps no record"}
	if unchecked.Failed() {
		t.Error("an unchecked result reports a failure")
	}
	if unchecked.Checked {
		t.Error("an unchecked result claims it was checked")
	}
	clean := IntegrityResult{Checked: true, Facts: map[string]string{"StateFlags": "4"}}
	if clean.Failed() {
		t.Error("a clean result reports a failure")
	}
	dirty := IntegrityResult{Checked: true, Problems: []string{"StateFlags is 1062, not 4"}}
	if !dirty.Failed() {
		t.Error("a result with problems does not report a failure")
	}
}

func TestVersionSaysUnknownRatherThanEmpty(t *testing.T) {
	var v Version
	if v.Known() {
		t.Error("the zero Version claims to be known")
	}
	if v.String() != "unknown" {
		t.Errorf("the zero Version prints as %q", v.String())
	}
	if got := (Version{ID: "18452901"}).String(); got != "18452901" {
		t.Errorf("got %q", got)
	}
	if got := (Version{ID: "18452901", Label: "41.78.16"}).String(); got != "41.78.16 (18452901)" {
		t.Errorf("got %q", got)
	}
}

func TestProgressIsSilentWhenNobodyIsListening(t *testing.T) {
	// The zero value has to be usable: a provider calls these unconditionally,
	// including on its error paths, which is where a nil check gets forgotten.
	var p Progress
	p.Step("install")
	p.Bytes(1, 2)

	steps, bytesSeen := 0, int64(0)
	p = Progress{
		OnStep:  func(string) { steps++ },
		OnBytes: func(done, _ int64) { bytesSeen = done },
	}
	p.Step("install")
	p.Bytes(42, 100)
	if steps != 1 || bytesSeen != 42 {
		t.Errorf("steps=%d bytes=%d", steps, bytesSeen)
	}
}
