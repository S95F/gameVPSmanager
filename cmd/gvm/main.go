// Command gvm manages game servers on a VPS.
//
// One binary serves every role; which one is decided by argv[1]. systemd
// invokes the hidden roles (gvmd, web, run) and a human types the rest.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/S95F/gameVPSmanager/internal/cli"
)

func main() {
	// A cancelled context, not a hard exit: the runner role has a shutdown
	// ladder to walk before the game process may be allowed to die, and
	// killing it here is the documented way to corrupt a world.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Main(ctx, cli.DefaultEnv(os.Args[1:])))
}
