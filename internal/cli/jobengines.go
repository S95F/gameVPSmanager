// The work engines a job runs, and the honest gaps between them.
//
// internal/jobs declares one seam per kind -- Backups, Updates, Contents,
// Archives, Rebuilds, Sweeper -- and refuses to build a step table without the
// one its kind needs. This file is where each of those becomes a real engine,
// and it is the only place in gvm that knows how.
//
// Two of them are complete and two are not, and the difference is stated here
// rather than discovered at 05:03. `prune`, `verify` and `guard` need nothing
// but internal/archive and internal/lifecycle, both of which are built, so the
// host-level timers this phase ships work on a real box today. `backup` needs
// the instance's spec, its pinned manifest and its console channel, all of
// which exist. `update`, `content_sync` and `rebuild` need two adapters that do
// not: the provider context a SteamCMD run executes through, and internal/
// content's APPLYING half (write the acquisition list, materialize, resolve,
// activate). Both are named below in one sentinel each, so an operator gets
// "this build cannot do that yet, and here is what is missing" rather than a
// nil-pointer panic inside a nightly -- and so the day they land, the compiler
// points at the two places that change.
//
// That is the same choice `gvm install` already made with ErrProviderUnbuilt,
// and for the same reason: a command that exited 0 having done nothing is the
// worst possible answer.

package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/avail"
	"github.com/S95F/gameVPSmanager/internal/backup"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/rebuild"
	"github.com/S95F/gameVPSmanager/internal/socket"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/update"
)

// ErrContentApplyUnbuilt is a job that needs internal/content's applying half.
//
// Built on avail.New so it matches avail.ErrUnavailable and exits 3: the verb
// is built, its decision-only paths work, and what is missing is one adapter.
// errors.Is still tells it apart from the provider sentinel for the callers
// that care.
var ErrContentApplyUnbuilt = avail.New(
	"applying mod changes is not in this build; the decision-only, read-only half works")

// engineWired reports whether THIS BUILD can execute kind, and when it cannot,
// one plain sentence naming what is missing.
//
// It is the single predicate four callers share, so the answer to "does a
// nightly of this kind actually run" is given in one place (U1):
//   - attachEngines, which refuses the run rather than panic on a nil seam;
//   - `gvm job list` (jobRows), which prints "cannot run in this build: <why>"
//     beside a timer, so a timer that fires against a kind nothing runs is not
//     the least observable failure in the subsystem;
//   - `gvm apply`, which marks a timer it will install but cannot fire;
//   - `gvm start`'s missing-files refusal, which must not point back at an
//     install that is not here either.
//
// in and m are the instance and its pinned manifest. This build's answer turns
// only on kind, but a later phase keys content_sync on the manifest's content
// block (an instance that declares no content has nothing to sync), so the
// signature already carries them and today's callers may pass nil.
func engineWired(kind lifecycle.JobKind, _ *spec.Instance, _ *spec.Manifest) (bool, string) {
	if _, err := jobs.Lookup(kind); err != nil {
		// A kind with no runner at all (restore) — internal/jobs already holds
		// the one honest sentence naming the package that brings it, and it is
		// the sentence `gvm job run` would print, so this does not invent a
		// second one.
		return false, err.Error()
	}
	switch kind {
	case lifecycle.JobUpdate:
		return false, "this build has no engine to run SteamCMD, so update jobs cannot run"
	case lifecycle.JobContentSync:
		return false, "applying mod changes is not in this build; " +
			"`gvm content sync <instance> --dry-run` shows what it would do"
	case lifecycle.JobRebuild:
		return false, "rebuilding a world is not in this build"
	}
	return true, ""
}

// JobSeams are the engines and host handles one job run needs.
//
// Every field is nil in production and filled by attachEngines. They exist as
// overrides for one reason, and it is rule 5 of this project: the job layer's
// own invariant -- a failure must never leave a server stopped -- is asserted
// against fakes on the development machine, and the CLI's assembly of that
// layer has to be drivable the same way or the only untested part of the path
// is the part that wires it together.
type JobSeams struct {
	Super    jobs.Supervisor
	Backups  jobs.Backups
	Updates  jobs.Updates
	Contents jobs.Contents
	Archives jobs.Archives
	Rebuilds jobs.Rebuilds
	Sweeper  jobs.Sweeper
	// Locker takes the per-instance job lock. Nil is a real flock(2) on
	// /run/gvm/<n>/job.lock, which is why a test must supply one: creating
	// /run on the development machine is not a thing a test may do.
	Locker jobs.Locker
	// Repair is the `validate` path (D-32). *update.Pipeline is the
	// production implementation and needs the provider context.
	Repair Repairer
	// Desc reads /etc/gvm/jobs/<slug>.json.
	Desc jobs.DescFS
	// Log is where the job layer's structured records go. Nil is
	// slog.Default, which for a unit-invoked run is the journal -- which is
	// where `journalctl -u gvm-job@pz-main_nightly` reads them from.
	Log *slog.Logger
	// PID goes in the lease and the lock's holder line. Zero is os.Getpid.
	PID int
}

// engineOptions are the command-line flags that tune a work engine.
//
// They are carried on jobOptions and handed to attachEngines rather than being
// applied to the engine afterwards, because "afterwards" is a window in which
// a job could already have started: the engine is built, the flags are
// honoured, and only then is anything executed.
type engineOptions struct {
	// DeepVerify widens `gvm backup verify` from shallow-everything to
	// deep-everything.
	DeepVerify bool
	// Rebuild is section 13.6's own command line.
	Rebuild rebuild.Options
	// Restore is section 13.2's.
	Restore restoreOptions
}

// Repairer is the `validate` path as the CLI needs it: one method, which
// *update.Pipeline already has.
type Repairer interface {
	Repair(ctx context.Context, req update.RepairRequest) (*update.RepairReport, error)
}

// attachEngines fills in the one seam this kind needs.
//
// One kind, one seam, and nothing else: building a backup engine for a prune
// job would read a manifest, render its templates and open a console socket
// for a run that has no use for any of them -- and a failure in any of that
// would be reported as a prune that could not run.
func (h *HostEnv) attachEngines(ctx context.Context, jenv *jobs.Env, eo engineOptions) error {
	// A kind this build cannot execute at all is internal/jobs' refusal and
	// not this file's, and it is asked for FIRST so that the operator gets the
	// sentence naming the package that will bring it -- `pending` in
	// execute.go is the single place that tracks those gaps, and a second
	// message here would be a second place to update.
	if _, err := jobs.Lookup(jenv.Kind); err != nil {
		return err
	}
	switch jenv.Kind {
	case lifecycle.JobGuard:
		jenv.Sweeper = h.sweeper(jenv)
		return nil

	case lifecycle.JobPrune, lifecycle.JobVerify:
		store, err := h.archiveStore(jenv, eo)
		if err != nil {
			return err
		}
		jenv.Archives = store
		return nil

	case lifecycle.JobBackup:
		eng, err := h.backupEngine(ctx, jenv)
		if err != nil {
			return err
		}
		jenv.Backups = eng
		return nil

	case lifecycle.JobRestart:
		// The ladder and the start, and nothing else. It is the one kind with
		// no work engine at all.
		return nil

	case lifecycle.JobUpdate:
		if h.Jobs.Updates != nil {
			jenv.Updates = h.Jobs.Updates
			return nil
		}
		return fmt.Errorf("%w: %s needs a provider context to run SteamCMD through "+
			"(internal/update.Bind over steamcmd.New + jobs.NewSteamRunner), which is the "+
			"same wiring `gvm install` is waiting on. `gvm job list` says which kinds this "+
			"build can run", ErrProviderUnbuilt, jenv.Slug)

	case lifecycle.JobContentSync:
		if h.Jobs.Contents != nil {
			jenv.Contents = h.Jobs.Contents
			return nil
		}
		return fmt.Errorf("%w: %s needs it. `gvm content sync %s --dry-run` is the whole of "+
			"the DECISION half and is read-only, so it answers what this job would do",
			ErrContentApplyUnbuilt, jenv.Slug, jenv.Instance)

	case lifecycle.JobRebuild:
		if h.Jobs.Rebuilds != nil {
			if j, ok := h.Jobs.Rebuilds.(*rebuild.Job); ok {
				j.Opt = eo.Rebuild
			}
			jenv.Rebuilds = h.Jobs.Rebuilds
			return nil
		}
		return fmt.Errorf("%w: a rebuild resolves and downloads the whole content list before "+
			"it deletes anything (section 13.6), so it needs the same adapter. Nothing was "+
			"stopped and %s is untouched", ErrContentApplyUnbuilt, jenv.Instance)
	}
	// Unreachable through Lookup, which refuses an unknown kind before this
	// is called. Checked because a kind added to internal/jobs without a row
	// here would otherwise run with a nil seam, and the step table's own
	// refusal ("a backup job needs a backup engine") would be the first thing
	// to say so -- after the lock and the lease had been taken.
	return fmt.Errorf("%w: this build has no work engine wired for the %s kind",
		ErrNotImplemented, jenv.Kind)
}

// sweeper is section 13.4's recovery sweep: lifecycle's reconciler for the
// enumeration, lifecycle's manager for the starts.
func (h *HostEnv) sweeper(jenv *jobs.Env) jobs.Sweeper {
	if h.Jobs.Sweeper != nil {
		return h.Jobs.Sweeper
	}
	return &jobs.Sweep{
		Rec:   &lifecycle.Reconciler{Obs: h.observer()},
		Super: h.supervisor(jenv.Actor, jenv.Slug, false),
		Files: h.files(),
		Paths: h.Paths,
		// Host is how a dead holder's lease.json is removed before the start
		// it would otherwise have refused. Audited, through the same verb
		// `gvm job clear` uses, and preferred over starting the supervisor
		// with Force -- which would also override every other refusal in that
		// table, present and future.
		Host:  h.Ops,
		Actor: jenv.Actor,
	}
}

// archiveStore is the `prune` and `verify` kinds' view of /var/backups/gvm.
//
// The retention policy is read per instance from its own spec, through a
// closure rather than up front, because the host-level sweep covers instances
// this run has not loaded yet -- and one unreadable spec must not stop the
// others being pruned.
func (h *HostEnv) archiveStore(jenv *jobs.Env, eo engineOptions) (jobs.Archives, error) {
	if h.Jobs.Archives != nil {
		return h.Jobs.Archives, nil
	}
	shallow := archive.VerifyOptions{Mode: archive.ModeShallow}
	if eo.DeepVerify {
		shallow.Mode = archive.ModeDeep
	}
	return &jobs.ArchiveStore{
		Shallow: shallow,
		Deep:    archive.VerifyOptions{Mode: archive.ModeDeep},
		// The receipt a standalone prune runs after, read back shallow:
		// seconds per archive, and enough to establish that the newest one is
		// history rather than a truncated file.
		Receipt: archive.VerifyOptions{Mode: archive.ModeShallow},
		Root:    instance.BackupRoot,
		Files:   h.files(),
		Dir:     func(name string) string { return h.backupDir(name) },
		Policy: func(name string) (archive.Policy, error) {
			in, _, err := loadInstance(h, name)
			if err != nil {
				return archive.Policy{}, err
			}
			return archive.Policy{
				Keep:     in.Spec.Backup.Keep,
				KeepDays: in.Spec.Backup.KeepDays,
			}, nil
		},
		Now: h.Now,
		Notes: func(s string) {
			// Into the job's own warnings would be better; the job has not
			// been built when the store is. The journal is where an operator
			// reading `journalctl -u gvm-job@_verify` finds them, which is
			// where they were going to look anyway.
			jobNote(jenv, s)
		},
	}, nil
}

// backupDir honours spec.backup.dir, falling back to the standard layout.
//
// A failure to read the spec falls back rather than refusing: the host-level
// sweep walks directories under /var/backups/gvm, and a decommissioned
// instance whose YAML is gone still has archives somebody may want verified.
func (h *HostEnv) backupDir(name string) string {
	if in, _, err := loadInstance(h, name); err == nil {
		if d := strings.TrimSpace(in.Spec.Backup.Dir); d != "" {
			return d
		}
	}
	return h.paths(name).Backups
}

// jobNote puts a sentence where an operator will find it.
func jobNote(jenv *jobs.Env, s string) {
	if jenv != nil && jenv.Log != nil {
		jenv.Log.Info(s, "slug", jenv.Slug)
	}
}

// backupEngine is section 13.2's engine, wired to this instance.
//
// The console channel is the interesting half. The freeze rungs -- `save-off`,
// `save-all`, `save-on` -- go through the runner's own console socket, which
// is the process that owns the PTY; a backup that sent them any other way
// would be sending them to a game that is not listening. A socket that cannot
// be dialled is NOT a failure here: a stopped instance has no runner, and
// archiving a stopped world needs no freeze at all. internal/backup records
// the absence and the freeze plan decides what it means.
func (h *HostEnv) backupEngine(ctx context.Context, jenv *jobs.Env) (jobs.Backups, error) {
	if h.Jobs.Backups != nil {
		return h.Jobs.Backups, nil
	}
	in, raw, err := loadInstance(h, jenv.Instance)
	if err != nil {
		return nil, err
	}
	m, err := pinnedManifest(h, in)
	if err != nil {
		return nil, err
	}
	ch, closeCh := h.freezeChannels(ctx, jenv)
	if closeCh != nil {
		h.closers = append(h.closers, closeCh)
	}
	return backup.New(backup.Config{
		Instance: jenv.Instance,
		Game:     in.Spec.Game,
		Spec:     in,
		Manifest: m,
		Renderer: spec.NewRenderer(renderData(jenv.Instance, in, spec.SecretValues{})),
		SpecYAML: string(raw),
		Reason:   string(archive.ClassRoutine),
		Paths:    h.paths(jenv.Instance),
		Channels: ch,
		Audit:    h.audit(),
		Actor:    jenv.Actor,
		Now:      h.Now,
		PID:      jenv.PID,
		// The pin that makes the lease watchdog's early thaw legal. Zero is
		// the fail-safe answer and is what the error path gives: off Linux
		// there is no /proc, and a lease the watcher cannot pin falls back to
		// its deadline rather than being read as owner-gone (see
		// backup.Watcher.alive).
		StartTicks: startTicksOf(jenv.PID),
		Unit:       jenv.Unit,
		Log:        jenv.Log,
	}), nil
}

// startTicksOf is /proc/<pid>/stat field 22, or zero when it cannot be read.
func startTicksOf(pid int) uint64 {
	if pid <= 0 {
		return 0
	}
	ticks, err := platform.StartTicks(pid)
	if err != nil {
		return 0
	}
	return ticks
}

// freezeChannels dials the console socket for the freeze rungs.
//
// The LogWatcher is deliberately absent and the gap is recorded rather than
// hidden: it needs a second connection following the ring, and internal/backup
// already treats a nil one as "every marker degrades to a plain, RECORDED
// wait" -- which produces a sentence in the result saying the flush was given
// a timeout and assumed, instead of a claim that a save marker was seen. A
// stale world beats no world; a world reported saved when nothing watched for
// the marker is the failure that discipline exists to prevent.
func (h *HostEnv) freezeChannels(ctx context.Context, jenv *jobs.Env) (backup.Channels, func() error) {
	ch := backup.Channels{Actor: "gvm-backup", Now: h.Now}
	c, err := socket.Dial(ctx, h.paths(jenv.Instance).ConsoleSock)
	if err != nil {
		return ch, nil
	}
	// The hello gets the same bound the rungs do. A runner whose pty write is
	// wedged accepts the connection and never answers, and this call runs
	// during ENGINE CONSTRUCTION -- before the lease is armed and before any
	// step with an Undo exists -- so an unbounded one would hang the job
	// somewhere nothing could report it. A failed hello is already handled:
	// it reads as "no console channel", which internal/backup records as a
	// degraded flush and carries on, because a stale world beats no world.
	c.SetCallTimeout(freezeCallTimeout)
	if _, err := c.Hello(clientID()); err != nil {
		_ = c.Close()
		return ch, nil
	}
	ch.Console = consoleSender{c: c}
	return ch, c.Close
}

// freezeCallTimeout bounds a freeze rung's round trip when the job's own
// context carries no deadline.
//
// Two minutes is the shipped manifests' own lease_budget, which is the honest
// source: a rung that has not landed inside the window the freeze is allowed
// to last has already failed for the purpose it was sent for. The point is
// that a wedged runner costs one DEGRADED rung and not the instance lock until
// TimeoutStartSec.
const freezeCallTimeout = 2 * time.Minute

// consoleSender is backup.ConsoleSender over the console socket.
//
// It returns the sequence the echo landed at, which is what lets a marker
// search start AFTER the send rather than matching a line that was already in
// the ring -- the difference between observing this save complete and
// observing the previous autosave's.
type consoleSender struct{ c *socket.Client }

// The context is NOT discarded. internal/backup hands this the job's context,
// and the thaw's Undo hands it a detached one with its own budget -- so the
// remaining time is the right bound for the rung, and the named constant is
// the fallback for a context with no deadline at all. Ignoring it is how a
// wedged runner used to hold the instance lock and lease.json for four hours.
func (s consoleSender) Send(ctx context.Context, actor, line string) (uint64, error) {
	d := freezeCallTimeout
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left > 0 && left < d {
			d = left
		}
	}
	s.c.SetCallTimeout(d)
	return s.c.Send(actor, line, nil)
}

// repairPipeline is the `validate` path's engine.
//
// It needs the provider context a SteamCMD run executes through, which is the
// same wiring `gvm install` is waiting on -- so the refusal names it rather
// than failing inside a run that had already taken a preserve archive.
func (h *HostEnv) repairPipeline(_ context.Context, name string, _ jobOptions) (Repairer, error) {
	if h.Jobs.Repair != nil {
		return h.Jobs.Repair, nil
	}
	return nil, fmt.Errorf("%w: `gvm update %s --repair` runs SteamCMD with `validate`, which "+
		"needs the provider context (internal/update.Bind over steamcmd.New + "+
		"jobs.NewSteamRunner). Nothing was written and the install tree is untouched",
		ErrProviderUnbuilt, name)
}

// Compile-time proof that the production console wiring is what the freeze
// asks for. The interface lives in internal/backup beside the rung that calls
// it, so this is what keeps the two from drifting.
var _ backup.ConsoleSender = consoleSender{}
