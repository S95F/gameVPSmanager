//go:build !linux

package platform

// newHost always fails off Linux.
//
// This stub is what keeps `make check` green on the Windows development
// machine. It ships in the same commit as the Linux side, never "later".
func newHost() (Host, error) { return nil, Unsupported("host operations") }
