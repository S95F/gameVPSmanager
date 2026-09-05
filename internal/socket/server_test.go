package socket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// fakeBackend is a runner made of console.Fake plus recorded lifecycle calls.
// Emit goes through the real ingest and the real ring, so these tests exercise
// the splitter and the fan-out that ship rather than kinder copies of them.
type fakeBackend struct {
	fake  *console.Fake
	build string

	mu        sync.Mutex
	status    json.RawMessage
	caps      json.RawMessage
	verbs     []verbCall
	stops     []stopCall
	verbErr   error
	statusErr error
	sendErr   error
	stopErr   error
	panicOn   console.Op
	steps     [][2]string
	stopGate  chan struct{}
}

type verbCall struct {
	actor, verb string
	args        map[string]string
}

type stopCall struct{ actor, reason string }

func newFakeBackend(instance string) *fakeBackend {
	return &fakeBackend{
		fake:   console.NewFake(console.FakeOptions{Instance: instance, PartialAfter: -1}),
		build:  "a1b2c3d",
		status: json.RawMessage(`{"state":"ready"}`),
		caps:   json.RawMessage(`[{"verb":"players","supported":true}]`),
		steps:  [][2]string{{"console", "save"}, {"console", "quit"}},
	}
}

func (b *fakeBackend) Instance() string        { return b.fake.Instance }
func (b *fakeBackend) Build() string           { return b.build }
func (b *fakeBackend) Ring() *console.Ring     { return b.fake.Ring }
func (b *fakeBackend) Close() error            { return b.fake.Close() }
func (b *fakeBackend) Emit(text string) uint64 { return b.fake.Emit(text) }

func (b *fakeBackend) maybePanic(op console.Op) {
	b.mu.Lock()
	p := b.panicOn
	b.mu.Unlock()
	if p == op {
		panic("fakeBackend: " + string(op))
	}
}

func (b *fakeBackend) Send(actor, line string) (uint64, error) {
	b.maybePanic(console.OpSend)
	b.mu.Lock()
	err := b.sendErr
	b.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return b.fake.Send(actor, line)
}

func (b *fakeBackend) Status() (json.RawMessage, error) {
	b.maybePanic(console.OpStatus)
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status, b.statusErr
}

func (b *fakeBackend) Caps() (json.RawMessage, error) {
	b.maybePanic(console.OpCaps)
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.caps, nil
}

func (b *fakeBackend) Verb(ctx context.Context, actor, verb string, args map[string]string) (VerbResult, error) {
	b.maybePanic(console.OpVerb)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.verbs = append(b.verbs, verbCall{actor: actor, verb: verb, args: args})
	if b.verbErr != nil {
		return VerbResult{}, b.verbErr
	}
	return VerbResult{Channel: "control", Output: "Message sent."}, nil
}

func (b *fakeBackend) Stop(ctx context.Context, actor, reason string, step func(rung, send string)) (json.RawMessage, error) {
	b.mu.Lock()
	b.stops = append(b.stops, stopCall{actor: actor, reason: reason})
	steps, err, gate := b.steps, b.stopErr, b.stopGate
	b.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	for _, s := range steps {
		step(s[0], s[1])
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(fmt.Sprintf(`{"instance":%q,"actor":%q,"reason":%q,"final_rung":"console","degraded":false}`,
		b.fake.Instance, actor, reason)), nil
}

func (b *fakeBackend) sentActors() []string {
	out := []string{}
	for _, s := range b.fake.Sent() {
		out = append(out, s.Actor)
	}
	return out
}

// harness is one server with one or more piped client connections.
type harness struct {
	t   *testing.T
	be  *fakeBackend
	srv *Server
	ctx context.Context

	mu    sync.Mutex
	conns []chan struct{}
}

func newHarness(t *testing.T, id Identifier, opts ...Option) *harness {
	t.Helper()
	be := newFakeBackend("pz-main")
	// A write deadline short enough that a stalled test finishes, long enough
	// that a scheduling hiccup is not a failure.
	opts = append([]Option{WithTimeouts(2*time.Second, -1)}, opts...)
	srv, err := New(be, testPolicy(), id, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, be: be, srv: srv, ctx: ctx}
	t.Cleanup(func() {
		cancel()
		srv.Close()
		h.mu.Lock()
		done := append([]chan struct{}(nil), h.conns...)
		h.mu.Unlock()
		for _, d := range done {
			select {
			case <-d:
			case <-time.After(5 * time.Second):
				t.Error("a connection goroutine did not finish")
			}
		}
		be.Close()
	})
	return h
}

// connect attaches one more client to the same server.
func (h *harness) connect() *Client {
	h.t.Helper()
	cliEnd, srvEnd := net.Pipe()
	done := make(chan struct{})
	h.mu.Lock()
	h.conns = append(h.conns, done)
	h.mu.Unlock()
	go func() {
		defer close(done)
		h.srv.serveConn(h.ctx, srvEnd)
	}()
	c := NewClient(cliEnd)
	h.t.Cleanup(func() { c.Close() })
	return c
}

func fixed(id Identity) Identifier {
	return func(net.Conn) (Identity, error) { return id, nil }
}

func mustHello(t *testing.T, c *Client) console.PeerInfo {
	t.Helper()
	pi, err := c.Hello("gvm-test/0.1.0")
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	return pi
}

// settle waits for the server to finish the op it was last sent.
//
// A `tail follow:true` with no backlog is answered by no frame at all, so a
// test that emits straight afterwards races the subscribe. Ops are handled in
// order on one goroutine, so a ping that comes back proves the subscription
// exists. Nothing may be streaming when this is called: the pong is read with
// intervening events discarded.
func settle(t *testing.T, c *Client) {
	t.Helper()
	if err := c.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func nextEvent(t *testing.T, c *Client) console.Event {
	t.Helper()
	ev, err := c.Next()
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	return ev
}

// TestHelloReportsTheTierItWillEnforce.
func TestHelloReportsTheTierItWillEnforce(t *testing.T) {
	cases := []struct {
		name                string
		id                  Identity
		write, verb, stopOK bool
	}{
		{"root", idRoot, true, true, true},
		{"console group", idWeb, true, false, false},
		{"instance user", idOwner, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, fixed(c.id))
			cli := h.connect()
			pi := mustHello(t, cli)
			if pi.Write != c.write || pi.Verb != c.verb || pi.Stop != c.stopOK {
				t.Errorf("peer block %+v, want write=%v verb=%v stop=%v", pi, c.write, c.verb, c.stopOK)
			}
			if cli.Instance() != "pz-main" || cli.Build() != "a1b2c3d" {
				t.Errorf("hello named %q/%q", cli.Instance(), cli.Build())
			}
			if pi.UID != c.id.Peer.UID {
				t.Errorf("hello reported uid %d, want %d", pi.UID, c.id.Peer.UID)
			}
		})
	}
}

// TestACredentialFailureRefusesTheConnection is the rule this whole package
// turns on: a connection whose peer credentials cannot be read is refused, not
// served at the least privileged tier.
func TestACredentialFailureRefusesTheConnection(t *testing.T) {
	boom := errors.New("getsockopt: connection reset by peer")
	h := newHarness(t, func(net.Conn) (Identity, error) { return Identity{}, boom })
	cli := h.connect()

	ev := nextEvent(t, cli)
	if ev.Ev != console.EvError || ev.Code != console.CodeDenied {
		t.Fatalf("refusal was %+v, want an error/denied event", ev)
	}
	if _, err := cli.Next(); !Gone(err) {
		t.Fatalf("the connection stayed open after a credential failure: %v", err)
	}
	// The refusal must not have reached the backend at all.
	if got := h.be.Ring().Head(); got != 0 {
		t.Errorf("a refused connection touched the ring (head %d)", got)
	}
}

// TestAStrangerCannotEvenSayHello. The runtime directory is 2750 so this
// should be unreachable; refusing it anyway is what makes the mode a second
// lock rather than the only one.
func TestAStrangerCannotEvenSayHello(t *testing.T) {
	h := newHarness(t, fixed(idOther))
	cli := h.connect()

	ev := nextEvent(t, cli)
	if ev.Ev != console.EvError || ev.Code != console.CodeDenied {
		t.Fatalf("refusal was %+v", ev)
	}
	if !strings.Contains(ev.Error, "1000") {
		t.Errorf("the refusal does not name the uid: %q", ev.Error)
	}
	if _, err := cli.Next(); !Gone(err) {
		t.Fatalf("a stranger's connection stayed open: %v", err)
	}
}

// TestTheDataPlaneIsOpenToTheConsoleGroup is D-09's whole point: gvm-web
// reaches tail/send/status/caps directly, with no gvmd in the path.
func TestTheDataPlaneIsOpenToTheConsoleGroup(t *testing.T) {
	h := newHarness(t, fixed(idWeb))
	cli := h.connect()
	mustHello(t, cli)
	h.be.Emit("SERVER STARTED")

	if err := cli.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := cli.Tail(0, 10, false); err != nil {
		t.Fatalf("tail: %v", err)
	}
	ev := nextEvent(t, cli)
	if ev.Ev != console.EvLines || len(ev.Lines) != 1 || ev.Lines[0].Text != "SERVER STARTED" {
		t.Fatalf("tail returned %+v", ev)
	}
	seq, err := cli.Send("user:alice@example.com", "players", nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if seq == 0 {
		t.Error("send returned seq 0")
	}
	if raw, err := cli.Status(nil); err != nil || string(raw) != `{"state":"ready"}` {
		t.Fatalf("status: %s, %v", raw, err)
	}
	if raw, err := cli.Caps(nil); err != nil || len(raw) == 0 {
		t.Fatalf("caps: %s, %v", raw, err)
	}
}

// TestLifecycleOpsAreRefusedWithoutClosingTheConnection. A denial is an answer,
// not a disconnect: gvm-web asks, is told to use gvmd, and keeps streaming the
// console it already had.
func TestLifecycleOpsAreRefusedWithoutClosingTheConnection(t *testing.T) {
	for _, id := range []Identity{idWeb, idOwner} {
		h := newHarness(t, fixed(id))
		cli := h.connect()
		mustHello(t, cli)

		if _, err := cli.Verb("user:alice@example.com", "broadcast", map[string]string{"Msg": "hi"}, nil); !Denied(err) {
			t.Fatalf("uid %d was allowed a verb: %v", id.Peer.UID, err)
		}
		if _, err := cli.Stop("user:alice@example.com", "operator requested", nil, nil); !Denied(err) {
			t.Fatalf("uid %d was allowed a stop: %v", id.Peer.UID, err)
		}
		if err := cli.Ping(); err != nil {
			t.Fatalf("the connection did not survive the refusals: %v", err)
		}
		h.be.mu.Lock()
		nv, ns := len(h.be.verbs), len(h.be.stops)
		h.be.mu.Unlock()
		if nv != 0 || ns != 0 {
			t.Fatalf("a refused op reached the backend: %d verbs, %d stops", nv, ns)
		}
	}
}

// TestRootDrivesTheLadderAndSeesEveryRung.
func TestRootDrivesTheLadderAndSeesEveryRung(t *testing.T) {
	h := newHarness(t, fixed(idRoot))
	cli := h.connect()
	mustHello(t, cli)

	res, err := cli.Verb("user:alice@example.com", "broadcast", map[string]string{"Msg": "restart in 5 minutes"}, nil)
	if err != nil {
		t.Fatalf("verb: %v", err)
	}
	if res.Channel != "control" || res.Output != "Message sent." {
		t.Fatalf("verb result %+v", res)
	}

	var rungs []string
	report, err := cli.Stop("user:alice@example.com", "operator requested",
		func(rung, send string) { rungs = append(rungs, rung+":"+send) }, nil)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if strings.Join(rungs, " ") != "console:save console:quit" {
		t.Fatalf("ladder narrated %v", rungs)
	}
	if !strings.Contains(string(report), `"final_rung":"console"`) {
		t.Fatalf("report was %s", report)
	}
	h.be.mu.Lock()
	stops := append([]stopCall(nil), h.be.stops...)
	h.be.mu.Unlock()
	if len(stops) != 1 || stops[0].actor != "user:alice@example.com" || stops[0].reason != "operator requested" {
		t.Fatalf("backend saw %+v", stops)
	}
}

// TestASecondShutdownIsRefusedWhileOneRuns. Two ladders on one world is the
// failure this package can actually cause, so the socket refuses rather than
// letting the runner sort it out.
func TestASecondShutdownIsRefusedWhileOneRuns(t *testing.T) {
	h := newHarness(t, fixed(idRoot))
	gate := make(chan struct{})
	h.be.mu.Lock()
	h.be.stopGate = gate
	h.be.mu.Unlock()

	first, second := h.connect(), h.connect()
	mustHello(t, first)
	mustHello(t, second)

	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = first.Stop("root_cli", "first", nil, nil)
	}()
	<-started
	// The first stop is inside the backend once the request has been read;
	// waiting on the flag is what makes this deterministic rather than timed.
	deadline := time.Now().Add(5 * time.Second)
	for !h.srv.stopping.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the first shutdown never reached the backend")
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := second.Stop("root_cli", "second", nil, nil); err == nil {
		t.Fatal("a second shutdown was accepted")
	} else if !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("second stop failed with %v", err)
	}
	close(gate)
}

// TestSendIsEchoedWithTheResolvedActorNotTheClaimedOne. The echo in the ring is
// the record of who typed what, so the instance user's claim is discarded.
func TestSendIsEchoedWithTheResolvedActorNotTheClaimedOne(t *testing.T) {
	h := newHarness(t, fixed(idOwner))
	cli := h.connect()
	mustHello(t, cli)

	seq, err := cli.Send("user:alice@example.com", "players", nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := h.be.sentActors(); len(got) != 1 || got[0] != "unix:pzserver" {
		t.Fatalf("the console log recorded actors %v, want [unix:pzserver]", got)
	}
	lines, _, _ := h.be.Ring().Snapshot(seq, 1)
	if len(lines) != 1 || lines[0].Source != console.SourceActor || lines[0].Actor != "unix:pzserver" {
		t.Fatalf("the ring echo is %+v", lines)
	}
}

// TestABadSendIsRefusedBeforeItReachesTheGame. The protocol's own validation
// runs on this side of the trust boundary too (D-09), so a line with a
// terminator in it never becomes two commands.
func TestABadSendIsRefusedBeforeItReachesTheGame(t *testing.T) {
	h := newHarness(t, fixed(idWeb))
	cli := h.connect()
	mustHello(t, cli)

	// Bypass the client's local validation to prove the server enforces it.
	if err := cli.Conn().WriteRequest(console.Request{Op: console.OpSend, Line: "say hi\nquit"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	ev := nextEvent(t, cli)
	if ev.Ev != console.EvError || ev.Code != console.CodeBadRequest {
		t.Fatalf("a two-command send was answered with %+v", ev)
	}
	if got := h.be.sentActors(); len(got) != 0 {
		t.Fatalf("the line reached the game anyway: %v", got)
	}
	if err := cli.Ping(); err != nil {
		t.Fatalf("the connection did not survive a bad frame: %v", err)
	}
}

// TestAnUnknownOpDoesNotEndTheConnection: a peer speaking a newer protocol
// keeps its stream.
func TestAnUnknownOpDoesNotEndTheConnection(t *testing.T) {
	h := newHarness(t, fixed(idRoot))
	cli := h.connect()
	mustHello(t, cli)

	if err := cli.Conn().WriteRequest(console.Request{Op: console.Op("teleport")}); err != nil {
		t.Fatalf("write: %v", err)
	}
	ev := nextEvent(t, cli)
	if ev.Ev != console.EvError || ev.Code != console.CodeBadRequest {
		t.Fatalf("unknown op answered with %+v", ev)
	}
	if err := cli.Ping(); err != nil {
		t.Fatalf("the connection died on an unknown op: %v", err)
	}
}

// TestTailSeparatesTheTwoCursorMeanings. from == 0 is a fresh attach and
// backlog is "the newest N"; from > 0 is a resume and backlog caps the first
// frame. Conflating them is how a backlog request returns the wrong end of a
// boot log.
func TestTailSeparatesTheTwoCursorMeanings(t *testing.T) {
	h := newHarness(t, fixed(idRoot))
	cli := h.connect()
	mustHello(t, cli)
	for i := 1; i <= 10; i++ {
		h.be.Emit(fmt.Sprintf("line %d", i))
	}

	if err := cli.Tail(0, 3, false); err != nil {
		t.Fatalf("tail: %v", err)
	}
	ev := nextEvent(t, cli)
	if got := texts(ev.Lines); strings.Join(got, ",") != "line 8,line 9,line 10" {
		t.Fatalf("a fresh attach returned %v, want the newest three", got)
	}
	if ev.Head != 10 || ev.Oldest != 1 {
		t.Fatalf("lines event reported head %d oldest %d", ev.Head, ev.Oldest)
	}

	if err := cli.Tail(2, 3, false); err != nil {
		t.Fatalf("tail: %v", err)
	}
	ev = nextEvent(t, cli)
	if got := texts(ev.Lines); strings.Join(got, ",") != "line 2,line 3,line 4" {
		t.Fatalf("a resume returned %v, want three lines from the cursor", got)
	}

	// A resume with no cap gets everything from its cursor: a cursor whose
	// history was silently truncated is worse than a large frame.
	if err := cli.Tail(9, 0, false); err != nil {
		t.Fatalf("tail: %v", err)
	}
	ev = nextEvent(t, cli)
	if got := texts(ev.Lines); strings.Join(got, ",") != "line 9,line 10" {
		t.Fatalf("an uncapped resume returned %v", got)
	}

	// A fresh attach with no backlog asks for no history at all, and gets none.
	if err := cli.Tail(0, 0, true); err != nil {
		t.Fatalf("tail: %v", err)
	}
	settle(t, cli)
	h.be.Emit("line 11")
	ev = nextEvent(t, cli)
	if got := texts(ev.Lines); strings.Join(got, ",") != "line 11" {
		t.Fatalf("a follow-only attach returned %v", got)
	}
}

// TestAnEvictedCursorIsToldItsHistoryIsGone. Resuming silently lets somebody
// conclude a crash left no trace.
func TestAnEvictedCursorIsToldItsHistoryIsGone(t *testing.T) {
	h := newHarness(t, fixed(idRoot))
	// A ring that holds three lines, so eviction is a fact rather than a wait.
	h.be.fake = console.NewFake(console.FakeOptions{Instance: "pz-main", RingLines: 3, PartialAfter: -1})
	cli := h.connect()
	mustHello(t, cli)
	for i := 1; i <= 6; i++ {
		h.be.Emit(fmt.Sprintf("line %d", i))
	}

	if err := cli.Tail(1, 0, false); err != nil {
		t.Fatalf("tail: %v", err)
	}
	ev := nextEvent(t, cli)
	if ev.Ev != console.EvTruncated {
		t.Fatalf("first event was %+v, want truncated", ev)
	}
	if ev.Oldest != 4 {
		t.Errorf("truncated reported oldest %d, want 4", ev.Oldest)
	}
	ev = nextEvent(t, cli)
	if got := texts(ev.Lines); strings.Join(got, ",") != "line 4,line 5,line 6" {
		t.Fatalf("after the truncation the lines were %v", got)
	}

	// A cursor that is still in the ring hears nothing about truncation.
	if err := cli.Tail(5, 0, false); err != nil {
		t.Fatalf("tail: %v", err)
	}
	if ev = nextEvent(t, cli); ev.Ev != console.EvLines {
		t.Fatalf("a live cursor got %+v", ev)
	}
}

func TestTruncatedFrom(t *testing.T) {
	cases := []struct {
		name                              string
		from, next, snapOldest, subOldest uint64
		want                              uint64
	}{
		{"fresh attach on an empty ring", 0, 1, 0, 0, 0},
		{"fresh attach never truncates", 0, 11, 1, 1, 0},
		{"cursor still in the ring", 5, 11, 4, 4, 0},
		{"cursor exactly at the oldest", 4, 11, 4, 4, 0},
		{"cursor below the oldest", 1, 11, 4, 4, 4},
		{"evicted between the read and the subscribe", 5, 11, 4, 20, 20},
		{"no subscription, no second rule", 5, 11, 4, 0, 0},
	}
	for _, c := range cases {
		if got := truncatedFrom(c.from, c.next, c.snapOldest, c.subOldest); got != c.want {
			t.Errorf("%s: truncatedFrom(%d,%d,%d,%d) = %d, want %d",
				c.name, c.from, c.next, c.snapOldest, c.subOldest, got, c.want)
		}
	}
}

// TestFollowIsGaplessAcrossTheBacklogHandoff. The backlog read and the
// subscribe are two separate ring operations; a line appended between them
// must still arrive exactly once.
func TestFollowIsGaplessAcrossTheBacklogHandoff(t *testing.T) {
	h := newHarness(t, fixed(idRoot))
	cli := h.connect()
	mustHello(t, cli)
	h.be.Emit("before")

	if err := cli.Tail(0, 10, true); err != nil {
		t.Fatalf("tail: %v", err)
	}
	ev := nextEvent(t, cli)
	if got := texts(ev.Lines); strings.Join(got, ",") != "before" {
		t.Fatalf("backlog was %v", got)
	}
	for i := 1; i <= 3; i++ {
		h.be.Emit(fmt.Sprintf("after %d", i))
	}
	var got []string
	for len(got) < 3 {
		got = append(got, texts(nextEvent(t, cli).Lines)...)
	}
	if strings.Join(got, ",") != "after 1,after 2,after 3" {
		t.Fatalf("the stream delivered %v", got)
	}
}

// TestASecondTailReplacesTheFirstFollower, so a client that re-attaches at a
// new cursor does not end up with two fan-outs interleaving into one socket.
func TestASecondTailReplacesTheFirstFollower(t *testing.T) {
	h := newHarness(t, fixed(idRoot))
	cli := h.connect()
	mustHello(t, cli)
	h.be.Emit("one")

	if err := cli.Tail(0, 10, true); err != nil {
		t.Fatalf("tail: %v", err)
	}
	if got := texts(nextEvent(t, cli).Lines); strings.Join(got, ",") != "one" {
		t.Fatalf("first backlog %v", got)
	}
	if err := cli.Tail(0, 10, true); err != nil {
		t.Fatalf("re-tail: %v", err)
	}
	if got := texts(nextEvent(t, cli).Lines); strings.Join(got, ",") != "one" {
		t.Fatalf("second backlog %v", got)
	}
	if n := h.be.Ring().Subscribers(); n != 1 {
		t.Fatalf("the ring has %d subscribers after a re-tail, want 1", n)
	}
	h.be.Emit("two")
	if got := texts(nextEvent(t, cli).Lines); strings.Join(got, ",") != "two" {
		t.Fatalf("the replacement stream delivered %v", got)
	}
}

// TestASlowFollowerIsLaggedAndTheProducerNeverBlocks. This is the ring's
// contract, re-asserted at the socket: a web client that stops reading must not
// be able to hang a server that is mid-save.
func TestASlowFollowerIsLaggedAndTheProducerNeverBlocks(t *testing.T) {
	h := newHarness(t, fixed(idRoot), WithSubDepth(2))
	cli := h.connect()
	mustHello(t, cli)
	if err := cli.Tail(0, 0, true); err != nil {
		t.Fatalf("tail: %v", err)
	}
	settle(t, cli)

	// Nothing reads the socket while these are produced: the fan-out goroutine
	// is blocked on the pipe after the first frame and the subscription fills.
	const lines = 5000
	start := time.Now()
	for i := 1; i <= lines; i++ {
		h.be.Emit(fmt.Sprintf("line %d", i))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the producer took %s for %d lines; the socket is applying backpressure", d, lines)
	}

	// The drop count rides the NEXT batch that fits, by design -- a reader must
	// never see a count for lines it has not received -- so production has to
	// continue while the reader drains for the debt to be reported at all.
	stop := make(chan struct{})
	var emitting sync.WaitGroup
	emitting.Add(1)
	go func() {
		defer emitting.Done()
		for i := lines + 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.be.Emit(fmt.Sprintf("line %d", i))
			time.Sleep(200 * time.Microsecond)
		}
	}()
	defer func() {
		close(stop)
		emitting.Wait()
	}()

	var lagged console.Event
	for lagged.Ev == "" {
		ev := nextEvent(t, cli)
		if ev.Ev == console.EvLagged {
			lagged = ev
		}
	}
	if lagged.Dropped == 0 {
		t.Fatal("a lagged event carried no drop count")
	}
	// resume_from must name the first line the client is about to receive, so
	// a reader can never attribute a drop to lines it has not seen.
	next := nextEvent(t, cli)
	if next.Ev != console.EvLines || len(next.Lines) == 0 {
		t.Fatalf("the event after lagged was %+v", next)
	}
	if lagged.ResumeFrom != next.Lines[0].Seq {
		t.Fatalf("lagged resume_from %d, next batch starts at %d", lagged.ResumeFrom, next.Lines[0].Seq)
	}
}

// TestTheRingClosingEndsTheStream: `gvm console` learns that the runner is gone
// rather than hanging on a connection nothing will write to again.
func TestTheRingClosingEndsTheStream(t *testing.T) {
	h := newHarness(t, fixed(idRoot))
	cli := h.connect()
	mustHello(t, cli)
	if err := cli.Tail(0, 0, true); err != nil {
		t.Fatalf("tail: %v", err)
	}
	settle(t, cli)
	h.be.Emit("last words")
	if got := texts(nextEvent(t, cli).Lines); strings.Join(got, ",") != "last words" {
		t.Fatalf("stream delivered %v", got)
	}
	h.be.Ring().Close()
	if _, err := cli.Next(); !Gone(err) {
		t.Fatalf("the connection outlived the ring: %v", err)
	}
}

// TestAPanickingBackendLosesOneConnectionAndNotTheServer is section 11.8 layer
// one: a panic in a connection handler must never reach the supervise loop,
// and it is recorded in the console an operator is already looking at.
func TestAPanickingBackendLosesOneConnectionAndNotTheServer(t *testing.T) {
	h := newHarness(t, fixed(idRoot))
	h.be.mu.Lock()
	h.be.panicOn = console.OpStatus
	h.be.mu.Unlock()

	cli := h.connect()
	mustHello(t, cli)
	if _, err := cli.Status(nil); err == nil {
		t.Fatal("a panicking status returned cleanly")
	}

	lines, _, _ := h.be.Ring().Snapshot(0, 0)
	found := false
	for _, l := range lines {
		if l.Source == console.SourceRunner && strings.Contains(l.Text, "panicked") {
			found = true
		}
	}
	if !found {
		t.Errorf("the panic was not recorded in the ring: %v", texts(lines))
	}

	// A second connection to the same server still works.
	other := h.connect()
	mustHello(t, other)
	if err := other.Ping(); err != nil {
		t.Fatalf("the server did not survive the panic: %v", err)
	}
}

// TestTooManyConnectionsAreRefusedNotQueued. Only three uids can reach the
// socket, so this bounds an fd leak in gvmd rather than a crowd.
func TestTooManyConnectionsAreRefusedNotQueued(t *testing.T) {
	h := newHarness(t, fixed(idRoot), WithMaxConns(1))
	first := h.connect()
	mustHello(t, first)

	second := h.connect()
	ev := nextEvent(t, second)
	if ev.Ev != console.EvError || !strings.Contains(ev.Error, "too many") {
		t.Fatalf("the second connection got %+v", ev)
	}
	if _, err := second.Next(); !Gone(err) {
		t.Fatalf("the refused connection stayed open: %v", err)
	}
	if err := first.Ping(); err != nil {
		t.Fatalf("the accepted connection was disturbed: %v", err)
	}
}

// TestBackendErrorsCarryACodeAClientCanBranchOn.
func TestBackendErrorsCarryACodeAClientCanBranchOn(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"unsupported", platform.Unsupported("verbs"), console.CodeUnsupported},
		{"bad argument", fmt.Errorf("broadcast: %w", ErrBadArgument), console.CodeBadRequest},
		{"anything else", errors.New("rcon: auth failed"), console.CodeInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, fixed(idRoot))
			h.be.mu.Lock()
			h.be.verbErr = c.err
			h.be.mu.Unlock()
			cli := h.connect()
			mustHello(t, cli)

			_, err := cli.Verb("root_cli", "broadcast", nil, nil)
			var pe *ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("verb error was %v", err)
			}
			if pe.Code != c.want {
				t.Errorf("code %q, want %q", pe.Code, c.want)
			}
		})
	}
}

// TestServeAcceptsAndClosesCleanly exercises the accept loop itself over a real
// listener, which on the development machine means TCP rather than AF_UNIX.
func TestServeAcceptsAndClosesCleanly(t *testing.T) {
	be := newFakeBackend("pz-main")
	t.Cleanup(func() { be.Close() })
	srv, err := New(be, testPolicy(), fixed(idRoot), WithTimeouts(2*time.Second, -1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener available: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), l) }()

	nc, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cli := NewClient(nc)
	mustHello(t, cli)
	if n := srv.Conns(); n != 1 {
		t.Errorf("server reports %d connections, want 1", n)
	}

	if err := srv.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v after Close", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
	if _, err := cli.Next(); !Gone(err) {
		t.Errorf("Close left a connection open: %v", err)
	}
}

// TestNewRefusesAServerItCannotAuthorize.
func TestNewRefusesAServerItCannotAuthorize(t *testing.T) {
	be := newFakeBackend("pz-main")
	t.Cleanup(func() { be.Close() })
	if _, err := New(be, testPolicy(), nil); err == nil {
		t.Error("a server with no identifier was accepted; peer credentials are the authorization")
	}
	if _, err := New(nil, testPolicy(), fixed(idRoot)); err == nil {
		t.Error("a server with no backend was accepted")
	}
	if _, err := New(be, Policy{ConsoleGIDs: []uint32{0}}, fixed(idRoot)); err == nil {
		t.Error("a policy with gid 0 as the console group was accepted")
	}
}

func texts(lines []console.Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}

// Drain lets `gvm run`'s teardown wait for a request that is mid-flight before
// it closes the ring underneath it. The one that matters is `op:stop`, which
// holds its connection for the whole shutdown ladder: closing the ring first
// makes the fan-out drop the very connection waiting to be told the shutdown
// finished, and the record of the stop is lost with it.
//
// It must NOT wait for readers idling in ReadRequest. An attached `gvm console`
// sits there for the life of the instance, and a teardown that waited for one
// would never return.
func TestDrainWaitsForAnInFlightRequestButNotForAnIdleReader(t *testing.T) {
	h := newHarness(t, fixed(idRoot))
	gate := make(chan struct{})
	h.be.mu.Lock()
	h.be.stopGate = gate
	h.be.mu.Unlock()

	// An idle attached client: connected, handshaken, and now blocked reading.
	idle := h.connect()
	mustHello(t, idle)

	stopper := h.connect()
	mustHello(t, stopper)
	go func() { _, _ = stopper.Stop("root_cli", "operator requested", nil, nil) }()

	deadline := time.Now().Add(5 * time.Second)
	for !h.srv.stopping.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the stop never reached the backend")
		}
		time.Sleep(time.Millisecond)
	}

	returned := make(chan struct{})
	go func() { h.srv.Drain(5 * time.Second); close(returned) }()
	select {
	case <-returned:
		t.Fatal("Drain returned while a request was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(gate)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain never returned after the request finished")
	}

	// With only the idle reader left it must return at once, not at the budget.
	start := time.Now()
	h.srv.Drain(5 * time.Second)
	if el := time.Since(start); el > time.Second {
		t.Errorf("Drain took %s with nothing in flight; it waited for an idle reader", el)
	}
}
