package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/S95F/gameVPSmanager/internal/avail"
)

// banner states rule 1 exactly, and no more than rule 1.
//
// The earlier wording ("every command below works over SSH as root with
// nothing else running") was the aspiration, not this build: install, update,
// rebuild and the firewall are not here yet, and a banner that claimed them
// would be the first dishonest line an operator read. The claim below is the
// one that is true today and stays true — the instance and operations verbs
// that run a world do not need gvmd, the dashboard or Postgres — and the "Not
// available in this build" group at the foot of the listing says what is
// missing, by name.
const banner = `gvm — game server VPS manager

Usage:
  gvm <command> [flags]

Instance and operations commands work over SSH as root with gvmd, the
dashboard and Postgres all down. What is not in this build is listed at the
end, by name.
`

func usage(w io.Writer) {
	fmt.Fprint(w, banner)

	// NotInBuild commands are pulled out of their group and listed last, under
	// one heading, so the groups above read as "what this box can do" and the
	// gap is one block an operator can scan rather than a surprise discovered
	// per command.
	var unbuilt []Command
	for _, g := range groupOrder {
		var in []Command
		for _, c := range commands {
			if c.Group != g.key || c.Hidden {
				continue
			}
			if c.Avail.State == avail.NotInBuild {
				unbuilt = append(unbuilt, c)
				continue
			}
			in = append(in, c)
		}
		if len(in) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", g.title)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, c := range in {
			fmt.Fprintf(tw, "  %s\t%s\n", c.Name, summaryWithAvail(c))
		}
		_ = tw.Flush()
	}

	if len(unbuilt) > 0 {
		fmt.Fprint(w, "\nNot available in this build:\n")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, c := range unbuilt {
			fmt.Fprintf(tw, "  %s\t%s\n", c.Name, c.Avail.Why)
		}
		_ = tw.Flush()
	}

	fmt.Fprint(w, "\nRun \"gvm help <command>\" for detail.\n")
}

// summaryWithAvail appends a Partial command's reason to its summary, so the
// one listing that an operator reads first already says a command is half
// here — `restore` and `adopt` work enough to be in their group, and the note
// says which half refuses.
func summaryWithAvail(c Command) string {
	if c.Avail.State == avail.Partial && c.Avail.Why != "" {
		return c.Summary + " (" + c.Avail.Why + ")"
	}
	return c.Summary
}
