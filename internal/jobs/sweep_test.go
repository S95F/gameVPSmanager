package jobs

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// fakeEnumerator is a crafted sweep report.
//
// A fake rather than a real lifecycle.Reconciler over a fake filesystem,
// because what is being tested here is the FILTER and the reason sentence --
// which instances a report turns into recoveries. Whether the report's
// findings are right is internal/lifecycle's own question, and it has its own
// tests for section 13.4's predicate; duplicating them here would mean two
// places to change when a term moves.
type fakeEnumerator struct {
	rep  lifecycle.SweepReport
	err  error
	runs int
}

func (f *fakeEnumerator) Sweep(context.Context) (lifecycle.SweepReport, error) {
	f.runs++
	return f.rep, f.err
}

// refusingSuper fails the test if anything starts it.
type refusingSuper struct {
	t *testing.T
	Supervisor
}

func (r refusingSuper) Start(context.Context, string) error {
	r.t.Error("the guard sweep started an instance while enumerating")
	return nil
}

func report(findings ...lifecycle.Finding) lifecycle.SweepReport {
	byInstance := map[string][]lifecycle.Finding{}
	var order []string
	for _, f := range findings {
		if _, ok := byInstance[f.Instance]; !ok {
			order = append(order, f.Instance)
		}
		byInstance[f.Instance] = append(byInstance[f.Instance], f)
	}
	rep := lifecycle.SweepReport{At: time.Now()}
	for _, name := range order {
		rep.Instances = append(rep.Instances, lifecycle.InstanceReport{
			Name: name, Findings: byInstance[name],
		})
	}
	return rep
}

func notRunning(name string) lifecycle.Finding {
	return lifecycle.Finding{
		Instance: name, Kind: lifecycle.FindingNotRunning, Severity: lifecycle.SevProblem,
		Detail: "enabled, not held, and the unit is inactive",
	}
}

// TestTheGuardSweepRecoversExactlyWhatTheReconcilerFound.
//
// The guard timer's whole job is section 13.4's predicate, and the predicate
// lives in internal/lifecycle. This asserts the adapter adds nothing to it and
// drops nothing from it: a held instance, a freeze in progress and a spec
// drift are all findings, and none of them is an instance to start.
func TestTheGuardSweepRecoversExactlyWhatTheReconcilerFound(t *testing.T) {
	en := &fakeEnumerator{rep: report(
		notRunning("pz-main"),
		lifecycle.Finding{Instance: "valheim-1", Kind: lifecycle.FindingHeld,
			Severity: lifecycle.SevNotice, Detail: "an operator stopped it"},
		lifecycle.Finding{Instance: "mc-1", Kind: lifecycle.FindingSpecDrift,
			Severity: lifecycle.SevProblem, Detail: "the spec was edited"},
		notRunning("ck-1"),
	)}
	s := &Sweep{Rec: en, Super: refusingSuper{t: t}}

	got, err := s.Desired(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range got {
		names = append(names, r.Instance)
	}
	if strings.Join(names, " ") != "pz-main ck-1" {
		t.Errorf("recoveries = %v, want pz-main and ck-1 only", names)
	}
	for _, r := range got {
		if r.Reason == "" {
			t.Errorf("%s has no reason; the `guard` run's whole value is saying why", r.Instance)
		}
	}
	if en.runs != 1 {
		t.Errorf("the sweep enumerated %d times; a second pass would be a second answer", en.runs)
	}
}

// TestAFailedEnumerationIsNotAnEmptyBox.
//
// The runner's step is Fatal for this: an unreadable /etc/gvm that reported
// "nothing to recover" would make a `guard` run succeed every five minutes on
// a box where every world was down.
func TestAFailedEnumerationIsNotAnEmptyBox(t *testing.T) {
	boom := errors.New("/etc/gvm/instances: permission denied")
	s := &Sweep{Rec: &fakeEnumerator{err: boom}, Super: refusingSuper{t: t}}
	if _, err := s.Desired(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the enumeration failure", err)
	}
}

// TestTheRecoveryReasonNamesTheJobThatLeftItDown.
//
// The guard's own Restore writes /run/gvm/<n>/degraded as it fails, naming the
// kind and the slug; this is the other end of that handshake. The sentence an
// operator finds on the `guard` run is then section 13.4's -- "recovered after
// <unit> ended without restoring service" -- rather than a restatement of what
// the sweep could see for itself.
func TestTheRecoveryReasonNamesTheJobThatLeftItDown(t *testing.T) {
	crumb := FormatDegraded(time.Date(2026, 9, 4, 5, 3, 0, 0, time.UTC),
		lifecycle.JobUpdate, "pz-main_nightly",
		[]string{"started pz-main and it is not running"})

	s := &Sweep{
		Rec:   &fakeEnumerator{rep: report(notRunning("pz-main"))},
		Super: refusingSuper{t: t},
		Files: mapFiles{instance.PathsFor("pz-main").Degraded: crumb},
	}
	got, err := s.Desired(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Desired() = %v, %v", got, err)
	}
	if !strings.Contains(got[0].Reason, "pz-main_nightly") {
		t.Errorf("reason = %q; it does not name the job that left the server down", got[0].Reason)
	}
	// One line. The reason chain belongs in the journal and in the marker, not
	// in a job_runs summary column.
	if strings.Contains(got[0].Reason, "\n") {
		t.Errorf("reason = %q; it is more than one line", got[0].Reason)
	}
}

// TestNoBreadcrumbStillProducesAnHonestReason is the SIGKILL path: the job
// process died before it could write anything, which is the case
// gvm-guard.timer exists for in the first place.
func TestNoBreadcrumbStillProducesAnHonestReason(t *testing.T) {
	s := &Sweep{
		Rec:   &fakeEnumerator{rep: report(notRunning("pz-main"))},
		Super: refusingSuper{t: t},
		Files: mapFiles{},
	}
	got, err := s.Desired(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Desired() = %v, %v", got, err)
	}
	if strings.Contains(got[0].Reason, "gvm-job@") {
		t.Errorf("reason = %q; with no marker gvm cannot know which job ended, and must not "+
			"invent one", got[0].Reason)
	}
	if !strings.Contains(got[0].Reason, "enabled, not held") {
		t.Errorf("reason = %q; it should say what was observed instead", got[0].Reason)
	}
}

// downWith builds an InstanceReport whose OBSERVATION carries section 13.4's
// terms, which is what the sweep now reads, rather than only a hand-made
// finding.
//
// A finding list is what a report SAYS; the observation is what it saw. The
// difference matters for exactly one case -- a stale lease -- where the best
// sentence ("a job died holding this instance") and the right action (start
// it) are not the same thing.
func downWith(name string, mutate func(*lifecycle.Observation)) lifecycle.InstanceReport {
	o := lifecycle.Observation{
		Name:        name,
		Enabled:     true,
		UnitChecked: true,
		Unit:        platform.UnitState{LoadState: "loaded", ActiveState: "inactive"},
		DropIn:      lifecycle.DropInHeader{Generated: true, SpecSHA256: "abcd"},
	}
	if mutate != nil {
		mutate(&o)
	}
	ir := lifecycle.InstanceReport{Name: name, Observation: o,
		Findings: lifecycle.Findings(o, lifecycle.Verdict{})}
	return ir
}

func reportOf(irs ...lifecycle.InstanceReport) lifecycle.SweepReport {
	rep := lifecycle.SweepReport{At: time.Now()}
	rep.Instances = append(rep.Instances, irs...)
	return rep
}

func staleLease(o *lifecycle.Observation) {
	o.Lease = &lifecycle.Lease{
		Proto: 1, Instance: o.Name, Kind: lifecycle.JobUpdate, PID: 48812,
		Unit: "gvm-job@" + o.Name + "_nightly.service",
	}
	o.LeaseState = lifecycle.LeaseStale
}

// TestAJobKilledHoldingTheLeaseIsStillRecovered.
//
// This is the only failure gvm-guard.timer exists for, spelled out in section
// 13.4's own introduction: "A `defer` does not survive `SIGKILL` of the job
// process (OOM killer, `systemctl kill`, power loss mid-update)." The job
// stopped the server, got killed before it could start it again, and left
// lease.json behind -- under /var/lib by design, so it survives a reboot.
//
// The lease then suppressed the very finding the sweep filters on, and the
// guard reported "every instance that should be running is running" every five
// minutes while the world stayed down. Section 13.4's predicate has three
// terms and none of them is a lease; the lease exclusion is an un-specced
// fourth term that fired on precisely the case the timer was written for.
//
// `update.sh` states the operator's contract in the opposite direction -- "keep
// going if it fails: an outdated server beats one left stopped" -- and the
// install tree being half-written is a reason to be careful about HOW the
// sweep starts it, which is what Recover's lease removal is, not a reason to
// leave the world down.
func TestAJobKilledHoldingTheLeaseIsStillRecovered(t *testing.T) {
	s := &Sweep{
		Rec:   &fakeEnumerator{rep: reportOf(downWith("pz-main", staleLease))},
		Super: refusingSuper{t: t},
	}
	got, err := s.Desired(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Instance != "pz-main" {
		t.Fatalf("recoveries = %+v; a SIGKILLed job's leftover lease must not suppress the "+
			"one recovery the guard timer exists for", got)
	}
	if !got[0].ClearStaleLease {
		t.Error("the recovery does not clear the dead holder's lease, so lifecycle.Allowed " +
			"will refuse the start it is about to attempt")
	}
	// The reason has to name the dead holder: on the SIGKILL path there is no
	// breadcrumb, so this sentence is all the operator gets.
	for _, want := range []string{"update", "48812", "gvm-job@pz-main_nightly.service"} {
		if !strings.Contains(got[0].Reason, want) {
			t.Errorf("reason = %q; it does not name %q", got[0].Reason, want)
		}
	}
}

// TestTheSweepLeavesAloneWhatItMustLeaveAlone.
//
// Each of these is a way the previous paragraph's fix could go wrong, and the
// held case is the one that would be worst: LifecycleSupervisor.Start CLEARS
// the hold, so a sweep that started a held instance would undo a deliberate
// `gvm stop` issued during a job -- and section 13.4's hold term is exactly
// what keeps the guard from fighting the operator.
func TestTheSweepLeavesAloneWhatItMustLeaveAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report lifecycle.SweepReport
	}{
		{"a LIVE lease: a job is working on it right now",
			reportOf(downWith("pz-main", func(o *lifecycle.Observation) {
				staleLease(o)
				o.LeaseState = lifecycle.LeaseLive
			}))},
		{"a stale lease on a HELD instance: a human asked for it to be down",
			reportOf(downWith("pz-main", func(o *lifecycle.Observation) {
				staleLease(o)
				o.Hold = true
			}))},
		{"a stale lease on a RUNNING unit: nothing to recover",
			reportOf(downWith("pz-main", func(o *lifecycle.Observation) {
				staleLease(o)
				o.Unit.ActiveState, o.Unit.SubState = "active", "running"
			}))},
		{"a stale lease on an instance that was never applied",
			reportOf(downWith("pz-main", func(o *lifecycle.Observation) {
				staleLease(o)
				o.DropIn = lifecycle.DropInHeader{}
			}))},
		{"a stale lease on a disabled instance",
			reportOf(downWith("pz-main", func(o *lifecycle.Observation) {
				staleLease(o)
				o.Enabled = false
			}))},
		{"activating: a forty-minute Project Zomboid boot is not a stopped server",
			reportOf(downWith("pz-main", func(o *lifecycle.Observation) {
				staleLease(o)
				o.Unit.ActiveState = "activating"
			}))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Sweep{Rec: &fakeEnumerator{rep: tc.report}, Super: refusingSuper{t: t}}
			got, err := s.Desired(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Errorf("the sweep would start %+v", got)
			}
		})
	}
}

// TestADeadHoldersLeaseIsRemovedThroughHostopsBeforeTheStart.
//
// Two gates stood between a SIGKILLed job and a recovered server, and the
// enumeration was only the first. lifecycle.Allowed refuses a start outright
// on a stale lease -- "a job died holding this instance and the install tree
// may be half-written" -- overridable only by Force, and the sweep's
// supervisor is wired with Force=false.
//
// So the lease is REMOVED rather than overridden. Removing it is better than
// passing Force, which would also paper over an unreadable lease and over
// every refusal later added to that table; and it goes through hostops rather
// than a silent unlink, because `gvm job clear` is audited and so must this
// be. If the removal fails the start is NOT attempted: an unrecovered failure
// is a DEGRADED guard run an operator can see, where a start refused on a
// gate nothing reported would look like a guard that did nothing.
func TestADeadHoldersLeaseIsRemovedThroughHostopsBeforeTheStart(t *testing.T) {
	inj := newInjector()
	super := &fakeSuper{inj: inj}
	host := newFakeHost(inj)
	s := &Sweep{Rec: &fakeEnumerator{}, Super: super, Host: host, Actor: audit.Timer()}

	rec := Recovery{Instance: "pz-main", Reason: "a dead holder", ClearStaleLease: true}
	if err := s.Recover(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if n := host.removed(hostops.FileLease); n != 1 {
		t.Errorf("removed the lease %d time(s) through hostops, want 1", n)
	}
	if _, starts := super.counts(); starts != 1 {
		t.Errorf("starts = %d, want 1", starts)
	}
	if got := inj.traced(); !sliceHasInOrder(got, "host.remove:lease", "super.start") {
		t.Errorf("order = %v; the lease has to be gone before the start, or Allowed refuses it",
			got)
	}

	// A removal that fails must not be followed by a start that cannot
	// succeed: the run is degraded with a readable error instead.
	inj2 := newInjector()
	super2 := &fakeSuper{inj: inj2}
	host2 := newFakeHost(inj2)
	host2.fail[hostops.FileLease] = errors.New("read-only filesystem")
	s2 := &Sweep{Rec: &fakeEnumerator{}, Super: super2, Host: host2, Actor: audit.Timer()}
	if err := s2.Recover(context.Background(), rec); err == nil {
		t.Error("a failed lease removal reported a successful recovery")
	}
	if _, starts := super2.counts(); starts != 0 {
		t.Error("started an instance whose stale lease is still on disk, which lifecycle " +
			"refuses, so the start could only fail")
	}

	// And a recovery that did not ask for it removes nothing: the ordinary
	// case is an instance with no lease at all.
	inj3 := newInjector()
	host3 := newFakeHost(inj3)
	s3 := &Sweep{Rec: &fakeEnumerator{}, Super: &fakeSuper{inj: inj3}, Host: host3,
		Actor: audit.Timer()}
	if err := s3.Recover(context.Background(), Recovery{Instance: "pz-main"}); err != nil {
		t.Fatal(err)
	}
	if n := host3.removed(hostops.FileLease); n != 0 {
		t.Errorf("removed %d lease(s) for a recovery that named none", n)
	}
}

// TestTheSweepWithNoHostStillEnumerates, because `gvm doctor` holds a Sweep to
// answer "what would the guard do" and has no business with a privileged
// surface.
func TestTheSweepWithNoHostStillEnumerates(t *testing.T) {
	s := &Sweep{
		Rec:   &fakeEnumerator{rep: reportOf(downWith("pz-main", staleLease))},
		Super: refusingSuper{t: t},
	}
	got, err := s.Desired(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Desired() = %+v, %v", got, err)
	}
	// And Recover refuses rather than starting into a refusal it cannot clear.
	if err := s.Recover(context.Background(), got[0]); err == nil {
		t.Error("recovered a stale-leased instance with no privileged surface to clear it with")
	}
}

func sliceHasInOrder(got []string, a, b string) bool {
	ai, bi := -1, -1
	for i, s := range got {
		if s == a && ai < 0 {
			ai = i
		}
		if s == b {
			bi = i
		}
	}
	return ai >= 0 && bi > ai
}

// TestRecoverStartsThroughLifecycleAndRefusesANameItCannotTrust.
func TestRecoverStartsThroughLifecycleAndRefusesANameItCannotTrust(t *testing.T) {
	inj := newInjector()
	super := &fakeSuper{inj: inj}
	s := &Sweep{Rec: &fakeEnumerator{}, Super: super}

	if err := s.Recover(context.Background(), Recovery{Instance: "pz-main"}); err != nil {
		t.Fatal(err)
	}
	if _, starts := super.counts(); starts != 1 {
		t.Errorf("starts = %d, want 1", starts)
	}
	// A report whose instance name is not one is a report gvm did not write.
	// Starting "../../etc" is not a thing to attempt and then fail at.
	if err := s.Recover(context.Background(), Recovery{Instance: "../../etc"}); err == nil {
		t.Error("recovered an instance whose name is not a name")
	}
	if _, starts := super.counts(); starts != 1 {
		t.Error("a refused name still reached the supervisor")
	}
}

// mapFiles is a read-only filesystem of exactly the files a test names.
type mapFiles map[string][]byte

func (m mapFiles) Stat(name string) (fs.FileInfo, error) {
	if _, ok := m[name]; !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return nil, nil
}

func (m mapFiles) ReadFile(name string) ([]byte, error) {
	b, ok := m[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return b, nil
}

func (m mapFiles) ReadDir(name string) ([]fs.DirEntry, error) {
	return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
}
