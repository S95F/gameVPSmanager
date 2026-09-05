//go:build linux

package platform

import (
	"fmt"
	"net"
)

// notifySend writes one sd_notify datagram.
//
// A fresh socket per call rather than a cached one: notifications are rare
// (readiness, a status change, a timeout extension every 30 s), an unconnected
// datagram socket is cheap, and holding one open across the fork that spawns
// the game would leak a manager-owned fd into the game's process.
func notifySend(addr, state string) error {
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("platform: sd_notify dial: %w", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(state)); err != nil {
		return fmt.Errorf("platform: sd_notify write: %w", err)
	}
	return nil
}
