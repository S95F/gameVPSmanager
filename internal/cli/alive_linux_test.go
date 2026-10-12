//go:build linux

package cli

import (
	"os"
	"os/exec"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// TestAliveTriAgainstRealProcesses is the half of the asymmetry that needs a
// /proc, and it is the one the shipped mutation failed: platform.Alive(pid, 0)
// returned No for THIS process, so every live job's lease classified stale.
//
// The portable half of the decision is in alive_test.go, which is what runs on
// the development machine.
func TestAliveTriAgainstRealProcesses(t *testing.T) {
	if got := aliveTri(os.Getpid()); got != lifecycle.Yes {
		t.Errorf("aliveTri(self) = %v, want Yes; a lease held by a running job must not read "+
			"as stale, or the guard starts a server that job deliberately stopped", got)
	}

	// A pid that is definitely gone: a reaped child.
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start a throwaway process: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	if got := aliveTri(pid); got != lifecycle.No {
		t.Errorf("aliveTri(reaped pid %d) = %v, want No; a lease whose holder is provably gone "+
			"is what `gvm job clear` and the recovery sweep act on", pid, got)
	}
}
