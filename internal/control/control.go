// Package control is the command path to a running game server: the console
// (always there, no reply) and the optional control channel (RCON today, a
// reply, and only once the game is listening).
//
// The thesis of section 11.9 is a single sentence and everything here follows
// from it: THE CONSOLE IS THE CONTROL PLANE, CONTROL IS AN ACCELERATOR. Half
// the catalogue has no RCON at all, and the half that does spends its first
// forty minutes not listening on it. A design that treats RCON as the way to
// talk to a server produces a dashboard that is broken for Valheim forever and
// broken for Project Zomboid for an hour every restart. So the Commander routes
// to control when it can answer and falls back to the console when it cannot,
// and the manifest says honestly which verbs exist at all.
//
// Section 11.9 spells the interface below `Control`. It is `Channel` here
// because `control.Control` stutters, and because the console is a Channel too:
// the routing decision is between two of the same shape, not between a
// first-class thing and a fallback.
//
// This package is pure Go with no build tags. The sockets live in the
// subpackages (rconctl dials TCP, stdinctl writes to whatever the runner gives
// it), so the routing, the capability reporting and the argument refusal all
// run and are tested on the Windows development box.
package control

import (
	"context"
	"errors"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Kind is the control channel a manifest asks for.
//
// It is an alias, not a second enum. internal/spec owns the set because the
// manifest is what names it, and a manifest must never be able to ask for
// behaviour this build does not have (D-07's rule, applied to the channel as
// well as to the dialect).
type Kind = spec.ControlKind

// Channel is one command path to a running server.
//
// Ready/Unready are two halves of the same answer and both are required: a
// channel that reports only "not ready" produces a greyed button with no
// tooltip, which is the exact UI failure section 8.5 exists to prevent. Unready
// returns one sentence, rendered verbatim.
type Channel interface {
	Kind() Kind

	// Ready reports whether Exec can be expected to work right now. It is
	// false for the whole of Project Zomboid's 40-60 minute first boot.
	Ready() bool

	// Unready explains why Ready is false, in one sentence, for the disabled
	// button's tooltip. It returns "" when Ready is true.
	Unready() string

	// Exec runs one already-rendered command and returns whatever the channel
	// got back. A console channel always returns "": there is no reply, and
	// pretending otherwise by scraping the ring would attribute the next line
	// of game output to the operator's command.
	Exec(ctx context.Context, cmd string) (string, error)

	Close() error
}

// Sender injects one line towards the game's stdin and returns the Seq of the
// echo the runner appended to the ring.
//
// The echo is not optional (section 11.4): the console log is the only durable
// record of who typed what into a game console, so the Seq coming back is the
// evidence that the record exists. A Sender that returns 0 is telling the
// caller there is no such record -- the standalone shutdown path writing to
// /proc/<pid>/fd/0 is the one legitimate case.
type Sender interface {
	Send(actor, line string) (seq uint64, err error)
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(actor, line string) (uint64, error)

// Send implements Sender.
func (f SenderFunc) Send(actor, line string) (uint64, error) { return f(actor, line) }

var (
	// ErrNotReady means the channel exists but is not up yet. It is not a
	// failure of the command and must never be counted as one: dialing a
	// server that has not finished booting is the caller's mistake, not the
	// server's.
	ErrNotReady = errors.New("control: channel is not ready")

	// ErrNoChannel means there is no such channel for this game at all --
	// Valheim asked for RCON. Distinct from ErrNotReady because one resolves
	// itself in forty minutes and the other never does.
	ErrNoChannel = errors.New("control: game has no control channel")

	// ErrClosed means the channel has been shut down.
	ErrClosed = errors.New("control: channel is closed")
)

// Status is the `control` block of status.json (section 11.4), and its field
// set is exactly that block's -- no more.
//
// It is written by the runner every 2 s and read by gvmd and the dashboard with
// Postgres down, so it carries the three facts an operator asks in order: is it
// up, when did it last work, and what did it say when it stopped working.
type Status struct {
	Kind                Kind      `json:"kind"`
	Dialect             string    `json:"dialect,omitempty"`
	Ready               bool      `json:"ready"`
	LastOK              time.Time `json:"last_ok,omitzero"`
	LastError           string    `json:"last_error"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}

// Reporter is the optional half of Channel: a channel that tracks its own
// health can describe it.
//
// It is separate from Channel so that a channel implementation stays the five
// methods section 11.9 declares. Anything that needs the block calls Describe,
// which always has an answer.
type Reporter interface {
	Snapshot() Status
}

// Describe returns the status block for any channel, whether or not it tracks
// health. A nil channel is reported as kind "none" rather than omitted: an
// absent block in status.json reads as "the runner has not got there yet",
// which is a different and much more alarming thing than "this game has no
// RCON".
func Describe(ch Channel) Status {
	if ch == nil {
		return Status{Kind: spec.ControlNone}
	}
	if r, ok := ch.(Reporter); ok {
		return r.Snapshot()
	}
	return Status{Kind: ch.Kind(), Ready: ch.Ready()}
}
