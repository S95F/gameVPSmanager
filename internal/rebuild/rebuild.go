// Package rebuild is section 13.6: `gvm rebuild`, archive-before-destroy,
// generalized.
//
// It is the generalisation of legacy/projectzomboid-bash/rebuild.sh, and that
// script's ordering is the entire design. Its own comments say why:
//
//	# 4. Download workshop content, resolve Mod IDs (world still intact)
//	pz_boot_and_wait "SERVER STARTED" 3600 \
//	  || die "Download boot failed - the world was NOT touched. ..."
//	...
//	tar -czf "$ARCHIVE" -C "$DATA_DIR" "${MEMBERS[@]}" \
//	  || die "Archive failed - NOT deleting the world. Check free disk space (df -h)."
//
// Content is resolved, downloaded and verified BEFORE anything is deleted, and
// the archive is taken after that verification and before the deletion. Every
// early exit leaves the world untouched, and -- this is the part that is easy
// to lose in a rewrite -- every early exit SAYS SO. "The world was NOT touched"
// appears in four of that script's error messages because the operator reading
// one of them at 2am needs to know, in the same sentence, whether they have
// lost anything.
//
// So Outcome carries two fields that are not decoration: WorldTouched, which is
// false on every path that returns before the destroy, and Untouched, the
// sentence. rebuild_test.go enumerates the fallible calls of every step before
// the archive, injects a failure, a panic and a cancellation at each, and
// asserts all three of: WorldTouched is false, Untouched is non-empty, and the
// destroyer recorded zero calls. A step added to this file is covered the day
// it is added.
//
// Destruction is not implemented here. It goes through instance.Destroy, which
// takes a *archive.Ref it refuses when nil or unverified, and through a jail
// built by instance.Jail.Rebuild, which is the only way a world path becomes
// destroyable and which refuses any path the jail does not already protect. So
// "we only ever delete backup.world_paths, and only after a verified archive"
// is a property of the types rather than of this file's control flow.
package rebuild

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
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/update"
)

// Rebuild refusals. Every one of them is raised before the archive step, which
// is why every one of them can promise the world is untouched.
var (
	// ErrConfig is a rebuild that cannot run.
	ErrConfig = errors.New("rebuild: not configured to run")
	// ErrUnaccounted is rebuild.sh lines 77-80, generalised to every
	// resolver: the parsed item count disagrees with the raw count, so the
	// acquisition list is NOT rewritten.
	ErrUnaccounted = errors.New("rebuild: the content parse could not account for every item")
	// ErrNotConfirmed is step 2. A full rebuild erases the map and everyone's
	// characters, so the operator types the instance name.
	//
	// It is jobs.ErrNotConfirmed ITSELF and not a second sentinel of the same
	// name. Section 16.4 requires one contract for the CLI, the API and the
	// dashboard, and a package with its own refusal would be a second one --
	// reachable through this entry point, with its own wording and its own
	// rules about what `--yes` means. errors.Is against either name finds the
	// same error because there is only one.
	ErrNotConfirmed = jobs.ErrNotConfirmed
	// ErrContentMissing is step 5: an acquire id that never arrived. The
	// world is untouched, the server is restarted, and the ids are named.
	ErrContentMissing = errors.New("rebuild: content did not arrive")
	// ErrArchive is step 7 failing -- the write, the verification, or a Ref
	// that came back unverified. It is the refusal the whole ordering exists
	// to make possible.
	ErrArchive = errors.New("rebuild: the pre-rebuild archive is not a receipt, so nothing is deleted")
	// ErrBlocked is content a human has to fetch by hand (section 12.8). A
	// rebuild on an incomplete list would generate a world missing mods, so
	// unlike the nightly update -- where a blocked file merely costs no
	// downtime -- here it stops before the confirm.
	ErrBlocked = errors.New("rebuild: content is awaiting manual files")
)

// DefaultBootBudget is step 4's wait: section 13.6's 3600s, which is
// rebuild.sh's own `pz_boot_and_wait "SERVER STARTED" 3600`.
//
// An hour looks absurd until the first boot of a 32-item Project Zomboid
// workshop list on a 2-core VPS, which is where the figure comes from.
const DefaultBootBudget = 3600 * time.Second

// DefaultStopBudget bounds the shutdown ladder each stop runs. Zero lets the
// instance's own shutdown.budget decide, which is what the rendered ladder
// already carries; this is the fallback for a caller that supplied neither.
const DefaultStopBudget = 5 * time.Minute

// unmaskBudget bounds the deferred unmask, which runs DETACHED from the job's
// context: see step 3. Thirty seconds is internal/jobs' own figure for a
// detached cleanup call, and the work is one D-Bus UnmaskUnitFiles plus a
// daemon-reload -- long enough for a loaded bus, short enough that a wedged
// systemd cannot hang the exit.
const unmaskBudget = 30 * time.Second

// Options is the command line, as rebuild.sh spells it.
type Options struct {
	// KeepWorld is --keep-world: apply the mods, leave the world. The run
	// exits after step 6 and never reaches the archive or the destroy, so
	// there is nothing to confirm.
	KeepWorld bool
	// Dry is --dry: stop after step 1 having touched nothing, not even the
	// acquisition list.
	Dry bool
	// NoFetch is --no-fetch: use the acquisition list as it stands, for the
	// day Steam is down or rate-limiting.
	NoFetch bool
	// Yes is --yes: automation. It skips the typed confirmation and nothing
	// else.
	Yes bool
	// Typed is what the operator typed at the prompt. It must equal the
	// instance name. A separate field from Yes so that "they typed it" and
	// "a script asserted it" are different facts in the audit row.
	Typed string
}

// Outcome is what happened, and whether the world survived it.
type Outcome struct {
	Instance string `json:"instance"`
	// Steps is the narration, in order. It is what the CLI prints and what
	// goes in the job summary.
	Steps []string `json:"steps,omitempty"`

	// Plan is step 1's answer: what the collection added and dropped.
	Plan jobs.ContentPlan `json:"plan,omitzero"`

	// WorldTouched is the safety property. False means nothing in
	// backup.world_paths was deleted, on every path -- success, failure,
	// panic and cancellation alike.
	WorldTouched bool `json:"world_touched"`
	// Untouched is the SENTENCE, and it is required on every early exit. A
	// bool an operator has to find in a JSON blob is not the same as "the
	// world was NOT touched" in the error they are already reading.
	Untouched string `json:"untouched,omitempty"`

	// Archive is the verified preserve archive, non-nil from step 7 onwards.
	Archive *archive.Ref `json:"archive,omitzero"`
	// Missing are step 5's ids, by name.
	Missing []string `json:"missing,omitempty"`
	// Resolution is step 6's answer.
	Resolution jobs.Resolution `json:"resolution,omitzero"`

	// KeptWorld and Dry echo the options, so a report cannot be mistaken for
	// a full rebuild.
	KeptWorld bool `json:"kept_world,omitempty"`
	Dry       bool `json:"dry,omitempty"`

	// Running is whether the server is up at exit. The rebuild stops it at
	// step 3 and is responsible for it afterwards, on every path.
	Running bool `json:"running"`
	// Restore is the command that puts the old world back. rebuild.sh printed
	// a three-line tar recipe; this is one `gvm restore`.
	Restore string `json:"restore,omitempty"`
}

func (o *Outcome) step(format string, args ...any) {
	o.Steps = append(o.Steps, fmt.Sprintf(format, args...))
}

// untouched is the one way an early exit returns.
//
// It records the sentence on the Outcome AND puts it in the error, because the
// two are read by different things: the sentence in the error reaches the
// operator's terminal and the journal, and the field reaches the job summary
// and the dashboard. rebuild.sh put it only in the message, which is why
// `gvm rebuild --dry`'s ancestor had no way to report it.
func (o *Outcome) untouched(err error, format string, args ...any) error {
	o.WorldTouched = false
	o.Untouched = fmt.Sprintf(format, args...)
	o.Archive = nil
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w -- %s", err, o.Untouched)
}

// Content is internal/content as the rebuild needs it.
//
// Plan is reads-only and is where the fail-closed count check lives; Write is
// the acquisition list; Settlement is the expectation set step 5 waits on, and
// it is the same type the nightly's phase 8 uses because it is the same
// question -- rebuild.sh's NOT_DOWNLOADED loop, with D-33's quiescence instead
// of its "Steam stalls sometimes: rerun ./rebuild.sh".
type Content interface {
	Plan(ctx context.Context, fetch bool) (jobs.ContentPlan, error)
	Write(ctx context.Context) error
	Settlement(ctx context.Context) (update.SettleInput, error)
	Resolve(ctx context.Context) (jobs.Resolution, error)
}

// Server is the rebuild's view of the game process. Three methods, and no
// Enable or Disable: a rebuild stops and starts a server, and whether it is
// SUPPOSED to run is intent, which has one write path (internal/reconcile).
type Server interface {
	Stop(ctx context.Context, budget time.Duration) (lifecycle.Outcome, error)
	Start(ctx context.Context) error
	// WaitReady waits for the game's own readiness marker, not for the unit.
	// Step 4's whole purpose is a boot that downloads content, and the unit
	// is active long before that finishes.
	WaitReady(ctx context.Context, budget time.Duration) error
}

// Timers masks this instance's job timers for the duration.
//
// rebuild.sh's line is `systemctl stop zomboid-restart.timer` and its comment
// says what it is for: "so it can't fire mid-rebuild". A nightly restart or
// nightly update firing between step 4's boot and step 8's destroy is the
// worst interleaving available, and masking is what makes it impossible rather
// than unlikely.
type Timers interface {
	Mask(ctx context.Context) error
	Unmask(ctx context.Context) error
}

// Archiver is step 7, and all three methods are ones *backup.Engine already
// has, so the production wiring is an assignment.
type Archiver interface {
	// Preflight is the disk check: section 13.6 wants free >= estimate * 1.2,
	// which is the engine's own Headroom.
	Preflight(ctx context.Context) error
	Snapshot(ctx context.Context, class archive.Class, reason string) (*archive.Ref, error)
	// VerifyReport returns the RECEIPT and not only an error, because step 7
	// has to refuse a verification too weak to authorize a destroy -- and a
	// shallow check cannot be told from a spot check by its error, since both
	// stamp VerifiedAt and only one of them re-reads any bytes.
	VerifyReport(ctx context.Context, ref *archive.Ref) (*archive.Report, error)
}

// Destroyer is step 8, and it is a seam ONLY so that the ordering is testable
// off Linux. The production implementation is JailDestroyer below, which is
// four lines and routes everything through instance.Destroy.
type Destroyer interface {
	Destroy(ctx context.Context, ref *archive.Ref, worldPaths []string) error
}

// Hooks is step 9: pz.apply_sandbox and its equivalents.
type Hooks interface {
	Apply(ctx context.Context) error
}

// Config is one rebuild.
type Config struct {
	Instance string

	Content   Content
	Server    Server
	Timers    Timers
	Archives  Archiver
	Destroyer Destroyer
	// Hooks may be nil: most games have nothing to apply after a world is
	// deleted. Project Zomboid has the sandbox preset.
	Hooks Hooks

	// WorldPaths is backup.world_paths, rendered. It is the destroy set and
	// nothing else, ever -- and the jail refuses anything that is not in it
	// whatever this field says.
	WorldPaths []string

	// BootBudget and StopBudget bound step 4 and every stop.
	BootBudget time.Duration
	StopBudget time.Duration

	// SettleBudget bounds step 5. Zero is update.BudgetRebuild.
	SettleBudget time.Duration
	// Poll and Quiet override step 5's quiescence figures.
	Poll, Quiet time.Duration

	Log   *slog.Logger
	Now   func() time.Time
	Notes func(format string, args ...any)
}

// Run executes section 13.6's ten steps.
//
// It returns a non-nil Outcome on every path, including a refusal, because the
// Outcome is where the "world untouched" promise is recorded and an error that
// dropped it would be an error the operator cannot act on.
func Run(ctx context.Context, cfg Config, opt Options) (*Outcome, error) {
	out := &Outcome{Instance: cfg.Instance, KeptWorld: opt.KeepWorld, Dry: opt.Dry}
	if verr := cfg.validate(); verr != nil {
		return out, out.untouched(verr, "nothing ran, so the world is untouched")
	}

	// There is deliberately no recover() here. A panic is a programming error
	// or a broken seam, and internal/jobs' engine is where one is caught,
	// sanitised into job_runs.panic and audited -- converting it to an error
	// in this file would erase that distinction. The world's safety does not
	// depend on catching it: the destroy is a SINGLE call near the end, so a
	// panic anywhere above it has deleted nothing, and rebuild_test.go asserts
	// that for every injection point rather than taking it on trust.

	// ------------------------------------------------------------------
	// 1. PLAN. Network reads, fail closed on the count check. --dry stops
	//    here having written nothing, not even the acquisition list.
	// ------------------------------------------------------------------
	plan, err := cfg.Content.Plan(ctx, !opt.NoFetch)
	if err != nil {
		return out, out.untouched(err, "the content plan failed before anything was stopped, "+
			"so the world is untouched and the acquisition list was not rewritten")
	}
	out.Plan = plan
	if n := len(plan.Unaccounted); n > 0 {
		return out, out.untouched(fmt.Errorf("%w: %d item(s) (%s)", ErrUnaccounted, n,
			strings.Join(plan.Unaccounted, " ")),
			"a source whose markup changed silently drops content, so the acquisition list "+
				"was NOT rewritten and the world is untouched")
	}
	if n := len(plan.Blocked); n > 0 {
		return out, out.untouched(fmt.Errorf("%w: %d item(s) need a human: %s", ErrBlocked, n,
			strings.Join(plan.Blocked, " ")),
			"a rebuild generates a new world from the full list, so an incomplete list is "+
				"refused before anything stops; the world is untouched")
	}
	if opt.NoFetch {
		out.step("skipping the collection fetch (--no-fetch); using the acquisition list as it stands")
	}
	describePlan(out, plan)

	if opt.Dry {
		out.step("dry run: nothing was written, nothing was stopped and nothing was deleted")
		return out, out.untouched(nil, "dry run; the world is untouched")
	}

	// ------------------------------------------------------------------
	// 2. CONFIRM. A full rebuild erases the map and everyone's characters.
	//    Accounts, passwords and server settings survive -- that asymmetry
	//    is the whole reason the destroy set is backup.world_paths and not
	//    the data directory.
	// ------------------------------------------------------------------
	if !opt.KeepWorld {
		if cerr := confirm(cfg.Instance, opt); cerr != nil {
			return out, out.untouched(cerr, "nothing was stopped and nothing was deleted; "+
				"the world is untouched")
		}
		out.step("confirmed: %s will be archived and then DELETED. Accounts, passwords and "+
			"server settings are kept; characters and the map are not", cfg.Instance)
	} else {
		out.step("--keep-world: the mods are applied and the world stays (brief downtime for " +
			"the content-download boot)")
	}

	// ------------------------------------------------------------------
	// 3. QUIESCE. Mask the timers FIRST, then stop. In that order: a
	//    nightly that fires between the stop and the mask would start the
	//    server back up underneath step 4.
	// ------------------------------------------------------------------
	if merr := cfg.Timers.Mask(ctx); merr != nil {
		return out, out.untouched(merr, "the job timers could not be masked, so the rebuild "+
			"refused to stop the server; the world is untouched")
	}
	// Unmasking is this step's own defer and runs on every path, including a
	// panic -- and on a CANCELLED context, which is the path that matters.
	// systemd reaches TimeoutStartSec or the operator presses ^C during the
	// 40-60 minute first boot of a large workshop list, and an Unmask issued
	// on the job's own context would fail with context canceled: every real
	// implementation is a D-Bus call through hostops, whose job() selects on
	// ctx.Done(). A box left with its nightlies masked is a box with no
	// backups, nobody notices for a week, and nothing else in gvm looks at
	// gvm-job@<n>_*.timer -- not the running guard (which has no Mask by
	// design), not the sweep, not lifecycle's derive, which reads the
	// instance's own unit. So this detaches exactly as internal/jobs' guard
	// and unwind do.
	//
	// The asymmetry with restore() below is deliberate: the
	// never-leaves-stopped guarantee is backstopped by the running guard,
	// which already detaches. The unmask is backstopped by nothing.
	defer func() {
		uctx, ucancel := context.WithTimeout(context.WithoutCancel(ctx), unmaskBudget)
		defer ucancel()
		if uerr := cfg.Timers.Unmask(uctx); uerr != nil {
			out.step("WARNING: the job timers could not be unmasked: %v -- run "+
				"`systemctl unmask gvm-job@%s_*.timer` by hand, or this instance has no "+
				"nightly backup", uerr, cfg.Instance)
		}
	}()
	out.step("masked this instance's job timers so a nightly cannot fire mid-rebuild")

	if serr := stop(ctx, cfg, out); serr != nil {
		return out, out.untouched(serr, "the shutdown ladder failed, so the rebuild stopped "+
			"before touching content; the world is untouched")
	}

	// From here until step 8 the server is DOWN and the world is INTACT, and
	// every exit has to restore the former and say so about the latter.
	//
	// restore is NOT a defer, deliberately. The never-leaves-stopped guarantee
	// belongs to internal/jobs' running guard, which is armed before this
	// function is entered and survives a panic, a cancelled context and a
	// SIGKILL'd process in ways a closure in this file cannot. What this is,
	// is section 13.6's own instruction -- step 5's "restart the server, exit
	// non-zero, world untouched" -- so that a rebuild run from the CLI with no
	// job wrapped around it still hands the operator back a running server.
	// It is idempotent (Start on a running unit is a no-op), and its failure is
	// recorded rather than substituted for the real error.
	restore := func() {
		if serr := cfg.Server.Start(ctx); serr != nil {
			out.step("WARNING: could not start %s again: %v -- the world is intact; "+
				"`gvm start %s`", cfg.Instance, serr, cfg.Instance)
			out.Running = false
			return
		}
		out.Running = true
	}

	// ------------------------------------------------------------------
	// 4. MATERIALIZE. Write the acquisition list, boot once to download,
	//    wait for the readiness marker, clean-stop. World still intact.
	// ------------------------------------------------------------------
	if werr := cfg.Content.Write(ctx); werr != nil {
		restore()
		return out, out.untouched(werr, "the acquisition list could not be written, so the "+
			"download boot never happened; the world is untouched")
	}
	out.step("wrote the acquisition list")

	if berr := cfg.Server.Start(ctx); berr != nil {
		// No restore() here: the call that would perform it is the one that
		// just failed, and running it twice produces a second identical error
		// rather than a running server. The job layer's running guard is the
		// thing that keeps trying.
		return out, out.untouched(berr, "boot #1 (the content download) could not be started, "+
			"so the server is DOWN and the world is untouched")
	}
	out.Running = true
	out.step("boot #1: downloading content with the old world still in place")

	if werr := cfg.Server.WaitReady(ctx, cfg.bootBudget()); werr != nil {
		return out, out.untouched(werr, "the download boot never reported ready; the world "+
			"was NOT touched -- read the console and rerun")
	}
	out.step("boot #1 reported ready")

	// Step 4 ends with a CLEAN-STOP, and everything below it depends on that.
	// The preserve archive is the only copy of everyone's characters, and a
	// JVM still writing map chunks either trips archive.Create's per-file
	// re-hash (ErrDrifted, which makes the rebuild impossible to complete on
	// a populated server) or produces a tarball that verifies while holding
	// the chunks, the player DB and the accounts DB captured at different
	// instants. Step 8 then unlinks paths that process holds open, and the
	// next autosave writes some of them back. Stopping HERE rather than
	// arming a freeze is also why Archiver has no Freeze method: a stopped
	// world needs no lease. rebuild.sh's pz_boot_and_wait sends `quit` after
	// the marker and drains the screen session for the same reason, which is
	// what makes its tar and its rm -rf safe.
	//
	// It is also what makes step 10's Start a real boot rather than a no-op
	// on an already-active unit, and what makes step 5's and step 6's
	// restore() -- and the --keep-world exit's -- the real restart section
	// 13.6 says they are.
	if serr := stop(ctx, cfg, out); serr != nil {
		restore()
		return out, out.untouched(serr, "the download boot could not be stopped, so the "+
			"pre-rebuild archive was NOT taken from a live world and nothing was deleted; "+
			"the world is untouched")
	}

	// ------------------------------------------------------------------
	// 5. VERIFY CONTENT. Every acquire id present and non-empty. This is
	//    rebuild.sh's NOT_DOWNLOADED loop with D-33's quiescence instead of
	//    its advice to rerun the script.
	// ------------------------------------------------------------------
	in, serr := cfg.Content.Settlement(ctx)
	if serr != nil {
		return out, out.untouched(serr, "the expectation set could not be read, so gvm cannot "+
			"prove the content arrived; the world is untouched")
	}
	settler := &update.Settler{
		Instance: cfg.Instance,
		Expect:   in.Expect,
		Roots:    in.Roots,
		Poll:     cfg.Poll,
		Quiet:    cfg.Quiet,
		Budget:   cfg.settleBudget(),
		Now:      cfg.Now,
		Notes:    cfg.Notes,
	}
	st, serr := settler.Wait(ctx)
	out.Missing = st.Missing
	switch {
	case serr != nil:
		return out, out.untouched(serr, "the content wait ended without proving every item "+
			"arrived; the world is untouched")
	case len(st.Missing) > 0:
		// rebuild.sh's own message, with the server put back: "Not
		// downloaded yet: ... - the world was NOT touched. Steam stalls
		// sometimes: rerun."
		restore()
		return out, out.untouched(
			fmt.Errorf("%w: %s", ErrContentMissing, strings.Join(st.Missing, " ")),
			"%s never downloaded; the world was NOT touched. Steam stalls: rerun the rebuild "+
				"(--no-fetch is fine). If one never arrives, check that the item is still up",
			strings.Join(st.Missing, " "))
	case !st.Positive:
		restore()
		return out, out.untouched(
			errors.New("rebuild: the content wait went quiet without the positive signal"),
			"nothing is missing by name, which means a declared content root matched nothing; "+
				"the world is untouched")
	}
	out.step("every expected item is present and non-empty after %s",
		st.Waited.Round(time.Second))

	// ------------------------------------------------------------------
	// 6. RESOLVE the activation ids. Failure is the same answer as step 5:
	//    world untouched, server back up, say which ids could not be named.
	// ------------------------------------------------------------------
	res, rerr := cfg.Content.Resolve(ctx)
	if rerr != nil {
		restore()
		return out, out.untouched(rerr, "mod id resolution failed, so the world was NOT "+
			"touched; fix the report above and rerun")
	}
	out.Resolution = res
	if n := len(res.Ambiguous); n > 0 {
		restore()
		return out, out.untouched(
			fmt.Errorf("rebuild: %d activation id(s) could not be determined: %s", n,
				strings.Join(res.Ambiguous, " ")),
			"a guessed activation name silently omits a mod while the server boots normally "+
				"(D-21), so the rebuild refused; the world is untouched")
	}
	out.step("resolved the activation list: %d id(s)", len(res.After))

	// ------------------------------------------------------------------
	//    --keep-world exits HERE: hooks, start, unmask, done. It is the
	//    whole of rebuild.sh's --keep-world branch and it never reaches the
	//    archive, which is why it needs no confirmation.
	// ------------------------------------------------------------------
	if opt.KeepWorld {
		if herr := applyHooks(ctx, cfg, out); herr != nil {
			restore()
			return out, out.untouched(herr, "the post-rebuild hooks failed with the world "+
				"kept; the world is untouched")
		}
		restore()
		out.step("done: the content list is applied and the world was kept")
		return out, out.untouched(nil, "--keep-world: the world is untouched by design")
	}

	// ------------------------------------------------------------------
	// 7. ARCHIVE, class=preserve. The last point of no return is AFTER
	//    this, and that is the invariant: disk preflight, write, verify.
	//    Any failure here -> DO NOT DELETE.
	// ------------------------------------------------------------------
	if perr := cfg.Archives.Preflight(ctx); perr != nil {
		restore()
		return out, out.untouched(perr, "there is not enough room for the pre-rebuild archive, "+
			"so nothing is deleted; the world is untouched")
	}
	ref, aerr := cfg.Archives.Snapshot(ctx, archive.ClassPreserve, "pre-rebuild")
	if aerr != nil {
		restore()
		return out, out.untouched(fmt.Errorf("%w: %v", ErrArchive, aerr),
			"the archive failed, so the world is NOT deleted; check free disk space (df -h)")
	}
	if ref == nil {
		restore()
		return out, out.untouched(fmt.Errorf("%w: the engine reported success and returned no "+
			"ref", ErrArchive), "there is no receipt, so nothing is deleted; the world is untouched")
	}
	out.step("archived the old world to %s (%s, preserve -- never pruned by any code path)",
		ref.Path, spec.FormatBytes(ref.Bytes))

	rep, verr := cfg.Archives.VerifyReport(ctx, ref)
	if verr != nil {
		restore()
		return out, out.untouched(fmt.Errorf("%w: %v", ErrArchive, verr),
			"%s did not verify, which is exactly the failure archive-before-destroy exists to "+
				"catch; the world is NOT deleted and that file is on disk for you to inspect",
			ref.Path)
	}
	if !ref.Verified() {
		restore()
		return out, out.untouched(fmt.Errorf("%w: %s was reported verified and its VerifiedAt "+
			"is still zero", ErrArchive, ref.Path),
			"an unverified Ref is not a receipt, so the world is NOT deleted")
	}
	// The MODE is a precondition and not a label. archive.VerifyRef stamps
	// VerifiedAt for a shallow check too -- it compares no member contents --
	// and instance.Destroy and archive.Destroy both gate only on
	// ref.Verified(), so a shallow receipt would satisfy every downstream
	// guard while proving nothing about the bytes about to become the only
	// copy of this world. Section 13.6 step 7 is "verify --spot 24. Any
	// failure -> DO NOT DELETE", and section 13.1 calls spot the mode run
	// automatically before every destructive action; this is that sentence
	// expressed as a refusal instead of as a printf, the way
	// `gvm destroy` already forces ModeSpot at its own destroy site.
	if rep == nil || rep.Mode == archive.ModeShallow {
		restore()
		mode := "nothing"
		if rep != nil {
			mode = string(rep.Mode)
		}
		return out, out.untouched(fmt.Errorf("%w: %s was verified with %s", ErrArchive,
			ref.Path, mode),
			"a shallow check compares no member contents, so it is not the receipt section "+
				"13.6 step 7 requires before a world is deleted; the world is NOT deleted and "+
				"that archive is on disk")
	}
	out.Archive = ref
	out.Restore = restoreCommand(cfg.Instance, ref)
	out.step("verified %s: %s-verified %d/%d member(s)", ref.Path, rep.Mode, rep.Checked,
		rep.Members)

	// ------------------------------------------------------------------
	// 8. DESTROY. The only destructive step, and the first line of this
	//    function that can make WorldTouched true. Everything it needs to
	//    be safe -- a verified Ref, a jail that only promotes declared
	//    world paths -- is enforced by instance.Destroy and not by the
	//    ordering above; the ordering is what makes the enforcement
	//    reachable.
	// ------------------------------------------------------------------
	out.WorldTouched = true
	out.Untouched = ""
	if derr := cfg.Destroyer.Destroy(ctx, ref, cfg.WorldPaths); derr != nil {
		restore()
		return out, fmt.Errorf("rebuild: %s: the destroy failed partway; the archive at %s is "+
			"verified and intact, and `gvm restore` is the way back: %w",
			cfg.Instance, ref.Path, derr)
	}
	out.step("deleted the old world (%d declared path(s))", len(cfg.WorldPaths))

	// ------------------------------------------------------------------
	// 9. APPLY HOOKS, then 10. START. The new world generates on boot with
	//    the full content list and the difficulty settings active.
	// ------------------------------------------------------------------
	if herr := applyHooks(ctx, cfg, out); herr != nil {
		// The world is already gone, so this is not a refusal -- it is a
		// degraded rebuild. The server still has to come back up.
		out.step("WARNING: the post-rebuild hooks failed: %v -- the new world will generate "+
			"with the game's own defaults", herr)
	}
	if serr := cfg.Server.Start(ctx); serr != nil {
		return out, fmt.Errorf("rebuild: %s: the old world is archived at %s and deleted, and "+
			"the server did not start: %w", cfg.Instance, ref.Path, serr)
	}
	out.Running = true
	out.step("started %s: the new world generates now (several minutes)", cfg.Instance)
	return out, nil
}

// confirm is step 2, and it is rebuild.sh's prompt -- checked by the one
// implementation of section 16.4's contract rather than by a copy of it.
//
// The operator types the instance name. Not "y", not "yes": the point of
// typing the name is that it cannot be answered by muscle memory.
//
// The trigger is TriggerCLI because this entry point IS the local one: Run is
// reached from `gvm rebuild` on the box, while a request that crossed a network
// arrives through internal/jobs' engine, which checks the same Confirmation
// against the trigger it actually came from and refuses `--yes` there. Passing
// the real trigger down here would be better still and is not possible without
// giving this package a field whose only use is to weaken its own check.
func confirm(name string, opt Options) error {
	c := &jobs.Confirmation{Typed: opt.Typed, Automated: opt.Yes}
	return c.Check(name, lifecycle.JobRebuild, jobs.TriggerCLI)
}

func stop(ctx context.Context, cfg Config, out *Outcome) error {
	o, err := cfg.Server.Stop(ctx, cfg.stopBudget())
	if err != nil {
		return err
	}
	out.Running = false
	if o.Degraded {
		out.step("the shutdown ladder needed a SIGKILL (rung %q); the world may have lost up "+
			"to one autosave interval, which the preserve archive below captures as it is",
			o.Rung)
		return nil
	}
	out.step("stopped %s cleanly (rung %q in %s)", cfg.Instance, o.Rung,
		o.Took.Round(time.Second))
	return nil
}

func applyHooks(ctx context.Context, cfg Config, out *Outcome) error {
	if cfg.Hooks == nil {
		return nil
	}
	if err := cfg.Hooks.Apply(ctx); err != nil {
		return err
	}
	out.step("applied the post-rebuild hooks")
	return nil
}

func describePlan(out *Outcome, plan jobs.ContentPlan) {
	switch {
	case len(plan.Added) == 0 && len(plan.Removed) == 0:
		out.step("the acquisition list already matches the source")
	default:
		if len(plan.Added) > 0 {
			out.step("new from the source: %s", strings.Join(plan.Added, " "))
		}
		if len(plan.Removed) > 0 {
			out.step("no longer in the source, removing: %s", strings.Join(plan.Removed, " "))
		}
	}
	if plan.Note != "" {
		out.step("%s", plan.Note)
	}
}

// restoreCommand is the last line rebuild.sh printed, as one command.
//
// The bash original closed with a three-line recipe -- stop, tar -xzf, chown,
// start -- which is four chances to get a path wrong at the exact moment the
// operator is already upset. `gvm restore` is the same thing with the ownership
// and the verification inside it.
func restoreCommand(name string, ref *archive.Ref) string {
	if ref == nil {
		return ""
	}
	return fmt.Sprintf("sudo gvm restore %s --instance %s", ref.Path, name)
}

func (c Config) validate() error {
	var errs []error
	if err := instance.ValidateName(c.Instance); err != nil {
		errs = append(errs, fmt.Errorf("%w: %v", ErrConfig, err))
	}
	for _, m := range []struct {
		nil  bool
		what string
	}{
		{c.Content == nil, "a content pipeline (steps 1, 4, 5, 6)"},
		{c.Server == nil, "a server seam (steps 3, 4, 10)"},
		{c.Timers == nil, "a timer seam (step 3)"},
		{c.Archives == nil, "an archive engine (step 7)"},
		{c.Destroyer == nil, "a destroyer (step 8)"},
	} {
		if m.nil {
			errs = append(errs, fmt.Errorf("%w: a rebuild needs %s", ErrConfig, m.what))
		}
	}
	if len(c.WorldPaths) == 0 {
		errs = append(errs, fmt.Errorf("%w: no world paths; a rebuild that destroys nothing "+
			"would report success having done nothing, and backup.world_paths is the destroy "+
			"set", ErrConfig))
	}
	return errors.Join(errs...)
}

func (c Config) bootBudget() time.Duration {
	if c.BootBudget > 0 {
		return c.BootBudget
	}
	return DefaultBootBudget
}

func (c Config) stopBudget() time.Duration {
	if c.StopBudget > 0 {
		return c.StopBudget
	}
	return DefaultStopBudget
}

func (c Config) settleBudget() time.Duration {
	if c.SettleBudget > 0 {
		return c.SettleBudget
	}
	return update.BudgetRebuild
}

// Job is the adapter internal/jobs' `rebuild` runner drives.
//
// Two methods, matching that runner's two steps: Plan is section 13.6 step 1,
// which is reads-only and refuses before anything stops, and Run is steps 2 to
// 10. The job layer deliberately does not express steps 2-10 as rows in its own
// table -- the ordering is the safety property and lives in Run alone.
type Job struct {
	Cfg Config
	Opt Options

	// planned is step 1's answer, kept so that Run does not fetch the
	// collection a second time. That is not a micro-optimisation: the fetch
	// is an HTTP request to a Steam endpoint that rate-limits, and
	// rebuild.sh's own failure message names rate-limiting as the likely
	// cause of a refused rebuild. Asking twice per rebuild doubles the chance
	// of being told no.
	planned *jobs.ContentPlan
}

// Compile-time proof that this is what the `rebuild` runner asks for. The
// interface lives in internal/jobs beside the step table that calls it.
var _ jobs.Rebuilds = (*Job)(nil)

// Plan is step 1.
func (j *Job) Plan(ctx context.Context) (jobs.ContentPlan, error) {
	if j.Cfg.Content == nil {
		return jobs.ContentPlan{}, fmt.Errorf("%w: a rebuild needs a content pipeline", ErrConfig)
	}
	p, err := j.Cfg.Content.Plan(ctx, !j.Opt.NoFetch)
	if err != nil {
		return p, err
	}
	j.planned = &p
	return p, nil
}

// Run is steps 2 to 10.
func (j *Job) Run(ctx context.Context) (jobs.Rebuilt, error) {
	cfg := j.Cfg
	if j.planned != nil && cfg.Content != nil {
		cfg.Content = &replayPlan{inner: cfg.Content, plan: *j.planned}
	}
	out, err := Run(ctx, cfg, j.Opt)
	res := jobs.Rebuilt{}
	if out != nil {
		res = jobs.Rebuilt{
			WorldTouched: out.WorldTouched,
			Untouched:    out.Untouched,
			Archive:      out.Archive,
			Missing:      out.Missing,
			Restore:      out.Restore,
			Running:      out.Running,
			Steps:        out.Steps,
		}
	}
	return res, err
}

// replayPlan hands Run the plan step 1 already paid for, exactly once.
//
// Exactly once, and not forever: if some future step asked for a second plan it
// would be asking a different question, and silently answering it with a stale
// one is how a rebuild acts on a list that changed underneath it. The second
// call goes to the real source.
type replayPlan struct {
	inner  Content
	plan   jobs.ContentPlan
	served bool
}

func (r *replayPlan) Plan(ctx context.Context, fetch bool) (jobs.ContentPlan, error) {
	if !r.served {
		r.served = true
		return r.plan, nil
	}
	return r.inner.Plan(ctx, fetch)
}

func (r *replayPlan) Write(ctx context.Context) error { return r.inner.Write(ctx) }

func (r *replayPlan) Settlement(ctx context.Context) (update.SettleInput, error) {
	return r.inner.Settlement(ctx)
}

func (r *replayPlan) Resolve(ctx context.Context) (jobs.Resolution, error) {
	return r.inner.Resolve(ctx)
}

// JailDestroyer is the production step 8.
//
// It is four lines, and every one of them is a refusal somebody else wrote:
// Jail.Rebuild refuses a path the jail does not already protect, ResolveAll
// refuses "..", symlink escapes and paths across a mount, and instance.Destroy
// refuses a nil or unverified Ref before it reaches archive.Destroy, which
// re-asserts both. There is nothing left here to get wrong, which is the point.
type JailDestroyer struct {
	// Jail is the instance's destroy jail, with the world paths still
	// PROTECTED. Rebuild is what promotes them, and it is called here rather
	// than by the caller so that a widened jail cannot be handed in.
	Jail *instance.Jail
}

// Destroy promotes the declared world paths and deletes them.
func (d JailDestroyer) Destroy(ctx context.Context, ref *archive.Ref, worldPaths []string) error {
	if d.Jail == nil {
		return fmt.Errorf("%w: the destroyer has no jail, and there is no path by which a "+
			"rebuild deletes a directory without one", ErrConfig)
	}
	j, err := d.Jail.Rebuild(worldPaths)
	if err != nil {
		return err
	}
	paths, err := j.ResolveAll(worldPaths)
	if err != nil {
		return err
	}
	return instance.Destroy(ctx, ref, paths)
}
