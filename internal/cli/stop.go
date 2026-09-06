// `gvm stop` — and the one seam that makes a stop reportable.
//
// The bash suite stopped a world with `ExecStop=screen -X stuff 'quit\015'`,
// which returned immediately and left no evidence at all: a world that saved in
// 40 s and a JVM that hung until systemd's SIGKILL produced byte-identical
// output in `systemctl status`. Section 11.6 exists to end that, and the way it
// ends is that a stop reports the RUNG it finished on.
//
// So the ladder is driven through the runner's own console socket -- the
// process that owns the PTY, knows the game's pid and can watch its process
// group go away -- and lifecycle.Manager REFUSES a stop with no such seam
// wired. That refusal is not a gap: audit.instance_stop requires `rung` on its
// success row, and a stop that cannot supply one writes a record saying a world
// was stopped and unable to say whether it was saved.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/runner"
	"github.com/S95F/gameVPSmanager/internal/socket"
)

func runStop(ctx context.Context, env *Env, args []string) error {
	return runLifecycle(ctx, env, lifecycle.VerbStop, args)
}

// shutdown returns the ladder seam, defaulting to the console socket client.
func (h *HostEnv) shutdown() lifecycle.ShutdownFunc {
	if h.Shutdown != nil {
		return h.Shutdown
	}
	return consoleShutdown
}

// consoleShutdown drives the ladder over /run/gvm/<name>/console.sock.
//
// The uid-0 `stop` op is the production path of section 11.4. What comes back
// is the runner's own shutdown report -- the same document appended to
// shutdown.jsonl -- so the rung this function returns and the rung an operator
// finds in the log later are the same field of the same record, not two
// independent observations that can disagree.
func consoleShutdown(ctx context.Context, name string, budget time.Duration) (lifecycle.Outcome, error) {
	path := instance.PathsFor(name).ConsoleSock
	c, err := socket.Dial(ctx, path)
	if err != nil {
		return lifecycle.Outcome{}, dialError(name, path, err)
	}
	defer c.Close()
	if _, err := c.Hello(clientID()); err != nil {
		return lifecycle.Outcome{}, err
	}

	// The budget is NOT sent: the ladder the runner walks is the one rendered
	// from the instance's own shutdown.budget and written to shutdown.json,
	// and a client-supplied deadline would be a second answer to "how long may
	// this world take to save". A caller who wants a different budget edits
	// the spec and applies it.
	_ = budget

	started := time.Now()
	raw, err := c.Stop("", "gvm stop", nil, nil)
	if err != nil {
		return lifecycle.Outcome{}, err
	}
	var rep runner.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return lifecycle.Outcome{}, fmt.Errorf("the runner's shutdown report did not parse: %w", err)
	}
	took := rep.Duration.Duration()
	if took == 0 {
		took = time.Since(started)
	}
	return lifecycle.Outcome{
		Rung:     string(rep.FinalRung),
		Degraded: rep.Degraded,
		Took:     took,
	}, nil
}
