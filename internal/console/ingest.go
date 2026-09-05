package console

import (
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

// Ingest defaults, from section 11.3.
const (
	// DefaultMaxLine force-splits a line at 64 KiB. Rule 4 exists because a
	// mod dumping a 40 MB stack trace with no newline in it must not be
	// buffered until the runner is OOM-killed -- and an OOM-killed runner
	// orphans a live game process.
	DefaultMaxLine = 64 << 10
	// DefaultPartialAfter is rule 5's idle flush. It is what makes an
	// interactive prompt ("Enter admin password:") visible at all, since a
	// prompt is by definition a line with no newline after it.
	DefaultPartialAfter = 250 * time.Millisecond
)

// ErrIngestClosed is returned by Write after Close.
var ErrIngestClosed = errors.New("console: ingest closed")

// provKind says what the ring line at Ingest.prov is, and therefore which
// later emission is allowed to rewrite it in place.
//
// Two kinds and not one, because the two rewrites mean opposite things about
// the bytes still buffered. After an idle flush the buffer still holds the
// text that was published, so the completing line MUST overdraw it or the
// prompt appears twice. After a bare CR the buffer is empty and the published
// line is complete, so only the next CR-terminated segment -- the next frame
// of the same progress bar -- may overdraw it. A newline ENDS the progress
// run: it starts a line of its own, which keeps the bar's final state on
// screen next to whatever followed it. Overdrawing there would be
// terminal-faithful and would throw away the last thing a stalled download
// managed to say.
type provKind uint8

const (
	provNone provKind = iota
	provPartial
	provRedraw
)

// IngestOptions configures an Ingest. The zero value of every optional field
// takes the section 11.3 default.
type IngestOptions struct {
	// Ring receives the lines. Required.
	Ring *Ring

	// Raw receives every byte exactly as the child wrote it, before any
	// normalisation, and it is written FIRST (rule 1) so the on-disk log is
	// complete even for lines the ring has since evicted. Optional; in the
	// runner it is the rotating log file.
	Raw io.Writer

	// Clock drives the partial-line timer.
	Clock Clock

	// StripANSI applies rule 3. It comes from the manifest, per game.
	StripANSI bool

	// MaxLine is rule 4's force-split threshold.
	MaxLine int

	// PartialAfter is rule 5's idle window. A non-positive value disables the
	// partial flush entirely, which is what a test that only cares about
	// complete lines wants.
	PartialAfter time.Duration

	// Source stamps the lines. Zero value SourceGame, which is what the pump
	// wants; the runner's own annotations go straight to the ring instead.
	Source Source
}

// Ingest is the only place bytes become lines, so each rule of section 11.3 is
// implemented exactly once. It satisfies io.Writer, and the pump hands it
// whatever a single read from the pty master returned.
//
// Write is called from the pump goroutine and the partial timer fires on
// another, so everything is under one mutex. Neither path ever blocks on a
// reader: the ring's fan-out is non-blocking, and Raw is a buffered file.
type Ingest struct {
	ring         *Ring
	raw          io.Writer
	clock        Clock
	stripANSI    bool
	maxLine      int
	partialAfter time.Duration
	source       Source

	mu  sync.Mutex
	cur []byte
	// prov is the Seq of a line already in the ring that a later emission may
	// rewrite in place instead of appending after, and provKind says which
	// kind of rewrite is owed. Zero means the next emission is a new line.
	prov      uint64
	provKind  provKind
	pendingCR bool
	timer     Timer
	armed     bool
	// flushedLen is how many bytes of cur the last idle flush published. -1
	// means "nothing published yet for this line", which is distinguishable
	// from a genuinely empty flush.
	flushedLen int
	closed     bool
	rawErr     error
	lastByte   time.Time
	bytesIn    int64
}

// NewIngest returns an Ingest writing into opts.Ring.
func NewIngest(opts IngestOptions) *Ingest {
	if opts.Ring == nil {
		panic("console: NewIngest needs a Ring")
	}
	if opts.MaxLine <= 0 {
		opts.MaxLine = DefaultMaxLine
	}
	if opts.PartialAfter == 0 {
		opts.PartialAfter = DefaultPartialAfter
	}
	return &Ingest{
		flushedLen:   -1,
		ring:         opts.Ring,
		raw:          opts.Raw,
		clock:        clockOr(opts.Clock),
		stripANSI:    opts.StripANSI,
		maxLine:      opts.MaxLine,
		partialAfter: opts.PartialAfter,
		source:       opts.Source,
	}
}

// Write consumes one read from the child. It always reports len(p) consumed:
// a failure to persist to the log must not look to the pump like a short write
// on the game's output, and the pump has nothing useful to do with the error
// anyway. Use RawErr to see whether the log is behind.
func (in *Ingest) Write(p []byte) (int, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return 0, ErrIngestClosed
	}

	// Rule 1: raw bytes to the log first, so the on-disk log is complete even
	// if the ring evicts these lines a second later.
	if in.raw != nil && len(p) > 0 {
		if _, err := in.raw.Write(p); err != nil && in.rawErr == nil {
			in.rawErr = err
			// Say so once, in the ring, where an operator tailing the console
			// will actually see it. Silently losing the post-mortem log is
			// the failure this whole subsystem exists to prevent.
			in.ring.Append(Line{
				At:     in.clock.Now(),
				Source: SourceRunner,
				Text:   "-- gvm: console log write failed: " + err.Error(),
			})
		}
	}

	if len(p) > 0 {
		in.bytesIn += int64(len(p))
		in.lastByte = in.clock.Now()
	}
	for _, b := range p {
		in.byteLocked(b)
	}
	in.armLocked()
	return len(p), nil
}

func (in *Ingest) byteLocked(b byte) {
	if in.pendingCR {
		in.pendingCR = false
		if b == '\n' {
			// Rule 2: CRLF is one terminator, not a terminator plus a rewrite.
			in.terminateLocked(false)
			return
		}
		// Rule 2: a bare CR terminates the line, and the next CR-terminated
		// segment overdraws it. That is what turns a SteamCMD progress bar
		// into one line that counts up, instead of a few hundred lines that
		// scroll the actual error off an operator's screen.
		in.terminateLocked(true)
		// fall through: b is the first byte of the redrawn line
	}
	switch b {
	case '\r':
		in.pendingCR = true
	case '\n':
		in.terminateLocked(false)
	default:
		in.cur = append(in.cur, b)
		if len(in.cur) >= in.maxLine {
			in.splitLocked()
		}
	}
}

// terminateLocked emits a complete line. cr says the terminator was a bare
// carriage return, which both permits this line to overdraw the previous frame
// of a progress bar and leaves it open to being overdrawn by the next one.
func (in *Ingest) terminateLocked(cr bool) {
	if cr && len(in.cur) == 0 {
		// "\r\r", or a CR at the very start of a line: there is no frame to
		// draw, and an empty one would be a blank row the game never printed.
		return
	}
	overdraw := in.provKind == provPartial || (cr && in.provKind == provRedraw)
	next := provNone
	if cr {
		next = provRedraw
	}
	in.publishLocked(in.textLocked(), false, overdraw, next)
	in.resetCurLocked()
}

// splitLocked implements rule 4. Each chunk is a line in its own right --
// marked Partial so a reader knows the break is gvm's and not the game's --
// and the next chunk is NOT allowed to overdraw it, because the whole point of
// splitting a 40 MB stack trace is to keep all of it.
func (in *Ingest) splitLocked() {
	in.publishLocked(in.textLocked(), true, in.provKind == provPartial, provNone)
	in.resetCurLocked()
}

// flushPartialLocked implements rule 5 and reports whether it published. cur is
// deliberately NOT cleared: the same bytes stay buffered, and when the line
// finally completes it overdraws this Seq with the full text.
//
// A flush with nothing new in it does nothing. Rule 5's own motivating example
// is a prompt -- "Enter admin password: " -- which stays pending until a human
// answers, and republishing the identical bytes would wake every subscriber
// four times a second forever while only moving the line's timestamp forward,
// which makes the console log lie about when the game printed it.
func (in *Ingest) flushPartialLocked() bool {
	if len(in.cur) == 0 || len(in.cur) == in.flushedLen {
		return false
	}
	in.publishLocked(in.textLocked(), true, in.provKind == provPartial, provPartial)
	in.flushedLen = len(in.cur)
	return true
}

// publishLocked writes one line, overdrawing the provisional Seq when it is
// allowed to and that Seq is still in the ring. next says what the line just
// written leaves behind for a later emission.
func (in *Ingest) publishLocked(text string, partial, overdraw bool, next provKind) uint64 {
	l := Line{At: in.clock.Now(), Source: in.source, Text: text, Partial: partial}
	if overdraw && in.prov != 0 && in.ring.Supersede(in.prov, l) {
		seq := in.prov
		in.provKind = next
		if next == provNone {
			in.prov = 0
		}
		return seq
	}
	seq := in.ring.Append(l)
	if next == provNone {
		in.prov, in.provKind = 0, provNone
	} else {
		in.prov, in.provKind = seq, next
	}
	return seq
}

func (in *Ingest) textLocked() string {
	text := decodeText(in.cur)
	if in.stripANSI {
		text = StripANSI(text)
	}
	return text
}

// resetCurLocked empties the pending line, keeping the backing array. Rule 4
// caps cur at maxLine, so the retained buffer is bounded by construction and
// there is nothing here that can grow without a bound.
func (in *Ingest) resetCurLocked() {
	in.cur = in.cur[:0]
	// The next line's first idle flush must always publish, whatever the last
	// one happened to be the length of. This is the single point every
	// terminator and every rule-4 split goes through.
	in.flushedLen = -1
}

// armLocked keeps the partial timer running exactly while a line is pending.
// It does not restart a timer that is already armed: rule 5 measures from the
// last line terminator, not from the last byte, so a line drawn slowly still
// becomes visible 250 ms after it started.
func (in *Ingest) armLocked() {
	if in.partialAfter <= 0 || in.closed {
		return
	}
	if len(in.cur) == 0 && !in.pendingCR {
		if in.armed {
			in.timer.Stop()
			in.armed = false
		}
		return
	}
	if in.armed {
		return
	}
	if in.timer == nil {
		in.timer = in.clock.AfterFunc(in.partialAfter, in.onTimer)
	} else {
		in.timer.Reset(in.partialAfter)
	}
	in.armed = true
}

func (in *Ingest) onTimer() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.armed = false
	if in.closed {
		return
	}
	if in.pendingCR {
		// A CR at the end of a read is ambiguous -- CRLF, or a progress bar
		// frame? -- and only the next byte can say. The idle window is the
		// answer: no LF arrived in 250 ms, so it was a bare CR, and the frame
		// is published as the complete line it is rather than as a partial.
		in.pendingCR = false
		in.terminateLocked(true)
		in.armLocked()
		return
	}
	if in.flushPartialLocked() {
		// Something was appended since the last fire, so keep watching: a line
		// being drawn a few bytes at a time still redraws every window.
		in.armLocked()
	}
	// Otherwise leave the timer disarmed. Write calls armLocked on every read
	// and `armed` is false now, so the next byte opens a fresh window.
}

// Flush emits whatever is buffered as a complete line. The pump calls it on
// EOF (and on EIO, which is what a pty master returns when the last slave
// closes) so the final line of a crash tail is not lost for want of a newline.
func (in *Ingest) Flush() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.flushFinalLocked()
}

func (in *Ingest) flushFinalLocked() {
	// A trailing CR with nothing after it carries no text, so it is dropped
	// rather than turned into a blank line the game never printed.
	in.pendingCR = false
	if len(in.cur) > 0 {
		in.terminateLocked(false)
	}
	if in.armed {
		in.timer.Stop()
		in.armed = false
	}
}

// Close flushes and stops the timer. It does not close Raw: the log file
// outlives the ingest, and the runner closes it after writing its final
// annotations.
func (in *Ingest) Close() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	in.flushFinalLocked()
	in.closed = true
	return nil
}

// RawErr reports the first error the raw log writer returned, if any. The
// runner surfaces it in status.json rather than failing the instance: a full
// /var/log must not stop a game.
func (in *Ingest) RawErr() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.rawErr
}

// LastByte is when the child last produced output, which is what the readiness
// stall detector watches: any byte at all counts as progress (section 11.5).
func (in *Ingest) LastByte() time.Time {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.lastByte
}

// BytesIn is how many bytes the child has produced.
func (in *Ingest) BytesIn() int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.bytesIn
}

// Pending is the length of the line currently being accumulated. Tests and
// `gvm doctor` use it; nothing on the hot path does.
func (in *Ingest) Pending() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.cur)
}

// decodeText turns the child's bytes into a string that survives JSON.
//
// A game server is not obliged to speak UTF-8: Minecraft's section sign is a
// bare 0xA7, and a server built on Windows emits CP-1252 punctuation. Handing
// invalid UTF-8 to encoding/json silently rewrites every bad byte to U+FFFD,
// which corrupts exactly the crash line the log is being read for. The rule is
// the same one internal/rcon applies to its own wire: valid UTF-8 is UTF-8,
// anything else is Latin-1, and nothing is ever replaced.
func decodeText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}
