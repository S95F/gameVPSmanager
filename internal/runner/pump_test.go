package runner

import (
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/console"
)

// errHangup stands in for the platform's end-of-stream error. On Linux that is
// EIO from a pty master whose last slave has closed; the loop's behaviour is
// asserted here, on a machine that has neither.
var errHangup = errors.New("input/output error")

func hangup(err error) bool { return errors.Is(err, errHangup) || errors.Is(err, io.EOF) }

// scriptReader replays a fixed sequence of (bytes, error) results, which is
// the only way to reproduce the read that returns DATA AND A HANGUP TOGETHER.
type scriptReader struct {
	steps []readStep
	i     int
}

type readStep struct {
	data string
	err  error
}

func (r *scriptReader) Read(p []byte) (int, error) {
	if r.i >= len(r.steps) {
		return 0, io.EOF
	}
	// A step longer than p is delivered over several reads, and its error
	// arrives with the LAST chunk -- which is what a real fd does and what
	// makes the crash-tail case above meaningful.
	s := &r.steps[r.i]
	n := copy(p, s.data)
	s.data = s.data[n:]
	if s.data != "" {
		return n, nil
	}
	r.i++
	return n, s.err
}

func newTestIngest() *console.Fake {
	// PartialAfter negative switches off the idle timer: these tests are about
	// what the pump delivers, not about when a prompt is flushed.
	return console.NewFake(console.FakeOptions{PartialAfter: -1})
}

func TestPumpKeepsTheCrashTailThatArrivedWithTheHangup(t *testing.T) {
	// THE trap. The last read of a dying process usually returns data AND the
	// error in one call: `n > 0, err = EIO`. Examining err first throws away
	// the thirty lines that say why the server died -- which is exactly the
	// output section 11.6 rule 8 promises the operator.
	f := newTestIngest()
	src := &scriptReader{steps: []readStep{
		{data: "Loading world...\n"},
		{data: "java.lang.OutOfMemoryError\n\tat zombie.Lua\n", err: errHangup},
	}}

	if err := pumpWith(src, f.Ingest, hangup); err != nil {
		t.Fatalf("pump returned %v; a hangup is a normal end of stream", err)
	}
	want := []string{"Loading world...", "java.lang.OutOfMemoryError", "\tat zombie.Lua"}
	if got := f.Texts(); !reflect.DeepEqual(got, want) {
		t.Errorf("console = %q, want %q", got, want)
	}
}

func TestPumpFlushesTheLastLineWithNoNewline(t *testing.T) {
	// A crash message is very often a line with no terminator: the process
	// died before it wrote one. Without the flush it never reaches the ring at
	// all.
	f := newTestIngest()
	src := &scriptReader{steps: []readStep{{data: "Segmentation fault", err: errHangup}}}

	if err := pumpWith(src, f.Ingest, hangup); err != nil {
		t.Fatal(err)
	}
	if got := f.Texts(); !reflect.DeepEqual(got, []string{"Segmentation fault"}) {
		t.Errorf("console = %q", got)
	}
}

func TestPumpReportsAnErrorThatIsNotAHangup(t *testing.T) {
	// The counterpart: a real read failure must not be laundered into a clean
	// end of stream, or a runner whose console broke would look like a game
	// that exited normally.
	f := newTestIngest()
	boom := errors.New("device not configured")
	src := &scriptReader{steps: []readStep{{data: "still here\n", err: boom}}}

	err := pumpWith(src, f.Ingest, hangup)
	if !errors.Is(err, boom) {
		t.Fatalf("pump = %v, want %v", err, boom)
	}
	// The bytes still made it: they were written before the error was judged.
	if got := f.Texts(); !reflect.DeepEqual(got, []string{"still here"}) {
		t.Errorf("console = %q", got)
	}
}

func TestPumpDrainsAPipeToEOF(t *testing.T) {
	// The whole loop against a real file descriptor, using the platform's own
	// end-of-stream predicate. On Linux this is the pipe-mode console; the pty
	// hangup is asserted in pty_linux_test.go where a pty exists.
	f := newTestIngest()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	go func() {
		defer w.Close()
		for _, line := range []string{"one\n", "two\r\n", "three"} {
			if _, err := io.WriteString(w, line); err != nil {
				return
			}
		}
	}()

	if err := pump(r, f.Ingest); err != nil {
		t.Fatalf("pump: %v", err)
	}
	want := []string{"one", "two", "three"}
	if got := f.Texts(); !reflect.DeepEqual(got, want) {
		t.Errorf("console = %q, want %q", got, want)
	}
}

func TestEndOfStreamAcceptsTheRunnersOwnClose(t *testing.T) {
	// Close racing a blocked read is the runner tidying up, not the game
	// misbehaving, and it must not be reported as a console failure.
	if !endOfStream(io.EOF) {
		t.Error("io.EOF is not an end of stream")
	}
	if !endOfStream(os.ErrClosed) {
		t.Error("os.ErrClosed is not an end of stream")
	}
	if !endOfStream(&os.PathError{Op: "read", Path: "/dev/ptmx", Err: os.ErrClosed}) {
		t.Error("a wrapped os.ErrClosed is not an end of stream")
	}
	if endOfStream(errors.New("permission denied")) {
		t.Error("an arbitrary error was accepted as an end of stream")
	}
}

func TestPumpSplitsALineLongerThanTheBuffer(t *testing.T) {
	// The pump's buffer is 32 KiB and the ingest force-splits at 64 KiB, so a
	// mod dumping a huge line crosses both boundaries. The bytes must survive
	// reassembly across reads.
	f := newTestIngest()
	long := strings.Repeat("x", 100*1024)
	src := &scriptReader{steps: []readStep{{data: long + "\n", err: errHangup}}}

	if err := pumpWith(src, f.Ingest, hangup); err != nil {
		t.Fatal(err)
	}
	var total int
	for _, l := range f.Texts() {
		total += len(l)
	}
	if total != len(long) {
		t.Errorf("reassembled %d bytes of a %d byte line", total, len(long))
	}
}
