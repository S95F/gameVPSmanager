package gamecfg

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	// ErrInlineTable means a key resolves into an inline table — `opts = {a = 1}`
	// addressed as `opts.a`.
	//
	// Editing inside one means rewriting the whole `{ ... }`, and every
	// round-tripping TOML library does that by re-serialising: the comments go,
	// the spacing goes, and the key order is whatever the library's map
	// iteration produced. Refusing is the honest answer, and refusing at load
	// means the manifest is corrected before anything is running on it.
	ErrInlineTable = errors.New("gamecfg: key resolves into an inline table")

	// ErrArrayOfTables means a key resolves into an [[array of tables]], where
	// "the" value does not exist: there are N of them and nothing in a dotted
	// path says which.
	ErrArrayOfTables = errors.New("gamecfg: key resolves into an array of tables")
)

// TOML edits a TOML file the way ini.go edits a flat one: by line, in place.
//
// It handles tables, dotted keys, quoted keys, multi-line strings and
// multi-line arrays well enough to find a key and to know when it must not
// touch one. It deliberately cannot write inside an inline table or an array
// of tables, and it never re-serialises the document, which is what makes
// comment preservation a property of the design rather than a feature that
// works until someone changes the TOML library.
type TOML struct {
	lines   [][]byte
	entries []tomlEntry
}

type tomlValueKind int

const (
	tomlString tomlValueKind = iota
	tomlBool
	tomlNumber
	// tomlOther is a datetime, or anything else this codec can read but has no
	// safe rule for rewriting.
	tomlOther
	tomlInline
	tomlMultiline
)

// tomlEntry is one assignment, located by line and by byte range within it.
type tomlEntry struct {
	path             string
	line             int
	lastLine         int // == line unless the value spans lines
	valStart, valEnd int // within the line's content, terminator excluded
	kind             tomlValueKind
	// inAOT marks an assignment inside an [[array of tables]] section.
	inAOT bool
}

// ParseTOML reads the document. It never fails: an unreadable line is
// passthrough text, exactly as in the other line-oriented codecs.
func ParseTOML(data []byte) *TOML {
	t := &TOML{lines: splitKeepEOL(data)}
	table, inAOT := "", false

	for i := 0; i < len(t.lines); i++ {
		text := string(trimEOL(t.lines[i]))
		trimmed := strings.TrimLeft(text, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			table, inAOT = parseTableHeader(trimmed)
			continue
		}

		key, valStart, ok := splitTOMLAssignment(text)
		if !ok {
			continue
		}
		e := tomlEntry{
			path:     joinPath(table, key),
			line:     i,
			lastLine: i,
			valStart: valStart,
			inAOT:    inAOT,
		}
		e.valEnd, e.kind, e.lastLine = t.scanValue(i, valStart)
		i = e.lastLine
		t.entries = append(t.entries, e)
	}
	return t
}

// parseTableHeader reads "[a.b]" or "[[a.b]]" and reports the table path and
// whether it is an array of tables.
func parseTableHeader(s string) (string, bool) {
	aot := strings.HasPrefix(s, "[[")
	open := 1
	if aot {
		open = 2
	}
	closeAt := strings.Index(s[open:], "]")
	if closeAt < 0 {
		return "", false
	}
	return normaliseTOMLKey(s[open : open+closeAt]), aot
}

// splitTOMLAssignment finds the key and the offset of the value on an
// assignment line.
func splitTOMLAssignment(text string) (key string, valStart int, ok bool) {
	i := 0
	depth := 0
	for i < len(text) {
		switch text[i] {
		case '"', '\'':
			q := text[i]
			i++
			for i < len(text) && text[i] != q {
				if q == '"' && text[i] == '\\' {
					i++
				}
				i++
			}
		case '=':
			if depth == 0 {
				k := normaliseTOMLKey(text[:i])
				if k == "" {
					return "", 0, false
				}
				j := i + 1
				for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
					j++
				}
				return k, j, true
			}
		case '#':
			return "", 0, false
		}
		i++
	}
	return "", 0, false
}

// normaliseTOMLKey turns `a . "b.c" . d` into a dotted path, decoding quoted
// segments so a quoted key is compared by what it means, not how it is spelled.
func normaliseTOMLKey(s string) string {
	var parts []string
	i := 0
	cur := strings.Builder{}
	for i < len(s) {
		switch s[i] {
		case '"', '\'':
			q := s[i]
			i++
			for i < len(s) && s[i] != q {
				if q == '"' && s[i] == '\\' && i+1 < len(s) {
					i++
				}
				cur.WriteByte(s[i])
				i++
			}
			i++
		case '.':
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
			i++
		default:
			cur.WriteByte(s[i])
			i++
		}
	}
	parts = append(parts, strings.TrimSpace(cur.String()))
	for _, p := range parts {
		if p == "" {
			return ""
		}
	}
	return strings.Join(parts, ".")
}

// scanValue measures the value starting at valStart on line i, reporting where
// it ends, what kind it is, and the last line it occupies.
func (t *TOML) scanValue(i, valStart int) (valEnd int, kind tomlValueKind, lastLine int) {
	text := string(trimEOL(t.lines[i]))
	if valStart >= len(text) {
		return valStart, tomlOther, i
	}

	switch {
	case strings.HasPrefix(text[valStart:], `"""`), strings.HasPrefix(text[valStart:], `'''`):
		delim := text[valStart : valStart+3]
		last := i
		if rest := text[valStart+3:]; !strings.Contains(rest, delim) {
			for last+1 < len(t.lines) {
				last++
				if strings.Contains(string(trimEOL(t.lines[last])), delim) {
					break
				}
			}
		}
		return len(text), tomlMultiline, last

	case text[valStart] == '{':
		return tomlScalarEnd(text, valStart), tomlInline, i

	case text[valStart] == '[':
		depth, last := tomlBracketDepth(text[valStart:], 0), i
		for depth > 0 && last+1 < len(t.lines) {
			last++
			depth = tomlBracketDepth(string(trimEOL(t.lines[last])), depth)
		}
		if last != i {
			return len(text), tomlMultiline, last
		}
		return tomlScalarEnd(text, valStart), tomlOther, i
	}

	end := tomlScalarEnd(text, valStart)
	return end, tomlScalarKind(text[valStart:end]), i
}

// tomlBracketDepth updates a running bracket depth across a line, ignoring
// brackets inside strings and after a comment marker.
func tomlBracketDepth(s string, depth int) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\'':
			q := s[i]
			i++
			for i < len(s) && s[i] != q {
				if q == '"' && s[i] == '\\' {
					i++
				}
				i++
			}
		case '#':
			return depth
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	return depth
}

// tomlScalarEnd finds where a single-line value ends: at an unquoted '#' or at
// end of line, trailing whitespace excluded.
func tomlScalarEnd(text string, start int) int {
	i := start
	for i < len(text) {
		switch text[i] {
		case '"', '\'':
			q := text[i]
			i++
			for i < len(text) && text[i] != q {
				if q == '"' && text[i] == '\\' {
					i++
				}
				i++
			}
		case '#':
			return trimRightSpace(text, start, i)
		}
		i++
	}
	return trimRightSpace(text, start, len(text))
}

func trimRightSpace(text string, start, end int) int {
	for end > start && (text[end-1] == ' ' || text[end-1] == '\t') {
		end--
	}
	return end
}

func tomlScalarKind(lit string) tomlValueKind {
	switch {
	case lit == "":
		return tomlOther
	case lit[0] == '"' || lit[0] == '\'':
		return tomlString
	case lit == "true" || lit == "false":
		return tomlBool
	}
	if isTOMLNumber(lit) {
		return tomlNumber
	}
	return tomlOther
}

func isTOMLNumber(lit string) bool {
	clean := strings.ReplaceAll(lit, "_", "")
	if _, err := strconv.ParseInt(clean, 0, 64); err == nil {
		return true
	}
	_, err := strconv.ParseFloat(clean, 64)
	return err == nil
}

// resolve finds the entry for a path, or explains why the path is not
// addressable.
func (t *TOML) resolve(path string) (tomlEntry, error) {
	var found tomlEntry
	ok := false
	for _, e := range t.entries {
		if e.path == path {
			found, ok = e, true
		}
	}
	if ok {
		if found.inAOT {
			return found, fmt.Errorf("%w: %s", ErrArrayOfTables, path)
		}
		return found, nil
	}

	// The path was not an assignment. If a PREFIX of it is an inline table or
	// sits in an array of tables, say that instead of "not found": the two need
	// different fixes and only one of them is a typo.
	for _, e := range t.entries {
		if !strings.HasPrefix(path, e.path+".") {
			continue
		}
		switch {
		case e.kind == tomlInline:
			return tomlEntry{}, fmt.Errorf("%w: %s is inside %s", ErrInlineTable, path, e.path)
		case e.inAOT:
			return tomlEntry{}, fmt.Errorf("%w: %s is inside %s", ErrArrayOfTables, path, e.path)
		}
	}
	for _, e := range t.entries {
		if e.inAOT && strings.HasPrefix(e.path, path+".") {
			return tomlEntry{}, fmt.Errorf("%w: %s", ErrArrayOfTables, path)
		}
	}
	return tomlEntry{}, fmt.Errorf("%w: %s", ErrKeyNotFound, path)
}

// Get returns a value: a string with its quotes removed, anything else as its
// literal text.
func (t *TOML) Get(path string) (string, bool) {
	e, err := t.resolve(path)
	if err != nil {
		return "", false
	}
	if e.kind == tomlInline || e.kind == tomlMultiline {
		// There is no single-line text to hand back, and returning the opening
		// brace or bracket would read as a value.
		return "", false
	}
	lit := string(trimEOL(t.lines[e.line]))[e.valStart:e.valEnd]
	if e.kind == tomlString {
		return unquoteTOMLString(lit), true
	}
	return lit, true
}

// Set rewrites a value in place, keeping the type the file already used.
//
// A key that is absent is ErrKeyNotFound rather than an append: a TOML config
// belongs to the game that reads it, and a key it does not define is one it
// will not read.
func (t *TOML) Set(path, value string) error {
	if err := (tomlKVCodec{}).KeySyntax(path); err != nil {
		return err
	}
	e, err := t.resolve(path)
	if err != nil {
		return err
	}

	var lit string
	switch e.kind {
	case tomlString:
		lit = quoteTOMLString(value)
	case tomlBool:
		if value != "true" && value != "false" {
			return fmt.Errorf("%w: %s holds a boolean and %q is not true or false", ErrUnsupported, path, value)
		}
		lit = value
	case tomlNumber:
		if !isTOMLNumber(value) {
			return fmt.Errorf("%w: %s holds a number and %q is not one", ErrUnsupported, path, value)
		}
		lit = value
	case tomlInline:
		return fmt.Errorf("%w: %s", ErrInlineTable, path)
	case tomlMultiline:
		return fmt.Errorf("%w: %s spans lines; rewriting it would reflow the rest of the file", ErrUnsupported, path)
	default:
		return fmt.Errorf("%w: %s holds a value this codec will not rewrite (%s)", ErrUnsupported, path,
			string(trimEOL(t.lines[e.line]))[e.valStart:e.valEnd])
	}

	t.lines[e.line] = spliceLine(t.lines[e.line], e.valStart, e.valEnd, lit)
	t.reparse()
	return nil
}

// Delete removes the lines an assignment occupies.
func (t *TOML) Delete(path string) int {
	n := 0
	for {
		e, err := t.resolve(path)
		if err != nil {
			return n
		}
		t.lines = append(t.lines[:e.line:e.line], t.lines[e.lastLine+1:]...)
		t.reparse()
		n++
	}
}

// reparse rebuilds the entry index after a mutation, so no offset can outlive
// the bytes it described.
func (t *TOML) reparse() {
	fresh := ParseTOML(t.Bytes())
	t.lines, t.entries = fresh.lines, fresh.entries
}

// Keys lists every addressable dotted path in file order.
//
// Inline tables, multi-line values and array-of-tables members are left out,
// because a key this codec refuses to write is not one a manifest should be
// able to name. A validator that checked existence against this list and then
// hit a refusal at apply time would be reporting the problem twice, in the
// wrong order.
func (t *TOML) Keys() []string {
	var out []string
	for _, e := range t.entries {
		if e.kind == tomlInline || e.kind == tomlMultiline || e.inAOT {
			continue
		}
		out = append(out, e.path)
	}
	return dedupe(out)
}

// Bytes renders the document.
func (t *TOML) Bytes() []byte {
	var b bytes.Buffer
	for _, ln := range t.lines {
		b.Write(ln)
	}
	return b.Bytes()
}

func (t *TOML) String() string { return string(t.Bytes()) }

// quoteTOMLString writes a basic string. A literal string would need no
// escaping but cannot express one either, so a value containing a single quote
// would have no representation at all.
func quoteTOMLString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func unquoteTOMLString(lit string) string {
	if len(lit) < 2 {
		return lit
	}
	if lit[0] == '\'' {
		// A literal string has no escapes at all, by definition.
		return strings.TrimSuffix(lit[1:], "'")
	}
	if s, err := strconv.Unquote(lit); err == nil {
		return s
	}
	return strings.TrimSuffix(strings.TrimPrefix(lit, `"`), `"`)
}

type tomlKVCodec struct{}

func (tomlKVCodec) Name() string { return "toml_kv" }

func (tomlKVCodec) Parse(data []byte) (Doc, error) { return ParseTOML(data), nil }

func (tomlKVCodec) New() (Doc, error) { return ParseTOML(nil), nil }

// KeySyntax requires bare-key segments. A quoted TOML key containing a dot has
// no unambiguous dotted spelling, so accepting one here would mean accepting a
// manifest key that can never match anything in the file.
func (tomlKVCodec) KeySyntax(key string) error {
	parts, err := dottedSegments(key)
	if err != nil {
		return err
	}
	for _, p := range parts {
		for _, r := range p {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '_' || r == '-':
			default:
				return fmt.Errorf("config key %q: %q is not a TOML bare key", key, p)
			}
		}
	}
	return nil
}
