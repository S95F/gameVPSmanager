//go:build !linux

package secrets

import "io/fs"

// OwnershipEnforced is false here. Every other Linux-only surface in gvm
// returns platform.Unsupported from its stub; this one degrades instead,
// deliberately, because the alternative is a store that cannot be exercised at
// all on the machine development happens on — and the logic worth testing (the
// layout, the name rules, the atomic write, the newline trim, the refusal of a
// group-readable master key as a predicate over mode bits) is all portable.
//
// The degradation is exported rather than hidden so a caller can say so.
const OwnershipEnforced = false

// permOf cannot report unix permission bits here, and says so rather than
// guessing.
//
// os.FileInfo.Mode on Windows synthesises 0666, or 0444 for a read-only file,
// out of the read-only attribute; it has nothing to do with who can read the
// file. Checking that against 0o077 would refuse every file on the box for a
// reason that does not exist, and forcing it to 0600 would report a
// restriction Windows never applied. The second value is the honest answer,
// and MasterKey.ModeChecked carries it out to the caller.
func permOf(fi fs.FileInfo) (fs.FileMode, bool) { return 0, false }

// chownRoot is a no-op. There is no root here, and os.Chown returns an error
// unconditionally on Windows, so attempting it would fail every write for a
// reason unrelated to the write.
func chownRoot(path string) error { return nil }
