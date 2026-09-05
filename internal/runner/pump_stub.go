//go:build !linux

package runner

import (
	"errors"
	"io"
	"os"
)

// endOfStream is the non-Linux half of the pty hangup rule.
//
// There is no pty here and therefore no EIO to recognise: a Go pipe on Windows
// reports a closed write end as io.EOF. The predicate still exists so the pump
// loop itself is one implementation compiled everywhere, which is what lets
// pump_test.go assert the crash-tail ordering on the development machine.
func endOfStream(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed)
}
