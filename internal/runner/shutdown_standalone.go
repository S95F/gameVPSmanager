package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/control/stdinctl"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// This is section 11.8 layer 3: a second, independent code path that can stop
// a game when the runner that started it is gone.
//
// Layers 1 and 2 (recover at every goroutine boundary, recover in main and run
// the ladder anyway) keep a panicking runner from taking a world with it. They
// cannot help when the runner was SIGKILLed -- an OOM kill, an operator with
// `kill -9`, a kernel panic on the host's other half. Then the game is
// orphaned in the cgroup with nobody waiting for it, and the only thing that
// knows how to stop it correctly is a file.
//
// `gvm run-shutdown <name>` reads /run/gvm/<name>/shutdown.json and executes
// the ladder with NO ring, NO socket, NO manifest loading and no spec
// rendering. A few hundred lines that gvmd or a human can invoke. It is worse
// than the real path in exactly two ways, both recorded in the report rather
// than hidden: `via: console` writes into /proc/<pid>/fd/0 with no echo, and
// `wait_for_log` has no ring to watch so it degrades to a plain wait.

// StandaloneOptions configures one run of the standalone ladder.
//
// Instance, ShutdownLog and LastShutdown come from the CALLER, not from the
// plan. /run/gvm/<name> is owned by the instance user and the runner that
// writes shutdown.json runs as that same uid, so a compromised game can unlink
// and replace the file whatever its own mode says -- and this path executes it
// as root. Every path root writes to therefore has to be derived from the
// instance name the CLI validated.
type StandaloneOptions struct {
	// PlanPath is /run/gvm/<name>/shutdown.json.
	PlanPath string
	// Instance is the name the caller asked to stop. A plan naming a different
	// instance is refused. Empty disables the trust checks, for a caller that
	// wrote the plan itself.
	Instance string
	// ShutdownLog and LastShutdown are where the report goes. They fall back
	// to the plan's own only when Instance is empty.
	ShutdownLog  string
	LastShutdown string

	Actor  string
	Reason string

	Clock console.Clock
	Log   logger

	// Control is the optional control channel. `gvm run-shutdown` normally has
	// none -- dialling RCON needs the instance's secret, which this path
	// deliberately does not load -- and every `via: control` rung is then
	// recorded as skipped and the ladder falls through to the console and the
	// signals, which is what the manifests are written to survive.
	Control ControlChannel

	// Adopt is the process seam. nil uses the platform's.
	Adopt func(Identity) (Process, error)
	// ProcCgroup reads a live process's cgroup, for proving the pin belongs to
	// this instance's unit. nil uses the platform's.
	ProcCgroup func(pid int) (string, error)
	// OpenConsole opens the game's stdin. nil uses os.OpenFile.
	OpenConsole func(path string) (io.WriteCloser, error)
	// Save writes the report. nil uses the paths in the plan.
	Save func(rep *Report) error
}

// ErrNoPlan means there is no rendered ladder to run.
var ErrNoPlan = errors.New("runner: no shutdown plan for this instance")

// RunShutdown executes the standalone ladder and BLOCKS until the game's
// process group is empty.
//
// It returns the report even when it also returns an error, whenever the
// ladder actually ran: the caller is owed the record of what was attempted,
// and an error that swallowed it would leave the operator with less
// information than the crashed runner did.
func RunShutdown(ctx context.Context, o StandaloneOptions) (*Report, error) {
	if o.PlanPath == "" {
		return nil, ErrNoPlan
	}
	pl, err := ReadPlan(o.PlanPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s does not exist, so this instance was never started by a gvm runner", ErrNoPlan, o.PlanPath)
		}
		return nil, err
	}
	if !pl.Process.Valid() {
		return nil, fmt.Errorf("%w: %s pins no usable process identity", ErrNoPlan, o.PlanPath)
	}
	if o.Instance != "" && pl.Instance != o.Instance {
		return nil, fmt.Errorf("%w: %s names instance %q, not %q",
			ErrNoPlan, o.PlanPath, pl.Instance, o.Instance)
	}
	// The terminator is appended VERBATIM after the line itself has been
	// validated, and successive steps share one file offset, so an arbitrary
	// terminator is arbitrary multi-line content written into whatever the
	// console descriptor names. Only the three a manifest can express are
	// accepted; the bytes stay on the wire so this path still loads no
	// manifest.
	if err := checkLineEnding(pl.LineEnding); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNoPlan, o.PlanPath, err)
	}

	adopt := o.Adopt
	if adopt == nil {
		adopt = Adopt
	}
	proc, err := adopt(pl.Process)
	if err != nil {
		if errors.Is(err, ErrGone) {
			// The pin is stale: the game exited, or its pid was recycled. Both
			// are "nothing to stop", and both must be a clean answer rather
			// than an error a caller might respond to by escalating.
			return goneReport(o, pl), nil
		}
		return nil, err
	}

	if o.Instance != "" {
		// The pin is a number in an attacker-writable file, and Adopt only
		// proves that number is alive: /proc/<pid>/stat is world-readable, so
		// any live pid can be pinned and the signal rungs would deliver
		// SIGTERM and then SIGKILL to that pgid as root. Requiring the pinned
		// process to sit in this instance's own unit cgroup is what bounds
		// the primitive to the game.
		procCgroup := o.ProcCgroup
		if procCgroup == nil {
			procCgroup = platform.ProcCgroup
		}
		cg, cgErr := procCgroup(pl.Process.PID)
		if cgErr != nil {
			cg = ""
		}
		if err := checkPinnedUnit(cg, instance.UnitName(o.Instance)); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrNoPlan, o.PlanPath, err)
		}
	}

	sender, closeConsole, err := standaloneConsole(pl, o.OpenConsole)
	if err != nil && o.Log != nil {
		// Not fatal. A ladder with no console still has its signal rungs, and
		// section 11.8 says so plainly: the step is recorded as skipped and
		// the ladder escalates. Honest, and better than nothing.
		o.Log.Warn("run-shutdown: no console for this instance: " + err.Error())
	}
	if closeConsole != nil {
		defer closeConsole()
	}

	var extended atomic.Int64
	l := &Ladder{
		Plan:    pl,
		Proc:    proc,
		Console: sender,
		Control: o.Control,
		Clock:   o.Clock,
		Log:     o.Log,
		// No Notify: this process is not the unit's main process, so an
		// sd_notify from it would be attributed to whatever unit invoked it --
		// and STOPPING=1 against gvmd's own unit is a way to stop gvmd.
		Extended: &extended,
	}
	// The ladder runs on a context the caller's cancellation cannot reach, for
	// the same reason `gvm run` does it: run-shutdown is dispatched with
	// main's signal.NotifyContext, and the operator or gvmd that invoked it may
	// well be taking the same SIGTERM. A cancelled context aborts the save on
	// the ladder's first select and leaves the exit watcher unable to ever
	// report the exit it is watching for. The WithCancel wrapper is what still
	// ends the watcher goroutines when this function returns.
	runCtx, endRun := context.WithCancel(context.WithoutCancel(ctx))
	defer endRun()

	rep := l.Run(runCtx, actorOr(o.Actor), reasonOr(o.Reason))
	rep.Standalone = true

	if err := reportSaver(o, pl)(rep); err != nil {
		return rep, fmt.Errorf("runner: the game was stopped but the report could not be written: %w", err)
	}
	return rep, nil
}

// reportSaver resolves where the report goes. The CALLER's paths win:
// AppendReport MkdirAlls and O_CREATEs whatever it is handed, which as root and
// from a game-user-writable plan is an append to any path on the box.
func reportSaver(o StandaloneOptions, pl Plan) func(*Report) error {
	if o.Save != nil {
		return o.Save
	}
	log, last := o.ShutdownLog, o.LastShutdown
	if o.Instance == "" {
		// No caller-supplied identity means nothing to derive paths from, and
		// nothing but the caller wrote this plan.
		log, last = pl.ShutdownLog, pl.LastShutdown
	}
	return func(rep *Report) error { return rep.Save(log, last) }
}

// checkLineEnding accepts only the three terminators a manifest can name.
func checkLineEnding(le string) error {
	switch le {
	case "", "\n", "\r", "\r\n":
		return nil
	}
	return fmt.Errorf("line_ending %q is not one of lf, cr or crlf", le)
}

// checkPinnedUnit reports whether a cgroup path belongs to unit.
//
// It is split from the /proc read so the comparison runs and is tested on the
// development machine. The match is on a whole path SEGMENT: a unit named
// not-gvm@pz-main.service ends with the same characters and is a different
// service.
func checkPinnedUnit(cgroup, unit string) error {
	if cgroup == "" {
		return fmt.Errorf("the pinned process's cgroup could not be read, so there is nothing "+
			"to prove it is %s's game rather than another process that happens to hold that pid", unit)
	}
	for _, seg := range strings.Split(cgroup, "/") {
		if seg == unit {
			return nil
		}
	}
	return fmt.Errorf("the pinned process is in %s, not in %s; the plan file is writable by the "+
		"instance user and this path signals as root", cgroup, unit)
}

// standaloneConsole opens the game's own stdin as a console sender.
//
// Two properties of /proc/<pid>/fd/0 make this safe and both are worth stating
// once. Opening it opens the same underlying file the game has open -- the pty
// slave, or the pipe -- so what is written arrives at the game's stdin. And
// closing this copy does NOT send the game EOF, because the game holds its own
// descriptor; closing a server's stdin is how a FIFO supervisor kills a game
// by accident, and this path must never do that.
func standaloneConsole(pl Plan, open func(string) (io.WriteCloser, error)) (ConsoleSender, func(), error) {
	if pl.ConsoleFD == "" {
		return nil, nil, errors.New("the plan records no console descriptor")
	}
	if open == nil {
		open = func(p string) (io.WriteCloser, error) {
			return os.OpenFile(p, os.O_WRONLY, 0)
		}
	}
	// The field is read as a BOOLEAN -- this game was console-captured -- and
	// the path is rebuilt from the pin Adopt has just verified. Opening the
	// path the plan names would let a game-user-writable file choose which file
	// root writes into, and what gets written is the plan's own send lines.
	fd := fmt.Sprintf("/proc/%d/fd/0", pl.Process.PID)
	f, err := open(fd)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", fd, err)
	}
	le := pl.LineEnding
	if le == "" {
		le = "\n"
	}
	// stdinctl.WriterSender applies the same validation and the same
	// one-Write-per-command rule the real path uses, and returns Seq 0 -- which
	// is information, not a placeholder: it says no durable echo exists, so
	// the step is recorded honestly as degraded.
	return senderAdapter{stdinctl.WriterSender(f, le)}, func() { _ = f.Close() }, nil
}

// senderAdapter narrows a control.Sender to the ladder's ConsoleSender. The
// two have the same method; the adapter exists so the ladder does not have to
// import the control package to name its own dependency.
type senderAdapter struct{ s control.Sender }

func (a senderAdapter) Send(actor, line string) (uint64, error) { return a.s.Send(actor, line) }

// goneReport is the record for an instance whose pinned process has already
// exited. It is written like any other report, because "there was nothing to
// stop" is an outcome gvmd should be able to see rather than infer from
// silence.
func goneReport(o StandaloneOptions, pl Plan) *Report {
	clk := o.Clock
	if clk == nil {
		clk = console.SystemClock{}
	}
	now := clk.Now()
	rep := &Report{
		Proto:      ReportProto,
		Instance:   pl.Instance,
		Game:       pl.Game,
		Unit:       pl.Unit,
		Reason:     reasonOr(o.Reason),
		Actor:      actorOr(o.Actor),
		StartedAt:  now,
		FinishedAt: now,
		FinalRung:  RungNone,
		ExitCode:   -1,
		Exited:     true,
		Standalone: true,
		Err:        "the pinned process was already gone",
	}
	save := o.Save
	if save == nil {
		save = func(rep *Report) error { return rep.Save(pl.ShutdownLog, pl.LastShutdown) }
	}
	_ = save(rep)
	return rep
}

func actorOr(a string) string {
	if a == "" {
		return "run-shutdown"
	}
	return a
}

func reasonOr(r string) string {
	if r == "" {
		return "gvm run-shutdown"
	}
	return r
}
