// Package gamecfg edits the configuration file formats that game servers use.
//
// Every editor here follows the same rule: a game server owns its config file
// and rewrites it on its own schedule, so gvm makes surgical edits and
// preserves everything it does not understand. Comments, ordering, blank
// lines and unrecognised keys all survive a write. Regenerating these files
// from a template would silently discard settings the server itself added.
package gamecfg

import (
	"bytes"
	"fmt"
	"strings"
)

// INI is a flat key=value configuration file, the format Project Zomboid uses
// for <servername>.ini. Despite the name there are no [sections]; the whole
// file is one namespace.
type INI struct {
	lines []string
	eol   string
	// trailingNewline records whether the source ended with a line break, so
	// a rewrite does not gratuitously add or remove one.
	trailingNewline bool
}

// ParseINI reads a flat key=value document. It never fails: any line it does
// not recognise is preserved verbatim as a passthrough line.
func ParseINI(data []byte) *INI {
	f := &INI{eol: detectEOL(data), trailingNewline: bytes.HasSuffix(data, []byte("\n"))}
	s := string(data)
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	if s == "" && len(data) == 0 {
		return f
	}
	for _, line := range strings.Split(s, "\n") {
		f.lines = append(f.lines, strings.TrimSuffix(line, "\r"))
	}
	return f
}

// detectEOL picks the line ending to write with. Game servers on Linux write
// LF; a file that has been through a Windows editor may be CRLF, and
// rewriting it as LF could surprise whoever is diffing it.
func detectEOL(data []byte) string {
	if i := bytes.IndexByte(data, '\n'); i > 0 && data[i-1] == '\r' {
		return "\r\n"
	}
	return "\n"
}

// matchesKey reports whether a line assigns exactly this key.
//
// The check is a prefix match on "key=" rather than on "key", which matters:
// Project Zomboid's ini contains Public, PublicName and PublicDescription,
// and matching the bare key would let a write to Public land on PublicName.
func matchesKey(line, key string) bool {
	return strings.HasPrefix(line, key+"=")
}

// Get returns the value of the first assignment of key.
func (f *INI) Get(key string) (string, bool) {
	for _, line := range f.lines {
		if matchesKey(line, key) {
			return line[len(key)+1:], true
		}
	}
	return "", false
}

// GetDefault returns the value of key, or fallback if it is absent.
func (f *INI) GetDefault(key, fallback string) string {
	if v, ok := f.Get(key); ok {
		return v
	}
	return fallback
}

// Has reports whether key is assigned.
func (f *INI) Has(key string) bool {
	_, ok := f.Get(key)
	return ok
}

// Set assigns key, rewriting the first existing assignment in place so the
// file's ordering is preserved, and appending at the end if there is none.
//
// Duplicate assignments of the same key after the first are removed. The game
// resolves duplicates by taking one of them and there is no way to tell which
// from the outside, so leaving a stale second copy behind means the file no
// longer describes the running configuration.
func (f *INI) Set(key, value string) {
	if strings.ContainsAny(key, "=\r\n") {
		panic(fmt.Sprintf("gamecfg: invalid ini key %q", key))
	}
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", " ")

	out := make([]string, 0, len(f.lines)+1)
	written := false
	for _, line := range f.lines {
		if !matchesKey(line, key) {
			out = append(out, line)
			continue
		}
		if written {
			continue // drop the duplicate
		}
		out = append(out, key+"="+value)
		written = true
	}
	if !written {
		out = append(out, key+"="+value)
	}
	f.lines = out
}

// SetAll applies several assignments. Keys are applied in the given order.
func (f *INI) SetAll(pairs [][2]string) {
	for _, kv := range pairs {
		f.Set(kv[0], kv[1])
	}
}

// Delete removes every assignment of key. It reports how many it removed.
func (f *INI) Delete(key string) int {
	out := make([]string, 0, len(f.lines))
	n := 0
	for _, line := range f.lines {
		if matchesKey(line, key) {
			n++
			continue
		}
		out = append(out, line)
	}
	f.lines = out
	return n
}

// Keys lists every assigned key, in file order, without duplicates.
func (f *INI) Keys() []string {
	var keys []string
	seen := map[string]bool{}
	for _, line := range f.lines {
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		k := line[:i]
		if seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	return keys
}

// Bytes renders the document back to its file representation.
func (f *INI) Bytes() []byte {
	var b strings.Builder
	for i, line := range f.lines {
		if i > 0 {
			b.WriteString(f.eol)
		}
		b.WriteString(line)
	}
	if f.trailingNewline && len(f.lines) > 0 {
		b.WriteString(f.eol)
	}
	return []byte(b.String())
}

func (f *INI) String() string { return string(f.Bytes()) }
