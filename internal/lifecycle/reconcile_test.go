package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// driftedHost is a box with one of everything wrong on it, and each wrong
// thing is one an over-eager reconciler would want to fix.
//
//	valheim-1  applied, enabled, and DOWN            -- "just start it"
//	pz-main    running with a spec edited since apply -- "just re-apply it"
//	ck-1       a stale update lease from a dead job   -- "just resume it"
//	sdtd-1     declared and never applied             -- "just apply it"
func driftedHost(t *testing.T) (*fakeFS, *fakeSystemd) {
	t.Helper()
	f, _ := fullHost(t)
	sd := newSystemd()

	// valheim-1: enabled, applied, inactive. The guard timer's case.
	sd.set(instance.UnitName("valheim-1"), platform.UnitState{
		LoadState: "loaded", ActiveState: "inactive", Result: "success",
	})

	// pz-main: running, and the YAML has been edited since the last apply.
	pz := instance.PathsFor("pz-main")
	f.put(pz.Spec, "apiVersion: gvm/v1\nkind: Instance\nmetadata: {name: pz-main}\n"+
		"spec:\n  game: projectzomboid\n  enabled: true\n  user: pzserver\n"+
		"  manifest: {"+pinLine(t, f, "pz-main", "projectzomboid")+"}\n"+
		"  paths: {install: /opt/pzserver, data: /home/pzserver/Zomboid, home: /home/pzserver, work: /home/pzserver}\n")
	f.put(instance.DropInPath("pz-main"), generatedMarker+"\n"+
		"# spec sha256 0000000000000000000000000000000000000000000000000000000000000000\n"+
		"# manifest    projectzomboid rev 7 sha256:1b40c9de (origin: shipped)\n[Service]\n")
	f.putAt(pz.StatusJSON, `{"proto":1,"instance":"pz-main","state":"ready"}`,
		at("2026-09-04T18:59:58Z"))
	sd.set(instance.UnitName("pz-main"), platform.UnitState{
		LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 41799,
	})

	// ck-1: a stale update lease. The one an auto-resuming reconciler would
	// pick up and finish, against an install tree a dead job may have had
	// open.
	ck := instance.PathsFor("ck-1")
	f.put(ck.Spec, "apiVersion: gvm/v1\nkind: Instance\nmetadata: {name: ck-1}\n"+
		"spec:\n  game: corekeeper\n  enabled: true\n  user: gvm-ck-1\n"+
		"  paths: {install: /srv/gvm/ck-1/install, data: /srv/gvm/ck-1/data, home: /srv/gvm/ck-1, work: /srv/gvm/ck-1}\n")
	f.put(instance.DropInPath("ck-1"), generatedMarker+"\n# spec sha256 abcd\n[Service]\n")
	lease := Lease{
		Instance: "ck-1", Kind: JobUpdate, PID: 31337,
		BootID: "an-older-boot", StartedAt: at("2026-09-04T02:00:00Z"),
	}
	body, err := lease.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	f.put(ck.Lease, string(body))
	sd.set(instance.UnitName("ck-1"), platform.UnitState{
		LoadState: "loaded", ActiveState: "inactive", Result: "success",
	})

	// sdtd-1: declared, never applied.
	f.put(instance.PathsFor("sdtd-1").Spec,
		"apiVersion: gvm/v1\nkind: Instance\nmetadata: {name: sdtd-1}\n"+
			"spec:\n  game: 7daystodie\n  enabled: true\n  user: gvm-sdtd-1\n"+
			"  paths: {install: /srv/gvm/sdtd-1/install, data: /srv/gvm/sdtd-1/data, home: /srv/gvm/sdtd-1, work: /srv/gvm/sdtd-1}\n")

	return f, sd
}

// pinLine writes an instance's pinned manifest from the SHIPPED one and
// returns the `revision:/sha256:/origin:` fragment its YAML must carry.
//
// Reading the real manifest rather than hardcoding a digest means the fixture
// cannot rot when games/<id>.yaml is edited: it would fail with "the pin does
// not verify", which is a true statement about a hand-written digest and not
// about the code under test.
func pinLine(t *testing.T, f *fakeFS, name, game string) string {
	t.Helper()
	raw, err := os.ReadFile("../../games/" + game + ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := spec.ValidateBytes("games/"+game+".yaml", raw)
	if err != nil {
		t.Fatal(err)
	}
	f.put(instance.PathsFor(name).ManifestLock, string(raw))
	return fmt.Sprintf("revision: %d, sha256: %s, origin: shipped", m.Metadata.Revision, m.SHA256)
}

func driftedReconciler(t *testing.T) (*Reconciler, *fakeSystemd) {
	t.Helper()
	f, sd := driftedHost(t)
	return &Reconciler{
		Obs: &Observer{
			FS: f, Units: sd, BootID: "5f1c0b3e-7a24-4b8e-9c31-2d8e6a4f0b17",
			Alive: aliveFunc(),
			Now:   func() time.Time { return at("2026-09-04T19:00:00Z") },
		},
		Log: slog.New(slog.DiscardHandler),
	}, sd
}

// TestReconcilerNeverActuates is the rule this package is arranged around.
//
// The fake implements the WHOLE of platform.Systemd -- start, stop, restart,
// enable, disable, daemon-reload -- and counts every call. The sweep runs over
// a host with four different kinds of drift on it, every one of which an
// over-eager reconciler would want to fix, and the count is zero.
//
// A reconciler that healed by itself would fight the operator who stopped an
// instance to repair a world by hand; would restart a server mid-rebuild
// because it briefly looked down; and would resume the update job whose owner
// died, which is how two writers end up in one install tree. Healing is
// deliberate, it belongs to `gvm-guard.timer`, and it leaves a job_runs row
// saying what it did.
func TestReconcilerNeverActuates(t *testing.T) {
	r, sd := driftedReconciler(t)
	rep, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Instances) != 4 {
		t.Fatalf("swept %d instances, want 4", len(rep.Instances))
	}
	// The drift has to actually be there, or zero actuations proves nothing.
	if len(rep.Findings) == 0 {
		t.Fatal("the sweep found no drift; a sweep over a healthy host would pass this test trivially")
	}
	if n := sd.Actuations(); n != 0 {
		t.Fatalf("the sweep made %d mutating systemd calls: starts=%v stops=%v restarts=%v "+
			"enables=%v disables=%v reloads=%d",
			n, sd.Starts, sd.Stops, sd.Restarts, sd.Enables, sd.Disables, sd.Reloads)
	}
	if sd.States == 0 {
		t.Fatal("the sweep never asked systemd anything, so it cannot have observed the unit state")
	}
}

// TestTheSweepSeesTheDriftItRefusesToFix. The companion assertion: zero
// actuations is only meaningful if the sweep NOTICED. Each row is a condition
// somebody has to be told about.
func TestTheSweepSeesTheDriftItRefusesToFix(t *testing.T) {
	r, _ := driftedReconciler(t)
	rep, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		phase Phase
		kind  FindingKind
	}{
		"valheim-1": {PhaseStopped, FindingNotRunning},
		"pz-main":   {PhaseRunning, FindingSpecDrift},
		"ck-1":      {PhaseFailed, FindingStaleLease},
		"sdtd-1":    {PhaseDeclared, FindingNotApplied},
	}
	for _, ir := range rep.Instances {
		w, ok := want[ir.Name]
		if !ok {
			t.Errorf("unexpected instance %q in the sweep", ir.Name)
			continue
		}
		if ir.Verdict.Phase != w.phase {
			t.Errorf("%s: phase %s, want %s (%s)", ir.Name, ir.Verdict.Phase, w.phase, ir.Verdict.Reason)
		}
		var found bool
		for _, f := range ir.Findings {
			if f.Kind == w.kind {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no %s finding; got %v", ir.Name, w.kind, kinds(ir.Findings))
		}
		delete(want, ir.Name)
	}
	for name := range want {
		t.Errorf("%s was never swept", name)
	}
}

func kinds(fs []Finding) []FindingKind {
	out := make([]FindingKind, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Kind)
	}
	return out
}

// failingMirror is a database that is down.
type failingMirror struct {
	calls int
	err   error
}

func (m *failingMirror) RecordPhase(context.Context, InstanceReport) error {
	m.calls++
	return m.err
}

// TestASweepSurvivesTheDatabaseBeingDown is section 1's promise at the one
// place a sweep touches Postgres at all.
//
// The list comes from /etc/gvm and the mirror is a copy. A mirror write that
// fails must produce a warning, a Problem, and a complete sweep -- not a short
// one, and certainly not an error. A dashboard that showed nothing because the
// database was down would be reporting on the database, not on the servers.
func TestASweepSurvivesTheDatabaseBeingDown(t *testing.T) {
	r, _ := driftedReconciler(t)
	m := &failingMirror{err: errors.New("dial unix /var/run/postgresql/.s.PGSQL.5432: connect: connection refused")}
	r.Store = m

	rep, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("a mirror failure aborted the sweep: %v", err)
	}
	if len(rep.Instances) != 4 {
		t.Fatalf("swept %d instances with the database down, want 4", len(rep.Instances))
	}
	if m.calls != 4 {
		t.Fatalf("the mirror was called %d times; the sweep stopped trying after a failure", m.calls)
	}
	var mirrored int
	for _, p := range rep.Problems {
		if p.Source == "mirror" {
			mirrored++
		}
	}
	if mirrored != 4 {
		t.Fatalf("%d mirror problems recorded, want 4: %+v", mirrored, rep.Problems)
	}
}

// TestASweepThatCannotEnumerateIsAnError.
//
// The one failure that is NOT tolerated. A sweep that found nothing and a
// sweep that could not look produce the same empty report otherwise, and the
// second must never be read as a clean bill of health for a host with eleven
// worlds on it.
// TestAMirrorFailureIsWarnedAbout. The Problem is for the report; the warning
// is for journald, which is the durable copy on a box whose database is the
// thing that has fallen over.
func TestAMirrorFailureIsWarnedAbout(t *testing.T) {
	r, _ := driftedReconciler(t)
	var buf bytes.Buffer
	r.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r.Store = &failingMirror{err: errors.New("connection refused")}

	if _, err := r.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("a failed mirror write logged nothing at warning level:\n%s", out)
	}
	for _, want := range []string{"instance=", "connection refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning does not carry %q:\n%s", want, out)
		}
	}
}

// TestFindingsDoNotPileOnOneInstance. Every excluded combination has a better
// sentence already being printed, and a finding list an operator skims because
// it always has four lines per instance is a finding list nobody reads.
func TestFindingsDoNotPileOnOneInstance(t *testing.T) {
	r, _ := driftedReconciler(t)
	rep, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ir := range rep.Instances {
		got := kinds(ir.Findings)
		switch ir.Name {
		case "sdtd-1":
			// Never applied. There is no unit for it to be down.
			if slices.Contains(got, FindingNotRunning) {
				t.Errorf("sdtd-1 was never applied and is reported as not running: %v", got)
			}
		case "ck-1":
			// A job died holding it. The lease is the sentence that matters.
			if slices.Contains(got, FindingNotRunning) {
				t.Errorf("ck-1 has a stale lease and is also reported as not running: %v", got)
			}
		}
	}
}

func TestASweepThatCannotEnumerateIsAnError(t *testing.T) {
	f := newFS()
	f.fail("/etc/gvm/instances", fs.ErrPermission)
	r := &Reconciler{Obs: &Observer{FS: f}, Log: slog.New(slog.DiscardHandler)}
	if _, err := r.Sweep(context.Background()); err == nil {
		t.Fatal("an unreadable /etc/gvm/instances reported a clean sweep")
	}
}

func TestSweepCountsBySeverity(t *testing.T) {
	r, _ := driftedReconciler(t)
	rep, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ok, notice, problem := rep.Counts()
	if ok != 1 || problem != 1 || notice != 2 {
		t.Fatalf("counts ok=%d notice=%d problem=%d; want 1 running, 1 failed, 2 in between",
			ok, notice, problem)
	}
}

// TestWatchSweepsOnEveryTickAndStopsWithTheContext.
//
// The tick is a parameter so this test never sleeps. A watcher built around a
// real ticker is a test that takes as long as its interval and then flakes on
// a loaded machine.
func TestWatchSweepsOnEveryTickAndStopsWithTheContext(t *testing.T) {
	r, _ := driftedReconciler(t)
	tick := make(chan time.Time, 3)
	ctx, cancel := context.WithCancel(context.Background())

	var got int
	done := make(chan error, 1)
	go func() {
		done <- r.Watch(ctx, tick, func(SweepReport) {
			got++
			if got == 3 {
				cancel()
			}
		})
	}()
	tick <- time.Now()
	tick <- time.Now()
	tick <- time.Now()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch returned %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Watch did not return after its context was cancelled")
	}
	if got != 3 {
		t.Fatalf("emitted %d reports for 3 ticks", got)
	}
}

// TestWatchKeepsGoingAfterAFailedSweep. /etc/gvm is momentarily unreadable
// during a package upgrade. A watcher that exited there would have to be
// noticed by a human before anything was observed again.
func TestWatchKeepsGoingAfterAFailedSweep(t *testing.T) {
	f := newFS()
	f.fail("/etc/gvm/instances", fs.ErrPermission)
	r := &Reconciler{Obs: &Observer{FS: f}, Log: slog.New(slog.DiscardHandler)}

	tick := make(chan time.Time, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Watch(ctx, tick, nil) }()

	tick <- time.Now()
	tick <- time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch returned %v after two failed sweeps, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Watch exited on a failed sweep instead of continuing")
	}
}

// TestFindingsAreDerivedFromTheSameFactsAsThePhase. Findings and Derive are
// separate functions over one Observation; if they ever disagreed, `gvm
// status` and `gvm doctor` would tell an operator two different stories about
// one server.
func TestFindingsAreDerivedFromTheSameFactsAsThePhase(t *testing.T) {
	for _, tc := range deriveCases() {
		o := base()
		tc.setup(&o)
		v := Derive(o)
		fs := Findings(o, v)
		if v.Phase.Severity() != SevProblem {
			continue
		}
		var found bool
		for _, f := range fs {
			if f.Kind == FindingPhase && strings.Contains(f.Detail, string(v.Phase)) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: phase %s is a problem and Findings does not report it", tc.name, v.Phase)
		}
	}
}
