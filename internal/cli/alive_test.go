package cli

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// TestALiveHolderIsNeverReportedGone.
//
// The asymmetry lifecycle.Lease.Classify is built on: evidence that the holder
// is GONE is required to call a lease stale, and absence of evidence leaves it
// live. Getting it backwards lets a second updater into an install tree the
// first one still has open, and -- since the guard sweep recovers a
// stale-leased instance -- would have gvm-guard.timer start a server a LIVE
// job had just stopped on purpose.
//
// The mutation this catches is the one that shipped: answering through
// platform.Alive(pid, 0). Alive compares /proc field 22 against the pin it was
// handed, and lifecycle.Lease has no pin, so a zero read every live process as
// (false, nil) -- provably dead. The pure half of the decision is asserted
// here; the half that needs a real /proc is alive_linux_test.go.
func TestALiveHolderIsNeverReportedGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want lifecycle.Tri
	}{
		{"the stat read succeeded", nil, lifecycle.Yes},
		{"there is no such pid", &fs.PathError{Op: "open", Err: fs.ErrNotExist}, lifecycle.No},
		{"no /proc on this platform", platform.Unsupported("/proc/<pid>/stat"), lifecycle.Unknown},
		{"not permitted to look", &fs.PathError{Op: "open", Err: fs.ErrPermission},
			lifecycle.Unknown},
		{"the stat line would not parse", errors.New("field 22 is not a number"),
			lifecycle.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := aliveFrom(tc.err); got != tc.want {
				t.Errorf("aliveFrom(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	// A lease with no pid recorded is not evidence of anything.
	if got := aliveTri(0); got != lifecycle.Unknown {
		t.Errorf("aliveTri(0) = %v, want Unknown", got)
	}
	if got := aliveTri(-1); got != lifecycle.Unknown {
		t.Errorf("aliveTri(-1) = %v, want Unknown", got)
	}
}
