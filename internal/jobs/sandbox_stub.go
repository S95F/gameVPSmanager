//go:build !linux

package jobs

import (
	"context"
	"io"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// RunSandboxed off Linux refuses rather than running the command unconfined.
//
// There is no transient unit here, no uid to drop to and no ProtectSystem, so
// the only two possible answers are "cannot ask" and running third-party code
// in gvm's own process as whoever started it. The whole of D-16 is that the
// second one is not an option, so the development machine gets the refusal and
// the job prints it.
//
// Args, ShowProperties, ParseShow and CheckShow are deliberately untagged: the
// exact command this would run and the exact verdict it would reach on the
// properties read back are both asserted by tests that run here.
func RunSandboxed(context.Context, Sandbox, []string, io.Writer) error {
	return platform.Unsupported("transient units (systemd-run)")
}

// ShowUnit off Linux has no unit to read.
func ShowUnit(context.Context, string, []string) (map[string]string, error) {
	return nil, platform.Unsupported("systemctl show")
}

// VerifyUnit off Linux refuses rather than approving.
//
// Returning nil here would be the worst available answer: it would make the
// development machine report a sandbox as verified when nothing looked at it.
func VerifyUnit(context.Context, Sandbox) error {
	return platform.Unsupported("systemctl show")
}
