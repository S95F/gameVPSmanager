package ports

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// ---------- helpers ----------

// shipped loads a manifest gvm actually ships, so the allocator is exercised
// against the real port tables rather than against a convenient invention. A
// stride that stops matching the manifest is a bug this catches and a hand-built
// fixture never would.
func shipped(t *testing.T, id string) *spec.Manifest {
	t.Helper()
	cat := spec.NewCatalog(spec.EmbeddedSource(games.FS, "embedded games"))
	m, diags, err := cat.Load(id, spec.DenyOverrides{})
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	if diags.HasErrors() {
		t.Fatalf("load %s: %s", id, diags)
	}
	return m
}

func gameReqs(t *testing.T, id string) ([]Requirement, map[string]int) {
	t.Helper()
	m := shipped(t, id)
	reqs, err := m.Spec.PortRequirements()
	if err != nil {
		t.Fatalf("%s: PortRequirements: %v", id, err)
	}
	strides := map[string]int{}
	for _, r := range reqs {
		strides[r.Group] = m.Spec.Stride(r.Group)
	}
	return reqs, strides
}

// emptyHost is an occupancy with a known ephemeral range and nothing claimed,
// so a test's expectations are about the allocator and not about whatever the
// machine running the test happens to be listening on.
func emptyHost() *Occupancy {
	return NewOccupancy(platform.PortRange{Low: 32768, High: 60999})
}

func mustAllocate(t *testing.T, reqs []Requirement, strides map[string]int, occ *Occupancy, self string, pins ...Pin) Allocation {
	t.Helper()
	a, err := Allocate(reqs, strides, occ, self, pins)
	if err != nil {
		t.Fatalf("Allocate(%s): %v", self, err)
	}
	return a
}

func wantPorts(t *testing.T, got Allocation, want map[string]int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("allocation = %s, want %d ports", got, len(want))
	}
	for name, port := range want {
		if got[name] != port {
			t.Errorf("%s = %d, want %d (full allocation: %s)", name, got[name], port, got)
		}
	}
}

// ---------- the group/stride rule ----------

func TestEachGroupShiftsByItsOwnStride(t *testing.T) {
	// The rule from section 6.7, against the shipped Project Zomboid manifest:
	// pz has a stride of 10 and admin a stride of 1, so the second instance's
	// rcon port moves by one while its game ports move by ten. A single global
	// offset would put rcon on 27025, which is a number nobody asked for and a
	// firewall rule nobody wrote.
	reqs, strides := gameReqs(t, "projectzomboid")
	occ := emptyHost()

	first := mustAllocate(t, reqs, strides, occ, "pz-main")
	wantPorts(t, first, map[string]int{"game": 16261, "direct": 16262, "rcon": 27015})

	second := mustAllocate(t, reqs, strides, occ, "pz-eu")
	wantPorts(t, second, map[string]int{"game": 16271, "direct": 16272, "rcon": 27016})

	third := mustAllocate(t, reqs, strides, occ, "pz-us")
	wantPorts(t, third, map[string]int{"game": 16281, "direct": 16282, "rcon": 27017})
}

func TestAGroupMovesAsAUnitSoContiguitySurvives(t *testing.T) {
	// 7 Days to Die hardcodes ServerPort+1 and ServerPort+2 in the engine. If
	// the allocator moved `game` and left `steam1` where it was, the server
	// would start and the Steam query would answer for a different instance.
	reqs, strides := gameReqs(t, "7daystodie")
	occ := emptyHost()

	// Something else already holds the middle of the default window, so the
	// whole group has to move even though two of its three ports were free.
	occ.Add(Claim{Endpoint: Endpoint{UDP, 26901}, Desc: "a hand-run server", Source: SourceListener})

	got := mustAllocate(t, reqs, strides, occ, "sdtd-1")
	if got["game"] != 26910 || got["steam1"] != 26911 || got["steam2"] != 26912 {
		t.Fatalf("group did not move as a unit: %s", got)
	}
	if got["steam1"] != got["game"]+1 || got["steam2"] != got["game"]+2 {
		t.Errorf("contiguity broken: %s", got)
	}
}

func TestBothProtocolsAreClaimedForAProtoBothPort(t *testing.T) {
	// 7DTD's game port is `proto: both`. Claiming only one transport leaves the
	// other free for a second instance, and the collision surfaces as a bind
	// failure at start rather than as a refusal at create.
	reqs, strides := gameReqs(t, "7daystodie")
	occ := emptyHost()
	mustAllocate(t, reqs, strides, occ, "sdtd-1")

	for _, pr := range []Proto{TCP, UDP} {
		if occ.Free(Endpoint{pr, 26900}, "someone-else") {
			t.Errorf("%s/26900 is still free after a proto:both allocation", pr)
		}
	}
}

func TestTwoGroupsOfOneInstanceCannotTakeTheSamePort(t *testing.T) {
	// Claims made by this allocation are owned by `self`, and Free ignores
	// self-owned claims on purpose so that re-allocating an existing instance
	// keeps its ports. Without a separate record of what this call has already
	// placed, two groups whose defaults collide would both be handed the same
	// number.
	reqs := []Requirement{
		{Name: "a", Group: "ga", Default: 20000, Protos: []Proto{UDP}},
		{Name: "b", Group: "gb", Default: 20000, Protos: []Proto{UDP}},
	}
	got := mustAllocate(t, reqs, map[string]int{"ga": 1, "gb": 1}, emptyHost(), "x")
	if got["a"] == got["b"] {
		t.Fatalf("both groups got %d", got["a"])
	}
	if got["a"] != 20000 || got["b"] != 20001 {
		t.Errorf("allocation = %s, want a=20000 b=20001", got)
	}
}

func TestReallocatingAnInstanceKeepsItsOwnPorts(t *testing.T) {
	// `gvm apply` re-derives nothing, but an adoption or a repair may run the
	// allocator again against an occupancy that already contains this
	// instance's own ports. Treating its own claims as blockers would move a
	// live server every time.
	reqs, strides := gameReqs(t, "projectzomboid")
	occ := emptyHost()
	first := mustAllocate(t, reqs, strides, occ, "pz-main")
	again := mustAllocate(t, reqs, strides, occ, "pz-main")
	if !first.Equal(again) {
		t.Fatalf("second allocation moved the instance: %s then %s", first, again)
	}
}

// ---------- pins ----------

func TestAPinFixesItsGroupAtShiftZero(t *testing.T) {
	reqs, strides := gameReqs(t, "projectzomboid")
	occ := emptyHost()
	// An unrelated instance already sits on the default window, so an unpinned
	// group would shift to 16271.
	occ.AddInstance("pz-main", map[string]int{"game": 16261, "direct": 16262, "rcon": 27015}, reqs)

	// Adoption pins from the live .ini. The legacy server is on the defaults,
	// and the pinned group must stay there.
	_, err := Allocate(reqs, strides, occ, "pz-legacy", []Pin{{Name: "game", Port: 16261}})
	if err == nil {
		t.Fatal("a pin colliding with another instance's declared port must be refused")
	}
	var ex *ExhaustedError
	if !errors.As(err, &ex) {
		t.Fatalf("error = %T %v, want *ExhaustedError", err, err)
	}
	if !ex.Pinned {
		t.Error("the error does not record that the group was pinned")
	}
	if len(ex.Rejections) != 1 {
		t.Errorf("a pinned group has exactly one window; got %d rejections", len(ex.Rejections))
	}
	if !strings.Contains(err.Error(), "gvm@pz-main:game") {
		t.Errorf("the refusal does not name the holder:\n%s", err)
	}
	if !strings.Contains(err.Error(), "is pinned") {
		t.Errorf("the refusal does not say the group was pinned:\n%s", err)
	}
}

func TestAPinSurvivesTheServerItDescribes(t *testing.T) {
	// The point of adoption: the live server IS the foreign listener on the
	// pinned port, and it is very likely on the operator's reserved list too.
	// Refusing on either would make gvm decline to adopt exactly the servers it
	// exists to adopt.
	reqs, strides := gameReqs(t, "projectzomboid")
	occ := emptyHost()
	occ.AddListeners([]Listener{
		{Endpoint: Endpoint{UDP, 16261}, PID: 4412, Comm: "java"},
		{Endpoint: Endpoint{UDP, 16262}, PID: 4412, Comm: "java"},
		{Endpoint: Endpoint{TCP, 27015}, PID: 4412, Comm: "java"},
	})
	occ.AddReserved([]int{16261})

	got := mustAllocate(t, reqs, strides, occ, "pz-legacy",
		Pin{Name: "game", Port: 16261}, Pin{Name: "rcon", Port: 27015})
	wantPorts(t, got, map[string]int{"game": 16261, "direct": 16262, "rcon": 27015})
}

func TestAPinnedNonDefaultPortIsHonouredExactly(t *testing.T) {
	// Ports come from the live .ini, never from manifest defaults. A server
	// somebody moved to 16300 must be adopted at 16300.
	reqs, strides := gameReqs(t, "projectzomboid")
	got := mustAllocate(t, reqs, strides, emptyHost(), "pz-legacy",
		Pin{Name: "game", Port: 16300})
	if got["game"] != 16300 {
		t.Fatalf("game = %d, want the pinned 16300", got["game"])
	}
	// The rest of the pinned group stays at k=0 rather than following the pin:
	// the manifest says direct is a separate port, not game+1.
	if got["direct"] != 16262 {
		t.Errorf("direct = %d, want 16262", got["direct"])
	}
}

func TestAPinIntoTheEphemeralRangeIsAdoptedAndThenReported(t *testing.T) {
	// A live server bound at 40000 is a fact. Refusing to adopt it does not
	// move it; it only leaves it unmanaged. So the allocator takes the pin and
	// Check is where the operator is told.
	reqs, strides := gameReqs(t, "projectzomboid")
	occ := emptyHost()
	got := mustAllocate(t, reqs, strides, occ, "pz-legacy", Pin{Name: "game", Port: 40000})
	if got["game"] != 40000 {
		t.Fatalf("game = %d, want the pinned 40000", got["game"])
	}
	var found bool
	for _, c := range Check("pz-legacy", reqs, got, occ) {
		if c.Kind == ConflictEphemeral && c.Name == "game" {
			found = true
		}
	}
	if !found {
		t.Error("Check did not report the adopted port as being in the ephemeral range")
	}
}

func TestAPinMustNameADeclaredPort(t *testing.T) {
	reqs, strides := gameReqs(t, "projectzomboid")
	_, err := Allocate(reqs, strides, emptyHost(), "pz", []Pin{{Name: "querry", Port: 16261}})
	if !errors.Is(err, ErrUnknownPin) {
		t.Fatalf("error = %v, want ErrUnknownPin", err)
	}
	// The message lists what the manifest does declare, because "querry" is a
	// typo for one of them and the operator is looking at a config file.
	if !strings.Contains(err.Error(), `"rcon"`) {
		t.Errorf("the error does not list the declared names: %v", err)
	}
}

func TestAPinOutsideThePortSpaceIsRefused(t *testing.T) {
	reqs, strides := gameReqs(t, "projectzomboid")
	for _, port := range []int{0, -1, 65536} {
		if _, err := Allocate(reqs, strides, emptyHost(), "pz", []Pin{{Name: "game", Port: port}}); !errors.Is(err, ErrPinOutOfRange) {
			t.Errorf("pin %d: error = %v, want ErrPinOutOfRange", port, err)
		}
	}
}

func TestContradictoryPinsAreRefused(t *testing.T) {
	reqs, strides := gameReqs(t, "projectzomboid")
	_, err := Allocate(reqs, strides, emptyHost(), "pz",
		[]Pin{{Name: "game", Port: 16261}, {Name: "game", Port: 16300}})
	if !errors.Is(err, ErrPinConflict) {
		t.Fatalf("error = %v, want ErrPinConflict", err)
	}
}

// ---------- the ephemeral range ----------

func TestTheEphemeralRangeIsNeverAllocatedInto(t *testing.T) {
	// The one everybody forgets. A listener bound inside ip_local_port_range
	// works until the day an outbound connection grabs the port first, and then
	// the unit fails to start with nothing in the diff to explain it.
	eph := platform.PortRange{Low: 32768, High: 60999}
	reqs := []Requirement{{Name: "game", Group: "g", Default: 32700, Protos: []Proto{UDP}}}
	occ := NewOccupancy(eph)

	// Stride 1 from 32700: shifts 0..67 are below the range, 68..(68+28231) are
	// inside it, and the search gives up at 200 without ever proposing one.
	for i := 0; i < 80; i++ {
		got, err := Allocate(reqs, map[string]int{"g": 1}, occ, fmt.Sprintf("i%d", i), nil)
		if err != nil {
			// Once every port below the range is taken there is nowhere left
			// to go, and refusing is the correct answer.
			if !errors.Is(err, ErrExhausted) {
				t.Fatalf("i%d: %v", i, err)
			}
			if !strings.Contains(err.Error(), "ephemeral range 32768-60999") {
				t.Errorf("the exhaustion report does not name the ephemeral range:\n%s", err)
			}
			return
		}
		if eph.Contains(got["game"]) {
			t.Fatalf("i%d was allocated %d, which is inside the ephemeral range %s", i, got["game"], eph)
		}
	}
	t.Fatalf("the allocator never ran out below the ephemeral range; the guard is not being reached")
}

func TestAnUnreadableEphemeralRangeFallsBackRatherThanBeingEmpty(t *testing.T) {
	// Treating the range as empty is the failure section 6.7 singles out. The
	// zero PortRange must therefore mean "substitute the kernel default", not
	// "allocate anywhere".
	occ := NewOccupancy(platform.PortRange{})
	if !occ.EphemeralAssumed() {
		t.Error("the fallback was not recorded, so nothing can report it")
	}
	if occ.Ephemeral() != platform.DefaultEphemeral {
		t.Errorf("ephemeral = %s, want the kernel default %s", occ.Ephemeral(), platform.DefaultEphemeral)
	}
	if !occ.InEphemeral(40000) {
		t.Error("40000 is inside the kernel default range and must be refused")
	}
}

func TestAPortIsNeverShiftedIntoPrivilegedSpace(t *testing.T) {
	// A negative stride is refused upstream, but the guard is cheap and the
	// failure it prevents -- a unit that starts and then cannot bind -- is
	// invisible until first start.
	reqs := []Requirement{{Name: "web", Group: "g", Default: 1030, Protos: []Proto{TCP}}}
	occ := emptyHost()
	occ.Add(Claim{Endpoint: Endpoint{TCP, 1030}, Owner: "other", Desc: "gvm@other:web", Source: SourceInstance})
	// Stride 0 is normalised to 1, so this shifts upward and succeeds; the
	// interesting half is that a manifest default already below 1024 is left
	// alone.
	got := mustAllocate(t, reqs, map[string]int{"g": 0}, occ, "x")
	if got["web"] != 1031 {
		t.Fatalf("web = %d, want 1031", got["web"])
	}

	low := []Requirement{{Name: "https", Group: "g", Default: 443, Protos: []Proto{TCP}}}
	got = mustAllocate(t, low, map[string]int{"g": 1}, emptyHost(), "edge")
	if got["https"] != 443 {
		t.Errorf("a manifest that asks for 443 gets 443, not %d", got["https"])
	}
}

// ---------- exhaustion ----------

func TestExhaustionNamesTheBlockingClaimForEveryRejectedWindow(t *testing.T) {
	// An allocator that says "no ports free" without saying who holds them is
	// unusable at two in the morning. Every window it looked at and turned down
	// has to appear, with the claim that turned it down.
	reqs, strides := gameReqs(t, "projectzomboid")
	occ := emptyHost()

	// Fill every window the pz group could reach with a different, nameable
	// holder, so a report that collapsed them would be obviously wrong.
	for k := 0; k <= MaxShift; k++ {
		port := 16261 + k*10
		switch k % 3 {
		case 0:
			occ.Add(Claim{Endpoint: Endpoint{UDP, port}, Owner: fmt.Sprintf("pz-%d", k),
				Desc: fmt.Sprintf("gvm@pz-%d:game", k), Source: SourceInstance})
		case 1:
			occ.AddListeners([]Listener{{Endpoint: Endpoint{UDP, port}, PID: 4412 + k, Comm: "python3"}})
		default:
			occ.AddReserved([]int{port})
		}
	}

	_, err := Allocate(reqs, strides, occ, "pz-new", nil)
	if err == nil {
		t.Fatal("want exhaustion")
	}
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("error = %v, want one wrapping ErrExhausted", err)
	}
	var ex *ExhaustedError
	if !errors.As(err, &ex) {
		t.Fatalf("error = %T, want *ExhaustedError", err)
	}
	if ex.Group != "pz" || ex.Stride != 10 || ex.Start != 16261 {
		t.Errorf("error names group=%q stride=%d start=%d", ex.Group, ex.Stride, ex.Start)
	}
	if len(ex.Rejections) != MaxShift+1 {
		t.Fatalf("%d rejections recorded, want one per window (%d)", len(ex.Rejections), MaxShift+1)
	}

	msg := err.Error()
	head := "ports: no free window for group \"pz\" after 200 shifts of 10 starting at 16261:"
	if !strings.HasPrefix(msg, head) {
		t.Errorf("first line = %q, want %q", strings.SplitN(msg, "\n", 2)[0], head)
	}
	// Every window, by number, with something that names its holder.
	for k, r := range ex.Rejections {
		if r.Shift != k {
			t.Fatalf("rejection %d records shift %d", k, r.Shift)
		}
		port := 16261 + k*10
		if r.Number != port {
			t.Errorf("rejection %d is for port %d, want %d", k, r.Number, port)
		}
		if len(r.Claims) == 0 {
			t.Errorf("rejection %d names no claim: %s", k, r)
		}
		line := r.String()
		if !strings.Contains(msg, line) {
			t.Errorf("the message omits window %d:\n%s", k, line)
		}
	}
	// And the three kinds of holder are each named in their own words.
	for _, want := range []string{
		"udp/16261 (game) held by gvm@pz-0:game",
		"udp/16271 (game) held by a foreign listener (pid 4413 python3)",
		"udp/16281 (game) held by reserved by the operator (gvm.yaml reserved_ports)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the report does not contain %q:\n%s", want, msg)
		}
	}
}

func TestAFailedAllocationLeavesTheOccupancyUntouched(t *testing.T) {
	// Otherwise a `gvm create` that ran out of ports leaves half a group's
	// worth of phantom claims behind for the next one to trip over.
	reqs := []Requirement{
		{Name: "a", Group: "ga", Default: 30000, Protos: []Proto{UDP}},
		{Name: "b", Group: "gb", Default: 31000, Protos: []Proto{UDP}},
	}
	occ := emptyHost()
	for k := 0; k <= MaxShift; k++ {
		occ.Add(Claim{Endpoint: Endpoint{UDP, 31000 + k}, Owner: "z", Desc: "gvm@z:b", Source: SourceInstance})
	}
	before := occ.Len()

	if _, err := Allocate(reqs, map[string]int{"ga": 1, "gb": 1}, occ, "x", nil); err == nil {
		t.Fatal("want exhaustion on the second group")
	}
	if occ.Len() != before {
		t.Errorf("occupancy grew from %d to %d claims on a failed allocation", before, occ.Len())
	}
	if !occ.Free(Endpoint{UDP, 30000}, "someone") {
		t.Error("the first group's claim was committed even though the allocation failed")
	}
}

// ---------- argument handling ----------

func TestAllocateRefusesAnEmptySelf(t *testing.T) {
	// With an empty name every unattributed claim on the host -- every foreign
	// listener -- would compare equal to `self` and be ignored.
	reqs, strides := gameReqs(t, "projectzomboid")
	if _, err := Allocate(reqs, strides, emptyHost(), "", nil); err == nil {
		t.Fatal("want a refusal for an empty instance name")
	}
}

func TestAllocateRefusesANilOccupancy(t *testing.T) {
	reqs, strides := gameReqs(t, "projectzomboid")
	if _, err := Allocate(reqs, strides, nil, "pz", nil); err == nil {
		t.Fatal("a nil occupancy is not an empty host, it is an unread one")
	}
}

func TestAllocateRefusesDuplicateRequirementNames(t *testing.T) {
	reqs := []Requirement{
		{Name: "game", Group: "g", Default: 100, Protos: []Proto{UDP}},
		{Name: "game", Group: "g", Default: 200, Protos: []Proto{UDP}},
	}
	if _, err := Allocate(reqs, nil, emptyHost(), "x", nil); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("error = %v, want ErrDuplicateName", err)
	}
}

func TestAllocateWithNoRequirementsIsAnEmptyAllocation(t *testing.T) {
	// Valheim declares no config files; a game with no ports is the same shape
	// of empty case and must not be an error.
	got, err := Allocate(nil, nil, emptyHost(), "x", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("Allocate(nil) = %v, %v", got, err)
	}
}

func TestGroupsArePlacedInManifestOrder(t *testing.T) {
	// The order a manifest writes its ports in is the order they are placed, so
	// two runs against the same host produce the same numbers and a re-created
	// instance lands where the last one did.
	reqs, strides := gameReqs(t, "valheim")
	a := mustAllocate(t, reqs, strides, emptyHost(), "vh-1")
	b := mustAllocate(t, reqs, strides, emptyHost(), "vh-1")
	if !a.Equal(b) {
		t.Fatalf("two runs disagree: %s vs %s", a, b)
	}
	wantPorts(t, a, map[string]int{"game": 2456, "query": 2457, "spare": 2458})
}
