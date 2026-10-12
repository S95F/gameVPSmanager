package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// TestNeverLeavesServerStopped is update.sh's comment, as a test.
//
//	# Also runs unattended every night ... so no failure here may leave the
//	# server stopped.
//
// It injects a failure at every seam of every job kind and asserts that for
// each one, if the instance was active at entry then EITHER it is active at
// exit, OR the run records left_stopped=true and a breadcrumb was written for
// `gvm-guard.timer` to act on. There is no third acceptable outcome.
//
// The injection points are enumerated FROM THE STEP TABLES rather than from a
// hand-written list: a clean run tells the test how many fallible calls the
// kind makes, and then it re-runs the job once per call, three times over --
// an error return, a panic, and a cancelled context. A phase added to a table
// next year is covered the day it is added, and a phase whose failure nobody
// injected cannot exist.
//
// It runs on Windows against the fake systemd and the fake work seams, which
// is the whole reason those are seams. The invariant that matters most is the
// one that must not wait for a Linux CI run to be checked.
func TestNeverLeavesServerStopped(t *testing.T) {
	// The total is reported and floored. Reported because the number is the
	// claim -- "a failure injected at every seam of every kind" means nothing
	// without saying how many seams that is -- and floored because the
	// enumeration derives its size from the step tables, which means a table
	// accidentally gutted would make this test pass with less to prove. The
	// floor only ever moves UP, and the message says so.
	var cases int
	for _, kind := range Kinds() {
		r, err := Lookup(kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !r.Scope.Accepts(testInstance) {
			// Host-level kinds have no single instance to guard. They get
			// their own assertion below: they must never stop anything.
			continue
		}
		t.Run(string(kind), func(t *testing.T) {
			ops := baselineOps(t, kind)
			if len(ops) < 2 {
				t.Fatalf("a clean %s run made %d fallible calls (%v); the enumeration below "+
					"would prove almost nothing", kind, len(ops), ops)
			}
			for i, op := range ops {
				for _, mode := range []string{"error", "panic", "cancel"} {
					// The op in the name is the one the BASELINE run made at
					// this index. A failure changes the path, so a later run
					// may reach a different call at the same index -- which
					// is fine and is part of the point: every reachable
					// interleaving still has to satisfy the invariant.
					t.Run(fmt.Sprintf("%02d-%s-%s", i, op, mode), func(t *testing.T) {
						rig := newRig(t, kind, testInstance)
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						switch mode {
						case "error":
							rig.inj.failIndex = i
						case "panic":
							rig.inj.panicIndex = i
						case "cancel":
							at := i
							rig.inj.onCall = func(n int, _ string) {
								if n == at {
									cancel()
								}
							}
						}
						jr, _ := Execute(ctx, rig.env)
						assertServerNotLeftStopped(t, jr, rig)
					})
					cases++
				}
			}
		})
	}
	t.Logf("%d injection points across %d job kinds", cases, len(Kinds()))
	// 102 at the time of writing: 34 fallible calls across the seven
	// instance-scoped kinds, each injected three ways. The floor sits just
	// under that.
	const floor = 96
	if cases < floor {
		t.Errorf("the enumeration produced %d cases, under the %d-case floor. The size comes "+
			"from the step tables, so a drop means a table lost steps rather than that this "+
			"test got cheaper; if a kind was deliberately removed, lower the floor in the same "+
			"commit", cases, floor)
	}
}

// baselineOps runs one clean job and reports the fallible calls it made.
func baselineOps(t *testing.T, kind lifecycle.JobKind) []string {
	t.Helper()
	rig := newRig(t, kind, testInstance)
	jr, err := Execute(context.Background(), rig.env)
	if err != nil {
		t.Fatalf("a clean %s run failed: %v\n%s", kind, err, jr.Report())
	}
	if jr.State != StateOK {
		t.Fatalf("a clean %s run is %q, want ok:\n%s", kind, jr.State, jr.Report())
	}
	if !rig.super.isActive() {
		t.Fatalf("a clean %s run left the server stopped", kind)
	}
	return rig.inj.ops()
}

// assertServerNotLeftStopped is the invariant, in one place.
func assertServerNotLeftStopped(t *testing.T, jr *Run, rig *testRig) {
	t.Helper()
	if jr == nil {
		t.Fatal("no run record; a job that did not produce one cannot be audited")
	}
	if !jr.Guard.Armed {
		t.Fatalf("no running guard was armed:\n%s", jr.Report())
	}
	if !jr.Guard.WasActive {
		t.Fatalf("the guard did not record the instance as active at entry:\n%s", jr.Report())
	}
	if rig.super.isActive() {
		if jr.LeftStopped {
			t.Errorf("the instance is running and the run records left_stopped:\n%s", jr.Report())
		}
		if jr.State == StateFailed && jr.Panic == "" && jr.Fatal == nil {
			t.Errorf("state is failed with nothing to blame:\n%s", jr.Report())
		}
		return
	}
	// The other acceptable outcome, and only with all three of its parts.
	if !jr.LeftStopped {
		t.Errorf("the instance is STOPPED and the run does not record left_stopped:\n%s",
			jr.Report())
	}
	if jr.State != StateFailed {
		t.Errorf("the instance is stopped and the run is %q; a server left down is failed "+
			"whatever the kind's failure policy says:\n%s", jr.State, jr.Report())
	}
	if rig.host.wrote(hostops.FileDegraded) == 0 {
		t.Errorf("the instance is stopped and no breadcrumb was written; gvm-guard.timer has "+
			"nothing to act on:\n%s", jr.Report())
	}
	if n := len(jr.Guard.Reasons); n == 0 {
		t.Errorf("left_stopped with no reason chain:\n%s", jr.Report())
	}
}

// TestTheGuardIsArmedBeforeTheFirstFallibleWork pins the ORDERING that section
// 13.4 is actually about.
//
// "Registered with defer as the FIRST statement after acquisition, before any
// fallible work" is not a style note. A guard installed one line later has a
// window exactly as wide as that line. The lease write is the first fallible
// thing the engine does after arming the guard, so failing it proves the guard
// was already there: the run carries a guard that observed the instance, and
// nothing was stopped.
func TestTheGuardIsArmedBeforeTheFirstFallibleWork(t *testing.T) {
	rig := newRig(t, lifecycle.JobUpdate, testInstance)
	rig.host.fail[hostops.FileLease] = errors.New("read-only filesystem")

	jr, err := Execute(context.Background(), rig.env)
	if err == nil {
		t.Fatal("a job whose lease could not be taken reported success")
	}
	if !jr.Guard.Armed || !jr.Guard.WasActive {
		t.Errorf("the guard was not armed before the lease write: %+v", jr.Guard)
	}
	if stops, _ := rig.super.counts(); stops != 0 {
		t.Errorf("the lease failed and the job stopped the server %d time(s)", stops)
	}
	if !rig.super.isActive() {
		t.Error("the lease failed and the server is not running")
	}
	if !strings.Contains(err.Error(), "lease") {
		t.Errorf("error %q does not name the lease", err)
	}
}

// TestTheLeaseIsReleasedBeforeTheGuardStarts pins a subtle ordering that is
// easy to get wrong and silent when it is.
//
// lifecycle.Allowed REFUSES a start while a live lease holds the instance --
// and at the moment the guard needs to start the server, the live lease is
// this job's own. So the engine's defers are registered in the order that
// releases the lease first. Get it backwards and the guard's start is refused
// by gvm itself, every time, on exactly the path that was supposed to save the
// situation.
func TestTheLeaseIsReleasedBeforeTheGuardStarts(t *testing.T) {
	// Fail the start STEP, so the guard is the one that brings it back.
	idx := indexIn(baselineOps(t, lifecycle.JobRestart), "super.start")
	if idx < 0 {
		t.Fatal("a restart made no start call")
	}
	rig := newRig(t, lifecycle.JobRestart, testInstance)
	rig.inj.failIndex = idx

	jr, _ := Execute(context.Background(), rig.env)
	if !rig.super.isActive() {
		t.Fatalf("the guard did not restore the server:\n%s", jr.Report())
	}
	trace := rig.inj.traced()
	release := indexIn(trace, "host.remove:lease")
	if release < 0 {
		t.Fatalf("the lease was never released: %v", trace)
	}
	// The guard's start is the LAST start in the trace.
	guardStart := lastIndexIn(trace, "super.start")
	if guardStart < release {
		t.Errorf("the guard started the server at trace position %d and the lease was released "+
			"at %d; lifecycle refuses a start while a live lease holds the instance:\n%v",
			guardStart, release, trace)
	}
}

// TestTheGuardSurvivesACancelledContext is the failure mode a naive
// implementation gets wrong.
//
// systemd reaches TimeoutStartSec on gvm-job@..., sends SIGTERM, and the job's
// context is cancelled. If Restore used that context it could not issue the
// start, and every timed-out job would leave a stopped world. It detaches with
// context.WithoutCancel and takes a budget of its own.
func TestTheGuardSurvivesACancelledContext(t *testing.T) {
	rig := newRig(t, lifecycle.JobRestart, testInstance)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.inj.onCall = func(_ int, op string) {
		if op == "super.stop" {
			// Cancelled while the ladder is running: the stop completes, the
			// start step is never reached, and only the guard can put it
			// back.
			cancel()
		}
	}

	jr, _ := Execute(ctx, rig.env)
	if !rig.super.isActive() {
		t.Fatalf("a cancelled job left the server stopped:\n%s", jr.Report())
	}
	if !jr.Guard.Started {
		t.Errorf("the server is up but the guard does not claim the start: %+v", jr.Guard)
	}
	if jr.State != StateCancelled {
		t.Errorf("state %q, want cancelled: a job an operator cancelled is not a defect:\n%s",
			jr.State, jr.Report())
	}
	if jr.LeftStopped {
		t.Error("left_stopped on a run whose server is up")
	}
}

// TestTheGuardSurvivesAPanicAndStillRecords.
//
// The deferred Restore runs while the panic is unwinding, BEFORE the recover
// that records it -- because defers run last-registered-first and the engine
// registers its recover earlier. So the server comes back and the panic is
// still reported, rather than one at the expense of the other.
func TestTheGuardSurvivesAPanicAndStillRecords(t *testing.T) {
	rig := newRig(t, lifecycle.JobRestart, testInstance)
	// Only the STEP's start panics. The guard's own start, which happens
	// afterwards on the unwinding stack, has to work -- that is the property
	// being tested.
	var starts int
	rig.inj.onCall = func(_ int, op string) {
		if op != "super.start" {
			return
		}
		starts++
		if starts == 1 {
			panic("the fake runner exploded")
		}
	}

	jr, err := Execute(context.Background(), rig.env)
	if !rig.super.isActive() {
		t.Fatalf("a panicking job left the server stopped:\n%s", jr.Report())
	}
	if jr.Panic == "" {
		t.Errorf("the panic was not recorded:\n%s", jr.Report())
	}
	if jr.State != StateFailed {
		t.Errorf("state %q, want failed: a panic is a gvm defect", jr.State)
	}
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("Execute returned %v; a recovered panic has to reach the caller as an error", err)
	}
}

// TestTheGuardItselfPanickingStillLeavesTheBreadcrumb.
//
// A Supervisor is a seam. A fake, or a future implementation, that panics
// inside the restore must not swallow the one marker `gvm-guard.timer` reads
// -- otherwise the belt fails and the braces were never fastened.
func TestTheGuardItselfPanickingStillLeavesTheBreadcrumb(t *testing.T) {
	rig := newRig(t, lifecycle.JobRestart, testInstance)
	// The step's start fails, so the guard is the one that tries; the guard's
	// start -- the SECOND one -- panics.
	rig.inj.failIndex = indexIn(baselineOps(t, lifecycle.JobRestart), "super.start")
	var starts int
	rig.inj.onCall = func(_ int, op string) {
		if op != "super.start" {
			return
		}
		starts++
		if starts == 2 {
			panic("the supervisor exploded inside the restore")
		}
	}

	jr, _ := Execute(context.Background(), rig.env)
	if rig.super.isActive() {
		t.Fatal("the fake started the server despite panicking")
	}
	if rig.host.wrote(hostops.FileDegraded) == 0 {
		t.Errorf("the guard panicked, the server is down, and no breadcrumb was written:\n%s",
			jr.Report())
	}
	if !jr.LeftStopped {
		t.Errorf("left_stopped is false after the guard panicked with the server down:\n%s",
			jr.Report())
	}
	if !joined(jr.Guard.Reasons, "panicked") {
		t.Errorf("no reason names the panic: %v", jr.Guard.Reasons)
	}
}

// TestWhenTheStartItselfFailsEverythingSection134AsksForHappens.
//
// Section 13.4's last bullet, item by item: state='failed', left_stopped=true,
// /run/gvm/<n>/degraded written, and a reason chain an operator can read.
func TestWhenTheStartItselfFailsEverythingSection134AsksForHappens(t *testing.T) {
	rig := newRig(t, lifecycle.JobRestart, testInstance)
	rig.super.failEveryStart = true

	jr, err := Execute(context.Background(), rig.env)
	if err == nil {
		t.Fatal("a job that left the server stopped reported success")
	}
	if jr.State != StateFailed {
		t.Errorf("state %q, want failed", jr.State)
	}
	if !jr.LeftStopped {
		t.Error("left_stopped is false; section 14.4 calls true here a doctor error, and this " +
			"is the case it means")
	}
	if n := rig.host.wrote(hostops.FileDegraded); n == 0 {
		t.Fatal("no degraded marker was written")
	}
	body := rig.host.body(hostops.FileDegraded)
	if !strings.Contains(body, "left this instance stopped") {
		t.Errorf("the breadcrumb does not say what happened:\n%s", body)
	}
	if !strings.Contains(body, "restart") {
		t.Errorf("the breadcrumb does not name the job kind:\n%s", body)
	}
	if len(jr.Guard.Reasons) == 0 {
		t.Error("no reason chain")
	}
	if p := jr.AuditParams()["left_stopped"]; p != true {
		t.Errorf("the audit row's left_stopped is %v", p)
	}
}

// TestTheGuardYieldsToAnOperatorHold.
//
// An operator ran `gvm stop` while the nightly was running. They win. A guard
// that fought an explicit stop would make maintenance impossible, and
// gvm-guard.timer honours the same marker for the same reason -- so a
// breadcrumb here would be a marker nothing acts on.
func TestTheGuardYieldsToAnOperatorHold(t *testing.T) {
	// The start step fails, so the guard would normally start it -- and an
	// operator ran `gvm stop` while the job was failing to bring it back.
	//
	// The hold is written on `super.start` rather than on `super.stop`, and
	// the distinction is the whole subject of this test. A job's own stop
	// writes NO hold (LifecycleSupervisor.Stop sends NoHold, because the
	// marker is a claim about a human), and the job's own start CLEARS one --
	// VerbStart declares HoldClear and clears it before the unit start, so a
	// failed start cannot leave a marker that suppresses gvm-guard.timer. A
	// hold that is present after both of those is therefore unambiguously
	// somebody's, which is what makes the guard's yield correct.
	rig2 := newRig(t, lifecycle.JobRestart, testInstance)
	rig2.inj.failIndex = indexIn(baselineOps(t, lifecycle.JobRestart), "super.start")
	rig2.inj.onCall = func(_ int, op string) {
		if op == "super.start" {
			rig2.super.mu.Lock()
			rig2.super.held = true
			rig2.super.mu.Unlock()
		}
	}

	jr, _ := Execute(context.Background(), rig2.env)
	if rig2.super.isActive() {
		t.Fatal("the guard started an instance an operator had held down")
	}
	if !jr.Guard.Held {
		t.Errorf("the guard does not record the hold: %+v", jr.Guard)
	}
	if jr.LeftStopped {
		t.Error("left_stopped on an instance a human asked to be down; that is a hold, not a " +
			"failure of the guard")
	}
	if n := rig2.host.wrote(hostops.FileDegraded); n != 0 {
		t.Errorf("wrote %d breadcrumb(s) for an instance under an operator hold; the hold "+
			"suppresses gvm-guard.timer anyway, so nothing would act on it", n)
	}
	if !joined(jr.Guard.Reasons, "hold") {
		t.Errorf("no reason names the hold: %v", jr.Guard.Reasons)
	}
}

// TestAnInstanceStoppedBeforeTheJobIsNotStarted.
//
// Section 13.4's third bullet, and the one place gvm deliberately diverges
// from update.sh. The bash script starts the server at the end whatever
// happened, including when it was deliberately down -- which is why it has to
// print "the server was stopped before this update; it is running now". gvm
// leaves it down, and prints that note only when something else started it.
func TestAnInstanceStoppedBeforeTheJobIsNotStarted(t *testing.T) {
	rig := newRig(t, lifecycle.JobBackup, testInstance)
	rig.super.active = false

	jr, err := Execute(context.Background(), rig.env)
	if err != nil {
		t.Fatalf("%v\n%s", err, jr.Report())
	}
	if rig.super.isActive() {
		t.Error("the job started a server that was deliberately stopped")
	}
	if _, starts := rig.super.counts(); starts != 0 {
		t.Errorf("%d start call(s) on an instance that was already down", starts)
	}
	if jr.Guard.WasActive {
		t.Error("the guard recorded a stopped instance as active")
	}
	if jr.Guard.Note != "" {
		t.Errorf("printed the bash suite's note when it does not apply: %q", jr.Guard.Note)
	}
	if jr.LeftStopped {
		t.Error("left_stopped for an instance the job never stopped")
	}
}

// TestTheNoteIsPrintedOnlyWhenItApplies is the other half of the bullet above.
func TestTheNoteIsPrintedOnlyWhenItApplies(t *testing.T) {
	rig := newRig(t, lifecycle.JobRestart, testInstance)
	rig.super.active = false

	jr, err := Execute(context.Background(), rig.env)
	if err != nil {
		t.Fatalf("%v\n%s", err, jr.Report())
	}
	if !rig.super.isActive() {
		t.Fatal("a restart of a stopped instance left it stopped")
	}
	if jr.Guard.Note == "" {
		t.Fatalf("the instance was stopped before the job and is running now; the bash suite's "+
			"own note applies and was not recorded:\n%s", jr.Report())
	}
	if !strings.Contains(jr.Guard.Note, "it is running now") {
		t.Errorf("note = %q", jr.Guard.Note)
	}
}

// TestAnUnreadableObservationIsTreatedAsRunning.
//
// The two mistakes are not symmetric. Starting a server that is already up is
// a no-op; failing to start one that is down is the outage. So "could not
// tell" has to mean "was running", and it has to say so in the reason chain.
func TestAnUnreadableObservationIsTreatedAsRunning(t *testing.T) {
	rig := newRig(t, lifecycle.JobBackup, testInstance)
	rig.super.activeErr = errors.New("dbus: connection refused")

	jr, _ := Execute(context.Background(), rig.env)
	if !jr.Guard.WasActive {
		t.Error("an unreadable observation was treated as 'was not running'")
	}
	if !jr.Guard.Unknown {
		t.Error("the guard does not record that it could not tell")
	}
	if _, starts := rig.super.counts(); starts == 0 {
		t.Error("the guard did not try to start an instance it could not observe")
	}
	if !joined(jr.Guard.Reasons, "could not tell") {
		t.Errorf("reasons do not say what happened: %v", jr.Guard.Reasons)
	}
}

// TestAHostLevelJobNeverStopsAnything is section 13.3's last row -- "guard |
// starts, never stops" -- asserted rather than documented.
func TestAHostLevelJobNeverStopsAnything(t *testing.T) {
	rig := newRig(t, lifecycle.JobGuard, "")
	jr, err := Execute(context.Background(), rig.env)
	if err != nil {
		t.Fatalf("%v\n%s", err, jr.Report())
	}
	if stops, _ := rig.super.counts(); stops != 0 {
		t.Errorf("the guard sweep made %d stop call(s)", stops)
	}
	if jr.Guard.Armed {
		t.Error("a host-level job armed a RunningGuard; there is no single instance to guard")
	}
	if n := rig.host.wrote(hostops.FileLease); n != 0 {
		t.Errorf("a host-level job took %d instance lease(s)", n)
	}
	// And the failure path, over every injectable call.
	for i, op := range rig.inj.ops() {
		t.Run(fmt.Sprintf("%02d-%s", i, op), func(t *testing.T) {
			r2 := newRig(t, lifecycle.JobGuard, "")
			r2.inj.failIndex = i
			if _, err := Execute(context.Background(), r2.env); err == nil {
				t.Log("the sweep absorbed the failure, which is what `warn` means")
			}
			if stops, _ := r2.super.counts(); stops != 0 {
				t.Errorf("a failing guard sweep made %d stop call(s)", stops)
			}
		})
	}
}

// TestAStoppingStepWithoutAnArmedGuardPanics.
//
// The engine refuses to run a step table that could stop a server with no
// guard in front of it, and it refuses by panicking rather than by skipping.
// That is archive.Destroy's reasoning applied here: reaching this state is a
// programming error, a panic in a job is recovered and audited, and a silent
// nil check could be routed around by a future caller.
func TestAStoppingStepWithoutAnArmedGuardPanics(t *testing.T) {
	bad := &Runner{
		Kind: lifecycle.JobGuard, Scope: ScopeHost, Policy: PolicyWarn, Stops: true,
		Steps: func(env *Env) ([]Step, error) {
			return []Step{{
				Name: "bogus.stop", Stops: true, Fatal: true,
				Do: func(context.Context, *Run) error { return nil },
			}}, nil
		},
	}
	rig := newRig(t, lifecycle.JobGuard, "")
	jr, err := bad.Execute(context.Background(), rig.env)
	if err == nil {
		t.Fatal("a stopping step with no guard ran")
	}
	if !strings.Contains(jr.Panic, "no running guard is armed") {
		t.Errorf("panic = %q; it has to name the problem", jr.Panic)
	}
}

// TestANonStoppingKindMayNotContainAStoppingStep pins section 13.3's third
// column against the step tables, so "stops the server? no" is a property of
// the code.
func TestANonStoppingKindMayNotContainAStoppingStep(t *testing.T) {
	for _, kind := range Kinds() {
		r, err := Lookup(kind)
		if err != nil {
			t.Fatal(err)
		}
		if r.Stops {
			continue
		}
		name := testInstance
		if !r.Scope.Accepts(name) {
			name = ""
		}
		rig := newRig(t, kind, name)
		stopping, err := StoppingSteps(rig.env)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if len(stopping) > 0 {
			t.Errorf("%s declares Stops=false and its table contains %v", kind, stopping)
		}
	}
}

// TestRestoreIsIdempotent: the engine registers it with defer and a hand-written
// job will reasonably also call it on the success path, exactly as
// audit.Span.Done is idempotent for the same reason.
func TestRestoreIsIdempotent(t *testing.T) {
	rig := newRig(t, lifecycle.JobBackup, testInstance)
	g, err := Guard(context.Background(), rig.env)
	if err != nil {
		t.Fatal(err)
	}
	rig.super.active = false
	g.Restore(context.Background())
	_, first := rig.super.counts()
	g.Restore(context.Background())
	g.Restore(context.Background())
	if _, n := rig.super.counts(); n != first {
		t.Errorf("three Restore calls made %d starts, want %d", n, first)
	}
	var nilGuard *RunningGuard
	nilGuard.Restore(context.Background()) // must not panic
}

// TestGuardRefusesWithoutASupervisor: a job with no way to put the server back
// must not run at all, and must say so before it has stopped anything.
func TestGuardRefusesWithoutASupervisor(t *testing.T) {
	rig := newRig(t, lifecycle.JobRestart, testInstance)
	rig.env.Super = nil
	if _, err := Execute(context.Background(), rig.env); err == nil {
		t.Fatal("a job ran with no supervisor")
	} else if !errors.Is(err, ErrEnv) {
		t.Errorf("err = %v, want ErrEnv", err)
	}
	if _, err := Guard(context.Background(), rig.env); err == nil {
		t.Fatal("Guard accepted an environment with no supervisor")
	}
}

// TestFormatDegradedIsReadableAndSafe.
//
// An operator is going to `cat` this file. A reason chain picked up from a
// subprocess's stderr can carry escape sequences and newlines; the first is a
// terminal control sequence in their terminal and the second makes half a
// record look like a whole one.
func TestFormatDegradedIsReadableAndSafe(t *testing.T) {
	at := time.Date(2026, 9, 4, 5, 3, 11, 0, time.UTC)
	body := string(FormatDegraded(at, lifecycle.JobUpdate, "pz-main_nightly", []string{
		"steam update failed\nand the start was refused",
		"unit \x1b[31mgvm@pz-main.service\x1b[0m entered failed state",
	}))
	if !strings.HasPrefix(body, "2026-09-04T05:03:11Z update (pz-main_nightly)") {
		t.Errorf("first line is %q", strings.SplitN(body, "\n", 2)[0])
	}
	if strings.Contains(body, "\x1b") {
		t.Error("an escape sequence survived into a file an operator will cat")
	}
	if n := strings.Count(body, "\n"); n != 3 {
		t.Errorf("%d lines; one header plus one per reason, with the newlines inside a reason "+
			"flattened:\n%s", n, body)
	}
	if !strings.HasSuffix(body, "\n") {
		t.Error("no trailing newline")
	}
}

func indexIn(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}

func lastIndexIn(list []string, want string) int {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == want {
			return i
		}
	}
	return -1
}

func joined(list []string, substr string) bool {
	return strings.Contains(strings.Join(list, " | "), substr)
}

// TestTheBreadcrumbIsWrittenBeforeTheStartAndClearedWhenItConfirms.
//
// The arithmetic in DeferredChainBudget bounds the guard's wait; it cannot
// save the evidence, because the wait it bounds is the one that gets killed.
// Super.Start waits for the systemd job to complete and gvm@.service is
// Type=notify, so for a game whose readiness.hard_deadline is an hour the
// start job does not complete until the world is up. If TimeoutStopSec runs
// out first, SIGKILL lands inside that wait -- after the decision was made and
// before anything recorded it.
//
// So the marker goes down first. The assertion is the ORDER, and then that a
// confirmed start takes it away again: a marker nobody clears is a marker
// gvm-guard.timer acts on five minutes later against an instance that is
// already running.
func TestTheBreadcrumbIsWrittenBeforeTheStartAndClearedWhenItConfirms(t *testing.T) {
	rig := newRig(t, lifecycle.JobRestart, testInstance)
	// The job's own start step fails, so the guard is the thing that puts the
	// server back -- the path where the breadcrumb matters.
	rig.inj.failIndex = indexIn(baselineOps(t, lifecycle.JobRestart), "super.start")

	jr, _ := Execute(context.Background(), rig.env)
	if !rig.super.isActive() {
		t.Fatalf("the guard did not restore the server: %+v", jr.Guard)
	}
	if jr.LeftStopped {
		t.Errorf("left_stopped on an instance the guard brought back: %+v", jr.Guard)
	}

	trace := rig.inj.traced()
	write := indexOf(trace, "host.write:degraded")
	if write < 0 {
		t.Fatal("no breadcrumb was written before the start; a SIGKILL inside Super.Start " +
			"would then leave no trace of the decision at all")
	}
	// The guard's own start is the LAST super.start in the trace: the job's
	// own start step came first and failed.
	last := -1
	for i, op := range trace {
		if op == "super.start" {
			last = i
		}
	}
	if last < 0 {
		t.Fatal("the guard never started anything")
	}
	if write > last {
		t.Errorf("the breadcrumb was written at %d and the guard's start is at %d; the marker "+
			"has to exist before the wait that can be killed\ntrace: %v", write, last, trace)
	}
	// And it is gone, because the start confirmed.
	if n := rig.host.wrote(hostops.FileDegraded); n < 1 {
		t.Errorf("breadcrumb writes = %d", n)
	}
	if n := rig.host.removed(hostops.FileDegraded); n != 1 {
		t.Errorf("the confirmed start removed the breadcrumb %d time(s), want 1; a marker left "+
			"behind makes gvm-guard.timer act on an instance that is already running", n)
	}
	if rig.host.body(hostops.FileDegraded) != "" {
		t.Error("the breadcrumb survived a restore that confirmed the instance is running")
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
