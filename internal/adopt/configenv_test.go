package adopt

import (
	"strings"
	"testing"
)

func TestAdoptConfigEnvNeverExecutes(t *testing.T) {
	input := `
# comment
PZ_USER=pzserver
INSTALL_DIR=/opt/pzserver
QUOTED="value with spaces"
SINGLE='single quoted'
DANGEROUS=$(rm -rf /)
BACKTICK=` + "`echo hacked`" + `
INTERP=${DANGEROUS}
DOLLAR=$PATH
TRAILING=value  # inline comment
`
	env := ParseConfigEnv([]byte(input))

	// Basic parsing.
	if env["PZ_USER"] != "pzserver" {
		t.Errorf("PZ_USER = %q, want %q", env["PZ_USER"], "pzserver")
	}
	if env["INSTALL_DIR"] != "/opt/pzserver" {
		t.Errorf("INSTALL_DIR = %q", env["INSTALL_DIR"])
	}
	if env["QUOTED"] != "value with spaces" {
		t.Errorf("QUOTED = %q", env["QUOTED"])
	}
	if env["SINGLE"] != "single quoted" {
		t.Errorf("SINGLE = %q", env["SINGLE"])
	}

	// Inline comment stripped for bare values.
	if env["TRAILING"] != "value" {
		t.Errorf("TRAILING = %q, want %q", env["TRAILING"], "value")
	}

	// Security invariant: shell metacharacters are literal strings, never executed.
	if !strings.Contains(env["DANGEROUS"], "$(rm -rf /)") {
		t.Errorf("DANGEROUS: shell substitution must be literal, got %q", env["DANGEROUS"])
	}
	if !strings.Contains(env["BACKTICK"], "echo hacked") {
		t.Errorf("BACKTICK: backtick must be literal, got %q", env["BACKTICK"])
	}
	if !strings.Contains(env["INTERP"], "${DANGEROUS}") {
		t.Errorf("INTERP: brace expansion must be literal, got %q", env["INTERP"])
	}
	if !strings.Contains(env["DOLLAR"], "$PATH") {
		t.Errorf("DOLLAR: dollar-var must be literal, got %q", env["DOLLAR"])
	}
}
