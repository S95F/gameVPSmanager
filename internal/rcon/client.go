// Package rcon implements the Source RCON protocol and the per-game
// deviations from it that real dedicated servers exhibit.
//
// The protocol itself is trivial. What is not trivial, and what this package
// exists to encapsulate, is that no two servers agree on how a client learns
// that a multi-packet response has ended. Getting that wrong produces the
// most common failure in RCON clients: a command appears to work, but its
// output is silently truncated at 4096 bytes, or the client blocks forever
// waiting for a terminator the server will never send. See dialect.go.
package rcon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

var (
	// ErrAuthFailed means the server rejected the password. Servers normally
	// close the connection immediately afterwards, so the client is dead.
	ErrAuthFailed = errors.New("rcon: authentication failed")

	// ErrCommandTooLong means the command exceeds what this dialect accepts.
	ErrCommandTooLong = errors.New("rcon: command too long for this server")

	// ErrClosed means the client has already been closed.
	ErrClosed = errors.New("rcon: client is closed")
)

// Default timings. Deliberately generous: a game server that is saving a
// large world can stall its RCON thread for seconds.
const (
	defaultDialTimeout = 10 * time.Second
	defaultTimeout     = 30 * time.Second
	defaultIdleWindow  = 400 * time.Millisecond
	trailingJunkWindow = 250 * time.Millisecond
)

// Options configures a Client.
type Options struct {
	// Dialect selects the quirk set. Zero value means DialectSource.
	Dialect Dialect
	// DialTimeout bounds the TCP connect plus the auth handshake.
	DialTimeout time.Duration
	// Timeout bounds a single Execute call.
	Timeout time.Duration
	// IdleWindow is how long DialectSimple waits for more output before
	// deciding a response is complete. Ignored by the other dialects.
	IdleWindow time.Duration
}

func (o Options) withDefaults() Options {
	if o.Dialect == "" {
		o.Dialect = DialectSource
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = defaultDialTimeout
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.IdleWindow <= 0 {
		o.IdleWindow = defaultIdleWindow
	}
	return o
}

// Client is a single authenticated RCON connection.
//
// RCON has no request multiplexing: responses are correlated only by their
// position on the wire, so exactly one command may be in flight at a time.
// Execute serializes callers rather than making that the caller's problem.
type Client struct {
	opts Options
	spec dialectSpec

	mu     sync.Mutex
	conn   net.Conn
	br     *bufio.Reader
	nextID int32
	closed bool
}

// Dial connects, authenticates, and returns a ready client.
func Dial(ctx context.Context, addr, password string, opts Options) (*Client, error) {
	opts = opts.withDefaults()

	dialCtx, cancel := context.WithTimeout(ctx, opts.DialTimeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("rcon: connecting to %s: %w", addr, err)
	}

	c := newClient(conn, opts)
	if err := c.authenticate(dialCtx, password); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func newClient(conn net.Conn, opts Options) *Client {
	return &Client{
		opts:   opts,
		spec:   specFor(opts.Dialect),
		conn:   conn,
		br:     bufio.NewReaderSize(conn, maxPacketSize),
		nextID: 1,
	}
}

// Close releases the connection. It is safe to call more than once.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

// Dialect reports the quirk set in use.
func (c *Client) Dialect() Dialect { return c.opts.Dialect }

// nextRequestID returns a fresh positive id. Zero is avoided so that a zeroed
// struct is never mistaken for a real correlation, and -1 is avoided because
// servers use it to signal auth failure.
func (c *Client) nextRequestID() int32 {
	c.nextID++
	if c.nextID <= 0 {
		c.nextID = 1
	}
	return c.nextID
}

func (c *Client) authenticate(ctx context.Context, password string) error {
	id := c.nextRequestID()
	if err := c.write(packet{ID: id, Type: TypeAuth, Body: []byte(password)}); err != nil {
		return err
	}

	deadline := c.deadline(ctx, c.opts.DialTimeout)
	for {
		p, err := c.read(deadline)
		if err != nil {
			// A closed connection during the handshake is how most servers
			// say "wrong password", after the -1 packet or instead of it.
			if errors.Is(err, net.ErrClosed) || isEOF(err) {
				return ErrAuthFailed
			}
			return fmt.Errorf("rcon: reading auth response: %w", err)
		}

		if c.spec.shouldDiscard(p.Type) {
			continue
		}
		// Some servers precede the auth response with an empty
		// RESPONSE_VALUE and some omit it. Tolerate both.
		if p.Type == TypeResponseValue {
			continue
		}
		if p.Type == TypeAuthResponse {
			if p.ID == authFailureID {
				return ErrAuthFailed
			}
			if p.ID == id {
				return nil
			}
			return fmt.Errorf("rcon: auth response id %d does not match request id %d", p.ID, id)
		}
		return fmt.Errorf("rcon: unexpected packet type %d during auth", p.Type)
	}
}

// Execute runs one console command and returns its complete output.
func (c *Client) Execute(ctx context.Context, cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return "", ErrClosed
	}
	if len(cmd) > c.spec.maxRequestBody {
		return "", fmt.Errorf("%w: %d bytes, limit %d", ErrCommandTooLong, len(cmd), c.spec.maxRequestBody)
	}

	cmdID := c.nextRequestID()
	if err := c.write(packet{ID: cmdID, Type: TypeExecCommand, Body: []byte(cmd)}); err != nil {
		return "", err
	}

	// Send the terminator probe immediately, before reading anything. The
	// server answers strictly in receive order, so its reply to the probe is
	// guaranteed to arrive after every fragment of the real response.
	//
	// termID stays 0 for the idle dialect, which sends no probe. Request ids
	// always start at 2, so 0 can never collide with a real response.
	var termID int32
	switch c.spec.terminator {
	case termMirror:
		termID = c.nextRequestID()
		if err := c.write(packet{ID: termID, Type: TypeResponseValue}); err != nil {
			return "", err
		}
	case termSecondCommand:
		termID = c.nextRequestID()
		if err := c.write(packet{ID: termID, Type: TypeExecCommand}); err != nil {
			return "", err
		}
	}

	var out bytes.Buffer
	deadline := c.deadline(ctx, c.opts.Timeout)

	for {
		readBy := deadline
		if c.spec.terminator == termIdle && out.Len() > 0 {
			// Nothing is coming to say the response ended, so a gap in output
			// has to stand in for the end of it.
			if idle := time.Now().Add(c.opts.IdleWindow); idle.Before(readBy) {
				readBy = idle
			}
		}

		p, err := c.read(readBy)
		if err != nil {
			if isTimeout(err) && (out.Len() > 0 || c.spec.terminator == termIdle) {
				// Either an idle-terminated dialect finishing normally, or a
				// server that never answered the probe. Output already
				// received is the answer; returning it beats erroring out.
				return decodeText(out.Bytes()), nil
			}
			return "", fmt.Errorf("rcon: reading response to %q: %w", cmd, err)
		}

		if c.spec.shouldDiscard(p.Type) {
			continue
		}
		if c.spec.remapNegativeID && p.ID == authFailureID {
			p.ID = cmdID
		}

		switch p.ID {
		case cmdID:
			out.Write(p.Body)
		case termID:
			c.swallowTrailingJunk()
			return decodeText(out.Bytes()), nil
		default:
			// A stale response from an earlier command, or a broadcast. Not
			// ours; dropping it keeps the stream aligned.
		}
	}
}

// swallowTrailingJunk consumes the malformed packet SRCDS emits after the
// mirrored terminator. Leaving it in the buffer would desynchronise the very
// next Execute on this connection.
func (c *Client) swallowTrailingJunk() {
	for i := 0; i < c.spec.trailingJunk; i++ {
		if _, err := c.read(time.Now().Add(trailingJunkWindow)); err != nil {
			return
		}
	}
}

func (c *Client) write(p packet) error {
	b, err := p.encode()
	if err != nil {
		return err
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.opts.Timeout)); err != nil {
		return err
	}
	if _, err := c.conn.Write(b); err != nil {
		return fmt.Errorf("rcon: writing packet: %w", err)
	}
	return nil
}

func (c *Client) read(deadline time.Time) (packet, error) {
	if err := c.conn.SetReadDeadline(deadline); err != nil {
		return packet{}, err
	}
	return readPacket(c.br)
}

func (c *Client) deadline(ctx context.Context, fallback time.Duration) time.Time {
	d := time.Now().Add(fallback)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(d) {
		return ctxDeadline
	}
	return d
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func isEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
