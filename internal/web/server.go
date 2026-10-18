// Package web implements the gvm dashboard HTTP server (gvm web).
//
// It is a separate process from gvmd, talking to gvmd only through the
// interfaces it accepts at construction time. It never imports store directly;
// anything that needs Postgres is behind the auth.SessionQuerier,
// auth.TokenQuerier or a dedicated querier interface.
//
// Two structurally separate muxes (D-40):
//
//	browser (routes.go)  sessions → CrossOriginProtection → RequireAuth
//	/api/v1 (apimux.go)  bearer tokens only; cookies are never read
package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/auth"
	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/webapi"
)

// InstanceQuerier is the minimal read interface the dashboard needs.
// The store package satisfies this; tests use stubs.
type InstanceQuerier interface {
	// ListInstances returns a lightweight summary for every instance.
	ListInstances(ctx context.Context) ([]InstanceSummary, error)
	// GetInstance returns a single instance by name.
	GetInstance(ctx context.Context, name string) (*InstanceDetail, error)
}

// InstanceSummary is the data shown in the instance list.
type InstanceSummary struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Game   string `json:"game"`
}

// InstanceDetail is the data shown on the instance detail page.
type InstanceDetail struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Game   string `json:"game"`
	Addr   string `json:"addr,omitempty"`
	Port   int    `json:"port,omitempty"`
}

// Dispatcher is the gvmd client behind start/stop/restart. A dispatch returns
// only after gvmd acknowledged it, with the operation id for the audit trail;
// a held instance returns *LeaseHeldError. With no Dispatcher wired the
// handlers answer 501 before consulting anything — a 2xx without a real
// dispatch is fake success (U3), and TestMutationsNeedDispatchAndRole holds
// both sides of that.
type Dispatcher interface {
	Start(ctx context.Context, name, actor string) (opID string, err error)
	Stop(ctx context.Context, name, actor string) (opID string, err error)
	Restart(ctx context.Context, name, actor string) (opID string, err error)
}

// LeaseHeldError is a refusal because a job holds the instance; Line is the
// holder's one-line description, rendered verbatim (409 on the API).
type LeaseHeldError struct{ Line string }

func (e *LeaseHeldError) Error() string { return "a job holds this server: " + e.Line }

// ErrControlUnavailable is the gvmd gateway not answering. Game servers are
// unaffected; the dashboard says so with the degraded answer.
var ErrControlUnavailable = errors.New("web: the control daemon is not reachable")

// Config holds everything the web server needs at construction time.
type Config struct {
	// Addr is the listen address: an absolute unix socket path
	// ("/run/gvm/web/dashboard.sock", the default deployment, D-06), or a TCP
	// address ("127.0.0.1:8130") for edge.listen — e.g. behind a foreign
	// proxy such as a serverGitUpdater-managed Caddy.
	Addr string

	// SessionQuerier backs the browser mux's session lookups.
	SessionQuerier auth.SessionQuerier

	// TokenQuerier backs the /api/v1 mux's bearer lookups.
	TokenQuerier auth.TokenQuerier

	// InstanceQuerier backs the dashboard page handlers.
	// When nil the server serves the single degraded template.
	InstanceQuerier InstanceQuerier

	// Dispatcher is the gvmd client for start/stop/restart; nil answers 501.
	Dispatcher Dispatcher

	// Identity backs the login engine; nil keeps /login an honest 501.
	Identity Identity

	// Authorizer resolves per-instance effective roles; nil falls back to the
	// session's or token's own role (global only).
	Authorizer Authorizer

	// InsecureCookies drops the Secure attribute for plain-HTTP development
	// (`gvm web --dev`); a Secure cookie over http://localhost is silently
	// dropped and the first login "succeeds" but never sticks (§16.2).
	InsecureCookies bool

	// Hub multiplexes Ring subscriptions to SSE connections.
	// When nil the console SSE endpoints send one comment and close.
	Hub *console.Hub

	// TrustedOrigins are extra Origins accepted by the browser mux's
	// cross-origin protection — required behind a foreign proxy that does not
	// pass Host through unchanged (§17.3), harmless behind Caddy, which does.
	TrustedOrigins []string
}

// Server is the gvm-web HTTP server.
type Server struct {
	cfg      Config
	sessions *auth.Middleware
	bearer   *auth.BearerMiddleware
	pending  *pendingStore
}

// New creates a Server from cfg. It errs on a malformed trusted origin rather
// than silently dropping CSRF protection for it.
func New(cfg Config) (*Server, error) {
	for _, origin := range cfg.TrustedOrigins {
		if err := validateOrigin(origin); err != nil {
			return nil, fmt.Errorf("web: trusted origin %q: %w", origin, err)
		}
	}
	return &Server{
		cfg:      cfg,
		sessions: auth.NewMiddleware(cfg.SessionQuerier),
		bearer:   auth.NewBearer(cfg.TokenQuerier),
		pending:  newPendingStore(),
	}, nil
}

// validateOrigin runs a throwaway CrossOriginProtection so New can reject a
// bad origin at construction time with the stdlib's own error.
func validateOrigin(origin string) error {
	return http.NewCrossOriginProtection().AddTrustedOrigin(origin)
}

// Handler returns the root http.Handler. Useful for testing without binding a
// port.
func (s *Server) Handler() http.Handler {
	root := http.NewServeMux()
	// /healthz is unauthenticated by design: load balancers and uptime
	// monitors hold no session (web-dashboard-3 exempts it).
	root.Handle("GET /healthz", http.HandlerFunc(s.handleHealthz))
	root.Handle("/api/v1/", buildAPIMux(s))
	root.Handle("/", buildBrowserMux(s))
	return webapi.RequestID(securityHeaders(root))
}

// securityHeaders applies §16.2's header set to every response. The CSP has
// no unsafe-*: pages carry no inline styles or scripts (everything is under
// /static), and csp-conscious tests keep it that way.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; "+
				"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe starts the HTTP server and blocks until ctx is cancelled or
// an unrecoverable error occurs. An Addr beginning with "/" is a unix socket
// (the directory carries the permission, D-06; the socket itself is opened
// 0660 so the proxy's group can connect); anything else is TCP.
func (s *Server) ListenAndServe(ctx context.Context) error {
	var ln net.Listener
	var err error
	if strings.HasPrefix(s.cfg.Addr, "/") {
		// A stale socket from an unclean shutdown refuses the bind; remove it.
		_ = os.Remove(s.cfg.Addr)
		ln, err = net.Listen("unix", s.cfg.Addr)
		if err == nil {
			err = os.Chmod(s.cfg.Addr, 0o660)
		}
	} else {
		ln, err = net.Listen("tcp", s.cfg.Addr)
	}
	if err != nil {
		return fmt.Errorf("web: listen %s: %w", s.cfg.Addr, err)
	}

	srv := &http.Server{
		Handler:      s.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE streams must not time out on write
		IdleTimeout:  120 * time.Second,
	}

	// Shut the server down when the context is cancelled.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("web: serve: %w", err)
	}
	return nil
}
