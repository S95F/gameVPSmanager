package control

import (
	"context"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Noop is a channel that is never ready and refuses every command with a
// sentence explaining why.
//
// Two things need it, and they are not the same thing:
//
//   - A game with `control: null`. Valheim has no remote control channel and
//     never will, so the reason is permanent and the dashboard OMITS the
//     button rather than greying it.
//   - A game whose manifest names a kind this build has not implemented yet
//     (7 Days to Die ships in v1 declaring `kind: telnet`). The reason is
//     temporary and names the release.
//
// Both are a nil pointer away from being handled by "if c.control == nil"
// scattered through the commander. They are a type instead because a nil check
// has no room to carry the sentence, and the sentence is the whole product
// difference between a disabled button and a broken one.
type Noop struct {
	kind   Kind
	reason string
}

// compile-time proof that the interface is satisfied by the value, not by a
// pointer, so a Noop stored in a Channel field cannot be nil-dereferenced.
var _ Channel = (*Noop)(nil)

// NewNoop returns a channel of kind k that is never ready, with reason as its
// Unready sentence.
func NewNoop(k Kind, reason string) *Noop {
	if reason == "" {
		reason = "this control channel is not available"
	}
	return &Noop{kind: k, reason: reason}
}

// None is the channel for a game that has no control channel at all. game is
// the game's display name, used in the sentence an operator reads.
func None(game string) *Noop {
	if game == "" {
		game = "this game"
	}
	return NewNoop(spec.ControlNone,
		fmt.Sprintf("%s has no remote control channel; the console is the only command path", game))
}

// Unsupported is the channel for a kind that is known but not implemented in
// this build. The sentence comes from spec.Capabilities so the tooltip on the
// disabled button and the reason in the capability list are one string, not
// two that drift a release apart.
func Unsupported(k Kind, reason string) *Noop { return NewNoop(k, reason) }

// Kind implements Channel.
func (n *Noop) Kind() Kind { return n.kind }

// Ready implements Channel. Always false; that is the point of the type.
func (n *Noop) Ready() bool { return false }

// Unready implements Channel.
func (n *Noop) Unready() string { return n.reason }

// Exec implements Channel. The error carries the reason so a caller that
// bypassed Ready still surfaces something an operator can act on.
func (n *Noop) Exec(context.Context, string) (string, error) {
	return "", fmt.Errorf("%w: %s", ErrNoChannel, n.reason)
}

// Close implements Channel.
func (n *Noop) Close() error { return nil }

// Snapshot implements Reporter.
func (n *Noop) Snapshot() Status {
	return Status{Kind: n.kind, LastError: n.reason}
}
