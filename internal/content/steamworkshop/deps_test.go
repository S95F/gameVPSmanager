package steamworkshop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// D-16 on the IMPORT GRAPH, for the package that parses attacker-supplied
// JSON and HTML.
//
// `gvm content-fetch` runs as neither root nor the instance user, in no group
// that can reach admin.sock, with no Postgres grant (section 12.4, section
// 7.7). That containment is a systemd unit; this test is the part of it the
// compiler can keep, and it is on the graph rather than on the source text
// because the ways it would actually go wrong are all INDIRECT. internal/jobs
// exports RunSandboxed, so importing anything that reaches it is a process
// spawn with an extra step -- and no grep of this package's own files can see
// that.
func TestTheWorkshopSourceCannotSpawnAProcessOrReachASecret(t *testing.T) {
	forbidden := map[string]string{
		"os/exec": "this package parses a JSON document and an HTML page that Steam, and " +
			"therefore any workshop author, controls; a process spawn in that call graph is " +
			"remote code execution with one bug in between",
		"github.com/S95F/gameVPSmanager/internal/jobs": "internal/jobs exports RunSandboxed, " +
			"so reaching it is a process spawn with an extra step",
		"github.com/S95F/gameVPSmanager/internal/secrets": "the keyless endpoints need no " +
			"credential at all, which is the whole reason a workshop sync works on a fresh " +
			"VPS; a secrets import here would be a key this source has no use for",
		"github.com/S95F/gameVPSmanager/internal/audit": "what this source did is recorded by " +
			"the job that called it; it has no audit sink and no Postgres grant",
		"github.com/S95F/gameVPSmanager/internal/gamecfg": "Resolve and Observe are handed the " +
			"two write keys' values as strings (content.Live) precisely so this package " +
			"cannot open the ini -- the RCON password lives in the same file as Mods=",
		"github.com/S95F/gameVPSmanager/internal/hostops": "this package produces bytes and " +
			"lines; the privileged writer that puts them down is somewhere else",
		"github.com/S95F/gameVPSmanager/internal/lifecycle": "the acquisition boot of section " +
			"12.6 step 5 belongs to the job layer that holds the instance lock, not to the " +
			"source that noticed the bytes were missing",
	}
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			for _, dep := range deps(t, "./internal/content/steamworkshop", goos) {
				if why, bad := forbidden[dep]; bad {
					t.Errorf("internal/content/steamworkshop reaches %q on GOOS=%s (%s)", dep,
						goos, why)
				}
			}
		})
	}
}

// TestThisPackageIsTheOneThatTalksToSteam is the other half, so the assertion
// above cannot pass because `go list` quietly returned something useless, and
// so that internal/content's own "cannot reach net/http" gate stays meaningful:
// the fetching lives HERE.
func TestThisPackageIsTheOneThatTalksToSteam(t *testing.T) {
	var http, fetch bool
	for _, dep := range deps(t, "./internal/content/steamworkshop", "linux") {
		switch dep {
		case "net/http":
			http = true
		case "github.com/S95F/gameVPSmanager/internal/content/fetch":
			fetch = true
		}
	}
	if !http || !fetch {
		t.Fatalf("net/http reached: %t, internal/content/fetch reached: %t; if this package no "+
			"longer fetches, the assertions about what it may not reach prove nothing", http,
			fetch)
	}
}

// deps resolves one package's full dependency list for one target. It mirrors
// internal/platform/deps_test.go's helper, which is the module-wide home for
// this kind of gate; this copy exists because the rule it enforces is about
// this package and should fail here.
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
		t.Fatalf("go list -deps %s returned %d packages; the resolution is broken and an "+
			"empty answer would make every assertion here vacuous", pkg, len(list))
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
