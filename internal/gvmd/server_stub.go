//go:build !linux

package gvmd

import (
	"net"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// DefaultIdentifier refuses off Linux: SO_PEERCRED is the whole authorization
// story and nothing portable substitutes for it. The daemon is Linux-only;
// this stub exists so the CLI wiring compiles into the cross-platform build.
func DefaultIdentifier(net.Conn) (platform.Peer, error) {
	return platform.Peer{}, platform.ErrNotSupported
}
