// Package version carries build metadata stamped in at link time.
//
// The values are set by -ldflags -X in the Makefile. A binary built with a
// plain `go build` has no ldflags, so instead of reporting "commit unknown" —
// which tells a bug report nothing — it falls back to the VCS metadata the Go
// toolchain already embeds (vcs.revision, vcs.modified). A release build's
// ldflags still win, because a tag is more precise than a bare revision.
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

// resolved is the effective version and commit after the VCS fallback, so
// String and Info never disagree about what this binary is.
func resolved() (version, commit string, dirty bool) {
	version, commit = Version, Commit

	// Only consult the build info when ldflags left a placeholder: a stamped
	// release must report its tag and short SHA, not the raw revision.
	needCommit := commit == "unknown"
	if !needCommit {
		return version, commit, false
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			commit = rev
			if dirty {
				commit += "-dirty"
			}
		}
	}
	return version, commit, dirty
}

// String renders a one-line human-readable version banner. A plain `go build`
// binary prints its VCS revision here, never "commit unknown".
func String() string {
	version, commit, _ := resolved()
	return fmt.Sprintf("gvm %s (commit %s, %s, %s/%s)",
		version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Info returns the structured form, for the API and the dashboard footer.
func Info() map[string]string {
	version, commit, dirty := resolved()
	m := map[string]string{
		"version":   version,
		"commit":    commit,
		"goVersion": runtime.Version(),
		"os":        runtime.GOOS,
		"arch":      runtime.GOARCH,
	}
	if dirty {
		m["dirty"] = "true"
	}
	return m
}
