//go:build !linux

package platform

// The parsing these functions wrap lives in proc.go with no build tag, so the
// field-22 trap, the pgid guard and the cgroup path arithmetic are all tested
// on the development machine. Only the /proc reads and kill(2) stop here.

// ReadStat is Linux-only: there is no /proc.
func ReadStat(pid int) (Stat, error) { return Stat{}, Unsupported("/proc/<pid>/stat") }

// StartTicks is Linux-only.
func StartTicks(pid int) (uint64, error) { return 0, Unsupported("/proc/<pid>/stat") }

// Alive is Linux-only. It reports false with an error rather than false with
// none, so a caller that ignores the error cannot conclude a live game is dead.
func Alive(pid int, wantStartTicks uint64) (bool, error) {
	return false, Unsupported("process liveness")
}

// SignalGroup is Linux-only: Windows has no process groups in kill(2)'s sense.
func SignalGroup(pgid int, sig Signal) error { return Unsupported("kill(-pgid)") }

// SelfCgroup is Linux-only.
func SelfCgroup() (string, error) { return "", Unsupported("cgroup v2") }

// CgroupProcs is Linux-only.
func CgroupProcs(rel string) ([]int, error) { return nil, Unsupported("cgroup v2") }
