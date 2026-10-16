package doctor

import (
	"context"

	"github.com/S95F/gameVPSmanager/internal/firewall"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/sshport"
)

// fwNotChecked is the one message every firewall check carries in this build.
//
// gvm does not manage the firewall yet and does not read the live ruleset, so a
// firewall check cannot honestly say "pass" or "fail" — it can only say it did
// not look. Reporting a Problem because the default `ufw` backend is "inactive"
// in a stub was the first false alarm a new operator met on a perfectly healthy
// box (the "Why now" of this phase), and no firewall check carries a Fix that
// tells an operator to enable a firewall gvm will not touch (U10). Phase 33
// reintroduces the real checks once it reads the live ruleset.
const fwNotChecked = "not checked: gvm does not manage the firewall in this build"

// FirewallChecks returns FW-* checks. Every one is a Skip in this build.
//
// The signature keeps the live-ruleset arguments the caller already gathers so
// that Phase 33, which will actually read them, changes only this file. They
// are deliberately unused here.
func FirewallChecks(
	_ *spec.HostConfig,
	_ []spec.Instance,
	_ map[string]*spec.Manifest,
	_ firewall.RuleSet,
	_ bool, _ bool,
	_ sshport.Detection,
) []lifecycle.DoctorCheck {
	ids := []struct{ id, summary string }{
		{"FW-001", "firewall backend is active"},
		{"FW-002", "SSH ports are detected and admitted by the firewall"},
		{"FW-003", "ufw status numbered fully parsed"},
		{"FW-005", "no ufw rule exists for any port declared expose:none"},
		{"FW-006", "all gvm-managed ports carry a gvm: comment tag"},
	}
	out := make([]lifecycle.DoctorCheck, 0, len(ids))
	for _, c := range ids {
		id := c.id
		out = append(out, lifecycle.DoctorCheck{
			ID:      id,
			Summary: c.summary,
			Run: func(context.Context, lifecycle.CheckEnv) lifecycle.CheckFinding {
				return lifecycle.CheckFinding{ID: id, Severity: lifecycle.CheckSkip, Message: fwNotChecked}
			},
		})
	}
	return out
}
