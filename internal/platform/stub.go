//go:build !linux

package platform

// Statfs is Linux-only. The arithmetic a preflight does with the result is in
// fs.go with no build tag, so the decision to refuse a destroy for lack of
// space is tested on the development machine even though the measurement is not.
func Statfs(path string) (DiskUsage, error) { return DiskUsage{}, Unsupported("statfs") }

// EphemeralRange is Linux-only. ParsePortRange is not, and it is where the
// parsing lives.
//
// A caller off Linux must not silently treat the range as empty: the allocator
// would then hand out a port the kernel may assign to an outbound connection.
// DefaultEphemeral is the documented substitute.
func EphemeralRange() (PortRange, error) {
	return PortRange{}, Unsupported("ip_local_port_range")
}

// UserExists is Linux-only. Answering from the Windows account database would
// be a different question with the same name.
func UserExists(name string) (bool, error) { return false, Unsupported("unix user lookup") }

// GroupList is Linux-only.
func GroupList(name string) ([]string, error) { return nil, Unsupported("unix group list") }
