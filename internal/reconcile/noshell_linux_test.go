//go:build linux

package reconcile

// candidateLists is where the runtime half of the no-shell check gets its
// input. The lists only exist on Linux, because the programs only exist there.
func candidateLists() [][]string {
	return [][]string{systemdAnalyzePaths, systemdRunPaths}
}
