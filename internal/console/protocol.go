package console

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Proto is the console socket's protocol version, echoed in the hello event.
// It is an integer and not a semver because there is exactly one client and
// one server and they ship in the same binary; the field exists so that a
// mismatched pair fails with a sentence instead of a decode error.
const Proto = 1

// Frame limits.
const (
	// MaxRequestFrame bounds an inbound op. Ops are tiny; the cap is here so
	// that a peer -- and after D-09 the web tier is a peer -- cannot make the
	// runner allocate by sending a gigabyte without a newline in it.
	MaxRequestFrame = 64 << 10
	// MaxEventFrame bounds an outbound event. A `lines` batch is the only
	// event that gets near it, which is what SplitBatch is for.
	MaxEventFrame = 4 << 20
	// MaxSendLine is the longest line an op may inject. Section 16.6 puts this
	// check in the web handler; it lives here as well because D-09 puts
	// gvm-web directly on this socket, and a limit enforced only on the far
	// side of a trust boundary is not a limit.
	MaxSendLine = 1024
	// MaxReason caps a stop frame's `reason`. It is generous against every
	// value the spec's own examples carry and against shutdown_reports.reason,
	// and it exists because the field is otherwise bounded only by the 64 KiB
	// frame limit -- a single stop could write 64 KiB into console.log.
	MaxReason = 512
	// MaxBacklog caps a tail request, so one client cannot ask the runner to
	// marshal an entire 20 000-line ring per reconnect.
	MaxBacklog = DefaultRingLines
)

// Op is a client request. The set is closed: an unknown op is an error, never
// a no-op, because a silently ignored `stop` is a world that never saved.
type Op string

const (
	OpHello  Op = "hello"
	OpTail   Op = "tail"
	OpSend   Op = "send"
	OpVerb   Op = "verb"
	OpStatus Op = "status"
	OpCaps   Op = "caps"
	OpPing   Op = "ping"
	OpStop   Op = "stop"
)

// Ops returns every declared op, in the order section 11.4 lists them.
func Ops() []Op { return []Op{OpHello, OpTail, OpSend, OpVerb, OpStatus, OpCaps, OpPing, OpStop} }

// Lifecycle reports whether this op is one of the two the peer-credential
// table restricts to uid 0 (section 11.4): `verb` and `stop` are lifecycle
// actions and reach the runner through gvmd, where they are RBAC-gated and
// audited a second time.
func (o Op) Lifecycle() bool { return o == OpVerb || o == OpStop }

// EventKind is a server event.
type EventKind string

const (
	EvHello     EventKind = "hello"
	EvLines     EventKind = "lines"
	EvLagged    EventKind = "lagged"
	EvTruncated EventKind = "truncated"
	EvSent      EventKind = "sent"
	EvVerb      EventKind = "verb"
	EvStatus    EventKind = "status"
	EvCaps      EventKind = "caps"
	EvPong      EventKind = "pong"
	EvShutdown  EventKind = "shutdown"
	EvError     EventKind = "error"
)

// Error codes carried by EvError. A closed set, so a client can branch on the
// code and only render the sentence.
const (
	CodeBadRequest  = "bad_request"
	CodeDenied      = "denied"
	CodeUnsupported = "unsupported"
	CodeInternal    = "internal"
)

// Shutdown event phases.
const (
	PhaseStep = "step"
	PhaseDone = "done"
)

// Protocol errors.
var (
	ErrFrameTooLarge = errors.New("console: NDJSON frame exceeds the frame limit")
	ErrEmptyOp       = errors.New("console: frame has no op")
	ErrLineTooLong   = fmt.Errorf("console: send line exceeds %d bytes", MaxSendLine)
	ErrLineEmpty     = errors.New("console: send line is empty")
	ErrLineBreak     = errors.New("console: send line contains a line break")
	ErrNoVerb        = errors.New("console: verb op names no verb")
	ErrBadBacklog    = errors.New("console: tail backlog is out of range")
	ErrReasonBreak   = errors.New("console: stop reason contains a line break")
	ErrReasonTooLong = fmt.Errorf("console: stop reason exceeds %d bytes", MaxReason)
)

// UnknownOpError names the op a peer sent that this build does not implement.
type UnknownOpError struct{ Op Op }

func (e *UnknownOpError) Error() string { return fmt.Sprintf("console: unknown op %q", string(e.Op)) }

// Request is one client frame. It is a single flat struct rather than a union
// because that is exactly what the wire is: NDJSON that stays greppable and
// stays debuggable with `socat - UNIX-CONNECT:/run/gvm/<n>/console.sock`.
type Request struct {
	Op Op `json:"op"`

	// hello
	Client string `json:"client,omitempty"`

	// tail
	From    uint64 `json:"from,omitempty"`
	Backlog int    `json:"backlog,omitempty"`
	Follow  bool   `json:"follow,omitempty"`

	// send
	Line string `json:"line,omitempty"`

	// verb
	Verb string            `json:"verb,omitempty"`
	Args map[string]string `json:"args,omitempty"`

	// stop
	Reason string `json:"reason,omitempty"`

	// send, verb and stop. The runner does not trust this string for
	// authorization -- that is SO_PEERCRED's job -- it records it, so the
	// console log says who typed what.
	Actor string `json:"actor,omitempty"`
}

// Validate checks the frame against its op.
func (r Request) Validate() error {
	switch r.Op {
	case "":
		return ErrEmptyOp
	case OpHello, OpStatus, OpCaps, OpPing:
		return nil
	case OpTail:
		if r.Backlog < 0 || r.Backlog > MaxBacklog {
			return ErrBadBacklog
		}
		return nil
	case OpSend:
		return ValidateSendLine(r.Line)
	case OpVerb:
		if strings.TrimSpace(r.Verb) == "" {
			return ErrNoVerb
		}
		return nil
	case OpStop:
		return ValidateStopReason(r.Reason)
	default:
		return &UnknownOpError{Op: r.Op}
	}
}

// ValidateSendLine checks one injected console line.
//
// One command per frame, and no embedded terminators. The runner appends the
// manifest's own line ending, so a CR or LF in the payload would run a second
// command that the audit row -- which records this exact string -- does not
// mention. A NUL would truncate the line at whatever reads it next.
func ValidateSendLine(line string) error {
	if line == "" {
		return ErrLineEmpty
	}
	if len(line) > MaxSendLine {
		return ErrLineTooLong
	}
	if strings.ContainsAny(line, "\r\n\x00") {
		return ErrLineBreak
	}
	return nil
}

// ValidateStopReason checks a stop frame's free-text reason.
//
// It is the one unchecked field on the frame and it is not gvm's own words:
// section 11's worked example is a web user's string relayed by gvmd under
// D-09. It reaches Runner.Note, which writes both the ring line and
// console.log, so a line break there forges a record of who asked for what --
// and the ring cannot otherwise be forged, because game output arrives tagged
// SourceGame. The neighbouring sinks already scrub this class: SanitizeActor
// for the actor, ValidateSendLine for an injected line, sanitizeStatus for the
// same string on its way to sd_notify.
//
// An EMPTY reason is accepted: the field is omitempty and clients legitimately
// leave it out.
func ValidateStopReason(reason string) error {
	if len(reason) > MaxReason {
		return ErrReasonTooLong
	}
	if strings.ContainsAny(reason, "\r\n\x00") {
		return ErrReasonBreak
	}
	return nil
}

// PeerInfo is what the hello event tells a client about itself: the identity
// SO_PEERCRED reported and, derived from it, which of the D-09 tiers it landed
// in. A client renders a disabled Stop button from Stop == false rather than
// discovering the tier by being refused.
type PeerInfo struct {
	UID   uint32 `json:"uid"`
	GID   uint32 `json:"gid,omitempty"`
	PID   int32  `json:"pid,omitempty"`
	Write bool   `json:"write"`
	Verb  bool   `json:"verb,omitempty"`
	Stop  bool   `json:"stop,omitempty"`
}

// Event is one server frame. Field order is the order section 11.4's examples
// print them, and protocol_test.go asserts every documented frame round-trips
// byte for byte -- so the spec's transcript is executable.
type Event struct {
	Ev EventKind `json:"ev"`

	// hello, sent, verb
	OK *bool `json:"ok,omitempty"`

	// hello
	Proto    int       `json:"proto,omitempty"`
	Instance string    `json:"instance,omitempty"`
	Build    string    `json:"build,omitempty"`
	Peer     *PeerInfo `json:"peer,omitempty"`

	// lines, lagged, truncated
	Head       uint64 `json:"head,omitempty"`
	Oldest     uint64 `json:"oldest,omitempty"`
	Lines      []Line `json:"lines,omitempty"`
	Dropped    uint64 `json:"dropped,omitempty"`
	ResumeFrom uint64 `json:"resume_from,omitempty"`

	// sent
	Seq uint64 `json:"seq,omitempty"`

	// verb
	Channel string `json:"channel,omitempty"`
	Output  string `json:"output,omitempty"`

	// status and caps carry the runner's own JSON verbatim. They are raw on
	// purpose: this package is the transport, and giving it a compile-time
	// dependency on the runner's status struct would close an import cycle
	// (the runner imports the ring).
	Status json.RawMessage `json:"status,omitempty"`
	Caps   json.RawMessage `json:"caps,omitempty"`

	// shutdown
	Phase  string          `json:"phase,omitempty"`
	Rung   string          `json:"rung,omitempty"`
	Send   string          `json:"send,omitempty"`
	Report json.RawMessage `json:"report,omitempty"`

	// error
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// okPtr returns a fresh pointer per event. A shared package-level &true would
// let one caller mutating *ev.OK rewrite every other event's answer.
func okPtr(b bool) *bool { return &b }

// Ok reports whether the event carries an explicit ok:true.
func (e Event) Ok() bool { return e.OK != nil && *e.OK }

// HelloEvent answers an op:hello.
func HelloEvent(instance, build string, peer PeerInfo) Event {
	return Event{Ev: EvHello, OK: okPtr(true), Proto: Proto, Instance: instance, Build: build, Peer: &peer}
}

// LinesEvent delivers console lines with the ring's bounds, so a client always
// knows how much history still exists behind its cursor.
func LinesEvent(head, oldest uint64, lines []Line) Event {
	return Event{Ev: EvLines, Head: head, Oldest: oldest, Lines: lines}
}

// LaggedEvent says this client was too slow and lines were dropped for it
// alone. resumeFrom is the Seq of the next line it will receive.
func LaggedEvent(dropped, resumeFrom uint64) Event {
	return Event{Ev: EvLagged, Dropped: dropped, ResumeFrom: resumeFrom}
}

// TruncatedEvent says the requested cursor is older than the ring's oldest
// line: history is gone. Saying so is the point -- resuming silently lets
// somebody conclude a crash left no trace.
func TruncatedEvent(oldest uint64) Event { return Event{Ev: EvTruncated, Oldest: oldest} }

// SentEvent confirms an injected line, naming the Seq of its echo in the ring.
func SentEvent(seq uint64) Event { return Event{Ev: EvSent, OK: okPtr(true), Seq: seq} }

// VerbEvent reports a verb's result and which channel served it, so an
// operator can see that a game with no RCON fell back to the console.
func VerbEvent(channel, output string) Event {
	return Event{Ev: EvVerb, OK: okPtr(true), Channel: channel, Output: output}
}

// StatusEvent carries status.json's bytes.
func StatusEvent(raw json.RawMessage) Event { return Event{Ev: EvStatus, Status: raw} }

// CapsEvent carries the derived capability states.
func CapsEvent(raw json.RawMessage) Event { return Event{Ev: EvCaps, Caps: raw} }

// PongEvent answers an op:ping.
func PongEvent() Event { return Event{Ev: EvPong, OK: okPtr(true)} }

// ShutdownStepEvent narrates one rung of the ladder while it runs, which is
// the difference between "stopping..." and "sending save, waiting for the
// marker" on an operator's screen during a 90 second save.
func ShutdownStepEvent(rung, send string) Event {
	return Event{Ev: EvShutdown, Phase: PhaseStep, Rung: rung, Send: send}
}

// ShutdownDoneEvent carries the final Report.
func ShutdownDoneEvent(report json.RawMessage) Event {
	return Event{Ev: EvShutdown, Phase: PhaseDone, Report: report}
}

// ErrorEvent refuses a frame with a code a client can branch on and a sentence
// a human can read.
func ErrorEvent(code, msg string) Event {
	return Event{Ev: EvError, OK: okPtr(false), Code: code, Error: msg}
}

// Encoder writes NDJSON frames.
//
// It serialises writes. The runner's connection handler answers ops on one
// goroutine while the ring fan-out streams lines on another, and two
// interleaved json.Encoder writes on the same socket would produce a frame
// neither side can parse.
type Encoder struct {
	mu  sync.Mutex
	w   io.Writer
	buf bytes.Buffer
	je  *json.Encoder
	max int
}

// NewEncoder returns an encoder writing to w, bounded by MaxEventFrame.
func NewEncoder(w io.Writer) *Encoder {
	e := &Encoder{w: w, max: MaxEventFrame}
	e.je = json.NewEncoder(&e.buf)
	// The far side is html/template, which escapes at render; escaping here as
	// well would only make the wire unreadable to the socat that section 11.4
	// promises works.
	e.je.SetEscapeHTML(false)
	return e
}

// SetMaxFrame overrides the outbound frame limit.
func (e *Encoder) SetMaxFrame(n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.max = n
}

// Encode writes one frame, newline-terminated, in a single Write.
func (e *Encoder) Encode(v any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.buf.Reset()
	if err := e.je.Encode(v); err != nil {
		return err
	}
	if e.max > 0 && e.buf.Len() > e.max {
		return ErrFrameTooLarge
	}
	_, err := e.w.Write(e.buf.Bytes())
	return err
}

// Event writes one server frame.
func (e *Encoder) Event(ev Event) error { return e.Encode(ev) }

// Request writes one client frame.
func (e *Encoder) Request(r Request) error { return e.Encode(r) }

// Decoder reads NDJSON frames with a hard size cap.
//
// It is a bufio.Reader and a length check rather than a json.Decoder because
// json.Decoder will happily buffer an unbounded frame, and this socket is
// reachable by the web tier.
type Decoder struct {
	r   *bufio.Reader
	max int
}

// NewDecoder returns a decoder reading from r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, 8<<10), max: MaxEventFrame}
}

// SetMaxFrame overrides the inbound frame limit.
func (d *Decoder) SetMaxFrame(n int) { d.max = n }

// Frame reads one raw NDJSON frame, without its terminator. Blank lines are
// skipped, so a peer may use a bare newline as a keepalive.
func (d *Decoder) Frame() ([]byte, error) { return d.frame(d.max) }

func (d *Decoder) frame(max int) ([]byte, error) {
	if max <= 0 {
		max = MaxEventFrame
	}
	for {
		var buf []byte
		for {
			chunk, err := d.r.ReadSlice('\n')
			if len(buf)+len(chunk) > max {
				return nil, ErrFrameTooLarge
			}
			buf = append(buf, chunk...)
			if err == nil {
				break
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(err, io.EOF) && len(buf) > 0 {
				// NDJSON frames are newline-terminated. A frame cut off by a
				// closed socket is a truncated frame, not a short one.
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if line := bytes.TrimRight(buf, "\r\n"); len(line) > 0 {
			return line, nil
		}
	}
}

// Request reads and validates one client frame. It returns the decoded request
// alongside a validation error, so a server can name the offending op in the
// error event it sends back.
func (d *Decoder) Request() (Request, error) {
	b, err := d.frame(MaxRequestFrame)
	if err != nil {
		return Request{}, err
	}
	var r Request
	if err := json.Unmarshal(b, &r); err != nil {
		return Request{}, fmt.Errorf("console: bad request frame: %w", err)
	}
	return r, r.Validate()
}

// Event reads one server frame.
func (d *Decoder) Event() (Event, error) {
	b, err := d.frame(d.max)
	if err != nil {
		return Event{}, err
	}
	var ev Event
	if err := json.Unmarshal(b, &ev); err != nil {
		return Event{}, fmt.Errorf("console: bad event frame: %w", err)
	}
	return ev, nil
}

// SplitBatch chops lines into groups whose encoded `lines` event stays under
// max bytes.
//
// The estimate is deliberately conservative -- a line's own bytes plus a fixed
// per-line overhead for the JSON keys and the RFC 3339 timestamp -- because
// the cost of guessing low is a frame the encoder refuses, which is console
// output an operator never sees. Encode still enforces the real cap.
func SplitBatch(lines []Line, max int) [][]Line {
	if len(lines) == 0 {
		return nil
	}
	if max <= 0 {
		max = MaxEventFrame
	}
	const perLineJSON = 128 // {"seq":N,"t":"2026-09-04T18:22:41.123456789Z","src":"game","line":""}
	const envelope = 128    // {"ev":"lines","head":N,"oldest":N,"lines":[]}
	budget := max - envelope
	if budget <= 0 {
		budget = max
	}

	var out [][]Line
	start, size := 0, 0
	for i, l := range lines {
		cost := len(l.Text) + len(l.Actor) + perLineJSON
		if i > start && size+cost > budget {
			out = append(out, lines[start:i])
			start, size = i, 0
		}
		size += cost
	}
	return append(out, lines[start:])
}
