//go:build !linux

package socket

import (
	"net"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// SocketMode is declared off Linux too, so the one number that section 5's
// layout fixes lives in exactly one place regardless of build.
const SocketMode = 0o660

// Listen is Linux-only. AF_UNIX exists on the development machine, but the
// 0660/setgid ownership that makes D-10 true does not, and a listener without
// it would be a socket with no access control at all.
func Listen(path string) (*net.UnixListener, error) {
	return nil, platform.Unsupported("console socket listen")
}

// PeerIdentifier is Linux-only, because SO_PEERCRED is. There is deliberately
// no permissive fallback: an identifier that guessed would be an authorization
// bypass rather than a degraded feature, and the pure matrix in auth.go is
// what makes the decision testable here instead.
func PeerIdentifier(c net.Conn) (Identity, error) {
	return Identity{}, platform.Unsupported("SO_PEERCRED")
}

// DefaultPolicy is Linux-only: it reads the process uid and a unix group.
func DefaultPolicy() (Policy, error) {
	return Policy{}, platform.Unsupported("console socket policy")
}
