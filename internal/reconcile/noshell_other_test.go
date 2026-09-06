//go:build !linux

package reconcile

// candidateLists is empty off Linux: there are no program paths here to check,
// because there are no programs. The STATIC half of the no-shell check -- the
// import scan and the string-literal scan -- still runs everywhere, and it is
// the half that catches a new program being added.
func candidateLists() [][]string { return nil }
