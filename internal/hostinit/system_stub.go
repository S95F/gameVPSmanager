//go:build !linux

package hostinit

import (
	"context"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// NewSystem refuses off Linux with the project's one sentinel. `gvm host init`
// installs systemd units, runs systemd's own tools and talks to the manager
// over D-Bus; none of that exists on a developer's Windows or macOS box, and
// failing with platform.ErrNotSupported is what lets the CLI, a developer and
// the test suite all recognise the refusal (the same contract `gvm console`,
// `gvm run` and the btrfs path use).
func NewSystem(_ context.Context) (System, func() error, error) {
	return nil, nil, platform.ErrNotSupported
}
