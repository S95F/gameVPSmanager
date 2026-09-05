//go:build linux

package socket

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// SocketMode is the mode section 5's layout gives console.sock: 0660, with the
// group coming from the setgid bit on /run/gvm/<name>. Owner and group are
// therefore gvm-<name> and gvm-console, and nobody else has a path to the
// socket at all.
const SocketMode = 0o660

// staleProbeTimeout bounds the "is anybody serving this?" dial. It is a local
// unix connect, so it either completes immediately or the socket is dead.
const staleProbeTimeout = 200 * time.Millisecond

// ErrNoConsoleGroup means group gvm-console does not exist on this host. The
// policy returned alongside it is still usable: it simply has no console tier,
// so gvm-web cannot reach the data plane until `gvm doctor` is satisfied.
var ErrNoConsoleGroup = fmt.Errorf("socket: group %s does not exist on this host", ConsoleGroup)

// Listen creates the instance's console socket at path.
//
// The mode is set after the bind, which leaves a window where the socket is
// whatever the umask made it. That window is not exploitable because
// /run/gvm/<name> is 2750 gvm-<name>:gvm-console: nobody outside the two tiers
// can traverse the directory to reach the socket in the first place, which is
// the same reason the peer-credential table's fourth column is unreachable.
func Listen(path string) (*net.UnixListener, error) {
	if err := ensureFree(path); err != nil {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("socket: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, SocketMode); err != nil {
		l.Close()
		return nil, fmt.Errorf("socket: chmod %s: %w", path, err)
	}
	// RuntimeDirectoryPreserve=restart keeps /run/gvm/<name> across a restart,
	// so the runner has to take its own socket away on the way out rather than
	// leaving one for the next start to reason about.
	l.SetUnlinkOnClose(true)
	return l, nil
}

// ensureFree removes a stale socket and refuses one that is not stale.
//
// The flock on job.lock (section 11.7) is the real guard against two runners
// on one world; this asks the same question of the socket itself, because
// unlinking a live socket would silently take the console away from the runner
// that owns it while leaving both processes convinced they are serving.
func ensureFree(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("socket: stat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("socket: %s exists and is not a socket", path)
	}
	if c, err := net.DialTimeout("unix", path, staleProbeTimeout); err == nil {
		c.Close()
		return fmt.Errorf("socket: %s is already served by a live runner", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("socket: remove stale %s: %w", path, err)
	}
	return nil
}

// PeerIdentifier is the real Identifier: SO_PEERCRED, plus the group set that
// the console tier needs and the kernel does not report.
func PeerIdentifier(c net.Conn) (Identity, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return Identity{}, fmt.Errorf("socket: peer credentials need a unix connection, got %T", c)
	}
	p, err := platform.PeerCred(uc)
	if err != nil {
		return Identity{}, err
	}
	if !p.Identified() {
		return Identity{}, fmt.Errorf("socket: the kernel reported no pid for the peer (%s)", p)
	}
	id := Identity{Peer: p}

	// A uid with no passwd entry, or an unreadable /etc/group, costs this peer
	// the console tier and nothing else: the group set can only ever grant, so
	// failing the lookup fails closed and the connection still gets whatever
	// its uid alone earns it.
	if name, gids, err := platform.PeerGroups(p.UID); err == nil {
		id.Name, id.Groups = name, gids
	}
	return id, nil
}

// DefaultPolicy builds the runner's policy: the process's own uid is the
// instance user, and gvm-console is looked up by name.
//
// It returns a usable Policy together with ErrNoConsoleGroup when the group is
// missing, because a host that has not been prepared yet should still get a
// console for root and for the instance user rather than no console at all.
func DefaultPolicy() (Policy, error) {
	p := Policy{OwnerUID: uint32(os.Getuid())}
	gid, err := platform.GroupID(ConsoleGroup)
	if err != nil {
		return p, fmt.Errorf("%w: %w", ErrNoConsoleGroup, err)
	}
	if gid == 0 {
		// Refusing here rather than silently dropping the tier, because a
		// gvm-console with gid 0 is a misconfigured host, not a missing group.
		return p, ErrConsoleGIDZero
	}
	p.ConsoleGIDs = []uint32{gid}
	return p, nil
}
