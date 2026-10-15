package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// designRefRe matches a design reference an operator should never see: a
// decision number (D-41), a section citation (section 12.9), or a § marker.
// These belong in comments and under GVM_DEBUG, not in the text a tired
// operator reads at 2 a.m. (U6).
var designRefRe = regexp.MustCompile(`D-[0-9][0-9]|[Ss]ection [0-9]+\.[0-9]+|§[0-9]`)

// TestNoDesignRefsInOperatorText rejects a design reference in any string
// literal of the operator-facing internal/cli package (comments are fine — the
// AST walk only reads string literals). It is the lint the plan asks for: a new
// `(D-41)` in an error, hint or help string turns the build red.
func TestNoDesignRefsInOperatorText(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "cli")
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot(t), path)
		ast.Inspect(f, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			s, uerr := strconv.Unquote(bl.Value)
			if uerr != nil {
				return true
			}
			if m := designRefRe.FindString(s); m != "" {
				t.Errorf("%s:%d: operator text carries the design reference %q — move it to a "+
					"comment or behind GVM_DEBUG:\n  %q", rel, fset.Position(bl.Pos()).Line, m, s)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
