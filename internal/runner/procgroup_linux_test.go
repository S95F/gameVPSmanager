//go:build linux

package runner

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The bug this whole file exists for: legacy start-server.sh launches the JVM
// in a subshell WITHOUT exec, so the unit's MainPID is the shell and the
// process holding the world open is its child. Signalling MainPID stops the
// shell and leaves the JVM writing.
//
// The script below is that topology in six words: a shell that starts a child
// in the background and waits for it.
const forkingLauncher = "sleep 300 & echo child $!; wait"

// pidLive reports whether pid names a live (non-zombie) process.
func pidLive(pid int) bool {
	st, err := platform.ReadStat(pid)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		return false
	}
	return st.State != "Z"
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !pidLive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d is still alive", pid)
}

// startForkingLauncher spawns the shell, reads the grandchild's pid out of the
// console and returns both.
func startForkingLauncher(t *testing.T) (*Spawned, int) {
	t.Helper()
	sp, err := spawn(shellLaunch(spec.ConsoleModePTY, forkingLauncher))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	f := console.NewFake(console.FakeOptions{PartialAfter: -1})
	go func() { _, _ = io.Copy(f.Ingest, sp.Out) }()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range f.Texts() {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "child "); ok {
				pid, err := strconv.Atoi(strings.TrimSpace(rest))
				if err != nil {
					t.Fatalf("child pid line %q: %v", line, err)
				}
				return sp, pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the launcher never reported its child's pid")
	return nil, 0
}

func TestSetsidMakesTheChildItsOwnProcessGroupLeader(t *testing.T) {
	// Without setsid the child joins the runner's group, and kill(-pgid) would
	// then include the runner itself -- the supervisor killing itself and the
	// game together, losing the shutdown report.
	sp, err := spawn(shellLaunch(spec.ConsoleModePTY, "exit 0"))
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	id := sp.Process.Identity()
	if id.PGID != id.PID {
		t.Errorf("identity = %s; the child is not a session leader", id)
	}
	if id.StartTicks == 0 {
		t.Error("no start time was pinned, so pid reuse cannot be detected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := sp.Process.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSignallingTheMainPIDLeavesTheEngineRunning(t *testing.T) {
	// This is the failure, demonstrated. It is the reason `scope: process` is
	// not the default and the reason the ladder ends in a group signal.
	//
	// It also pins the Setctty decision in spawn_linux.go: the launcher is a
	// session leader with no controlling terminal, so its death sends no
	// SIGHUP to the group. If the pty were made the child's ctty, the engine
	// would die here along with its launcher -- which would look tidy and
	// would mean a crashed runner takes an unsaved world with it.
	sp, grandchild := startForkingLauncher(t)
	defer sp.Close()
	// Whatever this test proves, it must not leak a sleep(1) onto CI.
	defer platform.SignalGroup(sp.Process.PGID(), platform.SIGKILL)

	if err := sp.Process.Signal(platform.SIGTERM, ScopeProcess); err != nil {
		t.Fatalf("signal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := sp.Process.Wait(ctx); err != nil {
		t.Fatalf("the launcher did not exit: %v", err)
	}
	if !pidLive(grandchild) {
		t.Fatal("the engine died with its launcher; this test can no longer prove anything")
	}
}

func TestSignallingTheGroupReachesTheEngineToo(t *testing.T) {
	// And this is the fix: one kill(-pgid) reaches the launcher and the engine
	// it forked.
	sp, grandchild := startForkingLauncher(t)
	defer sp.Close()
	defer platform.SignalGroup(sp.Process.PGID(), platform.SIGKILL)

	if err := sp.Process.Signal(platform.SIGTERM, ScopeGroup); err != nil {
		t.Fatalf("signal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := sp.Process.Wait(ctx); err != nil {
		t.Fatalf("the launcher did not exit: %v", err)
	}
	waitGone(t, grandchild)
}

func TestSignallingAProcessThatHasAlreadyGoneIsNotAFailure(t *testing.T) {
	// A rung that finds the game already gone has achieved what it was asked
	// to achieve, and ErrGone is how the ladder is told so.
	sp, err := spawn(shellLaunch(spec.ConsoleModePTY, "exit 0"))
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := sp.Process.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if alive, err := sp.Process.Alive(); err != nil || alive {
		t.Fatalf("Alive = %v, %v after the child exited", alive, err)
	}
	if err := sp.Process.Signal(platform.SIGTERM, ScopeProcess); !errors.Is(err, ErrGone) {
		t.Errorf("Signal = %v, want ErrGone", err)
	}
}

func TestTheCgroupSweepRefusesWhenItCannotProveTheCgroupIsGvms(t *testing.T) {
	// `go test` is not a systemd service, so the runner's cgroup here belongs
	// to whatever launched it. Refusing is the correct answer: "SIGKILL
	// everything left in the cgroup" against a developer's login session logs
	// them out of their own box.
	//
	// The skip is not politeness. On a CI host whose agent IS a systemd
	// service, this process inherits INVOCATION_ID and sits in a .service
	// cgroup, so the refusal cannot be provoked -- and the sweep below would
	// then be a real SIGKILL of the CI agent's own cgroup.
	if cg, err := sweepableCgroup(os.Getpid()); err == nil {
		t.Skipf("this process is itself in a service cgroup (%s), so the refusal cannot be provoked here", cg)
	}
	sp, err := spawn(shellLaunch(spec.ConsoleModePTY, "exit 0"))
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := sp.Process.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := sp.Process.Empty(); err == nil {
		t.Error("Empty answered from a cgroup it cannot prove is gvm's")
	} else if !errors.Is(err, ErrNoCgroup) {
		t.Errorf("Empty = %v, want ErrNoCgroup", err)
	}
	if err := sp.Process.Signal(platform.SIGKILL, ScopeCgroup); !errors.Is(err, ErrNoCgroup) {
		t.Errorf("cgroup sweep = %v, want ErrNoCgroup", err)
	}
}

// TestTheCgroupSweepRefusesAProcessInAnotherCgroup is the Adopt path's hole.
//
// `gvm run-shutdown` is designed to be invoked by gvmd or a human, so the
// process resolving the sweepable cgroup is usually not the game's parent. A
// child forked by a long-lived .service inherits its INVOCATION_ID and its
// cgroup, so checkServiceCgroup passes and the last rung SIGKILLs the caller's
// own cgroup. pid 1 stands in for the pin that does not belong here: it is in
// /init.scope, never in a gvm unit.
func TestTheCgroupSweepRefusesAProcessInAnotherCgroup(t *testing.T) {
	self, err := sweepableCgroup(os.Getpid())
	if err != nil {
		t.Skipf("this process is not in a provable service cgroup, so the check under test is unreachable here: %v", err)
	}
	_, err = sweepableCgroup(1)
	if !errors.Is(err, ErrNoCgroup) {
		t.Fatalf("sweepableCgroup(1) = %v, want ErrNoCgroup; the sweep would have targeted %s", err, self)
	}
	if !strings.Contains(err.Error(), self) {
		t.Errorf("the refusal does not name gvm's own cgroup, which is the operator's only clue: %v", err)
	}

	// And the refusal has to reach the two callers that would otherwise act on
	// it: the ladder's settled() check and the sweep itself.
	b := newProcBase(Identity{PID: 1, PGID: 1})
	if _, err := b.Empty(); !errors.Is(err, ErrNoCgroup) {
		t.Errorf("Empty = %v, want ErrNoCgroup", err)
	}
	if _, err := b.remaining(); !errors.Is(err, ErrNoCgroup) {
		t.Errorf("remaining = %v, want ErrNoCgroup", err)
	}
}
