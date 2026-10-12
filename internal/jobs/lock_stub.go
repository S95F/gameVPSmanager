//go:build !linux

package jobs

import (
	"os"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// sysFlock off Linux REFUSES, and returning nil here would be the worst
// available answer.
//
// The failure a job lock prevents is two SteamCMD runs writing one install
// tree, and the only recovery from that is a restore. A stub that pretended to
// lock would make the development machine the one platform where that is
// possible -- and it would do it silently, in the one code path whose entire
// purpose is to be noisy about contention.
//
// The logic around the syscall is deliberately NOT tagged: creating the
// directory, writing and truncating the holder line, and the diagnostic that
// names who is blocking are all portable, and lock_test.go exercises every one
// of them here by injecting a Flock. That is the whole reason Flock is a seam
// rather than this file being the whole of Acquire.
type sysFlock struct{}

func (sysFlock) TryLock(*os.File) error { return platform.Unsupported("flock(2)") }

func (sysFlock) Unlock(*os.File) error { return platform.Unsupported("flock(2)") }
