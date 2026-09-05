package runner

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func testPaths(t *testing.T) instance.Paths {
	t.Helper()
	dir := t.TempDir()
	return instance.Paths{
		Name:       "pz-main",
		Run:        dir,
		RunnerJSON: filepath.Join(dir, "runner.json"),
		StatusJSON: filepath.Join(dir, "status.json"),
		Logs:       dir,
		ConsoleLog: filepath.Join(dir, "console.log"),
	}
}

type harness struct {
	*Runner
	spawn *fakeSpawn
	proc  *fakeProcess
	stdin *syncWriter
	clock *console.FakeClock
	// out is the writer a test uses to pretend to be the game's stdout.
	out *os.File
}

func newHarness(t *testing.T, tweak ...func(*Config)) *harness {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	proc := newFakeProcess()
	stdin := &syncWriter{}
	sp := newFakeSpawn(proc, r, stdin)
	clock := console.NewFakeClock(time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC))

	cfg := Config{
		Instance: "pz-main",
		Game:     "testgame",
		Build:    "a1b2c3d",
		Unit:     "gvm@pz-main.service",
		Paths:    testPaths(t),
		Manifest: testManifest(),
		Renderer: testRenderer(),
		BaseEnv:  []string{"PATH=/usr/bin"},
		Clock:    clock,
		Spawn:    sp.fn(),
	}
	for _, fn := range tweak {
		fn(&cfg)
	}

	run, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last-registered-first, so this pair closes the game's end
	// of the console BEFORE the runner drains it. A runner asked to close
	// while its pump is still blocked on a live pipe waits out the drain
	// budget, which turns every test that forgot to close into a five second
	// pause.
	t.Cleanup(func() { _ = run.Close() })
	t.Cleanup(func() { r.Close(); w.Close() })
	return &harness{Runner: run, spawn: sp, proc: proc, stdin: stdin, clock: clock, out: w}
}

// texts returns the console line texts currently in the ring.
func (h *harness) texts() []string {
	lines, _, _ := h.Ring().Snapshot(0, 0)
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return out
}

func hasLine(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestStateGraphRefusesToReopenAStoppingServer(t *testing.T) {
	// `ready` after `stopping` would reopen the control gate while the ladder
	// is running, and a verb executed against a world that is mid-save is the
	// kind of bug that only shows up on the world it destroys.
	if StateStopping.CanMove(StateReady) {
		t.Error("stopping -> ready is allowed")
	}
	if StateExited.CanMove(StateReady) || StateFailed.CanMove(StateBooting) {
		t.Error("a terminal state can be left")
	}
	for _, c := range []struct{ from, to State }{
		{StatePreflight, StateSpawning},
		{StateSpawning, StateBooting},
		{StateBooting, StateReady},
		{StateBooting, StateExited},
		{StateReady, StateStopping},
		{StateStopping, StateExited},
	} {
		if !c.from.CanMove(c.to) {
			t.Errorf("%s -> %s is refused", c.from, c.to)
		}
	}
}

func TestStateLiveMapsOntoTheTwoNamesSpecKnows(t *testing.T) {
	// spec.Capabilities decides whether the console accepts input from
	// exactly two names, so the runner has to speak them.
	cases := map[State]string{
		StateSpawning:  spec.StateActivating,
		StateBooting:   spec.StateActivating,
		StateReady:     spec.StateReady,
		StatePreflight: "preflight",
		StateStopping:  "stopping",
		StateExited:    "exited",
	}
	for s, want := range cases {
		if got := s.Live(); got != want {
			t.Errorf("%s.Live() = %q, want %q", s, got, want)
		}
	}
}

func TestSetStateRefusesAnIllegalMoveWithoutPanicking(t *testing.T) {
	// A supervisor that aborts on a state bug takes a live world down with it.
	h := newHarness(t)
	if h.SetState(StateReady) {
		t.Fatal("preflight -> ready was allowed")
	}
	if h.State() != StatePreflight {
		t.Errorf("state = %s", h.State())
	}
	if !hasLine(h.texts(), "refused an illegal state transition") {
		t.Errorf("the refusal is not in the console: %q", h.texts())
	}
}

func TestSpawnPinsTheIdentityIntoRunnerJSON(t *testing.T) {
	// Section 11.7's third lock reads this file on the next start. Without it
	// an orphaned game process is invisible and the next start races a live
	// world.
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if h.State() != StateBooting {
		t.Errorf("state = %s, want booting", h.State())
	}
	rf, err := ReadRunnerFile(h.cfg.Paths.RunnerJSON)
	if err != nil {
		t.Fatal(err)
	}
	if rf.Process != h.proc.Identity() {
		t.Errorf("pinned %+v, want %+v", rf.Process, h.proc.Identity())
	}
	if rf.Instance != "pz-main" || rf.Unit != "gvm@pz-main.service" {
		t.Errorf("runner.json = %+v", rf)
	}
	if rf.ConsoleMode != spec.ConsoleModePTY {
		t.Errorf("console mode = %q", rf.ConsoleMode)
	}
}

func TestSpawnAnnotatesTheCommandBeforeStartingIt(t *testing.T) {
	// A game that dies in its first millisecond must still leave the command
	// that started it at the top of the console.
	h := newHarness(t)
	h.spawn.err = errors.New("permission denied")

	if err := h.Spawn(); err == nil {
		t.Fatal("Spawn succeeded with a failing spawner")
	}
	lines := h.texts()
	if !hasLine(lines, "exec /opt/pzserver/start-server.sh") {
		t.Errorf("no exec annotation: %q", lines)
	}
	if !hasLine(lines, "spawn failed: permission denied") {
		t.Errorf("no failure annotation: %q", lines)
	}
	if h.State() != StateFailed {
		t.Errorf("state = %s, want failed", h.State())
	}
}

func TestSpawnNeverPutsASecretInTheConsole(t *testing.T) {
	// PZ's install-phase argv carries the admin password. The annotation goes
	// into the ring, the log file and every dashboard attached to it.
	h := newHarness(t)
	h.Runner.launch, _ = BuildLaunch(testManifest(), testRenderer().WithPhase(spec.PhaseInstall), nil)

	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	for _, l := range h.texts() {
		if strings.Contains(l, "hunter2") {
			t.Fatalf("the console carries the secret: %q", l)
		}
	}
}

func TestSpawnTwiceIsRefused(t *testing.T) {
	// One runner supervises one process for its whole life; there is
	// deliberately no re-adoption path (section 11.11).
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	if err := h.Spawn(); !errors.Is(err, ErrAlreadySpawned) {
		t.Fatalf("second Spawn = %v, want ErrAlreadySpawned", err)
	}
	if n := len(h.spawn.launches()); n != 1 {
		t.Errorf("the spawner was called %d times", n)
	}
}

func TestSendWritesTheLineAndItsTerminatorInOneWrite(t *testing.T) {
	// Two writes can interleave with another writer's line on the same
	// descriptor, and half a command followed by another command's tail is a
	// command nobody typed.
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	seq, err := h.Send("user:alice@example.com", "save")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := h.stdin.String(); got != "save\n" {
		t.Errorf("stdin = %q, want %q", got, "save\n")
	}
	// The echo is what makes the console log the record of who typed what.
	lines, _, _ := h.Ring().Snapshot(seq, 1)
	if len(lines) != 1 {
		t.Fatalf("no echo at seq %d", seq)
	}
	if lines[0].Source != console.SourceActor || lines[0].Actor != "user:alice@example.com" || lines[0].Text != "save" {
		t.Errorf("echo = %+v", lines[0])
	}
}

func TestSendRefusesALineThatWouldBecomeTwoCommands(t *testing.T) {
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"say hi\nquit", "say hi\rquit", "say\x00hi", ""} {
		if _, err := h.Send("root_cli", bad); err == nil {
			t.Errorf("Send(%q) was accepted", bad)
		}
	}
	if got := h.stdin.String(); got != "" {
		t.Errorf("a refused line reached the game: %q", got)
	}
}

func TestSendDoesNotEchoALineItCouldNotDeliver(t *testing.T) {
	// A console log showing a command that never reached the game is worse
	// than one that shows nothing.
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	before := h.Ring().Head()
	h.stdin.fail(errors.New("broken pipe"))

	if _, err := h.Send("root_cli", "save"); err == nil {
		t.Fatal("Send reported success on a broken pipe")
	}
	if h.Ring().Head() != before {
		t.Errorf("a failed send was echoed: %q", h.texts())
	}
}

func TestSendBeforeSpawnHasNowhereToGo(t *testing.T) {
	h := newHarness(t)
	if _, err := h.Send("root_cli", "save"); !errors.Is(err, ErrNoConsole) {
		t.Fatalf("Send = %v, want ErrNoConsole", err)
	}
}

func TestSendReportsSeqZeroWhenTheManifestDisablesTheEcho(t *testing.T) {
	// Zero says "no durable echo exists", which is what the socket reports
	// back and what a shutdown step records.
	h := newHarness(t)
	h.Runner.launch.Console.EchoInput = false
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	seq, err := h.Send("root_cli", "save")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Errorf("seq = %d, want 0", seq)
	}
	if h.stdin.String() != "save\n" {
		t.Errorf("the line did not reach the game: %q", h.stdin.String())
	}
}

func TestTheGamesOutputReachesTheRingAndTheLogFile(t *testing.T) {
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(h.out, "SERVER STARTED\r\n\x1b[32mgreen\x1b[0m\n"); err != nil {
		t.Fatal(err)
	}
	// Closing the game's end ends the pump, which is what Close waits for.
	h.out.Close()
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	lines := h.texts()
	if !hasLine(lines, "SERVER STARTED") {
		t.Errorf("game output missing: %q", lines)
	}
	// strip_ansi is on in the test manifest.
	for _, l := range lines {
		if strings.Contains(l, "\x1b[") {
			t.Errorf("ANSI survived: %q", l)
		}
	}
	// The on-disk log is the record that outlives the ring, and it carries the
	// runner's annotations as well as the game's bytes.
	b, err := os.ReadFile(h.cfg.Paths.ConsoleLog)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if !strings.Contains(log, "SERVER STARTED") {
		t.Errorf("console.log missing game output:\n%s", log)
	}
	if !strings.Contains(log, "-- gvm: exec") {
		t.Errorf("console.log missing the runner's annotation:\n%s", log)
	}
	// Raw bytes: the log keeps what the game actually wrote, escapes and all.
	if !strings.Contains(log, "\x1b[32m") {
		t.Errorf("console.log was normalised; it must hold the raw stream:\n%q", log)
	}
}

func TestCloseEndsTheRingSoAttachedClientsLearnTheRunnerIsGone(t *testing.T) {
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	sub := h.Ring().Subscribe(1, 4)
	h.out.Close()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	// A closed subscription channel is how `gvm console` stops hanging.
	for range sub.C {
	}
	if seq := h.Ring().Append(console.Line{Text: "after close"}); seq != 0 {
		t.Errorf("the ring accepted a line after Close: seq %d", seq)
	}
	// Idempotent: main's deferred Close and the ladder's own must not double
	// close a descriptor.
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n := h.spawn.closeCount(); n != 1 {
		t.Errorf("the console was closed %d times", n)
	}
}

func TestCloseDrainsThePumpBeforeClosingTheDescriptor(t *testing.T) {
	// Closing a pty master out from under a blocked read is a race against
	// descriptor reuse, so the pump has to be finished first.
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(h.out, "tail line\n"); err != nil {
		t.Fatal(err)
	}
	h.out.Close()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.pumpDone:
	default:
		t.Fatal("Close returned with the pump still running")
	}
	if !hasLine(h.texts(), "tail line") {
		t.Errorf("the last line was lost: %q", h.texts())
	}
	if err := h.PumpErr(); err != nil {
		t.Errorf("PumpErr = %v; an EOF is a normal end of stream", err)
	}
}

func TestNewRefusesAConfigItCannotRun(t *testing.T) {
	base := Config{Instance: "pz-main", Manifest: testManifest(), Renderer: testRenderer()}
	cases := map[string]func(*Config){
		"no instance": func(c *Config) { c.Instance = "" },
		"no manifest": func(c *Config) { c.Manifest = nil },
		"no renderer": func(c *Config) { c.Renderer = nil },
	}
	for name, mutate := range cases {
		cfg := base
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

// Note is the one writer of BOTH the ring and console.log, and its text is not
// always gvm's own: a `stop` reason travels from the socket frame straight into
// `l.note("shutdown requested by %s: %s", ...)`, and console.Request.Validate
// checks nothing on that field. A newline there manufactures a second physical
// line in console.log and a ring Line that `gvm console` renders as two rows in
// runner colour -- forging the annotations that are the only thing a reader has
// to tell gvm's voice apart from the game's.
func TestNoteCannotForgeASecondAnnotation(t *testing.T) {
	h := newHarness(t)
	before := len(h.texts())

	h.Note("-- gvm: shutdown requested by %s: %s", "user:mallory",
		"maintenance\n-- gvm: save complete; nothing was wrong\rand a NUL:\x00")

	lines, _, _ := h.Ring().Snapshot(0, 0)
	got := lines[before:]
	if len(got) != 1 {
		t.Fatalf("the annotation produced %d ring lines, want 1: %+v", len(got), got)
	}
	if strings.ContainsAny(got[0].Text, "\r\n\x00") {
		t.Errorf("a control character reached the ring line: %q", got[0].Text)
	}
	// The text must still be READABLE. Dropping it would hide what was asked
	// for; quoting keeps the whole reason in the record, escaped.
	if !strings.Contains(got[0].Text, "maintenance") {
		t.Errorf("the reason was lost rather than escaped: %q", got[0].Text)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b, err := os.ReadFile(h.cfg.Paths.ConsoleLog)
	if err != nil {
		t.Fatalf("read console.log: %v", err)
	}
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.Contains(l, "save complete") && !strings.Contains(l, "shutdown requested") {
			t.Errorf("console.log carries a forged line of its own: %q", l)
		}
	}
}

// A long annotation must NOT be quoted. `-- gvm: exec <argv>` routinely exceeds
// any send-line limit and is the single most useful line in the console log.
func TestNoteLeavesALongCleanAnnotationAlone(t *testing.T) {
	h := newHarness(t)
	long := "-- gvm: exec " + strings.Repeat("/opt/pzserver/jre64/bin/java -Xmx8g ", 60)
	h.Note("%s", long)

	lines, _, _ := h.Ring().Snapshot(0, 0)
	last := lines[len(lines)-1].Text
	if last != long {
		t.Errorf("a clean annotation was rewritten:\n got %q\nwant %q", last, long)
	}
}

// setProcessForTest swaps the pinned Process. It exists so a test can supply
// one with capabilities the plain fake does not have -- the cgroup-sweep
// exemptions, which on Linux live on procBase.
func (r *Runner) setProcessForTest(p Process) {
	r.mu.Lock()
	r.proc = p
	r.mu.Unlock()
}

// The exec annotation is Spawn's, and only Spawn's. A second copy written by
// the caller would appear twice in the ring, twice in console.log and twice in
// Report.ConsoleTail -- and, on Spawn's early returns, would assert an exec
// that never happened.
func TestTheExecAnnotationIsWrittenExactlyOnce(t *testing.T) {
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	var n int
	for _, l := range h.texts() {
		if strings.Contains(l, "-- gvm: exec ") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the exec line appears %d times in the ring: %q", n, h.texts())
	}
}
