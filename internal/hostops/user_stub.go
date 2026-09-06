//go:build !linux

package hostops

import "github.com/S95F/gameVPSmanager/internal/platform"

// newUsers refuses off Linux.
//
// The Windows account database answers a different question with the same
// name: there is no /etc/passwd, no system uid range, no nologin shell and no
// notion of the per-instance group D-10 depends on. Answering from it would
// produce a platform.User that looks valid and means nothing.
//
// The logic in user.go is not gated by this -- the seams in systemUsers are
// what user_test.go drives, so "never modify an existing account" is asserted
// on the development machine and not only on a VPS.
func newUsers() (platform.Users, error) { return nil, platform.Unsupported("unix accounts") }
