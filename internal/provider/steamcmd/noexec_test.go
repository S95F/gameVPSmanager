package steamcmd

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// This package spawns nothing. The file is the assertion.
//
// SteamCMD self-updates and runs Valve-supplied code, so it executes in a
// transient systemd SERVICE as the instance user (D-16) -- not as root, not in
// a scope, and not from here. The Runner interface is where the job layer plugs
// that in, and the whole value of the seam evaporates the first time something
// in this package reaches for os/exec "just for the binary search".
//
// A PATH lookup is on the list for the same reason: Locator resolves PATH by
// hand, with an injectable stat, which is ten lines and keeps this test
// passing. exec.LookPath would also decide which binary installs a game based
// on an environment gvm inherited.
// The import gate below is the authoritative half and the token scan is the
// second line of defence; the module-level import-GRAPH assertion in
// internal/platform/deps_test.go is the first, because neither of these can see
// an indirect import of internal/jobs, which exports RunSandboxed.
var forbiddenImports = map[string]string{
	"os/exec": "third-party code runs in a transient systemd service, not in gvm's own process " +
		"(D-16)",
	"syscall":               "raw syscalls belong in internal/platform behind a build tag",
	"golang.org/x/sys/unix": "a Linux-only import would break the cross-platform build",
}

var forbiddenTokens = []struct {
	token string
	why   string
}{
	{"os/exec", "third-party code runs in a transient systemd service, not in gvm's own process (D-16)"},
	{"exec.Command", "the provider renders a runscript; the sandbox runs it"},
	{"exec.LookPath", "Locator resolves PATH itself so the search is testable and this stays true"},
	{"syscall.Exec", "nothing here replaces its own process"},
	// These three START a process without replacing the caller, which is why
	// syscall.Exec alone was not enough -- and `os` is already imported by
	// buildid.go, fake.go, shims.go and steamcmd.go, so os.StartProcess is the
	// one route that needs no new import and no shell.
	{"os.StartProcess", "the low-level spawn; `os` is already imported here"},
	{"syscall.StartProcess", "the same spawn, one layer down"},
	{"plugin.Open", "loading a shared object runs its init in gvm's own process"},
	{"sh -c", "there is no shell anywhere in this path"},
	{"bash -c", "there is no shell anywhere in this path"},
	// The runscript this package renders is a steamcmd command file, not a
	// shell script, so none of these can appear legitimately.
	{"/bin/sh", "there is no shell anywhere in this path"},
	{"/bin/bash", "there is no shell anywhere in this path"},
	{"/usr/bin/env", "env is a program that runs another program"},
}

// TestTheDriverImportsNothingThatCanSpawn is the comment-proof half.
//
// A token scan reads code text and can be defeated by a spelling it has not
// heard of; an import gate cannot, and it closes every syscall-level spawn in
// one assertion -- leaving only the os.* names, which do need the scan. It is
// the shape four sibling packages (ports, hostops, lifecycle, reconcile)
// already carry.
func TestTheDriverImportsNothingThatCanSpawn(t *testing.T) {
	fset := token.NewFileSet()
	var scanned int
	for name, src := range driverSources(t) {
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		scanned++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if why, bad := forbiddenImports[path]; bad {
				t.Errorf("%s imports %q (%s)", name, path, why)
			}
		}
	}
	if scanned < 8 {
		t.Fatalf("parsed only %d source files; the walk is broken", scanned)
	}
}

// driverSources is this package's non-test Go files.
func driverSources(t *testing.T) map[string][]byte {
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
	return out
}

func TestTheDriverSpawnsNothing(t *testing.T) {
	var scanned int
	for name, b := range driverSources(t) {
		scanned++
		for i, line := range strings.Split(string(b), "\n") {
			// Comments are where these tokens legitimately appear, explaining
			// why they are absent from the code.
			code, _, _ := strings.Cut(line, "//")
			if strings.TrimSpace(code) == "" {
				continue
			}
			for _, f := range forbiddenTokens {
				if strings.Contains(code, f.token) {
					t.Errorf("%s:%d contains %q (%s):\n\t%s",
						name, i+1, f.token, f.why, strings.TrimSpace(code))
				}
			}
		}
	}
	// A scan that found no files would pass silently forever.
	if scanned < 8 {
		t.Fatalf("scanned only %d source files; the walk is broken", scanned)
	}
}
