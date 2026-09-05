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
var ErrNotImplemented = errors.New("not implemented")

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
	{Name: "run", Group: "roles", Summary: "supervise one instance", Hidden: true, Run: notImplemented},

	{Name: "game", Group: "manifests", Summary: "validate and inspect game manifests", Run: notImplemented},

	{Name: "create", Group: "instances", Summary: "define a new instance", Run: notImplemented},
	{Name: "apply", Group: "instances", Summary: "reconcile an instance with its spec", Run: notImplemented},
	{Name: "start", Group: "instances", Summary: "start an instance", Run: notImplemented},
	{Name: "stop", Group: "instances", Summary: "stop an instance, saving cleanly", Run: notImplemented},
	{Name: "restart", Group: "instances", Summary: "restart an instance", Run: notImplemented},
	{Name: "status", Group: "instances", Summary: "show instance state", Run: notImplemented},
	{Name: "list", Group: "instances", Summary: "list instances", Run: notImplemented},
	{Name: "console", Group: "instances", Summary: "attach to an instance console", Run: notImplemented},

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

	if err := cmd.Run(ctx, env, env.Args[1:]); err != nil {
		if errors.Is(err, ErrNotImplemented) {
			fmt.Fprintf(env.Stderr, "gvm %s: not implemented yet\n", name)
			return 3
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
