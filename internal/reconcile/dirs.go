package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// rootOwnedDirs are the control-plane directories that belong to root and can
// be created before the instance's account exists.
//
// The order is not alphabetical and is not cosmetic: hostops creates ONE
// directory per call and refuses a missing parent (its mkdirLeaf, so that
// ensuring /var/log/gvm/<n> at 0750 cannot silently create /var/log/gvm at
// 0750 where section 5.1 says 0755). So DirState has to precede DirSnap and
// DirSteamScripts, which live inside it, and every root here has to have been
// created by the package's tmpfiles.d.
var rootOwnedDirs = []hostops.DirKind{
	hostops.DirSpecDir,
	hostops.DirSecrets,
	hostops.DirState,
	hostops.DirSnap,
	hostops.DirSteamScripts,
	hostops.DirBackups,
}

// instanceOwnedDirs belong to the account the game runs as.
//
// They are a separate list because they are a separate PHASE: hostops resolves
// their owner by reading the instance YAML, and chowns them to it, so both the
// spec file and the account have to exist first. For an adopted instance that
// account is pzserver, which is also why the owner is read rather than derived
// -- deriving gvm-pz-main would either fail or, worse, succeed and take a live
// world's directories away from the process using them.
var instanceOwnedDirs = []hostops.DirKind{
	hostops.DirSrv,
	hostops.DirLogs,
	hostops.DirSteamHome,
}

// managedGameDirs are the game's own install and data directories, and they are
// planned ONLY for a managed instance.
//
// They have to be created by something: D-15's write-probe lists both in the
// transient unit's ReadWritePaths=, and systemd refuses namespace setup
// (226/NAMESPACE) for a path that is not there -- so without these the FIRST
// apply of every new managed instance failed at the probe, before the drop-in,
// the reload and the enable. `gvm create` writes the YAML and touches nothing
// else, and both the spec and the plan put apply before install, so there is no
// earlier moment at which they could appear.
//
// They are excluded for an ADOPTED instance on purpose, and not merely as an
// optimisation. Its paths are /opt/pzserver and /home/pzserver/Zomboid --
// outside every root the package's tmpfiles.d creates, so mkdirLeaf's
// missing-parent refusal would fire on a legitimate host. More to the point,
// those directories existing is what adoption MEANS: gvm creating one would be
// gvm inventing the tree it was told to take over. The kinds are CreateOnly for
// the same reason, so even a managed re-apply never re-asserts a mode or an
// owner over a directory that is already there.
//
// They come after DirSrv because for a managed instance they live inside it.
var managedGameDirs = []hostops.DirKind{
	hostops.DirInstall,
	hostops.DirData,
}

// lateRootDirs are root-owned directories that live INSIDE an instance-owned
// one, so they cannot be created in the first phase.
//
// /var/log/gvm/<name>/jobs is the whole list and the reason is worth stating:
// the log directory belongs to the game (LogsDirectory= on the unit, and the
// runner writes console.log into it), while a job's log is root's and must not
// be readable by a compromised mod. So the parent is the instance's and the
// child is root's, and the order follows from that rather than from taste.
var lateRootDirs = []hostops.DirKind{
	hostops.DirJobLogs,
}

// DirAction ensures one directory exists with its declared mode and owner.
type DirAction struct {
	Instance string
	Kind     hostops.DirKind
	Actor    audit.Actor
	Host     Host

	// Path is what hostops will derive. It is carried so `--diff` can name
	// the directory without asking the host, and it comes from the same
	// derivation hostops uses, so the plan and the audit row agree.
	Path string
	// Present is what the planner saw. It decides the sigil only; the
	// authority on whether anything changed is hostops.Result.Changed.
	Present bool
}

func (a *DirAction) Describe() Step {
	ch := Add
	note := "created with the mode and owner section 5 gives it"
	if a.Present {
		ch = Unchanged
		note = "already present; the mode and owner are re-asserted"
	}
	return Step{
		Kind: KindDir, Change: ch, Instance: a.Instance,
		Object: a.Path, Note: note,
	}
}

// Destructive is false. dir.ensure creates; nothing here removes.
func (a *DirAction) Destructive() bool { return false }

func (a *DirAction) Do(ctx context.Context, _ *archive.Ref) error {
	_, err := a.Host.Do(ctx, hostops.Request{
		Verb: hostops.VerbDirEnsure, Actor: a.Actor,
		Instance: a.Instance, Dir: a.Kind,
	})
	return err
}

// dirActions builds the ordered directory steps for one phase.
//
// inst may be nil for a phase whose kinds are all name-derived; the two
// spec-resolved kinds need it, and report themselves as broken without it
// rather than guessing a path the verb would not touch.
func dirActions(kinds []hostops.DirKind, inst *spec.Instance, name string, actor audit.Actor, host Host, ins Inspector) []Action {
	out := make([]Action, 0, len(kinds))
	for _, k := range kinds {
		p, ok := hostops.DirPathFor(k, inst)
		if !ok {
			// Unreachable while the lists above name declared kinds, and a
			// silent skip would be a directory nobody creates and nothing
			// reports. hostops refuses the request too, so the failure is
			// loud either way; this keeps the plan honest about it.
			out = append(out, &brokenAction{
				step: Step{Kind: KindDir, Change: Warn, Instance: name,
					Object: string(k), Note: "hostops does not declare this directory kind"},
				err: fmt.Errorf("%w: dir kind %q", ErrPlan, k),
			})
			continue
		}
		out = append(out, &DirAction{
			Instance: name, Kind: k, Actor: actor, Host: host,
			Path: p, Present: dirPresent(ins, p),
		})
	}
	return out
}

func dirPresent(ins Inspector, p string) bool {
	if ins == nil {
		return false
	}
	fi, err := ins.Stat(p)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// Something other than absence -- a permission error on a parent,
			// a broken mount. Reporting it as "will be created" is the
			// conservative answer: the sigil is a prediction and hostops is
			// the authority.
			return false
		}
		return false
	}
	return fi.IsDir()
}

// brokenAction is a plan step that cannot run, carried so the failure appears
// in `--diff` rather than only at execution.
//
// A planner that returned an error instead would hide every other step of the
// plan behind the first broken one, and an operator fixing three things wants
// to see three things.
type brokenAction struct {
	step Step
	err  error
}

func (b *brokenAction) Describe() Step                         { return b.step }
func (b *brokenAction) Destructive() bool                      { return false }
func (b *brokenAction) Do(context.Context, *archive.Ref) error { return b.err }
