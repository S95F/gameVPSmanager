//go:build !linux

package platform

import "net"

// PeerCred is Linux-only: SO_PEERCRED has no portable equivalent, and a stub
// that guessed at an identity would be an authorization bypass rather than a
// degraded feature.
func PeerCred(c *net.UnixConn) (Peer, error) { return Peer{}, Unsupported("SO_PEERCRED") }
