//go:build linux

package backup

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// btrfsListSnaps returns the snapshot directories under dir whose names match
// the pattern this package writes: <instance>-<YYYYMMDD>-<HHMMSS>.snap.
//
// It is a companion to SweepSnapDir for the btrfs path: read-only btrfs
// subvolumes require BTRFS_IOC_SNAP_DESTROY_V2 to remove (see
// snapshot_linux.go) and cannot be deleted with os.RemoveAll. Listing them,
// however, is filesystem-agnostic: the names are the gate, not the file type.
func btrfsListSnaps(dir, instance string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pfx := instance + "-"
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, pfx) || !strings.HasSuffix(name, ".snap") {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimPrefix(name, pfx), ".snap")
		if _, ok := parseSnapStem(stem); ok {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}

// parseSnapStem parses the YYYYMMDD-HHMMSS timestamp embedded in a snapshot
// directory name.
//
// The format is the same one ClonePathFor uses when naming a clone directory,
// so the sweep and the writer always agree on which names are ours.
func parseSnapStem(stem string) (time.Time, bool) {
	t, err := time.ParseInLocation("20060102-150405", stem, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
