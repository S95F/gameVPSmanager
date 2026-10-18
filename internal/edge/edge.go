// Package edge computes the reverse-proxy plan from the host spec. It is
// pure: no I/O, no database, no network — so it unit-tests on Windows and the
// proxy config survives Postgres being down (§17).
//
// Routing is essentially static (§17.2): ONE vhost for edge.site, proxying to
// the dashboard's listener, with all per-instance routing done inside gvm-web
// at /i/{name}/…. Caddy config changes only when the operator changes a
// hostname, TLS mode or public endpoint — a rare, auditable event, and the
// reason instance creation never drops an operator's console stream.
package edge

import (
	"fmt"
	"net"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Upstream is the dashboard listener the proxy forwards to.
type Upstream struct {
	// Network is "unix" (the default deployment, D-06) or "tcp"
	// (edge.listen — e.g. gvm-web on 127.0.0.1:8130 behind a foreign proxy
	// or a serverGitUpdater-managed Caddy).
	Network string
	// Address is the socket path or the host:port.
	Address string
}

// TLSPolicy is the RESOLVED TLS mode for the vhost — "auto" never leaves
// ComputePlan (§17.4).
type TLSPolicy string

const (
	TLSAcmeHTTP TLSPolicy = "acme_http" // HTTP-01 + TLS-ALPN-01 (Let's Encrypt)
	TLSAcmeDNS  TLSPolicy = "acme_dns"  // DNS-01; needs caddy-dns/<provider>
	TLSInternal TLSPolicy = "internal"  // Caddy's internal CA (bare IP, no public DNS)
	TLSExternal TLSPolicy = "external"  // another proxy terminates TLS
)

// Plan is the computed edge plan for the host.
type Plan struct {
	Mode string // caddy_managed | caddy_snippet | proxy_external | none

	// The fields below are set only for the caddy modes.
	Site     string
	Upstream Upstream
	TLS      TLSPolicy
	// DNSProvider and DNSEnvVar configure the acme_dns challenge: the
	// caddy-dns module name, and the environment variable carrying its
	// credential (rendered as {env.<VAR>}, so the secret never lands in the
	// Caddyfile). DNSEnvVar is set when tls.dns_env names exactly one
	// variable; a multi-variable provider configures itself from its own
	// environment.
	DNSProvider  string
	DNSEnvVar    string
	Email        string
	HTTP3        bool
	HTTPRedirect bool
	// HSTS is true only when the operator opted in AND the site is a public
	// DNS name with real TLS — never for `tls internal` or a bare IP, where
	// RFC 6797 does not apply (§17.1).
	HSTS bool
}

// ComputePlan derives the edge plan from the host configuration. It is
// deterministic and has no side effects.
func ComputePlan(host *spec.HostConfig) (*Plan, error) {
	e := host.Spec.Edge

	switch e.Mode {
	case "", "none":
		return &Plan{Mode: "none"}, nil

	case "proxy_external":
		// gvm renders nothing for a foreign proxy; the plan records the
		// listener the operator must point it at. Without trusted origins,
		// a proxy that rewrites Host breaks every form POST (§17.3).
		if len(e.TrustedOrigins) == 0 {
			return nil, fmt.Errorf("edge: proxy_external requires at least one trusted_origins entry, " +
				"or cross-origin protection refuses every form POST behind a proxy that rewrites Host")
		}
		return &Plan{Mode: "proxy_external", Upstream: upstreamFrom(e)}, nil

	case "caddy_managed", "caddy_snippet":
		if e.Site == "" {
			return nil, fmt.Errorf("edge: mode %s requires edge.site (a domain or a bare IP)", e.Mode)
		}
		tls, err := resolveTLS(e)
		if err != nil {
			return nil, err
		}
		siteIsIP := net.ParseIP(e.Site) != nil
		return &Plan{
			Mode:         e.Mode,
			Site:         e.Site,
			Upstream:     upstreamFrom(e),
			TLS:          tls,
			DNSProvider:  e.TLS.DNSProvider,
			DNSEnvVar:    dnsEnvVar(e),
			Email:        e.TLS.Email,
			HTTP3:        e.HTTP3,
			HTTPRedirect: e.HTTPRedirect,
			HSTS:         e.HSTS && !siteIsIP && tls != TLSInternal && tls != TLSExternal,
		}, nil

	default:
		return nil, fmt.Errorf("edge: %q is not an edge mode", e.Mode)
	}
}

// dnsEnvVar returns the one environment variable tls.dns_env names, or "".
func dnsEnvVar(e spec.EdgeSpec) string {
	if len(e.TLS.DNSEnv) != 1 {
		return ""
	}
	for _, v := range e.TLS.DNSEnv {
		return v
	}
	return ""
}

// upstreamFrom picks the dashboard listener: edge.listen (TCP) when set,
// otherwise the unix socket.
func upstreamFrom(e spec.EdgeSpec) Upstream {
	if e.Listen != "" {
		return Upstream{Network: "tcp", Address: e.Listen}
	}
	return Upstream{Network: "unix", Address: e.Socket.Path}
}

// resolveTLS applies §17.4's decision tree so "auto" never reaches the
// renderer.
func resolveTLS(e spec.EdgeSpec) (TLSPolicy, error) {
	switch e.TLS.Mode {
	case "", "auto":
		if net.ParseIP(e.Site) != nil {
			return TLSInternal, nil
		}
		if e.TLS.DNSProvider != "" {
			return TLSAcmeDNS, nil
		}
		return TLSAcmeHTTP, nil
	case "acme_http":
		return TLSAcmeHTTP, nil
	case "acme_dns":
		if e.TLS.DNSProvider == "" {
			return "", fmt.Errorf("edge: tls.mode acme_dns requires tls.dns_provider")
		}
		return TLSAcmeDNS, nil
	case "internal":
		return TLSInternal, nil
	case "external":
		if e.Mode == "caddy_managed" {
			// §17.4: external resolves only when something else terminates
			// TLS; a Caddy gvm manages must manage certificates too.
			return "", fmt.Errorf("edge: tls.mode external means another proxy terminates TLS; " +
				"with caddy_managed use auto, acme_http, acme_dns or internal")
		}
		return TLSExternal, nil
	default:
		return "", fmt.Errorf("edge: %q is not a tls mode", e.TLS.Mode)
	}
}
