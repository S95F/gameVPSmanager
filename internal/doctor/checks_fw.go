package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/firewall"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/sshport"
)

// FirewallChecks returns FW-* checks.
func FirewallChecks(host *spec.HostConfig, instances []spec.Instance, manifests map[string]*spec.Manifest, liveRules firewall.RuleSet, liveActive bool, liveTrusted bool, det sshport.Detection) []lifecycle.DoctorCheck {
	return []lifecycle.DoctorCheck{
		{
			ID:      "FW-001",
			Summary: "firewall backend is active",
			Run:     checkFWActive(host, liveActive),
		},
		{
			ID:      "FW-002",
			Summary: "SSH ports are detected and admitted by the firewall",
			Run:     checkSSHAdmitted(det, liveRules, liveActive),
		},
		{
			ID:      "FW-003",
			Summary: "ufw status numbered fully parsed",
			Run:     checkFWFullyParsed(liveRules, liveTrusted),
		},
		{
			ID:      "FW-005",
			Summary: "no ufw rule exists for any port declared expose:none",
			Run:     checkNoRuleForExposeNone(host, instances, manifests, liveRules),
		},
		{
			ID:      "FW-006",
			Summary: "all gvm-managed ports carry a gvm: comment tag",
			Run:     checkGVMPortsTagged(host, instances, manifests, liveRules),
		},
	}
}

func checkFWActive(host *spec.HostConfig, active bool) func(context.Context, lifecycle.CheckEnv) lifecycle.CheckFinding {
	return func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
		backend := host.Spec.Firewall.Backend
		if backend == "none" {
			return lifecycle.CheckFinding{ID: "FW-001", Severity: lifecycle.CheckNote, Message: "firewall backend: none (unmanaged)"}
		}
		if !active {
			return lifecycle.CheckFinding{
				ID:       "FW-001",
				Severity: lifecycle.CheckProblem,
				Message:  fmt.Sprintf("firewall backend %s is not active", backend),
				Fix:      "gvm does not manage the firewall in this build",
			}
		}
		return lifecycle.CheckFinding{ID: "FW-001", Severity: lifecycle.CheckOK, Message: fmt.Sprintf("backend %s, active", backend)}
	}
}

func checkSSHAdmitted(det sshport.Detection, live firewall.RuleSet, active bool) func(context.Context, lifecycle.CheckEnv) lifecycle.CheckFinding {
	return func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
		if !active {
			return lifecycle.CheckFinding{ID: "FW-002", Severity: lifecycle.CheckSkip, Message: "firewall not active"}
		}
		if len(det.Ports) == 0 {
			return lifecycle.CheckFinding{
				ID:       "FW-002",
				Severity: lifecycle.CheckProblem,
				Message:  "could not detect any SSH port",
				Fix:      "set spec.firewall.ssh.ports explicitly",
			}
		}
		g := firewall.Guard{SSHPorts: det.Ports, LiveSessionPort: det.LiveSessionPort}
		if err := g.Check(live, true); err != nil {
			return lifecycle.CheckFinding{
				ID:       "FW-002",
				Severity: lifecycle.CheckProblem,
				Message:  err.Error(),
				Fix:      "gvm does not manage the firewall in this build",
			}
		}
		sources := make([]string, 0, len(det.Findings))
		for _, f := range det.Findings {
			sources = append(sources, string(f.Source))
		}
		return lifecycle.CheckFinding{
			ID:       "FW-002",
			Severity: lifecycle.CheckOK,
			Message:  fmt.Sprintf("ssh detected on %v (%s); admitted by firewall", det.Ports, strings.Join(sources, ", ")),
		}
	}
}

func checkFWFullyParsed(live firewall.RuleSet, trusted bool) func(context.Context, lifecycle.CheckEnv) lifecycle.CheckFinding {
	return func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
		if !trusted {
			return lifecycle.CheckFinding{
				ID:       "FW-003",
				Severity: lifecycle.CheckProblem,
				Message:  "ufw status numbered could not be fully parsed; deletions suppressed (D-42)",
				Fix:      "inspect output of: sudo ufw status numbered",
			}
		}
		return lifecycle.CheckFinding{
			ID:       "FW-003",
			Severity: lifecycle.CheckOK,
			Message:  fmt.Sprintf("ufw status numbered fully parsed (%d rules)", len(live)),
		}
	}
}

func checkNoRuleForExposeNone(host *spec.HostConfig, instances []spec.Instance, manifests map[string]*spec.Manifest, live firewall.RuleSet) func(context.Context, lifecycle.CheckEnv) lifecycle.CheckFinding {
	return func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
		var violations []string
		for _, inst := range instances {
			if !inst.Spec.Enabled {
				continue
			}
			m := manifests[inst.Spec.Game]
			if m == nil {
				continue
			}
			for _, p := range m.Spec.Ports {
				eff := effectiveExposeCheck(p.Expose, inst.Spec.Firewall.Expose[p.Name])
				if eff != spec.ExposeNone {
					continue
				}
				port, ok := inst.Spec.Ports[p.Name]
				if !ok || port == 0 {
					continue
				}
				for _, r := range live {
					if r.Port == port && (r.Verb == firewall.VerbAllow || r.Verb == firewall.VerbLimit) {
						violations = append(violations, fmt.Sprintf("%s: port %d declared expose:none but rule exists", inst.Metadata.Name, port))
					}
				}
			}
		}
		if len(violations) > 0 {
			return lifecycle.CheckFinding{
				ID:       "FW-005",
				Severity: lifecycle.CheckProblem,
				Message:  strings.Join(violations, "; "),
				Fix:      "gvm does not manage the firewall in this build",
			}
		}
		return lifecycle.CheckFinding{ID: "FW-005", Severity: lifecycle.CheckOK, Message: "no ufw rule for any expose:none port"}
	}
}

func checkGVMPortsTagged(host *spec.HostConfig, instances []spec.Instance, manifests map[string]*spec.Manifest, live firewall.RuleSet) func(context.Context, lifecycle.CheckEnv) lifecycle.CheckFinding {
	return func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
		managedPorts := map[int]bool{}
		for _, inst := range instances {
			for _, port := range inst.Spec.Ports {
				managedPorts[port] = true
			}
		}
		var untagged []string
		for _, r := range live {
			if !managedPorts[r.Port] {
				continue
			}
			if _, ok := firewall.ParseTag(r.Comment); !ok {
				untagged = append(untagged, fmt.Sprintf("%d/%s", r.Port, r.Proto))
			}
		}
		if len(untagged) > 0 {
			return lifecycle.CheckFinding{
				ID:       "FW-006",
				Severity: lifecycle.CheckWarning,
				Message:  fmt.Sprintf("%d rules on gvm-managed ports carry no gvm: tag: %s", len(untagged), strings.Join(untagged, ", ")),
				Fix:      "gvm does not manage the firewall in this build",
			}
		}
		return lifecycle.CheckFinding{ID: "FW-006", Severity: lifecycle.CheckOK, Message: "all gvm-managed ports carry a gvm: tag"}
	}
}

func effectiveExposeCheck(manifest, override spec.Expose) spec.Expose {
	if override == "" {
		return manifest
	}
	return override
}
