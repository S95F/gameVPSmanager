package firewall

import (
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

func TestDesiredSSHFirst(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Firewall.SSH.Ports = []int{22}
	host.Spec.Firewall.SSH.RateLimit = true

	rules := Desired(&host, nil, nil)
	if len(rules) == 0 {
		t.Fatal("expected at least one rule")
	}
	if rules[0].Port != 22 {
		t.Errorf("first rule must be SSH, got port %d", rules[0].Port)
	}
	if rules[0].Verb != VerbLimit {
		t.Errorf("SSH with RateLimit=true must be LIMIT, got %v", rules[0].Verb)
	}
}

func TestDesiredCaddyEdgeOpens80And443(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Firewall.SSH.Ports = []int{22}
	host.Spec.Edge.Mode = "caddy_managed"
	host.Spec.Edge.HTTP3 = true

	rules := Desired(&host, nil, nil)
	ports := map[int]bool{}
	protos := map[int]string{}
	for _, r := range rules {
		ports[r.Port] = true
		protos[r.Port] = string(r.Proto)
	}
	if !ports[80] {
		t.Error("caddy_managed must open 80/tcp")
	}
	if !ports[443] {
		t.Error("caddy_managed must open 443/tcp")
	}
	// HTTP/3 means 443/udp as well.
	foundUDP443 := false
	for _, r := range rules {
		if r.Port == 443 && r.Proto == ProtoUDP {
			foundUDP443 = true
		}
	}
	if !foundUDP443 {
		t.Error("http3:true must open 443/udp")
	}
}

func TestDesiredNoHTTP3NoUDPRule(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Firewall.SSH.Ports = []int{22}
	host.Spec.Edge.Mode = "caddy_managed"
	host.Spec.Edge.HTTP3 = false

	rules := Desired(&host, nil, nil)
	for _, r := range rules {
		if r.Port == 443 && r.Proto == ProtoUDP {
			t.Error("http3:false must NOT open 443/udp")
		}
	}
}

func TestDesiredDisabledInstanceGetsNoRules(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Firewall.SSH.Ports = []int{22}
	host.Spec.Edge.Mode = "none"

	inst := spec.Instance{
		Metadata: spec.InstanceMeta{Name: "pz1"},
		Spec: spec.InstanceSpec{
			Game:    "pz",
			Enabled: false,
			Ports:   map[string]int{"game": 16261},
		},
	}

	manifest := &spec.Manifest{
		Spec: spec.GameSpec{
			Ports: []spec.PortSpec{
				{Name: "game", Proto: "udp", Expose: spec.ExposePublic},
			},
		},
	}

	rules := Desired(&host, []spec.Instance{inst}, map[string]*spec.Manifest{"pz": manifest})
	for _, r := range rules {
		if r.Port == 16261 {
			t.Error("disabled instance must not generate any firewall rules")
		}
	}
}

func TestDesiredAdminPortOnlyAdmitsCIDRs(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Firewall.SSH.Ports = []int{22}
	host.Spec.Firewall.AdminCIDRs = []string{"10.0.0.0/8"}
	host.Spec.Edge.Mode = "none"

	inst := spec.Instance{
		Metadata: spec.InstanceMeta{Name: "pz1"},
		Spec: spec.InstanceSpec{
			Game:    "pz",
			Enabled: true,
			Ports:   map[string]int{"rcon": 27015},
		},
	}

	manifest := &spec.Manifest{
		Spec: spec.GameSpec{
			Ports: []spec.PortSpec{
				{Name: "rcon", Proto: "tcp", Expose: spec.ExposeAdmin},
			},
		},
	}

	rules := Desired(&host, []spec.Instance{inst}, map[string]*spec.Manifest{"pz": manifest})
	for _, r := range rules {
		if r.Port == 27015 {
			if r.From == "" {
				t.Error("admin port rule must have a From CIDR, not open to everyone")
			}
		}
	}
}

func TestDesiredTagsHaveGVMPrefix(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Firewall.SSH.Ports = []int{22}
	host.Spec.Edge.Mode = "caddy_managed"

	rules := Desired(&host, nil, nil)
	for _, r := range rules {
		if r.Comment == "" {
			t.Errorf("rule %+v has empty comment (must have gvm tag)", r)
			continue
		}
		if _, ok := ParseTag(r.Comment); !ok {
			t.Errorf("rule comment not a valid gvm tag: %q", r.Comment)
		}
	}
}
