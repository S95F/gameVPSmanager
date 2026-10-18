package nft

import (
	"strings"
	"testing"
)

// TestNftIsOffForNow is Phase 40 item 2. The nft backend is kept compiling but
// unavailable: gvm ships ufw first, and a host must never silently land on a
// backend the firewall surface has not been exercised against. Available
// reports false regardless of whether an nft binary is installed, and the
// message names the backends that do work.
func TestNftIsOffForNow(t *testing.T) {
	ok, msg := Backend{}.Available()
	if ok {
		t.Fatalf("the nft backend reports available; it is off for now")
	}
	if !strings.Contains(msg, "not supported") {
		t.Errorf("message = %q, want it to say the backend is not supported", msg)
	}
	for _, want := range []string{"ufw", "none"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want it to name the %q backend", msg, want)
		}
	}
}
