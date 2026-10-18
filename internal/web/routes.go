package web

// The BROWSER mux: sessions, cross-origin protection, RequireAuth (D-40).
// Its twin, the bearer-only /api/v1 mux, lives in apimux.go; nothing in this
// file registers an /api/ path and nothing in that file reads a cookie.

import (
	"net/http"

	"github.com/S95F/gameVPSmanager/internal/auth"
)

// browserRoute is one row of the browser route table. The table exists as
// data — not just mux registrations — so tests can walk it:
// TestBrowserRoutesRequireSession hits every non-public row anonymously, and
// TestSSERoutesLiveUnderStreamPrefix holds every SSE row under /stream/.
type browserRoute struct {
	method  string
	pattern string
	// public routes are reachable without a session: the login pages and
	// /static only (web-dashboard-3; /healthz is registered outside the mux).
	public bool
	// sse routes serve text/event-stream and must live under /stream/ so the
	// Caddy stream matcher can never drift (D-38).
	sse     bool
	handler func(s *Server, w http.ResponseWriter, r *http.Request)
}

var browserRoutes = []browserRoute{
	// ── Public: login and static assets ─────────────────────────────────────
	{method: "GET", pattern: "/login", public: true, handler: (*Server).handleLoginForm},
	{method: "POST", pattern: "/login", public: true, handler: (*Server).handleLoginSubmit},
	{method: "GET", pattern: "/login/totp", public: true, handler: (*Server).handleTOTPForm},
	{method: "POST", pattern: "/login/totp", public: true, handler: (*Server).handleTOTPSubmit},
	{method: "GET", pattern: "/static/{file}", public: true, handler: (*Server).handleStatic},

	// ── Session required ────────────────────────────────────────────────────
	{method: "POST", pattern: "/logout", handler: (*Server).handleLogout},
	{method: "GET", pattern: "/{$}", handler: (*Server).handleRoot},
	{method: "GET", pattern: "/instances", handler: (*Server).handleInstanceList},
	{method: "GET", pattern: "/i/{name}", handler: (*Server).handleInstanceDetail},
	{method: "GET", pattern: "/i/{name}/console", handler: (*Server).handleConsoleView},
	{method: "GET", pattern: "/stream/i/{name}/console", sse: true, handler: (*Server).handleConsoleStream},

	// Form POSTs are answered with 303 and a flash; nothing dispatches in
	// this build (U3 — see handleFormLifecycle).
	{method: "POST", pattern: "/i/{name}/start", handler: (*Server).handleFormStart},
	{method: "POST", pattern: "/i/{name}/stop", handler: (*Server).handleFormStop},
	{method: "POST", pattern: "/i/{name}/restart", handler: (*Server).handleFormRestart},
}

// buildBrowserMux assembles the browser handler chain, outermost first:
// LoadSession → CrossOriginProtection → (per-route) RequireAuth → handler
// (§16.2). CrossOriginProtection replaces per-form CSRF tokens: it compares
// Origin against Host, which works unchanged behind Caddy and needs
// cfg.TrustedOrigins behind a proxy that rewrites Host (§17.3).
func buildBrowserMux(s *Server) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range browserRoutes {
		rt := rt
		var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rt.handler(s, w, r)
		})
		if !rt.public {
			h = auth.RequireAuth(h)
		}
		mux.Handle(rt.method+" "+rt.pattern, h)
	}

	protection := http.NewCrossOriginProtection()
	for _, origin := range s.cfg.TrustedOrigins {
		// Validated in New; an error here would be a programming error.
		_ = protection.AddTrustedOrigin(origin)
	}
	return s.sessions.LoadSession(protection.Handler(mux))
}

// handleRoot redirects / to the instance list.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/instances", http.StatusFound)
}
