//go:build linux

package runner

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// spawn is the platform SpawnFunc: the only place in gvm that starts a game.
func spawn(l Launch) (*Spawned, error) {
	switch l.Console.Mode {
	case spec.ConsoleModePTY:
		return spawnPTY(l)
	case spec.ConsoleModePipe:
		return spawnPipe(l)
	case spec.ConsoleModeNone:
		return spawnNoConsole(l)
	}
	return nil, fmt.Errorf("runner: console mode %q cannot be spawned", l.Console.Mode)
}

// command builds the child.
//
// It is a struct literal rather than exec.Command because exec.Command
// consults PATH for a name with no separator, and a manifest that could name
// `java` would then start whichever java an environment variable pointed at.
// Launch.Path is already absolute (see resolveExec) and Args is already a
// list; nothing here parses, splits or quotes anything.
//
// SysProcAttr carries exactly one field, and the two that are absent are as
// deliberate as the one that is present:
//
//   - Setsid: true is the fix for the whole class of bug the bash suite has.
//     The child becomes a session leader, so its pgid equals its pid, and
//     kill(-pgid) reaches both a launcher script and the engine it forked
//     without exec. Signalling the pid alone reaches only the shell.
//   - Setctty is NOT set. Giving the child the pty as its controlling terminal
//     would make the kernel deliver SIGHUP to it the moment the master closes
//     -- so a runner that crashed would take a live, unsaved world down with
//     it, exactly when section 11.8's recover-and-run-the-ladder needs the
//     game still there.
//   - Pdeathsig is NOT set for the same reason, stated more directly: it is a
//     switch labelled "kill the world if the supervisor dies".
func command(l Launch) *exec.Cmd {
	return &exec.Cmd{
		Path:        l.Path,
		Args:        l.Argv(),
		Dir:         l.Dir,
		Env:         l.Env,
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}
}

func spawnPTY(l Launch) (*Spawned, error) {
	master, slave, err := openPTY(l.Console.Rows, l.Console.Cols)
	if err != nil {
		return nil, err
	}
	cmd := command(l)
	// One descriptor for all three: stdout and stderr on a terminal are one
	// interleaved stream, which is what the console shows and what makes a
	// stack trace land between the log lines it belongs with.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave

	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		return nil, fmt.Errorf("runner: exec %s: %w", l.Path, err)
	}

	// TRAP: the parent must drop its own copy of the slave now. While any
	// process holds a slave descriptor open, the master never reports the
	// hangup -- so the pump would sit in a read forever after the game exited
	// and the runner would never learn that the game was gone.
	if err := slave.Close(); err != nil {
		return nil, abandon(cmd, master.Close, fmt.Errorf("runner: closing the parent's pty slave: %w", err))
	}
	return finish(cmd, master, master, master.Close)
}

func spawnPipe(l Launch) (*Spawned, error) {
	p, err := openPipes()
	if err != nil {
		return nil, err
	}
	cmd := command(l)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = p.inR, p.outW, p.outW

	if err := cmd.Start(); err != nil {
		p.closeAll()
		return nil, fmt.Errorf("runner: exec %s: %w", l.Path, err)
	}
	// The same trap in its pipe form: while the parent still holds the write
	// end, the read end never sees EOF.
	if err := p.closeChildEnds(); err != nil {
		return nil, abandon(cmd, p.closeParentEnds, fmt.Errorf("runner: closing the parent's pipe ends: %w", err))
	}
	return finish(cmd, p.out, p.in, p.closeParentEnds)
}

// spawnNoConsole is console mode `none`: the game gets /dev/null and gvm makes
// no claim to be able to read or write its console.
func spawnNoConsole(l Launch) (*Spawned, error) {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	cmd := command(l)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	if err := cmd.Start(); err != nil {
		null.Close()
		return nil, fmt.Errorf("runner: exec %s: %w", l.Path, err)
	}
	if err := null.Close(); err != nil {
		return nil, abandon(cmd, noClose, fmt.Errorf("runner: closing /dev/null: %w", err))
	}
	return finish(cmd, nil, nil, noClose)
}

func noClose() error { return nil }

// finish pins the started child's identity and assembles the Spawned.
func finish(cmd *exec.Cmd, out io.Reader, in io.Writer, closeFn func() error) (*Spawned, error) {
	id, err := pinIdentity(cmd.Process.Pid)
	if err != nil {
		return nil, abandon(cmd, closeFn, err)
	}
	// newOSProcess starts reaping, so it must come AFTER the pin: once the
	// child is reaped its /proc entry is gone and its start time is
	// unknowable, and an unpinned process is one gvm may never signal.
	return &Spawned{Process: newOSProcess(cmd, id), Out: out, In: in, Close: closeFn}, nil
}

// abandon kills a child gvm has started but cannot supervise, and returns the
// reason it could not.
//
// This is the one place other than the shutdown ladder that signals a game,
// and the justification is narrow: it runs microseconds after exec, before the
// process has opened a world file, and the alternative is worse -- a live
// server with no identity pinned, which no ladder can ever stop and which the
// next start will refuse to run beside.
func abandon(cmd *exec.Cmd, closeFn func() error, cause error) error {
	// The group, not the pid: Setsid already made the child a session leader,
	// so its pid is its pgid, and a launcher script may have forked already.
	if err := platform.SignalGroup(cmd.Process.Pid, platform.SIGKILL); err != nil {
		cause = fmt.Errorf("%w (and killing the started process failed: %w)", cause, err)
	}
	_ = cmd.Wait()
	_ = closeFn()
	return cause
}

// pinIdentity reads back the identity of the process that was just started.
//
// It is read from /proc rather than assumed, because the pgid is a fact about
// the kernel's view of the child and not about what SysProcAttr asked for: a
// mismatch means setsid did not happen, and every group signal for the rest of
// this server's life would go to the wrong group.
//
// It is safe to read here and nowhere later: the child has not been waited on
// yet, so even a process that exited immediately is still a zombie with a
// readable /proc entry and its true start time.
func pinIdentity(pid int) (Identity, error) {
	st, err := platform.ReadStat(pid)
	if err != nil {
		return Identity{}, fmt.Errorf("runner: cannot pin the identity of pid %d: %w", pid, err)
	}
	id := Identity{PID: st.PID, PGID: st.PGID, StartTicks: st.StartTicks}
	if !id.Valid() {
		return Identity{}, fmt.Errorf("runner: pid %d yielded an unusable identity (%s)", pid, id)
	}
	if id.PGID != id.PID {
		return Identity{}, fmt.Errorf("runner: pid %d is in process group %d, so it is not a session leader; "+
			"kill(-pgid) would signal the wrong group", id.PID, id.PGID)
	}
	return id, nil
}
