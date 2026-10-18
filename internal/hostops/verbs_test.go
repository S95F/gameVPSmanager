package hostops

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/audit"
)

// declaredVerbConstants reads the Verb constants out of verbs.go's SOURCE.
//
// This is the third leg of the closure assertion and the only one that cannot
// be satisfied by a mistake agreeing with itself. Comparing the declaration
// map against the dispatch map proves they match each other; it proves nothing
// about whether either is spelled with the constants. A handler keyed by
// Verb("unit.kill") -- or a declaration for a verb whose constant was deleted
// -- passes a map-to-map comparison and fails here.
func declaredVerbConstants(t *testing.T) map[string]Verb {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "verbs.go", nil, 0)
	if err != nil {
		t.Fatalf("parse verbs.go: %v", err)
	}

	out := map[string]Verb{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != "Verb" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					t.Fatalf("%s has no value; a Verb constant declared by iota would have "+
						"no wire representation at all", name.Name)
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s is not a string literal; the verb set has to be readable "+
						"from the source for this test to mean anything", name.Name)
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", name.Name, err)
				}
				out[name.Name] = Verb(val)
			}
		}
	}
	// A parse that found nothing would pass every assertion below in silence.
	if len(out) < 10 {
		t.Fatalf("found only %d Verb constants in verbs.go; the source scan is broken", len(out))
	}
	return out
}

func sortedVerbs(m map[Verb]struct{}) []Verb {
	out := make([]Verb, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func setOf[T comparable, V any](m map[T]V) map[T]struct{} {
	out := make(map[T]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

// TestVerbEnumIsClosed is the assertion the two-daemon split is worth having.
//
// gvmd runs as root and answers a socket. A handler that can be invoked but
// was never declared is an undeclared privileged operation: it is absent from
// the capability listing, absent from `gvm doctor`, absent from any review of
// "what can the web tier ask for", and reachable anyway. Equally, a declared
// verb with no handler is a nil function pointer inside a root daemon.
func TestVerbEnumIsClosed(t *testing.T) {
	constants := declaredVerbConstants(t)

	byValue := map[Verb]string{}
	for name, v := range constants {
		if prev, dup := byValue[v]; dup {
			t.Fatalf("%s and %s are both %q; two constants for one verb make the "+
				"declaration table's key ambiguous", prev, name, v)
		}
		byValue[v] = name
	}

	declared := setOf(verbs)
	dispatch := setOf(handlers)
	fromSource := setOf(byValue)

	for _, v := range sortedVerbs(declared) {
		if _, ok := dispatch[v]; !ok {
			t.Errorf("%q is declared with no handler: Do would dereference a nil function", v)
		}
		if _, ok := fromSource[v]; !ok {
			t.Errorf("%q is declared but is not one of verbs.go's Verb constants", v)
		}
	}
	for _, v := range sortedVerbs(dispatch) {
		if _, ok := declared[v]; !ok {
			t.Errorf("%q has a handler and no declaration: an undeclared privileged operation", v)
		}
	}
	for _, v := range sortedVerbs(fromSource) {
		if _, ok := declared[v]; !ok {
			t.Errorf("the constant %s = %q has no entry in the declaration table",
				byValue[v], v)
		}
	}
}

// TestEveryMutatingVerbIsAuditable is the other half of the closure.
//
// A verb that changes the host and records nothing is worse than an undeclared
// one: it is in the listing, so a review of the surface sees it and concludes
// it is covered.
func TestEveryMutatingVerbIsAuditable(t *testing.T) {
	for _, d := range Declarations() {
		switch d.Effect {
		case Mutates:
			if d.Audit == "" {
				t.Errorf("%q changes the host and declares no audit action", d.Verb)
				continue
			}
			if !audit.Known(d.Audit) {
				t.Errorf("%q is audited as %q, which has no params allowlist in "+
					"internal/audit; every param it carries would be dropped", d.Verb, d.Audit)
			}
		case Observes:
			// Observing verbs carry no action deliberately. Giving them one
			// would mean either inventing an action the register does not
			// have, or filing a dashboard poll under an action that means
			// something else -- and audit_log is append-only by grant, so
			// noise written into it cannot be removed.
			if d.Audit != "" {
				t.Errorf("%q only observes but declares audit action %q", d.Verb, d.Audit)
			}
		default:
			t.Errorf("%q has no effect declared", d.Verb)
		}
	}
}

// TestARestartIsAuditedAsARestartNotAStart is Phase 37 item 6. A restart takes
// the server down and back up; a row that said "started" would hide the gap an
// operator reading the trail after a blip is trying to account for.
func TestARestartIsAuditedAsARestartNotAStart(t *testing.T) {
	for _, d := range Declarations() {
		if d.Verb == VerbUnitRestart {
			if d.Audit != audit.InstanceRestart {
				t.Errorf("VerbUnitRestart is audited as %q, want %q (a restart is not a start)",
					d.Audit, audit.InstanceRestart)
			}
			return
		}
	}
	t.Fatal("VerbUnitRestart has no declaration")
}

func TestVerbNamesAreWellFormed(t *testing.T) {
	for _, d := range Declarations() {
		if err := validVerbName(d.Verb); err != nil {
			t.Errorf("%v", err)
		}
		if d.Summary == "" {
			t.Errorf("%q has no summary; an enumerable surface nobody can read is not "+
				"much better than an unenumerable one", d.Verb)
		}
	}
}

// TestTheDangerousPrimitivesAreNotVerbs pins the shape of the enum, not just
// its consistency.
//
// Each name below is an operation this package's implementation can perform
// and that is deliberately not reachable over the socket. They are listed
// explicitly so that adding one is a decision somebody makes on purpose, in a
// diff that also deletes a line from this test, rather than a table entry that
// slips through review.
func TestTheDangerousPrimitivesAreNotVerbs(t *testing.T) {
	forbidden := map[Verb]string{
		"fs.write":      "writing an arbitrary path as root is the whole privilege, wearing an enum's name",
		"path.chown":    "chowning an arbitrary path hands over any file on the box",
		"group.member":  "adding an account to an arbitrary group is escalation by way of the sudo group",
		"user.modify":   "an adopted instance's account predates gvm and may be shared",
		"unit.any":      "starting an arbitrary unit is a root shell with extra steps",
		"secret.write":  "a credential must not travel inside a generic request struct",
		"command.run":   "a verb enum with an escape hatch in it is decorative",
		"systemd.run":   "transient units carry their own hardening and belong in internal/jobs",
		"file.readany":  "reading an arbitrary path leaks /etc/gvm/master.key",
		"dir.removeall": "recursive removal is internal/archive's chokepoint and requires a verified Ref",
	}
	for v, why := range forbidden {
		if _, ok := Lookup(v); ok {
			t.Errorf("%q is declared; %s", v, why)
		}
	}
}

func TestLookupAndVerbsAgree(t *testing.T) {
	list := Verbs()
	if len(list) != len(verbs) {
		t.Fatalf("Verbs() returned %d of %d declarations", len(list), len(verbs))
	}
	if !sort.SliceIsSorted(list, func(i, j int) bool { return list[i] < list[j] }) {
		t.Error("Verbs() is not sorted; the capability listing would reorder between builds")
	}
	for _, v := range list {
		d, ok := Lookup(v)
		if !ok {
			t.Fatalf("Verbs() returned %q, which Lookup does not know", v)
		}
		if d.Verb != v {
			t.Errorf("declaration keyed %q carries Verb %q", v, d.Verb)
		}
	}
	if _, ok := Lookup("unit.obliterate"); ok {
		t.Error("Lookup accepted a verb nobody declared")
	}
}

func TestValidVerbNameRejectsMalformedNames(t *testing.T) {
	for _, bad := range []Verb{"", "unitstart", "unit.", ".start", "Unit.Start", "unit.start.now", "unit.start "} {
		if err := validVerbName(bad); err == nil {
			t.Errorf("%q was accepted as a verb name", bad)
		}
	}
	for _, good := range []Verb{"unit.start", "daemon.reload", "job_timer.write"} {
		if err := validVerbName(good); err != nil {
			t.Errorf("%q was rejected: %v", good, err)
		}
	}
}

// TestEveryDeclaredArgBitIsDefined is the third closed set in this file.
//
// A declaration carrying a bit nobody named would be enforced by validate --
// which refuses everything the bit does not cover -- and would print an empty
// field name when it refused, so the operator would be told their request set
// "" and could not act on it.
func TestEveryDeclaredArgBitIsDefined(t *testing.T) {
	var all Args
	for bit := range argNames {
		if bit == 0 || bit&(bit-1) != 0 {
			t.Errorf("argNames contains %d, which is not a single bit", bit)
		}
		if all.Has(bit) {
			t.Errorf("%d appears twice in argNames", bit)
		}
		all |= bit
	}
	for _, d := range Declarations() {
		if stray := d.Args &^ all; stray != 0 {
			t.Errorf("%q declares undefined arg bits %d", d.Verb, stray)
		}
	}
}

// TestOnlyFileWriteReadsABody: a body is the one field that carries content
// rather than addressing, and content that reaches a verb which ignores it is
// a caller that believes it wrote a file and did not.
func TestOnlyFileWriteReadsABody(t *testing.T) {
	for _, d := range Declarations() {
		if d.Args.Has(ArgBody) && d.Verb != VerbFileWrite {
			t.Errorf("%q reads a body", d.Verb)
		}
	}
	if !verbs[VerbFileWrite].Args.Has(ArgBody) {
		t.Error("file.write does not read a body, so every write would be empty")
	}
}
