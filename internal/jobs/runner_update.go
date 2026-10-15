package jobs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The `update` kind: section 13.5, and the job update.sh's comment is about.
//
// The biggest win over the bash original is that on a normal night NOTHING
// STOPS. update.sh stops unconditionally and runs `app_update ... validate`
// every time; on a multi-instance box that is N full validations a night, each
// one preceded by a stop. Here phases 1 and 2 are network READS, the decision
// is a pure function over their two answers, and `skipped` is a first-class
// success (D-43). The server is never touched on the nights when there is
// nothing to do, which is most of them.
//
// Where it deliberately keeps the bash behaviour, it keeps it exactly:
//
//	"$SCRIPT_DIR/backup.sh" || warn "Backup failed - continuing anyway."
//	... || warn "Steam update failed or timed out - starting on the existing binaries."
//
// Both of those are non-fatal steps below. The comment above the second one in
// update.sh states the trade in the operator's own words -- "an outdated server
// beats one left stopped" -- and that is this kind's whole failure policy.
//
// What it does NOT keep is `sleep 180` (D-33: phase 8 is quiescence detection)
// and `validate` (D-32: removed, and paid for by a MANDATORY post-verification
// that runs after every app_update whether or not the app_update reported
// success -- because steamcmd can exit 0 after printing Error!).

// Diff is one axis's answer from phases 1 and 2.
//
// Two axes and not one, because they fail differently: the build axis is one
// number against one number, and the content axis is a set of ids with
// timestamps. Collapsing them into a bool would make "a mod updated" and "the
// game patched" the same event, and they need different phases afterwards.
type Diff struct {
	// Changed is whether this axis wants work.
	Changed bool `json:"changed"`
	// From and To are the build ids on the build axis. Empty on the content
	// axis, where there is no single version.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Items are the content ids that changed, by name, so that a degraded run
	// says WHICH mod rather than how many.
	Items []string `json:"items,omitempty"`
	Note  string   `json:"note,omitempty"`
}

// Decision is the gate in section 13.5's pipeline, between phase 2 and phase 3.
//
// It is the line "DECISION" in that diagram and it exists as a type because
// everything expensive and everything dangerous is on the far side of it: the
// stop, the backup, the download. D-43 names the outcome on the near side --
// `skipped`, a first-class success -- and this is what produces it.
type Decision struct {
	// Skip ends the job before anything is touched.
	Skip bool `json:"skip"`
	// Stop says the applied work needs the server down.
	Stop bool `json:"stop"`
	// ApplyBuild is whether the binary update will be performed. It is false
	// for an adopted instance even when a newer build exists, because phase 5
	// is refused at the type level there.
	ApplyBuild bool `json:"apply_build"`
	// ApplyContent is whether the content phases will run.
	ApplyContent bool `json:"apply_content"`
	// UpdateAvailable is `summary.update_available`: a build gvm is NOT
	// allowed to install. It is reported rather than acted on, because the
	// operator of a freshly adopted box has a working nightly steamcmd update
	// and adoption must not silently take it away without saying so.
	UpdateAvailable bool `json:"update_available,omitempty"`
	// Reason is the sentence printed and recorded.
	Reason string `json:"reason,omitempty"`
}

// Decide is the gate, and it is PURE.
//
// Pure because it is the one piece of the update pipeline that must be
// provable on the development machine: every branch here either does or does
// not stop a live server, and the test for "no build diff and no content diff
// records skipped and the fake systemd sees zero stop calls" is only worth
// anything if the decision it exercises is the decision production makes.
//
// managedInstall is spec.ManagedInstall. False is the adoption flag, and the
// asymmetry it produces is deliberate: an adopted instance's nightly still
// does content work (PZ downloads its own workshop items on boot and needs no
// steamcmd for them) and never stops the server for a binary update gvm is not
// allowed to perform.
func Decide(instanceName string, build, content Diff, managedInstall bool) Decision {
	switch {
	case !build.Changed && !content.Changed:
		return Decision{
			Skip: true,
			Reason: "no Steam build change and no content change; the server was not " +
				"touched (D-43)",
		}

	case build.Changed && !managedInstall && !content.Changed:
		// Section 13.5's message, verbatim in shape: what is available, why
		// gvm will not install it, and the one command that changes that.
		return Decision{
			Skip:            true,
			UpdateAvailable: true,
			Reason: fmt.Sprintf("binary update %s -> %s available; %s is adopted "+
				"(managed_install: false). Taking over binary updates for an adopted "+
				"instance is not available in this build", build.From, build.To, instanceName),
		}
	}

	d := Decision{
		ApplyBuild:      build.Changed && managedInstall,
		ApplyContent:    content.Changed,
		UpdateAvailable: build.Changed && !managedInstall,
	}
	// A binary update rewrites the install tree and must have it quiet. A
	// content change needs the server down too, because the acquisition list
	// is read at boot -- but only where gvm owns the install; an adopted
	// instance's own running server does its workshop downloads itself, and
	// stopping it to achieve that would be a stop gvm chose and nobody asked
	// for.
	d.Stop = d.ApplyBuild || (d.ApplyContent && managedInstall)

	var why []string
	if build.Changed {
		why = append(why, fmt.Sprintf("build %s -> %s", build.From, build.To))
	}
	if content.Changed {
		if n := len(content.Items); n > 0 {
			why = append(why, fmt.Sprintf("%d content item(s) changed: %s", n,
				strings.Join(content.Items, " ")))
		} else {
			why = append(why, "content changed")
		}
	}
	if d.UpdateAvailable {
		why = append(why, fmt.Sprintf("%s is adopted, so the binary update is reported and "+
			"not installed", instanceName))
	}
	d.Reason = strings.Join(why, "; ")
	return d
}

// Settlement is phase 8's answer (D-33).
type Settlement struct {
	// Positive is the signal that actually proves the download finished:
	// every acquire id has a non-empty directory under a declared root. It is
	// exactly rebuild.sh's NOT_DOWNLOADED check, inverted.
	Positive bool `json:"positive"`
	// Missing are the ids that never arrived, BY NAME. A count would make the
	// operator go looking; a name is something they can paste into the
	// Workshop.
	Missing []string `json:"missing,omitempty"`
	// Waited is how long quiescence took, for the summary.
	Waited time.Duration `json:"waited,omitzero"`
}

// Resolution is phase 9's answer: the activation namespace, re-derived.
type Resolution struct {
	// Changed is whether the lists actually differ from the lock. It is what
	// phase 10 turns on, and the reason phase 10 exists at all: the server
	// read its config at boot, so a changed list needs one more cycle.
	Changed bool     `json:"changed"`
	Before  []string `json:"before,omitempty"`
	After   []string `json:"after,omitempty"`
	// Ambiguous are ids whose activation name could not be determined. They
	// are reported and never guessed at: a wrong answer silently omits a mod
	// while the server boots normally, which is the failure D-21 refuses.
	Ambiguous []string `json:"ambiguous,omitempty"`
}

// Updates is the update pipeline (internal/update, section 13.5).
//
// Phases 1, 2 and 8 are network READS and nothing else, which is the
// structural reason a blocked mod or a Steam outage costs no downtime: the
// decision happens before the stop.
type Updates interface {
	// Preflight is phase 0: free disk against the estimate, and the instance
	// is not held.
	Preflight(ctx context.Context) error
	// CheckBuild is phase 1: purge the cached appinfo.vdf, app_info_print,
	// compare the remote branch buildid to the local .acf.
	CheckBuild(ctx context.Context) (Diff, error)
	// CheckContent is phase 2: upstream metadata against the lock. Reads
	// only.
	CheckContent(ctx context.Context) (Diff, error)
	// Backup is phase 4: class=routine, reason='pre-update'.
	Backup(ctx context.Context) error
	// AppUpdate is phase 5: the runscript, no `validate`, in a transient
	// service as the instance user.
	AppUpdate(ctx context.Context) error
	// PostVerify is phase 6, and it is MANDATORY (D-32).
	PostVerify(ctx context.Context) error
	// Settle is phase 8: workshop quiescence, replacing sleep 180 (D-33).
	Settle(ctx context.Context) (Settlement, error)
	// Resolve is phase 9: activation ids from disk, diffed against the lock.
	Resolve(ctx context.Context) (Resolution, error)
	// Activate is phase 10's middle step: write the game's own config.
	Activate(ctx context.Context) error
}

var updateRunner = &Runner{
	Kind:    lifecycle.JobUpdate,
	Scope:   ScopeInstance,
	Policy:  PolicyStartAnyway,
	Stops:   true,
	Summary: "check, decide, and only then stop (section 13.5)",
	Steps: func(env *Env) ([]Step, error) {
		u := env.Updates
		if u == nil {
			return nil, fmt.Errorf("%w: an update job needs an update pipeline (Env.Updates)",
				ErrEnv)
		}
		var build, content Diff
		var dec Decision
		var res Resolution

		// The two gates every later phase reads. They are closures over the
		// decision rather than fields on the Run, so that a phase cannot run
		// because somebody wrote the wrong key into a map.
		ifStop := func(*Run) (bool, string) {
			return dec.Stop, "the decision did not require stopping the server"
		}
		ifBuild := func(*Run) (bool, string) {
			return dec.ApplyBuild, "no binary update is being applied"
		}
		ifContent := func(*Run) (bool, string) {
			return dec.ApplyContent, "no content change was detected"
		}
		ifCycle := func(*Run) (bool, string) {
			return res.Changed, "the activation lists did not change, so no second cycle is needed"
		}

		return []Step{
			{
				Name: "update.preflight", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error { return u.Preflight(ctx) },
			},
			{
				Name: "update.check_build", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error {
					d, err := u.CheckBuild(ctx)
					if err != nil {
						return err
					}
					build = d
					jr.Set("build_local", d.From)
					jr.Set("build_remote", d.To)
					return nil
				},
			},
			{
				Name: "update.check_content", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error {
					d, err := u.CheckContent(ctx)
					if err != nil {
						return err
					}
					content = d
					jr.Set("content_changed", d.Changed)
					if len(d.Items) > 0 {
						jr.Set("content_items", d.Items)
					}
					return nil
				},
			},
			{
				// THE DECISION. Everything above reads; everything below
				// acts. This is the only step in the table that can end the
				// job without having touched anything, and on most nights it
				// does.
				Name: "update.decide", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error {
					dec = Decide(env.Instance, build, content, env.ManagedInstall)
					jr.Set("decision", dec)
					if dec.UpdateAvailable {
						jr.Set("update_available", true)
					}
					if dec.Skip {
						jr.Note("%s", dec.Reason)
						return fmt.Errorf("%s: %w", dec.Reason, ErrSkipJob)
					}
					jr.Note("%s", dec.Reason)
					return nil
				},
			},
			withWhen(stopStep("update.stop", env), ifStop),
			{
				// Phase 4, non-fatal: update.sh does exactly this, and its
				// line is `|| warn "Backup failed - continuing anyway."`. The
				// reasoning is not laziness about backups -- it is that a
				// backup failure at 05:00 must not leave the server stopped
				// while nobody is watching.
				Name: "update.backup",
				When: ifStop,
				Do:   func(ctx context.Context, jr *Run) error { return u.Backup(ctx) },
			},
			{
				// Phase 5, non-fatal: "Steam update failed or timed out -
				// starting on the existing binaries." The classification
				// ladder and the three retries live in the driver (section
				// 13.5); by the time a failure reaches here, Steam has been
				// down for long enough that an outdated server is the right
				// answer.
				Name: "update.app_update",
				When: ifBuild,
				Do:   func(ctx context.Context, jr *Run) error { return u.AppUpdate(ctx) },
			},
			{
				// Phase 6, and the `When` is ifBuild rather than "the
				// app_update succeeded" ON PURPOSE. D-32 makes this check
				// mandatory after EVERY app_update, and the reason is the
				// failure mode it exists for: steamcmd self-updates,
				// re-execs, and can exit 0 after printing "Error!". A
				// post-verification that only ran when the exit code looked
				// good would skip exactly the runs it was written for.
				//
				// Non-fatal, because D-32's prescribed response to a failure
				// is NEEDS_REPAIR plus starting the server anyway -- never an
				// automatic `validate`, which overwrites files the operator
				// customised.
				Name: "update.post_verify",
				When: ifBuild,
				Do: func(ctx context.Context, jr *Run) error {
					if err := u.PostVerify(ctx); err != nil {
						jr.Set("needs_repair", true)
						jr.Note("post-update verification failed; the server is being started "+
							"anyway. Run `gvm update %s --repair` -- it is the only path that "+
							"emits `validate`, and it will overwrite customised files",
							env.Instance)
						return err
					}
					return nil
				},
			},
			withWhen(startStep("update.start", env), ifStop),
			{
				// Phase 8: quiescence, not `sleep 180` (D-33). 180s is
				// simultaneously too long on a no-op night and too short
				// after adding a 2 GB mod.
				Name: "update.settle",
				When: ifContent,
				Do: func(ctx context.Context, jr *Run) error {
					s, err := u.Settle(ctx)
					jr.Set("settle_waited_ms", s.Waited.Milliseconds())
					if err != nil {
						return err
					}
					if len(s.Missing) > 0 {
						jr.Set("content_missing", s.Missing)
						jr.Degraded = true
						return fmt.Errorf("content stopped changing without arriving: %s "+
							"never downloaded; Steam stalls, and `gvm doctor` will suggest "+
							"clearing the item's directory",
							strings.Join(s.Missing, " "))
					}
					if !s.Positive {
						jr.Degraded = true
						return fmt.Errorf("content went quiet without the positive signal; " +
							"nothing is missing by name, so this is a declared root that " +
							"matched nothing")
					}
					return nil
				},
			},
			{
				// Phase 9. Non-fatal: a resolution gvm could not complete
				// leaves the server running on the lists it already had,
				// which is sync-mods.sh's own `|| warn "Mod ID resolution
				// reported problems."`.
				Name: "update.resolve",
				When: ifContent,
				Do: func(ctx context.Context, jr *Run) error {
					r, err := u.Resolve(ctx)
					if err != nil {
						return err
					}
					res = r
					jr.Set("activation_before", r.Before)
					jr.Set("activation_after", r.After)
					if len(r.Ambiguous) > 0 {
						jr.Set("activation_ambiguous", r.Ambiguous)
						jr.Degraded = true
						jr.Note("could not determine the activation name for %s; they are "+
							"reported rather than guessed at, because a wrong answer omits a "+
							"mod while the server boots normally (D-21)",
							strings.Join(r.Ambiguous, " "))
					}
					return nil
				},
			},
			// Phase 10, as three steps rather than one. The server read its
			// config at boot, so a changed list needs one more cycle -- and
			// the stop in the middle of it is as capable of leaving a world
			// down as the first one, which is why it is a declared stopping
			// step with the guard in front of it.
			withWhen(stopStep("update.cycle_stop", env), ifCycle),
			{
				Name: "update.activate", Fatal: true,
				When: ifCycle,
				Do:   func(ctx context.Context, jr *Run) error { return u.Activate(ctx) },
			},
			withWhen(startStep("update.cycle_start", env), ifCycle),
		}, nil
	},
}

// withWhen gates a shared step constructor.
//
// The stop and start steps are built by runner_restart.go so that every kind
// runs the same ladder and records the same rung; the gate is per-kind, so it
// is applied here rather than threaded through every constructor's arguments.
func withWhen(s Step, when func(*Run) (bool, string)) Step {
	s.When = when
	return s
}
