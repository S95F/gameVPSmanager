package spec

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func testRenderer() *Renderer {
	return NewRenderer(RenderData{
		Name:    "pz-main",
		Game:    "projectzomboid",
		Install: "/opt/pz-server",
		Data:    "/var/lib/gvm/instances/pz-main/data",
		Home:    "/var/lib/gvm/instances/pz-main",
		Work:    "/var/lib/gvm/instances/pz-main/steam",
		User:    "gvm-pz-main",
		Group:   "gvm-pz-main",
		Port:    PortValues{"game": 16261, "rcon": 27015, "unallocated": 0},
		Var: VarValues{
			"server_name": "pzserver",
			"public_name": "",
			"difficulty":  "Hard",
			"jvm_xmx":     "8G",
			"pvp":         "true",
			"pause_empty": "no",
			"weird_bool":  "yes please",
		},
		Secret: SecretValues{"rcon_password": "hunter2", "join_password": ""},
		Content: map[string]ContentValues{
			"mods":  {Acquire: []string{"2392709985", "2169435993"}, Activate: []string{"Brita", "ATA"}},
			"empty": {},
		},
		Host: HostValues{RamMB: 16384, CPUs: 4, IP4: "203.0.113.7"},
	})
}

func TestRenderClosedSurface(t *testing.T) {
	r := testRenderer()

	tests := []struct {
		name  string
		field string
		text  string
		want  string
	}{
		{"literal passthrough", "spec.runtime.args[0]", "-servername", "-servername"},
		{"scalar var", "spec.runtime.args[1]", "{{.Var.server_name}}", "pzserver"},
		{"path", "spec.runtime.cwd", "{{.Install}}", "/opt/pz-server"},
		{"mixed text", "spec.runtime.args[2]", "-cachedir={{.Data}}", "-cachedir=/var/lib/gvm/instances/pz-main/data"},
		{"port renders as a number", "spec.runtime.args[3]", "-port={{.Port.game}}", "-port=16261"},
		{"host facts", "spec.runtime.args[4]", "{{.Host.RamMB}}/{{.Host.CPUs}}/{{.Host.IP4}}", "16384/4/203.0.113.7"},
		{"phase defaults to normal", "spec.runtime.args[5]", "{{.Phase}}", "normal"},
		{"join acquire", "spec.config_files.ini.keys.WorkshopItems", `{{join ";" .Content.mods.Acquire}}`, "2392709985;2169435993"},
		{"join activate", "spec.config_files.ini.keys.Mods", `{{join ";" .Content.mods.Activate}}`, "Brita;ATA"},
		{"upper", "spec.runtime.args[6]", "{{upper .Var.difficulty}}", "HARD"},
		{"lower", "spec.runtime.args[7]", "{{lower .Var.difficulty}}", "hard"},
		{"pipeline to an allowlisted function", "spec.runtime.args[8]", "{{.Var.difficulty | upper}}", "HARD"},
		{"quote", "spec.config_files.pal.keys.ServerName", `{{quote .Var.server_name}}`, `"pzserver"`},
		{"default fills an empty var", "spec.runtime.args[9]", `{{default "gvm server" .Var.public_name}}`, "gvm server"},
		{"default leaves a set var", "spec.runtime.args[10]", `{{default "gvm server" .Var.server_name}}`, "pzserver"},
		{"nested call", "spec.runtime.args[11]", `{{quote (default "gvm server" .Var.public_name)}}`, `"gvm server"`},
		{"mib", "spec.runtime.args[12]", "-Xmx{{mib .Var.jvm_xmx}}m", "-Xmx8192m"},
		{"plusBytes keeps the operator's units", "spec.resources.memory_max", `{{plusBytes .Var.jvm_xmx "3G"}}`, "11G"},
		{"secret", "spec.control.password", "{{.Secret.rcon_password}}", "hunter2"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := r.Render(tc.field, tc.text)
			if err != nil {
				t.Fatalf("Render(%q) = error %v", tc.text, err)
			}
			if got != tc.want {
				t.Errorf("Render(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestTaint: naming a secret marks the value, wherever in the expression it is
// named. Taint is read off the parse tree, and with no control flow that is
// exact rather than conservative -- every action in a template is expanded.
func TestTaint(t *testing.T) {
	r := testRenderer()

	tests := []struct {
		name string
		text string
		want Taint
	}{
		{"plain text", "-servername", TaintNone},
		{"ordinary var", "{{.Var.server_name}}", TaintNone},
		{"secret", "{{.Secret.rcon_password}}", TaintSecret},
		{"secret in a function argument", "{{quote .Secret.rcon_password}}", TaintSecret},
		{"secret through a pipeline", "{{.Secret.rcon_password | upper}}", TaintSecret},
		{"secret in a nested call", `{{quote (default "x" .Secret.rcon_password)}}`, TaintSecret},
		{"one secret among many fields", "{{.Var.server_name}}:{{.Secret.rcon_password}}", TaintSecret},
		{"an EMPTY secret still taints", "{{.Secret.join_password}}", TaintSecret},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, taint, err := r.Render("spec.config_files.ini.keys.K", tc.text)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if taint != tc.want {
				t.Errorf("taint = %v, want %v", taint, tc.want)
			}
		})
	}
}

// TestRenderFileNamesTaintedKeys is what the differ relies on: it must be able
// to mask Password without rendering the secret a second time to discover that
// it should not print it.
func TestRenderFileNamesTaintedKeys(t *testing.T) {
	cf := ConfigFile{
		Name:   "ini",
		Path:   "{{.Data}}/Server/{{.Var.server_name}}.ini",
		Format: "pz_ini",
		Owner:  "server",
		Keys: map[string]string{
			"PublicName":   "{{.Var.server_name}}",
			"Password":     "{{.Secret.join_password}}",
			"RCONPassword": "{{.Secret.rcon_password}}",
			"MaxPlayers":   "32",
		},
	}

	rf, err := testRenderer().RenderFile(cf)
	if err != nil {
		t.Fatalf("RenderFile: %v", err)
	}

	if want := "/var/lib/gvm/instances/pz-main/data/Server/pzserver.ini"; rf.Path != want {
		t.Errorf("Path = %q, want %q", rf.Path, want)
	}
	if want := []string{"Password", "RCONPassword"}; !reflect.DeepEqual(rf.TaintedKeys, want) {
		t.Errorf("TaintedKeys = %v, want %v", rf.TaintedKeys, want)
	}
	if !rf.Taint.Has(TaintSecret) {
		t.Error("file taint does not carry TaintSecret")
	}

	if rf.Keys["RCONPassword"] != "hunter2" {
		t.Errorf("the value itself must still be rendered, got %q", rf.Keys["RCONPassword"])
	}
	if got := rf.Display("RCONPassword"); got != Redacted {
		t.Errorf("Display(RCONPassword) = %q, want %q", got, Redacted)
	}
	if got := rf.Display("PublicName"); got != "pzserver" {
		t.Errorf("Display(PublicName) = %q, want the value", got)
	}
	// An empty secret is still a secret: masking must not depend on the value,
	// or the mask disappears exactly when the password is blank and the operator
	// most needs to be told.
	if !rf.IsTainted("Password") {
		t.Error("an empty secret was not marked tainted")
	}
	if rf.IsTainted("MaxPlayers") {
		t.Error("a literal key was marked tainted")
	}
}

func TestRenderFileReportsEveryBrokenKey(t *testing.T) {
	cf := ConfigFile{
		Name: "ini",
		Path: "{{.Data}}/x.ini",
		Keys: map[string]string{
			"A": "{{if .Var.pvp}}x{{end}}",
			"B": "{{env \"HOME\"}}",
			"C": "{{.Var.server_name}}",
		},
	}
	rf, err := testRenderer().RenderFile(cf)
	if err == nil {
		t.Fatal("RenderFile accepted two broken keys")
	}
	for _, want := range []string{"spec.config_files.ini.keys.A", "spec.config_files.ini.keys.B"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s; one broken key per run means one edit per run", err, want)
		}
	}
	if rf.Keys["C"] != "pzserver" {
		t.Error("the keys that do render should still be returned, for the report")
	}
}

// A secret in a config file's path writes the password into a filename, where
// nothing masks it: a directory listing, a backup manifest and a `ls` in a
// support ticket all show it. Validation refuses it (8.6 check 4), but
// checkTemplates is skipped when the NodeIndex is nil — the apply-from-pin
// case — so RenderFile has to refuse it too.
func TestRenderFileRefusesASecretInThePath(t *testing.T) {
	for _, path := range []string{
		"{{.Data}}/{{.Secret.rcon_password}}.ini",
		"{{.Secret.rcon_password}}",
	} {
		rf, err := testRenderer().RenderFile(ConfigFile{Name: "ini", Path: path})
		if err == nil {
			t.Fatalf("RenderFile(%q) accepted a secret in the path, giving %q", path, rf.Path)
		}
		if !strings.Contains(err.Error(), "spec.config_files.ini.path") {
			t.Errorf("error %q does not name the path that is wrong", err)
		}
		if strings.Contains(rf.Path, "hunter2") {
			t.Errorf("the secret survived into rf.Path = %q", rf.Path)
		}
	}
}

// A path error must not short-circuit the key pass: one run, every complaint.
func TestRenderFileReportsAPathErrorAlongsideKeyErrors(t *testing.T) {
	rf, err := testRenderer().RenderFile(ConfigFile{
		Name: "ini",
		Path: "{{.Secret.rcon_password}}.ini",
		Keys: map[string]string{"B": `{{env "HOME"}}`, "C": "{{.Var.server_name}}"},
	})
	if err == nil {
		t.Fatal("RenderFile accepted a bad path and a bad key")
	}
	for _, want := range []string{"spec.config_files.ini.path", "spec.config_files.ini.keys.B"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	if rf.Keys["C"] != "pzserver" {
		t.Error("the keys that do render should still be returned, for the report")
	}
}

// TestRenderRefusesAnUnguardedString: Render re-runs the guard rather than
// trusting that the manifest was checked at load, so a Renderer handed a string
// from anywhere else cannot be a way around the closed surface.
func TestRenderRefusesAnUnguardedString(t *testing.T) {
	for _, text := range []string{`{{env "AWS_SECRET_ACCESS_KEY"}}`, `{{if .Var.pvp}}x{{end}}`, `{{.}}`} {
		got, taint, err := testRenderer().Render("spec.runtime.args[0]", text)
		if err == nil {
			t.Fatalf("Render(%q) succeeded, giving %q", text, got)
		}
		if !errors.Is(err, ErrTemplate) {
			t.Errorf("Render(%q) error %v does not wrap ErrTemplate", text, err)
		}
		if got != "" || taint != TaintNone {
			t.Errorf("Render(%q) = %q/%v, want no output at all", text, got, taint)
		}
	}
}

// TestRenderReturnsNoPartialOutput: half an argv element or half a config value
// is worse than none, because it looks like a value.
func TestRenderReturnsNoPartialOutput(t *testing.T) {
	got, _, err := testRenderer().Render("spec.runtime.args[0]", "-servername={{.Var.nonexistent}}")
	if err == nil {
		t.Fatal("a missing var rendered successfully; missingkey=error is not in force")
	}
	if got != "" {
		t.Errorf("output = %q, want the partial expansion discarded", got)
	}
	if !strings.Contains(err.Error(), "spec.runtime.args[0]") {
		t.Errorf("error %q does not name the field", err)
	}
}

func TestRenderVerbOperands(t *testing.T) {
	r := testRenderer().WithVerbArgs(VerbArgs{Msg: "restarting in 5", Player: "steve", Reason: "griefing", Duration: "30m"})

	tests := []struct {
		field, text, want string
	}{
		{"spec.verbs.broadcast.control", `servermsg "{{.Msg}}"`, `servermsg "restarting in 5"`},
		{"spec.verbs.kick.control", `kickuser "{{.Player}}" -r "{{.Reason}}"`, `kickuser "steve" -r "griefing"`},
		{"spec.verbs.ban.control", `banuser "{{.Player}}" {{.Duration}}`, `banuser "steve" 30m`},
	}
	for _, tc := range tests {
		got, _, err := r.Render(tc.field, tc.text)
		if err != nil {
			t.Fatalf("Render(%q): %v", tc.text, err)
		}
		if got != tc.want {
			t.Errorf("Render(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}

	// The operands are bound on a COPY: a verb invocation must not leave its
	// player name behind on the shared renderer.
	base := testRenderer()
	base.WithVerbArgs(VerbArgs{Player: "steve"})
	if base.Data().Player != "" {
		t.Error("WithVerbArgs mutated the receiver")
	}
}

func TestWithPhaseCopies(t *testing.T) {
	base := testRenderer()
	install := base.WithPhase(PhaseInstall)

	if base.Data().Phase != PhaseNormal {
		t.Errorf("receiver phase = %q, want it untouched", base.Data().Phase)
	}
	if install.Data().Phase != PhaseInstall {
		t.Errorf("copy phase = %q, want install", install.Data().Phase)
	}
	got, _, err := install.Render("spec.runtime.args[0]", "{{.Phase}}")
	if err != nil || got != "install" {
		t.Errorf("Render(.Phase) = %q, %v; want \"install\"", got, err)
	}
}

func TestTemplateFuncs(t *testing.T) {
	t.Run("mib", func(t *testing.T) {
		tests := []struct {
			in      string
			want    int64
			wantErr bool
		}{
			{"5g", 5120, false},
			{"8G", 8192, false},
			{"512M", 512, false},
			{"1048576", 1, false},
			// A size that is not a whole number of MiB is refused rather than
			// rounded: a silently shrunk heap surfaces as an OOM kill weeks later.
			{"1500K", 0, true},
			{"1.5G", 0, true},
			{"", 0, true},
			{"lots", 0, true},
		}
		for _, tc := range tests {
			got, err := tmplMiB(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("mib(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
				continue
			}
			if err == nil && got != tc.want {
				t.Errorf("mib(%q) = %d, want %d", tc.in, got, tc.want)
			}
		}
	})

	t.Run("plusBytes", func(t *testing.T) {
		tests := []struct {
			a, b    string
			want    string
			wantErr bool
		}{
			{"8G", "3G", "11G", false},
			{"1024M", "1024M", "2G", false},
			{"8G", "512M", "8704M", false},
			{"8G", "nonsense", "", true},
			{"nonsense", "8G", "", true},
			{"9223372036854775807", "1024", "", true},
		}
		for _, tc := range tests {
			got, err := tmplPlusBytes(tc.a, tc.b)
			if (err != nil) != tc.wantErr {
				t.Errorf("plusBytes(%q,%q) error = %v, wantErr %v", tc.a, tc.b, err, tc.wantErr)
				continue
			}
			if err == nil && got != tc.want {
				t.Errorf("plusBytes(%q,%q) = %q, want %q", tc.a, tc.b, got, tc.want)
			}
		}
	})

	t.Run("quote escapes what a game config would otherwise mis-parse", func(t *testing.T) {
		tests := []struct{ in, want string }{
			{`plain`, `"plain"`},
			{`say "hi"`, `"say \"hi\""`},
			{`back\slash`, `"back\\slash"`},
			{"new\nline", `"new\nline"`},
		}
		for _, tc := range tests {
			if got := tmplQuote(tc.in); got != tc.want {
				t.Errorf("quote(%q) = %s, want %s", tc.in, got, tc.want)
			}
		}
	})

	t.Run("join and default", func(t *testing.T) {
		if got := tmplJoin(";", []string{"a", "b"}); got != "a;b" {
			t.Errorf("join = %q", got)
		}
		if got := tmplJoin(";", nil); got != "" {
			t.Errorf("join(nil) = %q, want empty", got)
		}
		if got := tmplDefault("fb", ""); got != "fb" {
			t.Errorf("default(fb,\"\") = %q", got)
		}
		if got := tmplDefault("fb", "v"); got != "v" {
			t.Errorf("default(fb,v) = %q", got)
		}
	})
}

func TestRenderPhaseValid(t *testing.T) {
	for _, p := range RenderPhases() {
		if !p.Valid() {
			t.Errorf("%q is listed but not valid", p)
		}
	}
	for _, p := range []RenderPhase{"", "startup", "post_install", "Normal"} {
		if p.Valid() {
			t.Errorf("%q is not a render phase", p)
		}
	}
	// hook.Phase's values must not quietly satisfy the render phase: they are a
	// different enum with seven members and one overlapping shape.
	if RenderPhase("pre_start").Valid() {
		t.Error("a hook phase was accepted as a render phase")
	}
}

func TestTaintHas(t *testing.T) {
	if TaintNone.Has(TaintSecret) {
		t.Error("TaintNone claims a secret")
	}
	if !TaintSecret.Has(TaintSecret) {
		t.Error("TaintSecret does not carry itself")
	}
	if TaintSecret.String() != "secret" || TaintNone.String() != "none" {
		t.Errorf("String() = %q/%q", TaintSecret, TaintNone)
	}
}
