package platform

// The module-wide "gvm spawns no shell" gate.
//
// It replaces the line-oriented CI grep that matched only
// `exec.Command("sh"|"bash"|…)` on a single line. Every one of gvm's
// production exec sites passes the program as a VARIABLE, so that grep could
// not see a single one of them, and `exec.CommandContext(ctx, shell, "-c", …)`
// would have sailed straight through it.
//
// This walks the real AST of every production file under internal/ and cmd/,
// finds every os/exec spawn — the Command / CommandContext calls AND the
// exec.Cmd{Path: …} struct literals internal/runner uses — and resolves the
// program argument:
//
//   - a string literal that names a shell or a general-purpose interpreter
//     fails outright, wherever it appears;
//   - a non-literal program (a variable, an index, a field) is allowed only in
//     a file on execProgramAllowlist, which is the reviewed set of places that
//     legitimately run a resolved, absolute program. A new exec site with a
//     variable program fails until it is added here, which is the review.
//
// It lives beside the import-graph assertions in deps_test.go for the reason
// that file gives: a rule enforced from inside the package it constrains dies
// with the package, and this one has to outlive every package that spawns.

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

// shellBinaries are the program basenames a spawn may never name as a literal:
// the shells, plus the general-purpose interpreters whose whole job is to run
// code handed to them as a string.
var shellBinaries = map[string]bool{
	"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true,
	"csh": true, "tcsh": true, "ash": true, "fish": true, "busybox": true,
	"python": true, "python2": true, "python3": true,
	"perl": true, "ruby": true, "node": true, "php": true,
}

// execProgramAllowlist is the reviewed set of files that spawn a program named
// by something other than a string literal. Each entry is a promise, checked by
// a human, that the variable can only ever hold an absolute, gvm-resolved
// program — never a shell and never a PATH lookup. A path not in this map that
// spawns a non-literal program fails the gate.
var execProgramAllowlist = map[string]string{
	"internal/adopt/proc_linux.go":         "reads a running server's state via a resolved, absolute proc tool",
	"internal/hostinit/system_linux.go":    "host bring-up runs its tools by absolute, stat-resolved path; see hostinit/noshell_test.go",
	"internal/hostops/user_linux.go":       "shadow-utils (useradd/groupadd/gpasswd) by absolute path; see hostops/noshell_test.go",
	"internal/jobs/sandbox_linux.go":       "the D-16 sandbox seam runs the per-instance work in a transient unit",
	"internal/reconcile/calendar_linux.go": "systemd-analyze calendar, by absolute path, to normalise a timer expression",
	"internal/reconcile/probe_linux.go":    "the readiness probe tool, resolved to an absolute path",
	"internal/runner/spawn_linux.go":       "spawns the game server itself: an absolute Path the launch spec already resolved",
	"internal/runner/screencompat.go":      "re-execs gvm's own absolute binary under a pty shim",
}

// execSite is one spawn found in the tree.
type execSite struct {
	file string // module-root-relative, slash-separated
	line int
	prog ast.Expr // the program argument, or nil if it could not be located
}

func TestNoShellSpawnedAnywhere(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	var sites []execSite
	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			execName, ok := execImportName(f)
			if !ok {
				return nil // no os/exec import: no spawn can be spelled here
			}
			rel := relSlash(t, root, path)
			ast.Inspect(f, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					if prog, ok := execCallProgram(node, execName); ok {
						sites = append(sites, execSite{rel, fset.Position(node.Pos()).Line, prog})
					}
				case *ast.CompositeLit:
					if prog, ok := execCmdLiteralPath(node, execName); ok {
						sites = append(sites, execSite{rel, fset.Position(node.Pos()).Line, prog})
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// A walk that found nothing would pass silently forever. gvm legitimately
	// spawns a handful of absolute programs (steamcmd's sandbox, the account
	// tools, the game server itself), so the real count is well above this.
	if len(sites) < 8 {
		t.Fatalf("found only %d exec sites; the AST walk is broken, and an empty answer "+
			"would make this gate vacuous", len(sites))
	}

	seen := map[string]bool{}
	for _, s := range sites {
		if prog, ok := stringLiteral(s.prog); ok {
			if namesShell(prog) {
				t.Errorf("%s:%d spawns %q, which is a shell or interpreter; gvm must never "+
					"hand a program a command string to run (contract rule 1)", s.file, s.line, prog)
			}
			continue
		}
		// Non-literal program: allowed only in a reviewed file.
		if _, ok := execProgramAllowlist[s.file]; !ok {
			t.Errorf("%s:%d spawns a program named by a non-literal expression. That is only "+
				"allowed in a file on execProgramAllowlist, where a reviewer has confirmed the "+
				"value is always an absolute, gvm-resolved program and never a shell. Add it there "+
				"with a reason, or make the program an absolute string literal.", s.file, s.line)
			continue
		}
		seen[s.file] = true
	}

	// Keep the allowlist honest: an entry whose file no longer spawns a
	// non-literal program is stale and should be removed, or the next reader
	// trusts a promise nothing makes.
	for file := range execProgramAllowlist {
		if !seen[file] {
			t.Errorf("execProgramAllowlist names %s, but no non-literal spawn was found there; "+
				"remove the stale entry", file)
		}
	}
}

// execImportName returns the local name os/exec is imported under in f, or
// false if f does not import it.
func execImportName(f *ast.File) (string, bool) {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != "os/exec" {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name, true
		}
		return "exec", true
	}
	return "", false
}

// execCallProgram returns the program argument of an exec.Command /
// exec.CommandContext call, or false if the call is neither.
func execCallProgram(call *ast.CallExpr, execName string) (ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || x.Name != execName {
		return nil, false
	}
	switch sel.Sel.Name {
	case "Command":
		if len(call.Args) >= 1 {
			return call.Args[0], true
		}
	case "CommandContext":
		if len(call.Args) >= 2 {
			return call.Args[1], true
		}
	}
	return nil, false
}

// execCmdLiteralPath returns the Path expression of an exec.Cmd{…} composite
// literal (internal/runner builds its spawns this way), or false.
func execCmdLiteralPath(cl *ast.CompositeLit, execName string) (ast.Expr, bool) {
	sel, ok := cl.Type.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || x.Name != execName || sel.Sel.Name != "Cmd" {
		return nil, false
	}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Path" {
			return kv.Value, true
		}
	}
	return nil, false
}

// stringLiteral unquotes e if it is a string literal.
func stringLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// namesShell reports whether a program path's basename is a shell or a
// general-purpose interpreter.
func namesShell(prog string) bool {
	base := prog
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return shellBinaries[strings.ToLower(base)]
}

// TestNamesShellClassifies pins the shell detector so the gate above cannot
// pass for the wrong reason: the AST walk only exercises namesShell on the
// non-shell literals gvm already spawns (caddy, nft, ufw), so this proves it
// fires on the shells and interpreters it is meant to catch.
func TestNamesShellClassifies(t *testing.T) {
	shells := []string{"sh", "bash", "/bin/sh", "/usr/bin/bash", "/bin/dash", "busybox", "python3", "/usr/bin/perl"}
	for _, s := range shells {
		if !namesShell(s) {
			t.Errorf("namesShell(%q) = false; it is a shell/interpreter and must be caught", s)
		}
	}
	notShells := []string{"caddy", "/usr/bin/systemctl", "/usr/sbin/useradd", "nft", "ufw", "/usr/sbin/runuser", "steamcmd"}
	for _, s := range notShells {
		if namesShell(s) {
			t.Errorf("namesShell(%q) = true; it is a program gvm legitimately runs", s)
		}
	}
}

func relSlash(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("filepath.Rel(%q, %q): %v", root, path, err)
	}
	return filepath.ToSlash(rel)
}
