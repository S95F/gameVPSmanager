package console

import (
	"fmt"
	"sync"
	"time"
)

// FakeClock is a deterministic Clock. It exists so that the 250 ms partial
// rule, the 2 s log flush and -- in later steps -- the readiness extend loop
// are asserted rather than slept through: a suite whose timing tests take real
// seconds is a suite that gets a `-short` flag and then stops running.
//
// Advance fires due timers in deadline order, on the calling goroutine, with
// the clock's lock released, so a timer callback may re-arm itself. A callback
// that re-arms past the target simply does not fire again.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	nextID uint64
}

// NewFakeClock returns a clock reading t. A zero t starts at a fixed, obvious
// instant rather than the Go zero time, so timestamps in failure output are
// readable.
func NewFakeClock(t time.Time) *FakeClock {
	if t.IsZero() {
		t = time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC)
	}
	return &FakeClock{now: t}
}

// Now reports the fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc schedules f at now+d.
func (c *FakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	t := &fakeTimer{clock: c, id: c.nextID, deadline: c.now.Add(d), fn: f, active: true}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the clock forward by d, firing every timer that comes due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	c.advanceTo(target)
}

// Set moves the clock to t, firing every timer that comes due. It refuses to
// move backwards: a clock that goes back would let a test assert something no
// production run can reach.
func (c *FakeClock) Set(t time.Time) { c.advanceTo(t) }

func (c *FakeClock) advanceTo(target time.Time) {
	for {
		c.mu.Lock()
		if target.Before(c.now) {
			c.mu.Unlock()
			return
		}
		var due *fakeTimer
		for _, t := range c.timers {
			if !t.active || t.deadline.After(target) {
				continue
			}
			if due == nil || t.deadline.Before(due.deadline) || (t.deadline.Equal(due.deadline) && t.id < due.id) {
				due = t
			}
		}
		if due == nil {
			c.now = target
			c.compactLocked()
			c.mu.Unlock()
			return
		}
		due.active = false
		c.now = due.deadline
		fn := due.fn
		c.mu.Unlock()
		// Outside the lock: the callback re-arms its timer, which needs it.
		fn()
	}
}

// Timers is how many timers are armed. A test asserting that an idle console
// costs no wakeups reads it.
func (c *FakeClock) Timers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if t.active {
			n++
		}
	}
	return n
}

func (c *FakeClock) compactLocked() {
	live := c.timers[:0]
	for _, t := range c.timers {
		if t.active {
			live = append(live, t)
		}
	}
	for i := len(live); i < len(c.timers); i++ {
		c.timers[i] = nil
	}
	c.timers = live
}

type fakeTimer struct {
	clock    *FakeClock
	id       uint64
	deadline time.Time
	fn       func()
	active   bool
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.deadline = t.clock.now.Add(d)
	t.active = true
	found := false
	for _, x := range t.clock.timers {
		if x == t {
			found = true
			break
		}
	}
	if !found {
		t.clock.timers = append(t.clock.timers, t)
	}
	return was
}

// Sent is one line injected into the fake's "stdin", in order.
type Sent struct {
	At    time.Time
	Actor string
	Line  string
	Seq   uint64 // the Seq of its echo in the ring
}

// FakeOptions configures a Fake. Zero values take the package defaults.
type FakeOptions struct {
	Instance  string
	RingBytes int
	RingLines int
	StripANSI bool
	MaxLine   int
	// PartialAfter defaults to DefaultPartialAfter. A test that only cares
	// about complete lines sets it negative to switch the timer off.
	PartialAfter time.Duration
	Clock        *FakeClock
}

// Fake is an in-memory stand-in for a running instance's console: a ring, a
// real Ingest, a fake clock, and a record of everything written towards the
// game.
//
// Later steps' tests -- the socket server, the console client, the web hub --
// need something that produces console output and accepts injected lines
// without a pty, a process or a Linux box. This is that, and because Emit goes
// through the real Ingest, those tests exercise the actual splitter rather
// than a second, kinder implementation of it.
type Fake struct {
	Instance string
	Ring     *Ring
	Ingest   *Ingest
	Clock    *FakeClock

	mu   sync.Mutex
	sent []Sent
}

// NewFake returns a wired-up in-memory console.
func NewFake(opts FakeOptions) *Fake {
	clock := opts.Clock
	if clock == nil {
		clock = NewFakeClock(time.Time{})
	}
	if opts.Instance == "" {
		opts.Instance = "fake"
	}
	ring := NewRing(opts.RingBytes, opts.RingLines).WithClock(clock.Now)
	f := &Fake{Instance: opts.Instance, Ring: ring, Clock: clock}
	f.Ingest = NewIngest(IngestOptions{
		Ring:         ring,
		Clock:        clock,
		StripANSI:    opts.StripANSI,
		MaxLine:      opts.MaxLine,
		PartialAfter: opts.PartialAfter,
	})
	return f
}

// Write feeds raw bytes in, exactly as the pump would.
func (f *Fake) Write(p []byte) (int, error) { return f.Ingest.Write(p) }

// Emit produces one complete game line and returns its Seq.
func (f *Fake) Emit(text string) uint64 {
	_, _ = f.Ingest.Write([]byte(text + "\n"))
	return f.Ring.Head()
}

// Emitf is Emit with formatting.
func (f *Fake) Emitf(format string, a ...any) uint64 { return f.Emit(fmt.Sprintf(format, a...)) }

// Note appends a runner annotation, as the runner does around each rung of the
// shutdown ladder.
func (f *Fake) Note(text string) uint64 {
	return f.Ring.Append(Line{At: f.Clock.Now(), Source: SourceRunner, Text: text})
}

// Send injects a line towards the game. It validates like the real socket,
// records the line for inspection, and echoes it into the ring as SourceActor
// -- because that echo is what makes the console log a record of who typed
// what (section 11.4).
func (f *Fake) Send(actor, line string) (uint64, error) {
	if err := ValidateSendLine(line); err != nil {
		return 0, err
	}
	at := f.Clock.Now()
	seq := f.Ring.Append(Line{At: at, Source: SourceActor, Actor: actor, Text: line})
	f.mu.Lock()
	f.sent = append(f.sent, Sent{At: at, Actor: actor, Line: line, Seq: seq})
	f.mu.Unlock()
	return seq, nil
}

// Sent returns everything sent towards the game, in order.
func (f *Fake) Sent() []Sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Sent(nil), f.sent...)
}

// SentLines returns just the text of what was sent, which is what most
// assertions actually compare.
func (f *Fake) SentLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, s := range f.sent {
		out[i] = s.Line
	}
	return out
}

// Texts returns the text of every line currently in the ring, oldest first.
func (f *Fake) Texts() []string {
	lines, _, _ := f.Ring.Snapshot(0, 0)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}

// Close flushes any pending partial line and ends every subscription.
func (f *Fake) Close() error {
	err := f.Ingest.Close()
	f.Ring.Close()
	return err
}
