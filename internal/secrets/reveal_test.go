package secrets

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// revealBudget is how many places in gvm's own code may turn a Value back into
// a string.
//
// Section 14.7 names three, and they are the only three that can exist without
// a new argument: rendering argv for the game process, writing a game config
// key, and printing a credential once at bootstrap. The budget is one above
// that, so a genuinely new call site can land in a commit whose reviewer is
// looking at it, and a fifth cannot land in one whose reviewer is not.
//
// The number is not the point. Redaction does not rot by someone deleting it;
// it rots by accretion -- a Reveal into a local variable here, one into a
// debug log there -- and a count is the only thing that notices that.
const revealBudget = 4

func TestRevealHasAlmostNoCallSites(t *testing.T) {
	root := moduleRoot(t)
	var sites []string
	for _, tree := range []string{"internal", "cmd"} {
		sites = append(sites, findRevealCalls(t, root, tree)...)
	}
	if len(sites) > revealBudget {
		t.Errorf("%d call sites turn a secret back into a string, and the budget is %d:\n\t%s\n"+
			"Each one is a place a plaintext password exists as an ordinary string that some "+
			"future error message can interpolate. If the new one is genuinely necessary, raise "+
			"the budget in this file and say which of section 14.7's three it is.",
			len(sites), revealBudget, strings.Join(sites, "\n\t"))
	}
	t.Logf("%d of %d Reveal call sites used: %v", len(sites), revealBudget, sites)
}

// findRevealCalls walks one tree for `x.Reveal()` calls in non-test files.
//
// It parses rather than greps, which the document's own `grep -rn '\.Reveal()'`
// cannot: a grep counts the sentence in value.go that explains why Reveal is
// hard to find, and every future comment that mentions it, and would push the
// budget up by documentation alone.
func findRevealCalls(t *testing.T, root, tree string) []string {
	t.Helper()
	dir := filepath.Join(root, tree)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("%s is not in the module; this test is looking in the wrong place: %v", dir, err)
	}

	var out []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata holds compile-fail fixtures the go tool never sees.
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Reveal" {
				return true
			}
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel)+":"+
				strconv.Itoa(fset.Position(call.Pos()).Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

// moduleRoot walks up from the test's directory to the go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
