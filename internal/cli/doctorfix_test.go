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

	"github.com/S95F/gameVPSmanager/internal/avail"
	"github.com/S95F/gameVPSmanager/packaging"
)

// futureCommands are gvm subcommands a doctor Fix may name before the phase
// that builds them lands. `caddy` arrives with the dashboard (Phase 48); until
// then the EDGE checks that name it are Skip-gated, so the Fix never reaches an
// operator, but the static scan still sees the literal.
var futureCommands = map[string]bool{"caddy": true}

// knownExternalUnits are the non-gvm units a Fix may legitimately name.
var knownExternalUnits = map[string]bool{"postgresql": true, "caddy": true, "ufw": true}

// TestDoctorFixesNameRealThings is U-… made mechanical: a doctor Fix is only
// useful if the thing it tells the operator to run exists. Every `gvm <verb>`
// in a Fix must be an available command (not NotInBuild), and every
// `systemctl … <unit>` must name a unit this build packages, or postgresql /
// caddy / ufw. It goes red the day a Fix is changed to `gvm firewall enable`
// (unbuilt) or `systemctl restart gvmd` (not a packaged unit).
func TestDoctorFixesNameRealThings(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "doctor")
	units := packagedUnitBases(t)

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
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			if id, ok := kv.Key.(*ast.Ident); !ok || id.Name != "Fix" {
				return true
			}
			line := fset.Position(kv.Pos()).Line
			for _, s := range fixLiterals(kv.Value) {
				checkFixString(t, rel, line, s, units)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// fixLiterals pulls the string-literal parts out of a Fix value: a plain
// literal, a `"..." + ident` concatenation, or an fmt.Sprintf whose format is a
// literal. A Fix built entirely from runtime values yields nothing to check,
// which is correct — there is no literal command to validate.
func fixLiterals(expr ast.Expr) []string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			if s, err := strconv.Unquote(e.Value); err == nil {
				return []string{s}
			}
		}
	case *ast.BinaryExpr:
		return append(fixLiterals(e.X), fixLiterals(e.Y)...)
	case *ast.CallExpr:
		// fmt.Sprintf("...", …): the format string is the literal to scan.
		if len(e.Args) > 0 {
			return fixLiterals(e.Args[0])
		}
	}
	return nil
}

func checkFixString(t *testing.T, file string, line int, s string, units map[string]bool) {
	t.Helper()
	fields := strings.Fields(s)
	clean := func(w string) string {
		return strings.Trim(w, "`'\"().,;:")
	}
	// A "gvm" token is a command invocation only when it reads like one: it was
	// backtick-quoted (`gvm db migrate`), or it follows run:/sudo/try. Bare
	// "gvm" as a noun ("use gvm bundled build", "gvm database") is prose, not a
	// command to resolve.
	commandContext := map[string]bool{"run": true, "sudo": true, "try": true}
	for i := 0; i < len(fields); i++ {
		w := clean(fields[i])
		switch w {
		case "gvm":
			invoked := strings.HasPrefix(fields[i], "`")
			if i > 0 && commandContext[clean(fields[i-1])] {
				invoked = true
			}
			if !invoked || i+1 >= len(fields) {
				continue
			}
			verb := clean(fields[i+1])
			if verb == "" || futureCommands[verb] {
				continue
			}
			cmd, ok := lookup(verb)
			if !ok {
				t.Errorf("%s:%d: Fix names `gvm %s`, which is not a command:\n  %q", file, line, verb, s)
			} else if cmd.Avail.State == avail.NotInBuild {
				t.Errorf("%s:%d: Fix names `gvm %s`, which is not available in this build:\n  %q", file, line, verb, s)
			}
		case "systemctl":
			// systemctl <action> <unit…>: skip the action and any flags, then
			// every remaining word is a unit to validate.
			for j := i + 1; j < len(fields); j++ {
				if strings.HasPrefix(fields[j], "-") {
					continue
				}
				u := clean(fields[j])
				if u == "" || systemctlActions[u] {
					continue
				}
				base := strings.TrimSuffix(u, ".service")
				if !units[u] && !units[base] && !knownExternalUnits[base] {
					t.Errorf("%s:%d: Fix names `systemctl … %s`, which is not a packaged unit or postgresql/caddy/ufw:\n  %q", file, line, u, s)
				}
			}
		}
	}
}

// systemctlActions are the systemctl verbs, so the unit scan can skip them and
// validate only the unit operands.
var systemctlActions = map[string]bool{
	"start": true, "stop": true, "restart": true, "reload": true,
	"enable": true, "disable": true, "status": true, "mask": true,
	"unmask": true, "daemon-reload": true, "is-active": true, "is-enabled": true,
}

// packagedUnitBases is the set of unit file names this build ships, plus their
// bare bases, so a Fix may name either "gvm@.service" or "gvm@".
func packagedUnitBases(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := packaging.FS.ReadDir("systemd")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		out[name] = true
		out[strings.TrimSuffix(name, ".service")] = true
	}
	return out
}
