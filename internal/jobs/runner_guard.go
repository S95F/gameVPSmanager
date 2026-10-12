package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The `guard` kind: belt and braces beyond the `defer` (section 13.4).
//
// RunningGuard covers an error return, a panic and a cancelled context. What
// it cannot cover is SIGKILL of the job process -- the OOM killer,
// `systemctl kill`, power loss mid-update -- because a deferred function dies
// with its process. So `gvm-guard.timer` (OnBootSec=3min,
// OnUnitActiveSec=5min) runs `gvm job run _guard`, and this is what it runs.
//
// The assertion is section 13.4's, exactly:
//
//	desired_running := spec.enabled
//	               && the systemd unit is enabled
//	               && no /run/gvm/<n>/hold
//
// and the `hold` term is what stops this from fighting the operator. `gvm stop`
// writes it with the actor and the reason; `gvm start` removes it. /run is
// tmpfs, so a hold does not survive a reboot -- which exactly matches systemd's
// own semantics, where a stopped-but-enabled unit comes back on boot. A durable
// stop is `gvm disable`, which sets enabled: false and applies.
//
// This is the piece that turns "the nightly update crashed at 05:03" into a
// five-minute outage rather than an eight-hour one.
//
// Two properties of this runner are asserted rather than documented. It is
// HOST-level, so it takes no instance lease and arms no RunningGuard of its
// own -- there is no single instance to guard, and a job that locked one
// instance while starting another would block that instance's nightly. And it
// STARTS, NEVER STOPS: Runner.Stops is false, the engine panics on a stopping
// step in a non-stopping kind, and guard_test.go asserts the fake systemd
// records zero stop calls however the sweep fails.

// Recovery is one instance the guard sweep intends to start.
type Recovery struct {
	// Instance is the instance name.
	Instance string `json:"instance"`
	// Reason is the sentence recorded on the `guard` run, and section 13.4
	// gives its shape: "recovered after <unit> ended without restoring
	// service".
	Reason string `json:"reason"`
	// ClearStaleLease says the dead holder's lease.json has to go before the
	// start, because lifecycle.Allowed refuses a start on a stale lease and
	// the sweep's supervisor is deliberately not forced. It is carried on the
	// Recovery rather than re-derived in Recover so that Desired stays the
	// only place that reads an Observation -- a second opinion about the
	// lease would be a second opinion about whether to act.
	ClearStaleLease bool `json:"clear_stale_lease,omitempty"`
}

// Sweeper is the host-level recovery sweep.
//
// Desired is separated from Recover because the two have different
// consequences and deserve different failure handling: enumerating is a read
// over every instance's spec, unit state and markers, and a single unreadable
// spec must not stop the sweep from recovering the other four servers.
type Sweeper interface {
	// Desired lists every instance that should be running and is not, with
	// the reason each one qualified.
	//
	// It applies section 13.4's three terms. It does NOT start anything: a
	// sweep that actuated inside its own enumeration could not be dry-run,
	// and `gvm doctor` needs exactly this list without the starts.
	Desired(ctx context.Context) ([]Recovery, error)
	// Recover starts one instance.
	Recover(ctx context.Context, r Recovery) error
}

var guardRunner = &Runner{
	Kind:  lifecycle.JobGuard,
	Scope: ScopeHost,
	// `warn`: section 13.3's last row. Every failure here is a failure to
	// RECOVER something, which is worth reporting and is never itself an
	// outage -- and a guard that paged on its own partial success would be a
	// guard somebody disabled.
	Policy:  PolicyWarn,
	Stops:   false,
	Summary: "start every instance that should be running and is not",
	Steps: func(env *Env) ([]Step, error) {
		s := env.Sweeper
		if s == nil {
			return nil, fmt.Errorf("%w: a guard job needs a sweeper (Env.Sweeper)", ErrEnv)
		}
		var want []Recovery

		return []Step{
			{
				// Fatal: an enumeration that failed is not a sweep that found
				// nothing, and carrying on with an empty list would report
				// `ok` for a run that looked at nothing.
				Name: "guard.enumerate", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error {
					w, err := s.Desired(ctx)
					if err != nil {
						return err
					}
					want = w
					names := make([]string, 0, len(w))
					for _, r := range w {
						names = append(names, r.Instance)
					}
					jr.Set("candidates", names)
					if len(w) == 0 {
						// The normal outcome, every five minutes, forever.
						// `skipped` and not `ok` so that a `guard` run which
						// actually recovered something is findable in a
						// month of rows (D-43).
						return fmt.Errorf("every instance that should be running is running: %w",
							ErrSkipJob)
					}
					return nil
				},
			},
			{
				// NOT fatal, and the loop is inside one step rather than one
				// step per instance: the table is built before the
				// enumeration has run, so there is nothing to build a
				// per-instance table from. Each failure is recorded by name.
				Name: "guard.recover",
				Do: func(ctx context.Context, jr *Run) error {
					var recovered, failed []string
					var errs []error
					for _, r := range want {
						if err := s.Recover(ctx, r); err != nil {
							failed = append(failed, r.Instance)
							errs = append(errs, fmt.Errorf("%s: %w", r.Instance, err))
							continue
						}
						recovered = append(recovered, r.Instance)
						jr.Note("recovered %s: %s", r.Instance, r.Reason)
					}
					jr.Set("recovered", recovered)
					if len(failed) > 0 {
						jr.Set("unrecovered", failed)
						jr.Degraded = true
						return fmt.Errorf("could not start %s: %w",
							strings.Join(failed, " "), errors.Join(errs...))
					}
					return nil
				},
			},
		}, nil
	},
}
