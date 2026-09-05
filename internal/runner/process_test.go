package runner

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

func TestParseScopeDefaultsToTheProcessGroup(t *testing.T) {
	// An absent `scope:` is the common case in every shipped manifest, and it
	// has to mean the group: a launcher script that does not exec is the shape
	// this whole subsystem is built around.
	for _, in := range []string{"", "process_group"} {
		got, err := ParseScope(in)
		if err != nil {
			t.Fatalf("ParseScope(%q): %v", in, err)
		}
		if got != ScopeGroup {
			t.Errorf("ParseScope(%q) = %v, want %v", in, got, ScopeGroup)
		}
	}
	if got, err := ParseScope("process"); err != nil || got != ScopeProcess {
		t.Errorf("ParseScope(process) = %v, %v", got, err)
	}
}

func TestParseScopeRefusesTheCgroupSweep(t *testing.T) {
	// The cgroup rung is "SIGKILL everything left". It is added implicitly
	// after the declared steps; a manifest that could name it could put it
	// between `save` and `quit`.
	for _, in := range []string{"cgroup", "all", "unit"} {
		if _, err := ParseScope(in); err == nil {
			t.Errorf("ParseScope(%q) was accepted", in)
		}
	}
}

func TestIdentityRefusesAPinWithNoStartTime(t *testing.T) {
	// A zero StartTicks is what an unchecked ReadStat error leaves behind.
	// Treating it as a pin turns "we never learned the start time" into "this
	// pid is the game", which is the pid-reuse bug wearing a disguise.
	cases := []struct {
		name string
		id   Identity
		want bool
	}{
		{"complete", Identity{PID: 41799, PGID: 41799, StartTicks: 8391244}, true},
		{"no start ticks", Identity{PID: 41799, PGID: 41799}, false},
		{"no pid", Identity{PGID: 41799, StartTicks: 8391244}, false},
		{"no pgid", Identity{PID: 41799, StartTicks: 8391244}, false},
		{"zero", Identity{}, false},
	}
	for _, c := range cases {
		if got := c.id.Valid(); got != c.want {
			t.Errorf("%s: Valid() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestExitStatusSaysWhichWayTheProcessDied(t *testing.T) {
	clean := ExitStatus{}
	if !clean.OK() || clean.SignalName() != "" {
		t.Errorf("a zero exit is not clean: %+v", clean)
	}
	crashed := ExitStatus{Code: 1}
	if crashed.OK() {
		t.Error("exit 1 reported OK")
	}
	killed := ExitStatus{Code: -1, Signal: platform.SIGKILL, Signaled: true}
	if killed.OK() {
		t.Error("a SIGKILLed process reported OK")
	}
	// The report prints both fields, and "exit_code: -1" tells an operator
	// nothing at 2am while "killed by SIGKILL" is the whole story.
	if killed.SignalName() != "SIGKILL" || killed.String() != "killed by SIGKILL" {
		t.Errorf("killed = %q / %q", killed.SignalName(), killed.String())
	}
}

func TestSurvivorsExcludesTheRunnerItself(t *testing.T) {
	// The runner is in its own unit's cgroup. A sweep that includes it kills
	// the process holding the shutdown report seconds before it is written.
	got := survivors([]int{41799, 4242, 41822}, 4242)
	if want := []int{41799, 41822}; !reflect.DeepEqual(got, want) {
		t.Errorf("survivors = %v, want %v", got, want)
	}
}

func TestSurvivorsDropsGarbageAndDuplicates(t *testing.T) {
	// A pid of 0 reaching kill(2) signals the caller's own group. A duplicate
	// from a racing read would be signalled twice, which is harmless but makes
	// the report claim more processes than existed.
	got := survivors([]int{0, -1, 41822, 41822, 41799}, 4242)
	if want := []int{41799, 41822}; !reflect.DeepEqual(got, want) {
		t.Errorf("survivors = %v, want %v", got, want)
	}
	if got := survivors(nil, 4242); len(got) != 0 {
		t.Errorf("survivors(nil) = %v", got)
	}
}

func TestCheckServiceCgroupRefusesALoginSession(t *testing.T) {
	// This is the guard that keeps "SIGKILL everything left in the cgroup"
	// from logging an operator out of their own box. `gvm run` typed at a
	// shell lands in a session scope whose members are that shell, the ssh
	// connection and whatever else the operator is running.
	cases := []struct {
		name       string
		cgroup     string
		invocation string
		wantErr    bool
	}{
		{"a unit", "/system.slice/gvm.slice/gvm@pz-main.service", "b3f1", false},
		{"a login session", "/user.slice/user-1000.slice/session-3.scope", "b3f1", true},
		{"no systemd", "/system.slice/gvm.slice/gvm@pz-main.service", "", true},
		{"the root cgroup", "/", "b3f1", true},
		{"not a path", "system.slice/gvm@pz-main.service", "b3f1", true},
	}
	for _, c := range cases {
		err := checkServiceCgroup(c.cgroup, c.invocation)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: checkServiceCgroup(%q, %q) = %v", c.name, c.cgroup, c.invocation, err)
		}
		if err != nil && !errors.Is(err, ErrNoCgroup) {
			t.Errorf("%s: error does not wrap ErrNoCgroup: %v", c.name, err)
		}
	}
}

func TestFakeProcessWaitReturnsTheStatusItWasGiven(t *testing.T) {
	// The fake is load-bearing for every later test of the shutdown ladder, so
	// its own contract is asserted here rather than assumed there.
	p := newFakeProcess()
	p.exitOn = func(sig platform.Signal, scope Scope) bool { return sig == platform.SIGTERM }

	if err := p.Signal(platform.SIGTERM, ScopeGroup); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	st, err := p.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !st.Signaled || st.Signal != platform.SIGTERM {
		t.Errorf("exit = %+v", st)
	}
	if alive, _ := p.Alive(); alive {
		t.Error("still alive after exiting")
	}
	// A signal to a process that has already gone is the ladder's success
	// case, not a failure: the rung achieved what it was asked to achieve.
	if err := p.Signal(platform.SIGKILL, ScopeGroup); !errors.Is(err, ErrGone) {
		t.Errorf("second Signal = %v, want ErrGone", err)
	}
	if got := p.sent(); len(got) != 2 || got[0].Scope != ScopeGroup {
		t.Errorf("recorded signals = %+v", got)
	}
}

func TestFakeProcessWaitHonoursContextWithoutKilling(t *testing.T) {
	// Wait's contract: a cancelled context returns, and the child lives on. A
	// Wait that killed on cancellation would turn a cancelled HTTP request
	// into a world lost with no save.
	p := newFakeProcess()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
	if alive, _ := p.Alive(); !alive {
		t.Error("a cancelled Wait killed the process")
	}
}

func TestCheckSameCgroupRefusesAProcessTheCallerDidNotStart(t *testing.T) {
	// `gvm run-shutdown` is designed to be invoked by gvmd or a human, so the
	// process resolving the sweepable cgroup is usually NOT the game's parent.
	// A child forked by a long-lived .service inherits both INVOCATION_ID and
	// its parent's cgroup, so checkServiceCgroup alone sails through and the
	// last rung of the ladder SIGKILLs the caller's own cgroup -- gvmd, plus
	// whatever it had forked, mid-restore.
	const gvm = "/system.slice/gvm.slice/gvm@pz-main.service"
	cases := []struct {
		name         string
		self, target string
		wantErr      bool
		wantMentions []string
	}{
		{"the same unit", gvm, gvm, false, nil},
		{"a game in another instance's unit", "/system.slice/gvmd.service", gvm, true,
			[]string{"/system.slice/gvmd.service", gvm}},
		{"a game that has left the cgroup", gvm, "/system.slice/gvm.slice/gvm@other.service", true,
			[]string{gvm, "/system.slice/gvm.slice/gvm@other.service"}},
		{"an unreadable target", gvm, "", true, []string{gvm}},
	}
	for _, c := range cases {
		err := checkSameCgroup(c.self, c.target)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: checkSameCgroup(%q, %q) = %v", c.name, c.self, c.target, err)
			continue
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrNoCgroup) {
			t.Errorf("%s: error does not wrap ErrNoCgroup: %v", c.name, err)
		}
		// The message is the operator's only clue about why the sweep refused,
		// so it has to name both sides rather than say "mismatch".
		for _, want := range c.wantMentions {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not name %q", c.name, err, want)
			}
		}
	}
}

// TestSweepExemptionsAreThePidsGvmStartedItself.
//
// `console_compat: screen` runs `screen -dmS <name> gvm console` INSIDE the
// unit's cgroup, so its daemonised session leader and the `gvm console` it
// hosts sit there for the life of the instance. Empty() is cgroup-wide, so it
// is then never true, settled() can never be true, every rung reports
// Alive:true, and a textbook `quit` still walks all the way to the SIGKILL
// rungs -- which set Degraded and make `gvm run` exit 70 after a perfectly
// clean stop.
//
// Exempting them has to be narrow: a process forked by the GAME during the
// same window is in the game's process group and can never be exempted.
func TestSweepExemptionsAreThePidsGvmStartedItself(t *testing.T) {
	const gamePGID = 41799
	stats := map[int]platform.Stat{
		41799: {PID: 41799, PGID: gamePGID, StartTicks: 100},
		41830: {PID: 41830, PGID: gamePGID, StartTicks: 900}, // the game forked this
		52001: {PID: 52001, PGID: 52001, StartTicks: 901},    // screen's session leader
		52002: {PID: 52002, PGID: 52001, StartTicks: 902},    // the gvm console inside it
	}
	stat := func(pid int) (platform.Stat, error) {
		st, ok := stats[pid]
		if !ok {
			return platform.Stat{}, errors.New("no such process")
		}
		return st, nil
	}

	got := newExemptions([]int{41799}, []int{41799, 41830, 52001, 52002, 61000}, gamePGID, stat)
	want := []Identity{
		{PID: 52001, PGID: 52001, StartTicks: 901},
		{PID: 52002, PGID: 52001, StartTicks: 902},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exemptions = %+v, want %+v", got, want)
	}
	// 61000 was not in `after`'s stat table at all -- it exited while we were
	// reading. An unreadable process cannot be proven to be gvm's, so it is
	// not exempted.
	for _, id := range got {
		if id.PID == 61000 {
			t.Error("a process gvm could not read was exempted from the sweep")
		}
	}
}

func TestDropExemptRequiresTheIdentityToStillMatch(t *testing.T) {
	exempt := []Identity{{PID: 52001, PGID: 52001, StartTicks: 901}}
	stat := func(pid int) (platform.Stat, error) {
		switch pid {
		case 52001:
			// Same pid, different start time: the screen session died and the
			// number was handed to something else. An exemption pinned only by
			// pid is a hole a real survivor falls through.
			return platform.Stat{PID: 52001, PGID: 52001, StartTicks: 5000}, nil
		case 52002:
			return platform.Stat{PID: 52002, PGID: 52001, StartTicks: 902}, nil
		}
		return platform.Stat{}, errors.New("no such process")
	}
	got := dropExempt([]int{52001, 52002}, exempt, stat)
	if want := []int{52001, 52002}; !reflect.DeepEqual(got, want) {
		t.Errorf("remaining = %v, want %v: a recycled pid is not still exempt", got, want)
	}

	stat2 := func(pid int) (platform.Stat, error) {
		return platform.Stat{PID: 52001, PGID: 52001, StartTicks: 901}, nil
	}
	if got := dropExempt([]int{52001}, exempt, stat2); len(got) != 0 {
		t.Errorf("remaining = %v, want none: the exempted session is still itself", got)
	}
	// No exemptions at all must not change the answer.
	if got := dropExempt([]int{1, 2}, nil, stat2); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("remaining = %v with no exemptions", got)
	}
}
