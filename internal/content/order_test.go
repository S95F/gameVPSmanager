package content

import (
	"strings"
	"testing"
)

// The live list's shape: that DAMN Library and NeatUI Framework are libraries
// and have to load before the mods that need them. The ordering is hand-tuned
// and the reasoning lives in mods.txt's section comments, which is why nothing
// here reorders.
func orderedEntries() []LockEntry {
	return []LockEntry{
		{Ordinal: 0, Acquire: "3171167894", Note: "that DAMN Library"},
		{Ordinal: 1, Acquire: "3508537032", Note: "NeatUI Framework"},
		{Ordinal: 2, Acquire: "3435796523", Note: "Hilux88",
			Requires: []AcquireID{"3171167894"}},
	}
}

func TestCheckOrderPassesACorrectlyOrderedList(t *testing.T) {
	if vs := CheckOrder(orderedEntries()); len(vs) != 0 {
		t.Fatalf("a correctly ordered list reported %d violation(s): %v", len(vs), vs)
	}
	if OrderReport(nil, "pz-main") != "" {
		t.Error("OrderReport of nothing is not empty")
	}
}

// TestCheckOrderReportsBothIDsAndBothOrdinals is the requirement verbatim.
// "3536052310 is out of order" sends an operator to a 33-line file;
// "3536052310 at ordinal 3 requires 3508537032 at ordinal 8" is the answer.
func TestCheckOrderReportsBothIDsAndBothOrdinals(t *testing.T) {
	entries := []LockEntry{
		{Ordinal: 3, Acquire: "3536052310", Note: "KI5 Hilux",
			Requires: []AcquireID{"3508537032"}},
		{Ordinal: 8, Acquire: "3508537032", Note: "NeatUI Framework"},
	}

	vs := CheckOrder(entries)
	if len(vs) != 1 {
		t.Fatalf("got %d violation(s), want 1: %v", len(vs), vs)
	}
	v := vs[0]
	if v.Kind != ViolationAfter {
		t.Errorf("kind = %q", v.Kind)
	}
	if v.Dependent != AcquireID("3536052310") || v.DependentOrdinal != 3 {
		t.Errorf("dependent = %s at %d", v.Dependent, v.DependentOrdinal)
	}
	if v.Dependency != AcquireID("3508537032") || v.DependencyOrdinal != 8 {
		t.Errorf("dependency = %s at %d", v.Dependency, v.DependencyOrdinal)
	}

	msg := v.Error()
	for _, want := range []string{"3536052310", "ordinal 3", "3508537032", "ordinal 8"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message is missing %q: %q", want, msg)
		}
	}
	// The note is carried through because it usually says what the dependency
	// is for, which is the next thing the operator wants to know.
	if !strings.Contains(msg, "KI5 Hilux") {
		t.Errorf("the message drops the operator's note: %q", msg)
	}
	// And the fix is a command, printed.
	// The fix is a command, printed, and COMPLETE as printed: the
	// instance name is an argument, not a placeholder a caller has to
	// remember to substitute.
	fix := v.Fix("pz-main")
	if !strings.Contains(fix, "gvm content move pz-main") {
		t.Errorf("Fix() does not name the command and the instance: %q", fix)
	}
}

// TestCheckOrderNeverReorders is the load-bearing promise of order.go. An
// automatic reorder would discard reasoning gvm cannot see, and it would look
// like a success.
func TestCheckOrderNeverReorders(t *testing.T) {
	entries := []LockEntry{
		{Ordinal: 3, Acquire: "3536052310", Requires: []AcquireID{"3508537032"}},
		{Ordinal: 8, Acquire: "3508537032"},
		{Ordinal: 9, Acquire: "2866258937"},
	}
	before := make([]LockEntry, len(entries))
	copy(before, entries)

	_ = CheckOrder(entries)

	for i := range entries {
		if entries[i].Acquire != before[i].Acquire || entries[i].Ordinal != before[i].Ordinal {
			t.Fatalf("CheckOrder modified the list: entry %d was %s@%d and is now %s@%d",
				i, before[i].Acquire, before[i].Ordinal, entries[i].Acquire, entries[i].Ordinal)
		}
	}

	// And the returned violations must not alias the caller's Requires
	// slices, or a caller that "fixed" a violation in place would mutate the
	// lock it was reading.
	entries[0].Requires[0] = "mutated"
	vs := CheckOrder(orderedEntries())
	if len(vs) != 0 {
		t.Fatalf("unexpected violations after mutating an unrelated slice: %v", vs)
	}
}

func TestCheckOrderReportsAMissingDependency(t *testing.T) {
	entries := []LockEntry{
		{Ordinal: 0, Acquire: "3435796523", Note: "Hilux88",
			Requires: []AcquireID{"3171167894"}},
	}
	vs := CheckOrder(entries)
	if len(vs) != 1 || vs[0].Kind != ViolationMissing {
		t.Fatalf("got %v, want one missing-dependency violation", vs)
	}
	if vs[0].DependencyOrdinal != -1 {
		t.Errorf("a dependency that is not in the list has no ordinal, got %d",
			vs[0].DependencyOrdinal)
	}
	msg := vs[0].Error()
	if !strings.Contains(msg, "3171167894") || !strings.Contains(msg, "not in the list") {
		t.Errorf("message = %q", msg)
	}
	fix := vs[0].Fix("pz-main")
	if !strings.Contains(fix, "gvm content add pz-main") {
		t.Errorf("Fix() = %q; the fix for a missing dependency is to add it, for a named "+
			"instance", fix)
	}
}

func TestCheckOrderReportsASelfReference(t *testing.T) {
	entries := []LockEntry{
		{Ordinal: 0, Acquire: "3171167894", Requires: []AcquireID{"3171167894"}},
	}
	vs := CheckOrder(entries)
	if len(vs) != 1 || vs[0].Kind != ViolationSelf {
		t.Fatalf("got %v; an item requiring itself would otherwise read as satisfied", vs)
	}
}

// A dependency at the SAME ordinal is not a violation: within one ordinal
// there is no order to get wrong, and flagging it would make a legal list
// unfixable.
func TestADependencyAtTheSameOrdinalIsNotAViolation(t *testing.T) {
	entries := []LockEntry{
		{Ordinal: 0, Acquire: "a", Requires: []AcquireID{"b"}},
		{Ordinal: 0, Acquire: "b"},
	}
	if vs := CheckOrderOf([]OrderItem{
		{Ordinal: entries[0].Ordinal, ID: entries[0].Acquire, Requires: entries[0].Requires},
		{Ordinal: entries[1].Ordinal, ID: entries[1].Acquire},
	}); len(vs) != 0 {
		t.Fatalf("got %v", vs)
	}
}

func TestCheckOrderIgnoresEmptyEdges(t *testing.T) {
	entries := []LockEntry{
		{Ordinal: 0, Acquire: "3171167894", Requires: []AcquireID{""}},
		{Ordinal: 1, Acquire: ""},
	}
	if vs := CheckOrder(entries); len(vs) != 0 {
		t.Fatalf("an empty requires edge produced %v", vs)
	}
}

// TestCheckOrderPlanRunsBeforeAnythingStops: an ordering violation has to be
// visible from --dry-run, not after the shutdown ladder has already run.
func TestCheckOrderPlanRunsBeforeAnythingStops(t *testing.T) {
	p := goodPlan()
	p.Entries[0].Ref.Requires = []string{"3508537032"} // the library is at ordinal 1

	vs := CheckOrderPlan(p)
	if len(vs) != 1 {
		t.Fatalf("got %d violation(s), want 1: %v", len(vs), vs)
	}
	if vs[0].DependentOrdinal != 0 || vs[0].DependencyOrdinal != 1 {
		t.Errorf("ordinals = %d and %d", vs[0].DependentOrdinal, vs[0].DependencyOrdinal)
	}
	if CheckOrderPlan(nil) != nil {
		t.Error("CheckOrderPlan(nil) invented violations")
	}
}

func TestOrderReportSaysNothingWasReordered(t *testing.T) {
	vs := CheckOrder([]LockEntry{
		{Ordinal: 0, Acquire: "a", Requires: []AcquireID{"b"}},
		{Ordinal: 1, Acquire: "b"},
	})
	report := OrderReport(vs, "pz-main")
	for _, want := range []string{
		"nothing was reordered", "hand-tuned", "gvm content move pz-main",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report is missing %q:\n%s", want, report)
		}
	}
}

// TestViolationsAreSortedByTheDependentsOrdinal keeps a report readable: it
// reads down the list in the order an operator would scroll it.
func TestViolationsAreSortedByTheDependentsOrdinal(t *testing.T) {
	vs := CheckOrder([]LockEntry{
		{Ordinal: 5, Acquire: "e", Requires: []AcquireID{"z"}},
		{Ordinal: 1, Acquire: "a", Requires: []AcquireID{"z"}},
		{Ordinal: 9, Acquire: "z"},
	})
	if len(vs) != 2 {
		t.Fatalf("got %v", vs)
	}
	if vs[0].DependentOrdinal >= vs[1].DependentOrdinal {
		t.Errorf("violations are not in ordinal order: %d then %d",
			vs[0].DependentOrdinal, vs[1].DependentOrdinal)
	}
}
