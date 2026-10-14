//go:build linux

package adopt

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

type osDiscoverEnv struct{ root string }

// OSDiscoverEnv returns a DiscoverEnv backed by the real OS filesystem.
// root is the prefix prepended to every path (use "" for a live system).
func OSDiscoverEnv(root string) DiscoverEnv { return &osDiscoverEnv{root: root} }

func (e *osDiscoverEnv) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(filepath.Join(e.root, path))
}

func (e *osDiscoverEnv) ReadDir(path string) ([]fs.DirEntry, error) {
	return os.ReadDir(filepath.Join(e.root, path))
}

func (e *osDiscoverEnv) Stat(path string) (fs.FileInfo, error) {
	return os.Stat(filepath.Join(e.root, path))
}

// UserProcesses lists all processes owned by username by scanning /proc.
func (e *osDiscoverEnv) UserProcesses(username string) ([]ProcInfo, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("lookup user %q: %w", username, err)
	}
	uid := u.Uid

	procRoot := filepath.Join(e.root, "proc")
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var out []ProcInfo
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(de.Name())
		if err != nil {
			continue
		}
		statusPath := filepath.Join(procRoot, de.Name(), "status")
		statusData, err := os.ReadFile(statusPath)
		if err != nil {
			continue
		}
		if !procStatusMatchesUID(statusData, uid) {
			continue
		}
		cmdPath := filepath.Join(procRoot, de.Name(), "cmdline")
		cmdData, _ := os.ReadFile(cmdPath)
		cmdline := strings.ReplaceAll(string(cmdData), "\x00", " ")
		out = append(out, ProcInfo{PID: pid, Cmdline: strings.TrimSpace(cmdline)})
	}
	return out, nil
}

func procStatusMatchesUID(data []byte, uid string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == uid {
				return true
			}
		}
	}
	return false
}
