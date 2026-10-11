package hook

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

func pzEnv(t *testing.T, vars spec.VarValues) *env {
	t.Helper()
	base := pzVars()
	for k, v := range vars {
		base[k] = v
	}
	return newEnv(t, "projectzomboid.yaml", base)
}

// TestPZFirstBootAsksForABootItCannotPerform is the reconciliation of contract
// rule 3 with a hook that genuinely needs the server to have run once.
func TestPZFirstBootAsksForABootItCannotPerform(t *testing.T) {
	e := pzEnv(t, nil)
	if err := e.run("pz.first_boot"); err != nil {
		t.Fatalf("pz.first_boot: %v", err)
	}

	req := e.ctx.BootRequested()
	if req == nil {
		t.Fatal("no boot was requested, so <servername>.ini would never be generated and " +
			"every owned key would have nothing to be written into")
	}
	// Both facts come from the MANIFEST, so a game whose marker or stop verb
	// changed is a manifest revision rather than a code change. Guessing either
	// means quitting a server mid-world-generation.
	if req.WaitFor != "SERVER STARTED" {
		t.Errorf("WaitFor = %q, want the manifest's log_marker", req.WaitFor)
	}
	if req.Then != "quit" {
		t.Errorf("Then = %q, want the manifest's stop verb console command", req.Then)
	}
	if !strings.Contains(req.Why, "servertest.ini") {
		t.Errorf("Why does not name the file the boot creates: %q", req.Why)
	}

	// The DIRECTORY is created and the file is not. PZ writes the .ini itself,
	// and an empty one would be read as a config: the server would boot with
	// its compiled-in defaults and write nothing back.
	if !e.exists("data/Server") {
		t.Error("data/Server was not created for the server to write into")
	}
	if e.exists("data/Server/servertest.ini") {
		t.Error("an empty .ini was created; the server would read it as a config")
	}
}

func TestPZFirstBootDoesNothingOnceTheIniExists(t *testing.T) {
	e := pzEnv(t, nil)
	e.write("data/Server/servertest.ini", "PublicName=x\n")
	if err := e.run("pz.first_boot"); err != nil {
		t.Fatal(err)
	}
	if e.ctx.BootRequested() != nil {
		t.Fatal("a boot was requested for an instance whose config already exists; every " +
			"restart would take an hour")
	}
	if !strings.Contains(strings.Join(e.notes(), "\n"), "no first boot is needed") {
		t.Errorf("notes = %v", e.notes())
	}
}

// TestPZFirstBootRefusesToGuessTheMarker: without the log marker the pipeline
// cannot tell when the boot has gone far enough, and quitting early means
// quitting mid-world-generation.
func TestPZFirstBootRefusesToGuessTheMarker(t *testing.T) {
	e := pzEnv(t, nil)
	e.ctx.Manifest.Spec.Readiness.Probes = nil

	if err := e.run("pz.first_boot"); err != nil {
		t.Fatalf("err = %v, want a Problem and a clean phase", err)
	}
	if e.ctx.BootRequested() != nil {
		t.Fatal("a boot was requested with no marker to wait for")
	}
	if !strings.Contains(strings.Join(e.problems(), "\n"), "log_marker") {
		t.Errorf("problems = %v", e.problems())
	}
}

const apocalypsePreset = `return {
	Zombies = 1,
	Distribution = 1,
	ZombieConfig = {
		PopulationMultiplier = 1.0,
	},
}
`

// liveSandbox is what the server itself generates: the same table under an
// assignment, with a key the preset above does not mention and one the preset
// has that this does not.
const liveSandbox = `SandboxVars = {
	Zombies = 4,
	Distribution = 2,
	WaterShutModifier = 14,
	ZombieConfig = {
		PopulationMultiplier = 0.65,
	},
}
`

func TestPZApplySandbox(t *testing.T) {
	e := pzEnv(t, spec.VarValues{
		"sandbox_preset":    "Apocalypse",
		"sandbox_overrides": "WaterShutModifier = 30\nZombieConfig.PopulationMultiplier = 0.5\n",
	})
	e.write("install/media/lua/shared/Sandbox/Apocalypse.lua", apocalypsePreset)
	e.write("install/media/lua/shared/Sandbox/SandboxVars.lua", apocalypsePreset)
	e.write("data/Server/servertest_SandboxVars.lua", liveSandbox)

	if err := e.run("pz.apply_sandbox"); err != nil {
		t.Fatalf("pz.apply_sandbox: %v", err)
	}
	got := e.read("data/Server/servertest_SandboxVars.lua")

	// The preset's keys were SET on the live document, not copied over it: the
	// live file is the one the server wrote from its CURRENT schema, so its
	// structure and the keys the preset does not mention have to survive.
	for _, want := range []string{
		"Zombies = 1",      // from the preset
		"Distribution = 1", // from the preset
		"WaterShutModifier = 30",
		"PopulationMultiplier = 0.5",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	// The document is still a SandboxVars assignment and not a `return` chunk,
	// which is what the game can load.
	if !strings.HasPrefix(got, "SandboxVars = {") {
		t.Errorf("the file is no longer a SandboxVars table:\n%s", got)
	}
}

// TestPZApplySandboxNeverInventsAKey: the game generates SandboxVars from its
// own schema and silently ignores a key it does not know, so an invented key
// looks like it worked and does nothing.
func TestPZApplySandboxNeverInventsAKey(t *testing.T) {
	e := pzEnv(t, spec.VarValues{
		"sandbox_overrides": "ZombiesPerSecond = 9\nZombies = 2\n",
	})
	e.write("data/Server/servertest_SandboxVars.lua", liveSandbox)

	if err := e.run("pz.apply_sandbox"); err != nil {
		t.Fatal(err)
	}
	got := e.read("data/Server/servertest_SandboxVars.lua")
	if strings.Contains(got, "ZombiesPerSecond") {
		t.Errorf("a key the live file does not contain was invented:\n%s", got)
	}
	if !strings.Contains(got, "Zombies = 2") {
		t.Errorf("the valid override did not apply, so one bad line stopped the rest:\n%s", got)
	}
	// Loud, because a silently dropped difficulty setting is a server that
	// plays differently than its operator thinks it does -- the regression from
	// sync-mods.sh's visible warning that D-21 names.
	joined := strings.Join(e.problems(), "\n")
	if !strings.Contains(joined, "ZombiesPerSecond") {
		t.Errorf("the skipped override was not reported: %v", e.problems())
	}
}

func TestPZApplySandboxRefusesAnUnknownPreset(t *testing.T) {
	e := pzEnv(t, spec.VarValues{"sandbox_preset": "Nightmare"})
	e.write("install/media/lua/shared/Sandbox/Apocalypse.lua", apocalypsePreset)
	e.write("install/media/lua/shared/Sandbox/Survivor.lua", apocalypsePreset)
	e.write("data/Server/servertest_SandboxVars.lua", liveSandbox)

	err := e.run("pz.apply_sandbox")
	if err == nil {
		t.Fatal("an unknown preset was accepted; the sandbox settings would silently stay at " +
			"whatever the server generated")
	}
	// The list comes from the install tree, so it is whatever this build ships.
	for _, want := range []string{"Nightmare", "Apocalypse", "Survivor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestPresetListExcludesTheSchema: PZ's own SandboxVars.lua lives in the same
// directory and is the schema, not a preset. Offering it as a choice would be
// offering "apply the defaults to themselves".
func TestPresetListExcludesTheSchema(t *testing.T) {
	e := pzEnv(t, nil)
	for _, n := range []string{"Apocalypse", "Survivor", "SandboxVars"} {
		e.write("install/media/lua/shared/Sandbox/"+n+".lua", apocalypsePreset)
	}
	got, err := PresetFiles(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("PresetFiles = %v, want the two presets and not the schema", got)
	}
	for _, f := range got {
		if strings.Contains(f, "SandboxVars") {
			t.Errorf("the schema file is offered as a preset: %s", f)
		}
	}
}

// TestPZApplySandboxSurvivesAFileTheServerHasNotGeneratedYet is the deadlock
// this hook has to avoid: pre_start runs BEFORE exec, and failing here would
// prevent the start that creates the file, so the settings would never apply
// because the file would never exist.
func TestPZApplySandboxSurvivesAFileTheServerHasNotGeneratedYet(t *testing.T) {
	e := pzEnv(t, spec.VarValues{"sandbox_overrides": "Zombies = 2\n"})
	if err := e.run("pz.apply_sandbox"); err != nil {
		t.Fatalf("err = %v; a pre_start failure here would block the start that creates the "+
			"file", err)
	}
	joined := strings.Join(e.problems(), "\n")
	if !strings.Contains(joined, "next start") {
		t.Errorf("the problem does not say when the settings will apply: %v", e.problems())
	}
}

// TestPZApplySandboxWritesNothingWhenNothingChanged: pre_start runs on every
// single start, and rewriting an identical file each time would churn the .bak
// and make the file's mtime useless for working out when a setting last changed.
func TestPZApplySandboxWritesNothingWhenNothingChanged(t *testing.T) {
	e := pzEnv(t, spec.VarValues{"sandbox_overrides": "Zombies = 4\n"})
	e.write("data/Server/servertest_SandboxVars.lua", liveSandbox)

	if err := e.run("pz.apply_sandbox"); err != nil {
		t.Fatal(err)
	}
	if e.exists("data/Server/servertest_SandboxVars.lua.bak") {
		t.Fatal("an identical document was rewritten")
	}
	if !strings.Contains(strings.Join(e.notes(), "\n"), "nothing was written") {
		t.Errorf("notes = %v", e.notes())
	}
}

func TestPresetToSandboxVars(t *testing.T) {
	got, err := PresetToSandboxVars([]byte(apocalypsePreset))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "SandboxVars = {") {
		t.Fatalf("the transform did not rewrite the assignment:\n%s", got)
	}
	// Exactly one token changes, so a diff against the shipped preset shows one
	// line and not a reindented file.
	if strings.Count(string(got), "SandboxVars = ") != 1 {
		t.Errorf("more than one line was rewritten:\n%s", got)
	}
	if !strings.Contains(string(got), "\tZombies = 1,") {
		t.Errorf("the body was reindented:\n%s", got)
	}

	// Indentation and CRLF survive, because an operator may well be reading
	// this file next to the original.
	crlf, err := PresetToSandboxVars([]byte("  return {\r\n\tZombies = 1,\r\n}\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(crlf), "  SandboxVars = {\r\n") {
		t.Errorf("indentation or line ending was lost: %q", crlf)
	}

	// A file with no `return {` is refused rather than yielding no keys, which
	// would read as "this preset changes nothing".
	if _, err := PresetToSandboxVars([]byte("-- just a comment\n")); err == nil {
		t.Error("a file that is not a preset was accepted")
	}
}

// TestPZResolveModIDs is the two-list model: workshop ids in, mod ids out, both
// written.
func TestPZResolveModIDs(t *testing.T) {
	e := pzEnv(t, nil)
	e.ctx.Content = NewContentState(map[string]spec.ContentValues{
		"workshop": {Acquire: []string{"2392709985", "2169435993", "9999999999"}},
	})
	e.write("data/Server/servertest.ini", "PublicName=x\nMods=\nWorkshopItems=\n")

	const wsRoot = "data/steamapps/workshop/content/108600/"
	// A dual-build item: the B42 variant lives in a 42/ directory and must win.
	e.write(wsRoot+"2392709985/mods/Brita/mod.info", "name=Brita\nid=BritaB41\n")
	e.write(wsRoot+"2392709985/mods/42/mod.info", "name=Brita\nid=Brita42\n")
	// A single-mod item that declares versionMin 42.
	e.write(wsRoot+"2169435993/mods/Tsar/mod.info", "name=Tsar\nid=Tsarslib\nversionMin=42.0.1\n")

	if err := e.run("pz.resolve_mod_ids"); err != nil {
		t.Fatalf("pz.resolve_mod_ids: %v", err)
	}

	ini := e.read("data/Server/servertest.ini")
	// Semicolons, from the manifest. A comma breaks both lists with NO error,
	// which is a popular way to disable every mod at once by accident.
	if !strings.Contains(ini, "Mods=Brita42;Tsarslib") {
		t.Errorf("Mods= is wrong:\n%s", ini)
	}
	// keep_missing: the id that has not downloaded stays listed, which is what
	// tells the server to retry the download on its next boot.
	if !strings.Contains(ini, "WorkshopItems=2392709985;2169435993;9999999999") {
		t.Errorf("WorkshopItems= dropped the missing item:\n%s", ini)
	}

	// The activation namespace is recorded in the lock as well as in the ini,
	// because both have to persist.
	if got := e.ctx.Content.Activation("workshop"); strings.Join(got, ";") != "Brita42;Tsarslib" {
		t.Errorf("the lock's activation list = %v", got)
	}
	if !e.ctx.Content.Changed() {
		t.Error("the lock was rewritten but not marked as changed")
	}

	// The B41 variant is reported as dropped, so a surprising exclusion can be
	// explained without re-running anything.
	if !strings.Contains(strings.Join(e.notes(), "\n"), "BritaB41") {
		t.Errorf("the dropped variant was not reported: %v", e.notes())
	}
	// And the item that has not downloaded is a problem, not a silence.
	if !strings.Contains(strings.Join(e.problems(), "\n"), "9999999999") {
		t.Errorf("the missing item was not reported: %v", e.problems())
	}
}

// TestPZResolveModIDsReportsAnItemWithNoBuildMarker: no evidence either way
// means every id inside was enabled and a Build 41 variant may have slipped in.
// Reporting it is sync-mods.sh's visible `check` warning, which D-21 says must
// not become silent data loss.
func TestPZResolveModIDsReportsAnItemWithNoBuildMarker(t *testing.T) {
	e := pzEnv(t, nil)
	e.ctx.Content = NewContentState(map[string]spec.ContentValues{
		"workshop": {Acquire: []string{"111"}},
	})
	e.write("data/Server/servertest.ini", "Mods=\nWorkshopItems=\n")
	e.write("data/steamapps/workshop/content/108600/111/mods/A/mod.info", "name=A\nid=AMod\n")

	if err := e.run("pz.resolve_mod_ids"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(e.problems(), "\n")
	if !strings.Contains(joined, "no build marker") {
		t.Fatalf("problems = %v", e.problems())
	}
	if !strings.Contains(joined, "AMod") {
		t.Errorf("the problem does not say which ids were enabled: %v", e.problems())
	}
	// Enabled anyway: the mod is probably fine, and refusing to list it would
	// disable a working mod over a missing marker.
	if !strings.Contains(e.read("data/Server/servertest.ini"), "Mods=AMod") {
		t.Error("an unmarked mod was dropped rather than enabled with a warning")
	}
}

// TestPZResolveModIDsLeavesModsAloneWhenNothingHasDownloaded: resolving to an
// empty Mods= would start the server with every mod disabled, and PZ then
// rewrites the ini on shutdown -- making the empty list permanent.
func TestPZResolveModIDsLeavesModsAloneWhenNothingHasDownloaded(t *testing.T) {
	e := pzEnv(t, nil)
	e.ctx.Content = NewContentState(map[string]spec.ContentValues{
		"workshop": {Acquire: []string{"111", "222"}},
	})
	e.write("data/Server/servertest.ini", "Mods=Brita;Tsarslib\nWorkshopItems=111;222\n")

	if err := e.run("pz.resolve_mod_ids"); err != nil {
		t.Fatal(err)
	}
	if got := e.read("data/Server/servertest.ini"); !strings.Contains(got, "Mods=Brita;Tsarslib") {
		t.Fatalf("Mods= was emptied:\n%s", got)
	}
	if !strings.Contains(strings.Join(e.problems(), "\n"), "could not be resolved") {
		t.Errorf("problems = %v", e.problems())
	}
}

// The same data loss, one directory later: the root EXISTS and holds nothing
// that resolves.
//
// workshopRoots skips a missing root with a note and keeps going, so a guard
// written as "every root is absent" is defeated by one surviving root -- and
// that is the ordinary case, not an edge one. A box whose workshop root holds
// yesterday's items reaches it on any mod-list replacement, because findItem
// stats the specific id subdirectory and nothing else.
func TestPZResolveModIDsLeavesModsAloneWhenTheRootResolvesNothing(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env)
	}{
		{
			name: "the root exists and is empty",
			setup: func(e *env) {
				e.mkdir("data/steamapps/workshop/content/108600")
			},
		},
		{
			name: "the root holds different items",
			setup: func(e *env) {
				e.write("data/steamapps/workshop/content/108600/777/mods/A/mod.info",
					"name=A\nid=AMod\nversionMin=42\n")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := pzEnv(t, nil)
			e.ctx.Content = NewContentState(map[string]spec.ContentValues{
				"workshop": {Acquire: []string{"111", "222"}, Activate: []string{"Brita", "Tsarslib"}},
			})
			e.write("data/Server/servertest.ini", "Mods=Brita;Tsarslib\nWorkshopItems=111;222\n")
			tc.setup(e)

			if err := e.run("pz.resolve_mod_ids"); err != nil {
				t.Fatal(err)
			}
			ini := e.read("data/Server/servertest.ini")
			if !strings.Contains(ini, "Mods=Brita;Tsarslib") {
				t.Fatalf("Mods= was emptied, which starts the server with every mod disabled; "+
					"PZ then rewrites the ini on shutdown and the loss is permanent:\n%s", ini)
			}
			// The acquisition half still has to be written: PZ downloads what
			// WorkshopItems= lists, so suppressing that write is what turns a
			// recoverable state into a stall.
			if !strings.Contains(ini, "WorkshopItems=111;222") {
				t.Errorf("WorkshopItems= was not written, so PZ is never told to download the "+
					"items the next resolve needs:\n%s", ini)
			}
			if got := e.ctx.Content.Activation("workshop"); strings.Join(got, ";") != "Brita;Tsarslib" {
				t.Errorf("the lock's activation list was cleared: %v", got)
			}
			if e.ctx.Content.Changed() {
				t.Error("the empty activation list was recorded as a change, so it would be " +
					"written back to the lock")
			}
			joined := strings.Join(e.problems(), "\n")
			for _, want := range []string{"Mods was left alone", "WorkshopItems still lists them"} {
				if !strings.Contains(joined, want) {
					t.Errorf("problems = %v, missing %q", e.problems(), want)
				}
			}
		})
	}
}

// And the guard must not block a deliberate "disable every mod": an empty
// acquisition list is intent, not a failed download.
func TestPZResolveModIDsStillEmptiesModsWhenEveryItemWasRemoved(t *testing.T) {
	e := pzEnv(t, nil)
	e.ctx.Content = NewContentState(map[string]spec.ContentValues{
		"workshop": {Acquire: nil, Activate: []string{"Brita", "Tsarslib"}},
	})
	e.write("data/Server/servertest.ini", "Mods=Brita;Tsarslib\nWorkshopItems=111;222\n")
	e.mkdir("data/steamapps/workshop/content/108600")

	if err := e.run("pz.resolve_mod_ids"); err != nil {
		t.Fatal(err)
	}
	ini := e.read("data/Server/servertest.ini")
	if !strings.Contains(ini, "Mods=\n") || !strings.Contains(ini, "WorkshopItems=\n") {
		t.Fatalf("an operator removing every mod was refused:\n%s", ini)
	}
	if got := e.ctx.Content.Activation("workshop"); len(got) != 0 {
		t.Errorf("the lock still lists %v", got)
	}
	if !e.ctx.Content.Changed() {
		t.Error("the lock was rewritten but not marked as changed")
	}
}

func TestPZResolveModIDsNeedsTheContentLock(t *testing.T) {
	e := pzEnv(t, nil)
	e.ctx.Content = nil
	err := e.run("pz.resolve_mod_ids")
	if err == nil {
		t.Fatal("the resolver ran with no lock to write activation ids into")
	}
	if !strings.Contains(err.Error(), "post_content") {
		t.Errorf("the error does not say where the lock is available: %v", err)
	}
}

// TestPZResolveModIDsWritesNothingTwiceForOneFile: both keys go into the same
// ini, and two commits would mean two atomic replaces and two .bak rotations
// for one logical change.
func TestPZResolveModIDsCommitsOncePerFile(t *testing.T) {
	e := pzEnv(t, nil)
	e.ctx.Content = NewContentState(map[string]spec.ContentValues{
		"workshop": {Acquire: []string{"111"}},
	})
	e.write("data/Server/servertest.ini", "Mods=\nWorkshopItems=\n")
	e.write("data/steamapps/workshop/content/108600/111/mods/A/mod.info",
		"name=A\nid=AMod\nversionMin=42\n")

	if err := e.run("pz.resolve_mod_ids"); err != nil {
		t.Fatal(err)
	}
	ini := e.read("data/Server/servertest.ini")
	if !strings.Contains(ini, "Mods=AMod") || !strings.Contains(ini, "WorkshopItems=111") {
		t.Fatalf("both keys were not written:\n%s", ini)
	}
	// The .bak holds the state BEFORE the hook ran. If each key had been
	// committed separately it would hold the half-written version.
	bak := e.read("data/Server/servertest.ini.bak")
	if strings.Contains(bak, "AMod") {
		t.Errorf("the ini was committed twice; the .bak holds a half-written file:\n%s", bak)
	}
}

// TestPZResolveModIDsIsIdempotent: post_content re-runs on every content cycle,
// and a lock rewritten each time has an mtime that means nothing.
func TestPZResolveModIDsIsIdempotent(t *testing.T) {
	e := pzEnv(t, nil)
	e.ctx.Content = NewContentState(map[string]spec.ContentValues{
		"workshop": {Acquire: []string{"111"}, Activate: []string{"AMod"}},
	})
	e.write("data/Server/servertest.ini", "Mods=AMod\nWorkshopItems=111\n")
	e.write("data/steamapps/workshop/content/108600/111/mods/A/mod.info",
		"name=A\nid=AMod\nversionMin=42\n")

	if err := e.run("pz.resolve_mod_ids"); err != nil {
		t.Fatal(err)
	}
	if e.ctx.Content.Changed() {
		t.Error("an unchanged activation list was recorded as a change")
	}
	if e.exists("data/Server/servertest.ini.bak") {
		t.Error("an identical ini was rewritten")
	}
}
