//go:build linux

package adopt

import (
	"fmt"
	"os/exec"
)

// runProgram executes an external program. This is the ONLY place in the
// adopt package that spawns a process (cutover and rollback only).
func runProgram(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w\n%s", name, args, err, out)
	}
	return nil
}
