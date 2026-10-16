package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/rebuild"
)

// The job-layer fakes.
//
// internal/jobs already proves, on Windows, that no failure anywhere in a job
// leaves a server stopped. What these tests are about is the half that proof
// cannot reach: the ASSEMBLY. A jobs.Env with the wrong supervisor, a lock
// taken on the wrong path, a confirmation the CLI forgot to pass, an engine
// attached for the wrong kind -- every one of those is a defect the engine's
// own tests cannot see, because they build the Env themselves.
//
// So the fakes here stand in for the host and nothing else: the real
// internal/jobs engine, the real running guard, the real jobs.Sweep and the
// real jobs.ArchiveStore all execute.

// fakeJobSuper is a systemd that records what a job did to one instance.
type fakeJobSuper struct {
	active bool
	held   bool
	stops  int
	starts int
	err    error
}

func (s *fakeJobSuper) Active(context.Context, string) (bool, error) { return s.active, nil }
func (s *fakeJobSuper) Held(context.Context, string) (bool, error)   { return s.held, nil }

func (s *fakeJobSuper) Start(context.Context, string) error {
	s.starts++
	if s.err != nil {
		return s.err
	}
	s.active = true
	return nil
}

func (s *fakeJobSuper) Stop(context.Context, string, time.Duration) (lifecycle.Outcome, error) {
	s.stops++
	s.active = false
	return lifecycle.Outcome{Rung: "console", Took: time.Second}, nil
}

// fakeJobLocker locks nothing and records the path.
//
// It is required rather than optional: the production locker is a real
// flock(2) on /run/gvm/<n>/job.lock, and a test must not create /run on the
// machine it runs on. The path is recorded so the DERIVATION is still
// asserted -- D-13's empty instance part sends a host job's lock somewhere
// else entirely, and getting that wrong makes two different host jobs share
// one lock.
type fakeJobLocker struct {
	paths   []string
	holders []lifecycle.Holder
}

func (l *fakeJobLocker) Acquire(p string, h lifecycle.Holder) (jobs.Unlocker, error) {
	l.paths = append(l.paths, p)
	l.holders = append(l.holders, h)
	return nopJobLock{}, nil
}

type nopJobLock struct{}

func (nopJobLock) Release() error           { return nil }
func (nopJobLock) Holder() lifecycle.Holder { return lifecycle.Holder{} }
func (nopJobLock) Path() string             { return "" }

// fakeBackups is internal/backup reduced to the seam's seven methods.
type fakeBackups struct {
	calls []string
	ref   *archive.Ref
}

func (b *fakeBackups) note(op string) error { b.calls = append(b.calls, op); return nil }

func (b *fakeBackups) Preflight(context.Context) error { return b.note("preflight") }
func (b *fakeBackups) Freeze(context.Context) error    { return b.note("freeze") }
func (b *fakeBackups) Thaw(context.Context) error      { return b.note("thaw") }

func (b *fakeBackups) Snapshot(_ context.Context, c archive.Class, reason string) (*archive.Ref, error) {
	_ = b.note("snapshot")
	b.ref = &archive.Ref{
		Path:   "/var/backups/gvm/pz-main/pz-main-20260905-120000-" + string(c) + archive.Ext,
		SHA256: "1b40c9de00000000000000000000000000000000000000000000000000000000",
		Bytes:  2_251_799_813, Instance: "pz-main", Class: c, Reason: reason,
		CreatedAt: testTime,
	}
	return b.ref, nil
}

func (b *fakeBackups) Verify(_ context.Context, ref *archive.Ref) error {
	_ = b.note("verify")
	ref.VerifiedAt = testTime
	return nil
}

func (b *fakeBackups) Prune(context.Context, *archive.Ref) error { return b.note("prune") }
func (b *fakeBackups) Push(context.Context, *archive.Ref) error  { return b.note("push") }

// fakeSweeper is the host-level recovery sweep.
type fakeSweeper struct {
	want      []jobs.Recovery
	recovered []string
}

func (s *fakeSweeper) Desired(context.Context) ([]jobs.Recovery, error) { return s.want, nil }

func (s *fakeSweeper) Recover(_ context.Context, r jobs.Recovery) error {
	s.recovered = append(s.recovered, r.Instance)
	return nil
}

// fakeRebuilds is internal/rebuild's two seam methods.
type fakeRebuilds struct {
	plan jobs.ContentPlan
	runs int
}

func (r *fakeRebuilds) Plan(context.Context) (jobs.ContentPlan, error) { return r.plan, nil }

func (r *fakeRebuilds) Run(context.Context) (jobs.Rebuilt, error) {
	r.runs++
	return jobs.Rebuilt{WorldTouched: true, Running: true}, nil
}

// memDesc reads /etc/gvm/jobs out of the fake filesystem.
type memDesc struct{ m *memFS }

func (d memDesc) ReadFile(name string) ([]byte, error) { return d.m.ReadFile(name) }

// seedJobbableInstance is seedInstance plus the one directory `gvm apply`
// would have created: /var/lib/gvm/instances/<name>, which is where the lease
// goes.
//
// hostops creates one directory per call and refuses a missing parent, on
// purpose -- a verb that mkdir -p'd its way to a lease would also mkdir -p its
// way past a missing package install. A test that skipped it would be
// asserting against a box nobody has.
func seedJobbableInstance(t *testing.T, h *testHost, name string) {
	t.Helper()
	seedInstance(t, h, name, "projectzomboid", "--var", "public_name=Test")
	h.FS.mkdirAll("/var/lib/gvm/instances/" + name)
	h.FS.mkdirAll("/run/gvm/" + name)
}

// jobHost is newTestHost plus the job seams a run needs, and nothing more.
func jobHost(t *testing.T) (*testHost, *fakeJobSuper, *fakeJobLocker) {
	t.Helper()
	h := newTestHost(t)
	super := &fakeJobSuper{active: true}
	lock := &fakeJobLocker{}
	h.HostEnv.Audit = audit.New(h.Audit)
	h.HostEnv.Jobs = JobSeams{
		Super:  super,
		Locker: lock,
		Desc:   memDesc{h.FS},
		PID:    4242,
		// A discarding logger, so a test's output is the command's output.
		// Production leaves this nil and the records go to the journal.
		Log: slog.New(slog.NewTextHandler(io.Discard,
			&slog.HandlerOptions{Level: slog.LevelError + 1})),
	}
	h.HostEnv.BootID = "7f1c0a4e-0000-4000-8000-000000000001"
	return h, super, lock
}

// TestTheGuardJobRunsThroughTheRealSweep is the unit gvm-guard.timer invokes,
// end to end, with only the host faked.
//
// jobs.Sweep, lifecycle.Reconciler and the whole engine are real here. On a box
// with no instances the correct answer is `skipped` and NOT `ok`: a
// `guard` run that actually recovered something has to be findable in a month
// of five-minute rows.
func TestTheGuardJobRunsThroughTheRealSweep(t *testing.T) {
	h, super, lock := jobHost(t)

	code, out, errb := runWith(t, h, "", "job", "run", "_guard")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errb, out)
	}
	if !strings.Contains(out, "skipped _guard (guard)") {
		t.Errorf("out = %q; a sweep that found nothing is `skipped`", out)
	}
	// Section 13.4's own property, asserted at the top level: the guard kind
	// starts and never stops.
	if super.stops != 0 {
		t.Errorf("the guard job stopped something %d time(s)", super.stops)
	}
	// D-13: a host-level job's lock is NOT under any instance's runtime
	// directory, because there is no instance and /run/gvm//job.lock is a
	// lock two different host jobs would share.
	if len(lock.paths) != 1 || strings.Contains(lock.paths[0], "/run/gvm/") {
		t.Errorf("lock paths = %v; a host job locks the host-wide path", lock.paths)
	}
	if !strings.Contains(lock.paths[0], "_guard") {
		t.Errorf("lock path %q does not name the slug", lock.paths[0])
	}
	if lock.holders[0].PID != 4242 || lock.holders[0].Kind != "guard" {
		t.Errorf("holder = %+v; the line is pid:unit:kind:started_at and all four are filled "+
			"here because this is the only place that knows all four", lock.holders[0])
	}
}

// TestTheGuardJobStartsWhatTheSweepFound: the other half, with the sweep's
// answer supplied.
func TestTheGuardJobStartsWhatTheSweepFound(t *testing.T) {
	h, super, _ := jobHost(t)
	sw := &fakeSweeper{want: []jobs.Recovery{{
		Instance: "pz-main",
		Reason:   "recovered after gvm-job@pz-main_nightly.service ended without restoring service",
	}}}
	h.HostEnv.Jobs.Sweeper = sw

	code, out, errb := runWith(t, h, "", "job", "run", "_guard")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errb, out)
	}
	if len(sw.recovered) != 1 || sw.recovered[0] != "pz-main" {
		t.Errorf("recovered %v, want [pz-main]", sw.recovered)
	}
	if !strings.Contains(out, "ended without restoring service") {
		t.Errorf("out = %q; the reason is the whole value of a guard run", out)
	}
	if super.stops != 0 {
		t.Error("the guard job stopped something")
	}
}

// TestAHostLevelVerifyRunsAgainstEveryArchiveDirectory drives the real
// jobs.ArchiveStore from the real `verify` runner.
//
// An empty /var/backups/gvm is the state of a freshly provisioned box, and the
// nightly timer fires on it too. Reporting that as a failure would train the
// operator to ignore the one run that mattered.
func TestAHostLevelVerifyRunsAgainstEveryArchiveDirectory(t *testing.T) {
	h, _, lock := jobHost(t)

	code, out, errb := runWith(t, h, "", "backup", "verify")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errb, out)
	}
	for _, want := range []string{"verify.shallow", "verify.deep_newest_preserve"} {
		if !strings.Contains(out, want) {
			t.Errorf("out = %q; phase %s is missing", out, want)
		}
	}
	if len(lock.paths) != 1 || !strings.Contains(lock.paths[0], "_verify") {
		t.Errorf("lock paths = %v; a host-wide verify locks the host path", lock.paths)
	}
}

// TestTheBackupJobTakesTheLeaseAndReleasesIt.
//
// The lease is what makes `gvm start` refuse to race a running job, so an
// unaudited or unreleased one is a silent change to what an operator is
// allowed to do. Both halves are asserted because they fail differently: a
// lease never taken lets a start in, and one never released keeps starts out
// until somebody runs `gvm job clear`.
func TestTheBackupJobTakesTheLeaseAndReleasesIt(t *testing.T) {
	h, super, lock := jobHost(t)
	seedJobbableInstance(t, h, "pz-main")
	eng := &fakeBackups{}
	h.HostEnv.Jobs.Backups = eng

	code, out, errb := runWith(t, h, "", "backup", "pz-main")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errb, out)
	}
	// Section 13.2's ordering, as the step table declares it: the thaw is the
	// freeze step's own Undo, so it runs after the snapshot and before the
	// prune's view of the world -- and the prune never runs before the verify.
	if got := strings.Join(eng.calls, " "); got != "preflight freeze snapshot verify prune push thaw" {
		t.Errorf("engine calls = %q", got)
	}
	if n := h.FS.writes; len(n) == 0 {
		t.Fatal("nothing was written at all")
	}
	wrote, removed := leaseTraffic(h)
	if wrote == 0 {
		t.Error("the backup job took no lease, so `gvm start` would not refuse to race it")
	}
	if !removed {
		t.Error("the lease was never released")
	}
	// A backup never stops the server. The freeze pauses persistence; the
	// world keeps serving, which is what makes the `fail` policy affordable.
	if super.stops != 0 {
		t.Errorf("the backup job stopped the server %d time(s)", super.stops)
	}
	if !super.active {
		t.Error("the backup job left the server stopped")
	}
	if len(lock.paths) != 1 ||
		!strings.Contains(clean(lock.paths[0]), "/var/lib/gvm/locks/job_pz-main.lock") {
		t.Errorf("lock paths = %v", lock.paths)
	}
}

// leaseTraffic reports whether the lease was written and whether it is gone.
func leaseTraffic(h *testHost) (writes int, removed bool) {
	p := clean("/var/lib/gvm/instances/pz-main/lease.json")
	for _, w := range h.FS.writes {
		if clean(w) == p {
			writes++
		}
	}
	_, err := h.FS.ReadFile(p)
	return writes, err != nil
}

// TestARebuildFromTheCLIIsRefusedWithoutTheTypedName.
//
// The refusal is the ENGINE's, not this file's: section 16.4 requires one
// server-side check for the CLI, the API and the dashboard, so what the CLI
// contributes is passing the typed name through. This asserts both ends of
// that -- refused without it, and nothing stopped -- which is the property a
// `--yes` default or a forgotten flag would break.
func TestARebuildFromTheCLIIsRefusedWithoutTheTypedName(t *testing.T) {
	h, super, lock := jobHost(t)
	seedJobbableInstance(t, h, "pz-main")
	rb := &fakeRebuilds{}
	h.HostEnv.Jobs.Rebuilds = rb

	code, _, errb := runWith(t, h, "", "rebuild", "pz-main")
	if code == 0 {
		t.Fatal("a rebuild with no typed confirmation ran")
	}
	if !strings.Contains(errb, "type the instance name") {
		t.Errorf("stderr = %q; it does not say what to do", errb)
	}
	if !strings.Contains(errb, "DELETES") {
		t.Errorf("stderr = %q; it does not say what a rebuild destroys", errb)
	}
	if rb.runs != 0 || super.stops != 0 {
		t.Errorf("a refused rebuild ran %d time(s) and stopped %d time(s)", rb.runs, super.stops)
	}
	if len(lock.paths) != 0 {
		t.Errorf("a refused confirmation took the job lock: %v", lock.paths)
	}

	// The wrong name is refused too, and says so rather than silently
	// behaving like no answer at all.
	code, _, errb = runWith(t, h, "", "rebuild", "pz-main", "--confirm", "pz-test")
	if code == 0 {
		t.Fatal("a rebuild confirmed with another instance's name ran")
	}
	if !strings.Contains(errb, `"pz-test" is not "pz-main"`) {
		t.Errorf("stderr = %q", errb)
	}

	// And with the name typed it runs, through the real engine, with the
	// guard armed and the server up at the end.
	code, out, errb := runWith(t, h, "", "rebuild", "pz-main", "--confirm", "pz-main")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errb, out)
	}
	if rb.runs != 1 {
		t.Errorf("the rebuild ran %d time(s)", rb.runs)
	}
	if !super.active {
		t.Error("the rebuild left the server stopped")
	}
}

// TestRebuildFlagsReachTheEngine: --keep-world, --dry and --no-fetch are
// section 13.6's own command line, and a flag the CLI accepted and dropped
// would be a flag that silently performed a full rebuild.
//
// The assertion goes through attachEngines, which is where the options are
// applied, rather than through a fake engine that could not report them.
func TestRebuildFlagsReachTheEngine(t *testing.T) {
	h, _, _ := jobHost(t)
	seedJobbableInstance(t, h, "pz-main")
	job := &rebuild.Job{}
	h.HostEnv.Jobs.Rebuilds = job

	jenv := &jobs.Env{Kind: lifecycle.JobRebuild, Instance: "pz-main", Slug: "pz-main_rebuild"}
	eo := engineOptions{Rebuild: rebuild.Options{
		KeepWorld: true, NoFetch: true, Typed: "pz-main",
	}}
	if err := h.HostEnv.attachEngines(context.Background(), jenv, eo); err != nil {
		t.Fatal(err)
	}
	if job.Opt != eo.Rebuild {
		t.Errorf("rebuild options = %+v, want %+v", job.Opt, eo.Rebuild)
	}
	if jenv.Rebuilds == nil {
		t.Error("the rebuild engine was not attached")
	}

	// And the flags parse off the command line into exactly those fields.
	//
	// The run itself fails: a bare rebuild.Job has no content pipeline, so
	// step 1 refuses. That is the right failure and it is asserted too --
	// section 13.6's first step is reads-only and refuses before anything
	// stops, so the world is untouched and the message says which piece is
	// missing rather than panicking inside a job that had already stopped a
	// server.
	job.Opt = rebuild.Options{}
	code, out, errb := runWith(t, h, "",
		"rebuild", "pz-main", "--confirm", "pz-main", "--keep-world", "--no-fetch")
	if code == 0 {
		t.Fatal("a rebuild with no content pipeline reported success")
	}
	if !strings.Contains(errb+out, "needs a content pipeline") {
		t.Errorf("stderr = %q", errb)
	}
	if !job.Opt.KeepWorld || !job.Opt.NoFetch || job.Opt.Typed != "pz-main" {
		t.Errorf("the flags did not reach the engine: %+v", job.Opt)
	}
}

// TestTheRestoreKindNamesThePackageThatWillBringIt.
//
// `pending` in internal/jobs is the one place a tracked gap is recorded, and
// the CLI asks jobs.Lookup rather than inventing a second sentence. A command
// that said "not implemented" here would be indistinguishable from a command
// that does not exist.
func TestTheRestoreKindNamesThePackageThatWillBringIt(t *testing.T) {
	h, super, _ := jobHost(t)
	seedJobbableInstance(t, h, "pz-main")

	code, _, errb := runWith(t, h, "", "job", "run", "pz-main_restore")
	if code == 0 {
		t.Fatal("a restore ran in a build with no restore path")
	}
	if !strings.Contains(errb, "internal/backup's restore path") {
		t.Errorf("stderr = %q; it does not name what is missing", errb)
	}
	if strings.Contains(errb, "not implemented") {
		t.Errorf("stderr = %q; `not implemented` is the sentinel for a command that does not "+
			"exist, and this one does", errb)
	}
	if super.stops != 0 {
		t.Error("a refused restore stopped the server")
	}
}

// TestNoPreArchiveNeedsTheInstanceNameTyped.
//
// Skipping step 2 means the live world is overwritten with no copy of what was
// there, which is the one thing in this subsystem no later command can undo.
// `--yes` does not satisfy it: automation may authorise a restore, and this is
// a second decision.
func TestNoPreArchiveNeedsTheInstanceNameTyped(t *testing.T) {
	h, _, _ := jobHost(t)
	seedJobbableInstance(t, h, "pz-main")

	for _, args := range [][]string{
		{"restore", "/var/backups/gvm/pz-main/x.tar.gz", "--instance", "pz-main", "--no-pre-archive"},
		{"restore", "/var/backups/gvm/pz-main/x.tar.gz", "--instance", "pz-main",
			"--no-pre-archive", "--yes"},
		{"restore", "/var/backups/gvm/pz-main/x.tar.gz", "--instance", "pz-main",
			"--no-pre-archive", "--confirm", "pz-test"},
	} {
		code, _, errb := runWith(t, h, "", args...)
		if code == 0 {
			t.Fatalf("%v: accepted", args)
		}
		if !strings.Contains(errb, "nothing in gvm can undo that") {
			t.Errorf("%v: stderr = %q", args, errb)
		}
	}
}

// TestARelativeArchivePathIsRefused: a restore runs as a systemd job whose
// working directory is not the operator's shell, so a relative path would
// resolve to an archive nobody chose.
func TestARelativeArchivePathIsRefused(t *testing.T) {
	h, _, _ := jobHost(t)
	seedJobbableInstance(t, h, "pz-main")

	code, _, errb := runWith(t, h, "", "restore", "backup.tar.gz", "--instance", "pz-main")
	if code == 0 {
		t.Fatal("accepted a relative archive path")
	}
	if !strings.Contains(errb, "absolute path") {
		t.Errorf("stderr = %q", errb)
	}
}

// TestRestoreWithoutAnInstanceRefusesRatherThanGuessing.
//
// The archive's own manifest names the instance it came from, and believing it
// would let a tarball's contents choose which live world to overwrite.
func TestRestoreWithoutAnInstanceRefusesRatherThanGuessing(t *testing.T) {
	h, _, _ := jobHost(t)
	code, _, errb := runWith(t, h, "", "restore", "/var/backups/gvm/pz-main/x.tar.gz")
	if code == 0 {
		t.Fatal("accepted a restore with no target instance")
	}
	if !strings.Contains(errb, "--instance") {
		t.Errorf("stderr = %q", errb)
	}
}

// TestJobListAnswersWhatThisBoxRunsAndWhatItCannot.
//
// A timer that fires nightly against a kind nothing runs is the least
// observable failure in this subsystem, so the listing answers it directly
// rather than leaving an operator to read `pending` in the source.
func TestJobListAnswersWhatThisBoxRunsAndWhatItCannot(t *testing.T) {
	h, _, _ := jobHost(t)
	seedJobbableInstance(t, h, "pz-main")

	code, out, errb := runWith(t, h, "", "job", "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	for _, want := range []string{"_guard", "_verify", "OnUnitActiveSec=5min"} {
		if !strings.Contains(out, want) {
			t.Errorf("out = %q; %q is missing", out, want)
		}
	}
	// The instance's own jobs, from its YAML and not from a database.
	in := mustReadInstance(t, h, "pz-main")
	for name := range in.Spec.Jobs {
		if !strings.Contains(out, "pz-main_"+name) {
			t.Errorf("out = %q; pz-main_%s is missing", out, name)
		}
	}

	// JSON carries the runnable flag, which is what a dashboard needs.
	code, out, _ = runWith(t, h, "", "job", "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var rows []jobRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var sawGuard bool
	for _, r := range rows {
		if r.Slug == "_guard" {
			sawGuard = true
			if !r.Runnable {
				t.Error("_guard is reported as not runnable")
			}
			if r.Stops {
				t.Error("_guard is reported as stopping the server; it starts, never stops")
			}
		}
	}
	if !sawGuard {
		t.Errorf("no _guard row in %s", out)
	}
}

// TestJobListPZNightlyIsARunnableRestart: Project Zomboid's recommended nightly
// is declared as an `update`, but a game with mods cannot run an update in this
// build, and PZ's nightly exists for the restart that clears heap pressure. So
// create writes it as a `restart`, and `gvm job list` shows a runnable restart
// row — not an `update` row that fires nightly against a kind nothing runs
// (cli-ergonomics-1). Phase 44 restores the update action.
func TestJobListPZNightlyIsARunnableRestart(t *testing.T) {
	h, _, _ := jobHost(t)
	seedJobbableInstance(t, h, "pz-main")

	code, out, errb := runWith(t, h, "", "job", "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var rows []jobRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, r := range rows {
		if r.Kind == "update" {
			t.Errorf("pz-main still has an update row; its nightly should be a restart: %+v", r)
		}
	}
	var sawNightly bool
	for _, r := range rows {
		if r.Job != "nightly" {
			continue
		}
		sawNightly = true
		if r.Kind != "restart" {
			t.Errorf("pz-main nightly kind = %q, want restart", r.Kind)
		}
		if !r.Runnable {
			t.Errorf("pz-main's restart nightly should be runnable: %+v", r)
		}
	}
	if !sawNightly {
		t.Fatalf("no nightly row for pz-main; rows = %s", out)
	}
}

// TestJobClearRefusesALiveLease.
//
// A live lease is a running job. Removing it lets `gvm start` race an update
// that may still have the install tree open, which is the collision the lease
// exists to prevent -- so the refusal names the holder and the flag rather
// than making the decision for the operator.
func TestJobClearRefusesALiveLease(t *testing.T) {
	h, _, _ := jobHost(t)
	seedJobbableInstance(t, h, "pz-main")

	l := lifecycle.Lease{
		Instance: "pz-main", Kind: lifecycle.JobUpdate, Unit: "gvm-job@pz-main_nightly.service",
		PID: 4242, BootID: h.HostEnv.BootID, StartedAt: testTime,
		Deadline: testTime.Add(4 * time.Hour), Reason: "update",
	}
	body, err := l.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	h.FS.put("/var/lib/gvm/instances/pz-main/lease.json", body, 0o640)

	code, _, errb := runWith(t, h, "", "job", "clear", "pz-main")
	if code == 0 {
		t.Fatal("cleared a live lease")
	}
	if !strings.Contains(errb, "gvm-job@pz-main_nightly.service") {
		t.Errorf("stderr = %q; it does not name the holder", errb)
	}
	if _, err := h.FS.ReadFile(clean("/var/lib/gvm/instances/pz-main/lease.json")); err != nil {
		t.Error("the refusal removed the lease anyway")
	}

	// --force is the operator saying they know.
	code, out, errb := runWith(t, h, "", "job", "clear", "pz-main", "--force")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "cleared the lease") {
		t.Errorf("out = %q", out)
	}
	if _, err := h.FS.ReadFile(clean("/var/lib/gvm/instances/pz-main/lease.json")); err == nil {
		t.Error("--force did not remove the lease")
	}
}

// TestNoLeaseIsNotAnError: `gvm job clear` on a healthy instance is a no-op
// that says so, because an operator reaching for it is already worried.
func TestNoLeaseIsNotAnError(t *testing.T) {
	h, _, _ := jobHost(t)
	seedJobbableInstance(t, h, "pz-main")

	code, out, errb := runWith(t, h, "", "job", "clear", "pz-main")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "nothing to clear") {
		t.Errorf("out = %q", out)
	}
}

// TestAJobRunRecordsAnAuditSpan: section 14.8's job.run row, with the kind and
// left_stopped on it, for every run including one that did nothing.
func TestAJobRunRecordsAnAuditSpan(t *testing.T) {
	h, _, _ := jobHost(t)

	if code, _, errb := runWith(t, h, "", "job", "run", "_guard"); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var found bool
	for _, e := range h.Audit.Records() {
		if e.Action != audit.JobRun {
			continue
		}
		found = true
		if e.Params["kind"] != "guard" {
			t.Errorf("job.run params = %v; kind is required", e.Params)
		}
	}
	if !found {
		t.Error("no job.run audit row; every job run is auditable")
	}
}

// TestTheTriggerIsTimerForAUnitInvokedRunAndCLIForAnOperatorVerb.
//
// Section 14.4 records the trigger because the same failure means different
// things from different ones: a nightly `update` that fails at 05:03 has
// nobody watching. `gvm job run` is the unit's ExecStart, so UNDER A UNIT
// (systemd sets INVOCATION_ID) it is a timer run; typed at a keyboard it is a
// cli run, and so is every operator verb like `gvm backup`.
func TestTheTriggerIsTimerForAUnitInvokedRunAndCLIForAnOperatorVerb(t *testing.T) {
	for _, tc := range []struct {
		args      []string
		underUnit bool
		want      jobs.Trigger
		who       audit.ActorKind
	}{
		{args: []string{"job", "run", "_guard"}, underUnit: true, want: jobs.TriggerTimer, who: audit.ActorTimer},
		{args: []string{"job", "run", "_guard"}, underUnit: false, want: jobs.TriggerCLI, who: audit.ActorRootCLI},
		{args: []string{"backup", "verify"}, want: jobs.TriggerCLI, who: audit.ActorRootCLI},
		{
			args: []string{"job", "run", "_guard", "--trigger", "cli"}, underUnit: true,
			want: jobs.TriggerCLI, who: audit.ActorRootCLI,
		},
	} {
		name := strings.Join(tc.args, "-")
		if tc.underUnit {
			name += "-under-unit"
		}
		t.Run(name, func(t *testing.T) {
			h, _, _ := jobHost(t)
			getenv := func(string) string { return "" }
			if tc.underUnit {
				getenv = func(k string) string {
					if k == "INVOCATION_ID" {
						return "unit-abc"
					}
					return ""
				}
			}
			var outb, errbuf strings.Builder
			env := &Env{
				Args: tc.args, Stdin: strings.NewReader(""), Stdout: &outb, Stderr: &errbuf,
				Getenv: getenv, Geteuid: func() int { return 0 }, Host: h.HostEnv,
			}
			code := Main(context.Background(), env)
			out, errb := outb.String(), errbuf.String()
			if code != 0 {
				t.Fatalf("exit %d: %s\n%s", code, errb, out)
			}
			var kinds []audit.ActorKind
			for _, e := range h.Audit.Records() {
				if e.Action == audit.JobRun {
					kinds = append(kinds, e.Kind)
				}
			}
			if len(kinds) == 0 {
				t.Fatal("no job.run row")
			}
			if kinds[0] != tc.who {
				t.Errorf("actor = %q, want %q", kinds[0], tc.who)
			}
		})
	}
}

// TestAnUnknownTriggerIsRefusedRatherThanRecordedWrongly.
func TestAnUnknownTriggerIsRefusedRatherThanRecordedWrongly(t *testing.T) {
	h, _, _ := jobHost(t)
	code, _, errb := runWith(t, h, "", "job", "run", "_guard", "--trigger", "cron")
	if code == 0 {
		t.Fatal("accepted a trigger that is not in the schema")
	}
	if !strings.Contains(errb, "cron") {
		t.Errorf("stderr = %q", errb)
	}
}

// TestAJobSlugWithTwoSeparatorsIsRefused is D-45 at the CLI boundary: "a_b_c"
// has two legal readings and a job layer that picked one would address the
// wrong instance with a verb that stops servers.
func TestAJobSlugWithTwoSeparatorsIsRefused(t *testing.T) {
	h, _, _ := jobHost(t)
	code, _, errb := runWith(t, h, "", "job", "run", "pz_main_nightly")
	if code == 0 {
		t.Fatal("accepted an ambiguous slug")
	}
	if !strings.Contains(errb, "_") {
		t.Errorf("stderr = %q", errb)
	}
}

// TestEveryKindTheCLICanAssembleArmsTheGuard is the CLI's half of the single
// most important promise in the project.
//
// internal/jobs proves, on Windows, that no failure anywhere in a job leaves a
// server stopped. That proof takes the jobs.Env as given -- so it cannot see
// the one way this package could break it: an Env assembled without a
// supervisor, without a privileged surface or without an audit sink. The
// engine refuses such an Env (validate does, before the lock), which means the
// failure would not be a stopped server -- but it would be a nightly that
// never ran, every night, which is the failure section 13.3 calls the least
// observable in the subsystem.
//
// So this asserts the assembly: for every kind this build can execute against
// an instance, the Env the CLI produces carries everything the running guard
// needs. It enumerates from jobs.Kinds() rather than from a list here, so a
// kind added next year is covered the day it is added.
func TestEveryKindTheCLICanAssembleArmsTheGuard(t *testing.T) {
	for _, kind := range jobs.Kinds() {
		r, err := jobs.Lookup(kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !r.Scope.Accepts("pz-main") {
			continue
		}
		t.Run(string(kind), func(t *testing.T) {
			h, _, _ := jobHost(t)
			seedJobbableInstance(t, h, "pz-main")
			// Every work seam supplied, because what is under test is the
			// assembly and not the engines: a kind whose adapter this build
			// does not have yet would otherwise refuse before the Env was
			// finished, and the refusal would hide the thing being asserted.
			h.HostEnv.Jobs.Backups = &fakeBackups{}
			h.HostEnv.Jobs.Rebuilds = &fakeRebuilds{}
			h.HostEnv.Jobs.Sweeper = &fakeSweeper{}
			h.HostEnv.Jobs.Updates = nil
			h.HostEnv.Jobs.Contents = nil

			action := actionFor(kind)
			jenv, err := buildJobEnv(context.Background(), &Env{}, h.HostEnv, jobs.Resolved{
				Slug: jobs.Slug{Instance: "pz-main", Job: jobNameFor(action)}, Kind: kind, Action: action,
			}, jobOptions{Confirm: "pz-main"})
			if err != nil {
				// An honest gap is allowed and is named; anything else is a
				// broken assembly.
				if errors.Is(err, ErrProviderUnbuilt) || errors.Is(err, ErrContentApplyUnbuilt) {
					t.Skipf("tracked gap: %v", err)
				}
				t.Fatalf("the CLI could not assemble a %s: %v", kind, err)
			}
			if jenv.Super == nil {
				t.Error("no supervisor: the running guard could not observe or restore the " +
					"instance, and the engine would refuse the run")
			}
			if jenv.Host == nil {
				t.Error("no privileged surface: no lease would be taken and no breadcrumb " +
					"left if the guard's own start failed")
			}
			if jenv.Audit == nil {
				t.Error("no audit sink: the engine refuses a run it cannot record")
			}
			if jenv.Locker == nil {
				t.Error("no locker: two nightlies could overlap")
			}
			if jenv.BootID == "" {
				t.Error("no boot id: a lease from before the reboot would be undecidable")
			}
			if jenv.PID == 0 {
				t.Error("no pid: the lock's holder line could not name the process holding it")
			}
			if jenv.LeaseFor == 0 {
				t.Error("no lease deadline: a SIGKILLed job's lease would refuse starts forever")
			}
			// And the Env it produced is one the engine accepts, which is the
			// assertion that cannot be made field by field.
			if _, err := jobs.StepNames(jenv); err != nil {
				t.Errorf("the engine would not build a step table from it: %v", err)
			}
		})
	}
}

// TestAFailingBackupThroughTheCLILeavesTheServerServing is the promise itself,
// driven from argv.
//
// The engine's own enumeration covers every seam of every kind; this covers
// the path an unattended nightly actually takes -- `gvm backup pz-main` from
// the timer's unit -- and asserts the two outcomes section 13.4 allows and no
// third one. It injects at each phase of the real step table in turn, so a
// phase added to internal/backup's seam is covered here too.
func TestAFailingBackupThroughTheCLILeavesTheServerServing(t *testing.T) {
	phases := []string{"preflight", "freeze", "snapshot", "verify", "prune", "push", "thaw"}
	for _, at := range phases {
		t.Run(at, func(t *testing.T) {
			h, super, _ := jobHost(t)
			seedJobbableInstance(t, h, "pz-main")
			h.HostEnv.Jobs.Backups = &failingBackups{failAt: at}

			code, out, errb := runWith(t, h, "", "backup", "pz-main")
			_ = code
			// The invariant. Either the world is serving, or the run says in
			// as many words that it is not and left the breadcrumb the guard
			// timer acts on.
			if super.active {
				if strings.Contains(out, "THE SERVER IS STOPPED") {
					t.Errorf("the server is up and the report says it is down:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, "THE SERVER IS STOPPED") {
				t.Errorf("the server is stopped and nothing said so:\n%s%s", out, errb)
			}
			if _, err := h.FS.ReadFile(clean("/run/gvm/pz-main/degraded")); err != nil {
				t.Error("the server is stopped and no breadcrumb was written; " +
					"gvm-guard.timer has nothing to act on")
			}
		})
	}
}

// failingBackups fails exactly one phase, and models the hostile shape: a
// phase that fails having already had its effect.
type failingBackups struct {
	fakeBackups
	failAt string
}

func (b *failingBackups) check(op string) error {
	_ = b.fakeBackups.note(op)
	if op == b.failAt {
		return errors.New("injected failure at " + op)
	}
	return nil
}

func (b *failingBackups) Preflight(context.Context) error { return b.check("preflight") }
func (b *failingBackups) Freeze(context.Context) error    { return b.check("freeze") }
func (b *failingBackups) Thaw(context.Context) error      { return b.check("thaw") }

func (b *failingBackups) Snapshot(ctx context.Context, c archive.Class, reason string) (*archive.Ref, error) {
	if err := b.check("snapshot"); err != nil {
		return nil, err
	}
	return b.fakeBackups.Snapshot(ctx, c, reason)
}

func (b *failingBackups) Verify(ctx context.Context, ref *archive.Ref) error {
	if err := b.check("verify"); err != nil {
		return err
	}
	return b.fakeBackups.Verify(ctx, ref)
}

func (b *failingBackups) Prune(context.Context, *archive.Ref) error { return b.check("prune") }
func (b *failingBackups) Push(context.Context, *archive.Ref) error  { return b.check("push") }

// TestTheShippedHostTimersRunWhatTheyInvoke.
//
// packaging/systemd ships gvm-guard.timer and gvm-job@_verify.timer, and both
// of their services are `gvm job run %i` with an empty instance part. The
// packaging tests assert the units are well formed and that the job layer can
// resolve those slugs; this asserts the other half, which neither of them can:
// that the command actually RUNS, with the engines the CLI assembles for it, on
// a box where nothing else has happened yet.
//
// It is worth its own test because the failure it catches is silent. A timer
// whose service exits non-zero is visible in `systemctl list-timers` only as a
// failed unit nobody is looking at, and the two jobs here are precisely the
// ones whose whole purpose is to notice things nobody is looking at.
func TestTheShippedHostTimersRunWhatTheyInvoke(t *testing.T) {
	for _, slug := range []string{"_guard", "_verify"} {
		t.Run(slug, func(t *testing.T) {
			h, super, lock := jobHost(t)

			code, out, errb := runWith(t, h, "", "job", "run", slug)
			if code != 0 {
				t.Fatalf("exit %d: %s\n%s", code, errb, out)
			}
			// A host-level job holds no instance's lease and arms no running
			// guard: there is no single instance to guard, and a job that
			// locked one instance while starting another would block that
			// instance's own nightly.
			if n := len(lock.paths); n != 1 {
				t.Fatalf("took %d lock(s): %v", n, lock.paths)
			}
			if strings.Contains(clean(lock.paths[0]), "/run/gvm/") {
				t.Errorf("lock %q is in an instance's runtime directory", lock.paths[0])
			}
			if super.stops != 0 {
				t.Errorf("%s stopped something %d time(s)", slug, super.stops)
			}
			for _, w := range h.FS.writes {
				if strings.HasSuffix(clean(w), "/lease.json") {
					t.Errorf("%s took a lease on %s", slug, w)
				}
			}
		})
	}
}
