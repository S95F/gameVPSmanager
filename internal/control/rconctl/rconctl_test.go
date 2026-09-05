package rconctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/rcon"
	"github.com/S95F/gameVPSmanager/internal/secrets"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The manifest sentence, verbatim, from projectzomboid.yaml.
const pzReason = "RCON does not bind until Project Zomboid finishes booting"

type fakeConn struct {
	mu     sync.Mutex
	execs  []string
	out    string
	err    error
	closed int
}

func (c *fakeConn) Execute(_ context.Context, cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execs = append(c.execs, cmd)
	return c.out, c.err
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}

func (c *fakeConn) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.execs...)
}

func (c *fakeConn) closes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// rig is a Client with a scripted dialer and a fake clock, which is what makes
// a five-minute backoff a microsecond-long test.
type rig struct {
	*Client
	clock *console.FakeClock

	mu        sync.Mutex
	dials     int
	passwords []string
	conns     []*fakeConn

	// script decides what the nth dial (1-based) produces.
	script func(n int) (*fakeConn, error)
	gate   bool
}

func newRig(t *testing.T, opts func(*Options)) *rig {
	t.Helper()
	r := &rig{clock: console.NewFakeClock(time.Unix(1_700_000_000, 0).UTC())}
	r.script = func(int) (*fakeConn, error) { return &fakeConn{}, nil }

	o := Options{
		Addr:           "127.0.0.1:27015",
		Password:       secrets.New("hunter2-correct-horse"),
		Dialect:        spec.DialectSimple,
		NotReadyReason: pzReason,
		Gate:           func() bool { return r.gateOpen() },
		Clock:          r.clock.Now,
		Dialer: func(_ context.Context, _, password string, _ rcon.Options) (Conn, error) {
			r.mu.Lock()
			r.dials++
			n := r.dials
			r.passwords = append(r.passwords, password)
			script := r.script
			r.mu.Unlock()

			c, err := script(n)
			if err != nil {
				return nil, err
			}
			r.mu.Lock()
			r.conns = append(r.conns, c)
			r.mu.Unlock()
			return c, nil
		},
	}
	if opts != nil {
		opts(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Client = c
	return r
}

func (r *rig) gateOpen() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gate
}

func (r *rig) open(v bool) {
	r.mu.Lock()
	r.gate = v
	r.mu.Unlock()
}

func (r *rig) dialCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dials
}

func (r *rig) conn(i int) *fakeConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.conns) {
		return nil
	}
	return r.conns[i]
}

// ---------- rule 1: never dial before the gate opens ----------

// Project Zomboid does not bind RCON until it has finished booting, which on a
// modded first boot is forty to sixty minutes. A client that dials on a timer
// during that window writes one connection-refused per poll into the log an
// operator will read when something has actually gone wrong.
func TestNoDialWhileTheGateIsClosed(t *testing.T) {
	r := newRig(t, nil)

	if r.Ready() {
		t.Fatal("ready with a closed gate")
	}
	if got := r.Unready(); got != pzReason {
		t.Fatalf("Unready() = %q, want the manifest's sentence verbatim:\n  %q", got, pzReason)
	}

	for i := 0; i < 40; i++ {
		if _, err := r.Exec(context.Background(), "players"); !errors.Is(err, control.ErrNotReady) {
			t.Fatalf("Exec with the gate closed: %v, want ErrNotReady", err)
		}
		if _, ok, err := r.Poll(context.Background(), "players"); ok && err == nil {
			t.Fatal("Poll succeeded with the gate closed")
		}
		r.clock.Advance(time.Minute)
	}
	if n := r.dialCount(); n != 0 {
		t.Fatalf("%d dials during a boot; the whole point is zero", n)
	}

	r.open(true)
	if !r.Ready() {
		t.Fatal("not ready with the gate open")
	}
	if got := r.Unready(); got != "" {
		t.Errorf("Unready() = %q while ready", got)
	}
	if _, err := r.Exec(context.Background(), "players"); err != nil {
		t.Fatalf("Exec once the gate opened: %v", err)
	}
	if n := r.dialCount(); n != 1 {
		t.Fatalf("%d dials, want 1", n)
	}
}

// ---------- rule 2: close and redial on a protocol error ----------

// client.go's own comment is the reason: an unread fragment poisons every
// subsequent command on that connection, and a poisoned connection does not
// announce itself -- it returns the PREVIOUS command's output.
func TestProtocolErrorClosesAndRedials(t *testing.T) {
	r := newRig(t, nil)
	r.open(true)
	r.script = func(n int) (*fakeConn, error) {
		if n == 1 {
			return &fakeConn{err: io.ErrUnexpectedEOF}, nil
		}
		return &fakeConn{out: "1 player"}, nil
	}

	if _, err := r.Exec(context.Background(), "players"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Exec: %v, want the protocol error", err)
	}
	if n := r.dialCount(); n != 1 {
		t.Fatalf("%d dials for one failed command; a transparent retry would re-run a mutation", n)
	}
	if got := r.conn(0).closes(); got != 1 {
		t.Fatalf("the poisoned connection was closed %d times, want 1", got)
	}
	if s := r.Snapshot(); s.ConsecutiveFailures != 1 || s.LastError == "" {
		t.Errorf("health after a protocol error: %+v", s)
	}

	out, err := r.Exec(context.Background(), "players")
	if err != nil {
		t.Fatalf("second Exec: %v", err)
	}
	if out != "1 player" {
		t.Errorf("out %q", out)
	}
	if n := r.dialCount(); n != 2 {
		t.Fatalf("%d dials; the second command must be on a fresh connection", n)
	}
	if s := r.Snapshot(); s.ConsecutiveFailures != 0 || s.LastError != "" || s.LastOK.IsZero() {
		t.Errorf("a success did not clear the health counters: %+v", s)
	}
}

// A healthy connection is REUSED. Redialling per command would pay a TCP
// handshake and an auth round trip for every player-count poll, on the game's
// main server thread.
func TestAHealthyConnectionIsReused(t *testing.T) {
	r := newRig(t, nil)
	r.open(true)

	for i := 0; i < 5; i++ {
		if _, err := r.Exec(context.Background(), "players"); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.dialCount(); n != 1 {
		t.Fatalf("%d dials for 5 commands", n)
	}
	if got := len(r.conn(0).calls()); got != 5 {
		t.Fatalf("the connection ran %d commands", got)
	}
}

// ErrCommandTooLong is checked before rcon.Execute touches the socket, so the
// connection is still aligned. Dropping it would turn a caller's bug into a
// reconnect, and counting it would make a too-long command look like a sick
// server in status.json.
func TestCommandTooLongKeepsTheConnection(t *testing.T) {
	r := newRig(t, nil)
	r.open(true)
	r.script = func(int) (*fakeConn, error) {
		return &fakeConn{err: fmt.Errorf("%w: 5000 bytes, limit 1446", rcon.ErrCommandTooLong)}, nil
	}

	if _, err := r.Exec(context.Background(), "say "+strings.Repeat("x", 5000)); !errors.Is(err, rcon.ErrCommandTooLong) {
		t.Fatalf("Exec: %v", err)
	}
	if got := r.conn(0).closes(); got != 0 {
		t.Errorf("closed a connection that was never written to (%d times)", got)
	}
	if s := r.Snapshot(); s.ConsecutiveFailures != 0 || s.LastError != "" {
		t.Errorf("a caller's bug moved the channel health: %+v", s)
	}
	if n := r.dialCount(); n != 1 {
		t.Errorf("%d dials", n)
	}
}

// ---------- rule 3: auth backoff ----------

// A wrong password retried every minute against a plaintext console fills the
// log, trips fail2ban, and never once succeeds -- the password is in a file and
// files do not change their minds.
func TestAuthFailureBacksOffFiveMinutes(t *testing.T) {
	r := newRig(t, nil)
	r.open(true)
	r.script = func(int) (*fakeConn, error) { return nil, rcon.ErrAuthFailed }

	if _, err := r.Exec(context.Background(), "players"); !errors.Is(err, rcon.ErrAuthFailed) {
		t.Fatalf("Exec: %v, want ErrAuthFailed", err)
	}
	if n := r.dialCount(); n != 1 {
		t.Fatalf("%d dials", n)
	}
	if r.Ready() {
		t.Error("ready during an auth backoff; the dashboard would offer a button that cannot work")
	}

	// Hammering during the backoff must not reach the network at all.
	for i := 0; i < 30; i++ {
		if _, err := r.Exec(context.Background(), "players"); !errors.Is(err, control.ErrNotReady) {
			t.Fatalf("Exec during backoff: %v, want ErrNotReady", err)
		}
		r.clock.Advance(9 * time.Second)
	}
	if n := r.dialCount(); n != 1 {
		t.Fatalf("%d dials in the first 4m30s of a 5m backoff", n)
	}

	// The tooltip says what went wrong and when it will try again.
	un := r.Unready()
	if !strings.Contains(un, "authentication failed") || !strings.Contains(un, "not retrying") {
		t.Errorf("Unready() = %q", un)
	}

	r.clock.Advance(31 * time.Second)
	if !r.Ready() {
		t.Fatal("still backing off after five minutes")
	}
	r.script = func(int) (*fakeConn, error) { return &fakeConn{out: "0 players"}, nil }
	if _, err := r.Exec(context.Background(), "players"); err != nil {
		t.Fatalf("Exec after the backoff: %v", err)
	}
	if n := r.dialCount(); n != 2 {
		t.Fatalf("%d dials", n)
	}
	if s := r.Snapshot(); s.ConsecutiveFailures != 0 || s.LastError != "" {
		t.Errorf("health after recovery: %+v", s)
	}
}

// A stopped server is not a rejected password, and its backoff is seconds.
func TestOrdinaryDialFailureBacksOffBriefly(t *testing.T) {
	r := newRig(t, nil)
	r.open(true)
	refused := errors.New("dial tcp 127.0.0.1:27015: connect: connection refused")
	r.script = func(int) (*fakeConn, error) { return nil, refused }

	if _, err := r.Exec(context.Background(), "players"); !errors.Is(err, refused) {
		t.Fatalf("Exec: %v", err)
	}
	r.clock.Advance(DialBackoff - time.Second)
	if r.Ready() {
		t.Error("ready before the short backoff elapsed")
	}
	r.clock.Advance(2 * time.Second)
	if !r.Ready() {
		t.Fatalf("still backing off %s after a refused connection", DialBackoff)
	}
	if AuthBackoff <= DialBackoff {
		t.Fatal("the auth backoff is not longer than the ordinary one")
	}
}

// The gate's sentence wins over the backoff's: a stopped server is why the
// channel is down, whatever happened the last time it was up.
func TestTheGateReasonOutranksTheBackoffReason(t *testing.T) {
	r := newRig(t, nil)
	r.open(true)
	r.script = func(int) (*fakeConn, error) { return nil, rcon.ErrAuthFailed }
	if _, err := r.Exec(context.Background(), "players"); err == nil {
		t.Fatal("expected an auth failure")
	}

	r.open(false)
	if got := r.Unready(); got != pzReason {
		t.Fatalf("Unready() = %q, want the manifest's sentence once the server stopped", got)
	}
}

// ---------- min_poll ----------

// RCON commands execute on the game's MAIN SERVER THREAD. A player-count poll
// every five seconds is a TPS cost paid by every player, forever, so that a
// dashboard can be four seconds fresher.
func TestPollRespectsMinPoll(t *testing.T) {
	r := newRig(t, func(o *Options) { o.MinPoll = time.Minute })
	r.open(true)

	if _, ok, err := r.Poll(context.Background(), "players"); !ok || err != nil {
		t.Fatalf("first poll: ok=%v err=%v", ok, err)
	}
	for i := 0; i < 11; i++ {
		r.clock.Advance(5 * time.Second)
		if _, ok, _ := r.Poll(context.Background(), "players"); ok {
			t.Fatalf("poll %d ran %s after the last one", i, time.Duration(i+1)*5*time.Second)
		}
	}
	r.clock.Advance(5 * time.Second) // now exactly one minute
	if _, ok, err := r.Poll(context.Background(), "players"); !ok || err != nil {
		t.Fatalf("poll at min_poll: ok=%v err=%v", ok, err)
	}
	if got := len(r.conn(0).calls()); got != 2 {
		t.Fatalf("%d commands reached the server in one minute, want 2", got)
	}
}

// A FAILING poll still consumes its slot. One that reset nothing would retry as
// fast as the caller's loop allows, which is the hammering the floor exists to
// prevent.
func TestAFailingPollStillConsumesItsSlot(t *testing.T) {
	r := newRig(t, func(o *Options) { o.MinPoll = time.Minute })
	r.open(true)
	r.script = func(int) (*fakeConn, error) { return &fakeConn{err: io.ErrUnexpectedEOF}, nil }

	if _, ok, err := r.Poll(context.Background(), "players"); !ok || err == nil {
		t.Fatalf("first poll: ok=%v err=%v", ok, err)
	}
	r.clock.Advance(time.Second)
	if _, ok, _ := r.Poll(context.Background(), "players"); ok {
		t.Fatal("a failed poll left the floor unarmed")
	}
	if n := r.dialCount(); n != 1 {
		t.Fatalf("%d dials", n)
	}
}

func TestMinPollDefaults(t *testing.T) {
	r := newRig(t, nil)
	if got := r.MinPoll(); got != DefaultMinPoll {
		t.Fatalf("MinPoll() = %s, want %s (section 11.9)", got, DefaultMinPoll)
	}
}

// ---------- construction ----------

// A nil gate would mean "dial whenever", which is rule 1 inverted. Defaulting
// it to open is a mistake that only ever shows up as an hour of log noise on
// the one game that matters most, so it is refused at construction instead.
func TestNewRefusesAClientWithNoGate(t *testing.T) {
	_, err := New(Options{Addr: "127.0.0.1:27015", Dialect: spec.DialectSimple})
	if err == nil {
		t.Fatal("built a client with no readiness gate")
	}
	if !strings.Contains(err.Error(), "gate") {
		t.Errorf("error does not name the missing gate: %v", err)
	}
}

func TestNewRefusesAnUnknownDialect(t *testing.T) {
	_, err := New(Options{
		Addr: "127.0.0.1:27015", Dialect: "valve_empty_response",
		Gate: func() bool { return true },
	})
	if err == nil {
		t.Fatal("built a client for a dialect this build does not implement")
	}
}

func TestNewRefusesAnEmptyAddress(t *testing.T) {
	if _, err := New(Options{Gate: func() bool { return true }}); err == nil {
		t.Fatal("built a client with no address")
	}
}

// ---------- close ----------

func TestCloseDropsTheConnectionAndRefusesFurtherCommands(t *testing.T) {
	r := newRig(t, nil)
	r.open(true)
	if _, err := r.Exec(context.Background(), "players"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := r.conn(0).closes(); got != 1 {
		t.Fatalf("connection closed %d times", got)
	}
	if _, err := r.Exec(context.Background(), "players"); !errors.Is(err, control.ErrClosed) {
		t.Fatalf("Exec after Close: %v, want ErrClosed", err)
	}
	if r.Ready() {
		t.Error("ready after Close")
	}
	if !strings.Contains(r.Unready(), "shutting down") {
		t.Errorf("Unready() = %q", r.Unready())
	}
	if err := r.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// ---------- status.json ----------

func TestSnapshotIsTheStatusBlock(t *testing.T) {
	r := newRig(t, nil)
	r.open(true)

	s := r.Snapshot()
	if s.Kind != spec.ControlSourceRCON {
		t.Errorf("kind %q", s.Kind)
	}
	if s.Dialect != string(spec.DialectSimple) {
		t.Errorf("dialect %q; status.json shows the manifest's spelling", s.Dialect)
	}
	if !s.Ready || !s.LastOK.IsZero() || s.LastError != "" {
		t.Errorf("fresh snapshot %+v", s)
	}

	if _, err := r.Exec(context.Background(), "players"); err != nil {
		t.Fatal(err)
	}
	if got := r.Snapshot().LastOK; !got.Equal(r.clock.Now()) {
		t.Errorf("last_ok %s, want %s", got, r.clock.Now())
	}
}

// status.json is rewritten every two seconds and must never queue behind a
// thirty-second RCON command, so the health lock is not the exec lock.
func TestSnapshotDoesNotBlockBehindACommand(t *testing.T) {
	r := newRig(t, nil)
	r.open(true)

	release := make(chan struct{})
	entered := make(chan struct{})

	// Seed a connection whose Execute blocks until the test lets it go, so no
	// dial is needed and the only thing held is the exec lock.
	r.script = func(int) (*fakeConn, error) { return nil, errNoDialExpected }
	r.Client.setCurrent(&blockingConn{entered: entered, release: release})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Exec(context.Background(), "save")
	}()
	<-entered

	got := make(chan control.Status, 1)
	go func() { got <- r.Snapshot() }()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Error("Snapshot blocked behind an in-flight command; status.json would stop being written")
	}
	close(release)
	<-done
}

var errNoDialExpected = errors.New("the test pre-seeded the connection; no dial should happen")

type blockingConn struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingConn) Execute(context.Context, string) (string, error) {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return "", nil
}

func (c *blockingConn) Close() error { return nil }

// ---------- the password ----------

// The plaintext leaves through exactly one call, at the dial. A Client printed
// by a panicking goroutine's %+v must not put the RCON password in the journal.
func TestThePasswordIsRevealedOnlyAtTheDial(t *testing.T) {
	const pw = "hunter2-correct-horse"
	r := newRig(t, nil)
	r.open(true)
	if _, err := r.Exec(context.Background(), "players"); err != nil {
		t.Fatal(err)
	}

	r.mu.Lock()
	seen := append([]string(nil), r.passwords...)
	r.mu.Unlock()
	if len(seen) != 1 || seen[0] != pw {
		t.Fatalf("the dialer received %q", seen)
	}

	for _, dump := range []string{
		fmt.Sprintf("%v", r.Client),
		fmt.Sprintf("%+v", r.Client),
		// The case the secrets package's pointer boxing exists for: fmt walks
		// an UNEXPORTED field by reflection and cannot call String on it.
		fmt.Sprintf("%+v", struct{ p secrets.Value }{r.Client.password}),
	} {
		if strings.Contains(dump, pw) {
			t.Fatalf("a formatted Client leaks the password: %s", dump)
		}
	}
}

// The interfaces the runner wires it up through.
var (
	_ control.Channel  = (*Client)(nil)
	_ control.Reporter = (*Client)(nil)
)
