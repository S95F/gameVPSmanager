// Package edge computes the Caddy configuration from the host spec and
// instance list. It is pure: no I/O, no database, no network.
package edge

import (
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Route is one reverse-proxy route destined for a game server's HTTP socket.
type Route struct {
	// Host is the hostname this route matches, e.g. "pz.example.com".
	Host string
	// Upstream is the unix socket or TCP address for the game server's web API.
	Upstream string
	// Instance is the instance name.
	Instance string
	// TLS is the TLS policy for this route.
	TLS TLSPolicy
	// EnforceHTTPS redirects HTTP→HTTPS when true.
	EnforceHTTPS bool
	// Headers are extra response headers to emit.
	Headers map[string]string
}

// TLSPolicy controls how Caddy handles TLS for a vhost.
type TLSPolicy string

const (
	TLSAuto     TLSPolicy = "auto"     // ACME: Let's Encrypt or ZeroSSL
	TLSInternal TLSPolicy = "internal" // Caddy's internal CA (dev / no public DNS)
	TLSExternal TLSPolicy = "external" // Operator provides cert/key files
	TLSNone     TLSPolicy = "none"     // HTTP only — dangerous, only for testing
)

// Plan is the computed edge plan for the host.
type Plan struct {
	Mode   string  // caddy_managed | caddy_snippet | proxy_external | none
	Routes []Route // non-empty only when Mode requires Caddy
}

// ComputePlan derives the Caddy plan from the host configuration and live instances.
// It is deterministic and has no side effects.
func ComputePlan(host *spec.HostConfig, instances []spec.Instance) (*Plan, error) {
	mode := host.Spec.Edge.Mode
	if mode == "none" || mode == "" {
		return &Plan{Mode: "none"}, nil
	}

	if mode == "proxy_external" {
		if len(host.Spec.Edge.TrustedOrigins) == 0 {
			return nil, fmt.Errorf("edge: proxy_external requires at least one trusted_origins entry")
		}
		return &Plan{Mode: "proxy_external"}, nil
	}

	tlsPolicy := tlsPolicyFrom(host.Spec.Edge.TLS.Mode)
	var routes []Route

	for _, inst := range instances {
		if !inst.Spec.Enabled {
			continue
		}
		sock := inst.Spec.Paths.Home
		if sock == "" {
			continue
		}
		// The game server exposes its web API on a unix socket under its home dir.
		upstream := "unix/" + strings.TrimRight(sock, "/") + "/web.sock"

		host_ := inst.Metadata.Name
		if site := host.Spec.Edge.Site; site != "" {
			host_ = inst.Metadata.Name + "." + site
		}

		routes = append(routes, Route{
			Host:         host_,
			Upstream:     upstream,
			Instance:     inst.Metadata.Name,
			TLS:          tlsPolicy,
			EnforceHTTPS: host.Spec.Edge.HTTPRedirect,
		})
	}

	return &Plan{Mode: mode, Routes: routes}, nil
}

func tlsPolicyFrom(mode string) TLSPolicy {
	switch mode {
	case "internal":
		return TLSInternal
	case "external":
		return TLSExternal
	case "none":
		return TLSNone
	default:
		return TLSAuto
	}
}
