// Package stdinctl is the console seen as a command channel: the game's own
// stdin, which every game in the catalogue has, for the whole life of the
// process, whether or not it has ever heard of RCON.
//
// It exists so the Commander routes between two things of the same shape
// instead of between a first-class channel and a special case, and so the two
// places that write to a game's stdin -- the runner, through the pty, and the
// standalone shutdown path of section 11.8, through /proc/<pid>/fd/0 -- go
// through one implementation of "is it safe to write this, and who gets the
// blame for it".
//
// What it cannot do is return a reply. Exec always returns "". Scraping the
// next line out of the ring and calling it the answer would attribute whatever
// the game printed next -- a player joining, a mod's log line -- to the
// operator's command.
package stdinctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// DefaultActor attributes a line that the runner itself originated -- a
// shutdown rung, a hook -- as gvm rather than as a person.
const DefaultActor = "runner"

// Options configures a Channel.
type Options struct {
	// Sink is where a line goes. The runner passes its own injector, which
	// writes to the pty AND echoes into the ring; the standalone ladder passes
	// WriterSender.
	Sink control.Sender

	// Gate reports whether the game is accepting input: state is `activating`
	// or `ready` (section 8.5's console.write row). nil means always.
	Gate func() bool

	// Reason is the Unready sentence. It is short and about the SERVER, not
	// about gvm: an operator reading a tooltip wants "the server is stopped",
	// not "the console channel gate is closed".
	Reason string

	// Actor attributes an Exec, which has no actor of its own because the
	// shutdown ladder and the hook runner are not people.
	Actor string

	Clock func() time.Time
}

// Channel is a control.ConsoleChannel over a game's stdin.
type Channel struct {
	sink   control.Sender
	gate   func() bool
	reason string
	actor  string
	now    func() time.Time

	mu      sync.Mutex
	closed  bool
	lastOK  time.Time
	lastErr string
	fails   int
}

var (
	_ control.Channel = (*Channel)(nil)
	_ control.Sender  = (*Channel)(nil)
)

// New returns a Channel writing to o.Sink.
func New(o Options) (*Channel, error) {
	if o.Sink == nil {
		return nil, errors.New("stdinctl: no sink; a console channel with nowhere to write is not a console channel")
	}
	c := &Channel{
		sink:   o.Sink,
		gate:   o.Gate,
		reason: o.Reason,
		actor:  o.Actor,
		now:    o.Clock,
	}
	if c.reason == "" {
		c.reason = "the server is stopped"
	}
	if c.actor == "" {
		c.actor = DefaultActor
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// Kind implements control.Channel.
//
// "none" is the honest answer: this is not a remote control channel, it is the
// absence of one, and status.json saying `"kind": "none"` for Valheim is the
// fact an operator needs rather than a reassuring invention.
func (c *Channel) Kind() control.Kind { return spec.ControlNone }

// Ready implements control.Channel.
func (c *Channel) Ready() bool {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return false
	}
	return c.gate == nil || c.gate()
}

// Unready implements control.Channel.
func (c *Channel) Unready() string {
	if c.Ready() {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "the runner is shutting down"
	}
	return c.reason
}

// Exec implements control.Channel: it writes cmd and returns no output,
// because there is none.
func (c *Channel) Exec(_ context.Context, cmd string) (string, error) {
	_, err := c.Send(c.actor, cmd)
	return "", err
}

// Send implements control.Sender.
//
// The line is validated here rather than only at the socket, because this is
// also the path a manifest-rendered shutdown rung and a hook take, and neither
// of those came through the socket. One embedded newline in a `send:` string is
// a second command run on a server that is in the middle of saving.
func (c *Channel) Send(actor, line string) (uint64, error) {
	if err := console.ValidateSendLine(line); err != nil {
		return 0, fmt.Errorf("stdinctl: %w", err)
	}
	if !c.Ready() {
		return 0, fmt.Errorf("%w: %s", control.ErrNotReady, c.Unready())
	}
	seq, err := c.sink.Send(actor, line)
	c.record(err)
	return seq, err
}

func (c *Channel) record(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.fails++
		c.lastErr = err.Error()
		return
	}
	c.fails, c.lastErr, c.lastOK = 0, "", c.now()
}

// Close implements control.Channel. It does NOT close the sink: the runner owns
// the pty and closing the game's stdin from here would send it EOF, which is
// how a FIFO-based supervisor kills a server by accident (section 11.1).
func (c *Channel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// Snapshot implements control.Reporter.
func (c *Channel) Snapshot() control.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return control.Status{
		Kind:                spec.ControlNone,
		Ready:               !c.closed && (c.gate == nil || c.gate()),
		LastOK:              c.lastOK,
		LastError:           c.lastErr,
		ConsecutiveFailures: c.fails,
	}
}

// WriterSender adapts a raw writer to control.Sender for the standalone
// shutdown path of section 11.8, where there is no ring, no socket and no
// runner -- only an open fd on the game's stdin.
//
// It returns Seq 0 for every line, and that zero is information: it says no
// durable echo exists for this command, which is exactly why 11.8 records such
// a step honestly rather than claiming the ladder ran normally.
//
// lineEnding is the manifest's; empty means "\n".
func WriterSender(w io.Writer, lineEnding string) control.Sender {
	switch lineEnding {
	case "":
		lineEnding = "\n"
	case "\n", "\r", "\r\n":
	default:
		// The terminator is appended VERBATIM after the line itself has been
		// validated, so anything else is arbitrary bytes -- multiple lines,
		// even -- following a command that passed the one-command-per-line
		// rule. The standalone shutdown path reads this value out of a file
		// the instance user can write and then writes as root, so the
		// invariant belongs here rather than only at that caller.
		return control.SenderFunc(func(string, string) (uint64, error) {
			return 0, fmt.Errorf("stdinctl: line ending %q is not one of lf, cr or crlf", lineEnding)
		})
	}
	var mu sync.Mutex
	return control.SenderFunc(func(_, line string) (uint64, error) {
		if err := console.ValidateSendLine(line); err != nil {
			return 0, fmt.Errorf("stdinctl: %w", err)
		}
		// One Write for the line and its terminator: two writes can interleave
		// with another writer's line on the same fd, and half a command
		// followed by another command's tail is a command nobody typed.
		mu.Lock()
		defer mu.Unlock()
		_, err := io.WriteString(w, line+lineEnding)
		return 0, err
	})
}
