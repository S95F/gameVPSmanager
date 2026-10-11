package hook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// scripted is a hook whose behaviour a test chooses.
//
// It is inserted into the private registry for the duration of one test and
// removed afterwards. White-box, deliberately: the registry is closed to every
// other PACKAGE -- there is no Register function and no way to add a name from
// outside -- and the phase-ordering rules still need hooks whose failure a test
// can cause on purpose, because no shipped hook declares post_start and none of
// them can be made to fail on command.
type scripted struct {
	name   string
	phases []Phase
	run    func(h *Ctx) error
}

func (s scripted) Name() string    { return s.name }
func (s scripted) Phases() []Phase { return s.phases }
func (s scripted) Doc() string     { return "a test hook" }

func (s scripted) Run(_ context.Context, h *Ctx) error {
	if s.run == nil {
		return nil
	}
	return s.run(h)
}

func install(t *testing.T, hooks ...scripted) {
	t.Helper()
	for _, h := range hooks {
		if _, taken := registry[h.name]; taken {
			t.Fatalf("%q is a real hook; pick another name for a test double", h.name)
		}
		registry[h.name] = h
		name := h.name
		t.Cleanup(func() { delete(registry, name) })
	}
}

func TestRunExecutesInListOrder(t *testing.T) {
	var order []string
	install(t,
		scripted{"test.a", []Phase{PhasePostInstall}, func(h *Ctx) error {
			order = append(order, "a")
			h.Note("a ran")
			return nil
		}},
		scripted{"test.b", []Phase{PhasePostInstall}, func(h *Ctx) error {
			order = append(order, "b")
			h.Problem("b found something")
			return nil
		}},
	)

	h := &Ctx{Phase: PhasePostInstall}
	// Reversed in the list, so "list order" is distinguishable from
	// "alphabetical" and from "registry order".
	res, err := Run(t.Context(), h, []string{"test.b", "test.a"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Join(order, "") != "ba" {
		t.Errorf("ran %v, want the order the manifest lists", order)
	}
	if len(res.Ran) != 2 || res.Ran[0].Hook != "test.b" {
		t.Fatalf("Ran = %+v", res.Ran)
	}
	// Notes and problems are attributed per hook, so a doctor line can say
	// which hook reported what.
	if len(res.Ran[0].Problems) != 1 || len(res.Ran[1].Notes) != 1 {
		t.Errorf("findings were not attributed per hook: %+v", res.Ran)
	}
	if got := res.Problems(); len(got) != 1 || !strings.Contains(got[0], "b found") {
		t.Errorf("Problems() = %v", got)
	}
	if res.Degraded() {
		t.Error("a clean post_install run reported a degraded hook")
	}
}

// TestRunAbortsThePhaseOnTheFirstError, everywhere except post_start. A
// post_install hook that failed and then let the next one run would leave an
// install half-configured with no record of where it stopped.
func TestRunAbortsThePhaseOnTheFirstError(t *testing.T) {
	ran := 0
	install(t,
		scripted{"test.boom", []Phase{PhasePostInstall}, func(*Ctx) error {
			ran++
			return errors.New("the install tree is not there")
		}},
		scripted{"test.after", []Phase{PhasePostInstall}, func(*Ctx) error {
			ran++
			return nil
		}},
	)

	res, err := Run(t.Context(), &Ctx{Phase: PhasePostInstall},
		[]string{"test.boom", "test.after"})
	if err == nil {
		t.Fatal("Run returned no error")
	}
	if ran != 1 {
		t.Errorf("%d hooks ran; the phase must stop at the first failure", ran)
	}
	// The error names the hook and the phase, because a job log line saying
	// "the install tree is not there" with neither is unactionable.
	for _, want := range []string{"test.boom", "post_install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	if len(res.Ran) != 1 || res.Ran[0].Err == nil {
		t.Errorf("the result does not record the failure: %+v", res.Ran)
	}
}

// TestPostStartDegradesRatherThanFailing: the server is already up and serving
// players. Failing the job would either stop a running game or raise an alert
// nobody can act on, so the failure is recorded and surfaced in doctor.
func TestPostStartDegradesRatherThanFailing(t *testing.T) {
	ran := 0
	install(t,
		scripted{"test.late", []Phase{PhasePostStart}, func(*Ctx) error {
			ran++
			return errors.New("the metrics endpoint refused the connection")
		}},
		scripted{"test.later", []Phase{PhasePostStart}, func(*Ctx) error {
			ran++
			return nil
		}},
	)

	res, err := Run(t.Context(), &Ctx{Phase: PhasePostStart},
		[]string{"test.late", "test.later"})
	if err != nil {
		t.Fatalf("a post_start failure failed the phase: %v", err)
	}
	if ran != 2 {
		t.Errorf("%d hooks ran; a degraded post_start hook must not stop the rest", ran)
	}
	if !res.Degraded() {
		t.Fatal("the failure was not recorded as degraded, so doctor would never show it")
	}
	if res.Ran[0].Err == nil {
		t.Error("the error itself was dropped")
	}
}

// TestRunResolvesEverythingBeforeRunningAnything: a phase that aborted halfway
// because its third entry was misspelled would have APPLIED the first two,
// which is the opposite of what a closed registry is for.
func TestRunResolvesEverythingBeforeRunningAnything(t *testing.T) {
	ran := 0
	install(t, scripted{"test.first", []Phase{PhasePostInstall}, func(*Ctx) error {
		ran++
		return nil
	}})

	_, err := Run(t.Context(), &Ctx{Phase: PhasePostInstall},
		[]string{"test.first", "test.misspelled"})
	if err == nil {
		t.Fatal("Run accepted an unknown name")
	}
	if ran != 0 {
		t.Errorf("%d hooks ran before the list was validated", ran)
	}
}

func TestRunRefusesAnUndeclaredPhase(t *testing.T) {
	if _, err := Run(t.Context(), &Ctx{Phase: "whenever"}, nil); err == nil {
		t.Fatal("Run accepted a phase that is not a phase")
	}
	if _, err := Run(t.Context(), nil, nil); err == nil {
		t.Fatal("Run accepted a nil context")
	}
}

func TestRunStopsOnACancelledContext(t *testing.T) {
	ran := 0
	install(t, scripted{"test.slow", []Phase{PhasePostInstall}, func(*Ctx) error {
		ran++
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, &Ctx{Phase: PhasePostInstall}, []string{"test.slow"}); err == nil {
		t.Fatal("Run ignored a cancelled context")
	}
	if ran != 0 {
		t.Error("a hook ran after the job was cancelled")
	}
}

func TestBootRequestSurvivesThePhase(t *testing.T) {
	install(t, scripted{"test.asks", []Phase{PhasePostInstall}, func(h *Ctx) error {
		h.RequestBoot(BootRequest{Why: "the ini does not exist", WaitFor: "SERVER STARTED",
			Then: "quit"})
		return nil
	}})
	res, err := Run(t.Context(), &Ctx{Phase: PhasePostInstall}, []string{"test.asks"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Boot == nil || res.Boot.WaitFor != "SERVER STARTED" {
		t.Fatalf("the boot request did not reach the pipeline: %+v", res.Boot)
	}
}

func TestContentState(t *testing.T) {
	c := NewContentState(map[string]spec.ContentValues{
		"workshop": {Acquire: []string{"2392709985", "2169435993"}},
	})
	if got := c.Blocks(); len(got) != 1 || got[0] != "workshop" {
		t.Fatalf("Blocks() = %v", got)
	}
	if got := c.Acquire("workshop"); len(got) != 2 || got[0] != "2392709985" {
		t.Fatalf("Acquire() = %v", got)
	}
	if c.Changed() {
		t.Error("a freshly built state reports a change")
	}

	if err := c.SetActivation("workshop", []string{"Tsarslib", "Brita"}); err != nil {
		t.Fatal(err)
	}
	if !c.Changed() {
		t.Error("writing a different activation list was not recorded as a change")
	}
	if got := c.Activation("workshop"); strings.Join(got, ",") != "Tsarslib,Brita" {
		t.Errorf("Activation() = %v", got)
	}

	// A block the manifest does not declare is refused rather than created: the
	// lock is pinned intent, and inventing a namespace would write a lock no
	// manifest can explain.
	if err := c.SetActivation("nope", []string{"x"}); err == nil {
		t.Error("SetActivation invented a content block")
	}

	// The caller's slices are copied in and out, so a hook cannot mutate the
	// lock outside the one call that records the change.
	acq := c.Acquire("workshop")
	acq[0] = "mutated"
	if c.Acquire("workshop")[0] == "mutated" {
		t.Error("Acquire handed out the lock's own slice")
	}
	if v := c.Values()["workshop"]; len(v.Activate) != 2 || v.Acquire[0] != "2392709985" {
		t.Errorf("Values() = %+v", v)
	}
}

func TestContentStateWritesNothingWhenNothingChanged(t *testing.T) {
	c := NewContentState(map[string]spec.ContentValues{
		"workshop": {Acquire: []string{"1"}, Activate: []string{"Brita"}},
	})
	if err := c.SetActivation("workshop", []string{"Brita"}); err != nil {
		t.Fatal(err)
	}
	if c.Changed() {
		t.Error("re-writing the same activation list was recorded as a change; the lock would " +
			"be rewritten on every content cycle and its mtime would mean nothing")
	}
}

func TestNilContentStateIsSafeAndSaysWhy(t *testing.T) {
	var c *ContentState
	if c.Blocks() != nil || c.Acquire("x") != nil || c.Changed() {
		t.Error("the nil state answered as if it had content")
	}
	err := c.SetActivation("x", nil)
	if err == nil || !errors.Is(err, ErrNoSeam) {
		t.Fatalf("err = %v, want an ErrNoSeam naming post_content", err)
	}
	if !strings.Contains(err.Error(), "post_content") {
		t.Errorf("the error does not say where activation ids are writable: %v", err)
	}
}

func TestPhasesAreValid(t *testing.T) {
	seen := map[Phase]bool{}
	for _, p := range Phases() {
		if !p.Valid() {
			t.Errorf("%q is in Phases() and not Valid()", p)
		}
		if seen[p] {
			t.Errorf("%q appears twice", p)
		}
		seen[p] = true
	}
	if Phase("post_install_2").Valid() {
		t.Error("an invented phase validated")
	}
}
