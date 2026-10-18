package cli

import (
	"testing"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/backup"
	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/runner"
)

// TestFrozenFormats is usability principle U17: from Phase 20 until the N-1
// skew rule lands in Phase 35, the on-disk and on-socket formats are frozen.
//
// The hazard it guards is gvm-self-upgrade-uninstall-2: the first format bump
// breaks `gvm stop` and the backup freeze on every world that has not been
// restarted onto the new binary, because an old runner cannot read a new
// status.json, lease or shutdown plan. Additive JSON fields are fine (readers
// ignore unknown keys); a version bump is not, until a reader exists that
// accepts the previous version. This test is the reminder: bump a format here
// and the build goes red with the sentence saying to land the cross-version
// reader (Phase 35) first.
func TestFrozenFormats(t *testing.T) {
	frozen := []struct {
		name string
		got  int
	}{
		{"console.Proto", console.Proto},
		{"lifecycle.LeaseProto", lifecycle.LeaseProto},
		{"backup.LeaseSchema (the freeze lease)", backup.LeaseSchema},
		{"runner.StatusProto", runner.StatusProto},
		{"runner.RunnerFileProto", runner.RunnerFileProto},
		{"runner.PlanProto (the shutdown plan)", runner.PlanProto},
		{"archive.SchemaVersion (manifest and index)", archive.SchemaVersion},
	}
	const want = 1
	for _, f := range frozen {
		if f.got != want {
			t.Errorf("%s = %d, want %d: formats are frozen until the N-1 skew rule (Phase 35, U17). "+
				"Bumping one now breaks `gvm stop` and backups on every un-restarted world after an "+
				"upgrade; land the cross-version reader first.", f.name, f.got, want)
		}
	}
}
