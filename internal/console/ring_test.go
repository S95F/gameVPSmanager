package console

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"
)

func line(text string) Line { return Line{Source: SourceGame, Text: text} }

func texts(lines []Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}

func TestAppendAssignsContiguousSeqsFromOne(t *testing.T) {
	r := NewRing(0, 0)
	if got := r.Head(); got != 0 {
		t.Fatalf("empty ring Head = %d, want 0", got)
	}
	if got := r.Oldest(); got != 0 {
		t.Fatalf("empty ring Oldest = %d, want 0", got)
	}
	for i := 1; i <= 5; i++ {
		if got := r.Append(line(fmt.Sprintf("l%d", i))); got != uint64(i) {
			t.Fatalf("Append #%d returned Seq %d", i, got)
		}
	}
	if got := r.Head(); got != 5 {
		t.Errorf("Head = %d, want 5", got)
	}
	if got := r.Oldest(); got != 1 {
		t.Errorf("Oldest = %d, want 1", got)
	}
}

func TestAppendStampsAtFromTheClock(t *testing.T) {
	c := NewFakeClock(time.Time{})
	r := NewRing(0, 0).WithClock(c.Now)
	r.Append(line("a"))
	lines, _, _ := r.Snapshot(0, 0)
	if !lines[0].At.Equal(c.Now()) {
		t.Fatalf("At = %v, want %v", lines[0].At, c.Now())
	}

	// A producer that stamped its own time keeps it: the pump reads the
	// timestamp when the bytes arrived, not when the ring got round to them.
	want := c.Now().Add(-time.Hour)
	r.Append(Line{At: want, Text: "b"})
	lines, _, _ = r.Snapshot(0, 0)
	if !lines[1].At.Equal(want) {
		t.Fatalf("At = %v, want the producer's %v", lines[1].At, want)
	}
}

func TestRingEvictsOnLineCount(t *testing.T) {
	r := NewRing(1<<20, 3)
	for i := 1; i <= 6; i++ {
		r.Append(line(fmt.Sprintf("l%d", i)))
	}
	lines, oldest, head := r.Snapshot(0, 0)
	if got, want := texts(lines), []string{"l4", "l5", "l6"}; !eqStrings(got, want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	if oldest != 4 || head != 6 {
		t.Fatalf("oldest,head = %d,%d want 4,6", oldest, head)
	}
}

func TestRingEvictsOnByteBudget(t *testing.T) {
	// Four lines fit; the fifth must push the first out.
	r := NewRing(4*(lineOverhead+10), 10000)
	for i := 1; i <= 8; i++ {
		r.Append(line(fmt.Sprintf("%010d", i)))
	}
	if got := r.Len(); got != 4 {
		t.Fatalf("Len = %d, want 4", got)
	}
	if got, want := r.Bytes(), 4*(lineOverhead+10); got > want {
		t.Fatalf("Bytes = %d, over budget %d", got, want)
	}
	if got := r.Oldest(); got != 5 {
		t.Errorf("Oldest = %d, want 5", got)
	}
}

func TestASingleOversizedLineIsKeptRatherThanVanishing(t *testing.T) {
	// A line bigger than the whole budget must still be readable: an empty
	// ring is a worse answer than an over-budget one when the line in
	// question is the stack trace you are looking for.
	r := NewRing(64, 10)
	r.Append(line("first"))
	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'x'
	}
	r.Append(line(string(big)))
	if got := r.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
	lines, _, _ := r.Snapshot(0, 0)
	if len(lines[0].Text) != 4096 {
		t.Fatalf("kept line has %d bytes", len(lines[0].Text))
	}
}

func TestEvictionCompactsTheBackingArray(t *testing.T) {
	r := NewRing(1<<20, 4)
	for i := 0; i < 10000; i++ {
		r.Append(line("x"))
	}
	r.mu.Lock()
	backing := len(r.buf)
	r.mu.Unlock()
	if backing > 16 {
		t.Fatalf("backing array grew to %d slots for a 4-line ring", backing)
	}
}

func TestSnapshotPagesForwardAndTailReadsTheNewestEnd(t *testing.T) {
	r := NewRing(0, 0)
	for i := 1; i <= 10; i++ {
		r.Append(line(fmt.Sprintf("l%d", i)))
	}

	lines, _, _ := r.Snapshot(4, 3)
	if got, want := texts(lines), []string{"l4", "l5", "l6"}; !eqStrings(got, want) {
		t.Errorf("Snapshot(4,3) = %v, want %v", got, want)
	}
	lines, _, _ = r.Tail(3)
	if got, want := texts(lines), []string{"l8", "l9", "l10"}; !eqStrings(got, want) {
		t.Errorf("Tail(3) = %v, want %v", got, want)
	}
	if lines, _, _ = r.Snapshot(11, 0); len(lines) != 0 {
		t.Errorf("Snapshot past head returned %d lines", len(lines))
	}
	// A cursor below oldest is clamped, and the reported oldest is how the
	// caller learns to send a `truncated` event.
	lines, oldest, _ := r.Snapshot(1, 2)
	if oldest != 1 || len(lines) != 2 {
		t.Errorf("Snapshot(1,2) = %d lines, oldest %d", len(lines), oldest)
	}
}

func TestSnapshotReturnsACopy(t *testing.T) {
	r := NewRing(0, 0)
	r.Append(line("original"))
	lines, _, _ := r.Snapshot(0, 0)
	lines[0].Text = "mutated"
	again, _, _ := r.Snapshot(0, 0)
	if again[0].Text != "original" {
		t.Fatalf("Snapshot aliased the ring: %q", again[0].Text)
	}
}

func TestSupersedeRewritesInPlaceAndRepublishesTheSameSeq(t *testing.T) {
	r := NewRing(0, 0)
	seq := r.Append(Line{Source: SourceGame, Text: "downloading 1%", Partial: true})
	sub := r.Subscribe(1, 8)
	defer sub.Close()
	<-sub.C // the backlog batch

	if !r.Supersede(seq, Line{Source: SourceGame, Text: "downloading 100%"}) {
		t.Fatal("Supersede reported the line was gone")
	}
	if got := r.Head(); got != seq {
		t.Errorf("Head moved to %d; a supersede must not advance the cursor", got)
	}
	b := <-sub.C
	if len(b.Lines) != 1 || b.Lines[0].Seq != seq || b.Lines[0].Text != "downloading 100%" {
		t.Fatalf("republished %+v", b.Lines)
	}
	if b.Lines[0].Partial {
		t.Error("the completed line is still marked partial")
	}
	if r.Len() != 1 {
		t.Errorf("Len = %d, want 1", r.Len())
	}
}

func TestSupersedeRefusesAnEvictedOrUnknownSeq(t *testing.T) {
	r := NewRing(1<<20, 2)
	first := r.Append(line("one"))
	r.Append(line("two"))
	r.Append(line("three")) // evicts "one"

	if r.Supersede(first, line("rewritten")) {
		t.Error("superseded an evicted line")
	}
	if r.Supersede(99, line("rewritten")) {
		t.Error("superseded a Seq that was never assigned")
	}
	if r.Supersede(0, line("rewritten")) {
		t.Error("superseded Seq 0, which is never assigned")
	}
}

func TestSupersedeKeepsTheByteBudgetHonest(t *testing.T) {
	r := NewRing(1<<20, 100)
	seq := r.Append(line("short"))
	before := r.Bytes()
	r.Supersede(seq, line("a considerably longer line than before"))
	if got, want := r.Bytes(), before+len("a considerably longer line than before")-len("short"); got != want {
		t.Fatalf("Bytes = %d, want %d", got, want)
	}
}

func TestSubscribeDeliversTheBacklogAsOneBatch(t *testing.T) {
	r := NewRing(0, 0)
	for i := 1; i <= 200; i++ {
		r.Append(line(fmt.Sprintf("l%d", i)))
	}
	// Depth 1: 200 lines must still arrive, which they only can if the
	// backlog is one batch rather than 200.
	sub := r.Subscribe(0, 1)
	defer sub.Close()
	b := <-sub.C
	if len(b.Lines) != 200 || b.Dropped != 0 {
		t.Fatalf("backlog = %d lines, dropped %d", len(b.Lines), b.Dropped)
	}
	if b.Lines[0].Seq != 1 || b.Lines[199].Seq != 200 {
		t.Fatalf("backlog spans %d..%d", b.Lines[0].Seq, b.Lines[199].Seq)
	}

	from := r.Subscribe(150, 4)
	defer from.Close()
	b = <-from.C
	if b.Lines[0].Seq != 150 || len(b.Lines) != 51 {
		t.Fatalf("Subscribe(150) delivered %d lines from Seq %d", len(b.Lines), b.Lines[0].Seq)
	}
	oldest, head := from.Start()
	if oldest != 1 || head != 200 {
		t.Fatalf("Start = %d,%d want 1,200", oldest, head)
	}
}

// TestASubscriberThatNeverReadsCannotStallTheProducer is the invariant this
// whole package exists for: a browser that stopped reading must not be able to
// block the pump, and so must not be able to block the game's stdout.
func TestASubscriberThatNeverReadsCannotStallTheProducer(t *testing.T) {
	const lines = 50000
	r := NewRing(1<<20, 20000)

	stalled := r.Subscribe(0, 4) // never read from
	defer stalled.Close()

	healthy := r.Subscribe(0, DefaultSubDepth)
	defer healthy.Close()

	// The healthy reader drains inline after each append. That keeps the
	// assertion deterministic -- no scheduler race decides whether it kept up
	// -- while still proving the point: the wedged subscriber neither blocks
	// the producer nor costs the healthy one a single line.
	var (
		got     []uint64
		dropped uint64
	)
	drain := func() {
		for {
			select {
			case b, ok := <-healthy.C:
				if !ok {
					return
				}
				dropped += b.Dropped
				for _, l := range b.Lines {
					got = append(got, l.Seq)
				}
			default:
				return
			}
		}
	}

	start := time.Now()
	for i := 0; i < lines; i++ {
		r.Append(line("the quick brown fox jumps over the lazy dog"))
		drain()
	}
	elapsed := time.Since(start)

	// The budget is the assertion. 50 000 appends fanned out to two
	// subscribers, one of which is wedged, is microseconds of work; anything
	// approaching 200 ms means somebody made a send blocking.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("50000 appends took %v with one wedged subscriber; the producer blocked", elapsed)
	}

	if dropped != 0 {
		t.Errorf("the healthy subscriber lost %d lines because another one stalled", dropped)
	}
	if len(got) != lines {
		t.Fatalf("healthy subscriber saw %d of %d lines", len(got), lines)
	}
	for i, seq := range got {
		if seq != uint64(i+1) {
			t.Fatalf("healthy subscriber saw Seq %d at position %d", seq, i)
		}
	}

	// And the wedged one is exactly as far behind as it should be: full
	// channel, everything else counted as dropped.
	r.mu.Lock()
	drops := stalled.dropped
	r.mu.Unlock()
	if drops == 0 {
		t.Fatal("the wedged subscriber was never marked lagged")
	}
	if int(drops)+len(stalled.C) != lines {
		t.Errorf("wedged subscriber accounting: %d buffered + %d dropped != %d", len(stalled.C), drops, lines)
	}
}

func TestALaggedSubscriberIsToldHowMuchItLostAndWhereToResume(t *testing.T) {
	r := NewRing(0, 0)
	sub := r.Subscribe(0, 2)
	defer sub.Close()

	for i := 1; i <= 10; i++ {
		r.Append(line(fmt.Sprintf("l%d", i)))
	}
	// Two batches buffered, eight dropped.
	b1 := <-sub.C
	if b1.Dropped != 0 || b1.Lines[0].Seq != 1 {
		t.Fatalf("first batch: dropped %d, Seq %d", b1.Dropped, b1.Lines[0].Seq)
	}
	b2 := <-sub.C
	if b2.Dropped != 0 || b2.Lines[0].Seq != 2 {
		t.Fatalf("second batch: dropped %d, Seq %d", b2.Dropped, b2.Lines[0].Seq)
	}

	// Room again: the next line carries the drop count and its own Seq is the
	// resume_from the `lagged` event reports.
	r.Append(line("l11"))
	b3 := <-sub.C
	if b3.Dropped != 8 {
		t.Errorf("Dropped = %d, want 8", b3.Dropped)
	}
	if b3.Lines[0].Seq != 11 {
		t.Errorf("resume_from = %d, want 11", b3.Lines[0].Seq)
	}
}

func TestCloseUnblocksAReaderAndIsIdempotent(t *testing.T) {
	r := NewRing(0, 0)
	sub := r.Subscribe(0, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range sub.C {
		}
	}()
	r.Append(line("a"))
	sub.Close()
	sub.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not end the reader")
	}
	// A closed subscription must not keep the ring's fan-out list growing.
	if got := r.Subscribers(); got != 0 {
		t.Errorf("Subscribers = %d after Close", got)
	}
	r.Append(line("b")) // must not panic on a closed channel
}

func TestRingCloseEndsEverySubscriptionAndRefusesAppends(t *testing.T) {
	r := NewRing(0, 0)
	a := r.Subscribe(0, 4)
	b := r.Subscribe(0, 4)
	r.Close()
	r.Close()

	for _, s := range []*Subscription{a, b} {
		select {
		case _, ok := <-s.C:
			if ok {
				t.Fatal("channel delivered a batch after Ring.Close")
			}
		case <-time.After(time.Second):
			t.Fatal("Ring.Close did not close a subscription")
		}
	}
	if got := r.Append(line("late")); got != 0 {
		t.Errorf("Append after Close returned Seq %d", got)
	}
	// Subscribing to a closed ring returns an already-closed channel rather
	// than one that hangs forever.
	late := r.Subscribe(0, 4)
	if _, ok := <-late.C; ok {
		t.Error("Subscribe on a closed ring delivered a batch")
	}
	late.Close()
}

func TestWaitForRefusesAMatchThatPredatesTheSend(t *testing.T) {
	r := NewRing(0, 0)
	r.Append(line("SAVED")) // an hour ago
	since := r.Head() + 1   // captured before the send, per section 11.6 rule 1

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.WaitFor(ctx, since, regexp.MustCompile(`SAVED`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitFor accepted a stale match: err = %v", err)
	}
}

func TestWaitForMatchesALineThatArrivesLater(t *testing.T) {
	r := NewRing(0, 0)
	r.Append(line("SAVED"))
	since := r.Head() + 1

	go func() {
		time.Sleep(5 * time.Millisecond)
		r.Append(line("noise"))
		r.Append(line("world SAVED to disk"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := r.WaitFor(ctx, since, regexp.MustCompile(`SAVED`))
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if got.Text != "world SAVED to disk" || got.Seq != 3 {
		t.Fatalf("matched %+v", got)
	}
}

func TestWaitForSeesAMatchAlreadyInTheRing(t *testing.T) {
	r := NewRing(0, 0)
	r.Append(line("one"))
	r.Append(line("SERVER STARTED"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := r.WaitFor(ctx, 1, regexp.MustCompile(`SERVER STARTED`))
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if got.Seq != 2 {
		t.Fatalf("matched Seq %d", got.Seq)
	}
}

func TestWaitForRecoversAMatchItsOwnChannelDropped(t *testing.T) {
	// A marker must not be missed because the waiter's channel overflowed
	// while a modded server spewed 10 000 lines: the ring still holds them,
	// so WaitFor rescans rather than losing the shutdown ladder's rung.
	r := NewRing(1<<20, 20000)
	since := uint64(1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	found := make(chan Line, 1)
	errc := make(chan error, 1)
	release := make(chan struct{})
	go func() {
		<-release
		l, err := r.WaitFor(ctx, since, regexp.MustCompile(`^MARKER$`))
		if err != nil {
			errc <- err
			return
		}
		found <- l
	}()

	// Fill far past any subscriber depth before the waiter reads anything.
	sub := r.Subscribe(since, 4*DefaultSubDepth)
	for i := 0; i < 5000; i++ {
		r.Append(line("noise"))
	}
	r.Append(line("MARKER"))
	sub.Close()
	close(release)

	select {
	case l := <-found:
		if l.Text != "MARKER" {
			t.Fatalf("matched %q", l.Text)
		}
	case err := <-errc:
		t.Fatalf("WaitFor: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("WaitFor lost a marker its channel dropped")
	}
}

func TestWaitForNeedsAPattern(t *testing.T) {
	r := NewRing(0, 0)
	if _, err := r.WaitFor(context.Background(), 1, nil); err == nil {
		t.Fatal("WaitFor(nil) returned no error")
	}
}

func TestWaitForEndsWhenTheRingCloses(t *testing.T) {
	r := NewRing(0, 0)
	done := make(chan error, 1)
	go func() {
		_, err := r.WaitFor(context.Background(), 1, regexp.MustCompile(`never`))
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	r.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRingClosed) {
			t.Fatalf("err = %v, want ErrRingClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor did not return when the ring closed")
	}
}

func TestConcurrentProducersAssignUniqueContiguousSeqs(t *testing.T) {
	r := NewRing(1<<20, 20000)
	const producers, each = 8, 500

	var wg sync.WaitGroup
	seen := make([][]uint64, producers)
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				seen[p] = append(seen[p], r.Append(line("x")))
			}
		}(p)
	}
	// A reader in the mix, so the race detector sees the fan-out path too.
	sub := r.Subscribe(0, 8)
	go func() {
		for range sub.C {
		}
	}()
	wg.Wait()
	sub.Close()

	all := map[uint64]bool{}
	for _, s := range seen {
		for _, seq := range s {
			if all[seq] {
				t.Fatalf("Seq %d assigned twice", seq)
			}
			all[seq] = true
		}
	}
	if len(all) != producers*each {
		t.Fatalf("%d distinct Seqs for %d appends", len(all), producers*each)
	}
	if r.Head() != uint64(producers*each) {
		t.Fatalf("Head = %d", r.Head())
	}
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
