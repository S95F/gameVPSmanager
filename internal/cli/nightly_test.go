package cli

import (
	"strings"
	"testing"
)

// TestModdedUpdateNightlyWrittenAsRestart: a game with mods (Project Zomboid)
// cannot run an `update` nightly in this build, and PZ's nightly exists for the
// restart that clears heap pressure — so create writes it as `restart` and
// says why, rather than a nightly that would fail every night.
func TestModdedUpdateNightlyWrittenAsRestart(t *testing.T) {
	h := newTestHost(t)
	code, out, errb := runWith(t, h, "", "create", "pz1", "--game", "projectzomboid",
		"--var", "public_name=Test")
	if code != 0 {
		t.Fatalf("create pz1 exited %d:\n%s", code, errb)
	}
	in := mustReadInstance(t, h, "pz1")
	j, ok := in.Spec.Jobs["nightly"]
	if !ok {
		t.Fatal("pz1 has no nightly job")
	}
	if j.Action != "restart" {
		t.Errorf("pz1 nightly action = %q, want restart (update needs the mod engine)", j.Action)
	}
	if !strings.Contains(out, "restart through the ladder") || !strings.Contains(out, "mods") {
		t.Errorf("create did not explain the restart nightly:\n%s", out)
	}
}

// TestContentFreeGameKeepsUpdateDecision: the rewrite keys on a non-empty
// content block, so a content-free game (valheim: content: []) is not treated
// as modded.
func TestContentFreeGameKeepsUpdateDecision(t *testing.T) {
	cat := gameCatalog()
	load := func(id string) *struct{ modded bool } {
		cands, _, err := cat.Resolve(id, overrideAuth(false))
		if err != nil {
			t.Fatalf("resolve %s: %v", id, err)
		}
		for i := range cands {
			if cands[i].Winner {
				return &struct{ modded bool }{moddedUpdateUnavailable(cands[i].Manifest)}
			}
		}
		t.Fatalf("no winner for %s", id)
		return nil
	}
	if load("valheim").modded {
		t.Error("valheim (content: []) must not be treated as modded")
	}
	if !load("projectzomboid").modded {
		t.Error("projectzomboid (declares content) must be treated as modded")
	}
}
