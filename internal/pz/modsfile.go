// Package pz holds the Project Zomboid-specific knowledge that cannot be
// expressed in a generic game manifest.
//
// Almost all of it exists because PZ needs two different, non-interchangeable
// mod lists in its server config:
//
//   - WorkshopItems= takes the numeric Steam Workshop ids from the item URLs.
//   - Mods= takes each mod's internal string id, which lives inside the
//     downloaded folder's mod.info and is not derivable from the numeric id.
//
// A wrong entry in Mods= fails silently: the item downloads, the mod never
// enables, and nothing is logged. That is why ids are read out of the
// downloaded files rather than maintained by hand.
package pz

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ModsFile is the ordered workshop id list, mods.txt.
//
// Order is load order and is load-bearing: libraries and frameworks have to
// come before the mods that depend on them, and a wrong order shows up as red
// Lua errors on boot. The file is edited in place so hand-tuned ordering and
// the comments explaining it survive an automated collection sync.
type ModsFile struct {
	lines           []string
	eol             string
	trailingNewline bool
}

var reIDLine = regexp.MustCompile(`^[0-9]+$`)

// ParseModsFile reads mods.txt.
func ParseModsFile(data []byte) *ModsFile {
	eol := "\n"
	if i := strings.IndexByte(string(data), '\n'); i > 0 && data[i-1] == '\r' {
		eol = "\r\n"
	}
	m := &ModsFile{eol: eol, trailingNewline: strings.HasSuffix(string(data), "\n")}
	if len(data) == 0 {
		return m
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	for _, line := range strings.Split(s, "\n") {
		m.lines = append(m.lines, strings.TrimSuffix(line, "\r"))
	}
	return m
}

// lineID extracts the workshop id from a line, or "" if the line does not
// carry one.
//
// This mirrors the bash exactly: strip everything from "#", strip all
// whitespace, and accept the result only if it is entirely digits. So
// "3171167894   # that DAMN Library" yields "3171167894", and a line that is
// only a comment yields nothing.
func lineID(line string) string {
	s := line
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' {
			return -1
		}
		return r
	}, s)
	if !reIDLine.MatchString(s) {
		return ""
	}
	return s
}

// IDs returns every workshop id, in load order.
func (m *ModsFile) IDs() []string {
	var out []string
	for _, line := range m.lines {
		if id := lineID(line); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// Has reports whether the file already lists id.
func (m *ModsFile) Has(id string) bool {
	for _, line := range m.lines {
		if lineID(line) == id {
			return true
		}
	}
	return false
}

// CollectionItem is one entry from a Steam collection.
type CollectionItem struct {
	ID    string
	Title string
}

// SyncResult reports what a collection sync changed.
type SyncResult struct {
	Added   []CollectionItem
	Dropped []string
}

// Changed reports whether the sync modified anything.
func (r SyncResult) Changed() bool { return len(r.Added) > 0 || len(r.Dropped) > 0 }

// SyncCollection reconciles the file against a Steam collection.
//
// Items already listed keep their position, so manual load ordering is
// preserved. Items no longer in the collection are removed. New items are
// appended under a dated header, at the bottom, because a new item's correct
// load position is not knowable from the collection: if it turns out to be a
// library it has to be moved up by hand.
func (m *ModsFile) SyncCollection(items []CollectionItem, now time.Time) SyncResult {
	var res SyncResult

	inCollection := make(map[string]bool, len(items))
	for _, it := range items {
		inCollection[it.ID] = true
	}

	kept := make([]string, 0, len(m.lines))
	for _, line := range m.lines {
		id := lineID(line)
		if id == "" {
			kept = append(kept, line) // comment, blank line, or header
			continue
		}
		if inCollection[id] {
			kept = append(kept, line)
			continue
		}
		res.Dropped = append(res.Dropped, id)
	}

	known := make(map[string]bool)
	for _, line := range kept {
		if id := lineID(line); id != "" {
			known[id] = true
		}
	}

	for _, it := range items {
		if known[it.ID] {
			continue
		}
		if len(res.Added) == 0 {
			kept = append(kept, "", fmt.Sprintf("# --- Added from collection %s ---", now.Format("2006-01-02")))
		}
		kept = append(kept, strings.TrimRight(fmt.Sprintf("%-12s # %s", it.ID, it.Title), " "))
		known[it.ID] = true
		res.Added = append(res.Added, it)
	}

	m.lines = kept
	return res
}

// Bytes renders the file.
func (m *ModsFile) Bytes() []byte {
	var b strings.Builder
	for i, line := range m.lines {
		if i > 0 {
			b.WriteString(m.eol)
		}
		b.WriteString(line)
	}
	if m.trailingNewline && len(m.lines) > 0 {
		b.WriteString(m.eol)
	}
	return []byte(b.String())
}

func (m *ModsFile) String() string { return string(m.Bytes()) }
