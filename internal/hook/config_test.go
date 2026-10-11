package hook

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// pzVars is enough of Project Zomboid's var set to render its config paths.
func pzVars() spec.VarValues {
	return spec.VarValues{
		"server_name":       "servertest",
		"pz_build":          "42",
		"sandbox_preset":    "",
		"sandbox_overrides": "",
	}
}

func TestConfigAddressesFilesByNameOnly(t *testing.T) {
	e := newEnv(t, "projectzomboid.yaml", pzVars())
	_, _, err := e.ctx.Config("/etc/passwd")
	if err == nil {
		t.Fatal("a hook addressed a file by path")
	}
	if !errors.Is(err, ErrConfig) {
		t.Errorf("error does not wrap ErrConfig: %v", err)
	}
	// It names what IS declared, which is the actionable half.
	if !strings.Contains(err.Error(), "server_ini") {
		t.Errorf("the error does not list the declared files: %v", err)
	}
}

// TestDenyKeysAreRefusedEvenToHooks is section 8.8's rule, which is only worth
// declaring if something enforces it.
func TestDenyKeysAreRefusedEvenToHooks(t *testing.T) {
	e := newEnv(t, "projectzomboid.yaml", pzVars())
	e.write("data/Server/servertest.ini", "PublicName=x\nSteamVAC=true\nMaxPlayers=8\n")

	doc, commit, err := e.ctx.Config("server_ini")
	if err != nil {
		t.Fatal(err)
	}
	// SteamVAC and ServerPlayerID are projectzomboid.yaml's deny_keys.
	err = doc.Set("SteamVAC", "false")
	if err == nil {
		t.Fatal("a hook wrote a denied key")
	}
	if !strings.Contains(err.Error(), "deny_keys") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	// Case does not save it: the ini format matches keys case-insensitively,
	// so a deny list that was case-sensitive would be bypassed by one letter.
	if err := doc.Set("steamvac", "false"); err == nil {
		t.Fatal("a denied key was writable in a different case")
	}
	if n := doc.Delete("SteamVAC"); n != 0 {
		t.Errorf("Delete removed %d occurrences of a denied key", n)
	}

	// A key gvm does own still works, and the denied line survives the write.
	if err := doc.Set("MaxPlayers", "12"); err != nil {
		t.Fatal(err)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}
	after := e.read("data/Server/servertest.ini")
	if !strings.Contains(after, "SteamVAC=true") {
		t.Errorf("the denied key was lost on write:\n%s", after)
	}
	if !strings.Contains(after, "MaxPlayers=12") {
		t.Errorf("the owned key was not written:\n%s", after)
	}
}

// TestAnOwnerServerFileIsRefusedWhileTheUnitIsActive: the game rewrites those
// on shutdown, so a hook's edit under a live server is a write that silently
// vanishes -- which looks exactly like gvm working.
func TestAnOwnerServerFileIsRefusedWhileTheUnitIsActive(t *testing.T) {
	e := newEnv(t, "projectzomboid.yaml", pzVars())
	e.write("data/Server/servertest.ini", "PublicName=x\n")

	e.active = true
	if _, _, err := e.ctx.Config("server_ini"); err == nil {
		t.Fatal("an owner:server file was opened while the server was running")
	}

	// And the check is repeated at commit, because a post_start hook opens the
	// file with the server starting and writes it with the server up.
	e.active = false
	doc, commit, err := e.ctx.Config("server_ini")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set("PublicName", "y"); err != nil {
		t.Fatal(err)
	}
	e.active = true
	err = commit()
	if err == nil {
		t.Fatal("the write landed under a running server")
	}
	if !strings.Contains(err.Error(), "started while the hook was working") {
		t.Errorf("the refusal does not explain the race: %v", err)
	}
	if got := e.read("data/Server/servertest.ini"); strings.Contains(got, "y") {
		t.Errorf("the refused write reached disk:\n%s", got)
	}
}

// TestNothingWiredToReportActiveIsTreatedAsRunning: assuming stopped would make
// the one dangerous case the default for a caller that forgot the check.
func TestNothingWiredToReportActiveIsTreatedAsRunning(t *testing.T) {
	e := newEnv(t, "projectzomboid.yaml", pzVars())
	e.write("data/Server/servertest.ini", "PublicName=x\n")

	open, err := NewConfigAccess(ConfigAccess{
		Manifest: e.ctx.Manifest, Render: e.ctx.Render, Fs: e.ctx.Fs, Active: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := open("server_ini"); err == nil {
		t.Fatal("an owner:server file was opened with nothing able to say whether the unit " +
			"was running")
	}
}

func TestAFileTheGameHasNotGeneratedYet(t *testing.T) {
	e := newEnv(t, "projectzomboid.yaml", pzVars())
	_, _, err := e.ctx.Config("server_ini")
	if err == nil {
		t.Fatal("a file that does not exist was opened")
	}
	// create_if_missing is false for this file, and the message has to say
	// whose job it is to create it.
	if !strings.Contains(err.Error(), "generate it first") {
		t.Errorf("the error does not say the game generates it: %v", err)
	}
}

func TestConfigRefusesAnUndeclaredFormatAndABadMode(t *testing.T) {
	e := newEnv(t, "projectzomboid.yaml", pzVars())
	e.write("data/Server/servertest.ini", "PublicName=x\n")

	// A mode that is not octal, taken straight through the real accessor by
	// rewriting the pinned manifest's own value -- which is what a bad manifest
	// would do.
	m := e.ctx.Manifest
	for i := range m.Spec.Configs {
		if m.Spec.Configs[i].Name == "server_ini" {
			m.Spec.Configs[i].Mode = "640x"
		}
	}
	if _, _, err := e.ctx.Config("server_ini"); err == nil {
		t.Fatal("a non-octal mode was accepted")
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]fs.FileMode{
		"":      0o640,
		"0640":  0o640,
		"640":   0o640,
		" 0600": 0o600,
		"0755":  0o755,
	} {
		got, err := parseMode(in)
		if err != nil {
			t.Fatalf("parseMode(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("parseMode(%q) = %04o, want %04o", in, got, want)
		}
	}
	// "640" being read as DECIMAL 640 would be mode 01200: setgid, setuid and
	// nothing else -- a config file nobody can read, from a missing leading
	// zero.
	if got, _ := parseMode("640"); got != 0o640 {
		t.Errorf("parseMode(\"640\") = %04o; it must be octal", got)
	}
	for _, bad := range []string{"640x", "0999", "-1", "0", "0007"} {
		if _, err := parseMode(bad); err == nil {
			t.Errorf("parseMode(%q) was accepted", bad)
		}
	}
}

func TestConfigTargetFollowsTheManifest(t *testing.T) {
	e := newEnv(t, "projectzomboid.yaml", pzVars())
	root, rel, mode, err := ConfigTarget(e.ctx, "server_ini")
	if err != nil {
		t.Fatal(err)
	}
	if root != RootData || rel != "Server/servertest.ini" {
		t.Errorf("ConfigTarget = %s/%s", root, rel)
	}
	if mode != 0o640 {
		t.Errorf("mode = %04o, want the manifest's 0640", mode)
	}

	// Unlike Ctx.Config, this answers for a file that is not there -- which is
	// the state a seeding hook runs in.
	if e.exists("data/Server/servertest.ini") {
		t.Fatal("the fixture created the file")
	}

	if _, _, _, err := ConfigTarget(e.ctx, "nope"); err == nil {
		t.Error("ConfigTarget answered for an undeclared file")
	}
	if _, _, _, err := ConfigTarget(&Ctx{}, "server_ini"); err == nil {
		t.Error("ConfigTarget answered with no manifest")
	}
}

func TestNewConfigAccessRefusesAnIncompleteSeam(t *testing.T) {
	e := newEnv(t, "projectzomboid.yaml", pzVars())
	for name, in := range map[string]ConfigAccess{
		"no manifest": {Render: e.ctx.Render, Fs: e.ctx.Fs},
		"no renderer": {Manifest: e.ctx.Manifest, Fs: e.ctx.Fs},
		"no fs":       {Manifest: e.ctx.Manifest, Render: e.ctx.Render},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewConfigAccess(in); err == nil {
				t.Fatal("NewConfigAccess accepted it")
			}
		})
	}
}

func TestAHookWithNoConfigAccessorSaysSo(t *testing.T) {
	h := &Ctx{}
	_, _, err := h.openConfig("server_ini")
	if err == nil {
		t.Fatal("openConfig answered with nothing wired")
	}
	if !strings.Contains(err.Error(), "NewConfigAccess") {
		t.Errorf("the error does not say what builds the accessor: %v", err)
	}
}
