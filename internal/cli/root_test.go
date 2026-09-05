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
func TestUnimplementedCommandExitsNonZero(t *testing.T) {
	for _, name := range []string{"gvmd", "web", "run", "apply", "adopt", "doctor"} {
		code, _, errb := run(t, name)
		if code == 0 {
			t.Errorf("%s: exit 0, want non-zero while unimplemented", name)
		}
		if !strings.Contains(errb, "not implemented") {
			t.Errorf("%s: stderr = %q", name, errb)
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
