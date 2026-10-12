//go:build linux

package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// TestTheTwoRealLocksDoNotCollide uses real flock(2) on both production
// primitives at once, which no other test in the tree does: every engine test
// injects a fake Locker and every runner test injects req.Flock, and that is
// how the two ended up on one path with a green suite.
//
// The derivation half -- and the reasoning for why the paths must differ -- is
// TestTheJobLockIsNotTheRunnersWorldLock in slug_test.go, which runs on the
// development machine. This is the half that needs the syscall.
func TestTheTwoRealLocksDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	runnerLock := filepath.Join(dir, "run", "job.lock")
	jobLock := filepath.Join(dir, "locks", "job_pz-main.lock")
	if err := os.MkdirAll(filepath.Dir(runnerLock), 0o750); err != nil {
		t.Fatal(err)
	}

	// The runner takes its lock first and holds it, as it does for the whole
	// life of the unit.
	held, err := platform.Flock(runnerLock)
	if err != nil {
		t.Fatalf("the runner could not take its own lock: %v", err)
	}
	defer func() { _ = held.Unlock() }()

	// And a job acquires anyway, which is the whole point: an update on a
	// RUNNING instance has to be able to start.
	l := FlockLocker{}
	u, err := l.Acquire(jobLock, testHolder(time.Now()))
	if err != nil {
		t.Fatalf("a job could not acquire while the runner held its world lock (%v); that is "+
			"every instance-scoped kind refused on every live server", err)
	}

	// The job lock still excludes a SECOND job, which is what it is for.
	second, serr := l.Acquire(jobLock, testHolder(time.Now()))
	if serr == nil {
		t.Error("two jobs hold the same instance's lock")
		if second != nil {
			_ = second.Release()
		}
	} else {
		var b *Busy
		if !errors.As(serr, &b) {
			t.Errorf("a contended acquire returned %v, want a *Busy naming the holder", serr)
		} else if b.Holder.PID != 48812 || !b.Known {
			t.Errorf("the refusal does not name the holder: %+v", b)
		}
	}
	if err := u.Release(); err != nil {
		t.Fatal(err)
	}

	// And the runner can take its own lock after a job has come and gone,
	// which the shared-path version could not guarantee: systemd's removal of
	// the RuntimeDirectory used to unlink a flocked file under its holder.
	if _, serr := os.Stat(runnerLock); serr != nil {
		t.Errorf("the runner's lock file vanished while it was held: %v", serr)
	}
}
