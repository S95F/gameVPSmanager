package cli

import (
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
)

// flagUsage renders a flag set the way an operator reads it, and replaces
// flag.PrintDefaults for the commands that care.
//
// It fixes two things PrintDefaults does. First, PrintDefaults extracts a value
// placeholder from backticks in the usage string, so a usage line written
// without backticks prints a bare `--game` with no hint that it takes a value;
// this prints `--game VALUE` for every non-boolean flag, from the flag's type,
// not from punctuation in its help. Second, it hides the flags named in
// `hidden` — the internal ones a human should not be nudged toward (a unit's
// `--trigger`, a deprecated spelling) — which PrintDefaults cannot do at all.
func flagUsage(w io.Writer, fs *flag.FlagSet, hidden ...string) {
	hide := map[string]bool{}
	for _, h := range hidden {
		hide[h] = true
	}
	type row struct{ name, usage string }
	var rows []row
	fs.VisitAll(func(f *flag.Flag) {
		if hide[f.Name] {
			return
		}
		name := "--" + f.Name
		if !isBoolFlag(f) {
			name += " VALUE"
		}
		usage := f.Usage
		// A non-empty, non-false default is worth showing; "false" and "" are
		// the no-op defaults an operator does not need told.
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" && f.DefValue != "0s" {
			usage = fmt.Sprintf("%s (default %s)", usage, f.DefValue)
		}
		rows = append(rows, row{name, usage})
	})
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w, "Flags:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(tw, "  %s\t%s\n", r.name, r.usage)
	}
	_ = tw.Flush()
}

// isBoolFlag reports whether a flag is a boolean (so `--flag` needs no VALUE).
// The stdlib marks these with an IsBoolFlag() method on the Value, which is the
// same check flag.Parse uses to decide whether `--flag` consumes the next arg.
func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

// printExamples writes an Examples block for a command's `--help`. Each line is
// a whole command an operator can paste.
func printExamples(w io.Writer, examples ...string) {
	if len(examples) == 0 {
		return
	}
	fmt.Fprintln(w, "\nExamples:")
	for _, ex := range examples {
		fmt.Fprintf(w, "  %s\n", ex)
	}
}
