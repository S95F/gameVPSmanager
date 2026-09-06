//go:build linux

package archive

import (
	"io/fs"
	"os"
	"syscall"
)

// fileOwner reads the numeric owner of a file.
//
// The uid/gid pair is recorded per member so that `restore
// --preserve-ownership` can put an adopted server's files back exactly as they
// were when the uid still resolves to the same name -- an adopted Project
// Zomboid install is full of files owned by a uid gvm did not create and must
// not renumber.
//
// This uses syscall rather than golang.org/x/sys/unix deliberately: Stat_t is
// in the standard library, so the Linux-only half of this package pulls in no
// extra dependency and internal/platform/deps_test.go has nothing new to
// police.
func fileOwner(fi fs.FileInfo) (uid, gid int) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return int(st.Uid), int(st.Gid)
}

// chownPath restores a recorded owner.
func chownPath(path string, uid, gid int) error { return os.Lchown(path, uid, gid) }
