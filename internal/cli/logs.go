// `gvm logs` — the console log and the shutdown record, which are the two
// files an operator actually wants at 2am.
//
// The console does NOT go to journald: journald's default rate
// limit would put holes in exactly the post-mortem you need, so the runner
// writes /var/log/gvm/<name>/console.log itself and rotates it. This command is
// therefore a file reader, not a journal client, and `journalctl -u gvm@<name>`
// remains the right tool for the runner's own diagnostics -- which is why the
// output says so rather than pretending this is everything.
//
// tailLines is a pure function over an io.Reader for the usual reason: the
// interesting cases are a file shorter than the request, a file with no
// trailing newline and a file that is empty, and none of them need a Linux box
// to get wrong.

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/runner"
)

// DefaultLogLines is what `gvm logs` prints when no count is given. It matches
// the bash suite's `tail -n 30` on early exit, which is the number that has
// been enough in practice for years.
const DefaultLogLines = 30

type logsOptions struct {
	Name     string
	Lines    int
	Shutdown bool
	Remote   string
}

func parseLogsArgs(env *Env, args []string) (logsOptions, error) {
	var opt logsOptions
	fs := flag.NewFlagSet("gvm logs", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm logs <instance> [-n LINES] [--shutdown]\n\n"+
			"Prints the tail of the instance's console log.\n"+
			"--shutdown prints the recorded shutdown ladders instead.\n"+
			"For the runner's own diagnostics: journalctl -u gvm@<instance>\n\n")
		fs.PrintDefaults()
	}
	fs.IntVar(&opt.Lines, "n", DefaultLogLines, "how many trailing lines to print")
	fs.BoolVar(&opt.Shutdown, "shutdown", false, "print shutdown.jsonl instead of the console log")
	fs.StringVar(&opt.Remote, "remote", "", "dashboard URL to run this against instead of the local host")

	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return logsOptions{}, flagErr(err)
	}
	name, err := oneInstance(pos)
	if err != nil {
		fs.Usage()
		return logsOptions{}, err
	}
	opt.Name = name
	if opt.Lines < 0 {
		return logsOptions{}, fmt.Errorf("-n %d is negative", opt.Lines)
	}
	return opt, nil
}

func runLogs(ctx context.Context, env *Env, args []string) error {
	opt, err := parseLogsArgs(env, args)
	if err != nil {
		return err
	}
	t, err := Resolve(env, "logs", opt.Remote)
	if err != nil {
		return err
	}
	if err := t.RequireLocal(); err != nil {
		return err
	}
	h, err := env.host(ctx)
	if err != nil {
		return err
	}
	if opt.Shutdown {
		return printShutdowns(env.Stdout, h, opt)
	}
	return printConsoleTail(env.Stdout, h, opt)
}

func printConsoleTail(w io.Writer, h *HostEnv, opt logsOptions) error {
	p := instance.PathsFor(opt.Name).ConsoleLog
	raw, err := h.files().ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no console log at %s. It is created by the runner, so an "+
				"instance that has never started has none", p)
		}
		return err
	}
	lines := tailLines(strings.NewReader(string(raw)), opt.Lines)
	fmt.Fprintf(w, "\n%s  (last %d of %d lines)\n\n", p, len(lines), countLines(string(raw)))
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	fmt.Fprintf(w, "\n  the runner's own diagnostics are in the journal: "+
		"journalctl -u %s\n", instance.UnitName(opt.Name))
	return nil
}

// printShutdowns renders shutdown.jsonl, newest last.
//
// Each line is one runner.Report and the fields that matter are the rung and
// the degraded flag: those two are the difference between a world that saved
// and one that was SIGKILLed, and the whole reason this file is append-only.
func printShutdowns(w io.Writer, h *HostEnv, opt logsOptions) error {
	p := instance.PathsFor(opt.Name).ShutdownLog
	raw, err := h.files().ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no shutdown record at %s. It is appended when an instance "+
				"stops, so one that has never stopped has none", p)
		}
		return err
	}
	reps, bad := parseShutdownLog(strings.NewReader(string(raw)))
	fmt.Fprintf(w, "\n%s  (%d record(s))\n\n", p, len(reps))

	from := 0
	if opt.Lines > 0 && len(reps) > opt.Lines {
		from = len(reps) - opt.Lines
	}
	for _, r := range reps[from:] {
		degraded := ""
		if r.Degraded {
			degraded = "  DEGRADED (a SIGKILL was needed)"
		}
		fmt.Fprintf(w, "  %s  rung=%s  %s  exit=%d%s\n",
			r.FinishedAt.UTC().Format("2006-01-02 15:04:05Z"),
			r.FinalRung, r.Duration, r.ExitCode, degraded)
		if r.Reason != "" {
			fmt.Fprintf(w, "      %s (%s)\n", r.Reason, r.Actor)
		}
	}
	for _, b := range bad {
		fmt.Fprintf(w, "  ! line %d did not parse: %v\n", b.line, b.err)
	}
	return nil
}

type badLine struct {
	line int
	err  error
}

// parseShutdownLog decodes an NDJSON shutdown log.
//
// A line that does not parse is REPORTED and the rest are still shown: this is
// an append-only forensic record, and a single corrupt line -- a torn write
// from a power loss, which is precisely the event you are reading the file
// about -- must not hide every shutdown that came before it.
func parseShutdownLog(r io.Reader) ([]runner.Report, []badLine) {
	var out []runner.Report
	var bad []badLine
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rep runner.Report
		if err := json.Unmarshal([]byte(line), &rep); err != nil {
			bad = append(bad, badLine{line: n, err: err})
			continue
		}
		out = append(out, rep)
	}
	if err := sc.Err(); err != nil {
		bad = append(bad, badLine{line: 0, err: err})
	}
	return out, bad
}

// tailLines returns the last n lines, oldest first.
//
// n <= 0 means all of them. A trailing newline does not produce a final empty
// line, because "the file ends with a newline" is not something an operator
// wants a blank row for.
func tailLines(r io.Reader, n int) []string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var all []string
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	if n <= 0 || len(all) <= n {
		return all
	}
	return all[len(all)-n:]
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
