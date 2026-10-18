package cli

import (
	"strings"
	"testing"
)

// TestCreateRefusesUnacceptedEULA: a game behind a EULA (the Minecraft
// manifests) is not created until the operator accepts it themselves. The
// refusal shows the URL and the exact var to set (D-22).
func TestCreateRefusesUnacceptedEULA(t *testing.T) {
	h := newTestHost(t)
	code, _, errb := runWith(t, h, "", "create", "mc1", "--game", "minecraft-paper",
		"--var", "mc_version=1.21.4")
	if code != exitRefused {
		t.Fatalf("create of a EULA game exited %d, want %d (refused):\n%s", code, exitRefused, errb)
	}
	if !strings.Contains(errb, "minecraft.net/eula") {
		t.Errorf("refusal did not show the EULA URL:\n%s", errb)
	}
	if !strings.Contains(errb, "accept_eula=true") {
		t.Errorf("refusal did not name the var to set:\n%s", errb)
	}
	if n := len(h.FS.writes); n != 0 {
		t.Errorf("a refused create wrote %d file(s)", n)
	}
}

// TestCreateAcceptsEULAWhenVarSet: with the acceptance var set true, create
// gets past the gate (it no longer mentions the EULA).
func TestCreateAcceptsEULAWhenVarSet(t *testing.T) {
	h := newTestHost(t)
	code, _, errb := runWith(t, h, "", "create", "mc1", "--game", "minecraft-paper",
		"--var", "mc_version=1.21.4", "--var", "accept_eula=true")
	if strings.Contains(errb, "EULA") || strings.Contains(errb, "eula") {
		t.Errorf("accepting the EULA should get past the gate, but the error mentions it:\n%s", errb)
	}
	if code != 0 {
		t.Fatalf("create with the EULA accepted exited %d:\n%s", code, errb)
	}
}
