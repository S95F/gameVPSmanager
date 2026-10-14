package adopt

import "io/fs"

// DiscoverEnv abstracts all read-only filesystem and process access used
// during discovery. Every method is read-only by design so that
// TestDiscoverIsReadOnly can assert zero state-changing calls.
type DiscoverEnv interface {
	// ReadFile reads the named file. Path is absolute (e.g. /etc/systemd/…).
	ReadFile(path string) ([]byte, error)
	// ReadDir lists the named directory.
	ReadDir(path string) ([]fs.DirEntry, error)
	// Stat returns file information.
	Stat(path string) (fs.FileInfo, error)
	// UserProcesses returns ProcInfo for all processes owned by username.
	UserProcesses(username string) ([]ProcInfo, error)
}

// ProcInfo describes a running process.
type ProcInfo struct {
	PID     int
	Cmdline string
}
