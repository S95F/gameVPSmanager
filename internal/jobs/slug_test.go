package jobs

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// mapDescFS is /etc/gvm/jobs as a map. Absent keys report fs.ErrNotExist, so
// the "no descriptor yet" branch is the real one rather than a special case.
type mapDescFS map[string]string

func (m mapDescFS) ReadFile(name string) ([]byte, error) {
	b, ok := m[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return []byte(b), nil
}

// TestASlugSplitsUnambiguously is D-13 and D-45 meeting.
//
// The whole reason an instance name may not contain '_' is this function.
// Given "a_b_c" there are two legal readings, and a job layer that picked one
// would address the wrong instance with a verb that stops servers -- so two
// separators is a refusal, not a guess.
func TestASlugSplitsUnambiguously(t *testing.T) {
	tests := []struct {
		slug     string
		instance string
		job      string
		host     bool
		err      bool
	}{
		{slug: "pz-main_nightly", instance: "pz-main", job: "nightly"},
		{slug: "pz-main_backup", instance: "pz-main", job: "backup"},
		{slug: "vh1_restart", instance: "vh1", job: "restart"},
		// The two host-level jobs, spelt with an empty instance part.
		{slug: "_guard", job: "guard", host: true},
		{slug: "_verify", job: "verify", host: true},

		// An empty instance part is only legal for a job that IS host-level.
		// Without this rule "_nightly" would parse, resolve to no instance,
		// and be handed to a runner that derives paths from an empty name.
		{slug: "_nightly", err: true},
		{slug: "_backup", err: true},
		// Two separators: unreadable, so refused.
		{slug: "pz_main_nightly", err: true},
		// No separator at all.
		{slug: "pz-main", err: true},
		{slug: "", err: true},
		// A trailing empty job name.
		{slug: "pz-main_", err: true},
		// A reserved instance name is still reserved in a slug.
		{slug: "all_nightly", err: true},
		// Uppercase: SteamCMD's force_install_dir fails on paths with
		// capitals on some distributions (D-45).
		{slug: "PZ-Main_nightly", err: true},
	}
	for _, tc := range tests {
		got, err := ParseSlug(tc.slug)
		if tc.err {
			if err == nil {
				t.Errorf("ParseSlug(%q) = %+v, want a refusal", tc.slug, got)
			} else if !errors.Is(err, ErrSlug) && !errors.Is(err, instance.ErrName) {
				t.Errorf("ParseSlug(%q) error %v wraps neither ErrSlug nor instance.ErrName",
					tc.slug, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSlug(%q): %v", tc.slug, err)
			continue
		}
		if got.Instance != tc.instance || got.Job != tc.job {
			t.Errorf("ParseSlug(%q) = %q/%q, want %q/%q",
				tc.slug, got.Instance, got.Job, tc.instance, tc.job)
		}
		if got.Host() != tc.host {
			t.Errorf("ParseSlug(%q).Host() = %v, want %v", tc.slug, got.Host(), tc.host)
		}
		// And it round-trips, because the slug is also the systemd template
		// parameter: a split that did not recompose would mean the unit gvm
		// names and the unit systemd started could differ.
		if got.String() != tc.slug {
			t.Errorf("ParseSlug(%q).String() = %q", tc.slug, got.String())
		}
	}
}

// TestGuardAndVerifyAreTheHostLevelJobs pins D-13's two names and the kind
// each one resolves to with no descriptor on disk at all -- which is the
// situation `gvm-guard.timer` runs in on a box where /etc/gvm/jobs is empty.
func TestGuardAndVerifyAreTheHostLevelJobs(t *testing.T) {
	if got := HostJobs(); !slices.Equal(got, []string{"guard", "verify"}) {
		t.Fatalf("HostJobs() = %v, want [guard verify]", got)
	}
	// An empty filesystem: a host job must need nothing from it.
	empty := mapDescFS{}
	for slug, want := range map[string]lifecycle.JobKind{
		"_guard":  lifecycle.JobGuard,
		"_verify": lifecycle.JobVerify,
	} {
		got, err := Resolve(empty, slug)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", slug, err)
		}
		if got.Kind != want {
			t.Errorf("Resolve(%q).Kind = %q, want %q", slug, got.Kind, want)
		}
		if got.Via != ViaHostJob {
			t.Errorf("Resolve(%q).Via = %q, want %q", slug, got.Via, ViaHostJob)
		}
		if !got.Slug.Host() {
			t.Errorf("Resolve(%q) is not host-level", slug)
		}
		if _, err := Lookup(got.Kind); err != nil {
			t.Errorf("Resolve(%q) resolved to a kind with no runner: %v", slug, err)
		}
	}
}

// TestADescriptorTurnsAJobNameIntoAnAction is D-29's promise in one test: the
// trigger is systemd and the ledger is Postgres, so "what does pz-main_nightly
// do" is answerable from a file.
func TestADescriptorTurnsAJobNameIntoAnAction(t *testing.T) {
	path := instance.JobDescPath("pz-main", "nightly")
	fsys := mapDescFS{path: `{"instance":"pz-main","job":"nightly","action":"update"}`}

	got, err := Resolve(fsys, "pz-main_nightly")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != lifecycle.JobUpdate {
		t.Errorf("Kind = %q, want update", got.Kind)
	}
	if got.Action != "update" || got.Via != ViaDescriptor {
		t.Errorf("Action/Via = %q/%q", got.Action, got.Via)
	}
	if got.Slug.DescriptorPath() != path {
		t.Errorf("DescriptorPath() = %q, want %q", got.Slug.DescriptorPath(), path)
	}
}

// TestADescriptorThatDisagreesWithItsOwnNameIsRefused.
//
// The only ways to produce one are a hand edit and a copy between boxes, and
// the consequence of believing it is a job that stops the wrong world. There
// is no reading of this file that is safe to act on.
func TestADescriptorThatDisagreesWithItsOwnNameIsRefused(t *testing.T) {
	path := instance.JobDescPath("pz-main", "nightly")
	for _, body := range []string{
		`{"instance":"vh1","job":"nightly","action":"update"}`,
		`{"instance":"pz-main","job":"weekly","action":"update"}`,
	} {
		_, err := Resolve(mapDescFS{path: body}, "pz-main_nightly")
		if err == nil {
			t.Fatalf("accepted %s", body)
		}
		if !errors.Is(err, ErrDescriptor) {
			t.Errorf("err = %v, want ErrDescriptor", err)
		}
		if !strings.Contains(err.Error(), "wrong instance") {
			t.Errorf("err = %v; it has to say what would go wrong", err)
		}
	}
}

// TestACorruptDescriptorIsARefusalAndNeverAFallback.
//
// Falling through to the job name here would turn a corrupt file into a
// silently DIFFERENT action -- a "nightly" that quietly became a rebuild. That
// is the one outcome worse than not running.
func TestACorruptDescriptorIsARefusalAndNeverAFallback(t *testing.T) {
	path := instance.JobDescPath("pz-main", "backup")
	for _, body := range []string{
		`not json`,
		`{"instance":"pz-main","job":"backup"}`,
		`{"instance":"pz-main","job":"backup","action":"rm -rf /"}`,
		`{"instance":"pz-main","job":"backup","action":"backup","extra":1}`,
	} {
		got, err := Resolve(mapDescFS{path: body}, "pz-main_backup")
		if err == nil {
			t.Errorf("accepted %q as %q; the job name would have resolved it, and that is "+
				"exactly what must not happen", body, got.Kind)
		}
	}
}

// TestAMissingDescriptorFallsBackToTheJobNameOnlyWhenThatIsAnAction.
//
// `gvm backup pz-main` has to work during an adoption, before `gvm apply` has
// written any descriptors. A job called "backup" doing a backup is not a
// guess. A job called "nightly" has no such reading, and the refusal names the
// file it wanted and the command that writes it.
func TestAMissingDescriptorFallsBackToTheJobNameOnlyWhenThatIsAnAction(t *testing.T) {
	empty := mapDescFS{}

	got, err := Resolve(empty, "pz-main_backup")
	if err != nil {
		t.Fatalf("pz-main_backup with no descriptor: %v", err)
	}
	if got.Kind != lifecycle.JobBackup || got.Via != ViaJobName {
		t.Errorf("Kind/Via = %q/%q, want backup/%q", got.Kind, got.Via, ViaJobName)
	}

	_, err = Resolve(empty, "pz-main_nightly")
	if err == nil {
		t.Fatal("pz-main_nightly with no descriptor resolved to something")
	}
	if !strings.Contains(err.Error(), instance.JobDescPath("pz-main", "nightly")) {
		t.Errorf("err = %v; it has to name the file it wanted", err)
	}
	if !strings.Contains(err.Error(), "gvm apply") {
		t.Errorf("err = %v; it has to name the command that writes it", err)
	}
}

// TestAReadErrorIsNotAMissingFile.
//
// A descriptor gvm cannot read for any reason OTHER than absence is a refusal.
// Treating EACCES or EIO as "not there" would silently take the job-name
// fallback on a box with a permissions problem.
func TestAReadErrorIsNotAMissingFile(t *testing.T) {
	_, err := Resolve(errDescFS{errors.New("input/output error")}, "pz-main_backup")
	if err == nil {
		t.Fatal("an unreadable descriptor fell back to the job name")
	}
	if !errors.Is(err, ErrSlug) {
		t.Errorf("err = %v, want ErrSlug", err)
	}
}

type errDescFS struct{ err error }

func (e errDescFS) ReadFile(string) ([]byte, error) { return nil, e.err }

// TestNoSchedulableActionNeedsAnOperatorToTypeAName.
//
// The gap the test below cannot see. Its invariant is "has a runner", and
// `rebuild` HAS one -- so a scheduled rebuild passed validation, got a
// descriptor and a timer from `gvm apply`, and then failed on every single
// firing with "type the instance name (pz-main) to continue".
//
// It is structural and not a wiring oversight. §16.4 requires a destructive
// job to carry an instance name a person typed; gvm-job@.service's ExecStart
// is a bare `gvm job run %i` with no flags, and jobs.Descriptor is three
// fields with DisallowUnknownFields -- so neither the unit nor the descriptor
// has anywhere to put consent. Runner.validate checks it above the audit span,
// so Execute returns before the lock, the span and the step table: there is no
// job_runs row, no audit row, and nothing in `gvm job list` to look at.
// `jobRow.Runnable` reports true, because the build can indeed execute the
// kind. The whole trace is a failed unit and a journal line, forever.
//
// §13.3's table enumerates the KINDS the engine runs, which is a different
// list; the spec's own canonical instance YAML enumerates what a jobs block
// may say -- `update | restart | backup | content_sync` -- and rebuild is not
// in it.
//
// Asserted here rather than in internal/spec because internal/jobs imports
// internal/spec and not the reverse. It makes the invariant a property of the
// code: adding a destructive kind to jobActions, or giving a schedulable kind
// a riskOf sentence, fails the build instead of producing a nightly that
// cannot run.
func TestNoSchedulableActionNeedsAnOperatorToTypeAName(t *testing.T) {
	for _, action := range spec.JobActions() {
		kind, ok := KindForAction(action)
		if !ok {
			continue // the test below is the one that reports this
		}
		if Destructive(kind) {
			t.Errorf("an instance YAML may schedule %q, whose kind %s is DESTRUCTIVE: section "+
				"16.4 needs the instance name typed by a person, and a timer has no way to "+
				"supply one -- gvm-job@.service passes no flags and jobs.Descriptor has no "+
				"field for it. Such a timer can only ever fire and fail. Use `gvm %s` instead.",
				action, kind, action)
		}
	}
}

// TestEveryActionAnInstanceMaySchedulePointsAtARunnerOrAnExplainedGap.
//
// The dangerous drift is one-directional: spec's validator accepting an action
// nothing can execute produces a timer that fires every night and fails every
// night, which is the least observable failure in this subsystem. So every one
// of spec.JobActions() has to be in exactly one of the two tables -- a runner,
// or `pending` with the package that will bring it.
func TestEveryActionAnInstanceMaySchedulePointsAtARunnerOrAnExplainedGap(t *testing.T) {
	for _, action := range spec.JobActions() {
		kind, ok := KindForAction(action)
		if !ok {
			t.Errorf("spec accepts the job action %q and the job layer has no kind for it",
				action)
			continue
		}
		_, runnable := runners[kind]
		where, deferred := pending[kind]
		switch {
		case runnable && deferred:
			t.Errorf("%s (%s) is both runnable and pending (%s)", action, kind, where)
		case !runnable && !deferred:
			t.Errorf("spec accepts %q and nothing runs it; add a runner or a pending row "+
				"saying which package will", action)
		case deferred:
			// A tracked gap. The refusal has to be honest about it.
			_, err := Lookup(kind)
			if err == nil {
				t.Errorf("Lookup(%s) succeeded for a pending kind", kind)
			} else if !strings.Contains(err.Error(), where) {
				t.Errorf("Lookup(%s) = %v; it has to name %s", kind, err, where)
			}
		}
	}
}

// TestEveryExecutableActionResolvesToADeclaredKind keeps the two tables in
// this file honest about each other.
func TestEveryExecutableActionResolvesToADeclaredKind(t *testing.T) {
	for _, action := range Actions() {
		kind, ok := KindForAction(action)
		if !ok {
			t.Fatalf("Actions() lists %q and KindForAction does not know it", action)
		}
		if !kind.Valid() {
			t.Errorf("action %q maps to %q, which is not in job_kind_t", action, kind)
		}
		// The strings are deliberately identical: the word in the YAML, the
		// word in the ledger's enum and the word in `systemctl list-timers`
		// are the same word, so there is nothing to translate when reading a
		// journal.
		if string(kind) != action {
			t.Errorf("action %q maps to kind %q; they are spelt the same on purpose",
				action, kind)
		}
	}
	for name, kind := range hostJobs {
		if _, ok := actionKinds[name]; !ok {
			t.Errorf("the host job %q is not an executable action", name)
		}
		if _, err := Lookup(kind); err != nil {
			t.Errorf("the host job %q has no runner: %v", name, err)
		}
	}
}

// TestASlugDerivesItsOwnUnitTimerAndLock.
//
// All four come from one pair of names, and the host-level lock is the one
// worth asserting: a host job has no instance runtime directory, and a job
// layer that joined an empty instance name into /run/gvm would produce
// /run/gvm/job.lock -- one lock shared by two different host jobs.
// TestTheJobLockIsNotTheRunnersWorldLock.
//
// Section 11.7 lock 1 is the RUNNER's: flock(LOCK_EX|LOCK_NB) on
// /run/gvm/<n>/job.lock, taken in preflight and held for the whole life of the
// unit, which is what makes "one supervisor per world" true. The job engine's
// lock is a different lock with a different lifetime, and for a while it was
// the same FILE -- D-30 and the §5.3 tree both name that path as the
// per-instance JOB lock, so the implementation faithfully reproduced the
// spec's own conflation.
//
// The consequences were total, in both directions:
//
//   - Instance UP. The runner already holds it, so every instance-scoped kind
//     -- update, backup, restart, content_sync, prune, verify, rebuild,
//     restore -- is refused at step 1 of Execute, before the audit span and
//     before any step table. And with a diagnostic that cannot help:
//     platform.Flock writes no holder line, so ReadHolder finds an empty file,
//     Busy.Known is false, and the operator is told a job was killed between
//     taking the lock and writing the line and to run `gvm job clear` -- which
//     clears the LEASE, not the lock.
//   - Instance DOWN. The job acquires, and then its own startStep starts
//     gvm@<n>, whose ExecStart hits the job's lock in preflight and exits 75.
//     The release is a defer registered before the guard's Restore, so it runs
//     LAST: the guard re-issues the same start into the same lock and the
//     world stays down for the night. That is the one outcome §13.4 exists to
//     prevent.
//   - And the secondary hazard the design had already rejected: gvm@.service
//     declares RuntimeDirectory=gvm/%i, so systemd REMOVES /run/gvm/<n> when
//     the unit goes inactive, unlinking a flocked file under its holder.
//     Acquire's MkdirAll plus O_CREATE then hands a second job a fresh inode
//     at the same path and both lock successfully -- exactly what
//     Lock.Release's own comment ("unlinking a flocked file is the classic way
//     to end up with two holders") was written to avoid.
//
// Pure derivation, so it runs on the development machine, which is where it
// has to run: every engine test injects a fake Locker and every runner test
// injects req.Flock, so no test anywhere puts both real locks on one path.
func TestTheJobLockIsNotTheRunnersWorldLock(t *testing.T) {
	const name = "pz-main"
	job := Slug{Instance: name, Job: "nightly"}.LockPath()
	runner := instance.PathsFor(name).RunnerLock

	if job == runner {
		t.Fatalf("the job engine and the runner both flock %q; on a live instance the runner "+
			"holds it for the unit's whole life, so no instance-scoped job could ever run",
			job)
	}
	// Not merely different -- not under the runtime directory at all, because
	// that is the directory systemd deletes on a stop the job itself performs.
	if run := instance.PathsFor(name).Run; strings.HasPrefix(job, run+"/") || job == run {
		t.Errorf("the job lock %q is inside %q, which is gvm@.service's RuntimeDirectory= and "+
			"is removed across a unit start", job, run)
	}
	if want := "/var/lib/gvm/locks/job_pz-main.lock"; job != want {
		t.Errorf("job lock = %q, want %q", job, want)
	}

	// Per INSTANCE and not per slug: the invariant is one job at a time per
	// instance, so pz-main_nightly has to serialise against pz-main_update.
	other := Slug{Instance: name, Job: "update"}.LockPath()
	if other != job {
		t.Errorf("two jobs on one instance take different locks (%q and %q), so a nightly "+
			"backup and an update could run at the same time", job, other)
	}

	// And a host job's lock cannot collide with an instance's, because D-45
	// forbids `_` in an instance name.
	if host := (Slug{Job: "guard"}).LockPath(); host == job {
		t.Errorf("the host `guard` lock and an instance lock are the same file: %q", host)
	}
}

func TestASlugDerivesItsOwnUnitTimerAndLock(t *testing.T) {
	s := Slug{Instance: "pz-main", Job: "nightly"}
	for _, tc := range []struct{ got, want string }{
		{s.Unit(), "gvm-job@pz-main_nightly.service"},
		{s.Timer(), "gvm-job@pz-main_nightly.timer"},
		{s.DescriptorPath(), "/etc/gvm/jobs/pz-main_nightly.json"},
		{s.LockPath(), "/var/lib/gvm/locks/job_pz-main.lock"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}

	host := Slug{Job: "guard"}
	if got, want := host.Unit(), "gvm-job@_guard.service"; got != want {
		t.Errorf("host unit = %q, want %q", got, want)
	}
	if got, want := host.LockPath(), "/var/lib/gvm/locks/job__guard.lock"; got != want {
		t.Errorf("host lock = %q, want %q", got, want)
	}
	verify := Slug{Job: "verify"}
	if verify.LockPath() == host.LockPath() {
		t.Error("_guard and _verify share a lock file; one would block the other forever")
	}
}

// TestEveryStepNameIsPrefixedByItsKind keeps a job log readable and keeps the
// injection-point enumeration in guard_test.go attributable: a step called
// "stop" in three tables would make three different failures look like one.
func TestEveryStepNameIsPrefixedByItsKind(t *testing.T) {
	// content_sync's steps are prefixed "content." rather than
	// "content_sync.", because the ledger's kind and the phase prefix answer
	// different questions and "content_sync.content_sync" reads as a typo.
	prefixes := map[lifecycle.JobKind]string{lifecycle.JobContentSync: "content."}
	for _, kind := range Kinds() {
		r, err := Lookup(kind)
		if err != nil {
			t.Fatal(err)
		}
		name := testInstance
		if !r.Scope.Accepts(name) {
			name = ""
		}
		rig := newRig(t, kind, name)
		steps, err := StepNames(rig.env)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if len(steps) == 0 {
			t.Errorf("%s has no steps", kind)
		}
		want := prefixes[kind]
		if want == "" {
			want = string(kind) + "."
		}
		seen := map[string]bool{}
		for _, s := range steps {
			if !strings.HasPrefix(s, want) {
				t.Errorf("%s step %q is not prefixed %q", kind, s, want)
			}
			if seen[s] {
				t.Errorf("%s has two steps called %q", kind, s)
			}
			seen[s] = true
		}
	}
}

// TestKindsAndPendingDoNotOverlap, so Lookup's two branches cannot both apply.
func TestKindsAndPendingDoNotOverlap(t *testing.T) {
	for k := range pending {
		if _, ok := runners[k]; ok {
			t.Errorf("%q is both runnable and pending", k)
		}
		if !k.Valid() {
			t.Errorf("pending kind %q is not in job_kind_t", k)
		}
	}
	for k := range runners {
		if !k.Valid() {
			t.Errorf("runnable kind %q is not in job_kind_t", k)
		}
	}
	if _, err := Lookup(lifecycle.JobKind("nonsense")); err == nil {
		t.Error("Lookup accepted a kind that does not exist")
	} else if !errors.Is(err, ErrNoRunner) {
		t.Errorf("err = %v, want ErrNoRunner", err)
	}
}

// TestEveryRunnerDeclaresSection133sRow, so the table in the document and the
// tables in the code cannot drift silently.
func TestEveryRunnerDeclaresSection133sRow(t *testing.T) {
	want := map[lifecycle.JobKind]struct {
		scope  Scope
		policy Policy
		stops  bool
	}{
		lifecycle.JobBackup:      {ScopeInstance, PolicyFail, false},
		lifecycle.JobUpdate:      {ScopeInstance, PolicyStartAnyway, true},
		lifecycle.JobRestart:     {ScopeInstance, PolicyStartAnyway, true},
		lifecycle.JobContentSync: {ScopeInstance, PolicyStartAnyway, true},
		// Section 13.3's fifth row: "yes" and "start_anyway after archive".
		lifecycle.JobRebuild: {ScopeInstance, PolicyStartAnyway, true},
		lifecycle.JobPrune:   {ScopeEither, PolicyWarn, false},
		lifecycle.JobVerify:  {ScopeEither, PolicyWarn, false},
		lifecycle.JobGuard:   {ScopeHost, PolicyWarn, false},
	}
	for _, kind := range Kinds() {
		r, err := Lookup(kind)
		if err != nil {
			t.Fatal(err)
		}
		w, ok := want[kind]
		if !ok {
			t.Errorf("%s has no row here; a kind whose policy nobody asserted is a kind whose "+
				"failures nobody designed", kind)
			continue
		}
		if r.Scope != w.scope {
			t.Errorf("%s scope %v, want %v", kind, r.Scope, w.scope)
		}
		if r.Policy != w.policy {
			t.Errorf("%s policy %q, want %q", kind, r.Policy, w.policy)
		}
		if r.Stops != w.stops {
			t.Errorf("%s stops=%v, want %v", kind, r.Stops, w.stops)
		}
		if r.Summary == "" {
			t.Errorf("%s has no summary", kind)
		}
	}
	for kind := range want {
		if _, err := Lookup(kind); err != nil {
			t.Errorf("%s is asserted here and has no runner: %v", kind, err)
		}
	}
}

// TestParseDescriptorRejectsAnEnormousDocument: the reader is limited, because
// /etc/gvm is operator-editable and a job that OOMed reading a 2 GB descriptor
// would be a job that never ran again.
func TestParseDescriptorRejectsAnEnormousDocument(t *testing.T) {
	huge := `{"instance":"pz-main","job":"backup","action":"backup","pad":"` +
		strings.Repeat("x", 128<<10) + `"}`
	if _, err := ParseDescriptor(strings.NewReader(huge)); err == nil {
		t.Fatal("accepted a 128 KiB descriptor")
	}
}
