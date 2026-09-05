package runner

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Every shipped manifest's ladder has to render, and it has to render at
// CONSTRUCTION: a game whose shutdown steps cannot be built refuses to start,
// so a typo that would only surface at stop time -- on a live world, at 2am --
// is a `gvm apply` failure instead.

func shippedLadders(t *testing.T) map[string][]PlanStep {
	t.Helper()
	ents, err := fs.ReadDir(games.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	r := spec.NewRenderer(spec.RenderData{
		Name:    "inst",
		Install: "/opt/inst",
		Data:    "/home/inst/data",
		Home:    "/home/inst",
		User:    "inst",
		Phase:   spec.PhaseNormal,
	})

	out := map[string][]PlanStep{}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		raw, err := fs.ReadFile(games.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		m, diags, err := spec.ValidateBytes("games/"+name, raw)
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, diags)
		}
		steps, err := BuildPlan(m, r)
		if err != nil {
			t.Fatalf("%s: the shipped ladder does not render: %v", name, err)
		}
		out[strings.TrimSuffix(name, ".yaml")] = steps
	}
	if len(out) == 0 {
		t.Fatal("no shipped manifests were read; the walk is broken")
	}
	return out
}

func TestEveryShippedLadderRenders(t *testing.T) {
	ladders := shippedLadders(t)

	for id, steps := range ladders {
		for i, s := range steps {
			if _, err := RungFor(s.Via); err != nil {
				t.Errorf("%s step %d: %v", id, i, err)
			}
			// A signal step always names a scope, and the default is the
			// GROUP. That default is the whole lesson of section 1.2's second
			// bug: PZ's start-server.sh forks the JVM into a subshell, so a
			// signal to the pinned pid alone reaches the shell and leaves the
			// engine writing to the world.
			if s.Via == spec.ViaSignal && s.Scope == "" {
				t.Errorf("%s step %d: a signal step rendered with no scope", id, i)
			}
		}
	}

	t.Run("project zomboid saves before it quits", func(t *testing.T) {
		steps := ladders["projectzomboid"]
		if len(steps) == 0 {
			t.Fatal("no ladder")
		}
		if steps[0].Via != spec.ViaConsole || steps[0].Send != "save" {
			t.Fatalf("first rung = %+v, want a console save", steps[0])
		}
		if steps[0].WaitForLog == "" {
			t.Error("the save rung has no marker; the legacy script's blind `sleep 10` " +
				"is the worst case this is supposed to beat")
		}
		// The console, not the control channel: PZ's dialect is idle-terminated
		// and RCON is unavailable for the whole first boot, so the stop path
		// uses the channel that always works (D-08).
		var sawQuit bool
		for _, s := range steps {
			if s.Via == spec.ViaConsole && s.Send == "quit" {
				sawQuit = true
			}
		}
		if !sawQuit {
			t.Errorf("ladder = %+v, want a console quit", steps)
		}
	})

	t.Run("valheim saves on SIGINT and nothing else", func(t *testing.T) {
		steps := ladders["valheim"]
		// SIGTERM does not save and there is no console `stop` command. This
		// one signal is the entire ladder, which is why the signal is manifest
		// data rather than a constant.
		if len(steps) != 1 || steps[0].Via != spec.ViaSignal || steps[0].Signal != "SIGINT" {
			t.Fatalf("ladder = %+v, want exactly one SIGINT rung", steps)
		}
		if steps[0].Scope != ScopeGroup.String() {
			t.Errorf("scope = %q, want %q", steps[0].Scope, ScopeGroup)
		}
	})

	t.Run("7 days to die follows each control rung with a console one", func(t *testing.T) {
		steps := ladders["7daystodie"]
		// Its control kind is telnet, which this build does not implement, so
		// every control rung is recorded as skipped -- and the console rung
		// behind it is what actually stops the server. The pairing only works
		// because a skipped control rung does not escalate.
		var pairs int
		for i := 0; i+1 < len(steps); i++ {
			if steps[i].Via == spec.ViaControl && steps[i+1].Via == spec.ViaConsole &&
				steps[i].Send == steps[i+1].Send {
				pairs++
			}
		}
		if pairs < 2 {
			t.Errorf("ladder = %+v, want the control/console pairs the manifest declares", steps)
		}
	})
}

// TestEveryShippedLadderFitsItsBudget. `budget` becomes TimeoutStopSec, and a
// ladder whose declared waits do not leave room for the implicit rungs gets
// SIGKILLed by systemd mid-save -- at exactly the moment it was trying to
// prevent that.
func TestEveryShippedLadderFitsItsBudget(t *testing.T) {
	ents, err := fs.ReadDir(games.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		raw, err := fs.ReadFile(games.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		m, _, err := spec.ValidateBytes("games/"+name, raw)
		if err != nil {
			t.Fatal(err)
		}
		budget := m.Spec.Shutdown.Budget.Duration()
		if budget <= 0 {
			t.Errorf("%s: no shutdown budget", name)
			continue
		}
		var declared Millis
		for _, s := range m.Spec.Shutdown.Steps {
			declared += MillisOf(s.Wait.Duration())
		}
		// Enough room, in the worst case where every declared wait is spent, for
		// the implicit SIGKILL to be delivered AND for the record to be written
		// before systemd's own TimeoutStopSec ends the runner.
		if want := implicitKillWait + reportReserve; budget-declared.Duration() < want {
			t.Errorf("%s: the declared steps wait %s of a %s budget, leaving %s for the "+
				"implicit signal rungs and the report (want at least %s)",
				name, declared, m.Spec.Shutdown.Budget, budget-declared.Duration(), want)
		}
	}
}
