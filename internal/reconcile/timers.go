package reconcile

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// onCalendarPrefix is the directive whose value decides Start versus Restart.
//
// It is a constant because two things read it: the renderer that writes it and
// the comparison that reads it back out of whatever is already on disk. A
// second spelling would make a changed schedule look unchanged, which is
// exactly the bug this file exists to prevent.
const onCalendarPrefix = "OnCalendar="

// TimerInput is one job's timer, resolved.
type TimerInput struct {
	Instance string
	Job      string
	// Action is what the job does: update | restart | backup | content_sync.
	// It reaches the file only as the Description, but it is the thing an
	// operator greps `systemctl list-timers` for. The destructive kinds are
	// absent because they cannot be scheduled at all -- section 16.4's
	// confirmation is a name a person types, and a timer has nowhere to put
	// one (see spec.unschedulableActions).
	Action string

	Schedule        string
	RandomizedDelay time.Duration
	Persistent      bool

	SpecPath string
	SpecSHA  string
	Now      time.Time
	Version  string
}

// TimerFromJob builds the input from an instance's own jobs block.
func TimerFromJob(inst *spec.Instance, job string, j spec.Job, specSHA string, now time.Time, version string) (TimerInput, error) {
	if inst == nil {
		return TimerInput{}, fmt.Errorf("%w: no instance", ErrPlan)
	}
	name := inst.Metadata.Name
	if err := instance.ValidateName(name); err != nil {
		return TimerInput{}, err
	}
	if err := instance.ValidateJobName(job); err != nil {
		return TimerInput{}, err
	}
	action := j.Action
	if action == "" {
		// The manifest's recommended_jobs default an action to the job's own
		// name, and an instance that copied one may have left it out. A timer
		// whose Description said "gvm  for pz-main" is a timer nobody can
		// identify in `systemctl list-timers`.
		action = job
	}
	return TimerInput{
		Instance:        name,
		Job:             job,
		Action:          action,
		Schedule:        j.Schedule,
		RandomizedDelay: j.RandomizedDelay.Duration(),
		Persistent:      j.Persistent,
		SpecPath:        instance.PathsFor(name).Spec,
		SpecSHA:         specSHA,
		Now:             now,
		Version:         version,
	}, nil
}

type timerModel struct {
	Unit     string
	SpecPath string
	SpecSHA  string
	Rendered string
	Version  string

	Description        string
	Service            string
	OnCalendar         string
	RandomizedDelaySec int
	Persistent         bool
	PersistentNote     []string
}

// RenderTimer produces the .timer file for one job.
//
// The schedule is validated before it is rendered, not after: an OnCalendar
// systemd cannot parse produces a timer unit that loads and never fires, and
// "the nightly restart silently stopped happening" is the least observable
// failure in this whole subsystem.
func RenderTimer(in TimerInput) ([]byte, error) {
	if err := instance.ValidateName(in.Instance); err != nil {
		return nil, err
	}
	if err := instance.ValidateJobName(in.Job); err != nil {
		return nil, err
	}
	if err := ValidateCalendar(in.Schedule); err != nil {
		return nil, err
	}
	if err := checkUnitValue("job action", in.Action); err != nil {
		return nil, err
	}

	delay := int(in.RandomizedDelay / time.Second)
	if delay < 0 {
		return nil, fmt.Errorf("%w: randomized_delay is negative", ErrPlan)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	version := in.Version
	if version == "" {
		version = "dev"
	}

	md := timerModel{
		Unit:     instance.JobTimer(in.Instance, in.Job),
		SpecPath: in.SpecPath,
		SpecSHA:  in.SpecSHA,
		Rendered: now.UTC().Format(time.RFC3339),
		Version:  version,

		Description:        fmt.Sprintf("gvm %s for %s (job %s)", in.Action, in.Instance, in.Job),
		Service:            instance.JobUnit(in.Instance, in.Job),
		OnCalendar:         in.Schedule,
		RandomizedDelaySec: delay,
		Persistent:         in.Persistent,
		PersistentNote:     persistentNote(in.Persistent),
	}
	var buf bytes.Buffer
	if err := unitTemplates.ExecuteTemplate(&buf, tmplTimer, md); err != nil {
		return nil, fmt.Errorf("reconcile: render timer for %s_%s: %w", in.Instance, in.Job, err)
	}
	return buf.Bytes(), nil
}

// persistentNote carries the distinction the bash suite already made.
//
// zomboid-restart.timer set Persistent=false on purpose and the backup timer
// did not, and the reason is asymmetric: a missed restart that fires at boot
// restarts a live server mid-day with players on, while a missed backup that
// never catches up is a gap in the only thing that can undo a mistake.
func persistentNote(p bool) []string {
	if p {
		return []string{
			"true: a missed run catches up at the next boot. Correct for a backup --",
			"a missed one is a gap in the record, and the record is what makes every",
			"destructive operation reversible.",
		}
	}
	return []string{
		"false, exactly like zomboid-restart.timer: a missed restart must NOT fire",
		"the moment the box boots, mid-day, with players on.",
	}
}

// CalendarOf reads the OnCalendar value out of a rendered timer.
//
// It parses rather than remembers, because the value it must compare against
// is whatever is on disk -- possibly written by an older gvm, possibly by an
// operator who edited it, possibly absent. ok is false when the file has no
// OnCalendar at all, which a caller must treat as "changed": a timer with no
// schedule is not a timer whose schedule matches.
func CalendarOf(unit []byte) (string, bool) {
	for _, line := range strings.Split(string(unit), "\n") {
		t := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if v, ok := strings.CutPrefix(t, onCalendarPrefix); ok {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// TimerAction writes one job's timer and brings it live.
//
// This is nightly.sh's lesson as code. `systemctl enable --now` is NOT enough
// for a timer whose OnCalendar changed: a live timer computes its next elapse
// when it starts and does not recompute it on reload, so a running timer keeps
// firing at the OLD time forever while every observable -- the unit file, the
// enablement, `systemctl status` -- says the new one. The bash suite carries
// the comment; this carries the branch.
type TimerAction struct {
	Instance string
	Job      string
	Actor    audit.Actor
	Host     Host

	// Body is the rendered timer. It is built by the planner, so this action
	// renders nothing at Do time and `--diff` showed exactly these bytes.
	Body []byte

	// CalendarChanged is decided by the planner from what was on disk. It is a
	// field rather than a method because the file it was decided from will
	// have been overwritten by the time Do needs the answer.
	CalendarChanged bool

	// Was is the previous schedule, for the plan line. Empty means the timer
	// did not exist or carried no OnCalendar.
	Was string
	// Now is the schedule being written.
	Nowis string

	change Change
	diff   string
}

// Describe renders the plan line, naming the restart and why.
func (a *TimerAction) Describe() Step {
	note := ""
	switch {
	case a.CalendarChanged && a.Was != "":
		note = fmt.Sprintf("schedule %q -> %q; the timer is RESTARTED, not started: a live "+
			"timer recomputes its next elapse only on restart, so `enable --now` would leave "+
			"it firing at the old time.", a.Was, a.Nowis)
	case a.CalendarChanged:
		note = fmt.Sprintf("schedule %q; the timer is restarted so systemd computes its first "+
			"elapse from the file just written.", a.Nowis)
	default:
		note = fmt.Sprintf("schedule %q, unchanged; started rather than restarted so a timer "+
			"already counting down is not reset.", a.Nowis)
	}
	return Step{
		Kind:     KindTimer,
		Change:   a.change,
		Instance: a.Instance,
		Object:   instance.JobTimer(a.Instance, a.Job),
		Note:     note,
		Diff:     a.diff,
	}
}

// Destructive is false: a timer writes a file and starts a unit. Removing a
// job's timer is a KindRemove step of its own.
func (a *TimerAction) Destructive() bool { return false }

// Do writes the timer, reloads, enables, and then starts or RESTARTS.
func (a *TimerAction) Do(ctx context.Context, _ *archive.Ref) error {
	req := hostops.Request{
		Verb: hostops.VerbFileWrite, Actor: a.Actor,
		Instance: a.Instance, Job: a.Job,
		File: hostops.FileJobTimer, Body: a.Body,
	}
	if _, err := a.Host.Do(ctx, req); err != nil {
		return err
	}
	// The reload is per-timer rather than folded into the apply's single
	// daemon-reload because the enable and start below act on the unit file
	// just written: enabling a unit systemd has not read yet fails, and
	// starting one it read before the write starts the previous schedule.
	if _, err := a.Host.Do(ctx, hostops.Request{
		Verb: hostops.VerbDaemonReload, Actor: a.Actor, Instance: a.Instance,
	}); err != nil {
		return err
	}
	if _, err := a.Host.Do(ctx, hostops.Request{
		Verb: hostops.VerbUnitEnable, Actor: a.Actor,
		Instance: a.Instance, Job: a.Job, Unit: hostops.UnitJobTimer,
	}); err != nil {
		return err
	}

	verb := hostops.VerbUnitStart
	if a.CalendarChanged {
		verb = hostops.VerbUnitRestart
	}
	_, err := a.Host.Do(ctx, hostops.Request{
		Verb: verb, Actor: a.Actor,
		Instance: a.Instance, Job: a.Job, Unit: hostops.UnitJobTimer,
	})
	return err
}

// PlanTimer decides everything about one timer before anything is written.
//
// current is the bytes already on disk, or nil if the timer does not exist.
// Returning the action rather than performing the comparison inside Do is what
// makes the branch testable and what makes `--diff` able to say "the timer is
// restarted because the schedule changed" before it happens.
func PlanTimer(in TimerInput, current []byte, actor audit.Actor, host Host, mask *Mask) (*TimerAction, error) {
	body, err := RenderTimer(in)
	if err != nil {
		return nil, err
	}
	was, had := CalendarOf(current)
	nowis, _ := CalendarOf(body)

	a := &TimerAction{
		Instance: in.Instance, Job: in.Job, Actor: actor, Host: host,
		Body: body,
		// A timer that does not exist, or one whose file carries no
		// OnCalendar, counts as CHANGED. Starting it would be correct in the
		// first case and wrong in the second, and restart is correct in both:
		// restarting a timer that is not running just starts it.
		CalendarChanged: !had || was != nowis,
		Was:             was,
		Nowis:           nowis,
		change:          Add,
	}
	if current != nil {
		a.change = Update
		if bytes.Equal(stripRendered(current), stripRendered(body)) {
			a.change = Unchanged
			// Carry the on-disk bytes, for the reason planFile records: Do
			// writes unconditionally and hostops compares exactly, so an
			// Unchanged step must issue a byte-identical write or a converged
			// apply rewrites every timer it owns. CalendarChanged is computed
			// from the on-disk calendar above and is unaffected.
			a.Body = current
		} else {
			a.diff = Unified(instance.JobTimerPath(in.Instance, in.Job), current, body, mask)
		}
	}
	return a, nil
}

// OrphanTimerAction takes down the timer of a job the spec no longer declares.
//
// It exists because BuildPlan enumerates jobs from exactly one place --
// inst.Spec.Jobs -- so without this a reconciler can only converge by addition:
// delete a job from the YAML and its .timer, its enablement symlink, the live
// armed timer and its descriptor all survive, with `--diff` mentioning none of
// them because a plan step only exists if something added it. The register
// already settles the direction: D-42 has apply DELETE a ufw rule carrying
// gvm's own tag once it is no longer declared, and a reconciler that removes an
// undeclared firewall rule but not an undeclared timer is inconsistent with
// itself.
//
// It is deliberately NOT a KindRemove step. Plan.Validate enforces
// KindRemove <=> Destructive() in both directions, and Applier.Apply refuses a
// destructive plan that has no verified *archive.Ref -- so making this
// destructive would have every job-deleting apply demand an archive it has no
// reason to want. The asymmetry is the right one: a timer is regenerable from
// the spec in full, and a world is not.
type OrphanTimerAction struct {
	Instance string
	Job      string
	Actor    audit.Actor
	Host     Host
}

func (a *OrphanTimerAction) Describe() Step {
	return Step{
		Kind: KindTimer, Change: Delete, Instance: a.Instance,
		Object: instance.JobTimer(a.Instance, a.Job),
		Note: "the spec no longer declares this job; its timer is stopped, disabled and removed, " +
			"along with its descriptor. Left behind it would keep firing against an instance " +
			"that has no such job.",
	}
}

// Destructive is false. See the type comment: this removes a generated unit
// file, not data, and requiring an archive for it would make the archive gate
// meaningless where it matters.
func (a *OrphanTimerAction) Destructive() bool { return false }

// Do stops and disables before removing anything.
//
// The order is not cosmetic. DisableUnitFiles resolves the [Install] section
// out of the unit FILE to find the symlinks it planted, so removing the timer
// first strands its timers.target.wants link with nothing left to name it. And
// an interrupted teardown has to leave the timer disabled and inert rather than
// armed with no descriptor to resolve -- the same reasoning `gvm destroy`
// applies to the instance YAML it removes last.
func (a *OrphanTimerAction) Do(ctx context.Context, _ *archive.Ref) error {
	// daemon.reload addresses the manager rather than a unit, and hostops
	// refuses a request that sets a field the verb does not read -- so the job
	// is carried only on the requests that are about the job.
	for _, r := range []hostops.Request{
		{Verb: hostops.VerbUnitStop, Unit: hostops.UnitJobTimer, Job: a.Job,
			// A timer has no shutdown ladder, so the rung audit.InstanceStop
			// requires is "none" -- which is what RungNone means. Omitting it
			// would write an audit row hostops has already judged invalid.
			Params: audit.Params{"reason": "job removed from the spec", "rung": "none"}},
		{Verb: hostops.VerbUnitDisable, Unit: hostops.UnitJobTimer, Job: a.Job},
		{Verb: hostops.VerbFileRemove, File: hostops.FileJobTimer, Job: a.Job},
		{Verb: hostops.VerbDaemonReload},
		{Verb: hostops.VerbFileRemove, File: hostops.FileJobDesc, Job: a.Job},
	} {
		r.Actor, r.Instance = a.Actor, a.Instance
		if _, err := a.Host.Do(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// orphanTimers finds the jobs a PREVIOUS apply wrote that this spec no longer
// declares.
//
// The enumeration source is /etc/gvm/jobs rather than /etc/systemd/system, and
// that choice is what keeps it instance-scoped and cheap: the directory is flat
// and every entry is "<instance>_<job>.json" (D-13), instance names cannot
// contain "_" (D-45), so a prefix filter yields exactly this instance's job set
// as the last apply left it.
//
// A nil Inspector plans no removals rather than failing, exactly as readCurrent
// treats one: a planner that cannot look has not discovered an orphan.
func orphanTimers(inst *spec.Instance, name string, actor audit.Actor, host Host, ins Inspector) []Action {
	if ins == nil {
		return nil
	}
	entries, err := ins.ReadDir(instance.EtcJobsDir)
	if err != nil {
		// A missing /etc/gvm/jobs is the ordinary state of a box with no jobs.
		return nil
	}

	declared := make(map[string]bool, len(inst.Spec.Jobs))
	for job := range inst.Spec.Jobs {
		declared[job] = true
	}

	var out []Action
	var orphans []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		gotInst, job, err := instance.SplitJobSlug(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil || gotInst != name || declared[job] {
			continue
		}
		orphans = append(orphans, job)
	}
	// Sorted so a plan reads the same way twice and the audit trail of two
	// identical applies is identical.
	sort.Strings(orphans)
	for _, job := range orphans {
		out = append(out, &OrphanTimerAction{Instance: name, Job: job, Actor: actor, Host: host})
	}
	return out
}
