package firewall_test

import (
	"errors"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/firewall"
)

// TestLockoutTable is the DoD requirement: every row is a way to lock yourself out.
// Each must return ErrWouldLockOut, and there is no flag that bypasses it.
func TestLockoutTable(t *testing.T) {
	sshAllowRule := firewall.Rule{Verb: firewall.VerbAllow, Proto: firewall.ProtoTCP, Port: 22}
	sshLimitRule := firewall.Rule{Verb: firewall.VerbLimit, Proto: firewall.ProtoTCP, Port: 22}

	cases := []struct {
		name    string
		guard   firewall.Guard
		desired firewall.RuleSet
	}{
		{
			name:    "no ssh rule at all",
			guard:   firewall.Guard{SSHPorts: []int{22}, Confident: true},
			desired: firewall.RuleSet{firewall.Rule{Verb: firewall.VerbAllow, Proto: firewall.ProtoUDP, Port: 16261}},
		},
		{
			name:  "wrong port in desired",
			guard: firewall.Guard{SSHPorts: []int{22}, Confident: true},
			desired: firewall.RuleSet{
				firewall.Rule{Verb: firewall.VerbAllow, Proto: firewall.ProtoTCP, Port: 2222},
			},
		},
		{
			name:  "deny rule for ssh port",
			guard: firewall.Guard{SSHPorts: []int{22}, Confident: true},
			desired: firewall.RuleSet{
				firewall.Rule{Verb: firewall.VerbDeny, Proto: firewall.ProtoTCP, Port: 22},
			},
		},
		{
			name:  "live session port not admitted even if not in SSHPorts",
			guard: firewall.Guard{SSHPorts: []int{22}, LiveSessionPort: 2222, Confident: true},
			desired: firewall.RuleSet{
				sshAllowRule,
				// 2222 is the live session port but there's no rule for it.
			},
		},
		{
			name:  "ssh port with udp rule only",
			guard: firewall.Guard{SSHPorts: []int{22}, Confident: true},
			desired: firewall.RuleSet{
				firewall.Rule{Verb: firewall.VerbAllow, Proto: firewall.ProtoUDP, Port: 22},
			},
		},
		{
			name:  "multiple ssh ports, one missing",
			guard: firewall.Guard{SSHPorts: []int{22, 2222}, Confident: true},
			desired: firewall.RuleSet{
				sshAllowRule, // only 22, not 2222
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.guard.Check(c.desired, true)
			if !errors.Is(err, firewall.ErrWouldLockOut) {
				t.Errorf("Check() = %v, want ErrWouldLockOut", err)
			}
		})
	}

	// Sanity: a correct rule set does NOT trigger the error.
	t.Run("correct set does not lock out", func(t *testing.T) {
		g := firewall.Guard{SSHPorts: []int{22}, Confident: true}
		desired := firewall.RuleSet{sshAllowRule}
		if err := g.Check(desired, true); err != nil {
			t.Errorf("Check() = %v, want nil", err)
		}
	})
	t.Run("limit also admits", func(t *testing.T) {
		g := firewall.Guard{SSHPorts: []int{22}, Confident: true}
		desired := firewall.RuleSet{sshLimitRule}
		if err := g.Check(desired, true); err != nil {
			t.Errorf("Check() = %v, want nil", err)
		}
	})
	t.Run("allow-by-default never locks out", func(t *testing.T) {
		g := firewall.Guard{SSHPorts: []int{22}, Confident: true}
		// defaultIncomingDeny=false: an allow-by-default policy can't lock you out.
		if err := g.Check(firewall.RuleSet{}, false); err != nil {
			t.Errorf("Check() = %v, want nil (allow-by-default)", err)
		}
	})
}
