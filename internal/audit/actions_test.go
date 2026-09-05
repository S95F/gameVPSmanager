package audit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// declaredAction is one `X Action = "y.z"` found in the package source.
type declaredAction struct {
	Name  string // the Go constant name
	Value Action // the string that reaches the action column
	File  string
	Line  int
}

// actionShape is what an action string looks like. It is used to catch a
// constant that was declared with an inferred type instead of Action, which
// the AST walk below would otherwise not see -- and an action the walk cannot
// see is an action that can skip the allowlist check.
var actionShape = regexp.MustCompile(`^[a-z][a-z0-9]*\.[a-z][a-z0-9_]*$`)

// declaredActions walks the package's non-test sources for Action constants.
//
// A hand-maintained []Action would be simpler and would not work: the failure
// this test exists to catch is somebody adding a constant and forgetting the
// other place it has to be listed, and a slice is just another other place.
//
// The walk implements Go's implicit type repetition -- inside a const block a
// spec with neither a type nor a value repeats the previous spec's type -- so
// an iota-style block cannot hide a constant from it.
func declaredActions(t *testing.T) []declaredAction {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	var found []declaredAction
	sawSource := false

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		sawSource = true
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		actions, problems := actionsInFile(fset, name, f)
		found = append(found, actions...)
		for _, p := range problems {
			t.Error(p)
		}
	}
	if !sawSource {
		t.Fatal("found no non-test .go files; the walk would report an empty action set and every assertion below would pass vacuously")
	}
	return found
}

// actionsInFile returns the Action constants in one file, plus the constants
// that look like actions but were declared without the type. It takes no
// *testing.T so its own behaviour can be asserted directly.
func actionsInFile(fset *token.FileSet, file string, f *ast.File) (out []declaredAction, problems []string) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var carried ast.Expr // the type in force for value-less specs
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			typ := vs.Type
			switch {
			case typ != nil:
				carried = typ
			case len(vs.Values) == 0:
				// Implicit repetition of the previous spec, type included.
				typ = carried
			default:
				// A value with no type is an inferred constant. It does NOT
				// inherit the block's type, so neither does this walk.
				carried = nil
			}

			isAction := false
			if id, ok := typ.(*ast.Ident); ok && id.Name == "Action" {
				isAction = true
			}

			for i, nameIdent := range vs.Names {
				lit := literalAt(vs.Values, i)
				pos := fset.Position(nameIdent.Pos())
				switch {
				case isAction:
					out = append(out, declaredAction{
						Name: nameIdent.Name, Value: Action(lit), File: file, Line: pos.Line,
					})
				case actionShape.MatchString(lit):
					problems = append(problems, fmt.Sprintf(
						"%s:%d: const %s = %q looks like an action but is not declared with the "+
							"Action type. An untyped constant is invisible to this walk, so it could "+
							"be used as an Action without ever being checked against the allowlist. "+
							"Declare it as `%s Action = %q`.",
						file, pos.Line, nameIdent.Name, lit, nameIdent.Name, lit))
				}
			}
		}
	}
	return out, problems
}

// literalAt returns the i'th value as an unquoted string, or "".
func literalAt(values []ast.Expr, i int) string {
	if i >= len(values) {
		return ""
	}
	bl, ok := values[i].(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return ""
	}
	return s
}

// TestEveryActionHasAnAllowlist is section 14.8's assertion: every Action
// constant appears as a key of the allowlist.
//
// Without it, adding an action is a two-line change that compiles, runs, and
// silently logs an unclassified event with every one of its params dropped --
// the record would exist and would say nothing.
func TestEveryActionHasAnAllowlist(t *testing.T) {
	for _, d := range declaredActions(t) {
		if !Known(d.Value) {
			t.Errorf("%s:%d: Action constant %s (%q) has no entry in allowedParams.\n"+
				"Add one to internal/audit/actions.go declaring which params this action may record. "+
				"Until you do, every param it carries is dropped at runtime.",
				d.File, d.Line, d.Name, d.Value)
		}
	}
}

// TestEveryAllowlistEntryHasAConstant is the other direction: an entry keyed by
// a string nobody declared is a typo that will never match a real record, and
// it would make the test above pass while the action it was meant to cover
// still has no allowlist.
func TestEveryAllowlistEntryHasAConstant(t *testing.T) {
	declared := make(map[Action]string)
	for _, d := range declaredActions(t) {
		declared[d.Value] = d.Name
	}
	for a := range allowedParams {
		if _, ok := declared[a]; !ok {
			t.Errorf("allowedParams has an entry for %q, but no Action constant has that value. "+
				"Either the constant was renamed or the key is misspelled; a key nothing produces "+
				"protects nothing.", a)
		}
	}
}

// TestTheActionWalkHasATooth is the control for the two tests above.
//
// Both of them iterate what the AST walk found, so a walk that quietly finds
// nothing -- a refactor that renames the Action type, a file that stops
// parsing, a change in how the constants are written -- would make both pass
// while checking nothing. This asserts the walk still sees the real set.
func TestTheActionWalkHasATooth(t *testing.T) {
	found := declaredActions(t)
	if len(found) != len(allowedParams) {
		t.Fatalf("the AST walk found %d Action constants but the allowlist has %d entries; "+
			"if that is a real addition the tests above will say which, and if it is not, "+
			"the walk has stopped working", len(found), len(allowedParams))
	}
	byName := make(map[string]Action, len(found))
	for _, d := range found {
		byName[d.Name] = d.Value
	}
	if got := byName["InstanceStop"]; got != InstanceStop {
		t.Fatalf("the walk did not find InstanceStop = %q (got %q); it is no longer reading the "+
			"const block it is pointed at", InstanceStop, got)
	}
}

// TestTheActionWalkFollowsImplicitTypeRepetition proves the walk's handling of
// the one const-block form that could hide a constant from it: a spec with
// neither a type nor a value, which repeats the previous spec's type.
func TestTheActionWalkFollowsImplicitTypeRepetition(t *testing.T) {
	const src = `package audit

const (
	First  Action = "a.one"
	Second        // repeats both the type and the value
)

const (
	Untyped = "b.two"
)
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got, problems := actionsInFile(fset, "synthetic.go", f)
	if len(got) != 2 {
		t.Fatalf("walk found %d constants in a two-constant block, so implicit type repetition "+
			"is not being followed: %+v", len(got), got)
	}
	if got[1].Name != "Second" {
		t.Fatalf("second constant is %q, want Second", got[1].Name)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "Untyped") {
		t.Fatalf("problems = %v, want the untyped constant reported: it would otherwise be usable "+
			"as an Action without ever meeting the allowlist", problems)
	}
}

// TestNoAllowlistNamesASecret is a denylist applied to the allowlist.
//
// That inversion is on purpose and it is safe: the RUNTIME rule is an
// allowlist, because a denylist eventually misses something. This is a test,
// where the cost of a false positive is a failing build and the cost of a
// false negative is only that this particular tripwire did not fire. So it can
// afford to be a denylist, and it catches the one change nobody should make
// casually -- adding a field to the table gvm cannot DELETE from.
func TestNoAllowlistNamesASecret(t *testing.T) {
	banned := []string{
		"value", "password", "passwd", "secret", "token", "credential",
		"api_key", "apikey", "private_key", "seed", "totp", "master_key",
		"session", "cookie", "recovery_code",
	}
	for action, spec := range allowedParams {
		for _, p := range spec.Params {
			for _, b := range banned {
				if p == b || strings.Contains(p, "password") {
					t.Errorf("allowedParams[%q] permits %q. audit_log is append-only by grant: "+
						"the application roles hold SELECT and INSERT and nothing else, so a secret "+
						"written here cannot be deleted by the software that wrote it. "+
						"Record which secret changed (scope, name), never the value.", action, p)
				}
			}
		}
	}
}

func TestActionNamesAreSubjectDotVerb(t *testing.T) {
	for _, d := range declaredActions(t) {
		if !actionShape.MatchString(string(d.Value)) {
			t.Errorf("%s:%d: %s = %q is not <subject>.<verb> in lowercase; the action string is "+
				"grepped identically in journald and in audit_log and the two have to agree",
				d.File, d.Line, d.Name, d.Value)
		}
	}
}

func TestEveryRequiredParamIsAlsoAllowed(t *testing.T) {
	for action, spec := range allowedParams {
		for _, r := range spec.Required {
			found := false
			for _, p := range spec.Params {
				if p == r {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("allowedParams[%q] requires %q but does not allow it, so Redact drops it "+
					"and the required-field check then reports it missing on every single record",
					action, r)
			}
		}
	}
}

func TestRedactDropsUnallowlistedKeysAndSaysSo(t *testing.T) {
	got, dropped := Redact(SecretWrite, Params{
		"scope": "instance",
		"name":  "rcon",
		"value": "hunter2", // the whole point
		"note":  "irrelevant",
	})

	if _, leaked := got["value"]; leaked {
		t.Fatal("Redact kept a key that is not on the allowlist")
	}
	if got["scope"] != "instance" || got["name"] != "rcon" {
		t.Fatalf("Redact dropped allowlisted keys: %+v", got)
	}
	want := []string{"note", "value"} // sorted
	if len(dropped) != len(want) || dropped[0] != want[0] || dropped[1] != want[1] {
		t.Fatalf("dropped = %v, want %v (sorted, so the log line is stable)", dropped, want)
	}
	marker, ok := got[KeyUnallowlisted].([]string)
	if !ok || len(marker) != 2 {
		t.Fatalf("%s = %#v, want the dropped keys recorded in the record itself", KeyUnallowlisted, got[KeyUnallowlisted])
	}
}

func TestRedactOnAnUndeclaredActionDropsEverything(t *testing.T) {
	got, dropped := Redact(Action("instance.teleport"), Params{"reason": "why not"})
	if _, kept := got["reason"]; kept {
		t.Fatal("an action with no allowlist entry must keep no params: nobody has decided which of " +
			"its fields are safe to store")
	}
	if len(dropped) != 1 {
		t.Fatalf("dropped = %v, want the one key reported", dropped)
	}
}

// TestACallerCannotForgeTheUnallowlistedMarker: the marker says "these fields
// were dropped". A caller able to set it could make a dropped field look like
// it was never passed.
func TestACallerCannotForgeTheUnallowlistedMarker(t *testing.T) {
	got, _ := Redact(InstanceStart, Params{
		KeyUnallowlisted: []string{"nothing to see here"},
		"reason":         "nightly",
	})
	marker, ok := got[KeyUnallowlisted].([]string)
	if !ok {
		t.Fatalf("%s = %#v, want the audit system's own list", KeyUnallowlisted, got[KeyUnallowlisted])
	}
	if len(marker) != 1 || marker[0] != KeyUnallowlisted {
		t.Fatalf("%s = %v, want [%q]: the caller's attempt must itself be reported as dropped",
			KeyUnallowlisted, marker, KeyUnallowlisted)
	}
}

func TestRedactTruncatesAnUnboundedStringParam(t *testing.T) {
	huge := strings.Repeat("A", maxParamStringBytes*3)
	got, _ := Redact(ConsoleSend, Params{"line": huge})

	line, ok := got["line"].(string)
	if !ok {
		t.Fatalf("line = %#v", got["line"])
	}
	if len(line) >= len(huge) {
		t.Fatalf("line was not truncated: %d bytes", len(line))
	}
	if !strings.Contains(line, "truncated") {
		t.Fatal("a truncated value must say so; a reader must never think they have the whole line")
	}
}

// TestTruncationLandsOnARuneBoundary: cutting a multi-byte rune in half still
// marshals, because encoding/json substitutes U+FFFD. The damage would be a
// silently corrupted character rather than an error anyone notices.
func TestTruncationLandsOnARuneBoundary(t *testing.T) {
	// The one-byte prefix puts the cap in the middle of a two-byte rune; an
	// even-length string alone would land on a boundary by luck and prove
	// nothing.
	s := "x" + strings.Repeat("é", maxParamStringBytes)
	got := truncateParam(s).(string)

	head := got[:strings.Index(got, "…")]
	if !utf8.ValidString(head) {
		t.Fatal("truncation cut a rune in half. encoding/json substitutes U+FFFD for an invalid " +
			"byte, so this would not fail anywhere: it would silently corrupt the last character " +
			"of every truncated audit value.")
	}
	if !strings.HasSuffix(head, "é") {
		t.Fatalf("head ends %q, want a whole rune", head[len(head)-4:])
	}
}

func TestMissingRequiredParamsIsReportedSorted(t *testing.T) {
	got := missingRequired(BackupCreate, Params{"path": "/var/backups/gvm/pz-main/x.tar.zst"})
	want := []string{"bytes", "sha256"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("missingRequired = %v, want %v", got, want)
	}
	if n := len(missingRequired(Action("nope.nope"), nil)); n != 0 {
		t.Fatalf("an undeclared action requires nothing, got %d", n)
	}
}
