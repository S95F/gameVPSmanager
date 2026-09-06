// Package cli is gvm's command dispatch.
//
// Dispatch is a plain table over stdlib flag rather than a framework. The
// binary is one static file deployed to a VPS, and a command router is the
// least interesting thing in this project to take a dependency for.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/S95F/gameVPSmanager/internal/version"
)

// ErrNotImplemented marks a command that is routed but not yet built. It
// exits non-zero so a script cannot mistake an unbuilt phase for success.
var ErrNotImplemented = errors.New("not implemented yet")

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
	Run    func(ctx context.Context, env *Env, args []string) error
}

// commands is the dispatch table. Order within a group is the order listed.
var commands = []Command{
	{Name: "version", Group: "general", Summary: "print the version banner", Run: runVersion},

	{Name: "gvmd", Group: "roles", Summary: "run the privileged verb gateway (root)", Hidden: true, Run: notImplemented},
	{Name: "web", Group: "roles", Summary: "run the dashboard and API", Hidden: true, Run: notImplemented},
	{Name: "run", Group: "roles", Summary: "supervise one instance", Hidden: true, Run: runRun},
	// The three roles below are invoked by the unit and by gvmd, never typed.
	// run-shutdown in particular is the layer-3 recovery path of section 11.8:
	// it exists to be run when the runner is already gone.
	{Name: "run-prepare", Group: "roles", Summary: "chown the runtime directory (root, ExecStartPre)", Hidden: true, Run: runRunPrepare},
	{Name: "run-shutdown", Group: "roles", Summary: "run a stopped instance's shutdown ladder from shutdown.json", Hidden: true, Run: runRunShutdown},
	// probe-write runs INSIDE the transient unit `gvm apply` builds, as the
	// instance's own account and under the sandbox the drop-in will carry
	// (D-15). It is the only command whose caller is another copy of gvm.
	{Name: "probe-write", Group: "roles", Summary: "test whether the sandbox permits a write to each path (root, apply)", Hidden: true, Run: runProbeWrite},

	{Name: "game", Group: "manifests", Summary: "validate and inspect game manifests", Run: runGame},

	{Name: "create", Group: "instances", Summary: "define a new instance", Run: runCreate},
	{Name: "install", Group: "instances", Summary: "install or repair an instance's game files", Run: runInstall},
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

	{Name: "content", Group: "content", Summary: "manage mods and workshop items", Run: notImplemented},
	{Name: "backup", Group: "operations", Summary: "archive an instance", Run: notImplemented},
	{Name: "restore", Group: "operations", Summary: "restore an archive", Run: notImplemented},
	{Name: "update", Group: "operations", Summary: "update an instance", Run: notImplemented},
	{Name: "rebuild", Group: "operations", Summary: "rebuild a world, archiving it first", Run: notImplemented},

	{Name: "adopt", Group: "migration", Summary: "import an existing server install", Run: notImplemented},
	{Name: "firewall", Group: "host", Summary: "manage host firewall rules", Run: notImplemented},
	{Name: "doctor", Group: "host", Summary: "check the host for problems", Run: notImplemented},
}

// help is registered here rather than in the literal above because it calls
// lookup, which reads commands: as a literal that is an initialization cycle.
func init() {
	commands = append(commands, Command{
		Name: "help", Group: "general", Summary: "show usage for a command", Run: runHelp,
	})
}

var groupOrder = []struct{ key, title string }{
	{"general", "General"},
	{"instances", "Instances"},
	{"content", "Content"},
	{"operations", "Operations"},
	{"migration", "Migration"},
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
func Main(ctx context.Context, env *Env) int {
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
		fmt.Fprintf(env.Stderr, "gvm: unknown command %q\n\n", name)
		usage(env.Stderr)
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

	if err := cmd.Run(ctx, env, env.Args[1:]); err != nil {
		// The wrapped error is printed rather than a fixed sentence so that a
		// half-built subcommand can say WHICH half: "gvm control probe" and
		// "gvm web" are both unbuilt and an operator needs to tell them apart.
		if errors.Is(err, ErrNotImplemented) {
			fmt.Fprintf(env.Stderr, "gvm %s: %v\n", name, err)
			return 3
		}
		var xe *ExitError
		if errors.As(err, &xe) {
			if xe.Err != nil {
				fmt.Fprintf(env.Stderr, "gvm %s: %v\n", name, xe.Err)
			}
			return xe.Code
		}
		fmt.Fprintf(env.Stderr, "gvm %s: %v\n", name, err)
		return 1
	}
	return 0
}

func notImplemented(context.Context, *Env, []string) error { return ErrNotImplemented }

func runVersion(_ context.Context, env *Env, _ []string) error {
	fmt.Fprintln(env.Stdout, version.String())
	return nil
}

func runHelp(_ context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		usage(env.Stdout)
		return nil
	}
	cmd, ok := lookup(args[0])
	if !ok {
		return fmt.Errorf("unknown command %q", args[0])
	}
	fmt.Fprintf(env.Stdout, "gvm %s — %s\n", cmd.Name, cmd.Summary)
	return nil
}
