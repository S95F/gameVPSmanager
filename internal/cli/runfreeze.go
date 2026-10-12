// The runner's half of D-31: the freeze lease honour loop.
//
// internal/backup arms a lease before it turns a world's persistence off, and
// then relies on a DIFFERENT process to lift the freeze if its own does not
// come back. That is the whole argument of D-31 -- an in-process defer does
// not survive SIGKILL, the OOM killer or power loss, and a world accepting
// players while silently not saving is the worst failure in the system -- and
// the process it relies on is `gvm run`, which is already sitting next to the
// game with both channels open.
//
// It lives here rather than in internal/runner because internal/runner is a
// transitive dependency of internal/backup (through internal/lifecycle), so
// the honouring process cannot import the package that defines the document.
// internal/cli already imports both, which is why §13.2's sequence diagram
// puts the timer in `gvm run` and the wiring in this file.
//
// The spawning job is deliberately not the honouring process: the job is what
// gets killed.

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/S95F/gameVPSmanager/internal/backup"
	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// leaseThawer delivers one thaw step over the runner's own channels.
//
// Unlike the job side's consoleSender this needs no socket dial: the runner
// owns the pty and holds the control channel, so a `save-on` is one write away
// even when the job that armed the lease is gone and the console socket has
// nobody attached to it.
type leaseThawer struct {
	// console is the runner's stdin sink, which also echoes into the ring --
	// so the console log records that the WATCHDOG sent this, not an operator.
	console control.Sender
	// control may be nil (a game with no RCON) or unready (Project Zomboid's
	// first forty minutes). Both are refusals rather than silent successes:
	// a thaw reported as delivered when it was not is what leaves an operator
	// looking in the wrong place after a rollback.
	control control.Channel
}

// Thaw implements backup.Thawer.
func (t leaseThawer) Thaw(ctx context.Context, step backup.ThawStep) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch step.Via {
	case backup.ViaConsole:
		if t.console == nil {
			return errors.New("this runner has no console sink, so the freeze cannot be lifted " +
				"through the game console")
		}
		_, err := t.console.Send(freezeActor, step.Send)
		return err
	case backup.ViaControl:
		switch {
		case t.control == nil:
			return errors.New("this instance has no control channel, so the freeze cannot be " +
				"lifted through it")
		case !t.control.Ready():
			return fmt.Errorf("the control channel is not ready: %s", t.control.Unready())
		}
		_, err := t.control.Exec(ctx, step.Send)
		return err
	default:
		return fmt.Errorf("a freeze lease named an unknown channel %q", step.Via)
	}
}

// freezeActor is what the watchdog's sends are recorded as.
//
// Not runActor: an operator reading console.log after a rollback has to be
// able to tell the lease watchdog's save-on from the ladder's own, because
// only one of them means a backup job died holding this world frozen.
const freezeActor = "gvm-freeze-lease"

// freezeWatcher builds the runner's lease watcher.
//
// leasePath and fsys are parameters rather than derived here so that the
// assembly this function performs -- which path, whose filesystem, which
// channel each `via` goes to, where the unreadable-lease fallback comes from,
// and the liveness answer -- is assertable on the development machine against
// a temporary directory. "The mechanism exists and nothing calls it" was the
// defect class this file closes, and only a test of the real constructor
// catches the next one.
func freezeWatcher(leasePath string, fsys backup.FS, m *spec.Manifest, rend *spec.Renderer,
	th backup.Thawer, now func() time.Time, log *slog.Logger) *backup.Watcher {
	w := &backup.Watcher{
		Path: leasePath,
		FS:   fsys,
		Thaw: th,
		// Alive is what lets an expired-but-owner-dead lease be thawed within
		// one poll instead of at the end of the budget. The error is NOT
		// discarded: platform.Alive returns (false, err) for a pid it could
		// not inspect, and a caller that read that as "dead" would send
		// save-on into the middle of a live snapshot. Unknown is read as
		// "assume alive", so only the deadline acts.
		Alive: func(pid int, ticks uint64) lifecycle.Tri {
			ok, err := platform.Alive(pid, ticks)
			if err != nil {
				return lifecycle.Unknown
			}
			return lifecycle.TriOf(ok)
		},
		Now: now,
		Log: log,
	}
	// The fallback is this game's own freeze.after, for a lease that exists
	// and will not parse. The runner already rendered the shutdown ladder
	// from this manifest, so the truncated-document case has a correct thaw
	// available rather than only a log line. A manifest whose freeze does not
	// build is not fatal here: the runner's job is to supervise a game, and
	// the deadline path still works with no fallback at all.
	if m != nil {
		if plan, err := backup.BuildFreeze(m, rend); err != nil {
			if log != nil {
				log.Warn("freeze_lease no fallback thaw for an unreadable lease", "err", err)
			}
		} else {
			w.Fallback = plan.ThawSteps()
		}
	}
	return w
}

// startFreezeWatch sweeps once and then polls until ctx is done, returning the
// function that stops it and waits for it to be gone.
//
// The sweep is not an optimisation. The case it exists for is a runner that
// was SIGKILLed while a backup held a freeze: systemd restarts it,
// RuntimeDirectoryPreserve=restart has kept /run/gvm/<name>, and the lease is
// sitting there with nobody watching. A watcher that armed its timer only for
// leases it saw APPEAR would never look at that one.
//
// The returned stop waits, so the caller can tear the channels down knowing
// nothing is still writing to them.
func startFreezeWatch(ctx context.Context, w *backup.Watcher, note func(string, ...any) uint64) func() {
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Watch(wctx, func(t backup.Tick) {
			if note == nil || t.Decision == backup.DecisionWait {
				// A live lease every five seconds is a backup in progress,
				// and narrating it would bury the game's own output.
				return
			}
			switch {
			case t.Thawed && len(t.Steps) > 0:
				note("the backup's freeze lease expired and I have re-enabled saving (%s)",
					t.Why)
			case t.Err != nil:
				note("WARNING: the backup's freeze lease expired and the thaw FAILED (%v) -- "+
					"this world may not be saving: %s", t.Err, t.Why)
			case t.Thawed:
				// Nothing to send (`after: none`), so the lease has served
				// its purpose and is gone. Still worth a line: it is the
				// evidence that a backup job died holding this world.
				note("the backup's freeze lease expired with nothing to re-send: %s", t.Why)
			}
		})
	}()
	return func() {
		cancel()
		<-done
	}
}

// Compile-time proof that the runner's channels are what a lease thaw asks
// for. The seam lives in internal/backup beside the decision that calls it,
// so this is what keeps the two from drifting apart.
var _ backup.Thawer = leaseThawer{}
