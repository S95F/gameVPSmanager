package spec

import (
	"reflect"
	"testing"
)

func TestParseProtos(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    []Proto
		wantErr bool
	}{
		{in: "tcp", want: []Proto{ProtoTCP}},
		{in: "udp", want: []Proto{ProtoUDP}},
		{in: "both", want: []Proto{ProtoTCP, ProtoUDP}},

		// No default. Half these games are UDP and half are TCP, so a guess
		// produces a firewall rule for the wrong transport, which reads as
		// "the server is up but nobody can join".
		{in: "", wantErr: true},
		{in: "TCP", wantErr: true},
		{in: "sctp", wantErr: true},
	} {
		got, err := ParseProtos(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseProtos(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseProtos(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// pzPorts is the Project Zomboid layout of section 6.7: two game ports that must
// stay contiguous, and an RCON port on its own counter so `admin` shifts by 1
// while `pz` shifts by 10.
func pzPorts() GameSpec {
	return GameSpec{
		PortStride: map[string]int{"pz": 10, "admin": 1},
		Ports: []PortSpec{
			{Name: "game", Proto: "udp", Default: 16261, Group: "pz", Expose: ExposePublic,
				ConfigKey: "DefaultPort", ConfigFile: "server_ini"},
			{Name: "direct", Proto: "udp", Default: 16262, Group: "pz", Expose: ExposePublic,
				ConfigKey: "UDPPort", ConfigFile: "server_ini"},
			{Name: "rcon", Proto: "tcp", Default: 27015, Group: "admin", Expose: ExposeNone,
				Bind: "127.0.0.1", ConfigKey: "RCONPort"},
		},
		Configs: []ConfigFile{{Name: "server_ini"}},
	}
}

func TestPortRequirementsFlattenTheManifestForTheAllocator(t *testing.T) {
	reqs, err := pzPorts().PortRequirements()
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 3 {
		t.Fatalf("got %d requirements, want 3", len(reqs))
	}
	if got := GroupOrder(reqs); !reflect.DeepEqual(got, []string{"pz", "admin"}) {
		t.Errorf("group order = %v, want [pz admin]: groups are placed in the order the file writes them", got)
	}
	if got := MembersOf(reqs, "pz"); len(got) != 2 || got[0].Name != "game" || got[1].Name != "direct" {
		t.Errorf("MembersOf(pz) = %v, want game then direct", got)
	}
	if got := reqs[2].Protos; !reflect.DeepEqual(got, []Proto{ProtoTCP}) {
		t.Errorf("rcon protos = %v, want [tcp]", got)
	}
}

// TestAPortWithNoGroupShiftsAlone is the reading of "no contiguity requirement
// stated". Lumping every ungrouped port into one anonymous group would force
// unrelated ports to move together, so one collision would move all of them.
func TestAPortWithNoGroupShiftsAlone(t *testing.T) {
	g := GameSpec{Ports: []PortSpec{
		{Name: "game", Proto: "udp", Default: 27015},
		{Name: "relay", Proto: "udp", Default: 27016},
	}}
	reqs, err := g.PortRequirements()
	if err != nil {
		t.Fatal(err)
	}
	if got := GroupOrder(reqs); !reflect.DeepEqual(got, []string{"game", "relay"}) {
		t.Errorf("group order = %v, want each port in its own group", got)
	}
}

func TestStrideDefaultsToOneSoTheAllocatorCannotDisagree(t *testing.T) {
	g := pzPorts()
	if got := g.Stride("pz"); got != 10 {
		t.Errorf("Stride(pz) = %d, want 10", got)
	}
	// The allocator does `if stride <= 0 { stride = 1 }`; this must match, or a
	// group with a nonsense stride would be placed differently by the two.
	if got := g.Stride("nosuch"); got != 1 {
		t.Errorf("Stride(nosuch) = %d, want 1", got)
	}
	g.PortStride["broken"] = 0
	if got := g.Stride("broken"); got != 1 {
		t.Errorf("Stride(broken) = %d, want 1", got)
	}
}

func TestBothFlattensToTwoEndpoints(t *testing.T) {
	g := GameSpec{Ports: []PortSpec{{Name: "q", Proto: "both", Default: 27015}}}
	reqs, err := g.PortRequirements()
	if err != nil {
		t.Fatal(err)
	}
	// tcp/27015 and udp/27015 are distinct endpoints, and a `both` port has to
	// claim and open BOTH or half of it silently does not work.
	if !reflect.DeepEqual(reqs[0].Protos, []Proto{ProtoTCP, ProtoUDP}) {
		t.Errorf("got %v, want [tcp udp]", reqs[0].Protos)
	}
}

func TestPortRequirementsRefuseAPortWithNoProtocol(t *testing.T) {
	g := GameSpec{Ports: []PortSpec{{Name: "game", Default: 27015}}}
	if _, err := g.PortRequirements(); err == nil {
		t.Fatal("a port with no protocol must not flatten into something the allocator will claim")
	}
}

// TestConfigKeyPortsResolvesTheDefaultFile is what check 6 uses to refuse a key
// claimed by both a port and a config_files[].keys entry. `config_file` is
// optional and defaults to the first file, so the collision has to be detected
// through that default too.
func TestConfigKeyPortsResolvesTheDefaultFile(t *testing.T) {
	got := pzPorts().ConfigKeyPorts()
	byFile, ok := got["server_ini"]
	if !ok {
		t.Fatalf("got %v, want a server_ini entry", got)
	}
	for key, want := range map[string]string{
		"DefaultPort": "game",
		"UDPPort":     "direct",
		"RCONPort":    "rcon", // no config_file: falls back to the first one
	} {
		if byFile[key] != want {
			t.Errorf("server_ini[%s] = %q, want %q", key, byFile[key], want)
		}
	}
}

func TestPortByName(t *testing.T) {
	g := pzPorts()
	if p, ok := g.PortByName("rcon"); !ok || p.Default != 27015 {
		t.Errorf("PortByName(rcon) = %+v, %v", p, ok)
	}
	if _, ok := g.PortByName("nosuch"); ok {
		t.Error("PortByName invented a port")
	}
}
