// Package runner is the per-instance supervisor: one process per game server,
// the main process of gvm@<name>.service, never root (section 11).
//
// It owns a pty (or a pair of pipes), a ring buffer, a rotating log file and
// the identity of the process group it spawned. Everything that decides WHAT
// a line is lives in internal/console; everything that decides who may say it
// lives in internal/socket; this package is where a game becomes a process and
// where that process is held onto safely.
//
// Three traps shape the Linux half of it, and each one has cost somebody a
// world:
//
//  1. PROCESS GROUP. A launcher script that starts the engine in a subshell
//     without exec leaves the supervisor holding the SHELL's pid. Signalling
//     it does nothing to the engine. gvm calls setsid at spawn and signals the
//     GROUP, and pins (pid, pgid, start_ticks) so pid reuse cannot redirect
//     that signal at a stranger.
//  2. PTY EIO. Reading a pty master after the last slave closes returns EIO on
//     Linux, which is a normal end of stream. Treating it as an error
//     truncates the crash tail -- the output you need to find out why the
//     server died. And the parent must close its own copy of the slave right
//     after the fork, or the read never ends at all.
//  3. NO SHELL. Manifest values reach argv. If any path here built a command
//     string, a server name would be a remote code execution.
//
// The portable half -- state, launch building, preflight, the pump loop, the
// injector -- is pure Go and is tested on the Windows development machine
// against a fake process. The Linux files hold syscalls and nothing else.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// State is the runner's lifecycle, from section 11.5.
type State string

const (
	StatePreflight State = "preflight"
	StateSpawning  State = "spawning"
	StateBooting   State = "booting"
	StateReady     State = "ready"
	StateStopping  State = "stopping"
	StateExited    State = "exited"
	StateFailed    State = "failed"
)

// transitions is the legal state graph. It is a table rather than a pile of if
// statements because the states that must NOT follow each other are the
// interesting ones: `ready` after `stopping` would reopen the control gate
// while the ladder is running, and a verb executed against a server that is
// mid-save is the kind of bug that only shows up on the world it destroys.
var transitions = map[State][]State{
	StatePreflight: {StateSpawning, StateFailed},
	StateSpawning:  {StateBooting, StateExited, StateFailed},
	StateBooting:   {StateReady, StateStopping, StateExited, StateFailed},
	StateReady:     {StateStopping, StateExited, StateFailed},
	StateStopping:  {StateExited, StateFailed},
	StateExited:    nil,
	StateFailed:    nil,
}

// CanMove reports whether s may become to.
func (s State) CanMove(to State) bool {
	for _, ok := range transitions[s] {
		if ok == to {
			return true
		}
	}
	return false
}

// Terminal reports whether the runner has nowhere left to go.
func (s State) Terminal() bool { return s == StateExited || s == StateFailed }

// Live maps the runner's state onto the two names spec.Live understands, so
// the capability table and the runner cannot disagree about whether the
// console accepts input.
//
// `preflight` deliberately does not map to `activating`: there is no child
// yet, and a console write capability advertised before exec would render an
// enabled input box for a server that does not exist.
func (s State) Live() string {
	switch s {
	case StateSpawning, StateBooting:
		return spec.StateActivating
	case StateReady:
		return spec.StateReady
	}
	return string(s)
}

var (
	// ErrNoConsole means this instance has no stream to write to: console mode
	// `none`, or the child has not been spawned yet.
	ErrNoConsole = errors.New("runner: this instance has no console to write to")
	// ErrAlreadySpawned guards a second Spawn on one runner. One runner
	// supervises one process for its whole life; there is deliberately no
	// re-adoption path (section 11.11).
	ErrAlreadySpawned = errors.New("runner: the game has already been spawned")
)

// pumpDrainBudget is how long Close waits for the pump to finish before
// closing the descriptors under it. The pump normally ends on its own the
// moment the last slave closes; this bound exists so that a pathological read
// cannot stop the runner from exiting and releasing the world lock.
const pumpDrainBudget = 5 * time.Second

// Runner supervises one game process.
type Runner struct {
	cfg    Config
	launch Launch
	clock  console.Clock
	log    *slog.Logger
	notify *platform.Notifier
	spawn  SpawnFunc

	ring    *console.Ring
	logFile *console.LogFile
	ingest  *console.Ingest

	// steps is the shutdown ladder, rendered at construction. Rendering it
	// here means a manifest whose ladder cannot be built refuses to START the
	// server, rather than refusing to stop one that is already running.
	steps []PlanStep

	// extendedUsec totals every EXTEND_TIMEOUT_USEC this runner has asked
	// systemd for, across readiness and the shutdown. It is shared by both, so
	// one report answers "why did systemd wait eleven minutes for this unit".
	extendedUsec atomic.Int64
	// stopping is the one-ladder-at-a-time guarantee.
	stopping atomic.Bool
	// stopCh is closed the first time a shutdown ladder starts, so a readiness
	// wait can select on it. A stop is a legal transition out of `booting` and
	// nothing else on that path could learn about one: the ladder runs on the
	// socket connection's goroutine, and the stall clock only advances on GAME
	// bytes, so the boot would idle to stall_timeout on a world somebody had
	// already stopped.
	stopOnce sync.Once
	stopCh   chan struct{}

	mu         sync.Mutex
	state      State
	stateSince time.Time
	proc       Process
	io         *Spawned
	identity   Identity
	startedAt  time.Time
	readyAt    time.Time
	exit       ExitStatus
	exited     bool
	pumpDone   chan struct{}
	pumpErr    error
	control    control.Channel
	onStep     func(rung, send string)
	connsFn    func() int
	players    *PlayersStatus
	lastProbe  string
	rung       Rung
	degraded   bool
	// stopDone is closed by the ladder that won the stopping CAS, AFTER its
	// report has been written. stopReport is that report. Together they are
	// the barrier AwaitStop blocks on: without one, `gvm run`'s teardown and a
	// socket-initiated stop are two goroutines with nothing between them.
	stopDone   chan struct{}
	stopReport *Report

	// sendMu serialises injected lines. Two writers each doing one Write can
	// still interleave at the fd if they are not ordered, and half a command
	// followed by another command's tail is a command nobody typed.
	sendMu sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

// New builds a runner from cfg. It resolves the launch and opens the console
// (ring, log file, ingest); it does not touch the game.
func New(cfg Config) (*Runner, error) {
	switch {
	case cfg.Instance == "":
		return nil, errors.New("runner: New needs an instance name")
	case cfg.Manifest == nil:
		return nil, errors.New("runner: New needs a manifest")
	case cfg.Renderer == nil:
		return nil, errors.New("runner: New needs a renderer")
	}
	base := cfg.BaseEnv
	if base == nil {
		base = os.Environ()
	}
	l, err := BuildLaunch(cfg.Manifest, cfg.Renderer, base)
	if err != nil {
		return nil, err
	}
	steps, err := BuildPlan(cfg.Manifest, cfg.Renderer)
	if err != nil {
		return nil, err
	}

	clock := cfg.Clock
	if clock == nil {
		clock = console.SystemClock{}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	sp := cfg.Spawn
	if sp == nil {
		sp = spawn
	}

	r := &Runner{
		cfg:        cfg,
		launch:     l,
		clock:      clock,
		log:        logger,
		notify:     cfg.Notify,
		spawn:      sp,
		steps:      steps,
		state:      StatePreflight,
		stateSince: clock.Now(),
	}
	r.ring = console.NewRing(l.Console.RingBytes, 0).WithClock(clock.Now)

	if l.Console.Capturing() && cfg.Paths.ConsoleLog != "" {
		lf, err := console.OpenLog(console.LogOptions{
			Path:    cfg.Paths.ConsoleLog,
			Clock:   clock,
			OnError: func(err error) { logger.Error("console log", "err", err) },
		})
		if err != nil {
			return nil, err
		}
		r.logFile = lf
	}
	r.ingest = console.NewIngest(console.IngestOptions{
		Ring:      r.ring,
		Raw:       rawSink(r.logFile),
		Clock:     clock,
		StripANSI: l.Console.StripANSI,
	})
	return r, nil
}

// rawSink returns f as an io.Writer, or nil when there is no log file.
// Returning a typed nil pointer as an interface would give the ingest a
// non-nil Writer that panics on the first byte of game output.
func rawSink(f *console.LogFile) io.Writer {
	if f == nil {
		return nil
	}
	return f
}

// Instance is the instance name, for the socket's hello frame.
func (r *Runner) Instance() string { return r.cfg.Instance }

// Build is the gvm build id, for the socket's hello frame.
func (r *Runner) Build() string { return r.cfg.Build }

// Ring is the console ring buffer every reader subscribes to.
func (r *Runner) Ring() *console.Ring { return r.ring }

// Launch is the resolved command. Callers print Launch.Display(), never the
// fields, because argv can carry a secret.
func (r *Runner) Launch() Launch { return r.launch }

// Console is the resolved console configuration.
func (r *Runner) Console() ConsoleConfig { return r.launch.Console }

// State is the current lifecycle state.
func (r *Runner) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// StateSince is when the runner entered its current state.
func (r *Runner) StateSince() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateSince
}

// Identity is the pinned (pid, pgid, start_ticks) of the spawned process, zero
// before Spawn.
func (r *Runner) Identity() Identity {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.identity
}

// Process is the spawned game, nil before Spawn.
func (r *Runner) Process() Process {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.proc
}

// StartedAt is when the child was spawned.
func (r *Runner) StartedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startedAt
}

// SetState moves the runner to s and reports whether it moved.
//
// An illegal transition is refused and logged rather than panicking. A
// supervisor that aborts on a state bug takes a live world down with it; one
// that records the bug in the console an operator is already reading does not.
func (r *Runner) SetState(s State) bool {
	r.mu.Lock()
	from := r.state
	if from == s {
		r.mu.Unlock()
		return false
	}
	if !from.CanMove(s) {
		r.mu.Unlock()
		r.log.Error("refused an illegal state transition", "from", from, "to", s)
		r.Note("-- gvm: refused an illegal state transition %s -> %s", from, s)
		return false
	}
	now := r.clock.Now()
	r.state, r.stateSince = s, now
	if s == StateReady && r.readyAt.IsZero() {
		r.readyAt = now
	}
	r.mu.Unlock()

	r.log.Info("state", "from", from, "to", s)
	r.Note("-- gvm: %s -> %s", from, s)
	r.NotifyStatus(string(s))
	r.WriteStatus()
	return true
}

// ReadyAt is when the runner first reached `ready`, zero before that.
func (r *Runner) ReadyAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readyAt
}

// NotifyStatus sends an sd_notify STATUS= line. It is a no-op outside systemd.
//
// The name is not `Status` because that one belongs to the console socket's
// Backend, which wants this instance's status.json as bytes. Two very
// different answers to "what is the status", and conflating them would put a
// human sentence where a dashboard expects a document.
func (r *Runner) NotifyStatus(msg string) {
	if r.notify == nil {
		return
	}
	if err := r.notify.Status(msg); err != nil {
		r.log.Warn("sd_notify", "err", err)
	}
}

// Note appends one runner annotation to the console.
//
// It also goes to the log file. Section 11.3 gives the log file the pump's raw
// bytes, and this is the one deliberate addition to that stream: a console.log
// that does not say "gvm sent save" is missing the line that explains the next
// forty seconds of silence. The cost is that an annotation can land inside an
// unterminated game line on disk; the ring, which is what the report and the
// dashboard read, keeps them properly separated.
func (r *Runner) Note(format string, a ...any) uint64 {
	text := fmt.Sprintf(format, a...)
	if strings.ContainsAny(text, "\r\n\x00") {
		// Not everything reaching here is gvm's own words. A `stop` reason
		// arrives as free text on a socket frame, and errors interpolated with
		// %v can be multi-line. The ring's SourceRunner tag is the only thing
		// that distinguishes a gvm annotation from game output, so an embedded
		// newline manufactures annotations nothing else can forge -- and a
		// second physical line in console.log, which is the record of who
		// asked for what. Quoted rather than dropped or blanked: the reader
		// still needs to see what was sent. There is deliberately no LENGTH
		// rule here; `-- gvm: exec <argv>` is long and is the most useful line
		// in the log.
		text = strconv.Quote(text)
	}
	seq := r.ring.Append(console.Line{Source: console.SourceRunner, Text: text})
	if r.logFile != nil {
		if _, err := r.logFile.Write([]byte(text + "\n")); err != nil {
			r.log.Warn("console log", "err", err)
		}
	}
	return seq
}

// Spawn starts the game.
//
// Its error contract matters: a non-nil error does NOT mean nothing is
// running. Spawn can fail after exec -- writing the identity pin is the case
// that does -- and in that case Process() is set and the caller must run the
// shutdown ladder rather than exiting and orphaning a live world. This
// function never signals the game; exactly one function in gvm does.
func (r *Runner) Spawn() error {
	r.mu.Lock()
	if r.proc != nil {
		r.mu.Unlock()
		return ErrAlreadySpawned
	}
	r.mu.Unlock()

	if !r.SetState(StateSpawning) {
		return fmt.Errorf("runner: cannot spawn from state %s", r.State())
	}
	// Annotated before the exec, so a game that dies in its first millisecond
	// still leaves the command that started it at the top of the console.
	r.Note("-- gvm: exec %s", r.launch.Display())
	if r.launch.Dir != "" {
		r.Note("-- gvm: cwd %s", r.launch.Dir)
	}

	sp, err := r.spawn(r.launch)
	if err != nil {
		r.Note("-- gvm: spawn failed: %v", err)
		r.SetState(StateFailed)
		return err
	}

	id := sp.Process.Identity()
	now := r.clock.Now()
	r.mu.Lock()
	r.proc, r.io, r.identity, r.startedAt = sp.Process, sp, id, now
	if sp.Out != nil {
		r.pumpDone = make(chan struct{})
	}
	r.mu.Unlock()

	r.Note("-- gvm: started %s (console %s)", id, r.launch.Console.Mode)

	if err := r.writeRunnerFile(now); err != nil {
		// The pin is what the NEXT start reads to find out whether this world
		// already has a live server on it. Without it an orphan is invisible,
		// so this is fatal -- but the child is already running and stays in
		// Process() for the caller's ladder.
		r.Note("-- gvm: cannot write the identity pin: %v", err)
		r.SetState(StateFailed)
		return fmt.Errorf("runner: writing %s: %w", r.cfg.Paths.RunnerJSON, err)
	}
	if err := r.writePlanFile(); err != nil {
		// Section 11.8 layer 3 is the ladder that runs when this process is
		// gone. Without the file there is no such ladder, so this is fatal for
		// the same reason the identity pin is -- and, identically, the child
		// is already running and stays in Process() for the caller's ladder.
		r.Note("-- gvm: cannot write the standalone shutdown plan: %v", err)
		r.SetState(StateFailed)
		return fmt.Errorf("runner: writing %s: %w", r.cfg.Paths.ShutdownJSON, err)
	}

	if sp.Out != nil {
		go r.pumpLoop(sp.Out)
	}
	r.SetState(StateBooting)
	return nil
}

// pumpLoop drains the child's output until the stream ends.
func (r *Runner) pumpLoop(src io.Reader) {
	r.mu.Lock()
	done := r.pumpDone
	r.mu.Unlock()

	// Registered first so it runs LAST: a panic is recorded in the console
	// before anything waiting on the pump is released.
	defer close(done)
	defer r.recoverInto("console pump")

	err := pump(src, r.ingest)

	r.mu.Lock()
	r.pumpErr = err
	r.mu.Unlock()

	if err != nil {
		r.log.Error("console pump", "err", err)
		r.Note("-- gvm: console pump stopped: %v", err)
		return
	}
	if rawErr := r.ingest.RawErr(); rawErr != nil {
		r.log.Error("console log", "err", rawErr)
	}
	r.Note("-- gvm: console stream ended")
}

// PumpErr reports why the pump stopped, nil for a normal end of stream.
func (r *Runner) PumpErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pumpErr
}

// Send writes one line towards the game and echoes it into the ring.
//
// It is the sink internal/control's stdin channel wraps, so the gate (is the
// server accepting input?) and the health accounting live there and the
// mechanics live here. The echo is what makes the console log the record of
// who typed what (section 11.4).
func (r *Runner) Send(actor, line string) (uint64, error) {
	if err := console.ValidateSendLine(line); err != nil {
		return 0, err
	}
	r.mu.Lock()
	w := r.io
	r.mu.Unlock()
	if w == nil || w.In == nil {
		return 0, ErrNoConsole
	}

	r.sendMu.Lock()
	// One Write for the line and its terminator: two writes can interleave
	// with another writer's line on the same descriptor.
	_, err := io.WriteString(w.In, line+r.launch.Console.LineEnding)
	r.sendMu.Unlock()
	if err != nil {
		// No echo on a failed write. A console log showing a command that
		// never reached the game is worse than one that shows nothing.
		return 0, err
	}
	if !r.launch.Console.EchoInput {
		// Zero says "no durable echo exists", which is what the socket reports
		// back and what the shutdown report records.
		return 0, nil
	}
	return r.ring.Append(console.Line{Source: console.SourceActor, Actor: actor, Text: line}), nil
}

// Wait blocks until the child is reaped, or until ctx ends. A ctx that ends
// does not touch the child.
func (r *Runner) Wait(ctx context.Context) (ExitStatus, error) {
	r.mu.Lock()
	p := r.proc
	r.mu.Unlock()
	if p == nil {
		return ExitStatus{}, ErrNoConsole
	}
	st, err := p.Wait(ctx)
	if err != nil {
		return st, err
	}
	r.mu.Lock()
	r.exit, r.exited = st, true
	r.mu.Unlock()
	r.Note("-- gvm: game process %s", st)
	return st, nil
}

// Exit reports the child's exit status and whether it has been reaped.
func (r *Runner) Exit() (ExitStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exit, r.exited
}

// Close releases everything the runner owns, in the one order that is safe.
//
// The pump is drained FIRST, because closing a pty master out from under a
// blocked read is a race against descriptor reuse. Then the child's ends, then
// the ingest and the log file, and the ring last: closing the ring is how an
// attached `gvm console` learns the runner is gone instead of hanging.
//
// It does not stop the game. That is the ladder's job, and Close is called
// after it.
func (r *Runner) Close() error {
	r.closeOnce.Do(func() {
		var errs []error
		if !r.drainPump() {
			r.log.Warn("console pump did not finish", "budget", pumpDrainBudget)
		}
		r.mu.Lock()
		sp := r.io
		r.io = nil
		r.mu.Unlock()
		if sp != nil && sp.Close != nil {
			if err := sp.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if r.ingest != nil {
			if err := r.ingest.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if r.logFile != nil {
			if err := r.logFile.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		r.ring.Close()
		r.closeErr = errors.Join(errs...)
	})
	return r.closeErr
}

// drainPump waits for the pump goroutine, reporting whether it finished within
// the budget.
//
// This is the one timer in the package that deliberately does NOT come from the
// injected clock. It is a watchdog, and a watchdog wired to a clock the caller
// controls cannot bark: under a fake clock it would never fire, so a pump that
// hung would hang the runner -- and hang the test that was supposed to find
// out. It costs nothing in the normal case, where the timer is stopped
// microseconds after it is armed.
func (r *Runner) drainPump() bool {
	r.mu.Lock()
	done := r.pumpDone
	r.mu.Unlock()
	if done == nil {
		return true
	}
	t := time.NewTimer(pumpDrainBudget)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// writeRunnerFile pins this run's identity where the next start will find it.
func (r *Runner) writeRunnerFile(now time.Time) error {
	if r.cfg.Paths.RunnerJSON == "" {
		return nil
	}
	sha := ""
	if r.cfg.Manifest != nil {
		sha = r.cfg.Manifest.SHA256
	}
	return WriteRunnerFile(r.cfg.Paths.RunnerJSON, RunnerFile{
		Instance:    r.cfg.Instance,
		Game:        r.cfg.Game,
		Unit:        r.cfg.Unit,
		Process:     r.identity,
		StartedAt:   now.UTC().Format(time.RFC3339),
		ConsoleMode: r.launch.Console.Mode,
		ConsoleLog:  r.cfg.Paths.ConsoleLog,
		ManifestSHA: sha,
	})
}

// recoverInto is section 11.8 layer 1: every goroutine boundary in this
// package carries one, and the panic goes into the ring as well as the journal
// so it appears in the console the operator is already watching.
func (r *Runner) recoverInto(what string) {
	rec := recover()
	if rec == nil {
		return
	}
	// The path to the ring may itself panic if the panic corrupted something;
	// losing the annotation is acceptable, losing the process is not.
	defer func() { _ = recover() }()

	msg := fmt.Sprintf("-- gvm: %s panicked: %v", what, rec)
	r.log.Error(msg, "stack", string(debug.Stack()))
	r.ring.Append(console.Line{Source: console.SourceRunner, Text: msg})
}

// writeFileAtomic writes b to p through a temporary file in the same
// directory. Same directory, because rename(2) is only atomic within one
// filesystem, and /run and /var are not the same filesystem.
func writeFileAtomic(p string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(p)
	f, err := os.CreateTemp(dir, "."+filepath.Base(p)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename

	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	// Chmod before rename: the file must never be visible at its real name
	// with the default 0600 of CreateTemp, because a reader that opens it in
	// that window gets a permission error it will not retry.
	if err := f.Chmod(perm); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		f.Close()
		return err
	}
	// Synced because the next boot reads this file to decide whether a world
	// already has a supervisor, and that answer must survive a power cut.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
