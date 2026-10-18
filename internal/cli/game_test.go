package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/games"
)

// brokenManifest writes a copy of a shipped manifest with one replacement
// applied, so a test can assert on a REAL file's diagnostics rather than on a
// hand-rolled fragment that shares none of its structure.
func brokenManifest(t *testing.T, id, old, new string) string {
	t.Helper()
	raw, err := fs.ReadFile(games.FS, id+".yaml")
	if err != nil {
		t.Fatalf("read %s.yaml: %v", id, err)
	}
	edited := strings.Replace(string(raw), old, new, 1)
	if edited == string(raw) {
		t.Fatalf("%s.yaml no longer contains %q", id, old)
	}
	path := filepath.Join(t.TempDir(), id+".yaml")
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestGameValidateReportsAShippedManifestClean(t *testing.T) {
	code, out, errb := run(t, "game", "validate", "projectzomboid")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	for _, want := range []string{
		"apiVersion gvm/v1, driverABI 1",
		"provider steamcmd app 380870",
		"control source_rcon on port rcon, dialect simple",
		"0 errors, 0 warnings.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not mention %q:\n%s", want, out)
		}
	}
}

// A game whose control kind this build has not implemented still validates:
// it must report as a warning, and a warning must not be exit-coded like an
// error. We use a patched copy of projectzomboid with websocket_rcon (still
// planned in this build) to exercise the path.
func TestGameValidateReportsAPlannedControlKindAsAWarning(t *testing.T) {
	path := brokenManifest(t, "projectzomboid", "kind: source_rcon", "kind: websocket_rcon")
	code, out, _ := run(t, "game", "validate", path)
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "0 errors, 1 warning.") {
		t.Errorf("count line is wrong:\n%s", out)
	}
	if !strings.Contains(out, `! `) || !strings.Contains(out, `"websocket_rcon" is known but not implemented`) {
		t.Errorf("the websocket_rcon warning is missing or not marked as a warning:\n%s", out)
	}
	if strings.Contains(out, "✗") {
		t.Errorf("a warning was rendered with the error marker:\n%s", out)
	}
}

// The whole value of `gvm game validate` is that it says WHERE. A diagnostic
// with no line makes the operator grep a 300-line file.
func TestGameValidatePositionsAnErrorInTheFile(t *testing.T) {
	path := brokenManifest(t, "projectzomboid", "dialect: simple", "dialect: valve_empty_response")

	code, out, _ := run(t, "game", "validate", path)
	if code == 0 {
		t.Fatalf("exit 0 on a manifest with an error:\n%s", out)
	}
	if !strings.Contains(out, `"valve_empty_response" is not a dialect`) {
		t.Errorf("the offending value is not named:\n%s", out)
	}
	if !strings.Contains(out, path+":") {
		t.Errorf("the diagnostic does not carry the file it is about:\n%s", out)
	}
	if !strings.Contains(out, "1 error, 0 warnings.") {
		t.Errorf("count line is wrong:\n%s", out)
	}
	if !strings.Contains(out, "✗ control source_rcon") {
		t.Errorf("the control check line still claims to have passed:\n%s", out)
	}
}

// An undeclared variable is the failure D-21 exists for: it does not crash, it
// launches with an empty argument.
func TestGameValidateCatchesAnUndeclaredTemplateVariable(t *testing.T) {
	path := brokenManifest(t, "valheim", "{{ .Var.world_name }}", "{{ .Var.wrold_name }}")

	code, out, _ := run(t, "game", "validate", path)
	if code == 0 {
		t.Fatalf("exit 0 on an undeclared variable:\n%s", out)
	}
	if !strings.Contains(out, "not declared in vars") {
		t.Errorf("the undeclared reference is not reported:\n%s", out)
	}
	if !strings.Contains(out, "did you mean .Var.world_name?") {
		t.Errorf("no suggestion for an obvious typo:\n%s", out)
	}
}

// Section 8.6 check 6 requires every key to pass its FORMAT's KeySyntax, not
// only the format-independent half internal/spec can run. A key no codec can
// address is not a crash at apply — SandboxVars.Set returns ErrKeyNotFound and
// the setting is simply never written — so if validate does not refuse it the
// operator learns about it from a server running its compiled-in defaults.
func TestGameValidateRefusesAKeyItsFormatCannotAddress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		old, new string
		want     string
	}{
		{
			name: "sandbox nesting",
			old:  "keys: {}",
			new:  `keys: {ZombieConfig.Foo.Bar: "1"}`,
			want: "more than two levels deep",
		},
		{
			name: "a sandbox segment that is not a Lua identifier",
			old:  "keys: {}",
			new:  `keys: {ZombieConfig.2Fast: "1"}`,
			want: `"2Fast" is not a Lua identifier`,
		},
		{
			name: "a deny key the format cannot name denies nothing",
			old:  "deny_keys: [SteamVAC, ServerPlayerID]",
			new:  "deny_keys: [SteamVAC, Steam=VAC]",
			want: `contains '='`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := brokenManifest(t, "projectzomboid", tc.old, tc.new)
			code, out, _ := run(t, "game", "validate", path)
			if code == 0 {
				t.Fatalf("exit 0 on a key the codec would refuse:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the codec's own message is missing:\n%s", out)
			}
			if !strings.Contains(out, "✗ 2 config files") {
				t.Errorf("the config-files line still claims to have passed:\n%s", out)
			}
			if !strings.Contains(out, "1 error, 0 warnings.") {
				t.Errorf("count line is wrong:\n%s", out)
			}
		})
	}
}

// One mistake, one message. An '=' is refused by BOTH halves of check 6 — the
// format-independent one in internal/spec and pz_ini's own — and a reader who
// gets two lines for one typo learns to skim the list.
func TestGameValidateReportsABadKeyOnce(t *testing.T) {
	path := brokenManifest(t, "projectzomboid", `UPnP:              "false"`, `UP=nP:             "false"`)

	code, out, _ := run(t, "game", "validate", path)
	if code == 0 {
		t.Fatalf("exit 0 on a key with an '=' in it:\n%s", out)
	}
	if !strings.Contains(out, "1 error, 0 warnings.") {
		t.Errorf("one mistake produced more than one diagnostic:\n%s", out)
	}
}

func TestGameValidateWithNoTargetIsAUsageError(t *testing.T) {
	code, out, errb := run(t, "game", "validate")
	if code == 0 {
		t.Errorf("exit 0 with nothing to validate")
	}
	if out != "" {
		t.Errorf("a usage error wrote a report to stdout: %q", out)
	}
	if !strings.Contains(errb, "Usage: gvm game validate") {
		t.Errorf("stderr = %q", errb)
	}
}

func TestGameValidateOnAnUnknownGameIDFails(t *testing.T) {
	code, _, errb := run(t, "game", "validate", "nosuchgame")
	if code == 0 {
		t.Error("exit 0 for a game that does not exist")
	}
	if !strings.Contains(errb, "nosuchgame") {
		t.Errorf("stderr does not name the id: %q", errb)
	}
}

func TestGameListNamesEveryShippedGame(t *testing.T) {
	// --all so the template (generic-steam) is included; the plain listing
	// hides it, which TestGameListHidesTemplates covers.
	code, out, errb := run(t, "game", "list", "--all")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	for _, id := range []string{
		"7daystodie", "7daystodie-telnet", "ark-se", "corekeeper",
		"cs2", "enshrouded", "generic-steam",
		"minecraft-forge", "minecraft-neoforge", "minecraft-paper",
		"palworld", "projectzomboid", "rust", "satisfactory", "valheim", "vrising",
	} {
		if !strings.Contains(out, id) {
			t.Errorf("%q is missing from the listing:\n%s", id, out)
		}
	}
	if !strings.Contains(out, "16 games") {
		t.Errorf("no count line:\n%s", out)
	}
	// Where a manifest came from is half the answer: three shadowing layers
	// with no way to see which one is in play is the 2am trap of section 8.7.
	if !strings.Contains(out, "embedded games/") {
		t.Errorf("the listing does not say where each manifest came from:\n%s", out)
	}
}

func TestGameWhichNamesTheWinnerAndItsDigest(t *testing.T) {
	code, out, errb := run(t, "game", "which", "valheim")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "WINNER") || !strings.Contains(out, "embedded games/valheim.yaml") {
		t.Errorf("no winner line:\n%s", out)
	}
	if !strings.Contains(out, "sha256 ") {
		t.Errorf("no digest to compare an approval against:\n%s", out)
	}
	if !strings.Contains(out, "revision 4") {
		t.Errorf("no revision, which is what a pin is compared by:\n%s", out)
	}
}

func TestGameWhichRejectsAnIDThatCouldEscapeTheGamesDirectory(t *testing.T) {
	code, _, errb := run(t, "game", "which", "../../etc/shadow")
	if code == 0 {
		t.Error("exit 0 for a traversing game id")
	}
	if !strings.Contains(errb, "invalid game id") {
		t.Errorf("stderr = %q", errb)
	}
}

func TestGameHooksListsTheClosedRegistries(t *testing.T) {
	code, out, errb := run(t, "game", "hooks")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "pz.first_boot") || !strings.Contains(out, "not run by this build") {
		t.Errorf("implemented hooks are missing, or the listing does not say they are not run:\n%s", out)
	}
	if !strings.Contains(out, "mc.accept_eula") {
		t.Errorf("mc hooks are missing:\n%s", out)
	}
	if !strings.Contains(out, "pz.server_name_is_world_dir") {
		t.Errorf("validators are missing:\n%s", out)
	}
}

func TestGameWithNoSubcommandShowsItsUsage(t *testing.T) {
	code, _, errb := run(t, "game")
	if code == 0 {
		t.Error("exit 0 with no subcommand")
	}
	if !strings.Contains(errb, "gvm game validate") {
		t.Errorf("stderr = %q", errb)
	}
}

func TestGameUnknownSubcommand(t *testing.T) {
	code, _, errb := run(t, "game", "frobnicate")
	if code == 0 {
		t.Error("exit 0 for an unknown subcommand")
	}
	if !strings.Contains(errb, `unknown subcommand "frobnicate"`) {
		t.Errorf("stderr = %q", errb)
	}
}
