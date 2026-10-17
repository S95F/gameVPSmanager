package reconcile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Phase 39's intent writer: the one place that CHANGES an instance's YAML in
// response to a verb -- set, enable/disable, ports move, job pause, manifest
// upgrade -- rather than converging the host from it.
//
// Everything here edits the file as a DOCUMENT, through yaml.v3's Node API, so
// every comment and every untouched line survives. That is the same discipline
// the content writer uses for spec.content.<block>.acquire, generalised from
// one hard-coded path to any dotted key: the reasoning behind a hand-tuned
// value -- why max_players is 18 and not 20, which heap size stopped the OOM --
// lives in the comment beside it, and re-marshalling the decoded struct would
// discard all of it on every edit while reporting success. `gvm apply` still
// writes the file's own bytes back; this is the only writer that rewrites a
// value inside a document a human may have edited, which is why it is surgical.
//
// It does not write anything itself. It produces bytes and a digest; a verb
// reads the current file, checks the digest it read against the one here, calls
// EditInstanceDoc, and writes the result through hostops VerbFileWrite
// (FileInstanceSpec) like every other privileged write. Keeping the editing
// pure is what lets the whole of it be argued with from a literal string in a
// test.

// ErrSpecChanged is returned when the file on disk no longer matches the digest
// the caller read, so a writer refuses rather than overwrite a hand edit made
// in between. It is its own sentinel because the operator's next move -- re-read
// and try again -- differs from a validation failure.
var ErrSpecChanged = errors.New("reconcile: the instance file changed since it was read")

// SpecDigest is the If-Match value: the sha256 of the file's exact bytes. A
// verb computes it when it reads the file and checks it again just before it
// writes, so a concurrent hand edit between the two is caught instead of
// silently clobbered.
func SpecDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// CheckIfMatch reports whether current still carries the digest a caller read.
// An empty baseline is a deliberate opt-out (a verb that did not capture one),
// not a match against nothing.
func CheckIfMatch(current []byte, want string) error {
	if want == "" {
		return nil
	}
	if SpecDigest(current) != want {
		return ErrSpecChanged
	}
	return nil
}

// SpecEdit sets one scalar, addressed by its dotted path from the document root
// -- {"spec","vars","max_players"} for spec.vars.max_players. Missing mapping
// nodes along the path are grown (a vars block a create never wrote), and the
// leaf's own comments are kept.
type SpecEdit struct {
	Path  []string
	Value string
	// Tag forces the YAML tag ("!!str", "!!int", "!!bool"). Empty infers it
	// from Value, which is right for a human-typed number or bool; a verb that
	// knows a numeric-looking value is really a string (a world name "2024")
	// sets "!!str" so it is not written as an int.
	Tag string
}

// EditInstanceDoc applies edits to raw instance YAML and returns the re-encoded
// bytes, leaving every comment and every node it did not touch exactly as it
// found them.
//
// It refuses rather than guess when the document is not a mapping, when a path
// passes through a node that is not a mapping, or when the leaf it would set
// already holds a mapping or a sequence -- a hand-mangled file, or a path that
// names a block and not a value. A refusal here is a verb that stops with a
// sentence, which is always better than a write that quietly restructured the
// file.
func EditInstanceDoc(raw []byte, edits []SpecEdit) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("reconcile: reading the instance file as a document: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("reconcile: the instance file is not a YAML mapping")
	}
	root := doc.Content[0]
	for _, e := range edits {
		if len(e.Path) == 0 {
			return nil, errors.New("reconcile: an edit names no key")
		}
		if err := setScalarAtPath(root, e); err != nil {
			return nil, err
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	// The same indent spec.MarshalYAML uses, so a gvm-written file and a
	// hand-written one do not differ cosmetically.
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func setScalarAtPath(root *yaml.Node, e SpecEdit) error {
	n := root
	for _, key := range e.Path[:len(e.Path)-1] {
		child := mapChildNode(n, key)
		if child == nil {
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			n.Content = append(n.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
		}
		if child.Kind != yaml.MappingNode {
			return fmt.Errorf("reconcile: %q in %s is a %s, not a mapping, so the value cannot be set under it",
				key, strings.Join(e.Path, "."), kindName(child.Kind))
		}
		n = child
	}

	tag := e.Tag
	if tag == "" {
		tag = inferScalarTag(e.Value)
	}
	leafKey := e.Path[len(e.Path)-1]
	leaf := mapChildNode(n, leafKey)
	if leaf == nil {
		n.Content = append(n.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: leafKey},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: e.Value})
		return nil
	}
	if leaf.Kind != yaml.ScalarNode {
		return fmt.Errorf("reconcile: %s names a %s, not a value", strings.Join(e.Path, "."), kindName(leaf.Kind))
	}
	// Change the value in place, keeping the node's comments. Style is cleared so
	// a value that no longer needs the old node's quoting is re-emitted plainly.
	leaf.Value = e.Value
	leaf.Tag = tag
	leaf.Style = 0
	return nil
}

// inferScalarTag picks the tag a human would have written for a bare value: a
// bool for true/false, an int for a plain integer, a string otherwise.
func inferScalarTag(value string) string {
	switch value {
	case "true", "false":
		return "!!bool"
	}
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return "!!int"
	}
	return "!!str"
}

func mapChildNode(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.ScalarNode:
		return "value"
	default:
		return "node"
	}
}
