package console

import (
	"errors"
	"testing"
	"time"
)

func TestFakeProducesLinesThroughTheRealIngest(t *testing.T) {
	// The point of the fake is that later steps' tests exercise the actual
	// splitter. If Emit took a shortcut into the ring, every test built on it
	// would be testing a kinder implementation than the one that ships.
	f := NewFake(FakeOptions{StripANSI: true})
	defer f.Close()

	f.Emit("SERVER STARTED")
	f.Emitf("%d mods loaded", 1699)
	if _, err := f.Write([]byte("\x1b[31mERROR\x1b[0m: disk full\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f.Write([]byte("progress 1\rprogress 2\rdone\n"))

	want := []string{"SERVER STARTED", "1699 mods loaded", "ERROR: disk full", "progress 2", "done"}
	if got := f.Texts(); !eqStrings(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestFakeSendIsValidatedRecordedAndEchoed(t *testing.T) {
	f := NewFake(FakeOptions{})
	defer f.Close()

	f.Emit("SERVER STARTED")
	seq, err := f.Send("user:alice@example.com", "players")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	lines, _, head := f.Ring.Snapshot(0, 0)
	if head != seq {
		t.Errorf("Send returned Seq %d, head is %d", seq, head)
	}
	echo := lines[len(lines)-1]
	if echo.Source != SourceActor || echo.Actor != "user:alice@example.com" || echo.Text != "players" {
		t.Fatalf("echo = %+v", echo)
	}

	if got := f.SentLines(); !eqStrings(got, []string{"players"}) {
		t.Fatalf("SentLines = %q", got)
	}
	if got := f.Sent(); len(got) != 1 || got[0].Seq != seq || got[0].Actor != "user:alice@example.com" {
		t.Fatalf("Sent = %+v", got)
	}

	// The same rule the socket enforces: one command, no embedded terminators.
	if _, err := f.Send("root_cli", "save\nquit"); !errors.Is(err, ErrLineBreak) {
		t.Fatalf("Send with a newline: %v", err)
	}
	if got := len(f.Sent()); got != 1 {
		t.Errorf("a refused Send was still recorded: %d entries", got)
	}
}

func TestFakeNoteIsARunnerAnnotation(t *testing.T) {
	f := NewFake(FakeOptions{})
	defer f.Close()
	f.Note("-- gvm: sending save")
	lines, _, _ := f.Ring.Snapshot(0, 0)
	if lines[0].Source != SourceRunner {
		t.Fatalf("Note produced source %v", lines[0].Source)
	}
}

func TestFakeUsesItsClockForTimestampsAndTimers(t *testing.T) {
	f := NewFake(FakeOptions{})
	defer f.Close()

	f.Emit("first")
	lines, _, _ := f.Ring.Snapshot(0, 0)
	if !lines[0].At.Equal(f.Clock.Now()) {
		t.Errorf("At = %v, want the fake clock's %v", lines[0].At, f.Clock.Now())
	}

	f.Write([]byte("a prompt: "))
	if got := len(f.Texts()); got != 1 {
		t.Fatalf("%d lines before the idle window", got)
	}
	f.Clock.Advance(DefaultPartialAfter)
	if got := f.Texts(); len(got) != 2 || got[1] != "a prompt: " {
		t.Fatalf("after the idle window: %q", got)
	}
}

func TestFakeCloseFlushesAndEndsSubscriptions(t *testing.T) {
	f := NewFake(FakeOptions{})
	sub := f.Ring.Subscribe(0, 4)
	f.Write([]byte("no newline here"))

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := f.Texts(); !eqStrings(got, []string{"no newline here"}) {
		t.Fatalf("Close did not flush: %q", got)
	}
	select {
	case _, ok := <-drainSub(sub):
		if ok {
			t.Fatal("the subscription is still open after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not end the subscription")
	}
}

// drainSub returns a channel that yields once the subscription's own channel
// has closed, ignoring whatever batches were buffered first.
func drainSub(s *Subscription) <-chan Batch {
	out := make(chan Batch)
	go func() {
		defer close(out)
		for range s.C {
		}
	}()
	return out
}

func TestFakeClockFiresTimersInDeadlineOrder(t *testing.T) {
	c := NewFakeClock(time.Time{})
	var order []string
	c.AfterFunc(30*time.Millisecond, func() { order = append(order, "third") })
	c.AfterFunc(10*time.Millisecond, func() { order = append(order, "first") })
	c.AfterFunc(20*time.Millisecond, func() { order = append(order, "second") })

	c.Advance(25 * time.Millisecond)
	if !eqStrings(order, []string{"first", "second"}) {
		t.Fatalf("order = %v", order)
	}
	c.Advance(10 * time.Millisecond)
	if !eqStrings(order, []string{"first", "second", "third"}) {
		t.Fatalf("order = %v", order)
	}
	if got := c.Timers(); got != 0 {
		t.Errorf("%d timers still armed", got)
	}
}

func TestFakeClockLetsACallbackRearmItself(t *testing.T) {
	// The ingest's partial timer re-arms from inside its own callback, so a
	// clock that held its lock across the call would deadlock the suite.
	c := NewFakeClock(time.Time{})
	fired := 0
	var timer Timer
	timer = c.AfterFunc(10*time.Millisecond, func() {
		fired++
		if fired < 3 {
			timer.Reset(10 * time.Millisecond)
		}
	})
	c.Advance(100 * time.Millisecond)
	if fired != 3 {
		t.Fatalf("fired %d times, want 3", fired)
	}
}

func TestFakeClockStopAndReset(t *testing.T) {
	c := NewFakeClock(time.Time{})
	fired := 0
	timer := c.AfterFunc(10*time.Millisecond, func() { fired++ })
	if !timer.Stop() {
		t.Error("Stop on an armed timer returned false")
	}
	if timer.Stop() {
		t.Error("Stop on a stopped timer returned true")
	}
	c.Advance(time.Hour)
	if fired != 0 {
		t.Fatalf("a stopped timer fired %d times", fired)
	}
	if timer.Reset(5 * time.Millisecond) {
		t.Error("Reset on a stopped timer reported it was armed")
	}
	c.Advance(5 * time.Millisecond)
	if fired != 1 {
		t.Fatalf("fired %d times after Reset", fired)
	}
}

func TestFakeClockDoesNotRunBackwards(t *testing.T) {
	c := NewFakeClock(time.Time{})
	start := c.Now()
	c.Set(start.Add(-time.Hour))
	if !c.Now().Equal(start) {
		t.Fatalf("clock moved back to %v", c.Now())
	}
}
