package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/avail"
)

// TestReadmeUnbuiltMatchesRegistry is the CI diff for the README's "What works
// in this build" table (U1, U16): the "Not available in this build" line is
// derived from the command registry, so the table cannot drift from what the
// binary actually refuses. A phase that flips a row — or a new unbuilt command —
// turns this red until the README is updated to match.
func TestReadmeUnbuiltMatchesRegistry(t *testing.T) {
	var unbuilt []string
	for _, c := range commands {
		if c.Hidden {
			continue // gvmd and web are refused but not listed, like in `gvm help`
		}
		if c.Avail.State == avail.NotInBuild {
			unbuilt = append(unbuilt, "`"+c.Name+"`")
		}
	}
	sort.Strings(unbuilt)
	want := "**Not available in this build:** " + strings.Join(unbuilt, ", ") + "."

	readme := filepath.Join(repoRoot(t), "README.md")
	raw, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), want) {
		t.Errorf("README.md does not contain the exact line derived from the registry:\n  %s\n"+
			"Update the \"What works in this build\" table so it matches the NotInBuild rows.", want)
	}
}
