package reconcile

import (
	"strings"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// buildEgressDirectives converts an instance's egress policy into the systemd
// IPAddressDeny= / IPAddressAllow= lines written into the drop-in.
//
// The egress knob works via the kernel's cgroup-BPF address filter, which
// systemd wires up through these two directives. The allowlist policy emits
// IPAddressDeny=any followed by one IPAddressAllow= per CIDR in
// spec.egress.allow. The open policy (the default) emits nothing: the kernel
// default is allow-all, and a directive that says so is a second thing to keep
// in step with the default.
//
// CIDRs must be IPv4 or IPv6 CIDRs (e.g. "1.2.3.0/24", "::/0"). Hostnames
// are resolved ONCE at unit start and cached by systemd, so a backend that
// rotates its IP between restarts would silently break; the spec validator
// (section 18.4) refuses hostnames here. This function trusts the validator
// and writes the values as-is.
func buildEgressDirectives(p spec.EgressPolicy) []string {
	if p.Policy != spec.EgressAllowlist {
		return nil
	}
	lines := make([]string, 0, 1+len(p.Allow))
	lines = append(lines, "IPAddressDeny=any")
	for _, cidr := range p.Allow {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		lines = append(lines, "IPAddressAllow="+cidr)
	}
	return lines
}
