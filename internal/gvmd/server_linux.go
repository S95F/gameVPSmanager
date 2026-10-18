//go:build linux

package gvmd

import (
	"fmt"
	"net"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// DefaultIdentifier reads SO_PEERCRED from an accepted unix connection — the
// kernel-stamped identity the authorization decision runs on (D-10).
func DefaultIdentifier(conn net.Conn) (platform.Peer, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return platform.Peer{}, fmt.Errorf("gvmd: peer credentials need a unix connection, have %T", conn)
	}
	return platform.PeerCred(uc)
}
