//go:build linux

package reconcile

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// systemdRunPaths are the absolute paths systemd-run ships at.
//
// Absolute and stat-resolved rather than looked up through PATH: this runs as
// root, and an inherited PATH deciding which program starts a transient unit
// with --uid= is a decision nobody made.
var systemdRunPaths = []string{
	"/usr/bin/systemd-run",
	"/bin/systemd-run",
}

// ProbeWritable starts a transient systemd SERVICE carrying the same User=,
// ProtectHome=, ProtectSystem= and ReadWritePaths= the generated drop-in will
// carry, and runs `gvm probe-write <paths...>` inside it (D-15).
//
// A service and not a scope, for section 7.7's reason: a scope inherits the
// caller's context and carries none of the properties, so it would prove
// nothing about the sandbox. `--wait --pipe` because the answer is the child's
// stdout and the whole point is to have it before deciding anything.
//
// This is the check that would have caught the adopted-instance failure -- a
// world under /home hidden by ProtectHome -- that disqualified a competing
// design. Everything about it is arranged so the answer arrives BEFORE the
// drop-in is written: Plan.Validate refuses the ordering, and this function
// changes nothing on the host beyond a probe file it removes.
func ProbeWritable(ctx context.Context, p ProbeSpec) ([]ProbeResult, error) {
	args, err := p.Args()
	if err != nil {
		return nil, err
	}
	bin, err := findTool(systemdRunPaths)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", platform.Unsupported("systemd-run"), err)
	}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	// One rung above RuntimeMaxSec, so the kernel's own deadline is what
	// normally fires and this one only covers systemd-run itself hanging.
	ctx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = []string{}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	// The output is parsed whether or not the command failed: `gvm
	// probe-write` exits 78 when a path is unwritable, which is a SUCCESSFUL
	// probe reporting a real answer. Treating a non-zero exit as "no results"
	// would discard exactly the finding this exists to produce.
	results, parseErr := ParseProbeResults(&stdout)
	if len(results) > 0 {
		for i := range results {
			if !results[i].OK && results[i].Blocked == "" {
				results[i].Blocked = Blame(p.Sandbox, results[i].Path)
			}
		}
		return results, nil
	}
	if parseErr != nil {
		return nil, parseErr
	}
	if runErr != nil {
		return nil, fmt.Errorf("%w: the probe unit produced no results: %v: %s",
			ErrProbe, runErr, trimStderr(stderr.String()))
	}
	return nil, fmt.Errorf("%w: the probe unit exited cleanly and reported nothing, so no "+
		"path was proved writable", ErrProbe)
}
