package caddyconf

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/edge"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func defaultHost() spec.HostConfig {
	h := spec.DefaultHostConfig()
	h.Spec.Edge.Mode = "caddy_managed"
	h.Spec.Edge.Site = "example.com"
	h.Spec.Edge.TLS.Mode = "auto"
	return h
}

func renderToString(t *testing.T, host *spec.HostConfig, plan *edge.Plan) string {
	t.Helper()
	var b strings.Builder
	if err := Render(&b, host, plan); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return b.String()
}

func TestRenderAdminIsUnixSocket(t *testing.T) {
	// D-36: admin API must be a unix socket, never localhost:2019.
	host := defaultHost()
	plan := &edge.Plan{Mode: "none"}
	out := renderToString(t, &host, plan)
	if !strings.Contains(out, "admin unix/") {
		t.Error("Caddyfile must contain 'admin unix/' (D-36)")
	}
	if strings.Contains(out, "localhost:2019") || strings.Contains(out, "2019") {
		t.Error("Caddyfile must not contain localhost:2019 (D-36)")
	}
}

func TestRenderEncodeNeverMatchesTextStar(t *testing.T) {
	// encode's matcher must never include text/* — breaks SSE on text/event-stream.
	host := defaultHost()
	plan := &edge.Plan{
		Mode: "caddy_managed",
		Routes: []edge.Route{
			{Host: "pz1.example.com", Upstream: "unix//srv/gvm/pz1/web.sock", TLS: edge.TLSAuto},
		},
	}
	out := renderToString(t, &host, plan)
	if strings.Contains(out, "encode") && strings.Contains(out, "text/*") {
		t.Error("encode matcher must not include text/* (breaks SSE)")
	}
	if strings.Contains(out, "encode") && strings.Contains(out, "text/event-stream") {
		t.Error("encode matcher must not match text/event-stream")
	}
}

func TestRenderStreamRouteHasFlushInterval(t *testing.T) {
	host := defaultHost()
	plan := &edge.Plan{
		Mode: "caddy_managed",
		Routes: []edge.Route{
			{Host: "pz1.example.com", Upstream: "unix//srv/gvm/pz1/web.sock", TLS: edge.TLSAuto},
		},
	}
	out := renderToString(t, &host, plan)
	if !strings.Contains(out, "flush_interval -1") {
		t.Error("stream routes must have flush_interval -1")
	}
	if !strings.Contains(out, "stream_close_delay 5m") {
		t.Error("stream routes must have stream_close_delay 5m")
	}
}

func TestRenderAdminTCPAddressRejected(t *testing.T) {
	host := defaultHost()
	host.Spec.Edge.Caddy.Admin = "localhost:2019"
	plan := &edge.Plan{Mode: "none"}
	var b strings.Builder
	err := Render(&b, &host, plan)
	if err == nil {
		t.Error("expected error when caddy.admin is not a unix socket")
	}
	if !strings.Contains(err.Error(), "D-36") {
		t.Errorf("error should mention D-36, got: %v", err)
	}
}

func TestRenderNeverWritesEtcCaddyComment(t *testing.T) {
	// The generated file must note that gvm never writes /etc/caddy/Caddyfile (D-37).
	host := defaultHost()
	plan := &edge.Plan{Mode: "none"}
	out := renderToString(t, &host, plan)
	if !strings.Contains(out, "D-37") {
		t.Error("rendered Caddyfile must mention D-37 constraint")
	}
}

func TestRenderStreamRouteNotCompressed(t *testing.T) {
	// /stream/* must be excluded from the compress block.
	host := defaultHost()
	plan := &edge.Plan{
		Mode: "caddy_managed",
		Routes: []edge.Route{
			{Host: "pz1.example.com", Upstream: "unix//srv/gvm/pz1/web.sock", TLS: edge.TLSAuto},
		},
	}
	out := renderToString(t, &host, plan)
	if !strings.Contains(out, "not path /stream/*") {
		t.Error("compress block must exclude /stream/* routes")
	}
}

func TestRenderModeNoneEmitsAdminBlock(t *testing.T) {
	host := defaultHost()
	plan := &edge.Plan{Mode: "none"}
	out := renderToString(t, &host, plan)
	// Even with mode:none the global admin block must be present.
	if !strings.Contains(out, "admin unix/") {
		t.Error("admin block must appear even when mode is none")
	}
}
