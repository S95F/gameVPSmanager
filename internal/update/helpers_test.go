package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/provider"
)

const testInstance = "pz-main"

// recorder is a shared, ordered event log.
//
// Shared between the installer fake and the archive fake on purpose: the
// repair's safety property is an ORDER -- the preserve archive is written and
// verified BEFORE `validate` is allowed to overwrite anything -- and two
// separate per-fake counters cannot express an order between two fakes.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(format string, args ...any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf(format, args...))
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// index is where an event first appears, or -1.
func (r *recorder) index(want string) int {
	for i, e := range r.all() {
		if e == want {
			return i
		}
	}
	return -1
}

// The fakes. Four of them, none of which needs Linux, a network or a Steam
// account -- which is the point of every seam in this package: the two
// properties that matter here (nothing stops on a quiet night, and an adopted
// instance is never stopped for a build it may not install) are properties of
// control flow, and control flow is testable on the development machine.

// fakeInstaller is internal/provider, reduced to the four calls the pipeline
// makes.
//
// It renders plans the way the steamcmd driver renders them, including the two
// EMPTY ones that matter: "current, nothing to do" and "stale, and this
// instance is adopted". Those two are the whole of D-43's decision input, and a
// fake that collapsed them into one would make this file prove nothing.
type fakeInstaller struct {
	mu  sync.Mutex
	rec *recorder

	local, remote string
	// updateAvailable is the adopted case: a newer build gvm may not fetch.
	updateAvailable bool
	// writes is whether the update plan carries an artifact, i.e. whether gvm
	// is going to download something.
	writes bool
	bytes  int64

	insp *provider.Inspection

	// repairWrites makes the repair plan a real one. False is the manifest
	// that does not declare validate_on: repair.
	repairWrites bool

	planErr, applyErr, inspectErr error
	// verifyErr fails the IntentVerify apply, which is phase 6.
	verifyErr error

	plans   []provider.Intent
	applies []provider.Intent
}

func (f *fakeInstaller) Plan(_ context.Context, in provider.Intent) (*provider.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans = append(f.plans, in)
	if f.planErr != nil {
		return nil, f.planErr
	}
	pl := &provider.Plan{Intent: in, Reason: "fake " + string(in)}
	if f.local != "" {
		pl.Current = provider.Version{ID: f.local, Label: "public"}
	}
	if f.remote != "" {
		pl.Target = provider.Version{ID: f.remote, Label: "public"}
	}
	switch in {
	case provider.IntentUpdate:
		pl.UpdateAvailable = f.updateAvailable
		if f.writes {
			pl.Bytes = f.bytes
			pl.Artifacts = []provider.Artifact{{
				Digest:  provider.Digest(fmt.Sprintf("buildid:380870:%s", f.remote)),
				Size:    f.bytes,
				Source:  "steamcmd:app_update 380870",
				DestRel: ".",
			}}
		}
	case provider.IntentRepair:
		if f.repairWrites {
			pl.Destructive = true
			pl.Reason = "repair: this run passes `validate`, which OVERWRITES customised files"
			pl.Artifacts = []provider.Artifact{{
				Digest:  provider.Digest("buildid:380870:" + f.remote),
				Source:  "steamcmd:app_update 380870 validate",
				DestRel: ".",
			}}
		}
	}
	return pl, nil
}

func (f *fakeInstaller) Apply(_ context.Context, pl *provider.Plan) (*provider.Applied, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pl == nil {
		return nil, errors.New("nil plan")
	}
	// The real driver's first statement, reproduced: a plan that skipped
	// Authorize must not be appliable even against a fake.
	if err := pl.CheckAuthorized(true); err != nil {
		return nil, err
	}
	f.applies = append(f.applies, pl.Intent)
	f.rec.add("apply:%s", pl.Intent)
	if pl.Intent == provider.IntentVerify && f.verifyErr != nil {
		return nil, f.verifyErr
	}
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return &provider.Applied{Intent: pl.Intent, Changed: pl.Writes(), Note: "applied"}, nil
}

func (f *fakeInstaller) Inspect(context.Context) (*provider.Inspection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	if f.insp != nil {
		return f.insp, nil
	}
	return &provider.Inspection{Present: true}, nil
}

func (f *fakeInstaller) Observe(context.Context) (*provider.Observation, error) {
	return &provider.Observation{Version: provider.Version{ID: f.local}, Healthy: true}, nil
}

func (f *fakeInstaller) planned() []provider.Intent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.Intent(nil), f.plans...)
}

func (f *fakeInstaller) applied() []provider.Intent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.Intent(nil), f.applies...)
}

// fakeContent is internal/content's planner and resolver.
type fakeContent struct {
	plan    jobs.ContentPlan
	settle  SettleInput
	res     jobs.Resolution
	planErr error

	activated int
}

func (c *fakeContent) Plan(context.Context) (jobs.ContentPlan, error) {
	return c.plan, c.planErr
}

func (c *fakeContent) Settlement(context.Context) (SettleInput, error) { return c.settle, nil }
func (c *fakeContent) Resolve(context.Context) (jobs.Resolution, error) {
	return c.res, nil
}

func (c *fakeContent) Activate(context.Context) error {
	c.activated++
	return nil
}

// fakeArchives is internal/backup, reduced to Snapshot and Verify.
//
// skipVerifyStamp reproduces an engine that reports an archive verified and
// leaves VerifiedAt zero, which is the one failure mode a caller cannot see
// without checking -- and the reason both the pre-update backup and the repair
// check Ref.Verified() rather than trusting the error.
type fakeArchives struct {
	rec             *recorder
	refs            []*archive.Ref
	snapErr         error
	verifyErr       error
	noRef           bool
	skipVerifyStamp bool
}

func (a *fakeArchives) Snapshot(_ context.Context, class archive.Class, reason string) (*archive.Ref, error) {
	if a.snapErr != nil {
		return nil, a.snapErr
	}
	if a.noRef {
		return nil, nil
	}
	ref := &archive.Ref{
		Path:      "/var/backups/gvm/pz-main/pz-main-20260904-182211-" + string(class) + ".tar.gz",
		SHA256:    "1b40c9de00000000000000000000000000000000000000000000000000000000",
		Bytes:     2_251_799_813,
		Instance:  testInstance,
		Class:     class,
		Reason:    reason,
		CreatedAt: time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC),
	}
	a.refs = append(a.refs, ref)
	a.rec.add("snapshot:%s", class)
	return ref, nil
}

func (a *fakeArchives) Verify(_ context.Context, ref *archive.Ref) error {
	if a.verifyErr != nil {
		return a.verifyErr
	}
	if !a.skipVerifyStamp && ref != nil {
		ref.VerifiedAt = time.Date(2026, 9, 4, 18, 25, 0, 0, time.UTC)
	}
	a.rec.add("verify")
	return nil
}

// fakeSuper is a fake systemd, and the ONLY thing this file asserts about it
// is how many times Stop was called.
//
// That is the test section 13.5 asks for in so many words -- "pipeline_test.go
// must assert the fake systemd recorded no stops" -- and it is why the counter
// is here rather than a spy inside the pipeline: the pipeline cannot stop a
// server, so the only honest place to count stops is the thing that would
// perform one.
type fakeSuper struct {
	mu     sync.Mutex
	active bool
	held   bool
	stops  int
	starts int
}

func (s *fakeSuper) Active(context.Context, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, nil
}

func (s *fakeSuper) Held(context.Context, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held, nil
}

func (s *fakeSuper) Start(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	s.active = true
	return nil
}

func (s *fakeSuper) Stop(context.Context, string, time.Duration) (lifecycle.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	s.active = false
	return lifecycle.Outcome{Rung: "saved", Took: 12 * time.Second}, nil
}

func (s *fakeSuper) stopCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stops
}

// fakeHost accepts every privileged write. The lease and the degraded marker
// are internal/jobs' business and have their own tests there; what matters
// here is that they do not need a root filesystem.
type fakeHost struct {
	mu      sync.Mutex
	writes  []hostops.FileKind
	removes []hostops.FileKind
}

func (h *fakeHost) Do(_ context.Context, req hostops.Request) (hostops.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch req.Verb {
	case hostops.VerbFileWrite:
		h.writes = append(h.writes, req.File)
	case hostops.VerbFileRemove:
		h.removes = append(h.removes, req.File)
	}
	return hostops.Result{Verb: req.Verb}, nil
}

func (h *fakeHost) wrote(k hostops.FileKind) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, w := range h.writes {
		if w == k {
			return true
		}
	}
	return false
}

// fakeLocker is the per-instance job lock without a filesystem.
type fakeLocker struct{ held int }

func (l *fakeLocker) Acquire(path string, h lifecycle.Holder) (jobs.Unlocker, error) {
	l.held++
	return &fakeUnlock{path: path, h: h}, nil
}

type fakeUnlock struct {
	path string
	h    lifecycle.Holder
}

func (u *fakeUnlock) Release() error           { return nil }
func (u *fakeUnlock) Holder() lifecycle.Holder { return u.h }
func (u *fakeUnlock) Path() string             { return u.path }

// roomyDisk is a statfs that always has space, so a disk refusal in a test is
// one the test asked for.
func roomyDisk(string) (platform.DiskUsage, error) {
	return platform.DiskUsage{Total: 500 << 30, Free: 400 << 30, Avail: 400 << 30}, nil
}

func tightDisk(avail uint64) func(string) (platform.DiskUsage, error) {
	return func(string) (platform.DiskUsage, error) {
		return platform.DiskUsage{Total: 500 << 30, Free: avail, Avail: avail}, nil
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// jobEnv wires the REAL job engine around a pipeline.
//
// The two required assertions are about what the job layer does with this
// package's answers, so the test drives jobs.Execute rather than calling the
// phases by hand: a pipeline that returned the right diffs into a decision
// nobody consulted would pass a unit test and stop a live server.
func jobEnv(kind lifecycle.JobKind, p *Pipeline, super *fakeSuper, managed bool) (*jobs.Env, *fakeHost) {
	host := &fakeHost{}
	at := time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC)
	return &jobs.Env{
		Instance: testInstance,
		Job:      "nightly",
		Slug:     "pz-main_nightly",
		Action:   string(kind),
		Kind:     kind,
		Unit:     "gvm-job@pz-main_nightly.service",
		Trigger:  jobs.TriggerTimer,
		Actor:    audit.Timer(),

		ManagedInstall: managed,

		Super:  super,
		Host:   host,
		Locker: &fakeLocker{},
		Audit:  audit.New(audit.NewMemorySink()).WithLogger(discardLogger()),
		Log:    discardLogger(),
		Now: func() time.Time {
			at = at.Add(time.Second)
			return at
		},

		PID:    4242,
		BootID: "7f1c0a4e-0000-4000-8000-000000000001",

		StopBudget:    3 * time.Minute,
		RestoreBudget: time.Minute,

		Updates: p,
	}, host
}
