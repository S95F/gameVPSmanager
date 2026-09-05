package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

var (
	// ErrGone reports that the pinned process is no longer the process gvm
	// spawned: either it exited, or its pid was recycled and now belongs to
	// someone else.
	//
	// It is a sentinel rather than a plain error because the shutdown ladder
	// treats it as SUCCESS -- a rung that finds the game already gone has
	// achieved what it was asked to achieve -- while every other Signal error
	// is a rung that failed and has to escalate.
	ErrGone = errors.New("runner: the pinned process is gone")

	// ErrNoCgroup reports that this process cannot prove which cgroup the game
	// is in, so the cgroup rung and the cgroup emptiness check refuse to
	// answer.
	//
	// Refusing is the whole point. Outside a service cgroup, /proc/self/cgroup
	// names the cgroup of whatever started gvm -- an operator's login session,
	// which contains their shell, their editor and their ssh connection.
	// "SIGKILL everything left in the cgroup" against that set is a way to log
	// somebody out of their own box, so an unproven cgroup is an error and
	// never a pid list.
	ErrNoCgroup = errors.New("runner: not running in a service cgroup, so the cgroup is not gvm's to signal")
)

// Scope is how far a signal reaches.
//
// The default is the GROUP, not the process, and that default is the whole
// lesson of the bash suite: start-server.sh launches ProjectZomboid64 in a
// subshell without exec, so the pid gvm spawned is the shell and the pid that
// owns the world is a child of it. A SIGTERM delivered to the shell alone
// leaves the JVM writing to the world with nobody waiting for it.
type Scope uint8

const (
	// ScopeGroup is kill(-pgid): the launcher shell AND the engine it forked.
	ScopeGroup Scope = iota
	// ScopeProcess is kill(pid): the pinned process only. A manifest asks for
	// it when the launcher does exec and a group signal would be redundant, or
	// when a wrapper must survive its child.
	ScopeProcess
	// ScopeCgroup is the implicit last rung of section 11.6: every pid still
	// in the unit's cgroup, for a process that escaped its group by calling
	// setsid. It has no manifest spelling; a ladder cannot ask for it.
	ScopeCgroup
)

var scopeNames = [...]string{"process_group", "process", "cgroup"}

func (s Scope) String() string {
	if int(s) < len(scopeNames) {
		return scopeNames[s]
	}
	return "scope(" + strconv.Itoa(int(s)) + ")"
}

// ParseScope resolves a manifest's shutdown step scope.
//
// An empty value is ScopeGroup, matching spec's documented default. "cgroup"
// is deliberately NOT accepted: it is the implicit rung the runner adds after
// every declared step, and a manifest that could name it could put a
// kill-everything rung in the middle of a save.
func ParseScope(s string) (Scope, error) {
	switch strings.TrimSpace(s) {
	case "", "process_group":
		return ScopeGroup, nil
	case "process":
		return ScopeProcess, nil
	}
	return 0, fmt.Errorf("runner: %q is not a shutdown scope (want process|process_group)", s)
}

// Identity is the pin that makes signalling safe across pid reuse.
//
// A pid alone is not an identity. On a busy host a pid is recycled in seconds,
// and a shutdown ladder that signals a recycled pid signals a stranger --
// which is how a supervisor ends up killing the backup job that started while
// the game was exiting. StartTicks is /proc/<pid>/stat field 22, constant for
// the life of a process and never shared by two of them.
type Identity struct {
	PID        int    `json:"pid"`
	PGID       int    `json:"pgid"`
	StartTicks uint64 `json:"start_ticks"`
}

// Valid reports whether the pin names a process at all.
//
// A zero StartTicks is invalid even with a plausible pid: it is what an
// unchecked ReadStat error leaves behind, and treating it as a pin would turn
// "we never learned the start time" into "this pid is the game".
func (id Identity) Valid() bool { return id.PID > 0 && id.PGID > 0 && id.StartTicks > 0 }

func (id Identity) String() string {
	return fmt.Sprintf("pid %d pgid %d start_ticks %d", id.PID, id.PGID, id.StartTicks)
}

// ExitStatus is how the child ended.
//
// Signaled is carried separately rather than encoded as a negative Code
// because the shutdown report prints both fields and an operator reading
// "exit_code: -1" at 2am learns nothing, while "signal: SIGKILL" is the whole
// story.
type ExitStatus struct {
	Code     int
	Signal   platform.Signal
	Signaled bool
}

// OK reports a clean exit: status zero and no signal.
func (e ExitStatus) OK() bool { return !e.Signaled && e.Code == 0 }

// SignalName is the signal's name, or "" when the process exited normally.
func (e ExitStatus) SignalName() string {
	if !e.Signaled {
		return ""
	}
	return e.Signal.String()
}

func (e ExitStatus) String() string {
	if e.Signaled {
		return "killed by " + e.Signal.String()
	}
	return "exit status " + strconv.Itoa(e.Code)
}

// Process is the spawned game as the rest of the runner sees it.
//
// It is an interface so that the shutdown ladder -- the most safety-critical
// code in gvm and the part that cannot be exercised on the development
// machine -- runs against an in-memory fake on Windows. Everything that
// decides WHAT to signal and WHEN lives above this line and is tested
// everywhere; everything below it is kill(2), waitpid(2) and /proc.
//
// Alive returns an error as well as a bool, and callers must not discard it.
// A caller that reads any failure as "dead" will spawn a second server on a
// live world the first time /proc is briefly unreadable, which is the one
// failure this whole subsystem exists to prevent.
type Process interface {
	// PID is the process gvm spawned -- the launcher script, not necessarily
	// the engine.
	PID() int
	// PGID is the process group created by setsid at spawn. With Setsid it
	// equals PID.
	PGID() int
	// Identity is the (pid, pgid, start_ticks) pin written to runner.json.
	Identity() Identity
	// Alive reports whether the pinned identity is still running.
	Alive() (bool, error)
	// Signal delivers sig at scope. It returns ErrGone when there is nothing
	// left to signal, which the ladder counts as the rung succeeding.
	Signal(sig platform.Signal, scope Scope) error
	// Wait blocks until the child is reaped, or until ctx ends. A ctx that
	// ends does NOT kill the child: this package signals the game from
	// exactly one place, the ladder.
	Wait(ctx context.Context) (ExitStatus, error)
	// Empty reports whether no game process remains in the unit's cgroup. It
	// returns ErrNoCgroup when it cannot prove the answer.
	Empty() (bool, error)
}

// Spawned is a started game and the runner's ends of its console.
//
// Out and In are the SAME file in pty mode -- a pty master is bidirectional --
// which is why Close is one function rather than two Closers: closing the same
// descriptor twice, in a process that is also opening sockets and log files,
// is how a supervisor ends up writing a shutdown command into a rotated log.
type Spawned struct {
	Process Process

	// Out is the child's combined stdout and stderr: the pty master, or the
	// read end of the output pipe. nil in console mode `none`.
	Out io.Reader
	// In is the child's stdin: the pty master, or the write end of the stdin
	// pipe. nil in console mode `none`.
	In io.Writer

	// Close releases the runner's descriptors. It must not be called until the
	// pump has returned: closing a pty master out from under a blocked read is
	// a race whose worst outcome is a read against a descriptor number that
	// has since been reused by a log file.
	Close func() error
}

// SpawnFunc starts a game process.
//
// It deliberately takes no context. The obvious implementation of a
// context-aware spawn is exec.CommandContext, which SIGKILLs the child when
// the context ends -- and a SIGKILL that arrives because an HTTP request was
// cancelled is a world lost with no save and no report. Everything that ends
// this process goes through the shutdown ladder, which is the only code
// permitted to signal the game.
type SpawnFunc func(l Launch) (*Spawned, error)

// survivors reduces a cgroup.procs listing to the pids a sweep may signal.
//
// self is dropped because the runner is in its own unit's cgroup and a sweep
// that includes it kills the process holding the shutdown report seconds
// before it is written. The result is sorted and deduplicated so that a
// report lists the same pids in the same order every run, and so a pid that
// appears twice in a racing read is signalled once.
func survivors(procs []int, self int) []int {
	seen := make(map[int]struct{}, len(procs))
	out := make([]int, 0, len(procs))
	for _, p := range procs {
		if p <= 0 || p == self {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// statFunc is platform.ReadStat as a seam, so the identity arithmetic below
// runs and is tested on the development machine.
type statFunc func(pid int) (platform.Stat, error)

// newExemptions works out which processes gvm ITSELF put in the unit's cgroup
// during fn, so the cgroup sweep does not count them as game survivors.
//
// The one that matters is `console_compat: screen`. `screen -dmS` daemonises,
// so the pid exec.Cmd reports is a transient parent that exits and gvm never
// sees the session leader -- hence a before/after diff of cgroup.procs rather
// than a recorded pid. Without this, Empty() is never true for an instance
// with the bridge running, settled() can never be true, and a textbook `quit`
// still walks to the SIGKILL rungs and is reported degraded.
//
// The process-group filter is what makes it safe: a process the GAME forked
// during the same window is in the game's pgid and can never be exempted. And
// a process gvm cannot read is not exempted either -- "cannot prove it is
// mine" is never "it is mine".
func newExemptions(before, after []int, gamePGID int, stat statFunc) []Identity {
	was := make(map[int]struct{}, len(before))
	for _, pid := range before {
		was[pid] = struct{}{}
	}
	var out []Identity
	for _, pid := range after {
		if _, seen := was[pid]; seen {
			continue
		}
		st, err := stat(pid)
		if err != nil || st.PGID == gamePGID {
			continue
		}
		out = append(out, Identity{PID: pid, PGID: st.PGID, StartTicks: st.StartTicks})
	}
	return out
}

// dropExempt removes the exempted processes from a cgroup listing.
//
// Each exemption is re-verified against the live (pid, start_ticks) pin, for
// the same reason Alive does: a pid is recycled within seconds on a busy box,
// and an exemption held by number alone is a hole the next real survivor falls
// straight through.
func dropExempt(pids []int, exempt []Identity, stat statFunc) []int {
	if len(exempt) == 0 {
		return pids
	}
	out := pids[:0:0]
	for _, pid := range pids {
		if exemptStill(pid, exempt, stat) {
			continue
		}
		out = append(out, pid)
	}
	return out
}

func exemptStill(pid int, exempt []Identity, stat statFunc) bool {
	for _, id := range exempt {
		if id.PID != pid {
			continue
		}
		st, err := stat(pid)
		if err != nil {
			return false
		}
		return st.StartTicks == id.StartTicks
	}
	return false
}

// checkSameCgroup decides whether the cgroup gvm would sweep is the one the
// PINNED process is actually in.
//
// checkServiceCgroup proves the caller is a systemd service. It cannot prove
// the caller is THIS game's service, and on the Adopt path -- `gvm
// run-shutdown`, which section 11.8 designates for gvmd or a human to invoke
// -- it usually is not: a child forked by a long-lived unit inherits both that
// unit's INVOCATION_ID and its cgroup, so the sweep would end at SIGKILL over
// the caller's own cgroup. Both paths are named in the refusal because a bare
// "mismatch" leaves an operator with nothing to look at.
func checkSameCgroup(self, target string) error {
	if target == "" {
		return fmt.Errorf("%w: gvm is in %s but the pinned process's cgroup could not be read, "+
			"so there is nothing to prove the sweep would stay inside the game", ErrNoCgroup, self)
	}
	if self != target {
		return fmt.Errorf("%w: gvm is in %s but the game is in %s, so sweeping gvm's cgroup would "+
			"signal processes that are not the game", ErrNoCgroup, self, target)
	}
	return nil
}

// checkServiceCgroup decides whether a /proc/self/cgroup path is a cgroup gvm
// owns and may therefore sweep.
//
// Two conditions, and both are needed. INVOCATION_ID is set by systemd for a
// unit's own processes and by nothing else, so its absence means gvm was
// started by hand. The .service suffix rejects the case where it IS present
// but the runner has been re-parented into some other cgroup -- and, more
// importantly, rejects a login session's .scope, whose members are the
// operator's shell and ssh connection rather than a game.
func checkServiceCgroup(cgroup, invocationID string) error {
	if invocationID == "" {
		return fmt.Errorf("%w: INVOCATION_ID is unset, so gvm was not started by systemd", ErrNoCgroup)
	}
	if !strings.HasPrefix(cgroup, "/") {
		return fmt.Errorf("%w: %q is not an absolute cgroup path", ErrNoCgroup, cgroup)
	}
	if !strings.HasSuffix(cgroup, ".service") {
		return fmt.Errorf("%w: %q is not a service cgroup", ErrNoCgroup, cgroup)
	}
	return nil
}
