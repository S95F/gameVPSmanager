package socket

import (
	"errors"
	"net"
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
