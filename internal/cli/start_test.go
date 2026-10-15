package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// applied brings an instance all the way to "gvm apply has run", which is the
// state every lifecycle verb assumes.
func applied(t *testing.T, h *testHost, name string) {
	t.Helper()
	seedInstance(t, h, name, "valheim", "--var", "public_name=Test")
	if code, out, errb := runWith(t, h, "", "apply", name); code != 0 {
		t.Fatalf("apply %s: exit %d\n%s\n%s", name, code, out, errb)
	}
	// /run/gvm/<name> is created by the unit's RuntimeDirectory= at start,
	// and the hold file lives inside it.
	h.FS.mkdirAll(instance.PathsFor(name).Run)
	// systemd now knows the unit. The fake manager does not synthesise this
	// from the drop-in write, so the test does what the real one would; a
	// LoadState of not-found is what an unapplied instance looks like, and
	// every lifecycle verb is refused against it by design.
	h.Systemd.states[instance.UnitName(name)] = platform.UnitState{
		Name: instance.UnitName(name), LoadState: "loaded", ActiveState: "inactive",
	}
	installSentinel(t, h, name)
	// Everything above is setup; what a test asserts about is what comes next.
	h.Systemd.calls = nil
}

// installSentinel puts the file the manifest declares as proof of an install.
//
// "Installed" is the manifest's own question and not "is the directory
// non-empty": a directory holding 40 GB of a 65 GB download is neither empty
// nor installed, and lifecycle refuses a start against the second.
func installSentinel(t *testing.T, h *testHost, name string) {
	t.Helper()
	in := mustReadInstance(t, h, name)
	h.FS.put(in.Spec.Paths.Install+"/valheim_server.x86_64", []byte("ELF"), 0o755)
}

func TestStartActuatesThroughTheUnit(t *testing.T) {
	h := newTestHost(t)
	applied(t, h, "valheim-1")

	code, out, errb := runWith(t, h, "", "start", "valheim-1", "--reason", "first boot")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errb)
	}
	want := "start " + instance.UnitName("valheim-1")
	var found bool
	for _, c := range h.Systemd.calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("systemd calls = %v, want one of %q", h.Systemd.calls, want)
	}
	// A start clears the hold FIRST, so that a start which then fails is one
	// gvm-guard.timer can still pick up. A hold left behind by a failed start
	// would suppress exactly that recovery.
	if !strings.Contains(out, "start") {
		t.Errorf("output does not report the verb:\n%s", out)
	}
}

// A stop with no way to observe the shutdown ladder is REFUSED, and the
// refusal explains itself.
//
// audit.instance_stop requires `rung` on its success row. A stop that cannot
// supply one writes a record saying a world was stopped and unable to say
// whether it was saved -- which is precisely the record the bash suite already
// produced, and the reason the seam is required rather than optional.
func TestStopWithNoLadderIsRefusedAndSaysWhy(t *testing.T) {
	h := newTestHost(t)
	applied(t, h, "valheim-1")
	h.HostEnv.Shutdown = func(context.Context, string, time.Duration) (lifecycle.Outcome, error) {
		t.Fatal("the ladder must not run in this test")
		return lifecycle.Outcome{}, nil
	}
	h.HostEnv.Shutdown = nil // and now there is none at all

	// The default seam is the console socket, which is not reachable from a
	// test; what is asserted here is the shape of the refusal when a caller
	// wires nothing, which is what lifecycle.Manager enforces.
	mgr := &lifecycle.Manager{Obs: observerFor(h), Host: h.Ops}
	_, err := mgr.Do(t.Context(), lifecycle.Request{
		Verb: lifecycle.VerbStop, Instance: "valheim-1",
	})
	if err == nil {
		t.Fatal("a stop with no ladder succeeded")
	}
	got := lifecycleError(lifecycle.VerbStop, lifecycleOptions{Name: "valheim-1"}, err)
	if !strings.Contains(got.Error(), "whether the world was saved") {
		t.Errorf("the refusal does not explain itself:\n%v", got)
	}
	if len(h.Systemd.calls) != 0 {
		t.Errorf("a refused stop still talked to systemd: %v", h.Systemd.calls)
	}
}

// A stop reports the RUNG the ladder ended on, and says DEGRADED when a
// SIGKILL was needed. That difference -- saved at 40 s versus killed at 180 s
// -- was byte-identical in `systemctl status` under the bash suite.
func TestStopReportsTheRung(t *testing.T) {
	h := newTestHost(t)
	applied(t, h, "valheim-1")
	h.HostEnv.Shutdown = func(context.Context, string, time.Duration) (lifecycle.Outcome, error) {
		return lifecycle.Outcome{Rung: "sigkill", Degraded: true, Took: 180 * time.Second}, nil
	}

	code, out, errb := runWith(t, h, "", "stop", "valheim-1", "--reason", "maintenance")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errb)
	}
	if !strings.Contains(out, "ended on sigkill") {
		t.Errorf("the rung is not reported:\n%s", out)
	}
	if !strings.Contains(out, "DEGRADED") {
		t.Errorf("a SIGKILLed world was not flagged degraded:\n%s", out)
	}
	// The hold is what stops gvm-guard.timer undoing the stop five minutes
	// later, and it is written on BOTH sides of the systemd stop because a
	// clean stop removes the runtime directory it lives in.
	if _, err := h.FS.Stat(instance.PathsFor("valheim-1").Hold); err != nil {
		t.Errorf("no hold file after a stop: %v", err)
	}
}

// A start before the game files exist is refused, and the hint is the command
// that fixes it. Starting a server whose binaries are half-downloaded produces
// a unit that fails a second later with a message about a missing file, which
// is a worse way to learn the same thing.
func TestStartBeforeInstallIsRefusedWithTheInstallHint(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")
	if code, out, errb := runWith(t, h, "", "apply", "valheim-1"); code != 0 {
		t.Fatalf("apply: exit %d\n%s\n%s", code, out, errb)
	}
	h.FS.mkdirAll(instance.PathsFor("valheim-1").Run)
	h.Systemd.states[instance.UnitName("valheim-1")] = platform.UnitState{
		Name: instance.UnitName("valheim-1"), LoadState: "loaded", ActiveState: "inactive",
	}
	h.Systemd.calls = nil

	code, _, errb := runWith(t, h, "", "start", "valheim-1")
	if code == 0 {
		t.Fatal("start succeeded with no game files on disk")
	}
	// It never points back at `gvm install`, which is not in this build; it says
	// so plainly instead (U10).
	if !strings.Contains(errb, "not available in this build") {
		t.Errorf("the refusal does not say installing is unavailable:\n%s", errb)
	}
	if strings.Contains(errb, "gvm install") {
		t.Errorf("the refusal points back at `gvm install`, which is not in this build:\n%s", errb)
	}
	// It is overridable, and the CLI says so once rather than leaving the
	// operator to discover --force by guessing.
	if !strings.Contains(errb, "--force") {
		t.Errorf("an overridable refusal did not mention --force:\n%s", errb)
	}
	if len(h.Systemd.calls) != 0 {
		t.Errorf("a refused start still talked to systemd: %v", h.Systemd.calls)
	}
}

// A verb against an instance with no definition is refused by the precondition
// table, not by a nil dereference somewhere downstream.
func TestALifecycleVerbOnAnAbsentInstanceIsRefused(t *testing.T) {
	h := newTestHost(t)
	for _, verb := range []string{"start", "stop", "restart"} {
		code, _, errb := runWith(t, h, "", verb, "ghost-1")
		if code == 0 {
			t.Errorf("%s succeeded for an instance that does not exist", verb)
		}
		if strings.Contains(errb, "panic") {
			t.Errorf("%s panicked: %s", verb, errb)
		}
	}
	if len(h.Systemd.calls) != 0 {
		t.Errorf("a refused verb still talked to systemd: %v", h.Systemd.calls)
	}
}
