// Package ports decides which ports an instance gets, and refuses to guess
// (section 6.7).
//
// The unit of everything here is the Endpoint: a (protocol, port) pair. A
// listener occupies its endpoint regardless of the address it bound to, so
// 127.0.0.1:8080 and 0.0.0.0:8080 do collide, while tcp/27015 and udp/27015 do
// not. Every occupancy source in this package flattens to endpoints for that
// one reason.
//
// The package is pure. The five occupancy sources are gathered by the caller
// and handed in; the /proc reader takes an io.Reader and the flock takes a
// function. Nothing here shells out to ss or netstat -- their output format is
// a moving target across distributions, parsing it is strictly harder than
// parsing /proc, and an exec in the middle of allocation is an exec that can
// hang a `gvm create` behind a DNS timeout.
package ports

import (
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Proto and Requirement are spec's types, aliased rather than redeclared.
//
// A struct of our own would be a second definition of the same thing, and the
// two would drift the first time a manifest field is added: the allocator would
// keep placing ports while quietly ignoring whatever the schema learned. An
// alias cannot drift.
type (
	Proto       = spec.Proto
	Requirement = spec.PortRequirement
)

const (
	TCP = spec.ProtoTCP
	UDP = spec.ProtoUDP
)

const (
	// MaxPort is the last port a 16-bit port number can name.
	MaxPort = 65535

	// PrivilegedMax is the last port that needs CAP_NET_BIND_SERVICE. Game
	// servers run unprivileged and under systemd sandboxing, so a port down
	// here is a unit that starts and then fails to bind.
	PrivilegedMax = 1023

	// MaxShift bounds the search for a free window. It is a real limit and not
	// a safety net: at PZ's stride of 10 it covers 16261 through 18261, which
	// is two thousand ports of the same game on one host. A search that ran to
	// exhaustion of the port space would spend its time proving that a box
	// with 65535 occupied ports is full, which nobody needs proved.
	MaxShift = 200
)

// Errors this package returns. Each is a sentinel because the caller's response
// differs: exhaustion is an operator problem to be printed in full, a bad pin
// is a bug in whoever built the pin list, and a duplicate name is a manifest
// that should never have validated.
var (
	ErrExhausted     = errors.New("ports: no free window")
	ErrUnknownPin    = errors.New("ports: pin names a port the manifest does not declare")
	ErrPinConflict   = errors.New("ports: two pins name the same port with different numbers")
	ErrPinOutOfRange = errors.New("ports: pinned port is outside 1-65535")
	ErrDuplicateName = errors.New("ports: two requirements share a name")
)

// Endpoint is one (protocol, port) pair: the thing that is either free or held.
type Endpoint struct {
	Proto Proto
	Port  int
}

func (e Endpoint) String() string {
	if e.Proto == "" {
		return "port " + strconv.Itoa(e.Port)
	}
	return string(e.Proto) + "/" + strconv.Itoa(e.Port)
}

// Valid reports whether e names a bindable endpoint.
func (e Endpoint) Valid() bool {
	return (e.Proto == TCP || e.Proto == UDP) && e.Port >= 1 && e.Port <= MaxPort
}

// less orders endpoints by port then protocol, so every listing this package
// produces is stable and diffable between runs.
func (e Endpoint) less(o Endpoint) bool {
	if e.Port != o.Port {
		return e.Port < o.Port
	}
	return e.Proto < o.Proto
}

// SortEndpoints orders a slice in place.
func SortEndpoints(eps []Endpoint) {
	sort.Slice(eps, func(i, j int) bool { return eps[i].less(eps[j]) })
}

// Source is where a claim came from. All five of section 6.7's sources appear
// here, including the two that hold no endpoints of their own: an operator's
// reserved list and the kernel's ephemeral range.
type Source string

const (
	// SourceInstance is an /etc/gvm/instances/*.yaml ports: block. It is
	// authoritative, and it includes instances with enabled: false -- a
	// disabled server's ports stay reserved so that re-enabling it can never
	// collide with something created in the meantime.
	SourceInstance Source = "instance"

	// SourceLedger is an in-flight reservation from ports.lock.json, held
	// between `gvm create` choosing a window and the instance YAML being
	// written. Without it two concurrent creates both pick 16261.
	SourceLedger Source = "ledger"

	// SourceListener is a socket found in /proc/net. It catches everything gvm
	// did not create: a hand-run server, a metrics exporter, sshd on a
	// nonstandard port.
	SourceListener Source = "listener"

	// SourceReserved is the operator's own don't-touch list from gvm.yaml.
	SourceReserved Source = "reserved"

	// SourceEphemeral is the kernel's outbound port range. It is a range and
	// not a set of endpoints, so it is never stored as a claim -- Occupancy
	// answers it from InEphemeral and synthesises a claim only when something
	// has to be explained to an operator.
	SourceEphemeral Source = "ephemeral"
)

// rank orders sources by how specifically they identify a holder, most
// specific first. A report that leads with "reserved by the operator" when an
// instance also holds the port has buried the useful half.
func (s Source) rank() int {
	switch s {
	case SourceInstance:
		return 0
	case SourceLedger:
		return 1
	case SourceListener:
		return 2
	case SourceReserved:
		return 3
	case SourceEphemeral:
		return 4
	}
	return 5
}

// Claim is one reason an endpoint is not available.
//
// Owner is the instance that holds it, and is empty for everything gvm does not
// own -- a foreign listener, a reserved port. Empty never matches the instance
// being allocated for, so a claim gvm cannot attribute always blocks.
type Claim struct {
	Endpoint Endpoint
	Owner    string
	Desc     string
	Source   Source
}

func (c Claim) String() string {
	if c.Desc != "" {
		return c.Desc
	}
	if c.Owner != "" {
		return string(c.Source) + " " + c.Owner
	}
	return string(c.Source)
}

// less orders claims for stable reporting.
func (c Claim) less(o Claim) bool {
	if c.Source.rank() != o.Source.rank() {
		return c.Source.rank() < o.Source.rank()
	}
	if c.Owner != o.Owner {
		return c.Owner < o.Owner
	}
	return c.Desc < o.Desc
}

// Pin fixes one port at a number the allocator may not move.
//
// Adoption builds these from the live config file -- Project Zomboid's
// DefaultPort, UDPPort and RCONPort out of the running server's .ini -- never
// from manifest defaults, so discovery can never propose moving a server that
// players are connected to right now.
type Pin struct {
	Name string
	Port int
}

// Allocation is the materialised result: port name to number. It is written
// literally into the instance YAML and never recomputed at runtime, so a later
// change to this allocator cannot move a live server.
type Allocation map[string]int

// Names returns the allocated port names in sorted order.
func (a Allocation) Names() []string {
	out := make([]string, 0, len(a))
	for n := range a {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Endpoints flattens an allocation against the requirements that produced it.
// A port declared `proto: both` yields two endpoints, which is the whole reason
// this takes the requirements rather than reading the map alone.
func (a Allocation) Endpoints(reqs []Requirement) []Endpoint {
	var out []Endpoint
	for _, r := range reqs {
		p, ok := a[r.Name]
		if !ok {
			continue
		}
		for _, pr := range r.Protos {
			out = append(out, Endpoint{Proto: pr, Port: p})
		}
	}
	SortEndpoints(out)
	return out
}

// Equal reports whether two allocations name the same ports.
func (a Allocation) Equal(b Allocation) bool {
	if len(a) != len(b) {
		return false
	}
	for n, p := range a {
		if q, ok := b[n]; !ok || q != p {
			return false
		}
	}
	return true
}

// Clone returns an independent copy.
func (a Allocation) Clone() Allocation {
	out := make(Allocation, len(a))
	for n, p := range a {
		out[n] = p
	}
	return out
}

// String renders the allocation in a fixed order, so it can go straight into a
// diff or a test expectation.
func (a Allocation) String() string {
	var b strings.Builder
	for i, n := range a.Names() {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(n)
		b.WriteString("=")
		b.WriteString(strconv.Itoa(a[n]))
	}
	return b.String()
}

// nameList renders the declared port names for an error message.
func nameList(reqs []Requirement) string {
	names := make([]string, 0, len(reqs))
	for _, r := range reqs {
		names = append(names, strconv.Quote(r.Name))
	}
	return strings.Join(names, ", ")
}
