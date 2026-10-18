//go:build linux || windows

package cli

// The no-echo password prompt, on the two platforms gvm is developed and
// deployed on. x/term pulls x/sys/unix on darwin, which the platform package's
// dependency gate forbids outside Linux builds, so the import lives behind
// this tag and prompt_plain.go covers everything else.

import (
	"os"

	"golang.org/x/term"
)

// stdinIsTerminal reports whether f is an interactive terminal.
func stdinIsTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// readPasswordTerminal reads without echo. handled is false when f is not a
// terminal and the caller should read a plain line instead.
func readPasswordTerminal(f *os.File) (value string, handled bool, err error) {
	if !term.IsTerminal(int(f.Fd())) {
		return "", false, nil
	}
	b, err := term.ReadPassword(int(f.Fd()))
	return string(b), true, err
}
