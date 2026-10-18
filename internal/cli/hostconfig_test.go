package cli

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// TestCreateRefusesOnInvalidHostConfig: create allocates ports from
// /etc/gvm/gvm.yaml, so a config it cannot trust (a typo'd key YAML would
// silently ignore) stops the create before it hands out a port the operator
// meant to reserve. The refusal names the key and suggests the fix.
func TestCreateRefusesOnInvalidHostConfig(t *testing.T) {
	h := newTestHost(t)
	h.FS.put(spec.EtcDir+"/gvm.yaml",
		[]byte("spec:\n  firewall:\n    reserved_port: [8080]\n"), 0o644)

	code, _, errb := runWith(t, h, "", "create", "valheim-1", "--game", "valheim",
		"--var", "public_name=Test")
	if code != exitRefused {
		t.Errorf("create with a bad gvm.yaml exited %d, want %d (refused):\n%s", code, exitRefused, errb)
	}
	if !strings.Contains(errb, "does not validate") {
		t.Errorf("refusal did not say the config is invalid:\n%s", errb)
	}
	if !strings.Contains(errb, "reserved_ports") {
		t.Errorf("refusal did not carry the did-you-mean for the typo:\n%s", errb)
	}
	// Nothing was written: the refusal is a precondition, before allocation.
	if _, err := h.FS.Stat(instancePath(t, "valheim-1")); err == nil {
		t.Error("a refused create still wrote the instance spec")
	}
}

// TestCreateAcceptsValidHostConfig: a clean (or absent) config does not get in
// the way — the common case stays a one-liner.
func TestCreateAcceptsValidHostConfig(t *testing.T) {
	h := newTestHost(t)
	h.FS.put(spec.EtcDir+"/gvm.yaml",
		[]byte("spec:\n  firewall:\n    reserved_ports: [8080]\n"), 0o644)
	code, _, errb := runWith(t, h, "", "create", "valheim-1", "--game", "valheim",
		"--var", "public_name=Test")
	if code != 0 {
		t.Errorf("create with a valid gvm.yaml exited %d:\n%s", code, errb)
	}
}

func instancePath(t *testing.T, name string) string {
	t.Helper()
	return spec.EtcDir + "/instances/" + name + ".yaml"
}
