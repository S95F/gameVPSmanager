//go:build linux

package platform

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// ReadStat reads and decodes /proc/<pid>/stat.
func ReadStat(pid int) (Stat, error) {
	if pid <= 0 {
		return Stat{}, fmt.Errorf("platform: %d is not a pid", pid)
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return Stat{}, err
	}
	return ParseStat(b)
}

// StartTicks is field 22 of /proc/<pid>/stat, the process's start time in
// clock ticks since boot. Half of the identity pin written into runner.json.
func StartTicks(pid int) (uint64, error) {
	st, err := ReadStat(pid)
	return st.StartTicks, err
}

// Alive reports whether pid is still the process that was pinned with
// wantStartTicks. A recycled pid has a different start time, so this is the
// check that keeps the shutdown ladder from signalling a stranger.
//
// The error return is not decoration and must not be discarded. A caller that
// treats every failure as "dead" will spawn a second server on a live world
// the moment /proc becomes unreadable -- so a vanished process is (false,
// nil), and anything else is (false, err) for the caller to refuse on.
func Alive(pid int, wantStartTicks uint64) (bool, error) {
	st, err := ReadStat(pid)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return st.StartTicks == wantStartTicks, nil
}

// SignalGroup sends sig to an entire process group.
//
// Signalling the group, not the pid, is the point: a game launched through
// start-server.sh is a shell whose child is the JVM, and a SIGTERM delivered
// to the shell alone leaves the JVM writing to the world with nobody waiting
// for it (section 11.6, rule 6).
func SignalGroup(pgid int, sig Signal) error {
	if err := checkPGID(pgid); err != nil {
		return err
	}
	if !sig.Known() {
		return fmt.Errorf("platform: %s is not a signal gvm sends", sig)
	}
	if err := unix.Kill(-pgid, unix.Signal(sig)); err != nil {
		return fmt.Errorf("platform: kill(-%d, %s): %w", pgid, sig, err)
	}
	return nil
}

// SignalProcess sends sig to one pid.
//
// It is the narrow counterpart of SignalGroup, for the two cases where a group
// is the wrong target: a manifest step that declares `scope: process`, and the
// cgroup sweep, which signals pids that escaped the group by calling setsid
// and therefore have no relationship to the game's pgid at all.
//
// The caller is responsible for having verified the pid's identity first --
// this function cannot know whether the pid it is handed is still the process
// somebody pinned ten minutes ago.
func SignalProcess(pid int, sig Signal) error {
	if err := checkPID(pid); err != nil {
		return err
	}
	if !sig.Known() {
		return fmt.Errorf("platform: %s is not a signal gvm sends", sig)
	}
	if err := unix.Kill(pid, unix.Signal(sig)); err != nil {
		return fmt.Errorf("platform: kill(%d, %s): %w", pid, sig, err)
	}
	return nil
}

// SelfCgroup is this process's path in the unified hierarchy, which for a
// running unit is its own service cgroup -- the set of pids KillMode=mixed
// would reap, and the set the last rung of the ladder has to empty itself.
func SelfCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	return ParseSelfCgroup(b)
}

// ProcCgroup is the unified-hierarchy path of an ARBITRARY process.
//
// It exists because /proc/self is the wrong answer for anything gvm did not
// fork. `gvm run-shutdown` is invoked by gvmd or by a human, so the process
// resolving a sweepable cgroup is usually not the game's parent, and a child
// forked by a long-lived unit inherits that unit's cgroup along with its
// INVOCATION_ID.
func ProcCgroup(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("platform: %d is not a pid", pid)
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "", err
	}
	return ParseSelfCgroup(b)
}

// CgroupProcs lists the pids in the cgroup at rel, a path in the form
// /proc/self/cgroup reports. A cgroup that has gone away returns no pids and
// no error: an empty cgroup and a deleted one mean the same thing to the
// caller, which is that nothing is left to kill.
func CgroupProcs(rel string) ([]int, error) {
	p, err := cgroupProcsPath(rel)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return ParsePIDs(b)
}
