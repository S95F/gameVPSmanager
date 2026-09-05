//go:build !linux

package platform

// SameDevice is Linux-only: st_dev has no portable equivalent that means the
// same thing, and guessing from a drive letter would answer a different
// question with the same name.
//
// Callers must not read the error as "same device". The destroy jail treats
// ErrNotSupported as "this assertion could not be made here" and keeps every
// other check, which is honest on a development machine and irrelevant in
// production, where gvm only ever destroys anything on Linux.
func SameDevice(a, b string) (bool, error) { return false, Unsupported("same-device check") }
