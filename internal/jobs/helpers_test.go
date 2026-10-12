package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The fakes, and why there are this many of them.
//
// Rule: TestNeverLeavesServerStopped runs on WINDOWS. That is not a
// convenience -- it is the only way the invariant gets checked before the box
// it matters on. So every host-touching thing the job layer does is a seam with
// a fake here: a fake systemd (fakeSuper), a fake privileged surface
// (fakeHost), a fake lock (fakeLocker, so a test never creates /run/gvm), and a
// fake for each work seam.
//
// The injector is the interesting one. Instead of a hand-written list of forty
// injection points, every fake routes its fallible calls through it, numbered;
// the test runs a job once to learn how many calls it makes, then re-runs it
// once per call with a failure, a panic or a cancellation at that call. A phase
// added to a step table next year is covered the day it is added.

// --------------------------------------------------------------------------
// the injector

type injector struct {
	mu sync.Mutex
	// calls are the FALLIBLE calls, in order. The index into this is what a
	// failure is injected at.
	calls []string
	// trace is every call, fallible or not, including the privileged file
	// writes -- so a test can assert ORDER across seams (the lease is
	// released before the guard starts).
	trace []string
	n     int

	failIndex  int
	panicIndex int
	onCall     func(n int, op string)
}

func newInjector() *injector { return &injector{failIndex: -1, panicIndex: -1} }

// do records a fallible call and injects whatever this run was configured for.
func (i *injector) do(op string) error {
	i.mu.Lock()
	n := i.n
	i.n++
	i.calls = append(i.calls, op)
	i.trace = append(i.trace, op)
	hook := i.onCall
	fail := n == i.failIndex
	pan := n == i.panicIndex
	i.mu.Unlock()

	if hook != nil {
		hook(n, op)
	}
	if pan {
		panic(fmt.Sprintf("injected panic at %s (call %d)", op, n))
	}
	if fail {
		return fmt.Errorf("injected failure at %s (call %d)", op, n)
	}
	return nil
}

// note records a call that is never injected into.
func (i *injector) note(op string) {
	i.mu.Lock()
	i.trace = append(i.trace, op)
	i.mu.Unlock()
}

func (i *injector) ops() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.calls...)
}

func (i *injector) traced() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.trace...)
}

func (i *injector) count(op string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	var n int
	for _, c := range i.calls {
		if c == op {
			n++
		}
	}
	return n
}

// indexOf is the position of op in the full trace, or -1.
func (i *injector) indexOf(op string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	for k, c := range i.trace {
		if c == op {
			return k
		}
	}
	return -1
}

// --------------------------------------------------------------------------
// the fake systemd

type fakeSuper struct {
	inj *injector

	mu     sync.Mutex
	active bool
	held   bool
	stops  int
	starts int

	// activeErr and heldErr make the OBSERVATIONS fail, which is a different
	// hazard from the actions failing: the guard has to treat "cannot tell"
	// as "was running", because starting a running server is a no-op and
	// leaving a stopped one down is an outage.
	activeErr error
	heldErr   error
	// failEveryStart is the terminal case of section 13.4: the start itself
	// does not work.
	failEveryStart bool
	// stopDegrades makes the ladder report a SIGKILL.
	stopDegrades bool
	// holdOnStop models lifecycle.VerbStop's HoldSet. It is on by default in
	// newRig, because that is what production does; a test that wants a stop
	// which forges no intent clears it.
	holdOnStop bool
}

func (s *fakeSuper) Active(context.Context, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeErr != nil {
		return false, s.activeErr
	}
	return s.active, nil
}

func (s *fakeSuper) Held(context.Context, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.heldErr != nil {
		return false, s.heldErr
	}
	return s.held, nil
}

// Start clears the hold, because lifecycle.VerbStart declares HoldClear and
// clears it FIRST -- before the unit start, so a failed start cannot leave a
// marker that suppresses gvm-guard.timer.
func (s *fakeSuper) Start(context.Context, string) error {
	s.mu.Lock()
	s.starts++
	s.held = false
	fail := s.failEveryStart
	s.mu.Unlock()
	if fail {
		return errors.New("fake systemd: the unit entered a failed state")
	}
	if err := s.inj.do("super.start"); err != nil {
		return err
	}
	s.mu.Lock()
	s.active = true
	s.mu.Unlock()
	return nil
}

// Stop models the HOSTILE failure: a ladder that failed after the process was
// already gone.
//
// That is deliberate. A stop that fails and leaves the server running is the
// easy case -- the guard finds it active and does nothing. A stop that fails
// having already killed the game is the case that produces the outage, so it
// is the one the fake produces.
// It also writes the hold when holdOnStop is set, which is what production
// does: a stop through LifecycleSupervisor reaches lifecycle.VerbStop, whose
// Decl declares HoldSet, and Manager.execute writes /run/gvm/<n>/hold twice --
// before the ladder and again after it, to survive
// RuntimeDirectoryPreserve=restart. A fake that diverged from that is what hid
// the job's own stop forging operator intent: ~100 injection points in
// TestNeverLeavesServerStopped were blind to the hold dimension entirely.
func (s *fakeSuper) Stop(_ context.Context, _ string, _ time.Duration) (lifecycle.Outcome, error) {
	s.mu.Lock()
	s.stops++
	if s.holdOnStop {
		s.held = true
	}
	s.mu.Unlock()
	if err := s.inj.do("super.stop"); err != nil {
		s.mu.Lock()
		s.active = false
		s.mu.Unlock()
		return lifecycle.Outcome{}, err
	}
	s.mu.Lock()
	s.active = false
	degraded := s.stopDegrades
	s.mu.Unlock()
	rung := "console"
	if degraded {
		rung = "sigkill"
	}
	return lifecycle.Outcome{Rung: rung, Degraded: degraded, Took: time.Second}, nil
}

func (s *fakeSuper) isActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *fakeSuper) counts() (stops, starts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stops, s.starts
}

// --------------------------------------------------------------------------
// the fake privileged surface

type fakeHost struct {
	inj *injector

	mu      sync.Mutex
	writes  map[hostops.FileKind]int
	removes map[hostops.FileKind]int
	bodies  map[hostops.FileKind][]byte
	fail    map[hostops.FileKind]error
}

func newFakeHost(inj *injector) *fakeHost {
	return &fakeHost{
		inj:     inj,
		writes:  map[hostops.FileKind]int{},
		removes: map[hostops.FileKind]int{},
		bodies:  map[hostops.FileKind][]byte{},
		fail:    map[hostops.FileKind]error{},
	}
}

func (h *fakeHost) Do(_ context.Context, req hostops.Request) (hostops.Result, error) {
	path, err := hostops.FilePath(req.File, req.Instance, req.Job, req.LegacyUnit)
	if err != nil {
		return hostops.Result{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	verb := "write"
	if req.Verb == hostops.VerbFileRemove {
		verb = "remove"
	}
	h.inj.note("host." + verb + ":" + string(req.File))
	if e := h.fail[req.File]; e != nil {
		return hostops.Result{}, e
	}
	switch req.Verb {
	case hostops.VerbFileWrite:
		h.writes[req.File]++
		h.bodies[req.File] = append([]byte(nil), req.Body...)
	case hostops.VerbFileRemove:
		h.removes[req.File]++
		delete(h.bodies, req.File)
	default:
		return hostops.Result{}, fmt.Errorf("fake host: unexpected verb %q", req.Verb)
	}
	return hostops.Result{Verb: req.Verb, Path: path, Changed: true}, nil
}

func (h *fakeHost) wrote(k hostops.FileKind) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.writes[k]
}

func (h *fakeHost) removed(k hostops.FileKind) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.removes[k]
}

func (h *fakeHost) body(k hostops.FileKind) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.bodies[k])
}

// --------------------------------------------------------------------------
// the fake lock
//
// It records the PATH it was handed, so the derivation is still asserted, and
// locks nothing -- a test must not create /run/gvm or /var/lib/gvm on the
// machine it runs on. The real Acquire, holder line and contention diagnostic
// are tested in lock_test.go against a temp directory and a fake Flock.

type fakeLocker struct {
	mu       sync.Mutex
	paths    []string
	holders  []lifecycle.Holder
	released int
	err      error
}

func (l *fakeLocker) Acquire(path string, h lifecycle.Holder) (Unlocker, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	l.paths = append(l.paths, path)
	l.holders = append(l.holders, h)
	return &fakeLock{l: l, path: path, holder: h}, nil
}

type fakeLock struct {
	l      *fakeLocker
	path   string
	holder lifecycle.Holder
	done   bool
}

func (f *fakeLock) Release() error {
	if f.done {
		return nil
	}
	f.done = true
	f.l.mu.Lock()
	f.l.released++
	f.l.mu.Unlock()
	return nil
}

func (f *fakeLock) Holder() lifecycle.Holder { return f.holder }
func (f *fakeLock) Path() string             { return f.path }

// --------------------------------------------------------------------------
// the fake Flock, for lock_test.go
//
// An in-process registry keyed by the file's path. It is NOT a stand-in for
// flock(2) in production -- lock_stub.go refuses there on purpose -- it exists
// so the portable half of Acquire has something to lock against here.

type fakeFlock struct {
	mu   sync.Mutex
	held map[string]bool
	err  error
}

func newFakeFlock() *fakeFlock { return &fakeFlock{held: map[string]bool{}} }

func (f *fakeFlock) TryLock(fh *os.File) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.held[fh.Name()] {
		return ErrLocked
	}
	f.held[fh.Name()] = true
	return nil
}

func (f *fakeFlock) Unlock(fh *os.File) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.held, fh.Name())
	return nil
}

// --------------------------------------------------------------------------
// the fake work seams
//
// One type per interface rather than one type implementing all of them: two
// interfaces here declare a Preflight with the same signature, so a single
// fake could not report which of the two was called -- and "which phase
// failed" is the entire question this file exists to answer.

type fakeBackups struct {
	inj *injector
	ref *archive.Ref
	// skipVerifyStamp reproduces an engine that reports an archive verified
	// and leaves VerifiedAt zero.
	skipVerifyStamp bool
	// noRef reproduces one that reports success and returns nothing.
	noRef bool
}

func (b *fakeBackups) Preflight(context.Context) error { return b.inj.do("backup.preflight") }
func (b *fakeBackups) Freeze(context.Context) error    { return b.inj.do("backup.freeze") }
func (b *fakeBackups) Thaw(context.Context) error      { return b.inj.do("backup.thaw") }

func (b *fakeBackups) Snapshot(_ context.Context, class archive.Class, reason string) (*archive.Ref, error) {
	if err := b.inj.do("backup.snapshot"); err != nil {
		return nil, err
	}
	if b.noRef {
		return nil, nil
	}
	b.ref = &archive.Ref{
		Path: "/var/backups/gvm/pz-main/pz-main-20260904-182211-" + string(class) + ".tar.gz",
		// A plausible digest, not a real one: nothing here hashes anything.
		SHA256: "1b40c9de00000000000000000000000000000000000000000000000000000000",
		Bytes:  2_251_799_813, Instance: "pz-main", Class: class, Reason: reason,
		CreatedAt: time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC),
	}
	return b.ref, nil
}

func (b *fakeBackups) Verify(_ context.Context, ref *archive.Ref) error {
	if err := b.inj.do("backup.verify"); err != nil {
		return err
	}
	if !b.skipVerifyStamp && ref != nil {
		ref.VerifiedAt = time.Date(2026, 9, 4, 18, 25, 0, 0, time.UTC)
	}
	return nil
}

func (b *fakeBackups) Prune(context.Context, *archive.Ref) error { return b.inj.do("backup.prune") }
func (b *fakeBackups) Push(context.Context, *archive.Ref) error  { return b.inj.do("backup.push") }

type fakeUpdates struct {
	inj     *injector
	build   Diff
	content Diff
	settle  Settlement
	res     Resolution
}

func (u *fakeUpdates) Preflight(context.Context) error { return u.inj.do("update.preflight") }

func (u *fakeUpdates) CheckBuild(context.Context) (Diff, error) {
	if err := u.inj.do("update.check_build"); err != nil {
		return Diff{}, err
	}
	return u.build, nil
}

func (u *fakeUpdates) CheckContent(context.Context) (Diff, error) {
	if err := u.inj.do("update.check_content"); err != nil {
		return Diff{}, err
	}
	return u.content, nil
}

func (u *fakeUpdates) Backup(context.Context) error     { return u.inj.do("update.backup") }
func (u *fakeUpdates) AppUpdate(context.Context) error  { return u.inj.do("update.app_update") }
func (u *fakeUpdates) PostVerify(context.Context) error { return u.inj.do("update.post_verify") }

func (u *fakeUpdates) Settle(context.Context) (Settlement, error) {
	if err := u.inj.do("update.settle"); err != nil {
		return Settlement{}, err
	}
	return u.settle, nil
}

func (u *fakeUpdates) Resolve(context.Context) (Resolution, error) {
	if err := u.inj.do("update.resolve"); err != nil {
		return Resolution{}, err
	}
	return u.res, nil
}

func (u *fakeUpdates) Activate(context.Context) error { return u.inj.do("update.activate") }

type fakeContents struct {
	inj  *injector
	plan ContentPlan
	res  Resolution
}

func (c *fakeContents) Plan(context.Context) (ContentPlan, error) {
	if err := c.inj.do("content.plan"); err != nil {
		return ContentPlan{}, err
	}
	return c.plan, nil
}

func (c *fakeContents) Materialize(context.Context) error { return c.inj.do("content.materialize") }
func (c *fakeContents) Activate(context.Context) error    { return c.inj.do("content.activate") }

func (c *fakeContents) Resolve(context.Context) (Resolution, error) {
	if err := c.inj.do("content.resolve"); err != nil {
		return Resolution{}, err
	}
	return c.res, nil
}

// fakeRebuilds is internal/rebuild, reduced to its two seam methods.
//
// Run reports a world that WAS touched and an archive, which is the successful
// rebuild: the enumeration in guard_test.go then injects a failure at both
// methods, and the interesting one is the second -- a rebuild that fails after
// the destroy is exactly the case section 13.3's `start_anyway after archive`
// row exists for.
type fakeRebuilds struct {
	inj  *injector
	plan ContentPlan
	res  Rebuilt
}

func (r *fakeRebuilds) Plan(context.Context) (ContentPlan, error) {
	if err := r.inj.do("rebuild.plan"); err != nil {
		return ContentPlan{}, err
	}
	return r.plan, nil
}

func (r *fakeRebuilds) Run(context.Context) (Rebuilt, error) {
	if err := r.inj.do("rebuild.run"); err != nil {
		// Mirrors internal/rebuild.Run, which returns its Outcome alongside
		// the error precisely so the "world untouched" sentence survives the
		// failure path.
		return Rebuilt{Untouched: "the rebuild failed before the destroy step, so the world " +
			"is untouched"}, err
	}
	res := r.res
	if res.Archive == nil {
		res = Rebuilt{
			WorldTouched: true,
			Running:      true,
			Archive: &archive.Ref{
				Path:     "/var/backups/gvm/pz-main/pz-main-20260904-182211-preserve.tar.gz",
				Instance: "pz-main", Class: archive.ClassPreserve, Reason: "pre-rebuild",
				Bytes:      2_576_980_377,
				SHA256:     "1b40c9de00000000000000000000000000000000000000000000000000000000",
				CreatedAt:  time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC),
				VerifiedAt: time.Date(2026, 9, 4, 18, 31, 2, 0, time.UTC),
			},
			Restore: "sudo gvm restore /var/backups/gvm/pz-main/" +
				"pz-main-20260904-182211-preserve.tar.gz --instance pz-main",
		}
	}
	return res, nil
}

type fakeArchives struct {
	inj       *injector
	instances []string
	removed   int
	checked   int
}

func (a *fakeArchives) Instances(context.Context) ([]string, error) {
	if err := a.inj.do("archives.instances"); err != nil {
		return nil, err
	}
	return a.instances, nil
}

func (a *fakeArchives) Prune(context.Context, string) (int, error) {
	if err := a.inj.do("prune.retention"); err != nil {
		return 0, err
	}
	return a.removed, nil
}

func (a *fakeArchives) Reindex(context.Context, string) error { return a.inj.do("prune.reindex") }

func (a *fakeArchives) VerifyShallow(context.Context, string) (int, error) {
	if err := a.inj.do("verify.shallow"); err != nil {
		return 0, err
	}
	return a.checked, nil
}

func (a *fakeArchives) VerifyNewestPreserve(context.Context, string) (int, error) {
	if err := a.inj.do("verify.deep_newest_preserve"); err != nil {
		return 0, err
	}
	return 1, nil
}

type fakeSweeper struct {
	inj     *injector
	desired []Recovery
}

func (s *fakeSweeper) Desired(context.Context) ([]Recovery, error) {
	if err := s.inj.do("guard.enumerate"); err != nil {
		return nil, err
	}
	return s.desired, nil
}

func (s *fakeSweeper) Recover(context.Context, Recovery) error { return s.inj.do("guard.recover") }

// --------------------------------------------------------------------------
// the environment

const testInstance = "pz-main"

// testClock advances a fixed amount per reading, so every step records a
// non-zero duration and the order of timestamps is assertable.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	return &testClock{at: time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(time.Second)
	return c.at
}

// testRig is one job's worth of fakes.
type testRig struct {
	env    *Env
	inj    *injector
	super  *fakeSuper
	host   *fakeHost
	locker *fakeLocker
	sink   *audit.MemorySink

	backups  *fakeBackups
	updates  *fakeUpdates
	contents *fakeContents
	archives *fakeArchives
	rebuilds *fakeRebuilds
	sweeper  *fakeSweeper
}

// newRig wires a complete, WORKING environment for one kind.
//
// Everything succeeds and every optional gate is open, so that the baseline
// run visits every step in the table -- which is what makes the enumeration in
// guard_test.go cover every phase rather than the happy prefix of one.
func newRig(t *testing.T, kind lifecycle.JobKind, instanceName string) *testRig {
	t.Helper()
	inj := newInjector()
	clock := newTestClock()
	r := &testRig{
		inj:      inj,
		super:    &fakeSuper{inj: inj, active: true},
		host:     newFakeHost(inj),
		locker:   &fakeLocker{},
		sink:     audit.NewMemorySink(),
		backups:  &fakeBackups{inj: inj},
		updates:  &fakeUpdates{inj: inj},
		contents: &fakeContents{inj: inj},
		archives: &fakeArchives{inj: inj, instances: []string{testInstance}, removed: 2, checked: 7},
		rebuilds: &fakeRebuilds{inj: inj},
		sweeper:  &fakeSweeper{inj: inj},
	}
	// Both axes changed, so the update pipeline runs every phase including
	// the second cycle.
	r.updates.build = Diff{Changed: true, From: "18452901", To: "18461233"}
	r.updates.content = Diff{Changed: true, Items: []string{"2169435993"}}
	r.updates.settle = Settlement{Positive: true, Waited: 45 * time.Second}
	r.updates.res = Resolution{Changed: true, Before: []string{"A"}, After: []string{"A", "B"}}
	r.contents.plan = ContentPlan{Changed: true, NeedsRestart: true, Added: []string{"2169435993"}}
	r.rebuilds.plan = ContentPlan{Changed: true, Added: []string{"2169435993"}}
	r.contents.res = Resolution{Changed: true}
	r.sweeper.desired = []Recovery{{
		Instance: testInstance,
		Reason:   "recovered after gvm-job@pz-main_nightly.service ended without restoring service",
	}}

	job := string(kind)
	if kind == lifecycle.JobContentSync {
		job = "content-sync"
	}
	r.env = &Env{
		Instance: instanceName,
		Job:      job,
		Slug:     Slug{Instance: instanceName, Job: job}.String(),
		Action:   string(kind),
		Kind:     kind,
		Unit:     "gvm-job@" + Slug{Instance: instanceName, Job: job}.String() + ".service",
		Trigger:  TriggerTimer,
		Actor:    audit.Timer(),

		ManagedInstall: true,

		Super:  r.super,
		Host:   r.host,
		Locker: r.locker,
		Audit:  audit.New(r.sink).WithLogger(discardLogger()).WithClock(clock.now),
		Log:    discardLogger(),
		Now:    clock.now,

		PID:    4242,
		BootID: "7f1c0a4e-0000-4000-8000-000000000001",

		StopBudget:    3 * time.Minute,
		RestoreBudget: time.Minute,
		LeaseFor:      4 * time.Hour,

		// Section 16.4's typed confirmation, pre-answered. It is set for
		// every kind rather than only the destructive ones so that the rig
		// stays one function: Check ignores it for a kind that destroys
		// nothing, and a test that wants the refusal clears the field.
		Confirm: &Confirmation{Typed: instanceName, ArchiveAck: true},

		Backups:  r.backups,
		Updates:  r.updates,
		Contents: r.contents,
		Archives: r.archives,
		Rebuilds: r.rebuilds,
		Sweeper:  r.sweeper,
	}
	if instanceName == "" {
		// A host-level slug's job name is the whole of it (D-13).
		r.env.Job = job
		r.env.Slug = "_" + job
		r.env.Unit = "gvm-job@_" + job + ".service"
	}
	return r
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}
