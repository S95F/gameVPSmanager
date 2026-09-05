package cli

import (
	"fmt"
	"io"
	"text/tabwriter"
)

const banner = `gvm — game server VPS manager

Usage:
  gvm <command> [flags]

The rule this tool is built around: if gvmd, the dashboard and the database
are all dead, every game server still runs, restarts, updates and backs up.
So every command below works over SSH as root with nothing else running.
`

func usage(w io.Writer) {
	fmt.Fprint(w, banner)

	for _, g := range groupOrder {
		var in []Command
		for _, c := range commands {
			if c.Group == g.key && !c.Hidden {
				in = append(in, c)
			}
		}
		if len(in) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", g.title)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, c := range in {
			fmt.Fprintf(tw, "  %s\t%s\n", c.Name, c.Summary)
		}
		_ = tw.Flush()
	}
	fmt.Fprint(w, "\nRun \"gvm help <command>\" for detail.\n")
}
