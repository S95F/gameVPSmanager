// Package update is section 13.5: the update pipeline, and D-43 -- decide
// before you stop.
//
// It is the generalisation of legacy/projectzomboid-bash/update.sh, whose own
// comment states the contract this whole package is built around:
//
//	# Also runs unattended every night when UPDATE_ON_RESTART=true (see
//	# nightly.sh), so no failure here may leave the server stopped.
//
// That rule is enforced one level up, structurally, by internal/jobs' running
// guard. What is enforced HERE is the other half, and it is the bigger win:
// the script's shape was
//
//	stop -> backup -> steamcmd app_update validate -> start -> sleep 180 ->
//	re-resolve -> restart if the lists changed
//
// which on a night when nothing changed is several minutes of downtime, a full
// Steam validation pass, and two boots -- for nothing. Every one of those steps
// ran unconditionally because the script had no way to ask the question first.
//
// gvm asks first. Phases 1 and 2 are network READS performed with the server
// up and players online; the decision between phase 2 and phase 3 is pure and
// lives in internal/jobs (jobs.Decide); and when both axes are empty the job
// records `skipped` having issued zero stop calls. That is not an optimisation,
// it is the correctness property: on most nights the safest thing a nightly can
// do to a running world is nothing at all.
//
// Three refusals are structural rather than remembered:
//
//   - This package cannot stop a server. Nothing in Config is a supervisor,
//     nothing it holds has a Stop, Start or Restart method, and decide_test.go
//     asserts it over Config's own members. The stop belongs to the job layer's
//     step table, in front of which the running guard is already armed.
//   - This package does not re-decide. jobs.Decide owns "does this stop", and
//     a second opinion here would be a second policy.
//   - `validate` is emitted from repair.go and nowhere else. The schema already
//     refuses a manifest whose validate_on contains `update` (D-32), the driver
//     refuses it again in validateFor, and repair_test.go asserts that no other
//     file in this package even names the repair intent.
package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Pipeline refusals. Each is a sentinel because the next move differs: a
// misconfigured pipeline is a gvm bug, a held instance is an operator decision
// to respect, and no room on the volume is a host problem with a number
// attached.
var (
	// ErrConfig is a pipeline that cannot run. Every one of these surfaces in
	// Preflight, before anything has been read from the network and long
	// before anything could stop.
	ErrConfig = errors.New("update: the pipeline is not configured to run")
	// ErrHeld is /run/gvm/<name>/hold: an operator stopped this instance on
	// purpose. A nightly that updated it anyway would hand it back running,
	// because the running guard restores what it found -- and what it found
	// was a world a human put down.
	ErrHeld = errors.New("update: the instance is held by an operator")
	// ErrNoRoom is the disk refusal. It happens with the server still up, on
	// purpose: a full volume discovered after the shutdown ladder has run is
	// a stopped world plus a failed download.
	ErrNoRoom = errors.New("update: not enough free space for this update")
	// ErrAdopted is phase 5 refused at the type level. The provider refuses it
	// too; this is the earlier of the two refusals and the one that names the
	// pipeline phase.
	ErrAdopted = errors.New("update: the instance is adopted (managed_install:false)")
	// ErrOutOfOrder is a phase called before the phase it depends on. It is a
	// gvm bug and says so, because the alternative -- an AppUpdate that
	// quietly re-plans -- would perform a network read after the stop and
	// could apply a build the decision never saw.
	ErrOutOfOrder = errors.New("update: a phase ran before the phase it depends on")
)

// MinFreeBytes is section 13.5's phase-0 floor: free disk >= max(2 GiB,
// BytesToDownload * 1.5).
//
// The floor exists independently of the download size because a steamcmd run
// needs room for its own staging and its logs even when the delta is small,
// and because a box this close to full has other problems that an update is a
// bad time to discover.
const MinFreeBytes int64 = 2 << 30

// DiskHeadroom scales the download estimate. 1.5 is section 13.5's figure: a
// depot download stages before it moves, so the peak is meaningfully above the
// delta.
const DiskHeadroom = 1.5

// Installer is the provider seam as the pipeline needs it: four calls, each one
// a method internal/provider already declares.
//
// It exists as its own interface, bound to one Ctx by Bind, for two reasons.
// The phases become callable without threading a *provider.Ctx through every
// one of them, and a fake Installer is four short methods -- which is what
// makes "no diff means zero stops" and "an adopted instance is never stopped"
// assertable on the development machine against no Steam at all.
type Installer interface {
	// Plan performs network READS only and is safe with players online.
	Plan(ctx context.Context, in provider.Intent) (*provider.Plan, error)
	// Apply executes a plan that passed Authorize.
	Apply(ctx context.Context, pl *provider.Plan) (*provider.Applied, error)
	// Inspect is read-only and reports hand edits -- which is what repair
	// prints before it risks them.
	Inspect(ctx context.Context) (*provider.Inspection, error)
	// Observe reads local disk only.
	Observe(ctx context.Context) (*provider.Observation, error)
}

// SettleInput is what phase 8 needs from the content layer, and it is a VALUE:
// the expectation set and the roots are read once, before the wait, so the
// thing being waited for cannot change underneath the wait.
type SettleInput struct {
	Expect []Expectation
	Roots  []string
}

// Contents is internal/content as phases 2, 8, 9 and 10 need it.
//
// Plan is reads-only, which is the structural reason a blocked CurseForge file
// costs no downtime (section 13.5): the decision happens before the stop, and
// the plan that discovered the block never had to write anything.
type Contents interface {
	// Plan resolves the two lists and compares them to the lock. Network
	// READS only.
	Plan(ctx context.Context) (jobs.ContentPlan, error)
	// Settlement is phase 8's expectation set: every acquire id and the
	// directories it may arrive in, plus the roots quiescence is measured
	// over.
	Settlement(ctx context.Context) (SettleInput, error)
	// Resolve re-derives the activation namespace from disk and diffs it
	// against the lock.
	Resolve(ctx context.Context) (jobs.Resolution, error)
	// Activate writes the game's own config -- the list the server reads at
	// boot.
	Activate(ctx context.Context) error
}

// Archives is the pre-update backup and repair's preserve archive.
//
// Two methods, both of which *backup.Engine already has, so the production
// wiring is an assignment. Nothing here archives, verifies or prunes by
// itself: internal/archive does all three and a second retention policy is a
// second policy.
type Archives interface {
	Snapshot(ctx context.Context, class archive.Class, reason string) (*archive.Ref, error)
	Verify(ctx context.Context, ref *archive.Ref) error
}

// Config is one update run.
//
// Note what is absent: there is no Supervisor, no systemd handle, no lifecycle
// manager. The pipeline executes phases and cannot stop a server, which is the
// enforcement of D-43 rather than a description of it -- the stop is a step in
// internal/jobs' table, behind the running guard, and jobs.Decide is what
// decides whether that step runs at all.
type Config struct {
	Instance string
	// ManagedInstall is spec.ManagedInstall. False refuses phase 5 here and
	// again in the provider, and it is the reason phases 1, 2, 8, 9 and 10
	// still run for an adopted instance: Project Zomboid downloads its own
	// workshop items on boot and needs no steamcmd for them.
	ManagedInstall bool

	Installer Installer
	Content   Contents
	// Archives may be nil, and a nil one makes phase 4 a recorded refusal
	// rather than a silent skip. update.sh's line is `|| warn "Backup failed
	// - continuing anyway."`, and the job layer's step is non-fatal for that
	// reason -- but "there was no backup engine wired" must not read as "the
	// backup succeeded".
	Archives Archives

	// Held reports /run/gvm/<name>/hold. A func and not a Supervisor,
	// deliberately: the pipeline needs to READ whether a human put this
	// instance down, and must not be able to pick it back up.
	Held func(ctx context.Context) (bool, error)
	// Disk is the statfs seam. Nil uses platform.Statfs, which on the
	// development machine refuses -- and the pipeline's handling of that
	// refusal (record it, carry on, never approve a write off a measurement
	// that did not happen) is itself one of the behaviours worth testing.
	Disk func(path string) (platform.DiskUsage, error)

	// Paths is the control-plane path set. Zero means PathsFor(Instance).
	Paths instance.Paths
	// DiskPath is the directory whose VOLUME the preflight measures, and it
	// should be the install tree: that is where a 65 GB depot download lands,
	// and on a box where /opt and /var are separate filesystems -- which an
	// adopted Project Zomboid install usually is, with the server under /opt
	// and gvm's state under /var -- measuring the wrong one approves a
	// download that cannot fit. Empty falls back to Paths.State, which is
	// honest for a default layout and wrong for that one.
	DiskPath string

	// Settle overrides phase 8's figures. Zero takes the package defaults.
	Poll, Quiet, SettleBudget time.Duration

	// MinFree and Headroom override the phase-0 arithmetic.
	MinFree  int64
	Headroom float64

	Log   *slog.Logger
	Now   func() time.Time
	Notes func(format string, args ...any)
}

// Pipeline executes section 13.5's phases.
//
// It is stateful across the phases on purpose. The plan phase 1 produced is
// the plan phase 5 applies: re-planning after the stop would be a second
// network read, performed with the world down, whose answer the decision never
// saw -- and a build that moved in between would be applied without having
// been authorized against anything the operator was shown.
type Pipeline struct {
	cfg Config

	// plan is phase 1's answer, kept for phase 5.
	plan *provider.Plan
	// build and content are the two diffs, kept so a later phase's message
	// can name what started it.
	build, content jobs.Diff
	// contentPlan is phase 2's projection, kept for the blocked-items note.
	contentPlan jobs.ContentPlan
	// preflighted and checked are the ordering assertions. ErrOutOfOrder is a
	// gvm bug, and a loud one is better than an AppUpdate that re-planned.
	preflighted bool
	checked     bool
	// backup is the pre-update archive, recorded so the summary can name it.
	backup *archive.Ref
}

// New builds a pipeline. It validates nothing that needs the network, so a
// misconfiguration is a refusal from Preflight and not an error from a phase
// that has already stopped something.
func New(cfg Config) *Pipeline {
	if cfg.Paths.Name == "" && cfg.Instance != "" {
		cfg.Paths = instance.PathsFor(cfg.Instance)
	}
	return &Pipeline{cfg: cfg}
}

// Compile-time proof that the pipeline is what the job layer's `update` runner
// asks for. The interface lives in internal/jobs beside the step table that
// calls it, so this assertion is what keeps the two from drifting.
var _ jobs.Updates = (*Pipeline)(nil)

// Preflight is phase 0: the instance is not held, and the volume has the floor.
//
// The download-scaled half of section 13.5's arithmetic is NOT here, because
// the figure it scales does not exist yet -- BytesToDownload comes out of phase
// 1. It is applied at the end of CheckBuild instead, which is still on the near
// side of the decision and therefore still with the server running. Refusing a
// full disk is only useful while refusing is cheap.
func (p *Pipeline) Preflight(ctx context.Context) error {
	if err := p.validate(); err != nil {
		return err
	}
	if p.cfg.Held != nil {
		held, err := p.cfg.Held(ctx)
		if err != nil {
			return fmt.Errorf("update: %s: reading the operator hold: %w", p.cfg.Instance, err)
		}
		if held {
			return fmt.Errorf("%w: %s is present, so a human stopped %s on purpose; "+
				"`gvm start %s` removes it", ErrHeld, p.cfg.Paths.Hold, p.cfg.Instance,
				p.cfg.Instance)
		}
	}
	if err := p.checkRoom(p.cfg.minFree(), "the phase-0 floor"); err != nil {
		return err
	}
	p.preflighted = true
	return nil
}

// CheckBuild is phase 1: purge the cached appinfo.vdf, app_info_print, compare
// the published branch buildid to the local .acf.
//
// All of that is the provider's Plan, which performs network reads and nothing
// else -- and the appinfo purge, which is the one thing it writes, is a cache
// DELETION without which the remote buildid comes back stale forever. The
// pipeline's own contribution is to read the two ids off the plan in structured
// form and to apply the download-scaled disk check while the server is still
// up.
func (p *Pipeline) CheckBuild(ctx context.Context) (jobs.Diff, error) {
	if !p.preflighted {
		return jobs.Diff{}, fmt.Errorf("%w: CheckBuild before Preflight", ErrOutOfOrder)
	}
	pl, err := p.cfg.Installer.Plan(ctx, provider.IntentUpdate)
	if err != nil {
		return jobs.Diff{}, fmt.Errorf("update: %s: phase 1 (build check): %w",
			p.cfg.Instance, err)
	}
	if pl == nil {
		return jobs.Diff{}, fmt.Errorf("%w: the provider returned no plan and no error from "+
			"phase 1; a check that cannot answer must not read as `nothing to do`", ErrConfig)
	}
	p.plan = pl
	p.build = BuildDiff(pl)
	p.checked = true

	if pl.Writes() && pl.Bytes > 0 {
		need := platform.Required(pl.Bytes, p.cfg.headroom())
		if need < p.cfg.minFree() {
			need = p.cfg.minFree()
		}
		if err := p.checkRoom(need, fmt.Sprintf("%s to download, scaled by %.1f",
			spec.FormatBytes(pl.Bytes), p.cfg.headroom())); err != nil {
			return jobs.Diff{}, err
		}
	}
	p.note("phase 1: %s", pl.Reason)
	return p.build, nil
}

// CheckContent is phase 2: upstream metadata against the lock, reads only.
func (p *Pipeline) CheckContent(ctx context.Context) (jobs.Diff, error) {
	if p.cfg.Content == nil {
		// A game with no content block. Not an error and not a change: this
		// is Valheim's honest answer, and inventing a content diff for it
		// would stop a server to write a list that does not exist.
		p.content = jobs.Diff{Note: "this instance declares no content, so there is no content axis"}
		return p.content, nil
	}
	cp, err := p.cfg.Content.Plan(ctx)
	if err != nil {
		return jobs.Diff{}, fmt.Errorf("update: %s: phase 2 (content check): %w",
			p.cfg.Instance, err)
	}
	p.contentPlan = cp
	d, err := ContentDiff(cp)
	if err != nil {
		return jobs.Diff{}, err
	}
	p.content = d
	if d.Note != "" {
		p.note("phase 2: %s", d.Note)
	}
	return d, nil
}

// Backup is phase 4: class=routine, reason='pre-update'.
//
// The archive is VERIFIED here and not merely written. The job layer's step is
// non-fatal -- update.sh's own `|| warn "Backup failed - continuing anyway."`,
// kept because a backup failure at 05:00 must not leave the server stopped --
// and that is precisely why an unverified archive has to count as a failure:
// a non-fatal step whose success means "a tarball of unknown integrity exists"
// is a step that reports the wrong thing on the one night it matters.
func (p *Pipeline) Backup(ctx context.Context) error {
	if p.cfg.Archives == nil {
		return fmt.Errorf("%w: no archive engine is wired, so there is no pre-update backup; "+
			"the update continues, but this is not the same as a backup that succeeded", ErrConfig)
	}
	ref, err := p.cfg.Archives.Snapshot(ctx, archive.ClassRoutine, "pre-update")
	if err != nil {
		return fmt.Errorf("update: %s: phase 4 (pre-update backup): %w", p.cfg.Instance, err)
	}
	if ref == nil {
		return fmt.Errorf("%w: the archive engine reported success and returned no ref", ErrConfig)
	}
	if err := p.cfg.Archives.Verify(ctx, ref); err != nil {
		return fmt.Errorf("update: %s: phase 4: %s was written and failed verification: %w",
			p.cfg.Instance, ref.Path, err)
	}
	if !ref.Verified() {
		return fmt.Errorf("%w: %s was reported verified and its VerifiedAt is still zero, so "+
			"nothing may treat it as a receipt", ErrConfig, ref.Path)
	}
	p.backup = ref
	p.note("phase 4: pre-update archive %s (%s), verified", ref.Path, spec.FormatBytes(ref.Bytes))
	return nil
}

// Ref is the pre-update archive, once phase 4 has taken one.
func (p *Pipeline) Ref() *archive.Ref { return p.backup }

// AppUpdate is phase 5: the runscript, NO `validate`, in a transient service
// as the instance user.
//
// It applies the plan phase 1 produced. Three refusals in six lines, and each
// one has a failure behind it:
//
//   - an adopted instance never reaches app_update, because those files predate
//     gvm and may be hand-patched;
//   - a plan that is not phase 1's is refused, because a re-plan after the stop
//     would apply a build nothing authorized;
//   - a plan whose intent is not `update` is refused, because the only other
//     intent that reaches a steamcmd install is the one that emits `validate`,
//     and the whole of D-32 is that `validate` is never part of a routine
//     update.
func (p *Pipeline) AppUpdate(ctx context.Context) error {
	switch {
	case !p.checked || p.plan == nil:
		return fmt.Errorf("%w: AppUpdate before CheckBuild, so there is no authorized plan "+
			"and re-planning here would read the network with the world already down",
			ErrOutOfOrder)
	case !p.cfg.ManagedInstall:
		return fmt.Errorf("%w: phase 5 does not exist for %s; taking over binary updates for an "+
			"adopted instance is not available in this build", ErrAdopted, p.cfg.Instance)
	case p.plan.Intent != provider.IntentUpdate:
		return fmt.Errorf("%w: phase 5 was handed a %q plan; a routine update applies the "+
			"`update` intent and nothing else (D-32)", ErrConfig, string(p.plan.Intent))
	case !p.plan.Writes():
		return fmt.Errorf("%w: phase 5 was reached with an empty plan, which means the "+
			"decision let a no-op through", ErrOutOfOrder)
	}
	if err := p.plan.Authorize(p.cfg.ManagedInstall); err != nil {
		return err
	}
	applied, err := p.cfg.Installer.Apply(ctx, p.plan)
	if err != nil {
		// update.sh: "Steam update failed or timed out - starting on the
		// existing binaries." The retries and the classification ladder are
		// the driver's; by the time a failure arrives here Steam has been
		// down long enough that an outdated server is the right answer, and
		// the job layer's step is non-fatal for exactly that.
		return fmt.Errorf("update: %s: phase 5 (app_update): %w", p.cfg.Instance, err)
	}
	if applied != nil {
		p.note("phase 5: %s", strings.TrimSpace(applied.Note+" "+
			strings.Join(applied.Steps, "; ")))
	}
	return nil
}

// PostVerify is phase 6, and it is MANDATORY (D-32).
//
// It runs after EVERY app_update, including one that returned an error, because
// the failure it exists for is a steamcmd that self-updated, re-execed, printed
// `Error!` and exited 0. A verification gated on the exit code would skip
// exactly the runs it was written for.
//
// It is NEVER auto-remediated. The response to a failure is NEEDS_REPAIR plus
// starting the server anyway, because the one command that would repair it --
// `validate` -- overwrites files the operator customised. That command has its
// own path, its own flag and its own preserve archive, in repair.go.
func (p *Pipeline) PostVerify(ctx context.Context) error {
	pl, err := p.cfg.Installer.Plan(ctx, provider.IntentVerify)
	if err != nil {
		return fmt.Errorf("update: %s: phase 6 (post-verify): %w", p.cfg.Instance, err)
	}
	if err := pl.Authorize(p.cfg.ManagedInstall); err != nil {
		return err
	}
	applied, err := p.cfg.Installer.Apply(ctx, pl)
	if err != nil {
		return fmt.Errorf("update: %s: phase 6 (post-verify, D-32) failed: %w; run "+
			"`gvm update %s --repair` -- it is the only path that emits `validate`, and it "+
			"will overwrite customised files", p.cfg.Instance, err, p.cfg.Instance)
	}
	if applied != nil && applied.Note != "" {
		p.note("phase 6: %s", applied.Note)
	}
	return nil
}

// Settle is phase 8: workshop quiescence, replacing `sleep 180` (D-33).
func (p *Pipeline) Settle(ctx context.Context) (jobs.Settlement, error) {
	if p.cfg.Content == nil {
		return jobs.Settlement{Positive: true}, nil
	}
	in, err := p.cfg.Content.Settlement(ctx)
	if err != nil {
		return jobs.Settlement{}, fmt.Errorf("update: %s: phase 8: reading the expectation "+
			"set: %w", p.cfg.Instance, err)
	}
	s := &Settler{
		Instance: p.cfg.Instance,
		Expect:   in.Expect,
		Roots:    in.Roots,
		Poll:     p.cfg.Poll,
		Quiet:    p.cfg.Quiet,
		Budget:   p.cfg.SettleBudget,
		Now:      p.cfg.Now,
		Notes:    p.cfg.Notes,
	}
	return s.Wait(ctx)
}

// Resolve is phase 9: activation ids from disk, diffed against the lock.
func (p *Pipeline) Resolve(ctx context.Context) (jobs.Resolution, error) {
	if p.cfg.Content == nil {
		return jobs.Resolution{}, nil
	}
	r, err := p.cfg.Content.Resolve(ctx)
	if err != nil {
		return jobs.Resolution{}, fmt.Errorf("update: %s: phase 9 (resolve): %w",
			p.cfg.Instance, err)
	}
	return r, nil
}

// Activate is phase 10's middle step: write the game's own config.
//
// The server read its list at boot, which is why phase 10 exists at all and
// why its stop is a declared stopping step in the job's table with the running
// guard in front of it -- update.sh's final `systemctl restart`, with the
// difference that it only happens when the lists actually changed.
func (p *Pipeline) Activate(ctx context.Context) error {
	if p.cfg.Content == nil {
		return fmt.Errorf("%w: phase 10 was reached on an instance with no content layer",
			ErrOutOfOrder)
	}
	if err := p.cfg.Content.Activate(ctx); err != nil {
		return fmt.Errorf("update: %s: phase 10 (activate): %w", p.cfg.Instance, err)
	}
	return nil
}

// validate is every refusal that does not need the network.
func (p *Pipeline) validate() error {
	var errs []error
	if err := instance.ValidateName(p.cfg.Instance); err != nil {
		errs = append(errs, fmt.Errorf("%w: %v", ErrConfig, err))
	}
	if p.cfg.Installer == nil {
		errs = append(errs, fmt.Errorf("%w: no installer, so phases 1, 5 and 6 have nothing "+
			"to run against", ErrConfig))
	}
	return errors.Join(errs...)
}

// checkRoom is the disk arithmetic, and it refuses rather than guesses.
//
// A statfs that cannot answer is recorded and allowed through, because the
// development machine has no statfs at all and a pipeline that refused every
// run there would be a pipeline nobody tested. On Linux the measurement always
// happens, and that is where the refusal matters.
func (p *Pipeline) checkRoom(need int64, why string) error {
	if need <= 0 {
		return nil
	}
	target := p.cfg.DiskPath
	if target == "" {
		target = p.cfg.Paths.State
	}
	if target == "" {
		target = p.cfg.Paths.Srv
	}
	du, err := p.disk(target)
	if err != nil {
		p.note("free space on %s could not be measured (%v), so the %s check was not "+
			"performed", target, err, why)
		return nil
	}
	if du.Avail >= uint64(need) {
		return nil
	}
	return fmt.Errorf("%w: %s has %s available and this update needs %s (%s)",
		ErrNoRoom, target, spec.FormatBytes(int64(du.Avail)), spec.FormatBytes(need), why)
}

func (p *Pipeline) disk(path string) (platform.DiskUsage, error) {
	if p.cfg.Disk != nil {
		return p.cfg.Disk(path)
	}
	return platform.Statfs(path)
}

func (p *Pipeline) note(format string, args ...any) {
	if p.cfg.Notes != nil {
		p.cfg.Notes(format, args...)
		return
	}
	if p.cfg.Log != nil {
		p.cfg.Log.Info(fmt.Sprintf(format, args...))
	}
}

func (c Config) minFree() int64 {
	if c.MinFree > 0 {
		return c.MinFree
	}
	return MinFreeBytes
}

func (c Config) headroom() float64 {
	if c.Headroom >= 1 {
		return c.Headroom
	}
	return DiskHeadroom
}

// Bind is the production Installer: one provider and one Ctx, closed over.
//
// Progress is optional and arrives as a callback rather than as a field on
// Config, so a caller that wants none passes none and the driver still calls
// pr.Step unconditionally.
func Bind(prov provider.Provider, c *provider.Ctx, pr provider.Progress) Installer {
	return &bound{prov: prov, c: c, pr: pr}
}

type bound struct {
	prov provider.Provider
	c    *provider.Ctx
	pr   provider.Progress
}

func (b *bound) Plan(ctx context.Context, in provider.Intent) (*provider.Plan, error) {
	return b.prov.Plan(ctx, b.c, in)
}

func (b *bound) Apply(ctx context.Context, pl *provider.Plan) (*provider.Applied, error) {
	return b.prov.Apply(ctx, b.c, pl, b.pr)
}

func (b *bound) Inspect(ctx context.Context) (*provider.Inspection, error) {
	return b.prov.Inspect(ctx, b.c)
}

func (b *bound) Observe(ctx context.Context) (*provider.Observation, error) {
	return b.prov.Observe(ctx, b.c)
}
