package reconcile

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

// reconcile runs two programs and no shell, and touches no file itself. This
// file is the assertion.
//
// Why it needs one. Everything this package decides ends up as root's work,
// and the obvious shortcut for half of it is a shell: `systemctl daemon-reload
// && systemctl enable gvm@$name`, `install -m 0644 ... $path`. Each is wrong
// in the same four ways -- the command string is assembled from an instance
// name and a path so a quoting bug becomes a root shell; the program is
// resolved through PATH so what answers depends on an inherited environment;
// the output has to be parsed back and systemctl's exit codes conflate "the
// unit failed" with "the unit does not exist"; and a subprocess that hangs
// hangs a root process that has no timeout for it.
//
// The second half is the layering: every effect here is a hostops.Request. A
// reconciler with its own os.WriteFile would put a write outside the closed
// privileged surface, which is the one thing that surface is for.
//
// The two exceptions are systemd-analyze and systemd-run, and section 7.5 and
// 7.6 name both. systemd's calendar grammar has no Go implementation worth
// trusting and the authority on whether systemd accepts a string is systemd;
// a transient unit carrying arbitrary sandbox properties is not expressible
// over the manager's D-Bus API without reimplementing systemd-run.

// execAllowed is the complete list of files that may run a program, and why.
var execAllowed = map[string]string{
	"calendar_linux.go": "systemd's calendar grammar has no Go implementation worth trusting (7.5)",
	"probe_linux.go":    "a transient unit with arbitrary sandbox properties is systemd-run's job (7.6)",
}

// fsAllowed is the complete list of files that may touch the filesystem, and
// why. Every other effect in this package is a hostops.Request.
var fsAllowed = map[string]string{
	"calendar_linux.go": "stat, to resolve an absolute program path instead of searching PATH",
	"probe_linux.go":    "the same, for systemd-run",
}

var forbiddenImports = map[string]string{
	"os/exec":               "running a program",
	"os":                    "touching the filesystem; every effect here is a hostops.Request",
	"syscall":               "raw syscalls belong in internal/platform behind a build tag",
	"golang.org/x/sys/unix": "a Linux-only import would break the cross-platform build",
	"net/http":              "nothing in the write path may reach the network",
}

var forbiddenTokens = []struct {
	token string
	why   string
}{
	{"exec.LookPath", "a PATH lookup deciding which binary runs as root"},
	{"/bin/sh", "a shell interpreter"},
	{"/bin/bash", "a shell interpreter"},
	{`"sh"`, "a shell interpreter"},
	{`"bash"`, "a shell interpreter"},
	{`"-c"`, "the flag that turns an argv into a command string"},
	{`"systemctl"`, "systemd is driven through hostops over D-Bus, never by a subprocess"},
	{"os.Getenv", "an inherited environment is one more thing that decides what a root process does"},
	{"os.WriteFile", "the write path is hostops.Do and nothing else"},
	{"os.MkdirAll", "MkdirAll gives every intermediate directory the leaf's mode; dir.ensure exists"},
}

// programLiterals is every absolute program path this package may name.
//
// Anything else appearing as a string literal is a new program running as
// root, which is a decision that belongs in a review and not in a diff nobody
// looked at twice.
var programLiterals = []string{
	"/usr/bin/systemd-analyze", "/bin/systemd-analyze",
	"/usr/bin/systemd-run", "/bin/systemd-run",
	// The binary the probe unit executes. It is the PACKAGED path, because
	// the sandbox applies to the file that runs and probing with anything
	// else would test a path the unit never takes.
	"/usr/bin/gvm",
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
	// A walk that found nothing would pass silently forever.
	if len(out) < 10 {
		t.Fatalf("scanned only %d source files; the walk is broken", len(out))
	}
	for _, want := range []string{
		"plan.go", "apply.go", "units.go", "timers.go", "diff.go", "dirs.go", "users.go",
		"probe.go", "probe_linux.go", "probe_stub.go", "calendar.go", "calendar_linux.go",
	} {
		if _, ok := out[want]; !ok {
			t.Fatalf("%s was not scanned", want)
		}
	}
	return out
}

func TestReconcileRunsNothingButSystemdsOwnTools(t *testing.T) {
	fset := token.NewFileSet()
	for name, src := range packageSources(t) {
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			why, bad := forbiddenImports[path]
			if !bad {
				continue
			}
			allowed := execAllowed
			if path == "os" {
				allowed = fsAllowed
			}
			if _, ok := allowed[name]; ok {
				continue
			}
			t.Errorf("%s imports %q (%s); only %v may, and only for the reason recorded there",
				name, path, why, sortedNames(allowed))
		}
	}
}

func TestReconcileNamesNoShellAndNoExternalTool(t *testing.T) {
	for name, src := range packageSources(t) {
		for i, line := range strings.Split(string(src), "\n") {
			// Comments are where these names legitimately appear, explaining
			// why the code does not use them.
			code, _, _ := strings.Cut(line, "//")
			if strings.TrimSpace(code) == "" {
				continue
			}
			for _, f := range forbiddenTokens {
				if strings.Contains(code, f.token) {
					t.Errorf("%s:%d contains %q (%s):\n\t%s", name, i+1, f.token, f.why,
						strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestEveryProgramPathIsDeclared catches the case the import check cannot: a
// new absolute path added to an existing candidate list, in a file that is
// already allowed to exec.
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
				t.Errorf("%s:%d names %q, which is a program running as root that nobody declared",
					name, fset.Position(lit.Pos()).Line, s)
			}
			return true
		})
	}
}

// TestTheCandidateListsAreAbsoluteAndAreSystemdsOwnTools is the runtime half:
// the static scan proves no new literal appeared, this proves the ones that
// are there are what they claim to be.
func TestTheCandidateListsAreAbsoluteAndAreSystemdsOwnTools(t *testing.T) {
	for _, list := range candidateLists() {
		if len(list) == 0 {
			t.Fatal("an empty candidate list would resolve to nothing and report unchecked forever")
		}
		for _, p := range list {
			if !strings.HasPrefix(p, "/") {
				t.Errorf("%q is not absolute; PATH would decide what runs as root", p)
			}
			base := p[strings.LastIndex(p, "/")+1:]
			if !slices.Contains([]string{"systemd-analyze", "systemd-run"}, base) {
				t.Errorf("%q is not one of the two systemd tools this package may run", p)
			}
		}
	}
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
