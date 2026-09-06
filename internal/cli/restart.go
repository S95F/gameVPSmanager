// `gvm restart` — one verb, not a stop followed by a start.
//
// It is a single lifecycle verb because the two halves must not be separable
// by an operator's terminal dying between them. A `gvm stop` writes the hold
// file so that gvm-guard.timer does not undo it; if a restart were a stop and
// then a start typed as two commands, an interrupted one would leave a server
// down AND held, and the guard -- the thing whose whole job is to notice a
// world that should be running and is not -- would deliberately leave it that
// way. As one verb, the hold is never written at all.

package cli

import (
	"context"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

func runRestart(ctx context.Context, env *Env, args []string) error {
	return runLifecycle(ctx, env, lifecycle.VerbRestart, args)
}
