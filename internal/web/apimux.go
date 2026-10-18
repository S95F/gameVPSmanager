package web

// The /api/v1 mux: bearer tokens only (D-40).
//
// This file is deliberately free of sessions: no auth.Session, no
// SessionQuerier, no cookie read — the compiler-visible half of the rule that
// a cookie can never authenticate an API call. TestAPIIgnoresCookies holds
// the behavioural half.

import (
	"net/http"
)

// apiRoute is one row of the API route table; see browserRoute for why the
// table is data.
type apiRoute struct {
	method  string
	pattern string
	// public routes answer without a bearer token: the health probe only.
	public bool
	// sse routes serve text/event-stream and must live under /api/v1/stream/
	// (D-38).
	sse     bool
	handler func(s *Server, w http.ResponseWriter, r *http.Request)
}

var apiRoutes = []apiRoute{
	{method: "GET", pattern: "/api/v1/health", public: true, handler: (*Server).handleAPIHealth},

	{method: "GET", pattern: "/api/v1/instances", handler: (*Server).handleAPIInstanceList},
	{method: "GET", pattern: "/api/v1/instances/{name}/status", handler: (*Server).handleAPIInstanceStatus},
	{method: "GET", pattern: "/api/v1/stream/instances/{name}/console", sse: true, handler: (*Server).handleConsoleStream},

	// Honest 501s until Phase 48 wires gvmd dispatch (U3: no fake success).
	{method: "POST", pattern: "/api/v1/instances/{name}/start", handler: (*Server).handleAPIStart},
	{method: "POST", pattern: "/api/v1/instances/{name}/stop", handler: (*Server).handleAPIStop},
	{method: "POST", pattern: "/api/v1/instances/{name}/restart", handler: (*Server).handleAPIRestart},
}

// buildAPIMux assembles the bearer-only mux.
func buildAPIMux(s *Server) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range apiRoutes {
		rt := rt
		var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rt.handler(s, w, r)
		})
		if !rt.public {
			h = s.bearer.Wrap(h)
		}
		mux.Handle(rt.method+" "+rt.pattern, h)
	}
	return mux
}
