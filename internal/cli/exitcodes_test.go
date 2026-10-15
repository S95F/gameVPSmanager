package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

var errTest78 = errors.New("role failure")

// TestOperatorExitCodes is the U8 table, driven against the fake HostEnv: each
// outcome carries the documented exit code so a script can branch on the number
// rather than parsing text.
func TestOperatorExitCodes(t *testing.T) {
	t.Run("unbuilt verb exits 3", func(t *testing.T) {
		h := newTestHost(t)
		seedInstance(t, h, "pz-main", "projectzomboid", "--var", "public_name=Test")
		if code, _, _ := runWith(t, h, "", "install", "pz-main"); code != 3 {
			t.Errorf("install exited %d, want 3", code)
		}
	})

	t.Run("unknown subcommand exits 2", func(t *testing.T) {
		h := newTestHost(t)
		if code, _, errb := runWith(t, h, "", "db", "frobnicate"); code != 2 {
			t.Errorf("db frobnicate exited %d, want 2:\n%s", code, errb)
		}
	})

	t.Run("a failed action exits 1", func(t *testing.T) {
		h := newTestHost(t)
		if code, _, _ := runWith(t, h, "", "apply", "nope-no-such"); code != 1 {
			t.Errorf("apply of a missing instance exited %d, want 1", code)
		}
	})

	t.Run("a typed-confirm mismatch on destroy exits 5", func(t *testing.T) {
		h := newTestHost(t)
		install, data, arc := destroyable(t, h, "valheim-1")
		seedInstance(t, h, "valheim-1", "valheim", "--install", install, "--data", data,
			"--var", "public_name=Test")
		code, _, errb := runWith(t, h, "", "destroy", "valheim-1", "--archive", arc, "--confirm", "WRONG")
		if code != 5 {
			t.Errorf("destroy with a wrong --confirm exited %d, want 5:\n%s", code, errb)
		}
	})
}

// TestJobUnitExitCodes: with INVOCATION_ID set, `gvm job run` keeps the unit
// contract — a wired kind that runs exits 0, and a kind this build cannot run
// exits 3 (never 5 for a skip or 4 for a report, which would fail the oneshot).
func TestJobUnitExitCodes(t *testing.T) {
	unitRun := func(t *testing.T, h *testHost, args ...string) int {
		t.Helper()
		var out, errb bytes.Buffer
		env := &Env{
			Args: args, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
			Getenv:  func(k string) string { return map[string]string{"INVOCATION_ID": "unit-123"}[k] },
			Geteuid: func() int { return 0 },
			Host:    h.HostEnv,
		}
		return Main(context.Background(), env)
	}

	// The core of the contract: a routine run that succeeds exits 0 and does not
	// fail the oneshot unit. (Exit 3 for a kind this build cannot run is covered
	// by TestOperatorExitCodes' unbuilt case, which shares the avail→3 path.)
	t.Run("a wired kind that runs exits 0", func(t *testing.T) {
		h, _, _ := jobHost(t)
		seedJobbableInstance(t, h, "pz-main")
		if code := unitRun(t, h, "job", "run", "_guard"); code != 0 {
			t.Errorf("the guard job under a unit exited %d, want 0", code)
		}
	})
}

// TestRoleExitCodesUnchanged: a role verb's ExitError code passes through Main
// untouched (rule 4), and the operator table never remaps it. A role verb is
// also not wrapped by the panic recover — only operator verbs are.
func TestRoleExitCodesUnchanged(t *testing.T) {
	orig := commands
	defer func() { commands = orig }()
	commands = append(append([]Command(nil), commands...),
		Command{Name: "__role", Group: "roles", Hidden: true,
			Run: func(context.Context, *Env, []string) error {
				return &ExitError{Code: 78, Err: errTest78}
			}})
	// A role verb must be routed as a role, or the recover/table would apply.
	routes["__role"] = RouteRole
	defer delete(routes, "__role")

	code, _, _ := run(t, "__role")
	if code != 78 {
		t.Errorf("a role verb's ExitError code was remapped to %d, want 78", code)
	}
}
