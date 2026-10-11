package hook

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

func palEnv(t *testing.T) *env {
	t.Helper()
	return newEnv(t, "palworld.yaml", spec.VarValues{
		"server_name": "gvm", "max_players": "32",
	})
}

func sdtdEnv(t *testing.T) *env {
	t.Helper()
	return newEnv(t, "7daystodie.yaml", spec.VarValues{
		"server_name": "gvm", "game_world": "Navezgane", "world_gen_seed": "gvm",
	})
}

func TestPalSettingsSeed(t *testing.T) {
	e := palEnv(t)
	const shipped = "[/Script/Pal.PalGameWorldSettings]\nOptionSettings=(Difficulty=None)\n"
	e.write("install/DefaultPalWorldSettings.ini", shipped)

	if err := e.run("pal.settings_seed"); err != nil {
		t.Fatalf("pal.settings_seed: %v", err)
	}

	// The target is the path the MANIFEST declares, not a constant in the hook:
	// moving a config file in a manifest revision must not leave the seed
	// writing to the old location while everything else reads the new one.
	const target = "install/Pal/Saved/Config/LinuxServer/PalWorldSettings.ini"
	if got := e.read(target); got != shipped {
		t.Errorf("the seeded file is not the server's own default:\n%q", got)
	}
	// 0640 from the manifest, and handed to the instance account -- the server
	// rewrites this file when it exits, and a root-owned one means a world
	// whose settings silently revert.
	if !containsString(e.chowns, strings.TrimPrefix(target, "")+" 998:998") {
		t.Errorf("the seeded file was not handed to the instance account: %v", e.chowns)
	}
}

// TestASeedNeverOverwrites is what idempotent means for a seed: the target is a
// file the game and the operator both edit afterwards, so re-copying the default
// on every post_install would silently revert a server's settings the next time
// anybody ran a repair.
func TestASeedNeverOverwrites(t *testing.T) {
	e := palEnv(t)
	e.write("install/DefaultPalWorldSettings.ini", "OptionSettings=(Difficulty=None)\n")
	const target = "install/Pal/Saved/Config/LinuxServer/PalWorldSettings.ini"
	e.write(target, "OptionSettings=(Difficulty=Hard,ServerName=\"mine\")\n")

	if err := e.run("pal.settings_seed"); err != nil {
		t.Fatal(err)
	}
	if got := e.read(target); !strings.Contains(got, "mine") {
		t.Fatalf("the operator's settings were overwritten by the shipped default:\n%s", got)
	}
	if !strings.Contains(strings.Join(e.notes(), "\n"), "already there") {
		t.Errorf("notes = %v", e.notes())
	}
}

// TestASeedReportsAMissingShippedDefault rather than failing the phase: the
// install may be mid-repair, or this build of the game may ship the default
// somewhere else, and an aborted post_install would leave an instance that is
// otherwise installed in a failed state.
func TestASeedReportsAMissingShippedDefault(t *testing.T) {
	e := palEnv(t)
	if err := e.run("pal.settings_seed"); err != nil {
		t.Fatalf("err = %v, want a Problem and a clean phase", err)
	}
	joined := strings.Join(e.problems(), "\n")
	if !strings.Contains(joined, "DefaultPalWorldSettings.ini") {
		t.Fatalf("problems = %v", e.problems())
	}
	// And it says what the consequence is, because "file missing" alone does
	// not tell an operator that their server will boot with no name and no
	// password.
	if !strings.Contains(joined, "compiled-in default") {
		t.Errorf("the problem does not say what happens now: %v", e.problems())
	}
	if e.exists("install/Pal/Saved/Config/LinuxServer/PalWorldSettings.ini") {
		t.Error("an empty file was created in place of the seed")
	}
}

func TestASeedRefusesAnEmptyShippedDefault(t *testing.T) {
	e := palEnv(t)
	e.write("install/DefaultPalWorldSettings.ini", "")
	if err := e.run("pal.settings_seed"); err != nil {
		t.Fatal(err)
	}
	if len(e.problems()) == 0 {
		t.Fatal("an empty default was copied silently; the server would read it as a config " +
			"and boot with its compiled-in defaults")
	}
	if e.exists("install/Pal/Saved/Config/LinuxServer/PalWorldSettings.ini") {
		t.Error("an empty file was seeded")
	}
}

// TestSdtdSeedConfigCopiesOutOfTheInstallTree: keeping serverconfig.xml in the
// DATA tree is what stops a reinstall or a SteamCMD validate from overwriting
// an operator's settings, and nothing else puts it there.
func TestSdtdSeedConfigCopiesOutOfTheInstallTree(t *testing.T) {
	e := sdtdEnv(t)
	const shipped = "<?xml version=\"1.0\"?>\n<ServerSettings>\n" +
		"  <property name=\"ServerName\" value=\"My Game Host\"/>\n</ServerSettings>\n"
	e.write("install/serverconfig.xml", shipped)

	if err := e.run("sdtd.seed_config"); err != nil {
		t.Fatalf("sdtd.seed_config: %v", err)
	}
	if got := e.read("data/serverconfig.xml"); got != shipped {
		t.Errorf("the data-tree copy is not the shipped file:\n%q", got)
	}
	// And the source is untouched, so the next `validate` has something to
	// seed from again.
	if got := e.read("install/serverconfig.xml"); got != shipped {
		t.Error("the install tree's copy was modified")
	}
}

func TestSeedsRunWithNoFilesystem(t *testing.T) {
	for _, name := range []string{"pal.settings_seed", "sdtd.seed_config"} {
		h, err := Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Run(t.Context(), &Ctx{}); err == nil {
			t.Errorf("%s ran with no filesystem wired", name)
		}
	}
}
