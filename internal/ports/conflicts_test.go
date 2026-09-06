package ports

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

func kinds(cs []Conflict) []ConflictKind {
	out := make([]ConflictKind, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Kind)
	}
	return out
}

func findConflict(cs []Conflict, kind ConflictKind, name string) (Conflict, bool) {
	for _, c := range cs {
		if c.Kind == kind && (name == "" || c.Name == name) {
			return c, true
		}
	}
	return Conflict{}, false
}

func TestTwoInstancesOnOneEndpointAreReportedNotRefused(t *testing.T) {
	// D-24: the schema deliberately has no cross-instance unique constraint on
	// port allocations. A store that refuses to record reality does not make
	// the collision go away, it makes it unreportable -- so the collision is a
	// finding.
	occ := emptyHost()
	occ.AddInstance("pz-main", map[string]int{"game": 16261}, []Requirement{
		{Name: "game", Protos: []Proto{UDP}},
	})
	occ.AddInstance("pz-eu", map[string]int{"game": 16261}, []Requirement{
		{Name: "game", Protos: []Proto{UDP}},
	})

	cs := Conflicts(occ)
	if len(cs) != 1 {
		t.Fatalf("conflicts = %v, want exactly one overlap", kinds(cs))
	}
	c := cs[0]
	if c.Kind != ConflictOverlap || c.Endpoint != (Endpoint{UDP, 16261}) {
		t.Fatalf("conflict = %v", c)
	}
	// Both holders are named. One of them alone is half a bug report.
	s := c.String()
	for _, want := range []string{"gvm@pz-main:game", "gvm@pz-eu:game"} {
		if !strings.Contains(s, want) {
			t.Errorf("the report does not name %s:\n%s", want, s)
		}
	}
}

func TestAnInstancesOwnListenerIsNotAConflict(t *testing.T) {
	// A running server holds its own port. Reporting that would put a
	// permanent finding on every healthy host, which is how a non-zero problem
	// count trains an operator to stop reading the report.
	occ := emptyHost()
	occ.AddInstance("pz-main", map[string]int{"game": 16261}, []Requirement{{Name: "game", Protos: []Proto{UDP}}})
	occ.AddListeners([]Listener{{Endpoint: Endpoint{UDP, 16261}, PID: 4412, Comm: "java"}})

	if cs := Conflicts(occ); len(cs) != 0 {
		t.Fatalf("conflicts = %v, want none", cs)
	}
}

func TestAListenerOwnedBySomeoneElseIsAConflict(t *testing.T) {
	// Once the pid-to-instance mapping is available, a listener with a
	// different owner is the case that matters: something else is already on
	// the port this instance is about to be started on.
	occ := emptyHost()
	occ.AddInstance("pz-main", map[string]int{"game": 16261}, []Requirement{{Name: "game", Protos: []Proto{UDP}}})
	occ.AddListeners(AttributeListeners(
		[]Listener{{Endpoint: Endpoint{UDP, 16261}, PID: 4412, Comm: "java"}},
		map[int]string{4412: "pz-eu"},
	))

	cs := Conflicts(occ)
	if len(cs) != 1 || cs[0].Kind != ConflictOverlap {
		t.Fatalf("conflicts = %v, want one overlap", cs)
	}
	if !strings.Contains(cs[0].String(), "gvm@pz-eu") {
		t.Errorf("report does not name the other owner:\n%s", cs[0])
	}
}

func TestAnOwnedPortOnTheReservedListIsReported(t *testing.T) {
	// The operator wrote that list precisely to be told.
	occ := emptyHost()
	occ.AddInstance("pz-main", map[string]int{"rcon": 27015}, []Requirement{{Name: "rcon", Protos: []Proto{TCP}}})
	occ.AddReserved([]int{27015})

	c, ok := findConflict(Conflicts(occ), ConflictOverlap, "")
	if !ok {
		t.Fatal("a reserved port that an instance also declares must be reported")
	}
	if !strings.Contains(c.Detail, "reserved list") {
		t.Errorf("detail = %q", c.Detail)
	}
}

func TestAnOwnedPortInsideTheEphemeralRangeIsReported(t *testing.T) {
	occ := emptyHost()
	occ.AddInstance("pz-main", map[string]int{"game": 40000}, []Requirement{{Name: "game", Protos: []Proto{UDP}}})

	c, ok := findConflict(Conflicts(occ), ConflictEphemeral, "")
	if !ok {
		t.Fatalf("conflicts = %v, want an ephemeral finding", kinds(Conflicts(occ)))
	}
	if !strings.Contains(c.Detail, "32768-60999") {
		t.Errorf("detail = %q, want it to name the range", c.Detail)
	}
}

// ---------- Check: the audit of one instance's materialised allocation ----------

func TestCheckFindsAManifestPortWithNothingMaterialised(t *testing.T) {
	// What a manifest revision that added a port looks like from the old
	// instance YAML's side. Ports are written literally and never recomputed,
	// so nothing else would ever notice.
	reqs, _ := gameReqs(t, "projectzomboid")
	cs := Check("pz-main", reqs, Allocation{"game": 16261, "direct": 16262}, emptyHost())

	c, ok := findConflict(cs, ConflictMissing, "rcon")
	if !ok {
		t.Fatalf("conflicts = %v, want a missing rcon", kinds(cs))
	}
	if !strings.Contains(c.Detail, "27015") {
		t.Errorf("detail = %q, want it to name the manifest default", c.Detail)
	}
}

func TestCheckFindsAMaterialisedPortTheManifestNoLongerDeclares(t *testing.T) {
	// Harmless to the server and important to the firewall, which would
	// otherwise keep opening a hole for a port nothing listens on.
	reqs, _ := gameReqs(t, "valheim")
	cs := Check("vh-1", reqs, Allocation{"game": 2456, "query": 2457, "spare": 2458, "steam": 27016}, emptyHost())
	if _, ok := findConflict(cs, ConflictUnknown, "steam"); !ok {
		t.Fatalf("conflicts = %v, want an unknown port", kinds(cs))
	}
}

func TestCheckFindsHandEditedNonsense(t *testing.T) {
	reqs := []Requirement{
		{Name: "a", Default: 20000, Protos: []Proto{TCP}},
		{Name: "b", Default: 20001, Protos: []Proto{TCP}},
	}
	cs := Check("x", reqs, Allocation{"a": 70000, "b": 80}, emptyHost())
	if _, ok := findConflict(cs, ConflictRange, "a"); !ok {
		t.Errorf("70000 is not a port number; conflicts = %v", kinds(cs))
	}
	c, ok := findConflict(cs, ConflictPrivileged, "b")
	if !ok {
		t.Fatalf("80 needs a capability the game unit does not have; conflicts = %v", kinds(cs))
	}
	if !strings.Contains(c.Detail, "CAP_NET_BIND_SERVICE") {
		t.Errorf("detail = %q", c.Detail)
	}
}

func TestCheckDoesNotComplainAboutAPortTheManifestAskedForLow(t *testing.T) {
	reqs := []Requirement{{Name: "https", Default: 443, Protos: []Proto{TCP}}}
	if cs := Check("edge", reqs, Allocation{"https": 443}, emptyHost()); len(cs) != 0 {
		t.Fatalf("conflicts = %v, want none for a manifest that asked for 443", cs)
	}
}

func TestCheckReportsAnEphemeralPortThatWasFineWhenItWasAllocated(t *testing.T) {
	// The operator widened ip_local_port_range after the instance was created.
	// Nothing re-derives the allocation, so this is the only place it surfaces.
	reqs := []Requirement{{Name: "game", Default: 25000, Protos: []Proto{UDP}}}
	wide := NewOccupancy(platform.PortRange{Low: 20000, High: 60999})
	cs := Check("x", reqs, Allocation{"game": 25000}, wide)
	if _, ok := findConflict(cs, ConflictEphemeral, "game"); !ok {
		t.Fatalf("conflicts = %v, want an ephemeral finding", kinds(cs))
	}
}

func TestCheckIgnoresTheInstancesOwnListenerAndClaims(t *testing.T) {
	reqs, _ := gameReqs(t, "projectzomboid")
	alloc := Allocation{"game": 16261, "direct": 16262, "rcon": 27015}
	occ := emptyHost()
	occ.AddInstance("pz-main", alloc, reqs)
	occ.AddListeners([]Listener{{Endpoint: Endpoint{UDP, 16261}, PID: 4412, Comm: "java"}})

	if cs := Check("pz-main", reqs, alloc, occ); len(cs) != 0 {
		t.Fatalf("a healthy running instance produced %v", cs)
	}
}

func TestCheckReportsAnotherInstanceOnTheSamePort(t *testing.T) {
	reqs, _ := gameReqs(t, "projectzomboid")
	alloc := Allocation{"game": 16261, "direct": 16262, "rcon": 27015}
	occ := emptyHost()
	occ.AddInstance("pz-main", alloc, reqs)
	occ.AddInstance("pz-eu", map[string]int{"game": 16261}, []Requirement{{Name: "game", Protos: []Proto{UDP}}})

	c, ok := findConflict(Check("pz-main", reqs, alloc, occ), ConflictOverlap, "game")
	if !ok {
		t.Fatal("want an overlap for the port pz-eu also declares")
	}
	if len(c.Claims) != 1 || c.Claims[0].Owner != "pz-eu" {
		t.Errorf("claims = %v, want exactly pz-eu", c.Claims)
	}
}

func TestCheckToleratesANilOccupancy(t *testing.T) {
	// The schema-only half of the audit still works with no host to compare
	// against, which is what `gvm apply --diff` on a laptop does.
	reqs, _ := gameReqs(t, "projectzomboid")
	cs := Check("pz-main", reqs, Allocation{"game": 16261}, nil)
	if len(cs) != 2 {
		t.Fatalf("conflicts = %v, want the two missing ports", kinds(cs))
	}
}

func TestCheckSpecDrivesStraightFromAManifest(t *testing.T) {
	m := shipped(t, "projectzomboid")
	cs, err := CheckSpec("pz-main", m.Spec, Allocation{"game": 16261, "direct": 16262, "rcon": 27015}, emptyHost())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Fatalf("a correct allocation produced %v", cs)
	}
}

func TestConflictReportsAreOrderedDeterministically(t *testing.T) {
	// Two runs over the same host must produce the same report, or a diff of
	// two reports means nothing.
	reqs := []Requirement{
		{Name: "z", Default: 30000, Protos: []Proto{TCP}},
		{Name: "a", Default: 30001, Protos: []Proto{TCP}},
		{Name: "m", Default: 30002, Protos: []Proto{TCP}},
	}
	alloc := Allocation{"z": 70000, "a": 70001, "m": 70002}
	first := Check("x", reqs, alloc, emptyHost())
	for i := 0; i < 20; i++ {
		again := Check("x", reqs, alloc, emptyHost())
		for j := range first {
			if first[j].String() != again[j].String() {
				t.Fatalf("run %d differs at %d:\n%s\n%s", i, j, first[j], again[j])
			}
		}
	}
	if got := first[0].Endpoint.Port; got != 70000 {
		t.Errorf("first conflict is for port %d; findings are ordered by endpoint", got)
	}
}
