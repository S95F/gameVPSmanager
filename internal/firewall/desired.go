package firewall

import (
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Desired computes the desired firewall rule set from the host and instance
// configurations. It is pure: no I/O, no database, no network.
//
// SSH rules are first (so an interrupted apply cannot land between "deny incoming"
// and "allow ssh"), then edge rules, then instance ports.
func Desired(host *spec.HostConfig, instances []spec.Instance, manifests map[string]*spec.Manifest) RuleSet {
	var rules RuleSet

	fw := host.Spec.Firewall
	ssh := fw.SSH

	// 1. SSH rules — always first, always allow/limit TCP.
	for _, port := range sshPorts(ssh) {
		verb := VerbAllow
		if ssh.RateLimit {
			verb = VerbLimit
		}
		rules = append(rules, Rule{
			Verb:    verb,
			Proto:   ProtoTCP,
			Port:    port,
			Comment: Tag{Instance: "host", Label: "ssh"}.String(),
		})
	}

	// 2. Edge rules — only when Caddy manages the edge.
	mode := host.Spec.Edge.Mode
	if mode == "caddy_managed" || mode == "caddy_snippet" {
		rules = append(rules,
			Rule{Verb: VerbAllow, Proto: ProtoTCP, Port: 80, Comment: Tag{Instance: "host", Label: "http"}.String()},
			Rule{Verb: VerbAllow, Proto: ProtoTCP, Port: 443, Comment: Tag{Instance: "host", Label: "https"}.String()},
		)
		if host.Spec.Edge.HTTP3 {
			// HTTP/3 uses QUIC on UDP 443 — do NOT open if http3: false.
			rules = append(rules,
				Rule{Verb: VerbAllow, Proto: ProtoUDP, Port: 443, Comment: Tag{Instance: "host", Label: "https-alt"}.String()},
			)
		}
	}

	// 3. Instance port rules — only for enabled instances.
	for _, inst := range instances {
		if !inst.Spec.Enabled {
			// An inactive instance gets no firewall rules: enabled:false closes the door.
			continue
		}
		m := manifests[inst.Spec.Game]
		if m == nil {
			continue
		}
		instanceExpose := inst.Spec.Firewall.Expose
		for _, p := range m.Spec.Ports {
			eff := effectiveExpose(p.Expose, instanceExpose[p.Name])
			if eff == spec.ExposeNone {
				continue
			}

			allocatedPort, ok := inst.Spec.Ports[p.Name]
			if !ok || allocatedPort == 0 {
				continue
			}

			rule := Rule{
				Proto:   protoFrom(p.Proto),
				Port:    allocatedPort,
				Comment: Tag{Instance: inst.Metadata.Name, Label: slugLabel(p.Name)}.String(),
			}

			switch eff {
			case spec.ExposePublic:
				rule.Verb = VerbAllow
				rules = append(rules, rule)
			case spec.ExposeAdmin:
				// One rule per admin CIDR; no rule at all if none configured.
				for _, cidr := range adminCIDRs(fw, inst.Spec.Firewall) {
					r := rule
					r.Verb = VerbAllow
					r.From = cidr
					rules = append(rules, r)
				}
			}
		}
	}

	return rules
}

func sshPorts(ssh spec.SSHSpec) []int {
	if len(ssh.Ports) > 0 {
		return ssh.Ports
	}
	return []int{22}
}

func protoFrom(p string) Proto {
	switch p {
	case "tcp":
		return ProtoTCP
	case "udp":
		return ProtoUDP
	default:
		return ProtoBoth
	}
}

func slugLabel(name string) string {
	if len(name) > 32 {
		return name[:32]
	}
	return name
}

func effectiveExpose(manifest, override spec.Expose) spec.Expose {
	if override == "" {
		return manifest
	}
	return override
}

func adminCIDRs(fw spec.FirewallSpec, ifw spec.InstanceFirewall) []string {
	seen := map[string]bool{}
	for _, c := range fw.AdminCIDRs {
		seen[c] = true
	}
	for _, c := range ifw.AdminCIDRs {
		seen[c] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	return out
}
