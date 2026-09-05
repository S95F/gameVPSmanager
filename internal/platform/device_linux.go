//go:build linux

package platform

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// SameDevice reports whether two existing paths sit on the same filesystem.
//
// The destroy jail asks this because a prefix check alone cannot see a mount.
// A world bind-mounted, NFS-mounted or symlink-farmed into the instance tree
// passes every lexical test and is still not the jail's to delete: rm -rf
// follows the mount and takes the other filesystem with it. Comparing st_dev
// is the only test that distinguishes "inside the tree" from "reachable
// through the tree".
//
// Stat, not Lstat: the caller has already resolved symlinks, and the question
// is about the target that would actually be removed.
func SameDevice(a, b string) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return false, fmt.Errorf("platform: stat %s: %w", a, err)
	}
	if err := unix.Stat(b, &sb); err != nil {
		return false, fmt.Errorf("platform: stat %s: %w", b, err)
	}
	return sa.Dev == sb.Dev, nil
}
