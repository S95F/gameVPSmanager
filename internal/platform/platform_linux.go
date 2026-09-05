//go:build linux

package platform

// newHost returns the Linux host implementation.
//
// The concrete implementation lands in phase 3; until then Linux and Windows
// agree that host operations are unavailable, so nothing can accidentally
// depend on a half-built systemd driver.
func newHost() (Host, error) { return nil, Unsupported("host operations") }
