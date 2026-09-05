package stdinctl

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/control"
)

func newTestChannel(t *testing.T, ready *bool) (*Channel, *console.Fake) {
	t.Helper()
	f := console.NewFake(console.FakeOptions{Instance: "pz-main"})
	c, err := New(Options{
		Sink:   control.SenderFunc(f.Send),
		Gate:   func() bool { return *ready },
		Reason: "the server is stopped",
		Clock:  f.Clock.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, f
}

func TestNewRefusesASinklessChannel(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("built a console channel with nowhere to write")
	}
}

func TestSendEchoesWithTheActor(t *testing.T) {
	ready := true
	c, f := newTestChannel(t, &ready)

	seq, err := c.Send("user:alice@example.com", "save")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if seq == 0 {
		t.Fatal("no Seq; the echo is the record of who typed what (section 11.4)")
	}
	sent := f.Sent()
	if len(sent) != 1 || sent[0].Line != "save" || sent[0].Actor != "user:alice@example.com" {
		t.Fatalf("sent %+v", sent)
	}
}

// Exec is the shutdown ladder's and the hook runner's entry point, and neither
// is a person, so the line is attributed to the runner rather than to whoever
// happened to be logged in.
func TestExecAttributesToTheRunnerAndReturnsNoOutput(t *testing.T) {
	ready := true
	c, f := newTestChannel(t, &ready)

	out, err := c.Exec(context.Background(), "quit")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "" {
		t.Fatalf("Exec returned %q; the console never replies, and inventing one would attribute the game's next line to the command", out)
	}
	if got := f.Sent(); len(got) != 1 || got[0].Actor != DefaultActor {
		t.Fatalf("sent %+v", got)
	}
}

// The validation is here and not only at the socket, because a manifest's own
// `send:` string and a hook take this path without passing a socket at all.
func TestSendRefusesASecondCommand(t *testing.T) {
	ready := true
	c, f := newTestChannel(t, &ready)

	for _, line := range []string{"save\nquit", "save\r\nquit", "save\x00quit", ""} {
		if _, err := c.Send("root_cli", line); err == nil {
			t.Errorf("accepted %q", line)
		}
	}
	if got := f.Sent(); len(got) != 0 {
		t.Fatalf("something reached the game: %+v", got)
	}
}

func TestClosedGateRefusesWithTheReason(t *testing.T) {
	ready := false
	c, f := newTestChannel(t, &ready)

	if c.Ready() {
		t.Fatal("ready with a closed gate")
	}
	if c.Unready() != "the server is stopped" {
		t.Errorf("reason %q", c.Unready())
	}
	_, err := c.Send("root_cli", "save")
	if !errors.Is(err, control.ErrNotReady) {
		t.Fatalf("Send: %v, want ErrNotReady", err)
	}
	if !strings.Contains(err.Error(), "the server is stopped") {
		t.Errorf("error %q drops the reason", err)
	}
	if len(f.Sent()) != 0 {
		t.Error("wrote to a stopped server")
	}
}

// Close must not close the sink. Closing a game's stdin sends it EOF, which is
// how a FIFO-based supervisor kills a server by accident (section 11.1) -- the
// runner owns the pty and closes it when the process is gone.
func TestCloseDoesNotCloseTheSink(t *testing.T) {
	ready := true
	c, f := newTestChannel(t, &ready)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c.Ready() {
		t.Error("ready after Close")
	}
	if _, err := c.Send("root_cli", "save"); !errors.Is(err, control.ErrNotReady) {
		t.Fatalf("Send after Close: %v", err)
	}
	// The underlying console still works: only this view of it was closed.
	if _, err := f.Send("root_cli", "save"); err != nil {
		t.Errorf("the sink was closed underneath: %v", err)
	}
}

func TestSnapshotTracksHealth(t *testing.T) {
	ready := true
	f := console.NewFake(console.FakeOptions{Instance: "pz-main"})
	f.Clock.Set(time.Unix(1_700_000_000, 0).UTC())

	boom := errors.New("write /dev/ptmx: input/output error")
	fail := false
	c, err := New(Options{
		Sink: control.SenderFunc(func(actor, line string) (uint64, error) {
			if fail {
				return 0, boom
			}
			return f.Send(actor, line)
		}),
		Gate:  func() bool { return ready },
		Clock: f.Clock.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.Send("root_cli", "save"); err != nil {
		t.Fatal(err)
	}
	s := c.Snapshot()
	if !s.Ready || s.ConsecutiveFailures != 0 || s.LastError != "" || s.LastOK.IsZero() {
		t.Fatalf("after a good send: %+v", s)
	}

	fail = true
	for i := 0; i < 3; i++ {
		if _, err := c.Send("root_cli", "save"); err == nil {
			t.Fatal("the failing sink reported success")
		}
	}
	if s := c.Snapshot(); s.ConsecutiveFailures != 3 || !strings.Contains(s.LastError, "input/output error") {
		t.Fatalf("after three failures: %+v", s)
	}

	fail = false
	if _, err := c.Send("root_cli", "save"); err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.ConsecutiveFailures != 0 || s.LastError != "" {
		t.Fatalf("a success did not clear the failure count: %+v", s)
	}
}

// ---------- WriterSender: the standalone ladder of section 11.8 ----------

type recordingWriter struct {
	mu     sync.Mutex
	writes []string
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

// One Write per line, terminator included. Two writes can interleave with
// another writer on the same fd, and half a command followed by another
// command's tail is a command nobody typed.
func TestWriterSenderWritesOneCommandPerCall(t *testing.T) {
	w := &recordingWriter{}
	s := WriterSender(w, "")

	seq, err := s.Send("root_cli", "save")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if seq != 0 {
		t.Errorf("Seq %d; the standalone path has no ring, and 0 is how it says so", seq)
	}
	if _, err := s.Send("root_cli", "quit"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(w.writes) != 2 || w.writes[0] != "save\n" || w.writes[1] != "quit\n" {
		t.Fatalf("writes %q", w.writes)
	}
}

func TestWriterSenderHonoursTheLineEnding(t *testing.T) {
	w := &recordingWriter{}
	if _, err := WriterSender(w, "\r\n").Send("root_cli", "save"); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 || w.writes[0] != "save\r\n" {
		t.Fatalf("writes %q", w.writes)
	}
}

func TestWriterSenderValidates(t *testing.T) {
	w := &recordingWriter{}
	if _, err := WriterSender(w, "").Send("root_cli", "save\nquit"); err == nil {
		t.Fatal("accepted two commands in one line on the path with no socket in front of it")
	}
	if len(w.writes) != 0 {
		t.Fatalf("wrote %q anyway", w.writes)
	}
}

// The whole point of the type: it satisfies both halves of what a commander
// needs from a console.
var (
	_ control.Channel = (*Channel)(nil)
	_ control.Sender  = (*Channel)(nil)
)

// The terminator is appended verbatim after the line has been validated, and
// successive sends share one file offset, so an arbitrary terminator is
// arbitrary multi-line content written into whatever descriptor this holds. The
// standalone shutdown path reads it out of a file the instance user can write
// and then writes as root.
func TestWriterSenderRefusesATerminatorNoManifestCanExpress(t *testing.T) {
	var b strings.Builder
	s := WriterSender(&b, "\n[Unit]\nExecStart=/bin/sh\n")
	if _, err := s.Send("root_cli", "save"); err == nil {
		t.Fatal("an arbitrary line terminator was accepted")
	}
	if b.Len() != 0 {
		t.Errorf("bytes reached the descriptor anyway: %q", b.String())
	}
	for _, le := range []string{"", "\n", "\r", "\r\n"} {
		var ok strings.Builder
		if _, err := WriterSender(&ok, le).Send("root_cli", "save"); err != nil {
			t.Errorf("line ending %q was refused: %v", le, err)
		}
	}
}
