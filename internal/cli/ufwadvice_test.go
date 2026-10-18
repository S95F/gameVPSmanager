package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoUfwEnableAdvice is usability principle U10 made mechanical: gvm never
// tells an operator to run `ufw enable`.
//
// Enabling a firewall before the live SSH port is admitted locks the operator
// out of their own box — the exact Ubuntu-24.04 ssh.socket trap gvm's detection
// exists to prevent — so the only path that ever enables ufw is the typed,
// SSH-verified `sudo gvm firewall enable` (Phase 40), which passes "enable" to
// ufw as an argv, not as advice. This test scans the operator-facing advice
// surfaces — the cli and doctor string literals (not comments), the embedded
// help topics, and the shipped manifests — and fails if any of them spells out
// `ufw enable`. (It does not scan internal/firewall, whose backend legitimately
// execs ufw with an "enable" argument, nor the plan/review docs, which discuss
// the rule.)
func TestNoUfwEnableAdvice(t *testing.T) {
	root := repoRoot(t)
	const banned = "ufw enable"

	// Go string literals in the two operator-facing packages.
	for _, pkg := range []string{"internal/cli", "internal/doctor"} {
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		walkGoStringLiterals(t, dir, func(file string, line int, s string) {
			if strings.Contains(strings.ToLower(s), banned) {
				t.Errorf("%s:%d: advice string contains %q (U10): %q", file, line, banned, s)
			}
		})
	}

	// Help topics and shipped manifests: plain text.
	for _, g := range []string{"docs/help/*.md", "games/*.yaml"} {
		matches, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(g)))
		for _, m := range matches {
			b, err := os.ReadFile(m)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.ToLower(string(b)), banned) {
				rel, _ := filepath.Rel(root, m)
				t.Errorf("%s contains %q (U10): gvm never tells an operator to enable the firewall", rel, banned)
			}
		}
	}
}

// walkGoStringLiterals calls fn for every string-literal token in the package's
// non-test .go files. Comments are not literals, so a comment that mentions the
// banned phrase to explain why it is avoided does not trip the scan.
func walkGoStringLiterals(t *testing.T, dir string, fn func(file string, line int, s string)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, uerr := strconv.Unquote(lit.Value); uerr == nil {
				fn(name, fset.Position(lit.Pos()).Line, s)
			}
			return true
		})
	}
}
