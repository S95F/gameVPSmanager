package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// PlanInput is everything BuildPlan needs. Every field is data the caller
// already holds; nothing here is read off the host except through Inspect.
type PlanInput struct {
	Instance *spec.Instance
	// Manifest is the PINNED manifest. BuildPlan writes its Raw bytes to
	// manifest.lock.yaml, so the file the runner will load and the header the
	// drop-in carries describe the same manifest (D-17).
	Manifest *spec.Manifest

	// SpecBytes is the instance YAML as it will be on disk. It is both what
	// gets written and what the drop-in header's sha256 is taken over, so
	// `sha256sum /etc/gvm/instances/<name>.yaml` matches the header line.
	SpecBytes []byte

	Actor   audit.Actor
	Host    Host
	Inspect Inspector

	// Probe is the write-probe seam; nil means the real one.
	Probe ProbeFunc
	// AllowUnprobed is for `--diff` and for a development machine. It never
	// defaults to true.
	AllowUnprobed bool

	// Mask decides what a diff may print. A nil Mask prints everything, which
	// is correct only for files with no secrets in them; the planner adds the
	// manifest's tainted keys itself.
	Mask *Mask

	// HasSecret reports whether a credential file exists, for the
	// LoadCredential= lines.
	HasSecret func(id string) bool

	Now     time.Time
	Version string
}

// JobDescriptor is /etc/gvm/jobs/<instance>_<job>.json (D-13).
//
// It is what turns the "<instance>_<job>" systemd passes to `gvm job run` back
// into an instance, a job and an action with no database in the picture --
// which is the whole reason the scheduler is systemd and the ledger is
// Postgres and never the reverse (D-29).
type JobDescriptor struct {
	Instance string `json:"instance"`
	Job      string `json:"job"`
	Action   string `json:"action"`
}

// BuildPlan renders every byte and decides every ordering, and touches the
// host only through Inspect.
//
// The ordering below is not a style choice. Each step depends on the one
// before it in a way that fails loudly if reversed:
//
//   - the control-plane directories first, because hostops creates ONE
//     directory per call and refuses a missing parent;
//   - the instance YAML before the instance-owned directories, because
//     hostops reads the account out of that file rather than deriving
//     gvm-<name> -- which is what keeps an adopted world's directories owned
//     by pzserver;
//   - the account before those directories too, because they are chowned to
//     it;
//   - the WRITE-PROBE before the drop-in, because a sandbox that blocks the
//     game's own save directory has to be caught while nothing has changed
//     (D-15). Plan.Validate enforces this one rather than trusting the order
//     here.
//   - one daemon-reload after the unit files and before the enable.
func BuildPlan(in PlanInput) (*Plan, error) {
	if in.Instance == nil || in.Manifest == nil {
		return nil, fmt.Errorf("%w: BuildPlan needs an instance and its pinned manifest", ErrPlan)
	}
	inst := in.Instance
	name := inst.Metadata.Name
	if err := instance.ValidateName(name); err != nil {
		return nil, err
	}
	actor, host, ins := in.Actor, in.Host, in.Inspect
	mask := in.Mask
	if mask == nil {
		mask = NewMask()
	}

	p := &Plan{Instance: name}
	specSHA := SpecSHA256(in.SpecBytes)

	// 1. The root-owned control-plane directories.
	p.Add(dirActions(rootOwnedDirs, inst, name, actor, host, ins)...)

	// 2. The instance definition itself. It goes down before anything reads
	//    the account out of it.
	specStep, err := planFile(hostops.FileInstanceSpec, KindFile, name, "", "", in.SpecBytes,
		actor, host, ins, mask, "THE definition of this server; every later step reads it")
	if err != nil {
		return nil, err
	}
	p.Add(specStep)

	// 3. The account.
	p.Add(accountAction(inst, actor, host, accountPresence(ins)))

	// 4. The directories that belong to it, then the root-owned ones that sit
	//    inside those.
	p.Add(dirActions(instanceOwnedDirs, inst, name, actor, host, ins)...)
	if inst.Spec.ManagedInstall {
		p.Add(dirActions(managedGameDirs, inst, name, actor, host, ins)...)
	}
	p.Add(dirActions(lateRootDirs, inst, name, actor, host, ins)...)

	// 5. The pinned manifest bytes, verbatim (D-04). Writing m.Raw rather
	//    than a re-serialisation is what makes the pin a byte comparison
	//    instead of a semantic one.
	lockStep, err := planFile(hostops.FileManifestLock, KindFile, name, "", "", in.Manifest.Raw,
		actor, host, ins, mask, fmt.Sprintf("%s rev %d, pinned verbatim (D-04)",
			in.Manifest.Metadata.ID, in.Manifest.Metadata.Revision))
	if err != nil {
		return nil, err
	}
	p.Add(lockStep)

	// 6. Job descriptors, before the timers that reference them.
	jobs := sortedJobNames(inst.Spec.Jobs)
	if len(jobs) > 0 {
		p.Add(dirActions([]hostops.DirKind{hostops.DirJobs}, inst, name, actor, host, ins)...)
	}
	for _, job := range jobs {
		j := inst.Spec.Jobs[job]
		action := j.Action
		if action == "" {
			action = job
		}
		body, err := json.MarshalIndent(JobDescriptor{Instance: name, Job: job, Action: action}, "", "  ")
		if err != nil {
			return nil, err
		}
		body = append(body, '\n')
		step, err := planFile(hostops.FileJobDesc, KindFile, name, job, "", body,
			actor, host, ins, mask, "slug -> instance, job, action; what `gvm job run` reads (D-13)")
		if err != nil {
			return nil, err
		}
		p.Add(step)
	}

	// 7. THE WRITE-PROBE, then the drop-in it authorises.
	body, sb, err := BuildDropIn(DropInInput{
		Instance: inst, Manifest: in.Manifest, SpecSHA: specSHA,
		HasSecret: in.HasSecret, Now: in.Now, Version: in.Version,
	})
	if err != nil {
		return nil, err
	}
	p.Add(&ProbeAction{
		Spec: ProbeSpec{
			Instance: name,
			User:     inst.Spec.User,
			Group:    groupOf(inst),
			Sandbox:  sb,
		},
		Probe:         in.Probe,
		AllowUnprobed: in.AllowUnprobed,
	})
	dropIn, err := planFile(hostops.FileDropIn, KindDropIn, name, "", "", body,
		actor, host, ins, mask, "user, paths, env, credentials, resources, sandbox, arch, JIT")
	if err != nil {
		return nil, err
	}
	p.Add(dropIn)

	// 8. One reload, then the enablement the spec asks for.
	p.Add(&ReloadAction{Instance: name, Actor: actor, Host: host, Needed: unitFileChanged(p)})
	p.Add(enableAction(inst, actor, host, ins))

	// 9. The timers, each of which reloads for itself because its enable and
	//    start act on the file it has just written (section 7.5).
	for _, job := range jobs {
		ti, err := TimerFromJob(inst, job, inst.Spec.Jobs[job], specSHA, in.Now, in.Version)
		if err != nil {
			return nil, err
		}
		cur, err := readCurrent(ins, instance.JobTimerPath(name, job))
		if err != nil {
			return nil, err
		}
		ta, err := PlanTimer(ti, cur, actor, host, mask)
		if err != nil {
			return nil, err
		}
		p.Add(ta)
	}

	// Jobs the spec no longer declares, taken down AFTER the declared ones are
	// written and started -- so a job that was merely RENAMED converges in one
	// apply, with the new timer live before the old one stops and no window in
	// which the instance has no nightly at all.
	p.Add(orphanTimers(inst, name, actor, host, ins)...)

	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// unitFileChanged reports whether anything the plan writes so far is a unit
// file systemd would have to re-read.
//
// A reload that had nothing to re-read is harmless and a reload that was
// skipped when it was needed is a unit running from the previous file, so the
// question is asked conservatively: any drop-in write that is not explicitly
// Unchanged counts.
func unitFileChanged(p *Plan) bool {
	for _, s := range p.Describe() {
		if s.Kind == KindDropIn && s.Change != Unchanged {
			return true
		}
	}
	return false
}

func sortedJobNames(m map[string]spec.Job) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func accountPresence(ins Inspector) func(string) bool {
	if ins == nil {
		return func(string) bool { return false }
	}
	return ins.AccountExists
}

// StepResult is what one executed step reports.
type StepResult struct {
	Step Step
	Err  error
}

// Report is the outcome of an apply.
type Report struct {
	Instance string
	Results  []StepResult

	// Changed and Unchanged count the steps by what the PLAN predicted, which
	// is what `gvm apply` prints. hostops.Result.Changed is the authority on
	// what actually happened and is what lands in the audit row.
	Changed   int
	Unchanged int

	// Verified names the files whose bytes were read back and matched. It is
	// empty when no Inspector was given, and an apply that could not verify
	// says so rather than claiming it did.
	Verified []string
}

// Applier runs a plan: confirm, execute, verify.
type Applier struct {
	Host    Host
	Inspect Inspector
	Out     io.Writer

	// Confirm is asked before a DESTRUCTIVE plan runs and is not consulted
	// otherwise. A nil Confirm on a destructive plan is a refusal, not an
	// assent: section 16.4's destructive-action contract is that the operator
	// says yes, and a caller that forgot to wire the prompt must not be
	// counted as one.
	Confirm func(ctx context.Context, p *Plan) (bool, error)
}

// ErrNotConfirmed is a destructive plan the operator did not approve.
var ErrNotConfirmed = fmt.Errorf("reconcile: destructive plan not confirmed")

// Apply executes a plan.
//
// The order of the checks at the top is the order in which they can refuse
// without having done anything: the plan's shape, then the archive, then the
// operator. By the time the first Action.Do runs, everything that could say no
// has said yes.
func (a *Applier) Apply(ctx context.Context, p *Plan, ref *archive.Ref) (*Report, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: no plan", ErrPlan)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.Destructive() {
		if !ref.Verified() {
			return nil, fmt.Errorf("%w: this plan removes data and the archive it would be "+
				"authorised by is nil or unverified", ErrNoArchive)
		}
		if a.Confirm == nil {
			return nil, fmt.Errorf("%w: no confirmation was wired for a plan that removes data",
				ErrNotConfirmed)
		}
		ok, err := a.Confirm(ctx, p)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrNotConfirmed
		}
	}

	rep := &Report{Instance: p.Instance}
	for _, act := range p.Steps {
		s := act.Describe()
		if s.Change == Unchanged {
			rep.Unchanged++
		} else {
			rep.Changed++
		}
		if a.Out != nil {
			fmt.Fprintln(a.Out, "  "+s.String())
		}
		if err := act.Do(ctx, ref); err != nil {
			rep.Results = append(rep.Results, StepResult{Step: s, Err: err})
			// An apply stops at the first failure rather than carrying on.
			// The steps are ordered by dependency, so continuing past a
			// failed directory or a failed probe would execute steps whose
			// precondition is known to be false -- and the most likely next
			// one is the drop-in write the probe just refused.
			return rep, fmt.Errorf("reconcile: %s %s: %w", s.Kind, s.Object, err)
		}
		rep.Results = append(rep.Results, StepResult{Step: s})
	}

	if err := a.verify(p, rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// verify reads back what the apply wrote.
//
// It is a real check and not a formality: hostops skips a write whose bytes
// and mode already match, an atomic rename can lose a race with an operator
// editing the same file, and a drop-in that is not what gvm thinks it is
// governs a live server. Reading it back costs one open per file.
//
// The `# rendered` line is excluded from the comparison for the same reason it
// is excluded from the write decision: it is the one line that legitimately
// differs between two renderings of identical inputs.
func (a *Applier) verify(p *Plan, rep *Report) error {
	if a.Inspect == nil {
		return nil
	}
	for _, act := range p.Steps {
		fa, ok := act.(*FileAction)
		if !ok {
			continue
		}
		got, err := a.Inspect.ReadFile(fa.Path)
		if err != nil {
			return fmt.Errorf("reconcile: wrote %s and could not read it back: %w", fa.Path, err)
		}
		same := bytes.Equal(got, fa.Body)
		if fa.StepKind == KindDropIn || fa.Kind == hostops.FileJobTimer {
			same = SameDropIn(got, fa.Body)
		}
		if !same {
			return fmt.Errorf("reconcile: %s on disk is not what this apply wrote; "+
				"something else is writing it", fa.Path)
		}
		rep.Verified = append(rep.Verified, fa.Path)
	}
	return nil
}
