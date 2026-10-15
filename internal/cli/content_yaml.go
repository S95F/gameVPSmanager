package cli

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/S95F/gameVPSmanager/internal/content"
)

// Editing the instance YAML as a DOCUMENT rather than re-serialising the struct.
//
// ARCHITECTURE 6.1, directly under its example instance YAML -- whose acquire
// list carries `# --- Libraries / frameworks (must load first) ---` and
// `# --- Vehicles ---` headers -- says the file "is read and written through
// gopkg.in/yaml.v3's Node API so head comments survive an automated collection
// sync, exactly as mods.txt's section headers do today". Section 12.5 adds that
// a collection sync appends new items under a dated head comment, and
// steamworkshop.Merge's own doc comment defers that comment to "the writer's
// job: this function produces refs, and a YAML comment needs the YAML node".
//
// This is that writer. It matters because load order is hand-tuned and the
// REASONING behind it lives only in those comments: which mod broke which other
// mod, on which build, in which order someone found worked. gvm cannot see any
// of it, cannot reconstruct it, and re-marshalling the decoded struct discarded
// all of it on every `gvm content add`, `remove`, `move` and `collection sync`
// -- while reporting success. The one `.bak` hostops keeps meant the comments
// survived exactly one further edit, and collection sync is both the verb most
// likely to run twice in a row and the verb 12.5 specifically names.
//
// It is also the only place in the tree that rewrites a document gvm did not
// author, which is why it is surgical: `gvm apply` writes the file's own bytes
// back and D-04 writes the pinned manifest verbatim.

// editAcquireSequence replaces spec.content.<block>.acquire in raw, leaving
// every other node -- and every comment on it -- alone.
//
// The second return value is false when the document has no such path, which
// is the ordinary post-create state: `gvm create` writes no content block at
// all, so `gvm content add` on a fresh instance has nothing to edit and the
// caller falls back to a full marshal. That is a fallback and not a failure.
//
// added names the refs that were appended rather than kept, and header is the
// dated head comment to render above the first of them. Both are empty for the
// verbs that do not append.
func editAcquireSequence(raw []byte, block, source string, refs []content.Ref,
	added []content.AcquireID, header string) ([]byte, bool, error) {

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, false, fmt.Errorf("re-reading the instance file as a document: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		// An empty or non-mapping document is the only thing that falls back to
		// a full marshal; everything else is edited (or grown) in place.
		return nil, false, nil
	}

	// The ordinary post-create state has no content block at all: `gvm create`
	// writes none. The first `gvm content add` therefore GROWS the document —
	// spec -> content -> <block> -> acquire — inside the parsed node rather than
	// re-serialising the struct, so the operator's section comments and every
	// other hand-written line survive the first edit (content-mods-4). Only an
	// existing node of the wrong kind (a hand-mangled file) still falls back.
	root := doc.Content[0]
	specNode, ok := mapGetOrCreate(root, "spec", yaml.MappingNode)
	if !ok {
		return nil, false, nil
	}
	contentNode, ok := mapGetOrCreate(specNode, "content", yaml.MappingNode)
	if !ok {
		return nil, false, nil
	}
	blockNode, ok := mapGetOrCreate(contentNode, block, yaml.MappingNode)
	if !ok {
		return nil, false, nil
	}
	acquire, ok := mapGetOrCreate(blockNode, "acquire", yaml.SequenceNode)
	if !ok {
		return nil, false, nil
	}

	// The comments, keyed by acquisition id, before anything is replaced. Keyed
	// by id and not by position so that `gvm content move` carries an item's
	// own comment along with the line -- which is what makes a move readable in
	// `gvm diff` -- and so a removed item takes its comment with it rather than
	// leaving it stranded above whatever moved up into its place.
	type comments struct{ head, line, foot string }
	old := make(map[content.AcquireID]comments, len(acquire.Content))
	for _, item := range acquire.Content {
		id := content.AcquireID(strings.TrimSpace(scalarAt(item, "id")))
		if id == "" {
			continue
		}
		old[id] = comments{item.HeadComment, item.LineComment, item.FootComment}
	}

	appended := make(map[content.AcquireID]bool, len(added))
	for _, id := range added {
		appended[id] = true
	}

	next := make([]*yaml.Node, 0, len(refs))
	firstAppended := true
	for _, ref := range refs {
		n, err := refNode(ref)
		if err != nil {
			return nil, false, err
		}
		id := content.RefID(ref)
		if c, ok := old[id]; ok {
			n.HeadComment, n.LineComment, n.FootComment = c.head, c.line, c.foot
		} else if header != "" && appended[id] && firstAppended {
			n.HeadComment = header
			firstAppended = false
		}
		next = append(next, n)
	}
	acquire.Content = next

	// `source:` is recorded on the way out, as the struct writer did, so the
	// written YAML says which namespace these ids are in rather than relying on
	// the manifest's block order. Only when the document does not already say
	// so: this edit never overwrites what the operator wrote.
	if source != "" && mapValue(blockNode, "source") == nil {
		blockNode.Content = append(blockNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "source"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: source})
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	// The same settings spec.MarshalYAML uses, so one gvm-written file and one
	// hand-written file still do not differ cosmetically.
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := enc.Close(); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

// refNode renders one content.Ref as the mapping node that goes in the
// sequence, through the same yaml tags the struct writer used.
func refNode(ref content.Ref) (*yaml.Node, error) {
	body, err := yaml.Marshal(ref)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("rendering item %q: yaml produced %d node(s)", ref.ID,
			len(doc.Content))
	}
	return doc.Content[0], nil
}

// mapGetOrCreate returns the value node for key, creating it as an empty node
// of the given kind when it is absent. ok is false when n is not a mapping, or
// when key exists with a different kind than wanted — a hand-mangled document
// the caller should fall back to a full marshal for rather than edit blindly.
func mapGetOrCreate(n *yaml.Node, key string, kind yaml.Kind) (*yaml.Node, bool) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, false
	}
	if v := mapValue(n, key); v != nil {
		if v.Kind != kind {
			return nil, false
		}
		return v, true
	}
	val := &yaml.Node{Kind: kind}
	switch kind {
	case yaml.MappingNode:
		val.Tag = "!!map"
	case yaml.SequenceNode:
		val.Tag = "!!seq"
	}
	n.Content = append(n.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		val)
	return val, true
}

// mapValue is one key's value node, or nil. Nil-tolerant so a walk down a path
// that does not exist reads as one expression rather than four.
func mapValue(n *yaml.Node, key string) *yaml.Node {
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

// scalarAt is one mapping key's scalar value.
func scalarAt(n *yaml.Node, key string) string {
	v := mapValue(n, key)
	if v == nil || v.Kind != yaml.ScalarNode {
		return ""
	}
	return v.Value
}

// datedHeadComment is the header section 12.5 asks for above an appended block.
//
// It names the collection as well as the date, because the next operator's
// question about an id they do not recognise is where it came from, and
// "appended by a sync on 2026-09-05" does not answer it.
func datedHeadComment(collection, date string, added []content.AcquireID) string {
	if len(added) == 0 {
		return ""
	}
	return fmt.Sprintf("--- appended %s from collection %s (%d item(s)) ---\n"+
		"a new item's load position is unknowable from a collection, so these went at the "+
		"end;\nmove one up by hand if it turns out to be a library: gvm content move", date,
		collection, len(added))
}
