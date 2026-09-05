package spec

import (
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// whenManifest wraps a `when:` block around one argv element and returns the
// decoded guard alongside the index, so a diagnostic can be checked against the
// line the operator actually wrote.
func whenManifest(t *testing.T, whenBlock string) (*When, *NodeIndex, string) {
	t.Helper()
	body := `apiVersion: gvm/v1
kind: Game
metadata:
  id: pz
  name: Project Zomboid
  revision: 1
spec:
  driverABI: 1
  runtime:
    exec: ./start-server.sh
    args:
      - value: "-adminpassword"
        when:
` + whenBlock

	ix, err := IndexBytes("projectzomboid.yaml", []byte(body))
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	var m Manifest
	if err := yaml.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(m.Spec.Runtime.Args) != 1 {
		t.Fatalf("want 1 arg, got %d", len(m.Spec.Runtime.Args))
	}
	return m.Spec.Runtime.Args[0].When, ix, body
}

// lineOf reports the one-based line of the first line containing needle, so a
// test asserts the diagnostic points at the operator's own text rather than at a
// number someone has to recount every time the fixture changes.
func lineOf(t *testing.T, body, needle string) int {
	t.Helper()
	for i, l := range strings.Split(body, "\n") {
		if strings.Contains(l, needle) {
			return i + 1
		}
	}
	t.Fatalf("%q is not in the fixture", needle)
	return 0
}

// TestWhenExactlyOne: two predicates have no defined combination -- a reader
// would have to guess between AND and last-one-wins -- so the second is refused,
// at its own line, naming the first.
func TestWhenExactlyOne(t *testing.T) {
	w, ix, body := whenManifest(t, "          phase: install\n          var_true: pvp\n")

	diags := w.Check(ix, "spec.runtime.args[0].when")
	if len(diags) != 1 {
		t.Fatalf("want 1 diagnostic, got %d: %s", len(diags), diags)
	}
	d := diags[0]

	wantLine := lineOf(t, body, "var_true: pvp")
	if d.File != "projectzomboid.yaml" || d.Line != wantLine {
		t.Errorf("position = %s:%d, want projectzomboid.yaml:%d", d.File, d.Line, wantLine)
	}
	if !strings.Contains(d.Msg, "var_true") || !strings.Contains(d.Msg, "phase") {
		t.Errorf("message = %q, want it to name both predicates", d.Msg)
	}
	if want := "projectzomboid.yaml:" + strconv.Itoa(wantLine) + ":"; !strings.Contains(d.String(), want) {
		t.Errorf("rendered diagnostic %q does not carry %q", d.String(), want)
	}

	// Eval must refuse the same thing the check refuses: a guard that validation
	// never ran over is not a guard to start guessing about.
	if _, err := w.Eval(&RenderData{Phase: PhaseInstall, Var: VarValues{"pvp": "true"}}); err == nil {
		t.Error("Eval accepted two predicates")
	}
}

func TestWhenCheck(t *testing.T) {
	tests := []struct {
		name       string
		block      string
		wantN      int
		wantMsg    string
		wantLineOf string
	}{
		{
			name:  "one predicate is fine",
			block: "          phase: install\n",
			wantN: 0,
		},
		{
			name:  "not alone is fine",
			block: "          not:\n            phase: install\n",
			wantN: 0,
		},
		{
			name:  "a predicate plus not is a conjunction, not a collision",
			block: "          phase: normal\n          not:\n            var_true: dev_mode\n",
			wantN: 0,
		},
		{
			name:       "three predicates report the two extras",
			block:      "          phase: install\n          var_true: pvp\n          var_set: motd\n",
			wantN:      2,
			wantMsg:    "exactly one predicate may be set",
			wantLineOf: "var_true: pvp",
		},
		{
			// A misspelled key decodes to a zero When with no error at all, and a
			// zero When that meant "always" would put an install-only
			// -adminpassword on every restart's argv.
			name:       "a misspelled predicate is an empty guard",
			block:      "          var_tru: pvp\n",
			wantN:      1,
			wantMsg:    "has no predicate",
			wantLineOf: "var_tru: pvp",
		},
		{
			name:       "unknown phase",
			block:      "          phase: startup\n",
			wantN:      1,
			wantMsg:    "is not a phase",
			wantLineOf: "phase: startup",
		},
		{
			name:       "var_equals without a name",
			block:      "          var_equals:\n            value: \"42\"\n",
			wantN:      1,
			wantMsg:    "var_equals needs a `name`",
			wantLineOf: "value: \"42\"",
		},
		{
			name:       "the collision inside not is reported too",
			block:      "          not:\n            phase: install\n            var_set: motd\n",
			wantN:      1,
			wantMsg:    "collides with",
			wantLineOf: "var_set: motd",
		},
		{
			// `not(nothing)` is the same empty guard one level down, and the
			// outer block is NOT also reported: `not:` is a predicate as far as
			// the outer level is concerned.
			name:       "an empty not",
			block:      "          not: {}\n",
			wantN:      1,
			wantMsg:    "has no predicate",
			wantLineOf: "not: {}",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, ix, body := whenManifest(t, tc.block)
			diags := w.Check(ix, "spec.runtime.args[0].when")
			if len(diags) != tc.wantN {
				t.Fatalf("want %d diagnostics, got %d: %s", tc.wantN, len(diags), diags)
			}
			if tc.wantN == 0 {
				return
			}
			if !strings.Contains(diags[0].Msg, tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", diags[0].Msg, tc.wantMsg)
			}
			if want := lineOf(t, body, tc.wantLineOf); diags[0].Line != want {
				t.Errorf("line = %d, want %d (%q)", diags[0].Line, want, tc.wantLineOf)
			}
			for _, d := range diags {
				if d.Sev != SevError {
					t.Errorf("severity = %q, want error", d.Sev)
				}
			}
		})
	}
}

func TestWhenEval(t *testing.T) {
	data := &RenderData{
		Phase:  PhaseNormal,
		Var:    VarValues{"pvp": "true", "pause_empty": "off", "motd": "hello", "empty": "", "build": "42", "junk": "yes please"},
		Secret: SecretValues{"rcon_password": "hunter2", "join_password": ""},
		Port:   PortValues{"game": 16261, "unallocated": 0},
		Content: map[string]ContentValues{
			"mods":         {Acquire: []string{"2392709985"}},
			"activateonly": {Activate: []string{"Brita"}},
			"empty":        {},
		},
	}

	tests := []struct {
		name    string
		when    *When
		want    bool
		wantErr bool
	}{
		{"nil applies", nil, true, false},
		{"var_true true", &When{VarTrue: "pvp"}, true, false},
		{"var_true off", &When{VarTrue: "pause_empty"}, false, false},
		{"var_true empty is false", &When{VarTrue: "empty"}, false, false},
		// A var that is neither true nor false is an error, never false: a
		// silently dropped argument boots a server that looks fine.
		{"var_true junk", &When{VarTrue: "junk"}, false, true},
		{"var_true missing var", &When{VarTrue: "nope"}, false, true},

		{"var_set", &When{VarSet: "motd"}, true, false},
		{"var_set on an empty value", &When{VarSet: "empty"}, false, false},
		{"var_set on a missing var", &When{VarSet: "nope"}, false, false},

		{"var_equals hit", &When{VarEquals: &NameVal{Name: "build", Value: "42"}}, true, false},
		{"var_equals miss", &When{VarEquals: &NameVal{Name: "build", Value: "41"}}, false, false},
		{"var_equals without a name", &When{VarEquals: &NameVal{Value: "42"}}, false, true},

		{"secret_set", &When{SecretSet: "rcon_password"}, true, false},
		{"secret_set on an empty secret", &When{SecretSet: "join_password"}, false, false},
		{"secret_set on a missing secret", &When{SecretSet: "nope"}, false, false},

		{"port_set", &When{PortSet: "game"}, true, false},
		{"port_set on an unallocated port", &When{PortSet: "unallocated"}, false, false},
		{"port_set on a missing port", &When{PortSet: "nope"}, false, false},

		{"content_nonempty via acquire", &When{ContentNonEmpty: "mods"}, true, false},
		{"content_nonempty via activate", &When{ContentNonEmpty: "activateonly"}, true, false},
		{"content_nonempty on an empty block", &When{ContentNonEmpty: "empty"}, false, false},
		{"content_nonempty on a missing block", &When{ContentNonEmpty: "nope"}, false, false},

		{"phase hit", &When{Phase: "normal"}, true, false},
		{"phase miss", &When{Phase: "install"}, false, false},
		{"phase unknown", &When{Phase: "startup"}, false, true},

		{"not inverts", &When{Not: &When{Phase: "install"}}, true, false},
		{"not inverts a true predicate", &When{Not: &When{Phase: "normal"}}, false, false},
		{"predicate and not", &When{Phase: "normal", Not: &When{VarTrue: "pause_empty"}}, true, false},
		{"predicate and not, negated arm true", &When{Phase: "normal", Not: &When{VarTrue: "pvp"}}, false, false},
		{"nested not", &When{Not: &When{Not: &When{Phase: "normal"}}}, true, false},
		{"an error inside not propagates", &When{Not: &When{VarTrue: "junk"}}, false, true},

		{"empty guard is an error", &When{}, false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.when.Eval(data)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Eval error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("Eval = %v, want %v", got, tc.want)
			}
			if err != nil && got {
				t.Error("Eval returned true alongside an error")
			}
		})
	}
}

func TestWhenEvalWithoutValues(t *testing.T) {
	if _, err := (&When{Phase: "normal"}).Eval(nil); err == nil {
		t.Error("Eval with no bound values should not guess")
	}
}

func TestWhenPredicates(t *testing.T) {
	tests := []struct {
		name string
		when *When
		want []string
	}{
		{"nil", nil, nil},
		{"empty", &When{}, nil},
		{"not is not a predicate", &When{Not: &When{Phase: "install"}}, nil},
		{"one", &When{Phase: "install"}, []string{"phase"}},
		// Declaration order, because that is the order the collision message
		// reads best in: the first one wins the sentence.
		{"declaration order", &When{Phase: "install", VarTrue: "pvp", SecretSet: "s"},
			[]string{"var_true", "secret_set", "phase"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.when.Predicates()
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("Predicates() = %v, want %v", got, tc.want)
			}
		})
	}
}
