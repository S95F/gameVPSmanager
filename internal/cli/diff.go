// The plan renderer -- section 17.6's shape, for `gvm apply --diff`.
//
// It is a separate file from the verb because the SHAPE is the contract. An
// operator reads a plan the same way whether it came from apply, from a future
// `gvm firewall --diff` or from the dashboard's plan endpoint: a sigil, the
// kind, the object the host will name it by, the reason indented under it, and
// the masked diff under that. Everything that renders a plan renders it here.
//
// The two "!" lines at the bottom exist because a plan that stays quiet about
// what it could NOT check reads as a plan that checked everything.

package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/reconcile"
)

// printPlan renders the plan in section 17.6's shape.
//
// Sigil, kind, object; the WHY indented under it; the masked diff under that;
// and a closing line that counts what would change and states plainly that
// nothing has. The last part is not decoration -- an operator who ran --diff
// has to be able to say afterwards that they touched nothing.
func printPlan(w io.Writer, pl *planned, verbose bool) {
	in, m := pl.Instance, pl.Manifest
	fmt.Fprintf(w, "\n  instance %s   (%s rev %d, %s, pinned)\n\n",
		in.Metadata.Name, m.Metadata.ID, m.Metadata.Revision, m.Origin)

	var add, upd, del, warn, same int
	for _, s := range pl.Plan.Describe() {
		switch s.Change {
		case reconcile.Add:
			add++
		case reconcile.Update:
			upd++
		case reconcile.Delete:
			del++
		case reconcile.Warn:
			warn++
		default:
			same++
		}
	}

	// Default to the rows that change: a plan is read to answer "what will this
	// do", and a screen of "already correct" lines buries the two that matter.
	// --verbose restores the full table for the operator who wants to see
	// everything the reconciler considered.
	shown := pl.Plan.Changes()
	if verbose {
		shown = pl.Plan.Describe()
	}
	if len(shown) == 0 {
		fmt.Fprint(w, "  no changes: the host already matches the spec.\n")
	} else {
		fmt.Fprint(w, renderSteps(shown))
	}

	fmt.Fprintf(w, "\n  %d to add, %d to change, %d to remove, %d already correct",
		add, upd, del, same)
	if warn > 0 {
		fmt.Fprintf(w, ", %d needing attention", warn)
	}
	fmt.Fprintln(w, ".")
	if !verbose && same > 0 {
		fmt.Fprintf(w, "  %d already-correct row(s) hidden; --verbose shows them.\n", same)
	}

	if pl.Unprobed {
		fmt.Fprintf(w, "  ! the write-probe was NOT run: --diff never launches a transient unit,\n"+
			"    so the sandbox in the drop-in above is unverified until `gvm apply %s`.\n",
			in.Metadata.Name)
	}
	if pl.UncheckedSchedules {
		fmt.Fprint(w, "  ! the job schedules were NOT checked against systemd on this host;\n"+
			"    they are only known to be shaped like an OnCalendar expression.\n")
	}
	printUnrunnableJobs(w, pl)
	if k := strings.TrimSpace(pl.Instance.Spec.Backup.Remote.Kind); k != "" && k != "none" {
		fmt.Fprint(w, "  ! an off-box backup target is configured, but this build has no SFTP "+
			"transport; nightly backups are taken and verified locally, and the off-box copy is "+
			"not made (recorded as a degraded step).\n")
	}
	fmt.Fprintln(w, "\n  nothing has been applied.")
}

// renderSteps prints a list of plan steps in section 17.6's shape — the sigil,
// kind and object on one line, the masked diff indented under it. It is the
// same rendering Plan.String() does, over whatever subset the caller chose
// (the changed rows by default, the whole plan under --verbose).
func renderSteps(steps []reconcile.Step) string {
	var b strings.Builder
	for _, s := range steps {
		b.WriteString("  " + s.String() + "\n")
		if s.Diff != "" {
			for _, line := range strings.Split(strings.TrimRight(s.Diff, "\n"), "\n") {
				b.WriteString("      " + line + "\n")
			}
		}
	}
	return b.String()
}

// printUnrunnableJobs warns that a timer apply installs is armed against a kind
// this build cannot run, so an operator reading the plan knows the nightly is a
// no-op before it fires rather than after.
func printUnrunnableJobs(w io.Writer, pl *planned) {
	for _, j := range pl.UnrunnableJobs {
		fmt.Fprintf(w, "  ! timer %s_%s (%s) will be installed, but this build cannot run %s jobs: %s\n",
			pl.Instance.Metadata.Name, j.Job, j.Kind, j.Kind, j.Why)
	}
}

func printApplyReport(w io.Writer, pl *planned, rep *reconcile.Report) {
	fmt.Fprintf(w, "\n  %d changed, %d already correct", rep.Changed, rep.Unchanged)
	if n := len(rep.Verified); n > 0 {
		fmt.Fprintf(w, ", %d file(s) read back and matched", n)
	} else {
		fmt.Fprint(w, "; no file was read back, so this apply cannot say the bytes on disk "+
			"are the ones it wrote")
	}
	fmt.Fprintln(w, ".")
	if pl.UncheckedSchedules {
		fmt.Fprint(w, "  ! the job schedules were NOT checked against systemd on this host.\n")
	}
	printUnrunnableJobs(w, pl)
	for _, r := range rep.Results {
		if r.Err != nil {
			fmt.Fprintf(w, "  ! %s %s: %v\n", r.Step.Kind, r.Step.Object, r.Err)
		}
	}
}
