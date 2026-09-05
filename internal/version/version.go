// Package version carries build metadata stamped in at link time.
//
// The values are set by -ldflags -X in the Makefile. A binary built with a
// plain `go build` keeps the placeholders below, which is a useful signal in
// a bug report: it means the binary did not come from the build system.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var (
	// Version is the git describe output, e.g. "v0.3.1" or "v0.3.1-4-gabc123-dirty".
	Version = "dev"
	// Commit is the short git SHA.
	Commit = "unknown"
)

// String renders a one-line human-readable version banner.
func String() string {
	return fmt.Sprintf("gvm %s (commit %s, %s, %s/%s)",
		Version, Commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Info returns the structured form, for the API and the dashboard footer.
func Info() map[string]string {
	m := map[string]string{
		"version":   Version,
		"commit":    Commit,
		"goVersion": runtime.Version(),
		"os":        runtime.GOOS,
		"arch":      runtime.GOARCH,
	}
	// A binary built straight from a VCS checkout carries its own metadata,
	// which is more trustworthy than the ldflags when they were not set.
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if Commit == "unknown" {
					m["commit"] = s.Value
				}
			case "vcs.modified":
				m["dirty"] = s.Value
			}
		}
	}
	return m
}
