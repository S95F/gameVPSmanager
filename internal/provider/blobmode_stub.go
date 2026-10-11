//go:build !linux

package provider

import "io/fs"

// BlobMode off Linux is 0600 rather than section 5's 0444, and the difference
// is deliberate.
//
// Windows maps the owner write bit onto FILE_ATTRIBUTE_READONLY and then
// refuses to replace a read-only file, so a 0444 blob could not be published
// over: both the self-healing re-Put of a corrupted blob and two jobs
// resolving the same mod at once would fail with "access is denied" on bytes
// that were perfectly good. Linux's rename(2) does not consult the victim's
// mode, so there is nothing to work around on the platform gvm actually serves
// blobs from, and no reason to weaken the mode that protects the shared cache
// there.
//
// gvm never runs a blob store off Linux. This exists so the store's logic is
// testable on the development machine.
const BlobMode fs.FileMode = 0o600
