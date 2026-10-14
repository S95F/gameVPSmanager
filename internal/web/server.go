// Package web implements the gvm dashboard HTTP server (gvm web).
//
// It is a separate process from gvmd, talking to gvmd only through the
// interfaces it accepts at construction time. It never imports store directly;
// anything that needs Postgres is behind the auth.SessionQuerier or a
// dedicated querier interface.
package web

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/S95F/gameVPSmanager/internal/auth"
	"github.com/S95F/gameVPSmanager/internal/console"
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

// Config holds everything the web server needs at construction time.
type Config struct {
	// Addr is the listen address, e.g. ":8080" or "127.0.0.1:8080".
	Addr string

	// SessionQuerier backs the auth middleware's session lookups.
	// When nil the server always returns 503.
	SessionQuerier auth.SessionQuerier

	// InstanceQuerier backs the dashboard page handlers.
	// When nil the server returns stub/empty data so the binary still compiles
	// and runs during development before the store is wired up.
	InstanceQuerier InstanceQuerier

	// Hub multiplexes Ring subscriptions to SSE connections.
	// When nil the console SSE endpoint returns an empty stream.
	Hub *console.Hub

	// CSRFSecret is used to initialise CSRF protection.
	// When nil a random 32-byte secret is generated on each process start;
	// that is acceptable for a single-node dashboard.
	CSRFSecret []byte
}

// Server is the gvm-web HTTP server.
type Server struct {
	cfg  Config
	mw   *auth.Middleware
	csrf *auth.CSRFProtection
}

// New creates a Server from cfg.
func New(cfg Config) *Server {
	secret := cfg.CSRFSecret
	if len(secret) == 0 {
		secret = mustRandomBytes(32)
	}
	return &Server{
		cfg:  cfg,
		mw:   auth.NewMiddleware(cfg.SessionQuerier),
		csrf: auth.NewCSRF(secret),
	}
}

// Handler returns the root http.Handler. Useful for testing without binding a
// port.
func (s *Server) Handler() http.Handler {
	return buildMux(s)
}

// ListenAndServe starts the HTTP server and blocks until ctx is cancelled or
// an unrecoverable error occurs.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
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

// mustRandomBytes returns n cryptographically random bytes; panics on failure
// (entropy exhaustion is unrecoverable at startup).
func mustRandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("web: crypto/rand: " + err.Error())
	}
	return b
}
