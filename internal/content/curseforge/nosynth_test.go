package curseforge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestNoCurseForgeURLSynthesis is the structural gate that enforces the core
// security rule for this package: no CDN URL is ever synthesised from project
// IDs, file IDs, or slugs. Every URL that reaches a CDN comes verbatim from
// the API response's downloadUrl field.
//
// Why this matters: the CurseForge CDN lives at two well-known prefixes. A
// pattern like
//
//	fmt.Sprintf("https://edge.forgecdn.net/files/%d/%d/%s", a, b, slug)
//
// would produce a valid-looking URL for most files, bypass the API entirely,
// and (crucially) skip the blocked-mod check: a mod author who opted out of
// API distribution would be silently fetched anyway. The only way to tell that
// a URL came from an API response rather than local construction is a
// structural scan of this package's source.
func TestNoCurseForgeURLSynthesis(t *testing.T) {
	// Forbidden substrings: any string literal in this package that contains
	// one of these is a synthesised CDN URL.
	forbidden := []string{
		"forgecdn.net",
		"edge.forgecdn",
	}

	sources := packageSources(t)

	fset := token.NewFileSet()
	for name, src := range sources {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("%s: parse error: %v", name, err)
		}
		// Walk all AST nodes, checking every string literal.
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, needle := range forbidden {
				if strings.Contains(lit.Value, needle) {
					pos := fset.Position(lit.Pos())
					t.Errorf("%s:%d: string literal contains %q, which would synthesise a CDN URL;\n"+
						"\t%s\n"+
						"\tAll CDN URLs must come verbatim from the CurseForge API's downloadUrl field.",
						pos.Filename, pos.Line, needle, lit.Value)
				}
			}
			return true
		})

		// Second pass: scan raw code lines for the patterns, after stripping
		// comments. The literal scan above catches quoted strings; this catches
		// raw strings and any token that does not look like a Go literal.
		for i, line := range strings.Split(string(src), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if strings.TrimSpace(code) == "" {
				continue
			}
			for _, needle := range forbidden {
				if strings.Contains(code, needle) {
					t.Errorf("%s:%d: code line contains %q (CDN URL synthesis guard):\n\t%s",
						name, i+1, needle, strings.TrimSpace(code))
				}
			}
		}
	}

	// A scan that found no files would pass vacuously.
	if len(sources) < 8 {
		t.Fatalf("scanned only %d source files; the walk is broken and every "+
			"assertion above is vacuous", len(sources))
	}
}

// packageSources returns this package's non-test Go source files.
func packageSources(t *testing.T) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]byte)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = b
	}
	return out
}
