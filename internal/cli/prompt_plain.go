//go:build !linux && !windows

package cli

// On platforms gvm neither targets nor develops on, the prompt falls back to
// a plain line read: x/term would pull x/sys/unix into a darwin build, which
// the platform package's dependency gate forbids.

import "os"

func stdinIsTerminal(*os.File) bool { return false }

func readPasswordTerminal(*os.File) (string, bool, error) { return "", false, nil }
