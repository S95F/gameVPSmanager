package gamecfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// JSONDoc edits a JSON configuration file — Enshrouded's and V Rising's — by
// dotted path.
//
// It is order-preserving because it never rebuilds the document: every read
// and write is expressed against byte offsets in the original file, so key
// order, indentation, blank lines and any trailing text survive untouched. A
// map[string]any round trip would reorder every key on a write, which turns a
// one-setting change into a whole-file diff and makes the drift check useless.
//
// Offsets come from encoding/json's own decoder rather than a hand-written
// scanner, so string escapes, number forms and malformed input are the
// standard library's problem and not a second parser to get subtly wrong.
type JSONDoc struct {
	raw []byte
}

type jsonKind int

const (
	jsonScalarString jsonKind = iota
	jsonScalarNumber
	jsonScalarBool
	jsonScalarNull
	jsonObject
	jsonArray
)

// jsonMember is one key in one object, located in the raw bytes.
type jsonMember struct {
	path string
	kind jsonKind
	// keyStart is the opening quote of the key; valStart and valEnd bracket
	// the value exactly as written.
	keyStart         int
	valStart, valEnd int
	// parent is the dotted path of the containing object, "" at the root.
	parent string
}

// ParseJSON reads a JSON document. Unlike the line-oriented codecs it CAN
// fail: a config file that is not valid JSON is one the game could not read
// either, and reporting that is more useful than silently editing nothing.
func ParseJSON(data []byte) (*JSONDoc, error) {
	d := &JSONDoc{raw: append([]byte{}, data...)}
	if _, err := d.scan(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *JSONDoc) scan() ([]jsonMember, error) {
	dec := json.NewDecoder(bytes.NewReader(d.raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("gamecfg: json_path: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("gamecfg: json_path: the document's top level must be an object")
	}
	var out []jsonMember
	if err := d.walkObject(dec, "", &out); err != nil {
		return nil, fmt.Errorf("gamecfg: json_path: %w", err)
	}
	return out, nil
}

// walkObject consumes members up to and including the object's closing brace.
func (d *JSONDoc) walkObject(dec *json.Decoder, prefix string, out *[]jsonMember) error {
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("expected an object key, got %v", tok)
		}
		afterKey := int(dec.InputOffset())

		m := jsonMember{
			path:     joinPath(prefix, key),
			parent:   prefix,
			keyStart: jsonStringStart(d.raw, afterKey),
			valStart: jsonValueStart(d.raw, afterKey),
		}
		idx := len(*out)
		*out = append(*out, m)

		vt, err := dec.Token()
		if err != nil {
			return err
		}
		switch v := vt.(type) {
		case json.Delim:
			switch v {
			case '{':
				(*out)[idx].kind = jsonObject
				if err := d.walkObject(dec, m.path, out); err != nil {
					return err
				}
			case '[':
				(*out)[idx].kind = jsonArray
				if err := skipJSONArray(dec); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unexpected %q", v)
			}
		case string:
			(*out)[idx].kind = jsonScalarString
		case json.Number:
			(*out)[idx].kind = jsonScalarNumber
		case bool:
			(*out)[idx].kind = jsonScalarBool
		case nil:
			(*out)[idx].kind = jsonScalarNull
		}
		(*out)[idx].valEnd = int(dec.InputOffset())
	}
	_, err := dec.Token() // the closing '}'
	return err
}

// skipJSONArray consumes an array's elements and its closing bracket. Array
// elements are not addressable by a dotted path, so their contents are only
// skipped, never indexed.
func skipJSONArray(dec *json.Decoder) error {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '[', '{':
				depth++
			case ']', '}':
				depth--
			}
		}
	}
	return nil
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// jsonStringStart walks back from the offset just past a string's closing
// quote to the offset of its opening quote.
func jsonStringStart(raw []byte, afterEnd int) int {
	for i := afterEnd - 2; i >= 0; i-- {
		if raw[i] != '"' {
			continue
		}
		slashes := 0
		for j := i - 1; j >= 0 && raw[j] == '\\'; j-- {
			slashes++
		}
		if slashes%2 == 0 {
			return i
		}
	}
	return afterEnd - 1
}

// jsonValueStart walks forward from just past a key to the first byte of its
// value.
func jsonValueStart(raw []byte, afterKey int) int {
	i := afterKey
	for i < len(raw) && isJSONSpace(raw[i]) {
		i++
	}
	if i < len(raw) && raw[i] == ':' {
		i++
	}
	for i < len(raw) && isJSONSpace(raw[i]) {
		i++
	}
	return i
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func (d *JSONDoc) find(path string) (jsonMember, bool) {
	members, err := d.scan()
	if err != nil {
		return jsonMember{}, false
	}
	var found jsonMember
	ok := false
	for _, m := range members {
		if m.path == path {
			// The last duplicate is the one a decoder keeps, so it is the one
			// the game is running on.
			found, ok = m, true
		}
	}
	return found, ok
}

// Get returns a scalar's value: a string with its escapes resolved, and any
// other scalar as its literal text.
func (d *JSONDoc) Get(path string) (string, bool) {
	m, ok := d.find(path)
	if !ok {
		return "", false
	}
	lit := string(d.raw[m.valStart:m.valEnd])
	if m.kind == jsonScalarString {
		var s string
		if err := json.Unmarshal([]byte(lit), &s); err != nil {
			return "", false
		}
		return s, true
	}
	if m.kind == jsonObject || m.kind == jsonArray {
		return "", false
	}
	return lit, true
}

// Set assigns a scalar, keeping the JSON type the file already used.
//
// Type is copied rather than inferred because the two are not interchangeable
// to the game: a slot holding 16 is a count and a slot holding "16" is a name,
// and a server that reads the wrong one either ignores the setting or refuses
// to start. A value that does not fit the existing type is refused, not
// coerced.
func (d *JSONDoc) Set(path, value string) error {
	if err := (jsonPathCodec{}).KeySyntax(path); err != nil {
		return err
	}
	m, ok := d.find(path)
	if !ok {
		return d.insert(path, value)
	}

	var lit string
	switch m.kind {
	case jsonScalarString:
		lit = jsonQuote(value)
	case jsonScalarNumber:
		if !isJSONNumber(value) {
			return fmt.Errorf("%w: %s holds a number and %q is not one", ErrUnsupported, path, value)
		}
		lit = value
	case jsonScalarBool:
		if value != "true" && value != "false" {
			return fmt.Errorf("%w: %s holds a boolean and %q is not true or false", ErrUnsupported, path, value)
		}
		lit = value
	case jsonScalarNull:
		lit = inferJSONLiteral(value)
	default:
		return fmt.Errorf("%w: %s holds an object or array, not a value", ErrUnsupported, path)
	}

	d.splice(m.valStart, m.valEnd, lit)
	return nil
}

// insert adds a key to an object that already exists.
//
// It never creates the intermediate objects of a path. An object the game did
// not write is a shape the game does not read, so inventing "world.limits"
// because the manifest addressed "world.limits.max" would produce a file that
// parses, applies nothing, and reports no error anywhere.
func (d *JSONDoc) insert(path, value string) error {
	parent, leaf := splitParent(path)
	members, err := d.scan()
	if err != nil {
		return err
	}

	openAt, closeAt := 0, len(d.raw)
	if parent != "" {
		found := false
		for _, m := range members {
			if m.path == parent && m.kind == jsonObject {
				openAt, closeAt, found = m.valStart, m.valEnd, true
			}
		}
		if !found {
			return fmt.Errorf("%w: %s (its parent object %q is not in the file)", ErrKeyNotFound, path, parent)
		}
	} else {
		openAt = bytes.IndexByte(d.raw, '{')
		closeAt = jsonRootEnd(d.raw)
	}

	entry := jsonQuote(leaf) + ": " + inferJSONLiteral(value)

	// Sit the new key after the object's last member, so a file that groups
	// related keys together keeps its grouping.
	lastEnd, lastIndent := -1, ""
	for _, m := range members {
		if m.parent == parent && m.valEnd > lastEnd && m.valEnd <= closeAt && m.keyStart >= openAt {
			lastEnd, lastIndent = m.valEnd, lineIndent(d.raw, m.keyStart)
		}
	}
	if lastEnd >= 0 {
		d.splice(lastEnd, lastEnd, ",\n"+lastIndent+entry)
		return nil
	}

	// An empty object has no sibling to copy, so this is the one place the
	// codec picks an indentation of its own.
	indent := lineIndent(d.raw, openAt)
	d.splice(openAt+1, closeAt-1, "\n"+indent+"  "+entry+"\n"+indent)
	return nil
}

// jsonRootEnd returns the offset just past the root object's closing brace.
func jsonRootEnd(raw []byte) int {
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] == '}' {
			return i + 1
		}
	}
	return len(raw)
}

// lineIndent returns the leading whitespace of the line containing off.
func lineIndent(raw []byte, off int) string {
	start := off
	for start > 0 && raw[start-1] != '\n' {
		start--
	}
	i := start
	for i < off && (raw[i] == ' ' || raw[i] == '\t') {
		i++
	}
	return string(raw[start:i])
}

func (d *JSONDoc) splice(start, end int, text string) {
	d.raw = splice(d.raw, start, end, text)
}

// Delete removes a key and its value, and the separating comma that would
// otherwise be left dangling.
func (d *JSONDoc) Delete(path string) int {
	n := 0
	for {
		m, ok := d.find(path)
		if !ok {
			return n
		}
		start, end := m.keyStart, m.valEnd
		for start > 0 && (d.raw[start-1] == ' ' || d.raw[start-1] == '\t') {
			start--
		}

		if after := nextNonSpace(d.raw, end); after < len(d.raw) && d.raw[after] == ',' {
			end = after + 1
			if nl := restOfLine(d.raw, end); nl > end {
				end = nl
			}
		} else if before := prevNonSpace(d.raw, start); before >= 0 && d.raw[before] == ',' {
			start = before
		} else if nl := restOfLine(d.raw, end); nl > end {
			end = nl
		}

		d.splice(start, end, "")
		n++
	}
}

func nextNonSpace(raw []byte, i int) int {
	for i < len(raw) && isJSONSpace(raw[i]) {
		i++
	}
	return i
}

func prevNonSpace(raw []byte, i int) int {
	i--
	for i >= 0 && isJSONSpace(raw[i]) {
		i--
	}
	return i
}

// restOfLine returns the offset past the newline ending the line at i, when
// everything between is whitespace.
func restOfLine(raw []byte, i int) int {
	j := i
	for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t' || raw[j] == '\r') {
		j++
	}
	if j < len(raw) && raw[j] == '\n' {
		return j + 1
	}
	return i
}

// Keys lists every scalar leaf's dotted path, in document order.
//
// Objects appear only through their leaves and arrays not at all, because
// neither is something a manifest key can assign.
func (d *JSONDoc) Keys() []string {
	members, err := d.scan()
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range members {
		if m.kind == jsonObject || m.kind == jsonArray {
			continue
		}
		out = append(out, m.path)
	}
	return dedupe(out)
}

// Bytes returns the document.
func (d *JSONDoc) Bytes() []byte { return d.raw }

func (d *JSONDoc) String() string { return string(d.raw) }

func splitParent(path string) (parent, leaf string) {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[:i], path[i+1:]
	}
	return "", path
}

func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func isJSONNumber(s string) bool {
	var n json.Number
	return json.Unmarshal([]byte(s), &n) == nil
}

// inferJSONLiteral picks a type for a value with no existing one to copy.
// Everything that is not unambiguously a number or a boolean becomes a string,
// because a string is the type a game config reads back unchanged.
func inferJSONLiteral(value string) string {
	if value == "true" || value == "false" {
		return value
	}
	if isJSONNumber(value) {
		return value
	}
	return jsonQuote(value)
}

type jsonPathCodec struct{}

func (jsonPathCodec) Name() string { return "json_path" }

func (jsonPathCodec) Parse(data []byte) (Doc, error) { return ParseJSON(data) }

func (jsonPathCodec) New() (Doc, error) { return &JSONDoc{raw: []byte("{}\n")}, nil }

// KeySyntax rejects array subscripts explicitly rather than letting them fail
// as a missing key, so a manifest written against a config with a list says so
// at load instead of applying nothing at 3am.
func (jsonPathCodec) KeySyntax(key string) error {
	if _, err := dottedSegments(key); err != nil {
		return err
	}
	if strings.ContainsAny(key, "[]") {
		return fmt.Errorf("config key %q subscripts an array; json_path addresses object keys only", key)
	}
	return nil
}
