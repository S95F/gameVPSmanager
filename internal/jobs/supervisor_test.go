package jobs

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// LifecycleSupervisor is thin, and the one decision it makes is the one worth
// a test: WHICH systemd ActiveStates count as "the server is up".
//
// `systemctl is-active --quiet`, and therefore update.sh, says no for
// `activating`. Project Zomboid's first boot with a large workshop list is 40
// to 60 minutes and the unit is `activating` for all of it, so a guard that
// used is-active's answer would decide the instance had been DOWN at entry --
// and a nightly firing mid-boot would then leave it down, with the run
// recording nothing wrong at all. That is the quietest possible version of the
// failure this package exists to prevent.

// emptyFiles is a control plane with nothing in it. Observe is resilient to
// that by design -- a missing source is an absent fact and not an error -- so
// the unit state is the only thing these tests vary.
type emptyFiles struct {
	hold bool
	// spec serves /etc/gvm/instances/<n>.yaml, which the lifecycle MANAGER
	// requires -- it refuses a verb on an instance it cannot read -- while
	// the observer treats as an absent fact.
	spec bool
}

// minimalInstance is the smallest spec lifecycle.Allowed will act on.
const minimalInstance = "apiVersion: gvm/v1\nkind: Instance\n" +
	"metadata: {name: " + testInstance + "}\n" +
	"spec:\n  game: projectzomboid\n  enabled: true\n  user: pzserver\n" +
	"  paths: {install: /opt/pzserver, data: /home/pzserver/Zomboid, " +
	"home: /home/pzserver, work: /home/pzserver}\n"

func (f emptyFiles) Stat(name string) (fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

func (f emptyFiles) ReadFile(name string) ([]byte, error) {
	if f.hold && name == instance.PathsFor(testInstance).Hold {
		return []byte("2026-09-04T05:00:00Z root_cli maintenance window\n"), nil
	}
	if f.spec && name == instance.PathsFor(testInstance).Spec {
		return []byte(minimalInstance), nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (f emptyFiles) ReadDir(name string) ([]fs.DirEntry, error) {
	return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
}

type fakeUnits struct {
	state platform.UnitState
	err   error
}

func (u fakeUnits) State(context.Context, string) (platform.UnitState, error) {
	return u.state, u.err
}

func (u fakeUnits) ListUnits(context.Context, string) ([]platform.UnitState, error) {
	return nil, nil
}

func TestActiveIncludesActivatingBecauseOfTheFortyMinuteBoot(t *testing.T) {
	tests := map[string]bool{
		"active":       true,
		"activating":   true,
		"reloading":    true,
		"deactivating": true, // still saving; a start into a shutdown is worse
		"inactive":     false,
		"failed":       false,
	}
	for state, want := range tests {
		s := LifecycleSupervisor{
			Obs: &lifecycle.Observer{
				FS:    emptyFiles{},
				Units: fakeUnits{state: platform.UnitState{ActiveState: state}},
			},
		}
		got, err := s.Active(context.Background(), testInstance)
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if got != want {
			t.Errorf("ActiveState %q -> %v, want %v", state, got, want)
		}
	}
}

// TestAnUnreachableManagerIsAnErrorAndNotAStoppedServer.
//
// A zero UnitState is indistinguishable from "inactive, cleanly". Reporting a
// manager gvm could not reach as a stopped server is how the guard decides an
// instance was down at entry and then does nothing about a world it did stop
// -- so the observation has to fail rather than answer.
func TestAnUnreachableManagerIsAnErrorAndNotAStoppedServer(t *testing.T) {
	// No Units at all: the development machine's shape, and `--offline`.
	s := LifecycleSupervisor{Obs: &lifecycle.Observer{FS: emptyFiles{}}}
	if _, err := s.Active(context.Background(), testInstance); err == nil {
		t.Error("an unchecked unit reported a definite answer")
	}

	// A manager that answered with an error.
	s2 := LifecycleSupervisor{
		Obs: &lifecycle.Observer{
			FS:    emptyFiles{},
			Units: fakeUnits{err: errors.New("dbus: connection refused")},
		},
	}
	if _, err := s2.Active(context.Background(), testInstance); err == nil {
		t.Error("a failed State() reported a definite answer")
	}
}

// TestHeldReadsTheOperatorMarker, which is what keeps the guard from fighting
// `gvm stop`.
func TestHeldReadsTheOperatorMarker(t *testing.T) {
	for _, hold := range []bool{false, true} {
		s := LifecycleSupervisor{
			Obs: &lifecycle.Observer{
				FS:    emptyFiles{hold: hold},
				Units: fakeUnits{state: platform.UnitState{ActiveState: "inactive"}},
			},
		}
		got, err := s.Held(context.Background(), testInstance)
		if err != nil {
			t.Fatal(err)
		}
		if got != hold {
			t.Errorf("Held() = %v, want %v", got, hold)
		}
	}
}

// TestTheSupervisorRefusesWithoutItsHalves, rather than reporting a stopped
// server or a successful start.
func TestTheSupervisorRefusesWithoutItsHalves(t *testing.T) {
	var s LifecycleSupervisor
	if _, err := s.Active(context.Background(), testInstance); err == nil {
		t.Error("Active with no observer answered")
	}
	if err := s.Start(context.Background(), testInstance); err == nil {
		t.Error("Start with no manager reported success")
	}
	if _, err := s.Stop(context.Background(), testInstance, 0); err == nil {
		t.Error("Stop with no manager reported success")
	}
}

// TestAJobsOwnStopDoesNotForgeTheOperatorHold.
//
// /run/gvm/<n>/hold means exactly one thing -- a HUMAN asked for this instance
// to be down -- and two separate mechanisms stand aside for it:
// lifecycle.FindingNotRunning's `!Hold` term, which is what gvm-guard.timer
// filters on, and RunningGuard.restore, which yields rather than start.
//
// So a stop issued by a JOB must not write it. It used to: every stopping kind
// goes through this one method, and lifecycle.VerbStop declares HoldSet, so an
// update or content_sync that failed fatally between its own stop and its own
// start left the world down overnight, recorded `Held` rather than
// `left_stopped`, wrote no breadcrumb, and disarmed the five-minute recovery
// timer -- the belt and the braces failing the same way for the same reason.
//
// Driven through a REAL lifecycle.Manager, because the defect was in the
// request this method sends and a fake Supervisor cannot see one.
func TestAJobsOwnStopDoesNotForgeTheOperatorHold(t *testing.T) {
	newMgr := func(h *recordingHost) *lifecycle.Manager {
		return &lifecycle.Manager{
			Obs: &lifecycle.Observer{
				FS:    emptyFiles{spec: true},
				Units: fakeUnits{state: platform.UnitState{ActiveState: "active"}},
			},
			Host: h,
			Shutdown: func(context.Context, string, time.Duration) (lifecycle.Outcome, error) {
				return lifecycle.Outcome{Rung: "console", Took: time.Second}, nil
			},
			Now: func() time.Time { return time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC) },
		}
	}

	// The job's stop, through the supervisor the guard and every step table
	// use.
	jobHost := &recordingHost{}
	out, err := LifecycleSupervisor{Mgr: newMgr(jobHost)}.
		Stop(context.Background(), testInstance, 90*time.Second)
	if err != nil {
		t.Fatalf("the job's stop: %v", err)
	}
	if n := jobHost.writes[hostops.FileHold]; n != 0 {
		t.Errorf("a job's own stop wrote the operator hold %d time(s); that marker disarms "+
			"gvm-guard.timer and makes the running guard stand aside, so the job would be "+
			"forging the one thing that could recover from it", n)
	}
	// Everything else about the stop is unchanged: the ladder ran and the rung
	// is on the record, which is what the audit row requires.
	if out.Rung != "console" {
		t.Errorf("rung = %q; suppressing the hold must not change the ladder", out.Rung)
	}
	if n := jobHost.units[hostops.VerbUnitStop]; n != 1 {
		t.Errorf("the unit was stopped %d time(s), want 1", n)
	}

	// An operator's `gvm stop` still writes it -- twice, before and after the
	// ladder, because RuntimeDirectoryPreserve=restart deletes the first copy.
	// That ordering is what this half protects.
	opHost := &recordingHost{}
	if _, err := newMgr(opHost).Do(context.Background(), lifecycle.Request{
		Verb: lifecycle.VerbStop, Instance: testInstance, Reason: "maintenance window",
	}); err != nil {
		t.Fatalf("an operator's stop: %v", err)
	}
	if n := opHost.writes[hostops.FileHold]; n != 2 {
		t.Errorf("an operator's stop wrote the hold %d time(s), want 2 (before the ladder so a "+
			"power loss still leaves the marker, and after it because the stop takes "+
			"/run/gvm/<n> with it)", n)
	}
}

// recordingHost counts the privileged calls a manager makes.
type recordingHost struct {
	writes map[hostops.FileKind]int
	units  map[hostops.Verb]int
}

func (h *recordingHost) Do(_ context.Context, req hostops.Request) (hostops.Result, error) {
	if h.writes == nil {
		h.writes, h.units = map[hostops.FileKind]int{}, map[hostops.Verb]int{}
	}
	switch req.Verb {
	case hostops.VerbFileWrite:
		h.writes[req.File]++
	case hostops.VerbFileRemove:
	default:
		h.units[req.Verb]++
	}
	return hostops.Result{Changed: true}, nil
}

// TestTheSupervisorSatisfiesTheSeam, so the production wiring cannot drift
// from the interface the guard holds.
func TestTheSupervisorSatisfiesTheSeam(t *testing.T) {
	var _ Supervisor = LifecycleSupervisor{}
	var _ Host = (*fakeHost)(nil)
	var _ Locker = (*fakeLocker)(nil)
	var _ Unlocker = (*Lock)(nil)
	var _ Flock = sysFlock{}
}
