package platform

// The module-wide "gvm does not SIGKILL a game" gate.
//
// It replaces the CI grep that matched `syscall.SIGKILL`, `kill -9` and
// `Signal(9)` and then excluded any line whose PATH contained `adopt` or
// `rollback`. A path exclusion is the wrong shape twice over: it blesses every
// future file under those trees, and it still misses `unix.SIGKILL`,
// `syscall.Kill(pid, 9)` and `p.Signal(syscall.Signal(9))`, none of which the
// pattern spells.
//
// SIGKILL corrupts a world: it is the last rung of the shutdown ladder and the
// abandon path of a just-failed spawn, and nowhere else. This walks the AST of
// every production file under internal/ and cmd/ and flags, inside any function
// body:
//
//   - a reference to a SIGKILL identifier (platform.SIGKILL, syscall.SIGKILL,
//     unix.SIGKILL, or a bare SIGKILL inside this package), and
//   - an integer literal 9 handed to a signal call (Kill / Signal /
//     SignalGroup).
//
// A function that does either must be named in sigkillAllowlist — a reviewed
// list of FUNCTIONS, not a path prefix. The signal-number definition itself
// (`SIGKILL Signal = 9`) and the name table live at package scope, outside any
// function, so they are exempt without being listed.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sigkillAllowlist is the reviewed set of functions, keyed "relpath#func", that
// may send or name SIGKILL. Any other function that does fails the gate until
// it is added here with a reason — which is the review the path exclusion
// never got.
var sigkillAllowlist = map[string]string{
	"internal/runner/shutdown.go#runImplicit":      "§11.6 rule 7: after the declared steps, SIGTERM to the group, then SIGKILL to the group, then SIGKILL to the cgroup",
	"internal/runner/shutdown.go#degradesBySignal": "classifies whether a declared step is a SIGKILL, so the declared and implicit ladders agree on what 'degraded' means",
	"internal/runner/spawn_linux.go#abandon":       "kills a just-spawned group microseconds after exec when launch setup failed, before any world file is open",
}

// signalCalls are the function/selector names that send or build a signal; an
// integer 9 handed to one of these is a raw SIGKILL.
var signalCalls = map[string]bool{
	"Kill": true, "Signal": true, "SignalGroup": true,
}

func TestNoSigkillOutsideTheLadder(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// offending maps a function key to its first SIGKILL/signal-9 site; seen
	// records which allowlisted keys were actually observed (stale-entry check).
	offending := map[string]token.Position{}
	seen := map[string]bool{}
	refs := 0

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
			rel := relSlash(t, root, path)

			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				key := rel + "#" + fn.Name.Name
				handled := map[token.Pos]bool{} // SIGKILL idents already seen via a selector
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch node := n.(type) {
					case *ast.SelectorExpr:
						if node.Sel.Name == "SIGKILL" {
							handled[node.Sel.Pos()] = true
							record(offending, &refs, key, fset.Position(node.Pos()))
						}
					case *ast.Ident:
						if node.Name == "SIGKILL" && !handled[node.Pos()] {
							record(offending, &refs, key, fset.Position(node.Pos()))
						}
					case *ast.CallExpr:
						if isSignalNine(node) {
							record(offending, &refs, key, fset.Position(node.Pos()))
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// SIGKILL legitimately appears in a few functions (the ladder, abandon); a
	// walk that found none would mean the scan is broken and the gate vacuous.
	if refs < 3 {
		t.Fatalf("found only %d SIGKILL/signal-9 references in function bodies; the AST walk is "+
			"broken and an empty answer would make this gate prove nothing", refs)
	}

	for key, pos := range offending {
		if _, ok := sigkillAllowlist[key]; ok {
			seen[key] = true
			continue
		}
		t.Errorf("%s names or sends SIGKILL (first at %s). SIGKILL corrupts a world and is "+
			"allowed only in the shutdown ladder and the spawn-abandon path. If this is a new such "+
			"place, add %q to sigkillAllowlist with a reason; otherwise stop the game gracefully.",
			key, pos, key)
	}

	for key := range sigkillAllowlist {
		if !seen[key] {
			t.Errorf("sigkillAllowlist names %s, but no SIGKILL reference was found there; remove the stale entry", key)
		}
	}
}

// record notes the first offending site in a function and bumps the counter.
func record(offending map[string]token.Position, refs *int, key string, pos token.Position) {
	*refs++
	if _, ok := offending[key]; !ok {
		offending[key] = pos
	}
}

// TestIsSignalNineDetectsRawNine pins the raw-9 detector: no production code
// passes a literal 9 to a signal call today, so the walk above never exercises
// this branch, and a silently-broken detector would let `syscall.Kill(pid, 9)`
// through.
func TestIsSignalNineDetectsRawNine(t *testing.T) {
	fires := []string{"syscall.Kill(pid, 9)", "unix.Signal(9)", "p.Signal(9)", "platform.SignalGroup(pid, 9)"}
	for _, s := range fires {
		e, err := parser.ParseExpr(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if !isSignalNine(e.(*ast.CallExpr)) {
			t.Errorf("isSignalNine(%q) = false; a literal 9 to a signal call is a raw SIGKILL", s)
		}
	}
	quiet := []string{"syscall.Kill(pid, sig)", "unix.Signal(15)", "doThing(9)", "p.Signal(platform.SIGTERM)"}
	for _, s := range quiet {
		e, err := parser.ParseExpr(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if isSignalNine(e.(*ast.CallExpr)) {
			t.Errorf("isSignalNine(%q) = true; it is not a raw SIGKILL", s)
		}
	}
}

// isSignalNine reports whether a call is a signal API (Kill/Signal/SignalGroup)
// handed an integer literal 9 — a raw SIGKILL that dodges the named constant.
func isSignalNine(call *ast.CallExpr) bool {
	name := ""
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		name = fn.Sel.Name
	case *ast.Ident:
		name = fn.Name
	}
	if !signalCalls[name] {
		return false
	}
	for _, arg := range call.Args {
		if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.INT && lit.Value == "9" {
			return true
		}
	}
	return false
}
