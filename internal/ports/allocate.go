package ports

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Rejection is one window the allocator looked at and turned down, together
// with the reason and the claims behind it.
//
// Every rejected window produces exactly one of these -- the first blocker
// found, because that is the one that made the window unusable. Keeping them is
// the whole difference between "no ports free" and a report an operator can act
// on at two in the morning.
type Rejection struct {
	Shift    int      // k: how many strides above the manifest default
	Name     string   // the requirement that blocked the window
	Number   int      // the port number it wanted
	Endpoint Endpoint // zero when the reason is not protocol-specific
	Reason   string
	Claims   []Claim
}

func (r Rejection) String() string {
	where := r.Endpoint.String()
	if !r.Endpoint.Valid() {
		where = "port " + strconv.Itoa(r.Number)
	}
	var b strings.Builder
	b.WriteString(where)
	b.WriteString(" (")
	b.WriteString(r.Name)
	b.WriteString(") ")
	b.WriteString(r.Reason)
	for i, c := range r.Claims {
		if i == 0 {
			b.WriteString(" ")
			b.WriteString(c.String())
			continue
		}
		// More than one holder on one endpoint is either SO_REUSEPORT or a
		// conflict somebody has yet to notice. Either way the operator needs
		// all of them, not the first.
		b.WriteString("\n      also ")
		b.WriteString(c.String())
	}
	return b.String()
}

// ExhaustedError reports that a group has nowhere to go, and names the blocking
// claim for every window that was rejected.
//
// It lists all of them rather than the first few. A truncated list is a list
// that stops exactly before the interesting line: the operator is looking for
// the one window whose blocker is something they can remove, and the allocator
// has no way to know which one that is.
type ExhaustedError struct {
	Group      string
	Stride     int
	Start      int
	MaxShift   int
	Pinned     bool
	Rejections []Rejection
}

func (e *ExhaustedError) Error() string {
	var b strings.Builder
	if e.Pinned {
		// A pinned group has exactly one window by definition, so the shift
		// count would be a lie.
		fmt.Fprintf(&b, "ports: group %q is pinned and its declared ports are not free:", e.Group)
	} else {
		fmt.Fprintf(&b, "ports: no free window for group %q after %d shifts of %d starting at %d:",
			e.Group, e.MaxShift, e.Stride, e.Start)
	}
	for _, r := range e.Rejections {
		b.WriteString("\n  ")
		b.WriteString(r.String())
	}
	return b.String()
}

func (e *ExhaustedError) Unwrap() error { return ErrExhausted }

// Allocate places every port group in reqs, shifting each group by its own
// stride until it finds a free window.
//
// A group shifts as a unit, which is what keeps the contiguity requirements
// real games have -- ARK 7777+7778, DayZ 2302-2305, 7DTD 26900-26902 -- intact
// under collision. Each group carries its own shift counter, so Project
// Zomboid's second instance gets 16271/16272 from a stride of 10 while its rcon
// port moves 27015 to 27016 on a stride of 1. A single global offset would put
// rcon on 27025 for no reason and would break the moment two games disagreed
// about how far apart their ports must be.
//
// occ is mutated only on success, with the claims this allocation made, so that
// a caller placing several instances in a row cannot hand the same port to two
// of them.
func Allocate(reqs []Requirement, strides map[string]int, occ *Occupancy,
	self string, pins []Pin) (Allocation, error) {

	if occ == nil {
		return nil, fmt.Errorf("ports: allocate needs an occupancy; a nil one is not an empty host, it is an unread one")
	}
	if self == "" {
		return nil, fmt.Errorf("ports: allocate needs the instance name; with an empty one every unattributed claim on the host looks like this instance's own")
	}
	if len(reqs) == 0 {
		return Allocation{}, nil
	}

	byName := make(map[string]Requirement, len(reqs))
	for _, r := range reqs {
		if _, dup := byName[r.Name]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateName, r.Name)
		}
		byName[r.Name] = r
	}

	pinned := make(map[string]int, len(pins))
	for _, p := range pins {
		if _, ok := byName[p.Name]; !ok {
			return nil, fmt.Errorf("%w: %q (the manifest declares %s)", ErrUnknownPin, p.Name, nameList(reqs))
		}
		if p.Port < 1 || p.Port > MaxPort {
			return nil, fmt.Errorf("%w: %s=%d", ErrPinOutOfRange, p.Name, p.Port)
		}
		if prev, dup := pinned[p.Name]; dup && prev != p.Port {
			return nil, fmt.Errorf("%w: %s is pinned to both %d and %d", ErrPinConflict, p.Name, prev, p.Port)
		}
		pinned[p.Name] = p.Port
	}

	scratch := occ.Clone()
	// taken is what THIS call has already placed. It cannot be left to
	// scratch.Free, which ignores claims owned by self on purpose so that
	// re-allocating an existing instance keeps its own ports: without this map
	// two groups whose defaults happen to collide would both be handed the same
	// number and the collision would surface as a bind failure at first start.
	taken := make(map[Endpoint]string)
	out := Allocation{}
	var added []Claim

	for _, g := range spec.GroupOrder(reqs) {
		stride := strides[g]
		if stride <= 0 {
			stride = 1
		}
		members := spec.MembersOf(reqs, g)

		hasPin := false
		for _, m := range members {
			if _, ok := pinned[m.Name]; ok {
				hasPin = true
				break
			}
		}

		var placed Allocation
		var rejects []Rejection
		for k := 0; k <= MaxShift; k++ {
			cand, rej := tryWindow(members, k, stride, pinned, hasPin, scratch, self, taken)
			if rej == nil {
				placed = cand
				break
			}
			rejects = append(rejects, *rej)
			if hasPin {
				// A pin fixes its group at k=0. Shifting would move a port that
				// a live server is bound to right now, which is the one thing
				// adoption may never do, so there is no second window to try
				// and no point reporting two hundred identical refusals.
				break
			}
		}
		if placed == nil {
			return nil, &ExhaustedError{
				Group:      g,
				Stride:     stride,
				Start:      members[0].Default,
				MaxShift:   MaxShift,
				Pinned:     hasPin,
				Rejections: rejects,
			}
		}

		for _, m := range members {
			p := placed[m.Name]
			out[m.Name] = p
			for _, pr := range m.Protos {
				ep := Endpoint{Proto: pr, Port: p}
				taken[ep] = m.Name
				c := Claim{
					Endpoint: ep,
					Owner:    self,
					Desc:     "gvm@" + self + ":" + m.Name,
					Source:   SourceInstance,
				}
				scratch.Add(c)
				added = append(added, c)
			}
		}
	}

	occ.AddAll(added)
	return out, nil
}

// tryWindow tests one shift of one group, all or nothing. It returns the
// candidate placement or the first reason the window failed.
//
// groupPinned skips the ephemeral test and narrows the occupancy test to
// FreeForPin, and it does so for EVERY member of the group rather than only the
// pinned one. A pin fixes the group's window, so its unpinned members are
// equally immovable: refusing one of them because a foreign listener holds it
// offers the operator no alternative, it only declines to adopt the server that
// listener belongs to. What a pin never relaxes is another instance's declared
// port or a live reservation -- those are two servers wanting one port, which
// no amount of pinning makes work.
func tryWindow(members []Requirement, k, stride int, pinned map[string]int, groupPinned bool,
	occ *Occupancy, self string, taken map[Endpoint]string) (Allocation, *Rejection) {

	cand := Allocation{}
	// local catches a group whose own members would land on the same endpoint,
	// which happens when a manifest gives two ports the same default.
	local := map[Endpoint]string{}

	for _, r := range members {
		p := r.Default + k*stride
		if v, ok := pinned[r.Name]; ok {
			if k != 0 {
				return nil, &Rejection{Shift: k, Name: r.Name, Number: p,
					Reason: fmt.Sprintf("is pinned to %d, and a pin fixes its group at its declared ports", v)}
			}
			p = v
		}

		if p < 1 || p > MaxPort {
			return nil, &Rejection{Shift: k, Name: r.Name, Number: p,
				Reason: fmt.Sprintf("is outside 1-%d", MaxPort)}
		}
		// A port that started unprivileged and shifted below 1024 would need a
		// capability the game server unit deliberately does not have. A port
		// whose manifest default is already privileged was asked for on
		// purpose and is left alone.
		if p <= PrivilegedMax && r.Default > PrivilegedMax {
			return nil, &Rejection{Shift: k, Name: r.Name, Number: p,
				Reason: fmt.Sprintf("is privileged (<=%d) and the manifest default %d is not", PrivilegedMax, r.Default)}
		}
		if !groupPinned && occ.InEphemeral(p) {
			return nil, &Rejection{Shift: k, Name: r.Name, Number: p,
				Reason: "is inside the kernel ephemeral range " + occ.Ephemeral().String() +
					"; the kernel may hand it to an outbound connection before the server binds"}
		}

		for _, pr := range r.Protos {
			ep := Endpoint{Proto: pr, Port: p}
			if other, ok := taken[ep]; ok {
				return nil, &Rejection{Shift: k, Name: r.Name, Number: p, Endpoint: ep,
					Reason: fmt.Sprintf("was already placed as %q by an earlier group of this same instance", other)}
			}
			if other, ok := local[ep]; ok && other != r.Name {
				return nil, &Rejection{Shift: k, Name: r.Name, Number: p, Endpoint: ep,
					Reason: fmt.Sprintf("collides with %q in its own group", other)}
			}
			if groupPinned {
				if !occ.FreeForPin(ep, self) {
					return nil, &Rejection{Shift: k, Name: r.Name, Number: p, Endpoint: ep,
						Reason: "is in a pinned group and held by", Claims: occ.PinBlockers(ep, self)}
				}
			} else if !occ.Free(ep, self) {
				return nil, &Rejection{Shift: k, Name: r.Name, Number: p, Endpoint: ep,
					Reason: "held by", Claims: occ.Blockers(ep, self)}
			}
			local[ep] = r.Name
		}
		cand[r.Name] = p
	}
	return cand, nil
}
