package edge

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

func TestComputePlanModeNone(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "none"
	plan, err := ComputePlan(&host)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "none" {
		t.Errorf("expected mode none, got %q", plan.Mode)
	}
}

func TestComputePlanProxyExternalRequiresTrustedOrigins(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "proxy_external"
	host.Spec.Edge.TrustedOrigins = nil
	_, err := ComputePlan(&host)
	if err == nil {
		t.Error("expected error when proxy_external has no trusted_origins")
	}
}

func TestComputePlanProxyExternalWithOrigins(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "proxy_external"
	host.Spec.Edge.TrustedOrigins = []string{"https://gvm.example.com"}
	plan, err := ComputePlan(&host)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "proxy_external" {
		t.Errorf("expected proxy_external, got %q", plan.Mode)
	}
}

// TestComputePlanOneVhostToDashboardSocket: the plan is ONE vhost for
// edge.site proxying to the dashboard socket — never a route per instance
// (web-dashboard-11).
func TestComputePlanOneVhostToDashboardSocket(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "caddy_managed"
	host.Spec.Edge.Site = "gvm.example.com"

	plan, err := ComputePlan(&host)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Site != "gvm.example.com" {
		t.Errorf("site = %q", plan.Site)
	}
	if plan.Upstream.Network != "unix" || plan.Upstream.Address != "/run/gvm/web/dashboard.sock" {
		t.Errorf("upstream = %+v, want the dashboard unix socket", plan.Upstream)
	}
	if strings.Contains(plan.Upstream.Address, "unix/") {
		t.Errorf("upstream address %q carries a unix/ prefix; the renderer adds it exactly once", plan.Upstream.Address)
	}
}

// TestComputePlanListenBecomesTCPUpstream: edge.listen switches the upstream
// to TCP — gvm-web on a loopback port behind a foreign proxy (e.g. a
// serverGitUpdater-managed Caddy whose snippets proxy to 127.0.0.1:<port>).
func TestComputePlanListenBecomesTCPUpstream(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "caddy_snippet"
	host.Spec.Edge.Site = "gvm.example.com"
	host.Spec.Edge.Listen = "127.0.0.1:8130"

	plan, err := ComputePlan(&host)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Upstream.Network != "tcp" || plan.Upstream.Address != "127.0.0.1:8130" {
		t.Errorf("upstream = %+v, want tcp 127.0.0.1:8130", plan.Upstream)
	}
}

func TestComputePlanSiteRequired(t *testing.T) {
	for _, mode := range []string{"caddy_managed", "caddy_snippet"} {
		host := spec.DefaultHostConfig()
		host.Spec.Edge.Mode = mode
		host.Spec.Edge.Site = ""
		if _, err := ComputePlan(&host); err == nil {
			t.Errorf("mode %s with no site: expected error", mode)
		}
	}
}

// TestTLSAutoResolution pins §17.4's decision tree: auto never leaves
// ComputePlan.
func TestTLSAutoResolution(t *testing.T) {
	cases := []struct {
		name     string
		site     string
		provider string
		want     TLSPolicy
	}{
		{"bare IP → internal", "203.0.113.7", "", TLSInternal},
		{"DNS name → acme_http", "gvm.example.com", "", TLSAcmeHTTP},
		{"DNS name + provider → acme_dns", "gvm.example.com", "cloudflare", TLSAcmeDNS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := spec.DefaultHostConfig()
			host.Spec.Edge.Mode = "caddy_managed"
			host.Spec.Edge.Site = tc.site
			host.Spec.Edge.TLS.Mode = "auto"
			host.Spec.Edge.TLS.DNSProvider = tc.provider
			plan, err := ComputePlan(&host)
			if err != nil {
				t.Fatal(err)
			}
			if plan.TLS != tc.want {
				t.Errorf("resolved TLS = %q, want %q", plan.TLS, tc.want)
			}
		})
	}
}

func TestTLSAcmeDNSRequiresProvider(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "caddy_managed"
	host.Spec.Edge.Site = "gvm.example.com"
	host.Spec.Edge.TLS.Mode = "acme_dns"
	if _, err := ComputePlan(&host); err == nil {
		t.Error("acme_dns with no dns_provider: expected error")
	}
}

func TestTLSExternalRefusedWithCaddyManaged(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "caddy_managed"
	host.Spec.Edge.Site = "gvm.example.com"
	host.Spec.Edge.TLS.Mode = "external"
	if _, err := ComputePlan(&host); err == nil {
		t.Error("external TLS under caddy_managed: expected error")
	}
}

// TestHSTSOnlyForPublicDNSWithRealTLS: HSTS is a one-way door per hostname;
// it must never be emitted for a bare IP or tls internal even when opted in.
func TestHSTSOnlyForPublicDNSWithRealTLS(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "caddy_managed"
	host.Spec.Edge.HSTS = true

	host.Spec.Edge.Site = "203.0.113.7"
	plan, err := ComputePlan(&host)
	if err != nil {
		t.Fatal(err)
	}
	if plan.HSTS {
		t.Error("HSTS emitted for a bare IP (RFC 6797 does not apply)")
	}

	host.Spec.Edge.Site = "gvm.example.com"
	plan, err = ComputePlan(&host)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.HSTS {
		t.Error("HSTS opted in for a public DNS name with ACME TLS, but not planned")
	}
}
