package web

import (
	"net/http"
)

// buildMux constructs the root http.Handler with all routes wired up.
//
// Routing notes:
//   - The auth middleware wraps all routes so session context is always set.
//   - SSE endpoint sets Content-Type: text/event-stream — the Caddy
//     encode matcher must NEVER include text/* (would buffer SSE frames).
//     The encode matcher should use application/x-www-form-urlencoded
//     for compressed responses on form POSTs.
//   - No text/* appears in any route matcher; only exact content-types.
func buildMux(s *Server) http.Handler {
	mux := http.NewServeMux()

	// ── Browser pages ──────────────────────────────────────────────────────

	// GET / → redirect to instance list.
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/instances", http.StatusFound)
	}))

	// GET /instances → dashboard instance list.
	mux.Handle("GET /instances", http.HandlerFunc(s.handleInstanceList))

	// GET /instances/{name} → instance detail page.
	mux.Handle("GET /instances/{name}", http.HandlerFunc(s.handleInstanceDetail))

	// GET /instances/{name}/console → console SSE viewer page.
	mux.Handle("GET /instances/{name}/console", http.HandlerFunc(s.handleConsoleView))

	// ── Health (unauthenticated) ───────────────────────────────────────────

	// GET /healthz → plain "OK" (used by load balancers / uptime monitors).
	mux.Handle("GET /healthz", http.HandlerFunc(s.handleHealthz))

	// GET /api/v1/health → JSON {"ok": true}.
	mux.Handle("GET /api/v1/health", http.HandlerFunc(s.handleAPIHealth))

	// ── API v1 (wrapped by auth middleware) ───────────────────────────────

	mux.Handle("GET /api/v1/instances", s.mw.Wrap(http.HandlerFunc(s.handleAPIInstanceList)))
	mux.Handle("GET /api/v1/instances/{name}/status", s.mw.Wrap(http.HandlerFunc(s.handleAPIInstanceStatus)))

	// Console SSE stream — sets text/event-stream, never buffered by Caddy.
	mux.Handle("GET /api/v1/instances/{name}/console/events", s.mw.Wrap(http.HandlerFunc(s.handleAPIConsoleEvents)))

	// Lifecycle mutations — CSRF required on POST.
	mux.Handle("POST /api/v1/instances/{name}/start", s.mw.Wrap(http.HandlerFunc(s.handleAPIStart)))
	mux.Handle("POST /api/v1/instances/{name}/stop", s.mw.Wrap(http.HandlerFunc(s.handleAPIStop)))
	mux.Handle("POST /api/v1/instances/{name}/restart", s.mw.Wrap(http.HandlerFunc(s.handleAPIRestart)))

	return mux
}
