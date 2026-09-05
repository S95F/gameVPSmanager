package spec

// HostConfig is /etc/gvm/gvm.yaml: everything about the box that is not about
// one instance.
//
// The edge and the firewall are DERIVED from this file plus the instance specs
// plus the shipped manifests, by a pure function. No database, no network, no
// syscall — so the proxy config and the rule set both survive Postgres being
// down, and both unit-test anywhere.
type HostConfig struct {
	APIVersion string   `yaml:"apiVersion"` // "gvm/v1"
	Kind       string   `yaml:"kind"`       // "Host"
	Spec       HostSpec `yaml:"spec"`
}

type HostSpec struct {
	Edge     EdgeSpec      `yaml:"edge"`
	Firewall FirewallSpec  `yaml:"firewall"`
	Steam    SteamHostSpec `yaml:"steam,omitempty"`
}

type EdgeSpec struct {
	Mode string `yaml:"mode"` // caddy_managed | caddy_snippet | proxy_external | none
	Site string `yaml:"site"` // domain, bare IP, or "" when mode: none

	HTTPRedirect bool `yaml:"http_redirect"`

	// HTTP3=false emits `protocols h1 h2` and does NOT open 443/udp. Opening a
	// UDP port for a protocol the proxy is not speaking is a rule nobody can
	// later explain.
	HTTP3 bool `yaml:"http3"`

	// HSTS is a one-way door per hostname, so it is opt-in and only ever
	// emitted for a public DNS name — never for `tls internal` or a bare IP,
	// where RFC 6797 does not apply.
	HSTS bool `yaml:"hsts"`

	Socket EdgeSocket `yaml:"socket"`
	Listen string     `yaml:"listen,omitempty"` // optional extra listener, e.g. 127.0.0.1:8443

	// TrustedProxies is EMPTY by default and emitted only when configured. When
	// Caddy is the edge there is nothing in front of it, and trusting private
	// ranges would let anything that can reach the box spoof X-Forwarded-For
	// into the audit log and the per-IP lockout (D-39).
	TrustedProxies []string `yaml:"trusted_proxies"`

	// TrustedOrigins are extra Origins for net/http.CrossOriginProtection.
	TrustedOrigins []string `yaml:"trusted_origins"`

	TLS   TLSSpec   `yaml:"tls"`
	Caddy CaddySpec `yaml:"caddy"`
}

// EdgeSocket is the dashboard socket. The DIRECTORY carries the permission
// (2750 gvm:<group>, setgid) so the socket inherits the group; the proxy can
// traverse and connect but cannot unlink it (D-06).
type EdgeSocket struct {
	Path  string `yaml:"path"`
	Group string `yaml:"group"` // caddy | www-data | traefik
}

type TLSSpec struct {
	Mode        string            `yaml:"mode"`  // auto | acme_http | acme_dns | internal | external
	Email       string            `yaml:"email"` // ACME account
	ACMECa      string            `yaml:"acme_ca,omitempty"`
	DNSProvider string            `yaml:"dns_provider,omitempty"` // needs caddy-dns/<provider> in the binary
	DNSEnv      map[string]string `yaml:"dns_env,omitempty"`
}

type CaddySpec struct {
	ManageUnit bool   `yaml:"manage_unit"`
	ConfigPath string `yaml:"config_path"` // gvm owns THIS file, never /etc/caddy/Caddyfile (D-37)

	// Admin is a unix socket, not localhost:2019. gvm-web is internet-reachable
	// through this same Caddy, and an SSRF that can POST to the admin API
	// replaces the entire proxy config (D-36).
	Admin string `yaml:"admin"`

	// MinVersion gates configuration entirely: an older Caddy is refused with
	// the upgrade command rather than half-configured.
	MinVersion string `yaml:"min_version"`

	LogPath string `yaml:"log_path"`
}

type FirewallSpec struct {
	Backend string `yaml:"backend"` // ufw | none ("nft" parses and returns ErrUnsupported)

	// Manage=false computes and shows the plan and never applies it.
	Manage bool `yaml:"manage"`

	Logging string  `yaml:"logging"` // off | low | medium | high | full
	SSH     SSHSpec `yaml:"ssh"`

	// AdminCIDRs are the host-wide sources allowed to reach `expose: admin`
	// ports. Empty means an admin port gets no rule at all.
	AdminCIDRs []string `yaml:"admin_cidrs"`

	// ReservedPorts is the operator's own don't-touch list, one of the five
	// occupancy sources the allocator consults. A bare number reserves the port
	// on BOTH protocols, because "do not use 8080" never means one of them.
	ReservedPorts []int `yaml:"reserved_ports"`

	Egress EgressPolicy `yaml:"egress"`
}

type SSHSpec struct {
	// Ports empty means autodetect. An explicit list wins over every detected
	// source, and no computed rule set may fail to admit SSH.
	Ports []int `yaml:"ports"`

	// RateLimit emits `ufw limit`, not `ufw allow` — firewall.sh already did
	// this and dropping it would be a silent regression.
	RateLimit bool `yaml:"rate_limit"`

	Fail2Ban bool `yaml:"fail2ban"`
}

// SteamHostSpec collapses every instance's SteamCMD HOME onto one host-wide
// directory when disk is tight. Doing so makes the host flock the ONLY thing
// preventing two concurrent steamcmd runs from corrupting ~/.steam, which is why
// it is opt-in and why doctor says so (D-30).
type SteamHostSpec struct {
	SharedCache bool `yaml:"shared_cache,omitempty"`
}

// DefaultHostConfig is the configuration a box gets before an operator edits
// anything. It exists because most of these fields are booleans whose safe value
// is true, and a zero-valued struct would silently mean "no HTTP redirect, no
// HTTP/3, unmanaged firewall, no SSH rate limit" — every one of those a
// regression against the shipped default rather than a choice anyone made.
func DefaultHostConfig() HostConfig {
	return HostConfig{
		APIVersion: "gvm/v1",
		Kind:       "Host",
		Spec: HostSpec{
			Edge: EdgeSpec{
				Mode:         "none",
				HTTPRedirect: true,
				HTTP3:        true,
				HSTS:         false,
				Socket: EdgeSocket{
					Path:  "/run/gvm/web/dashboard.sock",
					Group: "caddy",
				},
				TLS: TLSSpec{Mode: "auto"},
				Caddy: CaddySpec{
					ManageUnit: true,
					ConfigPath: "/etc/gvm/caddy/Caddyfile",
					Admin:      "unix//run/caddy/admin.sock",
					MinVersion: "2.11.1",
					LogPath:    "/var/log/caddy/gvm.log",
				},
			},
			Firewall: FirewallSpec{
				Backend: "ufw",
				Manage:  true,
				Logging: "low",
				SSH:     SSHSpec{RateLimit: true, Fail2Ban: true},
				Egress:  EgressPolicy{Policy: "open"},
			},
		},
	}
}
