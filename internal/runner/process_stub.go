//go:build !linux

package runner

import (
	"context"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// unsupportedProcess is what a Process is off Linux: a value that refuses
// every question rather than answering one wrongly.
//
// The refusals matter more than they look. Alive returning (false, nil) here
// would mean "the world is free" on a platform that cannot see a single
// process, and the caller of Alive is the code that decides whether to start a
// second server on a live world. Empty returning (true, nil) would mean "the
// cgroup is clear" and end a shutdown ladder that had killed nothing.
type unsupportedProcess struct{ id Identity }

var _ Process = (*unsupportedProcess)(nil)

func (p *unsupportedProcess) PID() int           { return p.id.PID }
func (p *unsupportedProcess) PGID() int          { return p.id.PGID }
func (p *unsupportedProcess) Identity() Identity { return p.id }

func (p *unsupportedProcess) Alive() (bool, error) {
	return false, platform.Unsupported("process liveness")
}

func (p *unsupportedProcess) Signal(sig platform.Signal, scope Scope) error {
	return platform.Unsupported("kill(2)")
}

func (p *unsupportedProcess) Wait(ctx context.Context) (ExitStatus, error) {
	return ExitStatus{Code: -1}, platform.Unsupported("waitpid(2)")
}

func (p *unsupportedProcess) Empty() (bool, error) {
	return false, platform.Unsupported("cgroup v2")
}

// Adopt is Linux-only: there is no /proc to verify a pinned identity against,
// and adopting one unverified is how the standalone shutdown path would signal
// a stranger.
func Adopt(id Identity) (Process, error) {
	return nil, platform.Unsupported("adopting a pinned process")
}
