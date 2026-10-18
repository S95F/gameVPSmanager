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

// TestParseConfigEnvQuotesAndComments pins the quote-aware rules (migration-
// adopt-12): a quoted value runs to its closing quote and drops a trailing
// comment, and a bare value's comment begins only at a `#` after whitespace.
func TestParseConfigEnvQuotesAndComments(t *testing.T) {
	cases := []struct{ key, line, want string }{
		{"SERVER_PASSWORD", `SERVER_PASSWORD="" # none`, ""},
		{"HASHINQUOTE", `HASHINQUOTE="a # b"`, "a # b"},
		{"URLFRAG", `URLFRAG=http://x/y#z`, "http://x/y#z"},
		{"BARECOMMENT", `BARECOMMENT=value # trailing`, "value"},
		{"SINGLE", `SINGLE='a # b'`, "a # b"},
		{"EMPTY", `EMPTY=`, ""},
	}
	for _, tc := range cases {
		got := ParseConfigEnv([]byte(tc.line))
		if got[tc.key] != tc.want {
			t.Errorf("%s: parsed %q, want %q", tc.key, got[tc.key], tc.want)
		}
	}
}

// TestUnsafePassword: a password gvm cannot carry unaltered is flagged, a plain
// one is not.
func TestUnsafePassword(t *testing.T) {
	for _, bad := range []string{`a'b`, `a"b`, `a\b`, `a$b`, "a`b"} {
		if !UnsafePassword(bad) {
			t.Errorf("%q should be flagged as unsafe to carry", bad)
		}
	}
	for _, ok := range []string{"", "plainpass", "P4ss-word_123", "with spaces"} {
		if UnsafePassword(ok) {
			t.Errorf("%q should be safe", ok)
		}
	}
}
