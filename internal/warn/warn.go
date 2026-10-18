// Package warn is the decision logic for Phase 41's planned-restart warning:
// WHEN to broadcast a countdown, and WHICH operator messages are safe to send
// to a game console.
//
// It is deliberately pure. The runtime half -- the flags on stop/restart/update
// and `warn:` on a job, the actual broadcast over the runner's control path at
// each tick, the Ctrl-C that aborts before any state change, and the "skip the
// wait when the player count is known to be 0" -- drives these two functions
// but lives with the lifecycle. Keeping the schedule and the message gate here,
// as functions a test can call with a literal duration and a literal string,
// is what lets "when does a 90-second warning actually broadcast" and "is this
// message safe" be argued with directly.
package warn

import (
	"errors"
	"sort"
	"time"
)

// Schedule returns the offsets BEFORE the stop at which a countdown broadcasts,
// largest first (so a caller sleeps from one to the next).
//
// The first broadcast is the full warning -- "restarting in <warn>" -- and the
// one-minute and ten-second marks are the reminders a player glances up for. A
// mark is included only when it falls at or inside the window, and duplicates
// collapse: a two-minute warning broadcasts at 2m, 1m and 10s; a thirty-second
// one at 30s and 10s; a ten-second one just once. A zero or negative warn is
// the CLI default -- off -- and broadcasts nothing.
func Schedule(warn time.Duration) []time.Duration {
	if warn <= 0 {
		return nil
	}
	cand := []time.Duration{warn, time.Minute, 10 * time.Second}
	seen := map[time.Duration]bool{}
	out := make([]time.Duration, 0, len(cand))
	for _, d := range cand {
		if d <= 0 || d > warn || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out
}

// MaxMessage caps a broadcast message. It is about a broadcast being a
// broadcast, the same reason control.MaxArg is 512: a longer "message" is an
// accident or a probe at the game parser's limits, not something a player reads.
const MaxMessage = 512

// ErrUnsafeMessage is a message carrying a character a game console cannot be
// trusted to quote. The offending value is never in the error: it is on its way
// into a log line and an audit row, and echoing a newline back into either
// would reproduce the injection the refusal just prevented.
var ErrUnsafeMessage = errors.New("warn: the message contains a character a game console cannot be trusted to quote")

// ErrMessageTooLong is a message past MaxMessage characters.
var ErrMessageTooLong = errors.New("warn: the message is too long for a broadcast")

// ValidateMessage gates an operator's broadcast message.
//
// A game console has no quoting standard gvm can escape to, so the only safe
// rule is refusal of the characters that end or extend a command: the quotes, a
// backslash, a dollar, a backtick, and every control character -- a newline in
// a Minecraft "message" is not a rude word in the chat, it is a second command
// running as the server. An empty message is fine: it means warn with the
// countdown alone and no text.
func ValidateMessage(msg string) error {
	if len(msg) > MaxMessage {
		return ErrMessageTooLong
	}
	for _, r := range msg {
		if r < 0x20 || r == 0x7f {
			return ErrUnsafeMessage // C0 controls incl. \n, \r and NUL, plus DEL
		}
		switch r {
		case '\'', '"', '\\', '$', '`':
			return ErrUnsafeMessage
		}
	}
	return nil
}
