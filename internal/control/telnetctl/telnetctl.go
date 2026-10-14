// Package telnetctl implements the telnet control channel (section 10.2).
//
// 7 Days to Die exposes a raw TCP line protocol on port 8081: each command is
// one newline-terminated string and the reply is one or more newline-terminated
// lines ending with a blank line. There is no authentication handshake and no
// framing — the gate (r.ControlGateOpen) prevents dialling before the server
// is ready to accept connections.
//
// The channel is stateless between commands: we open a connection, send one
// command, read until the blank-line sentinel, then close. This avoids the
// connection-lifetime complexity of rconctl and matches 7DTD's own telnet
// behaviour (it closes idle connections after ~30 s anyway).
package telnetctl

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

const (
	dialTimeout  = 5 * time.Second
	writeTimeout = 5 * time.Second
	readTimeout  = 30 * time.Second
)

// Options configures a Client.
type Options struct {
	// Addr is host:port for the telnet listener (typically 127.0.0.1:8081).
	Addr string

	// Gate reports whether the server is ready to accept connections. Required;
	// a nil gate is treated as always-closed.
	Gate func() bool

	// NotReadyReason is the manifest's control.not_ready_reason, returned
	// verbatim while the gate is closed.
	NotReadyReason string

	// Dialer replaces net.DialTimeout for tests.
	Dialer func(network, addr string, timeout time.Duration) (net.Conn, error)
}

// Client is a single-command telnet control channel.
type Client struct {
	addr   string
	gate   func() bool
	reason string
	dial   func(network, addr string, timeout time.Duration) (net.Conn, error)
	mu     sync.Mutex
}

// New returns a Client.
func New(o Options) (*Client, error) {
	if o.Addr == "" {
		return nil, fmt.Errorf("telnetctl: Addr is required")
	}
	d := o.Dialer
	if d == nil {
		d = net.DialTimeout
	}
	gate := o.Gate
	if gate == nil {
		gate = func() bool { return false }
	}
	return &Client{
		addr:   o.Addr,
		gate:   gate,
		reason: o.NotReadyReason,
		dial:   d,
	}, nil
}

func (c *Client) Kind() spec.ControlKind { return spec.ControlTelnet }

// Ready reports whether the gate is open.
func (c *Client) Ready() bool { return c.gate() }

// Unready returns the not-ready reason while the gate is closed.
func (c *Client) Unready() string {
	if c.gate() {
		return ""
	}
	if c.reason != "" {
		return c.reason
	}
	return "waiting for the server to finish starting up"
}

// Exec sends cmd over a fresh TCP connection and returns the server's reply.
func (c *Client) Exec(ctx context.Context, cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, err := c.dial("tcp", c.addr, dialTimeout)
	if err != nil {
		return "", fmt.Errorf("telnetctl: dial %s: %w", c.addr, err)
	}
	defer conn.Close()

	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return "", fmt.Errorf("telnetctl: set write deadline: %w", err)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return "", fmt.Errorf("telnetctl: write: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		return "", fmt.Errorf("telnetctl: set read deadline: %w", err)
	}

	var sb strings.Builder
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break // blank line is the end-of-reply sentinel
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(line)
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("telnetctl: read: %w", err)
	}
	return sb.String(), nil
}

// Close is a no-op: connections are opened per-command.
func (c *Client) Close() error { return nil }

// Snapshot implements control.Reporter.
func (c *Client) Snapshot() control.Status { return control.Describe(c) }

var _ control.Channel = (*Client)(nil)
