package socket

import (
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
)

// TestTheClientRefusesItsOwnBadFramesLocally. A send with a terminator in it
// is a client bug; failing here names the real reason instead of spending a
// round trip to be told `bad_request`.
func TestTheClientRefusesItsOwnBadFramesLocally(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	cli := NewClient(a)

	if err := cli.Request(console.Request{Op: console.OpSend, Line: "say hi\nquit"}); !errors.Is(err, console.ErrLineBreak) {
		t.Fatalf("a two-command send returned %v", err)
	}
	if err := cli.Request(console.Request{Op: console.OpSend, Line: ""}); !errors.Is(err, console.ErrLineEmpty) {
		t.Fatalf("an empty send returned %v", err)
	}
	if err := cli.Request(console.Request{Op: console.OpVerb}); !errors.Is(err, console.ErrNoVerb) {
		t.Fatalf("a verb with no verb returned %v", err)
	}
	// Nothing reached the wire: net.Pipe is synchronous, so an attempted write
	// with no reader would have blocked until the deadline.
	if _, err := cli.Send("root_cli", "\x00", nil); err == nil {
		t.Fatal("a NUL in a send was accepted")
	}
}

// TestAProtocolMismatchFailsWithASentence. There is one client and one server
// and they ship in the same binary, so a mismatch means somebody is running a
// half-upgraded box -- which deserves a sentence rather than a decode error
// three frames later.
func TestAProtocolMismatchFailsWithASentence(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	cli := NewClient(a)

	go func() {
		srv := NewConn(b, time.Second, -1)
		if _, err := srv.ReadRequest(); err != nil {
			return
		}
		ev := console.HelloEvent("pz-main", "deadbee", console.PeerInfo{UID: 0, Write: true})
		ev.Proto = console.Proto + 98
		_ = srv.WriteEvent(ev)
	}()

	_, err := cli.Hello("gvm-test/0.1.0")
	if err == nil {
		t.Fatal("a protocol mismatch was accepted")
	}
	if !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("mismatch error was %q", err)
	}
}

// TestDoHandsOtherEventsToTheHandler. A `tail follow:true` upgrades the
// connection to a stream and a client may still interleave ops on it, so a
// request/response call has to keep the lines it is not waiting for rather
// than mistaking one for its answer.
func TestDoHandsOtherEventsToTheHandler(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	cli := NewClient(a)

	// A scripted peer rather than the real server, because the point is the
	// client's routing and the order has to be the test's to choose.
	go func() {
		srv := NewConn(b, time.Second, -1)
		req, err := srv.ReadRequest()
		if err != nil || req.Op != console.OpStatus {
			return
		}
		_ = srv.WriteEvent(console.LinesEvent(2, 1, []console.Line{{Seq: 2, Text: "SERVER STARTED"}}))
		_ = srv.WriteEvent(console.ShutdownStepEvent("console", "save"))
		_ = srv.WriteEvent(console.StatusEvent([]byte(`{"state":"ready"}`)))
	}()

	var streamed []string
	var others int
	raw, err := cli.Status(func(ev console.Event) {
		if ev.Ev == console.EvLines {
			streamed = append(streamed, texts(ev.Lines)...)
			return
		}
		others++
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if string(raw) != `{"state":"ready"}` {
		t.Fatalf("status returned %s", raw)
	}
	if strings.Join(streamed, ",") != "SERVER STARTED" {
		t.Fatalf("the handler collected %v", streamed)
	}
	if others != 1 {
		t.Fatalf("the handler saw %d other events, want the shutdown step", others)
	}
}

// TestDeniedIsDistinguishableFromABrokenSocket, because a dashboard renders
// one as a disabled button and the other as a retry.
func TestDeniedIsDistinguishableFromABrokenSocket(t *testing.T) {
	denied := &ProtocolError{Code: console.CodeDenied, Msg: "op \"stop\" is uid 0 only"}
	if !Denied(denied) {
		t.Error("a denied error was not recognised")
	}
	if Denied(&ProtocolError{Code: console.CodeInternal, Msg: "rcon: auth failed"}) {
		t.Error("an internal error was read as a denial")
	}
	if Denied(errors.New("connection refused")) || Denied(nil) {
		t.Error("a transport error was read as a denial")
	}
	if got := denied.Error(); !strings.HasPrefix(got, console.CodeDenied+": ") {
		t.Errorf("error string is %q", got)
	}
}

// TestStopReportsEveryRungThenTheReport, from the client's side of the wire.
func TestStopReportsEveryRungThenTheReport(t *testing.T) {
	h := newHarness(t, fixed(idRoot))
	h.be.mu.Lock()
	h.be.steps = [][2]string{{"console", "save"}, {"control", "quit"}, {"wait_exit", ""}}
	h.be.mu.Unlock()

	cli := h.connect()
	mustHello(t, cli)

	var rungs []string
	report, err := cli.Stop("root_cli", "unit stopping", func(rung, send string) {
		rungs = append(rungs, rung)
	}, nil)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if strings.Join(rungs, ",") != "console,control,wait_exit" {
		t.Fatalf("rungs were %v", rungs)
	}
	if !strings.Contains(string(report), `"instance":"pz-main"`) {
		t.Fatalf("report was %s", report)
	}
}

// TestDialNamesThePathItCouldNotReach. `gvm console` against a stopped
// instance is the common case, and "no such file" plus the path is the whole
// diagnosis.
func TestDialNamesThePathItCouldNotReach(t *testing.T) {
	ctx := t.Context()
	path := t.TempDir() + "/console.sock"
	_, err := Dial(ctx, path)
	if err == nil {
		t.Fatal("dialling a socket that does not exist succeeded")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("dial error does not name the path: %v", err)
	}
}

// TestASilentRunnerDoesNotHangACallForever.
//
// A client call used to have no read deadline at all. NewClient is
// NewConn(rw, 0, -1), and only `handshake == 0` takes the default -- a
// negative value switches it off -- so every branch of armRead is gated on
// `c.handshake > 0` and nothing ever armed one. That is deliberate for a
// `tail follow:true` reader, which is legitimately silent for hours and reads
// with Next directly. It is wrong for a request/response call.
//
// The precondition is reachable from the other end: Server.dispatch runs
// backend.Send synchronously on the connection's own goroutine, and
// Runner.Send does io.WriteString to the pty master under sendMu with no
// deadline. A blocked pty write wedges dispatch, so no `ev:sent` is ever
// written and the caller blocks forever -- which for a backup's freeze rung
// means a job holding the instance lock and lease.json until
// TimeoutStartSec=14400 fires. internal/jobs states that discipline in its own
// words twice ("an infinite wait inside a deferred function is a job that
// never exits"; "not 'forever'"), and this adapter is where it was dropped.
//
// The write side is bounded already (DefaultWriteTimeout through
// deadlineWriter), so a small `send` frame goes out fine and the hang is
// purely on the read.
func TestASilentRunnerDoesNotHangACallForever(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	cli := NewClient(a)
	cli.SetCallTimeout(80 * time.Millisecond)

	// A peer that accepts the frame and then says nothing, which is what a
	// runner with a wedged pty write looks like.
	read := make(chan struct{})
	go func() {
		srv := NewConn(b, time.Second, -1)
		if _, err := srv.ReadRequest(); err == nil {
			close(read)
		}
		<-make(chan struct{}) // never answer
	}()

	done := make(chan error, 1)
	go func() { _, err := cli.Send("gvm-backup", "save-on", nil); done <- err }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a call that was never answered reported success")
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("err = %v, want a read deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call never returned; a wedged runner holds the instance lock until " +
			"TimeoutStartSec")
	}
	<-read

	// And the connection is latched broken rather than left to be reused. A
	// read whose deadline fired may have stopped mid-frame and nothing can
	// resynchronise from that, so a second call -- the freeze's thaw on the
	// same client -- has to fail fast instead of putting a second goroutine
	// into ReadEvent, which Conn's own doc forbids.
	if _, err := cli.Send("gvm-backup", "save-on", nil); err == nil {
		t.Error("a second call on a timed-out connection was accepted")
	} else if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the second call returned %v, want the latched error", err)
	}
	if cli.Conn().Err() == nil {
		t.Error("the connection does not record why it is unusable")
	}
}

// TestAFollowingClientIsStillAllowedToBeSilent, which is why the bound belongs
// on Do and not on the connection.
//
// `gvm console` on a sleeping server reads with Next for as long as the
// operator leaves the window open. A connection-wide idle deadline would close
// it, which is why NewClient switches the handshake deadline off in the first
// place.
func TestAFollowingClientIsStillAllowedToBeSilent(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	cli := NewClient(a)
	cli.SetCallTimeout(30 * time.Millisecond)

	go func() {
		srv := NewConn(b, time.Second, -1)
		if _, err := srv.ReadRequest(); err != nil {
			return
		}
		// Quiet for longer than a call is allowed to take, then a line.
		time.Sleep(120 * time.Millisecond)
		_ = srv.WriteEvent(console.LinesEvent(7, 1, []console.Line{{Seq: 7, Text: "ZOMBIES"}}))
	}()

	if err := cli.Tail(0, 10, true); err != nil {
		t.Fatal(err)
	}
	ev, err := cli.Next()
	if err != nil {
		t.Fatalf("a following read was cut off after a quiet stretch: %v", err)
	}
	if len(ev.Lines) != 1 || ev.Lines[0].Text != "ZOMBIES" {
		t.Fatalf("event = %+v", ev)
	}
}
