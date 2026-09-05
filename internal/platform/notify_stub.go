//go:build !linux

package platform

// notifySend cannot run off Linux: unixgram sockets and the abstract namespace
// are both Linux-only. The formatting half of sd_notify is in notify.go with no
// build tag, which is where all of its logic lives and where it is tested.
func notifySend(addr, state string) error { return Unsupported("sd_notify") }
