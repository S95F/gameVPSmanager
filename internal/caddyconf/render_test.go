package caddyconf

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/edge"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/packaging"
)

// go test ./internal/caddyconf -run Golden -update rewrites the goldens.
// CI then runs `caddy validate` (pinned binary) over the committed files, so
// an update that produces an invalid Caddyfile fails there, not on a VPS.
var update = flag.Bool("update", false, "rewrite the golden Caddyfiles")

// ── Fixtures ─────────────────────────────────────────────────────────────────

func managedHost() spec.HostConfig {
	h := spec.DefaultHostConfig()
	h.Spec.Edge.Mode = "caddy_managed"
	h.Spec.Edge.Site = "gvm.example.com"
	h.Spec.Edge.TLS.Email = "ops@example.com"
	return h
}

// fixtures maps each golden file to its host config. Between them they cover
// acme_http, acme_dns (+HSTS), internal on a bare IP (+h1 h2 only), and the
// snippet mode with a TCP upstream — the deployment behind a foreign Caddy
// (e.g. one that serverGitUpdater manages) with gvm-web on a loopback port.
var fixtures = map[string]func() spec.HostConfig{
	"acme-http": managedHost,
	"acme-dns": func() spec.HostConfig {
		h := managedHost()
		h.Spec.Edge.TLS.DNSProvider = "cloudflare"
		h.Spec.Edge.TLS.DNSEnv = map[string]string{"CF_API_TOKEN": "CF_API_TOKEN"}
		h.Spec.Edge.HSTS = true
		return h
	},
	"internal-bareip": func() spec.HostConfig {
		h := managedHost()
		h.Spec.Edge.Site = "203.0.113.7"
		h.Spec.Edge.HTTP3 = false
		return h
	},
	"snippet-import": func() spec.HostConfig {
		h := managedHost()
		h.Spec.Edge.Mode = "caddy_snippet"
		h.Spec.Edge.Listen = "127.0.0.1:8130"
		return h
	},
}

func render(t *testing.T, host *spec.HostConfig) string {
	t.Helper()
	plan, err := edge.ComputePlan(host)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	var b strings.Builder
	if err := Render(&b, host, plan); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return b.String()
}

func renderFixture(t *testing.T, name string) string {
	t.Helper()
	host := fixtures[name]()
	return render(t, &host)
}

// crlf-insensitive comparison: the goldens are committed with LF, but a
// Windows checkout may hand the test CRLF.
func normalize(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// ── Goldens ──────────────────────────────────────────────────────────────────

func TestGoldenCaddyfiles(t *testing.T) {
	for name := range fixtures {
		t.Run(name, func(t *testing.T) {
			got := renderFixture(t, name)
			path := filepath.Join("testdata", name+".golden")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden (run with -update): %v", err)
			}
			if normalize(string(want)) != normalize(got) {
				t.Errorf("rendered Caddyfile differs from %s (re-run with -update and review the diff):\n%s", path, got)
			}
		})
	}
}

// ── D-36: the admin API stays on a unix socket ───────────────────────────────

func TestRenderAdminIsUnixSocket(t *testing.T) {
	host := managedHost()
	out := render(t, &host)
	if !strings.Contains(out, "admin unix/") {
		t.Error("Caddyfile must contain 'admin unix/' (D-36)")
	}
	if strings.Contains(out, "2019") {
		t.Error("Caddyfile must not reference the TCP admin port (D-36)")
	}
}

func TestRenderAdminTCPAddressRejected(t *testing.T) {
	host := managedHost()
	host.Spec.Edge.Caddy.Admin = "localhost:2019"
	plan, err := edge.ComputePlan(&host)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	err = Render(&b, &host, plan)
	if err == nil {
		t.Fatal("expected error when caddy.admin is not a unix socket")
	}
	if !strings.Contains(err.Error(), "D-36") {
		t.Errorf("error should mention D-36, got: %v", err)
	}
}

// ── D-38: SSE survives the encoder and the matcher cannot drift ──────────────

func TestRenderEncodeNeverMatchesTextStar(t *testing.T) {
	host := managedHost()
	out := render(t, &host)
	for _, banned := range []string{"Content-Type text/*", "Content-Type text/event-stream"} {
		if strings.Contains(out, banned) {
			t.Errorf("encode matcher must not include %q (breaks SSE)", banned)
		}
	}
}

func TestRenderStreamHandlesCoverBothPrefixes(t *testing.T) {
	// Every text/event-stream route the app serves lives under /stream/ or
	// /api/v1/stream/ (asserted in internal/web); both prefixes must get the
	// immediate-flush treatment here.
	host := managedHost()
	out := render(t, &host)
	for _, prefix := range []string{"handle /stream/*", "handle /api/v1/stream/*"} {
		if !strings.Contains(out, prefix) {
			t.Errorf("missing %q block", prefix)
		}
	}
	if n := strings.Count(out, "flush_interval -1"); n != 2 {
		t.Errorf("flush_interval -1 appears %d times, want 2 (one per stream prefix)", n)
	}
	if n := strings.Count(out, "stream_close_delay 5m"); n != 2 {
		t.Errorf("stream_close_delay 5m appears %d times, want 2", n)
	}
}

// ── web-dashboard-11: one vhost, one upstream, single unix/ prefix ───────────

func TestRenderSingleUnixPrefix(t *testing.T) {
	host := managedHost()
	out := render(t, &host)
	if !strings.Contains(out, "reverse_proxy unix//run/gvm/web/dashboard.sock") {
		t.Error("expected the dashboard socket as the one upstream")
	}
	if strings.Contains(out, "unix/unix") {
		t.Error("the unix/ prefix is doubled")
	}
	if strings.Contains(out, "web.sock") {
		t.Error("per-instance web.sock routes must be gone; routing is /i/{name} inside the app")
	}
}

func TestRenderTCPUpstream(t *testing.T) {
	out := renderFixture(t, "snippet-import")
	if !strings.Contains(out, "reverse_proxy 127.0.0.1:8130") {
		t.Error("edge.listen should become a TCP upstream")
	}
	if strings.Contains(out, "unix/") {
		t.Error("a TCP upstream must not carry a unix/ prefix")
	}
}

// ── Modes ────────────────────────────────────────────────────────────────────

func TestRenderNeverWritesEtcCaddyComment(t *testing.T) {
	// The generated file must note that gvm never writes /etc/caddy/Caddyfile (D-37).
	host := managedHost()
	if out := render(t, &host); !strings.Contains(out, "D-37") {
		t.Error("rendered Caddyfile must mention the D-37 constraint")
	}
}

func TestRenderModeNoneEmitsAdminBlock(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "none"
	// Even with mode:none the global admin block must be present.
	if out := render(t, &host); !strings.Contains(out, "admin unix/") {
		t.Error("admin block must appear even when mode is none")
	}
}

func TestRenderProxyExternalRefused(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Mode = "proxy_external"
	host.Spec.Edge.TrustedOrigins = []string{"https://gvm.example.com"}
	plan, err := edge.ComputePlan(&host)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := Render(&b, &host, plan); err == nil {
		t.Error("proxy_external must refuse to render: gvm writes nothing for a foreign proxy")
	}
}

func TestRenderSnippetHasNoGlobalBlock(t *testing.T) {
	out := renderFixture(t, "snippet-import")
	if strings.Contains(out, "admin unix/") || strings.Contains(out, "servers {") {
		t.Error("snippet mode must emit only the site block; global options belong to the operator's Caddyfile")
	}
	if !strings.Contains(out, "import") {
		t.Error("snippet header should tell the operator how to import it")
	}
}

// ── TLS and HSTS ─────────────────────────────────────────────────────────────

func TestRenderTLSDirectives(t *testing.T) {
	if out := renderFixture(t, "internal-bareip"); !strings.Contains(out, "tls internal") {
		t.Error("bare-IP site should use tls internal")
	}
	if out := renderFixture(t, "acme-dns"); !strings.Contains(out, "dns cloudflare") {
		t.Error("acme_dns should emit the dns provider")
	}
	if out := renderFixture(t, "acme-http"); strings.Contains(out, "\ttls") {
		t.Error("acme_http needs no tls directive; automatic HTTPS handles it")
	}
}

func TestHSTSOnlyWhereItApplies(t *testing.T) {
	if out := renderFixture(t, "internal-bareip"); strings.Contains(out, "Strict-Transport-Security") {
		t.Error("HSTS emitted for a bare IP / tls internal (RFC 6797 does not apply)")
	}
	if out := renderFixture(t, "acme-dns"); !strings.Contains(out, "Strict-Transport-Security") {
		t.Error("HSTS opted in on a public DNS name but not emitted")
	}
}

func TestRenderHTTP3Toggle(t *testing.T) {
	if out := renderFixture(t, "acme-http"); !strings.Contains(out, "protocols h1 h2 h3") {
		t.Error("http3 on by default: want protocols h1 h2 h3")
	}
	out := renderFixture(t, "internal-bareip")
	if !strings.Contains(out, "protocols h1 h2\n") || strings.Contains(out, "protocols h1 h2 h3") {
		t.Error("http3: false must emit protocols h1 h2 (and the firewall then opens no 443/udp)")
	}
}

// ── The tmpfiles split (onboarding-1) ────────────────────────────────────────

func TestTmpfilesGroupComesFromSpec(t *testing.T) {
	out := Tmpfiles("www-data")
	if strings.Count(out, "www-data") != 2 {
		t.Errorf("Tmpfiles should carry the group on both lines:\n%s", out)
	}
	if strings.Contains(out, " caddy ") {
		t.Error("a non-caddy group must fully replace the hard-coded default")
	}
}

// TestPackagingShipsTheCaddyDefault keeps packaging/tmpfiles/gvm-web.conf and
// Tmpfiles from drifting apart: the shipped file IS the caddy rendering.
func TestPackagingShipsTheCaddyDefault(t *testing.T) {
	shipped, err := packaging.FS.ReadFile("tmpfiles/gvm-web.conf")
	if err != nil {
		t.Fatalf("packaging does not ship tmpfiles/gvm-web.conf: %v", err)
	}
	if normalize(string(shipped)) != normalize(Tmpfiles("caddy")) {
		t.Error("packaging/tmpfiles/gvm-web.conf differs from Tmpfiles(\"caddy\")")
	}
}

// TestGamesOnlyTmpfilesHasNoCaddyGroup: after the split, gvm.conf must not
// reference the caddy group at all, so systemd-tmpfiles converges on a host
// that never installed a proxy.
func TestGamesOnlyTmpfilesHasNoCaddyGroup(t *testing.T) {
	shipped, err := packaging.FS.ReadFile("tmpfiles/gvm.conf")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(normalize(string(shipped)), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, "caddy") {
			t.Errorf("gvm.conf still carries a caddy-group line: %q", line)
		}
	}
}
