//go:build !linux

package ports

import "github.com/S95F/gameVPSmanager/internal/platform"

// ProcRoot has no meaning off Linux; it is declared so that code referring to
// it compiles here and fails at the call, where the error can say why.
const ProcRoot = ""

// HostListeners is Linux-only.
//
// It returns an error rather than an empty slice. An empty slice would mean
// "nothing is listening on this host", the allocator would believe it, and
// every test on the development machine would then prove that a port is free
// when the deployment target says otherwise. CollectListeners and
// ParseProcNet are untagged and are where the logic is actually tested, against
// the fixtures in testdata.
func HostListeners() ([]Listener, error) {
	return nil, platform.Unsupported("/proc/net listener scan")
}
