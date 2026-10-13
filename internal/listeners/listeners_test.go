package listeners_test

import (
	"testing"

	"github.com/S95F/gameVPSmanager/internal/listeners"
)

func TestByNameDoesNotPanic(t *testing.T) {
	// We can't guarantee any process names on a test machine, but it must not panic.
	_, err := listeners.ByName([]string{"nonexistent-process-name"})
	if err != nil {
		t.Logf("ByName error (may be expected off-Linux): %v", err)
	}
}

func TestByNameEmptyReturnsAll(t *testing.T) {
	// An empty name list returns all listeners on Linux, nil elsewhere.
	// Must not panic on any platform.
	_, err := listeners.ByName(nil)
	if err != nil {
		t.Logf("ByName(nil) error: %v", err)
	}
}
