// Package console turns a game server's byte stream into lines that many
// readers can follow at once, and defines the NDJSON protocol the runner's
// console socket speaks (sections 11.3 and 11.4).
//
// Everything here is pure Go: no syscalls, no build tags, no platform seam.
// That is deliberate. The PTY, the unix socket and the process live in
// internal/runner and internal/socket; everything that decides what a line IS,
// and everything that decides who gets dropped when a reader stalls, lives
// here, where it runs and is tested on the Windows development box.
//
// The invariant this package exists to hold: THE PRODUCER NEVER BLOCKS. A
// game server's stdout is a pty or a pipe with a small kernel buffer. If the
// pump draining it stops, the game's next write blocks in the kernel, and a
// game blocked on a write is a game that has stopped saving. So every step
// from Ingest.Write through Ring.Append to the last subscriber is either
// non-blocking or bounded by a rename -- never by how fast a browser reads.
package console

import (
	"errors"
	"strconv"
	"time"
)

// Source says where a console line came from. It is part of the wire format,
// so it marshals as a name rather than as the integer.
type Source uint8

const (
	// SourceGame is bytes read from the child process.
	SourceGame Source = iota
	// SourceRunner is gvm's own annotation, e.g. "-- gvm: sending save".
	// These interleave with game output so the log shows what the supervisor
	// was doing at the moment the game did whatever it did.
	SourceRunner
	// SourceActor is the echo of an injected command, attributed. The console
	// log is the only durable record of who typed what into a game console
	// (section 11.4), so the echo is a requirement, not a nicety.
	SourceActor
)

var sourceNames = [...]string{"game", "runner", "actor"}

// ErrBadSource is returned when a wire frame names a source this build does
// not know. The set is closed on purpose: an unknown source in an audit-facing
// record must fail loudly rather than decode to SourceGame.
var ErrBadSource = errors.New("console: unknown line source")

func (s Source) String() string {
	if int(s) < len(sourceNames) {
		return sourceNames[s]
	}
	return "source(" + strconv.Itoa(int(s)) + ")"
}

// Valid reports whether s is one of the three declared sources.
func (s Source) Valid() bool { return int(s) < len(sourceNames) }

// MarshalJSON writes the source name.
func (s Source) MarshalJSON() ([]byte, error) {
	if !s.Valid() {
		return nil, ErrBadSource
	}
	return strconv.AppendQuote(nil, sourceNames[s]), nil
}

// UnmarshalJSON accepts only the three declared names.
func (s *Source) UnmarshalJSON(b []byte) error {
	name, err := strconv.Unquote(string(b))
	if err != nil {
		return ErrBadSource
	}
	for i, n := range sourceNames {
		if n == name {
			*s = Source(i)
			return nil
		}
	}
	return ErrBadSource
}

// Line is one console line.
//
// Seq is monotonic per runner process and is the cursor for every reader: the
// socket, the dashboard's SSE `id:`, and the log tail. It is assigned by the
// Ring and is never reused within one runner -- except by Ring.Supersede,
// which deliberately republishes the SAME Seq when a line is rewritten in
// place. A reader keyed on Seq therefore replaces rather than appends, which
// is what makes a progress bar and a partially flushed line render correctly
// instead of scrolling a hundred near-identical rows past the operator.
type Line struct {
	Seq     uint64    `json:"seq"`
	At      time.Time `json:"t"`
	Source  Source    `json:"src"`
	Actor   string    `json:"actor,omitempty"` // "user:alice@x" | "token:ab12" | "root_cli" | "unix:pzserver"
	Text    string    `json:"line"`
	Partial bool      `json:"partial,omitempty"` // no line terminator seen: an idle flush or a force-split
}

// lineOverhead is charged against the ring's byte budget on top of the line's
// own strings, so that 20 000 empty lines cannot sit in a "2 MiB" ring while
// costing several megabytes of Go structs and timestamps.
const lineOverhead = 64

// size is what this line costs the ring's byte budget.
func (l Line) size() int { return len(l.Text) + len(l.Actor) + lineOverhead }
