package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// Inspector is the read-only view of the host a PLANNER may use.
//
// Reads and no writes, deliberately: building a plan must not change anything,
// because `gvm apply --diff` builds one and an operator who ran --diff has to
// be able to say "nothing happened". The write half is hostops, reached only
// through Action.Do.
//
// The production implementation belongs to the CALLER, not to this package.
// noshell_test.go forbids `os` here, and that is the same rule stated from the
// other side: a reconciler that could read the filesystem directly would
// eventually write to it, and every write has to be a declared hostops verb.
type Inspector interface {
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	// ReadDir lists a directory. It exists for exactly one question the
	// planner cannot answer from the spec: which jobs a PREVIOUS apply wrote,
	// so that one the operator has since deleted can be taken back down.
	// Without it a reconciler can only ever converge by addition.
	ReadDir(path string) ([]fs.DirEntry, error)
	// AccountExists answers whether a unix account is present. It is here
	// rather than on hostops because it is an observation and hostops audits
	// its mutating verbs; a planner asking "does pzserver exist" should not
	// write an audit row.
	AccountExists(name string) bool
}

// FileAction writes one of the control-plane files gvm owns.
//
// The BYTES are rendered by the planner and carried here, so `--diff` showed
// exactly what Do will write. An action that rendered at Do time would make
// the diff a prediction rather than the plan.
type FileAction struct {
	Instance   string
	Job        string
	LegacyUnit string
	Kind       hostops.FileKind
	Actor      audit.Actor
	Host       Host

	Body []byte
	Path string

	// StepKind lets the drop-in identify itself, because Plan.Validate has to
	// be able to find it: the write-probe rule is about that one file.
	StepKind Kind

	change Change
	diff   string
	note   string
}

func (a *FileAction) Describe() Step {
	k := a.StepKind
	if k == "" {
		k = KindFile
	}
	return Step{
		Kind: k, Change: a.change, Instance: a.Instance,
		Object: a.Path, Note: a.note, Diff: a.diff,
	}
}

// Destructive is false. A file.write replaces contents; hostops keeps a .bak
// for the operator-editable kinds, and none of them is a world.
func (a *FileAction) Destructive() bool { return false }

func (a *FileAction) Do(ctx context.Context, _ *archive.Ref) error {
	_, err := a.Host.Do(ctx, hostops.Request{
		Verb: hostops.VerbFileWrite, Actor: a.Actor,
		Instance: a.Instance, Job: a.Job, LegacyUnit: a.LegacyUnit,
		File: a.Kind, Body: a.Body,
	})
	return err
}

// planFile builds a FileAction and computes its sigil and diff against what is
// on disk.
func planFile(k hostops.FileKind, stepKind Kind, name, job, legacy string, body []byte,
	actor audit.Actor, host Host, ins Inspector, m *Mask, note string) (Action, error) {

	path, err := hostops.FilePath(k, name, job, legacy)
	if err != nil {
		return nil, err
	}
	a := &FileAction{
		Instance: name, Job: job, LegacyUnit: legacy, Kind: k,
		Actor: actor, Host: host, Body: body, Path: path,
		StepKind: stepKind, change: Add, note: note,
	}
	cur, err := readCurrent(ins, path)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return a, nil
	}
	a.change = Update
	same := string(cur) == string(body)
	if stepKind == KindDropIn || k == hostops.FileJobTimer {
		// The generated units carry a `# rendered <timestamp>` line, which
		// differs on every render. Comparing on it would make every apply a
		// write, every write a new mtime, and the reconciler's daemon-reload
		// look like it had a reason on a box where nothing changed.
		same = SameDropIn(cur, body)
	}
	if same {
		a.change = Unchanged
		// The body becomes the bytes ALREADY ON DISK, not the freshly rendered
		// ones. The two differ only in the `# rendered <timestamp>` line this
		// comparison just decided to ignore -- but Do issues the write
		// regardless of change, and hostops decides with an exact bytes.Equal,
		// so handing it the new render would make every converged apply rewrite
		// the file: a new mtime and inode, a NeedDaemonReload systemd had no
		// reason for, and an audit row recording a write for a step the
		// operator was shown as "=". The planner's verdict and hostops' verdict
		// have to be the same decision.
		a.Body = cur
		return a, nil
	}
	a.diff = Unified(path, cur, body, m)
	return a, nil
}

// readCurrent returns the file's contents, or nil when it does not exist.
//
// Absence is nil-and-no-error because "not there yet" is the ordinary state of
// a first apply and is not a failure to report. Anything else IS reported: a
// drop-in gvm cannot read is a drop-in gvm cannot diff, and silently treating
// an EACCES as "the file is new" would print an `+` for a file that already
// governs a running server.
func readCurrent(ins Inspector, path string) ([]byte, error) {
	if ins == nil {
		return nil, nil
	}
	b, err := ins.ReadFile(path)
	switch {
	case err == nil:
		return b, nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	}
	return nil, fmt.Errorf("%w: reading %s to diff against: %w", ErrPlan, path, err)
}

// ReloadAction is the single daemon-reload an apply performs.
//
// One, not one per file: a three-file apply that reloaded three times would be
// three times as slow for no gain, and systemd's reload is the expensive part.
// It is declared instance-scoped in hostops even though the manager call is
// host-wide, because every reload gvm performs is the tail of one instance's
// apply and the audit row has to say whose.
type ReloadAction struct {
	Instance string
	Actor    audit.Actor
	Host     Host
	// Needed is false when nothing this apply wrote is a unit file, in which
	// case the step is reported and skipped rather than issued.
	Needed bool
}

func (a *ReloadAction) Describe() Step {
	ch, note := Update, "systemd re-reads the unit files this apply wrote"
	if !a.Needed {
		ch = Unchanged
		note = "no unit file changed, so systemd has nothing to re-read"
	}
	return Step{Kind: KindReload, Change: ch, Instance: a.Instance,
		Object: "systemctl daemon-reload", Note: note}
}

func (a *ReloadAction) Destructive() bool { return false }

func (a *ReloadAction) Do(ctx context.Context, _ *archive.Ref) error {
	if !a.Needed {
		return nil
	}
	_, err := a.Host.Do(ctx, hostops.Request{
		Verb: hostops.VerbDaemonReload, Actor: a.Actor, Instance: a.Instance,
	})
	return err
}

// UnitAction is one supervision verb on the instance's own unit.
type UnitAction struct {
	Instance string
	Verb     hostops.Verb
	Actor    audit.Actor
	Host     Host
	Note     string
	Change   Change
}

func (a *UnitAction) Describe() Step {
	ch := a.Change
	if ch == "" {
		ch = Update
	}
	return Step{
		Kind: KindUnit, Change: ch, Instance: a.Instance,
		Object: string(a.Verb) + " " + instance.UnitName(a.Instance), Note: a.Note,
	}
}

func (a *UnitAction) Destructive() bool { return false }

func (a *UnitAction) Do(ctx context.Context, _ *archive.Ref) error {
	_, err := a.Host.Do(ctx, hostops.Request{
		Verb: a.Verb, Actor: a.Actor, Instance: a.Instance,
	})
	return err
}

// ProbeFunc is the write-probe seam.
//
// It is a function type rather than the package function taken directly so a
// test can drive every branch -- a clean pass, one refused path, a probe that
// answered for four of six paths, a host with no systemd -- without a
// transient unit and without Linux.
type ProbeFunc func(ctx context.Context, p ProbeSpec) ([]ProbeResult, error)

// ProbeAction is D-15: prove the sandbox before writing the drop-in.
type ProbeAction struct {
	Spec  ProbeSpec
	Probe ProbeFunc

	// AllowUnprobed lets a caller proceed when the host cannot probe at all
	// (no systemd-run, not Linux). It defaults to FALSE, because "we could
	// not check" silently becoming "checked" is precisely the failure this
	// step exists to prevent. `gvm apply --diff` sets it, because a plan is
	// not an apply.
	AllowUnprobed bool
}

func (a *ProbeAction) Describe() Step {
	return Step{
		Kind: KindProbe, Change: Unchanged, Instance: a.Spec.Instance,
		Object: a.Spec.UnitName(),
		Note: fmt.Sprintf("transient unit as %s with ProtectHome=%s, ProtectSystem=%s, "+
			"writing and removing a probe file in each of: %v (D-15)",
			a.Spec.User, a.Spec.Sandbox.ProtectHome, a.Spec.Sandbox.ProtectSystem, a.Spec.Sandbox.Paths),
	}
}

// Destructive is false. The probe writes a file it then unlinks, and it is the
// step that runs BEFORE anything destructive could.
func (a *ProbeAction) Destructive() bool { return false }

func (a *ProbeAction) Do(ctx context.Context, _ *archive.Ref) error {
	probe := a.Probe
	if probe == nil {
		probe = ProbeWritable
	}
	results, err := probe(ctx, a.Spec)
	if err != nil {
		if errors.Is(err, platform.ErrNotSupported) && a.AllowUnprobed {
			return nil
		}
		return err
	}
	if missing := MissingProbePaths(a.Spec.Sandbox.Paths, results); len(missing) > 0 {
		// Silence is not approval. A probe that answered for four of six
		// paths has proved nothing about the other two, and the drop-in is
		// about to declare all six writable.
		return fmt.Errorf("%w: the probe reported nothing for %v, so those paths are unproven",
			ErrProbe, missing)
	}
	return ProbeError(a.Spec.Sandbox, results)
}

// RemoveAction is the only step shape that deletes data.
//
// It exists in this package so that a plan CAN contain a destroy -- `gvm
// rebuild` is an apply with one -- and it is written the way it is so that the
// archive is a parameter of the work rather than a precondition somebody
// remembered to check. Do hands the Ref straight to Remove, which hands it to
// instance.Destroy, which re-asserts the receipt and every target's proof.
type RemoveAction struct {
	Instance string
	Targets  []instance.Jailed
	Reason   string
}

func (a *RemoveAction) Describe() Step {
	return Step{
		Kind: KindRemove, Change: Delete, Instance: a.Instance,
		Object: fmt.Sprintf("%d path(s) under the destroy jail", len(a.Targets)),
		Note:   a.Reason,
	}
}

// Destructive is true, which is what makes Apply demand a verified archive
// before this action's Do is ever reached.
func (a *RemoveAction) Destructive() bool { return true }

func (a *RemoveAction) Do(ctx context.Context, ref *archive.Ref) error {
	return Remove(ctx, ref, a.Targets)
}
