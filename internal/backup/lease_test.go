package backup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// leaseOwnerEnv names the directory the child process arms its lease in. Its
// presence is also what tells the child it IS the child.
const leaseOwnerEnv = "GVM_TEST_FREEZE_LEASE_DIR"

// TestTheFreezeLeaseOwnerChild is the process that gets killed.
//
// It is a test function because that is how a Go test binary re-executes
// itself, and it is a real separate process because the failure D-31 exists
// for is a real separate process dying. Modelling the owner's death as a
// boolean in the parent would test the branch and not the mechanism: the whole
// claim is that the guard survives the guarded process being destroyed by the
// kernel, and only an actual kill demonstrates that.
func TestTheFreezeLeaseOwnerChild(t *testing.T) {
	dir := os.Getenv(leaseOwnerEnv)
	if dir == "" {
		t.Skip("not the lease-owner child; this runs only when re-executed by " +
			"TestKillingTheLeaseOwnerStillThawsAtTheDeadline")
	}
	at := time.Now().UTC()
	if _, _, err := Arm(context.Background(), OS, ArmRequest{
		Path: filepath.Join(dir, "freeze.lease"),
		Lease: Lease{
			Instance:  "pz-main",
			Reason:    "backup:routine",
			OwnerPID:  os.Getpid(),
			OwnerUnit: "gvm-job@pz-main_nightly.service",
			ArmedAt:   at,
			Deadline:  at.Add(2 * time.Minute),
			ThawSteps: []ThawStep{{Via: ViaControl, Send: "save-on"}},
		},
	}); err != nil {
		t.Fatalf("child could not arm the lease: %v", err)
	}
	// Now behave like a backup job between save-off and save-on, and wait to
	// be destroyed. The parent kills this process; the sleep is only a bound
	// so that a parent which failed to kill it does not leave it forever.
	time.Sleep(2 * time.Minute)
}

// TestKillingTheLeaseOwnerStillThawsAtTheDeadline is D-31.
//
// A child process arms a freeze lease and is then SIGKILLed (TerminateProcess
// on the development machine), which is precisely the failure the lease
// exists for: a backup that died between `save-off` and `save-on`, leaving a
// running server that has silently stopped persisting. Nothing in the dead
// process can help -- its defer is gone, its goroutine is gone -- so the only
// thing left is the file it wrote and a different process willing to honour it.
//
// The fake runner's liveness probe deliberately answers Unknown, which is the
// honest answer for a pid a runner cannot inspect and the HOSTILE case: the
// runner cannot tell that the owner is dead, so nothing but the deadline saves
// the world. The early-thaw path, where the owner is provably gone, is the
// test below.
func TestKillingTheLeaseOwnerStillThawsAtTheDeadline(t *testing.T) {
	dir := t.TempDir()
	leasePath := filepath.Join(dir, "freeze.lease")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTheFreezeLeaseOwnerChild$")
	cmd.Env = append(os.Environ(), leaseOwnerEnv+"="+dir)
	if err := cmd.Start(); err != nil {
		t.Skipf("this test binary cannot re-execute itself: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Wait for the owner to get as far as arming.
	var armed *Lease
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		l, err := ReadLease(OS, leasePath)
		if err == nil && l != nil {
			armed = l
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if armed == nil {
		t.Fatal("the child never armed a freeze lease, so there is nothing to honour")
	}
	if armed.OwnerPID != cmd.Process.Pid {
		t.Errorf("lease owner_pid = %d, want the child's %d", armed.OwnerPID, cmd.Process.Pid)
	}

	// Kill the owner. This is the whole premise: everything in that process
	// that could have thawed the world is now gone.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("could not kill the lease owner: %v", err)
	}
	if _, err := cmd.Process.Wait(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Logf("waiting on the killed owner: %v", err)
	}
	if l, err := ReadLease(OS, leasePath); err != nil || l == nil {
		t.Fatalf("the lease did not survive its owner (lease=%v err=%v); a watchdog that dies "+
			"with the process it guards is the goroutine this design replaces", l, err)
	}

	// A different process, honouring the file. The clock is moved past the
	// deadline rather than waited out.
	thawer := &fakeThawer{}
	clock := newClock(armed.Deadline.Add(-time.Second))
	w := &Watcher{
		Path: leasePath,
		FS:   OS,
		Thaw: thawer,
		// Unknown: the hostile case. The runner cannot establish whether the
		// owner is alive, so only the deadline acts.
		Alive: func(int, uint64) lifecycle.Tri { return lifecycle.Unknown },
		Now:   clock.Now,
		Log:   quietLogger(),
	}

	if tick := w.Step(context.Background()); tick.Decision != DecisionWait {
		t.Fatalf("one second before the deadline the decision is %q, want %q: thawing early on "+
			"an inconclusive probe would re-enable autosave in the middle of somebody's snapshot",
			tick.Decision, DecisionWait)
	}
	if len(thawer.steps()) != 0 {
		t.Fatalf("the runner thawed before the deadline: %+v", thawer.steps())
	}

	clock.Advance(2 * time.Second)
	tick := w.Step(context.Background())
	if tick.Decision != DecisionThaw {
		t.Fatalf("past the deadline the decision is %q, want %q", tick.Decision, DecisionThaw)
	}
	if !tick.Thawed {
		t.Fatalf("the thaw did not complete: %v", tick.Err)
	}
	got := thawer.steps()
	if len(got) != 1 || got[0].Via != ViaControl || got[0].Send != "save-on" {
		t.Fatalf("the runner sent %+v, want one control save-on", got)
	}
	if !tick.Cleared {
		t.Error("a completed thaw must clear the lease; leaving it would thaw again every tick")
	}
	if l, err := ReadLease(OS, leasePath); err != nil || l != nil {
		t.Errorf("the lease is still on disk after a successful thaw (lease=%v err=%v)", l, err)
	}
}

// TestSweepOnRunnerStartThawsAnAlreadyExpiredLease is the other half of D-31's
// failure table.
//
// The runner was SIGKILLed while a backup held a freeze.
// RuntimeDirectoryPreserve=restart keeps /run/gvm/<name> across the restart, so
// systemd brings a fresh runner up next to a lease that expired while nobody
// was watching it. A runner that armed timers only for leases it saw APPEAR
// would never look at that one, and the world would stay unsaved until
// somebody noticed.
func TestSweepOnRunnerStartThawsAnAlreadyExpiredLease(t *testing.T) {
	f := newFS()
	p := "/run/gvm/pz-main/freeze.lease"
	mustArm(t, f, ArmRequest{
		Path:  p,
		Owner: "gvm-pz-main",
		Lease: armed("pz-main", testNow, 2*time.Minute, ThawStep{Via: ViaControl, Send: "save-on"}),
	})

	thawer := &fakeThawer{}
	w := &Watcher{
		Path: p, FS: f, Thaw: thawer,
		// The box has been up a while: the clock is well past the deadline.
		Now: newClock(testNow.Add(time.Hour)).Now,
		Log: quietLogger(),
	}
	tick := w.Sweep(context.Background())
	if tick.Decision != DecisionThaw || !tick.Thawed {
		t.Fatalf("Sweep returned %+v; an expired lease found at runner start must be thawed", tick)
	}
	if got := thawer.steps(); len(got) != 1 || got[0].Send != "save-on" {
		t.Fatalf("Sweep sent %+v, want save-on", got)
	}
	if f.has(p) {
		t.Error("Sweep thawed and did not clear the lease")
	}
}

// TestAProvablyDeadOwnerIsThawedBeforeTheDeadline.
//
// The deadline is the backstop, not the signal. When the runner CAN establish
// that the owner is gone -- a dead pid, on a host with /proc -- waiting for the
// deadline would leave the world unsaved for the rest of the budget for no
// reason. The asymmetry is in the safe direction: a pid that cannot be
// inspected, or has been reused, reads as alive and the deadline still applies.
func TestAProvablyDeadOwnerIsThawedBeforeTheDeadline(t *testing.T) {
	f := newFS()
	p := "/run/gvm/pz-main/freeze.lease"
	mustArm(t, f, ArmRequest{Path: p, Owner: "gvm-pz-main",
		Lease: armed("pz-main", testNow, 30*time.Minute, ThawStep{Via: ViaConsole, Send: "save-on"})})

	thawer := &fakeThawer{}
	w := &Watcher{
		Path: p, FS: f, Thaw: thawer,
		Alive: func(int, uint64) lifecycle.Tri { return lifecycle.No },
		Now:   newClock(testNow.Add(time.Second)).Now,
		Log:   quietLogger(),
	}
	tick := w.Step(context.Background())
	if tick.Decision != DecisionThaw || !tick.Thawed {
		t.Fatalf("Step returned %+v with a provably dead owner 29 minutes before the deadline", tick)
	}
	if !strings.Contains(tick.Why, "is gone") {
		t.Errorf("the reason does not say the owner is gone: %q", tick.Why)
	}
}

// TestAnUnpinnedOwnerIsNeverProvablyDead.
//
// The field's own contract: "Zero means 'not recorded', which the liveness
// check reads as 'cannot establish' rather than as 'gone'." This is where that
// has to be true, because the obvious wiring gets it backwards.
// platform.Alive(pid, want) ends in `st.StartTicks == want`, so a perfectly
// LIVE process read against a zero pin answers (false, nil) -- "provably
// gone". A watcher that passed that through would send save-on on its first
// five-second poll of every unpinned lease, in the middle of somebody's
// snapshot, and then clear the lease that was the only evidence. There is
// already one adapter in the tree shaped that way (cli's aliveTri calls
// platform.Alive(pid, 0)), so the guard belongs here, where the decision is
// made, rather than in each caller.
func TestAnUnpinnedOwnerIsNeverProvablyDead(t *testing.T) {
	f := newFS()
	p := "/run/gvm/pz-main/freeze.lease"
	l := armed("pz-main", testNow, 30*time.Minute, ThawStep{Via: ViaConsole, Send: "save-on"})
	l.OwnerStartTicks = 0
	mustArm(t, f, ArmRequest{Path: p, Owner: "gvm-pz-main", Lease: l})

	thawer := &fakeThawer{}
	w := &Watcher{
		Path: p, FS: f, Thaw: thawer,
		// A constant No, which is what an unpinned platform.Alive returns for
		// a live process.
		Alive: func(int, uint64) lifecycle.Tri { return lifecycle.No },
		Now:   newClock(testNow.Add(time.Second)).Now,
		Log:   quietLogger(),
	}
	tick := w.Step(context.Background())
	if tick.Decision != DecisionWait {
		t.Fatalf("Step on an UNPINNED lease decided %q, want %q: without field 22 there is no "+
			"way to tell a dead owner from a recorded-nothing one, so only the deadline may act",
			tick.Decision, DecisionWait)
	}
	if got := thawer.steps(); len(got) != 0 {
		t.Fatalf("the watchdog sent %+v into the middle of a live snapshot", got)
	}
	if !f.has(p) {
		t.Error("the lease was cleared while its owner may still be working")
	}
}

// TestAnUnreadableLeaseIsThawedFromTheRunnersOwnManifest.
//
// A truncated or corrupt lease is treated as a freeze that IS armed. The
// alternative is to leave a possibly-frozen world unsaved because a JSON
// document lost its tail, and the two mistakes are not symmetric: re-sending
// `save-on` to a server that is already saving is a no-op.
//
// The thaw comes from the runner's own manifest, which it has anyway -- it
// rendered the shutdown ladder from it -- so the unreadable case still has a
// correct thaw available rather than only a log line.
func TestAnUnreadableLeaseIsThawedFromTheRunnersOwnManifest(t *testing.T) {
	f := newFS()
	p := "/run/gvm/pz-main/freeze.lease"
	if err := f.WriteFile(p, []byte(`{"schema":1,"instance":"pz-m`), LeaseFileMode); err != nil {
		t.Fatal(err)
	}
	thawer := &fakeThawer{}
	w := &Watcher{
		Path: p, FS: f, Thaw: thawer,
		Fallback: []ThawStep{{Via: ViaControl, Send: "save-on"}},
		Now:      newClock(testNow).Now,
		Log:      quietLogger(),
	}
	tick := w.Step(context.Background())
	if tick.Decision != DecisionUnreadable {
		t.Fatalf("decision = %q, want %q", tick.Decision, DecisionUnreadable)
	}
	if got := thawer.steps(); len(got) != 1 || got[0].Send != "save-on" {
		t.Fatalf("the fallback thaw sent %+v, want the manifest's save-on", got)
	}
	if !tick.Thawed || f.has(p) {
		t.Error("a successful fallback thaw must complete and clear the lease")
	}
}

// TestAFailedThawKeepsTheLease.
//
// The lease is the only remaining evidence that this world may not be saving,
// and lifecycle's freeze_armed finding is what surfaces it to doctor and the
// dashboard. Clearing it after a thaw that did not land would delete the
// breadcrumb and leave a silently-unsaving server with nothing anywhere saying
// so.
func TestAFailedThawKeepsTheLease(t *testing.T) {
	f := newFS()
	p := "/run/gvm/pz-main/freeze.lease"
	mustArm(t, f, ArmRequest{Path: p, Owner: "gvm-pz-main",
		Lease: armed("pz-main", testNow, time.Minute, ThawStep{Via: ViaControl, Send: "save-on"})})

	w := &Watcher{
		Path: p, FS: f,
		Thaw: &fakeThawer{Fail: errInjected},
		Now:  newClock(testNow.Add(time.Hour)).Now,
		Log:  quietLogger(),
	}
	tick := w.Step(context.Background())
	if tick.Thawed {
		t.Fatal("a thaw whose command failed reported success")
	}
	if tick.Cleared || !f.has(p) {
		t.Fatal("the lease was cleared after a failed thaw; it is the only record left that " +
			"autosave may still be off")
	}
	if tick.Err == nil {
		t.Error("no error was reported for a thaw that did not land")
	}
}

// TestARunnerWithNoChannelDoesNotClaimToHaveThawed.
//
// An honest failure. A runner that cannot reach its own game must not report a
// world as saving again, because the lease would then be cleared and nobody
// would ever look at it.
func TestARunnerWithNoChannelDoesNotClaimToHaveThawed(t *testing.T) {
	f := newFS()
	p := "/run/gvm/pz-main/freeze.lease"
	mustArm(t, f, ArmRequest{Path: p, Owner: "gvm-pz-main",
		Lease: armed("pz-main", testNow, time.Minute, ThawStep{Via: ViaControl, Send: "save-on"})})

	w := &Watcher{Path: p, FS: f, Now: newClock(testNow.Add(time.Hour)).Now, Log: quietLogger()}
	tick := w.Step(context.Background())
	if tick.Thawed || tick.Cleared {
		t.Fatalf("a runner with no thaw channel reported %+v", tick)
	}
}

// TestALeaseWithNoThawStepsIsCompleteWithNothingSent.
//
// `after: none` is how every shipped manifest except Minecraft's spells a
// one-shot flush that has no mode to toggle back -- Project Zomboid's `save`,
// Palworld's `Save`, 7 Days to Die's `saveworld`. For those the lease is cheap
// insurance and the correct thaw is to send nothing, so the tick completes
// and clears without needing a channel at all.
func TestALeaseWithNoThawStepsIsCompleteWithNothingSent(t *testing.T) {
	f := newFS()
	p := "/run/gvm/pz-main/freeze.lease"
	mustArm(t, f, ArmRequest{Path: p, Owner: "gvm-pz-main",
		Lease: armed("pz-main", testNow, time.Minute)})

	thawer := &fakeThawer{}
	w := &Watcher{Path: p, FS: f, Thaw: thawer,
		Now: newClock(testNow.Add(time.Hour)).Now, Log: quietLogger()}
	tick := w.Step(context.Background())
	if !tick.Thawed || !tick.Cleared {
		t.Fatalf("tick = %+v, want a complete thaw", tick)
	}
	if len(thawer.steps()) != 0 {
		t.Errorf("nothing should have been sent, got %+v", thawer.steps())
	}
}

// TestNoLeaseIsNotAnEvent: the normal state of every instance, most of the
// time. It must cost one stat and produce no decision.
func TestNoLeaseIsNotAnEvent(t *testing.T) {
	w := &Watcher{Path: "/run/gvm/pz-main/freeze.lease", FS: newFS(),
		Now: newClock(testNow).Now, Log: quietLogger()}
	if tick := w.Step(context.Background()); tick.Decision != DecisionNone {
		t.Fatalf("decision = %q, want %q", tick.Decision, DecisionNone)
	}
}

// TestTheLeaseIsWrittenZeroSixHundredAndHandedToTheInstanceAccount.
//
// Section 5 says 0600, and the owner is the instance account rather than root
// because the file is WRITTEN by the job (root) and READ by the runner, which
// runs as gvm-<name> under User= in the generated drop-in. Root-owned 0600
// would be a safety net the runner cannot see.
func TestTheLeaseIsWrittenZeroSixHundredAndHandedToTheInstanceAccount(t *testing.T) {
	f := newFS()
	p := "/run/gvm/pz-main/freeze.lease"
	mustArm(t, f, ArmRequest{Path: p, Owner: "pzserver",
		Lease: armed("pz-main", testNow, time.Minute, ThawStep{Via: ViaControl, Send: "save-on"})})

	if got := f.mode(p); got != LeaseFileMode {
		t.Errorf("lease mode = %v, want %v", got, LeaseFileMode)
	}
	if got := f.owner(p); got != "pzserver" {
		t.Errorf("lease owner = %q, want the account from the instance YAML, not a derived "+
			"gvm-pz-main: an adopted instance runs as pzserver (section 6.4)", got)
	}
}

// TestAFailedChownFailsTheArmAndLeavesNoLease.
//
// This is the one chown in gvm that is fatal. A lease the runner cannot read
// exists and does nothing, which is indistinguishable from not having armed one
// -- except that the backup believes it is protected and goes on to send
// `save-off`. Refusing here means the freeze never starts.
//
// The half-armed file is removed too: a 0600 root-owned lease left on disk
// would show up in lifecycle's freeze_armed finding as a freeze nobody armed.
func TestAFailedChownFailsTheArmAndLeavesNoLease(t *testing.T) {
	f := newFS()
	f.FailChown = "freeze.lease"
	p := "/run/gvm/pz-main/freeze.lease"
	_, _, err := Arm(context.Background(), f, ArmRequest{Path: p, Owner: "gvm-pz-main",
		Lease: armed("pz-main", testNow, time.Minute, ThawStep{Via: ViaControl, Send: "save-on"})})
	if !errors.Is(err, ErrArm) {
		t.Fatalf("Arm error = %v, want ErrArm", err)
	}
	if !strings.Contains(err.Error(), "no watchdog") {
		t.Errorf("the message does not say what was lost: %v", err)
	}
	if f.has(p) {
		t.Error("the half-armed lease was left on disk")
	}
	// Nothing was ever published, so there was no window in which the runner
	// could have read an unreadable lease either. See the test below.
	if len(f.Writes) != 0 {
		t.Errorf("a failed chown still published the lease: %v", f.Writes)
	}
}

// TestTheLeaseIsNeverPublishedWithoutItsOwner.
//
// Arm used to WriteFile and then Chown, which published a root-owned 0600
// lease and handed ownership over afterwards. On the real /run/gvm/<name>
// (2750 gvm-<n>:gvm-console, with the runner under User=gvm-<n>) the runner
// cannot open that file -- and ReadLease special-cases only fs.ErrNotExist, so
// EACCES becomes ErrLease, Watcher.Step calls it DecisionUnreadable, and the
// "unreadable means armed" rule thaws and then CLEARS a freeze that was armed
// a microsecond earlier. With an empty Fallback it clears it having sent
// nothing at all, which destroys D-31's only on-disk guard for the rest of the
// snapshot.
//
// The window is a passwd lookup plus one syscall and the poll is five seconds,
// so it is rare rather than routine -- but the package's own standard already
// condemns the state: Arm makes a failed chown FATAL because "a lease the
// runner cannot read exists and does nothing, which is indistinguishable from
// not having armed one at all except that the backup believes it is
// protected". Publication has to be one owned step, for the same reason the
// mode is set on the temporary file rather than after the rename.
//
// The reader is run from inside the publish, through the write hook, which is
// how a one-syscall race becomes a deterministic test on the development
// machine.
func TestTheLeaseIsNeverPublishedWithoutItsOwner(t *testing.T) {
	f := newFS()
	// The runner's account, which is not the job's: the fake refuses a 0600
	// file whose owner is somebody else, exactly as the kernel would.
	f.ReadAs = "gvm-pz-main"
	p := "/run/gvm/pz-main/freeze.lease"

	thawer := &fakeThawer{}
	w := &Watcher{
		Path: p, FS: f, Thaw: thawer,
		// No Fallback, which is the harmful case: an unreadable lease with no
		// manifest steps behind it is cleared having sent nothing.
		//
		// The clock is INSIDE the budget, which is what makes this sharp: a
		// lease the runner can read is DecisionWait and nothing happens, so
		// the only route to a thaw during the publish is unreadability.
		Now: newClock(testNow.Add(time.Second)).Now,
		Log: quietLogger(),
	}

	var seen []Decision
	f.onWrite = func(path string) {
		if !strings.Contains(path, "freeze.lease") {
			return
		}
		seen = append(seen, w.Step(context.Background()).Decision)
	}
	mustArm(t, f, ArmRequest{Path: p, Owner: "gvm-pz-main",
		Lease: armed("pz-main", testNow, time.Minute, ThawStep{Via: ViaControl, Send: "save-on"})})
	f.onWrite = nil

	if len(seen) == 0 {
		t.Fatal("the reader never ran, so this test asserts nothing")
	}
	for _, d := range seen {
		switch d {
		case DecisionNone, DecisionWait:
		default:
			t.Fatalf("the runner saw %q while the lease was being armed; an unreadable lease is "+
				"read as a freeze that IS armed, so it would have thawed a freeze armed a "+
				"microsecond earlier and cleared the only record of it", d)
		}
	}
	if got := thawer.steps(); len(got) != 0 {
		t.Errorf("the watchdog sent %+v during the arm", got)
	}
	// And the armed lease is readable by the runner, which is the whole point
	// of the chown.
	if l, err := ReadLease(f, p); err != nil || l == nil {
		t.Fatalf("the runner cannot read the armed lease: %v, %v", l, err)
	}
	if got := f.owner(p); got != "gvm-pz-main" {
		t.Errorf("lease owner = %q, want the instance user", got)
	}
	if got := f.mode(p); got != LeaseFileMode {
		t.Errorf("lease mode = %v, want %v", got, LeaseFileMode)
	}
}

// TestArmRefusesALeaseThatIsNotAWatchdog.
//
// Every refusal here is fatal to the backup on purpose: a freeze with D-31's
// guard removed is worth less than the tarball it would produce.
func TestArmRefusesALeaseThatIsNotAWatchdog(t *testing.T) {
	cases := []struct {
		name  string
		lease Lease
		want  string
	}{
		{
			name:  "no deadline",
			lease: Lease{Instance: "pz-main", ArmedAt: testNow},
			want:  "is not a watchdog",
		},
		{
			name:  "deadline before armed_at",
			lease: Lease{Instance: "pz-main", ArmedAt: testNow, Deadline: testNow.Add(-time.Minute)},
			want:  "is not after armed_at",
		},
		{
			name:  "past the ceiling",
			lease: Lease{Instance: "pz-main", ArmedAt: testNow, Deadline: testNow.Add(2 * MaxLeaseBudget)},
			want:  "ceiling",
		},
		{
			name: "a thaw step on a channel a freeze cannot use",
			lease: Lease{Instance: "pz-main", ArmedAt: testNow, Deadline: testNow.Add(time.Minute),
				ThawSteps: []ThawStep{{Via: "signal", Send: "SIGTERM"}}},
			want: "thaw_steps[0].via",
		},
		{
			name: "a thaw step that sends nothing",
			lease: Lease{Instance: "pz-main", ArmedAt: testNow, Deadline: testNow.Add(time.Minute),
				ThawSteps: []ThawStep{{Via: ViaControl, Send: "  "}}},
			want: "sends nothing",
		},
		{
			name:  "no instance",
			lease: Lease{ArmedAt: testNow, Deadline: testNow.Add(time.Minute)},
			want:  "no instance",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFS()
			_, _, err := Arm(context.Background(), f, ArmRequest{
				Path: "/run/gvm/pz-main/freeze.lease", Lease: tc.lease})
			if !errors.Is(err, ErrArm) {
				t.Fatalf("Arm error = %v, want ErrArm", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message %q does not contain %q", err, tc.want)
			}
			if f.has("/run/gvm/pz-main/freeze.lease") {
				t.Error("a refused arm wrote a lease anyway")
			}
		})
	}
}

// TestAZeroDeadlineReadsAsExpired.
//
// The fail-safe direction. A document whose deadline is missing describes a
// freeze with no watchdog behind it, and the correct response to "I cannot tell
// when this expires" is to thaw now rather than to wait forever.
func TestAZeroDeadlineReadsAsExpired(t *testing.T) {
	l := &Lease{Instance: "pz-main", ArmedAt: testNow}
	if !l.Expired(testNow) {
		t.Fatal("a lease with no deadline read as live")
	}
	if got := l.Remaining(testNow); got != 0 {
		t.Errorf("Remaining = %v, want 0", got)
	}
	if !strings.Contains(l.Describe(testNow), "already expired") {
		t.Errorf("Describe does not say so: %q", l.Describe(testNow))
	}
}

// TestParseLeaseRefusesWhatItCannotDecide.
func TestParseLeaseRefusesWhatItCannotDecide(t *testing.T) {
	cases := map[string]string{
		"a schema from a future gvm":     `{"schema":2,"instance":"pz-main","armed_at":"2026-09-04T04:00:03Z"}`,
		"no instance":                    `{"schema":1,"armed_at":"2026-09-04T04:00:03Z"}`,
		"no armed_at":                    `{"schema":1,"instance":"pz-main"}`,
		"a thaw step on a bad channel":   `{"schema":1,"instance":"pz-main","armed_at":"2026-09-04T04:00:03Z","thaw_steps":[{"via":"signal","send":"x"}]}`,
		"a thaw step that sends nothing": `{"schema":1,"instance":"pz-main","armed_at":"2026-09-04T04:00:03Z","thaw_steps":[{"via":"console","send":""}]}`,
		"not json at all":                `{nope`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseLease(strings.NewReader(body)); !errors.Is(err, ErrLease) {
				t.Fatalf("ParseLease error = %v, want ErrLease", err)
			}
		})
	}
}

// TestALeaseRoundTripsThroughItsOwnBytes: the document the runner reads is the
// document the job wrote, field for field. It is the whole interface between
// two processes, so it gets an explicit test rather than being implied by the
// ones above.
func TestALeaseRoundTripsThroughItsOwnBytes(t *testing.T) {
	want := armed("pz-main", testNow, 2*time.Minute,
		ThawStep{Via: ViaControl, Send: "save-on"},
		ThawStep{Via: ViaConsole, Send: "save"})
	b, err := want.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseLease(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("ParseLease: %v\n%s", err, b)
	}
	if got.Instance != want.Instance || got.Reason != want.Reason ||
		got.OwnerPID != want.OwnerPID || got.OwnerUnit != want.OwnerUnit ||
		!got.ArmedAt.Equal(want.ArmedAt) || !got.Deadline.Equal(want.Deadline) ||
		len(got.ThawSteps) != 2 || got.ThawSteps[1].Send != "save" {
		t.Fatalf("round trip lost something:\n got %+v\nwant %+v", got, want)
	}
	if got.Schema != LeaseSchema {
		t.Errorf("schema = %d, want %d", got.Schema, LeaseSchema)
	}
}

// TestRenewOnlyEverWritesALeaseArmWouldHaveAccepted.
//
// Renew exists for the heartbeat around a long direct snapshot, and it fails
// safe by construction: if the renewing code dies, no later deadline is
// written and the runner thaws at the one already on disk. What it must not do
// is let a heartbeat write a document the initial arm would have refused.
func TestRenewOnlyEverWritesALeaseArmWouldHaveAccepted(t *testing.T) {
	f := newFS()
	p := "/run/gvm/pz-main/freeze.lease"
	l := mustArm(t, f, ArmRequest{Path: p, Owner: "gvm-pz-main",
		Lease: armed("pz-main", testNow, 2*time.Minute, ThawStep{Via: ViaControl, Send: "save-on"})})

	next, err := Renew(context.Background(), f, p, l, testNow.Add(10*time.Minute), "gvm-pz-main")
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if !next.Deadline.Equal(testNow.Add(10 * time.Minute)) {
		t.Errorf("deadline = %v, want the renewed one", next.Deadline)
	}
	// The write REPLACES the file rather than editing it, so a renewal that
	// forgot the owner would leave a root-owned 0600 lease where a
	// gvm-pz-main-owned one had been: still on disk, still claiming a freeze
	// is armed, and no longer readable by the runner meant to honour it.
	if got := f.owner(p); got != "gvm-pz-main" {
		t.Errorf("after Renew the lease owner is %q; the runner can no longer read it", got)
	}
	if got := f.mode(p); got != LeaseFileMode {
		t.Errorf("after Renew the lease mode is %v, want %v", got, LeaseFileMode)
	}
	if _, err := Renew(context.Background(), f, p, l, testNow.Add(2*MaxLeaseBudget), "gvm-pz-main"); !errors.Is(err, ErrArm) {
		t.Errorf("a renewal past the ceiling was accepted: %v", err)
	}
	// And the on-disk document is still the last ACCEPTED one, not the
	// refused one: a rejected renewal must not have moved the deadline.
	onDisk, err := ReadLease(f, p)
	if err != nil {
		t.Fatal(err)
	}
	if !onDisk.Deadline.Equal(testNow.Add(10 * time.Minute)) {
		t.Errorf("on-disk deadline = %v after a refused renewal", onDisk.Deadline)
	}
}

// TestBudgetForCoversTheWorkInsideTheFreeze.
//
// The budget has to cover whatever happens between save-off and save-on, and
// that differs by snapshot family: a reflink clone is milliseconds and the
// archive comes afterwards, while a direct tar IS the freeze. A 120-second
// lease on a seventy-four-second direct archive of an 8 GB world would thaw in
// the middle of the read -- and archive.Create's own two-pass digest check
// would then report ErrDrifted on a world that was changing underneath it.
func TestBudgetForCoversTheWorkInsideTheFreeze(t *testing.T) {
	const declared = 2 * time.Minute
	const gig = int64(1) << 30

	fast, why := BudgetFor(declared, MethodReflink, 8*gig, 20)
	if fast != declared {
		t.Errorf("reflink budget = %v, want the declared %v: the archive is written after the "+
			"thaw", fast, declared)
	}
	if !strings.Contains(why, "thaws before the archive") {
		t.Errorf("reason = %q", why)
	}

	slow, why := BudgetFor(declared, MethodDirect, 8*gig, 20)
	if slow <= declared {
		t.Fatalf("direct budget = %v, want more than the declared %v", slow, declared)
	}
	// 8 GiB at a 20 MB/s floor is 409 seconds, plus the declared two minutes.
	if want := declared + 409*time.Second; slow != want {
		t.Errorf("direct budget = %v, want %v", slow, want)
	}
	if !strings.Contains(why, "inside the freeze") {
		t.Errorf("reason = %q", why)
	}

	capped, why := BudgetFor(declared, MethodDirect, 4096*gig, 20)
	if capped != MaxLeaseBudget {
		t.Errorf("budget = %v, want the %v ceiling", capped, MaxLeaseBudget)
	}
	if !strings.Contains(why, "capped") {
		t.Errorf("a capped budget must say so: %q", why)
	}

	if got, _ := BudgetFor(0, MethodReflink, 0, 0); got != DefaultLeaseBudget {
		t.Errorf("an undeclared budget = %v, want %v", got, DefaultLeaseBudget)
	}

	// The two cells the table above omits, and they are the ones that reach
	// the fallback sentence with a method that does NOT thaw before the
	// archive. Below floorMBps MiB the arithmetic truncates to zero seconds,
	// so no extension is needed -- which is correct -- but the sentence then
	// told the operator that a `direct` snapshot thaws before its archive is
	// written, which is the opposite of what direct does: it is the one
	// method for which the freeze IS the archive. LeaseWhy is persisted as
	// job_runs' freeze_lease_why, and extendForDirect reuses it to say "the
	// freeze lease was extended to <t>: the direct snapshot thaws before the
	// archive is written" -- self-contradictory, on the exact fallback path
	// D-31 exists for.
	//
	// Reachable on any small world on ext4 (Choose's default branch): a
	// Valheim or Core Keeper save, or any instance backed up before its world
	// has grown.
	for _, bytes := range []int64{0, 12 << 20, 19 << 20} {
		b, why := BudgetFor(declared, MethodDirect, bytes, 20)
		if b != declared {
			t.Errorf("direct budget at %d bytes = %v, want the declared %v", bytes, b, declared)
		}
		if strings.Contains(why, "thaws before the archive") {
			t.Errorf("direct at %d bytes reports a thaw before the archive: %q", bytes, why)
		}
		if !strings.Contains(why, "inside the freeze") {
			t.Errorf("direct at %d bytes = %q, want it to say the archive is inside the freeze",
				bytes, why)
		}
	}
}
