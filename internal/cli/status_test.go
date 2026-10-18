package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/ports"
	"github.com/S95F/gameVPSmanager/internal/reconcile"
)

// A created-but-unapplied instance is `declared`, and the hint is the command
// that moves it on. That pairing -- a phase plus the next action -- is what
// makes the table something an operator argues with rather than reads.
func TestStatusReportsDeclaredBeforeTheFirstApply(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")

	code, out, errb := runWith(t, h, "", "status", "valheim-1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, string(lifecycle.PhaseDeclared)) {
		t.Errorf("phase is not `declared`:\n%s", out)
	}
	if !strings.Contains(out, instance.UnitName("valheim-1")) {
		t.Errorf("the report does not name the unit:\n%s", out)
	}
}

// --json is the mirror's own column names, so a dashboard reading this and one
// reading instance_status cannot grow two vocabularies for the same facts.
func TestStatusJSONUsesTheMirrorsFieldNames(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")

	code, out, errb := runWith(t, h, "", "status", "valheim-1", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json did not emit JSON: %v\n%s", err, out)
	}
	for _, k := range []string{"instance", "phase", "phase_rule", "unit", "unit_known", "drift"} {
		if _, ok := got[k]; !ok {
			t.Errorf("--json is missing the %q key: %v", k, got)
		}
	}
}

// `gvm status` after an apply reports no drift; editing the spec afterwards
// makes it report drift, which is section 20's INST-005 and the single most
// common reason a box does not behave the way the YAML says.
func TestStatusReportsSpecDriftAfterAnEdit(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")
	if code, out, errb := runWith(t, h, "", "apply", "valheim-1"); code != 0 {
		t.Fatalf("apply: exit %d\n%s\n%s", code, out, errb)
	}

	_, out, _ := runWith(t, h, "", "status", "valheim-1")
	if strings.Contains(out, "drift") {
		t.Errorf("a freshly applied instance reports drift:\n%s", out)
	}

	// The operator edits the YAML and does not apply. The drop-in still
	// carries the sha256 of the version that WAS applied.
	raw, err := h.FS.ReadFile(instance.PathsFor("valheim-1").Spec)
	if err != nil {
		t.Fatal(err)
	}
	h.FS.put(instance.PathsFor("valheim-1").Spec,
		append(raw, []byte("\n# an operator edited this\n")...), 0o640)

	_, out, _ = runWith(t, h, "", "status", "valheim-1")
	if !strings.Contains(out, "drift") {
		t.Errorf("an edited-and-unapplied spec does not report drift:\n%s", out)
	}
}

// `gvm list` enumerates from /etc and says so when there is nothing, rather
// than printing an empty table an operator has to interpret.
func TestListEnumeratesFromEtc(t *testing.T) {
	h := newTestHost(t)

	code, out, errb := runWith(t, h, "", "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "no instances") {
		t.Errorf("an empty host printed:\n%s", out)
	}

	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=One")
	seedInstance(t, h, "valheim-2", "valheim", "--var", "public_name=Two")

	_, out, _ = runWith(t, h, "", "list")
	for _, want := range []string{"valheim-1", "valheim-2", "valheim"} {
		if !strings.Contains(out, want) {
			t.Errorf("list is missing %q:\n%s", want, out)
		}
	}
	// The summary line follows the rows. Two freshly created, unapplied
	// instances are declared, so they count as stopped, not running.
	if !strings.Contains(out, "needing attention") {
		t.Errorf("the summary line is missing:\n%s", out)
	}
	if !strings.Contains(out, "2 stopped") {
		t.Errorf("two declared instances should read as 2 stopped:\n%s", out)
	}
}

// TestListProblemsOnAHealthyHostSaysSo is day2-ops-14: `list --problems` on a
// host with nothing wrong used to print "no instances", which reads as "this
// box is empty" rather than "this box is fine". It now says so and accounts for
// the servers it checked, and exits 0 so a monitor sees a clean result.
func TestListProblemsOnAHealthyHostSaysSo(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=One")
	seedInstance(t, h, "valheim-2", "valheim", "--var", "public_name=Two")

	code, out, errb := runWith(t, h, "", "list", "--problems")
	if code != 0 {
		t.Fatalf("a host with nothing wrong exited %d, want 0\n%s\n%s", code, out, errb)
	}
	if !strings.Contains(out, "nothing needs attention") {
		t.Errorf("did not say the host is fine:\n%s", out)
	}
	if !strings.Contains(out, "2 instances") {
		t.Errorf("did not account for the servers it checked:\n%s", out)
	}
}

// TestListProblemsListsTheUnhealthyAndExitsFour: a drifting instance is
// something a person has to act on, so `list --problems` shows it, points at
// the two commands that say more, and exits 4 (checked and found something).
func TestListProblemsListsTheUnhealthyAndExitsFour(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")
	if code, _, errb := runWith(t, h, "", "apply", "valheim-1"); code != 0 {
		t.Fatalf("apply: %s", errb)
	}
	// Edit the spec without applying: the drop-in's recorded sha no longer
	// matches, which is drift -- a problem an operator must resolve.
	raw, err := h.FS.ReadFile(instance.PathsFor("valheim-1").Spec)
	if err != nil {
		t.Fatal(err)
	}
	h.FS.put(instance.PathsFor("valheim-1").Spec,
		append(raw, []byte("\n# edited, not applied\n")...), 0o640)

	code, out, errb := runWith(t, h, "", "list", "--problems")
	if code != exitChecked {
		t.Fatalf("exit %d, want %d (checked)\n%s\n%s", code, exitChecked, out, errb)
	}
	if !strings.Contains(out, "valheim-1") {
		t.Errorf("the unhealthy instance is not listed:\n%s", out)
	}
	if !strings.Contains(out, "gvm status") || !strings.Contains(out, "gvm doctor") {
		t.Errorf("the result does not point at the commands that say more:\n%s", out)
	}
}

// `gvm list` takes no instance name, and says which command does. Silently
// ignoring the argument would leave an operator believing they had filtered.
func TestListRefusesAnArgumentAndPointsAtStatus(t *testing.T) {
	h := newTestHost(t)
	code, _, errb := runWith(t, h, "", "list", "valheim-1")
	if code == 0 {
		t.Fatal("gvm list accepted an instance name")
	}
	if !strings.Contains(errb, "gvm status valheim-1") {
		t.Errorf("stderr = %s", errb)
	}
}

// `gvm ports` reports the ephemeral range as ASSUMED when it could not be
// read. Section 6.7 singles out the opposite failure -- treating the range as
// empty -- and presenting an assumption as a measurement is how that failure
// gets made by somebody reading this output.
func TestPortsSaysWhenTheEphemeralRangeWasAssumed(t *testing.T) {
	h := newTestHost(t)
	// Readers that fail, not nil: nil means "use the real platform reader",
	// which on Linux reads the real /proc and measures instead of assuming.
	h.HostEnv.Ephemeral = func() (platform.PortRange, error) {
		return platform.PortRange{}, platform.ErrNotSupported
	}
	h.HostEnv.Listeners = func() ([]ports.Listener, error) {
		return nil, platform.ErrNotSupported
	}

	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=One")

	code, out, errb := runWith(t, h, "", "ports")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "ASSUMED") {
		t.Errorf("the assumed ephemeral range is not disclosed:\n%s", out)
	}
	if !strings.Contains(out, "occupancy is UNKNOWN here, not empty") {
		t.Errorf("an unreadable listener table is not disclosed:\n%s", out)
	}
	// The instance's own ports are still held.
	if !strings.Contains(out, "udp/2456") {
		t.Errorf("valheim-1's ports are not in the table:\n%s", out)
	}
}

// One instance's allocation is re-checked against live occupancy, because
// ports are written literally and never recomputed: nothing else looks.
func TestPortsChecksAMaterialisedAllocation(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=One")

	code, out, errb := runWith(t, h, "", "ports", "valheim-1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "its materialised allocation still holds") {
		t.Errorf("a healthy allocation was not reported as such:\n%s", out)
	}
}

// The enablement observation is what makes apply idempotent, and it must not
// follow the symlink: on a box whose unit template is missing, a following
// stat reports an enabled unit as disabled and every apply re-enables it.
func TestWantsLinkNamesTheEnablementSymlink(t *testing.T) {
	got := reconcile.WantsLink("valheim-1")
	if !strings.HasSuffix(got, "/multi-user.target.wants/"+instance.UnitName("valheim-1")) {
		t.Errorf("WantsLink = %q", got)
	}
}
