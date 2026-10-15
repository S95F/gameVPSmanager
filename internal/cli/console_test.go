package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/socket"
)

// The client is exercised against the REAL socket server over an in-memory
// listener. That is the whole point of the split in this package: none of
// SO_PEERCRED, /run/gvm or a pty exists on the development machine, but the
// framing, the authorization decision, the backlog handoff and every line of
// the attach/detach UI do, so all of it runs here.

// pipeListener hands out net.Pipe pairs, so socket.Server.Serve — the same
// accept loop the runner uses — can be driven with no filesystem.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

// dial returns the client end of a new connection, or fails the test if the
// server has stopped accepting.
func (l *pipeListener) dial(t *testing.T) net.Conn {
	t.Helper()
	cli, srv := net.Pipe()
	select {
	case l.conns <- srv:
		return cli
	case <-l.closed:
		t.Fatal("listener is closed")
	case <-time.After(5 * time.Second):
		t.Fatal("nothing accepted the connection")
	}
	return nil
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// testBackend is socket.Backend built on console.Fake. Emit goes through the
// real ingest and the real ring, so the backlog these tests read is produced by
// the splitter that ships.
type testBackend struct {
	fake  *console.Fake
	stops chan struct{}
}

func newTestBackend(name string) *testBackend {
	return &testBackend{
		fake:  console.NewFake(console.FakeOptions{Instance: name, PartialAfter: -1}),
		stops: make(chan struct{}, 4),
	}
}

func (b *testBackend) Instance() string                 { return b.fake.Instance }
func (b *testBackend) Build() string                    { return "a1b2c3d" }
func (b *testBackend) Ring() *console.Ring              { return b.fake.Ring }
func (b *testBackend) Send(a, l string) (uint64, error) { return b.fake.Send(a, l) }
func (b *testBackend) Status() (json.RawMessage, error) {
	return json.RawMessage(`{"state":"ready"}`), nil
}
func (b *testBackend) Caps() (json.RawMessage, error) {
	return json.RawMessage(`[{"cap":"console.write","supported":true,"available":true}]`), nil
}
func (b *testBackend) Close() error { return b.fake.Close() }

func (b *testBackend) Verb(_ context.Context, actor, verb string, args map[string]string) (socket.VerbResult, error) {
	return socket.VerbResult{Channel: "control", Output: "Message sent."}, nil
}

func (b *testBackend) Stop(_ context.Context, actor, reason string, step func(rung, send string)) (json.RawMessage, error) {
	b.stops <- struct{}{}
	step("console", "save")
	return json.RawMessage(`{"final_rung":"console"}`), nil
}

// harness is one server, one backend and a listener, cleaned up together.
type consoleHarness struct {
	t   *testing.T
	be  *testBackend
	srv *socket.Server
	l   *pipeListener
}

const (
	testOwnerUID = 4242
	testRootUID  = 0
)

func newConsoleHarness(t *testing.T, uid uint32) *consoleHarness {
	t.Helper()
	be := newTestBackend("pz-main")
	id := socket.Identity{Peer: platform.Peer{PID: 99, UID: uid, GID: uid}, Name: "tester"}
	if uid == 0 {
		id.Name = "root"
	}
	srv, err := socket.New(be,
		socket.Policy{OwnerUID: testOwnerUID},
		func(net.Conn) (socket.Identity, error) { return id, nil },
		socket.WithTimeouts(5*time.Second, -1))
	if err != nil {
		t.Fatalf("socket.New: %v", err)
	}
	l := newPipeListener()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		_ = l.Close()
		_ = srv.Close()
		<-done
		_ = be.Close()
	})
	return &consoleHarness{t: t, be: be, srv: srv, l: l}
}

func (h *consoleHarness) client() *socket.Client {
	h.t.Helper()
	c := socket.NewClient(h.l.dial(h.t))
	h.t.Cleanup(func() { _ = c.Close() })
	return c
}

// attach runs one whole session with the given stdin, and returns what the
// operator saw.
func (h *consoleHarness) attach(opt consoleOptions, stdin io.Reader) (string, error) {
	h.t.Helper()
	var out bytes.Buffer
	env := &Env{Stdout: &out, Stderr: io.Discard, Stdin: stdin}
	err := attachConsole(context.Background(), env, h.client(), opt)
	return out.String(), err
}

func plainOpts() consoleOptions {
	return consoleOptions{Name: "pz-main", Backlog: 100, Follow: true}
}

// THE assertion this command exists for. A user who reads a detach as a stop
// does something worse next, so both the banner and the notice have to say what
// happened to the SERVER, not just what happened to the console.
func TestDetachSaysTheServerIsStillRunning(t *testing.T) {
	h := newConsoleHarness(t, testRootUID)
	h.be.fake.Emit("SERVER STARTED")

	out, err := h.attach(plainOpts(), strings.NewReader(":detach\n"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	for _, want := range []string{
		"Ctrl-D or :detach leaves the console. The server keeps running.",
		"detached from pz-main. The server is STILL RUNNING.",
		"to stop it: gvm stop pz-main",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the session never said %q:\n%s", want, out)
		}
	}
}

// Ctrl-D is EOF on stdin and must be indistinguishable from :detach. It is the
// documented way out (section 20's runbook line says so in as many words), so a
// silent exit here would be the failure this whole file is about.
func TestCtrlDDetachesWithTheSameNotice(t *testing.T) {
	h := newConsoleHarness(t, testRootUID)
	out, err := h.attach(plainOpts(), strings.NewReader(""))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !strings.Contains(out, "The server is STILL RUNNING") {
		t.Errorf("EOF did not produce the detach notice:\n%s", out)
	}
}

// The inverse confusion: `:stop` must not be a way to leave, and must not be a
// way to stop the server by accident either. It answers, and does nothing.
func TestStopIsNotAConsoleCommand(t *testing.T) {
	h := newConsoleHarness(t, testRootUID)
	out, err := h.attach(plainOpts(), strings.NewReader(":stop\n:detach\n"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !strings.Contains(out, "it would have to stop the SERVER") {
		t.Errorf("`:stop` did not explain itself:\n%s", out)
	}
	if !strings.Contains(out, "gvm stop pz-main") {
		t.Errorf("`:stop` did not name the command that would work:\n%s", out)
	}
	select {
	case <-h.be.stops:
		t.Fatal("`:stop` ran the shutdown ladder")
	default:
	}
}

// A session must not lose the line that explains everything before it. The
// backlog is read from the ring and the follow subscription starts where it
// ended, so the boot line and a line printed after the attach both arrive, in
// order, exactly once.
func TestBacklogThenFollowIsGapless(t *testing.T) {
	h := newConsoleHarness(t, testRootUID)
	for _, s := range []string{"boot 1", "boot 2", "SERVER STARTED"} {
		h.be.fake.Emit(s)
	}

	in, w := io.Pipe()
	t.Cleanup(func() { _ = in.Close() })

	type result struct {
		out string
		err error
	}
	res := make(chan result, 1)
	go func() {
		out, err := h.attach(plainOpts(), in)
		res <- result{out, err}
	}()

	// The emit has to happen after the follower is subscribed, and the only
	// observable that says so is the note the session prints once the pong
	// closes the backlog. Writing a no-op line and waiting for the ordering
	// would be a race; instead the ring is written to until the line shows up.
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.be.fake.Emit("live line")
		if time.Now().After(deadline) {
			t.Fatal("the session never reached the follow phase")
		}
		time.Sleep(5 * time.Millisecond)
		if len(h.be.fake.Texts()) > 20 {
			break
		}
	}
	_, _ = io.WriteString(w, ":detach\n")
	_ = w.Close()

	r := <-res
	if r.err != nil {
		t.Fatalf("attach: %v", r.err)
	}
	for _, want := range []string{"boot 1", "boot 2", "SERVER STARTED", "live line"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the session never printed %q:\n%s", want, r.out)
		}
	}
	if n := strings.Count(r.out, "SERVER STARTED"); n != 1 {
		t.Errorf("SERVER STARTED printed %d times; the backlog and the stream overlap", n)
	}
}

// --no-follow is the "what just happened" mode: history, then exit, with no
// stdin read at all. Without the ping marker it could not know when the backlog
// had ended, and would hang on an idle server.
func TestNoFollowPrintsTheBacklogAndReturns(t *testing.T) {
	h := newConsoleHarness(t, testRootUID)
	h.be.fake.Emit("first")
	h.be.fake.Emit("second")

	opt := plainOpts()
	opt.Follow = false
	done := make(chan string, 1)
	go func() {
		// A blocking reader: if --no-follow read stdin at all, this hangs.
		pr, _ := io.Pipe()
		out, err := h.attach(opt, pr)
		if err != nil {
			t.Errorf("attach: %v", err)
		}
		done <- out
	}()
	select {
	case out := <-done:
		if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
			t.Errorf("backlog missing:\n%s", out)
		}
		if strings.Contains(out, "STILL RUNNING") {
			t.Errorf("--no-follow printed a detach notice; it never attached:\n%s", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("--no-follow did not return")
	}
}

// A line typed at the console reaches the game and comes back attributed, which
// is what makes console.log the record of who typed what.
func TestTypedLineIsSentAndEchoedWithItsActor(t *testing.T) {
	h := newConsoleHarness(t, testRootUID)

	in, w := io.Pipe()
	res := make(chan string, 1)
	go func() {
		out, err := h.attach(plainOpts(), in)
		if err != nil {
			t.Errorf("attach: %v", err)
		}
		res <- out
	}()

	_, _ = io.WriteString(w, "players\n")
	deadline := time.Now().Add(5 * time.Second)
	for len(h.be.fake.SentLines()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the line never reached the backend")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, _ = io.WriteString(w, ":detach\n")
	_ = w.Close()
	out := <-res

	if got := h.be.fake.SentLines(); len(got) != 1 || got[0] != "players" {
		t.Fatalf("backend received %q", got)
	}
	// root_cli, resolved by the socket from the credentials — not claimed by
	// the client, which sent no actor at all.
	if !strings.Contains(out, "root_cli> players") {
		t.Errorf("the echo is not attributed in the console:\n%s", out)
	}
}

// The owner tier may write but may not run verbs. The client
// must render that from the hello frame rather than discovering it by being
// refused, because a refusal after the fact reads as a broken server.
func TestVerbIsRefusedLocallyForATierThatCannotUseIt(t *testing.T) {
	h := newConsoleHarness(t, testOwnerUID)
	out, err := h.attach(plainOpts(), strings.NewReader(":verb broadcast Msg=hi\n:detach\n"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !strings.Contains(out, "may not run verbs") {
		t.Errorf("the client did not explain the tier:\n%s", out)
	}
}

// A runner that exits closes the ring, and that is how an attached console
// learns the server is gone. It must not look like a detach.
func TestRunnerGoingAwayIsNotADetach(t *testing.T) {
	h := newConsoleHarness(t, testRootUID)
	h.be.fake.Emit("about to die")

	in, _ := io.Pipe()
	t.Cleanup(func() { _ = in.Close() })
	res := make(chan string, 1)
	go func() {
		out, err := h.attach(plainOpts(), in)
		if err != nil {
			t.Errorf("attach: %v", err)
		}
		res <- out
	}()

	// Give the follower time to subscribe, then end the ring the way
	// Runner.Close does.
	time.Sleep(100 * time.Millisecond)
	_ = h.be.Close()

	select {
	case out := <-res:
		if !strings.Contains(out, "the console ended. The runner is gone.") {
			t.Errorf("a dead runner rendered as something else:\n%s", out)
		}
		if strings.Contains(out, "STILL RUNNING") {
			t.Errorf("a dead runner rendered as a detach:\n%s", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not notice the runner had gone")
	}
}

// Ring.Supersede republishes the SAME seq when a partial line completes. A
// client that appended instead of replacing would scroll a hundred rows of a
// progress bar past the operator.
func TestASupersededLineIsRewrittenInPlace(t *testing.T) {
	var out bytes.Buffer
	u := &consoleUI{opt: consoleOptions{ANSI: true}, out: &out}
	u.lineLocked(console.Line{Seq: 7, Text: "downloading 10%", Partial: true})
	u.lineLocked(console.Line{Seq: 7, Text: "downloading 90%", Partial: true})
	u.lineLocked(console.Line{Seq: 7, Text: "downloading 100%"})

	got := out.String()
	if strings.Count(got, "\r\x1b[K") != 2 {
		t.Errorf("the row was not rewritten in place: %q", got)
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("a partial line was terminated: %q", got)
	}
}

// With no escape sequences allowed — the screen bridge, or a pipe into a file —
// the same lines must still be readable, one per row, rather than overwriting
// each other with a bare CR.
func TestASupersededLineWithoutANSIFallsBackToRows(t *testing.T) {
	var out bytes.Buffer
	u := &consoleUI{opt: consoleOptions{ANSI: false}, out: &out}
	u.lineLocked(console.Line{Seq: 7, Text: "downloading 10%", Partial: true})
	u.lineLocked(console.Line{Seq: 7, Text: "downloading 100%"})

	got := out.String()
	if strings.Contains(got, "\x1b") {
		t.Errorf("--no-color emitted an escape sequence: %q", got)
	}
	if !strings.Contains(got, "downloading 10%\ndownloading 100%\n") {
		t.Errorf("lines are not on their own rows: %q", got)
	}
}

// Colour must never be left open across a newline: it bleeds into whatever the
// game prints next, and on a wrapped terminal into the scrollback above it.
func TestPaintResetsBeforeTheNewline(t *testing.T) {
	got := paint(true, colorBold, "hello\n")
	if !strings.HasSuffix(got, colorReset+"\n") {
		t.Errorf("reset is not before the newline: %q", got)
	}
	if plain := paint(false, colorBold, "hello\n"); plain != "hello\n" {
		t.Errorf("colour off still painted: %q", plain)
	}
}

func TestActorPrefixKeepsTheNameAndDropsTheAddress(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"user:alice@example.com", "alice"},
		{"root_cli", "root_cli"},
		{"unix:pzserver", "pzserver"},
		{"token:ab12", "ab12"},
		{"", ""},
	} {
		if got := actorPrefix(tc.in); got != tc.want {
			t.Errorf("actorPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseConsoleArgs(t *testing.T) {
	env := &Env{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader("")}

	t.Run("name resolves to the socket path", func(t *testing.T) {
		opt, err := parseConsoleArgs(env, []string{"pz-main"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if want := instance.PathsFor("pz-main").ConsoleSock; opt.Socket != want {
			t.Errorf("socket = %q, want %q", opt.Socket, want)
		}
		if !opt.Follow || !opt.ANSI || opt.Backlog != DefaultBacklog {
			t.Errorf("defaults are wrong: %+v", opt)
		}
	})

	// The screen bridge renders exactly this, so a rename here breaks
	// `screen -r pzserver` on an adopted box. runner.ScreenCompat.Argv is the
	// other half of the contract and this asserts they still meet.
	t.Run("the screen bridge's argv parses", func(t *testing.T) {
		opt, err := parseConsoleArgs(env, []string{"--socket", "/run/gvm/pz-main/console.sock", "--no-color"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if opt.ANSI {
			t.Error("--no-color did not turn escape sequences off")
		}
		if opt.Name != "" {
			t.Errorf("--socket should not imply an instance name, got %q", opt.Name)
		}
	})

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no target", nil, "name an instance"},
		{"two names", []string{"a-name", "b-name"}, "name one instance"},
		{"both forms", []string{"pz-main", "--socket", "/tmp/x"}, "not both"},
		{"bad name", []string{"../etc"}, "instance"},
		{"negative backlog", []string{"pz-main", "--backlog", "-1"}, "negative"},
		{"actor with a CR", []string{"pz-main", "--actor", "user:alice\rquit"}, "attribution"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConsoleArgs(env, tc.args)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A refused actor claim must not be quoted back: doing so reproduces the
// injection one line lower, in the message complaining about it.
func TestARefusedActorIsNotEchoed(t *testing.T) {
	env := &Env{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader("")}
	_, err := parseConsoleArgs(env, []string{"pz-main", "--actor", "user:alice\nsave"})
	if err == nil {
		t.Fatal("accepted a control character in an actor claim")
	}
	if strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "\n") {
		t.Errorf("the refusal echoed the value: %q", err)
	}
}

// Off Linux the socket cannot exist. Failing with the project's one sentinel is
// what lets a developer, doctor and the test suite all recognise it.
func TestHostCommandsRefuseCleanlyOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("this asserts the non-Linux refusal")
	}
	for _, args := range [][]string{
		{"console", "pz-main"},
		{"control", "caps", "pz-main"},
		{"run", "pz-main"},
		{"run-prepare", "pz-main"},
		{"run-shutdown", "pz-main"},
	} {
		env := &Env{
			Args: args, Stdin: strings.NewReader(""),
			Stdout: io.Discard, Stderr: io.Discard,
			Getenv: func(string) string { return "" }, Geteuid: func() int { return 0 },
		}
		cmd, ok := lookup(args[0])
		if !ok {
			t.Fatalf("%s is not dispatchable", args[0])
		}
		err := cmd.Run(context.Background(), env, args[1:])
		if !errors.Is(err, platform.ErrNotSupported) {
			t.Errorf("%v: err = %v, want platform.ErrNotSupported", args, err)
		}
	}
}

// The dial failure an operator hits most often is "the server is not running",
// and ENOENT on a socket path means exactly that. Printing the raw syscall
// error instead leaves every reader to translate it.
func TestDialErrorNamesTheInstance(t *testing.T) {
	err := dialError("pz-main", "/run/gvm/pz-main/console.sock", os.ErrNotExist)
	if !strings.Contains(err.Error(), "pz-main is not running") {
		t.Errorf("error = %q", err)
	}
	perm := dialError("pz-main", "/run/gvm/pz-main/console.sock", os.ErrPermission)
	if !strings.Contains(perm.Error(), socket.ConsoleGroup) {
		t.Errorf("a permission failure did not name the group: %q", perm)
	}
}

// render holds u.mu across its whole switch, so every branch must reach the
// terminal through a *Locked writer. One branch calling the unlocked twin
// self-deadlocks on a non-reentrant sync.Mutex and freezes the console for
// good: the reader never returns, so :detach's end() blocks on <-events too.
// Table-driven over every EventKind the runner can send, so a branch added
// later cannot reintroduce it.
func TestRenderNeverTakesItsOwnLockTwice(t *testing.T) {
	t.Parallel()
	evs := []console.Event{
		console.LinesEvent(2, 1, []console.Line{{Seq: 1, Text: "a", Partial: true}}),
		console.LaggedEvent(3, 9),
		console.TruncatedEvent(4),
		console.SentEvent(5),
		console.VerbEvent("control", "Message sent.\nsecond line"),
		console.VerbEvent("control", ""),
		console.StatusEvent(json.RawMessage(`{"state":"running"}`)),
		console.StatusEvent(nil),
		console.CapsEvent(json.RawMessage(`{"stop":true}`)),
		{Ev: console.EvShutdown, Phase: console.PhaseStep, Rung: "verb", Send: "save"},
		{Ev: console.EvShutdown, Phase: console.PhaseStep, Rung: "sigterm"},
		{Ev: console.EvShutdown, Phase: console.PhaseDone, Report: json.RawMessage(`{"ok":true}`)},
		{Ev: console.EvShutdown, Phase: "begin"},
		console.ErrorEvent(console.CodeDenied, "no"),
		{Ev: console.EvPong},
		{Ev: "something-this-build-has-never-heard-of"},
	}
	for _, ev := range evs {
		t.Run(string(ev.Ev), func(t *testing.T) {
			// A partial line first, so closeOpenLineLocked runs on the branches
			// that are supposed to close one before speaking as gvm.
			u := &consoleUI{out: &bytes.Buffer{}, openLine: true}
			done := make(chan struct{})
			go func() { defer close(done); u.render(ev) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("render(%q) never returned; it took u.mu twice", ev.Ev)
			}
		})
	}
}
