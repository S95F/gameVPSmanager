// Package socket serves an instance's console socket and dials it.
//
// The socket is NDJSON over SOCK_STREAM (section 11.4) and its authorization
// is SO_PEERCRED, not a token: the credentials are stamped by the kernel at
// connect time and cannot be forged, which is what lets gvm-web sit directly
// on the data plane (D-09) while `verb` and `stop` stay uid-0 only.
//
// The split in this package follows that: auth.go is a pure function from a
// peer and an op to a decision, so the whole matrix is table-tested on the
// development machine; server.go is transport-agnostic and driven by an
// Identifier seam; and only server_linux.go performs the getsockopt and
// creates the unix socket.
package socket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// Server defaults.
const (
	// DefaultMaxConns bounds concurrent connections. Only three uids can reach
	// the socket at all, so this is not a defence against a crowd -- it is a
	// bound on an fd leak in gvmd or gvm-web taking the runner's console down
	// with it.
	DefaultMaxConns = 64
	// DefaultAcceptBackoff is the first pause after an accept failure that is
	// not a closed listener. A supervisor must not exit its console loop
	// because the box hit EMFILE for a second.
	DefaultAcceptBackoff = 5 * time.Millisecond
	// MaxAcceptBackoff caps that pause.
	MaxAcceptBackoff = time.Second
)

// ErrBadArgument lets a backend report a caller's mistake -- an unsafe verb
// argument, an unknown verb -- without this package importing the control
// layer to recognise its error types. It maps to a bad_request event.
var ErrBadArgument = errors.New("socket: invalid argument")

// VerbResult is one verb's outcome: which channel served it, and what came
// back. The channel matters to an operator, because it is the difference
// between a structured RCON reply and a console write with no reply at all.
type VerbResult struct {
	Channel string
	Output  string
}

// Backend is everything the socket needs from a runner.
//
// It is an interface here rather than a concrete runner type so the import
// arrow points one way -- the runner imports the socket -- and so the whole
// server is testable against console.Fake with no process, no pty and no
// Linux box.
type Backend interface {
	// Instance and Build fill in the hello event.
	Instance() string
	Build() string
	// Ring is the console history and the fan-out source.
	Ring() *console.Ring
	// Send injects one line towards the game and returns the Seq of its echo.
	Send(actor, line string) (uint64, error)
	// Status and Caps return the runner's own JSON, verbatim.
	Status() (json.RawMessage, error)
	Caps() (json.RawMessage, error)
	// Verb runs one manifest verb.
	Verb(ctx context.Context, actor, verb string, args map[string]string) (VerbResult, error)
	// Stop runs the shutdown ladder. step narrates each rung while it runs,
	// which is the difference between "stopping..." and "sending save,
	// waiting for the marker" on an operator's screen during a 90 second save.
	Stop(ctx context.Context, actor, reason string, step func(rung, send string)) (json.RawMessage, error)
}

// Identifier resolves the credentials of an accepted connection. It is a seam
// because SO_PEERCRED is Linux-only and the authorization it feeds is not.
//
// An Identifier that cannot name the peer MUST return an error. Returning a
// zero Identity would land the connection in TierNone, which is refused, but
// making the contract explicit is cheaper than relying on that.
type Identifier func(net.Conn) (Identity, error)

// Option configures a Server.
type Option func(*Server)

// WithLogger sets the diagnostic logger. Runner diagnostics go to the journal;
// the console itself deliberately does not (section 11.3).
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

// WithMaxConns bounds concurrent connections.
func WithMaxConns(n int) Option { return func(s *Server) { s.maxConns = n } }

// WithTimeouts sets the per-frame write deadline and the first-frame deadline.
func WithTimeouts(write, handshake time.Duration) Option {
	return func(s *Server) { s.writeTimeout, s.handshake = write, handshake }
}

// WithSubDepth sets the per-follower channel depth, in batches.
func WithSubDepth(n int) Option { return func(s *Server) { s.subDepth = n } }

// Server serves one instance's console socket.
type Server struct {
	backend  Backend
	policy   Policy
	identify Identifier

	log          *slog.Logger
	maxConns     int
	writeTimeout time.Duration
	handshake    time.Duration
	subDepth     int

	stopping atomic.Bool

	// inflight counts request handlers that are mid-op, so a shutting-down
	// runner can let them finish before the ring under them is closed. It
	// deliberately does not count the readers blocked in ReadRequest: an
	// attached `gvm console` sits there forever, and a drain that waited for
	// it would never return.
	inflight sync.WaitGroup

	mu     sync.Mutex
	conns  map[*Conn]struct{}
	lis    net.Listener
	closed bool
}

// New returns a server. It refuses a nil backend or identifier rather than
// defaulting either: a socket that accepts connections it cannot attribute is
// worse than a socket that does not start.
func New(b Backend, p Policy, id Identifier, opts ...Option) (*Server, error) {
	if b == nil {
		return nil, errors.New("socket: server needs a backend")
	}
	if id == nil {
		return nil, errors.New("socket: server needs an identifier; peer credentials are the authorization")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	s := &Server{
		backend:  b,
		policy:   p,
		identify: id,
		log:      slog.New(slog.DiscardHandler),
		maxConns: DefaultMaxConns,
		subDepth: console.DefaultSubDepth,
		conns:    make(map[*Conn]struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.maxConns <= 0 {
		s.maxConns = DefaultMaxConns
	}
	if s.subDepth <= 0 {
		s.subDepth = console.DefaultSubDepth
	}
	return s, nil
}

// Serve accepts connections until the listener closes, ctx ends, or Close is
// called. It returns nil for an orderly stop.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		l.Close()
		return net.ErrClosed
	}
	s.lis = l
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The listener is what Accept blocks on, so cancellation has to reach it
	// through Close rather than through a select the accept loop cannot make.
	watch := make(chan struct{})
	defer close(watch)
	go func() {
		select {
		case <-ctx.Done():
			l.Close()
		case <-watch:
		}
	}()

	delay := time.Duration(0)
	for {
		nc, err := l.Accept()
		if err != nil {
			if s.isClosed() || errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			// EMFILE and friends are transient and must not end the console
			// for the life of the runner. Back off, keep serving.
			if delay == 0 {
				delay = DefaultAcceptBackoff
			} else if delay *= 2; delay > MaxAcceptBackoff {
				delay = MaxAcceptBackoff
			}
			s.log.Warn("console socket accept failed", "err", err, "retry_in", delay)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
			continue
		}
		delay = 0
		go s.serveConn(ctx, nc)
	}
}

// Drain waits up to d for the request handlers that are mid-op to finish, so
// their replies reach the client before anything under them is torn down.
//
// The one that matters is `op:stop`: it blocks its connection for the ladder's
// whole duration, and closing the ring first makes the fan-out drop the very
// connection that is waiting to be told the shutdown finished. It is bounded
// rather than unbounded because a teardown must always complete -- an operator
// who has already been told the world is stopped should never watch the unit
// hang on a socket.
//
// Readers idling in ReadRequest are not counted, so an attached console does
// not hold this open.
func (s *Server) Drain(d time.Duration) {
	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		s.log.Warn("console socket still had a request in flight when the runner tore down")
	}
}

// Close stops accepting and drops every live connection. Callers block on
// nothing: a connection goroutine unwinds when its socket dies.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	lis := s.lis
	conns := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	var err error
	if lis != nil {
		err = lis.Close()
	}
	for _, c := range conns {
		c.Close()
	}
	return err
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// track registers a connection, refusing it when the server is closing or the
// limit is reached.
func (s *Server) track(c *Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.conns) >= s.maxConns {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c *Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// Conns reports the number of live connections, which is what status.json's
// console.subscribers is derived from alongside the ring's own count.
func (s *Server) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// connState is one connection's mutable state. It is touched only by that
// connection's reader goroutine -- the fan-out goroutine never reads it -- so
// it carries no lock of its own.
type connState struct {
	f *follower
}

// follower is an active `tail follow:true` on one connection.
type follower struct {
	sub  *console.Subscription
	stop chan struct{}
	done chan struct{}
}

func (s *Server) serveConn(ctx context.Context, nc net.Conn) {
	c := NewConn(nc, s.writeTimeout, s.handshake)
	// Registered in this order so the recover runs BEFORE the close: a client
	// that learns of the panic by its socket dying should find the reason
	// already in the ring, not appended a scheduler tick later.
	defer c.Close()
	defer s.recoverInto("console connection handler")

	if !s.track(c) {
		_ = c.WriteEvent(console.ErrorEvent(console.CodeInternal,
			fmt.Sprintf("too many console connections (limit %d)", s.maxConns)))
		return
	}
	defer s.untrack(c)

	id, err := s.identify(nc)
	if err != nil {
		// A connection whose credentials cannot be read is refused, never
		// demoted to the least privileged tier: "unknown" is not an identity,
		// and the failure modes of getsockopt are not a permission level.
		s.log.Warn("console socket refused a connection with unreadable credentials", "err", err)
		_ = c.WriteEvent(console.ErrorEvent(console.CodeDenied,
			"peer credentials unavailable; connection refused"))
		return
	}
	tier := s.policy.Tier(id)
	if !tier.Serveable() {
		d := s.policy.Authorize(id, console.OpHello)
		s.log.Warn("console socket refused a connection", "peer", id.Peer.String(), "tier", tier.String())
		_ = c.WriteEvent(console.ErrorEvent(d.Code, d.Reason))
		return
	}

	st := &connState{}
	defer st.stopFollow()

	for {
		req, err := c.ReadRequest()
		if err != nil {
			if !s.replyReadError(c, err) {
				return
			}
			continue
		}
		s.inflight.Add(1)
		alive := s.dispatch(ctx, c, st, id, tier, req)
		s.inflight.Done()
		if !alive {
			return
		}
	}
}

// replyReadError answers a bad frame and reports whether the connection is
// still usable.
func (s *Server) replyReadError(c *Conn, err error) bool {
	switch {
	case Gone(err), Timeout(err):
		return false
	case errors.Is(err, console.ErrFrameTooLarge):
		// The rest of that frame is still in the reader, so there is no
		// resynchronising: say why, then hang up.
		_ = c.WriteEvent(console.ErrorEvent(console.CodeBadRequest, err.Error()))
		return false
	default:
		// A malformed or unknown op is one bad frame, not a bad client: a peer
		// speaking a newer protocol keeps its connection and its stream.
		return s.reply(c, console.ErrorEvent(console.CodeBadRequest, err.Error()))
	}
}

// reply writes one event and reports whether the connection survived it. A
// frame refused for size did not touch the wire, so it is not fatal.
func (s *Server) reply(c *Conn, ev console.Event) bool {
	err := c.WriteEvent(ev)
	return err == nil || errors.Is(err, console.ErrFrameTooLarge)
}

func (s *Server) dispatch(ctx context.Context, c *Conn, st *connState, id Identity, tier Tier, req console.Request) bool {
	if d := s.policy.Authorize(id, req.Op); !d.Allow {
		s.log.Info("console socket denied an op", "op", string(req.Op),
			"peer", id.Peer.String(), "tier", d.Tier.String())
		return s.reply(c, console.ErrorEvent(d.Code, d.Reason))
	}

	switch req.Op {
	case console.OpHello:
		return s.reply(c, console.HelloEvent(s.backend.Instance(), s.backend.Build(), tier.PeerInfo(id.Peer)))

	case console.OpPing:
		return s.reply(c, console.PongEvent())

	case console.OpTail:
		return s.handleTail(c, st, req)

	case console.OpSend:
		seq, err := s.backend.Send(s.policy.Actor(id, tier, req.Actor), req.Line)
		if err != nil {
			return s.replyBackendError(c, err)
		}
		return s.reply(c, console.SentEvent(seq))

	case console.OpStatus:
		raw, err := s.backend.Status()
		if err != nil {
			return s.replyBackendError(c, err)
		}
		if len(raw) == 0 {
			return s.reply(c, console.ErrorEvent(console.CodeInternal, "status is not available yet"))
		}
		return s.reply(c, console.StatusEvent(raw))

	case console.OpCaps:
		raw, err := s.backend.Caps()
		if err != nil {
			return s.replyBackendError(c, err)
		}
		if len(raw) == 0 {
			return s.reply(c, console.ErrorEvent(console.CodeInternal, "capabilities are not available yet"))
		}
		return s.reply(c, console.CapsEvent(raw))

	case console.OpVerb:
		res, err := s.backend.Verb(ctx, s.policy.Actor(id, tier, req.Actor), req.Verb, req.Args)
		if err != nil {
			return s.replyBackendError(c, err)
		}
		return s.reply(c, console.VerbEvent(res.Channel, res.Output))

	case console.OpStop:
		return s.handleStop(ctx, c, id, tier, req)

	default:
		// Unreachable: Authorize refuses an op outside the closed set. Kept so
		// that adding an op to the protocol fails here loudly.
		return s.reply(c, console.ErrorEvent(console.CodeBadRequest,
			fmt.Sprintf("op %q is not implemented", string(req.Op))))
	}
}

// handleTail serves history and, optionally, upgrades the connection to a
// stream.
//
// The two cursors mean different things and are served by different ring
// methods on purpose. from == 0 is a fresh attach and backlog is "the newest
// N", where 0 means none: defaulting a bare attach to the whole ring is the
// 20 000-line marshal MaxBacklog exists to prevent. from > 0 is a resume and
// backlog is a cap on the first frame, where 0 means "everything since my
// cursor" -- a resume that silently dropped history would defeat the point of
// having a cursor at all.
func (s *Server) handleTail(c *Conn, st *connState, req console.Request) bool {
	ring := s.backend.Ring()
	var (
		lines  []console.Line
		oldest uint64
		head   uint64
	)
	switch {
	case req.From == 0 && req.Backlog <= 0:
		// A bare attach asks to follow from now. Reading the bounds through a
		// cursor past the head is how they come back consistent with each
		// other under one lock, without copying a ring nobody asked for.
		_, oldest, head = ring.Snapshot(^uint64(0), 0)
	case req.From == 0:
		lines, oldest, head = ring.Tail(req.Backlog)
	default:
		lines, oldest, head = ring.Snapshot(req.From, req.Backlog)
	}

	// Where the follower picks up. Taking head+1 when the backlog is empty is
	// what makes the two reads gapless: any line appended between them has a
	// Seq at or above this cursor, and Subscribe replays the ring from there.
	next := head + 1
	if n := len(lines); n > 0 {
		next = lines[n-1].Seq + 1
	}

	st.stopFollow() // one follower per connection; a second tail replaces the first

	var sub *console.Subscription
	var subOldest uint64
	if req.Follow {
		sub = ring.Subscribe(next, s.subDepth)
		subOldest, _ = sub.Start()
	}
	if lost := truncatedFrom(req.From, next, oldest, subOldest); lost > 0 {
		if !s.reply(c, console.TruncatedEvent(lost)) {
			if sub != nil {
				sub.Close()
			}
			return false
		}
	}
	if len(lines) > 0 {
		if err := c.WriteLines(head, oldest, lines); err != nil && !errors.Is(err, console.ErrFrameTooLarge) {
			if sub != nil {
				sub.Close()
			}
			return false
		}
	}
	if sub != nil {
		st.start(s, c, sub)
	}
	return true
}

// truncatedFrom reports the oldest surviving Seq when history the client asked
// for is already gone, and 0 when nothing was lost.
//
// There are two ways to lose it, and only the first is obvious: the cursor is
// older than the ring's oldest line, or the ring evicted past the follow
// cursor in the unlocked window between reading the backlog and subscribing.
// The second is rare and otherwise silent, which is exactly why it is named:
// resuming without a word lets somebody conclude a crash left no trace.
func truncatedFrom(from, next, snapOldest, subOldest uint64) uint64 {
	if from > 0 && snapOldest > 0 && from < snapOldest {
		return snapOldest
	}
	if subOldest > 0 && next < subOldest {
		return subOldest
	}
	return 0
}

// start attaches a fan-out goroutine to this connection.
func (st *connState) start(s *Server, c *Conn, sub *console.Subscription) {
	f := &follower{sub: sub, stop: make(chan struct{}), done: make(chan struct{})}
	st.f = f
	go s.stream(c, f)
}

// stopFollow ends the current follower and waits for its goroutine, so a
// replaced subscription can never write a frame after its replacement's
// backlog.
func (st *connState) stopFollow() {
	if st.f == nil {
		return
	}
	close(st.f.stop)
	st.f.sub.Close()
	<-st.f.done
	st.f = nil
}

func (s *Server) stream(c *Conn, f *follower) {
	defer close(f.done)
	defer s.recoverInto("console fan-out")

	ring := s.backend.Ring()
	for {
		select {
		case <-f.stop:
			return
		case b, ok := <-f.sub.C:
			if !ok {
				select {
				case <-f.stop:
					return // replaced, or the connection is shutting down
				default:
				}
				// The ring closed, so the runner is on its way out. Closing
				// the socket is how `gvm console` learns that instead of
				// hanging on a connection nothing will ever write to again.
				c.Close()
				return
			}
			if b.Dropped > 0 && len(b.Lines) > 0 {
				// The drop count travels in the batch, so it always belongs to
				// lines this reader has not seen: resume_from is the first Seq
				// it is about to get.
				if !s.reply(c, console.LaggedEvent(b.Dropped, b.Lines[0].Seq)) {
					return
				}
			}
			head, oldest := ring.Head(), ring.Oldest()
			if err := c.WriteLines(head, oldest, b.Lines); err != nil && !errors.Is(err, console.ErrFrameTooLarge) {
				return // WriteLines already closed the connection
			}
		}
	}
}

func (s *Server) handleStop(ctx context.Context, c *Conn, id Identity, tier Tier, req console.Request) bool {
	if !s.stopping.CompareAndSwap(false, true) {
		return s.reply(c, console.ErrorEvent(console.CodeBadRequest, "a shutdown is already in progress"))
	}
	defer s.stopping.Store(false)

	// Deliberately synchronous. The ladder can run for minutes and this
	// connection stops answering other ops for its duration -- but the fan-out
	// goroutine keeps streaming console lines, which is what an operator
	// actually watches during a save, and one ladder at a time is a property
	// worth more than an interleaved ping.
	step := func(rung, send string) {
		_ = c.WriteEvent(console.ShutdownStepEvent(rung, send))
	}
	report, err := s.backend.Stop(ctx, s.policy.Actor(id, tier, req.Actor), req.Reason, step)
	if err != nil {
		return s.replyBackendError(c, err)
	}
	return s.reply(c, console.ShutdownDoneEvent(report))
}

// replyBackendError turns a backend failure into an event with a code a client
// can branch on.
func (s *Server) replyBackendError(c *Conn, err error) bool {
	switch {
	case errors.Is(err, platform.ErrNotSupported):
		return s.reply(c, console.ErrorEvent(console.CodeUnsupported, err.Error()))
	case errors.Is(err, ErrBadArgument), errors.Is(err, console.ErrLineTooLong),
		errors.Is(err, console.ErrLineEmpty), errors.Is(err, console.ErrLineBreak):
		return s.reply(c, console.ErrorEvent(console.CodeBadRequest, err.Error()))
	default:
		return s.reply(c, console.ErrorEvent(console.CodeInternal, err.Error()))
	}
}

// recoverInto is section 11.8 layer 1: every goroutine boundary in this package
// carries one, and the panic is recorded in the ring as well as the journal so
// it shows up in the console an operator is already looking at.
func (s *Server) recoverInto(what string) {
	r := recover()
	if r == nil {
		return
	}
	// A backend that panicked once may panic again on the way to the ring;
	// losing the ring line is acceptable, losing the process is not.
	defer func() { _ = recover() }()

	msg := fmt.Sprintf("-- gvm: %s panicked: %v", what, r)
	s.log.Error(msg, "stack", string(debug.Stack()))
	if ring := s.backend.Ring(); ring != nil {
		ring.Append(console.Line{Source: console.SourceRunner, Text: msg})
	}
}
