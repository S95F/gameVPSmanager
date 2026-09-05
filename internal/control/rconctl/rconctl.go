// Package rconctl wraps internal/rcon with the three things a supervisor needs
// and a raw RCON client should not have (section 11.9).
//
// internal/rcon is already correct and already tested: exact framing, the auth
// handshake, four dialects' terminator strategies, the Latin-1 fallback. None
// of that is reimplemented here. What is added is the policy a long-lived
// supervisor needs and a one-shot client has no opinion about:
//
//  1. NEVER DIAL BEFORE THE GATE OPENS. Project Zomboid does not bind its RCON
//     port until it has finished booting, which on a modded first boot is forty
//     to sixty minutes. A client that dials on a timer during that window
//     produces one connection-refused per poll, for an hour, in the log an
//     operator will read when something actually goes wrong.
//  2. CLOSE AND REDIAL ON ANY PROTOCOL ERROR. client.go's own comment is the
//     reason: an unread fragment left in the buffer desynchronises every
//     subsequent command on that connection. A poisoned connection does not
//     announce itself -- it returns the PREVIOUS command's output.
//  3. BACK OFF ON AUTH FAILURE. A wrong password retried every minute against
//     a plaintext console fills the log, trips fail2ban, and never once
//     succeeds, because the password is in a file and files do not change
//     their minds.
package rconctl

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/rcon"
	"github.com/S95F/gameVPSmanager/internal/secrets"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

const (
	// AuthBackoff is section 11.9's five minutes. It is long on purpose: the
	// only thing that fixes a rejected password is an operator editing a file,
	// and no retry schedule can be fast enough to matter for that while being
	// slow enough not to be an attack on the operator's own server.
	AuthBackoff = 5 * time.Minute

	// DialBackoff applies to every other dial failure. Short, because
	// connection-refused during a restart resolves itself in seconds, and the
	// gate already covers the long wait.
	DialBackoff = 5 * time.Second

	// DefaultMinPoll is section 11.9's floor. RCON commands execute on the
	// game's MAIN SERVER THREAD: a player-count poll every five seconds is a
	// measurable TPS cost paid by every player, forever, so that a dashboard
	// can be four seconds fresher.
	DefaultMinPoll = 60 * time.Second
)

// Conn is the part of *rcon.Client this package uses, extracted so the policy
// above is testable with no listener, no timing and no Linux box. rconctl_test
// counts dials; that is only possible because this is an interface.
type Conn interface {
	Execute(ctx context.Context, cmd string) (string, error)
	Close() error
}

// Dialer opens one authenticated connection.
type Dialer func(ctx context.Context, addr, password string, opts rcon.Options) (Conn, error)

// NetDialer is the real one.
func NetDialer(ctx context.Context, addr, password string, opts rcon.Options) (Conn, error) {
	return rcon.Dial(ctx, addr, password, opts)
}

// Options configures a Client.
type Options struct {
	// Addr is host:port. It is always a loopback address: section 11.9 and
	// D-20 put every RCON port on bind 127.0.0.1, because the protocol
	// transmits the password in cleartext.
	Addr string

	// Password is held as a secrets.Value so that a Client printed by a
	// panicking goroutine's %+v does not put the RCON password in the journal.
	// It is revealed at the dial call and nowhere else.
	Password secrets.Value

	// Dialect is the manifest's. An unknown one is refused by New rather than
	// silently becoming Source (D-07).
	Dialect spec.Dialect

	// Gate reports whether the instance has reached `ready`. It is REQUIRED:
	// a nil gate would mean "dial whenever", which is rule 1 inverted, and a
	// default of "always open" is a mistake that only shows up as an hour of
	// log noise on the one game that matters most.
	Gate func() bool

	// NotReadyReason is the manifest's `control.not_ready_reason`, returned
	// VERBATIM by Unready while the gate is closed. It is the disabled
	// button's tooltip, written by whoever wrote the manifest, and rewording
	// it here would be this package inventing product copy.
	NotReadyReason string

	// MinPoll bounds Poll. Zero means DefaultMinPoll.
	MinPoll time.Duration

	Timeout     time.Duration
	DialTimeout time.Duration
	IdleWindow  time.Duration

	// Dialer defaults to NetDialer.
	Dialer Dialer

	// Clock is injectable so the five-minute backoff is a test that runs in
	// microseconds rather than one nobody runs.
	Clock func() time.Time
}

// Client is a lazily-dialed, self-healing RCON channel.
type Client struct {
	addr     string
	password secrets.Value
	dialect  rcon.Dialect
	specDial spec.Dialect
	opts     rcon.Options
	gate     func() bool
	reason   string
	minPoll  time.Duration
	dial     Dialer
	now      func() time.Time

	// execMu serializes commands and the dial that precedes one. RCON has no
	// multiplexing: two commands in flight on one connection get each other's
	// replies.
	execMu sync.Mutex

	// connMu guards only the connection pointer, and is never held across a
	// network call, so Close can drop a connection out from under an Execute
	// that is blocked on a server which has stopped answering.
	connMu sync.Mutex
	conn   Conn

	// mu guards health and scheduling. Ready, Unready and Snapshot take only
	// this one: status.json is rewritten every two seconds and must not queue
	// behind a thirty-second command.
	mu       sync.Mutex
	closed   bool
	nextDial time.Time
	lastPoll time.Time
	lastOK   time.Time
	lastErr  string
	fails    int
}

var (
	_ control.Channel  = (*Client)(nil)
	_ control.Reporter = (*Client)(nil)
)

// New returns a Client. It does not dial: nothing in this package dials until
// a command is run and the gate is open.
func New(o Options) (*Client, error) {
	if o.Addr == "" {
		return nil, errors.New("rconctl: no address")
	}
	if o.Gate == nil {
		return nil, errors.New("rconctl: no readiness gate; dialling before the game listens is an hour of connection-refused on Project Zomboid")
	}
	d, err := Dialect(o.Dialect)
	if err != nil {
		return nil, err
	}
	sd, err := SpecDialect(d)
	if err != nil {
		return nil, err
	}

	c := &Client{
		addr:     o.Addr,
		password: o.Password,
		dialect:  d,
		specDial: sd,
		opts: rcon.Options{
			Dialect:     d,
			DialTimeout: o.DialTimeout,
			Timeout:     o.Timeout,
			IdleWindow:  o.IdleWindow,
		},
		gate:    o.Gate,
		reason:  o.NotReadyReason,
		minPoll: o.MinPoll,
		dial:    o.Dialer,
		now:     o.Clock,
	}
	if c.reason == "" {
		c.reason = "the control channel does not accept connections until the server has finished booting"
	}
	if c.minPoll <= 0 {
		c.minPoll = DefaultMinPoll
	}
	if c.dial == nil {
		c.dial = NetDialer
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// Kind implements control.Channel.
func (c *Client) Kind() control.Kind { return spec.ControlSourceRCON }

// Addr reports the address this client dials.
func (c *Client) Addr() string { return c.addr }

// SpecDialect reports the manifest spelling of the dialect in use.
func (c *Client) SpecDialect() spec.Dialect { return c.specDial }

// MinPoll reports the poll floor, so a caller's ticker and Poll's own refusal
// are derived from one number.
func (c *Client) MinPoll() time.Duration { return c.minPoll }

// Ready implements control.Channel.
//
// Three conditions, and the backoff is one of them: during a five-minute auth
// backoff the channel is genuinely not usable, and reporting ready would put an
// enabled button in front of an operator that fails the moment they press it.
func (c *Client) Ready() bool {
	c.mu.Lock()
	closed, until := c.closed, c.nextDial
	now := c.now()
	c.mu.Unlock()
	if closed || now.Before(until) {
		return false
	}
	return c.gate()
}

// Unready implements control.Channel.
func (c *Client) Unready() string {
	c.mu.Lock()
	closed, until, lastErr := c.closed, c.nextDial, c.lastErr
	now := c.now()
	c.mu.Unlock()

	switch {
	case closed:
		return "the runner is shutting down"
	case !c.gate():
		// The manifest's sentence, verbatim, and checked BEFORE the backoff:
		// a stopped server is why the channel is down, whatever happened last
		// time it was up.
		return c.reason
	case now.Before(until):
		wait := until.Sub(now).Round(time.Second)
		if lastErr == "" {
			return fmt.Sprintf("the control channel is backing off for %s", wait)
		}
		return fmt.Sprintf("%s; not retrying for %s", lastErr, wait)
	}
	return ""
}

// Exec implements control.Channel: one command, one connection, no retry.
//
// There is deliberately no transparent retry after a redial. Every verb that
// reaches this path is a MUTATION -- kick, ban, broadcast, save -- and a
// transparent retry of a command whose reply was lost is a second kick, or a
// second save landing on a world mid-write. The connection is dropped so the
// NEXT command starts clean; this one reports what happened.
func (c *Client) Exec(ctx context.Context, cmd string) (string, error) {
	c.execMu.Lock()
	defer c.execMu.Unlock()

	if c.isClosed() {
		return "", control.ErrClosed
	}
	if !c.Ready() {
		return "", fmt.Errorf("%w: %s", control.ErrNotReady, c.Unready())
	}

	conn, err := c.connect(ctx)
	if err != nil {
		return "", err
	}

	out, err := conn.Execute(ctx, cmd)
	switch {
	case err == nil:
		c.succeeded()
		return out, nil

	case errors.Is(err, rcon.ErrCommandTooLong):
		// Nothing was written: rcon.Execute checks the length before it
		// touches the socket, so the connection is still aligned and the
		// health counters have no business moving. This is a caller's bug,
		// not a channel fault.
		return "", err

	default:
		c.drop(err)
		return "", err
	}
}

// Poll runs a read-only command, no more often than MinPoll.
//
// It returns ok == false when the call was suppressed, rather than an error,
// because a suppressed poll is the system working: the previous answer is still
// the answer. The distinction matters because a caller that treated suppression
// as failure would show "player count unavailable" for fifty-nine seconds out
// of every sixty.
func (c *Client) Poll(ctx context.Context, cmd string) (out string, ok bool, err error) {
	c.mu.Lock()
	now := c.now()
	if !c.lastPoll.IsZero() && now.Sub(c.lastPoll) < c.minPoll {
		c.mu.Unlock()
		return "", false, nil
	}
	// Stamped on the ATTEMPT, not on success: a failing poll that reset
	// nothing would retry as fast as the caller's loop allows, which is the
	// hammering this floor exists to prevent.
	c.lastPoll = now
	c.mu.Unlock()

	out, err = c.Exec(ctx, cmd)
	return out, true, err
}

// connect returns the live connection, dialling if there is none.
func (c *Client) connect(ctx context.Context) (Conn, error) {
	if conn := c.current(); conn != nil {
		return conn, nil
	}

	c.mu.Lock()
	now, until := c.now(), c.nextDial
	c.mu.Unlock()
	if now.Before(until) {
		return nil, fmt.Errorf("%w: %s", control.ErrNotReady, c.Unready())
	}

	conn, err := c.dial(ctx, c.addr, c.password.Reveal(), c.opts)
	if err != nil {
		backoff := DialBackoff
		if errors.Is(err, rcon.ErrAuthFailed) {
			backoff = AuthBackoff
		}
		c.failed(err, backoff)
		return nil, err
	}
	c.setCurrent(conn)
	return conn, nil
}

func (c *Client) current() Conn {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.conn
}

func (c *Client) setCurrent(conn Conn) {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	c.conn = conn
}

// take clears and returns the connection, so exactly one caller closes it.
func (c *Client) take() Conn {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	conn := c.conn
	c.conn = nil
	return conn
}

// drop closes the poisoned connection and records the failure. The close error
// is discarded: the connection is already unusable and a failure to close it is
// not information anyone can act on.
func (c *Client) drop(err error) {
	if conn := c.take(); conn != nil {
		_ = conn.Close()
	}
	c.failed(err, 0)
}

func (c *Client) failed(err error, backoff time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fails++
	c.lastErr = err.Error()
	if backoff > 0 {
		c.nextDial = c.now().Add(backoff)
	}
}

func (c *Client) succeeded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fails, c.lastErr = 0, ""
	c.nextDial = time.Time{}
	c.lastOK = c.now()
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Close implements control.Channel. It is safe to call while a command is in
// flight: the connection pointer is taken under its own short-lived lock, so
// the blocked Execute fails on a closed socket instead of holding the runner's
// exit open for the length of its timeout.
func (c *Client) Close() error {
	c.mu.Lock()
	already := c.closed
	c.closed = true
	c.mu.Unlock()
	if already {
		return nil
	}
	if conn := c.take(); conn != nil {
		return conn.Close()
	}
	return nil
}

// Snapshot implements control.Reporter: the `control` block of status.json.
func (c *Client) Snapshot() control.Status {
	ready := c.Ready()
	c.mu.Lock()
	defer c.mu.Unlock()
	return control.Status{
		Kind:                spec.ControlSourceRCON,
		Dialect:             string(c.specDial),
		Ready:               ready,
		LastOK:              c.lastOK,
		LastError:           c.lastErr,
		ConsecutiveFailures: c.fails,
	}
}
