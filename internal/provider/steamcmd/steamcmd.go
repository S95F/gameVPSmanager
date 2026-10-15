// Package steamcmd is the SteamCMD install driver: the one provider that
// actually fetches bytes in v1, and the one place in gvm that has to survive a
// tool which lies about whether it worked.
//
// THE CENTRAL FACT. SteamCMD's exit code is not evidence. A run can print
//
//	Error! App '380870' state is 0x602 after update job.
//
// and then exit 0. Valve publishes no exit-code table
// (ValveSoftware/steam-for-linux#4341 asked and was never answered), steamcmd
// self-updates and re-execs mid-run, and with `+`-style arguments a failed
// app_update still falls through to `+quit`. An installer that trusts the exit
// code therefore reports a successful install of a broken or partial game; the
// server then fails to boot for reasons that look nothing like a download
// problem, and the operator debugs the wrong layer for hours.
//
// So this package decides three times, in three different ways:
//
//	classify.go  reads STDOUT and sorts the run into hard-fail / retry /
//	             in-progress. The exit status is consulted only when stdout said
//	             nothing recognisable.
//	verify.go    reads Steam's OWN record on disk -- appmanifest_<appid>.acf --
//	             after EVERY install and update. This is D-32, and it is what
//	             pays for removing update.sh's nightly `validate`.
//	buildid.go   compares the installed build to the branch's published build
//	             before anything stops, which is why a normal night stops
//	             nothing at all (D-43).
//
// Nothing here spawns a process. SteamCMD downloads and executes Valve-supplied
// code, so it runs in a transient systemd service as the instance user (D-16);
// this package renders the runscript and reads the output, and the Runner seam
// is where the job layer plugs that sandbox in. That is also what makes the
// whole driver testable on the Windows development machine: every parser is
// pure, the filesystem arrives as an fs.FS, and fake.go is an in-memory
// SteamCMD that writes a real appmanifest.
package steamcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The driver's own refusals. The verification and classification sentinels live
// beside the code that produces them, in verify.go and classify.go.
var (
	// ErrManifest is a pinned manifest this driver cannot act on.
	ErrManifest = errors.New("steamcmd: the pinned manifest cannot drive this provider")
	// ErrNoSteamCMD is "steamcmd is not installed on this host".
	ErrNoSteamCMD = errors.New("steamcmd: no steamcmd on this host")
	// ErrNoRunner is the driver being asked to execute something with no
	// sandbox to execute it in. Terminal: nothing about retrying supplies one.
	ErrNoRunner = errors.New("steamcmd: no sandbox to run steamcmd in")
	// ErrCredentialed is a game that needs a real Steam account.
	ErrCredentialed = errors.New("steamcmd: this game needs a Steam account")
)

// Budgets. The update budget matches gvm-job@.service's RuntimeMaxSec (section
// 7.5): comfortably above the legitimate worst case -- a 65 GB CS2 download on
// a slow link -- and far below forever. The check budget is small because
// app_info_print downloads nothing; a check that hangs would hold the instance
// lock while deciding not to do anything.
const (
	DefaultCheckBudget  = 3 * time.Minute
	DefaultUpdateBudget = 4 * time.Hour
)

// DefaultSteamCMDDir is where gvm's host preparation extracts the tarball.
const DefaultSteamCMDDir = "/opt/gvm/steamcmd"

// maxCapturedOutput bounds what Plan keeps in memory to parse. One app's
// app_info_print output is a few hundred kilobytes; a 65 GB download's progress
// output is unbounded, so the capture keeps the head and the Classifier (which
// needs no buffer) sees everything.
const maxCapturedOutput = 8 << 20

// Invocation is one steamcmd run, as the sandbox receives it.
//
// Script and ScriptPath are passed as data rather than written here on purpose:
// the file has to be mode 0600 owned by the INSTANCE user (section 13.5), and
// the only component that can both create a root-owned directory and drop to
// that uid is the job layer. A provider that wrote it itself would either leave
// it root-owned -- unreadable by the steamcmd it is for -- or need privileges
// this seam exists to deny it.
type Invocation struct {
	// Purpose is "app_info" or "app_update". It names the transient unit and
	// the job-log line.
	Purpose string
	Binary  string
	Args    []string
	// Script is the runscript's bytes. It may contain a private-branch
	// password, so it is never logged; Runscript.Describe is what gets logged.
	Script     []byte
	ScriptPath string
	ScriptMode fs.FileMode
	// Env is the environment, including the per-instance HOME (D-30).
	Env map[string]string
	// WorkDir is the directory steamcmd runs in.
	WorkDir string
	// RunAs is the account the run drops to. Never root (D-16); UnixIdent
	// refuses the zero value, so "nobody filled this in" cannot become uid 0.
	RunAs provider.UnixIdent
	// Stdout receives the combined output, line by line as it arrives.
	Stdout io.Writer
	Budget time.Duration
}

// Runner executes an Invocation.
//
// This is the D-16 seam. The real implementation is internal/jobs' transient
// systemd SERVICE -- `systemd-run --unit=… --slice=gvm-jobs.slice --uid=<owner>`
// with explicit hardening properties -- and never a scope, which would inherit
// the caller's context and carry no confinement at all.
type Runner interface {
	Run(ctx context.Context, inv Invocation) (Exit, error)
}

// refusingRunner is the default, and it refuses.
//
// The registered driver is reachable through provider.Lookup for everything
// read-only -- Inspect, Observe, verification, the sentinel verdict -- and the
// write path requires a caller that has supplied a sandbox. Defaulting to
// os/exec instead would mean the first unwired call site ran a self-updating
// Valve binary as root, which is the exact regression D-16 names.
type refusingRunner struct{}

func (refusingRunner) Run(context.Context, Invocation) (Exit, error) {
	return Exit{}, fmt.Errorf("%w: steamcmd must run in a transient systemd service as the "+
		"instance user (D-16), and nothing has given this driver one. The job layer supplies it; "+
		"a caller that needs the write path has to construct the driver with "+
		"steamcmd.New(steamcmd.WithRunner(...))", ErrNoRunner)
}

// Provider is the steamcmd driver. Stateless with respect to instances:
// everything about one arrives in the provider.Ctx.
type Provider struct {
	runner Runner
	// binary and dir pin the steamcmd installation. Either may be empty, in
	// which case locate() searches.
	binary string
	dir    string
	// lookPath and stat are the search's seams, so Locator's rules are tested
	// without a steamcmd on the development machine.
	lookPath string
	stat     func(string) (fs.FileInfo, error)
	// tree opens an install tree. os.DirFS by default; an in-memory tree in
	// tests, which is the only way to exercise the sentinel's execute bit on
	// Windows.
	tree   func(root string) fs.FS
	shimFS ShimFS
	sleep  func(ctx context.Context, d time.Duration) error

	attempts     int
	checkBudget  time.Duration
	updateBudget time.Duration
}

// Option configures a driver.
type Option func(*Provider)

// WithRunner supplies the sandbox the runs happen in.
func WithRunner(r Runner) Option {
	return func(p *Provider) {
		if r != nil {
			p.runner = r
		}
	}
}

// WithSteamCMD pins the steamcmd launcher explicitly, skipping the search.
func WithSteamCMD(binary, dir string) Option {
	return func(p *Provider) { p.binary, p.dir = binary, dir }
}

// WithSteamCMDDir names the directory the tarball was extracted into.
func WithSteamCMDDir(dir string) Option {
	return func(p *Provider) { p.dir = dir }
}

// WithSearch overrides the binary search's inputs, for tests.
func WithSearch(pathEnv string, stat func(string) (fs.FileInfo, error)) Option {
	return func(p *Provider) { p.lookPath, p.stat = pathEnv, stat }
}

// WithTreeFS replaces os.DirFS as the way an install tree is read.
func WithTreeFS(open func(root string) fs.FS) Option {
	return func(p *Provider) {
		if open != nil {
			p.tree = open
		}
	}
}

// WithShimFS replaces the real filesystem for the shim apply step.
func WithShimFS(f ShimFS) Option {
	return func(p *Provider) {
		if f != nil {
			p.shimFS = f
		}
	}
}

// WithSleep replaces the retry ladder's wait, so a test asserts the backoff
// instead of serving it.
func WithSleep(f func(ctx context.Context, d time.Duration) error) Option {
	return func(p *Provider) {
		if f != nil {
			p.sleep = f
		}
	}
}

// WithAttempts overrides the retry count. Section 13.5's three is the default.
func WithAttempts(n int) Option {
	return func(p *Provider) {
		if n > 0 {
			p.attempts = n
		}
	}
}

// WithBudgets overrides the per-run time budgets.
func WithBudgets(check, update time.Duration) Option {
	return func(p *Provider) {
		if check > 0 {
			p.checkBudget = check
		}
		if update > 0 {
			p.updateBudget = update
		}
	}
}

// New returns the driver.
func New(opts ...Option) *Provider {
	p := &Provider{
		runner:       refusingRunner{},
		shimFS:       OSShimFS,
		attempts:     MaxAttempts,
		checkBudget:  DefaultCheckBudget,
		updateBudget: DefaultUpdateBudget,
		sleep:        sleepCtx,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { provider.Register(New()) }

func (p *Provider) Kind() string { return spec.ProviderSteamCMD }

var _ provider.Provider = (*Provider)(nil)

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// decision is Plan's Opaque: everything Apply needs, so Apply never repeats a
// network read Plan already paid for.
//
// It is unexported, which makes the type assertion in Apply a real check: a
// plan that did not come out of this package's Plan cannot carry one.
type decision struct {
	intent   provider.Intent
	build    BuildCheck
	script   Runscript
	bytes    int64
	validate bool
	// purged is the appinfo.vdf files the check deleted, for the job log.
	purged []string
	// extra are the ExtraApps builds, keyed by appid, for the artifact list.
	extra map[int]string
}

// Plan performs network READS only.
//
// For an adopted instance (managed_install:false) install and repair are
// refused here, at the type level: section 13.5's Phase 5 does not exist for an
// install gvm does not own. `update` is NOT refused, because the nightly still
// has to run the CHECK phase and report "a binary update is available and this
// instance is adopted" -- taking that away silently on the day after cutover
// would remove a working nightly the operator already has.
//
// The one thing Plan writes is steamcmd's own appinfo.vdf cache, which it
// DELETES. That is not a write an operator owns: it is the cache that makes the
// read wrong, and without purging it the remote buildid comes back stale
// forever (see PurgeAppInfo).
func (p *Provider) Plan(ctx context.Context, c *provider.Ctx, in provider.Intent) (*provider.Plan, error) {
	sc, err := steamSpec(c)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(provider.Intents(), in) {
		return nil, fmt.Errorf("%w: %q is not an intent", provider.ErrInvalidPlan, string(in))
	}

	switch in {
	case provider.IntentVerify:
		// Read-only, and it needs neither steamcmd nor the network. Apply of
		// this plan performs the D-32 check and reports it.
		return provider.EmptyPlan(in, fmt.Sprintf(
			"verify %s: exactly one %s, StateFlags == %d, both byte pairs, and %s present%s "+
				"(D-32). Nothing is downloaded and nothing stops.",
			c.Name, appManifestName(sc.InstallAppID), StateInstalled,
			sentinelPath(sc), executableSuffix(sc))), nil
	case provider.IntentInstall, provider.IntentRepair:
		if !c.ManagedInstall {
			return nil, fmt.Errorf("%w: %s is adopted, so gvm never runs app_update or validate "+
				"against %s. Taking over an adopted install is not available in this build; a %s "+
				"would overwrite files that predate gvm, including any the operator customised",
				provider.ErrAdopted, c.Name, c.Paths.Install.Path(), in)
		}
	}

	if !sc.Anonymous {
		// The account, its cached refresh token and its sentry files are
		// host-level credentials with their own storage and their own RBAC gate
		// (D-02), and none of that reaches a provider.Ctx yet. Refusing is the
		// honest answer: the alternative is an anonymous login that fails with
		// "No subscription" and looks like a Steam problem.
		return nil, fmt.Errorf("%w: %s declares anonymous:false, and gvm's Steam account wiring "+
			"is not in this build. Install this app by hand once and adopt the instance",
			ErrCredentialed, c.Manifest.Metadata.ID)
	}

	branch := branchOf(sc)
	local, err := p.localBuild(c, sc)
	if err != nil {
		return nil, err
	}

	check, info, purged, err := p.remoteBuild(ctx, c, sc, local)
	if err != nil {
		return nil, err
	}

	dec := &decision{intent: in, build: check, purged: purged, extra: map[int]string{}}
	for _, id := range sc.ExtraApps {
		if node, perr := ParseAppInfo(bytes.NewReader(info), id); perr == nil {
			if b, berr := RemoteBuildID(node, DefaultBranch); berr == nil {
				dec.extra[id] = b
			}
		}
	}

	// An update with nothing to fetch is the normal answer on most nights, and
	// it must not stop the server or page anyone (D-43).
	if in == provider.IntentUpdate && !check.UpdateAvailable() {
		pl := provider.EmptyPlan(in, fmt.Sprintf("%s: %s, so nothing is downloaded and nothing "+
			"stops", c.Name, check.Summary()))
		// BuildAhead lands here too, and it is reported as "no update" on
		// purpose (see BuildState): rolling a live world backwards on a
		// nightly timer is not something gvm does unasked. The two ids are
		// still carried, so the job log says which pair produced the no-op.
		stampBuilds(pl, check, false)
		return pl, nil
	}

	// An adopted instance reaching here has an update available and no
	// permission to perform it. The plan is empty and says what to do, verbatim
	// from section 13.5.
	if !c.ManagedInstall {
		pl := provider.EmptyPlan(in, fmt.Sprintf(
			"%s; %s is adopted (managed_install: false). Taking over binary updates for an "+
				"adopted instance is not available in this build", check.Summary(), c.Name))
		// The one place UpdateAvailable is true. Without it the pipeline sees
		// an empty plan and records `skipped` with no `update_available`, and
		// the operator is never told that the nightly they had before adoption
		// is now reporting nothing.
		stampBuilds(pl, check, true)
		return pl, nil
	}

	validate := validateFor(sc, in)
	populated, err := provider.Populated(c.Paths.Install)
	if err != nil {
		return nil, err
	}

	script := Runscript{
		Intent:     in,
		InstallDir: c.Paths.Install.Path(),
		Platform:   sc.Platform,
		Login:      Login{Anonymous: true},
		Apps: []AppUpdate{{
			AppID:          sc.InstallAppID,
			Branch:         nonDefaultBranch(branch),
			BranchPassword: branchPassword(c, sc),
			Validate:       validate,
		}},
	}
	for _, id := range sc.ExtraApps {
		script.Apps = append(script.Apps, AppUpdate{AppID: id, Validate: validate})
	}
	// Render once here, at plan time, so a runscript gvm cannot generate is a
	// refusal the operator sees while nothing is stopped -- rather than an error
	// after the shutdown ladder has already run.
	if _, err := script.Render(); err != nil {
		return nil, err
	}
	dec.script = script
	dec.validate = validate
	dec.bytes = EstimateBytes(mustNode(info, sc.InstallAppID), sc.Platform)

	pl := &provider.Plan{
		Intent:      in,
		Reason:      planReason(c, sc, check, in, validate),
		Bytes:       dec.bytes,
		Destructive: validate && populated,
		Opaque:      dec,
	}
	stampBuilds(pl, check, false)
	pl.Artifacts = append(pl.Artifacts, provider.Artifact{
		Digest:  provider.Digest(fmt.Sprintf("buildid:%d:%s", sc.InstallAppID, check.Remote)),
		Size:    dec.bytes,
		Source:  fmt.Sprintf("steamcmd:app_update %d", sc.InstallAppID),
		DestRel: ".",
	})
	for _, id := range sc.ExtraApps {
		build := dec.extra[id]
		if build == "" {
			build = "current"
		}
		pl.Artifacts = append(pl.Artifacts, provider.Artifact{
			Digest:  provider.Digest(fmt.Sprintf("buildid:%d:%s", id, build)),
			Source:  fmt.Sprintf("steamcmd:app_update %d", id),
			DestRel: ".",
		})
	}

	desc, err := script.Describe()
	if err != nil {
		return nil, err
	}
	pl.Steps = append(pl.Steps,
		fmt.Sprintf("build check: %s", check.Summary()),
		fmt.Sprintf("disk: about %s, pre-flighted before anything stops", spec.FormatBytes(dec.bytes)),
		fmt.Sprintf("runscript (%s, mode %04o, owned by %s):\n%s",
			scriptPath(c), RunscriptMode, c.RunAs.User(), indent(desc)),
		fmt.Sprintf("post-verify: %s, StateFlags == %d, both byte pairs, %s (D-32)",
			appManifestName(sc.InstallAppID), StateInstalled, sentinelPath(sc)),
	)
	if validate {
		pl.Steps = append(pl.Steps, "this run passes `validate`, which OVERWRITES every file in "+
			"the install tree that differs from Steam's copy, including ones the operator edited")
	}
	if err := pl.Validate(); err != nil {
		return nil, err
	}
	return pl, nil
}

// Apply executes a plan. Its first statement is the authorization check.
//
// It returns a non-nil Applied alongside an error whenever the run got far
// enough to have done something: a failed post-verification is a finding about
// the install, not a reason to lose the record of what was downloaded, and
// D-32's response to it is to start the server anyway and tell the operator to
// run `gvm update <n> --repair`.
func (p *Provider) Apply(ctx context.Context, c *provider.Ctx, pl *provider.Plan, pr provider.Progress) (*provider.Applied, error) {
	if err := pl.CheckAuthorized(c.ManagedInstall); err != nil {
		return nil, err
	}
	sc, err := steamSpec(c)
	if err != nil {
		return nil, err
	}

	if pl.Intent == provider.IntentVerify {
		return p.applyVerify(c, sc, pl)
	}
	if !pl.Writes() {
		// The nightly no-op and the adopted refusal both land here, and the
		// shared helper is what guarantees neither one can write.
		return provider.ApplyEmptyPlan(c, pl, fmt.Sprintf(
			"this plan carries no artifacts, so there is nothing for steamcmd to install into %s",
			c.Paths.Install.Path()))
	}

	dec, ok := pl.Opaque.(*decision)
	if !ok || dec == nil {
		return nil, fmt.Errorf("%w: this plan has artifacts and no steamcmd decision, so it did "+
			"not come from this driver's Plan; Apply will not invent the runscript for it",
			provider.ErrInvalidPlan)
	}
	if dec.intent != pl.Intent {
		return nil, fmt.Errorf("%w: the plan says %s and the decision inside it says %s",
			provider.ErrInvalidPlan, pl.Intent, dec.intent)
	}

	applied := &provider.Applied{Intent: pl.Intent, StartedAt: c.Now()}
	for _, path := range dec.purged {
		applied.Steps = append(applied.Steps, "purged stale appinfo cache "+path)
	}

	verdict, runErr := p.runUpdate(ctx, c, dec, pr, applied)
	if runErr != nil {
		applied.EndedAt = c.Now()
		applied.Note = fmt.Sprintf("steamcmd did not complete: %v", runErr)
		return applied, runErr
	}

	applied.Changed = !verdict.AlreadyCurrent
	applied.Wrote = []string{"."}
	applied.Bytes = dec.bytes
	if verdict.AlreadyCurrent {
		applied.Wrote = nil
		applied.Bytes = 0
	}
	if !verdict.Installed && !verdict.AlreadyCurrent {
		// Recorded, not failed. A Valve wording change to the Success line
		// would otherwise produce a false NEEDS_REPAIR and with it a
		// recommendation to run `--repair`, which overwrites customised files;
		// the build comparison below is the evidence-based rule, and this is
		// the sentence that explains it when it fires.
		applied.Steps = append(applied.Steps, "steamcmd printed no outcome line for this run: "+
			"neither a `fully installed` nor an `already up to date` claim was seen, so the "+
			"appmanifest is the only thing that says whether anything happened")
	}

	// POST-VERIFY. Mandatory, after every single app_update (D-32).
	//
	// dec.build.Remote is passed even for an AlreadyCurrent verdict: a plan
	// only reaches here with artifacts when UpdateAvailable() was true, so
	// "already up to date" from app_update while the disk is still on the old
	// build is the stale-appinfo failure PurgeAppInfo's header describes.
	pr.Step("post-verify")
	report, verifyErr := p.verify(c, sc, dec.build.Remote)
	applied.Version = report.Version()
	applied.Steps = append(applied.Steps, verifySteps(report)...)
	applied.EndedAt = c.Now()

	if verifyErr != nil {
		if errors.Is(verifyErr, ErrWrongBuild) && report.Manifest != nil &&
			report.Manifest.BuildID == dec.build.Local {
			// The tree is still on the build it started on, so Changed:true
			// and a written path are both claims this run cannot make.
			applied.Changed, applied.Wrote, applied.Bytes = false, nil, 0
		}
		applied.Note = fmt.Sprintf("%s installed build %s and post-verification FAILED: %s. "+
			"gvm does not auto-remediate this, because the only tool that would is `validate`, "+
			"and validate overwrites files the operator customised. Start the server anyway and "+
			"run `gvm update %s --repair`, which takes a preserve archive first (D-32)",
			c.Name, report.Version().ID, strings.Join(report.Problems, "; "), c.Name)
		return applied, verifyErr
	}

	if !verdict.AlreadyCurrent {
		if m := report.Manifest; m != nil && m.HasDownloadPair && m.BytesDownloaded > 0 {
			// Steam's figure for the transfer that just happened. dec.bytes is
			// EstimateBytes -- the worst-case depot sum the disk preflight
			// needs -- and the job record must not present an estimate as a
			// measurement.
			applied.Bytes = m.BytesDownloaded
		}
	}

	res, shimErr := p.applyShims(c, sc)
	applied.EndedAt = c.Now()
	if shimErr != nil {
		applied.Note = fmt.Sprintf("installed build %s, verified, and the Steam shims failed: %v",
			report.Version().ID, shimErr)
		return applied, shimErr
	}
	applied.Steps = append(applied.Steps, shimSteps(res)...)

	switch {
	case verdict.AlreadyCurrent:
		applied.Note = fmt.Sprintf("%s was already on build %s; nothing was downloaded and the "+
			"install verified", c.Name, report.Version().ID)
	default:
		applied.Note = fmt.Sprintf("%s installed and verified: %s", c.Name, verifiedSummary(report))
	}
	if len(res.Problems) > 0 {
		applied.Note += ". Shim warnings: " + strings.Join(res.Problems, "; ")
	}
	applied.EndedAt = c.Now()
	return applied, nil
}

// applyVerify is Apply for the read-only intent.
//
// A failed verification is NOT an Apply error: the check did its job, and the
// finding is the output. The typed verdict lives in Inspect, whose Integrity
// and Sentinel are what a caller branches on; this returns the sentence for the
// job log, which is the whole output of a `skipped` nightly's doctor step.
func (p *Provider) applyVerify(c *provider.Ctx, sc *spec.SteamCMDSpec, pl *provider.Plan) (*provider.Applied, error) {
	now := c.Now()
	report, err := p.verify(c, sc, "")
	applied := &provider.Applied{
		Intent:    pl.Intent,
		Version:   report.Version(),
		Changed:   false,
		Steps:     verifySteps(report),
		StartedAt: now,
		EndedAt:   c.Now(),
	}
	if err != nil {
		applied.Note = fmt.Sprintf("%s FAILED verification: %s. Run `gvm update %s --repair`",
			c.Name, strings.Join(report.Problems, "; "), c.Name)
		return applied, nil
	}
	applied.Note = fmt.Sprintf("%s verified: %s", c.Name, verifiedSummary(report))
	return applied, nil
}

// runUpdate is the retry ladder of section 13.5: three attempts, 30s/2m/8m.
//
// A hard failure stops immediately, because retrying a credential error locks
// the Steam account and retrying "out of disk" cannot work. Everything else is
// retried, because the alternative -- the bash suite's no retry at all -- turns
// a thirty-second content-server blip into a missed nightly.
func (p *Provider) runUpdate(ctx context.Context, c *provider.Ctx, dec *decision, pr provider.Progress, applied *provider.Applied) (Verdict, error) {
	script, err := dec.script.Render()
	if err != nil {
		return Verdict{}, err
	}
	bin, _, err := p.locate()
	if err != nil {
		return Verdict{}, err
	}

	attempts := p.attempts
	if attempts < 1 {
		attempts = 1
	}
	var last Verdict
	for attempt := 1; attempt <= attempts; attempt++ {
		step := fmt.Sprintf("app_update %d (attempt %d of %d)", dec.script.Apps[0].AppID,
			attempt, attempts)
		pr.Step(step)
		applied.Steps = append(applied.Steps, step)

		cl := NewAppUpdateClassifier()
		exit, runErr := p.runner.Run(ctx, Invocation{
			Purpose:    "app_update",
			Binary:     bin,
			Args:       []string{"+runscript", scriptPath(c)},
			Script:     []byte(script),
			ScriptPath: scriptPath(c),
			ScriptMode: RunscriptMode,
			Env:        p.env(c),
			WorkDir:    c.Paths.Install.Path(),
			RunAs:      c.RunAs,
			Stdout:     cl,
			Budget:     p.updateBudget,
		})
		if runErr != nil {
			// The sandbox itself failed. ErrNoRunner and a cancelled context are
			// terminal; anything else (systemd-run refused, the unit vanished)
			// is infrastructure and gets the same ladder as a Steam blip.
			if errors.Is(runErr, ErrNoRunner) || ctx.Err() != nil {
				return last, runErr
			}
			applied.Steps = append(applied.Steps, "the sandbox failed: "+runErr.Error())
			if attempt == attempts {
				return last, runErr
			}
			if err := p.wait(ctx, attempt, applied); err != nil {
				return last, err
			}
			continue
		}

		last = cl.Verdict(exit)
		applied.Steps = append(applied.Steps, outcomeStep(last))
		switch {
		case last.OK():
			return last, nil
		case last.Terminal():
			return last, last.Err()
		case attempt == attempts:
			return last, last.Err()
		}
		if err := p.wait(ctx, attempt, applied); err != nil {
			return last, err
		}
	}
	return last, last.Err()
}

func (p *Provider) wait(ctx context.Context, attempt int, applied *provider.Applied) error {
	d := Backoff(attempt)
	applied.Steps = append(applied.Steps, "waiting "+d.String()+" before the next attempt")
	return p.sleep(ctx, d)
}

// remoteBuild is the CHECK phase: purge the cache, ask Steam, compare.
//
// It returns the captured output as well as the comparison, so Plan can read
// the extra apps' builds and the depot sizes out of the same run rather than
// paying for a second one.
func (p *Provider) remoteBuild(ctx context.Context, c *provider.Ctx, sc *spec.SteamCMDSpec, local string) (BuildCheck, []byte, []string, error) {
	bin, _, err := p.locate()
	if err != nil {
		return BuildCheck{}, nil, nil, err
	}

	// MANDATORY, and the reason is in PurgeAppInfo: without it Steam serves the
	// cached metadata and the nightly reports "no update" forever.
	purged, purgeErr := p.purge(c)
	if purgeErr != nil {
		return BuildCheck{}, nil, purged, purgeErr
	}

	script := Runscript{
		Intent:  provider.IntentVerify, // reads only; nothing in it can validate
		Login:   Login{Anonymous: true},
		AppInfo: append([]int{sc.InstallAppID}, sc.ExtraApps...),
	}
	rendered, err := script.Render()
	if err != nil {
		return BuildCheck{}, nil, purged, err
	}

	cl := NewClassifier()
	capture := &capWriter{limit: maxCapturedOutput}
	ctx, cancel := context.WithTimeout(ctx, p.checkBudget)
	defer cancel()

	exit, runErr := p.runner.Run(ctx, Invocation{
		Purpose:    "app_info",
		Binary:     bin,
		Args:       []string{"+runscript", checkScriptPath(c)},
		Script:     []byte(rendered),
		ScriptPath: checkScriptPath(c),
		ScriptMode: RunscriptMode,
		Env:        p.env(c),
		WorkDir:    c.Paths.Install.Path(),
		RunAs:      c.RunAs,
		Stdout:     io.MultiWriter(cl, capture),
		Budget:     p.checkBudget,
	})
	if runErr != nil {
		return BuildCheck{}, capture.Bytes(), purged, runErr
	}
	if v := cl.Verdict(exit); !v.OK() {
		return BuildCheck{}, capture.Bytes(), purged, v.Err()
	}

	node, err := ParseAppInfo(bytes.NewReader(capture.Bytes()), sc.InstallAppID)
	if err != nil {
		return BuildCheck{}, capture.Bytes(), purged, err
	}
	remote, err := RemoteBuildID(node, branchOf(sc))
	if err != nil {
		return BuildCheck{}, capture.Bytes(), purged, err
	}
	check, err := CheckBuild(local, remote, branchOf(sc))
	return check, capture.Bytes(), purged, err
}

// purge deletes the cached appinfo.vdf under the instance's SteamCMD HOME.
func (p *Provider) purge(c *provider.Ctx) ([]string, error) {
	root := c.Paths.Work
	if !root.Valid() {
		// A manifest with no steamcmd HOME is a misconfiguration this driver
		// cannot work around: the cache would then live in whatever HOME the
		// sandbox inherits, shared with every other instance (D-30).
		return nil, fmt.Errorf("%w: %s has no steam work directory, so there is no per-instance "+
			"SteamCMD HOME to purge the appinfo cache from (D-30)", ErrManifest, c.Name)
	}
	return PurgeAppInfo(root.Path())
}

func (p *Provider) env(c *provider.Ctx) map[string]string {
	return SteamCMDEnv(ShimInput{Home: c.Paths.Home.Path(), Work: c.Paths.Work.Path()})
}

// localBuild reads the installed buildid off disk, best effort.
//
// An unreadable or absent appmanifest is "" and never an error: that is the
// install case, and the plan for it is the same plan.
//
// A REFUSED buildid is an error, and that asymmetry is the point. The value
// flows into CheckBuild and out through Summary into Plan.Reason, so a forged
// one would reach the plan preview; and an empty string there would answer
// BuildNotInstalled, turning a tampered appmanifest into a plan to reinstall
// the game over the top of a live world.
func (p *Provider) localBuild(c *provider.Ctx, sc *spec.SteamCMDSpec) (string, error) {
	m, err := p.readACF(c, sc)
	if err != nil {
		return "", nil
	}
	if m.BuildIDSuspect {
		return "", fmt.Errorf("%w: %s records a buildid that is not a plausible build id, so gvm "+
			"will not plan against it. Something other than Steam wrote that file; the install "+
			"tree is writable by the instance account, so a compromised workshop item is the "+
			"first thing to rule out. `gvm doctor %s` reports the whole finding",
			ErrBuildID, CanonicalACF(sc.InstallAppID), c.Name)
	}
	return m.BuildID, nil
}

func (p *Provider) readACF(c *provider.Ctx, sc *spec.SteamCMDSpec) (*AppManifest, error) {
	tree, err := p.installTree(c)
	if err != nil {
		return nil, err
	}
	data, err := fs.ReadFile(tree, CanonicalACF(sc.InstallAppID))
	if err != nil {
		return nil, err
	}
	return ParseAppManifest(bytes.NewReader(data))
}

func (p *Provider) installTree(c *provider.Ctx) (fs.FS, error) {
	if c == nil || !c.Paths.Install.Valid() {
		return nil, fmt.Errorf("%w: the install path did not come from a jail", provider.ErrPaths)
	}
	if p.tree != nil {
		return p.tree(c.Paths.Install.Path()), nil
	}
	return os.DirFS(c.Paths.Install.Path()), nil
}

// verify runs the D-32 check against this instance's tree.
//
// wantBuild is the build the run was authorized to fetch, and is "" for every
// read-only path: Inspect and IntentVerify have no plan, so there is no
// authorization to compare the tree against and asserting one would invent it.
func (p *Provider) verify(c *provider.Ctx, sc *spec.SteamCMDSpec, wantBuild string) (*Report, error) {
	tree, err := p.installTree(c)
	if err != nil {
		return &Report{AppID: sc.InstallAppID}, err
	}
	return Verify(VerifyRequest{
		Tree:      tree,
		AppID:     sc.InstallAppID,
		Branch:    branchOf(sc),
		Sentinel:  sc.Sentinel,
		Verify:    sc.Verify,
		WantBuild: wantBuild,
	})
}

// applyShims makes the three shims true after an install.
func (p *Provider) applyShims(c *provider.Ctx, sc *spec.SteamCMDSpec) (*ShimResult, error) {
	_, dir, err := p.locate()
	if err != nil {
		return nil, err
	}
	runtimeAppID, _ := RuntimeAppID(c.Manifest)
	in := ShimInput{
		Home:         c.Paths.Home.Path(),
		Work:         c.Paths.Work.Path(),
		SteamCMDDir:  dir,
		Arch:         c.Manifest.Spec.Runtime.Arch,
		RuntimeAppID: runtimeAppID,
	}
	shims, err := HomeShims(in)
	if err != nil {
		return nil, err
	}
	return ApplyShims(shims, p.shimFS)
}

// locate resolves the steamcmd launcher and the directory it lives in.
func (p *Provider) locate() (binary, dir string, err error) {
	if p.binary != "" {
		d := p.dir
		if d == "" {
			d = filepath.Dir(p.binary)
		}
		return p.binary, d, nil
	}
	loc := Locator{Dir: p.dir, PATH: p.lookPath, Stat: p.stat}
	bin, err := loc.Find()
	if err != nil {
		return "", "", err
	}
	d := p.dir
	if d == "" {
		d = filepath.Dir(bin)
	}
	return bin, d, nil
}

// Locator finds the steamcmd launcher.
//
// There is deliberately no exec.LookPath here, and nothing in this package
// imports os/exec: the driver does not spawn processes, the sandbox does (D-16).
// Resolving PATH by hand costs ten lines and keeps that true.
type Locator struct {
	// Dir is the configured steamcmd directory and wins when it holds one.
	Dir string
	// PATH is the search path; os.Getenv("PATH") when empty.
	PATH string
	// Stat is os.Stat when nil.
	Stat func(string) (fs.FileInfo, error)
}

// Find returns the launcher's absolute path.
func (l Locator) Find() (string, error) {
	stat := l.Stat
	if stat == nil {
		stat = os.Stat
	}
	var tried []string
	try := func(p string) (string, bool) {
		if p == "" {
			return "", false
		}
		tried = append(tried, p)
		fi, err := stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			return "", false
		}
		// The execute bit is the point: a steamcmd.sh that was unpacked without
		// one fails with a message about a directory, not about a mode.
		if fi.Mode().Perm()&0o111 == 0 {
			return "", false
		}
		return p, true
	}

	var candidates []string
	if d := strings.TrimSpace(l.Dir); d != "" {
		candidates = append(candidates, filepath.Join(d, "steamcmd.sh"))
	}
	candidates = append(candidates,
		filepath.Join(DefaultSteamCMDDir, "steamcmd.sh"),
		"/usr/games/steamcmd",
		"/usr/bin/steamcmd",
	)
	pathEnv := l.PATH
	if pathEnv == "" {
		pathEnv = os.Getenv("PATH")
	}
	for _, el := range filepath.SplitList(pathEnv) {
		if el == "" {
			continue
		}
		candidates = append(candidates,
			filepath.Join(el, "steamcmd.sh"),
			filepath.Join(el, "steamcmd"),
		)
	}
	for _, cand := range candidates {
		if p, ok := try(cand); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: looked at %s. steamcmd is a 32-bit binary, so a Debian or Ubuntu "+
		"host needs `dpkg --add-architecture i386 && apt-get install lib32gcc-s1` before the "+
		"tarball is extracted into %s",
		ErrNoSteamCMD, strings.Join(tried, ", "), DefaultSteamCMDDir)
}

// capWriter keeps the head of a stream, bounded.
//
// The head rather than the tail: what Plan parses out of app_info_print is the
// app's block, which comes right after the login preamble, while the unbounded
// part of any steamcmd output is the progress lines at the end.
type capWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		w.buf.Write(p[:room])
	}
	return len(p), nil
}

func (w *capWriter) Bytes() []byte { return w.buf.Bytes() }

// steamSpec is the one place the pinned manifest is checked for usability.
func steamSpec(c *provider.Ctx) (*spec.SteamCMDSpec, error) {
	switch {
	case c == nil:
		return nil, fmt.Errorf("%w: no provider context", ErrManifest)
	case c.Manifest == nil:
		return nil, fmt.Errorf("%w: %s has no pinned manifest", ErrManifest, c.Name)
	}
	// Checked here so that every entry point has validated the name before
	// scriptPath derives a control-plane path from it: instance.PathsFor
	// panics on a name nothing validated, which is the right behaviour for a
	// path helper and the wrong behaviour for a driver.
	if err := instance.ValidateName(c.Name); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	sc := c.Manifest.Spec.Provider.SteamCMD
	switch {
	case sc == nil:
		return nil, fmt.Errorf("%w: %s declares provider kind %q and carries no steamcmd block",
			ErrManifest, c.Manifest.Metadata.ID, c.Manifest.Spec.Provider.Kind)
	case sc.InstallAppID <= 0:
		return nil, fmt.Errorf("%w: %s declares install_appid %d",
			ErrManifest, c.Manifest.Metadata.ID, sc.InstallAppID)
	case strings.TrimSpace(sc.Sentinel.Path) == "":
		// D-32's fifth check. The schema requires it; a pinned manifest from an
		// older gvm might not have had to.
		return nil, fmt.Errorf("%w: %s declares no install sentinel, so no run of this driver "+
			"could ever prove it installed anything (D-32)", ErrManifest, c.Manifest.Metadata.ID)
	case !sc.Verify.AppManifest || !sc.Verify.ByteParity:
		// The same reasoning as the sentinel, and the same place to apply it:
		// refuse the pin at the door rather than after 65 GB is downloaded.
		// spec.LoadPinned decodes and guards the document but never runs the
		// checker, so a pin written before the schema made these mandatory
		// still loads -- and with both off, verification falls back to "the
		// directory is not empty".
		return nil, fmt.Errorf("%w: %s does not declare verify.appmanifest and "+
			"verify.byte_parity, so no run of this driver could show an install or update "+
			"finished; re-run `gvm apply <instance>` to re-validate and re-pin the manifest "+
			"(D-32)", ErrManifest, c.Manifest.Metadata.ID)
	}
	return sc, nil
}

func branchOf(sc *spec.SteamCMDSpec) string {
	if b := strings.TrimSpace(sc.Branch); b != "" {
		return b
	}
	return DefaultBranch
}

// nonDefaultBranch is what goes on the app_update line: `-beta public` is legal
// and means stable, but emitting it on every run would make every diff and
// every log line carry a flag that changes nothing.
func nonDefaultBranch(branch string) string {
	if branch == DefaultBranch {
		return ""
	}
	return branch
}

// branchPassword resolves a private branch's password.
//
// Secrets first, then vars: a manifest declaring `branch_password_var` with a
// var of type secret or password resolves through SecretValues, and the taint
// that comes with it is why the rendered script is never logged (see
// Runscript.Describe).
func branchPassword(c *provider.Ctx, sc *spec.SteamCMDSpec) string {
	name := strings.TrimSpace(sc.BranchPasswordVar)
	if name == "" || c.Render == nil {
		return ""
	}
	d := c.Render.Data()
	if v := d.Secret[name]; v != "" {
		return v
	}
	return d.Var[name]
}

// validateFor decides whether this run emits the positional `validate`.
//
// D-32 twice over: the manifest has to have declared the intent, and `update`
// can never be one of them. The schema already refuses a manifest whose
// validate_on contains update; this is the second place it cannot happen, in
// the code that would otherwise do it.
func validateFor(sc *spec.SteamCMDSpec, in provider.Intent) bool {
	if in == provider.IntentUpdate {
		return false
	}
	return slices.Contains(sc.ValidateOn, in)
}

// stampBuilds puts the comparison's two ids on the plan in structured form.
//
// Every exit of Plan that performed a build check calls it, including the two
// that return an EMPTY plan, because those are the two the update pipeline has
// to tell apart: "current, nothing to do" and "stale, and this instance is
// adopted so gvm will not touch it". The Label carries the branch so that a
// diff printed for a beta instance does not read as if it were public.
func stampBuilds(pl *provider.Plan, check BuildCheck, updateAvailable bool) {
	if pl == nil {
		return
	}
	if check.Local != "" {
		pl.Current = provider.Version{ID: check.Local, Label: check.Branch}
	}
	if check.Remote != "" {
		pl.Target = provider.Version{ID: check.Remote, Label: check.Branch}
	}
	pl.UpdateAvailable = updateAvailable
}

func planReason(c *provider.Ctx, sc *spec.SteamCMDSpec, check BuildCheck, in provider.Intent, validate bool) string {
	var b strings.Builder
	switch in {
	case provider.IntentInstall:
		fmt.Fprintf(&b, "install %s (app %d) into %s: %s", c.Manifest.Metadata.ID,
			sc.InstallAppID, c.Paths.Install.Path(), check.Summary())
	case provider.IntentRepair:
		fmt.Fprintf(&b, "repair %s (app %d) in %s: %s", c.Manifest.Metadata.ID,
			sc.InstallAppID, c.Paths.Install.Path(), check.Summary())
	default:
		fmt.Fprintf(&b, "update %s: %s", c.Name, check.Summary())
	}
	if validate {
		b.WriteString("; this run validates, which overwrites customised files")
	}
	return b.String()
}

func sentinelPath(sc *spec.SteamCMDSpec) string {
	if s := strings.TrimSpace(sc.Sentinel.Path); s != "" {
		return s
	}
	return "the declared sentinel"
}

func executableSuffix(sc *spec.SteamCMDSpec) string {
	if sc.Sentinel.Executable {
		return " and executable"
	}
	return ""
}

func verifiedSummary(r *Report) string {
	if r.Manifest == nil {
		return "nothing to report"
	}
	return fmt.Sprintf("build %s, StateFlags %s, %s downloaded, %s present",
		r.Manifest.BuildLabel(), StateDescription(r.Manifest.StateFlags),
		spec.FormatBytes(r.Manifest.BytesDownloaded), r.Sentinel.Rel)
}

func verifySteps(r *Report) []string {
	if r == nil {
		return nil
	}
	out := []string{"verify: " + verifiedSummary(r)}
	for _, p := range r.Problems {
		out = append(out, "verify problem: "+p)
	}
	return out
}

func shimSteps(res *ShimResult) []string {
	if res == nil {
		return nil
	}
	var out []string
	for _, s := range res.Created {
		out = append(out, "shim created "+s)
	}
	for _, s := range res.Problems {
		out = append(out, "shim problem: "+s)
	}
	return out
}

func outcomeStep(v Verdict) string {
	if v.OK() {
		return fmt.Sprintf("steamcmd finished (%s), states seen: %s", v.Exit,
			strings.Join(v.Progress, ", "))
	}
	return "steamcmd: " + v.Err().Error()
}

// scriptPath and checkScriptPath are section 13.5's
// /var/lib/gvm/instances/<name>/steam/update.txt, and its read-only sibling.
//
// They are derived with instance.PathsFor, which is pure path arithmetic, so
// the provider names the file and the job layer -- which has the privileges and
// the uid -- writes it.
func scriptPath(c *provider.Ctx) string {
	return instance.PathsFor(c.Name).SteamScriptDir + "/update.txt"
}

func checkScriptPath(c *provider.Ctx) string {
	return instance.PathsFor(c.Name).SteamScriptDir + "/check.txt"
}

// mustNode re-parses the captured app_info output for the depot sizes. A parse
// that already succeeded once cannot fail here; an empty node yields a zero
// estimate, which the disk preflight reads as "no figure" rather than "no
// space needed".
func mustNode(info []byte, appid int) *KV {
	node, err := ParseAppInfo(bytes.NewReader(info), appid)
	if err != nil {
		return &KV{Block: true}
	}
	return node
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
