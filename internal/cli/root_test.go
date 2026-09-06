package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := &Env{
		Args: args, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Getenv:  func(string) string { return "" },
		Geteuid: func() int { return 1000 },
	}
	return Main(context.Background(), env), out.String(), errb.String()
}

func TestVersionSucceeds(t *testing.T) {
	code, out, _ := run(t, "version")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.HasPrefix(out, "gvm ") {
		t.Errorf("version output = %q", out)
	}
}

// An unbuilt phase must never look like success to a shell script.
//
// `run` and `console` left this list when Phase 4 built them, and `apply` left
// it when Phase 7 did. Moving a name out as it is implemented is the point:
// weakening the assertion to "non-zero" would keep passing for a command that
// had grown a usage error instead of a working body.
func TestUnimplementedCommandExitsNonZero(t *testing.T) {
	for _, name := range []string{"gvmd", "web", "content", "backup", "restore",
		"update", "rebuild", "adopt", "firewall", "doctor"} {
		code, _, errb := run(t, name)
		if code == 0 {
			t.Errorf("%s: exit 0, want non-zero while unimplemented", name)
		}
		if !strings.Contains(errb, "not implemented") {
			t.Errorf("%s: stderr = %q", name, errb)
		}
	}
}

// The other half of the same assertion: a verb this phase built must no longer
// answer with the unbuilt sentinel, however it fails.
func TestImplementedCommandsAreNotStubs(t *testing.T) {
	for _, args := range [][]string{
		{"run"}, {"run-prepare"}, {"run-shutdown"}, {"console"}, {"control"}, {"game"},
		{"create"}, {"install"}, {"apply"}, {"start"}, {"stop"}, {"restart"},
		{"status"}, {"list"}, {"logs"}, {"ports"}, {"destroy"},
	} {
		_, _, errb := run(t, args...)
		if strings.Contains(errb, "not implemented") {
			t.Errorf("%v: still reports as unimplemented: %q", args, errb)
		}
	}
}

// The hidden roles the unit and gvmd invoke must be dispatchable by their exact
// spelling: an ExecStartPre= that typoes one fails a unit start with "unknown
// command", which is a boot failure nobody reads as a packaging bug.
func TestRoleVerbsAreDispatchable(t *testing.T) {
	for _, name := range []string{"run", "run-prepare", "run-shutdown"} {
		c, ok := lookup(name)
		if !ok {
			t.Fatalf("%s is not dispatchable", name)
		}
		if !c.Hidden {
			t.Errorf("%s is listed in usage; it is invoked by systemd, not typed", name)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, errb := run(t, "frobnicate")
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errb, "unknown command") {
		t.Errorf("stderr = %q", errb)
	}
}

func TestNoArgsShowsUsageOnStderr(t *testing.T) {
	code, out, errb := run(t)
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if out != "" {
		t.Errorf("usage went to stdout on error: %q", out)
	}
	if !strings.Contains(errb, "Usage:") {
		t.Errorf("stderr = %q", errb)
	}
}

func TestHelpGoesToStdout(t *testing.T) {
	code, out, _ := run(t, "--help")
	if code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("stdout = %q", out)
	}
}

// Hidden roles are dispatchable but must not clutter the operator's usage.
func TestHiddenRolesAreNotListed(t *testing.T) {
	_, out, _ := run(t, "--help")
	for _, hidden := range []string{"gvmd", "web"} {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), hidden+" ") {
				t.Errorf("hidden role %q appears in usage: %q", hidden, line)
			}
		}
	}
	if _, ok := lookup("gvmd"); !ok {
		t.Error("hidden role gvmd is not dispatchable")
	}
}

func TestCommandTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	groups := map[string]bool{}
	for _, g := range groupOrder {
		groups[g.key] = true
	}
	for _, c := range commands {
		if seen[c.Name] {
			t.Errorf("duplicate command %q", c.Name)
		}
		seen[c.Name] = true
		if c.Run == nil {
			t.Errorf("%s: nil Run", c.Name)
		}
		if c.Summary == "" {
			t.Errorf("%s: empty summary", c.Name)
		}
		if c.Group == "roles" {
			continue // roles are hidden and have no usage heading
		}
		if !groups[c.Group] {
			t.Errorf("%s: group %q has no heading in groupOrder", c.Name, c.Group)
		}
	}
}
