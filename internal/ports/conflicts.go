package ports

import (
	"fmt"
	"sort"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// ConflictKind classifies what is wrong with a materialised port.
//
// The kinds are separate because the fix is different for each, and `gvm
// doctor` prints the fix. "Two instances declare udp/16261" is resolved by
// moving one of them; "this port is inside the kernel ephemeral range" is
// resolved by moving it or by narrowing ip_local_port_range; "the manifest no
// longer declares this port" is resolved by re-running create.
type ConflictKind string

const (
	// ConflictOverlap is two owners on one endpoint. This is D-24 in practice:
	// the schema deliberately has no cross-instance unique constraint on port
	// allocations, because a database that refuses to record reality does not
	// make the collision go away, it makes it unreportable. The collision is a
	// finding, not a write error.
	ConflictOverlap ConflictKind = "overlap"

	// ConflictEphemeral is a port inside the kernel's outbound range. The
	// server binds successfully today and fails on the day an outbound
	// connection got there first.
	ConflictEphemeral ConflictKind = "ephemeral"

	// ConflictPrivileged is a port below 1024 that the manifest did not ask
	// for. The unit starts and then cannot bind.
	ConflictPrivileged ConflictKind = "privileged"

	// ConflictRange is a port number that is not a port number.
	ConflictRange ConflictKind = "range"

	// ConflictMissing is a declared port with nothing materialised for it,
	// which is what a manifest revision that added a port looks like from the
	// old instance YAML's side.
	ConflictMissing ConflictKind = "missing"

	// ConflictUnknown is a materialised port the manifest no longer declares.
	// Harmless to the server and important to the firewall, which would
	// otherwise keep opening a hole for a port nothing listens on.
	ConflictUnknown ConflictKind = "unknown"
)

// Conflict is one finding about one endpoint.
type Conflict struct {
	Kind     ConflictKind
	Endpoint Endpoint
	Instance string // whose allocation this is; empty for a host-wide finding
	Name     string // the port's name in the manifest, when known
	Claims   []Claim
	Detail   string
}

func (c Conflict) String() string {
	var b strings.Builder
	b.WriteString(string(c.Kind))
	b.WriteString(": ")
	b.WriteString(c.Endpoint.String())
	if c.Instance != "" || c.Name != "" {
		b.WriteString(" (")
		if c.Instance != "" {
			b.WriteString(c.Instance)
			if c.Name != "" {
				b.WriteString(":")
			}
		}
		b.WriteString(c.Name)
		b.WriteString(")")
	}
	if c.Detail != "" {
		b.WriteString(" ")
		b.WriteString(c.Detail)
	}
	for _, cl := range c.Claims {
		b.WriteString("\n    ")
		b.WriteString(cl.String())
	}
	return b.String()
}

func (c Conflict) less(o Conflict) bool {
	if c.Endpoint != o.Endpoint {
		return c.Endpoint.less(o.Endpoint)
	}
	if c.Instance != o.Instance {
		return c.Instance < o.Instance
	}
	if c.Name != o.Name {
		return c.Name < o.Name
	}
	return c.Kind < o.Kind
}

// SortConflicts orders findings in place, so two runs over the same host
// produce the same report and a diff of two reports means something.
func SortConflicts(cs []Conflict) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].less(cs[j]) })
}

// Conflicts reports every endpoint that two owners both hold.
//
// Only owned claims count. An unattributed listener sharing an endpoint with an
// instance is overwhelmingly that instance's own running server, and reporting
// it would put a permanent finding on every healthy host -- which is how a
// non-zero problem count trains an operator to stop reading the report. Run
// AttributeListeners first if the pid-to-instance mapping is available; a
// listener with a *different* owner then does show up here, which is the case
// that matters: something else is already on the port this instance is about to
// be started on.
//
// A reserved port that an instance also declares is reported, because the
// operator wrote that list precisely to be told.
func Conflicts(o *Occupancy) []Conflict {
	var out []Conflict
	for _, ep := range o.Endpoints() {
		claims := o.Claims(ep)

		owners := map[string]bool{}
		var owned []Claim
		var reserved []Claim
		for _, c := range claims {
			if c.Owner != "" {
				owners[c.Owner] = true
				owned = append(owned, c)
				continue
			}
			if c.Source == SourceReserved {
				reserved = append(reserved, c)
			}
		}
		switch {
		case len(owners) > 1:
			out = append(out, Conflict{
				Kind:     ConflictOverlap,
				Endpoint: ep,
				Claims:   owned,
				Detail:   fmt.Sprintf("%d owners hold this endpoint", len(owners)),
			})
		case len(owners) == 1 && len(reserved) > 0:
			out = append(out, Conflict{
				Kind:     ConflictOverlap,
				Endpoint: ep,
				Instance: owned[0].Owner,
				Claims:   append(append([]Claim{}, owned...), reserved...),
				Detail:   "an owned port is also on the operator's reserved list",
			})
		}
		if len(owners) >= 1 && o.InEphemeral(ep.Port) {
			out = append(out, Conflict{
				Kind:     ConflictEphemeral,
				Endpoint: ep,
				Instance: owned[0].Owner,
				Claims:   owned,
				Detail:   "inside the kernel ephemeral range " + o.Ephemeral().String(),
			})
		}
	}
	SortConflicts(out)
	return out
}

// Check audits one instance's materialised allocation against the manifest that
// declared it and the host it runs on.
//
// This is the half of section 6.7 that runs long after allocation. Ports are
// written literally into the instance YAML and never recomputed, which is what
// keeps a gvm upgrade from moving a live server -- and it is also why the
// allocator's guarantees decay: the manifest gains a port, the operator widens
// ip_local_port_range, someone hand-edits the YAML. Nothing re-derives the
// allocation, so something has to re-check it.
func Check(self string, reqs []Requirement, alloc Allocation, o *Occupancy) []Conflict {
	var out []Conflict
	declared := map[string]bool{}

	for _, r := range reqs {
		declared[r.Name] = true
		p, ok := alloc[r.Name]
		if !ok {
			out = append(out, Conflict{
				Kind:     ConflictMissing,
				Instance: self,
				Name:     r.Name,
				Detail:   fmt.Sprintf("the manifest declares it (default %d) and the instance materialises no port for it", r.Default),
			})
			continue
		}
		if p < 1 || p > MaxPort {
			out = append(out, Conflict{
				Kind:     ConflictRange,
				Endpoint: Endpoint{Port: p},
				Instance: self,
				Name:     r.Name,
				Detail:   fmt.Sprintf("%d is outside 1-%d", p, MaxPort),
			})
			continue
		}
		if p <= PrivilegedMax && r.Default > PrivilegedMax {
			out = append(out, Conflict{
				Kind:     ConflictPrivileged,
				Endpoint: Endpoint{Port: p},
				Instance: self,
				Name:     r.Name,
				Detail: fmt.Sprintf("%d needs CAP_NET_BIND_SERVICE and the manifest default %d does not",
					p, r.Default),
			})
		}
		if o != nil && o.InEphemeral(p) {
			out = append(out, Conflict{
				Kind:     ConflictEphemeral,
				Endpoint: Endpoint{Port: p},
				Instance: self,
				Name:     r.Name,
				Detail: "inside the kernel ephemeral range " + o.Ephemeral().String() +
					"; the bind works until an outbound connection takes it first",
			})
		}
		if o == nil {
			continue
		}
		for _, pr := range r.Protos {
			ep := Endpoint{Proto: pr, Port: p}
			var foreign []Claim
			for _, c := range o.Claims(ep) {
				if c.Owner == self {
					continue
				}
				// An unattributed listener is this instance's own server in the
				// common case; see Conflicts.
				if c.Owner == "" && c.Source == SourceListener {
					continue
				}
				foreign = append(foreign, c)
			}
			if len(foreign) > 0 {
				out = append(out, Conflict{
					Kind:     ConflictOverlap,
					Endpoint: ep,
					Instance: self,
					Name:     r.Name,
					Claims:   foreign,
					Detail:   "also held elsewhere",
				})
			}
		}
	}

	for _, name := range alloc.Names() {
		if declared[name] {
			continue
		}
		out = append(out, Conflict{
			Kind:     ConflictUnknown,
			Endpoint: Endpoint{Port: alloc[name]},
			Instance: self,
			Name:     name,
			Detail:   "materialised in the instance but not declared by the manifest",
		})
	}

	SortConflicts(out)
	return out
}

// CheckSpec is Check driven straight from a manifest, for the caller that has
// the GameSpec rather than the flattened requirements.
func CheckSpec(self string, g spec.GameSpec, alloc Allocation, o *Occupancy) ([]Conflict, error) {
	reqs, err := g.PortRequirements()
	if err != nil {
		return nil, err
	}
	return Check(self, reqs, alloc, o), nil
}
