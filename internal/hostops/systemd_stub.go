//go:build !linux

package hostops

import (
	"context"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// newSystemd refuses off Linux, and ships in the same commit as the Linux side
// rather than "later".
//
// It refuses rather than returning a no-op driver on purpose. A no-op would
// make `gvm apply` on the development machine report a clean run against a
// host it never touched, which is the single most misleading result this
// package could produce. Everything worth testing here -- the verb table, the
// path derivation, the ordering -- runs against the seams in Config, so the
// stub costs no coverage.
func newSystemd(context.Context) (platform.Systemd, func() error, error) {
	return nil, nil, platform.Unsupported("systemd")
}
