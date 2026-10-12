package rebuild

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/update"
)

// TestACleanRebuildRunsTenStepsInOrder.
//
// The baseline, and it is also the fixture the enumeration below reads: the
// order recorded here IS section 13.6's order, so a step moved above the
// archive or below the destroy changes this list and fails this test by name.
func TestACleanRebuildRunsTenStepsInOrder(t *testing.T) {
	r := newRig()
	out, err := Run(context.Background(), r.cfg, Options{Yes: true})
	if err != nil {
		t.Fatalf("a clean rebuild failed: %v\nsteps: %v", err, out.Steps)
	}
	want := []string{
		"content.plan",       // 1 PLAN
		"timers.mask",        // 3 QUIESCE: mask BEFORE the stop
		"server.stop",        //   then stop
		"content.write",      // 4 MATERIALIZE
		"server.start",       //   boot #1
		"server.waitready",   //   wait for the readiness marker
		"server.stop",        //   then CLEAN-STOP: nothing below reads a live world
		"content.settlement", // 5 VERIFY CONTENT
		"content.resolve",    // 6 RESOLVE
		"archives.preflight", // 7 ARCHIVE: disk first
		"archives.snapshot",  //   write
		"archives.verify",    //   verify -- the last thing before the point
		"destroyer.destroy",  // 8 DESTROY
		"hooks.apply",        // 9 APPLY HOOKS
		"server.start",       // 10 START
		"timers.unmask",      //   and the deferred unmask
	}
	if got := r.inj.recorded(); !slices.Equal(got, want) {
		t.Fatalf("order =\n  %v\nwant\n  %v", got, want)
	}
	if !out.WorldTouched {
		t.Error("a completed rebuild reports the world untouched")
	}
	if out.Untouched != "" {
		t.Errorf("a completed rebuild carries an `untouched` sentence: %q", out.Untouched)
	}
	if out.Archive == nil || !out.Archive.Verified() {
		t.Error("no verified archive in the outcome")
	}
	if out.Archive.Class != archive.ClassPreserve {
		t.Errorf("archive class = %q, want %q; a pre-rebuild archive is never pruned",
			out.Archive.Class, archive.ClassPreserve)
	}
	if out.Archive.Reason != "pre-rebuild" {
		t.Errorf("archive reason = %q, want pre-rebuild", out.Archive.Reason)
	}
	if !out.Running {
		t.Error("the server is not running at exit")
	}
	if out.Restore == "" || !strings.Contains(out.Restore, "gvm restore") {
		t.Errorf("restore = %q; rebuild.sh printed a three-line tar recipe and this is meant "+
			"to be one command", out.Restore)
	}
	if r.tim.unmasked != 1 {
		t.Errorf("timers unmasked %d time(s), want 1; a box left with its nightlies masked "+
			"has no backups and nobody notices for a week", r.tim.unmasked)
	}
}

// TestEveryEarlyExitLeavesTheWorldUntouchedAndSaysSo is this package's reason
// for existing.
//
// rebuild.sh's comments state the invariant in the operator's own words --
// "every destructive step happens only after the downloads are verified; any
// earlier failure leaves the old world untouched" -- and it put the sentence
// "the world was NOT touched" in four separate `die` messages because that is
// the fact the operator needs first.
//
// So: take the clean run's call list, and for every call that happens BEFORE
// the destroy, re-run the rebuild once with an error there, once with a panic
// and once with a cancellation. Every one of those 33 cases must satisfy all
// three of:
//
//   - the destroyer recorded ZERO calls;
//   - Outcome.WorldTouched is false;
//   - Outcome.Untouched is a non-empty sentence (except for a panic, where
//     there is no Outcome to carry one -- internal/jobs' engine is what
//     records a panic, and the only claim this test makes about that case is
//     that nothing was deleted).
//
// Because the enumeration comes from the call list and not from a hand-written
// table, a step added to rebuild.go is covered the day it is added.
func TestEveryEarlyExitLeavesTheWorldUntouchedAndSaysSo(t *testing.T) {
	clean := newRig()
	if _, err := Run(context.Background(), clean.cfg, Options{Yes: true}); err != nil {
		t.Fatalf("the baseline rebuild failed, so there is nothing to enumerate: %v", err)
	}
	calls := clean.inj.recorded()
	destroyAt := slices.Index(calls, "destroyer.destroy")
	if destroyAt < 0 {
		t.Fatal("the baseline run never reached the destroy")
	}
	before := calls[:destroyAt]
	if len(before) < 10 {
		t.Fatalf("only %d call(s) happen before the destroy; section 13.6 has six steps' worth",
			len(before))
	}

	for i, name := range before {
		for _, mode := range []string{"error", "panic", "cancel"} {
			t.Run(fmt.Sprintf("%02d-%s-%s", i, name, mode), func(t *testing.T) {
				r := newRig()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				r.inj.at, r.inj.mode, r.inj.cancel = name, mode, cancel

				var out *Outcome
				var err error
				func() {
					defer func() {
						if p := recover(); p != nil {
							if mode != "panic" {
								t.Fatalf("unexpected panic: %v", p)
							}
						}
					}()
					out, err = Run(ctx, r.cfg, Options{Yes: true})
				}()

				// 1. Nothing was deleted. This is the assertion that holds in
				//    all three modes and it is the one the world depends on.
				if n := r.des.count(); n != 0 {
					t.Fatalf("the destroyer was called %d time(s) after a %s at %s", n, mode, name)
				}
				if mode == "panic" {
					return
				}
				if err == nil {
					t.Fatalf("a %s at %s did not fail the rebuild", mode, name)
				}
				if out == nil {
					t.Fatal("no outcome, so there is nothing for the job summary to record")
				}
				// 2. The flag.
				if out.WorldTouched {
					t.Error("the outcome says the world was touched")
				}
				// 3. The sentence, in the outcome AND in the error -- they
				//    are read by different things.
				if out.Untouched == "" {
					t.Error("the outcome carries no `untouched` sentence")
				} else if !strings.Contains(err.Error(), out.Untouched) {
					t.Errorf("the error does not carry the sentence:\n err = %v\n sentence = %q",
						err, out.Untouched)
				}
				if out.Archive != nil {
					t.Errorf("an early exit reports an archive: %s", out.Archive.Path)
				}
				// 4. And the nightlies are back. A rebuild that masked this
				//    instance's job timers and exited without unmasking them
				//    is a box with no backups, which nobody notices for a
				//    week -- and nothing else in gvm looks at
				//    gvm-job@<n>_*.timer: not the running guard (which has no
				//    Mask by design), not the sweep, not lifecycle's derive,
				//    which reads the instance's own unit. Comparing the two
				//    counters rather than asserting 1 keeps the cases that
				//    fail before step 3 correct at 0/0.
				if r.tim.masked > 0 && r.tim.unmasked != r.tim.masked {
					t.Errorf("a %s at %s left the job timers masked (masked=%d unmasked=%d)",
						mode, name, r.tim.masked, r.tim.unmasked)
				}
			})
		}
	}
}

// TestAFailedArchiveVerificationAbortsBeforeAnyDelete, called out separately
// from the enumeration because it is the specific failure the whole ordering
// exists for.
//
// A truncated tarball and a good one are indistinguishable without reading it
// back. rebuild.sh could not tell them apart at all; here the verification is
// the last thing that happens before the point of no return, and all three ways
// it can come back wrong stop the rebuild.
func TestAFailedArchiveVerificationAbortsBeforeAnyDelete(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*rig)
		want  error
	}{
		{"the write failed", func(r *rig) {
			r.inj.at, r.inj.mode = "archives.snapshot", "error"
		}, ErrArchive},
		{"the engine returned no ref", func(r *rig) { r.arch.noRef = true }, ErrArchive},
		{"verification failed", func(r *rig) {
			r.inj.at, r.inj.mode = "archives.verify", "error"
		}, ErrArchive},
		{"verified and VerifiedAt is zero", func(r *rig) {
			r.arch.skipVerifyStamp = true
		}, ErrArchive},
		// The failure that cannot be seen from an error, and the one the
		// printed sentence used to hide. archive.VerifyRef stamps VerifiedAt
		// for a SHALLOW check too -- its own comment says ModeShallow
		// "legitimately compares no member CONTENTS" -- and both
		// instance.Destroy and archive.Destroy gate only on ref.Verified().
		// So a shallow receipt satisfies every downstream guard while
		// proving nothing about the bytes, and section 13.6 step 7 is
		// explicit: "Then verify --spot 24. Any failure -> DO NOT DELETE."
		// Section 13.1 adds that spot is the mode "run automatically before
		// every destructive action".
		{"a shallow check is not a receipt", func(r *rig) {
			r.arch.mode = archive.ModeShallow
		}, ErrArchive},
		{"the engine returned no report", func(r *rig) { r.arch.noReport = true }, ErrArchive},
		{"there is no room", func(r *rig) {
			r.inj.at, r.inj.mode = "archives.preflight", "error"
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig()
			tc.setup(r)
			out, err := Run(context.Background(), r.cfg, Options{Yes: true})
			if err == nil {
				t.Fatal("the rebuild proceeded without a receipt")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if r.des.count() != 0 {
				t.Fatal("the world was deleted without a verified archive")
			}
			if out.WorldTouched || out.Untouched == "" {
				t.Errorf("outcome = %+v; the world is untouched and must say so", out)
			}
			// And the server is back up: the rebuild stopped it at step 3 and
			// is responsible for it on every exit afterwards.
			if !r.srv.isActive() {
				t.Error("the server was left stopped by a refusal that changed nothing")
			}
		})
	}
}

// TestTheVerifiedLineQuotesTheVerificationThatRan.
//
// It used to quote a Config field -- a DESCRIPTION the caller passed to the
// printer, separate from the VerifyOptions the engine actually carried -- so
// the rebuild record could state a verification that never happened: "verified
// <path> with --spot 24" over a shallow check that re-hashed nothing. Section
// 13.6's own sample output quotes the counts ("class=preserve, spot-verified
// 24/24 members"), which is only possible from the report.
//
// Separating the label from the instruction is what let the label overstate.
// The protection the field's comment described -- a rebuild must not be able
// to weaken the verification its destroy depends on -- is achieved by making
// the destroy check the REPORT, which is the case above.
func TestTheVerifiedLineQuotesTheVerificationThatRan(t *testing.T) {
	r := newRig()
	r.arch.mode, r.arch.members, r.arch.checked = archive.ModeSpot, 31, 24
	out, err := Run(context.Background(), r.cfg, Options{Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(out.Steps, " ")
	if !strings.Contains(joined, "spot-verified 24/31") {
		t.Errorf("the verified line does not quote the report's own counts: %v", out.Steps)
	}
}

// TestTheDestroyIsOnlyEverReachedWithAVerifiedRefAndTheDeclaredPaths.
//
// The two arguments ARE the safety property: instance.Destroy refuses a nil or
// unverified Ref, and the jail refuses any path that is not one of the declared
// world paths. This asserts the rebuild hands it exactly those.
func TestTheDestroyIsOnlyEverReachedWithAVerifiedRefAndTheDeclaredPaths(t *testing.T) {
	r := newRig()
	if _, err := Run(context.Background(), r.cfg, Options{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if r.des.count() != 1 {
		t.Fatalf("destroys = %d, want 1", r.des.count())
	}
	ref := r.des.refs[0]
	if ref == nil || !ref.Verified() {
		t.Fatalf("the destroy was handed %v", ref)
	}
	if !slices.Equal(r.des.targets[0], r.cfg.WorldPaths) {
		t.Errorf("targets = %v, want exactly backup.world_paths %v",
			r.des.targets[0], r.cfg.WorldPaths)
	}
}

// TestTheArchiveAndTheDestroyHappenWithTheServerStopped.
//
// The ordering list above is a hand-written fixture, and a fixture can be made
// to agree with a wrong ordering. This asserts the property instead, read from
// the fake at the moment of the call.
//
// Both halves are data loss. The preserve archive is the only copy of
// everyone's characters, and a JVM still writing map chunks either trips
// archive.Create's per-file re-hash (ErrDrifted, so the rebuild can never
// complete on a populated server) or produces a tarball that verifies while
// holding the chunks, the player DB and the accounts DB captured at different
// instants. Then the destroy unlinks paths that process holds open and the
// next autosave writes some of them back. Section 13.6 step 4 ends in
// "clean-stop" and rebuild.sh's pz_boot_and_wait sends `quit` and drains the
// session for exactly this reason.
func TestTheArchiveAndTheDestroyHappenWithTheServerStopped(t *testing.T) {
	r := newRig()
	if _, err := Run(context.Background(), r.cfg, Options{Yes: true}); err != nil {
		t.Fatalf("a clean rebuild failed: %v", err)
	}
	if got := r.arch.liveAtSnapshot; len(got) != 1 || got[0] {
		t.Errorf("server live at the preserve snapshot = %v, want exactly one call with the "+
			"world stopped", got)
	}
	if got := r.des.liveAtCall; len(got) != 1 || got[0] {
		t.Errorf("server live at the destroy = %v, want exactly one call with the world "+
			"stopped", got)
	}
	// Two stops (step 3's quiesce and step 4's clean-stop) and two starts
	// (boot #1 and the new world). A missing second stop shows up here as
	// 1/2, which is what this package shipped before.
	if r.srv.stops != 2 {
		t.Errorf("stops = %d, want 2: step 3's quiesce and step 4's clean-stop", r.srv.stops)
	}
	if r.srv.starts != 2 {
		t.Errorf("starts = %d, want 2: boot #1 and the new world", r.srv.starts)
	}
}

// TestAFailedCleanStopAfterTheDownloadBootDeletesNothing.
//
// The injector fails every occurrence of a name, so the enumeration's
// `server.stop` case always trips step 3's stop and can never reach step 4's.
// This is that case: the download boot came up, and the ladder will not take
// it back down. The only safe answer is to archive nothing and delete nothing.
func TestAFailedCleanStopAfterTheDownloadBootDeletesNothing(t *testing.T) {
	r := newRig()
	stops := 0
	r.srv.stopHook = func() error {
		stops++
		if stops >= 2 {
			return errors.New("the ladder reached SIGKILL and the unit is still active")
		}
		return nil
	}
	out, err := Run(context.Background(), r.cfg, Options{Yes: true})
	if err == nil {
		t.Fatal("a server that will not stop did not refuse the rebuild")
	}
	if out.WorldTouched {
		t.Error("the outcome says the world was touched")
	}
	if out.Untouched == "" {
		t.Error("the outcome carries no `untouched` sentence")
	} else if !strings.Contains(err.Error(), out.Untouched) {
		t.Errorf("the error does not carry the sentence: err = %v, sentence = %q",
			err, out.Untouched)
	}
	if n := r.des.count(); n != 0 {
		t.Fatalf("the destroyer was called %d time(s) with the world still live", n)
	}
	if r.inj.count("archives.snapshot") != 0 {
		t.Error("the preserve archive was taken from a live world")
	}
	// And the box is not left down by the refusal.
	if !r.srv.isActive() {
		t.Error("the server was left stopped by a refusal that changed nothing")
	}
}

// TestDryStopsAtStepOneHavingWrittenNothing.
//
// Not even the acquisition list: rebuild.sh's --dry showed what would change
// and left mods.txt alone, and the one thing an operator uses it for is finding
// out whether the collection parse works at all.
func TestDryStopsAtStepOneHavingWrittenNothing(t *testing.T) {
	r := newRig()
	out, err := Run(context.Background(), r.cfg, Options{Dry: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.inj.recorded(); !slices.Equal(got, []string{"content.plan"}) {
		t.Errorf("calls = %v, want only the plan", got)
	}
	if r.cont.writes != 0 {
		t.Error("a dry run wrote the acquisition list")
	}
	if r.srv.stops != 0 || r.tim.masked != 0 || r.des.count() != 0 {
		t.Error("a dry run stopped, masked or deleted something")
	}
	if !out.Dry || out.WorldTouched || out.Untouched == "" {
		t.Errorf("outcome = %+v", out)
	}
	// It still reports the diff, which is the whole point.
	if len(out.Steps) == 0 || !strings.Contains(strings.Join(out.Steps, " "), "2169435993") {
		t.Errorf("a dry run did not report what would change: %v", out.Steps)
	}
}

// TestKeepWorldNeverReachesTheArchiveOrTheDestroy.
//
// --keep-world is a content change with a restart, and that is why it needs no
// confirmation: it exits after step 6 and the two destructive steps are below
// that line.
func TestKeepWorldNeverReachesTheArchiveOrTheDestroy(t *testing.T) {
	r := newRig()
	out, err := Run(context.Background(), r.cfg, Options{KeepWorld: true})
	if err != nil {
		t.Fatalf("--keep-world needs no confirmation and must not fail: %v", err)
	}
	for _, forbidden := range []string{
		"archives.preflight", "archives.snapshot", "archives.verify", "destroyer.destroy",
	} {
		if r.inj.count(forbidden) != 0 {
			t.Errorf("--keep-world reached %s", forbidden)
		}
	}
	if out.WorldTouched {
		t.Error("--keep-world reports the world touched")
	}
	if !out.KeptWorld || out.Untouched == "" {
		t.Errorf("outcome = %+v", out)
	}
	if r.hook.applied != 1 {
		t.Errorf("hooks applied %d time(s), want 1", r.hook.applied)
	}
	if !out.Running {
		t.Error("--keep-world left the server down")
	}
}

// TestAFullRebuildNeedsTheInstanceNameTyped.
//
// Section 16.4's contract, and rebuild.sh's own prompt. Not "y": the point of
// typing the name is that muscle memory cannot answer it.
func TestAFullRebuildNeedsTheInstanceNameTyped(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  Options
		ok   bool
	}{
		{"nothing typed", Options{}, false},
		{"the wrong name", Options{Typed: "pz-test"}, false},
		{"y", Options{Typed: "y"}, false},
		{"the name", Options{Typed: testInstance}, true},
		{"--yes", Options{Yes: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig()
			out, err := Run(context.Background(), r.cfg, tc.opt)
			if tc.ok {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrNotConfirmed) {
				t.Fatalf("err = %v, want ErrNotConfirmed", err)
			}
			if r.srv.stops != 0 || r.tim.masked != 0 {
				t.Error("an unconfirmed rebuild stopped the server or masked the timers")
			}
			if out.WorldTouched || out.Untouched == "" {
				t.Errorf("outcome = %+v", out)
			}
		})
	}
}

// TestAnUnaccountedItemRefusesBeforeAnythingStops.
//
// rebuild.sh lines 77-80, which counted the ids it parsed against a count taken
// from the raw HTML and refused to rewrite mods.txt when they disagreed. The
// generalisation matters more here than in the nightly: a rebuild generates a
// NEW world from the list, so an incomplete list is not a degraded sync but a
// world permanently missing mods.
func TestAnUnaccountedItemRefusesBeforeAnythingStops(t *testing.T) {
	r := newRig()
	r.cont.plan = jobs.ContentPlan{Changed: true, Unaccounted: []string{"2169435993"}}
	out, err := Run(context.Background(), r.cfg, Options{Yes: true})
	if !errors.Is(err, ErrUnaccounted) {
		t.Fatalf("err = %v, want ErrUnaccounted", err)
	}
	if r.cont.writes != 0 || r.srv.stops != 0 || r.des.count() != 0 {
		t.Error("the refusal happened after something had already changed")
	}
	if !strings.Contains(err.Error(), "NOT rewritten") {
		t.Errorf("the refusal must say the list was not rewritten: %v", err)
	}
	if out.WorldTouched {
		t.Error("outcome says the world was touched")
	}
}

// TestABlockedItemRefusesARebuildEvenThoughItWouldNotRefuseANightly.
//
// The asymmetry is deliberate and it is the difference between the two jobs. A
// nightly with a blocked CurseForge file leaves the existing world alone, so it
// reports and carries on without downtime (section 13.5). A rebuild DISCARDS
// the world and regenerates it from the list, so an incomplete list has to stop
// it before the confirmation.
func TestABlockedItemRefusesARebuildEvenThoughItWouldNotRefuseANightly(t *testing.T) {
	r := newRig()
	r.cont.plan = jobs.ContentPlan{Changed: true, Blocked: []string{"238222"}}
	_, err := Run(context.Background(), r.cfg, Options{Yes: true})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if r.srv.stops != 0 {
		t.Error("a blocked item stopped the server")
	}

	// And the same plan through the nightly's derivation is NOT a content
	// diff, which is the other half of the asymmetry.
	d, derr := update.ContentDiff(r.cont.plan)
	if derr != nil || d.Changed {
		t.Errorf("the nightly's content axis reads a blocked item as work: %+v, %v", d, derr)
	}
}

// TestTimersAreMaskedBeforeTheStopAndUnmaskedOnEveryPath.
//
// In that order: a nightly firing between the stop and the mask would start the
// server back up underneath step 4's download boot. And unmasking is a defer,
// so it happens on the refusal paths too.
func TestTimersAreMaskedBeforeTheStopAndUnmaskedOnEveryPath(t *testing.T) {
	r := newRig()
	r.inj.at, r.inj.mode = "server.waitready", "error"
	if _, err := Run(context.Background(), r.cfg, Options{Yes: true}); err == nil {
		t.Fatal("expected a failure")
	}
	calls := r.inj.recorded()
	mask := slices.Index(calls, "timers.mask")
	stop := slices.Index(calls, "server.stop")
	if mask < 0 || stop < 0 || mask > stop {
		t.Fatalf("order = %v; the mask must come first", calls)
	}
	if r.tim.unmasked != 1 {
		t.Errorf("timers unmasked %d time(s) on the failure path, want 1", r.tim.unmasked)
	}
}

// TestAFailedUnmaskIsReportedAndDoesNotMaskTheRealError.
//
// The real error is what the operator is acting on. A deferred cleanup that
// replaced it would be a cleanup that hid the fault.
func TestAFailedUnmaskIsReportedAndDoesNotMaskTheRealError(t *testing.T) {
	r := newRig()
	r.inj.at, r.inj.mode = "timers.unmask", "error"
	out, err := Run(context.Background(), r.cfg, Options{Yes: true})
	if err != nil {
		t.Fatalf("a failed unmask must not fail a completed rebuild: %v", err)
	}
	joined := strings.Join(out.Steps, " ")
	if !strings.Contains(joined, "could not be unmasked") {
		t.Errorf("the failed unmask was not reported: %v", out.Steps)
	}
	if !strings.Contains(joined, "no nightly backup") {
		t.Errorf("the report does not say what the consequence is: %v", out.Steps)
	}
}

// TestMissingContentRestartsTheServerAndNamesTheIds.
//
// Step 5's exact instruction, and rebuild.sh's own message. "Not downloaded
// yet: <ids> - the world was NOT touched. Steam stalls sometimes: rerun."
func TestMissingContentRestartsTheServerAndNamesTheIds(t *testing.T) {
	r := newRig()
	// A real (empty) directory tree, so the Settler's default filesystem seam
	// is the one under test: the item's directory never appears, and nothing
	// under the root ever changes, so quiescence is the answer.
	root := t.TempDir()
	r.cont.settle = update.SettleInput{
		Expect: []update.Expectation{{
			ID:   "2169435993",
			Dirs: []string{filepath.Join(root, "2169435993")},
		}},
		Roots: []string{root},
	}
	r.cfg.Poll, r.cfg.Quiet = time.Millisecond, time.Millisecond
	out, err := Run(context.Background(), r.cfg, Options{Yes: true})
	if !errors.Is(err, ErrContentMissing) {
		t.Fatalf("err = %v, want ErrContentMissing", err)
	}
	if !strings.Contains(err.Error(), "2169435993") {
		t.Errorf("the ids are not named: %v", err)
	}
	if r.des.count() != 0 {
		t.Fatal("the world was deleted with content missing")
	}
	if out.WorldTouched || out.Untouched == "" {
		t.Errorf("outcome = %+v", out)
	}
	if !slices.Contains(out.Missing, "2169435993") {
		t.Errorf("missing = %v", out.Missing)
	}
	if !r.srv.isActive() {
		t.Error("the server was left stopped; step 5 says to restart it")
	}
}

// TestAnAmbiguousActivationIdRefusesRatherThanGuesses.
//
// D-21's reason: a guessed activation name silently omits a mod while the
// server boots normally, and on a rebuild that mod is missing from a world
// nobody can roll back without the archive.
func TestAnAmbiguousActivationIdRefusesRatherThanGuesses(t *testing.T) {
	r := newRig()
	r.cont.res = jobs.Resolution{Changed: true, Ambiguous: []string{"2169435993"}}
	out, err := Run(context.Background(), r.cfg, Options{Yes: true})
	if err == nil {
		t.Fatal("an ambiguous activation id was accepted")
	}
	if !strings.Contains(err.Error(), "2169435993") {
		t.Errorf("the id is not named: %v", err)
	}
	if r.des.count() != 0 {
		t.Fatal("the world was deleted on an unresolved list")
	}
	if out.WorldTouched || out.Untouched == "" {
		t.Errorf("outcome = %+v", out)
	}
	if !r.srv.isActive() {
		t.Error("the server was left stopped")
	}
}

// TestAFailureAfterTheDestroyIsNotReportedAsUntouched.
//
// The other direction, and it matters as much: once the world is gone the
// outcome must say so. An operator told "the world is untouched" by a rebuild
// that deleted it would look for the fault in the wrong place and might well
// rerun the thing.
func TestAFailureAfterTheDestroyIsNotReportedAsUntouched(t *testing.T) {
	r := newRig()
	// The SECOND start fails: boot #1 succeeds, so the rebuild reaches the
	// destroy, and step 10 is where it falls over. The archive exists, the
	// world does not.
	calls := 0
	r.srv.startHook = func() error {
		calls++
		if calls >= 2 {
			return errors.New("unit failed to start")
		}
		return nil
	}
	out, err := Run(context.Background(), r.cfg, Options{Yes: true})
	if err == nil {
		t.Fatal("expected the final start to fail")
	}
	if !out.WorldTouched {
		t.Error("the world was deleted and the outcome says it was not touched")
	}
	if out.Untouched != "" {
		t.Errorf("the outcome claims %q after a destroy", out.Untouched)
	}
	if out.Archive == nil {
		t.Fatal("no archive recorded, so there is nothing to restore from")
	}
	if !strings.Contains(err.Error(), out.Archive.Path) {
		t.Errorf("the error does not name the archive to restore from: %v", err)
	}
}

// TestHooksFailingAfterTheDestroyDoesNotStopTheServerComingBack.
//
// Step 9 is a degraded rebuild, not a refusal: the world is already gone, so
// the only acceptable end state is a server generating the new one. Section
// 13.3's `start_anyway after archive`.
func TestHooksFailingAfterTheDestroyDoesNotStopTheServerComingBack(t *testing.T) {
	r := newRig()
	r.inj.at, r.inj.mode = "hooks.apply", "error"
	out, err := Run(context.Background(), r.cfg, Options{Yes: true})
	if err != nil {
		t.Fatalf("a failed hook after the destroy must not fail the rebuild: %v", err)
	}
	if !out.Running {
		t.Error("the server did not come back")
	}
	if !strings.Contains(strings.Join(out.Steps, " "), "hooks failed") {
		t.Errorf("the failed hook was not reported: %v", out.Steps)
	}
}

// TestAMisconfiguredRebuildRefusesBeforeItTouchesAnything.
func TestAMisconfiguredRebuildRefusesBeforeItTouchesAnything(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mangle func(*Config)
	}{
		{"no instance", func(c *Config) { c.Instance = "" }},
		{"no content", func(c *Config) { c.Content = nil }},
		{"no server", func(c *Config) { c.Server = nil }},
		{"no timers", func(c *Config) { c.Timers = nil }},
		{"no archives", func(c *Config) { c.Archives = nil }},
		{"no destroyer", func(c *Config) { c.Destroyer = nil }},
		{"no world paths", func(c *Config) { c.WorldPaths = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig()
			tc.mangle(&r.cfg)
			out, err := Run(context.Background(), r.cfg, Options{Yes: true})
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("err = %v, want ErrConfig", err)
			}
			if len(r.inj.recorded()) != 0 {
				t.Errorf("a misconfigured rebuild made %d call(s)", len(r.inj.recorded()))
			}
			if out.WorldTouched || out.Untouched == "" {
				t.Errorf("outcome = %+v", out)
			}
		})
	}
}

// TestNoFetchUsesTheListAsItStands, which is the Steam-is-down path.
func TestNoFetchUsesTheListAsItStands(t *testing.T) {
	r := newRig()
	if _, err := Run(context.Background(), r.cfg, Options{Yes: true, NoFetch: true}); err != nil {
		t.Fatal(err)
	}
	if len(r.cont.fetch) != 1 || r.cont.fetch[0] {
		t.Errorf("fetch = %v, want one call with fetch=false", r.cont.fetch)
	}
}

// TestTheJobAdapterDoesNotFetchTheCollectionTwice.
//
// The job layer's table has a `rebuild.plan` step so that the fail-closed count
// check refuses before the guard has anything to restore. That means Plan is
// called by the step AND would be called again inside Run -- and rebuild.sh's
// own failure message names Steam rate-limiting as the likely cause of a
// refused rebuild, so asking twice per rebuild is a real cost.
func TestTheJobAdapterDoesNotFetchTheCollectionTwice(t *testing.T) {
	r := newRig()
	j := &Job{Cfg: r.cfg, Opt: Options{Yes: true}}

	if _, err := j.Plan(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("the rebuild failed: %v", err)
	}
	if n := r.inj.count("content.plan"); n != 1 {
		t.Errorf("content.plan ran %d time(s), want 1", n)
	}
	if !res.WorldTouched || res.Archive == nil || res.Restore == "" {
		t.Errorf("Rebuilt = %+v; the job summary needs all three", res)
	}
}

// TestTheJobAdapterCarriesTheUntouchedSentenceToTheJobSummary.
func TestTheJobAdapterCarriesTheUntouchedSentenceToTheJobSummary(t *testing.T) {
	r := newRig()
	r.inj.at, r.inj.mode = "archives.verify", "error"
	j := &Job{Cfg: r.cfg, Opt: Options{Yes: true}}
	res, err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected a failure")
	}
	if res.WorldTouched {
		t.Error("Rebuilt says the world was touched")
	}
	if res.Untouched == "" {
		t.Error("Rebuilt carries no sentence, so the job row cannot answer the only question " +
			"the operator has")
	}
}

// TestJailDestroyerRefusesWithoutAJail, the one branch of the production
// destroyer that does not belong to another package.
func TestJailDestroyerRefusesWithoutAJail(t *testing.T) {
	err := JailDestroyer{}.Destroy(context.Background(), &archive.Ref{}, []string{"/srv"})
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig", err)
	}
}
