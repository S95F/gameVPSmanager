package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

func managerFor(t *testing.T, active bool) (*Manager, *fakeFS, *fakeHost) {
	t.Helper()
	f, _ := fullHost(t)
	st := platform.UnitState{LoadState: "loaded", ActiveState: "inactive", Result: "success"}
	if active {
		st = platform.UnitState{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 41799}
	}
	sd := newSystemd().set(instance.UnitName("valheim-1"), st)
	h := &fakeHost{}
	m := &Manager{
		Obs: &Observer{
			FS: f, Units: sd,
			Now: func() time.Time { return at("2026-09-04T19:00:00Z") },
		},
		Host: h,
		Now:  func() time.Time { return at("2026-09-04T19:00:00Z") },
		Shutdown: func(context.Context, string, time.Duration) (Outcome, error) {
			return Outcome{Rung: "console", Degraded: false, Took: 42 * time.Second}, nil
		},
	}
	return m, f, h
}

func TestStartClearsTheHoldBeforeStarting(t *testing.T) {
	m, f, h := managerFor(t, false)
	f.put(instance.PathsFor("valheim-1").Hold, "2026-09-04T18:00:00Z root_cli disk swap\n")

	res, err := m.Do(context.Background(), Request{
		Verb: VerbStart, Instance: "valheim-1", Actor: audit.RootCLI(), Reason: "back after the swap",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []hostops.Verb{hostops.VerbFileRemove, hostops.VerbUnitStart}
	if got := h.verbs(); !equalVerbs(got, want) {
		t.Fatalf("issued %v, want %v", got, want)
	}
	// The ORDER is the assertion. A hold removed after a failed start would
	// still be there, and the guard timer -- the thing that recovers a failed
	// start within five minutes -- would leave the instance down.
	if h.reqs[0].Verb != hostops.VerbFileRemove {
		t.Fatal("the hold was not cleared before the start")
	}
	if h.reqs[0].File != hostops.FileHold {
		t.Fatalf("the removed file kind was %q", h.reqs[0].File)
	}
	if got := h.reqs[1].Params["reason"]; got != "back after the swap" {
		t.Errorf("the start's audit params carry reason=%v", got)
	}
	if res.Before.Phase != PhaseStopped {
		t.Errorf("Before = %s, want stopped", res.Before.Phase)
	}
}

// TestStopWritesTheHoldOnBothSidesOfTheStop is a systemd detail with teeth.
//
// The packaged gvm@.service declares RuntimeDirectoryPreserve=restart, so a
// clean STOP removes /run/gvm/<name> and everything in it -- the hold
// included. A hold written only before the stop is therefore deleted by the
// very stop it exists to survive, and `gvm-guard.timer` brings the instance
// back within five minutes, over the operator's decision.
//
// So it is written twice: before, so a crash mid-stop still leaves the marker,
// and after, because the successful path deletes it.
func TestStopWritesTheHoldOnBothSidesOfTheStop(t *testing.T) {
	m, f, h := managerFor(t, true)
	hold := instance.PathsFor("valheim-1").Hold
	// Model systemd: the runtime directory goes away with the unit.
	h.onStop = func() { delete(f.files, hold) }

	res, err := m.Do(context.Background(), Request{
		Verb: VerbStop, Instance: "valheim-1", Actor: audit.RootCLI(),
		Reason: "swapping the disk", Budget: 300 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []hostops.Verb{hostops.VerbFileWrite, hostops.VerbUnitStop, hostops.VerbFileWrite}
	if got := h.verbs(); !equalVerbs(got, want) {
		t.Fatalf("issued %v, want %v -- see RuntimeDirectoryPreserve=restart", got, want)
	}
	for i, r := range h.reqs {
		if r.Verb != hostops.VerbFileWrite {
			continue
		}
		if r.File != hostops.FileHold {
			t.Fatalf("step %d wrote file kind %q", i, r.File)
		}
		if !strings.Contains(string(r.Body), "swapping the disk") {
			t.Errorf("the hold does not record why: %q", r.Body)
		}
		if !strings.Contains(string(r.Body), "root_cli") {
			t.Errorf("the hold does not record who: %q", r.Body)
		}
	}
	if res.Shutdown == nil || res.Shutdown.Rung != "console" {
		t.Fatalf("the shutdown outcome was not carried back: %+v", res.Shutdown)
	}
}

// TestTheStopRowCanSayHowTheWorldWasSaved.
//
// audit.InstanceStop REQUIRES `rung` on its success row: it is the field that
// turns "the server stopped" into "the server saved", and section 14.8 makes
// it mandatory precisely because the bash suite's stops could not say. hostops
// has no way to know it, so the manager assembles it from the ladder's own
// report.
func TestTheStopRowCanSayHowTheWorldWasSaved(t *testing.T) {
	m, _, h := managerFor(t, true)
	if _, err := m.Do(context.Background(), Request{
		Verb: VerbStop, Instance: "valheim-1", Actor: audit.RootCLI(),
		Reason: "nightly",
	}); err != nil {
		t.Fatal(err)
	}
	req, ok := h.find(hostops.VerbUnitStop)
	if !ok {
		t.Fatal("no stop was issued")
	}
	for _, key := range []string{"rung", "degraded", "reason"} {
		if _, ok := req.Params[key]; !ok {
			t.Errorf("the stop's params carry no %q: %+v", key, req.Params)
		}
	}
	// --budget was removed (Phase 34): a stop never records a budget_sec, so
	// the only ladder bound an auditor can trust is the unit's TimeoutStopSec.
	if _, ok := req.Params["budget_sec"]; ok {
		t.Errorf("the stop recorded budget_sec; --budget is gone and the ladder bound is "+
			"spec.shutdown.budget: %+v", req.Params)
	}
	// The params must survive the allowlist, or the row records _unallowlisted
	// instead of the value.
	filtered, dropped := audit.Redact(audit.InstanceStop, req.Params)
	if len(dropped) != 0 {
		t.Errorf("the stop passed params the allowlist drops: %v", dropped)
	}
	if filtered["rung"] != "console" {
		t.Errorf("rung survived as %v", filtered["rung"])
	}
}

// TestAStopWithNoLadderIsRefused.
//
// Not a convenience left unimplemented. A stop issued with no way to learn the
// rung writes an audit row that says a world was stopped and cannot say
// whether it was saved first -- which is exactly the record the bash suite
// already produced, and the reason the field exists.
func TestAStopWithNoLadderIsRefused(t *testing.T) {
	m, _, h := managerFor(t, true)
	m.Shutdown = nil
	_, err := m.Do(context.Background(), Request{
		Verb: VerbStop, Instance: "valheim-1", Actor: audit.RootCLI(),
	})
	if !errors.Is(err, ErrNoLadder) {
		t.Fatalf("Do = %v, want ErrNoLadder", err)
	}
	if len(h.reqs) != 0 {
		t.Fatalf("it issued %v before refusing; nothing may reach the host", h.verbs())
	}
}

// TestARefusedVerbTouchesNothing. The precondition runs before the first
// privileged call, so a refusal leaves the host exactly as it was -- including
// the hold, which a partially-executed stop would have written.
func TestARefusedVerbTouchesNothing(t *testing.T) {
	m, f, h := managerFor(t, false)
	// A stale update lease: the install tree may be half-written.
	l := Lease{Instance: "valheim-1", Kind: JobUpdate, PID: 900, StartedAt: at("2026-09-04T18:00:00Z")}
	body, err := l.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	f.put(LeasePath("valheim-1"), string(body))
	m.Obs.Alive = aliveFunc()

	res, err := m.Do(context.Background(), Request{
		Verb: VerbStart, Instance: "valheim-1", Actor: audit.RootCLI(),
	})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Do = %v, want a refusal", err)
	}
	if len(h.reqs) != 0 {
		t.Fatalf("a refused start still issued %v", h.verbs())
	}
	if res.Before.Rule != RuleLeaseStale {
		t.Errorf("the refusal reports rule %s", res.Before.Rule)
	}
}

// TestForceOverridesOnlyWhatItMayOverride.
//
// --force is for the refusals where the operator plausibly knows something gvm
// does not. It must not open the ones where forcing only relocates the
// failure: a manifest pin mismatch makes the runner exit 64 a second later,
// and swapping a clear message for a unit in "restart prevented" is worse.
func TestForceOverridesOnlyWhatItMayOverride(t *testing.T) {
	t.Run("a stale lease is forceable", func(t *testing.T) {
		m, f, h := managerFor(t, false)
		l := Lease{Instance: "valheim-1", Kind: JobUpdate, PID: 900, StartedAt: at("2026-09-04T18:00:00Z")}
		body, err := l.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		f.put(LeasePath("valheim-1"), string(body))
		m.Obs.Alive = aliveFunc()

		res, err := m.Do(context.Background(), Request{
			Verb: VerbStart, Instance: "valheim-1", Actor: audit.RootCLI(), Force: true,
		})
		if err != nil {
			t.Fatalf("--force did not get past a forceable refusal: %v", err)
		}
		if !res.Forced {
			t.Error("the result does not record that gvm was overridden")
		}
		if _, ok := h.find(hostops.VerbUnitStart); !ok {
			t.Fatalf("nothing was started: %v", h.verbs())
		}
	})

	t.Run("a manifest pin mismatch is not", func(t *testing.T) {
		m, f, h := managerFor(t, false)
		// Break the pin by replacing the pinned manifest with different bytes.
		f.put(instance.PathsFor("valheim-1").ManifestLock,
			"apiVersion: gvm/v1\nkind: GameManifest\nmetadata: {id: valheim, revision: 9}\n")

		_, err := m.Do(context.Background(), Request{
			Verb: VerbStart, Instance: "valheim-1", Actor: audit.RootCLI(), Force: true,
		})
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("--force got past a pin mismatch: %v", err)
		}
		if len(h.reqs) != 0 {
			t.Fatalf("a forced start on a bad pin still issued %v", h.verbs())
		}
	})
}

func TestRestartIsOneJobAndClearsTheHold(t *testing.T) {
	m, _, h := managerFor(t, true)
	if _, err := m.Do(context.Background(), Request{
		Verb: VerbRestart, Instance: "valheim-1", Actor: audit.RootCLI(), Reason: "config change",
	}); err != nil {
		t.Fatal(err)
	}
	want := []hostops.Verb{hostops.VerbFileRemove, hostops.VerbUnitRestart}
	if got := h.verbs(); !equalVerbs(got, want) {
		t.Fatalf("issued %v, want %v", got, want)
	}
	// Not stop-then-start: RuntimeDirectoryPreserve=restart keeps
	// /run/gvm/<name>, so the console socket an operator is attached to
	// survives a restart and does not survive a stop followed by a start.
	for _, r := range h.reqs {
		if r.Verb == hostops.VerbUnitStop {
			t.Fatal("a restart was issued as a stop; the console socket would be recreated")
		}
	}
}

func TestDoRefusesAnUndeclaredVerbAndABadName(t *testing.T) {
	m, _, h := managerFor(t, true)
	if _, err := m.Do(context.Background(), Request{
		Verb: Verb("kill"), Instance: "valheim-1", Actor: audit.RootCLI(),
	}); !errors.Is(err, ErrUnknownVerb) {
		t.Fatalf("Do(kill) = %v", err)
	}
	if _, err := m.Do(context.Background(), Request{
		Verb: VerbStart, Instance: "../../etc/passwd", Actor: audit.RootCLI(),
	}); err == nil {
		t.Fatal("a path traversal passed as an instance name")
	}
	if len(h.reqs) != 0 {
		t.Fatalf("a refused request reached the host: %v", h.verbs())
	}
}

func TestFormatHoldRecordsWhoAndWhyAndStripsControlCharacters(t *testing.T) {
	// The reason is operator-typed and reaches a file. A newline in it would
	// make the second line look like a second record to whatever eventually
	// parses this.
	got := string(FormatHold(at("2026-09-04T19:00:00Z"), audit.User("u-1", "web"),
		"disk swap\nrm -rf /\x1b[31m"))
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("the hold is not one line: %q", got)
	}
	for _, want := range []string{"2026-09-04T19:00:00Z", "user:u-1", "disk swap", "rm -rf /"} {
		if !strings.Contains(got, want) {
			t.Errorf("hold %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("an escape sequence reached a file an operator will cat: %q", got)
	}
}

func equalVerbs(a, b []hostops.Verb) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
