// Package help holds gvm's embedded help topics: the prose an operator reads
// with `gvm help <topic>`, offline, on the box.
//
// They are Markdown files embedded at build time rather than Go string
// literals, for two reasons. The first is that a topic is a document an
// operator (or a doc reviewer) edits, and a .md file is where that belongs —
// not escaped inside a .go file. The second is a structural one: the CLI has a
// test that fails if the admin control socket's path appears as a string
// literal anywhere in internal/cli. The `transport` topic has to explain why
// gvm will not use that socket, and keeping the explanation in a .md file is
// what lets it say so without tripping the test that keeps the CLI from ever
// dialling it.
package help

import (
	"embed"
	"sort"
	"strings"
)

//go:embed *.md
var files embed.FS

// Topic returns the text of one help topic. The bool is false for a name that
// is not a topic, so the caller can offer a did-you-mean rather than printing
// an empty page.
func Topic(name string) (string, bool) {
	b, err := files.ReadFile(name + ".md")
	if err != nil {
		return "", false
	}
	return string(b), true
}

// Names is every topic, sorted, for the help listing and for did-you-mean.
func Names() []string {
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(out)
	return out
}
