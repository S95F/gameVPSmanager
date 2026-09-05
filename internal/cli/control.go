// `gvm control` — the command path to a running server, from the terminal.
//
// It is the same socket `gvm console` attaches to, asked one question at a time
// instead of followed. `caps` is the interesting one: it prints what this
// instance can be asked to do RIGHT NOW and why not, which is the answer to
// almost every "the button is greyed out" support thread, and it is computed
// from the same spec.Capabilities the dashboard renders — not a second table
// that will drift from it.
//
// Everything here goes through the console socket rather than dialling RCON
// itself. Two reasons, and the second is the one that matters: the runner holds
// the credential and the gate (never dial before the game is listening —
// section 11.9 rule 1), and a second RCON client in the CLI would mean two
// processes with no shared serialisation running mutations against one server
// that has no multiplexing.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/socket"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// requireLinuxHost is the guard every host-touching command runs first.
//
// Without it a Windows developer typing `gvm console pz-main` gets whatever
// net.Dial does with a path that cannot exist on this OS, which is a different
// error on every build. platform.ErrNotSupported is the project's one answer to
// "this only runs on the deployment target", and gvm doctor and the test suite
// both key on it.
func requireLinuxHost(what string) error {
	if runtime.GOOS != "linux" {
		return platform.Unsupported(what)
	}
	return nil
}

// parseInterspersed parses flags wherever they appear, and returns the
// positional arguments in order.
//
// stdlib flag stops at the first non-flag argument, so `gvm console pz-main
// --no-follow` would parse as two instance names and the flag would be
// silently ignored. The instance is the subject of the sentence and an operator
// types it first; a CLI that quietly did the opposite of what was asked because
// of argument order is worse than one that is verbose about it.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

var controlSubs = []struct {
	name    string
	args    string
	summary string
	run     func(context.Context, *Env, []string) error
}{
	{"caps", "<instance>", "what this instance can be asked to do right now, and why not", runControlCaps},
	{"status", "<instance>", "the control channel's health, from status.json", runControlStatus},
	{"verb", "<instance> <verb> [Key=value]...", "run one manifest verb through the runner", runControlVerb},
	{"probe", "<instance>", "test the RCON dialect against the live server and record the finding", runControlProbe},
}

func runControl(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		controlUsage(env.Stderr)
		return errors.New("name a subcommand")
	}
	switch args[0] {
	case "-h", "--help", "help":
		controlUsage(env.Stdout)
		return nil
	}
	for _, s := range controlSubs {
		if s.name == args[0] {
			return s.run(ctx, env, args[1:])
		}
	}
	controlUsage(env.Stderr)
	return fmt.Errorf("unknown subcommand %q", args[0])
}

func controlUsage(w io.Writer) {
	fmt.Fprint(w, "gvm control — inspect and drive an instance's control channel\n\nUsage:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range controlSubs {
		fmt.Fprintf(tw, "  gvm control %s %s\t%s\n", s.name, s.args, s.summary)
	}
	_ = tw.Flush()
}

// controlTarget parses the shared "one instance name" prefix.
func controlTarget(env *Env, sub, argspec string, args []string, min int) (string, []string, error) {
	fs := flag.NewFlagSet("gvm control "+sub, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() { fmt.Fprintf(env.Stderr, "Usage: gvm control %s %s\n", sub, argspec) }
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return "", nil, flagErr(err)
	}
	if len(pos) < 1+min {
		fs.Usage()
		return "", nil, errors.New("name an instance")
	}
	name := pos[0]
	if err := instance.ValidateName(name); err != nil {
		return "", nil, err
	}
	return name, pos[1:], nil
}

// dialInstance opens one short-lived connection to an instance's console socket
// and says hello.
func dialInstance(ctx context.Context, name string) (*socket.Client, error) {
	if err := requireLinuxHost("gvm control"); err != nil {
		return nil, err
	}
	path := instance.PathsFor(name).ConsoleSock
	c, err := socket.Dial(ctx, path)
	if err != nil {
		return nil, dialError(name, path, err)
	}
	if _, err := c.Hello(clientID()); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func runControlCaps(ctx context.Context, env *Env, args []string) error {
	name, _, err := controlTarget(env, "caps", "<instance>", args, 0)
	if err != nil {
		return err
	}
	c, err := dialInstance(ctx, name)
	if err != nil {
		return err
	}
	defer c.Close()

	raw, err := c.Caps(nil)
	if err != nil {
		return err
	}
	var caps []spec.CapState
	if err := json.Unmarshal(raw, &caps); err != nil {
		return fmt.Errorf("the runner's capability document did not parse: %w", err)
	}
	printCaps(env.Stdout, caps)
	return nil
}

// printCaps renders the three-state table of section 8.5.
//
// The three states are printed as three different words rather than as two
// booleans, because "supported: true, available: false" is a shape an operator
// has to decode and "not yet" plus the manifest's own sentence is one they can
// act on.
func printCaps(w io.Writer, caps []spec.CapState) {
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range caps {
		state := "no"
		switch {
		case c.Available:
			state = "yes"
		case c.Supported:
			state = "not yet"
		}
		via := c.Channel
		if via == "" {
			via = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", c.Cap, state, via, c.Reason)
	}
	_ = tw.Flush()
	fmt.Fprint(w, "\n  \"not yet\" is a capability this game HAS and cannot use at this moment;\n"+
		"  \"no\" is one it does not have at all.\n")
}

func runControlStatus(ctx context.Context, env *Env, args []string) error {
	name, _, err := controlTarget(env, "status", "<instance>", args, 0)
	if err != nil {
		return err
	}
	c, err := dialInstance(ctx, name)
	if err != nil {
		return err
	}
	defer c.Close()

	raw, err := c.Status(nil)
	if err != nil {
		return err
	}
	// Only the control block is decoded. status.json is the runner's document
	// and will grow; decoding the whole of it here would make `gvm control
	// status` fail on a field this command does not care about.
	var doc struct {
		Instance string         `json:"instance"`
		State    string         `json:"state"`
		Control  control.Status `json:"control"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("the runner's status document did not parse: %w", err)
	}
	printControlStatus(env.Stdout, name, doc.State, doc.Control)
	return nil
}

func printControlStatus(w io.Writer, name, state string, st control.Status) {
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  instance\t%s (%s)\n", name, state)
	fmt.Fprintf(tw, "  kind\t%s\n", st.Kind)
	if st.Dialect != "" {
		fmt.Fprintf(tw, "  dialect\t%s\n", st.Dialect)
	}
	ready := "no"
	if st.Ready {
		ready = "yes"
	}
	fmt.Fprintf(tw, "  ready\t%s\n", ready)
	if !st.LastOK.IsZero() {
		fmt.Fprintf(tw, "  last ok\t%s\n", st.LastOK.Format("2006-01-02 15:04:05Z07:00"))
	}
	if st.LastError != "" {
		fmt.Fprintf(tw, "  last error\t%s\n", st.LastError)
	}
	if st.ConsecutiveFailures > 0 {
		fmt.Fprintf(tw, "  consecutive failures\t%d\n", st.ConsecutiveFailures)
	}
	// Why it is not ready lives in the capability table, not in this block:
	// `not_ready_reason` is the disabled button's tooltip and belongs to the
	// verb it disables, so `gvm control caps` prints it against each one.
	if !st.Ready {
		fmt.Fprintf(tw, "  \tsee `gvm control caps %s` for what that disables\n", name)
	}
	_ = tw.Flush()
	fmt.Fprintln(w)
}

func runControlVerb(ctx context.Context, env *Env, args []string) error {
	name, rest, err := controlTarget(env, "verb", "<instance> <verb> [Key=value]...", args, 1)
	if err != nil {
		return err
	}
	verb := rest[0]
	vargs, err := parseVerbArgs(rest[1:])
	if err != nil {
		return err
	}
	c, err := dialInstance(ctx, name)
	if err != nil {
		return err
	}
	defer c.Close()

	res, err := c.Verb("", verb, vargs, nil)
	if err != nil {
		if socket.Denied(err) {
			return fmt.Errorf("%w — verbs are uid 0 only on this socket (D-09)", err)
		}
		return err
	}
	if out := strings.TrimRight(res.Output, "\n"); out != "" {
		fmt.Fprintln(env.Stdout, out)
	}
	fmt.Fprintf(env.Stdout, "-- gvm: %s ran on the %s channel.\n", verb, res.Channel)
	return nil
}

// parseVerbArgs turns `Msg=restart in 5` into the operand map the manifest's
// verb templates read.
//
// The keys are spec.VerbArgs' field names, matched the way control.BindArgs
// matches them, so the CLI and the socket accept exactly the same spellings.
// The value keeps everything after the FIRST '=': a broadcast containing an
// equals sign is ordinary text, and splitting on the last one would silently
// truncate it.
func parseVerbArgs(fields []string) (map[string]string, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return nil, fmt.Errorf("verb operand %q is not Key=value (keys: %s)",
				f, strings.Join(control.ArgNames(), ", "))
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("verb operand %q given twice", k)
		}
		out[k] = v
	}
	// BindArgs owns the closed key set, the case-insensitive match AND
	// section 11.10's character check on every value, so an operand is refused
	// by the same code the runner runs -- and refused here, before any socket
	// is opened, so a refusal names the operand and the character and never
	// reaches a game console at all.
	if _, err := control.BindArgs(out); err != nil {
		return nil, err
	}
	return out, nil
}

// runControlProbe is the D-07 evidence gatherer of section 11.9: connect, run
// the one read-only command, then send an empty SERVERDATA_RESPONSE_VALUE and
// see whether the server mirrors it.
//
// It is not built. That last step needs internal/rcon to expose a raw packet
// write, which it deliberately does not today, and the honest failure is this
// one: a guess recorded as a finding would be worse than no finding, because
// `gvm doctor` re-reads it after every game update and would keep confirming
// the guess.
func runControlProbe(_ context.Context, env *Env, args []string) error {
	if _, _, err := controlTarget(env, "probe", "<instance>", args, 0); err != nil {
		return err
	}
	return fmt.Errorf("%w: the RCON mirror probe needs a raw packet write that internal/rcon "+
		"does not expose; until it exists the manifest's declared dialect stands", ErrNotImplemented)
}
