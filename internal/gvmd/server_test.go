package gvmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/identity"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// recorder is a Backend whose every call is written down.
type recorder struct {
	calls []string
	// failures
	leaseHeld  bool
	totpErr    error
	enrollErr  error
	statusErr  error
	lifecycErr error
}

func (r *recorder) backend() Backend {
	return Backend{
		Statuses: func(_ context.Context, instance string) ([]lifecycle.Status, error) {
			r.calls = append(r.calls, "statuses "+instance)
			if r.statusErr != nil {
				return nil, r.statusErr
			}
			if instance != "" && instance != "pz1" {
				return nil, nil
			}
			return []lifecycle.Status{{Name: "pz1", Game: "projectzomboid"}}, nil
		},
		Start:   func(_ context.Context, in string) error { return r.lifecycle("start", in) },
		Stop:    func(_ context.Context, in string) error { return r.lifecycle("stop", in) },
		Restart: func(_ context.Context, in string) error { return r.lifecycle("restart", in) },
		ConsoleSend: func(_ context.Context, in, line string) error {
			r.calls = append(r.calls, "send "+in+" "+line)
			return nil
		},
		BackupNow: func(_ context.Context, in string) error {
			r.calls = append(r.calls, "backup "+in)
			return nil
		},
		VerifyTOTP: func(_ context.Context, email, code string) error {
			r.calls = append(r.calls, "totp "+email)
			return r.totpErr
		},
		Enroll: func(_ context.Context, invite, hash, secret string) error {
			r.calls = append(r.calls, "enroll "+invite)
			return r.enrollErr
		},
	}
}

func (r *recorder) lifecycle(verb, instance string) error {
	r.calls = append(r.calls, verb+" "+instance)
	if r.leaseHeld {
		return &ErrLeaseHeld{Line: "backup running since 05:00"}
	}
	return r.lifecycErr
}

func newTestServer(t *testing.T, r *recorder) *Server {
	t.Helper()
	s, err := NewServer(r.backend(),
		func(net.Conn) (platform.Peer, error) { return platform.Peer{PID: 1, UID: 0}, nil },
		nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestVerbSetIsClosed holds the dispatch table to Verbs() in both directions:
// a handler nobody declared is an undeclared privileged operation.
func TestVerbSetIsClosed(t *testing.T) {
	declared := map[Verb]bool{}
	for _, v := range Verbs() {
		declared[v] = true
		if handlers[v] == nil {
			t.Errorf("verb %s is declared and has no handler", v)
		}
	}
	for v := range handlers {
		if !declared[v] {
			t.Errorf("handler for %s exists but the verb is not declared", v)
		}
	}
}

func TestDispatchStatus(t *testing.T) {
	r := &recorder{}
	s := newTestServer(t, r)

	resp := s.dispatch(context.Background(), Request{Verb: VerbStatus})
	if !resp.OK || len(resp.Statuses) != 1 || resp.Statuses[0].Name != "pz1" {
		t.Fatalf("status: %+v", resp)
	}

	resp = s.dispatch(context.Background(), Request{Verb: VerbStatus, Instance: "typo"})
	if resp.OK || resp.Code != CodeNotFound {
		t.Errorf("unknown instance: %+v, want not_found", resp)
	}
}

func TestDispatchLifecycle(t *testing.T) {
	r := &recorder{}
	s := newTestServer(t, r)

	resp := s.dispatch(context.Background(), Request{Verb: VerbRestart, Instance: "pz1", Actor: "user:alice"})
	if !resp.OK || resp.OpID == "" {
		t.Fatalf("restart: %+v, want OK with an op id", resp)
	}
	if len(r.calls) != 1 || r.calls[0] != "restart pz1" {
		t.Errorf("calls = %v", r.calls)
	}

	// Missing instance is refused before the backend is touched.
	r.calls = nil
	resp = s.dispatch(context.Background(), Request{Verb: VerbStart})
	if resp.OK || resp.Code != CodeBadRequest || len(r.calls) != 0 {
		t.Errorf("start with no instance: %+v calls=%v", resp, r.calls)
	}
}

// TestLeaseRefusalCarriesTheLine: a held instance answers lease_held with the
// holder's line, never a dispatch (the web tier renders it as 409).
func TestLeaseRefusalCarriesTheLine(t *testing.T) {
	r := &recorder{leaseHeld: true}
	s := newTestServer(t, r)
	resp := s.dispatch(context.Background(), Request{Verb: VerbStop, Instance: "pz1"})
	if resp.OK || resp.Code != CodeLeaseHeld {
		t.Fatalf("stop under lease: %+v", resp)
	}
	if !strings.Contains(resp.Lease, "backup running") {
		t.Errorf("lease line missing: %+v", resp)
	}
	if resp.OpID != "" {
		t.Error("a refusal must not carry an op id")
	}
}

func TestDispatchConsoleSend(t *testing.T) {
	r := &recorder{}
	s := newTestServer(t, r)

	resp := s.dispatch(context.Background(), Request{Verb: VerbConsoleSend, Instance: "pz1", Line: "say hello"})
	if !resp.OK {
		t.Fatalf("send: %+v", resp)
	}

	cases := []Request{
		{Verb: VerbConsoleSend, Instance: "pz1"},                                              // no line
		{Verb: VerbConsoleSend, Instance: "pz1", Line: strings.Repeat("a", MaxConsoleLine+1)}, // too long
		{Verb: VerbConsoleSend, Instance: "pz1", Line: "two\nlines"},                          // not one line
	}
	for _, req := range cases {
		if resp := s.dispatch(context.Background(), req); resp.OK || resp.Code != CodeBadRequest {
			t.Errorf("%q: %+v, want bad_request", req.Line, resp)
		}
	}
}

func TestDispatchVerifyTOTP(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"valid", nil, ""},
		{"wrong code", identity.ErrInvalidTOTP, CodeTOTPInvalid},
		{"not enrolled", ErrTOTPNotEnrolled, CodeNoTOTP},
		{"postgres down", ErrDegraded, CodeDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{totpErr: tc.err}
			s := newTestServer(t, r)
			resp := s.dispatch(context.Background(), Request{Verb: VerbVerifyTOTP, Email: "a@b.c", Code: "123456"})
			if tc.code == "" {
				if !resp.OK {
					t.Fatalf("%+v", resp)
				}
				return
			}
			if resp.OK || resp.Code != tc.code {
				t.Errorf("%+v, want code %s", resp, tc.code)
			}
		})
	}
}

func TestDispatchEnroll(t *testing.T) {
	r := &recorder{}
	s := newTestServer(t, r)

	resp := s.dispatch(context.Background(), Request{
		Verb: VerbEnroll, Invite: "gvmi_x", PasswordHash: "$argon2id$...", TOTPSecret: "SECRET",
	})
	if !resp.OK || len(r.calls) != 1 {
		t.Fatalf("enroll: %+v calls=%v", resp, r.calls)
	}

	// A dead invite answers its own code, and a partial request is refused
	// before the backend sees it.
	r = &recorder{enrollErr: ErrInviteInvalid}
	s = newTestServer(t, r)
	resp = s.dispatch(context.Background(), Request{
		Verb: VerbEnroll, Invite: "gvmi_x", PasswordHash: "h", TOTPSecret: "s",
	})
	if resp.OK || resp.Code != CodeInviteInvalid {
		t.Errorf("dead invite: %+v, want invite_invalid", resp)
	}
	r.calls = nil
	resp = s.dispatch(context.Background(), Request{Verb: VerbEnroll, Invite: "gvmi_x"})
	if resp.OK || resp.Code != CodeBadRequest || len(r.calls) != 0 {
		t.Errorf("partial enroll: %+v calls=%v, want bad_request before the backend", resp, r.calls)
	}
}

// TestCrossFieldDiscipline: a field set on a verb that ignores it is refused,
// so a confused caller cannot half-succeed.
func TestCrossFieldDiscipline(t *testing.T) {
	r := &recorder{}
	s := newTestServer(t, r)
	cases := []Request{
		{Verb: VerbStatus, Line: "say hi"},
		{Verb: VerbStart, Instance: "pz1", Email: "a@b.c"},
		{Verb: VerbVerifyTOTP, Email: "a@b.c", Code: "1", Instance: "pz1"},
	}
	for _, req := range cases {
		if resp := s.dispatch(context.Background(), req); resp.OK || resp.Code != CodeBadRequest {
			t.Errorf("%+v: %+v, want bad_request", req, resp)
		}
	}
	if len(r.calls) != 0 {
		t.Errorf("a refused request reached the backend: %v", r.calls)
	}
}

func TestDispatchUnknownVerb(t *testing.T) {
	s := newTestServer(t, &recorder{})
	resp := s.dispatch(context.Background(), Request{Verb: "fs.write"})
	if resp.OK || resp.Code != CodeUnknownVerb {
		t.Errorf("%+v, want unknown_verb", resp)
	}
}

// ── The wire ─────────────────────────────────────────────────────────────────

// serveTCP runs the server on a loopback TCP listener (the transport is
// seam-identified, so the tests run on every development platform; production
// listens on the unix socket and identifies with SO_PEERCRED).
func serveTCP(t *testing.T, s *Server) net.Addr {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr()
}

func roundTrip(t *testing.T, addr net.Addr, reqs ...Request) []Response {
	t.Helper()
	conn, err := net.DialTimeout(addr.Network(), addr.String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	enc := json.NewEncoder(conn)
	sc := bufio.NewScanner(conn)
	var out []Response
	for _, req := range reqs {
		if err := enc.Encode(req); err != nil {
			t.Fatal(err)
		}
		if !sc.Scan() {
			t.Fatalf("no response: %v", sc.Err())
		}
		var resp Response
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		out = append(out, resp)
	}
	return out
}

func TestServeAnswersOverTheWire(t *testing.T) {
	r := &recorder{}
	s := newTestServer(t, r)
	addr := serveTCP(t, s)

	resps := roundTrip(t, addr,
		Request{ID: "a", Verb: VerbStatus},
		Request{ID: "b", Verb: VerbStart, Instance: "pz1"},
	)
	if !resps[0].OK || resps[0].ID != "a" || len(resps[0].Statuses) != 1 {
		t.Errorf("status over the wire: %+v", resps[0])
	}
	if !resps[1].OK || resps[1].ID != "b" || resps[1].OpID == "" {
		t.Errorf("start over the wire: %+v", resps[1])
	}
}

// TestServeRefusesUnauthorizedPeer: a peer that is neither root nor the web
// account gets one refusal and a closed connection — before any verb runs.
func TestServeRefusesUnauthorizedPeer(t *testing.T) {
	r := &recorder{}
	s, err := NewServer(r.backend(),
		func(net.Conn) (platform.Peer, error) { return platform.Peer{PID: 1, UID: 1000}, nil },
		func(uid uint32) bool { return uid == 33 }, // the web account is 33, not 1000
		nil)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveTCP(t, s)

	resps := roundTrip(t, addr, Request{Verb: VerbStatus})
	if resps[0].OK || resps[0].Code != CodeUnauthorized {
		t.Errorf("%+v, want unauthorized", resps[0])
	}
	if len(r.calls) != 0 {
		t.Errorf("an unauthorized peer reached the backend: %v", r.calls)
	}
}

// TestClientRoundTrip drives the real Client against the server on a unix
// socket, as production does.
func TestClientRoundTrip(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets not available here: %v", err)
	}
	r := &recorder{}
	s := newTestServer(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })

	c := &Client{Path: path, Timeout: 5 * time.Second}
	sts, err := c.Statuses(context.Background(), "")
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	if len(sts) != 1 || sts[0].Name != "pz1" {
		t.Errorf("statuses = %+v", sts)
	}

	op, err := c.Lifecycle(context.Background(), VerbRestart, "pz1", "user:alice")
	if err != nil || op == "" {
		t.Errorf("Lifecycle: op=%q err=%v", op, err)
	}

	if err := c.VerifyTOTP(context.Background(), "a@b.c", "123456"); err != nil {
		t.Errorf("VerifyTOTP: %v", err)
	}

	var remote *RemoteError
	_, err = c.Lifecycle(context.Background(), VerbStatus, "pz1", "")
	if err == nil {
		t.Error("Lifecycle(status) should refuse: not a lifecycle verb")
	}
	r.totpErr = identity.ErrInvalidTOTP
	err = c.VerifyTOTP(context.Background(), "a@b.c", "000000")
	if !errors.As(err, &remote) || remote.Code != CodeTOTPInvalid {
		t.Errorf("wrong code: %v, want RemoteError totp_invalid", err)
	}
}

// TestClientGatewayDown: a dead socket is ErrGatewayDown, the signal the web
// tier maps to its degraded answer.
func TestClientGatewayDown(t *testing.T) {
	c := &Client{Path: shortSocketPath(t), Timeout: time.Second}
	_, err := c.Statuses(context.Background(), "")
	if !errors.Is(err, ErrGatewayDown) {
		t.Errorf("err = %v, want ErrGatewayDown", err)
	}
}

// shortSocketPath returns a path short enough for AF_UNIX's ~100-byte limit;
// t.TempDir can exceed it on machines with deep temp paths.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/g.sock"
	if len(path) > 90 {
		t.Skipf("temp path too long for AF_UNIX: %d bytes", len(path))
	}
	return path
}
