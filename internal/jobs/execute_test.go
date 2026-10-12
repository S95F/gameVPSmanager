package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// TestNoDiffMeansSkippedAndZeroStops is D-43, and it is the biggest single win
// over update.sh.
//
// The bash script stops unconditionally and runs `app_update ... validate`
// every night. Here phases 1 and 2 are network reads, the decision is a pure
// function over their answers, and on a night with no build change and no
// content change the fake systemd records ZERO stop calls. The server is not
// touched, and `skipped` is a success that must not page anyone.
func TestNoDiffMeansSkippedAndZeroStops(t *testing.T) {
	rig := newRig(t, lifecycle.JobUpdate, testInstance)
	rig.updates.build = Diff{Changed: false, From: "18452901", To: "18452901"}
	rig.updates.content = Diff{Changed: false}

	jr, err := Execute(context.Background(), rig.env)
	if err != nil {
		t.Fatalf("a skipped update reported an error: %v\n%s", err, jr.Report())
	}
	if jr.State != StateSkipped {
		t.Errorf("state %q, want skipped:\n%s", jr.State, jr.Report())
	}
	stops, starts := rig.super.counts()
	if stops != 0 {
		t.Errorf("%d stop call(s) on a night with nothing to do", stops)
	}
	if starts != 0 {
		t.Errorf("%d start call(s) on a night with nothing to do", starts)
	}
	if !rig.super.isActive() {
		t.Error("the server is not running after a skipped update")
	}
	if rig.inj.count("update.app_update") != 0 {
		t.Error("a skipped update ran app_update")
	}
	if rig.inj.count("update.backup") != 0 {
		t.Error("a skipped update took a backup")
	}
	if !strings.Contains(jr.SkipReason, "no Steam build change") {
		t.Errorf("skip reason = %q", jr.SkipReason)
	}
	// Every later phase is RECORDED as skipped rather than omitted: "phase 5
	// did not run because the build had not changed" is the answer an
	// operator wants, and an absent row is not it.
	var skipped int
	for _, s := range jr.Steps {
		if s.Skipped {
			skipped++
		}
	}
	if skipped < 5 {
		t.Errorf("only %d phases recorded as skipped:\n%s", skipped, jr.Report())
	}
}

// TestAnAdoptedInstanceIsNeverStoppedForABinaryUpdateItMayNotPerform.
//
// Section 13.5: phase 5 is refused at the type level for managed_install:
// false, so a build diff records state='skipped', summary.update_available=
// true, and prints the promote command. This matters on day one after cutover
// -- the operator has a working nightly steamcmd update, and adoption must not
// silently take it away without saying so.
func TestAnAdoptedInstanceIsNeverStoppedForABinaryUpdateItMayNotPerform(t *testing.T) {
	rig := newRig(t, lifecycle.JobUpdate, testInstance)
	rig.env.ManagedInstall = false
	rig.updates.build = Diff{Changed: true, From: "18452901", To: "18461233"}
	rig.updates.content = Diff{Changed: false}

	jr, err := Execute(context.Background(), rig.env)
	if err != nil {
		t.Fatalf("%v\n%s", err, jr.Report())
	}
	if jr.State != StateSkipped {
		t.Errorf("state %q, want skipped", jr.State)
	}
	if v, _ := jr.Get("update_available"); v != true {
		t.Errorf("summary.update_available = %v, want true", v)
	}
	if stops, _ := rig.super.counts(); stops != 0 {
		t.Errorf("%d stop call(s) for a binary update gvm is not allowed to perform", stops)
	}
	if rig.inj.count("update.app_update") != 0 {
		t.Error("an adopted instance ran app_update")
	}
	for _, want := range []string{"18452901 -> 18461233", "managed_install: false",
		"gvm instance promote pz-main"} {
		if !strings.Contains(jr.SkipReason, want) {
			t.Errorf("the reason does not contain %q:\n%s", want, jr.SkipReason)
		}
	}
}

// TestAnAdoptedInstanceStillDoesContentWork is the other half of that
// paragraph: the nightly still runs phases 1, 2, 8, 9 and 10, because PZ
// downloads its own workshop items on boot and needs no steamcmd for them.
func TestAnAdoptedInstanceStillDoesContentWork(t *testing.T) {
	rig := newRig(t, lifecycle.JobUpdate, testInstance)
	rig.env.ManagedInstall = false
	rig.updates.build = Diff{Changed: true, From: "1", To: "2"}
	rig.updates.content = Diff{Changed: true, Items: []string{"2169435993"}}
	rig.updates.res = Resolution{Changed: false}

	jr, err := Execute(context.Background(), rig.env)
	if err != nil {
		t.Fatalf("%v\n%s", err, jr.Report())
	}
	if jr.State == StateSkipped {
		t.Fatalf("a content change on an adopted instance was skipped:\n%s", jr.Report())
	}
	if rig.inj.count("update.settle") != 1 {
		t.Error("the content settle did not run")
	}
	if rig.inj.count("update.resolve") != 1 {
		t.Error("the resolution did not run")
	}
	if rig.inj.count("update.app_update") != 0 {
		t.Error("an adopted instance ran app_update")
	}
	if stops, _ := rig.super.counts(); stops != 0 {
		t.Errorf("%d stop call(s): an adopted server does its own workshop downloads, so "+
			"stopping it would be a stop gvm chose and nobody asked for", stops)
	}
	if v, _ := jr.Get("update_available"); v != true {
		t.Error("the available binary update was not reported")
	}
}

// TestDecide is the gate, exhaustively, because every branch of it either does
// or does not stop a live server.
func TestDecide(t *testing.T) {
	no := Diff{}
	build := Diff{Changed: true, From: "1", To: "2"}
	content := Diff{Changed: true, Items: []string{"x"}}

	tests := []struct {
		name    string
		build   Diff
		content Diff
		managed bool
		want    Decision
	}{
		{
			name: "most nights", build: no, content: no, managed: true,
			want: Decision{Skip: true},
		},
		{
			name: "most nights, adopted", build: no, content: no, managed: false,
			want: Decision{Skip: true},
		},
		{
			name: "a game patch", build: build, content: no, managed: true,
			want: Decision{Stop: true, ApplyBuild: true},
		},
		{
			name: "a game patch on an adopted instance", build: build, content: no, managed: false,
			want: Decision{Skip: true, UpdateAvailable: true},
		},
		{
			name: "a mod update", build: no, content: content, managed: true,
			want: Decision{Stop: true, ApplyContent: true},
		},
		{
			name: "a mod update on an adopted instance", build: no, content: content, managed: false,
			want: Decision{ApplyContent: true},
		},
		{
			name: "both", build: build, content: content, managed: true,
			want: Decision{Stop: true, ApplyBuild: true, ApplyContent: true},
		},
		{
			name: "both, adopted", build: build, content: content, managed: false,
			want: Decision{ApplyContent: true, UpdateAvailable: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide("pz-main", tc.build, tc.content, tc.managed)
			if got.Skip != tc.want.Skip || got.Stop != tc.want.Stop ||
				got.ApplyBuild != tc.want.ApplyBuild || got.ApplyContent != tc.want.ApplyContent ||
				got.UpdateAvailable != tc.want.UpdateAvailable {
				t.Errorf("Decide = %+v\nwant     %+v", got, tc.want)
			}
			if got.Reason == "" {
				t.Error("no reason; this is the sentence the operator reads")
			}
			if got.ApplyBuild && !tc.managed {
				t.Error("an adopted instance was authorized to app_update")
			}
			if got.Skip && got.Stop {
				t.Error("a skipped update that stops the server is not a skip")
			}
		})
	}
}

// TestPruneOnlyRunsAfterAVerifiedCreate is section 13.2's invariant: "a
// failing backup must never erode history".
func TestPruneOnlyRunsAfterAVerifiedCreate(t *testing.T) {
	// The snapshot fails: nothing is pruned.
	rig := newRig(t, lifecycle.JobBackup, testInstance)
	rig.inj.failIndex = indexIn(baselineOps(t, lifecycle.JobBackup), "backup.snapshot")
	jr, err := Execute(context.Background(), rig.env)
	if err == nil {
		t.Fatal("a failed snapshot reported success")
	}
	if rig.inj.count("backup.prune") != 0 {
		t.Errorf("retention ran after a failed snapshot:\n%s", jr.Report())
	}
	if jr.State != StateFailed {
		t.Errorf("state %q, want failed: a backup's policy is `fail` because a gap in history "+
			"has to be loud", jr.State)
	}

	// The verification fails: still nothing is pruned.
	rig2 := newRig(t, lifecycle.JobBackup, testInstance)
	rig2.inj.failIndex = indexIn(baselineOps(t, lifecycle.JobBackup), "backup.verify")
	jr2, err := Execute(context.Background(), rig2.env)
	if err == nil {
		t.Fatal("a failed verification reported success")
	}
	if rig2.inj.count("backup.prune") != 0 {
		t.Errorf("retention ran after a failed verification:\n%s", jr2.Report())
	}

	// And an engine that reports an archive verified without stamping it is
	// caught, because Ref.Verified() is what authorizes both a prune and a
	// destroy.
	rig3 := newRig(t, lifecycle.JobBackup, testInstance)
	rig3.backups.skipVerifyStamp = true
	jr3, err := Execute(context.Background(), rig3.env)
	if err == nil {
		t.Fatal("an unstamped Ref was accepted as verified")
	}
	if rig3.inj.count("backup.prune") != 0 {
		t.Errorf("retention ran on an unverified archive:\n%s", jr3.Report())
	}
	if !strings.Contains(err.Error(), "VerifiedAt") {
		t.Errorf("err = %v; it has to name what was missing", err)
	}
}

// TestTheFreezeIsThawedOnEveryPath is D-31's fast half.
//
// The out-of-process lease is what survives SIGKILL; this is what runs on an
// error return, a panic and a cancelled context. A freeze that is not thawed
// leaves a RUNNING server that has silently stopped persisting, which section
// 13.2 calls the worst failure in the system -- so the thaw is the freeze
// step's own Undo rather than a line at the end of the happy path.
func TestTheFreezeIsThawedOnEveryPath(t *testing.T) {
	ops := baselineOps(t, lifecycle.JobBackup)
	for _, after := range []string{"backup.snapshot", "backup.verify"} {
		idx := indexIn(ops, after)
		if idx < 0 {
			t.Fatalf("no %s step", after)
		}
		t.Run("error-at-"+after, func(t *testing.T) {
			rig := newRig(t, lifecycle.JobBackup, testInstance)
			rig.inj.failIndex = idx
			jr, _ := Execute(context.Background(), rig.env)
			if rig.inj.count("backup.thaw") != 1 {
				t.Errorf("the freeze was not thawed:\n%s", jr.Report())
			}
		})
		t.Run("panic-at-"+after, func(t *testing.T) {
			rig := newRig(t, lifecycle.JobBackup, testInstance)
			rig.inj.panicIndex = idx
			jr, _ := Execute(context.Background(), rig.env)
			if rig.inj.count("backup.thaw") != 1 {
				t.Errorf("a panic skipped the thaw:\n%s", jr.Report())
			}
		})
		t.Run("cancel-at-"+after, func(t *testing.T) {
			rig := newRig(t, lifecycle.JobBackup, testInstance)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rig.inj.onCall = func(n int, _ string) {
				if n == idx {
					cancel()
				}
			}
			jr, _ := Execute(ctx, rig.env)
			if rig.inj.count("backup.thaw") != 1 {
				t.Errorf("a cancelled job skipped the thaw:\n%s", jr.Report())
			}
		})
	}
}

// TestAFailedThawDegradesTheRunAndIsRecordedAgainstItsStep: a cleanup that did
// not clean up is a separate, and usually worse, problem than the step's own
// failure, so it gets its own field.
func TestAFailedThawDegradesTheRunAndIsRecordedAgainstItsStep(t *testing.T) {
	rig := newRig(t, lifecycle.JobBackup, testInstance)
	rig.inj.failIndex = indexIn(baselineOps(t, lifecycle.JobBackup), "backup.thaw")

	jr, _ := Execute(context.Background(), rig.env)
	var found bool
	for _, s := range jr.Steps {
		if s.Name == "backup.freeze" {
			found = s.UndoErr != ""
			if s.Err != "" {
				t.Errorf("the freeze itself is recorded as failed: %q", s.Err)
			}
		}
	}
	if !found {
		t.Errorf("the thaw's failure was not recorded against backup.freeze:\n%s", jr.Report())
	}
	if !jr.Degraded {
		t.Error("a failed thaw did not degrade the run")
	}
}

// TestNonFatalStepsAreUpdateShsWarnLines.
//
// `"$SCRIPT_DIR/backup.sh" || warn "Backup failed - continuing anyway."` and
// `... || warn "Steam update failed or timed out - starting on the existing
// binaries."` -- both are fields on a step rather than a habit a future editor
// has to remember.
func TestNonFatalStepsAreUpdateShsWarnLines(t *testing.T) {
	ops := baselineOps(t, lifecycle.JobUpdate)
	for _, step := range []string{"update.backup", "update.app_update", "update.post_verify",
		"update.settle", "update.resolve"} {
		idx := indexIn(ops, step)
		if idx < 0 {
			t.Fatalf("no %s step in %v", step, ops)
		}
		t.Run(step, func(t *testing.T) {
			rig := newRig(t, lifecycle.JobUpdate, testInstance)
			rig.inj.failIndex = idx
			jr, _ := Execute(context.Background(), rig.env)
			if !rig.super.isActive() {
				t.Fatalf("a %s failure left the server stopped:\n%s", step, jr.Report())
			}
			if jr.State != StateDegraded {
				t.Errorf("state %q, want degraded: start_anyway means the failure is reported "+
					"and the server is up:\n%s", jr.State, jr.Report())
			}
			if len(jr.Warnings) == 0 {
				t.Error("the failure was not recorded as a warning")
			}
			// The job carried on: later phases still ran.
			if rig.inj.count("update.resolve")+rig.inj.count("update.settle") == 0 &&
				step != "update.resolve" && step != "update.settle" {
				t.Errorf("the job stopped at %s instead of warning and continuing", step)
			}
		})
	}
}

// TestAFailedPostVerifySetsNeedsRepairAndStartsAnyway is D-32's prescribed
// response, item by item. It never auto-remediates with `validate`, because
// validate overwrites files the operator customised.
func TestAFailedPostVerifySetsNeedsRepairAndStartsAnyway(t *testing.T) {
	rig := newRig(t, lifecycle.JobUpdate, testInstance)
	rig.inj.failIndex = indexIn(baselineOps(t, lifecycle.JobUpdate), "update.post_verify")

	jr, _ := Execute(context.Background(), rig.env)
	if v, _ := jr.Get("needs_repair"); v != true {
		t.Errorf("summary.needs_repair = %v, want true:\n%s", v, jr.Report())
	}
	if !rig.super.isActive() {
		t.Error("the server was not started anyway")
	}
	notes := strings.Join(jr.Notes, " | ")
	if !strings.Contains(notes, "--repair") {
		t.Errorf("the notes do not name the repair command:\n%s", notes)
	}
	if !strings.Contains(notes, "overwrite customised files") {
		t.Errorf("the notes do not warn what --repair costs:\n%s", notes)
	}
}

// TestThePostVerificationRunsEvenWhenTheUpdateFailed is the subtle half of
// D-32: steamcmd self-updates, re-execs, and can exit 0 after printing
// "Error!". A post-verification that only ran when the exit code looked good
// would skip exactly the runs it was written for.
func TestThePostVerificationRunsEvenWhenTheUpdateFailed(t *testing.T) {
	rig := newRig(t, lifecycle.JobUpdate, testInstance)
	rig.inj.failIndex = indexIn(baselineOps(t, lifecycle.JobUpdate), "update.app_update")

	jr, _ := Execute(context.Background(), rig.env)
	if rig.inj.count("update.post_verify") != 1 {
		t.Errorf("the mandatory post-verification was skipped because the update failed:\n%s",
			jr.Report())
	}
}

// TestTheSecondCycleOnlyRunsWhenTheListsChanged is update.sh's own comment:
// "The server read the ini at boot, so a changed list needs one more cycle."
func TestTheSecondCycleOnlyRunsWhenTheListsChanged(t *testing.T) {
	for _, changed := range []bool{true, false} {
		rig := newRig(t, lifecycle.JobUpdate, testInstance)
		rig.updates.res = Resolution{Changed: changed, Before: []string{"A"}, After: []string{"A"}}
		jr, err := Execute(context.Background(), rig.env)
		if err != nil {
			t.Fatalf("changed=%v: %v\n%s", changed, err, jr.Report())
		}
		stops, _ := rig.super.counts()
		want := 1
		if changed {
			want = 2
		}
		if stops != want {
			t.Errorf("changed=%v: %d stop call(s), want %d:\n%s", changed, stops, want,
				jr.Report())
		}
		if got := rig.inj.count("update.activate"); changed != (got == 1) {
			t.Errorf("changed=%v: activate ran %d time(s)", changed, got)
		}
	}
}

// TestContentFailsClosedOnAnUnaccountedParse is rebuild.sh lines 77-80
// generalised to every resolver: refuse to rewrite the list off a bad parse,
// and do it before anything is stopped.
func TestContentFailsClosedOnAnUnaccountedParse(t *testing.T) {
	rig := newRig(t, lifecycle.JobContentSync, testInstance)
	rig.contents.plan = ContentPlan{
		Changed: true, NeedsRestart: true, Unaccounted: []string{"2169435993", "2392709985"},
	}

	jr, err := Execute(context.Background(), rig.env)
	if err == nil {
		t.Fatal("a sync ran off a parse it could not account for")
	}
	if stops, _ := rig.super.counts(); stops != 0 {
		t.Errorf("%d stop call(s) before a refused sync", stops)
	}
	if rig.inj.count("content.materialize") != 0 {
		t.Error("the refused plan was materialized")
	}
	if !strings.Contains(err.Error(), "2169435993") {
		t.Errorf("err = %v; it has to name the items", err)
	}
	if v, _ := jr.Get("unaccounted"); v == nil {
		t.Error("the unaccounted items were not recorded")
	}
}

// TestBlockedContentCostsNoDowntime is section 13.5's CurseForge paragraph: a
// blocked mod sets AWAITING_MANUAL_FILES and exits WITHOUT stopping the
// server.
func TestBlockedContentCostsNoDowntime(t *testing.T) {
	rig := newRig(t, lifecycle.JobContentSync, testInstance)
	rig.contents.plan = ContentPlan{
		Changed: true, NeedsRestart: true, Blocked: []string{"journeymap"},
	}

	jr, err := Execute(context.Background(), rig.env)
	if err != nil {
		t.Fatalf("blocked content failed the job: %v\n%s", err, jr.Report())
	}
	if jr.State != StateSkipped {
		t.Errorf("state %q, want skipped", jr.State)
	}
	if stops, _ := rig.super.counts(); stops != 0 {
		t.Errorf("%d stop call(s) for content a human has to fetch", stops)
	}
	if !strings.Contains(strings.Join(jr.Notes, " "), "blocked-mods.json") {
		t.Errorf("the notes do not name the file to look at: %v", jr.Notes)
	}
}

// TestContentStopsOnlyToWriteConfig is section 13.3's third column for
// content_sync.
func TestContentStopsOnlyToWriteConfig(t *testing.T) {
	rig := newRig(t, lifecycle.JobContentSync, testInstance)
	rig.contents.plan = ContentPlan{Changed: true, NeedsRestart: false}

	jr, err := Execute(context.Background(), rig.env)
	if err != nil {
		t.Fatalf("%v\n%s", err, jr.Report())
	}
	if stops, starts := rig.super.counts(); stops != 0 || starts != 0 {
		t.Errorf("%d stop(s) and %d start(s) for a change that does not need the server down:\n%s",
			stops, starts, jr.Report())
	}
	if rig.inj.count("content.activate") != 1 {
		t.Error("the config was not written")
	}
}

// TestTheLeaseIsTakenAndReleased: taking it is what makes `gvm start` refuse,
// so it travels through the privileged surface and it is recorded.
func TestTheLeaseIsTakenAndReleased(t *testing.T) {
	rig := newRig(t, lifecycle.JobBackup, testInstance)
	jr, err := Execute(context.Background(), rig.env)
	if err != nil {
		t.Fatal(err)
	}
	if n := rig.host.wrote(hostops.FileLease); n != 1 {
		t.Errorf("%d lease write(s), want 1", n)
	}
	if n := rig.host.removed(hostops.FileLease); n != 1 {
		t.Errorf("%d lease remove(s), want 1", n)
	}
	if jr.State != StateOK {
		t.Errorf("state %q", jr.State)
	}
}

// TestTheLockNamesTheJobAndIsReleased, including the derived path -- which the
// fake records rather than creating, so a test never makes /run/gvm on the
// machine it runs on.
func TestTheLockNamesTheJobAndIsReleased(t *testing.T) {
	rig := newRig(t, lifecycle.JobBackup, testInstance)
	if _, err := Execute(context.Background(), rig.env); err != nil {
		t.Fatal(err)
	}
	if len(rig.locker.paths) != 1 {
		t.Fatalf("%d lock(s) taken", len(rig.locker.paths))
	}
	// The job engine's lock, under LocksDir -- NOT /run/gvm/<n>/job.lock,
	// which the runner holds for the whole life of the unit. See
	// TestTheJobLockIsNotTheRunnersWorldLock.
	if got, want := rig.locker.paths[0], "/var/lib/gvm/locks/job_pz-main.lock"; got != want {
		t.Errorf("locked %q, want %q", got, want)
	}
	h := rig.locker.holders[0]
	if h.PID != 4242 || h.Kind != "backup" || h.Unit == "" || h.StartedAt.IsZero() {
		t.Errorf("holder = %+v; all four fields exist so doctor can say who is blocking", h)
	}
	if rig.locker.released != 1 {
		t.Errorf("%d release(s)", rig.locker.released)
	}
}

// TestAContendedLockDoesNotProduceAStartedRow.
//
// A job that could not take the lock has not started, and a `started` audit
// row whose terminal row says "somebody else was running" is a row that makes
// history harder to read rather than easier.
func TestAContendedLockDoesNotProduceAStartedRow(t *testing.T) {
	rig := newRig(t, lifecycle.JobBackup, testInstance)
	rig.locker.err = &Busy{Path: "/var/lib/gvm/locks/job_pz-main.lock", Known: true,
		Holder: lifecycle.Holder{PID: 48812, Kind: "update"}}

	jr, err := Execute(context.Background(), rig.env)
	if err == nil {
		t.Fatal("two jobs ran against one instance")
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("err = %v, want ErrLocked", err)
	}
	if jr.State != StateFailed {
		t.Errorf("state %q", jr.State)
	}
	if rig.sink.Len() != 0 {
		t.Errorf("%d audit row(s) for a job that never started", rig.sink.Len())
	}
	if n := rig.host.wrote(hostops.FileLease); n != 0 {
		t.Errorf("a job that did not run took %d lease(s)", n)
	}
}

// TestEveryRunIsAudited, with section 14.8's allowlist for job.run: kind,
// unit, run_as, left_stopped, and kind required on the success row.
func TestEveryRunIsAudited(t *testing.T) {
	rig := newRig(t, lifecycle.JobBackup, testInstance)
	if _, err := Execute(context.Background(), rig.env); err != nil {
		t.Fatal(err)
	}
	recs := rig.sink.Records()
	if len(recs) != 2 {
		t.Fatalf("%d audit row(s), want a started and a terminal one", len(recs))
	}
	var started, done *audit.Record
	for i := range recs {
		switch recs[i].Outcome {
		case audit.Started:
			started = &recs[i]
		default:
			done = &recs[i]
		}
	}
	if started == nil || done == nil {
		t.Fatalf("outcomes = %v, %v", recs[0].Outcome, recs[1].Outcome)
	}
	if done.Outcome != audit.OK {
		t.Errorf("terminal outcome = %q", done.Outcome)
	}
	if done.Action != audit.JobRun {
		t.Errorf("action = %q", done.Action)
	}
	if done.Params["kind"] != "backup" {
		t.Errorf("params.kind = %v", done.Params["kind"])
	}
	if done.Params["run_as"] != "root" {
		t.Errorf("params.run_as = %v; D-16 records this so a future regression is visible in "+
			"history", done.Params["run_as"])
	}
	if done.Params["left_stopped"] != false {
		t.Errorf("params.left_stopped = %v", done.Params["left_stopped"])
	}
	if err := done.Validate(); err != nil {
		t.Errorf("the terminal row is invalid: %v", err)
	}
}

// TestACancelledJobIsStillAudited: the terminal row is the evidence that the
// guard ran, so it is written on a context of its own.
func TestACancelledJobIsStillAudited(t *testing.T) {
	rig := newRig(t, lifecycle.JobRestart, testInstance)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	jr, _ := Execute(ctx, rig.env)
	if rig.sink.Len() < 2 {
		t.Errorf("%d audit row(s) for a cancelled job:\n%s", rig.sink.Len(), jr.Report())
	}
	if n := rig.host.removed(hostops.FileLease); n != 1 {
		t.Errorf("a cancelled job released %d lease(s); the release runs on a context of its "+
			"own for exactly this reason", n)
	}
}

// TestTheVerdictTable is the priority order, and the first row is the one that
// matters: a server left stopped is `failed` whatever the kind's policy says.
// oneStep stands for "a phase ran", which is what separates a degraded run
// from one that never started.
var oneStep = []StepResult{{Name: "x", Err: "x", Fatal: true}}

func TestTheVerdictTable(t *testing.T) {
	tests := []struct {
		name   string
		policy Policy
		run    Run
		want   State
	}{
		{"left stopped beats a warn policy", PolicyWarn,
			Run{LeftStopped: true, Guard: Restoration{Armed: true}}, StateFailed},
		{"left stopped beats start_anyway", PolicyStartAnyway,
			Run{LeftStopped: true, Guard: Restoration{Armed: true}}, StateFailed},
		{"a panic is failed", PolicyWarn, Run{Panic: "boom"}, StateFailed},
		{"a job that never ran a phase is failed whatever the policy", PolicyWarn,
			Run{Fatal: errors.New("no engine")}, StateFailed},
		{"fail policy fails", PolicyFail,
			Run{Fatal: errors.New("x"), Steps: oneStep,
				Guard: Restoration{Armed: true, ActiveAfter: true}},
			StateFailed},
		{"warn policy degrades", PolicyWarn,
			Run{Fatal: errors.New("x"), Steps: oneStep}, StateDegraded},
		{"start_anyway degrades while serving", PolicyStartAnyway,
			Run{Fatal: errors.New("x"), Steps: oneStep,
				Guard: Restoration{Armed: true, ActiveAfter: true}},
			StateDegraded},
		{"start_anyway fails when not serving", PolicyStartAnyway,
			Run{Fatal: errors.New("x"), Steps: oneStep, Guard: Restoration{Armed: true}},
			StateFailed},
		{"cancelled survives", PolicyFail, Run{State: StateCancelled}, StateCancelled},
		{"skipped survives", PolicyFail, Run{State: StateSkipped}, StateSkipped},
		{"a warning degrades", PolicyFail, Run{Warnings: []string{"x"}}, StateDegraded},
		{"nothing wrong is ok", PolicyFail, Run{}, StateOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{Kind: lifecycle.JobBackup, Policy: tc.policy}
			got := r.Verdict(&Env{}, &tc.run)
			if got != tc.want {
				t.Errorf("Verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestASpecOnFailureOverridesTheKindsDefault, which is what
// `on_failure: fail` in an instance's jobs block is for.
//
// It changes how the failure is REPORTED and never whether the server comes
// back: the same failure is `failed` under `fail` and `degraded` under
// `start_anyway`, and under both the guard has already put the instance back
// before the verdict is computed.
func TestASpecOnFailureOverridesTheKindsDefault(t *testing.T) {
	// A FATAL step, because that is where the policy applies. A non-fatal
	// step is update.sh's `|| warn` and is degraded under every policy -- a
	// failed off-box push must not report a verified local backup as failed.
	idx := indexIn(baselineOps(t, lifecycle.JobUpdate), "update.preflight")

	for policy, want := range map[Policy]State{
		PolicyFail:        StateFailed,
		PolicyStartAnyway: StateDegraded,
	} {
		rig := newRig(t, lifecycle.JobUpdate, testInstance)
		rig.env.Policy = policy
		rig.inj.failIndex = idx

		jr, _ := Execute(context.Background(), rig.env)
		if !rig.super.isActive() {
			t.Fatalf("%s left the server stopped", policy)
		}
		if jr.State != want {
			t.Errorf("%s: state %q, want %q:\n%s", policy, jr.State, want, jr.Report())
		}
	}
}

// TestALeftStoppedRunIsAnErrorEvenWithNoFailedStep.
//
// The reachable shape: the entry observation was inconclusive, so the guard
// treated the instance as running (which is the safe direction), and its start
// did not work. No step failed. Without LeftStopped in Run.Err, `gvm job run`
// would exit 0 and the timer would report success for a stopped world.
func TestALeftStoppedRunIsAnErrorEvenWithNoFailedStep(t *testing.T) {
	rig := newRig(t, lifecycle.JobBackup, testInstance)
	rig.super.active = false
	rig.super.activeErr = errors.New("dbus: connection refused")
	rig.super.failEveryStart = true

	jr, err := Execute(context.Background(), rig.env)
	if !jr.LeftStopped {
		t.Fatalf("expected left_stopped:\n%s", jr.Report())
	}
	for _, s := range jr.Steps {
		if s.Failed() {
			t.Fatalf("a step failed, so this is not the case under test: %s", s.Name)
		}
	}
	if err == nil {
		t.Error("Execute returned no error for a run that left the server stopped; the CLI " +
			"would exit 0 and the timer would report success")
	}
	if jr.State != StateFailed {
		t.Errorf("state %q, want failed", jr.State)
	}
	// And the audit row says so.
	var terminal *audit.Record
	for _, rec := range rig.sink.Records() {
		if rec.Outcome.Terminal() {
			r := rec
			terminal = &r
		}
	}
	if terminal == nil {
		t.Fatal("no terminal audit row")
	}
	if terminal.Outcome != audit.Failed {
		t.Errorf("audit outcome = %q, want error", terminal.Outcome)
	}
	if terminal.Params["left_stopped"] != true {
		t.Errorf("audit params.left_stopped = %v", terminal.Params["left_stopped"])
	}
}

// TestAnInstanceJobRefusesWithoutAPrivilegedSurface: no lease means `gvm start`
// would not refuse to race the job, and no breadcrumb means the guard timer
// has nothing to act on. Both are silent absences.
func TestAnInstanceJobRefusesWithoutAPrivilegedSurface(t *testing.T) {
	rig := newRig(t, lifecycle.JobBackup, testInstance)
	rig.env.Host = nil
	_, err := Execute(context.Background(), rig.env)
	if err == nil {
		t.Fatal("a job ran with no privileged surface")
	}
	if !errors.Is(err, ErrEnv) {
		t.Errorf("err = %v, want ErrEnv", err)
	}
	if stops, _ := rig.super.counts(); stops != 0 {
		t.Errorf("%d stop call(s)", stops)
	}
}

// TestAnEnvironmentWithoutItsWorkSeamFailsBeforeStoppingAnything.
func TestAnEnvironmentWithoutItsWorkSeamFailsBeforeStoppingAnything(t *testing.T) {
	for kind, clear := range map[lifecycle.JobKind]func(*Env){
		lifecycle.JobBackup:      func(e *Env) { e.Backups = nil },
		lifecycle.JobUpdate:      func(e *Env) { e.Updates = nil },
		lifecycle.JobContentSync: func(e *Env) { e.Contents = nil },
		lifecycle.JobPrune:       func(e *Env) { e.Archives = nil },
		lifecycle.JobVerify:      func(e *Env) { e.Archives = nil },
	} {
		rig := newRig(t, kind, testInstance)
		clear(rig.env)
		jr, err := Execute(context.Background(), rig.env)
		if err == nil {
			t.Errorf("%s ran with no work seam", kind)
			continue
		}
		if !errors.Is(err, ErrEnv) {
			t.Errorf("%s: err = %v, want ErrEnv", kind, err)
		}
		if stops, _ := rig.super.counts(); stops != 0 {
			t.Errorf("%s stopped the server %d time(s) before discovering it had no engine",
				kind, stops)
		}
		if jr == nil || jr.State != StateFailed {
			t.Errorf("%s produced no failed record", kind)
		}
	}
}

// TestAScopeRefusesTheWrongSlugShape, which is what keeps a host job from
// deriving paths from an empty instance name and an instance job from being
// handed none.
func TestAScopeRefusesTheWrongSlugShape(t *testing.T) {
	rig := newRig(t, lifecycle.JobBackup, "")
	if _, err := Execute(context.Background(), rig.env); err == nil {
		t.Error("a backup ran with no instance")
	}
	hostOnly := newRig(t, lifecycle.JobGuard, testInstance)
	if _, err := Execute(context.Background(), hostOnly.env); err == nil {
		t.Error("the guard sweep ran against one instance")
	}
}

// TestAHostLevelVerifySweepsEveryInstance, which is how the nightly
// gvm-job@_verify.timer covers a box with no database.
func TestAHostLevelVerifySweepsEveryInstance(t *testing.T) {
	rig := newRig(t, lifecycle.JobVerify, "")
	rig.archives.instances = []string{"pz-main", "vh1"}

	jr, err := Execute(context.Background(), rig.env)
	if err != nil {
		t.Fatalf("%v\n%s", err, jr.Report())
	}
	if n := rig.inj.count("verify.shallow"); n != 2 {
		t.Errorf("shallow-verified %d instance(s), want 2:\n%s", n, jr.Report())
	}
	if n := rig.inj.count("verify.deep_newest_preserve"); n != 2 {
		t.Errorf("deep-verified %d instance(s), want 2", n)
	}
	if n := rig.inj.count("archives.instances"); n != 2 {
		t.Errorf("enumerated the instances %d time(s)", n)
	}
	// Scoped to one instance, the same table does not enumerate at all.
	one := newRig(t, lifecycle.JobVerify, testInstance)
	if _, err := Execute(context.Background(), one.env); err != nil {
		t.Fatal(err)
	}
	if n := one.inj.count("archives.instances"); n != 0 {
		t.Errorf("a per-instance verify enumerated the box %d time(s)", n)
	}
}

// TestOneInstanceFailingDoesNotStopTheSweep: a corrupt archive directory for a
// decommissioned server must not prevent the live one from being verified.
func TestOneInstanceFailingDoesNotStopTheSweep(t *testing.T) {
	rig := newRig(t, lifecycle.JobVerify, "")
	rig.archives.instances = []string{"pz-main", "vh1", "mc1"}
	// Fail the first shallow verification.
	rig.inj.failIndex = 1

	jr, _ := Execute(context.Background(), rig.env)
	if n := rig.inj.count("verify.shallow"); n != 3 {
		t.Errorf("stopped after %d of 3 instances:\n%s", n, jr.Report())
	}
	if jr.State != StateDegraded {
		t.Errorf("state %q, want degraded", jr.State)
	}
}

// TestTheRunRecordIsReadable: Line and Report are what a CLI prints and what
// an operator reads at 07:00, so they have to say the important thing first.
func TestTheRunRecordIsReadable(t *testing.T) {
	rig := newRig(t, lifecycle.JobRestart, testInstance)
	rig.super.failEveryStart = true
	jr, _ := Execute(context.Background(), rig.env)

	line := jr.Line()
	if !strings.HasPrefix(line, "failed pz-main_restart") {
		t.Errorf("Line() = %q", line)
	}
	if !strings.Contains(line, "THE SERVER IS STOPPED") {
		t.Errorf("Line() = %q; the one thing that matters is not in it", line)
	}
	rep := jr.Report()
	if !strings.Contains(rep, "restart.stop") || !strings.Contains(rep, "guard ") {
		t.Errorf("Report() does not list the phases and the guard:\n%s", rep)
	}
	if jr.Took <= 0 {
		t.Error("the run has no duration")
	}
}

// TestStepNamesDoesNotRunAnything, so `gvm update <n> --diff` can print the
// phases it would execute without executing one.
func TestStepNamesDoesNotRunAnything(t *testing.T) {
	rig := newRig(t, lifecycle.JobUpdate, testInstance)
	names, err := StepNames(rig.env)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 10 {
		t.Errorf("%d phases, want the whole of section 13.5's pipeline: %v", len(names), names)
	}
	if len(rig.inj.ops()) != 0 {
		t.Errorf("StepNames made %v", rig.inj.ops())
	}
	if stops, starts := rig.super.counts(); stops != 0 || starts != 0 {
		t.Errorf("StepNames made %d stop(s) and %d start(s)", stops, starts)
	}
	stopping, err := StoppingSteps(rig.env)
	if err != nil {
		t.Fatal(err)
	}
	if len(stopping) != 2 {
		t.Errorf("StoppingSteps = %v; an update stops once for the binaries and once for the "+
			"second cycle", stopping)
	}
}

// TestTheVocabularyMatchesTheSchema, so a state or a trigger that is not in
// job_state_t / job_trigger_t cannot be written.
func TestTheVocabularyMatchesTheSchema(t *testing.T) {
	for _, s := range States() {
		if !s.Valid() {
			t.Errorf("States() lists %q and Valid() refuses it", s)
		}
	}
	if len(States()) != 7 {
		t.Errorf("%d states; job_state_t has 7", len(States()))
	}
	if StateSkipped.Bad() {
		t.Error("skipped is a first-class success (D-43) and must not page anyone")
	}
	if !StateDegraded.Bad() {
		t.Error("degraded is the run most worth reading")
	}
	if StateRunning.Terminal() {
		t.Error("running is terminal")
	}
	if len(Triggers()) != 6 {
		t.Errorf("%d triggers; job_trigger_t has 6", len(Triggers()))
	}
	for _, p := range Policies() {
		if !p.Valid() {
			t.Errorf("Policies() lists %q and Valid() refuses it", p)
		}
	}
	if ScopeInstance.Accepts("") || !ScopeHost.Accepts("") || !ScopeEither.Accepts("") {
		t.Error("Scope.Accepts is wrong about the empty instance name")
	}
}

// TestAStalledContentSettleDegradesTheRunAndNamesTheIds.
//
// The second half of the settle DoD item, and the half nothing asserted.
//
// internal/update's Settler returns a stall as a RESULT and not as an error --
// its own comment says so, and settle_test.go pins that side and then says
// "the job layer turns this into degraded", which was an unasserted handoff.
// So the nil-error -> error conversion in runner_update.go is the ONLY thing
// standing between a Steam stall and a green nightly: drop the
// `return fmt.Errorf(...)` and the step succeeds, Verdict falls through to
// StateOK, and the one remaining trace is a summary field nobody reads.
//
// The nearest existing coverage does not reach it: the non-fatal enumeration
// drives `update.settle` through the INJECTOR, so the fake returns
// (Settlement{}, err) -- which is the budget/cancel path, not the stall.
//
// Asserted on the warning and the state rather than only on jr.Degraded,
// because `jr.Degraded = true` is redundant with the jr.Warn the returned
// error produces -- a test that checked only the flag would still pass after
// the exact refactor at issue.
func TestAStalledContentSettleDegradesTheRunAndNamesTheIds(t *testing.T) {
	t.Run("items that never arrived", func(t *testing.T) {
		rig := newRig(t, lifecycle.JobUpdate, testInstance)
		rig.updates.settle = Settlement{
			Missing: []string{"2460154811"}, Waited: 45 * time.Second,
		}
		jr, _ := Execute(context.Background(), rig.env)
		if jr.State != StateDegraded {
			t.Errorf("state = %q, want %q: a mod that never downloaded is not a clean run",
				jr.State, StateDegraded)
		}
		missing, ok := jr.Get("content_missing")
		if !ok || !strings.Contains(fmt.Sprint(missing), "2460154811") {
			t.Errorf("summary content_missing = %v (present=%v); the ids are what an operator "+
				"acts on", missing, ok)
		}
		warn := strings.Join(jr.Warnings, " ")
		if !strings.Contains(warn, "2460154811") {
			t.Errorf("warnings = %q; the id is not named where the operator reads", jr.Warnings)
		}
		if !strings.Contains(warn, "doctor") {
			t.Errorf("warnings = %q; nothing says what to do about it", jr.Warnings)
		}
		// Non-fatal, so the rest of the table ran and the world is up. A
		// stalled mod must not leave the server down.
		if rig.inj.count("update.resolve") != 1 {
			t.Errorf("update.resolve ran %d time(s); the settle is non-fatal",
				rig.inj.count("update.resolve"))
		}
		if !rig.super.isActive() {
			t.Error("a stalled download left the world stopped")
		}
	})

	t.Run("a declared root that matched nothing", func(t *testing.T) {
		rig := newRig(t, lifecycle.JobUpdate, testInstance)
		// Not reachable from today's Settler -- every non-positive return it
		// makes carries Missing or an error -- so this pins the runner's
		// FAIL-CLOSED response to any future Settle implementation rather
		// than a path that exists. Said here so a later reader does not
		// delete it as dead.
		rig.updates.settle = Settlement{Waited: 45 * time.Second}
		jr, _ := Execute(context.Background(), rig.env)
		if jr.State != StateDegraded {
			t.Errorf("state = %q, want %q", jr.State, StateDegraded)
		}
		// And the two cases stay distinguishable in the row.
		if v, ok := jr.Get("content_missing"); ok {
			t.Errorf("content_missing = %v for a stall with nothing missing by name", v)
		}
		if !strings.Contains(strings.Join(jr.Warnings, " "), "matched nothing") {
			t.Errorf("warnings = %q; it does not say what went quiet", jr.Warnings)
		}
	})
}
