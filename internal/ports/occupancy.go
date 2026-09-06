package ports

import (
	"sort"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// Occupancy is what the host currently owes: every endpoint somebody holds,
// plus the kernel range nobody may allocate into.
//
// It is built by the caller from all five sources of section 6.7 and handed to
// the allocator. Nothing in here reads a file, so the interesting logic is
// testable without a host to read.
type Occupancy struct {
	claims map[Endpoint][]Claim

	eph platform.PortRange
	// ephAssumed records that the live ip_local_port_range could not be read
	// and the kernel's compiled-in default was substituted. It is worth
	// surfacing: a host whose administrator widened the range is exactly the
	// host where the assumption is wrong, and the failure it produces --
	// intermittent bind failures weeks later -- is unattributable by then.
	ephAssumed bool
}

// NewOccupancy starts an empty occupancy over the kernel's ephemeral range.
//
// An invalid or zero range is replaced with platform.DefaultEphemeral rather
// than treated as empty. Treating it as empty is the one failure mode section
// 6.7 singles out: the allocator would hand out a port the kernel may later
// give to an outbound connection, and the server fails to bind on some
// unrelated Tuesday with nothing in the diff to explain it.
func NewOccupancy(eph platform.PortRange) *Occupancy {
	o := &Occupancy{claims: map[Endpoint][]Claim{}, eph: eph}
	if !eph.Valid() {
		o.eph = platform.DefaultEphemeral
		o.ephAssumed = true
	}
	return o
}

// HostEphemeral reads the live range, falling back to the kernel default.
//
// The error is returned alongside the range rather than instead of it, because
// the caller must proceed -- refusing to allocate because a sysctl was
// unreadable helps nobody -- but must also be able to say in the log which of
// the two numbers it used.
func HostEphemeral() (platform.PortRange, error) {
	r, err := platform.EphemeralRange()
	if err != nil {
		return platform.DefaultEphemeral, err
	}
	return r, nil
}

// Ephemeral is the range in force.
func (o *Occupancy) Ephemeral() platform.PortRange { return o.eph }

// EphemeralAssumed reports that the range is the kernel default rather than
// this host's live value.
func (o *Occupancy) EphemeralAssumed() bool { return o.ephAssumed }

// InEphemeral reports whether p falls in the kernel's outbound range.
func (o *Occupancy) InEphemeral(p int) bool { return o.eph.Contains(p) }

// Add records a claim. Duplicate identical claims are collapsed so that
// rebuilding an occupancy twice does not double every blocker line.
func (o *Occupancy) Add(c Claim) {
	if !c.Endpoint.Valid() {
		return
	}
	for _, have := range o.claims[c.Endpoint] {
		if have == c {
			return
		}
	}
	o.claims[c.Endpoint] = append(o.claims[c.Endpoint], c)
}

// AddAll records a batch.
func (o *Occupancy) AddAll(cs []Claim) {
	for _, c := range cs {
		o.Add(c)
	}
}

// AddInstance records one instance's materialised ports.
//
// reqs supplies the protocols, because the instance YAML stores only
// name-to-number. When reqs is nil -- a disabled instance whose manifest is no
// longer installed, which is exactly the case section 6.7 insists must still
// reserve its ports -- both protocols are claimed. Claiming one and guessing
// wrong would hand the other to a new instance and produce a collision on the
// day the old one is re-enabled.
func (o *Occupancy) AddInstance(name string, alloc map[string]int, reqs []Requirement) {
	byName := make(map[string][]Proto, len(reqs))
	for _, r := range reqs {
		byName[r.Name] = r.Protos
	}
	for port, num := range alloc {
		protos := byName[port]
		if len(protos) == 0 {
			protos = []Proto{TCP, UDP}
		}
		for _, pr := range protos {
			o.Add(Claim{
				Endpoint: Endpoint{Proto: pr, Port: num},
				Owner:    name,
				Desc:     "gvm@" + name + ":" + port,
				Source:   SourceInstance,
			})
		}
	}
}

// AddReserved records the operator's don't-touch list.
//
// A bare number reserves the port on BOTH protocols. "Do not use 8080" has
// never once meant one of the two transports, and the schema documents the
// same rule, so honouring only tcp here would quietly re-open half of every
// reservation.
func (o *Occupancy) AddReserved(ports []int) {
	for _, p := range ports {
		for _, pr := range []Proto{TCP, UDP} {
			o.Add(Claim{
				Endpoint: Endpoint{Proto: pr, Port: p},
				Desc:     "reserved by the operator (gvm.yaml reserved_ports)",
				Source:   SourceReserved,
			})
		}
	}
}

// AddListeners records sockets found in /proc/net.
func (o *Occupancy) AddListeners(ls []Listener) {
	for _, l := range ls {
		o.Add(Claim{
			Endpoint: l.Endpoint,
			Owner:    l.Owner,
			Desc:     l.Describe(),
			Source:   SourceListener,
		})
	}
}

// AddReservations records in-flight ledger entries.
func (o *Occupancy) AddReservations(rs []Reservation) {
	for _, r := range rs {
		o.Add(r.Claim())
	}
}

// Free reports that no other owner holds ep.
//
// It answers the claims question only. The range questions -- inside the
// ephemeral span, below 1024, past 65535 -- are the allocator's, because none
// of them is a claim anybody holds and all three have to be phrased differently
// in the report an operator reads. Blockers is where the two halves come back
// together.
//
// A claim with an empty Owner never matches self, so anything gvm could not
// attribute always blocks.
func (o *Occupancy) Free(ep Endpoint, self string) bool {
	for _, c := range o.claims[ep] {
		if c.Owner == "" || c.Owner != self {
			return false
		}
	}
	return true
}

// FreeForPin is Free restricted to the claims gvm itself made, and is what a
// pinned group is tested against.
//
// A pin is a statement of fact about a live server, not a request: adoption
// takes it from the running server's own config file. The sources it therefore
// ignores all describe that same server -- its own socket in /proc/net, an
// operator reservation that most likely exists to protect it, and the ephemeral
// range it is already successfully bound inside. Refusing on any of them would
// make gvm decline to adopt precisely the servers it was written to adopt,
// which does not move the server, it only leaves it unmanaged.
//
// A pinned group still loses to another instance's declared ports or to a live
// reservation, because those are two servers wanting one port and no amount of
// pinning makes that work. `gvm doctor` reports the ignored cases through
// Check, so a pin into the ephemeral range is adopted and then complained about
// rather than silently blessed.
func (o *Occupancy) FreeForPin(ep Endpoint, self string) bool {
	for _, c := range o.claims[ep] {
		switch c.Source {
		case SourceInstance, SourceLedger:
			if c.Owner == "" || c.Owner != self {
				return false
			}
		}
	}
	return true
}

// Claims returns every claim on ep, most specifically attributed first.
func (o *Occupancy) Claims(ep Endpoint) []Claim {
	src := o.claims[ep]
	if len(src) == 0 {
		return nil
	}
	out := make([]Claim, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool { return out[i].less(out[j]) })
	return out
}

// Blockers is the complete answer to "why can I not have this endpoint": every
// foreign claim, plus a synthetic claim when the port sits in the kernel's
// ephemeral range. It is what the exhaustion report prints.
func (o *Occupancy) Blockers(ep Endpoint, self string) []Claim {
	var out []Claim
	for _, c := range o.Claims(ep) {
		if c.Owner != "" && c.Owner == self {
			continue
		}
		out = append(out, c)
	}
	if o.InEphemeral(ep.Port) {
		out = append(out, Claim{
			Endpoint: ep,
			Desc:     "the kernel ephemeral range " + o.eph.String(),
			Source:   SourceEphemeral,
		})
	}
	return out
}

// PinBlockers is Blockers narrowed to what a pin actually loses to, so a
// refused pin does not print the listener that is the pinned server itself.
func (o *Occupancy) PinBlockers(ep Endpoint, self string) []Claim {
	var out []Claim
	for _, c := range o.Claims(ep) {
		switch c.Source {
		case SourceInstance, SourceLedger:
			if c.Owner != "" && c.Owner == self {
				continue
			}
			out = append(out, c)
		}
	}
	return out
}

// Endpoints lists every endpoint holding at least one claim, in port order.
func (o *Occupancy) Endpoints() []Endpoint {
	out := make([]Endpoint, 0, len(o.claims))
	for ep := range o.claims {
		out = append(out, ep)
	}
	SortEndpoints(out)
	return out
}

// Len is the number of distinct claimed endpoints.
func (o *Occupancy) Len() int { return len(o.claims) }

// Clone returns an independent copy. The allocator works on one so that a
// failed allocation leaves the caller's occupancy exactly as it was found --
// otherwise a `gvm create` that ran out of ports would leave half a group's
// worth of phantom claims behind for the next one to trip over.
func (o *Occupancy) Clone() *Occupancy {
	c := &Occupancy{claims: make(map[Endpoint][]Claim, len(o.claims)), eph: o.eph, ephAssumed: o.ephAssumed}
	for ep, cs := range o.claims {
		dup := make([]Claim, len(cs))
		copy(dup, cs)
		c.claims[ep] = dup
	}
	return c
}
