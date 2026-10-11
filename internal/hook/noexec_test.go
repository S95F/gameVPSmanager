package hook

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/console"
)

// A hook is a Go function, not a shell snippet. The file is the assertion.
//
// This is the structural half of D-21. A manifest names hooks from a closed
// registry; if a hook could shell out, the manifest would become a scripting
// language through the back door -- which is exactly what internal/spec's
// closed template grammar (no if, no range, no printf, seven predicates and
// `not`) exists to prevent. Closing one door and leaving the other open would
// be worse than closing neither, because the schema would look safe.
//
// os/exec also has a second, independent reason to be absent: post_install and
// post_content hooks run in the JOB process, which is root. Anything that
// executes code here executes it as root, and D-16 says third-party code runs
// in a transient systemd service as the account that owns the destination. A
// hook that needs to run a program is a Provider.
// The import gate below is the authoritative half and the token scan is the
// second line of defence; the module-level import-GRAPH assertion in
// internal/platform/deps_test.go is the first, because neither of these can see
// an indirect import of internal/jobs, which exports RunSandboxed.
var forbiddenImports = map[string]string{
	"os/exec": "a hook that needs to run a program is a Provider; post_install runs in the job " +
		"process as root and D-16 says third-party code runs in a transient service as the " +
		"instance user",
	"syscall":               "raw syscalls belong in internal/platform behind a build tag",
	"golang.org/x/sys/unix": "a Linux-only import would break the cross-platform build",
}

var forbiddenTokens = []struct {
	token string
	why   string
}{
	{"os/exec", "a hook that needs to run a program is a Provider; post_install runs as root " +
		"and D-16 says third-party code runs in a transient service as the instance user"},
	{"exec.Command", "hooks are Go functions, not shell snippets"},
	{"exec.LookPath", "nothing here chooses a program to run"},
	{"syscall.Exec", "nothing here replaces its own process"},
	// These three START a process without replacing the caller, which is why
	// syscall.Exec alone was not enough -- and `os` is already imported in this
	// package, so os.StartProcess is the one route that needs no new import and
	// no shell. A hook calling a game's own helper binary that way is a
	// plausible accident, not deliberate evasion.
	{"os.StartProcess", "the low-level spawn; `os` is already imported here"},
	{"syscall.StartProcess", "the same spawn, one layer down"},
	{"plugin.Open", "loading a shared object runs its init as root, with no sandbox at all"},
	{"sh -c", "a shell here would make the manifest a scripting language through the back door"},
	{"bash -c", "a shell here would make the manifest a scripting language through the back door"},
	{"/bin/sh", "there is no shell anywhere in this package"},
	{"/bin/bash", "there is no shell anywhere in this package"},
	{"/usr/bin/env", "env is a program that runs another program"},
}

// TestNoHookImportsSomethingThatCanSpawn is the comment-proof half.
//
// A token scan reads code text and can be defeated by a spelling it has not
// heard of; an import gate cannot, and it closes every syscall-level spawn in
// one assertion -- leaving only the os.* names, which do need the scan. It is
// the shape four sibling packages (ports, hostops, lifecycle, reconcile)
// already carry, and the two packages with the highest stakes had only the
// weaker half.
func TestNoHookImportsSomethingThatCanSpawn(t *testing.T) {
	fset := token.NewFileSet()
	var scanned int
	for name, src := range hookSources(t) {
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
	if scanned < 6 {
		t.Fatalf("parsed only %d source files; the walk is broken", scanned)
	}
}

// hookSources is this package's non-test Go files.
func hookSources(t *testing.T) map[string][]byte {
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

func TestHooksAreGoFunctionsAndNotShellSnippets(t *testing.T) {
	var scanned int
	for name, b := range hookSources(t) {
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
	// A scan that found no files would pass silently forever. There are seven
	// non-test files in this package; the floor is deliberately below that so
	// adding one does not break the test, and far enough above zero to catch a
	// walk that stopped working.
	if scanned < 6 {
		t.Fatalf("scanned only %d source files; the walk is broken", scanned)
	}
}

// TestNoHookCanStartTheGame is contract rule 3, read off the type rather than
// off the code.
//
// There is nothing in Ctx that could start, stop or signal a game: the console
// handle is a reader, and there is no process handle, no systemd connection and
// no verb dispatcher. pz.first_boot genuinely needs a boot to have happened and
// asks for one with RequestBoot, which the pipeline may decline -- and that
// asymmetry is the point, because the pipeline owns the readiness deadline and
// the shutdown ladder and a hook does not.
func TestNoHookCanStartTheGame(t *testing.T) {
	// A reader satisfies the whole of the console surface a hook is given. If
	// a writer ever appears on ConsoleView this stops compiling, which is the
	// intent: sending a command to a running game is a verb, and verbs are
	// RBAC-gated and audited.
	var _ ConsoleView = readOnlyConsole{}

	// And the request a hook CAN make is a value, not a call. pz.first_boot
	// records one and the pipeline decides; the hook has no way to act on it.
	var h Ctx
	h.RequestBoot(BootRequest{Why: "x", WaitFor: "SERVER STARTED", Then: "quit"})
	if got := h.BootRequested(); got == nil || got.Then != "quit" {
		t.Fatalf("BootRequested() = %+v", got)
	}
}

type readOnlyConsole struct{}

func (readOnlyConsole) Snapshot(int) []console.Line { return nil }
