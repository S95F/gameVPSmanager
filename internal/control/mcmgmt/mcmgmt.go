// Package mcmgmt implements the MCMgmt control channel (section 10.2).
//
// MCMgmt is a JSON-RPC 2.0 channel exposed by the Paper/Fabric/Purpur
// management sidecar over a plain TCP connection (not WebSocket). Commands
// are newline-delimited JSON objects and replies are newline-delimited JSON
// objects. The channel is primarily used by minecraft-paper manifests for
// save-world and stop commands.
//
// Like telnetctl, the channel is stateless between calls: each Exec opens a
// fresh connection, sends one JSON-RPC request, reads the response, then
// closes. The gate (r.ControlGateOpen) prevents dialling before the server is
// ready.
package mcmgmt

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
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
	// Addr is host:port for the MCMgmt listener (typically 127.0.0.1:25575).
	Addr string

	// Gate reports whether the server is ready. Required.
	Gate func() bool

	// NotReadyReason is returned verbatim while the gate is closed.
	NotReadyReason string

	// Dialer replaces net.DialTimeout for tests.
	Dialer func(network, addr string, timeout time.Duration) (net.Conn, error)
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      int64  `json:"id"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	ID      int64           `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Client is the MCMgmt control channel.
type Client struct {
	addr   string
	gate   func() bool
	reason string
	dial   func(network, addr string, timeout time.Duration) (net.Conn, error)
	mu     sync.Mutex
	seq    atomic.Int64
}

// New returns a Client.
func New(o Options) (*Client, error) {
	if o.Addr == "" {
		return nil, fmt.Errorf("mcmgmt: Addr is required")
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

func (c *Client) Kind() spec.ControlKind { return spec.ControlMCMgmt }

func (c *Client) Ready() bool { return c.gate() }

func (c *Client) Unready() string {
	if c.gate() {
		return ""
	}
	if c.reason != "" {
		return c.reason
	}
	return "waiting for the Minecraft server to finish starting up"
}

// Exec sends cmd as a JSON-RPC "exec" call and returns the result string.
func (c *Client) Exec(ctx context.Context, cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, err := c.dial("tcp", c.addr, dialTimeout)
	if err != nil {
		return "", fmt.Errorf("mcmgmt: dial %s: %w", c.addr, err)
	}
	defer conn.Close()

	id := c.seq.Add(1)
	req := rpcRequest{
		JSONRPC: "2.0",
		Method:  "exec",
		Params:  []string{cmd},
		ID:      id,
	}
	enc, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("mcmgmt: marshal: %w", err)
	}
	enc = append(enc, '\n')

	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return "", fmt.Errorf("mcmgmt: set write deadline: %w", err)
	}
	if _, err := conn.Write(enc); err != nil {
		return "", fmt.Errorf("mcmgmt: write: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		return "", fmt.Errorf("mcmgmt: set read deadline: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("mcmgmt: read: %w", err)
		}
		return "", fmt.Errorf("mcmgmt: connection closed before response")
	}

	var resp rpcResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("mcmgmt: decode response: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("mcmgmt: rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	var result string
	if len(resp.Result) > 0 {
		_ = json.Unmarshal(resp.Result, &result)
	}
	return result, nil
}

// Close is a no-op: connections are opened per-command.
func (c *Client) Close() error { return nil }

// Snapshot implements control.Reporter.
func (c *Client) Snapshot() control.Status { return control.Describe(c) }

var _ control.Channel = (*Client)(nil)
