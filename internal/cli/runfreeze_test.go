package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/backup"
	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The tests in this file are about the ASSEMBLY, not about the decision table.
//
// internal/backup/lease_test.go already proves that a Watcher expires, sweeps
// and refuses the right leases -- every one of those tests is itself the
// honouring process. What none of them can prove is that anything in the
// shipped binary ever builds one, and "the mechanism exists and nothing calls
// it" is exactly how D-31's out-of-process half came to be inert. So these
// drive the production constructor, over a real filesystem in a temp dir, on
// the development machine.

// recordingThawer is a runner's channels, reduced to what a lease asks of them.
type recordingThawer struct {
	mu   sync.Mutex
	got  []backup.ThawStep
	fail error
}

func (t *recordingThawer) Thaw(_ context.Context, step backup.ThawStep) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fail != nil {
		return t.fail
	}
	t.got = append(t.got, step)
	return nil
}

func (t *recordingThawer) steps() []backup.ThawStep {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]backup.ThawStep(nil), t.got...)
}

// writeLease puts a lease document on disk the way backup.Arm does.
func writeLease(t *testing.T, path string, l backup.Lease) {
	t.Helper()
	l.Schema = 1
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o640); err != nil {
		t.Fatal(err)
	}
}

func pzManifest(t *testing.T) (*spec.Manifest, *spec.Renderer) {
	t.Helper()
	raw, err := fs.ReadFile(games.FS, "projectzomboid.yaml")
	if err != nil {
		t.Fatalf("read projectzomboid.yaml: %v", err)
	}
	m, diags, err := spec.ValidateBytes("games/projectzomboid.yaml", raw)
	if err != nil {
		t.Fatalf("projectzomboid.yaml: %v\n%s", err, diags)
	}
	return m, spec.NewRenderer(spec.RenderData{})
}

func quietLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func fmtSprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

// notReadyChannel is a control channel that exists and refuses, which is
// Project Zomboid's first forty minutes.
type notReadyChannel struct{ why string }

func (c *notReadyChannel) Kind() control.Kind { return spec.ControlSourceRCON }
func (c *notReadyChannel) Ready() bool        { return false }
func (c *notReadyChannel) Unready() string    { return c.why }
func (c *notReadyChannel) Exec(context.Context, string) (string, error) {
	return "", errors.New("a not-ready channel was asked to exec")
}
func (c *notReadyChannel) Close() error { return nil }

// TestTheRunnerSweepsAnExpiredFreezeLeaseOnStart.
//
// The SIGKILL case D-31 is built around, driven through the real constructor.
// A backup armed a lease, turned persistence off and died; systemd restarted
// the runner; RuntimeDirectoryPreserve=restart kept /run/gvm/<name>, so the
// lease is sitting there with nobody watching it. The runner's first act has
// to be to look.
func TestTheRunnerSweepsAnExpiredFreezeLeaseOnStart(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), "freeze.lease")
	armed := time.Date(2026, 9, 4, 4, 0, 0, 0, time.UTC)
	writeLease(t, leasePath, backup.Lease{
		Instance:  "pz-main",
		Reason:    "backup:routine",
		OwnerPID:  48812,
		OwnerUnit: "gvm-job@pz-main_nightly.service",
		ArmedAt:   armed,
		Deadline:  armed.Add(2 * time.Minute),
		ThawSteps: []backup.ThawStep{{Via: backup.ViaControl, Send: "save-on"}},
	})

	th := &recordingThawer{}
	m, rend := pzManifest(t)
	w := freezeWatcher(leasePath, backup.OS, m, rend, th,
		func() time.Time { return armed.Add(time.Hour) }, quietLog())

	tick := w.Sweep(context.Background())
	if tick.Decision != backup.DecisionThaw || !tick.Thawed {
		t.Fatalf("Sweep returned %+v; an expired lease found at runner start must be thawed", tick)
	}
	got := th.steps()
	if len(got) != 1 || got[0].Send != "save-on" || got[0].Via != backup.ViaControl {
		t.Fatalf("the runner delivered %+v, want one control save-on", got)
	}
	if _, err := os.Stat(leasePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lease survived a completed thaw (%v); it would thaw again every tick", err)
	}
}

// TestTheFreezeWatchIsStartedAndNarratesTheThawToTheConsole.
//
// The goroutine half, which is the one `supervise` wires. Two claims: the
// watch acts without anybody calling Step by hand, and the thaw reaches the
// console ring -- an operator watching `gvm console` after a backup died
// deserves the line that explains why saving came back on, and the lease file
// is otherwise the only trace.
func TestTheFreezeWatchIsStartedAndNarratesTheThawToTheConsole(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), "freeze.lease")
	armed := time.Date(2026, 9, 4, 4, 0, 0, 0, time.UTC)
	writeLease(t, leasePath, backup.Lease{
		Instance:  "pz-main",
		ArmedAt:   armed,
		Deadline:  armed.Add(2 * time.Minute),
		ThawSteps: []backup.ThawStep{{Via: backup.ViaConsole, Send: "save-on"}},
	})

	th := &recordingThawer{}
	m, rend := pzManifest(t)
	w := freezeWatcher(leasePath, backup.OS, m, rend, th,
		func() time.Time { return armed.Add(time.Hour) }, quietLog())
	w.Interval = time.Millisecond

	var mu sync.Mutex
	var notes []string
	done := make(chan struct{})
	stop := startFreezeWatch(context.Background(), w, func(f string, a ...any) uint64 {
		mu.Lock()
		notes = append(notes, fmtSprintf(f, a...))
		n := len(notes)
		mu.Unlock()
		if n == 1 {
			close(done)
		}
		return uint64(n)
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		stop()
		t.Fatal("the freeze watch never reported a tick, so nothing is honouring the lease")
	}
	stop()

	if got := th.steps(); len(got) != 1 || got[0].Send != "save-on" {
		t.Fatalf("the watch delivered %+v, want one save-on", got)
	}
	mu.Lock()
	first := notes[0]
	mu.Unlock()
	if !strings.Contains(first, "re-enabled saving") {
		t.Errorf("the console was not told what happened: %q", first)
	}
}

// TestAFreezeWatchStopWaitsForItsGoroutine.
//
// `supervise` tears the control channel and the pty down after this returns,
// so a stop that only cancelled would let a thaw still on the wire write to a
// closed channel. The defer is registered after the teardown closure precisely
// so LIFO runs it first, and that ordering is only worth anything if the stop
// actually waits.
func TestAFreezeWatchStopWaitsForItsGoroutine(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), "freeze.lease")
	w := freezeWatcher(leasePath, backup.OS, nil, nil, &recordingThawer{}, time.Now, quietLog())
	w.Interval = time.Hour

	entered := make(chan struct{}, 1)
	released := make(chan struct{})
	w.Thaw = backup.ThawerFunc(func(context.Context, backup.ThawStep) error {
		entered <- struct{}{}
		<-released
		return nil
	})
	writeLease(t, leasePath, backup.Lease{
		Instance:  "pz-main",
		ArmedAt:   time.Now().Add(-3 * time.Minute),
		Deadline:  time.Now().Add(-time.Minute),
		ThawSteps: []backup.ThawStep{{Via: backup.ViaConsole, Send: "save-on"}},
	})

	stop := startFreezeWatch(context.Background(), w, nil)
	// Bounded, not a bare receive. If the watch ever stops reaching the thaw,
	// an unbounded receive here hangs the whole package until the go test
	// timeout, which reports a stack and names no cause. Failing in two
	// seconds with a sentence is strictly better than that.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the freeze watch never reached the thaw, so this test has nothing to wait on")
	}

	returned := make(chan struct{})
	go func() { defer close(returned); stop() }()
	select {
	case <-returned:
		t.Fatal("stop returned while a thaw was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(released)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("stop never returned")
	}
}

// TestTheUnreadableLeaseFallbackComesFromThisGamesManifest.
//
// A lease that exists and will not parse is treated as armed, because the
// alternative is leaving a possibly-frozen world unsaved over a truncated JSON
// document. That path has no steps to read out of the document, so the thaw
// has to come from the runner's own manifest -- and this asserts the
// constructor actually derives it rather than leaving the field nil.
func TestTheUnreadableLeaseFallbackComesFromThisGamesManifest(t *testing.T) {
	m, rend := pzManifest(t)
	// Project Zomboid ships `after: none`, so give the manifest the shape the
	// mechanism exists for: a game whose freeze has a mode to toggle back.
	m.Spec.Backup.Freeze.After = &spec.ShutdownStep{Via: spec.ViaControl, Send: "save-on"}

	dir := t.TempDir()
	leasePath := filepath.Join(dir, "freeze.lease")
	if err := os.WriteFile(leasePath, []byte("{\"schema\":1,\"dead"), 0o640); err != nil {
		t.Fatal(err)
	}

	th := &recordingThawer{}
	w := freezeWatcher(leasePath, backup.OS, m, rend, th, time.Now, quietLog())
	if len(w.Fallback) != 1 || w.Fallback[0].Send != "save-on" {
		t.Fatalf("fallback = %+v, want this game's own freeze.after", w.Fallback)
	}
	tick := w.Step(context.Background())
	if tick.Decision != backup.DecisionUnreadable || !tick.Thawed {
		t.Fatalf("Step returned %+v on a truncated lease", tick)
	}
	if got := th.steps(); len(got) != 1 || got[0].Send != "save-on" {
		t.Fatalf("the runner delivered %+v from the fallback", got)
	}
}

// TestALeaseThawRefusesRatherThanClaimingAChannelItDoesNotHave.
//
// freeze.go's rule, at the one place the runner can break it: the thaw must
// never report success for a command it could not deliver. A game with no RCON
// and a lease that names `via: control` is a refusal, which leaves the lease
// on disk as the evidence lifecycle's freeze_armed finding surfaces.
func TestALeaseThawRefusesRatherThanClaimingAChannelItDoesNotHave(t *testing.T) {
	for _, tc := range []struct {
		name string
		th   leaseThawer
		step backup.ThawStep
		want string
	}{
		{"no control channel", leaseThawer{},
			backup.ThawStep{Via: backup.ViaControl, Send: "save-on"}, "no control channel"},
		{"the control channel is not ready",
			leaseThawer{control: &notReadyChannel{why: "the game is still booting"}},
			backup.ThawStep{Via: backup.ViaControl, Send: "save-on"}, "not ready"},
		{"no console sink", leaseThawer{},
			backup.ThawStep{Via: backup.ViaConsole, Send: "save-on"}, "no console sink"},
		{"an unknown channel", leaseThawer{},
			backup.ThawStep{Via: backup.Via("carrier pigeon"), Send: "save-on"}, "unknown channel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.th.Thaw(context.Background(), tc.step)
			if err == nil {
				t.Fatal("the thaw reported success for a command it could not deliver")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
