//go:build !linux

package platform

// Flock is unavailable off Linux. There is no partial credit here: a lock that
// pretends to succeed would let a test on the development machine "prove"
// world-safety that the deployment host is the only place actually enforcing.
func Flock(path string) (*FileLock, error) { return nil, Unsupported("flock") }

// Unlock is a no-op because Flock never returns a lock on this platform.
func (l *FileLock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	return Unsupported("flock")
}
