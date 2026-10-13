// Package listeners enumerates TCP listening sockets by process name.
// It is purely computational: no exec.Command, no iproute2, no ss(1).
// The implementation lives in linux.go and stub.go.
package listeners

// Entry is one listening socket.
type Entry struct {
	Port int
	PID  int
	Comm string // process name (up to 15 bytes, from /proc/<pid>/comm)
}

// ByName returns all TCP listening ports held by processes whose comm name
// matches any entry in names. An empty names slice returns all listeners.
func ByName(names []string) ([]Entry, error) { return byName(names) }
