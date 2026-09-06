package ports

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

func TestTCPAndUDPOnOnePortAreDistinctButAddressesAreNot(t *testing.T) {
	// 127.0.0.1:8080 and 0.0.0.0:8080 collide; tcp/27015 and udp/27015 do not.
	// Keeping the protocols apart in the type is what lets the occupancy map
	// say that once, and nothing anywhere records a bind address.
	occ := emptyHost()
	occ.AddListeners([]Listener{{Endpoint: Endpoint{TCP, 8080}, PID: 1, Comm: "caddy"}})

	if occ.Free(Endpoint{TCP, 8080}, "x") {
		t.Error("tcp/8080 is held")
	}
	if !occ.Free(Endpoint{UDP, 8080}, "x") {
		t.Error("udp/8080 is a different endpoint and must be free")
	}
}

func TestADisabledInstanceStillHoldsItsPorts(t *testing.T) {
	// A disabled server's ports stay reserved so re-enabling it can never
	// collide with something created in the meantime. With no manifest to say
	// which transport, both are held: claiming one and guessing wrong hands the
	// other away and the collision appears on the day the old one comes back.
	occ := emptyHost()
	occ.AddInstance("pz-old", map[string]int{"game": 16261}, nil)
	for _, pr := range []Proto{TCP, UDP} {
		if occ.Free(Endpoint{pr, 16261}, "pz-new") {
			t.Errorf("%s/16261 is free despite a disabled instance holding it", pr)
		}
	}
}

func TestAReservedPortIsHeldOnBothProtocols(t *testing.T) {
	// "Do not use 8080" has never once meant one of the two transports.
	occ := emptyHost()
	occ.AddReserved([]int{9100})
	for _, pr := range []Proto{TCP, UDP} {
		if occ.Free(Endpoint{pr, 9100}, "x") {
			t.Errorf("%s/9100 is not reserved", pr)
		}
	}
	if b := occ.Blockers(Endpoint{TCP, 9100}, "x"); len(b) != 1 || !strings.Contains(b[0].String(), "reserved_ports") {
		t.Errorf("blockers = %v", b)
	}
}

func TestAnUnattributedClaimNeverMatchesSelf(t *testing.T) {
	// Otherwise every foreign listener would be treated as the instance's own.
	occ := emptyHost()
	occ.Add(Claim{Endpoint: Endpoint{UDP, 16261}, Desc: "something", Source: SourceListener})
	if occ.Free(Endpoint{UDP, 16261}, "") {
		t.Error("an empty owner matched an empty self")
	}
	if occ.Free(Endpoint{UDP, 16261}, "pz-main") {
		t.Error("an unattributed claim must block")
	}
}

func TestBlockersExplainARangeThatNoClaimCovers(t *testing.T) {
	// The claims question and the range questions are answered by different
	// functions on purpose; Blockers is where the two halves come back
	// together, so "why can I not have this endpoint" has one answer.
	occ := emptyHost()
	b := occ.Blockers(Endpoint{UDP, 40000}, "x")
	if len(b) != 1 || b[0].Source != SourceEphemeral {
		t.Fatalf("blockers = %v, want the ephemeral range", b)
	}
	if !strings.Contains(b[0].String(), "32768-60999") {
		t.Errorf("%q does not name the range", b[0])
	}
	// Free still answers only the claims question.
	if !occ.Free(Endpoint{UDP, 40000}, "x") {
		t.Error("Free must not fold in the range checks; the allocator phrases those differently")
	}
}

func TestBlockersAreOrderedMostSpecificFirst(t *testing.T) {
	// A report that leads with "reserved by the operator" when an instance also
	// holds the port has buried the useful half.
	occ := emptyHost()
	occ.AddReserved([]int{16261})
	occ.AddListeners([]Listener{{Endpoint: Endpoint{UDP, 16261}, PID: 4412, Comm: "java"}})
	occ.AddInstance("pz-main", map[string]int{"game": 16261}, []Requirement{{Name: "game", Protos: []Proto{UDP}}})

	b := occ.Blockers(Endpoint{UDP, 16261}, "pz-new")
	if len(b) != 3 {
		t.Fatalf("blockers = %v, want three", b)
	}
	want := []Source{SourceInstance, SourceListener, SourceReserved}
	for i, s := range want {
		if b[i].Source != s {
			t.Errorf("blocker %d is %s, want %s (full: %v)", i, b[i].Source, s, b)
		}
	}
}

func TestIdenticalClaimsAreCollapsed(t *testing.T) {
	// Rebuilding an occupancy twice must not double every blocker line.
	occ := emptyHost()
	c := Claim{Endpoint: Endpoint{UDP, 16261}, Owner: "pz", Desc: "gvm@pz:game", Source: SourceInstance}
	occ.Add(c)
	occ.Add(c)
	if got := occ.Claims(Endpoint{UDP, 16261}); len(got) != 1 {
		t.Errorf("claims = %v", got)
	}
}

func TestAnUnbindableEndpointIsNotRecorded(t *testing.T) {
	// A claim on port 0 or 70000 could never block anything real, and storing
	// it would only make Endpoints() lie about what the host holds.
	occ := emptyHost()
	occ.Add(Claim{Endpoint: Endpoint{UDP, 0}, Source: SourceListener})
	occ.Add(Claim{Endpoint: Endpoint{UDP, 70000}, Source: SourceListener})
	occ.Add(Claim{Endpoint: Endpoint{Proto("icmp"), 1}, Source: SourceListener})
	if occ.Len() != 0 {
		t.Errorf("occupancy holds %d unbindable endpoints: %v", occ.Len(), occ.Endpoints())
	}
}

func TestCloneIsIndependent(t *testing.T) {
	occ := emptyHost()
	occ.AddInstance("a", map[string]int{"game": 16261}, []Requirement{{Name: "game", Protos: []Proto{UDP}}})
	c := occ.Clone()
	c.AddInstance("b", map[string]int{"game": 16271}, []Requirement{{Name: "game", Protos: []Proto{UDP}}})
	c.Add(Claim{Endpoint: Endpoint{UDP, 16261}, Owner: "c", Desc: "gvm@c:game", Source: SourceInstance})

	if !occ.Free(Endpoint{UDP, 16271}, "x") {
		t.Error("a claim added to the clone reached the original")
	}
	if got := occ.Claims(Endpoint{UDP, 16261}); len(got) != 1 {
		t.Errorf("the original's claim slice was appended to through the clone: %v", got)
	}
	if c.Ephemeral() != occ.Ephemeral() || c.EphemeralAssumed() != occ.EphemeralAssumed() {
		t.Error("the clone lost the ephemeral range")
	}
}

func TestEndpointsAreSortedByPortThenProtocol(t *testing.T) {
	occ := emptyHost()
	occ.AddReserved([]int{9100, 22})
	eps := occ.Endpoints()
	want := []Endpoint{{TCP, 22}, {UDP, 22}, {TCP, 9100}, {UDP, 9100}}
	if len(eps) != len(want) {
		t.Fatalf("endpoints = %v", eps)
	}
	for i := range want {
		if eps[i] != want[i] {
			t.Fatalf("endpoints = %v, want %v", eps, want)
		}
	}
}

func TestHostEphemeralNeverReturnsAnEmptyRange(t *testing.T) {
	// A caller must proceed even when the sysctl was unreadable, but must be
	// able to say in the log which of the two numbers it used.
	r, err := HostEphemeral()
	if !r.Valid() {
		t.Fatalf("range = %s, err = %v; an invalid range would mean allocating anywhere", r, err)
	}
	if err != nil && r != platform.DefaultEphemeral {
		t.Errorf("the fallback is not the documented kernel default: %s", r)
	}
}

func TestEndpointStringing(t *testing.T) {
	if got := (Endpoint{UDP, 16261}).String(); got != "udp/16261" {
		t.Errorf("String() = %q", got)
	}
	// A range finding has no protocol; it must still print something a person
	// can read.
	if got := (Endpoint{Port: 70000}).String(); got != "port 70000" {
		t.Errorf("String() = %q", got)
	}
}

func TestAllocationHelpers(t *testing.T) {
	reqs, _ := gameReqs(t, "7daystodie")
	a := Allocation{"game": 26900, "steam1": 26901, "steam2": 26902, "telnet": 8081}

	// A proto:both port yields two endpoints, which is why this takes the
	// requirements rather than reading the map alone.
	eps := a.Endpoints(reqs)
	if len(eps) != 5 {
		t.Fatalf("endpoints = %v, want five", eps)
	}
	if eps[0] != (Endpoint{TCP, 8081}) {
		t.Errorf("endpoints are not sorted: %v", eps)
	}

	if got := a.String(); got != "game=26900 steam1=26901 steam2=26902 telnet=8081" {
		t.Errorf("String() = %q", got)
	}
	b := a.Clone()
	b["game"] = 1
	if a["game"] != 26900 {
		t.Error("Clone aliases the original")
	}
	if a.Equal(b) {
		t.Error("Equal ignored a changed port")
	}
	if !a.Equal(a.Clone()) {
		t.Error("Equal rejected an identical clone")
	}
	if a.Equal(Allocation{"game": 26900}) {
		t.Error("Equal ignored a missing port")
	}
}
