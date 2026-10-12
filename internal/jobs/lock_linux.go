//go:build linux

package jobs

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// sysFlock is flock(2), which is the lock D-30 chose.
//
// flock and not fcntl/POSIX locks, and not a lock file created with O_EXCL:
//
//   - O_EXCL leaves a lock behind when the holder is SIGKILLed, so every
//     nightly after the first failure refuses to run. The whole point of this
//     subsystem is that unattended failures recover.
//   - A POSIX record lock (F_SETLK) is released by closing ANY descriptor the
//     process has on the file, which makes an unrelated `ReadHolder` in the
//     same process drop the lock it was reporting on.
//   - flock is held by the open file description, released by the kernel when
//     the last descriptor closes -- including when the process dies for any
//     reason -- and is what `flock(1)` and systemd's own tooling use, so an
//     operator can take the same lock from a shell.
//
// NFS is not a concern: every path this locks is on local disk, under /run
// (tmpfs) or /var/lib/gvm.
type sysFlock struct{}

func (sysFlock) TryLock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EWOULDBLOCK):
		// EWOULDBLOCK and EAGAIN are the same number on Linux, and this is
		// the contended case rather than a failure: another holder has it.
		return ErrLocked
	default:
		return fmt.Errorf("flock %s: %w", f.Name(), err)
	}
}

func (sysFlock) Unlock(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		return fmt.Errorf("flock -u %s: %w", f.Name(), err)
	}
	return nil
}
