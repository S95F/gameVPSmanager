package update

import (
	"errors"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/provider"
)

// The two axes, derived. Pure, and deliberately the only interpretation of a
// provider plan or a content plan this package performs.
//
// What is NOT here is the decision. `jobs.Decide` owns "does this stop the
// server", it is pure for exactly that reason, and the job layer calls it
// between phase 2 and phase 3 with these two Diffs as its input. This file
// produces the input and nothing else: a second place that could conclude
// "stop" would be a second policy, and the one being protected -- D-43, the
// server is not touched on a night when nothing changed -- is the kind that
// is only ever violated by a well-meant special case somewhere downstream.
//
// So the asymmetry is on purpose: internal/update computes diffs and executes
// phases, and it cannot stop a server because it holds nothing that could.
// decide_test.go asserts that structurally, over Config's own members.

// ErrUnaccounted is the fail-closed refusal, and it happens on the near side
// of the decision while the server is still running.
//
// rebuild.sh counted the item ids it parsed out of a collection page against a
// count taken from the raw HTML and refused to rewrite mods.txt when they
// disagreed, because a changed Steam markup silently drops mods and a server
// that boots normally with four of them missing is worse than one that did not
// sync at all. internal/content carries that count as Plan.Unaccounted; this is
// where the nightly refuses on it.
var ErrUnaccounted = errors.New("update: the content parse could not account for every item")

// BuildDiff is phase 1's answer, read off the provider's plan.
//
// Changed means "this axis wants work", which includes the adopted case: there
// IS a newer build, and the question of whether gvm is allowed to install it
// belongs to jobs.Decide, not here. Getting that wrong in the other direction
// is the failure section 13.5 names -- an adopted instance whose nightly goes
// quiet about an available update on the day after cutover.
func BuildDiff(pl *provider.Plan) jobs.Diff {
	if pl == nil {
		return jobs.Diff{}
	}
	d := jobs.Diff{
		Changed: pl.Writes() || pl.UpdateAvailable,
		From:    pl.Current.ID,
		To:      pl.Target.ID,
	}
	if !d.Changed {
		// The no-op night. The reason is kept because "nothing to do" with no
		// pair of ids beside it is indistinguishable from a check that broke,
		// and a check that silently reports "nothing to do" for a month is
		// the most expensive outcome in this subsystem.
		d.Note = pl.Reason
	}
	return d
}

// ContentDiff is phase 2's answer, with the two fail-closed gates applied.
//
// Unaccounted is an error: nothing is stopped, nothing is written, and the
// message names the items. Blocked is NOT an error and NOT a content diff --
// section 13.5's CurseForge paragraph is explicit that a modpack whose author
// revoked third-party download rights must not cost downtime, so the axis
// reports no work and carries the sentence instead. A build diff on the same
// night still proceeds on its own merits: the binary update is real work and
// has nothing to do with a file a human has to fetch by hand.
func ContentDiff(p jobs.ContentPlan) (jobs.Diff, error) {
	if n := len(p.Unaccounted); n > 0 {
		return jobs.Diff{}, fmt.Errorf("%w: %d item(s) (%s). A source whose markup changed "+
			"silently drops content, and a server running with mods missing is worse than one "+
			"that did not sync; nothing was stopped and nothing was written",
			ErrUnaccounted, n, strings.Join(p.Unaccounted, " "))
	}
	if n := len(p.Blocked); n > 0 {
		return jobs.Diff{
			Note: fmt.Sprintf("%d item(s) cannot be downloaded by gvm and need a human: %s. "+
				"The content axis reports no work, so the server is not stopped for them",
				n, strings.Join(p.Blocked, " ")),
		}, nil
	}
	if !p.Changed {
		return jobs.Diff{Note: p.Note}, nil
	}
	d := jobs.Diff{Changed: true, Note: p.Note}
	d.Items = append(d.Items, p.Added...)
	d.Items = append(d.Items, p.Removed...)
	return d, nil
}
