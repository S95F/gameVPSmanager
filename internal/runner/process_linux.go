//go:build linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// waitExitPoll is section 11.6 rule 4's polling interval for `via: wait_exit`.
// It polls the PINNED identity rather than the unit's MainPID, which for a
// game launched through a script is the shell and not the engine.
const waitExitPoll = 250 * time.Millisecond

// procBase is everything a Process can answer from /proc and kill(2), shared
// by the process gvm spawned and one it merely adopted.
type procBase struct {
	id   Identity
	self int
	// cgroup is the unit cgroup this runner may sweep, and cgroupErr is why it
	// may not. Resolved once at construction: the answer cannot change for the
	// life of the process, and re-deriving it inside the last rung of a
	// shutdown is not the moment to discover /proc is unreadable.
	cgroup    string
	cgroupErr error

	// exempt holds the processes gvm itself put in the cgroup. It is a POINTER
	// because procBase is returned by value from newProcBase, and because it
	// is filled after construction -- the screen bridge starts once readiness
	// is reached -- and read by the shutdown ladder on another goroutine.
	exempt *exemptSet
}

// exemptSet is the mutable half of procBase, held behind a pointer so the
// struct itself stays copyable.
type exemptSet struct {
	mu  sync.Mutex
	ids []Identity
}

func (e *exemptSet) all() []Identity {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Identity(nil), e.ids...)
}

func (e *exemptSet) add(ids []Identity) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, ids...)
}

func (p *procBase) PID() int           { return p.id.PID }
func (p *procBase) PGID() int          { return p.id.PGID }
func (p *procBase) Identity() Identity { return p.id }

// Alive reports whether the pinned identity is still a running process.
//
// Three answers that are all "no" and are easy to get wrong:
//
//   - The pid does not exist. Dead, and not an error.
//   - The pid exists with a different start time. That is a RECYCLED pid: some
//     unrelated process now owns the number, and reporting it alive is how a
//     ladder ends up sending SIGKILL to a backup job.
//   - The pid exists with our start time and state Z. A zombie has already
//     exited; only its exit status is left. Reporting it alive would make
//     `wait_exit` spin until the reaper caught up, and on the standalone path
//     -- where nobody is going to reap it -- forever.
func (p *procBase) Alive() (bool, error) {
	if !p.id.Valid() {
		return false, fmt.Errorf("runner: no process identity is pinned")
	}
	st, err := platform.ReadStat(p.id.PID)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		// Not "dead". A caller that reads an unreadable /proc as an empty
		// world starts a second server on a live one.
		return false, err
	case st.StartTicks != p.id.StartTicks:
		return false, nil
	}
	return st.State != "Z", nil
}

// Signal delivers sig at scope, refusing to signal anything it cannot first
// prove is still the game.
func (p *procBase) Signal(sig platform.Signal, scope Scope) error {
	if !p.id.Valid() {
		return fmt.Errorf("runner: refusing to signal: no process identity is pinned")
	}
	switch scope {
	case ScopeProcess:
		alive, err := p.Alive()
		if err != nil {
			return err
		}
		if !alive {
			return ErrGone
		}
		return platform.SignalProcess(p.id.PID, sig)

	case ScopeGroup:
		live, err := p.groupLive()
		if err != nil {
			return err
		}
		if !live {
			return ErrGone
		}
		return platform.SignalGroup(p.id.PGID, sig)

	case ScopeCgroup:
		pids, err := p.remaining()
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return ErrGone
		}
		var errs []error
		for _, pid := range pids {
			if err := platform.SignalProcess(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
				// ESRCH is the pid exiting between the read and the kill,
				// which is the outcome the sweep wanted anyway.
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	return fmt.Errorf("runner: %v is not a signal scope", scope)
}

// osProcess is a process this runner spawned and is therefore the parent of.
type osProcess struct {
	procBase
	cmd  *exec.Cmd
	done chan struct{}
	st   ExitStatus
	err  error
}

var _ Process = (*osProcess)(nil)

// newOSProcess pins a started command and begins reaping it.
//
// Reaping starts immediately rather than on the first Wait, because a child
// nobody has waited for stays a zombie -- and a zombie keeps its pid, which
// keeps /proc/<pid>/stat readable, which makes every liveness check answer
// "still there" for a process that exited an hour ago.
func newOSProcess(cmd *exec.Cmd, id Identity) *osProcess {
	p := &osProcess{procBase: newProcBase(id), cmd: cmd, done: make(chan struct{})}
	go p.reap()
	return p
}

func (p *osProcess) reap() {
	defer close(p.done)
	err := p.cmd.Wait()
	p.st, p.err = exitStatusOf(p.cmd.ProcessState, err)
}

// Wait blocks until the child is reaped or ctx ends. Ending the context does
// not touch the child: the ladder is the only thing that signals a game.
func (p *osProcess) Wait(ctx context.Context) (ExitStatus, error) {
	select {
	case <-p.done:
		return p.st, p.err
	case <-ctx.Done():
		return ExitStatus{}, ctx.Err()
	}
}

// exitStatusOf decodes a finished command.
//
// An *exec.ExitError is the normal case -- the child returned non-zero or was
// signalled -- and is not an error to the caller: a server that exits 1 has
// still exited, and the shutdown report wants the number. Anything else (wait
// failing outright) is a real failure with no status to report.
func exitStatusOf(ps *os.ProcessState, err error) (ExitStatus, error) {
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return ExitStatus{Code: -1}, err
	}
	if ps == nil {
		return ExitStatus{Code: -1}, fmt.Errorf("runner: the child produced no exit status")
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ExitStatus{Code: -1, Signal: platform.Signal(ws.Signal()), Signaled: true}, nil
	}
	return ExitStatus{Code: ps.ExitCode()}, nil
}

// adoptedProcess is a pinned process this runner did NOT spawn: the standalone
// shutdown path of section 11.8, which is handed nothing but the identity
// written to shutdown.json.
type adoptedProcess struct {
	procBase
}

var _ Process = (*adoptedProcess)(nil)

// Adopt returns a Process for an identity read from disk.
//
// The identity is verified up front: adopting a pid whose start time no longer
// matches would hand the caller a Process that signals a stranger, and the
// caller is by definition running a shutdown ladder.
func Adopt(id Identity) (Process, error) {
	p := &adoptedProcess{procBase: newProcBase(id)}
	alive, err := p.Alive()
	if err != nil {
		return nil, err
	}
	if !alive {
		return nil, fmt.Errorf("%w: %s", ErrGone, id)
	}
	return p, nil
}

// Wait polls the pin, because this process is not our child and waitpid(2)
// will not tell us anything about it. The exit status is genuinely unknown --
// only a parent learns that -- and is reported as -1 rather than invented.
func (p *adoptedProcess) Wait(ctx context.Context) (ExitStatus, error) {
	t := time.NewTicker(waitExitPoll)
	defer t.Stop()
	for {
		alive, err := p.Alive()
		if err != nil {
			return ExitStatus{Code: -1}, err
		}
		if !alive {
			return ExitStatus{Code: -1}, nil
		}
		select {
		case <-ctx.Done():
			return ExitStatus{Code: -1}, ctx.Err()
		case <-t.C:
		}
	}
}
