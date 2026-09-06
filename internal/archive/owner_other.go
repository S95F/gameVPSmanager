//go:build !linux

package archive

import (
	"errors"
	"io/fs"
)

// errNoOwnership is the honest answer off Linux. It is returned rather than
// silently ignored so a restore run on the development machine cannot report
// "ownership preserved" about a filesystem that has no such concept.
var errNoOwnership = errors.New("archive: file ownership is not available on this platform")

// fileOwner has no answer off Linux, and 0/0 is the correct placeholder: an
// archive created on the development machine records root:root, which is what
// a restore onto a Linux box would then apply, and is the mode every path
// under /etc/gvm and /var/backups/gvm already has (D-02, D-44).
func fileOwner(fs.FileInfo) (uid, gid int) { return 0, 0 }

// chownPath refuses off Linux. Restore treats the refusal as a warning, not a
// failure, because extracting an archive on a workstation to look inside it is
// a legitimate thing to do and ownership is not what the operator is checking.
func chownPath(string, int, int) error { return errNoOwnership }
