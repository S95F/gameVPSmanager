package jobs

import (
	"os"
	"strings"
	"testing"
)

// This package is the one place in gvm that starts a process it did not write,
// so the two things it must never do are asserted on the source rather than
// left to review.
//
// --scope: a scope runs the command in the CALLER's cgroup and unit context. It
// takes no meaningful hardening properties, inherits root's environment and
// capabilities, and dies with whoever started it. Using one would give SteamCMD
// strictly less confinement than the game it installs, which is the regression
// D-16 exists to name. There is exactly one correct spelling here, and it is
// --unit.
//
// A shell: every command this package starts is a []string handed to
// systemd-run, so a path containing a semicolon is one argv element rather than
// a second command. The first `sh -c` would make every path, every env value
// and every manifest-derived argument an injection surface, and the manifests
// come off disk where an unapproved override can sit.
var forbiddenTokens = []struct {
	token string
	why   string
}{
	{"--scope", "third-party code runs in a transient SERVICE, never a scope (D-16)"},
	{"sh -c", "every command is argv; there is no shell anywhere in this path"},
	{"bash -c", "every command is argv; there is no shell anywhere in this path"},
	{"syscall.Exec", "nothing here replaces its own process; the job layer stays alive to " +
		"stream the output and to report the exit"},
	{"exec.LookPath", "an inherited PATH deciding which program starts a unit with --uid= is " +
		"a decision nobody made; the tool paths are absolute and stat-resolved"},
	// os/exec is this package's business -- it is the D-16 seam's
	// implementation -- but it has exactly one correct use here: starting
	// systemd-run. These three start a process WITHOUT replacing the caller and
	// WITHOUT a unit, which is the same regression --scope names: a child with
	// no slice, no uid, no RuntimeMaxSec and root's capabilities.
	{"os.StartProcess", "a child with no transient unit around it (D-16); everything this " +
		"package starts goes through systemd-run"},
	{"syscall.StartProcess", "the same spawn, one layer down"},
	{"plugin.Open", "loading a shared object runs its init inside the job process, as root, " +
		"with no unit around it at all"},
}

func TestTheJobLayerNeverBuildsAScopeOrAShell(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
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
	// Only the cross-platform files and one of the two sandbox implementations
	// compile in any given build, and a walk that found neither would pass
	// silently forever.
	if scanned < 4 {
		t.Fatalf("scanned only %d source files; the walk is broken", scanned)
	}
}
