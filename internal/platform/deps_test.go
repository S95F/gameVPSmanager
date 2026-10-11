package platform

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// forbidden is the import that must never be reachable from a build that is
// not Linux. It compiles on Linux and macOS and not at all on Windows, so a
// leak into an untagged file breaks the development machine outright -- which
// is a discovery every developer should make from this test's message rather
// than from a wall of "undefined: unix.Statfs".
const forbidden = "golang.org/x/sys/unix"

// TestNoUnixImportOutsideLinuxBuilds resolves the whole module's import graph
// for two non-Linux targets and fails if x/sys/unix is reachable from either.
//
// It is module-wide rather than package-local on purpose: this package is the
// designated home for Linux-only code, but hostops, ports and reconcile all
// grow _linux.go files later, and the assertion that keeps the cross-platform
// build honest has to outlive the package it was written in.
func TestNoUnixImportOutsideLinuxBuilds(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH to resolve the import graph with")
	}
	root := moduleRoot(t)

	// windows is the development machine and cannot compile x/sys/unix at
	// all; darwin can, so only darwin catches a leak into a file tagged
	// `!linux` rather than untagged. Both are needed.
	for _, goos := range []string{"windows", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			cmd := exec.CommandContext(ctx, goBin, "list", "-deps", "./...")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go list -deps for GOOS=%s failed, which is itself the leak this test looks for:\n%s", goos, out)
			}
			for _, line := range strings.Split(string(out), "\n") {
				if strings.TrimSpace(line) == forbidden {
					t.Fatalf("%s is reachable from a GOOS=%s build; a Linux-only import escaped its //go:build linux file", forbidden, goos)
				}
			}
		})
	}
}

// TestUnixImportIsReachableOnLinux is the other half: if the Linux side ever
// stops importing x/sys/unix, the test above passes for the wrong reason and
// stops proving anything.
func TestUnixImportIsReachableOnLinux(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH to resolve the import graph with")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, goBin, "list", "-deps", "./internal/platform")
	cmd.Dir = moduleRoot(t)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps for GOOS=linux failed:\n%s", out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == forbidden {
			return
		}
	}
	t.Fatalf("%s is no longer imported on Linux, so TestNoUnixImportOutsideLinuxBuilds proves nothing", forbidden)
}

// The two packages that must not be able to spawn a process at all, asserted
// on the IMPORT GRAPH rather than on the source text.
//
// Section 8.8's hook contract rule 1 is "a hook MUST NOT exec a process", and
// the mechanism the document prescribes for it is a grep of the package for
// os/exec, sh -c, bash -c and syscall.Exec. A grep cannot see an INDIRECT
// import: internal/jobs exports RunSandboxed, nothing imports internal/hook, and
// internal/jobs imports none of hook's dependencies -- so there is no cycle
// stopping a hook from importing the job layer and calling it, and that walks
// past every token on every list. The same holds for the SteamCMD driver, whose
// whole D-16 seam is that the sandbox runs the process and the driver does not.
//
// It lives beside the x/sys/unix assertion for the reason that file already
// gives: a dependency rule enforced from inside the package it constrains dies
// with the package, and this one has to outlive both. The two forbidden paths
// are checked for linux AND windows, because a //go:build linux file is invisible
// to a Windows-only resolution.
func TestNothingExecutableIsReachableFromAHookOrTheSteamCMDDriver(t *testing.T) {
	const jobs = "github.com/S95F/gameVPSmanager/internal/jobs"
	forbidden := map[string]string{
		"os/exec": "a hook that needs to run a program is a Provider, and the driver's runs " +
			"happen in a transient systemd service as the instance user (D-16); post_install " +
			"runs in the job process as root",
		jobs: "internal/jobs exports RunSandboxed, so importing it is a process spawn with an " +
			"extra step -- and one no source-text grep of either package can see",
	}
	for _, pkg := range []string{
		"./internal/hook",
		"./internal/provider/steamcmd",
	} {
		for _, goos := range []string{"linux", "windows"} {
			t.Run(strings.TrimPrefix(pkg, "./internal/")+"/"+goos, func(t *testing.T) {
				for _, dep := range deps(t, pkg, goos) {
					if why, bad := forbidden[dep]; bad {
						t.Errorf("%s reaches %q on GOOS=%s (%s)", pkg, dep, goos, why)
					}
				}
			})
		}
	}
}

// And the other half, so the test above cannot pass for the wrong reason.
//
// If the sandbox stops exec'ing, or `go list` silently starts returning
// nothing, the assertion above proves nothing at all. internal/jobs is where
// os/exec legitimately lives -- it is the D-16 seam's implementation -- so its
// own forbidden properties (--scope, a shell) are string-level and stay with
// its source scan.
func TestTheJobLayerIsStillWhereExecLives(t *testing.T) {
	for _, dep := range deps(t, "./internal/jobs", "linux") {
		if dep == "os/exec" {
			return
		}
	}
	t.Fatal("internal/jobs no longer reaches os/exec, so the assertion that internal/hook and " +
		"internal/provider/steamcmd cannot reach it proves nothing")
}

// deps resolves one package's full dependency list for one target.
func deps(t *testing.T, pkg, goos string) []string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH to resolve the import graph with")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, goBin, "list", "-deps", pkg)
	cmd.Dir = moduleRoot(t)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s for GOOS=%s failed:\n%s", pkg, goos, out)
	}
	var list []string
	for _, line := range strings.Split(string(out), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			list = append(list, s)
		}
	}
	if len(list) < 10 {
		t.Fatalf("go list -deps %s returned %d packages; the resolution is broken and an empty "+
			"answer would make every assertion below vacuous", pkg, len(list))
	}
	return list
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
