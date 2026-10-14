//go:build !linux

package adopt

import (
	"io/fs"
	"os"
	"path/filepath"
)

type osDiscoverEnv struct{ root string }

// OSDiscoverEnv returns a DiscoverEnv backed by the real OS filesystem rooted
// at root. On non-Linux platforms it cannot enumerate processes.
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

// UserProcesses is not supported on non-Linux platforms.
func (e *osDiscoverEnv) UserProcesses(_ string) ([]ProcInfo, error) { return nil, nil }
