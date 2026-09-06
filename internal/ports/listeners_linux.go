//go:build linux

package ports

// ProcRoot is where the kernel's process and network tables live.
const ProcRoot = "/proc"

// HostListeners reads this host's socket tables.
//
// It reads /proc directly and never runs `ss` or `netstat`. Those tools parse
// the same files this does, but through an output format that differs between
// iproute2 versions and distributions, in a subprocess that can block on a name
// lookup, with a PATH that decides which binary answers. A wrong answer here
// hands a live port to a new instance, so the shortest path to the kernel's own
// data is the only acceptable one.
func HostListeners() ([]Listener, error) {
	return CollectListeners(NewDirProcSource(ProcRoot))
}
