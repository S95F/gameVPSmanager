package spec

import "fmt"

// Proto is one transport. tcp/27015 and udp/27015 are DISTINCT endpoints, and a
// listener on either occupies it regardless of the address it bound to —
// 127.0.0.1:8080 and 0.0.0.0:8080 do collide (section 6.7). Keeping the two
// protocols apart in the type is what lets the occupancy map say that once.
type Proto string

const (
	ProtoTCP Proto = "tcp"
	ProtoUDP Proto = "udp"
)

// ProtoBoth is a manifest spelling, not a protocol: it flattens to both real
// transports. It is not a Proto constant because nothing downstream may ever
// hold "both" as a thing to bind, allow through a firewall, or claim.
const ProtoBoth = "both"

// ParseProtos flattens a manifest's `proto:` into the transports the port
// actually occupies.
//
// An empty value is refused rather than defaulted. Half the games here are UDP
// (Valheim 2456, PZ 16261) and half are TCP (RCON 27015); a default would be
// wrong for one of them, and the failure mode is a firewall rule for the wrong
// transport, which reads as "the server is up but nobody can join".
func ParseProtos(s string) ([]Proto, error) {
	switch s {
	case string(ProtoTCP):
		return []Proto{ProtoTCP}, nil
	case string(ProtoUDP):
		return []Proto{ProtoUDP}, nil
	case ProtoBoth:
		return []Proto{ProtoTCP, ProtoUDP}, nil
	case "":
		return nil, fmt.Errorf("proto is required (want tcp|udp|both)")
	}
	return nil, fmt.Errorf("%q is not a protocol (want tcp|udp|both)", s)
}

// PortRequirement is one port to place, flattened out of the manifest: the
// allocator's input. internal/ports declares `type Requirement =
// spec.PortRequirement` rather than a struct of its own, so the allocator and
// the schema cannot drift apart.
//
// Group is never empty here even when the manifest left it out — see
// PortRequirements.
type PortRequirement struct {
	Name    string
	Group   string
	Default int
	Protos  []Proto
	Expose  Expose
	Bind    string

	// ConfigKey and ConfigFile carry the allocation into the game's own config
	// file. They are the ONLY way a port reaches one; naming the same key in
	// config_files[].keys is a validation error (check 6 of section 8.6).
	ConfigKey  string
	ConfigFile string

	Note string
}

// PortRequirements flattens spec.ports into the allocator's input, in manifest
// order.
//
// A port that names no group forms a group of its own, named after the port. A
// group shifts as a unit, so lumping every ungrouped port into one anonymous
// group would force unrelated ports to move together, and the first collision on
// any of them would move all of them; a solo group is the honest reading of "no
// contiguity requirement stated".
func (g GameSpec) PortRequirements() ([]PortRequirement, error) {
	out := make([]PortRequirement, 0, len(g.Ports))
	for _, p := range g.Ports {
		protos, err := ParseProtos(p.Proto)
		if err != nil {
			return nil, fmt.Errorf("ports.%s: %w", p.Name, err)
		}
		group := p.Group
		if group == "" {
			group = p.Name
		}
		out = append(out, PortRequirement{
			Name:       p.Name,
			Group:      group,
			Default:    p.Default,
			Protos:     protos,
			Expose:     p.Expose,
			Bind:       p.Bind,
			ConfigKey:  p.ConfigKey,
			ConfigFile: p.ConfigFile,
			Note:       p.Note,
		})
	}
	return out, nil
}

// Stride is how far a group moves per shift. Absent or non-positive means 1,
// which is what the allocator assumes, so the two cannot disagree about a group
// nobody declared a stride for.
func (g GameSpec) Stride(group string) int {
	if n, ok := g.PortStride[group]; ok && n > 0 {
		return n
	}
	return 1
}

// PortByName finds a declared port. Readiness probes, shutdown steps and
// templates reference ports by NAME, never by number, so this is the lookup
// every one of those checks makes.
func (g GameSpec) PortByName(name string) (PortSpec, bool) {
	for _, p := range g.Ports {
		if p.Name == name {
			return p, true
		}
	}
	return PortSpec{}, false
}

// GroupOrder returns each group once, in first-appearance order. The allocator
// places groups in this order, so the order a manifest writes its ports in is
// the order they get placed — stable, and reproducible from the file.
func GroupOrder(reqs []PortRequirement) []string {
	seen := make(map[string]bool, len(reqs))
	var out []string
	for _, r := range reqs {
		if !seen[r.Group] {
			seen[r.Group] = true
			out = append(out, r.Group)
		}
	}
	return out
}

// MembersOf returns the requirements in one group, in manifest order.
func MembersOf(reqs []PortRequirement, group string) []PortRequirement {
	var out []PortRequirement
	for _, r := range reqs {
		if r.Group == group {
			out = append(out, r)
		}
	}
	return out
}

// ConfigKeyPorts maps a config file name to the keys ports write into it, so
// check 6 can refuse a key that a port and a config_files[].keys entry both
// claim. The empty file name means "the manifest's first config file", which is
// what config_file's default resolves to.
func (g GameSpec) ConfigKeyPorts() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, p := range g.Ports {
		if p.ConfigKey == "" {
			continue
		}
		file := p.ConfigFile
		if file == "" && len(g.Configs) > 0 {
			file = g.Configs[0].Name
		}
		if out[file] == nil {
			out[file] = map[string]string{}
		}
		out[file][p.ConfigKey] = p.Name
	}
	return out
}
