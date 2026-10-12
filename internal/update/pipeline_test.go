package update

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/platform"

	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/provider"
)

// newPipeline is a working pipeline with both axes quiet.
func newPipeline(t *testing.T, mutate func(*Config)) (*Pipeline, *fakeInstaller, *fakeContent, *fakeArchives) {
	t.Helper()
	inst := &fakeInstaller{local: "18452901", remote: "18452901"}
	cont := &fakeContent{}
	arch := &fakeArchives{}
	cfg := Config{
		Instance:       testInstance,
		ManagedInstall: true,
		Installer:      inst,
		Content:        cont,
		Archives:       arch,
		Disk:           roomyDisk,
		Log:            discardLogger(),
		Notes:          func(string, ...any) {},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(cfg), inst, cont, arch
}

// TestNoDiffMeansSkippedAndZeroStops is section 13.5's own required assertion,
// and it is the whole of D-43.
//
// update.sh's shape was stop -> backup -> app_update validate -> start ->
// sleep 180 -> re-resolve -> maybe restart again, every night, unconditionally.
// On a night when neither Steam nor the workshop moved -- which is most of them
// -- that is several minutes of downtime, a full validation pass and two boots,
// for nothing. The fake systemd here is the thing that would have performed
// those stops, and the assertion is that it recorded NONE.
func TestNoDiffMeansSkippedAndZeroStops(t *testing.T) {
	p, inst, _, arch := newPipeline(t, nil)
	super := &fakeSuper{active: true}
	env, _ := jobEnv(lifecycle.JobUpdate, p, super, true)

	run, err := jobs.Execute(context.Background(), env)
	if err != nil {
		t.Fatalf("a quiet nightly must not fail: %v", err)
	}
	if run.State != jobs.StateSkipped {
		t.Errorf("state = %q, want %q", run.State, jobs.StateSkipped)
	}
	if n := super.stopCount(); n != 0 {
		t.Errorf("the fake systemd recorded %d stop(s); D-43 is that a nightly with nothing to "+
			"do never touches the server", n)
	}
	if super.starts != 0 {
		t.Errorf("the fake systemd recorded %d start(s); nothing was stopped, so nothing "+
			"needed starting", super.starts)
	}
	if !super.active {
		t.Error("the instance is not active at exit")
	}
	// Nothing expensive ran either: no app_update, and no pre-update archive.
	for _, in := range inst.applied() {
		if in == provider.IntentUpdate {
			t.Error("phase 5 ran on a night with no build diff")
		}
	}
	if len(arch.refs) != 0 {
		t.Errorf("a quiet nightly took %d archive(s); the pre-update backup is for the run "+
			"that is about to change something", len(arch.refs))
	}
}

// TestAnAdoptedInstanceIsNeverStoppedForABinaryUpdateItMayNotPerform is the
// other required assertion, and it is the one that matters on the day after a
// cutover.
//
// The operator has a working nightly steamcmd update. Adoption must not take it
// away silently: the run records `skipped` with `update_available` and prints
// the one command that changes the situation. What it must never do is stop a
// live world for a download it is structurally forbidden from performing.
func TestAnAdoptedInstanceIsNeverStoppedForABinaryUpdateItMayNotPerform(t *testing.T) {
	p, inst, _, _ := newPipeline(t, func(c *Config) { c.ManagedInstall = false })
	inst.local, inst.remote = "18452901", "18461233"
	inst.updateAvailable = true
	// An adopted instance's plan is EMPTY -- gvm may write nothing into that
	// tree -- which is exactly why UpdateAvailable has to be its own field.
	inst.writes = false

	super := &fakeSuper{active: true}
	env, _ := jobEnv(lifecycle.JobUpdate, p, super, false)

	run, err := jobs.Execute(context.Background(), env)
	if err != nil {
		t.Fatalf("an adopted nightly must not fail: %v", err)
	}
	if run.State != jobs.StateSkipped {
		t.Errorf("state = %q, want %q", run.State, jobs.StateSkipped)
	}
	if n := super.stopCount(); n != 0 {
		t.Errorf("the fake systemd recorded %d stop(s) for an adopted instance", n)
	}
	if run.Summary["update_available"] != true {
		t.Errorf("summary.update_available = %v, want true; the operator must be told that the "+
			"nightly they had before adoption now reports nothing",
			run.Summary["update_available"])
	}
	// And the sentence has to carry both ids and the escape hatch.
	notes := strings.Join(run.Notes, " ")
	for _, want := range []string{"18452901", "18461233", "gvm instance promote pz-main"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the recorded reason does not mention %q: %s", want, notes)
		}
	}
	for _, in := range inst.applied() {
		if in == provider.IntentUpdate {
			t.Error("phase 5 ran against an adopted install")
		}
	}
}

// TestABuildDiffRunsThePipelineInOrder is the positive case: something really
// did change, so the stop is correct and every phase after it runs once.
func TestABuildDiffRunsThePipelineInOrder(t *testing.T) {
	p, inst, cont, arch := newPipeline(t, nil)
	inst.local, inst.remote = "18452901", "18461233"
	inst.writes, inst.bytes = true, 420<<20
	cont.plan = jobs.ContentPlan{Changed: true, Added: []string{"2169435993"}}
	cont.settle = SettleInput{}
	cont.res = jobs.Resolution{Changed: true, Before: []string{"A"}, After: []string{"A", "B"}}

	super := &fakeSuper{active: true}
	env, _ := jobEnv(lifecycle.JobUpdate, p, super, true)

	run, err := jobs.Execute(context.Background(), env)
	if err != nil {
		t.Fatalf("the pipeline failed: %v", err)
	}
	if run.State != jobs.StateOK {
		t.Errorf("state = %q, want %q (steps: %+v)", run.State, jobs.StateOK, run.Steps)
	}
	// Two stops: the update's own, and phase 10's second cycle, because the
	// activation lists changed. That second one is update.sh's final
	// `systemctl restart`, with the difference that it is conditional.
	if n := super.stopCount(); n != 2 {
		t.Errorf("stops = %d, want 2 (the update, then phase 10's cycle)", n)
	}
	if !super.active {
		t.Error("the instance is not active at exit")
	}
	if len(arch.refs) != 1 {
		t.Fatalf("archives = %d, want 1 pre-update backup", len(arch.refs))
	}
	if got := arch.refs[0].Reason; got != "pre-update" {
		t.Errorf("archive reason = %q, want %q", got, "pre-update")
	}
	if !arch.refs[0].Verified() {
		t.Error("the pre-update archive was not verified")
	}
	// Phase 5 applied the `update` intent, and phase 6 ran after it.
	applied := inst.applied()
	if len(applied) != 2 || applied[0] != provider.IntentUpdate ||
		applied[1] != provider.IntentVerify {
		t.Errorf("applied = %v, want [update verify]; D-32 makes the post-verification "+
			"mandatory after every app_update", applied)
	}
	if cont.activated != 1 {
		t.Errorf("activate ran %d time(s), want 1", cont.activated)
	}
}

// TestPhase5RefusesAnAdoptedInstallAtTheTypeLevel, belt to the provider's own
// braces. The pipeline is the earlier of the two refusals and it is the one
// that names the phase.
func TestPhase5RefusesAnAdoptedInstallAtTheTypeLevel(t *testing.T) {
	p, inst, _, _ := newPipeline(t, func(c *Config) { c.ManagedInstall = false })
	inst.local, inst.remote = "1", "2"
	inst.writes = true
	ctx := context.Background()
	if err := p.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CheckBuild(ctx); err != nil {
		t.Fatal(err)
	}
	err := p.AppUpdate(ctx)
	if !errors.Is(err, ErrAdopted) {
		t.Fatalf("err = %v, want ErrAdopted", err)
	}
	if len(inst.applied()) != 0 {
		t.Error("the driver was asked to apply something")
	}
}

// TestAppUpdateRefusesToReplanAfterTheStop.
//
// The plan phase 1 produced is the plan phase 5 applies. Re-planning inside
// phase 5 would be a network read performed with the world already down, whose
// answer the decision never saw -- and a build that moved in between would be
// installed without having been authorized against anything the operator was
// shown.
func TestAppUpdateRefusesToReplanAfterTheStop(t *testing.T) {
	p, inst, _, _ := newPipeline(t, nil)
	inst.local, inst.remote = "1", "2"
	inst.writes = true
	if err := p.AppUpdate(context.Background()); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("err = %v, want ErrOutOfOrder", err)
	}
	if n := len(inst.planned()); n != 0 {
		t.Errorf("the driver was asked to plan %d time(s) from inside phase 5", n)
	}
}

// TestPhase5NeverEmitsTheRepairIntent, which is the same statement as "a
// routine update never validates" (D-32) in the only place it could go wrong.
func TestPhase5NeverEmitsTheRepairIntent(t *testing.T) {
	p, inst, _, _ := newPipeline(t, nil)
	inst.local, inst.remote = "18452901", "18461233"
	inst.writes = true
	ctx := context.Background()
	for _, step := range []func() error{
		func() error { return p.Preflight(ctx) },
		func() error { _, err := p.CheckBuild(ctx); return err },
		func() error { _, err := p.CheckContent(ctx); return err },
		func() error { return p.AppUpdate(ctx) },
		func() error { return p.PostVerify(ctx) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	for _, in := range append(inst.planned(), inst.applied()...) {
		if in == provider.IntentRepair || in == provider.IntentInstall {
			t.Errorf("a routine update reached the %q intent; `validate` has exactly one "+
				"path and it is `gvm update <n> --repair`", in)
		}
	}
}

// TestPostVerifyRunsEvenWhenTheAppUpdateFailed.
//
// D-32's reason, and it is specific: steamcmd self-updates, re-execs, and can
// exit 0 after printing "Error!". A verification gated on the exit code would
// skip exactly the runs it was written for -- so the job layer's `When` for
// phase 6 is "a build was being applied", never "phase 5 succeeded".
func TestPostVerifyRunsEvenWhenTheAppUpdateFailed(t *testing.T) {
	p, inst, _, _ := newPipeline(t, nil)
	inst.local, inst.remote = "18452901", "18461233"
	inst.writes = true
	inst.applyErr = errors.New("Error! App '380870' state is 0x202 after update job")

	super := &fakeSuper{active: true}
	env, _ := jobEnv(lifecycle.JobUpdate, p, super, true)
	run, err := jobs.Execute(context.Background(), env)
	if err != nil {
		t.Fatalf("start_anyway means a Steam failure is not a job failure: %v", err)
	}
	if run.State != jobs.StateDegraded {
		t.Errorf("state = %q, want %q", run.State, jobs.StateDegraded)
	}
	// The verify plan was still applied. (It fails here too, because the fake
	// returns applyErr for every intent -- which is the honest simulation of
	// a broken install, and it is why the run is degraded rather than failed.)
	var sawVerify bool
	for _, in := range inst.applied() {
		if in == provider.IntentVerify {
			sawVerify = true
		}
	}
	if !sawVerify {
		t.Error("phase 6 did not run after a failed phase 5, which is the one case it exists for")
	}
	if !super.active {
		t.Error("the instance is not active at exit; an outdated server beats one left stopped")
	}
}

// TestAnUnverifiedPreUpdateArchiveIsAFailedBackup.
//
// Phase 4 is non-fatal on purpose -- update.sh's `|| warn "Backup failed -
// continuing anyway."` -- and that is precisely why "a tarball of unknown
// integrity exists" must not count as success: a non-fatal step reporting the
// wrong thing is a step whose report nobody can use.
func TestAnUnverifiedPreUpdateArchiveIsAFailedBackup(t *testing.T) {
	p, inst, _, arch := newPipeline(t, nil)
	inst.local, inst.remote = "1", "2"
	inst.writes = true
	arch.skipVerifyStamp = true

	ctx := context.Background()
	if err := p.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CheckBuild(ctx); err != nil {
		t.Fatal(err)
	}
	err := p.Backup(ctx)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig", err)
	}
	if !strings.Contains(err.Error(), "VerifiedAt") {
		t.Errorf("the message must name the field: %v", err)
	}
	if p.Ref() != nil {
		t.Error("an unverified ref was recorded as this run's backup")
	}
}

// TestPreflightRespectsAnOperatorHold.
//
// A nightly that updated a held instance would hand it back RUNNING, because
// the running guard restores what it found -- and what it found was a world a
// human put down on purpose.
func TestPreflightRespectsAnOperatorHold(t *testing.T) {
	p, _, _, _ := newPipeline(t, func(c *Config) {
		c.Held = func(context.Context) (bool, error) { return true, nil }
	})
	err := p.Preflight(context.Background())
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("err = %v, want ErrHeld", err)
	}
	if !strings.Contains(err.Error(), "gvm start pz-main") {
		t.Errorf("the refusal must say how to clear the hold: %v", err)
	}
}

// TestTheDiskRefusalHappensWithTheServerStillRunning.
//
// Section 13.5's phase 0 is free >= max(2 GiB, BytesToDownload * 1.5), and the
// download figure only exists after phase 1 -- so the scaled half is applied at
// the end of CheckBuild, which is still on the NEAR side of the decision. A
// full volume discovered after the shutdown ladder is a stopped world plus a
// failed download.
func TestTheDiskRefusalHappensWithTheServerStillRunning(t *testing.T) {
	var measured []string
	p, inst, _, _ := newPipeline(t, func(c *Config) {
		// Room for the 2 GiB floor, not for 1.5 * 8 GiB.
		c.Disk = func(path string) (platform.DiskUsage, error) {
			measured = append(measured, path)
			return tightDisk(3 << 30)(path)
		}
		// The install tree, not the state directory: that is where the depot
		// download lands, and on an adopted box the two are different
		// filesystems.
		c.DiskPath = "/opt/pzserver"
	})
	inst.local, inst.remote = "1", "2"
	inst.writes, inst.bytes = true, 8<<30

	ctx := context.Background()
	if err := p.Preflight(ctx); err != nil {
		t.Fatalf("the phase-0 floor is met, so preflight must pass: %v", err)
	}
	_, err := p.CheckBuild(ctx)
	if !errors.Is(err, ErrNoRoom) {
		t.Fatalf("err = %v, want ErrNoRoom", err)
	}
	for _, m := range measured {
		if m != "/opt/pzserver" {
			t.Errorf("measured %q; the volume that matters is the one the download lands on", m)
		}
	}
}

// TestAStatfsThatCannotAnswerDoesNotRefuse.
//
// The development machine has no statfs at all. A pipeline that refused every
// run there would be a pipeline nobody ran, so an unmeasurable volume is
// recorded and allowed through -- and on Linux, where the measurement always
// happens, the refusal above is the one that fires.
func TestAStatfsThatCannotAnswerDoesNotRefuse(t *testing.T) {
	var notes []string
	p, _, _, _ := newPipeline(t, func(c *Config) {
		c.Disk = nil // platform.Statfs, which refuses off Linux
		c.Notes = func(f string, a ...any) { notes = append(notes, f) }
	})
	err := p.Preflight(context.Background())
	if err != nil && errors.Is(err, ErrNoRoom) {
		t.Fatalf("an unmeasurable volume must not be reported as a full one: %v", err)
	}
}

// TestNoContentLayerIsNotAContentDiff.
//
// Valheim's honest answer. Inventing a content change for a game with no
// content block would stop a server to write a list that does not exist.
func TestNoContentLayerIsNotAContentDiff(t *testing.T) {
	p, _, _, _ := newPipeline(t, func(c *Config) { c.Content = nil })
	d, err := p.CheckContent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Changed {
		t.Error("an instance with no content declared a content change")
	}
	if d.Note == "" {
		t.Error("and it said nothing about why")
	}
	st, err := p.Settle(context.Background())
	if err != nil || !st.Positive {
		t.Errorf("Settle = %+v, %v; with nothing to wait for the answer is immediate and "+
			"positive", st, err)
	}
}
