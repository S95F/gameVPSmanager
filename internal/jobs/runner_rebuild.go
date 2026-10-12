package jobs

import (
	"context"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The `rebuild` kind: section 13.6, and the one job in the table whose purpose
// is to destroy something.
//
// Its step table is two steps and not ten, which is a deliberate departure
// from every other runner here, and the reason is the thing section 13.6 is
// about. The ordering -- content resolved, downloaded and VERIFIED before
// anything is deleted; the archive taken after that verification and before
// the deletion; every early exit leaving the world untouched and SAYING SO --
// is the safety property, and it lives in exactly one place
// (internal/rebuild.Run). Expressing it a second time as rows in this table
// would create two orderings that have to agree forever, and the failure of
// that agreement is a deleted world.
//
// So this table declares what the job layer is responsible for and nothing
// more:
//
//	rebuild.plan   reads only. The fail-closed count check (rebuild.sh lines
//	               77-80) refuses here, before the guard has anything to
//	               restore and before anything has stopped.
//	rebuild.run    Stops: true. Steps 2-10, behind the running guard.
//
// Declaring Stops on the second step is what arms the engine's assertion: a
// table that takes a server down with no guard in front of it panics rather
// than runs. And `start_anyway after archive` -- section 13.3's fifth row --
// is the policy, because once the world is gone the only acceptable end state
// is a server that is up generating the new one.

// Rebuilt is what the rebuild reports back, as this layer needs it.
//
// WorldTouched and Untouched are carried into the job summary rather than left
// in an error string, because they are the two facts an operator reading a
// failed rebuild at 2am needs first and the only two that cannot be inferred
// from anything else in the row.
type Rebuilt struct {
	// WorldTouched is false on every path that returned before the destroy.
	WorldTouched bool `json:"world_touched"`
	// Untouched is the sentence, present whenever WorldTouched is false.
	Untouched string `json:"untouched,omitempty"`
	// Archive is the verified preserve archive, once step 7 took one.
	Archive *archive.Ref `json:"archive,omitzero"`
	// Missing are step 5's content ids, by name.
	Missing []string `json:"missing,omitempty"`
	// Restore is the command that puts the old world back.
	Restore string `json:"restore,omitempty"`
	// Running is whether the server is up at exit.
	Running bool `json:"running"`
	// Steps is the narration, in order.
	Steps []string `json:"steps,omitempty"`
}

// Rebuilds is internal/rebuild (section 13.6).
type Rebuilds interface {
	// Plan is step 1: fetch the source list, compare it to the acquisition
	// list, fail closed when the parsed count disagrees with the raw count.
	// Network READS only, and the acquisition list is not rewritten.
	Plan(ctx context.Context) (ContentPlan, error)
	// Run is steps 2 to 10. It STOPS the server, and the ordering inside it
	// is the safety property -- see this file's own comment for why it is one
	// call.
	Run(ctx context.Context) (Rebuilt, error)
}

var rebuildRunner = &Runner{
	Kind:  lifecycle.JobRebuild,
	Scope: ScopeInstance,
	// Section 13.3's fifth row: `start_anyway` after archive. A rebuild that
	// failed after the destroy has a world that no longer exists, and leaving
	// the server down on top of that would turn a recoverable rebuild into an
	// outage with a restore in front of it.
	Policy:  PolicyStartAnyway,
	Stops:   true,
	Summary: "resolve, download, verify, archive, destroy, start (section 13.6)",
	Steps: func(env *Env) ([]Step, error) {
		r := env.Rebuilds
		if r == nil {
			return nil, fmt.Errorf("%w: a rebuild job needs a rebuild engine (Env.Rebuilds)",
				ErrEnv)
		}
		return []Step{
			{
				// Step 1. Reads only, and the two fail-closed gates are the
				// same two content_sync applies -- for the stronger reason
				// here: a rebuild generates a NEW world from the list, so an
				// incomplete list is not a degraded sync, it is a world
				// permanently missing mods.
				Name: "rebuild.plan", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error {
					p, err := r.Plan(ctx)
					if err != nil {
						return err
					}
					jr.Set("added", p.Added)
					jr.Set("removed", p.Removed)
					if n := len(p.Unaccounted); n > 0 {
						jr.Set("unaccounted", p.Unaccounted)
						return fmt.Errorf("refusing to rebuild: %d item(s) the parse could not "+
							"account for (%s). The acquisition list was NOT rewritten, nothing "+
							"was stopped, and the world is untouched",
							n, strings.Join(p.Unaccounted, " "))
					}
					if n := len(p.Blocked); n > 0 {
						jr.Set("blocked", p.Blocked)
						return fmt.Errorf("refusing to rebuild: %d item(s) cannot be downloaded "+
							"by gvm and need a human (%s). A rebuild generates a new world from "+
							"the full list, so nothing was stopped and the world is untouched",
							n, strings.Join(p.Blocked, " "))
					}
					if p.Note != "" {
						jr.Note("%s", p.Note)
					}
					return nil
				},
			},
			{
				// Steps 2-10, behind the guard.
				Name: "rebuild.run", Fatal: true, Stops: true,
				Do: func(ctx context.Context, jr *Run) error {
					res, err := r.Run(ctx)
					recordRebuilt(jr, res)
					return err
				},
			},
		}, nil
	},
}

// recordRebuilt puts the outcome in the summary on BOTH paths.
//
// On the failure path especially: an error that said "the archive did not
// verify" without `world_touched: false` beside it in the row would leave the
// operator unable to answer the only question they have.
func recordRebuilt(jr *Run, res Rebuilt) {
	jr.Set("world_touched", res.WorldTouched)
	if res.Untouched != "" {
		jr.Set("world_untouched_because", res.Untouched)
		jr.Note("%s", res.Untouched)
	}
	if res.Archive != nil {
		jr.Set("archive", res.Archive.Path)
		jr.Set("archive_bytes", res.Archive.Bytes)
	}
	if len(res.Missing) > 0 {
		jr.Set("content_missing", res.Missing)
	}
	if res.Restore != "" {
		jr.Set("restore", res.Restore)
		jr.Note("old world: %s", res.Restore)
	}
	for _, s := range res.Steps {
		jr.Note("%s", s)
	}
}
