package socket

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
)

// TestAStalledPeerLosesItsConnectionNotTheRunner. The ring's drop policy keeps
// a slow subscriber from stalling the producer; nothing but this deadline
// keeps a peer that stopped reading its socket from parking the fan-out
// goroutine in write(2) for the life of the runner.
func TestAStalledPeerLosesItsConnectionNotTheRunner(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewConn(a, 50*time.Millisecond, -1)

	start := time.Now()
	err := c.WriteEvent(console.PongEvent())
	if err == nil {
		t.Fatal("a write to a peer that never reads succeeded")
	}
	if !Timeout(err) {
		t.Fatalf("write failed with %v, want a deadline", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the write blocked for %s", d)
	}
	if c.Err() == nil {
		t.Error("the connection was not marked broken")
	}
	// A half-written frame cannot be resynchronised from, so the connection is
	// dropped rather than reused.
	if err := c.WriteEvent(console.PongEvent()); err == nil || Timeout(err) {
		t.Errorf("the connection was still writable after a timeout: %v", err)
	}
}

// TestAFrameRefusedForSizeLeavesTheConnectionUsable: the encoder checks the
// size before it writes a byte, so an oversize frame is a refusal, not a
// desync.
func TestAFrameRefusedForSizeLeavesTheConnectionUsable(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewConn(a, time.Second, -1)
	c.SetMaxEventFrame(200)

	read := readEvents(t, b)
	huge := console.LinesEvent(1, 1, []console.Line{{Seq: 1, Text: strings.Repeat("x", 400)}})
	if err := c.WriteEvent(huge); !errors.Is(err, console.ErrFrameTooLarge) {
		t.Fatalf("an oversize frame returned %v", err)
	}
	if c.Err() != nil {
		t.Fatalf("an oversize frame broke the connection: %v", c.Err())
	}
	if err := c.WriteEvent(console.PongEvent()); err != nil {
		t.Fatalf("the connection was unusable afterwards: %v", err)
	}
	if ev := <-read; ev.Ev != console.EvPong {
		t.Fatalf("the peer received %+v", ev)
	}
}

// TestAnOversizeBatchIsHalvedRatherThanLost. SplitBatch estimates a line's
// encoded cost; JSON escaping beats the estimate when a game prints control
// bytes, and the encoder refuses rather than truncating. Console output an
// operator never sees is the worst outcome available, so the chunk is halved
// and retried.
func TestAnOversizeBatchIsHalvedRatherThanLost(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewConn(a, 2*time.Second, -1)
	c.SetMaxEventFrame(1000)

	// \x01 costs one byte in the ring and six in JSON: exactly the case
	// SplitBatch's fixed per-line overhead cannot cover.
	lines := make([]console.Line, 6)
	for i := range lines {
		lines[i] = console.Line{Seq: uint64(i + 1), Text: strings.Repeat("\x01", 100)}
	}
	read := readEvents(t, b)
	done := make(chan error, 1)
	go func() { done <- c.WriteLines(6, 1, lines) }()

	var got []uint64
	var frames int
	for len(got) < len(lines) {
		ev := <-read
		if ev.Ev != console.EvLines {
			t.Fatalf("received %+v", ev)
		}
		frames++
		for _, l := range ev.Lines {
			got = append(got, l.Seq)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("WriteLines: %v", err)
	}
	if frames < 2 {
		t.Fatalf("six oversize lines went out in %d frame(s); the halving never ran", frames)
	}
	for i, seq := range got {
		if seq != uint64(i+1) {
			t.Fatalf("lines arrived out of order: %v", got)
		}
	}
}

// TestASingleUnsplittableLineIsReportedNotSilentlyDropped. A line can always be
// halved down to one; if that one still does not fit, the caller has to hear
// about it.
func TestASingleUnsplittableLineIsReportedNotSilentlyDropped(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewConn(a, time.Second, -1)
	c.SetMaxEventFrame(120)
	_ = readEvents(t, b)

	err := c.WriteLines(1, 1, []console.Line{{Seq: 1, Text: strings.Repeat("y", 500)}})
	if !errors.Is(err, console.ErrFrameTooLarge) {
		t.Fatalf("WriteLines returned %v, want ErrFrameTooLarge", err)
	}
}

// TestTheHandshakeDeadlineCoversTheFirstFrameOnly. A follower is legitimately
// silent for hours; a peer that connects and says nothing at all is holding a
// connection slot.
func TestTheHandshakeDeadlineCoversTheFirstFrameOnly(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewConn(a, time.Second, 40*time.Millisecond)

	if _, err := c.ReadRequest(); !Timeout(err) {
		t.Fatalf("a silent peer's first frame returned %v, want a deadline", err)
	}

	// A connection that did speak first is not on a clock afterwards.
	d, e := net.Pipe()
	defer e.Close()
	c2 := NewConn(d, time.Second, 40*time.Millisecond)
	peer := NewConn(e, time.Second, -1)
	go func() {
		_ = peer.WriteRequest(console.Request{Op: console.OpHello, Client: "gvm-test"})
		time.Sleep(120 * time.Millisecond)
		_ = peer.WriteRequest(console.Request{Op: console.OpPing})
	}()
	if _, err := c2.ReadRequest(); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	req, err := c2.ReadRequest()
	if err != nil {
		t.Fatalf("a quiet client was disconnected after its handshake: %v", err)
	}
	if req.Op != console.OpPing {
		t.Fatalf("second frame was %+v", req)
	}
}

// TestAnOversizeRequestIsRefusedBeforeItIsBuffered. D-09 puts the web tier
// directly on this socket, so a peer must not be able to make the runner
// allocate by sending a gigabyte with no newline in it.
func TestAnOversizeRequestIsRefusedBeforeItIsBuffered(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	c := NewConn(a, time.Second, -1)

	go func() {
		// No newline: the frame never ends.
		buf := strings.Repeat("x", 8<<10)
		for i := 0; i < 16; i++ {
			if _, err := b.Write([]byte(buf)); err != nil {
				return
			}
		}
		b.Close()
	}()
	if _, err := c.ReadRequest(); !errors.Is(err, console.ErrFrameTooLarge) {
		t.Fatalf("an unterminated 128 KiB frame returned %v", err)
	}
}

// TestGoneRecognisesAnOrdinaryDeparture. A console client detaching and a
// runner exiting mid-stream are both ordinary; logging them as errors buries
// the ones that are not.
func TestGoneRecognisesAnOrdinaryDeparture(t *testing.T) {
	gone := []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		net.ErrClosed,
		io.ErrClosedPipe,
		&net.OpError{Op: "read", Err: io.EOF},
	}
	for _, err := range gone {
		if !Gone(err) {
			t.Errorf("Gone(%v) = false", err)
		}
	}
	notGone := []error{nil, console.ErrFrameTooLarge, errors.New("bad json")}
	for _, err := range notGone {
		if Gone(err) {
			t.Errorf("Gone(%v) = true", err)
		}
	}
	if Timeout(io.EOF) {
		t.Error("Timeout(io.EOF) = true")
	}
}

// TestConcurrentWritersProduceParseableFrames. The runner answers ops on one
// goroutine and streams lines on another; two interleaved encodes would
// produce a frame neither side can parse.
func TestConcurrentWritersProduceParseableFrames(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewConn(a, 5*time.Second, -1)
	peer := NewConn(b, 5*time.Second, -1)

	const each = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < each; i++ {
			if err := c.WriteEvent(console.PongEvent()); err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < each; i++ {
			line := console.Line{Seq: uint64(i + 1), Text: strings.Repeat("z", 200)}
			if err := c.WriteLines(uint64(i+1), 1, []console.Line{line}); err != nil {
				return
			}
		}
	}()

	pongs, lines := 0, 0
	for pongs+lines < 2*each {
		ev, err := peer.ReadEvent()
		if err != nil {
			t.Fatalf("after %d pongs and %d line frames: %v", pongs, lines, err)
		}
		switch ev.Ev {
		case console.EvPong:
			pongs++
		case console.EvLines:
			lines++
		default:
			t.Fatalf("unexpected frame %+v", ev)
		}
	}
	wg.Wait()
	if pongs != each || lines != each {
		t.Fatalf("received %d pongs and %d line frames, want %d each", pongs, lines, each)
	}
}

// readEvents drains a connection into a channel, so a test can write without
// deadlocking on net.Pipe's synchronous transport.
func readEvents(t *testing.T, rw io.ReadWriteCloser) <-chan console.Event {
	t.Helper()
	ch := make(chan console.Event, 64)
	dec := console.NewDecoder(rw)
	go func() {
		defer close(ch)
		for {
			ev, err := dec.Event()
			if err != nil {
				return
			}
			ch <- ev
		}
	}()
	return ch
}
