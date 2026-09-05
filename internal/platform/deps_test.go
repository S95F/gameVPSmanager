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
