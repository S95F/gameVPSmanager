package gamecfg

import (
	"errors"
	"strings"
	"testing"
)

// A realistic SandboxVars.lua: top-level scalars, nested tables, a string
// value, and the header comment the tooling writes above the table.
const sandboxLua = `-- Written by the pz scripts from preset apocalypse + sandbox.txt.
-- Edit config.env / sandbox.txt instead of this file.
SandboxVars = {
    VERSION = 5,
    Zombies = 4,
    Distribution = 1,
    DayLength = 4,
    WaterShutModifier = 14,
    ElecShutModifier = 14,
    Map = "Muldraugh, KY",
    StarterKit = false,
    ZombieConfig = {
        PopulationMultiplier = 0.65,
        PopulationStartMultiplier = 1.0,
        PopulationPeakMultiplier = 1.5,
        PopulationPeakDay = 28,
    },
    ZombieLore = {
        Speed = 4,
        Strength = 2,
        Toughness = 4,
        SprinterPercentage = 0,
    },
    MultiplierConfig = {
        Global = 1.0,
    },
}
`

func TestSandboxGetTopLevelAndNested(t *testing.T) {
	sv := ParseSandboxVars([]byte(sandboxLua))
	for _, tc := range []struct{ key, want string }{
		{"Zombies", "4"},
		{"DayLength", "4"},
		{"Map", `"Muldraugh, KY"`},
		{"StarterKit", "false"},
		{"ZombieConfig.PopulationMultiplier", "0.65"},
		{"ZombieConfig.PopulationPeakDay", "28"},
		{"ZombieLore.Speed", "4"},
		{"MultiplierConfig.Global", "1.0"},
	} {
		got, ok := sv.Get(tc.key)
		if !ok {
			t.Errorf("Get(%q): not found", tc.key)
			continue
		}
		if got != tc.want {
			t.Errorf("Get(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// Speed exists only inside ZombieLore. Addressing it without the section must
// not find it, or a "Speed = 2" override would silently edit a nested key the
// user did not name.
func TestSandboxScopingIsEnforced(t *testing.T) {
	sv := ParseSandboxVars([]byte(sandboxLua))

	if v, ok := sv.Get("Speed"); ok {
		t.Errorf("bare Speed resolved to %q; it exists only under ZombieLore", v)
	}
	if err := sv.Set("Speed", "2"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("Set(Speed) = %v, want ErrKeyNotFound", err)
	}
	// And a nested key must not be reachable through the wrong section.
	if _, ok := sv.Get("ZombieConfig.Speed"); ok {
		t.Error("ZombieConfig.Speed resolved; Speed lives under ZombieLore")
	}
}

// "Zombie" must not match the line "Zombies = 4".
func TestSandboxKeyPrefixCollision(t *testing.T) {
	sv := ParseSandboxVars([]byte(sandboxLua))
	if _, ok := sv.Get("Zombie"); ok {
		t.Error("key Zombie matched the line for Zombies")
	}
	if err := sv.Set("Zombie", "1"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("Set(Zombie) = %v, want ErrKeyNotFound", err)
	}
	if got, _ := sv.Get("Zombies"); got != "4" {
		t.Errorf("Zombies was modified: %q", got)
	}
}

func TestSandboxSetPreservesIndentAndFile(t *testing.T) {
	sv := ParseSandboxVars([]byte(sandboxLua))
	if err := sv.Set("ZombieConfig.PopulationMultiplier", "1.25"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	out := sv.String()

	if !strings.Contains(out, "        PopulationMultiplier = 1.25,") {
		t.Errorf("indentation or formatting lost:\n%s", out)
	}
	if got, _ := sv.Get("ZombieConfig.PopulationMultiplier"); got != "1.25" {
		t.Errorf("readback = %q, want 1.25", got)
	}
	// Everything else must be byte-identical.
	if strings.Count(out, "\n") != strings.Count(sandboxLua, "\n") {
		t.Error("line count changed")
	}
	for _, must := range []string{
		"-- Written by the pz scripts",
		"    Zombies = 4,",
		`    Map = "Muldraugh, KY",`,
		"        PopulationStartMultiplier = 1.0,",
		"    MultiplierConfig = {",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("line lost from file: %q", must)
		}
	}
}

// Replacing "ZombieConfig = {" with a scalar would orphan every key inside it
// and leave a file the game cannot load.
func TestSandboxNeverRewritesSectionHeader(t *testing.T) {
	sv := ParseSandboxVars([]byte(sandboxLua))
	if err := sv.Set("ZombieConfig", "5"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Set(ZombieConfig) = %v, want ErrKeyNotFound", err)
	}
	if !strings.Contains(sv.String(), "    ZombieConfig = {") {
		t.Error("the ZombieConfig section header was rewritten")
	}
}

// A brace inside a string value or a comment must not shift the depth for the
// rest of the file.
func TestSandboxBraceCountingIgnoresStringsAndComments(t *testing.T) {
	src := `SandboxVars = {
    Note = "a { brace } in a string",
    -- a { brace } in a comment
    Zombies = 4,
    ZombieConfig = {
        PopulationMultiplier = 0.65,
    },
}
`
	sv := ParseSandboxVars([]byte(src))
	if got, ok := sv.Get("Zombies"); !ok || got != "4" {
		t.Errorf("Zombies = %q ok=%v; depth tracking was thrown off by a brace in a string or comment", got, ok)
	}
	if got, ok := sv.Get("ZombieConfig.PopulationMultiplier"); !ok || got != "0.65" {
		t.Errorf("nested key = %q ok=%v", got, ok)
	}
}

func TestSandboxKeys(t *testing.T) {
	sv := ParseSandboxVars([]byte(sandboxLua))
	keys := sv.Keys()

	index := map[string]bool{}
	for _, k := range keys {
		index[k] = true
	}
	for _, want := range []string{
		"VERSION", "Zombies", "Map", "StarterKit",
		"ZombieConfig.PopulationMultiplier", "ZombieLore.SprinterPercentage",
		"MultiplierConfig.Global",
	} {
		if !index[want] {
			t.Errorf("Keys() missing %q", want)
		}
	}
	// Section headers are containers, not settable keys.
	for _, unwanted := range []string{"ZombieConfig", "ZombieLore", "MultiplierConfig"} {
		if index[unwanted] {
			t.Errorf("Keys() included the section header %q", unwanted)
		}
	}
}

func TestValidateLuaScalar(t *testing.T) {
	for _, ok := range []string{"4", "-1", "0.65", "1.0", "1e3", "-2.5e-3", "true", "false", "nil", `"Muldraugh, KY"`, `"a \" b"`} {
		if err := ValidateLuaScalar(ok); err != nil {
			t.Errorf("ValidateLuaScalar(%q) = %v, want nil", ok, err)
		}
	}
	// Each of these would produce a file the game cannot parse, and the
	// resulting Lua error surfaces nowhere near the cause.
	for _, bad := range []string{"", "4, Zombies = 1", "}", "{", `"unterminated`, "4\nZombies = 1", "os.exit()", "-- comment"} {
		if err := ValidateLuaScalar(bad); err == nil {
			t.Errorf("ValidateLuaScalar(%q) = nil, want an error", bad)
		}
	}
}

func TestSandboxSetRejectsUnsafeValue(t *testing.T) {
	sv := ParseSandboxVars([]byte(sandboxLua))
	before := sv.String()
	if err := sv.Set("Zombies", "4, StarterKit = true"); err == nil {
		t.Fatal("Set accepted an injection payload")
	}
	if sv.String() != before {
		t.Error("a rejected Set still modified the document")
	}
}

func TestParseOverrides(t *testing.T) {
	src := `# a comment line
Zombies = 4
ZombieConfig.PopulationMultiplier=0.65

  DayLength   =   4      # trailing comment
NotAnAssignment
EmptyValue =
= 5
Map = "Muldraugh, KY"
`
	got := ParseOverrides([]byte(src))
	want := []Override{
		{Key: "Zombies", Value: "4", Line: 2},
		{Key: "ZombieConfig.PopulationMultiplier", Value: "0.65", Line: 3},
		{Key: "DayLength", Value: "4", Line: 5},
		{Key: "Map", Value: `"Muldraugh, KY"`, Line: 9},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d overrides, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("override %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// One bad line must not stop the good ones from applying — that was the bash
// behaviour and it is the right one for an unattended restart cycle.
func TestApplyOverridesPartialSuccess(t *testing.T) {
	sv := ParseSandboxVars([]byte(sandboxLua))
	res := sv.ApplyOverrides(ParseOverrides([]byte(`
Zombies = 2
ZombieConfig.PopulationMultiplier = 1.5
TypoedKeyName = 3
Zombies2 = 9
DayLength = 6
`)))

	if len(res.Applied) != 3 {
		t.Errorf("applied %d, want 3: %+v", len(res.Applied), res.Applied)
	}
	if len(res.Skipped) != 2 {
		t.Errorf("skipped %d, want 2: %+v", len(res.Skipped), res.Skipped)
	}
	for _, s := range res.Skipped {
		if !errors.Is(s.Err, ErrKeyNotFound) {
			t.Errorf("skip reason for %q = %v, want ErrKeyNotFound", s.Key, s.Err)
		}
	}
	for _, tc := range []struct{ key, want string }{
		{"Zombies", "2"},
		{"ZombieConfig.PopulationMultiplier", "1.5"},
		{"DayLength", "6"},
	} {
		if got, _ := sv.Get(tc.key); got != tc.want {
			t.Errorf("Get(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestSandboxRoundTripUnchanged(t *testing.T) {
	sv := ParseSandboxVars([]byte(sandboxLua))
	if got := sv.String(); got != sandboxLua {
		t.Errorf("parse/render is not byte-identical.\ngot:\n%s\nwant:\n%s", got, sandboxLua)
	}
}

func TestSandboxEmptyInput(t *testing.T) {
	sv := ParseSandboxVars(nil)
	if got := sv.String(); got != "" {
		t.Errorf("empty input rendered %q", got)
	}
	if len(sv.Keys()) != 0 {
		t.Errorf("empty input yielded keys: %v", sv.Keys())
	}
	if err := sv.Set("Zombies", "4"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("Set on empty doc = %v, want ErrKeyNotFound", err)
	}
}

// Delete exists because Doc requires it. The guard that matters is the one it
// shares with Set: a line that opens a nested table is never a target, since
// removing "ZombieConfig = {" orphans every key inside it and the game then
// fails to load with a Lua error nowhere near the setting anyone changed.
func TestSandboxVarsDelete(t *testing.T) {
	in := readSample(t, "servertest_SandboxVars.lua")

	sv := ParseSandboxVars(in)
	if n := sv.Delete("ZombieConfig.PopulationPeakDay"); n != 1 {
		t.Fatalf("Delete removed %d lines; want 1", n)
	}
	if _, ok := sv.Get("ZombieConfig.PopulationPeakDay"); ok {
		t.Error("the key survived Delete")
	}
	if _, ok := sv.Get("ZombieConfig.PopulationMultiplier"); !ok {
		t.Error("Delete took a sibling with it")
	}

	sv = ParseSandboxVars(in)
	if n := sv.Delete("ZombieConfig"); n != 0 {
		t.Fatalf("Delete removed the line opening a nested table (%d lines); "+
			"that orphans every key inside it", n)
	}
	if string(sv.Bytes()) != string(in) {
		t.Error("a no-op Delete changed the file")
	}

	sv = ParseSandboxVars(in)
	if n := sv.Delete("NoSuchKey"); n != 0 {
		t.Errorf("Delete of an absent key reported %d", n)
	}
}
