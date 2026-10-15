package cli

import (
	"strings"
	"testing"
)

// TestEveryHelpPathExitsZero is usability principle U7: help never acts.
//
// For every operator command and subcommand, `<cmd> --help`, `<cmd> -h` and
// `gvm help <cmd> [sub]` must exit 0, print a usage line, and touch no host —
// the panic the plan's "Why now" describes was a help request that fell through
// into the command body with a zero-valued options struct.
func TestEveryHelpPathExitsZero(t *testing.T) {
	subs := map[string][]string{
		"game":    {"validate", "list", "which", "hooks"},
		"content": {"list", "diff", "why", "add", "remove", "move", "sync", "update", "lock", "blocked"},
		"job":     {"run", "list", "clear"},
		"backup":  {"verify", "prune", "list"},
		"control": {"caps", "status", "verb", "probe"},
		"user":    {"list", "create", "show", "set-role", "set-password", "disable", "enable", "reset-totp"},
		"token":   {"create", "list", "revoke"},
		"secret":  {"list", "set", "rotate", "show-path"},
		"audit":   {"tail", "search"},
		"db":      {"migrate", "status", "version", "replay-spool", "check-grants"},
		"adopt":   {"discover", "cutover", "rollback", "finalize"},
	}

	check := func(t *testing.T, args ...string) {
		t.Helper()
		h := newTestHost(t)
		code, out, errb := runWith(t, h, "", args...)
		combined := out + errb
		if code != 0 {
			t.Errorf("`gvm %s` exited %d, want 0:\n%s", strings.Join(args, " "), code, combined)
		}
		// `version` and `db version` have no options, so their help is the value
		// they print, not a usage line.
		trivial := false
		for _, a := range args {
			if a == "version" {
				trivial = true
			}
		}
		if !trivial && !strings.Contains(strings.ToLower(combined), "usage") {
			t.Errorf("`gvm %s` printed no usage line:\n%s", strings.Join(args, " "), combined)
		}
		if n := len(h.FS.writes); n != 0 {
			t.Errorf("`gvm %s` wrote %d file(s); help must touch no host: %v",
				strings.Join(args, " "), n, h.FS.writes)
		}
	}

	for _, c := range commands {
		if c.Hidden {
			continue // roles and aliases: invoked by systemd, not typed for help
		}
		check(t, c.Name, "--help")
		check(t, c.Name, "-h")
		check(t, "help", c.Name)
		for _, s := range subs[c.Name] {
			check(t, c.Name, s, "--help")
			check(t, c.Name, s, "-h")
			check(t, "help", c.Name, s)
		}
	}
}
