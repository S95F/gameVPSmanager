package hostinit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// hostinit installs the host, so unlike internal/reconcile it is ALLOWED to
// touch the filesystem — writing files is its whole job. What it is not allowed
// to do is the thing an installer reaches for first and regrets: a shell.
// `systemd-sysusers`, `systemd-tmpfiles --create`, the daemon reload — the
// obvious shortcut for all three is `/bin/sh -c "..."`, and each is wrong in
// the same ways (a command string assembled from paths, a program resolved
// through PATH, exit codes parsed back from text). This file is the assertion
// that it never does.
//
// The two programs it MAY run are systemd's own sysusers and tmpfiles tools,
// each by absolute path with an argv, and only from system_linux.go. The OS is
// touched only from there too: the cross-platform files (manifest, apt, the
// orchestration) go through the System interface, so the layering is visible
// and a test can drive an install with no host at all.

// osAllowed is the complete list of files that may import os / os/exec, and
// why. Everything else must reach the host through the System interface.
var osAllowed = map[string]string{
	"system_linux.go": "the Linux System implementation: the one place that writes files and runs systemd's tools",
}

var forbiddenImports = map[string]string{
	"os":       "touching the filesystem outside the Linux System impl; the rest goes through System",
	"os/exec":  "running a program outside the Linux System impl",
	"net/http": "nothing in the install path may reach the network",
}

var forbiddenTokens = []struct{ token, why string }{
	{"exec.LookPath", "a PATH lookup deciding which binary runs as root"},
	{"/bin/sh", "a shell interpreter"},
	{"/bin/bash", "a shell interpreter"},
	{`"sh"`, "a shell interpreter"},
	{`"bash"`, "a shell interpreter"},
	{`"-c"`, "the flag that turns an argv into a command string"},
	{`"systemctl"`, "systemd is reloaded and enabled over D-Bus, never by a subprocess"},
	{`"apt"`, "rule 7: apt lines are printed, never run"},
	{`"apt-get"`, "rule 7: apt lines are printed, never run"},
	{`"ufw"`, "rule 7: the firewall is never touched"},
	{"os.Getenv", "an inherited environment is one more thing that decides what a root process does"},
}

// programLiterals is every absolute program path this package may name. The
// install destinations under /usr/lib are not programs and are not here; only
// the binary's own path and systemd's two tools are.
var programLiterals = []string{
	"/usr/bin/gvm",
	"/usr/bin/systemd-sysusers", "/bin/systemd-sysusers",
	"/usr/bin/systemd-tmpfiles", "/bin/systemd-tmpfiles",
}

func packageSources(t *testing.T) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
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
	for _, want := range []string{"manifest.go", "apt.go", "hostinit.go", "system_linux.go", "system_stub.go"} {
		if _, ok := out[want]; !ok {
			t.Fatalf("%s was not scanned; the walk is broken", want)
		}
	}
	return out
}

func TestHostInitImportsAreLayered(t *testing.T) {
	fset := token.NewFileSet()
	for name, src := range packageSources(t) {
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			why, bad := forbiddenImports[path]
			if !bad {
				continue
			}
			if _, ok := osAllowed[name]; ok {
				continue
			}
			t.Errorf("%s imports %q (%s); only %v may", name, path, why, sortedKeys(osAllowed))
		}
	}
}

func TestHostInitNamesNoShellAndNoForbiddenTool(t *testing.T) {
	for name, src := range packageSources(t) {
		for i, line := range strings.Split(string(src), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if strings.TrimSpace(code) == "" {
				continue
			}
			for _, f := range forbiddenTokens {
				if strings.Contains(code, f.token) {
					t.Errorf("%s:%d contains %q (%s):\n\t%s", name, i+1, f.token, f.why, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestEveryProgramPathIsDeclared catches a new absolute program path added to
// a file already allowed to exec.
func TestEveryProgramPathIsDeclared(t *testing.T) {
	fset := token.NewFileSet()
	prefixes := []string{"/bin/", "/sbin/", "/usr/bin/", "/usr/sbin/", "/usr/local/bin/", "/usr/local/sbin/"}
	for name, src := range packageSources(t) {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, p := range prefixes {
				if !strings.HasPrefix(s, p) {
					continue
				}
				if slices.Contains(programLiterals, s) {
					return true
				}
				t.Errorf("%s:%d names %q, a program path nobody declared",
					name, fset.Position(lit.Pos()).Line, s)
			}
			return true
		})
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
