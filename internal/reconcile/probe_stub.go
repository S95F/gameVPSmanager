//go:build !linux

package reconcile

import (
	"context"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// ProbeWritable off Linux refuses rather than approving.
//
// There is no transient unit, no ProtectHome and no uid to drop to here, so
// the only honest answers are "cannot ask" and a lie. It returns the refusal,
// and `gvm apply` on a development machine reports the probe as not run --
// which is what the plan prints, rather than a tick nobody earned.
//
// Args() and ParseProbeResults() are cross-platform on purpose, so the exact
// command this would run and the exact output it would read back are both
// asserted by tests that run here.
func ProbeWritable(context.Context, ProbeSpec) ([]ProbeResult, error) {
	return nil, platform.Unsupported("the write-probe (systemd-run)")
}
