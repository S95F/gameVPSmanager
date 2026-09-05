package spec

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"text/template"
)

// argManifest puts one templated argv element in a manifest and reports the
// line it landed on, so a test can assert the diagnostic points AT it rather
// than merely somewhere in the file.
func argManifest(payload string) (yaml string, path string, line int) {
	const head = `apiVersion: gvm/v1
kind: Game
metadata:
  id: pz
  name: Project Zomboid
  revision: 3
spec:
  driverABI: 1
  runtime:
    exec: ./start-server.sh
    args:
      - `
	body := head + strconv.Quote(payload) + "\n"
	return body, "spec.runtime.args[0]", strings.Count(head, "\n") + 1
}

// verbManifest is the other surface: spec.verbs.* is the only place .Msg and
// friends exist.
func verbManifest(payload string) (yaml string, path string, line int) {
	const head = `apiVersion: gvm/v1
kind: Game
metadata:
  id: pz
  name: Project Zomboid
  revision: 3
spec:
  driverABI: 1
  runtime:
    exec: ./start-server.sh
  verbs:
    broadcast:
      control: `
	body := head + strconv.Quote(payload) + "\n"
	return body, "spec.verbs.broadcast.control", strings.Count(head, "\n") + 1
}

func guardDiags(t *testing.T, body, file string) Diags {
	t.Helper()
	ix, err := IndexBytes(file, []byte(body))
	if err != nil {
		t.Fatalf("index %s: %v", file, err)
	}
	return GuardDocument(ix)
}

// TestGuardRejectsControlFlowAndUnlistedFunctions is the corpus §21 names: every
// construct that would turn a manifest into a program, each rejected with the
// manifest's own file, line and column.
func TestGuardRejectsControlFlowAndUnlistedFunctions(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string // substring of the message
	}{
		{"if", `{{if .Var.pvp}}-pvp{{end}}`, "{{if}} is not allowed"},
		{"range", `{{range .Content.mods.Acquire}}{{end}}`, "{{range}} is not allowed"},
		{"with", `{{with .Var.motd}}{{end}}`, "{{with}} is not allowed"},
		{"template", `{{template "other"}}`, "{{template}}/{{block}} is not allowed"},
		{"block", `{{block "other" .}}x{{end}}`, "{{template}}/{{block}} is not allowed"},
		{"define", `{{define "other"}}x{{end}}`, `{{define "other"}} is not allowed`},
		{"call", `{{call .Var.f}}`, `function "call" is not on the allowlist`},
		{"printf", `{{printf "%s" .Var.motd}}`, `function "printf" is not on the allowlist`},
		{"index", `{{index .Content.mods.Acquire 0}}`, `function "index" is not on the allowlist`},
		{"env", `{{env "AWS_SECRET_ACCESS_KEY"}}`, `function "env" not defined`},
		{"js", `{{js .Var.motd}}`, `function "js" is not on the allowlist`},
		{"urlquery", `{{urlquery .Var.motd}}`, `function "urlquery" is not on the allowlist`},
		{"print", `{{print .Var.motd}}`, `function "print" is not on the allowlist`},
		{"len", `{{len .Content.mods.Acquire}}`, `function "len" is not on the allowlist`},
		{"slice", `{{slice .Var.motd 1 2}}`, `function "slice" is not on the allowlist`},
		{"eq", `{{eq .Var.motd "x"}}`, `function "eq" is not on the allowlist`},
		{"and", `{{and .Var.motd .Var.pvp}}`, `function "and" is not on the allowlist`},
		{"not", `{{not .Var.pvp}}`, `function "not" is not on the allowlist`},
		{"undeclared variable", `{{.Env.PATH}}`, "there is no template variable .Env"},
		{"bare dot", `-name={{.}}`, "renders the entire variable set"},
		{"whole secret map", `{{.Secret}}`, ".Secret takes exactly one name"},
		{"deep field", `{{.Var.motd.Length}}`, ".Var takes exactly one name"},
		{"unknown host field", `{{.Host.Hostname}}`, ".Host has exactly"},
		{"content leaf", `{{.Content.mods.Ids}}`, ".Content takes a block name"},
		{"variable assignment", `{{$x := .Var.motd}}`, "variable assignment is not allowed"},
		{"the root variable", `{{$}}`, "template variables ($) are not allowed"},
		{"nil", `{{nil}}`, "nil is not a value"},
		{"pipe to unlisted", `{{.Var.motd | printf "%s"}}`, `function "printf" is not on the allowlist`},
		{"pipe to a field", `{{.Var.motd | .Var.other}}`, "pipeline stage must be one of the seven functions"},
		{"wrong arity", `{{join ";"}}`, "join takes 2 argument(s), got 1"},
		{"bad root inside a call", `{{join ";" .Env.PATH}}`, "there is no template variable .Env"},
		{"unlisted function inside a nested call", `{{quote (printf "%s" .Var.motd)}}`, `function "printf" is not on the allowlist`},
		{"the whole data value as an argument", `{{quote .}}`, "renders the entire variable set"},
		{"verb var outside a verb", `{{.Player}}`, ".Player exists only in a verb template"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, path, line := argManifest(tc.payload)
			diags := guardDiags(t, body, "projectzomboid.yaml")
			if len(diags) != 1 {
				t.Fatalf("want exactly 1 diagnostic, got %d: %s", len(diags), diags)
			}
			d := diags[0]
			if !strings.Contains(d.Msg, tc.want) {
				t.Errorf("message = %q, want it to contain %q", d.Msg, tc.want)
			}
			if d.Sev != SevError {
				t.Errorf("severity = %q, want error", d.Sev)
			}
			if d.Path != path {
				t.Errorf("path = %q, want %q", d.Path, path)
			}
			if d.File != "projectzomboid.yaml" || d.Line != line || d.Col == 0 {
				t.Errorf("position = %s:%d:%d, want projectzomboid.yaml:%d with a column",
					d.File, d.Line, d.Col, line)
			}
			// The rendered form is what an operator's editor parses; a message
			// with no file:line is unactionable no matter how good the wording.
			if want := "projectzomboid.yaml:" + strconv.Itoa(line) + ":"; !strings.Contains(d.String(), want) {
				t.Errorf("rendered diagnostic %q does not carry %q", d.String(), want)
			}
		})
	}
}

// TestGuardAcceptsTheClosedSurface pins what a manifest author is allowed to
// write. A guard that rejected these would be a guard nobody could ship a
// manifest through.
func TestGuardAcceptsTheClosedSurface(t *testing.T) {
	payloads := []string{
		`-servername`,
		`{{.Var.server_name}}`,
		`-cachedir={{.Data}}`,
		`{{.Install}}/start-server.sh`,
		`-port={{.Port.game}}`,
		`{{.Secret.admin_password}}`,
		`{{join ";" .Content.mods.Acquire}}`,
		`{{join ";" .Content.mods.Activate}}`,
		`{{upper .Var.difficulty}}`,
		`{{lower .Var.difficulty}}`,
		`{{.Var.difficulty | upper}}`,
		`{{quote .Var.public_name}}`,
		`{{quote (default "gvm server" .Var.public_name)}}`,
		`-Xmx{{mib .Var.jvm_xmx}}m`,
		`{{plusBytes .Var.jvm_xmx "3G"}}`,
		`{{.Host.RamMB}}/{{.Host.CPUs}}/{{.Host.IP4}}`,
		`{{.Phase}}`,
		`{{.Name}}-{{.Game}}-{{.User}}-{{.Group}}-{{.Home}}-{{.Work}}`,
	}
	for _, p := range payloads {
		t.Run(p, func(t *testing.T) {
			body, _, _ := argManifest(p)
			if diags := guardDiags(t, body, "pz.yaml"); len(diags) != 0 {
				t.Fatalf("rejected a legal template: %s", diags)
			}
		})
	}
}

func TestVerbOperandsAreSurfaceScoped(t *testing.T) {
	for _, v := range []string{".Msg", ".Player", ".Reason", ".Duration"} {
		t.Run(v, func(t *testing.T) {
			body, _, _ := verbManifest(`servermsg "{{` + v + `}}"`)
			if diags := guardDiags(t, body, "pz.yaml"); len(diags) != 0 {
				t.Fatalf("%s rejected inside a verb: %s", v, diags)
			}

			body, _, line := argManifest(`{{` + v + `}}`)
			diags := guardDiags(t, body, "pz.yaml")
			if len(diags) != 1 || diags[0].Line != line {
				t.Fatalf("%s outside a verb: want one diagnostic at line %d, got %s", v, line, diags)
			}
			if !strings.Contains(diags[0].Msg, "only in a verb template") {
				t.Errorf("message = %q, want it to explain the verb surface", diags[0].Msg)
			}
		})
	}
}

// TestGuardRefusesTheManifestAtLoad is the difference between a guard and a
// linter: a manifest carrying {{env}} must not become a *Manifest at all, in any
// of the three code paths that decode one.
func TestGuardRefusesTheManifestAtLoad(t *testing.T) {
	body, path, line := argManifest(`{{env "AWS_SECRET_ACCESS_KEY"}}`)

	m, diags, err := parseManifest("override/pz.yaml", []byte(body))
	if err == nil {
		t.Fatal("parseManifest accepted a manifest that reads the daemon's environment")
	}
	if m != nil {
		t.Error("a refused manifest must not be returned")
	}
	if !errors.Is(err, ErrTemplate) {
		t.Errorf("error = %v, want it to wrap ErrTemplate", err)
	}
	if len(diags) != 1 || diags[0].Path != path || diags[0].Line != line {
		t.Fatalf("want one diagnostic at %s line %d, got %s", path, line, diags)
	}
}

// TestBannedTemplateInAnOverrideDoesNotFallThrough: the packaged manifest is
// NOT quietly used when the operator's own override is refused. Falling back
// would mean a box running argv nobody chose, from a file nobody is looking at.
func TestBannedTemplateInAnOverrideDoesNotFallThrough(t *testing.T) {
	l := newThreeLayers(t)
	writeGame(t, l.shareDir, "pz", 1)

	body, _, _ := argManifest(`{{env "AWS_SECRET_ACCESS_KEY"}}`)
	if err := os.WriteFile(filepath.Join(l.etcDir, "pz.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	m, diags, err := l.cat.Load("pz", nil)
	if err == nil {
		t.Fatalf("Load returned a manifest from %s instead of refusing", m.Origin)
	}
	if !errors.Is(err, ErrTemplate) {
		t.Errorf("error = %v, want it to wrap ErrTemplate", err)
	}
	if !diags.HasErrors() {
		t.Error("no error diagnostic; the operator has nothing to fix")
	}
}

// TestGuardIgnoresScalarsWithoutTemplates keeps the walk from turning every
// brace-free string in the document into work, and pins that a manifest with no
// templates at all loads clean.
func TestGuardIgnoresScalarsWithoutTemplates(t *testing.T) {
	body, _, _ := argManifest(`-servername`)
	if diags := guardDiags(t, body, "pz.yaml"); len(diags) != 0 {
		t.Fatalf("plain manifest produced diagnostics: %s", diags)
	}
}

// TestGuardReportsTheLineWithinABlockScalar: a multi-line value's YAML position
// is its first line, so the message has to say which line of the value broke or
// the caret is silently wrong.
func TestGuardReportsTheLineWithinABlockScalar(t *testing.T) {
	body := `apiVersion: gvm/v1
kind: Game
metadata:
  id: pz
  name: pz
  revision: 1
spec:
  driverABI: 1
  runtime:
    exec: ./start.sh
  vars:
    motd:
      type: string
      help: |
        first line is fine
        {{if .Var.x}}second is not{{end}}
`
	diags := guardDiags(t, body, "pz.yaml")
	if len(diags) != 1 {
		t.Fatalf("want 1 diagnostic, got %d: %s", len(diags), diags)
	}
	if !strings.Contains(diags[0].Msg, "line 2 of the value") {
		t.Errorf("message = %q, want it to name the line within the value", diags[0].Msg)
	}
	if diags[0].Line == 0 {
		t.Error("diagnostic has no file position")
	}
}

// TestFuncMapMatchesAllowlist is the anti-rot check. The FuncMap and the
// allowlist are separate declarations on purpose, and this golden list is a
// third: adding a function to the template surface has to be a deliberate,
// reviewable edit in three places rather than something that slips in with a
// helper.
func TestFuncMapMatchesAllowlist(t *testing.T) {
	want := []string{"default", "join", "lower", "mib", "plusBytes", "quote", "upper"}

	got := make([]string, 0, len(templateFuncs))
	for name := range templateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("templateFuncs = %v, want exactly %v.\n"+
			"A new template function is a change to the closed surface (D-21): add it to "+
			"allowedFuncs with its arity, document it in render.go's surface comment, and update this list.",
			got, want)
	}

	allowed := make([]string, 0, len(allowedFuncs))
	for name := range allowedFuncs {
		allowed = append(allowed, name)
	}
	sort.Strings(allowed)
	if strings.Join(allowed, " ") != strings.Join(want, " ") {
		t.Errorf("allowedFuncs = %v, want exactly %v", allowed, want)
	}
}

// TestGuardRejectsAFuncMapEntryNotOnTheAllowlist proves the guard does not read
// its allowlist out of the FuncMap. If it did, adding a function to the map
// would silently widen the surface, which is the exact failure mode the two
// declarations exist to prevent.
func TestGuardRejectsAFuncMapEntryNotOnTheAllowlist(t *testing.T) {
	const text = `{{shell .Var.motd}}`

	widened := template.FuncMap{"shell": func(s string) string { return s }}
	tmpl, err := template.New("spec.runtime.args[0]").Funcs(widened).Parse(text)
	if err != nil {
		t.Fatalf("the widened FuncMap should parse: %v", err)
	}

	te := guardNode("spec.runtime.args[0]", text, tmpl.Tree.Root, SurfaceManifest)
	if te == nil {
		t.Fatal("guard accepted a function that is in a FuncMap but not on the allowlist")
	}
	if !strings.Contains(te.Msg, `"shell" is not on the allowlist`) {
		t.Errorf("message = %q, want it to name the function", te.Msg)
	}
	if !errors.Is(te, ErrTemplate) {
		t.Errorf("error does not wrap ErrTemplate: %v", te)
	}
}

func TestSurfaceFor(t *testing.T) {
	tests := []struct {
		path string
		want Surface
	}{
		{"spec.verbs.broadcast.control", SurfaceVerb},
		{"spec.verbs.kick.console", SurfaceVerb},
		{"verbs.ban.control", SurfaceVerb},
		{"spec.runtime.args[3]", SurfaceManifest},
		{"spec.config_files.ini.keys.Password", SurfaceManifest},
		{"spec.verbsomething", SurfaceManifest},
		{"", SurfaceManifest},
	}
	for _, tc := range tests {
		if got := SurfaceFor(tc.path); got != tc.want {
			t.Errorf("SurfaceFor(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestTextPos(t *testing.T) {
	tests := []struct {
		text     string
		off      int
		wantLine int
		wantCol  int
	}{
		{"abc", 0, 1, 1},
		{"abc", 2, 1, 3},
		{"a\nbc", 2, 2, 1},
		{"a\nbc", 3, 2, 2},
		{"a\nb\nc", 4, 3, 1},
		{"abc", 99, 1, 4},
		{"abc", -1, 1, 1},
	}
	for _, tc := range tests {
		line, col := textPos(tc.text, tc.off)
		if line != tc.wantLine || col != tc.wantCol {
			t.Errorf("textPos(%q, %d) = %d:%d, want %d:%d",
				tc.text, tc.off, line, col, tc.wantLine, tc.wantCol)
		}
	}
}
