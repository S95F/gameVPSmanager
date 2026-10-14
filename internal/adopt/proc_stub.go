//go:build !linux

package adopt

import "fmt"

// runProgram is not supported on non-Linux platforms; cutover requires Linux.
func runProgram(name string, args ...string) error {
	return fmt.Errorf("runProgram: not supported on this platform (requires Linux)")
}
