package cli

import (
	"testing"

	"github.com/S95F/gameVPSmanager/internal/avail"
)

// TestAvailabilityMatchesBehaviour is usability principle U1: the one
// availability registry and the behaviour never disagree.
//
// For every command, it drives Main with the fake HostEnv and checks the exit
// code against the registry: exit 3 is the "not available in this build" code
// (Main maps avail.ErrUnavailable to it and to nothing else), so a NotInBuild
// row MUST exit 3 and a Works or Partial row must NOT. It fails both ways — when
// a Works row starts refusing and when a NotInBuild row starts working — so a
// phase that finishes a journey cannot flip a row without changing the
// behaviour in the same commit, and cannot change the behaviour without flipping
// the row.
//
// NotInBuild commands are invoked in the form that reaches their refusal (an
// instance name for install/update/rebuild, a subcommand for the admin groups);
// Works and Partial commands default to a bare invocation, whose arg or usage
// error (exit 1 or 2) is enough to prove the row does not answer with avail.
func TestAvailabilityMatchesBehaviour(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "pz-main", "projectzomboid", "--var", "public_name=Test")

	// The invocation that reaches a NotInBuild command's refusal. A bare
	// `gvm install` would stop at "name an instance" (exit 2), never reaching
	// the provider refusal, so the registry is exercised against the real path.
	reach := map[string][]string{
		"install":  {"install", "pz-main"},
		"update":   {"update", "pz-main"},
		"rebuild":  {"rebuild", "pz-main"},
		"firewall": {"firewall"},
		"user":     {"user", "list"},
		"token":    {"token", "list"},
		"secret":   {"secret", "list"},
		"audit":    {"audit", "tail"},
		"gvmd":     {"gvmd"},
		"web":      {"web"},
	}

	// The systemd roles have their own exit contract (ExitError, section 7.8),
	// not the operator-verb contract this test reads, so they are out of scope.
	skip := map[string]bool{
		"run": true, "run-prepare": true, "run-shutdown": true, "probe-write": true,
	}

	const exitUnavailable = 3
	for _, c := range commands {
		if skip[c.Name] {
			continue
		}
		args, ok := reach[c.Name]
		if !ok {
			args = []string{c.Name}
		}
		code, _, errb := runWith(t, h, "", args...)
		switch c.Avail.State {
		case avail.NotInBuild:
			if code != exitUnavailable {
				t.Errorf("%s is NotInBuild in the registry but `gvm %v` exited %d, not %d "+
					"(not available): the row and the behaviour disagree.\n%s",
					c.Name, args, code, exitUnavailable, errb)
			}
		default: // Works or Partial
			if code == exitUnavailable {
				t.Errorf("%s is %v in the registry but `gvm %v` exited %d (not available): the "+
					"row says it works and the behaviour refuses.\n%s",
					c.Name, c.Avail.State, args, code, errb)
			}
		}
	}
}

// TestAvailabilityRegistryIsWellFormed: a NotInBuild or Partial row must carry a
// reason, and a Works row must not — the reason is what help, the dashboard and
// every refusal print, and a blank one is a row that explains nothing.
func TestAvailabilityRegistryIsWellFormed(t *testing.T) {
	for _, c := range commands {
		switch c.Avail.State {
		case avail.Works:
			if c.Avail.Why != "" {
				t.Errorf("%s is Works but carries a reason %q; a reason implies a limit", c.Name, c.Avail.Why)
			}
		case avail.Partial, avail.NotInBuild:
			if c.Avail.Why == "" {
				t.Errorf("%s is %v with no reason; the reason is what every refusal prints", c.Name, c.Avail.State)
			}
		}
	}
}
