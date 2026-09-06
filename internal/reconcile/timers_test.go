package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func nightlyInput(t *testing.T) TimerInput {
	t.Helper()
	in, raw := loadInstance(t, "pz-main")
	ti, err := TimerFromJob(in, "nightly", in.Spec.Jobs["nightly"], SpecSHA256(raw), renderedAt, goldenVersion)
	if err != nil {
		t.Fatal(err)
	}
	return ti
}

func TestGoldenTimerMatchesByteForByte(t *testing.T) {
	body, err := RenderTimer(nightlyInput(t))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join("testdata", "golden", "pz-main_nightly.timer")
	if *update {
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(want) {
		t.Errorf("rendered timer differs from %s\n--- want ---\n%s\n--- got ---\n%s", p, want, body)
	}
}

// TestAChangedScheduleRestartsAndAnUnchangedOneStarts is nightly.sh's lesson,
// and it is the reason this file exists.
//
// `systemctl enable --now` is not enough for a timer whose OnCalendar changed:
// a live timer computes its next elapse when it STARTS and does not recompute
// it on a reload. So a running timer whose schedule was edited keeps firing at
// the old time forever, while the unit file, the enablement and `systemctl
// status` all report the new one. The bug is invisible until somebody notices
// the nightly restart happening at the wrong hour.
func TestAChangedScheduleRestartsAndAnUnchangedOneStarts(t *testing.T) {
	base := nightlyInput(t)
	onDisk, err := RenderTimer(base)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		schedule string
		current  []byte
		wantVerb hostops.Verb
	}{
		{
			name: "unchanged schedule starts", schedule: base.Schedule, current: onDisk,
			wantVerb: hostops.VerbUnitStart,
		},
		{
			name: "changed schedule restarts", schedule: "*-*-* 06:30:00 America/Denver", current: onDisk,
			wantVerb: hostops.VerbUnitRestart,
		},
		{
			// A timer that does not exist yet counts as changed. Restart is
			// correct for it too -- restarting a unit that is not running just
			// starts it -- and the alternative would need a third branch that
			// nothing ever exercises.
			name: "absent timer restarts", schedule: base.Schedule, current: nil,
			wantVerb: hostops.VerbUnitRestart,
		},
		{
			// A file with no OnCalendar at all is not a file whose schedule
			// matches. Treating "cannot tell" as "unchanged" is how a hand-
			// edited timer keeps its old elapse through every apply.
			name: "timer with no OnCalendar restarts", schedule: base.Schedule,
			current:  []byte("[Unit]\nDescription=hand written\n\n[Timer]\nOnBootSec=5min\n"),
			wantVerb: hostops.VerbUnitRestart,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := base
			in.Schedule = c.schedule
			host := &fakeHost{}
			a, err := PlanTimer(in, c.current, rootActor(), host, NewMask())
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Do(context.Background(), nil); err != nil {
				t.Fatal(err)
			}

			// The sequence matters as much as the last verb: the file has to
			// be written before the reload, and systemd has to have reloaded
			// before the enable acts on it.
			want := []string{
				"file.write job_timer/nightly",
				"daemon.reload",
				"unit.enable job_timer/nightly",
				string(c.wantVerb) + " job_timer/nightly",
			}
			got := host.verbs()
			if strings.Join(got, " -> ") != strings.Join(want, " -> ") {
				t.Errorf("verb sequence\n got %v\nwant %v", got, want)
			}
		})
	}
}

// TestTheRestartIsExplainedInThePlan.
//
// An operator reading `gvm apply --diff` has to be able to see that a timer is
// being restarted and why, because a restart of a live timer is the one thing
// in this file with a visible consequence.
func TestTheRestartIsExplainedInThePlan(t *testing.T) {
	base := nightlyInput(t)
	onDisk, err := RenderTimer(base)
	if err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.Schedule = "*-*-* 06:30:00 America/Denver"

	a, err := PlanTimer(changed, onDisk, rootActor(), &fakeHost{}, NewMask())
	if err != nil {
		t.Fatal(err)
	}
	step := a.Describe()
	for _, want := range []string{"05:00:00", "06:30:00", "RESTARTED"} {
		if !strings.Contains(step.Note, want) {
			t.Errorf("the plan line does not mention %q:\n%s", want, step.Note)
		}
	}
	if step.Change != Update {
		t.Errorf("change = %q, want %q", step.Change, Update)
	}
	if step.Diff == "" {
		t.Error("a changed timer produced no diff for --diff to print")
	}
}

// TestAnIdenticalTimerIsUnchangedDespiteItsTimestamp.
//
// The timer carries a `# rendered` line like every generated file, so a byte
// comparison would call every apply a change, restart every timer on the box
// and reset every countdown.
func TestAnIdenticalTimerIsUnchangedDespiteItsTimestamp(t *testing.T) {
	base := nightlyInput(t)
	onDisk, err := RenderTimer(base)
	if err != nil {
		t.Fatal(err)
	}
	later := base
	later.Now = renderedAt.AddDate(0, 1, 0)

	a, err := PlanTimer(later, onDisk, rootActor(), &fakeHost{}, NewMask())
	if err != nil {
		t.Fatal(err)
	}
	if a.Describe().Change != Unchanged {
		t.Errorf("a re-render a month later is reported as %q", a.Describe().Change)
	}
	if a.CalendarChanged {
		t.Error("an unchanged schedule was reported as changed; the timer would be restarted")
	}
}

// TestPersistentCarriesTheReasonItIsSetEitherWay.
//
// The asymmetry is the bash suite's and is easy to get backwards: a missed
// restart that fires at boot restarts a live server mid-day, while a missed
// backup that never catches up is a gap in the only thing that makes a
// mistake reversible.
func TestPersistentCarriesTheReasonItIsSetEitherWay(t *testing.T) {
	in, raw := loadInstance(t, "pz-main")

	nightly, err := RenderTimer(mustTimer(t, in, "nightly", raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nightly), "\nPersistent=false\n") {
		t.Error("the nightly restart timer is Persistent=true; a reboot would fire it mid-day")
	}
	if !strings.Contains(string(nightly), "zomboid-restart.timer") {
		t.Error("the reason Persistent=false is set is not in the file")
	}

	backup, err := RenderTimer(mustTimer(t, in, "backup", raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backup), "\nPersistent=true\n") {
		t.Error("the backup timer is not Persistent=true; a missed backup would never catch up")
	}
}

func mustTimer(t *testing.T, in *spec.Instance, job string, raw []byte) TimerInput {
	t.Helper()
	ti, err := TimerFromJob(in, job, in.Spec.Jobs[job], SpecSHA256(raw), renderedAt, goldenVersion)
	if err != nil {
		t.Fatal(err)
	}
	return ti
}

// TestATimerNamesTheServiceItTriggers.
//
// D-13's slug is `<instance>_<job>`, and the timer and the service have to
// derive it identically or the timer fires a unit that does not exist -- which
// systemd reports once, in the journal, at 05:00.
func TestATimerNamesTheServiceItTriggers(t *testing.T) {
	body, err := RenderTimer(nightlyInput(t))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "\nUnit=gvm-job@pz-main_nightly.service\n") {
		t.Errorf("the timer does not trigger the job unit:\n%s", text)
	}
	if !strings.Contains(text, "# unit        gvm-job@pz-main_nightly.timer") {
		t.Error("the header does not name the timer itself")
	}
}

// TestAScheduleCannotSmuggleADirective.
//
// jobs.<name>.schedule is operator-editable YAML that is written verbatim into
// a unit file that runs as root. A newline in it is not a malformed schedule;
// it is a second directive.
func TestAScheduleCannotSmuggleADirective(t *testing.T) {
	in := nightlyInput(t)
	in.Schedule = "*-*-* 05:00:00\nExecStart=/bin/sh -c id"
	if _, err := RenderTimer(in); err == nil {
		t.Fatal("a schedule carrying a newline rendered into a timer")
	}

	in.Schedule = ""
	if _, err := RenderTimer(in); err == nil {
		t.Fatal("an empty schedule rendered a timer that would load and never fire")
	}
	in.Schedule = " *-*-* 05:00:00 "
	if _, err := RenderTimer(in); err == nil {
		t.Fatal("a padded schedule was accepted; it would compare as changed on every apply")
	}
}

// TestDeletingAJobFromTheSpecRemovesItsTimer.
//
// BuildPlan enumerates jobs from ONE place -- sortedJobNames(inst.Spec.Jobs) --
// so a job the operator deleted from the YAML is simply not planned for, and
// everything the previous apply left behind survives: the .timer file, its
// enablement symlink, the live armed timer and the descriptor under
// /etc/gvm/jobs. `--diff` mentions none of it, because a plan step only exists
// if something added it, and nothing else in gvm removes either -- there is no
// sweeper, and `gvm destroy` strands them too.
//
// Two places in the codebase already assert this behaviour should exist:
// hostops' fileRemove is documented and tested as re-runnable specifically so
// that "`gvm apply` removing a timer for a job the operator deleted" works, and
// TimerAction.Destructive says "Removing a job's timer is a KindRemove step of
// its own". The primitives were built for a caller that was never written.
//
// The steps are deliberately NOT KindRemove: Plan.Validate enforces
// KindRemove <=> Destructive(), and Applier.Apply refuses a destructive plan
// with no verified *archive.Ref -- which would make every job-deleting apply
// demand an archive it has no reason to want. A timer is regenerable from the
// spec; a world is not.
func TestDeletingAJobFromTheSpecRemovesItsTimer(t *testing.T) {
	// A host that a previous apply left with a nightly job.
	first, _, _ := planFor(t, "pz-main", "projectzomboid", nil)
	ins := newInspect().account("pzserver")
	for _, a := range first.Steps {
		switch v := a.(type) {
		case *FileAction:
			ins.put(v.Path, v.Body)
		case *DirAction:
			ins.mkdir(v.Path)
		case *TimerAction:
			ins.put(timerPathOf(v), v.Body)
		}
	}

	// The operator deletes the job.
	p, host, _ := planFor(t, "pz-main", "projectzomboid", func(pi *PlanInput) {
		pi.Inspect = ins
		pi.Instance.Spec.Jobs = nil
	})

	if err := p.Validate(); err != nil {
		t.Fatalf("the plan does not validate: %v", err)
	}
	for i, a := range p.Steps {
		if a.Destructive() {
			t.Fatalf("step %d is destructive, so this apply would demand an archive ref for "+
				"removing a regenerable timer", i)
		}
	}

	if _, err := (&Applier{Host: host, Inspect: ins}).Apply(context.Background(), p, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := strings.Join(host.verbs(), "\n")
	for _, want := range []string{
		"unit.stop job_timer/nightly",
		"unit.disable job_timer/nightly",
		"file.remove job_timer/nightly",
		"file.remove job_desc/nightly",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q; the deleted job's timer stays armed and fires at 05:00 against "+
				"an instance whose spec no longer declares it:\n%s", want, got)
		}
	}
	// Ordering: the enablement symlink is resolved out of the unit file, so the
	// file cannot go first, and an interrupted teardown must leave the timer
	// disabled and inert rather than armed with no descriptor to resolve.
	if i, j := strings.Index(got, "unit.disable job_timer"), strings.Index(got, "file.remove job_timer"); i > j {
		t.Errorf("the timer file is removed before it is disabled, which strands its "+
			"timers.target.wants symlink:\n%s", got)
	}
	// daemon.reload addresses the MANAGER, not a unit, and hostops refuses a
	// request that sets a field the verb does not read. The fake host here
	// records requests without validating them, so this asserts the shape
	// directly -- the alternative is a plan that only fails against the real
	// gateway, at the end of a destroy.
	for _, v := range host.verbs() {
		if strings.HasPrefix(v, "daemon.reload") && strings.Contains(v, "/") {
			t.Errorf("the reload carries a job (%q); hostops refuses that, so this apply would "+
				"fail part-way through the teardown", v)
		}
	}
}

// TestAConvergedHostPlansNoTimerRemovals is the other half: the orphan sweep
// must not fire on a host whose job set is exactly what the spec declares, or
// every apply would report work.
func TestAConvergedHostPlansNoTimerRemovals(t *testing.T) {
	first, _, _ := planFor(t, "pz-main", "projectzomboid", nil)
	ins := newInspect().account("pzserver")
	for _, a := range first.Steps {
		switch v := a.(type) {
		case *FileAction:
			ins.put(v.Path, v.Body)
		case *DirAction:
			ins.mkdir(v.Path)
		case *TimerAction:
			ins.put(timerPathOf(v), v.Body)
		}
	}

	p, _, _ := planFor(t, "pz-main", "projectzomboid", func(pi *PlanInput) { pi.Inspect = ins })
	for _, s := range p.Describe() {
		if s.Change == Delete {
			t.Errorf("a converged apply plans to remove %s", s.Object)
		}
	}
}
