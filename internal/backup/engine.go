// Package backup is section 13.2: the backup engine and the out-of-process
// freeze lease.
//
// It is the generalisation of legacy/projectzomboid-bash/backup.sh, and it is
// worth saying what that script got right before saying what it got wrong,
// because three of its properties are preserved here deliberately.
//
// It flushed before archiving. It archived before pruning -- `tar` precedes
// `find | rm`, so a failed backup could not erode history. And it chmod 600'd
// the tarball. All three are kept: the first as a manifest-declared flush with
// a marker, the second as archive.Prune's requirement of a VERIFIED Ref it can
// only be handed by a create that succeeded, the third as
// archive.ArchiveFileMode.
//
// What it got wrong is in four places, and each is a named decision here:
//
//   - `sleep 15`. A number chosen once against one world, simultaneously too
//     long on a small one and too short on a large one. Replaced by the
//     manifest's wait_for_log marker, with the blind wait kept only where no
//     marker exists -- and RECORDED there, so an assumed flush is visibly an
//     assumption (freeze.go).
//   - No unfreeze guarantee. backup.sh's `save` is a one-shot flush so it had
//     nothing to unfreeze; a Minecraft `save-off` with no `save-on` leaves a
//     running server that silently stops persisting. Fixed by a defer AND an
//     out-of-process lease the runner honours, because a defer dies with the
//     process it is defending (lease.go, D-31).
//   - No integrity story. A truncated tarball was indistinguishable from a
//     good one. internal/archive's manifest, member digest table and
//     shallow/spot/deep verification are that story; this package does not
//     write a second archiver.
//   - A freeze the length of the whole tar, on every filesystem. Replaced by
//     reflink and btrfs snapshots where the host can, with the stall figure
//     recorded either way (snapshot.go).
//
// Nothing in here archives, verifies or prunes by itself. internal/archive
// already does all three, including "preserve is never pruned" and "prune only
// after a verified create", and a second implementation of a retention policy
// is a second policy.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Engine errors.
var (
	// ErrConfig is an engine that cannot run: no manifest, no renderer, a
	// backup block that names nothing. Every one of these is caught in the
	// preflight, before anything is frozen.
	ErrConfig = errors.New("backup: the engine is not configured to run")
	// ErrNoRoom is the preflight's refusal. It is the one refusal that
	// happens before the freeze on purpose: discovering a full disk halfway
	// through a tar leaves a truncated archive where a verified one was
	// promised, and under `direct` it leaves it there with autosave off.
	ErrNoRoom = errors.New("backup: not enough free space for this archive")
	// ErrUnplaced is a rendered backup.include path that falls under none of
	// the instance's declared roots. It is an error and never a warning: the
	// Paper-family `_nether` and `_the_end` siblings exist precisely because
	// a backup that quietly skips part of a world is the failure mode that
	// takes months to notice (section 13.2).
	ErrUnplaced = errors.New("backup: a declared backup path is under none of the instance's roots")
)

// DefaultHeadroom scales the size estimate when checking free space.
//
// The estimate is the UNCOMPRESSED size of the source tree, which is already a
// generous proxy for a gzipped archive -- a Project Zomboid world compresses
// to roughly a third. Ten percent on top covers the manifest, the member
// table, and a world that grew between the walk and the write. A larger factor
// would start refusing backups on the small volumes this tool exists for.
const DefaultHeadroom = 1.1

// DefaultSnapGrace is how long a leftover clone must have existed before the
// startup sweep removes it.
//
// It is not tidiness: another job on the same instance cannot be running,
// because the job lock serialises them, but `gvm backup` run by hand while a
// nightly is mid-clone must not have its clone deleted underneath it. An hour
// is far beyond any clone's useful life and well short of a disk filling up.
const DefaultSnapGrace = time.Hour

// Config is everything one backup run needs.
//
// Every host-touching member is a seam with a working fake, which is rule 5 of
// this project rather than a testing convenience: the freeze ordering, the
// lease arming and the never-leaves-the-world-unsaved rule all have to be
// assertable on the Windows development machine, because they are the rules
// whose violation is only discovered on the box it is already too late to
// discover it on.
type Config struct {
	Instance string
	// Game is the game id, for the archive manifest's subject.
	Game string

	// Spec is the instance YAML: the account the lease is handed to, the
	// game's own paths, the retention numbers, the remote target.
	Spec *spec.Instance
	// Manifest is the game manifest: what to archive, what to exclude, the
	// world paths, and the freeze.
	Manifest *spec.Manifest
	// Renderer expands the manifest's templates against this instance.
	Renderer *spec.Renderer
	// SpecYAML is the instance document, embedded in the archive so a restore
	// on a box with no gvm has the definition beside the bytes.
	SpecYAML string

	// Reason is what this run is for, as it appears in the freeze lease:
	// "routine" for a nightly, "pre-update" for the archive an update takes
	// before it touches anything. Empty is "routine".
	Reason string

	// Paths is the control-plane path set. Zero means PathsFor(Instance),
	// which is what production wants; a test overrides it to a temp
	// directory.
	Paths instance.Paths

	FS FS
	// Channels delivers the freeze rungs.
	Channels Channels
	// Remote is the off-box target, or nil for none.
	Remote *Remote

	Log   *slog.Logger
	Audit *audit.Logger
	Actor audit.Actor
	Now   func() time.Time
	// PID and Unit go in the freeze lease, so an operator who finds one knows
	// which process armed it and which journal to read.
	PID  int
	Unit string
	// StartTicks pins PID against pid reuse (/proc/<pid>/stat field 22), and
	// it is what makes the watchdog's EARLY thaw legal: without it the
	// watcher can only ever fall back to the deadline, because a zero pin
	// cannot distinguish a dead owner from a recorded-nothing one. Injected
	// rather than read inside Freeze, so the engine carries no /proc read
	// that cannot succeed on the development machine. Zero is the safe
	// direction and is recorded as a note.
	StartTicks uint64

	// Verify is how the archive is read back. Zero is shallow, which is
	// section 13.2's default for a nightly.
	Verify archive.VerifyOptions
	// Retention overrides the instance's own keep/keep_days. Zero takes them
	// from Spec.Backup.
	Retention *archive.Policy
	// AllowMethods restricts the snapshot choice. Nil allows all three.
	AllowMethods []Method
	// Headroom scales the size estimate. Zero is DefaultHeadroom.
	Headroom float64
	// FloorMBps is the throughput floor the direct lease extension is derived
	// from. Zero is DefaultFloorMBps.
	FloorMBps int

	// Probe and Clone are the two Linux-only operations, injected so the
	// ordering around them is testable off Linux. Nil uses the package's own,
	// which refuse on a non-Linux host -- and the engine's handling of that
	// refusal is itself one of the behaviours worth testing.
	Probe func(worldPaths []string, snapDir string) (Capability, error)
	Clone func(ctx context.Context, m Method, src, dst string) ([]string, error)

	// Notes receives every non-fatal observation: a missing optional include,
	// a degraded flush, a filesystem that cannot clone. They belong in the
	// job log, not in an error.
	Notes func(string)
}

// Result is what a run did, flat enough to go straight into a job summary.
type Result struct {
	Instance string `json:"instance"`
	Snapshot SnapshotResult
	Flush    FlushResult
	Thaw     ThawResult
	// Lease is the freeze lease that was armed, nil when the game's freeze is
	// `{before: none, after: none}` and there was nothing to guard.
	Lease *Lease `json:"freeze_lease,omitempty"`
	// LeaseWhy is BudgetFor's sentence: why the deadline is the number it is.
	LeaseWhy string `json:"freeze_lease_why,omitempty"`
	// EstimateBytes is the uncompressed source size the preflight measured.
	EstimateBytes int64 `json:"estimate_bytes"`
	Archive       *archive.Ref
	Prune         *archive.PruneResult `json:"prune,omitempty"`
	Push          *PushResult          `json:"push,omitempty"`
	Notes         []string             `json:"notes,omitempty"`
}

// Engine performs one backup. It is single-use and not safe for concurrent
// use: a second backup of the same instance is prevented one level up by the
// job lock (D-30), not by a mutex here.
//
// It satisfies internal/jobs' Backups interface structurally. That interface
// is declared next to the step table that calls it rather than here, and this
// package deliberately does NOT import internal/jobs: the import would pull
// the job layer -- and therefore os/exec -- into the dependency graph of a
// package whose whole job is to not spawn anything. The contract is asserted
// from a test instead, which proves the same thing and costs nothing at
// runtime.
type Engine struct {
	cfg Config

	mu      sync.Mutex
	notes   []string
	prepped bool

	freeze  FreezePlan
	plan    Plan
	sources []archive.Source
	fp      *archive.Fingerprint
	est     int64

	// thawMu serialises Thaw's three callers. It is separate from mu because
	// mu must not be held while rungs are on the wire, and two callers
	// sending the same rung at once would be a second kind of wrong answer.
	thawMu sync.Mutex

	lease     *Lease
	leaseWhy  string
	frozenAt  time.Time
	thawed    bool
	thawRes   ThawResult
	flushRes  FlushResult
	snapRes   SnapshotResult
	ref       *archive.Ref
	pruneRes  *archive.PruneResult
	pushRes   *PushResult
	cloneMade string
}

// New builds an engine. It validates nothing: every refusal belongs in
// Preflight, which runs inside the job's step table where a failure is
// recorded, audited and visible -- rather than at construction time, where it
// would be a bare error from a CLI with no run to attach it to.
func New(cfg Config) *Engine {
	if cfg.Paths.Name == "" && cfg.Instance != "" {
		cfg.Paths = instance.PathsFor(cfg.Instance)
	}
	if cfg.FS == nil {
		cfg.FS = OS
	}
	if cfg.Probe == nil {
		cfg.Probe = Probe
	}
	if cfg.Clone == nil {
		cfg.Clone = Clone
	}
	return &Engine{cfg: cfg}
}

func (e *Engine) now() time.Time {
	if e.cfg.Now != nil {
		return e.cfg.Now()
	}
	return time.Now()
}

func (e *Engine) log() *slog.Logger {
	if e.cfg.Log != nil {
		return e.cfg.Log
	}
	return slog.Default()
}

// note records a non-fatal observation, once, in order.
func (e *Engine) note(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	e.mu.Lock()
	e.notes = append(e.notes, s)
	e.mu.Unlock()
	if e.cfg.Notes != nil {
		e.cfg.Notes(s)
	}
	e.log().Info("backup: "+s, "instance", e.cfg.Instance)
}

// Result is what the run did so far. Safe to call at any point, including from
// a deferred function after a failure, which is when a job record is written.
func (e *Engine) Result() Result {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Result{
		Instance:      e.cfg.Instance,
		Snapshot:      e.snapRes,
		Flush:         e.flushRes,
		Thaw:          e.thawRes,
		Lease:         e.lease,
		LeaseWhy:      e.leaseWhy,
		EstimateBytes: e.est,
		Archive:       e.ref,
		Prune:         e.pruneRes,
		Push:          e.pushRes,
		Notes:         slices.Clone(e.notes),
	}
}

// Preflight decides everything and changes nothing.
//
// It is the near side of the whole subsystem: the freeze plan is rendered, the
// source set is resolved, the filesystem is probed, the snapshot method is
// chosen and the free space is checked -- all before a single `save-off` is
// sent. A refusal here costs an operator a job-failed row. The same refusal
// one step later costs them a world that is not saving while gvm works out
// what to do about it.
func (e *Engine) Preflight(ctx context.Context) error {
	if err := e.validate(); err != nil {
		return err
	}
	p := e.cfg.Paths

	fz, err := BuildFreeze(e.cfg.Manifest, e.cfg.Renderer)
	if err != nil {
		return err
	}
	e.freeze = fz
	if !fz.Frozen() {
		e.note("this game declares no freeze, so the archive is taken from the live tree with " +
			"no flush and no lease; its own autosave is the only consistency guarantee")
	}

	srcs, notes, err := BuildSources(e.cfg.Manifest, e.cfg.Renderer, e.cfg.Spec.Spec.Paths)
	if err != nil {
		return err
	}
	for _, n := range notes {
		e.note("%s", n)
	}
	e.sources = srcs

	// The estimate reuses internal/archive's own walker rather than adding a
	// second one. It over-counts by whatever the excludes would have dropped,
	// which is the safe direction for both of the things it feeds: a disk
	// check and a lease budget.
	est, fp, err := estimate(ctx, srcs)
	if err != nil {
		return err
	}
	e.est = est
	e.fp = fp

	worlds, wnotes := e.worldPaths()
	for _, n := range wnotes {
		e.note("%s", n)
	}
	capab, perr := e.cfg.Probe(worlds, p.SnapDir)
	if perr != nil {
		// A probe that failed is not a backup that failed. The method is
		// `direct`, which always works, and the reason is recorded so that
		// "why is my nightly stalling the world for a minute" has an answer
		// in the job log.
		e.note("the filesystem under the world paths could not be probed (%v), so the archive "+
			"is taken inside the freeze", perr)
	}
	method, why := Choose(capab, e.cfg.AllowMethods)
	e.plan = Plan{
		Method:  method,
		Why:     appendNote(why, capab.Note),
		Cap:     capab,
		SnapDir: p.SnapDir,
		Sources: worlds,
	}
	if method.ThawBeforeArchive() {
		e.plan.Clone = ClonePathFor(p.SnapDir, e.cfg.Instance, e.now())
	}
	e.snapRes = SnapshotResult{Method: method, Why: e.plan.Why}

	// The startup sweep of section 13.2's "a defer plus a startup sweep". The
	// defer covers an error and a panic; it does not cover SIGKILL, and a
	// 2.4 GB clone left behind once a week fills a VPS in a month.
	if removed, serr := SweepSnapDir(e.cfg.FS, p.SnapDir, e.cfg.Instance, e.now(), DefaultSnapGrace); serr != nil {
		e.note("a leftover snapshot clone could not be swept: %v", serr)
	} else if len(removed) > 0 {
		e.note("swept %d leftover snapshot clone(s) from %s: %s",
			len(removed), p.SnapDir, strings.Join(removed, ", "))
	}

	if err := e.checkRoom(method); err != nil {
		return err
	}
	e.prepped = true
	return nil
}

// validate is the configuration half of the preflight.
func (e *Engine) validate() error {
	var errs []error
	if e.cfg.Instance == "" {
		errs = append(errs, fmt.Errorf("%w: no instance name", ErrConfig))
	}
	if e.cfg.Spec == nil {
		errs = append(errs, fmt.Errorf("%w: no instance spec; the account the freeze lease is "+
			"handed to and the game's own paths both come from it", ErrConfig))
	}
	if e.cfg.Manifest == nil {
		errs = append(errs, fmt.Errorf("%w: no game manifest; what gets archived is "+
			"manifest-declared (section 9.1) and guessing it is how a world loses a dimension",
			ErrConfig))
	}
	if e.cfg.Paths.Backups == "" {
		errs = append(errs, fmt.Errorf("%w: no archive directory", ErrConfig))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if e.cfg.Spec.Spec.User == "" {
		// The lease has to be readable by the account the game runs as, and
		// that account is READ from the spec rather than derived from the
		// instance name: an adopted pz-main runs as pzserver (section 6.4).
		return fmt.Errorf("%w: %s names no user, so the freeze lease could not be handed to the "+
			"account the runner reads it as", ErrConfig, e.cfg.Paths.Spec)
	}
	return nil
}

// worldPaths renders the manifest's world_paths, which is what the filesystem
// probe is about: the capability that matters is the one under the WORLD, not
// under /var/lib/gvm.
func (e *Engine) worldPaths() ([]string, []string) {
	var out, notes []string
	for i, tmpl := range e.cfg.Manifest.Spec.Backup.WorldPaths {
		field := fmt.Sprintf("spec.backup.world_paths[%d]", i)
		p, _, err := renderRung(e.cfg.Renderer, field, tmpl)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s could not be rendered (%v), so it was left out "+
				"of the filesystem probe", field, err))
			continue
		}
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		// Core Keeper and the generic Steam manifest declare none. The probe
		// then has nothing to ask about and the answer is `direct`.
		notes = append(notes, "this game declares no world_paths, so the filesystem could not be "+
			"probed for a fast snapshot")
	}
	return out, notes
}

// diskUsage measures the filesystem holding p, or the nearest ancestor that
// exists.
//
// The walk up is not defensiveness: /var/backups/gvm/<name> is created by the
// first archive, so on the first ever backup of an instance the directory is
// not there -- and a preflight that gave up at that point would skip the one
// check it exists to make, on the one run where the operator has least idea
// how much space a world needs.
func (e *Engine) diskUsage(p string) (platform.DiskUsage, error) {
	var lastErr error
	for cur := p; ; {
		du, err := e.cfg.FS.DiskUsage(cur)
		if err == nil {
			return du, nil
		}
		lastErr = err
		next := dirOf(cur)
		if next == cur {
			return platform.DiskUsage{}, lastErr
		}
		cur = next
	}
}

// checkRoom is the free-space refusal.
func (e *Engine) checkRoom(m Method) error {
	headroom := e.cfg.Headroom
	if headroom <= 0 {
		headroom = DefaultHeadroom
	}
	du, err := e.diskUsage(e.cfg.Paths.Backups)
	if err != nil {
		// Unknown free space is not a reason to refuse. On a host where
		// statfs cannot be asked the create itself will fail with ENOSPC,
		// which internal/archive turns into a removed partial file and a
		// named error -- a worse outcome than a preflight, but not a worse one
		// than not backing up at all.
		e.note("free space under %s could not be measured (%v), so the %d-byte estimate was not "+
			"checked against it", e.cfg.Paths.Backups, err, e.est)
		return nil
	}
	if du.Sufficient(e.est, headroom) {
		return nil
	}
	short := du.Shortfall(e.est, headroom)
	return fmt.Errorf("%w: %s holds %d bytes free and this archive needs about %d (%d source "+
		"bytes plus %.0f%% headroom); %d bytes short. Refused here, before the freeze: under a "+
		"%s snapshot a disk that fills mid-archive leaves a truncated tarball AND a world with "+
		"saving switched off",
		ErrNoRoom, e.cfg.Paths.Backups, du.Avail, int64(float64(e.est)*headroom), e.est,
		(headroom-1)*100, short, m)
}

// Freeze arms the lease and runs the manifest's flush.
//
// The ORDER inside this function is D-31 and it is the only order that works:
// the lease is written BEFORE the first rung is sent. A lease armed afterwards
// leaves a window exactly as wide as the send that turns persistence off,
// which is the window the whole mechanism exists to close.
func (e *Engine) Freeze(ctx context.Context) error {
	if !e.prepped {
		return fmt.Errorf("%w: Freeze ran before Preflight, so nothing has been decided yet",
			ErrConfig)
	}
	if !e.freeze.Frozen() {
		// Nothing to suspend and nothing to guard. Arming a lease here would
		// put a watchdog on a world nobody touched and show a freeze in every
		// `gvm status` during every nightly.
		e.frozenAt = e.now()
		e.thawed = true
		e.thawRes = ThawResult{Thawed: true}
		return nil
	}

	at := e.now()
	budget, why := BudgetFor(e.freeze.Budget, e.plan.Method, e.est, e.cfg.FloorMBps)
	e.leaseWhy = why

	if e.cfg.PID > 0 && e.cfg.StartTicks == 0 {
		// Worth a line in the record, because it changes what the watchdog
		// can do: an unpinned owner reads as "cannot establish", so the early
		// thaw is unavailable and the world waits out the whole budget if
		// this job dies.
		e.note("the freeze lease is UNPINNED (no start-ticks for pid %d), so only its deadline "+
			"will thaw it -- an owner that dies cannot be proved dead", e.cfg.PID)
	}
	l, notes, err := Arm(ctx, e.cfg.FS, ArmRequest{
		Path:  e.cfg.Paths.FreezeLease,
		Owner: e.cfg.Spec.Spec.User,
		Lease: Lease{
			Instance:        e.cfg.Instance,
			Reason:          "backup:" + e.reason(),
			OwnerPID:        e.cfg.PID,
			OwnerStartTicks: e.cfg.StartTicks,
			OwnerUnit:       e.cfg.Unit,
			ArmedAt:         at,
			Deadline:        at.Add(budget),
			ThawSteps:       e.freeze.ThawSteps(),
		},
	})
	for _, n := range notes {
		e.note("%s", n)
	}
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.lease = l
	e.frozenAt = at
	e.mu.Unlock()
	e.note("freeze lease armed at %s with a %s deadline: %s",
		at.UTC().Format(time.RFC3339), budget, why)

	flush := RunRungs(ctx, e.cfg.Channels, e.freeze.Before)
	e.mu.Lock()
	e.flushRes = flush
	e.mu.Unlock()
	if flush.Degraded {
		// Not a failure. A backup of an unflushed world is worth far more
		// than no backup, and the flag plus the sentence is what turns "the
		// world rolled back" into something with an entry to find.
		e.note("the pre-backup flush is DEGRADED: %s", flush.Why)
	}
	return nil
}

// reason is the lease's reason string: "backup:routine", "backup:pre-update".
//
// It comes from the configuration rather than from the Snapshot call's own
// reason because the lease is armed BEFORE the snapshot step runs, and an
// operator who finds a lease wants to know which job armed it -- "a nightly"
// and "the backup a pending update took first" lead to different places.
func (e *Engine) reason() string {
	if r := strings.TrimSpace(e.cfg.Reason); r != "" {
		return r
	}
	return "routine"
}

// Thaw lifts the freeze and clears the lease. It is idempotent.
//
// Three callers reach it and that is the design: Snapshot calls it as its last
// act so the freeze ends when the bytes are captured, the job's step Undo
// calls it on every unwind path including a panic, and the runner's lease timer
// is the third -- out of process, and the only one that survives this process
// being killed.
// `thawed` means "the rung LANDED", never "the rung was attempted". That
// distinction is the whole of this function's correctness: Snapshot's thaw runs
// on the job's own context, which systemd's TimeoutStartSec may already have
// cancelled, while the step Undo runs on context.WithoutCancel with a fresh
// budget precisely so it can still deliver `save-on`. Latching before the
// attempt would hand that one retry a nil return and throw it away -- which is
// the one thing freeze.go's Thaw says it must never do, report success for a
// command it could not deliver. Re-sending is free: `save-on` to a server that
// is already saving is a no-op, and not sending it is a world accepting players
// and not persisting.
//
// Serialised on thawMu rather than guarded by an in-flight flag, so a second
// caller waits and then observes the real answer instead of being told nil
// about a send still on the wire.
func (e *Engine) Thaw(ctx context.Context) error {
	e.thawMu.Lock()
	defer e.thawMu.Unlock()

	e.mu.Lock()
	done := e.thawed
	e.mu.Unlock()
	if done {
		return nil
	}

	res := Thaw(ctx, e.cfg.Channels, e.freeze.After)
	if res.Thawed {
		if err := ClearLease(e.cfg.FS, e.cfg.Paths.FreezeLease); err != nil {
			e.note("the freeze was lifted but its lease could not be cleared: %v", err)
		} else {
			res.LeaseCleared = true
		}
	}
	e.mu.Lock()
	if res.Thawed {
		e.thawed = true
	}
	e.thawRes = res
	if !e.frozenAt.IsZero() {
		// Recomputed on a successful retry, which is more correct and not
		// less: the world was not persisting until the thaw actually landed.
		e.snapRes.StallMs = e.now().Sub(e.frozenAt).Milliseconds()
	}
	e.mu.Unlock()

	if !res.Thawed {
		// Fatal, and deliberately the opposite of the flush's forgiveness. A
		// server left with saving disabled is the worst failure in the
		// system; the lease stays armed and the runner will re-send the thaw
		// at its deadline, which is exactly what it is for.
		e.log().Error("DEGRADED_FREEZE the thaw did not land", "instance", e.cfg.Instance,
			"why", res.Why)
		return fmt.Errorf("%w: %s", ErrFreeze, res.Why)
	}
	if e.snapRes.Red() {
		e.note("the world spent %dms not persisting, which is above the %s the dashboard renders "+
			"in red; a reflink or btrfs filesystem under the world paths would cut it to "+
			"milliseconds", e.snapRes.StallMs, StallRed)
	}
	return nil
}

// Snapshot captures the bytes and ends the freeze.
//
// The two families differ in exactly one place and this function is where the
// difference lives (section 13.2):
//
//	reflink/btrfs: clone -> THAW -> archive from the clone -> delete the clone
//	direct:        archive from the live tree -> THAW
//
// Under both, the thaw happens before this function returns. That is not the
// same as leaving it to the step's Undo: the Undo runs during the job's
// unwind, which is after verify, after prune and after the off-box push -- so
// a `direct` backup whose thaw waited for the Undo would hold a world
// unsaved through a deep verification and a 2 GB upload. The Undo remains, as
// the net under a panic.
func (e *Engine) Snapshot(ctx context.Context, class archive.Class, reason string) (*archive.Ref, error) {
	if !e.prepped {
		return nil, fmt.Errorf("%w: Snapshot ran before Preflight", ErrConfig)
	}
	sources := e.sources
	// Registered before the clone is attempted, not after it succeeds: a
	// clone that was created and then had its thaw fail would otherwise leave
	// a 2.4 GB copy of the world behind on the error return. It is a no-op
	// when no clone was made.
	defer e.dropClone()

	if e.plan.Method.ThawBeforeArchive() {
		cloned, err := e.clone(ctx)
		switch {
		case err != nil && errors.Is(err, ErrNoClone):
			// The capability answer changed between the probe and the
			// attempt: a btrfs path that is not really a subvolume, a kernel
			// that refuses FICLONE on this file. The archive is taken
			// directly instead -- which means it now happens INSIDE the
			// freeze, so the lease has to be extended to cover it before
			// anything else.
			e.note("the %s snapshot was refused (%v), so the archive is taken inside the freeze",
				e.plan.Method, err)
			if rerr := e.extendForDirect(ctx); rerr != nil {
				return nil, rerr
			}
			e.mu.Lock()
			e.snapRes.Fallback = true
			e.snapRes.Method = MethodDirect
			e.snapRes.Why = appendNote(e.snapRes.Why, "fell back to direct: "+err.Error())
			e.mu.Unlock()
		case err != nil:
			return nil, err
		default:
			sources = cloned
			// The world is captured. Everything from here reads the clone, so
			// the game can start saving again -- and this is the whole point
			// of the fast methods: the freeze lasted a metadata copy.
			if terr := e.Thaw(ctx); terr != nil {
				return nil, terr
			}
		}
	}

	ref, err := e.create(ctx, sources, class, reason)
	if err != nil {
		// The thaw still has to happen, and it has to happen now rather than
		// during the unwind: the archive failed, the world is still frozen,
		// and the next thing the job does is record a failure.
		if terr := e.Thaw(ctx); terr != nil {
			return nil, errors.Join(err, terr)
		}
		return nil, err
	}
	e.mu.Lock()
	e.ref = ref
	e.mu.Unlock()

	if terr := e.Thaw(ctx); terr != nil {
		// The bytes are on disk and the world is not saving. The archive is
		// reported anyway through the engine's Result, because it exists and
		// is worth having; the error is what fails the job.
		return nil, terr
	}
	return ref, nil
}

// clone takes the fast snapshot and returns sources rebased onto it.
func (e *Engine) clone(ctx context.Context) ([]archive.Source, error) {
	if len(e.plan.Sources) == 0 {
		return nil, fmt.Errorf("%w: this game declares no world_paths, so there is nothing to "+
			"clone", ErrNoClone)
	}
	if err := e.cfg.FS.MkdirAll(e.plan.Clone, SnapDirMode); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrSnapshot, e.plan.Clone, err)
	}
	e.mu.Lock()
	e.cloneMade = e.plan.Clone
	// Recorded so a clone this run's cleanup did not get to remove is
	// nameable in the job log, rather than only discoverable by the next
	// run's startup sweep.
	e.snapRes.ClonePath = e.plan.Clone
	e.mu.Unlock()

	// One clone per source base, under a per-base subdirectory of the clone,
	// so that two bases with the same leaf name cannot collide.
	out := make([]archive.Source, 0, len(e.sources))
	for i, s := range e.sources {
		dst := filepath.Join(filepath.FromSlash(e.plan.Clone), fmt.Sprintf("r%d", i))
		notes, err := e.cfg.Clone(ctx, e.plan.Method, s.Base, dst)
		for _, n := range notes {
			e.note("%s", n)
		}
		if err != nil {
			return nil, err
		}
		rebased := s
		rebased.Base = dst
		out = append(out, rebased)
	}
	return out, nil
}

// dropClone removes the staging clone. Failure is a note: a leftover clone
// costs disk space and the startup sweep gets it next time, while failing the
// job over it would discard a verified archive.
func (e *Engine) dropClone() {
	e.mu.Lock()
	p := e.cloneMade
	e.cloneMade = ""
	e.mu.Unlock()
	if p == "" {
		return
	}
	if err := e.cfg.FS.RemoveAll(p); err != nil {
		// A read-only btrfs subvolume clone needs the subvolume-destroy ioctl,
		// not RemoveAll. Try it before giving up, so the clone is actually
		// reclaimed now rather than left for a sweep that (before this) could
		// not remove it either (offsite-backup-dr-10).
		if derr := destroyBtrfsSubvolume(p); derr != nil {
			e.note("the snapshot clone %s could not be removed (%v; subvolume destroy: %v)",
				p, err, derr)
			return
		}
	}
	e.mu.Lock()
	e.snapRes.ClonePath = ""
	e.mu.Unlock()
}

// extendForDirect lengthens the lease to cover an archive that now happens
// inside the freeze.
//
// It fails the backup when it cannot, and that is the right way round: a lease
// whose deadline is shorter than the work it bounds would have the runner
// re-enable autosave in the middle of the tar, and archive.Create's own
// two-pass digest check would then report ErrDrifted on a world that was
// changing underneath it.
func (e *Engine) extendForDirect(ctx context.Context) error {
	e.mu.Lock()
	l := e.lease
	e.mu.Unlock()
	if l == nil {
		return nil
	}
	budget, why := BudgetFor(e.freeze.Budget, MethodDirect, e.est, e.cfg.FloorMBps)
	next, err := Renew(ctx, e.cfg.FS, e.cfg.Paths.FreezeLease, l, l.ArmedAt.Add(budget),
		e.cfg.Spec.Spec.User)
	if err != nil {
		return fmt.Errorf("%w: the snapshot fell back to a direct archive and the freeze lease "+
			"could not be extended to cover it: %w", ErrFreeze, err)
	}
	e.mu.Lock()
	e.lease = next
	e.leaseWhy = appendNote(e.leaseWhy, why)
	e.mu.Unlock()
	e.note("the freeze lease was extended to %s: %s", next.Deadline.UTC().Format(time.RFC3339), why)
	return nil
}

// create writes the archive through internal/archive.
func (e *Engine) create(ctx context.Context, sources []archive.Source, class archive.Class, reason string) (*archive.Ref, error) {
	opts := archive.Options{
		Dir:      e.cfg.Paths.Backups,
		Instance: e.cfg.Instance,
		Class:    class,
		Reason:   reason,
		Sources:  sources,
		Creator:  e.creator(reason),
		Subject:  e.subject(),
		SpecYAML: e.cfg.SpecYAML,
		Now:      e.cfg.Now,
		OnNote:   func(s string) { e.note("%s", s) },
	}
	// The fingerprint is the pre-archive record of the world's shape (D-34).
	// It is embedded only for a single-root archive, because Fingerprint's
	// paths are relative to ONE base and a fingerprint whose entries came
	// from two would describe neither.
	if len(sources) == 1 {
		opts.Fingerprint = e.fp
	} else if e.fp != nil {
		e.note("this archive has %d roots, so no world fingerprint was embedded: a fingerprint's "+
			"paths are relative to one base", len(sources))
	}

	ref, err := archive.Create(ctx, opts)
	if e.cfg.Audit != nil {
		// Recorded whether it worked or not: section 14.8's backup.create row
		// exists so that "there is no backup from that night" is answerable,
		// and a failure that wrote no row is the case where it is not.
		params := audit.Params{
			"retention_class": string(class),
			"snapshot_method": string(e.snapRes.Method),
			"freeze_ms":       e.snapRes.StallMs,
		}
		if ref != nil {
			params["path"] = ref.Path
			params["sha256"] = ref.SHA256
			params["bytes"] = ref.Bytes
		}
		e.cfg.Audit.Record(ctx, audit.Event{
			Actor:    e.cfg.Actor,
			Action:   audit.BackupCreate,
			Instance: e.cfg.Instance,
			Params:   params,
		}, outcomeOf(err), err)
	}
	return ref, err
}

// outcomeOf maps an error to an audit outcome.
func outcomeOf(err error) audit.Outcome {
	if err != nil {
		return audit.Failed
	}
	return audit.OK
}

func (e *Engine) creator(reason string) archive.Creator {
	kind := string(e.cfg.Actor.Kind)
	if kind == "" {
		kind = "system"
	}
	c := archive.Creator{Kind: kind, Reason: reason}
	if e.cfg.Actor.UserID != "" {
		id := e.cfg.Actor.UserID
		c.User = &id
	}
	return c
}

func (e *Engine) subject() archive.Subject {
	s := archive.Subject{
		Name:     e.cfg.Instance,
		Game:     e.cfg.Game,
		UnixUser: e.cfg.Spec.Spec.User,
	}
	if s.Game == "" {
		s.Game = e.cfg.Spec.Spec.Game
	}
	s.GameManifestSHA256 = e.cfg.Spec.Spec.Manifest.SHA256
	s.GameManifestRevision = e.cfg.Spec.Spec.Manifest.Revision
	sp := e.cfg.Spec.Spec.Paths
	s.Paths = map[string]string{}
	for k, v := range map[string]string{
		"install": sp.Install, "data": sp.Data, "home": sp.Home,
		"logs": sp.Logs, "backups": sp.Backups,
	} {
		if v != "" {
			s.Paths[k] = v
		}
	}
	return s
}

// Verify reads the archive back and stamps the Ref.
//
// It delegates to archive.VerifyRef, which is the only function in gvm that
// sets VerifiedAt -- and that single writer is what makes "a Ref that
// authorizes a prune has been read back" a property of the code rather than of
// everybody remembering. internal/jobs' step re-checks ref.Verified()
// afterwards, which is belt and braces on exactly the right buckle.
func (e *Engine) Verify(ctx context.Context, ref *archive.Ref) error {
	_, err := e.VerifyReport(ctx, ref)
	return err
}

// VerifyReport is Verify with the receipt.
//
// A caller that is about to DELETE something needs to know what the check
// actually did, not only that it did not fail: archive.VerifyRef stamps
// VerifiedAt for a shallow check as well -- it "legitimately compares no
// member CONTENTS" -- and both instance.Destroy and archive.Destroy gate only
// on ref.Verified(). So a shallow receipt passes every downstream guard while
// proving nothing about the bytes, and §13.6 step 7's "verify --spot 24, any
// failure -> DO NOT DELETE" can only be enforced by somebody reading the Mode.
//
// Verify is kept as the narrow spelling so the existing interfaces -- and
// internal/update's pipeline, which declares the same method on the same
// concrete type -- do not have to widen for a need they do not have.
func (e *Engine) VerifyReport(ctx context.Context, ref *archive.Ref) (*archive.Report, error) {
	if ref == nil {
		return nil, errors.New("backup: nothing to verify")
	}
	rep, err := archive.VerifyRef(ctx, ref, e.cfg.Verify)
	if rep != nil {
		for _, w := range rep.Warnings {
			e.note("verify: %s", w)
		}
	}
	return rep, err
}

// Prune applies retention, and only ever after a verified create.
//
// The policy comes from the instance's own keep/keep_days and the mechanics
// from archive.Prune, which already refuses to touch a preserve archive, an
// unmanaged tarball, or anything at all without a fresh verified Ref. Nothing
// in this package re-implements any of that: a second retention policy is a
// second set of rules about deleting worlds.
func (e *Engine) Prune(ctx context.Context, after *archive.Ref) error {
	pol := e.policy()
	if pol.Keep <= 0 && pol.KeepDays <= 0 && !pol.Force {
		e.note("this instance declares no retention (keep and keep_days are both zero), so " +
			"nothing was pruned; a policy that keeps nothing is always a typo, so zero is read " +
			"as no limit")
		return nil
	}
	res, err := archive.Prune(ctx, e.cfg.Paths.Backups, pol, after)
	e.mu.Lock()
	e.pruneRes = res
	e.mu.Unlock()
	if res != nil && res.PreservedBytes > 0 {
		// Section 13.2 asks doctor to report this so an operator notices
		// before the disk fills, which is the price of "never pruned, by any
		// code path". The job log is the other place they will see it.
		e.note("%d preserve archive(s) totalling %d bytes were left alone; they are never pruned "+
			"by any code path and only a human may remove them",
			len(res.Preserved), res.PreservedBytes)
	}
	return err
}

// policy is the retention this run applies.
func (e *Engine) policy() archive.Policy {
	if e.cfg.Retention != nil {
		p := *e.cfg.Retention
		if p.Now == nil {
			p.Now = e.cfg.Now
		}
		return p
	}
	b := e.cfg.Spec.Spec.Backup
	return archive.Policy{
		Keep:     b.Keep,
		KeepDays: b.KeepDays,
		Now:      e.cfg.Now,
		OnNote:   func(s string) { e.note("%s", s) },
	}
}

// Push copies the archive off-box.
//
// Failing must never fail the local backup, which is the actual protection
// (section 13.2) -- so the policy lives in the Remote and the decision is
// `on_failure`, defaulting to warn. What this method will NOT do is succeed
// quietly when a recipient was configured and no encrypter is available: see
// remote_age.go.
func (e *Engine) Push(ctx context.Context, ref *archive.Ref) error {
	if e.cfg.Remote == nil || !e.cfg.Remote.Enabled() {
		return nil
	}
	res, err := e.cfg.Remote.Push(ctx, ref, func(s string) { e.note("%s", s) })
	e.mu.Lock()
	e.pushRes = res
	e.mu.Unlock()
	if err == nil {
		return nil
	}
	if e.cfg.Remote.FailsTheJob() {
		return err
	}
	e.note("the off-box push failed and this target is on_failure=warn, so the local backup "+
		"stands: %v", err)
	return nil
}

// estimate sums the uncompressed size of the sources.
//
// It reuses archive.TakeFingerprint rather than walking the tree again,
// because that walker already answers the two questions this needs -- how many
// bytes and when was the newest write -- and already has the rule about never
// following a symlink out of the world. A second walker here would be a second
// set of answers to compare against the first.
func estimate(ctx context.Context, sources []archive.Source) (int64, *archive.Fingerprint, error) {
	var total int64
	var first *archive.Fingerprint
	for _, s := range sources {
		roots := s.Include
		if len(roots) == 0 && len(s.IncludeOptional) == 0 {
			roots = []string{"."}
		}
		roots = append(append([]string(nil), roots...), s.IncludeOptional...)
		fp, err := archive.TakeFingerprint(ctx, s.Base, roots)
		if err != nil {
			return 0, nil, fmt.Errorf("backup: sizing %s: %w", s.Base, err)
		}
		total += fp.Bytes
		if first == nil {
			first = fp
		}
	}
	return total, first, nil
}

// BuildSources turns the manifest's backup block into internal/archive's
// source set.
//
// It is pure and it is where section 13.2's two specific warnings are
// enforced. The first is the exclusion of {{ .Data }}/steamapps/workshop/** --
// the difference between a 2.4 GB nightly and an 8 GB one -- which is carried
// through as an Exclude on whichever root contains it. The second is the
// Paper-family _nether and _the_end siblings, which is the reason an include
// that lands under NO declared root is an error here rather than a warning: a
// backup that silently skips part of a world is discovered as a missing
// dimension months later.
//
// The grouping is by longest matching root, so a Data of /srv/gvm/x/data
// inside a Home of /srv/gvm/x resolves to Data and not to Home. Prefixes are
// the root's own name, so a member path says which of the game's directories
// it came from and a restore can put it back.
func BuildSources(m *spec.Manifest, r *spec.Renderer, p spec.Paths) ([]archive.Source, []string, error) {
	if m == nil {
		return nil, nil, fmt.Errorf("%w: no manifest", ErrConfig)
	}
	roots := rootsOf(p)
	if len(roots) == 0 {
		return nil, nil, fmt.Errorf("%w: this instance declares no paths, so there is nowhere "+
			"for a backup path to be relative to", ErrConfig)
	}

	b := m.Spec.Backup
	if len(b.Include) == 0 && len(b.IncludeOptional) == 0 {
		return nil, nil, fmt.Errorf("%w: this game's manifest declares no backup.include; an "+
			"archive of nothing would satisfy every check it is asked", ErrConfig)
	}

	var notes []string
	// placed keeps one Source per root, created on first use, so a root with
	// nothing in it produces no empty source for archive.Create to reject.
	placed := map[string]*archive.Source{}
	order := []string{}
	get := func(rt root) *archive.Source {
		if s, ok := placed[rt.Label]; ok {
			return s
		}
		s := &archive.Source{Prefix: rt.Label + "/", Base: rt.Path}
		placed[rt.Label] = s
		order = append(order, rt.Label)
		return s
	}

	render := func(field string, tmpls []string) ([]string, error) {
		out := make([]string, 0, len(tmpls))
		for i, t := range tmpls {
			f := fmt.Sprintf("%s[%d]", field, i)
			v, _, err := renderRung(r, f, t)
			if err != nil {
				return nil, err
			}
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
		return out, nil
	}

	inc, err := render("spec.backup.include", b.Include)
	if err != nil {
		return nil, notes, err
	}
	optional, err := render("spec.backup.include_optional", b.IncludeOptional)
	if err != nil {
		return nil, notes, err
	}
	excl, err := render("spec.backup.exclude", b.Exclude)
	if err != nil {
		return nil, notes, err
	}

	for _, abs := range inc {
		rt, rel, ok := place(roots, abs)
		if !ok {
			return nil, notes, fmt.Errorf("%w: %s is under none of %s. A backup that quietly "+
				"skips part of a world is the failure nobody notices for months, so this is "+
				"refused rather than warned about", ErrUnplaced, abs, rootNames(roots))
		}
		s := get(rt)
		s.Include = appendUnique(s.Include, rel)
	}
	for _, abs := range optional {
		rt, rel, ok := place(roots, abs)
		if !ok {
			// An OPTIONAL include is allowed to be unplaceable: its whole
			// contract is that its absence is a note rather than an error
			// (section 8.2).
			notes = append(notes, fmt.Sprintf("the optional backup path %s is under none of %s, "+
				"so it was left out", abs, rootNames(roots)))
			continue
		}
		s := get(rt)
		s.IncludeOptional = appendUnique(s.IncludeOptional, rel)
	}
	// Excludes come in two shapes and both are in the shipped manifests.
	// "{{ .Data }}/steamapps/workshop/**" renders to an absolute path and is
	// placed by its literal head; "**/*.lock" is a bare pattern that was
	// plainly meant to apply everywhere, and dropping it on the floor because
	// it is under no root would quietly put lock files in every archive.
	var global []string
	for _, pat := range excl {
		if !looksAbsolute(pat) {
			global = append(global, pat)
			continue
		}
		rt, rel, ok := place(roots, pat)
		if !ok {
			notes = append(notes, fmt.Sprintf("the exclude %s is an absolute path under none of "+
				"%s, so it excludes nothing from this archive", pat, rootNames(roots)))
			continue
		}
		s := get(rt)
		s.Exclude = appendUnique(s.Exclude, rel)
	}

	out := make([]archive.Source, 0, len(order))
	for _, label := range order {
		s := placed[label]
		for _, pat := range global {
			s.Exclude = appendUnique(s.Exclude, pat)
		}
		out = append(out, *s)
	}
	// Two roots where one contains the other, both in use, means a recursive
	// include under the outer one would store the inner one's files twice --
	// once under each prefix. No shipped manifest does it; saying so when it
	// happens costs one note and saves a confusing 2x archive.
	notes = append(notes, nestingNotes(out)...)
	return out, notes, nil
}

// root is one of the instance's declared directories, with the member-path
// label its contents are stored under.
type root struct {
	Label string
	Path  string
}

// rootsOf is the instance's directories, longest path first so that place
// resolves to the most specific one.
//
// Work is deliberately absent: it is SteamCMD's per-instance HOME (D-30), it
// holds a download cache and an appmanifest, and archiving it would put a
// rebuildable 8 GB of Steam state in every nightly.
func rootsOf(p spec.Paths) []root {
	candidates := []root{
		{"data", p.Data},
		{"install", p.Install},
		{"home", p.Home},
		{"logs", p.Logs},
	}
	seen := map[string]bool{}
	out := make([]root, 0, len(candidates))
	for _, c := range candidates {
		c.Path = strings.TrimRight(path.Clean(strings.TrimSpace(c.Path)), "/")
		if c.Path == "" || c.Path == "." || seen[c.Path] {
			continue
		}
		seen[c.Path] = true
		out = append(out, c)
	}
	slices.SortStableFunc(out, func(a, b root) int { return len(b.Path) - len(a.Path) })
	return out
}

func rootNames(roots []root) string {
	parts := make([]string, 0, len(roots))
	for _, r := range roots {
		parts = append(parts, r.Label+"="+r.Path)
	}
	return strings.Join(parts, ", ")
}

// place finds the most specific root containing abs and returns the path
// relative to it.
//
// The comparison is on path BOUNDARIES and not on string prefixes: /srv/gvm/pz
// must not be treated as containing /srv/gvm/pz-main, which is exactly the
// kind of near-miss that produces an archive full of another instance's world.
func place(roots []root, abs string) (root, string, bool) {
	clean := path.Clean(strings.TrimSpace(abs))
	for _, r := range roots {
		if clean == r.Path {
			return r, ".", true
		}
		if strings.HasPrefix(clean, r.Path+"/") {
			return r, strings.TrimPrefix(clean, r.Path+"/"), true
		}
	}
	return root{}, "", false
}

// looksAbsolute reports whether a rendered backup path names a location
// rather than a pattern.
//
// It accepts a Windows drive letter as well as a leading slash, because every
// production path here is a slash-separated Linux path built by
// internal/instance while every test path comes from t.TempDir -- and a
// placement rule that behaved differently between the two would be a rule
// tested on neither.
func looksAbsolute(p string) bool {
	switch {
	case strings.HasPrefix(p, "/"):
		return true
	case len(p) >= 3 && p[1] == ':' && (p[2] == '/' || p[2] == '\\'):
		return isLetter(p[0])
	default:
		return false
	}
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func appendUnique(xs []string, x string) []string {
	if slices.Contains(xs, x) {
		return xs
	}
	return append(xs, x)
}

// nestingNotes reports roots in use that contain one another.
func nestingNotes(srcs []archive.Source) []string {
	var out []string
	for i := range srcs {
		for j := range srcs {
			if i == j {
				continue
			}
			a, b := path.Clean(srcs[i].Base), path.Clean(srcs[j].Base)
			if strings.HasPrefix(a, b+"/") {
				out = append(out, fmt.Sprintf("the %s root (%s) is inside the %s root (%s); a "+
					"recursive include under %s would store those files under both prefixes",
					strings.TrimSuffix(srcs[i].Prefix, "/"), a,
					strings.TrimSuffix(srcs[j].Prefix, "/"), b,
					strings.TrimSuffix(srcs[j].Prefix, "/")))
			}
		}
	}
	return out
}
