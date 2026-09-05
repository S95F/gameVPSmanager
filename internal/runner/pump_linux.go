//go:build linux

package runner

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// endOfStream reports whether err is a normal end of the child's output.
//
// EIO is the one that matters and the one everybody gets wrong. Reading a pty
// master after the last slave descriptor closes returns EIO on Linux, not
// io.EOF -- that is simply how the tty layer reports a hangup. A pump that
// treats it as a failure logs "input/output error" and drops whatever it had
// buffered, which is the crash tail of a server that just died: the exact
// thirty lines the operator needs.
//
// os.ErrClosed is here for the shutdown path: if the runner's own Close races
// a blocked read, the read fails with "file already closed", and that is the
// runner tidying up rather than the game misbehaving.
func endOfStream(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, unix.EIO) || errors.Is(err, os.ErrClosed)
}
