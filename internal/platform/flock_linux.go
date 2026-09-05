//go:build linux

package platform

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Flock takes an exclusive advisory lock on path, creating it if absent, and
// never blocks: a blocking acquire inside a systemd start job would hang the
// unit past TimeoutStartSec with no diagnosis, where ErrLocked names the
// situation immediately.
//
// flock(2) rather than fcntl(2) locking, because an flock lock belongs to the
// open file description: it survives across the runner's exec, it is released
// by the kernel if the process dies for any reason including SIGKILL, and it
// is not silently dropped when some unrelated code path closes a second
// descriptor on the same file.
func Flock(path string) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("platform: flock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("platform: flock %s: %w", path, err)
	}
	return &FileLock{path: path, f: f}, nil
}

// Unlock releases the lock. The file itself is deliberately left on disk:
// unlinking a lock file lets the next two acquirers open two different inodes
// and both succeed, which is the classic way an advisory lock stops working
// months after someone added the tidy-up.
func (l *FileLock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	// Close alone would release the lock; LOCK_UN first so the release is
	// explicit and a Close error cannot be mistaken for a still-held lock.
	uerr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	cerr := f.Close()
	if uerr != nil {
		return fmt.Errorf("platform: unlock %s: %w", l.path, uerr)
	}
	if cerr != nil {
		return fmt.Errorf("platform: unlock %s: %w", l.path, cerr)
	}
	return nil
}
