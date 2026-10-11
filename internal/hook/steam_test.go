package hook

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// steamEnv is a Core Keeper instance: steamcmd, three steam hooks, and an
// install appid (1963720) that differs from the appid the server has to
// ANNOUNCE (1621690) -- which is the case steam.appid_shim exists for.
func steamEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, "corekeeper.yaml", spec.VarValues{
		"world_name": "gvm", "world_index": "0", "max_players": "8",
	})
	// The steamclient.so the sdk link points at. Without it the apply path
	// refuses the link, which is the behaviour the last subtest checks.
	fake := e.ctx.Links.(*fakeShimFS)
	fake.files["/opt/gvm/steamcmd/linux64/steamclient.so"] = []byte("elf")
	return e
}

func TestSteamHomeLayout(t *testing.T) {
	e := steamEnv(t)
	if err := e.run("steam.home_layout"); err != nil {
		t.Fatalf("steam.home_layout: %v", err)
	}
	fake := e.ctx.Links.(*fakeShimFS)

	home, _ := e.ctx.Fs.Path(RootHome)
	work, _ := e.ctx.Fs.Path(RootWork)
	for _, h := range []string{home, work} {
		data := h + "/.local/share/Steam"
		if !fake.dirs[data] {
			t.Errorf("%s was not created", data)
		}
		for _, link := range []string{"/.steam/root", "/.steam/steam"} {
			if got := fake.links[h+link]; got != data {
				t.Errorf("%s -> %q, want %q", h+link, got, data)
			}
		}
	}

	// BOTH homes, because steamcmd's confusion about ~/.steam affects its own
	// HOME and the dlopen affects the game's, and a layout applied to one of
	// them is the version of this bug that takes an evening to find.
	if home == work {
		t.Fatal("the fixture gave the game and steamcmd the same HOME, so the two-home case " +
			"is not being tested")
	}

	// This hook does NOT create the sdk links; steam.link_sdk does. A manifest
	// naming only one of them has to get only one of them.
	for p := range fake.links {
		if strings.Contains(p, "sdk") {
			t.Errorf("steam.home_layout created %s, which is steam.link_sdk's job", p)
		}
	}
}

func TestSteamLinkSDK(t *testing.T) {
	e := steamEnv(t)
	if err := e.run("steam.link_sdk"); err != nil {
		t.Fatalf("steam.link_sdk: %v", err)
	}
	fake := e.ctx.Links.(*fakeShimFS)
	home, _ := e.ctx.Fs.Path(RootHome)

	link := home + "/.steam/sdk64/steamclient.so"
	want := "/opt/gvm/steamcmd/linux64/steamclient.so"
	if got := fake.links[link]; got != want {
		t.Fatalf("%s -> %q, want %q. Without it the server prints [S_API FAIL] "+
			"SteamAPI_Init() failed and is invisible in the server browser", link, got, want)
	}

	// corekeeper.yaml's arch is native, so sdk32 is NOT linked: a 64-bit
	// engine never looks there, and `gvm doctor` lists these as facts about the
	// instance rather than as a superset that might be true.
	if _, ok := fake.links[home+"/.steam/sdk32/steamclient.so"]; ok {
		t.Error("sdk32 was linked for a 64-bit engine")
	}

	// And it does not re-create the layout.
	if _, ok := fake.links[home+"/.steam/root"]; ok {
		t.Error("steam.link_sdk created the HOME layout, which is steam.home_layout's job")
	}
}

// TestSteamLinkSDKReportsRatherThanCreatingADanglingLink: a link to a target
// that is not there is WORSE than no link, because the dlopen failure it
// produces looks identical to the missing-shim failure it was supposed to fix,
// while the real problem (steamcmd is not where gvm thinks) is one directory
// away.
func TestSteamLinkSDKReportsRatherThanCreatingADanglingLink(t *testing.T) {
	e := steamEnv(t)
	fake := e.ctx.Links.(*fakeShimFS)
	delete(fake.files, "/opt/gvm/steamcmd/linux64/steamclient.so")

	if err := e.run("steam.link_sdk"); err != nil {
		t.Fatalf("a missing target failed the phase instead of reporting: %v", err)
	}
	if len(e.problems()) == 0 {
		t.Fatal("nothing was reported")
	}
	if !strings.Contains(strings.Join(e.problems(), "\n"), "steamclient.so") {
		t.Errorf("the problem does not name the missing file: %v", e.problems())
	}
	for p := range fake.links {
		if strings.Contains(p, "sdk64") {
			t.Errorf("a dangling link was created anyway: %s", p)
		}
	}
}

// TestSteamShimsDoNotReplaceAFileGvmDidNotCreate: a real steamclient.so an
// operator copied in by hand works perfectly, and replacing it is not a hook's
// call to make. It is reported so doctor can show it, and the phase succeeds.
func TestSteamShimsDoNotReplaceAFileGvmDidNotCreate(t *testing.T) {
	e := steamEnv(t)
	fake := e.ctx.Links.(*fakeShimFS)
	home, _ := e.ctx.Fs.Path(RootHome)
	fake.files[home+"/.steam/sdk64/steamclient.so"] = []byte("a real library")

	if err := e.run("steam.link_sdk"); err != nil {
		t.Fatalf("err = %v, want a Problem and a clean phase", err)
	}
	joined := strings.Join(e.problems(), "\n")
	if !strings.Contains(joined, "will not replace a file it did not create") {
		t.Errorf("problems = %v", e.problems())
	}
	if string(fake.files[home+"/.steam/sdk64/steamclient.so"]) != "a real library" {
		t.Error("the operator's own file was replaced")
	}
}

// TestSteamShimsAreIdempotent is contract rule 2: every phase can re-run after
// a crash, and a repair re-runs post_install.
func TestSteamShimsAreIdempotent(t *testing.T) {
	e := steamEnv(t)
	for _, name := range []string{"steam.home_layout", "steam.link_sdk", "steam.appid_shim"} {
		if err := e.run(name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	before := e.read("install/steam_appid.txt")

	for _, name := range []string{"steam.home_layout", "steam.link_sdk", "steam.appid_shim"} {
		if err := e.run(name); err != nil {
			t.Fatalf("%s on a second run: %v", name, err)
		}
		if len(e.problems()) != 0 {
			t.Errorf("%s reported a problem on a second run: %v", name, e.problems())
		}
	}
	if got := e.read("install/steam_appid.txt"); got != before {
		t.Errorf("steam_appid.txt changed on a re-run: %q -> %q", before, got)
	}
	// The second appid run has to say it found the file correct rather than
	// writing it again: a .bak rotation per install run is noise in a tree an
	// operator reads.
	if !strings.Contains(strings.Join(e.notes(), "\n"), "already correct") {
		t.Errorf("the second run did not report the file as already correct: %v", e.notes())
	}
	if e.exists("install/steam_appid.txt.bak") {
		t.Error("the idempotent re-run rotated a .bak")
	}
}

func TestSteamAppIDShimWritesTheFallbackBesideTheBinary(t *testing.T) {
	e := steamEnv(t)
	if err := e.run("steam.appid_shim"); err != nil {
		t.Fatalf("steam.appid_shim: %v", err)
	}
	// corekeeper.yaml: install_appid 1963720, runtime.env SteamAppId 1621690.
	// The ANNOUNCED appid is the one that goes in the file; using the install
	// appid would give a server that starts and never appears in the browser.
	if got := e.read("install/steam_appid.txt"); strings.TrimSpace(got) != "1621690" {
		t.Errorf("steam_appid.txt = %q, want the announced appid 1621690", got)
	}
	// runtime.exec is ./CoreKeeperServer.x86_64, so the file belongs at the
	// tree's root: the Steam API reads it from the process's working directory.
	if e.exists("install/steam_appid.txt") == false {
		t.Error("the file is not beside the binary")
	}
	if !strings.Contains(strings.Join(e.notes(), "\n"), "SteamAppId=1621690") {
		t.Errorf("the env pair was not recorded: %v", e.notes())
	}
}

// TestSteamAppIDShimDoesNothingForAGameThatNeedsNoShim: valheim and corekeeper
// announce a different appid; generic-steam does not, and writing a
// steam_appid.txt holding the install appid would be a file that does nothing
// and a fact doctor would report as a shim.
func TestSteamAppIDShimDoesNothingForAGameThatNeedsNoShim(t *testing.T) {
	e := newEnv(t, "generic-steam.yaml", spec.VarValues{"public_name": "x"})
	if err := e.run("steam.appid_shim"); err != nil {
		t.Fatal(err)
	}
	if e.exists("install/steam_appid.txt") {
		t.Error("a steam_appid.txt was written for a game that announces its install appid")
	}
	if !strings.Contains(strings.Join(e.notes(), "\n"), "no shim is needed") {
		t.Errorf("notes = %v", e.notes())
	}
}

func TestSteamShimReportIsReadOnly(t *testing.T) {
	e := steamEnv(t)
	got, err := SteamShimReport(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("the report is empty")
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"SteamAppId=1621690", "sdk64", ".steam/root"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report does not mention %q:\n%s", want, joined)
		}
	}
	// Read-only: doctor calls this, and doctor must not change an instance.
	fake := e.ctx.Links.(*fakeShimFS)
	if len(fake.links) != 0 || len(fake.dirs) != 0 {
		t.Errorf("SteamShimReport wrote to the filesystem: %v %v", fake.links, fake.dirs)
	}
}

func TestSteamHooksRefuseAnInstanceWithNoHome(t *testing.T) {
	e := steamEnv(t)
	// A copy, with the jailed HOME blanked. The zero instance.Jailed is the
	// one forgery another package can write -- Paths{} compiles anywhere -- so
	// the hooks have to reject it rather than resolve a shim against "".
	bad := *e.ctx
	bad.Paths.Home = instance.Jailed{}
	h, err := Get("steam.home_layout")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Run(t.Context(), &bad); err == nil {
		t.Fatal("a shim plan was built for an instance with no HOME; the symlink would have " +
			"been created relative to nothing")
	}
}
