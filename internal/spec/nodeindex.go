package spec

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Pos is a one-based line and column inside a YAML file, as yaml.v3 reports it.
//
// The zero Pos means "not located". A diagnostic about a key the file never
// wrote still has to print, so the absence of a position is a state the
// formatter handles rather than an error the caller has to unwrap.
type Pos struct {
	Line int
	Col  int
}

// Known reports whether the position came from a real node.
func (p Pos) Known() bool { return p.Line > 0 }

// NodeIndex maps a document path such as "spec.ports[1].port" to the position of
// the node at it.
//
// Section 8.6 requires every diagnostic to read
//
//	projectzomboid.yaml:118:7: spec.control.dialect: "valve_empty_response" is
//	not a dialect (want source|minecraft|rust|simple)
//
// which means decoding twice: once into yaml.Node for positions, once into the
// structs for values. The struct decode throws the file's geometry away — by the
// time a validator can see that a dialect is wrong, nothing in the Manifest
// remembers which line it came from. This index is the other half.
//
// Paths use exactly the syntax the manifest diff emits, so a change reported by
// Diff can be looked up here and printed with the file position it came from.
type NodeIndex struct {
	// File is the name diagnostics print. It is whatever the caller wants a
	// human to see (a path, "embedded games/pz.yaml"), never re-derived here.
	File string

	entries map[string]nodeEntry
	order   []string
	nullSeq []string
}

type nodeEntry struct {
	value *yaml.Node
	key   *yaml.Node // nil for sequence elements and for the document root
}

const nullTag = "!!null"

// IndexBytes parses data for positions only. The returned error is the YAML
// parse error, which the caller will hit again on the struct decode; indexing
// first means a syntax error is reported once, from the parser, rather than
// twice in two different shapes.
func IndexBytes(file string, data []byte) (*NodeIndex, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	return IndexNode(file, &root), nil
}

// IndexNode indexes an already-parsed document.
func IndexNode(file string, root *yaml.Node) *NodeIndex {
	ix := &NodeIndex{File: file, entries: make(map[string]nodeEntry)}
	n := root
	if n != nil && n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return ix
		}
		n = n.Content[0]
	}
	if n == nil || n.Kind == 0 {
		return ix
	}
	ix.add("", n, nil)
	ix.walk("", n)
	return ix
}

func (ix *NodeIndex) add(path string, value, key *yaml.Node) {
	// First occurrence wins. yaml.v3 already refuses a duplicate mapping key, so
	// a collision here can only come from a path that two different keys spell
	// the same way (a key containing a dot), and reporting the earlier line is
	// the less confusing of the two wrong answers.
	if _, dup := ix.entries[path]; dup {
		return
	}
	ix.entries[path] = nodeEntry{value: value, key: key}
	ix.order = append(ix.order, path)
}

func (ix *NodeIndex) walk(path string, n *yaml.Node) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value == "" {
				// A complex or empty key cannot be spelled in a dotted path, so
				// indexing it would produce an address no diagnostic can print.
				continue
			}
			child := k.Value
			if path != "" {
				child = path + "." + k.Value
			}
			ix.add(child, v, k)
			ix.walk(child, v)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			child := path + "[" + strconv.Itoa(i) + "]"
			ix.add(child, c, nil)
			if c.Tag == nullTag {
				ix.nullSeq = append(ix.nullSeq, child)
			}
			ix.walk(child, c)
		}
	case yaml.AliasNode:
		// Deliberately not followed. The anchor's own content is walked at the
		// path where it was defined; following the alias would file one node
		// under two paths and would not terminate on a recursive anchor.
	}
}

// Pos returns the position of the VALUE at path.
func (ix *NodeIndex) Pos(path string) (Pos, bool) {
	if ix == nil {
		return Pos{}, false
	}
	e, ok := ix.entries[path]
	if !ok || e.value == nil {
		return Pos{}, false
	}
	return Pos{Line: e.value.Line, Col: e.value.Column}, true
}

// KeyPos returns the position of the KEY at path, which is where a diagnostic
// about a key itself (unknown, misspelled, forbidden) should point. Sequence
// elements have no key and report false.
func (ix *NodeIndex) KeyPos(path string) (Pos, bool) {
	if ix == nil {
		return Pos{}, false
	}
	e, ok := ix.entries[path]
	if !ok || e.key == nil {
		return Pos{}, false
	}
	return Pos{Line: e.key.Line, Col: e.key.Column}, true
}

// Node returns the raw node at path, for the rare check that needs the node's
// style or tag rather than its decoded value.
func (ix *NodeIndex) Node(path string) *yaml.Node {
	if ix == nil {
		return nil
	}
	return ix.entries[path].value
}

// At resolves path to the closest position the file actually has, returning the
// path that matched. A diagnostic about a MISSING key — "readiness.hard_deadline
// is required" — has no node of its own; pointing at `readiness` puts the
// operator's cursor within a screen of the fix, and pointing at nothing at all
// makes the message unactionable.
func (ix *NodeIndex) At(path string) (Pos, string) {
	if ix == nil {
		return Pos{}, ""
	}
	for p := path; ; p = parentPath(p) {
		if pos, ok := ix.Pos(p); ok {
			return pos, p
		}
		if p == "" {
			return Pos{}, ""
		}
	}
}

// Paths returns every indexed path in document order.
func (ix *NodeIndex) Paths() []string {
	if ix == nil {
		return nil
	}
	out := make([]string, len(ix.order))
	copy(out, ix.order)
	return out
}

// NullSeqEntries returns the paths of null elements in sequences.
//
// yaml.v3 SKIPS a null sequence element instead of reporting it, so
//
//	ports:
//	  - {name: game, default: 16261}
//	  -
//	  - {name: rcon, default: 27015}
//
// decodes to two ports with no error of any kind. The same hazard exists for
// argv (a dropped -servername generates a fresh world), shutdown steps, readiness
// probes and the content acquire list. Guarding each of them in its own
// UnmarshalYAML is eight places to forget; this walk sees every sequence in the
// document, so the loader can refuse the file once.
func (ix *NodeIndex) NullSeqEntries() []string {
	if ix == nil {
		return nil
	}
	out := make([]string, len(ix.nullSeq))
	copy(out, ix.nullSeq)
	return out
}

// parentPath drops the last segment of a document path: "spec.ports[1].port" ->
// "spec.ports[1]" -> "spec.ports" -> "spec" -> "".
//
// A mapping key that itself contains "." or "[" (an env var named JAVA.OPTS)
// splits in the wrong place. That costs a fallback position a few lines of
// accuracy and is not worth an escaping scheme in a path an operator has to read.
func parentPath(path string) string {
	i := strings.LastIndexAny(path, ".[")
	if i < 0 {
		return ""
	}
	return path[:i]
}
