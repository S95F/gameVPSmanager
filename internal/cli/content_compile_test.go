package cli

import (
	"testing"

	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// TestEveryShippedContentBlockCompiles runs content.Compile on every content
// block of every shipped manifest (U3, phase A of content-mods-1).
//
// A shipped block that does not compile is a game gvm offers whose content
// management would fail the moment an operator ran `gvm content list` against
// it — and before Phase 20 the three Minecraft manifests were exactly that, so
// `openContent` turned a mis-declared block into four schema errors. The fix is
// a block that compiles (activation by presence) plus a one-sentence refusal in
// `openContent`; this test is the gate that keeps the block compiling.
func TestEveryShippedContentBlockCompiles(t *testing.T) {
	cat := gameCatalog()
	ids, err := cat.List()
	if err != nil {
		t.Fatalf("catalog list: %v", err)
	}
	for _, id := range ids {
		m, _, lerr := cat.Load(id, spec.DenyOverrides{})
		if lerr != nil {
			t.Errorf("%s: load: %v", id, lerr)
			continue
		}
		for _, block := range m.Spec.Content {
			if _, cerr := content.Compile(block, spec.ContentList{}, block.Roots); cerr != nil {
				t.Errorf("%s block %q does not compile: %v", id, block.Name, cerr)
			}
		}
	}
}
