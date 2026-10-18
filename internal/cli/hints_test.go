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

	"gopkg.in/yaml.v3"
)

// TestHintsResolve is usability principle U2: every printed `gvm …` command
// runs, or it says it is not available in this build.
//
// It reads the places a hint actually reaches an operator — every string
// literal in non-test Go under internal/, the hint/note/help fields of
// games/*.yaml, and the fenced code blocks of README.md and docs/*.md — and
// checks every command-shaped `gvm <verb> [<sub>]` it finds against the real
// dispatch tables: a verb that does not exist (manifest, instance), a
// subcommand a grouped verb does not have (content resolve-blocked, game
// approve), or an enum-flag value outside its set (--role owner) is a dead end.
//
// Scope and the one allowlist. The only exemption is a literal that also
// contains the exact phrase "not available in this build" — an honest refusal
// is allowed to name the thing it is refusing. The check deliberately does NOT
// flag a reference to a routed-but-unbuilt command such as `gvm update
// --repair`: that command resolves and refuses honestly (Main prints the
// standard "not available in this build" line and exits 3), so it is not a dead
// end, and flagging every such reference inside engine packages that only run
// once their engine is wired would be noise, not safety. What it catches is the
// thing that produces "unknown command": a verb or subcommand that was never
// built, and an enum value that is not in the enum.
//
// The DoD: this is green on the tree as shipped, and goes red the moment
// `gvm manifest upgrade` or `--role owner` is added to any scanned literal.
func TestHintsResolve(t *testing.T) {
	// Known top-level verbs come from the real dispatch table, plus the `mods`
	// alias, so a new command is covered the day it is added.
	known := map[string]bool{"mods": true}
	for _, c := range commands {
		known[c.Name] = true
	}

	// Strictly-grouped verbs: an unknown second token is a dead subcommand, not
	// an instance name. backup is intentionally absent — `gvm backup pz-main`
	// is the nightly-by-instance form, so its second token is not a subcommand.
	strict := map[string]map[string]bool{
		"game":    subNames(gameNames()),
		"content": subNames(contentNames()),
		"mods":    subNames(contentNames()),
		"job":     subNames(jobNames()),
	}

	// Command-shaped words that are deliberately NOT commands in this build.
	// They look like `gvm <noun> <verb>` and would read as real to an operator,
	// so a reintroduced one must turn this test red (the DoD names the first).
	deadPhrases := map[string]bool{
		"manifest upgrade": true,
		"instance promote": true,
	}

	// Enum flags and their value sets. A value outside the set is a dead hint
	// the same way a missing verb is.
	enums := map[string]map[string]bool{
		// owner joined the role_t enum with the §15.2 matrix (Phase 47).
		"role":       set("viewer", "operator", "admin", "owner"),
		"on-failure": set("fail", "start_anyway", "warn"),
	}

	cmdRe := regexp.MustCompile(`(?:sudo\s+)?gvm\s+([a-z][a-z0-9-]*)(?:\s+([a-z][a-z0-9-]*))?`)
	flagRe := regexp.MustCompile(`--([a-z][a-z0-9-]*)[ =]([a-z_][a-z0-9_]*)`)

	check := func(t *testing.T, where, s string) {
		if strings.Contains(s, "not available in this build") {
			return // an honest refusal may name what it refuses
		}
		for _, m := range cmdRe.FindAllStringSubmatch(s, -1) {
			verb, sub := m[1], m[2]
			switch {
			case !known[verb]:
				if deadPhrases[verb+" "+sub] {
					t.Errorf("%s: `gvm %s %s` is not a command in this build: %q",
						where, verb, sub, s)
				}
			case strict[verb] != nil && sub != "" && !strict[verb][sub]:
				t.Errorf("%s: `gvm %s` has no subcommand %q: %q", where, verb, sub, s)
			}
		}
		for _, m := range flagRe.FindAllStringSubmatch(s, -1) {
			flag, val := m[1], m[2]
			if vs := enums[flag]; vs != nil && !vs[val] {
				t.Errorf("%s: --%s has no value %q (want %s): %q",
					where, flag, val, strings.Join(sortedKeys(vs), "|"), s)
			}
		}
	}

	for _, lit := range goStringLiterals(t, repoRoot(t)) {
		check(t, lit.where, lit.text)
	}
	for _, lit := range manifestHintFields(t, filepath.Join(repoRoot(t), "games")) {
		check(t, lit.where, lit.text)
	}
	for _, lit := range docFencedBlocks(t, repoRoot(t)) {
		check(t, lit.where, lit.text)
	}
}

type hintLit struct{ where, text string }

func set(vs ...string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func gameNames() []string {
	out := make([]string, 0, len(gameSubs))
	for _, s := range gameSubs {
		out = append(out, s.name)
	}
	return out
}

func contentNames() []string {
	out := make([]string, 0, len(contentSubs))
	for _, s := range contentSubs {
		out = append(out, s.name)
	}
	return out
}

func jobNames() []string {
	out := make([]string, 0, len(jobSubs))
	for _, s := range jobSubs {
		out = append(out, s.name)
	}
	return out
}

func subNames(names []string) map[string]bool { return set(names...) }

// repoRoot walks up from the cli package directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the module root (go.mod)")
	return ""
}

// goStringLiterals returns every string literal in non-test .go files under
// internal/.
func goStringLiterals(t *testing.T, root string) []hintLit {
	t.Helper()
	var out []hintLit
	err := filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // a file that does not parse is a different test's problem
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(f, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			s, uerr := strconv.Unquote(bl.Value)
			if uerr != nil {
				s = bl.Value
			}
			out = append(out, hintLit{where: rel + ":" + lineOf(fset, bl.Pos()), text: s})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func lineOf(fset *token.FileSet, p token.Pos) string {
	return strconv.Itoa(fset.Position(p).Line)
}

// manifestHintFields returns every hint/note/help field value in games/*.yaml.
// metadata.doc and descriptions are NOT scanned: long-form documentation may
// name a forthcoming command, and only the operator-facing hint/note/help lines
// are U2's concern.
func manifestHintFields(t *testing.T, dir string) []hintLit {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out []hintLit
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var tree any
		if err := yaml.Unmarshal(raw, &tree); err != nil {
			continue
		}
		base := filepath.Base(f)
		for _, v := range collectKeys(tree, "hint", "note", "help") {
			out = append(out, hintLit{where: base + " (hint/note/help)", text: v})
		}
	}
	return out
}

// collectKeys walks a decoded YAML tree and returns the string values of any
// map key whose name is one of want.
func collectKeys(node any, want ...string) []string {
	wanted := set(want...)
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			for k, child := range v {
				if s, ok := child.(string); ok && wanted[k] {
					out = append(out, s)
				}
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(node)
	return out
}

// docFencedBlocks returns the text of fenced code blocks in README.md and
// docs/*.md, excluding the target-design documents (ARCHITECTURE,
// IMPLEMENTATION-PLAN, USABILITY-PLAN), which carry a banner instead.
func docFencedBlocks(t *testing.T, root string) []hintLit {
	t.Helper()
	paths := []string{filepath.Join(root, "README.md")}
	md, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, md...)

	skip := set("ARCHITECTURE.md", "IMPLEMENTATION-PLAN.md", "USABILITY-PLAN.md")
	var out []hintLit
	for _, p := range paths {
		if skip[filepath.Base(p)] {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(root, p)
		inFence := false
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence
				continue
			}
			if inFence {
				out = append(out, hintLit{
					where: rel + ":" + strconv.Itoa(i+1), text: line})
			}
		}
	}
	return out
}
