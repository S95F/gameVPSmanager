package spec

import (
	"reflect"
	"testing"
)

// posDoc's line and column numbers are asserted literally below, so the first
// character of `apiVersion` is line 1, column 1. Do not reindent it.
const posDoc = `apiVersion: gvm/v1
kind: Game
metadata:
  id: projectzomboid
spec:
  ports:
    - name: game
      default: 16261
    - name: rcon
      default: 27015
  control:
    dialect: simple
  runtime:
    args:
      - "-cachedir=/srv"
      - {value: "-adminpassword", when: {phase: install}}
`

func TestIndexResolvesNestedPaths(t *testing.T) {
	ix, err := IndexBytes("projectzomboid.yaml", []byte(posDoc))
	if err != nil {
		t.Fatalf("IndexBytes: %v", err)
	}

	tests := []struct {
		path    string
		wantPos Pos
		wantKey Pos
	}{
		{"apiVersion", Pos{1, 13}, Pos{1, 1}},
		{"metadata.id", Pos{4, 7}, Pos{4, 3}},
		{"spec.ports", Pos{7, 5}, Pos{6, 3}},
		{"spec.ports[0].name", Pos{7, 13}, Pos{7, 7}},
		{"spec.ports[1].default", Pos{10, 16}, Pos{10, 7}},
		{"spec.control.dialect", Pos{12, 14}, Pos{12, 5}},
		{"spec.runtime.args[0]", Pos{15, 9}, Pos{}},
		{"spec.runtime.args[1].when.phase", Pos{16, 49}, Pos{16, 42}},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got, ok := ix.Pos(tc.path)
			if !ok {
				t.Fatalf("Pos(%q) not indexed; indexed paths: %v", tc.path, ix.Paths())
			}
			if got != tc.wantPos {
				t.Errorf("Pos(%q) = %+v, want %+v", tc.path, got, tc.wantPos)
			}
			key, hasKey := ix.KeyPos(tc.path)
			if tc.wantKey == (Pos{}) {
				if hasKey {
					t.Errorf("KeyPos(%q) = %+v, want none: a sequence element has no key", tc.path, key)
				}
				return
			}
			if !hasKey || key != tc.wantKey {
				t.Errorf("KeyPos(%q) = %+v (%v), want %+v", tc.path, key, hasKey, tc.wantKey)
			}
		})
	}
}

func TestIndexAtFallsBackToTheNearestAncestor(t *testing.T) {
	ix, err := IndexBytes("projectzomboid.yaml", []byte(posDoc))
	if err != nil {
		t.Fatalf("IndexBytes: %v", err)
	}

	tests := []struct {
		name     string
		path     string
		wantPath string
		wantPos  Pos
	}{
		// A diagnostic about a REQUIRED key the file never wrote still has to
		// point somewhere the operator can act on.
		{"missing leaf", "spec.control.password", "spec.control", Pos{12, 5}},
		{"missing block", "spec.readiness.hard_deadline", "spec", Pos{6, 3}},
		{"missing element field", "spec.ports[1].expose", "spec.ports[1]", Pos{9, 7}},
		{"nothing in common", "totally.absent", "", Pos{1, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pos, path := ix.At(tc.path)
			if path != tc.wantPath {
				t.Errorf("At(%q) matched %q, want %q", tc.path, path, tc.wantPath)
			}
			if pos != tc.wantPos {
				t.Errorf("At(%q) = %+v, want %+v", tc.path, pos, tc.wantPos)
			}
		})
	}
}

func TestIndexReportsNullSequenceElements(t *testing.T) {
	// yaml.v3 drops each of these silently, so nothing downstream can tell that
	// the operator wrote four ports and got three.
	const doc = `spec:
  ports:
    - {name: game}
    -
    - {name: rcon}
  runtime:
    args:
      - "-servername"
      -
`
	ix, err := IndexBytes("bad.yaml", []byte(doc))
	if err != nil {
		t.Fatalf("IndexBytes: %v", err)
	}
	got := ix.NullSeqEntries()
	want := []string{"spec.ports[1]", "spec.runtime.args[1]"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NullSeqEntries() = %v, want %v", got, want)
	}
	if pos, _ := ix.Pos("spec.ports[1]"); pos.Line != 4 {
		t.Errorf("null element position = %+v, want line 4", pos)
	}
}

func TestIndexDoesNotFollowAliases(t *testing.T) {
	// Following an alias would file one node under two paths, and would not
	// terminate on a recursive anchor. The alias itself is still positioned, so
	// a diagnostic about the merged value points at the reference the operator
	// wrote.
	const doc = `defaults: &d
  mode: pty
runtime:
  console: *d
`
	ix, err := IndexBytes("alias.yaml", []byte(doc))
	if err != nil {
		t.Fatalf("IndexBytes: %v", err)
	}
	if pos, ok := ix.Pos("runtime.console"); !ok || pos.Line != 4 {
		t.Errorf("Pos(runtime.console) = %+v (%v), want line 4", pos, ok)
	}
	if _, ok := ix.Pos("runtime.console.mode"); ok {
		t.Error("Pos(runtime.console.mode) resolved: aliases must not be expanded into the index")
	}
	if pos, ok := ix.Pos("defaults.mode"); !ok || pos.Line != 2 {
		t.Errorf("Pos(defaults.mode) = %+v (%v), want line 2: the anchor's own path is indexed", pos, ok)
	}
}

func TestIndexHandlesDegenerateDocuments(t *testing.T) {
	tests := []struct{ name, doc string }{
		{"empty", ""},
		{"comment only", "# nothing here\n"},
		{"scalar document", "just-a-string\n"},
		{"root sequence", "- a\n- b\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ix, err := IndexBytes("x.yaml", []byte(tc.doc))
			if err != nil {
				t.Fatalf("IndexBytes: %v", err)
			}
			// The contract these share: nothing panics and every lookup is
			// answerable, because a diagnostic about an empty file still prints.
			if _, ok := ix.Pos("spec.anything"); ok {
				t.Error("Pos resolved a path that cannot exist")
			}
			ix.At("spec.anything")
		})
	}
}

func TestNilIndexIsUsable(t *testing.T) {
	// Diags are built before a file has been parsed (an unreadable layer), and
	// that path must not need a nil check at every call site.
	var ix *NodeIndex
	if _, ok := ix.Pos("a"); ok {
		t.Error("nil index resolved a path")
	}
	if d := ix.Errorf("spec.x", "boom"); d.Line != 0 || d.Msg != "boom" {
		t.Errorf("nil index Errorf = %+v", d)
	}
}

func TestParentPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"spec.ports[1].port", "spec.ports[1]"},
		{"spec.ports[1]", "spec.ports"},
		{"spec.ports", "spec"},
		{"spec", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := parentPath(tc.in); got != tc.want {
			t.Errorf("parentPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
