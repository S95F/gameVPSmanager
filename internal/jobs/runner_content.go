package jobs

import (
	"context"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The `content_sync` kind: resolve, materialize, activate (section 12,
// section 13.3).
//
// It stops the server "only to write config", which is the third column of
// section 13.3's table, and the two fail-closed gates below are what make that
// affordable.
//
// UNACCOUNTED is rebuild.sh lines 77-80 generalised. That script counts the
// item ids it parsed out of a collection page and compares the number to a
// count taken straight from the raw HTML, and refuses to rewrite mods.txt if
// they disagree -- because a changed Steam markup silently drops mods, and a
// server that boots normally with four mods missing is a worse outcome than
// one that refuses to sync. internal/content.Plan carries that count as
// Unaccounted; this table refuses on it before anything is stopped.
//
// BLOCKED is section 13.5's CurseForge paragraph: if any file is BLOCKED the
// plan writes blocked-mods.json, the instance is AWAITING_MANUAL_FILES, and
// the job exits WITHOUT STOPPING THE SERVER. A modpack whose author revoked
// third-party download rights must not cost downtime.

// ContentPlan is the plan phase's answer, as this layer needs it.
//
// It is a projection of internal/content's own plan rather than that type: the
// job layer needs four facts to decide whether to stop a server, and importing
// the resolver registry, the blob store and the fetcher to learn them would put
// all three into the dependency graph of anything that wanted to print a phase
// list.
type ContentPlan struct {
	// Changed is whether anything needs doing.
	Changed bool `json:"changed"`
	// NeedsRestart is whether activating the change requires the server down.
	// For Project Zomboid, writing WorkshopItems= does; for a server that
	// rereads its mod list on a console command, it does not (section 12.6).
	NeedsRestart bool `json:"needs_restart"`
	// Unaccounted is the fail-closed gate: items the parse could not account
	// for. Non-empty refuses the sync.
	Unaccounted []string `json:"unaccounted,omitempty"`
	// Blocked is content a human has to fetch by hand (section 12.8).
	Blocked []string `json:"blocked,omitempty"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// Contents is the content pipeline (internal/content, section 12).
type Contents interface {
	// Plan resolves the two lists and compares them to the lock. Network
	// READS only -- nothing is downloaded and nothing is written.
	Plan(ctx context.Context) (ContentPlan, error)
	// Materialize fetches and links the acquisition namespace: blobs into the
	// store, hard links into the instance tree.
	Materialize(ctx context.Context) error
	// Activate writes the game's own config -- the list the server reads at
	// boot.
	Activate(ctx context.Context) error
	// Resolve re-derives the activation namespace from disk and rewrites the
	// lock.
	Resolve(ctx context.Context) (Resolution, error)
}

var contentRunner = &Runner{
	Kind:    lifecycle.JobContentSync,
	Scope:   ScopeInstance,
	Policy:  PolicyStartAnyway,
	Stops:   true,
	Summary: "plan, materialize, activate -- stopping only to write config",
	Steps: func(env *Env) ([]Step, error) {
		c := env.Contents
		if c == nil {
			return nil, fmt.Errorf("%w: a content_sync job needs a content pipeline "+
				"(Env.Contents)", ErrEnv)
		}
		var plan ContentPlan

		ifRestart := func(*Run) (bool, string) {
			return plan.NeedsRestart, "activating this change does not need the server down"
		}

		return []Step{
			{
				Name: "content.plan", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error {
					p, err := c.Plan(ctx)
					if err != nil {
						return err
					}
					plan = p
					jr.Set("added", p.Added)
					jr.Set("removed", p.Removed)
					return nil
				},
			},
			{
				// The gate, and like the update's decision it is on the near
				// side of every expensive and every dangerous thing.
				Name: "content.decide", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error {
					if n := len(plan.Unaccounted); n > 0 {
						// Fail closed, and say so in rebuild.sh's own terms.
						// The server is untouched at this point and stays
						// that way.
						jr.Set("unaccounted", plan.Unaccounted)
						return fmt.Errorf("refusing to sync: %d item(s) the parse could not "+
							"account for (%s). A source whose markup changed silently drops "+
							"content, and a server running with mods missing is worse than one "+
							"that did not sync; nothing was stopped and nothing was written",
							n, strings.Join(plan.Unaccounted, " "))
					}
					if n := len(plan.Blocked); n > 0 {
						// AWAITING_MANUAL_FILES. Not a failure and not a
						// reason to stop anything: the operator fetches the
						// files by hand and runs the sync again (section
						// 12.8).
						jr.Set("blocked", plan.Blocked)
						jr.Note("%d item(s) cannot be downloaded by gvm and need a human: %s. "+
							"The server was not stopped; see %s",
							n, strings.Join(plan.Blocked, " "),
							instance.PathsFor(env.Instance).BlockedMods)
						return fmt.Errorf("content is awaiting manual files: %w", ErrSkipJob)
					}
					if !plan.Changed {
						return fmt.Errorf("the content lists already match the lock: %w",
							ErrSkipJob)
					}
					if plan.Note != "" {
						jr.Note("%s", plan.Note)
					}
					return nil
				},
			},
			{
				// Materialize runs with the server UP, deliberately: fetching
				// and linking happen in the blob store and under the
				// instance's content roots, and nothing the game has open
				// changes until the activation list does. This is what keeps
				// "stops only to write config" true.
				Name: "content.materialize", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error { return c.Materialize(ctx) },
			},
			withWhen(stopStep("content.stop", env), ifRestart),
			{
				Name: "content.activate", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error { return c.Activate(ctx) },
			},
			withWhen(startStep("content.start", env), ifRestart),
			{
				// Non-fatal, like the update's phase 9 and for the same
				// reason: sync-mods.sh's own `|| warn "Mod ID resolution
				// reported problems."` leaves a working server.
				Name: "content.resolve",
				Do: func(ctx context.Context, jr *Run) error {
					r, err := c.Resolve(ctx)
					if err != nil {
						return err
					}
					jr.Set("activation_before", r.Before)
					jr.Set("activation_after", r.After)
					if len(r.Ambiguous) > 0 {
						jr.Set("activation_ambiguous", r.Ambiguous)
						jr.Degraded = true
						jr.Note("could not determine the activation name for %s",
							strings.Join(r.Ambiguous, " "))
					}
					return nil
				},
			},
		}, nil
	},
}
