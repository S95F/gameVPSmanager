package platform

import "strconv"

// Peer is the identity the kernel attaches to the far end of a unix socket.
//
// It is the console socket's whole authorization story (D-09, D-10): the
// credentials are stamped by the kernel at connect time and cannot be forged
// by the client, which is why the socket needs no tokens, no TLS and no
// password to distinguish gvmd from the instance user from gvm-web.
type Peer struct {
	PID      int32
	UID, GID uint32
}

// Identified reports whether the kernel could name the peer at all.
//
// SO_PEERCRED reports pid 0 when the peer lives in a different pid namespace,
// and the zero Peer is what a failed PeerCred leaves in the caller's variable.
// Both mean the same thing to an authorization decision: this connection has
// no established identity.
func (p Peer) Identified() bool { return p.PID > 0 }

// Root reports whether the peer is uid 0 -- gvmd, or a root CLI, the only tier
// permitted the lifecycle ops (verb, stop).
//
// An unidentified peer is not root, so a caller that forgot to check
// PeerCred's error grants nothing rather than everything.
func (p Peer) Root() bool { return p.Identified() && p.UID == 0 }

func (p Peer) String() string {
	return "pid=" + strconv.FormatInt(int64(p.PID), 10) +
		" uid=" + strconv.FormatUint(uint64(p.UID), 10) +
		" gid=" + strconv.FormatUint(uint64(p.GID), 10)
}
