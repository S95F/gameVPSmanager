package edge

import (
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

func TestComputePlanModeNone(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "none"
	plan, err := ComputePlan(&host, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "none" {
		t.Errorf("expected mode none, got %q", plan.Mode)
	}
	if len(plan.Routes) != 0 {
		t.Errorf("mode none should produce no routes")
	}
}

func TestComputePlanProxyExternalRequiresTrustedOrigins(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "proxy_external"
	host.Spec.Edge.TrustedOrigins = nil
	_, err := ComputePlan(&host, nil)
	if err == nil {
		t.Error("expected error when proxy_external has no trusted_origins")
	}
}

func TestComputePlanProxyExternalWithOrigins(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "proxy_external"
	host.Spec.Edge.TrustedOrigins = []string{"https://example.com"}
	plan, err := ComputePlan(&host, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "proxy_external" {
		t.Errorf("expected proxy_external, got %q", plan.Mode)
	}
}

func TestComputePlanCaddyManagedSkipsDisabled(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "caddy_managed"
	host.Spec.Edge.Site = "example.com"

	instances := []spec.Instance{
		{
			Metadata: spec.InstanceMeta{Name: "pz1"},
			Spec: spec.InstanceSpec{
				Enabled: true,
				Paths:   spec.Paths{Home: "/srv/gvm/pz1"},
			},
		},
		{
			Metadata: spec.InstanceMeta{Name: "pz2"},
			Spec: spec.InstanceSpec{
				Enabled: false,
				Paths:   spec.Paths{Home: "/srv/gvm/pz2"},
			},
		},
	}

	plan, err := ComputePlan(&host, instances)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 1 {
		t.Errorf("expected 1 route (disabled skipped), got %d", len(plan.Routes))
	}
	if plan.Routes[0].Instance != "pz1" {
		t.Errorf("expected pz1 route, got %q", plan.Routes[0].Instance)
	}
}

func TestComputePlanRouteHostname(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "caddy_managed"
	host.Spec.Edge.Site = "example.com"

	instances := []spec.Instance{
		{
			Metadata: spec.InstanceMeta{Name: "pz1"},
			Spec: spec.InstanceSpec{
				Enabled: true,
				Paths:   spec.Paths{Home: "/srv/gvm/pz1"},
			},
		},
	}

	plan, err := ComputePlan(&host, instances)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(plan.Routes))
	}
	want := "pz1.example.com"
	if plan.Routes[0].Host != want {
		t.Errorf("route host: want %q, got %q", want, plan.Routes[0].Host)
	}
}
