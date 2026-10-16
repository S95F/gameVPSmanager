package cli

import (
	"strings"
	"testing"
)

// TestGameShow: the offline summary carries the data an operator needs to
// decide how to create — the immutable/apply badge on a var, the secrets it
// references (by name, never a value), and a paste-ready create line.
func TestGameShow(t *testing.T) {
	h := newTestHost(t)
	code, out, errb := runWith(t, h, "", "game", "show", "valheim")
	combined := out + errb
	if code != 0 {
		t.Fatalf("exit %d: %s", code, combined)
	}
	for _, want := range []string{
		"world_name", "immutable", "rebuild",
		"join_password",                         // a referenced secret, by name
		"sudo gvm create <name> --game valheim", // the paste-ready line
		"provider",
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("game show valheim missing %q:\n%s", want, combined)
		}
	}
}

// TestGameShowYAMLRoundTrips: --yaml emits the manifest bytes (DoD: it feeds
// back into `gvm game validate`).
func TestGameShowYAMLRoundTrips(t *testing.T) {
	h := newTestHost(t)
	code, out, _ := runWith(t, h, "", "game", "show", "generic-steam", "--yaml")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "apiVersion:") {
		t.Errorf("--yaml did not emit the manifest bytes:\n%s", out[:min(120, len(out))])
	}
}

// TestGameListHidesTemplates: a template is hidden from the plain listing and
// shown under --all (game-catalog-config-11).
func TestGameListHidesTemplates(t *testing.T) {
	h := newTestHost(t)
	_, plain, _ := runWith(t, h, "", "game", "list")
	if strings.Contains(plain, "generic-steam") {
		t.Errorf("game list showed the template generic-steam:\n%s", plain)
	}
	if !strings.Contains(plain, "template hidden") {
		t.Errorf("game list did not note the hidden template:\n%s", plain)
	}
	_, all, _ := runWith(t, h, "", "game", "list", "--all")
	if !strings.Contains(all, "generic-steam") {
		t.Errorf("game list --all did not show the template:\n%s", all)
	}
}

// TestCreateRefusesTemplate: `gvm create --game <template>` is refused with the
// copy-it-first instruction, not a confusing allocation failure later.
func TestCreateRefusesTemplate(t *testing.T) {
	h := newTestHost(t)
	code, _, errb := runWith(t, h, "", "create", "g1", "--game", "generic-steam")
	if code != exitRefused {
		t.Errorf("create of a template exited %d, want %d (refused):\n%s", code, exitRefused, errb)
	}
	if !strings.Contains(errb, "template") || !strings.Contains(errb, "copy it to /etc/gvm/games") {
		t.Errorf("refusal did not tell the operator to copy the template:\n%s", errb)
	}
}
