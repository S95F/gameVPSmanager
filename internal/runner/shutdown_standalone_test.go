package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The standalone path exists for the case layers 1 and 2 cannot help with: the
// runner was SIGKILLed, so the game is orphaned in the cgroup with nobody
// waiting for it and the only thing that knows how to stop it correctly is a
// file.

// fakeStdin stands in for /proc/<pid>/fd/0.
type fakeStdin struct {
	mu     sync.Mutex
	writes []string
	closed bool
	err    error
}

func (f *fakeStdin) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.writes = append(f.writes, string(p))
	return len(p), nil
}

func (f *fakeStdin) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeStdin) written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

func standalonePlan(t *testing.T, dir string, steps []PlanStep) string {
	t.Helper()
	p := filepath.Join(dir, "shutdown.json")
	err := WritePlan(p, Plan{
		Instance:     "pz-main",
		Game:         "projectzomboid",
		Unit:         "gvm@pz-main.service",
		Process:      Identity{PID: 41799, PGID: 41799, StartTicks: 8391244},
		ConsoleFD:    "/proc/41799/fd/0",
		LineEnding:   "\r",
		Steps:        steps,
		ShutdownLog:  filepath.Join(dir, "shutdown.jsonl"),
		LastShutdown: filepath.Join(dir, "last-shutdown.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunShutdownDrivesTheGameStdin. `via: console` degrades here to writing
// into the game's own stdin: no ring, so no echo and no wait_for_log. Honest,
// and better than nothing.
func TestRunShutdownDrivesTheGameStdin(t *testing.T) {
	dir := t.TempDir()
	p := standalonePlan(t, dir, []PlanStep{
		{Via: spec.ViaConsole, Send: "save", WaitForLog: "(?i)saved", Wait: MillisOf(30 * time.Second)},
		{Via: spec.ViaConsole, Send: "quit"},
	})
	proc := newFakeProcess()
	proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }
	stdin := &fakeStdin{}
	clock := console.NewFakeClock(time.Time{})

	done := make(chan *Report, 1)
	errs := make(chan error, 1)
	go func() {
		rep, err := RunShutdown(context.Background(), StandaloneOptions{
			PlanPath: p, Actor: "root_cli", Reason: "operator",
			Clock: clock, Log: slog.New(slog.DiscardHandler),
			Adopt:       func(Identity) (Process, error) { return proc, nil },
			OpenConsole: func(string) (io.WriteCloser, error) { return stdin, nil },
		})
		errs <- err
		done <- rep
	}()

	settle(t, "the save command", func() bool { return len(stdin.written()) >= 1 })
	settle(t, "the ladder to arm its wait", func() bool { return clock.Timers() >= 1 })
	clock.Advance(30 * time.Second)

	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	rep := <-done

	// One Write per command, terminator included: two writes can interleave
	// with another writer on the same descriptor, and half a command followed
	// by another command's tail is a command nobody typed.
	want := []string{"save\r", "quit\r"}
	got := stdin.written()
	if len(got) != len(want) {
		t.Fatalf("writes = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("write %d = %q, want %q", i, got[i], want[i])
		}
	}
	if !stdin.closed {
		t.Error("the standalone path must release its copy of the descriptor")
	}
	if !rep.Standalone {
		t.Error("the report must say it came from run-shutdown; otherwise an empty " +
			"console_tail is indistinguishable from a game that printed nothing")
	}
	if len(rep.ConsoleTail) != 0 {
		t.Errorf("tail = %v, want none on a path with no ring", rep.ConsoleTail)
	}
	if rep.Steps[0].Outcome != OutcomeTimeout {
		t.Errorf("save outcome = %q, want %q with no ring to watch", rep.Steps[0].Outcome, OutcomeTimeout)
	}
	if !strings.Contains(rep.Steps[0].Note, "no console ring") {
		t.Errorf("note = %q, want it to say why the marker was not watched", rep.Steps[0].Note)
	}
	if _, err := ReadLastShutdown(filepath.Join(dir, "last-shutdown.json")); err != nil {
		t.Errorf("the standalone path must still write the report: %v", err)
	}
}

// TestRunShutdownWithNoConsoleStillSignals. Section 11.8: if the console write
// fails the step is recorded and the ladder escalates.
func TestRunShutdownWithNoConsoleStillSignals(t *testing.T) {
	dir := t.TempDir()
	p := standalonePlan(t, dir, []PlanStep{{Via: spec.ViaConsole, Send: "quit"}})
	proc := newFakeProcess()
	proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }

	rep, err := RunShutdown(context.Background(), StandaloneOptions{
		PlanPath: p, Clock: console.NewFakeClock(time.Time{}), Log: slog.New(slog.DiscardHandler),
		Adopt:       func(Identity) (Process, error) { return proc, nil },
		OpenConsole: func(string) (io.WriteCloser, error) { return nil, errors.New("permission denied") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Steps[0].Outcome != OutcomeSkipped {
		t.Errorf("console outcome = %q, want %q", rep.Steps[0].Outcome, OutcomeSkipped)
	}
	if !hasRung(rep, RungSIGTERM) {
		t.Errorf("rungs = %v, want the ladder to escalate", rungs(rep))
	}
	if rep.Actor != "run-shutdown" {
		t.Errorf("actor = %q, want the path to name itself", rep.Actor)
	}
}

// TestRunShutdownOnAStalePin. The pid exited or was recycled: nothing to stop,
// and that must be a clean answer rather than an error a caller might respond
// to by escalating.
func TestRunShutdownOnAStalePin(t *testing.T) {
	dir := t.TempDir()
	p := standalonePlan(t, dir, []PlanStep{{Via: spec.ViaConsole, Send: "quit"}})

	rep, err := RunShutdown(context.Background(), StandaloneOptions{
		PlanPath: p, Clock: console.NewFakeClock(time.Time{}),
		Adopt: func(id Identity) (Process, error) { return nil, ErrGone },
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.FinalRung != RungNone || !rep.Exited {
		t.Errorf("report = %+v, want a clean 'nothing to stop'", rep)
	}
	if _, err := ReadLastShutdown(filepath.Join(dir, "last-shutdown.json")); err != nil {
		t.Errorf("'there was nothing to stop' is an outcome gvmd should see: %v", err)
	}
}

func TestRunShutdownRefusals(t *testing.T) {
	t.Run("no plan file", func(t *testing.T) {
		_, err := RunShutdown(context.Background(), StandaloneOptions{
			PlanPath: filepath.Join(t.TempDir(), "missing.json"),
		})
		if !errors.Is(err, ErrNoPlan) {
			t.Fatalf("err = %v, want ErrNoPlan", err)
		}
	})
	t.Run("no identity pinned", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "shutdown.json")
		if err := WritePlan(p, Plan{Instance: "pz-main"}); err != nil {
			t.Fatal(err)
		}
		_, err := RunShutdown(context.Background(), StandaloneOptions{PlanPath: p})
		if !errors.Is(err, ErrNoPlan) {
			t.Fatalf("err = %v, want ErrNoPlan for a plan with no pin", err)
		}
	})
	t.Run("unknown proto", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "shutdown.json")
		if err := writeFileAtomic(p, []byte(`{"proto":99}`), 0o640); err != nil {
			t.Fatal(err)
		}
		_, err := RunShutdown(context.Background(), StandaloneOptions{PlanPath: p})
		if err == nil || !strings.Contains(err.Error(), "proto 99") {
			t.Fatalf("err = %v, want a refusal naming the generation", err)
		}
	})
}

// TestSpawnPublishesTheStandalonePlan. The file has to exist before anyone
// needs it, which means at spawn -- a plan written at stop time is a plan a
// SIGKILLed runner never wrote.
func TestSpawnPublishesTheStandalonePlan(t *testing.T) {
	h := newHarness(t)
	h.cfg.Paths.ShutdownJSON = filepath.Join(h.cfg.Paths.Run, "shutdown.json")
	h.cfg.Manifest.Spec.Shutdown.Steps = []spec.ShutdownStep{
		{Via: spec.ViaConsole, Send: "quit", Wait: spec.Duration(5 * time.Second)},
	}
	steps, err := BuildPlan(h.cfg.Manifest, h.cfg.Renderer)
	if err != nil {
		t.Fatal(err)
	}
	h.steps = steps

	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	pl, err := ReadPlan(h.cfg.Paths.ShutdownJSON)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Process != h.proc.Identity() {
		t.Errorf("pin = %+v, want %+v", pl.Process, h.proc.Identity())
	}
	if pl.ConsoleFD == "" || !strings.Contains(pl.ConsoleFD, "/fd/0") {
		t.Errorf("console fd = %q, want the game's own stdin", pl.ConsoleFD)
	}
	if len(pl.Steps) != 1 || pl.Steps[0].Send != "quit" {
		t.Errorf("steps = %+v", pl.Steps)
	}
}

// TestRunShutdownSurvivesACancelledContext. `gvm run-shutdown` is dispatched
// with main's signal.NotifyContext, and the caller that invokes it -- gvmd, or
// a human in a terminal -- may well be receiving the same SIGTERM. Handing that
// context to the ladder would abort the save on its first select, and would
// also leave watchExit unable to ever report the exit it is watching for.
func TestRunShutdownSurvivesACancelledContext(t *testing.T) {
	dir := t.TempDir()
	p := standalonePlan(t, dir, []PlanStep{
		{Via: spec.ViaConsole, Send: "save", Wait: MillisOf(30 * time.Second)},
		{Via: spec.ViaConsole, Send: "quit"},
		{Via: spec.ViaSignal, Signal: "SIGTERM", Scope: ScopeGroup.String(), Wait: MillisOf(20 * time.Second)},
	})
	proc := newFakeProcess()
	proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }
	stdin := &fakeStdin{}
	clock := console.NewFakeClock(time.Time{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan *Report, 1)
	errs := make(chan error, 1)
	go func() {
		rep, err := RunShutdown(ctx, StandaloneOptions{
			PlanPath: p, Actor: "root_cli", Reason: "operator",
			Clock: clock, Log: slog.New(slog.DiscardHandler),
			Adopt:       func(Identity) (Process, error) { return proc, nil },
			OpenConsole: func(string) (io.WriteCloser, error) { return stdin, nil },
		})
		errs <- err
		done <- rep
	}()

	settle(t, "the save command", func() bool { return len(stdin.written()) >= 1 })
	// The wait timer proves the ladder is really waiting rather than falling
	// straight through a closed Done channel.
	settle(t, "the ladder to arm its wait", func() bool { return clock.Timers() >= 1 })
	clock.Advance(30 * time.Second)

	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	rep := <-done
	if got, want := stdin.written(), []string{"save", "quit"}; len(got) != len(want) {
		t.Fatalf("writes = %q, want %q", got, want)
	}
	if rep.Err != "" {
		t.Errorf("report err = %q, want none", rep.Err)
	}
	if !rep.Exited {
		t.Error("the game was never stopped")
	}
}

// The plan file lives in /run/gvm/<name>, a directory the INSTANCE USER owns
// (RunDirMode is setgid|0750, chowned to the instance user at prepare). The
// runner that writes shutdown.json is that same uid -- the architecture is
// explicit that the runner never runs as root -- so a compromised mod can
// unlink and replace the file whatever its own mode says. `gvm run-shutdown`
// then executes it as root. Everything the plan names is therefore untrusted
// input, and the paths root writes must come from the instance name the CLI
// validated instead.
func TestRunShutdownTreatsThePlanFileAsUntrusted(t *testing.T) {
	dir := t.TempDir()
	forged := filepath.Join(dir, "forged")
	steps := []PlanStep{{Via: spec.ViaConsole, Send: "save"}}

	write := func(t *testing.T, pl Plan) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "shutdown.json")
		if err := WritePlan(p, pl); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := func() Plan {
		return Plan{
			Instance: "pz-main", Game: "projectzomboid", Unit: "gvm@pz-main.service",
			Process:   Identity{PID: 41799, PGID: 41799, StartTicks: 8391244},
			ConsoleFD: "/proc/41799/fd/0", LineEnding: "\r", Steps: steps,
			ShutdownLog:  filepath.Join(dir, "shutdown.jsonl"),
			LastShutdown: filepath.Join(dir, "last-shutdown.json"),
		}
	}
	run := func(t *testing.T, planPath string, o StandaloneOptions) (*Report, error) {
		t.Helper()
		o.PlanPath = planPath
		o.Clock = console.NewFakeClock(time.Time{})
		o.Log = slog.New(slog.DiscardHandler)
		if o.Adopt == nil {
			proc := newFakeProcess()
			proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }
			o.Adopt = func(Identity) (Process, error) { return proc, nil }
		}
		if o.OpenConsole == nil {
			o.OpenConsole = func(string) (io.WriteCloser, error) { return &fakeStdin{}, nil }
		}
		if o.ProcCgroup == nil {
			o.ProcCgroup = func(int) (string, error) {
				return "/system.slice/gvm.slice/gvm@pz-main.service", nil
			}
		}
		return RunShutdown(context.Background(), o)
	}

	t.Run("the report paths come from the caller, not the plan", func(t *testing.T) {
		pl := base()
		// AppendReport MkdirAlls and O_CREATEs whatever it is handed, so this
		// is a root-owned append to an arbitrary path.
		pl.ShutdownLog = filepath.Join(forged, "etc", "cron.d", "gvm")
		pl.LastShutdown = filepath.Join(forged, "etc", "ld.so.preload")

		log := filepath.Join(dir, "trusted.jsonl")
		last := filepath.Join(dir, "trusted-last.json")
		if _, err := run(t, write(t, pl), StandaloneOptions{
			Instance: "pz-main", ShutdownLog: log, LastShutdown: last,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(forged); err == nil {
			t.Error("the plan chose where root wrote the report")
		}
		if _, err := os.Stat(last); err != nil {
			t.Errorf("the caller's report path was not used: %v", err)
		}
	})

	t.Run("a plan for another instance is refused", func(t *testing.T) {
		pl := base()
		pl.Instance = "some-other-world"
		_, err := run(t, write(t, pl), StandaloneOptions{Instance: "pz-main"})
		if !errors.Is(err, ErrNoPlan) {
			t.Fatalf("err = %v, want ErrNoPlan", err)
		}
		if !strings.Contains(err.Error(), "some-other-world") || !strings.Contains(err.Error(), "pz-main") {
			t.Errorf("the refusal names neither side: %v", err)
		}
	})

	t.Run("the console descriptor is derived from the pin", func(t *testing.T) {
		pl := base()
		pl.ConsoleFD = "/etc/ld.so.preload"
		var opened string
		if _, err := run(t, write(t, pl), StandaloneOptions{
			Instance: "pz-main", ShutdownLog: filepath.Join(dir, "a.jsonl"),
			LastShutdown: filepath.Join(dir, "a.json"),
			OpenConsole: func(p string) (io.WriteCloser, error) {
				opened = p
				return &fakeStdin{}, nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		if want := "/proc/41799/fd/0"; opened != want {
			t.Errorf("opened %q, want %q -- the plan chose the file root wrote into", opened, want)
		}
	})

	t.Run("an arbitrary line terminator is refused", func(t *testing.T) {
		pl := base()
		// The terminator is appended verbatim after ValidateSendLine has run on
		// the line, and successive steps share one fd offset: arbitrary
		// multi-line content into the file above.
		pl.LineEnding = "\n[Unit]\nExecStart=/bin/sh\n"
		_, err := run(t, write(t, pl), StandaloneOptions{Instance: "pz-main"})
		if !errors.Is(err, ErrNoPlan) {
			t.Fatalf("err = %v, want ErrNoPlan", err)
		}
	})

	t.Run("a pin outside this instance's unit is refused", func(t *testing.T) {
		pl := base()
		_, err := run(t, write(t, pl), StandaloneOptions{
			Instance:   "pz-main",
			ProcCgroup: func(int) (string, error) { return "/system.slice/ssh.service", nil },
		})
		if !errors.Is(err, ErrNoPlan) {
			t.Fatalf("err = %v, want ErrNoPlan", err)
		}
		if !strings.Contains(err.Error(), "ssh.service") {
			t.Errorf("the refusal does not say where the pin actually is: %v", err)
		}
	})
}

func TestCheckPinnedUnit(t *testing.T) {
	const unit = "gvm@pz-main.service"
	for _, c := range []struct {
		name    string
		cgroup  string
		wantErr bool
	}{
		{"the instance's own unit", "/system.slice/gvm.slice/" + unit, false},
		{"a nested delegate of it", "/system.slice/gvm.slice/" + unit + "/game", false},
		{"another instance", "/system.slice/gvm.slice/gvm@other.service", true},
		{"sshd", "/system.slice/ssh.service", true},
		{"a name that merely ends the same way", "/system.slice/not-gvm@pz-main.service", true},
		{"unreadable", "", true},
	} {
		err := checkPinnedUnit(c.cgroup, unit)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: checkPinnedUnit(%q, %q) = %v", c.name, c.cgroup, unit, err)
		}
	}
}
