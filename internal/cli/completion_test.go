package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCompletionScriptsNameEveryVisibleVerb is the point of generating the
// script from the table: a verb added to `commands` must appear in all three
// shells' completion without a second list being edited. It also asserts the
// hidden roles stay out.
func TestCompletionScriptsNameEveryVisibleVerb(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			var out bytes.Buffer
			env := &Env{Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")}
			if err := runCompletion(context.Background(), env, []string{shell}); err != nil {
				t.Fatalf("completion %s: %v", shell, err)
			}
			script := out.String()
			if script == "" {
				t.Fatal("empty script")
			}
			for _, c := range commands {
				named := strings.Contains(script, c.Name)
				if c.Hidden && (c.Name == "gvmd" || c.Name == "run-shutdown") && named {
					t.Errorf("hidden role %q leaked into %s completion", c.Name, shell)
				}
				if !c.Hidden && !named {
					t.Errorf("visible verb %q is missing from %s completion", c.Name, shell)
				}
			}
		})
	}
}

// TestCompletionRejectsAnUnknownShell: a mistyped shell is a usage error (exit
// 2 under the table), not a silent empty script.
func TestCompletionRejectsAnUnknownShell(t *testing.T) {
	var out bytes.Buffer
	env := &Env{Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")}
	err := runCompletion(context.Background(), env, []string{"powershell"})
	if err == nil {
		t.Fatal("accepted an unknown shell")
	}
	var ce *codedError
	if !errors.As(err, &ce) || ce.code != exitUsage {
		t.Errorf("err = %v, want a usage error (exit %d)", err, exitUsage)
	}
}

// TestCompletionZshDescriptionsAreEscaped guards the one way the generated zsh
// is syntactically fragile: an unescaped ':' in a summary would split the
// `name:desc` pair and silently truncate the description.
func TestCompletionZshDescriptionsAreEscaped(t *testing.T) {
	got := zshEscape("does a: thing with 'quotes'")
	if strings.Contains(strings.ReplaceAll(got, `\:`, ""), ":") {
		t.Errorf("a colon was left unescaped: %q", got)
	}
	if !strings.Contains(got, `'\''`) {
		t.Errorf("a single quote was not escaped as '\\'': %q", got)
	}
}
