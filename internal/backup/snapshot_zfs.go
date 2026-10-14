//go:build linux

package backup

import "os"

// zfsMagic is the statfs(2) f_type for a ZFS-on-Linux dataset.
//
// The constant does not appear in golang.org/x/sys/unix as of this writing,
// so it is spelled out with its derivation rather than imported from an opaque
// header. The same pattern is used for btrfsIocSnapCreateV2.
//
//	linux/include/uapi/linux/magic.h: ZFS_SUPER_MAGIC 0x2FC12FC1
const zfsMagic = int64(0x2FC12FC1)

// ZFSAvailable reports whether the ZFS device node is present on this host.
//
// The presence of /dev/zfs is necessary but not sufficient for a running ZFS
// pool; it means only that the ZFS kernel modules are loaded. Probe (via
// statfs in snapshot_linux.go) determines whether a specific path is actually
// on a ZFS dataset.
//
// This function is a host-level heuristic for `gvm doctor` and is not on the
// backup hot path.
func ZFSAvailable() bool {
	_, err := os.Stat("/dev/zfs")
	return err == nil
}
