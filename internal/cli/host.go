// `gvm host` — install and inspect gvm's own presence on the host.
//
// `host init` is the one command that runs before gvm is installed, so it is
// the one host operation that is RouteLocal rather than RouteHostOp: it cannot
// be carried by gvmd (gvmd is one of the things it installs) and there is no
// transport to resolve. It checks for root itself and does its work in
// internal/hostinit, which runs systemd's own tools by absolute path and never
// shells out, runs apt, or touches the firewall.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/hostinit"
)

func runHost(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "usage: gvm host <subcommand>")
		fmt.Fprintln(env.Stderr, "subcommands: init")
		return usageErr(errors.New("host: subcommand required"))
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(env.Stdout, "Usage: gvm host <subcommand>")
		fmt.Fprintln(env.Stdout, "subcommands:")
		fmt.Fprintln(env.Stdout, "  init    install gvm's binary, units, tmpfiles, accounts and a default config")
		return errHelpShown
	case "init":
		return runHostInit(ctx, env, args[1:])
	default:
		return usageErr(fmt.Errorf("host: unknown subcommand %q", args[0]))
	}
}

func runHostInit(ctx context.Context, env *Env, args []string) error {
	var diff, printManifest bool
	fs := flag.NewFlagSet("gvm host init", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: sudo gvm host init [--diff]\n\n"+
			"Installs gvm on this host: this binary at /usr/bin/gvm, the systemd units,\n"+
			"the tmpfiles and sysusers fragments, and a default /etc/gvm/gvm.yaml if none\n"+
			"exists. It is idempotent — run it again and it converges — runs systemd's own\n"+
			"tools by absolute path, and never runs apt or touches the firewall. It prints\n"+
			"the apt line each game family needs; you run those.\n\n"+
			"Examples:\n"+
			"  sudo gvm host init\n"+
			"  sudo gvm host init --diff   # show every change, make none\n\n")
		fs.PrintDefaults()
	}
	fs.BoolVar(&diff, "diff", false, "print every change and make none")
	fs.BoolVar(&printManifest, "print-manifest", false,
		"print the install table (source, destination, mode) and exit; what `make install` reads")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}

	// --print-manifest is a read of the build's own table and needs no root and
	// no host: `make install` runs it to learn what to copy where.
	if printManifest {
		for _, f := range hostinit.Manifest() {
			fmt.Fprintf(env.Stdout, "%s\t%s\t%04o\n", f.Src, f.Dest, f.Mode)
		}
		return nil
	}

	// --diff is also harmless, but it reports host state (which timers are
	// already managed, whether a config exists), so it needs the real System.
	// Everything below here touches the host and needs root.
	if env.Geteuid() != 0 {
		return refused(errors.New("host init must run as root. Run: sudo gvm host init"))
	}
	sys, closeSys, err := hostinit.NewSystem(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = closeSys() }()
	return hostinit.Run(ctx, sys, env.Stdout, hostinit.Options{Diff: diff})
}
