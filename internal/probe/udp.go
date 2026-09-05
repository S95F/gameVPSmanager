package probe

import (
	"context"
	"errors"
	"net"
	"os"
	"time"
)

// KindUDP is the manifest's `readiness.probes[].kind` for this probe.
const KindUDP = "udp_probe"

// DefaultUDPWindow is how long a UDP probe waits before concluding that
// silence means "listening".
const DefaultUDPWindow = 2 * time.Second

// UDP reports a port ready when a datagram sent to it is not rejected.
//
// Read that again, because it is the honest and slightly unsatisfying truth
// about probing UDP: THERE IS NO POSITIVE ACKNOWLEDGEMENT. Nothing in the
// protocol says "received". What a probe can observe is the NEGATIVE: when
// nothing is bound to the port, the host answers with ICMP port-unreachable,
// and a *connected* UDP socket surfaces that as an error on the next read.
// Silence is therefore the pass condition, and it is a weak one -- a dropped
// datagram, a firewall that eats ICMP, or a host that rate-limits unreachables
// all look exactly like a healthy server.
//
// That weakness is why every shipped manifest sequences `udp_probe` with
// `after: log_marker` (Valheim's does). The log marker says the game is up; the
// UDP probe adds the one thing a log line cannot prove -- that the socket
// really got bound, so a port collision with another instance shows up as a
// failed start instead of as players who cannot join.
//
// ExpectReply flips it into a real check for protocols that answer (A2S,
// server-list-ping in Phase 5). Then silence is a failure.
type UDP struct {
	// Port is the manifest port NAME, for the description.
	Port string

	// Addr is host:port.
	Addr string

	// Payload is the datagram. Zero-length datagrams are not delivered
	// consistently by every stack, and a longer payload risks being PARSED by
	// the game, so the default is one inert byte.
	Payload []byte

	// ExpectReply requires a datagram back. Off by default: most game ports
	// answer nothing to a byte they cannot parse, and a probe that demanded a
	// reply would fail forever on a perfectly healthy server.
	ExpectReply bool

	// Window is how long to wait for a rejection (or a reply). Zero means
	// DefaultUDPWindow.
	Window time.Duration

	// Timeout bounds the whole attempt including the dial. Zero defers to the
	// caller's context.
	Timeout time.Duration

	// Dial is the seam for tests. nil uses a real net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

var _ Probe = (*UDP)(nil)

// Kind implements Probe.
func (p *UDP) Kind() string { return KindUDP }

// Describe implements Probe.
func (p *UDP) Describe() string {
	what := "a UDP socket bound on " + p.Addr
	if p.ExpectReply {
		what = "a UDP reply from " + p.Addr
	}
	if p.Port != "" {
		what += " (port " + p.Port + ")"
	}
	return what
}

// Run implements Probe.
func (p *UDP) Run(ctx context.Context) error {
	ctx, cancel := withTimeout(ctx, p.Timeout)
	defer cancel()

	dial := p.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	// "udp" and not ListenPacket: only a CONNECTED socket is told about the
	// ICMP unreachable, which is the only negative signal that exists here.
	c, err := dial(ctx, "udp", p.Addr)
	if err != nil {
		return fail(KindUDP, p.Addr, "cannot open a udp socket", err)
	}
	defer func() { _ = c.Close() }()

	window := p.Window
	if window <= 0 {
		window = DefaultUDPWindow
	}
	deadline := time.Now().Add(window)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetDeadline(deadline)

	payload := p.Payload
	if payload == nil {
		payload = []byte{0}
	}
	if _, err := c.Write(payload); err != nil {
		// On Linux a previous unreachable can surface on the WRITE rather than
		// the read, so this path is a real answer and not just plumbing.
		return classify(KindUDP, p.Addr, 0, err, p.ExpectReply)
	}

	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	return classify(KindUDP, p.Addr, n, err, p.ExpectReply)
}

// classify turns a read/write outcome into the probe's verdict.
//
// It is a free function taking the outcome rather than a method doing the I/O,
// because this is the only genuinely platform-shaped decision in the package
// (Linux reports ECONNREFUSED, Windows reports WSAECONNRESET) and it has to be
// provably right on a box where neither can be produced on demand. The rule
// avoids naming either errno: a TIMEOUT means nothing rejected us, and
// ANY OTHER error means something did.
func classify(kind, addr string, n int, err error, expectReply bool) error {
	switch {
	case err == nil:
		if n == 0 && expectReply {
			return fail(kind, addr, "empty reply", nil)
		}
		return nil

	case isTimeout(err):
		if expectReply {
			return fail(kind, addr, "no reply within the window", nil)
		}
		// Nothing rejected the datagram. This is the pass, and it is weak on
		// purpose: see the type comment.
		return nil

	default:
		return fail(kind, addr, "rejected", err)
	}
}

// isTimeout reports a deadline expiry, including one that arrives wrapped in an
// *os.SyscallError or a *net.OpError.
func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
