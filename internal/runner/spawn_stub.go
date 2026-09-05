//go:build !linux

package runner

import "github.com/S95F/gameVPSmanager/internal/platform"

// spawn refuses off Linux, with no partial credit.
//
// A "good enough" spawn here -- os/exec with no setsid, no pty and no identity
// pin -- would let the development machine run tests that appear to prove the
// supervisor works, while proving nothing about the one host that matters: no
// process group to signal, no start time to pin against pid reuse, and no
// hangup to end the pump. Everything above this line is tested on Windows
// against a fake process instead, which is honest about what it covers.
func spawn(l Launch) (*Spawned, error) {
	return nil, platform.Unsupported("spawning a game process")
}
