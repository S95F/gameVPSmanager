package socket

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
)

// Frame timing defaults.
const (
	// DefaultWriteTimeout bounds ONE frame write.
	//
	// The ring's drop policy protects the producer from a slow subscriber, but
	// nothing protects the fan-out goroutine from a peer that has stopped
	// reading its socket: without a deadline it blocks in write(2) forever,
	// holding a subscription that keeps a connection alive. The timeout is the
	// second half of "a reader that stopped reading loses its lines, not the
	// server".
	DefaultWriteTimeout = 10 * time.Second
	// DefaultHandshakeTimeout bounds the FIRST frame only. A follower is
	// legitimately silent for hours, so there is no idle read timeout; a peer
	// that connects and never speaks is a different thing, and it holds a
	// connection slot.
	DefaultHandshakeTimeout = 30 * time.Second
)

// Conn is one NDJSON connection: the console codec, plus the deadlines and the
// failure bookkeeping that a socket needs and a codec should not know about.
//
// Writes are safe from several goroutines -- the runner answers ops on one and
// streams lines on another -- because console.Encoder serialises them. Reads
// are not: exactly one goroutine calls ReadRequest or ReadEvent.
type Conn struct {
	rw  io.ReadWriteCloser
	enc *console.Encoder
	dec *console.Decoder

	// dl is nil for a transport with no deadlines, in which case a stalled
	// peer is bounded by nothing and the caller has chosen that.
	dl deadliner

	handshake time.Duration

	once sync.Once

	mu       sync.Mutex
	first    bool
	armed    bool
	broken   error
	maxFrame int
}

// deadliner is the deadline half of net.Conn. Both ends of a net.Pipe
// implement it, which is what makes the write-timeout path testable on the
// development machine.
type deadliner interface {
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// NewConn wraps a transport. writeTimeout and handshake take the defaults when
// non-positive; a negative value switches that deadline off.
func NewConn(rw io.ReadWriteCloser, writeTimeout, handshake time.Duration) *Conn {
	if writeTimeout == 0 {
		writeTimeout = DefaultWriteTimeout
	}
	if handshake == 0 {
		handshake = DefaultHandshakeTimeout
	}
	c := &Conn{rw: rw, dec: console.NewDecoder(rw), handshake: handshake, first: true,
		maxFrame: console.MaxEventFrame}
	if dl, ok := rw.(deadliner); ok {
		c.dl = dl
	}
	// The deadline is armed inside the writer, so it runs under the encoder's
	// own lock: two goroutines each setting a deadline before their own Write
	// would race to clobber each other's.
	c.enc = console.NewEncoder(&deadlineWriter{c: c, d: writeTimeout})
	return c
}

// deadlineWriter arms a per-frame write deadline immediately before the write
// it applies to.
type deadlineWriter struct {
	c *Conn
	d time.Duration
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	if w.c.dl != nil && w.d > 0 {
		_ = w.c.dl.SetWriteDeadline(time.Now().Add(w.d))
	}
	return w.c.rw.Write(p)
}

// SetMaxEventFrame overrides the outbound frame cap. Tests use it to prove the
// oversize-batch path without building a four-megabyte batch.
func (c *Conn) SetMaxEventFrame(n int) {
	c.mu.Lock()
	c.maxFrame = n
	c.mu.Unlock()
	c.enc.SetMaxFrame(n)
}

func (c *Conn) maxEventFrame() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxFrame
}

// WriteEvent writes one server frame.
//
// A frame refused for size leaves the stream intact -- the encoder checks the
// size before it writes a byte -- so it is reported and the connection lives.
// Any other write error may have left half a frame on the wire, which no
// reader can resynchronise from, so the connection is marked broken.
func (c *Conn) WriteEvent(ev console.Event) error {
	err := c.enc.Event(ev)
	if err != nil && !errors.Is(err, console.ErrFrameTooLarge) {
		c.fail(err)
	}
	return err
}

// WriteRequest writes one client frame, with the same failure rule.
func (c *Conn) WriteRequest(r console.Request) error {
	err := c.enc.Request(r)
	if err != nil && !errors.Is(err, console.ErrFrameTooLarge) {
		c.fail(err)
	}
	return err
}

// WriteLines emits a `lines` event, split so each frame stays under the cap.
//
// console.SplitBatch estimates; JSON escaping can beat an estimate (a line of
// control bytes inflates six-fold), and the encoder refuses rather than
// truncates. So a chunk the encoder still rejects is halved and retried, which
// terminates because a single line is bounded by the ingest's 64 KiB split and
// cannot exceed the 4 MiB cap even fully escaped.
func (c *Conn) WriteLines(head, oldest uint64, lines []console.Line) error {
	for _, chunk := range console.SplitBatch(lines, c.maxEventFrame()) {
		if err := c.writeChunk(head, oldest, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (c *Conn) writeChunk(head, oldest uint64, chunk []console.Line) error {
	err := c.WriteEvent(console.LinesEvent(head, oldest, chunk))
	if !errors.Is(err, console.ErrFrameTooLarge) || len(chunk) < 2 {
		return err
	}
	half := len(chunk) / 2
	if err := c.writeChunk(head, oldest, chunk[:half]); err != nil {
		return err
	}
	return c.writeChunk(head, oldest, chunk[half:])
}

// ReadRequest reads and validates one client frame. The returned request is
// filled in even when the error is a validation failure, so a server can name
// the offending op in the error event it sends back.
func (c *Conn) ReadRequest() (console.Request, error) {
	c.armRead()
	return c.dec.Request()
}

// ReadEvent reads one server frame.
func (c *Conn) ReadEvent() (console.Event, error) {
	c.armRead()
	return c.dec.Event()
}

// armRead applies the handshake deadline to the first frame and clears it once,
// on the second.
//
// A client that has said hello is allowed to be quiet for hours, and clearing
// a deadline that is not set would be a syscall per request frame for nothing.
func (c *Conn) armRead() {
	if c.dl == nil {
		return
	}
	c.mu.Lock()
	first, armed := c.first, c.armed
	c.first = false
	c.armed = first && c.handshake > 0
	c.mu.Unlock()

	switch {
	case first && c.handshake > 0:
		_ = c.dl.SetReadDeadline(time.Now().Add(c.handshake))
	case !first && armed:
		_ = c.dl.SetReadDeadline(time.Time{})
	}
}

// armCall applies a read deadline for the duration of one request/response
// call and returns the function that clears it.
//
// Deliberately not a connection-wide idle deadline: the handshake deadline is
// switched off for a client (NewConn with a negative value) because a
// following reader is allowed to be silent for hours, and this must not take
// that back. Only Client.Do arms it.
func (c *Conn) armCall(d time.Duration) func() {
	if c.dl == nil || d <= 0 {
		return func() {}
	}
	_ = c.dl.SetReadDeadline(time.Now().Add(d))
	return func() { _ = c.dl.SetReadDeadline(time.Time{}) }
}

// Err reports the write error that broke this connection, if any.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.broken
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.broken == nil {
		c.broken = err
	}
	c.mu.Unlock()
	// A half-written frame is unrecoverable for the peer, so the only honest
	// thing left is to drop the connection and let it reconnect at a cursor.
	c.Close()
}

// Close closes the transport once, whichever goroutine gets there first.
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() { err = c.rw.Close() })
	return err
}

// Gone reports whether an error means "the peer went away", as distinct from a
// protocol violation worth answering.
//
// A console client detaching with Ctrl-D and a runner exiting mid-stream are
// both ordinary; logging them as errors would bury the ones that are not.
func Gone(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	case errors.Is(err, net.ErrClosed), errors.Is(err, os.ErrClosed), errors.Is(err, io.ErrClosedPipe):
		return true
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return true
	default:
		return false
	}
}

// Timeout reports whether an error is a deadline firing, which is how a stalled
// peer surfaces.
func Timeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
