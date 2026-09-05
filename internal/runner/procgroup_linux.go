//go:build linux

package runner

import (
	"fmt"
	"os"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// This file is the process-group half of the Process seam: what gvm is
// entitled to signal, and how it proves that a group or a cgroup still belongs
// to the game rather than to whoever inherited the numbers.

func newProcBase(id Identity) procBase {
	b := procBase{id: id, self: os.Getpid(), exempt: &exemptSet{}}
	b.cgroup, b.cgroupErr = sweepableCgroup(id.PID)
	return b
}

// sweepableCgroup resolves the cgroup whose leftover pids gvm may kill, or the
// reason there is none.
//
// It is resolved once, at construction. The answer cannot change for the life
// of the runner, and the last rung of a shutdown ladder is not the moment to
// discover that /proc is unreadable.
//
// Two proofs, and both are needed. checkServiceCgroup says the caller is a
// systemd service rather than a login session. checkSameCgroup says the pinned
// game is in that same cgroup -- which on the spawn path is true by
// construction, and on the Adopt path is the whole question: `gvm run-shutdown`
// is invoked by gvmd or a human, and a child forked by a long-lived unit
// inherits its parent's cgroup along with its INVOCATION_ID.
//
// A read error is "cannot prove", never a fallback to the caller's own path.
// The ladder degrades correctly from there: settled() takes its documented
// ErrNoCgroup branch once the leader is dead, and ScopeCgroup refuses.
func sweepableCgroup(pid int) (string, error) {
	cg, err := platform.SelfCgroup()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoCgroup, err)
	}
	if err := checkServiceCgroup(cg, os.Getenv("INVOCATION_ID")); err != nil {
		return "", err
	}
	// A game that has not been spawned yet has no cgroup to compare against.
	// The runner constructs its Process only after the fork, so this is the
	// zero identity of a plan that pins nothing, and refusing is correct.
	target, err := platform.ProcCgroup(pid)
	if err != nil {
		target = ""
	}
	if err := checkSameCgroup(cg, target); err != nil {
		return "", err
	}
	return cg, nil
}

// groupLive reports whether the pinned process group still contains processes
// gvm can prove are its own.
//
// The leader being alive settles it. When the leader has exited but the engine
// it forked has not -- which is the normal shape of a launcher script that
// does not exec -- the group id alone proves nothing, because a pgid whose
// every member has died can be handed to an unrelated new session leader. So
// the members are confirmed through the unit's cgroup, and when that is not
// available the answer is an ERROR rather than a guess: a ladder told "gone"
// incorrectly reports a shutdown that never happened and stops escalating.
func (p *procBase) groupLive() (bool, error) {
	alive, err := p.Alive()
	if err != nil {
		return false, err
	}
	if alive {
		return true, nil
	}
	pids, err := p.remaining()
	if err != nil {
		return false, fmt.Errorf("runner: the pinned group leader %d has exited and the group cannot be confirmed: %w",
			p.id.PID, err)
	}
	for _, pid := range pids {
		st, err := platform.ReadStat(pid)
		if err != nil {
			continue // it exited while we were reading; nothing to signal there
		}
		if st.PGID == p.id.PGID {
			return true, nil
		}
	}
	return false, nil
}

// remaining lists the pids left in the unit's cgroup that a sweep may act on:
// not the runner itself, and not the helpers gvm started there on purpose.
//
// The exemption applies here rather than only in Empty, so it covers the
// ScopeCgroup sweep too. SIGKILLing the operator's own `screen -r` session
// because the game took a moment to exit is the same bug wearing a hat.
func (p *procBase) remaining() ([]int, error) {
	if p.cgroupErr != nil {
		return nil, p.cgroupErr
	}
	procs, err := platform.CgroupProcs(p.cgroup)
	if err != nil {
		return nil, err
	}
	return dropExempt(survivors(procs, p.self), p.exempt.all(), platform.ReadStat), nil
}

// SweepExempt lists the processes gvm itself put in the unit's cgroup.
func (p *procBase) SweepExempt() []Identity { return p.exempt.all() }

// ExemptAround runs fn and exempts from the cgroup sweep whatever it left
// behind in the unit's cgroup outside the game's process group.
//
// It is a before/after diff because the thing being started daemonises:
// `screen -dmS` forks the session and exits, so the pid exec reports is a
// transient parent and the session leader is never handed to gvm at all.
func (p *procBase) ExemptAround(fn func() error) error {
	if p.cgroupErr != nil {
		// Nothing is sweepable, so nothing needs exempting. fn still runs:
		// refusing to start the console bridge because gvm cannot read a
		// cgroup would be the tail wagging the dog.
		return fn()
	}
	before, _ := platform.CgroupProcs(p.cgroup)
	err := fn()
	after, err2 := platform.CgroupProcs(p.cgroup)
	if err2 != nil {
		return err
	}
	if ids := newExemptions(before, after, p.id.PGID, platform.ReadStat); len(ids) > 0 {
		p.exempt.add(ids)
	}
	return err
}

// Empty reports whether no game process is left in the unit's cgroup. It is
// the condition the shutdown ladder blocks on: the game process group being
// empty, not the main pid having exited, because the main pid is often a
// launcher script that exits long before the engine it started.
func (p *procBase) Empty() (bool, error) {
	pids, err := p.remaining()
	if err != nil {
		return false, err
	}
	return len(pids) == 0, nil
}
