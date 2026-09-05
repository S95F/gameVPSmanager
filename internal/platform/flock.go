package platform

import (
	"errors"
	"os"
)

// ErrLocked reports that another process already holds the lock. It is a
// distinct sentinel because the caller's response differs: a held job.lock
// means a runner is already supervising this world and this process must exit
// 75 so systemd retries, while an I/O failure means the host is broken.
var ErrLocked = errors.New("platform: lock is held by another process")

// FileLock is an advisory whole-file lock, held for as long as the descriptor
// is open. It is what stops a second server from opening the same world
// (section 11.7, lock 1 of 3) and what serialises steamcmd host-wide (D-30).
type FileLock struct {
	path string
	f    *os.File
}

// Path is the lock file this lock is held on.
func (l *FileLock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}
