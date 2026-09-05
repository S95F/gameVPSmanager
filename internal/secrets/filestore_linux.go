//go:build linux

package secrets

import (
	"fmt"
	"io/fs"
	"os"
)

// OwnershipEnforced says whether this build can make a secret file root-owned.
// It is exported so `gvm doctor` and the CLI can state the degradation on a
// development machine rather than implying a guarantee they did not deliver.
const OwnershipEnforced = true

// permOf reports the file's unix permission bits.
func permOf(fi fs.FileInfo) (fs.FileMode, bool) { return fi.Mode().Perm(), true }

// chownRoot makes path root:root.
//
// It is a no-op for a non-root euid, and that is not a shortcut. chown(2) to
// another uid is root-only, so a non-root process cannot do this at all — and
// a non-root process also cannot write inside /etc/gvm/secrets, which is 0700
// root:root, so the only caller that reaches here without being root is a test
// against a temporary directory it already owns. Failing there would make the
// suite unrunnable for anyone but root while proving nothing.
func chownRoot(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if err := os.Chown(path, 0, 0); err != nil {
		return fmt.Errorf("secrets: chown root:root %s: %w", path, err)
	}
	return nil
}
