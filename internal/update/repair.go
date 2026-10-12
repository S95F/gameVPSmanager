package update

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// `gvm update <n> --repair`: the ONLY path in gvm that emits SteamCMD
// `validate`.
//
// update.sh ran it every night:
//
//	as_pz "timeout 1800 '$STEAMCMD_DIR/steamcmd.sh' +force_install_dir '$INSTALL_DIR' \
//	    +login anonymous +app_update $PZ_APPID validate +quit"
//
// and that is the single most consequential difference between the bash suite
// and this one. `validate` does not check the install -- it REWRITES every file
// that differs from Steam's copy. On a tree gvm created that is merely slow. On
// an adopted tree it is destructive: the operator's edited start-server.sh,
// their patched JVM arguments, anything they fixed by hand, silently replaced,
// on a timer, at 05:00, with no record that it happened.
//
// So D-32 removes it from the nightly and pays for the removal with a mandatory
// post-verification (phase 6), and what is left is this: a deliberate repair
// action that
//
//  1. refuses unless it was explicitly asked for -- a flag, never a fallback,
//     and never something phase 6 escalates to on its own;
//  2. refuses outright on an adopted instance;
//  3. NAMES THE FILES IT IS ABOUT TO RISK, from Inspect's own record of hand
//     edits and unmanaged files, before anything runs;
//  4. takes a `preserve` archive first and VERIFIES it, because a preserve
//     archive is never pruned by any code path and is therefore the one copy
//     of those files that will still exist next month;
//  5. only then plans the repair intent and applies it.
//
// Step 4 before step 5 is the same ordering invariant as rebuild.sh's, for the
// same reason: every failure before the archive leaves the install untouched,
// and a failed archive verification aborts before anything is overwritten.

// Repair refusals.
var (
	// ErrRepairNotRequested is a repair that nobody asked for. `validate`
	// overwrites customised files, so it cannot be reached by a code path
	// deciding on the operator's behalf that it would help.
	ErrRepairNotRequested = errors.New("update: a repair must be asked for explicitly")
	// ErrRepairNoArchive is a repair with no archive engine wired. It is a
	// refusal rather than a warning -- unlike the pre-update backup, which is
	// update.sh's `|| warn` -- because the thing about to happen is an
	// overwrite of files the operator may have edited, and the archive is the
	// only way back.
	ErrRepairNoArchive = errors.New("update: a repair needs an archive engine")
	// ErrRepairNoValidate is a repair plan that would not actually validate.
	// A manifest whose validate_on omits `repair` has declared that this game
	// is not repaired that way, and running the plan anyway would be a
	// pointless full download dressed as a repair.
	ErrRepairNoValidate = errors.New("update: this manifest does not declare validate_on: repair")
)

// RepairRequest is the operator's instruction.
type RepairRequest struct {
	// Confirmed is the explicit flag. False is ErrRepairNotRequested, and
	// there is no default that sets it: the CLI sets it from `--repair` and
	// nothing else does.
	Confirmed bool
	// Reason goes in the preserve archive's manifest and the audit row. A
	// repair with no sentence beside it is a row nobody can act on a month
	// later, when the only question is why half the install tree has Steam's
	// mtimes.
	Reason string
	// DryRun stops after the at-risk list. It is how an operator sees what a
	// repair would overwrite without overwriting it, which is the whole of
	// point 3 above.
	DryRun bool
}

// RepairReport is what happened, in the order it happened.
type RepairReport struct {
	// AtRisk are the files `validate` may overwrite: the provider's own record
	// of what differs from what it wrote, plus the files it never wrote at
	// all. It is printed BEFORE anything runs.
	AtRisk []string
	// AtRiskKnown is false when the provider keeps no such record. "Nothing
	// differs" and "nobody looked" must never be the same answer, and on this
	// path the difference decides whether an operator should proceed.
	AtRiskKnown bool
	// Archive is the verified preserve archive. Non-nil on every path that
	// reached the apply, by construction.
	Archive *archive.Ref
	// Applied is the driver's record of the run.
	Applied *provider.Applied
	// Verified is whether the post-repair D-32 check passed.
	Verified bool
	// Steps is the narration, suitable for the CLI and the job log.
	Steps []string
	// DryRun echoes the request, so a report cannot be mistaken for a run.
	DryRun bool
}

func (r *RepairReport) step(format string, args ...any) {
	r.Steps = append(r.Steps, fmt.Sprintf(format, args...))
}

// Repair is the `validate` path.
//
// It returns a non-nil report alongside an error whenever the run got far
// enough to have learned something -- the at-risk list and the archive are
// exactly what an operator needs when a repair refused, and losing them to a
// bare error would make the refusal useless.
func (p *Pipeline) Repair(ctx context.Context, req RepairRequest) (*RepairReport, error) {
	rep := &RepairReport{DryRun: req.DryRun}
	if !req.Confirmed {
		return rep, fmt.Errorf("%w: `validate` rewrites every file in the install tree that "+
			"differs from Steam's copy, including ones you edited. Run `gvm update %s --repair`",
			ErrRepairNotRequested, p.cfg.Instance)
	}
	if err := p.validate(); err != nil {
		return rep, err
	}
	if !p.cfg.ManagedInstall {
		return rep, fmt.Errorf("%w: %s is adopted, so gvm never runs `validate` against files "+
			"that predate it; `gvm instance promote %s` is what changes that",
			ErrAdopted, p.cfg.Instance, p.cfg.Instance)
	}

	// 1. Name what is at risk. This happens first, before the archive and
	//    before the plan, because it is the only part of a repair an operator
	//    can act on -- and because a dry run has to be able to stop here
	//    having touched nothing at all.
	insp, err := p.cfg.Installer.Inspect(ctx)
	if err != nil {
		return rep, fmt.Errorf("update: %s: repair: inspecting the install before risking it: %w",
			p.cfg.Instance, err)
	}
	rep.AtRisk, rep.AtRiskKnown = atRisk(insp)
	switch {
	case !rep.AtRiskKnown:
		rep.step("this provider keeps no record of what it wrote, so gvm cannot say which " +
			"files `validate` would overwrite -- which is itself a reason to read the archive " +
			"path below before continuing")
	case len(rep.AtRisk) == 0:
		rep.step("no file in the install tree differs from the provider's own record, so " +
			"`validate` should overwrite nothing; it will still re-download anything Steam " +
			"considers wrong")
	default:
		rep.step("`validate` may overwrite %d file(s) that differ from what gvm installed: %s",
			len(rep.AtRisk), strings.Join(rep.AtRisk, " "))
	}
	if req.DryRun {
		rep.step("dry run: nothing was archived, nothing was downloaded and nothing was " +
			"overwritten")
		return rep, nil
	}

	// 2. The preserve archive, verified. Before the plan, because the plan is
	//    a network read that can take minutes and the operator's files should
	//    not be sitting unprotected for them -- and before the apply by the
	//    same invariant rebuild.sh states: every destructive step happens only
	//    after what came before it is verified.
	if p.cfg.Archives == nil {
		return rep, fmt.Errorf("%w: a repair overwrites files the operator may have edited, "+
			"and the preserve archive is the only way back", ErrRepairNoArchive)
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "pre-repair"
	}
	ref, err := p.cfg.Archives.Snapshot(ctx, archive.ClassPreserve, reason)
	if err != nil {
		return rep, fmt.Errorf("update: %s: repair: the preserve archive failed, so NOTHING "+
			"was validated and the install is untouched: %w", p.cfg.Instance, err)
	}
	if ref == nil {
		return rep, fmt.Errorf("%w: the archive engine reported success and returned no ref, "+
			"so nothing was validated and the install is untouched", ErrConfig)
	}
	if err := p.cfg.Archives.Verify(ctx, ref); err != nil {
		return rep, fmt.Errorf("update: %s: repair: %s failed verification, so NOTHING was "+
			"validated and the install is untouched: %w", p.cfg.Instance, ref.Path, err)
	}
	if !ref.Verified() {
		return rep, fmt.Errorf("%w: %s was reported verified and its VerifiedAt is still zero; "+
			"nothing was validated and the install is untouched", ErrConfig, ref.Path)
	}
	rep.Archive = ref
	rep.step("preserve archive %s (%s), verified -- never pruned by any code path",
		ref.Path, spec.FormatBytes(ref.Bytes))

	// 3. Plan and apply the repair intent. This is the one call in gvm whose
	//    runscript carries the positional `validate`.
	pl, err := p.cfg.Installer.Plan(ctx, provider.IntentRepair)
	if err != nil {
		return rep, fmt.Errorf("update: %s: repair: planning: %w", p.cfg.Instance, err)
	}
	if pl == nil {
		return rep, fmt.Errorf("%w: the provider returned no repair plan and no error", ErrConfig)
	}
	if !pl.Destructive && !pl.Writes() {
		// A repair that would write nothing is a repair this manifest does
		// not describe. Applying it would be a full re-download reported as a
		// repair, and the honest answer is to say which declaration is
		// missing.
		return rep, fmt.Errorf("%w: the repair plan for %s writes nothing (%s)",
			ErrRepairNoValidate, p.cfg.Instance, pl.Reason)
	}
	rep.step("repair plan: %s", pl.Reason)
	if err := pl.Authorize(p.cfg.ManagedInstall); err != nil {
		return rep, err
	}
	applied, err := p.cfg.Installer.Apply(ctx, pl)
	rep.Applied = applied
	if err != nil {
		return rep, fmt.Errorf("update: %s: repair: %w (the preserve archive at %s is intact)",
			p.cfg.Instance, err, ref.Path)
	}

	// 4. The D-32 check again. A repair that did not end in a verified install
	//    is a repair that did not work, and saying so is the difference
	//    between one more repair and a week of a server that starts and dies.
	if err := p.PostVerify(ctx); err != nil {
		return rep, err
	}
	rep.Verified = true
	rep.step("post-repair verification passed (D-32)")
	return rep, nil
}

// atRisk is the file list `validate` may overwrite.
//
// HandEdits first, because those are files the provider KNOWS differ from what
// it wrote -- the operator's own changes. Unmanaged after, because a file the
// provider never wrote is either the operator's or left over from an older
// install, and `validate` treats both the same way. The bool is the honest
// "nobody looked": a provider with no record of what it wrote reports none, and
// an empty list from such a provider must not read as "nothing is at risk".
func atRisk(insp *provider.Inspection) ([]string, bool) {
	if insp == nil {
		return nil, false
	}
	known := insp.Integrity.Checked || len(insp.HandEdits) > 0 || len(insp.Unmanaged) > 0
	seen := map[string]bool{}
	var out []string
	for _, group := range [][]string{insp.HandEdits, insp.Unmanaged} {
		names := append([]string(nil), group...)
		sort.Strings(names)
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, known
}
