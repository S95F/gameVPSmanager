//go:build linux

package platform

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// PeerCred reads SO_PEERCRED from an accepted unix connection.
//
// The lookup goes through SyscallConn's Control so the runtime keeps the fd
// alive for the duration of the getsockopt; taking the fd out of the poller by
// hand would race the connection being closed under a misbehaving client.
func PeerCred(c *net.UnixConn) (Peer, error) {
	if c == nil {
		return Peer{}, fmt.Errorf("platform: SO_PEERCRED on a nil connection")
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return Peer{}, fmt.Errorf("platform: SO_PEERCRED: %w", err)
	}
	var (
		ucred *unix.Ucred
		serr  error
	)
	if err := raw.Control(func(fd uintptr) {
		ucred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return Peer{}, fmt.Errorf("platform: SO_PEERCRED: %w", err)
	}
	if serr != nil {
		return Peer{}, fmt.Errorf("platform: SO_PEERCRED: %w", serr)
	}
	return Peer{PID: ucred.Pid, UID: ucred.Uid, GID: ucred.Gid}, nil
}
