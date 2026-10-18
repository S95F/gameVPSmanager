package gvmd

// The client gvm-web holds. One connection per call: the dashboard's call
// rate is a human's click rate, and a connection pool would be complexity
// spent making the unmeasured faster.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// DefaultCallTimeout bounds one round trip. A stop can legitimately walk a
// long shutdown ladder, but the DISPATCH acknowledgement is quick; anything
// slower than this is a gateway that is not answering.
const DefaultCallTimeout = 30 * time.Second

// ErrGatewayDown means the admin socket did not answer: gvmd is not running.
var ErrGatewayDown = errors.New("gvmd: the verb gateway is not reachable")

// RemoteError is a non-OK Response as an error, carrying the machine code.
type RemoteError struct {
	Code    string
	Message string
	Lease   string
}

func (e *RemoteError) Error() string {
	if e.Message != "" {
		return "gvmd: " + e.Message
	}
	return "gvmd: " + e.Code
}

// Client dials the admin socket per call.
type Client struct {
	// Path is the admin socket. The caller supplies it (the CLI owns the
	// constant; tests point it at a temporary socket).
	Path string
	// Timeout bounds one call; zero means DefaultCallTimeout.
	Timeout time.Duration
}

// Call performs one request/response round trip.
func (c *Client) Call(ctx context.Context, req Request) (Response, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrGatewayDown, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("gvmd: send: %w", err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4096), 1024*1024)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return Response{}, fmt.Errorf("gvmd: read: %w", err)
		}
		return Response{}, fmt.Errorf("%w: connection closed before a response", ErrGatewayDown)
	}
	var resp Response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return Response{}, fmt.Errorf("gvmd: decode: %w", err)
	}
	return resp, nil
}

// call wraps Call, turning a non-OK response into a *RemoteError.
func (c *Client) call(ctx context.Context, req Request) (Response, error) {
	resp, err := c.Call(ctx, req)
	if err != nil {
		return Response{}, err
	}
	if !resp.OK {
		return Response{}, &RemoteError{Code: resp.Code, Message: resp.Error, Lease: resp.Lease}
	}
	return resp, nil
}

// Statuses returns every instance's status, or one instance's.
func (c *Client) Statuses(ctx context.Context, instance string) ([]lifecycle.Status, error) {
	resp, err := c.call(ctx, Request{Verb: VerbStatus, Instance: instance})
	if err != nil {
		return nil, err
	}
	return resp.Statuses, nil
}

// Lifecycle dispatches one of start/stop/restart and returns the op id.
func (c *Client) Lifecycle(ctx context.Context, verb Verb, instance, actor string) (string, error) {
	switch verb {
	case VerbStart, VerbStop, VerbRestart:
	default:
		return "", fmt.Errorf("gvmd: %q is not a lifecycle verb", verb)
	}
	resp, err := c.call(ctx, Request{Verb: verb, Instance: instance, Actor: actor})
	if err != nil {
		return "", err
	}
	return resp.OpID, nil
}

// ConsoleSend writes one line to the instance's console.
func (c *Client) ConsoleSend(ctx context.Context, instance, line, actor string) error {
	_, err := c.call(ctx, Request{Verb: VerbConsoleSend, Instance: instance, Line: line, Actor: actor})
	return err
}

// VerifyTOTP checks a login code.
func (c *Client) VerifyTOTP(ctx context.Context, email, code string) error {
	_, err := c.call(ctx, Request{Verb: VerbVerifyTOTP, Email: email, Code: code})
	return err
}

// Enroll redeems an invite with an already-hashed password and the TOTP
// secret to seal.
func (c *Client) Enroll(ctx context.Context, invite, passwordHash, totpSecret string) error {
	_, err := c.call(ctx, Request{Verb: VerbEnroll, Invite: invite, PasswordHash: passwordHash, TOTPSecret: totpSecret})
	return err
}
