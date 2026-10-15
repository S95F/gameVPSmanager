//go:build !linux

package hostinit

import (
	"context"
	"errors"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// Off Linux there is no systemd to install into, so NewSystem refuses with the
// project's one sentinel — the same contract `gvm console`, `gvm run` and the
// btrfs path use, which is what lets the CLI, a developer and the test suite
// all recognise it.
func TestNewSystemRefusesOffLinux(t *testing.T) {
	_, _, err := NewSystem(context.Background())
	if !errors.Is(err, platform.ErrNotSupported) {
		t.Errorf("NewSystem off Linux: err = %v, want platform.ErrNotSupported", err)
	}
}
