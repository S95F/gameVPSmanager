package spec

import (
	"strings"
	"testing"
)

func TestLoadHostMergesOntoDefaults(t *testing.T) {
	// A file that sets one section keeps every other default.
	data := []byte("apiVersion: gvm/v1\nkind: Host\nspec:\n  firewall: {}\n")
	h, diags := LoadHost("gvm.yaml", data)
	if diags.HasErrors() {
		t.Fatalf("clean config reported errors: %s", diags)
	}
	if h.Spec.Firewall.Backend != "ufw" {
		t.Errorf("firewall backend = %q, want the default ufw", h.Spec.Firewall.Backend)
	}
	if h.Spec.Edge.Mode != "none" {
		t.Errorf("edge mode = %q, want the default none", h.Spec.Edge.Mode)
	}
	if !h.Spec.Edge.HTTP3 {
		t.Error("http3 default (true) was lost by the merge")
	}
}

func TestLoadHostUnknownKeyDidYouMean(t *testing.T) {
	data := []byte("spec:\n  firewall:\n    reserved_port: [8080]\n")
	_, diags := LoadHost("gvm.yaml", data)
	if !diags.HasErrors() {
		t.Fatal("a typo'd key should be an error")
	}
	got := diags.String()
	if !strings.Contains(got, "unknown key reserved_port") {
		t.Errorf("no unknown-key error:\n%s", got)
	}
	if !strings.Contains(got, "did you mean reserved_ports?") {
		t.Errorf("no did-you-mean for reserved_port:\n%s", got)
	}
}

func TestValidateHostEnumsCIDRsPorts(t *testing.T) {
	h := DefaultHostConfig()
	h.Spec.Firewall.Backend = "iptables"
	h.Spec.Edge.Mode = "nginx"
	h.Spec.Edge.TLS.Mode = "letsencrypt"
	h.Spec.Firewall.AdminCIDRs = []string{"10.0.0.0/8", "not-a-cidr"}
	h.Spec.Firewall.ReservedPorts = []int{8080, 70000}

	ds := ValidateHost("gvm.yaml", &h)
	got := ds.String()
	for _, want := range []string{
		"is not a firewall backend",
		"is not an edge mode",
		"is not a tls mode",
		"is not a CIDR",
		"is not a port",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ValidateHost missing %q:\n%s", want, got)
		}
	}
	if !ds.HasErrors() {
		t.Error("validation findings should be errors")
	}
}

// A valid config validates clean — the default must never fail its own checks.
func TestValidateHostAcceptsDefault(t *testing.T) {
	h := DefaultHostConfig()
	if ds := ValidateHost("gvm.yaml", &h); ds.HasErrors() {
		t.Errorf("the default host config does not validate:\n%s", ds)
	}
}
