package gamecfg

import (
	"bytes"
	"fmt"
	"strings"
)

// XMLProps edits 7 Days to Die's serverconfig.xml, whose entire settable
// surface is a flat list of
//
//	<property name="ServerName" value="My Server"/>
//
// elements inside <ServerSettings>.
//
// It is a byte-range edit of one attribute, never a parse and re-emit. The
// shipped serverconfig.xml is 200 lines where every property carries a comment
// explaining its range and its interaction with three others, the file is
// tab-indented, and the game's own updates rewrite it. encoding/xml would
// return a document that renders back with the comments in different places,
// self-closing tags respelled, and the indentation gone — a diff no operator
// can review, hiding the one value that actually changed.
//
// Commented-out properties are invisible to this codec. That is deliberate:
// the shipped file disables several options by wrapping them in <!-- -->, and
// treating one as live would let a Set write into a comment and report success
// while the setting stays off.
type XMLProps struct {
	raw []byte
}

// xmlProp is one live <property> element, located in the raw bytes.
type xmlProp struct {
	name string
	// tagStart and tagEnd bracket the whole element, "<property" through the
	// closing ">".
	tagStart, tagEnd int
	// valStart and valEnd bracket the raw text INSIDE the value attribute's
	// quotes. Both are -1 when the element has no value attribute.
	valStart, valEnd int
}

// ParseXMLProps reads the document. It never fails; a file with no property
// elements simply has no addressable keys, which the manifest's own key list
// then reports as missing at apply time.
func ParseXMLProps(data []byte) *XMLProps {
	return &XMLProps{raw: append([]byte{}, data...)}
}

// scan locates every live <property> element.
//
// It is a hand scanner rather than a regexp because it has to skip comments
// and CDATA, and a regexp that tries to do that is either wrong at the first
// nested "--" or unreadable.
func (x *XMLProps) scan() []xmlProp {
	var out []xmlProp
	data := x.raw
	for i := 0; i < len(data); {
		switch {
		case hasAt(data, i, "<!--"):
			end := bytes.Index(data[i:], []byte("-->"))
			if end < 0 {
				return out
			}
			i += end + 3
		case hasAt(data, i, "<![CDATA["):
			end := bytes.Index(data[i:], []byte("]]>"))
			if end < 0 {
				return out
			}
			i += end + 3
		case hasAt(data, i, "<property") && isTagNameEnd(data, i+len("<property")):
			p, next := parsePropertyTag(data, i)
			if next <= i {
				return out
			}
			if p != nil {
				out = append(out, *p)
			}
			i = next
		default:
			i++
		}
	}
	return out
}

func hasAt(data []byte, i int, s string) bool {
	return i+len(s) <= len(data) && string(data[i:i+len(s)]) == s
}

// isTagNameEnd reports whether the element name stops here, so that
// <propertyGroup> is not mistaken for <property>.
func isTagNameEnd(data []byte, i int) bool {
	if i >= len(data) {
		return false
	}
	switch data[i] {
	case ' ', '\t', '\r', '\n', '/', '>':
		return true
	}
	return false
}

// parsePropertyTag reads one element starting at "<property" and returns it
// plus the offset just past its closing ">".
func parsePropertyTag(data []byte, start int) (*xmlProp, int) {
	p := xmlProp{tagStart: start, valStart: -1, valEnd: -1}
	i := start + len("<property")

	for i < len(data) {
		for i < len(data) && isXMLSpace(data[i]) {
			i++
		}
		if i >= len(data) {
			return nil, len(data)
		}
		if data[i] == '>' {
			p.tagEnd = i + 1
			break
		}
		if data[i] == '/' && i+1 < len(data) && data[i+1] == '>' {
			p.tagEnd = i + 2
			break
		}

		nameStart := i
		for i < len(data) && data[i] != '=' && !isXMLSpace(data[i]) && data[i] != '>' && data[i] != '/' {
			i++
		}
		attr := string(data[nameStart:i])
		if attr == "" {
			// Nothing consumed: a stray '/' or similar. Step over it, or the
			// scan never terminates.
			i++
			continue
		}
		for i < len(data) && isXMLSpace(data[i]) {
			i++
		}
		if i >= len(data) || data[i] != '=' {
			continue // a valueless attribute; not something this format uses
		}
		i++
		for i < len(data) && isXMLSpace(data[i]) {
			i++
		}
		if i >= len(data) || (data[i] != '"' && data[i] != '\'') {
			return nil, i + 1
		}
		quote := data[i]
		i++
		vs := i
		for i < len(data) && data[i] != quote {
			i++
		}
		if i >= len(data) {
			return nil, len(data)
		}
		switch attr {
		case "name":
			p.name = unescapeXML(string(data[vs:i]))
		case "value":
			p.valStart, p.valEnd = vs, i
		}
		i++
	}

	if p.tagEnd == 0 {
		return nil, len(data)
	}
	if p.name == "" {
		return nil, p.tagEnd
	}
	return &p, p.tagEnd
}

func isXMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func (x *XMLProps) find(name string) (xmlProp, bool) {
	for _, p := range x.scan() {
		if p.name == name {
			return p, true
		}
	}
	return xmlProp{}, false
}

// Get returns the value attribute of a property.
func (x *XMLProps) Get(name string) (string, bool) {
	p, ok := x.find(name)
	if !ok || p.valStart < 0 {
		return "", false
	}
	return unescapeXML(string(x.raw[p.valStart:p.valEnd])), true
}

// Set rewrites one property's value attribute in place.
//
// It refuses a property that has no value attribute rather than inventing one:
// a <property name="X"/> with no value is a malformed line in a file the game
// generated, and adding an attribute to it hides that from whoever has to fix
// the file.
func (x *XMLProps) Set(name, value string) error {
	if err := (xmlPropsCodec{}).KeySyntax(name); err != nil {
		return err
	}
	p, ok := x.find(name)
	if !ok {
		return fmt.Errorf("%w: <property name=%q>", ErrKeyNotFound, name)
	}
	if p.valStart < 0 {
		return fmt.Errorf("%w: <property name=%q> has no value attribute", ErrUnsupported, name)
	}
	x.raw = splice(x.raw, p.valStart, p.valEnd, escapeXMLAttr(value))
	return nil
}

// Delete removes every element declaring this property, and the line it sat on
// when nothing else shares it.
//
// A trailing comment on the same line goes with it: in this file the comment
// documents the property, and leaving it behind orphaned reads as a setting
// that is still there.
func (x *XMLProps) Delete(name string) int {
	n := 0
	for {
		p, ok := x.find(name)
		if !ok {
			return n
		}
		start, end := p.tagStart, p.tagEnd
		for start > 0 && (x.raw[start-1] == ' ' || x.raw[start-1] == '\t') {
			start--
		}
		if rest := lineRemainder(x.raw, end); rest > end {
			end = rest
		}
		x.raw = splice(x.raw, start, end, "")
		n++
	}
}

// lineRemainder returns the offset past the end of the line containing i,
// but only when everything from i to that newline is whitespace or a comment.
func lineRemainder(data []byte, i int) int {
	j := i
	for j < len(data) {
		switch {
		case data[j] == '\n':
			return j + 1
		case data[j] == ' ' || data[j] == '\t' || data[j] == '\r':
			j++
		case hasAt(data, j, "<!--"):
			end := bytes.Index(data[j:], []byte("-->"))
			if end < 0 {
				return i
			}
			j += end + 3
		default:
			return i
		}
	}
	return j
}

// Keys lists every live property name in document order.
func (x *XMLProps) Keys() []string {
	var out []string
	for _, p := range x.scan() {
		out = append(out, p.name)
	}
	return dedupe(out)
}

// Bytes returns the document.
func (x *XMLProps) Bytes() []byte { return x.raw }

func (x *XMLProps) String() string { return string(x.raw) }

var xmlAttrEscapes = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
	"\r", "&#13;",
	"\n", "&#10;",
	"\t", "&#9;",
)

// escapeXMLAttr escapes what an attribute value cannot contain literally.
// The line terminators are escaped as character references because an XML
// parser normalises a literal newline inside an attribute to a space, which
// would silently rewrite a multi-line motd into one line.
func escapeXMLAttr(s string) string { return xmlAttrEscapes.Replace(s) }

// unescapeXML resolves the five named entities and numeric character
// references. Anything else is left alone, since an unknown entity in a file
// the game wrote is not ours to reinterpret.
func unescapeXML(s string) string {
	if !strings.ContainsRune(s, '&') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '&' {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i:], ';')
		if end < 0 {
			b.WriteString(s[i:])
			break
		}
		ent := s[i+1 : i+end]
		switch ent {
		case "amp":
			b.WriteByte('&')
		case "lt":
			b.WriteByte('<')
		case "gt":
			b.WriteByte('>')
		case "quot":
			b.WriteByte('"')
		case "apos":
			b.WriteByte('\'')
		default:
			if r, ok := numericCharRef(ent); ok {
				b.WriteRune(r)
			} else {
				b.WriteString(s[i : i+end+1])
			}
		}
		i += end + 1
	}
	return b.String()
}

func numericCharRef(ent string) (rune, bool) {
	if !strings.HasPrefix(ent, "#") || len(ent) < 2 {
		return 0, false
	}
	digits, base := ent[1:], 10
	if digits[0] == 'x' || digits[0] == 'X' {
		digits, base = digits[1:], 16
	}
	if digits == "" {
		return 0, false
	}
	var r rune
	for i := 0; i < len(digits); i++ {
		var d rune
		c := digits[i]
		switch {
		case c >= '0' && c <= '9':
			d = rune(c - '0')
		case base == 16 && c >= 'a' && c <= 'f':
			d = rune(c-'a') + 10
		case base == 16 && c >= 'A' && c <= 'F':
			d = rune(c-'A') + 10
		default:
			return 0, false
		}
		r = r*rune(base) + d
	}
	return r, true
}

type xmlPropsCodec struct{}

func (xmlPropsCodec) Name() string { return "xml_property_list" }

func (xmlPropsCodec) Parse(data []byte) (Doc, error) { return ParseXMLProps(data), nil }

// New refuses: 7 Days to Die ships serverconfig.xml with every property and its
// documentation, and sdtd.seed_config copies that file into the instance. A
// synthetic one would be missing every key the shipped file explains.
func (xmlPropsCodec) New() (Doc, error) {
	return nil, fmt.Errorf("%w: 7 Days to Die ships serverconfig.xml; sdtd.seed_config copies it", ErrCannotCreate)
}

// KeySyntax rejects any name that would need XML escaping to appear in the
// file. The scanner matches a property by the decoded text of its name
// attribute, and a name that has to be written as "A&amp;B" in the manifest is
// a manifest nobody can read; refusing it at load is clearer than a key that
// never matches anything.
func (xmlPropsCodec) KeySyntax(key string) error {
	if err := plainKeySyntax(key); err != nil {
		return err
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == ':':
		default:
			return fmt.Errorf("config key %q contains %q; an xml_property_list name may use letters, digits and _-.: only", key, r)
		}
	}
	return nil
}
