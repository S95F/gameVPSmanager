package gvmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/identity"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// maxRequestLine bounds one NDJSON request. A console line is at most
// MaxConsoleLine; everything else is names and codes.
const maxRequestLine = 8 * 1024

// MaxConsoleLine is the console.send payload cap (§16.6: one line, at most
// 1024 bytes).
const MaxConsoleLine = 1024

// Identifier names the far end of an accepted connection. The Linux
// implementation reads SO_PEERCRED; tests inject their own.
type Identifier func(conn net.Conn) (platform.Peer, error)

// Server serves the closed verb set on a listener.
type Server struct {
	backend  Backend
	identify Identifier
	// allowUID admits a non-root peer: the web tier's own account. Root is
	// always admitted.
	allowUID func(uint32) bool
	log      *slog.Logger
}

// NewServer builds a Server, refusing a partial backend so no verb dangles.
func NewServer(b Backend, identify Identifier, allowUID func(uint32) bool, log *slog.Logger) (*Server, error) {
	if err := b.validate(); err != nil {
		return nil, err
	}
	if identify == nil {
		return nil, errors.New("gvmd: NewServer needs an Identifier")
	}
	if allowUID == nil {
		allowUID = func(uint32) bool { return false }
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{backend: b, identify: identify, allowUID: allowUID, log: log}, nil
}

// Serve accepts connections until ctx is cancelled or the listener fails.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	var wg sync.WaitGroup
	done := make(chan struct{})
	defer func() { close(done); wg.Wait() }()

	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-done:
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("gvmd: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(ctx, conn)
		}()
	}
}

// handleConn authorizes the peer once, then answers requests line by line.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	enc := json.NewEncoder(conn)

	peer, err := s.identify(conn)
	if err != nil || !peer.Identified() || (!peer.Root() && !s.allowUID(peer.UID)) {
		// One refusal, then hang up. The socket is 0660 root:gvm, so a peer
		// reaching here at all is in group gvm; the uid check is what keeps
		// "in the group for /etc/gvm" from meaning "holds the verb surface".
		_ = enc.Encode(Response{OK: false, Code: CodeUnauthorized, Error: "peer is not authorized for the verb gateway"})
		// Half-close and drain briefly so the refusal reaches the peer before
		// the close can abort it (a bare Close may RST an unread write).
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			_, _ = io.Copy(io.Discard, conn)
		}
		return
	}

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4096), maxRequestLine)
	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(Response{OK: false, Code: CodeBadRequest, Error: "request is not valid JSON"})
			return
		}
		resp := s.dispatch(ctx, req)
		resp.ID = req.ID
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

// handler is one verb's implementation over the backend.
type handler func(s *Server, ctx context.Context, req Request) Response

// handlers is the closed dispatch table. TestVerbSetIsClosed holds it equal
// to Verbs() in both directions.
var handlers = map[Verb]handler{
	VerbStatus:      (*Server).doStatus,
	VerbStart:       (*Server).doStart,
	VerbStop:        (*Server).doStop,
	VerbRestart:     (*Server).doRestart,
	VerbConsoleSend: (*Server).doConsoleSend,
	VerbBackupNow:   (*Server).doBackupNow,
	VerbVerifyTOTP:  (*Server).doVerifyTOTP,
}

func (s *Server) dispatch(ctx context.Context, req Request) Response {
	h, ok := handlers[req.Verb]
	if !ok {
		return Response{OK: false, Code: CodeUnknownVerb,
			Error: fmt.Sprintf("%q is not a verb this gateway exposes", req.Verb)}
	}
	if resp, ok := validateFields(req); !ok {
		return resp
	}
	return h(s, ctx, req)
}

// validateFields refuses a field set on a verb that ignores it — a caller
// that believes it did something it did not (the hostops rule, kept here).
func validateFields(req Request) (Response, bool) {
	bad := func(field string) (Response, bool) {
		return Response{OK: false, Code: CodeBadRequest,
			Error: fmt.Sprintf("verb %s does not read %s", req.Verb, field)}, false
	}
	if req.Line != "" && req.Verb != VerbConsoleSend {
		return bad("line")
	}
	if (req.Email != "" || req.Code != "") && req.Verb != VerbVerifyTOTP {
		return bad("email/code")
	}
	if req.Instance != "" && req.Verb == VerbVerifyTOTP {
		return bad("instance")
	}
	return Response{}, true
}

// requireInstance is the shared argument check for the per-instance verbs.
func requireInstance(req Request) (Response, bool) {
	if strings.TrimSpace(req.Instance) == "" {
		return Response{OK: false, Code: CodeBadRequest,
			Error: fmt.Sprintf("verb %s needs an instance", req.Verb)}, false
	}
	return Response{}, true
}

func (s *Server) doStatus(ctx context.Context, req Request) Response {
	sts, err := s.backend.Statuses(ctx, req.Instance)
	if err != nil {
		return errorResponse(err)
	}
	if req.Instance != "" && len(sts) == 0 {
		return Response{OK: false, Code: CodeNotFound,
			Error: fmt.Sprintf("no instance %q on this host", req.Instance)}
	}
	return Response{OK: true, Statuses: sts}
}

func (s *Server) doStart(ctx context.Context, req Request) Response {
	return s.lifecycleVerb(ctx, req, s.backend.Start)
}

func (s *Server) doStop(ctx context.Context, req Request) Response {
	return s.lifecycleVerb(ctx, req, s.backend.Stop)
}

func (s *Server) doRestart(ctx context.Context, req Request) Response {
	return s.lifecycleVerb(ctx, req, s.backend.Restart)
}

// lifecycleVerb acknowledges only a dispatch systemd accepted: OK carries an
// op id AFTER the backend returned, never before (web-dashboard-2).
func (s *Server) lifecycleVerb(ctx context.Context, req Request, do func(context.Context, string) error) Response {
	if resp, ok := requireInstance(req); !ok {
		return resp
	}
	if err := do(ctx, req.Instance); err != nil {
		var lease *ErrLeaseHeld
		if errors.As(err, &lease) {
			return Response{OK: false, Code: CodeLeaseHeld, Lease: lease.Line, Error: err.Error()}
		}
		return errorResponse(err)
	}
	op := newOpID()
	s.log.Info("gvmd: dispatched", "verb", req.Verb, "instance", req.Instance, "actor", req.Actor, "op", op)
	return Response{OK: true, OpID: op}
}

func (s *Server) doConsoleSend(ctx context.Context, req Request) Response {
	if resp, ok := requireInstance(req); !ok {
		return resp
	}
	if req.Line == "" {
		return Response{OK: false, Code: CodeBadRequest, Error: "console.send needs a line"}
	}
	if len(req.Line) > MaxConsoleLine {
		return Response{OK: false, Code: CodeBadRequest,
			Error: fmt.Sprintf("console.send takes one line of at most %d bytes", MaxConsoleLine)}
	}
	if strings.ContainsAny(req.Line, "\r\n") {
		return Response{OK: false, Code: CodeBadRequest, Error: "console.send takes ONE line"}
	}
	if err := s.backend.ConsoleSend(ctx, req.Instance, req.Line); err != nil {
		return errorResponse(err)
	}
	s.log.Info("gvmd: console line sent", "instance", req.Instance, "actor", req.Actor)
	return Response{OK: true}
}

func (s *Server) doBackupNow(ctx context.Context, req Request) Response {
	if resp, ok := requireInstance(req); !ok {
		return resp
	}
	if err := s.backend.BackupNow(ctx, req.Instance); err != nil {
		return errorResponse(err)
	}
	op := newOpID()
	s.log.Info("gvmd: backup job started", "instance", req.Instance, "actor", req.Actor, "op", op)
	return Response{OK: true, OpID: op}
}

func (s *Server) doVerifyTOTP(ctx context.Context, req Request) Response {
	if req.Email == "" || req.Code == "" {
		return Response{OK: false, Code: CodeBadRequest, Error: "auth.verify_totp needs email and code"}
	}
	err := s.backend.VerifyTOTP(ctx, req.Email, req.Code)
	switch {
	case err == nil:
		return Response{OK: true}
	case errors.Is(err, identity.ErrInvalidTOTP):
		// The web tier shows one sentence for a wrong code and an unknown
		// account alike; detail here would be an oracle.
		return Response{OK: false, Code: CodeTOTPInvalid, Error: "the code is not valid"}
	case errors.Is(err, ErrTOTPNotEnrolled):
		return Response{OK: false, Code: CodeNoTOTP, Error: "the account has no authenticator enrolled"}
	default:
		return errorResponse(err)
	}
}

// errorResponse maps a backend error onto the degraded/internal split.
func errorResponse(err error) Response {
	if errors.Is(err, ErrDegraded) {
		return Response{OK: false, Code: CodeDegraded,
			Error: "the datastore is unavailable; game servers are not affected"}
	}
	return Response{OK: false, Code: CodeInternal, Error: err.Error()}
}
