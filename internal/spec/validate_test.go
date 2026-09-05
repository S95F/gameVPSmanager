package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wantDiag is the EXACT text `gvm game validate` prints for each fixture in
// testdata/bad, including the line and column.
//
// Golden-comparing the whole rendering rather than a substring is the point.
// Section 8.6 promises a position, and a position that quietly drifts to the
// wrong line is worse than none: the operator edits whatever the caret pointed
// at. The comparison covers the WHOLE Diags string, so a check that starts
// emitting a second diagnostic about one mistake fails here too — the
// suppression rule of this package, enforced rather than merely intended.
var wantDiag = map[string]string{
	"bad_regex.yaml":               "bad_regex.yaml:45:37: spec.readiness.probes[0].pattern: \"SERVER ((STARTED\" is not a valid RE2 pattern: missing closing )",
	"expose_bind_conflict.yaml":    "expose_bind_conflict.yaml:57:67: spec.ports[0].expose: expose: public contradicts bind: 127.0.0.1\n    a loopback-bound port is reachable only from this host; set expose: none, or drop the bind",
	"missing_sentinel.yaml":        "missing_sentinel.yaml:22:17: spec.provider.steamcmd.sentinel.path: a provider needs a sentinel path\n    the sentinel is how gvm knows an install finished; without one a half-downloaded tree looks installed",
	"missing_verify.yaml":          "missing_verify.yaml:23:15: spec.provider.steamcmd.verify: steamcmd needs verify.appmanifest and verify.byte_parity; byte_parity is not set\n    gvm never runs `validate` on the nightly update, so the post-download integrity check is the only thing left that catches a partial install (D-32)",
	"port_key_no_config_file.yaml": "port_key_no_config_file.yaml:58:20: spec.ports[0].config_key: port \"game\" writes \"DefaultPort\" but the manifest declares no config file\n    config_key is the ONLY way a port reaches a config file; declare the file under config_files, or drop the config_key",
	"port_key_twice.yaml":          "port_key_twice.yaml:61:20: spec.ports[1].config_key: \"DefaultPort\" in \"server_ini\" is already written by port \"game\" at spec.ports[0]\n    one key holds one value; the second allocation is opened in the firewall and written nowhere",
	"port_key_collision.yaml":      "port_key_collision.yaml:68:22: spec.config_files[0].keys.DefaultPort: \"DefaultPort\" is already written by port \"game\" via config_key\n    config_key is the ONLY way a port reaches a config file, so the allocator and the file cannot drift; delete this key",
	"readiness_no_deadline.yaml":   "readiness_no_deadline.yaml:42:5: spec.readiness.hard_deadline: hard_deadline is required and must be finite\n    it becomes TimeoutStartSec; without it a wedged start sits in `activating` forever and Restart=on-failure never fires (D-12)",
	"template_env.yaml":            "template_env.yaml:30:9: spec.runtime.args[1]: function \"env\" not defined\n    the complete function list is default join lower mib plusBytes quote upper",
	"template_if.yaml":             "template_if.yaml:30:9: spec.runtime.args[1]: {{if}} is not allowed: a manifest template has no control flow\n    spec.runtime.args is the only place the schema takes a `when:` guard, and it is checked at load time and shows up in the diff; a config key is a plain string, so render it unconditionally and use `default` for the empty case",
	"template_if_config_key.yaml":  "template_if_config_key.yaml:67:15: spec.config_files[0].keys.UPnP: {{if}} is not allowed: a manifest template has no control flow\n    spec.runtime.args is the only place the schema takes a `when:` guard, and it is checked at load time and shows up in the diff; a config key is a plain string, so render it unconditionally and use `default` for the empty case",
	"template_printf.yaml":         "template_printf.yaml:30:9: spec.runtime.args[1]: function \"printf\" is not on the allowlist\n    the complete function list is default join lower mib plusBytes quote upper",
	"template_range.yaml":          "template_range.yaml:29:9: spec.runtime.args[1]: {{range}} is not allowed: a manifest template has no control flow\n    spec.runtime.args is the only place the schema takes a `when:` guard, and it is checked at load time and shows up in the diff; a config key is a plain string, so render it unconditionally and use `default` for the empty case",
	"two_when_predicates.yaml":     "two_when_predicates.yaml:31:61: spec.runtime.args[2].when.var_set: when: var_set collides with phase (line 31); exactly one predicate may be set\n    split the arg in two, or nest one of them under `not:`",
	"undeclared_var.yaml":          "undeclared_var.yaml:30:9: spec.runtime.args[1]: template references .Var.wolrd_name, which is not declared in vars\n    did you mean .Var.world_name?",
	"unknown_hook.yaml":            "unknown_hook.yaml:63:39: spec.hooks.post_install[1]: \"steam.home_layot\" is not a hook (want mc.accept_eula|mc.gamerule_fallback|mc.select_jdk|mc.write_properties|pal.settings_seed|pz.apply_sandbox|pz.first_boot|pz.resolve_mod_ids|sdtd.seed_config|steam.appid_shim|steam.home_layout|steam.link_sdk)\n    did you mean \"steam.home_layout\"?",
	"validate_on_update.yaml":      "validate_on_update.yaml:21:38: spec.provider.steamcmd.validate_on[2]: `update` may not be on validate_on\n    SteamCMD's validate overwrites customised files; a nightly update that validates silently reverts every hand edit (D-32). Use `repair` for the operator-requested case",
}

func TestEachBadManifestProducesItsExactDiagnostic(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "bad", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	if len(files) != len(wantDiag) {
		t.Errorf("testdata/bad holds %d fixtures but there are %d goldens; every fixture needs one",
			len(files), len(wantDiag))
	}

	for _, f := range files {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			want, ok := wantDiag[name]
			if !ok {
				t.Fatalf("no golden for %s", name)
			}
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			_, diags, _ := ValidateBytes(name, raw)
			if got := diags.String(); got != want {
				t.Errorf("diagnostic mismatch\n got: %s\nwant: %s", got, want)
			}
			if !diags.HasErrors() {
				t.Errorf("a fixture in testdata/bad produced no error: %s", diags)
			}
		})
	}
}

// fixture decodes one testdata manifest, keeping its position index.
func fixture(t *testing.T, rel string) (*Manifest, *NodeIndex) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatal(err)
	}
	m, ix, diags, err := parseManifestIndexed(filepath.Base(rel), raw)
	if err != nil {
		t.Fatalf("%s: %v\n%s", rel, err, diags)
	}
	return m, ix
}

// paths lists the document paths a diagnostic set names, which is what most
// assertions below actually care about: WHICH node was condemned, not the
// wording, since the wording is golden-compared above.
func paths(ds Diags) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, string(d.Sev)+" "+d.Path)
	}
	return out
}

func TestTheShippedShapesValidateWithNoDiagnosticsAtAll(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "good", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no good fixtures: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			_, diags, err := ValidateBytes(filepath.Base(f), raw)
			if err != nil {
				t.Fatalf("did not load: %v", err)
			}
			// Warnings count. A manifest that ships with a permanent warning
			// teaches the reader that warnings are decoration.
			if len(diags) != 0 {
				t.Errorf("want a clean manifest, got:\n%s", diags)
			}
		})
	}
}

// TestAnEarlierCheckSilencesTheLaterOnesOnTheSameNode is the suppression rule.
// One mistake must not produce a paragraph: an unknown provider kind is one
// error, not that plus "provider kind steamcmd needs a steamcmd block" plus
// every later check that would have read the block.
func TestAnEarlierCheckSilencesTheLaterOnesOnTheSameNode(t *testing.T) {
	t.Run("unknown provider kind does not also fail the provider checks", func(t *testing.T) {
		m, ix := fixture(t, "good/minimal.yaml")
		m.Spec.Provider.Kind = "steamcmdd"
		m.Spec.Provider.SteamCMD = nil

		got := Validate(m, ix)
		if want := []string{"error spec.provider.kind"}; !equalStrings(paths(got), want) {
			t.Errorf("got %v, want %v\n%s", paths(got), want, got)
		}
	})

	t.Run("unknown control kind does not also fail the dialect check", func(t *testing.T) {
		m, ix := fixture(t, "good/rich.yaml")
		m.Spec.Control.Kind = "telent"
		m.Spec.Control.Dialect = "valve_empty_response"

		got := Validate(m, ix)
		if want := []string{"error spec.control.kind"}; !equalStrings(paths(got), want) {
			t.Errorf("got %v, want %v\n%s", paths(got), want, got)
		}
	})
}

// TestAKnownButUnimplementedNameIsAWarning is what lets 7daystodie.yaml ship in
// v1 declaring a telnet control channel: it loads, it validates, and the
// capability renders as unavailable with a reason (10.2).
func TestAKnownButUnimplementedNameIsAWarning(t *testing.T) {
	m, ix := fixture(t, "good/rich.yaml")
	m.Spec.Control.Kind = string(ControlTelnet)

	got := Validate(m, ix)
	if got.HasErrors() {
		t.Fatalf("a planned control kind must not be an error:\n%s", got)
	}
	if len(got) != 1 || got[0].Sev != SevWarning || got[0].Path != "spec.control.kind" {
		t.Fatalf("want one warning at spec.control.kind, got:\n%s", got)
	}
	if !strings.Contains(got[0].String(), "warning: ") {
		t.Errorf("a warning must be labelled so it does not read as an error: %s", got[0])
	}
}

func TestDialectDiagnosticReadsExactlyAsTheSpecPromises(t *testing.T) {
	m, ix := fixture(t, "good/rich.yaml")
	m.Spec.Control.Dialect = "valve_empty_response"

	got := Validate(m, ix)
	if len(got) != 1 {
		t.Fatalf("want one diagnostic, got:\n%s", got)
	}
	const want = `"valve_empty_response" is not a dialect (want source|minecraft|rust|simple)`
	if got[0].Msg != want {
		t.Errorf("got %q, want %q", got[0].Msg, want)
	}
	if got[0].Line == 0 || got[0].File == "" {
		t.Errorf("a diagnostic without a position makes the operator grep: %+v", got[0])
	}
}

func TestDriverABIRefusesAManifestFromTheFuture(t *testing.T) {
	m, ix := fixture(t, "good/minimal.yaml")
	m.Spec.DriverABI = BuiltDriverABI + 1

	got := Validate(m, ix)
	if !got.HasErrors() {
		t.Fatal("a driverABI this build does not implement must be refused; it is the axis that protects the RUNTIME")
	}
	if got[0].Path != "spec.driverABI" {
		t.Errorf("got %s, want spec.driverABI", got[0].Path)
	}
}

// TestASecretMayOnlyBeRenderedWhereItIsSafe pins check 4's placement rule. The
// NAME is deliberately unchecked -- a secret is declared in /etc/gvm/secrets,
// not in the manifest -- so placement is the whole of the protection.
func TestASecretMayOnlyBeRenderedWhereItIsSafe(t *testing.T) {
	for _, tc := range []struct {
		path  string
		allow bool
	}{
		{"spec.runtime.args[4]", true},
		{"spec.runtime.env.RCON_PASSWORD", true},
		{"spec.control.password", true},
		{"spec.verbs.login.control", true},
		{"spec.config_files[0].keys.Password", true},
		{"spec.config_files[12].keys.RCONPassword", true},

		{"metadata.doc", false},
		{"spec.readiness.probes[0].pattern", false},
		{"spec.backup.include[0]", false},
		{"spec.content[0].acquisition.write_key", false},
		{"spec.runtime.argsx", false},
		{"spec.config_files[0].path", false},
	} {
		if got := secretAllowedAt(tc.path); got != tc.allow {
			t.Errorf("secretAllowedAt(%q) = %v, want %v", tc.path, got, tc.allow)
		}
	}
}

func TestShutdownLadderMustFitInsideItsBudget(t *testing.T) {
	m, ix := fixture(t, "good/rich.yaml")
	m.Spec.Shutdown.Budget = Duration(60 * time.Second)

	got := Validate(m, ix)
	if len(got) != 1 || got[0].Path != "spec.shutdown.budget" {
		t.Fatalf("want one error at spec.shutdown.budget, got:\n%s", got)
	}
	// budget becomes TimeoutStopSec, so a ladder longer than the budget is a
	// SIGKILL mid-save on EVERY stop, not just an unlucky one.
	if !strings.Contains(got[0].Fix, "TimeoutStopSec") {
		t.Errorf("the fix must say why it matters: %q", got[0].Fix)
	}
}

func TestStallTimeoutAtOrAboveTheDeadlineCanNeverFire(t *testing.T) {
	m, ix := fixture(t, "good/minimal.yaml")
	m.Spec.Readiness.StallTimeout = m.Spec.Readiness.HardDeadline

	got := Validate(m, ix)
	if len(got) != 1 || got[0].Path != "spec.readiness.stall_timeout" {
		t.Fatalf("want one error at spec.readiness.stall_timeout, got:\n%s", got)
	}
}

func TestCrossCuttingFindingsAreWarningsAndDoNotStopAnApply(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Manifest)
		path string
	}{
		{
			name: "a control channel with no players verb",
			edit: func(m *Manifest) { delete(m.Spec.Verbs, "players") },
			path: "spec.verbs",
		},
		{
			name: "a world path nothing backs up",
			edit: func(m *Manifest) { m.Spec.Backup.Include = nil },
			path: "spec.backup.world_paths[0]",
		},
		{
			name: "a deprecated manifest",
			edit: func(m *Manifest) { m.Metadata.Deprecated = "use richgame2" },
			path: "metadata.deprecated",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ix := fixture(t, "good/rich.yaml")
			tc.edit(m)

			got := Validate(m, ix)
			if got.HasErrors() {
				t.Fatalf("a cross-cutting finding must be a warning, not an error:\n%s", got)
			}
			if len(got) == 0 || got[0].Path != tc.path {
				t.Fatalf("want a warning at %s, got:\n%s", tc.path, got)
			}
		})
	}
}

func TestAVarDefaultMustParseAsItsOwnType(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    VarSpec
		value   string
		wantErr bool
	}{
		{"a memory var takes a systemd size", VarSpec{Type: VarMemory}, "5g", false},
		{"a memory var refuses a bare word", VarSpec{Type: VarMemory}, "lots", true},
		{"an int var honours its declared minimum", VarSpec{Type: VarInt, Min: intp(1)}, "0", true},
		{"an int var honours its declared maximum", VarSpec{Type: VarInt, Max: intp(64)}, "65", true},
		{"a bool var takes the dashboard spellings", VarSpec{Type: VarBool}, "yes", false},
		{"a bool var refuses prose", VarSpec{Type: VarBool}, "yes please", true},
		{"a duration var refuses a unitless number", VarSpec{Type: VarDuration}, "300", true},
		{"an enum refuses a value not on its list", VarSpec{Type: VarEnum, Enum: []string{"41", "42"}}, "43", true},
		{"an enum sourced from presets defers to apply time", VarSpec{Type: VarEnum, EnumFrom: "presets"}, "Apocalypse", false},
		{"a declared pattern is applied to the value", VarSpec{Type: VarString, Pattern: "^[a-z]+$"}, "Mixed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkVarValue(tc.spec, tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkVarValue(%+v, %q) = %v, wantErr %v", tc.spec, tc.value, err, tc.wantErr)
			}
		})
	}
}

func TestValidateIsUsableWithoutAPositionIndex(t *testing.T) {
	// `gvm apply` validates the PINNED manifest, which arrives as bytes nothing
	// indexed. The diagnostics must still name their path.
	m, _ := fixture(t, "good/minimal.yaml")
	m.Metadata.Revision = 0

	got := Validate(m, nil)
	if len(got) != 1 || got[0].Path != "metadata.revision" {
		t.Fatalf("want one error at metadata.revision, got:\n%s", got)
	}
	if got[0].Line != 0 || got[0].File != "" {
		t.Errorf("with no index there is no position to invent: %+v", got[0])
	}
	if !strings.Contains(got[0].String(), "metadata.revision: ") {
		t.Errorf("an unpositioned diagnostic still has to say what it is about: %s", got[0])
	}
}

func TestNilManifestValidatesToNothing(t *testing.T) {
	if got := Validate(nil, nil); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestNearestNameOnlySuggestsWhenItIsWorthTrusting(t *testing.T) {
	names := []string{"server_name", "jvm_xmx", "public_name"}
	if got := nearestName("server_nmae", names); got != "server_name" {
		t.Errorf("got %q, want server_name", got)
	}
	// A suggestion that is merely the closest of three unrelated words sends the
	// reader to edit a line that was never the problem.
	if got := nearestName("zzzzzzzzzz", names); got != "" {
		t.Errorf("got %q, want no suggestion", got)
	}
}

func intp(n int) *int { return &n }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mutate loads a fixture's bytes and rewrites one line of it, so a test can
// assert a POSITIONED diagnostic without another file in testdata.
func mutate(t *testing.T, rel, old, new string) (string, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, old) {
		t.Fatalf("%s does not contain %q", rel, old)
	}
	return filepath.Base(rel), []byte(strings.Replace(s, old, new, 1))
}

// TestASecretRenderedOutsideItsSurfaceIsRefusedWithAPosition is the end-to-end
// half of check 4. A backup include is not on the list, and it lands in an
// archive manifest and in `gvm apply --diff`, where taint tracking cannot help
// because the value should never have been there.
func TestASecretRenderedOutsideItsSurfaceIsRefusedWithAPosition(t *testing.T) {
	name, raw := mutate(t, "good/minimal.yaml",
		`    include: ["{{ .Data }}/worlds"]`,
		`    include: ["{{ .Data }}/{{ .Secret.rcon_password }}"]`)

	_, diags, err := ValidateBytes(name, raw)
	if err != nil {
		t.Fatal(err)
	}
	var found *Diag
	for i := range diags {
		if diags[i].Path == "spec.backup.include[0]" && diags[i].Sev == SevError {
			found = &diags[i]
		}
	}
	if found == nil {
		t.Fatalf("a secret in backup.include must be refused, got:\n%s", diags)
	}
	if found.Line == 0 {
		t.Errorf("the refusal must carry the manifest's own line: %+v", *found)
	}
}

// TestAConsoleShutdownStepNeedsAConsole: the ladder is what stops a live server,
// so a rung on a channel the game does not have is a rung that always times out.
func TestAConsoleShutdownStepNeedsAConsole(t *testing.T) {
	m, ix := fixture(t, "good/rich.yaml")
	m.Spec.Runtime.Console.Mode = ConsoleModeNone

	got := Validate(m, ix)
	if !got.HasErrors() {
		t.Fatalf("want errors for the console rungs, got:\n%s", got)
	}
	for _, want := range []string{
		"spec.shutdown.steps[0].via",
		"spec.shutdown.steps[1].via",
		"spec.backup.freeze.before.via",
	} {
		if !hasPath(got, want) {
			t.Errorf("no diagnostic at %s:\n%s", want, got)
		}
	}
}

func TestAControlShutdownStepNeedsAControlChannel(t *testing.T) {
	m, ix := fixture(t, "good/minimal.yaml")
	m.Spec.Shutdown.Steps = []ShutdownStep{{Via: ViaControl, Send: "quit", Wait: Duration(5 * time.Second)}}

	got := Validate(m, ix)
	if len(got) != 1 || got[0].Path != "spec.shutdown.steps[0].via" {
		t.Fatalf("want one error at spec.shutdown.steps[0].via, got:\n%s", got)
	}
}

// TestAFreezeWithNoThawIsRefused: dying between save-off and save-on leaves a
// running server that has silently stopped persisting, which is the worst
// failure in the system (D-31).
func TestAFreezeWithNoThawIsRefused(t *testing.T) {
	m, ix := fixture(t, "good/rich.yaml")
	m.Spec.Backup.Freeze.During = &ShutdownStep{Via: ViaConsole, Send: "save-off"}
	m.Spec.Backup.Freeze.After = nil

	got := Validate(m, ix)
	if !hasPath(got, "spec.backup.freeze.during") {
		t.Fatalf("want an error at spec.backup.freeze.during, got:\n%s", got)
	}
}

func TestAnUnknownSignalIsRefusedBecauseValheimSavesOnSIGINT(t *testing.T) {
	m, ix := fixture(t, "good/minimal.yaml")
	m.Spec.Shutdown.Steps[0].Signal = "SIGSAVE"

	got := Validate(m, ix)
	if len(got) != 1 || got[0].Path != "spec.shutdown.steps[0].signal" {
		t.Fatalf("want one error at spec.shutdown.steps[0].signal, got:\n%s", got)
	}
}

func TestAProbeMayOnlyNameAPortThatExists(t *testing.T) {
	m, ix := fixture(t, "good/rich.yaml")
	m.Spec.Readiness.Probes[1].Port = "gaem"

	got := Validate(m, ix)
	if len(got) != 1 || got[0].Path != "spec.readiness.probes[1].port" {
		t.Fatalf("want one error at spec.readiness.probes[1].port, got:\n%s", got)
	}
	if !strings.Contains(got[0].Fix, "game") {
		t.Errorf("a one-character typo deserves the suggestion: %q", got[0].Fix)
	}
}

func TestHeapVarMustNameAMemoryVar(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Manifest)
	}{
		{"undeclared", func(m *Manifest) { m.Spec.Sizing.HeapVar = "jvm_xmxx" }},
		{"the wrong type", func(m *Manifest) { m.Spec.Sizing.HeapVar = "server_name" }},
		{"missing entirely", func(m *Manifest) { m.Spec.Sizing.HeapVar = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ix := fixture(t, "good/rich.yaml")
			tc.edit(m)

			got := Validate(m, ix)
			if len(got) != 1 || got[0].Path != "spec.sizing.heap_var" {
				t.Fatalf("want one error at spec.sizing.heap_var, got:\n%s", got)
			}
		})
	}
}

func TestASentinelMustStayInsideTheInstall(t *testing.T) {
	for _, path := range []string{"/usr/bin/true", "../../../bin/sh", "a/../../b"} {
		m, ix := fixture(t, "good/minimal.yaml")
		m.Spec.Provider.SteamCMD.Sentinel.Path = path

		got := Validate(m, ix)
		if len(got) != 1 || got[0].Path != "spec.provider.steamcmd.sentinel.path" {
			t.Fatalf("sentinel %q: want one error naming the path, got:\n%s", path, got)
		}
	}
}

func TestDuplicatePortNamesAreRefusedBecauseNamesAreTheAllocatorsKeys(t *testing.T) {
	m, ix := fixture(t, "good/rich.yaml")
	m.Spec.Ports[1].Name = "game"

	got := Validate(m, ix)
	if len(got) != 1 || got[0].Path != "spec.ports[1].name" {
		t.Fatalf("want one error at spec.ports[1].name, got:\n%s", got)
	}
}

func TestAGroupWithNoStrideIsRefused(t *testing.T) {
	m, ix := fixture(t, "good/rich.yaml")
	delete(m.Spec.PortStride, "admin")

	got := Validate(m, ix)
	if len(got) != 1 || got[0].Path != "spec.ports[2].group" {
		t.Fatalf("want one error at spec.ports[2].group, got:\n%s", got)
	}
	// A group shifts as a unit; the stride is what keeps ARK's 7777+7778 and
	// Valheim's 2456-2458 contiguous through a shift.
	if !strings.Contains(got[0].Fix, "contiguity") {
		t.Errorf("the fix has to say what a stride is for: %q", got[0].Fix)
	}
}

func TestAKeyInBothKeysAndDenyKeysIsRefused(t *testing.T) {
	m, ix := fixture(t, "good/rich.yaml")
	m.Spec.Configs[0].DenyKeys = append(m.Spec.Configs[0].DenyKeys, "UPnP")

	got := Validate(m, ix)
	if len(got) != 1 || got[0].Path != "spec.config_files[0].keys.UPnP" {
		t.Fatalf("want one error at spec.config_files[0].keys.UPnP, got:\n%s", got)
	}
}

func hasPath(ds Diags, path string) bool {
	for _, d := range ds {
		if d.Path == path {
			return true
		}
	}
	return false
}
