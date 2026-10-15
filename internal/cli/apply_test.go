package cli

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/reconcile"
)

// TestApplyRefusesAnOOMProneHeap: a hand edit that raises the heap var but
// leaves spec.resources.memory_max unchanged arms the OOM killer mid-save
// (game-catalog-config-4, rule 4). apply must refuse, naming both numbers and
// the field to raise, and change nothing.
func TestApplyRefusesAnOOMProneHeap(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "pz1", "projectzomboid", "--var", "public_name=Test")

	// Raise the heap to 9g while leaving the memory_max that create computed for
	// 5g. 9g heap + 3G native_headroom now exceeds the recorded memory_max.
	in := mustReadInstance(t, h, "pz1")
	in.Spec.Vars["jvm_xmx"] = "9g"
	raw, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	h.FS.put(instance.PathsFor("pz1").Spec, raw, 0o640)

	before := len(h.FS.writes)
	code, _, errb := runWith(t, h, "", "apply", "pz1", "--diff")
	if code == 0 {
		t.Fatal("apply accepted an OOM-prone heap")
	}
	for _, want := range []string{"memory_max", "jvm_xmx", "OUTSIDE the Java heap"} {
		if !strings.Contains(errb, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, errb)
		}
	}
	if len(h.FS.writes) != before {
		t.Errorf("a refused apply wrote %d file(s)", len(h.FS.writes)-before)
	}
}

// TestApplyRefusesAnAgeIdentityInTheSpec: an age identity (private key) pasted
// into the instance YAML is a hard error — it would be copied into every
// archive (offsite-backup-dr-3). apply refuses and names the fix.
func TestApplyRefusesAnAgeIdentityInTheSpec(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "pz1", "projectzomboid", "--var", "public_name=Test")

	p := instance.PathsFor("pz1").Spec
	raw, err := h.FS.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	const id = "AGE-SECRET-KEY-1QQPQZRFR7ZZ2WCVQYE2WEXHZW6MM3FK3RT8T8FR0WX8U9YQ0X3ZSABCDEF"
	h.FS.put(p, append(raw, []byte("\n# leaked: "+id+"\n")...), 0o640)

	code, _, errb := runWith(t, h, "", "apply", "pz1", "--diff")
	if code == 0 {
		t.Fatal("apply accepted an age identity in the spec")
	}
	if !strings.Contains(errb, "age identity") || !strings.Contains(errb, "age1") {
		t.Errorf("the refusal does not name the fix (paste the age1 recipient):\n%s", errb)
	}
}

// `gvm apply --diff` prints the plan and changes nothing.
//
// "Changes nothing" is asserted against the filesystem and against systemd,
// not against the output, because the only useful form of the promise is the
// one an operator can rely on after they have run it.
func TestApplyDiffPrintsThePlanAndChangesNothing(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")
	writesAfterCreate := len(h.FS.writes)

	code, out, errb := runWith(t, h, "", "apply", "valheim-1", "--diff")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errb)
	}
	if len(h.FS.writes) != writesAfterCreate {
		t.Errorf("--diff wrote %v", h.FS.writes[writesAfterCreate:])
	}
	if len(h.FS.dirs) != 0 || len(h.Systemd.calls) != 0 {
		t.Errorf("--diff touched the host: dirs=%v systemd=%v", h.FS.dirs, h.Systemd.calls)
	}

	// The section 17.6 shape: a header naming the instance and its pinned
	// manifest, sigil-led step lines, and a closing count that says plainly
	// that nothing happened.
	for _, want := range []string{
		"instance valheim-1", "valheim rev", "pinned",
		"+ dir /etc/gvm/instances/valheim-1.d",
		// `gvm create` already wrote the definition, so apply reports it as
		// unchanged rather than re-writing it. "unchanged" being a first-class
		// outcome is the whole reason the sigil column exists.
		"= file /etc/gvm/instances/valheim-1.yaml",
		"+ user gvm-valheim-1",
		"+ dropin " + instance.DropInPath("valheim-1"),
		"already correct", "nothing has been applied",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--diff output is missing %q:\n%s", want, out)
		}
	}
	// A diff never runs the write-probe, and must say so rather than showing
	// a sandbox that looks checked.
	if !strings.Contains(out, "write-probe was NOT run") {
		t.Errorf("--diff does not disclaim the unrun write-probe:\n%s", out)
	}
}

// Idempotence: apply converges, and a second apply reports zero actions.
//
// This is the property that makes `gvm apply` safe to type when you do not
// know what state a box is in. A plan that always shows work -- a reload
// nothing needed, a re-enable of an already-enabled unit -- trains an operator
// to skim, and the apply that mattered is the one they skim past.
func TestASecondApplyReportsZeroActions(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")

	code, out, errb := runWith(t, h, "", "apply", "valheim-1")
	if code != 0 {
		t.Fatalf("first apply: exit %d\n%s\n%s", code, out, errb)
	}
	if !strings.Contains(out, "changed") {
		t.Fatalf("the first apply reported no summary:\n%s", out)
	}
	// It converged: the drop-in, the pin and the account are all in place.
	for _, p := range []string{
		instance.DropInPath("valheim-1"),
		instance.PathsFor("valheim-1").ManifestLock,
	} {
		if _, err := h.FS.Stat(p); err != nil {
			t.Errorf("apply did not write %s", p)
		}
	}
	if !h.Users.have[instance.UserName("valheim-1")] {
		t.Error("apply did not create the instance account")
	}

	// systemd records enablement as a wants symlink; the fake manager does
	// not create one, so the test does what the real manager would.
	h.FS.put(reconcile.WantsLink("valheim-1"), nil, 0o777)

	pl, err := buildApply(t.Context(), h.HostEnv, Transport{Mode: ModeLocalRoot}, "valheim-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if changes := pl.Plan.Changes(); len(changes) != 0 {
		for _, s := range changes {
			t.Errorf("a converged host still plans: %s %s %s", s.Change, s.Kind, s.Object)
		}
	}
}

// The write-probe runs BEFORE the drop-in, and a plan that got that wrong is
// refused rather than executed (D-15).
func TestTheApplyPlanProbesBeforeItWritesTheDropIn(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")

	pl, err := buildApply(t.Context(), h.HostEnv, Transport{Mode: ModeLocalRoot}, "valheim-1", true)
	if err != nil {
		t.Fatal(err)
	}
	probeAt, dropInAt := -1, -1
	for i, s := range pl.Plan.Describe() {
		switch s.Kind {
		case reconcile.KindProbe:
			probeAt = i
		case reconcile.KindDropIn:
			dropInAt = i
		}
	}
	if probeAt < 0 || dropInAt < 0 {
		t.Fatalf("plan has probe=%d dropin=%d", probeAt, dropInAt)
	}
	if probeAt > dropInAt {
		t.Errorf("the drop-in (step %d) is written before the write-probe (step %d)",
			dropInAt, probeAt)
	}
}

// An apply against an instance whose YAML does not exist fails by naming the
// file, not by producing an empty plan.
func TestApplyNamesTheMissingDefinition(t *testing.T) {
	h := newTestHost(t)
	code, _, errb := runWith(t, h, "", "apply", "nope-1")
	if code == 0 {
		t.Fatal("apply succeeded for an instance that does not exist")
	}
	if !strings.Contains(errb, instance.PathsFor("nope-1").Spec) {
		t.Errorf("the error does not name the file: %s", errb)
	}
}

// Every verb refuses cleanly off a Linux host when nothing is injected. The
// developer machine must never be able to half-apply into C:\etc\gvm.
func TestTheHostSeamsRefuseOffLinux(t *testing.T) {
	if isLinux() {
		t.Skip("this asserts the refusal that only exists off Linux")
	}
	for _, args := range [][]string{
		{"apply", "valheim-1"}, {"create", "v1", "--game", "valheim", "--var", "public_name=x"},
		{"start", "valheim-1"}, {"stop", "valheim-1"}, {"restart", "valheim-1"},
		{"status", "valheim-1"}, {"list"}, {"logs", "valheim-1"}, {"ports"},
		{"install", "valheim-1"}, {"destroy", "valheim-1", "--archive", "x.tar.gz"},
	} {
		code, _, errb := runWith(t, nil, "", args...)
		if code == 0 {
			t.Errorf("%v: exit 0 on a non-Linux host", args)
		}
		if !strings.Contains(errb, "not supported on this operating system") {
			t.Errorf("%v: stderr = %q, want platform.ErrNotSupported", args, errb)
		}
	}
}

// TestAConvergedApplyWritesNothingEvenWhenTheClockHasMoved.
//
// The idempotence assertion above holds at the PLAN level, and it holds with a
// frozen clock. Neither is the property that matters on a real box. The
// generated units carry a `# rendered <timestamp>` line: planFile and PlanTimer
// deliberately ignore it (SameDropIn / stripRendered) and report Unchanged,
// while FileAction.Do and TimerAction.Do issue the write unconditionally and
// hostops decides with an exact bytes.Equal. In production h.now() is
// time.Now(), so those two relations disagree on exactly the generated files,
// every single time.
//
// The consequence is what hostops' own comment says the design exists to
// prevent: a no-op apply gives the drop-in a new mtime and inode, systemd then
// reports NeedDaemonReload for an instance nothing changed about, and the audit
// row records a file.write for a step the operator was shown as "=".
func TestAConvergedApplyWritesNothingEvenWhenTheClockHasMoved(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")

	if code, out, errb := runWith(t, h, "", "apply", "valheim-1"); code != 0 {
		t.Fatalf("first apply: exit %d\n%s\n%s", code, out, errb)
	}
	h.FS.put(reconcile.WantsLink("valheim-1"), nil, 0o777)

	// The clock moves, as it does between any two real applies.
	h.HostEnv.Now = func() time.Time { return testTime.Add(90 * time.Second) }
	before := len(h.FS.writes)

	code, out, errb := runWith(t, h, "", "apply", "valheim-1")
	if code != 0 {
		t.Fatalf("second apply: exit %d\n%s\n%s", code, out, errb)
	}
	if got := h.FS.writes[before:]; len(got) != 0 {
		t.Fatalf("a converged apply rewrote %v.\nThe planner and hostops must reach the same "+
			"verdict: an Unchanged step has to issue a byte-identical write, or the operator is "+
			"shown '=' for a file that just got a new mtime.\n%s", got, out)
	}
}
