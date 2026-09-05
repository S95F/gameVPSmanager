package probe

import (
	"context"
	"net"
	"time"
)

// KindTCP is the manifest's `readiness.probes[].kind` for this probe.
const KindTCP = "tcp_probe"

// TCP reports a port ready when a TCP connection to it completes.
//
// It connects and closes without writing a byte, and that is not laziness. The
// probe has no idea what protocol is on the other side: a game's own handshake
// parser reading an unexpected byte is at best a warning in the server log and
// at worst a disconnect loop the operator gets blamed for. A completed
// three-way handshake is the whole answer to "is something listening".
//
// What it cannot tell you is whether the SERVER is ready -- only that its
// listener is bound. Games bind early and load late, which is why the manifests
// sequence a tcp_probe AFTER a log marker rather than instead of one.
type TCP struct {
	// Port is the manifest port NAME, for the description an operator reads.
	// Empty is fine; the address is always shown.
	Port string

	// Addr is host:port.
	Addr string

	// Timeout bounds one attempt. Zero defers to the caller's context.
	Timeout time.Duration

	// Dial is the seam for tests. nil uses a real net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

var _ Probe = (*TCP)(nil)

// Kind implements Probe.
func (p *TCP) Kind() string { return KindTCP }

// Describe implements Probe.
func (p *TCP) Describe() string {
	if p.Port != "" {
		return "a TCP listener on " + p.Addr + " (port " + p.Port + ")"
	}
	return "a TCP listener on " + p.Addr
}

// Run implements Probe.
func (p *TCP) Run(ctx context.Context) error {
	ctx, cancel := withTimeout(ctx, p.Timeout)
	defer cancel()

	dial := p.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	c, err := dial(ctx, "tcp", p.Addr)
	if err != nil {
		return fail(KindTCP, p.Addr, "connect failed", err)
	}
	// The close error is discarded deliberately: the handshake completing is
	// the entire result, and a failure to close a socket we are done with says
	// nothing about whether the game is ready.
	_ = c.Close()
	return nil
}
