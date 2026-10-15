// Package cli is gvm's command dispatch.
//
// Dispatch is a plain table over stdlib flag rather than a framework. The
// binary is one static file deployed to a VPS, and a command router is the
// least interesting thing in this project to take a dependency for.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/avail"
	"github.com/S95F/gameVPSmanager/internal/version"
)

// errHelpShown is returned by a parse*Args (through flagErr) when the operator
// asked for help: the FlagSet has already printed usage, so there is nothing
// left to do and nothing went wrong. Main maps it — and a raw flag.ErrHelp from
// the few commands that return one directly — to exit 0, so `gvm <cmd> --help`
// never continues into the command with zero options (U7, the panic the plan's
// "Why now" describes). Requested help is success, not a usage error.
var errHelpShown = errors.New("help shown")

// ErrNotImplemented marks a command that is routed but not yet built.
//
// It is built on avail.New so it matches avail.ErrUnavailable: a routed-but-
// unbuilt command and a half-built seam both exit 3 and both read "not
// available in this build", because to an operator they are the same answer —
// "this gvm cannot do that" — and a script telling them apart was never the
// point. errors.Is(err, ErrNotImplemented) still works for the few call sites
// that want the narrower match.
var ErrNotImplemented = avail.New("not implemented yet")

// ExitError lets a command choose the process exit status.
//
// It exists for `gvm run`, whose exit status is a contract with systemd rather
// than a convention: section 7.8 gives 64 and 78 to failures that must NOT be
// retried and 75 to the one that must, and after a clean stop the status is the
// game's own, because that is what Restart=on-failure reads. Collapsing all of
// those to 1 would turn a manifest pin mismatch into a restart loop.
//
// A nil Err exits quietly with Code: the runner has already said everything it
// has to say, in the journal and in the shutdown report.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// Env is everything a command is allowed to touch, so tests can drive the
// CLI without a real terminal or process environment.
type Env struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Getenv is os.Getenv by default. Commands read GVM_URL and GVM_TOKEN
	// through it so transport selection is testable.
	Getenv func(string) string
	// Geteuid is os.Geteuid by default, and decides local-root vs remote
	// transport.
	Geteuid func() int

	// Host is the set of host-touching seams the instance verbs use. Nil
	// means the production one, which refuses off Linux -- see hostEnv.
	//
	// It is one field rather than a dozen because a test that wants to drive
	// `gvm create` on Windows needs all of them at once and none of them
	// individually: the interesting thing about these commands is the ORDER
	// they touch the host in, and that is only observable when every touch
	// goes to the same recorder.
	Host *HostEnv
}

// DefaultEnv builds an Env bound to the real process.
func DefaultEnv(args []string) *Env {
	return &Env{
		Args:    args,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Getenv:  os.Getenv,
		Geteuid: os.Geteuid,
	}
}

// Command is one dispatchable verb.
type Command struct {
	Name string
	// Summary is one line, shown in the top-level usage listing.
	Summary string
	// Group orders the usage listing. Commands sharing a group are printed
	// together under its heading.
	Group string
	// Hidden keeps a command out of usage without disabling it. Used for
	// internal roles that systemd invokes but a human never types.
	Hidden bool
	// Avail is this command's entry in the build's one availability registry
	// (U1). The zero value is Works, which is most of them; a command only
	// carries a row here when it is Partial or NotInBuild. usage() reads it to
	// move NotInBuild commands under "Not available in this build", and
	// TestAvailabilityMatchesBehaviour reads it to fail when a Works row refuses
	// or a NotInBuild row starts working.
	Avail avail.Availability
	Run   func(ctx context.Context, env *Env, args []string) error
}

// commands is the dispatch table. Order within a group is the order listed.
var commands = []Command{
	{Name: "version", Group: "general", Summary: "print the version banner", Run: runVersion},

	{Name: "gvmd", Group: "roles", Summary: "run the privileged verb gateway (root)", Hidden: true,
		Avail: avail.Unbuilt("the privileged verb gateway is not in this build"), Run: notImplemented},
	{Name: "web", Group: "roles", Summary: "run the dashboard and API", Hidden: true,
		Avail: avail.Unbuilt("the dashboard and API are not in this build"), Run: notImplemented},
	{Name: "run", Group: "roles", Summary: "supervise one instance", Hidden: true, Run: runRun},
	// The three roles below are invoked by the unit and by gvmd, never typed.
	// run-shutdown in particular is the layer-3 recovery path of section 11.8:
	// it exists to be run when the runner is already gone.
	{Name: "run-prepare", Group: "roles", Summary: "chown the runtime directory (root, ExecStartPre)", Hidden: true, Run: runRunPrepare},
	{Name: "run-shutdown", Group: "roles", Summary: "run a stopped instance's shutdown ladder from shutdown.json", Hidden: true, Run: runRunShutdown},
	// probe-write runs INSIDE the transient unit `gvm apply` builds, as the
	// instance's own account and under the sandbox the drop-in will carry
	//. It is the only command whose caller is another copy of gvm.
	{Name: "probe-write", Group: "roles", Summary: "test whether the sandbox permits a write to each path (root, apply)", Hidden: true, Run: runProbeWrite},

	{Name: "game", Group: "manifests", Summary: "validate and inspect game manifests", Run: runGame},

	{Name: "create", Group: "instances", Summary: "define a new instance", Run: runCreate},
	{Name: "install", Group: "instances", Summary: "install or repair an instance's game files",
		Avail: avail.Unbuilt("installing game files is not in this build; `gvm install` reports its preflight, then refuses"), Run: runInstall},
	{Name: "apply", Group: "instances", Summary: "reconcile an instance with its spec", Run: runApply},
	{Name: "start", Group: "instances", Summary: "start an instance", Run: runStart},
	{Name: "stop", Group: "instances", Summary: "stop an instance, saving cleanly", Run: runStop},
	{Name: "restart", Group: "instances", Summary: "restart an instance", Run: runRestart},
	{Name: "status", Group: "instances", Summary: "show instance state", Run: runStatus},
	{Name: "list", Group: "instances", Summary: "list instances", Run: runList},
	{Name: "logs", Group: "instances", Summary: "print an instance's console log and shutdown records", Run: runLogs},
	{Name: "ports", Group: "instances", Summary: "show port occupancy and conflicts", Run: runPorts},
	{Name: "destroy", Group: "instances", Summary: "remove an instance, archiving it first", Run: runDestroy},
	{Name: "console", Group: "instances", Summary: "attach to an instance console", Run: runConsole},
	{Name: "control", Group: "instances", Summary: "inspect and drive an instance's control channel", Run: runControl},

	{Name: "content", Group: "content", Summary: "manage mods and workshop items", Run: runContent},
	// `gvm mods` is an alias, because operators say "mods". It
	// is hidden so the usage listing names one spelling: two visible entries
	// for one verb invites the question of how they differ.
	{Name: "mods", Group: "content", Summary: "alias for `gvm content`", Hidden: true, Run: runContent},
	{Name: "backup", Group: "operations", Summary: "archive an instance, verify and prune archives", Run: runBackup},
	{Name: "restore", Group: "operations", Summary: "restore an archive over a world",
		Avail: avail.Partialf("`gvm restore --list` works; restoring a world is not in this build"), Run: runRestore},
	{Name: "update", Group: "operations", Summary: "update an instance, deciding before it stops",
		Avail: avail.Unbuilt("updating game files is not in this build; the decision-only report works"), Run: runUpdate},
	{Name: "rebuild", Group: "operations", Summary: "rebuild a world, archiving it first",
		Avail: avail.Unbuilt("rebuilding a world is not in this build"), Run: runRebuild},
	// `gvm job run %i` is gvm-job@.service's ExecStart and is
	// also how an operator fires a scheduled job by hand, so it is listed
	// rather than hidden: `gvm job list` is the answer to "what does this box
	// run at 05:00", which is a question asked far more often than it is run.
	{Name: "job", Group: "operations", Summary: "run and inspect the scheduled work", Run: runJob},

	{Name: "adopt", Group: "migration", Summary: "import an existing server install",
		Avail: avail.Partialf("`gvm adopt discover` and `finalize` work; cutover and rollback are not in this build"), Run: runAdopt},

	{Name: "user", Group: "admin", Summary: "manage dashboard users and roles",
		Avail: avail.Unbuilt("dashboard users are not in this build"), Run: runUser},
	{Name: "token", Group: "admin", Summary: "manage API tokens",
		Avail: avail.Unbuilt("API tokens are not in this build"), Run: runToken},
	{Name: "secret", Group: "admin", Summary: "manage instance secrets",
		Avail: avail.Unbuilt("setting instance secrets is not in this build"), Run: runSecret},
	{Name: "audit", Group: "admin", Summary: "view audit log records",
		Avail: avail.Unbuilt("reading the audit log is not in this build"), Run: runAudit},

	{Name: "host", Group: "host", Summary: "install gvm on this host (host init)", Run: runHost},
	{Name: "firewall", Group: "host", Summary: "manage host firewall rules",
		Avail: avail.Unbuilt("gvm does not manage the firewall in this build"), Run: runFirewall},
	{Name: "doctor", Group: "host", Summary: "check the host for problems", Run: runDoctor},
	{Name: "db", Group: "host", Summary: "manage the datastore schema", Run: runDB},
}

// help and completion are registered here rather than in the literal above
// because they read `commands` (help via lookup, completion via
// completionCommands): as a literal that is an initialization cycle.
func init() {
	commands = append(commands,
		Command{Name: "help", Group: "general", Summary: "show usage for a command", Run: runHelp},
		Command{Name: "completion", Group: "general", Summary: "print a shell completion script (bash|zsh|fish)", Run: runCompletion},
	)
}

var groupOrder = []struct{ key, title string }{
	{"general", "General"},
	{"instances", "Instances"},
	{"content", "Content"},
	{"operations", "Operations"},
	{"migration", "Migration"},
	{"admin", "Administration"},
	{"host", "Host"},
	{"manifests", "Manifests"},
}

func lookup(name string) (Command, bool) {
	for _, c := range commands {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// Main runs the CLI and returns a process exit code.
func Main(ctx context.Context, env *Env) (code int) {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.Geteuid == nil {
		env.Geteuid = os.Geteuid
	}

	if len(env.Args) == 0 {
		usage(env.Stderr)
		return 2
	}

	name := env.Args[0]
	switch name {
	case "-h", "--help":
		usage(env.Stdout)
		return 0
	case "-v", "--version":
		name = "version"
	}

	cmd, ok := lookup(name)
	if !ok {
		if s := didYouMean(name, commandNames()); s != "" {
			fmt.Fprintf(env.Stderr, "gvm: unknown command %q — did you mean %q?\n", name, s)
		} else {
			fmt.Fprintf(env.Stderr, "gvm: unknown command %q\n\n", name)
			usage(env.Stderr)
		}
		return 2
	}

	// Whatever the command built gets released before the process exits: the
	// production HostEnv holds a systemd connection and an open audit spool,
	// and the spool's last records are only durable once it is closed. It is a
	// defer rather than a call at the end of the happy path because the run
	// that most needs its audit trail flushed is the one that failed.
	defer func() {
		if env.Host != nil {
			_ = env.Host.Close()
		}
	}()

	// A panic in an operator verb is a gvm bug, not an operator error: catch it,
	// say so in one line, and exit 1 rather than dumping a stack at someone who
	// cannot act on it. The role verbs (run, run-shutdown, …) are NOT wrapped —
	// they crash into the journal with their own exit codes, which is what
	// systemd's Restart and RestartPreventExitStatus read (U8, rule 4).
	if routeOf(name) != RouteRole {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(env.Stderr, "gvm %s: %v\n", name, r)
				fmt.Fprintln(env.Stderr, "this is a gvm bug; GVM_DEBUG=1 shows the stack")
				if env.Getenv("GVM_DEBUG") != "" {
					fmt.Fprintf(env.Stderr, "%s\n", debug.Stack())
				}
				code = 1
			}
		}()
	}

	if err := cmd.Run(ctx, env, env.Args[1:]); err != nil {
		// Requested help is success: the FlagSet already printed usage, so a
		// parse that stopped for -h/--help exits 0 and continues into nothing.
		if errors.Is(err, errHelpShown) || errors.Is(err, flag.ErrHelp) {
			return 0
		}
		// A role's exit code is a contract with systemd (see ExitError), so it
		// is read before anything else and passed through untouched.
		var xe *ExitError
		if errors.As(err, &xe) {
			if xe.Err != nil {
				fmt.Fprintf(env.Stderr, "gvm %s: %v\n", name, xe.Err)
			}
			return xe.Code
		}
		// Everything "not in this build" exits 3 with one standard line, so a
		// script can tell "gvm cannot do this" from a real failure by the code
		// alone (U8). The reason is printed below it, in the command's own
		// words — "gvm control probe" and "gvm web" are both unbuilt and an
		// operator still needs to tell them apart — but the "not available"
		// sentence is said once, by Main, and never by the error.
		if errors.Is(err, avail.ErrUnavailable) {
			fmt.Fprintf(env.Stderr, "gvm %s: not available in this build; nothing was changed.\n", name)
			fmt.Fprintf(env.Stderr, "  %s\n", err)
			return 3
		}
		// The forgotten `sudo`: one line that echoes the whole command, so the
		// fix is a copy-paste and not a paragraph about transports. Every
		// remaining ErrNoTransport is a non-root operator who needs root — the
		// remote advice is gone with the API client (U10) — so the message is
		// the same for a host op and for the console socket.
		if errors.Is(err, ErrNoTransport) && env.Geteuid() != 0 {
			full := "gvm " + strings.Join(env.Args, " ")
			fmt.Fprintf(env.Stderr, "%s needs root. Run: sudo %s\n", full, full)
			return 1
		}
		// The operator exit-code table (U8): a verb that refused (5), checked
		// and found something (4) or was mis-typed (2) carries its own code,
		// which Main reads here for operator verbs. Role verbs never reach this:
		// their ExitError was already returned above, keeping their systemd
		// contract intact.
		var ce *codedError
		if errors.As(err, &ce) {
			fmt.Fprintf(env.Stderr, "gvm %s: %v\n", name, ce.err)
			return ce.code
		}
		fmt.Fprintf(env.Stderr, "gvm %s: %v\n", name, err)
		return 1
	}
	return 0
}

// Operator exit codes (U8). Role verbs keep their own contract (ExitError).
const (
	exitOK          = 0
	exitFailed      = 1
	exitUsage       = 2
	exitUnavailable = 3
	exitChecked     = 4 // checked and found something: a problem, a conflict, an available update
	exitRefused     = 5 // refused or not confirmed
)

// codedError carries an operator exit code up to Main. The message is the
// inner error's; only the code is added, so a command says what happened and
// Main decides which number a script sees.
type codedError struct {
	code int
	err  error
}

func (c *codedError) Error() string { return c.err.Error() }
func (c *codedError) Unwrap() error { return c.err }

// refused marks a verb that declined to act (5): a typed-name mismatch, a
// lifecycle refusal, an unmet precondition the operator must clear.
func refused(err error) error { return &codedError{exitRefused, err} }

// checked marks a verb that looked and found something worth a non-zero code
// without failing (4): doctor problems, `list --problems`, discover blockers,
// an available update. A pipeline can branch on it without parsing text.
func checked(err error) error { return &codedError{exitChecked, err} }

// usageErr marks a usage mistake (2): an unknown subcommand, a missing
// required argument. It is distinct from a failure so `|| exit 2` means "the
// operator typed it wrong", not "the action failed".
func usageErr(err error) error { return &codedError{exitUsage, err} }

// commandNames is the visible, dispatchable command names, for did-you-mean.
func commandNames() []string {
	out := make([]string, 0, len(commands))
	for _, c := range commands {
		if !c.Hidden {
			out = append(out, c.Name)
		}
	}
	return out
}

// didYouMean returns the closest candidate within a small edit distance, or ""
// when nothing is close enough. A typo like "creat" for "create" is one edit; a
// word that resembles nothing offers no misleading suggestion.
func didYouMean(input string, candidates []string) string {
	best, bestDist := "", 1<<30
	for _, c := range candidates {
		d := editDistance(input, c)
		if d < bestDist {
			best, bestDist = c, d
		}
	}
	// Allow at most a third of the longer word to differ, capped at 3 — enough
	// for a transposition or a dropped letter, not a different word.
	limit := len(input)/3 + 1
	if limit > 3 {
		limit = 3
	}
	if bestDist <= limit {
		return best
	}
	return ""
}

// editDistance is the Levenshtein distance, two rolling rows.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	cur := make([]int, len(rb)+1)
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// isHelpFlag reports whether a token is a request for help.
func isHelpFlag(s string) bool { return s == "-h" || s == "--help" || s == "help" }

// helpRequested reports whether any argument asks for help. It lets a
// subcommand that has no FlagSet of its own (an unbuilt one) still answer
// `--help` with exit 0 rather than its refusal.
func helpRequested(args []string) bool {
	for _, a := range args {
		if isHelpFlag(a) {
			return true
		}
	}
	return false
}

func notImplemented(context.Context, *Env, []string) error { return ErrNotImplemented }

// runFirewall is a routed-but-unbuilt verb that still answers --help with exit 0
// (U7) before refusing. gvmd and web stay on notImplemented because they are
// hidden roles a human never types for help.
func runFirewall(_ context.Context, env *Env, args []string) error {
	if helpRequested(args) {
		fmt.Fprintln(env.Stdout, "Usage: gvm firewall <subcommand>")
		fmt.Fprintln(env.Stdout, "\ngvm does not manage the firewall in this build.")
		return errHelpShown
	}
	return ErrNotImplemented
}

func runVersion(_ context.Context, env *Env, _ []string) error {
	fmt.Fprintln(env.Stdout, version.String())
	return nil
}

func runHelp(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 || isHelpFlag(args[0]) {
		usage(env.Stdout)
		return nil
	}
	cmd, ok := lookup(args[0])
	if !ok {
		if s := didYouMean(args[0], commandNames()); s != "" {
			return fmt.Errorf("unknown command %q — did you mean %q?", args[0], s)
		}
		return fmt.Errorf("unknown command %q", args[0])
	}
	fmt.Fprintf(env.Stdout, "gvm %s — %s\n", cmd.Name, cmd.Summary)
	switch cmd.Avail.State {
	case avail.NotInBuild:
		fmt.Fprintf(env.Stdout, "\n  Not available in this build: %s\n", cmd.Avail.Why)
	case avail.Partial:
		fmt.Fprintf(env.Stdout, "\n  Partly available in this build: %s\n", cmd.Avail.Why)
	}
	// Delegate to the command's own `--help` so `gvm help <cmd> [sub]` prints
	// the same full usage `<cmd> [sub] --help` does, from one source. The
	// delegated call prints usage and returns errHelpShown, which is success.
	fmt.Fprintln(env.Stdout)
	rest := append(append([]string(nil), args[1:]...), "--help")
	if err := cmd.Run(ctx, env, rest); err != nil && !errors.Is(err, errHelpShown) && !errors.Is(err, flag.ErrHelp) {
		return err
	}
	return nil
}
