package ports

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The allocator never runs a program. This file is the assertion.
//
// Why it needs one: the obvious way to find out what is listening on a host is
// `ss -lntup`, and it is wrong in four ways at once. Its output format differs
// between iproute2 versions and distributions, so the parser silently stops
// matching after a distro upgrade and every port looks free. It resolves the
// binary through PATH, so what answers depends on the environment gvm inherited
// from systemd. It can block -- on a name lookup, on a hung netlink socket --
// inside a `gvm create` that has no timeout for it. And it reads exactly the
// same /proc files this package reads directly, so the subprocess buys nothing
// at all. A wrong answer here hands a live port to a new instance.

var forbiddenImports = map[string]string{
	"os/exec":               "running a program to find out what is listening",
	"syscall":               "raw syscalls belong in internal/platform behind a build tag",
	"golang.org/x/sys/unix": "a Linux-only import would break the cross-platform build",
}

var forbiddenTokens = []struct {
	token string
	why   string
}{
	{"exec.Command", "spawning a process"},
	{"exec.LookPath", "a PATH lookup deciding which binary answers"},
	{`"ss"`, "the iproute2 socket tool, whose output format is a moving target"},
	{`"netstat"`, "net-tools, deprecated and absent on a minimal Debian"},
	{"/bin/sh", "a shell interpreter"},
	{"/bin/bash", "a shell interpreter"},
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
	if len(out) < 6 {
		t.Fatalf("scanned only %d source files; the walk is broken", len(out))
	}
	for _, want := range []string{"procnet.go", "listeners.go", "allocate.go"} {
		if _, ok := out[want]; !ok {
			t.Fatalf("%s was not scanned", want)
		}
	}
	return out
}

func TestThePortsPackageRunsNoPrograms(t *testing.T) {
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
			if why, bad := forbiddenImports[path]; bad {
				t.Errorf("%s imports %q (%s)", name, path, why)
			}
		}
	}
}

func TestThePortsPackageNamesNoExternalTool(t *testing.T) {
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
					t.Errorf("%s:%d contains %q (%s):\n\t%s", name, i+1, f.token, f.why, strings.TrimSpace(line))
				}
			}
		}
	}
}

func TestTheProcReaderTakesItsSourceAsASeam(t *testing.T) {
	// The structural assertion has a behavioural half: everything except the
	// four os calls in DirProcSource goes through ProcSource, which is how the
	// whole scan runs against fixtures on a machine with no /proc.
	var _ ProcSource = DirProcSource{}
	var _ ProcSource = fakeProc{}

	if _, err := CollectListeners(wholeHost()); err != nil {
		t.Fatalf("the fixture-backed scan must work with no host to read: %v", err)
	}
}
