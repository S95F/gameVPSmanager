package backup

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// shippedManifest loads one of the six manifests gvm ships.
//
// The freeze tests run against the REAL manifests rather than against
// fixtures, because the contract being tested is "this game's declared flush
// is what happens" -- and a fixture that drifted from games/valheim.yaml would
// pass while the shipped behaviour changed underneath it.
func shippedManifest(t *testing.T, id string) *spec.Manifest {
	t.Helper()
	raw, err := fs.ReadFile(games.FS, id+".yaml")
	if err != nil {
		t.Fatalf("read %s.yaml: %v", id, err)
	}
	m, diags, err := spec.ValidateBytes("games/"+id+".yaml", raw)
	if err != nil {
		t.Fatalf("%s.yaml: %v\n%s", id, err, diags)
	}
	return m
}

// TestTheShippedFreezesAreReadAsTheirManifestsDeclareThem.
//
// Every shipped manifest's freeze, as code sees it. The numbers come from
// games/*.yaml and the point of the test is that they still do: a freeze read
// one rung short, or with a budget of zero, is a backup taken mid-write and
// nothing anywhere would say so.
func TestTheShippedFreezesAreReadAsTheirManifestsDeclareThem(t *testing.T) {
	cases := []struct {
		game     string
		frozen   bool
		before   []string
		marker   string
		after    []string
		budget   time.Duration
		firstVia Via
	}{
		{
			game: "projectzomboid", frozen: true,
			before: []string{"save"}, marker: "(?i)(save complete|saved)",
			after: nil, budget: 120 * time.Second, firstVia: ViaConsole,
		},
		{
			game: "valheim", frozen: true,
			before: []string{"save"}, after: nil,
			budget: 60 * time.Second, firstVia: ViaConsole,
		},
		{
			game: "palworld", frozen: true,
			before: []string{"Save"}, after: nil,
			budget: 60 * time.Second, firstVia: ViaControl,
		},
		{
			game: "7daystodie", frozen: true,
			before: []string{"saveworld"}, after: nil,
			budget: 90 * time.Second, firstVia: ViaConsole,
		},
		// Core Keeper and the generic Steam manifest declare
		// `{before: none, after: none}`: nothing is suspended, so no lease is
		// armed and the game's own autosave is the only guarantee.
		{game: "corekeeper", frozen: false, budget: 30 * time.Second},
		{game: "generic-steam", frozen: false, budget: 30 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.game, func(t *testing.T) {
			m := shippedManifest(t, tc.game)
			// A nil renderer is correct here and is itself an assertion: not
			// one shipped freeze rung contains a template, so none of them can
			// fail to render at 4am.
			plan, err := BuildFreeze(m, nil)
			if err != nil {
				t.Fatalf("BuildFreeze: %v", err)
			}
			if plan.Frozen() != tc.frozen {
				t.Errorf("Frozen() = %v, want %v", plan.Frozen(), tc.frozen)
			}
			if plan.Budget != tc.budget {
				t.Errorf("lease_budget = %v, want %v", plan.Budget, tc.budget)
			}
			if len(plan.Before) != len(tc.before) {
				t.Fatalf("%d before rungs, want %d: %+v", len(plan.Before), len(tc.before), plan.Before)
			}
			for i, want := range tc.before {
				if plan.Before[i].Send != want {
					t.Errorf("before[%d].send = %q, want %q", i, plan.Before[i].Send, want)
				}
			}
			if len(tc.before) > 0 {
				if plan.Before[0].Via != tc.firstVia {
					t.Errorf("before[0].via = %q, want %q", plan.Before[0].Via, tc.firstVia)
				}
				if plan.Before[0].WaitForLog != tc.marker {
					t.Errorf("before[0].wait_for_log = %q, want %q", plan.Before[0].WaitForLog, tc.marker)
				}
			}
			if len(plan.After) != len(tc.after) {
				t.Errorf("%d after rungs, want %d: %+v", len(plan.After), len(tc.after), plan.After)
			}
		})
	}
}

// minecraftShapedFreeze is the three-rung freeze section 13.2 uses as its
// example: save-off, save-all flush, save-on.
//
// It is built here rather than loaded because Minecraft is not in the v1
// shipped set, and it is the ONLY shape where a missing thaw is catastrophic
// rather than merely unconfirmed -- so the tests that are about the thaw need
// it.
func minecraftShapedFreeze() *spec.Manifest {
	m := &spec.Manifest{}
	m.Spec.Backup.Freeze = spec.FreezeSpec{
		Before:      &spec.ShutdownStep{Via: spec.ViaControl, Send: "save-off", Wait: spec.Duration(5 * time.Second)},
		During:      &spec.ShutdownStep{Via: spec.ViaControl, Send: "save-all flush", Wait: spec.Duration(120 * time.Second)},
		After:       &spec.ShutdownStep{Via: spec.ViaControl, Send: "save-on", Wait: spec.Duration(5 * time.Second)},
		LeaseBudget: spec.Duration(300 * time.Second),
	}
	return m
}

// TestTheThreeRungFreezeOrdersBeforeThenDuringThenThaw.
func TestTheThreeRungFreezeOrdersBeforeThenDuringThenThaw(t *testing.T) {
	plan, err := BuildFreeze(minecraftShapedFreeze(), nil)
	if err != nil {
		t.Fatalf("BuildFreeze: %v", err)
	}
	if len(plan.Before) != 2 || plan.Before[0].Send != "save-off" || plan.Before[1].Send != "save-all flush" {
		t.Fatalf("before rungs = %+v, want save-off then save-all flush", plan.Before)
	}
	if len(plan.After) != 1 || plan.After[0].Send != "save-on" {
		t.Fatalf("after rungs = %+v, want save-on", plan.After)
	}
	steps := plan.ThawSteps()
	if len(steps) != 1 || steps[0].Via != ViaControl || steps[0].Send != "save-on" {
		t.Fatalf("ThawSteps() = %+v; this is what the lease hands to the runner", steps)
	}
}

// TestAFreezeWithNoThawIsRefusedHereToo.
//
// spec's own validator already refuses it ("every freeze needs its thaw"), and
// this is the same claim one layer down -- because by here the manifest may
// have come from a pinned manifest.lock.yaml written by an older gvm whose
// validator did not make that check (D-04).
func TestAFreezeWithNoThawIsRefusedHereToo(t *testing.T) {
	m := minecraftShapedFreeze()
	m.Spec.Backup.Freeze.After = nil
	_, err := BuildFreeze(m, nil)
	if !errors.Is(err, ErrFreeze) {
		t.Fatalf("BuildFreeze error = %v, want ErrFreeze", err)
	}
	if !strings.Contains(err.Error(), "silently stops persisting") {
		t.Errorf("the message does not say what goes wrong: %v", err)
	}
}

// TestAFreezeRungCannotBeASignal: a signal does not flush a world, and a
// ladder rung that wandered into the freeze block would be a kill where a save
// was meant.
func TestAFreezeRungCannotBeASignal(t *testing.T) {
	m := &spec.Manifest{}
	m.Spec.Backup.Freeze.Before = &spec.ShutdownStep{Via: spec.ViaSignal, Signal: "SIGUSR1"}
	_, err := BuildFreeze(m, nil)
	if !errors.Is(err, ErrFreeze) {
		t.Fatalf("BuildFreeze error = %v, want ErrFreeze", err)
	}
	if !strings.Contains(err.Error(), "does not flush a world") {
		t.Errorf("message = %v", err)
	}
}

// TestTheFlushWaitsForTheMarkerAndNotForAClock.
//
// This is the end of `sleep 15`. Project Zomboid declares
// `wait_for_log: "(?i)(save complete|saved)"` with a 20-second budget, so the
// flush returns when the GAME says it is done -- and the budget is a bound
// rather than a duration. Nothing sleeps.
func TestTheFlushWaitsForTheMarkerAndNotForAClock(t *testing.T) {
	plan, err := BuildFreeze(shippedManifest(t, "projectzomboid"), nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := newClock(testNow)
	console := &fakeConsole{StartSeq: 100}
	log := &fakeLog{cursor: 10}
	ch := Channels{Console: console, Log: log, Sleep: clock.Sleep, Now: clock.Now}

	res := RunRungs(context.Background(), ch, plan.Before)
	if res.Degraded {
		t.Fatalf("a confirmed flush reported itself degraded: %s", res.Why)
	}
	if len(res.Rungs) != 1 || !res.Rungs[0].Delivered || !res.Rungs[0].MarkerMatched {
		t.Fatalf("rungs = %+v, want one delivered rung with a matched marker", res.Rungs)
	}
	if res.Rungs[0].Assumed {
		t.Error("a marker-confirmed flush must not be recorded as assumed")
	}
	if got := clock.Slept(); len(got) != 0 {
		t.Errorf("the flush slept %v; the marker is the signal and backup.sh's sleep is gone", got)
	}
	if got := console.lines(); len(got) != 1 || got[0] != "save" {
		t.Fatalf("console got %q, want one save", got)
	}
	asked := log.asked()
	if len(asked) != 1 {
		t.Fatalf("the marker was awaited %d times", len(asked))
	}
	if asked[0].Pattern != "(?i)(save complete|saved)" {
		t.Errorf("pattern = %q, want the manifest's", asked[0].Pattern)
	}
	if asked[0].Budget != 20*time.Second {
		t.Errorf("budget = %v, want the manifest's 20s", asked[0].Budget)
	}
}

// TestTheMarkerCursorIsTakenBeforeTheSend.
//
// A "save complete" from the PREVIOUS autosave sitting in the console ring
// would satisfy the wait instantly, and the archive would be taken while the
// game was still writing. So the cursor is read before the command goes out,
// and the echo's own sequence moves it forward again.
func TestTheMarkerCursorIsTakenBeforeTheSend(t *testing.T) {
	plan, err := BuildFreeze(shippedManifest(t, "projectzomboid"), nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := newClock(testNow)
	// The ring's cursor is behind the echo's sequence, which is the ordinary
	// case: the send lands after everything that was already there.
	console := &fakeConsole{StartSeq: 100}
	log := &fakeLog{cursor: 10}
	RunRungs(context.Background(), Channels{Console: console, Log: log,
		Sleep: clock.Sleep, Now: clock.Now}, plan.Before)

	asked := log.asked()
	if len(asked) != 1 {
		t.Fatalf("awaits = %+v", asked)
	}
	// 101 is the echo's sequence, so the search starts at 102: strictly after
	// the line the send itself produced.
	if asked[0].Since != 102 {
		t.Errorf("the marker search started at %d, want 102 (one past the echo at 101); "+
			"starting earlier would let a previous autosave's marker satisfy this flush",
			asked[0].Since)
	}
}

// TestAGameWithNoMarkerGetsAnExplicitRecordedTimeout.
//
// Valheim declares `{via: console, send: "save", wait: 10s}` and no marker.
// There is nothing better available than a timeout -- and the timeout is now in
// the record, which is the whole difference from `sleep 15`. A degraded flush
// that is visible is a different thing from one that is assumed.
func TestAGameWithNoMarkerGetsAnExplicitRecordedTimeout(t *testing.T) {
	plan, err := BuildFreeze(shippedManifest(t, "valheim"), nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := newClock(testNow)
	console := &fakeConsole{}
	res := RunRungs(context.Background(), Channels{Console: console,
		Sleep: clock.Sleep, Now: clock.Now}, plan.Before)

	if !res.Degraded {
		t.Fatal("an unconfirmed flush must be recorded as degraded")
	}
	if !strings.Contains(res.Why, "declares no save marker") {
		t.Errorf("the reason does not name the cause: %q", res.Why)
	}
	if len(res.Rungs) != 1 || !res.Rungs[0].Assumed {
		t.Fatalf("rungs = %+v, want one assumed rung", res.Rungs)
	}
	if res.Rungs[0].Waited != 10*time.Second {
		t.Errorf("waited %v, want the manifest's 10s", res.Rungs[0].Waited)
	}
	if got := clock.Slept(); len(got) != 1 || got[0] != 10*time.Second {
		t.Errorf("slept %v, want one 10s wait", got)
	}
}

// TestAMarkerWithNoRingToWatchDegradesAndSaysSo.
//
// The standalone path, and `gvm backup` against an instance whose console ring
// gvm is not holding. The marker cannot be watched, so the flush falls back to
// a timeout -- and the record says which of the two happened, because
// "waited and the marker never came" and "could not watch for the marker" lead
// an operator to different places.
func TestAMarkerWithNoRingToWatchDegradesAndSaysSo(t *testing.T) {
	plan, err := BuildFreeze(shippedManifest(t, "projectzomboid"), nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := newClock(testNow)
	res := RunRungs(context.Background(), Channels{Console: &fakeConsole{},
		Sleep: clock.Sleep, Now: clock.Now}, plan.Before)

	if !res.Degraded || !res.Rungs[0].Assumed {
		t.Fatalf("res = %+v, want a degraded assumed flush", res)
	}
	if !strings.Contains(res.Rungs[0].Note, "no console ring") {
		t.Errorf("the note does not explain why: %q", res.Rungs[0].Note)
	}
}

// TestAMarkerThatNeverAppearsDegradesTheFlushAndNotTheBackup.
//
// The archive is still taken. A backup of a world that may be a few minutes
// stale is worth far more than no backup, and the flag plus the sentence is
// what turns "the world rolled back" from a mystery into a job log entry.
func TestAMarkerThatNeverAppearsDegradesTheFlushAndNotTheBackup(t *testing.T) {
	plan, err := BuildFreeze(shippedManifest(t, "projectzomboid"), nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := newClock(testNow)
	res := RunRungs(context.Background(), Channels{
		Console: &fakeConsole{},
		Log:     &fakeLog{Match: ErrNoMarker},
		Sleep:   clock.Sleep, Now: clock.Now,
	}, plan.Before)

	if !res.Degraded {
		t.Fatal("a flush whose marker never arrived must be degraded")
	}
	if res.Rungs[0].MarkerMatched {
		t.Error("MarkerMatched is true for a marker that timed out")
	}
	if !res.Rungs[0].Delivered {
		t.Error("the command WAS delivered; only its confirmation is missing")
	}
	if !strings.Contains(res.Why, "the marker never appeared") {
		t.Errorf("reason = %q", res.Why)
	}
}

// TestAnUndeliveredFlushIsRecordedRatherThanSwallowed.
//
// backup.sh wrote `|| true` here, which made "the save never reached the game"
// and "the save completed" the same outcome. The command still cannot be
// retried usefully and the backup still proceeds -- but the failure is in the
// record with the reason attached.
func TestAnUndeliveredFlushIsRecordedRatherThanSwallowed(t *testing.T) {
	plan, err := BuildFreeze(shippedManifest(t, "palworld"), nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := newClock(testNow)
	res := RunRungs(context.Background(), Channels{
		Control: &fakeControl{NotReady: "RCON does not bind until the server is fully booted"},
		Sleep:   clock.Sleep, Now: clock.Now,
	}, plan.Before)

	if !res.Degraded {
		t.Fatal("an undelivered flush reported itself fine")
	}
	if res.Rungs[0].Delivered {
		t.Error("Delivered is true for a channel that was not ready")
	}
	if !strings.Contains(res.Why, "was not delivered") {
		t.Errorf("reason = %q", res.Why)
	}
}

// TestTheThawIsStrictWhereTheFlushIsForgiving.
//
// The asymmetry is the point. A flush that did not land costs the minutes since
// the last autosave. A thaw that did not land leaves a running server that has
// silently stopped persisting -- section 13.2's worst failure in the system --
// so it is never absorbed, and the message says that the lease stays armed.
func TestTheThawIsStrictWhereTheFlushIsForgiving(t *testing.T) {
	plan, err := BuildFreeze(minecraftShapedFreeze(), nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := newClock(testNow)
	res := Thaw(context.Background(), Channels{
		Control: &fakeControl{Fail: errInjected},
		Sleep:   clock.Sleep, Now: clock.Now,
	}, plan.After)

	if res.Thawed {
		t.Fatal("a thaw whose command failed reported success")
	}
	if !strings.Contains(res.Why, "the runner will re-send it at the deadline") {
		t.Errorf("the message does not point at the lease: %q", res.Why)
	}
}

// TestAThawWithNothingToSendSucceeds: `after: none`, which is four of the six
// shipped manifests.
func TestAThawWithNothingToSendSucceeds(t *testing.T) {
	plan, err := BuildFreeze(shippedManifest(t, "projectzomboid"), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := Thaw(context.Background(), Channels{Now: newClock(testNow).Now}, plan.After)
	if !res.Thawed {
		t.Fatalf("a freeze with no thaw rung did not report itself thawed: %+v", res)
	}
}

// TestTheMinecraftFlushDoesNotTakeTwoMinutesToTest.
//
// Rule 5, made concrete: the 120-second `save-all flush` is a budget handed to
// an injected clock, so the whole three-rung freeze runs in microseconds. A
// freeze that could only be exercised in real time is a freeze nobody
// exercises.
func TestTheMinecraftFlushDoesNotTakeTwoMinutesToTest(t *testing.T) {
	plan, err := BuildFreeze(minecraftShapedFreeze(), nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := newClock(testNow)
	control := &fakeControl{}
	start := time.Now()
	res := RunRungs(context.Background(), Channels{Control: control,
		Sleep: clock.Sleep, Now: clock.Now}, plan.Before)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the flush took %v of real time", took)
	}
	if got := control.lines(); len(got) != 2 || got[0] != "save-off" || got[1] != "save-all flush" {
		t.Fatalf("control got %q, want save-off then save-all flush", got)
	}
	// The elapsed figure is the CLOCK's, which is what goes in the record and
	// is the leading part of the stall under a direct snapshot.
	if res.Elapsed != 125*time.Second {
		t.Errorf("elapsed = %v, want the 5s + 120s the manifest declares", res.Elapsed)
	}
}

// TestARungThatRendersToNothingIsRefused.
//
// An empty line to a console is a newline, and a newline is not a save. A
// template that rendered away -- a var that resolved to "" -- must be a
// preflight refusal rather than a silent no-op in the middle of a freeze.
func TestARungThatRendersToNothingIsRefused(t *testing.T) {
	m := &spec.Manifest{}
	m.Spec.Backup.Freeze.Before = &spec.ShutdownStep{Via: spec.ViaConsole, Send: "{{ .Var.saver }}"}
	r := spec.NewRenderer(spec.RenderData{Var: spec.VarValues{"saver": ""}})
	_, err := BuildFreeze(m, r)
	if !errors.Is(err, ErrFreeze) {
		t.Fatalf("BuildFreeze error = %v, want ErrFreeze", err)
	}
	if !strings.Contains(err.Error(), "to nothing") {
		t.Errorf("message = %v", err)
	}
}
