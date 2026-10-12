package jobs

import (
	"context"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The `restart` kind, and the two step constructors every other stopping kind
// reuses.
//
// It is the smallest job in the system -- ladder, then start -- and that makes
// it the clearest statement of what the running guard buys. The bash suite's
// equivalent is `ExecStart=/bin/systemctl restart zomboid.service`, which is
// genuinely safe because systemd's own restart job cannot stop a unit and
// forget to start it. gvm's is two calls, because the ladder is gvm's (section
// 11.6) and the save has to be observed, and two calls is exactly where the
// hazard appears: a failure between them leaves a stopped world. So the guard
// is not optional here either, and guard_test.go injects a failure at both.

var restartRunner = &Runner{
	Kind:    lifecycle.JobRestart,
	Scope:   ScopeInstance,
	Policy:  PolicyStartAnyway,
	Stops:   true,
	Summary: "run the shutdown ladder, then start",
	Steps: func(env *Env) ([]Step, error) {
		return []Step{
			stopStep("restart.stop", env),
			startStep("restart.start", env),
		}, nil
	},
}

// stopStep is the shutdown ladder as a step.
//
// It records the RUNG, which is the field that turns "the server stopped" into
// "the server SAVED" -- audit.InstanceStop requires it on its success row for
// that reason, and a job summary without it cannot answer the only question
// that matters after an unattended restart.
//
// A SIGKILL rung degrades the run without failing it. The world may have lost
// up to one autosave interval, which is worth reporting and is not worth
// refusing to restart over.
func stopStep(name string, env *Env) Step {
	return Step{
		Name:  name,
		Fatal: true,
		Stops: true,
		Do: func(ctx context.Context, jr *Run) error {
			out, err := env.Super.Stop(ctx, env.Instance, env.StopBudget)
			if err != nil {
				return err
			}
			jr.Set("rung", out.Rung)
			jr.Set("stop_ms", out.Took.Milliseconds())
			if out.Degraded {
				jr.Degraded = true
				jr.Note("the shutdown ladder needed a SIGKILL (rung %q); the world may have "+
					"lost up to one autosave interval", out.Rung)
			}
			return nil
		},
	}
}

// startStep brings the instance back and, if the caller supplied a way,
// waits for the game rather than for the unit.
//
// Readiness failing is reported as this step failing, and that is the honest
// reading: the remaining phases of an update -- content settle, mod
// resolution -- need a server that has finished booting, and running them
// against one that has not would produce a resolution from a half-written
// tree. The server is up either way, so the guard finds nothing to do and the
// run is degraded rather than failed.
func startStep(name string, env *Env) Step {
	return Step{
		Name:  name,
		Fatal: true,
		Do: func(ctx context.Context, jr *Run) error {
			if err := env.Super.Start(ctx, env.Instance); err != nil {
				return err
			}
			if env.WaitReady == nil {
				return nil
			}
			if err := env.WaitReady(ctx, env.Instance); err != nil {
				return fmt.Errorf("started %s, and it did not report ready: %w",
					env.Instance, err)
			}
			return nil
		},
	}
}
