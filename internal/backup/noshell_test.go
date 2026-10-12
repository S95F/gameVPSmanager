package backup

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The backup engine takes a snapshot and writes an archive. It starts no
// processes, and these are the two rules that keep that true.
//
// It matters here more than it looks. A backup runs as root, on a timer, with
// the world's own paths and the operator's own template variables in scope --
// which is the exact shape of a thing that should never be allowed to build a
// command line. internal/jobs is where exec legitimately lives (the D-16
// sandbox seam), so the freeze's commands reach the game over the console or
// the control channel and the SteamCMD runs happen inside a transient unit;
// neither of those is a spawn from here.
//
// The import gate is the authoritative control, because an import cannot be
// hidden the way a cleverly spelled token can. The token scan is the second
// line of defence for the spellings that need no new import once `os` is in
// scope.

// spawnImports are import paths that can start a process or reach something
// that can.
var spawnImports = map[string]string{
	"os/exec": "this package orchestrates no subprocesses; the freeze speaks to the game over " +
		"the console or the control channel, and third-party code runs in a transient unit " +
		"through internal/jobs (D-16)",
	"github.com/S95F/gameVPSmanager/internal/jobs": "internal/jobs exports RunSandboxed, so " +
		"importing it is a process spawn with an extra step -- and the job layer declares the " +
		"Backups seam this package satisfies, so the dependency would also point the wrong way",
}

// lowLevelImports may appear in exactly one file: the Linux snapshot.
//
// syscall is there for Stat_t (the btrfs subvolume inode check) and unsafe for
// the one ioctl x/sys/unix does not export. Both are legitimate and both are
// the kind of thing that should be in one auditable place rather than
// anywhere.
var lowLevelImports = map[string]string{
	"syscall": "raw syscalls belong behind a build tag",
	"unsafe":  "pointer arithmetic belongs behind a build tag",
	"golang.org/x/sys/unix": "a Linux-only import outside its build-tagged file breaks the " +
		"Windows development build outright",
}

// lowLevelFiles are the files allowed to hold them.
const lowLevelFile = "snapshot_linux.go"

func TestNothingInThisPackageCanStartAProcess(t *testing.T) {
	fset := token.NewFileSet()
	var scanned int
	for name, src := range packageSources(t) {
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
			if why, bad := spawnImports[path]; bad {
				t.Errorf("%s imports %q (%s)", name, path, why)
			}
			if why, low := lowLevelImports[path]; low && name != lowLevelFile {
				t.Errorf("%s imports %q (%s)", name, path, why)
			}
		}
	}
	if scanned < 8 {
		t.Fatalf("parsed only %d source files; the walk is broken and every assertion above is "+
			"vacuous", scanned)
	}
}

// TestTheEngineRunsNoShell.
//
// The second line of defence. It deliberately does not ban the legacy scripts'
// own names: the engine's comments quote backup.sh's `sleep 15` and its
// `|| true` on purpose, because what those lines did and why they are gone is
// the explanation a reader needs -- and a scan that banned the words would ban
// the explanation rather than the behaviour. Calling one of those scripts needs
// os/exec, which the import gate refuses outright.
func TestTheEngineRunsNoShell(t *testing.T) {
	tokens := []struct{ token, why string }{
		{"exec.Command", "the engine archives; it does not spawn"},
		{"exec.CommandContext", "the engine archives; it does not spawn"},
		{"os.StartProcess", "no spawn needs no new import, which is why this is here"},
		{"/bin/sh", "there is no shell in the backup path"},
		{"/bin/bash", "there is no shell in the backup path"},
		{"/usr/bin/env", "env is a program that runs another program"},
	}
	var scanned int
	for name, b := range packageSources(t) {
		scanned++
		for i, line := range strings.Split(string(b), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if strings.TrimSpace(code) == "" {
				continue
			}
			for _, f := range tokens {
				if strings.Contains(code, f.token) {
					t.Errorf("%s:%d contains %q (%s):\n\t%s", name, i+1, f.token, f.why,
						strings.TrimSpace(code))
				}
			}
		}
	}
	if scanned < 8 {
		t.Fatalf("scanned only %d files; the walk is broken", scanned)
	}
}

// TestEveryLinuxOnlyFileHasItsStub.
//
// Rule 4 of the project, asserted rather than remembered: a _linux.go with no
// matching stub is a package that stops compiling on the development machine,
// and the discovery should come from this message rather than from a wall of
// undefined symbols.
func TestEveryLinuxOnlyFileHasItsStub(t *testing.T) {
	src := packageSources(t)
	var linuxFiles int
	for name := range src {
		base, ok := strings.CutSuffix(name, "_linux.go")
		if !ok {
			continue
		}
		linuxFiles++
		if _, has := src[base+"_stub.go"]; !has {
			t.Errorf("%s has no %s_stub.go; the Windows build would stop compiling", name, base)
		}
	}
	if linuxFiles == 0 {
		t.Fatal("no _linux.go file was found, so this assertion is about nothing")
	}
}

// TestTheBackupEngineCannotReachTheJobLayerOnTheImportGraph.
//
// The import gate above reads this package's own source text; this reads the
// whole resolved graph, which is the only thing that can see an INDIRECT
// route. internal/jobs declares the Backups interface that *Engine satisfies,
// so the dependency is supposed to point from jobs to the seam and never back
// -- and the compile-time assertion that proves the interface is satisfied
// lives in a _test.go file precisely so that the production graph stays this
// shape.
func TestTheBackupEngineCannotReachTheJobLayerOnTheImportGraph(t *testing.T) {
	const forbidden = "github.com/S95F/gameVPSmanager/internal/jobs"
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH to resolve the import graph with")
	}
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, goBin, "list", "-deps", ".")
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go list -deps for GOOS=%s failed:\n%s", goos, out)
			}
			lines := strings.Split(string(out), "\n")
			if len(lines) < 10 {
				t.Fatalf("go list -deps returned %d packages; an empty answer would make this "+
					"assertion vacuous", len(lines))
			}
			for _, line := range lines {
				if strings.TrimSpace(line) == forbidden {
					t.Fatalf("internal/backup reaches %q on GOOS=%s; the seam points the other "+
						"way", forbidden, goos)
				}
			}
		})
	}
}

// packageSources is this package's non-test Go files.
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
	return out
}
