package caddyconf

import "fmt"

// Tmpfiles renders the gvm-web tmpfiles fragment: the two dashboard-only
// directories, split out of gvm.conf so `systemd-tmpfiles --create` converges
// on a games-only host that has no caddy group (onboarding-1).
//
// The group comes from edge.socket.group (§17.3) — caddy by default,
// www-data for nginx, traefik for traefik. Changing it rewrites this snippet
// and re-runs systemd-tmpfiles; packaging ships the caddy default, asserted
// equal to Tmpfiles("caddy") so the two cannot drift.
func Tmpfiles(group string) string {
	if group == "" {
		group = "caddy"
	}
	return fmt.Sprintf(`# /usr/lib/tmpfiles.d/gvm-web.conf -- ARCHITECTURE.md 17.3 (D-05, D-06).
#
# Installed only with the dashboard (gvm host init --dashboard), never on a
# games-only host. The group below is edge.socket.group.
#
#   /run/gvm/web    2750 gvm:<group>, setgid: the DIRECTORY carries the
#                   permission, so the dashboard socket inherits the group;
#                   the proxy can traverse and connect but cannot unlink it.
#   /etc/gvm/caddy  0750 root:<group>: the proxy reads the config gvm renders
#                   (D-37); only root writes it.
d /run/gvm/web              2750 gvm  %s -
d /etc/gvm/caddy            0750 root %s -
`, group, group)
}
