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

// TestValidateHostRejectsNftBackend is Phase 40 item 2's other lock: the nft
// backend is off, so a host config that asks for it is refused at validation,
// not left to fail later when nft.Available returns false. ufw and none stay
// accepted.
func TestValidateHostRejectsNftBackend(t *testing.T) {
	h := DefaultHostConfig()
	h.Spec.Firewall.Backend = "nft"
	ds := ValidateHost("gvm.yaml", &h)
	if !ds.HasErrors() {
		t.Fatalf("backend: nft validated clean:\n%s", ds.String())
	}
	if got := ds.String(); !strings.Contains(got, "is not a firewall backend") {
		t.Errorf("the refusal does not name the backend problem:\n%s", got)
	}

	for _, ok := range []string{"ufw", "none", ""} {
		h := DefaultHostConfig()
		h.Spec.Firewall.Backend = ok
		if ds := ValidateHost("gvm.yaml", &h); ds.HasErrors() {
			t.Errorf("backend %q was rejected but is supported:\n%s", ok, ds.String())
		}
	}
}

// A valid config validates clean — the default must never fail its own checks.
func TestValidateHostAcceptsDefault(t *testing.T) {
	h := DefaultHostConfig()
	if ds := ValidateHost("gvm.yaml", &h); ds.HasErrors() {
		t.Errorf("the default host config does not validate:\n%s", ds)
	}
}
