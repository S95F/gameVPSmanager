package console

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"time"
)

// Ring defaults, from section 11.3.
const (
	// DefaultRingBytes is the byte budget. The ring stores LINES with a byte
	// budget rather than raw bytes with a line index, because every reader of
	// the ring consumes lines; the raw bytes have a different consumer (the
	// log file) and it is fed from the pump, upstream of the splitter.
	DefaultRingBytes = 2 << 20
	// DefaultRingLines bounds the line count independently, so a server that
	// prints a million empty lines evicts on count instead of sitting in a
	// nearly empty ring.
	DefaultRingLines = 20000
	// DefaultSubDepth is the subscriber channel depth, in batches.
	DefaultSubDepth = 64
)

// ErrRingClosed means the ring was closed while a reader was waiting. A
// runner closes its ring on exit, which is how `gvm console` learns that the
// thing it was tailing is gone rather than hanging on a dead socket.
var ErrRingClosed = errors.New("console: ring closed")

// Batch is one delivery to a subscriber.
//
// Dropped is the whole backpressure contract in one field: it counts the lines
// discarded since this subscriber's previous batch because its channel was
// full. Non-zero means the reader owes its own client a `lagged` event, and
// ResumeFrom for that event is Lines[0].Seq. Carrying the count IN the batch
// rather than in a side channel is what makes it race-free -- a reader can
// never observe a drop count that belongs to lines it has not seen yet.
type Batch struct {
	Lines   []Line
	Dropped uint64
}

// Ring is a bounded buffer of console lines with multiple subscribers.
//
// Ring NEVER blocks the producer. A subscriber that cannot keep up has its
// backlog dropped and is told how much it lost, because stalling the pump
// would stall the game's stdout and eventually the game itself -- a web client
// that stopped reading must not be able to hang a server that is mid-save.
type Ring struct {
	mu       sync.Mutex
	buf      []Line
	start    int // index in buf of the oldest live line
	bytes    int
	maxBytes int
	maxLines int
	next     uint64 // Seq to assign to the next appended line
	closed   bool
	subs     map[*Subscription]struct{}
	now      func() time.Time
}

// NewRing returns a ring bounded by both a byte budget and a line count.
// Non-positive arguments take the section 11.3 defaults.
func NewRing(maxBytes, maxLines int) *Ring {
	if maxBytes <= 0 {
		maxBytes = DefaultRingBytes
	}
	if maxLines <= 0 {
		maxLines = DefaultRingLines
	}
	return &Ring{
		maxBytes: maxBytes,
		maxLines: maxLines,
		next:     1, // Seq 0 means "before the beginning" for every cursor
		subs:     make(map[*Subscription]struct{}),
		now:      time.Now,
	}
}

// WithClock replaces the clock that stamps Line.At when a producer left it
// zero. Tests only, and only during construction.
func (r *Ring) WithClock(now func() time.Time) *Ring {
	r.now = now
	return r
}

// Append adds a line, assigning it the next Seq, and returns that Seq.
//
// It fans the line out to every subscriber with a non-blocking send. It does
// not allocate for subscribers that are already full, and it never waits.
func (r *Ring) Append(l Line) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0
	}
	if l.At.IsZero() {
		l.At = r.now()
	}
	l.Seq = r.next
	r.next++
	r.buf = append(r.buf, l)
	r.bytes += l.size()
	r.evictLocked()
	r.fanoutLocked(l)
	return l.Seq
}

// Supersede rewrites the line at seq in place and republishes it to every
// subscriber under the same Seq. It reports whether the line was still in the
// ring; a caller that gets false should Append instead.
//
// This is how rule 5's "the completed line supersedes the partial one" and the
// bare-CR progress bar are implemented without inventing a second cursor
// space: the head does not move, so a reader that already has seq N simply
// replaces its copy, and a reader that has not reached N yet sees only the
// final text.
func (r *Ring) Supersede(seq uint64, l Line) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.liveLocked() == 0 {
		return false
	}
	oldest := r.buf[r.start].Seq
	if seq < oldest || seq > r.next-1 {
		return false
	}
	i := r.start + int(seq-oldest)
	old := r.buf[i]
	l.Seq = seq
	if l.At.IsZero() {
		l.At = old.At
	}
	r.bytes += l.size() - old.size()
	r.buf[i] = l
	r.evictLocked()
	r.fanoutLocked(l)
	return true
}

// Head is the highest Seq the ring has assigned, or 0 when nothing has been
// appended. The shutdown ladder captures it before every send, so that a
// wait_for_log can only be satisfied by a line that arrived AFTER the command
// it is waiting on (section 11.6 rule 1).
func (r *Ring) Head() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next - 1
}

// Oldest is the lowest Seq still retained, or 0 when the ring is empty. A
// reader asking for a cursor below it has lost history and must be told so
// rather than silently resumed at the oldest line.
func (r *Ring) Oldest() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.oldestLocked()
}

// Len is the number of lines currently retained.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.liveLocked()
}

// Bytes is the ring's current occupancy against its byte budget.
func (r *Ring) Bytes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytes
}

// Subscribers is the count status.json reports.
func (r *Ring) Subscribers() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subs)
}

// Snapshot returns up to limit lines starting at Seq >= from, plus the ring's
// current bounds. A non-positive limit means "everything from there".
//
// It pages FORWARD from a cursor; for "the last N lines" use Tail. The two are
// separate methods because conflating them is how a backlog request silently
// returns the wrong end of a boot log.
func (r *Ring) Snapshot(from uint64, limit int) (lines []Line, oldest, head uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	oldest, head = r.oldestLocked(), r.next-1
	live := r.liveLocked()
	if live == 0 || from > head {
		return nil, oldest, head
	}
	i := r.start
	if from > oldest {
		i += int(from - oldest)
	}
	out := r.buf[i : r.start+live]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return append([]Line(nil), out...), oldest, head
}

// Tail returns the newest n lines plus the ring's bounds. This is what a
// `tail` op with from == 0 asks for. A non-positive n means "everything",
// matching Snapshot's limit, so the two cannot be read as opposites.
func (r *Ring) Tail(n int) (lines []Line, oldest, head uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	oldest, head = r.oldestLocked(), r.next-1
	live := r.liveLocked()
	if live == 0 {
		return nil, oldest, head
	}
	i := r.start
	if n > 0 && n < live {
		i = r.start + live - n
	}
	return append([]Line(nil), r.buf[i:r.start+live]...), oldest, head
}

// Close ends every subscription and refuses further appends. Readers see their
// channel close, which is how a console client learns the runner is gone.
func (r *Ring) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for s := range r.subs {
		s.closed = true
		close(s.ch)
		delete(r.subs, s)
	}
}

func (r *Ring) liveLocked() int { return len(r.buf) - r.start }

func (r *Ring) oldestLocked() uint64 {
	if r.liveLocked() == 0 {
		return 0
	}
	return r.buf[r.start].Seq
}

// evictLocked drops from the front until both budgets are met. It always keeps
// one line, so that a single line larger than the whole byte budget is still
// visible instead of vanishing into an empty ring.
func (r *Ring) evictLocked() {
	for r.liveLocked() > 1 && (r.bytes > r.maxBytes || r.liveLocked() > r.maxLines) {
		r.bytes -= r.buf[r.start].size()
		r.buf[r.start] = Line{} // release the strings; the slot is dead
		r.start++
	}
	// Compact once the dead prefix is half the slice, which keeps eviction
	// amortised O(1) without letting the backing array grow without bound.
	if r.start > 0 && r.start*2 >= len(r.buf) {
		n := copy(r.buf, r.buf[r.start:])
		for i := n; i < len(r.buf); i++ {
			r.buf[i] = Line{}
		}
		r.buf = r.buf[:n]
		r.start = 0
	}
}

func (r *Ring) fanoutLocked(l Line) {
	for s := range r.subs {
		s.offerLocked(l)
	}
}

// Subscription is one reader's view of the ring.
//
// Read C until it closes. A Batch carrying Dropped > 0 says lines were lost
// before Lines[0] because this reader was too slow; the reader is expected to
// tell its own client and carry on, not to expect the ring to have waited.
type Subscription struct {
	// C delivers batches in Seq order. It is closed by Close or by Ring.Close.
	C <-chan Batch

	ring *Ring
	ch   chan Batch

	// The rest is guarded by ring.mu, not by a lock of its own: every write
	// happens inside Ring.Append's fan-out, so a second mutex would only add
	// a second thing to get the order wrong.
	dropped uint64
	closed  bool
	oldest  uint64
	head    uint64
}

// Subscribe returns a reader positioned at Seq >= from, with depth batches of
// buffering. A non-positive depth takes DefaultSubDepth.
//
// The backlog that already satisfies from is delivered as the first batch, in
// one piece, so a fresh subscriber cannot be marked lagged by its own history.
func (r *Ring) Subscribe(from uint64, depth int) *Subscription {
	if depth <= 0 {
		depth = DefaultSubDepth
	}
	ch := make(chan Batch, depth)
	s := &Subscription{C: ch, ring: r, ch: ch}

	r.mu.Lock()
	defer r.mu.Unlock()
	s.oldest, s.head = r.oldestLocked(), r.next-1
	if r.closed {
		s.closed = true
		close(ch)
		return s
	}
	if live := r.liveLocked(); live > 0 && from <= s.head {
		i := r.start
		if from > s.oldest {
			i += int(from - s.oldest)
		}
		if backlog := r.buf[i : r.start+live]; len(backlog) > 0 {
			// depth >= 1 and the channel is empty, so this cannot block.
			ch <- Batch{Lines: append([]Line(nil), backlog...)}
		}
	}
	r.subs[s] = struct{}{}
	return s
}

// Start reports the ring's bounds at the moment this subscription was created.
// A caller whose requested cursor is below oldest owes its client a
// `truncated` event: history was lost, and saying so is better than resuming
// silently and letting someone conclude a crash left no trace.
func (s *Subscription) Start() (oldest, head uint64) { return s.oldest, s.head }

// Close ends the subscription and closes C. It is safe to call twice.
func (s *Subscription) Close() {
	s.ring.mu.Lock()
	defer s.ring.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	delete(s.ring.subs, s)
	close(s.ch)
}

// offerLocked is the only place the producer touches a subscriber, and it is
// non-blocking by construction. Called with ring.mu held.
func (s *Subscription) offerLocked(l Line) {
	if s.closed {
		return
	}
	select {
	case s.ch <- Batch{Lines: []Line{l}, Dropped: s.dropped}:
		s.dropped = 0
	default:
		s.dropped++
	}
}

// WaitFor blocks until a line with Seq >= since matches re, or ctx ends.
//
// It is the primitive behind readiness markers, fail markers and the shutdown
// ladder's wait_for_log. Callers that must not accept a stale match pass
// since = ring.Head() + 1, captured before the command was sent: a `save`
// typed an hour ago must not satisfy the wait that guards `quit`.
func (r *Ring) WaitFor(ctx context.Context, since uint64, re *regexp.Regexp) (Line, error) {
	if re == nil {
		return Line{}, errors.New("console: WaitFor needs a pattern")
	}
	return r.WaitForFunc(ctx, since, func(l Line) bool { return re.MatchString(l.Text) })
}

// WaitForFunc is WaitFor with an arbitrary predicate, so a caller can also ask
// WHO wrote the line. The ring stays deliberately permissive about that -- it
// is a transcript, not a policy -- and the rule lives one level up in
// probe.LogMarker, which is what the ladder and readiness both go through.
func (r *Ring) WaitForFunc(ctx context.Context, since uint64, match func(Line) bool) (Line, error) {
	if match == nil {
		return Line{}, errors.New("console: WaitForFunc needs a predicate")
	}
	if since == 0 {
		since = 1 // Seq 0 is never assigned; 0 means "from the beginning"
	}
	// A deep channel plus the recovery scan below, because a missed match here
	// strands the shutdown ladder on a live world.
	sub := r.Subscribe(since, 4*DefaultSubDepth)
	defer sub.Close()

	last := since - 1
	for {
		select {
		case <-ctx.Done():
			return Line{}, ctx.Err()
		case b, ok := <-sub.C:
			if !ok {
				return Line{}, ErrRingClosed
			}
			scan := b.Lines
			if b.Dropped > 0 {
				// This reader fell behind, but the ring itself may still hold
				// the lines. Pay for a snapshot rather than lose a marker.
				scan, _, _ = r.Snapshot(last+1, 0)
			}
			for _, l := range scan {
				if l.Seq > last {
					last = l.Seq
				}
				if l.Seq >= since && match(l) {
					return l, nil
				}
			}
		}
	}
}
