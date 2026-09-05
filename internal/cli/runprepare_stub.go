//go:build !linux

package cli

import "github.com/S95F/gameVPSmanager/internal/platform"

// applyPrepare has no meaning off Linux: there is no /run/gvm, no setgid bit on
// a directory, and no gvm-console group. It refuses rather than pretending, so
// a developer who reaches it gets the project's one answer for "this runs on
// the deployment target only".
func applyPrepare(prepareTarget) error { return platform.Unsupported("gvm run-prepare") }
