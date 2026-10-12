package socket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
)

// DefaultDialTimeout bounds a connect to a local unix socket. A runner that is
// alive answers instantly; anything slower is a runner that is not.
const DefaultDialTimeout = 5 * time.Second

// DefaultCallTimeout bounds one request/response round trip.
//
// It exists because the runner can be alive and still never answer: a
// `console.OpSend` is dispatched synchronously on the connection's own
// goroutine and ends in a write to the pty master with no deadline, so a
// blocked pty write wedges the dispatcher and no `ev:sent` is ever written.
// With no bound the caller waits for TimeoutStartSec -- four hours for a job,
// holding the instance lock and lease.json against the next nightly.
//
// It bounds Do and therefore every call built on it, and NOT Next: a
// `tail follow:true` reader is legitimately silent for as long as an operator
// leaves `gvm console` open, which is why NewClient switches the handshake
// deadline off in the first place.
//
// Sixty seconds rather than five: a `verb` can legitimately take a while on a
// loaded box, and the point of this number is to turn "forever" into
// "eventually", not to second-guess the game.
const DefaultCallTimeout = 60 * time.Second

// ProtocolError is an `ev:error` frame the server sent back. Code is one of
// console's closed set, so a caller branches on the code and renders only the
// sentence.
type ProtocolError struct {
	Code string
	Msg  string
}

func (e *ProtocolError) Error() string { return e.Code + ": " + e.Msg }

// Denied reports whether err is the server refusing on authorization grounds
// -- the D-09 answer a dashboard turns into a disabled button rather than a
// retry.
func Denied(err error) bool {
	var pe *ProtocolError
	return errors.As(err, &pe) && pe.Code == console.CodeDenied
}

// ErrWrongEvent means the server answered with a frame this call was not
// waiting for and could not hand to a handler.
var ErrWrongEvent = errors.New("socket: unexpected event")

// Client speaks the console protocol.
//
// It is deliberately half-synchronous. Request/response calls (Hello, Ping,
// Status, Send) read until their own reply arrives; Tail does not wait at all,
// because a `tail` on an empty ring is answered by nothing until the game
// prints a line, and a client that blocked on it would hang on a healthy
// server. A following client therefore writes with Request and reads with
// Next from its own goroutine.
//
// One goroutine reads; any number may write.
type Client struct {
	c *Conn

	instance string
	build    string
	peer     console.PeerInfo

	// mu guards callTimeout only. A caller may narrow the bound per call --
	// internal/cli's freeze channel derives it from the job's remaining
	// context -- and the field is read by Do, so the two are serialised even
	// though the surrounding protocol is not.
	mu          sync.Mutex
	callTimeout time.Duration
}

// Dial connects to an instance's console socket.
func Dial(ctx context.Context, path string) (*Client, error) {
	d := net.Dialer{Timeout: DefaultDialTimeout}
	nc, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("socket: dial %s: %w", path, err)
	}
	return NewClient(nc), nil
}

// NewClient wraps an established transport, which is how tests drive the
// client over a net.Pipe and how a caller that already holds a connection
// (an accepted fd, a test double) reuses one.
func NewClient(rw io.ReadWriteCloser) *Client {
	return &Client{c: NewConn(rw, 0, -1), callTimeout: DefaultCallTimeout}
}

// SetCallTimeout narrows or widens the bound Do applies to one call's reads.
// Zero or negative switches it off, which is only ever right for a caller that
// has its own ceiling.
func (c *Client) SetCallTimeout(d time.Duration) {
	c.mu.Lock()
	c.callTimeout = d
	c.mu.Unlock()
}

// CallTimeout reports the current bound.
func (c *Client) CallTimeout() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callTimeout
}

// Conn exposes the framing, for a caller that needs raw frames.
func (c *Client) Conn() *Conn { return c.c }

// Close closes the connection.
func (c *Client) Close() error { return c.c.Close() }

// Instance, Build and Peer report what the hello event said. They are zero
// until Hello has been called.
func (c *Client) Instance() string       { return c.instance }
func (c *Client) Build() string          { return c.build }
func (c *Client) Peer() console.PeerInfo { return c.peer }

// Request writes one frame, after validating it here.
func (c *Client) Request(r console.Request) error {
	if err := r.Validate(); err != nil {
		// Fail locally with the real reason rather than spending a round trip
		// to be told `bad_request` by the runner.
		return err
	}
	return c.c.WriteRequest(r)
}

// Next reads one event.
func (c *Client) Next() (console.Event, error) { return c.c.ReadEvent() }

// Do writes a request and reads until the wanted event arrives.
//
// Events that are not the answer -- console lines on a connection that is
// already following, a shutdown step -- go to on, which may be nil to discard
// them. An `ev:error` frame is returned as a *ProtocolError.
func (c *Client) Do(req console.Request, want console.EventKind, on func(console.Event)) (console.Event, error) {
	// A connection whose read deadline has already fired is unusable: the
	// decoder may have stopped mid-frame and nothing can resynchronise from
	// that. Refusing here is what makes a retry on the same client -- a
	// freeze's thaw after its flush timed out -- fail fast instead of putting
	// a second goroutine into ReadEvent, which Conn's own doc forbids.
	if err := c.c.Err(); err != nil {
		return console.Event{}, err
	}
	if err := c.Request(req); err != nil {
		return console.Event{}, err
	}
	// Bounded for the whole call rather than per frame: the budget is for one
	// answer, and a peer that dribbled intermediate events forever would
	// otherwise reset it each time.
	restore := c.c.armCall(c.CallTimeout())
	defer restore()
	for {
		ev, err := c.Next()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				c.c.fail(err)
			}
			return console.Event{}, err
		}
		switch ev.Ev {
		case want:
			return ev, nil
		case console.EvError:
			return ev, &ProtocolError{Code: ev.Code, Msg: ev.Error}
		default:
			if on != nil {
				on(ev)
			}
		}
	}
}

// Hello performs the handshake and records what the server said about this
// connection's tier, so a caller can disable a control it is not allowed to
// use instead of discovering the tier by being refused.
func (c *Client) Hello(client string) (console.PeerInfo, error) {
	ev, err := c.Do(console.Request{Op: console.OpHello, Client: client}, console.EvHello, nil)
	if err != nil {
		return console.PeerInfo{}, err
	}
	if ev.Proto != console.Proto {
		return console.PeerInfo{}, fmt.Errorf("socket: runner speaks console protocol %d, this build speaks %d",
			ev.Proto, console.Proto)
	}
	c.instance, c.build = ev.Instance, ev.Build
	if ev.Peer != nil {
		c.peer = *ev.Peer
	}
	return c.peer, nil
}

// Ping round-trips one frame.
func (c *Client) Ping() error {
	_, err := c.Do(console.Request{Op: console.OpPing}, console.EvPong, nil)
	return err
}

// Tail asks for history and, with follow, upgrades the connection to a stream.
// It does not wait: the reply is zero or more `lines` events, possibly
// preceded by `truncated`, and on an idle server it is legitimately nothing at
// all. Read them with Next.
func (c *Client) Tail(from uint64, backlog int, follow bool) error {
	return c.Request(console.Request{Op: console.OpTail, From: from, Backlog: backlog, Follow: follow})
}

// Send injects one line and returns the Seq of its echo in the ring.
func (c *Client) Send(actor, line string, on func(console.Event)) (uint64, error) {
	ev, err := c.Do(console.Request{Op: console.OpSend, Line: line, Actor: actor}, console.EvSent, on)
	if err != nil {
		return 0, err
	}
	return ev.Seq, nil
}

// Status returns status.json's bytes, verbatim.
func (c *Client) Status(on func(console.Event)) (json.RawMessage, error) {
	ev, err := c.Do(console.Request{Op: console.OpStatus}, console.EvStatus, on)
	if err != nil {
		return nil, err
	}
	return ev.Status, nil
}

// Caps returns the derived capability states.
func (c *Client) Caps(on func(console.Event)) (json.RawMessage, error) {
	ev, err := c.Do(console.Request{Op: console.OpCaps}, console.EvCaps, on)
	if err != nil {
		return nil, err
	}
	return ev.Caps, nil
}

// Verb runs one manifest verb. It is uid-0 only; every other tier gets a
// *ProtocolError with CodeDenied and should have been routed through gvmd.
func (c *Client) Verb(actor, verb string, args map[string]string, on func(console.Event)) (VerbResult, error) {
	ev, err := c.Do(console.Request{Op: console.OpVerb, Verb: verb, Args: args, Actor: actor}, console.EvVerb, on)
	if err != nil {
		return VerbResult{}, err
	}
	return VerbResult{Channel: ev.Channel, Output: ev.Output}, nil
}

// Stop runs the shutdown ladder and blocks until it finishes, calling step for
// each rung as it happens. Every other event still goes to on, so a caller
// that is following keeps seeing the console during the save it is waiting on.
func (c *Client) Stop(actor, reason string, step func(rung, send string), on func(console.Event)) (json.RawMessage, error) {
	if err := c.Request(console.Request{Op: console.OpStop, Reason: reason, Actor: actor}); err != nil {
		return nil, err
	}
	for {
		ev, err := c.Next()
		if err != nil {
			return nil, err
		}
		switch {
		case ev.Ev == console.EvError:
			return nil, &ProtocolError{Code: ev.Code, Msg: ev.Error}
		case ev.Ev == console.EvShutdown && ev.Phase == console.PhaseDone:
			return ev.Report, nil
		case ev.Ev == console.EvShutdown:
			if step != nil {
				step(ev.Rung, ev.Send)
			}
		default:
			if on != nil {
				on(ev)
			}
		}
	}
}
