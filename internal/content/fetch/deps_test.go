package fetch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The two halves of D-16, asserted on the IMPORT GRAPH rather than on the
// source text.
//
// `gvm content-fetch` is the only part of gvm that talks to the internet AND
// parses attacker-supplied archives, so it runs as neither root nor the
// instance user, in no group that can reach admin.sock, with no Postgres grant
// (section 12.4, section 7.7). That containment is a systemd unit; these tests
// are the part of it the compiler can keep.
//
// A source-text grep cannot see an INDIRECT import, which is where both of
// these would actually go wrong: internal/jobs exports RunSandboxed, so
// importing anything that imports it is a process spawn with an extra step, and
// internal/secrets would arrive through a package that merely wanted a config
// type. The import graph sees all of it.

func TestTheFetcherCannotSpawnAProcessOrReachASecret(t *testing.T) {
	forbidden := map[string]string{
		"os/exec": "the fetcher parses attacker-supplied JSON, HTML and archives; a process " +
			"spawn in that call graph is remote code execution with one bug in between",
		"github.com/S95F/gameVPSmanager/internal/jobs": "internal/jobs exports RunSandboxed, " +
			"so importing it is a process spawn with an extra step -- and one no source-text " +
			"grep of this package can see",
		"github.com/S95F/gameVPSmanager/internal/secrets": "gvm-fetch holds no secrets at " +
			"rest; a CurseForge or Modrinth token arrives as a header the CALLER sets from " +
			"LoadCredential (section 12.4), so this package has no business reading one",
		"github.com/S95F/gameVPSmanager/internal/audit": "the fetcher has no audit sink and " +
			"no Postgres grant; what it did is recorded by the job that called it",
		"github.com/S95F/gameVPSmanager/internal/content": "the fetcher is a leaf. If it could " +
			"see the plan, the lock or the two namespaces, the package that chooses URLs and " +
			"the package that decides what is accounted for would be one blast radius",
	}
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			for _, dep := range deps(t, "./internal/content/fetch", goos) {
				if why, bad := forbidden[dep]; bad {
					t.Errorf("internal/content/fetch reaches %q on GOOS=%s (%s)", dep, goos, why)
				}
			}
		})
	}
}

// TestTheContentCoreCannotMakeANetworkCall is the reason this package is
// separate from internal/content at all.
//
// Resolve is documented as "performs network READS and writes nothing" and
// Materialize as the only half that writes bytes -- but the core itself holds
// the plan, the lockfile and the fail-closed gate, and it must not be able to
// reach the network to do any of it. A resolver that could open a socket could
// turn a `gvm content list` into egress.
func TestTheContentCoreCannotMakeANetworkCall(t *testing.T) {
	forbidden := map[string]string{
		"net/http": "internal/content holds the plan, the lock and the gate; the fetcher is " +
			"internal/content/fetch and runs as a different uid (D-16)",
	}
	for _, dep := range deps(t, "./internal/content", "linux") {
		if why, bad := forbidden[dep]; bad {
			t.Errorf("internal/content reaches %q (%s)", dep, why)
		}
	}
}

// TestThisPackageDoesReachNetHTTP is the other half, so the assertion above
// cannot pass because `go list` silently returned nothing useful.
func TestThisPackageDoesReachNetHTTP(t *testing.T) {
	for _, dep := range deps(t, "./internal/content/fetch", "linux") {
		if dep == "net/http" {
			return
		}
	}
	t.Fatal("internal/content/fetch no longer reaches net/http, so the assertion that " +
		"internal/content cannot proves nothing")
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
