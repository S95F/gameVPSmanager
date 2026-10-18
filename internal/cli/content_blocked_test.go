package cli

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/content/curseforge"
)

// TestBlockedReportNamesTheSharedDropBox: the `gvm content blocked` report tells
// the operator where to drop the jars by the one exported constant, not a
// literal of its own, so it can never name a different directory from the one
// the resolver scans.
func TestBlockedReportNamesTheSharedDropBox(t *testing.T) {
	var b strings.Builder
	v := &contentView{Name: "mc2", Schema: &content.Schema{Source: content.KindCurseForge}}
	rep := blockedReport{
		Block: "mods",
		Files: []blockedFile{{
			Name: "waystones.jar", Project: "waystones", Size: 1024,
			URL: "https://www.curseforge.com/minecraft/mc-mods/waystones",
		}},
	}
	printBlockedReport(&b, v, rep)

	out := b.String()
	if !strings.Contains(out, curseforge.DropBoxDir) {
		t.Errorf("the report does not name the shared drop-box %q:\n%s", curseforge.DropBoxDir, out)
	}
}
