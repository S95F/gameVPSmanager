package content

import (
	"fmt"
	"sort"
	"strings"
)

// ViolationKind is what is wrong with a declared requires: edge.
type ViolationKind string

const (
	// ViolationAfter is a dependent that loads BEFORE its dependency. That
	// DAMN Library at ordinal 8 and a KI5 vehicle mod at ordinal 3 is red Lua
	// errors on boot.
	ViolationAfter ViolationKind = "loads_before_its_dependency"
	// ViolationMissing is a dependency that is not in the list at all.
	ViolationMissing ViolationKind = "dependency_not_declared"
	// ViolationSelf is an item requiring itself, which is a typo in the
	// operator's requires: line and would otherwise read as satisfied.
	ViolationSelf ViolationKind = "requires_itself"
)

// OrderViolation is one broken requires: edge.
//
// It carries BOTH ids and BOTH ordinals, always. "3536052310 is out of order"
// sends an operator to a 33-line file to work out what it was supposed to be
// after; "3536052310 at ordinal 3 requires 3508537032 at ordinal 8" is the
// whole answer, and the note field explains why the dependency exists at all.
type OrderViolation struct {
	Kind ViolationKind

	// Dependent is the item that declared the requires:.
	Dependent        AcquireID
	DependentOrdinal int
	// DependentNote is the operator's comment on the dependent, carried
	// through because it usually says what the dependency is for.
	DependentNote string

	// Dependency is the id that was required.
	Dependency AcquireID
	// DependencyOrdinal is -1 when the dependency is not in the list, which is
	// the one case where there is no second ordinal to report.
	DependencyOrdinal int
	DependencyNote    string
}

// Error renders the violation as the sentence an operator reads.
func (v OrderViolation) Error() string {
	switch v.Kind {
	case ViolationMissing:
		return fmt.Sprintf("%s (ordinal %d) requires %s, which is not in the list%s",
			v.Dependent, v.DependentOrdinal, v.Dependency, noteSuffix(v.DependentNote))
	case ViolationSelf:
		return fmt.Sprintf("%s (ordinal %d) requires itself%s",
			v.Dependent, v.DependentOrdinal, noteSuffix(v.DependentNote))
	default:
		return fmt.Sprintf("%s (ordinal %d) requires %s (ordinal %d), which loads after it%s",
			v.Dependent, v.DependentOrdinal, v.Dependency, v.DependencyOrdinal,
			noteSuffix(v.DependentNote))
	}
}

// Fix is the command that corrects this violation. It is PRINTED, never run:
// see CheckOrder.
//
// The instance name is an ARGUMENT and not a `<inst>` placeholder the caller
// substitutes later. This is a command an operator pastes, so it has to be
// complete as printed -- and a placeholder is only ever as good as the least
// careful of its call sites. There were two: one did a ReplaceAll over the
// rendered report and the other, feeding the `gvm content diff` rows and the
// --json payload, printed the placeholder verbatim.
func (v OrderViolation) Fix(inst string) string {
	switch v.Kind {
	case ViolationMissing:
		return fmt.Sprintf("gvm content add %s %s --before %s", inst, v.Dependency, v.Dependent)
	case ViolationSelf:
		return fmt.Sprintf("gvm content add %s %s --requires ''  # drop the self-reference",
			inst, v.Dependent)
	default:
		return fmt.Sprintf("gvm content move %s %s --before %s", inst, v.Dependency, v.Dependent)
	}
}

func noteSuffix(note string) string {
	if note = strings.TrimSpace(note); note == "" {
		return ""
	}
	return " [" + note + "]"
}

// OrderItem is the minimum CheckOrder needs: a position, an id, the declared
// edges and the note that explains them.
//
// It exists so the same check serves a plan (before anything is locked) and a
// lock (on every later verify) without one of them growing its own copy of the
// comparison -- and a second copy is how "both ordinals" becomes "one ordinal"
// in one of the two messages.
type OrderItem struct {
	Ordinal  int
	ID       AcquireID
	Requires []AcquireID
	Note     string
}

// CheckOrder enforces every declared Requires edge against the ordinal order.
//
// It NEVER reorders on its own. Load order is hand-tuned -- libraries first,
// that DAMN Library and NeatUI Framework before their dependents -- and the
// section comments in mods.txt record reasoning that gvm cannot see: which mod
// broke which other mod, on which build, in which order someone found worked.
// An automatic reorder would discard all of that silently and look like a
// success. So this reports violations and lets `gvm content move` fix them,
// which keeps the operator holding the one piece of knowledge they have and
// gvm does not.
//
// The input slice is not modified, and the violations do not alias it.
func CheckOrder(entries []LockEntry) []OrderViolation {
	items := make([]OrderItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, OrderItem{
			Ordinal:  e.Ordinal,
			ID:       e.Acquire,
			Requires: append([]AcquireID(nil), e.Requires...),
			Note:     e.Note,
		})
	}
	return CheckOrderOf(items)
}

// CheckOrderPlan is CheckOrder over a plan, for `--dry-run`: an ordering
// violation should be visible before anything stops, not after.
func CheckOrderPlan(p *Plan) []OrderViolation {
	if p == nil {
		return nil
	}
	items := make([]OrderItem, 0, len(p.Entries))
	for _, e := range p.Entries {
		items = append(items, OrderItem{
			Ordinal:  e.Ordinal,
			ID:       e.AcquireID(),
			Requires: RefRequires(e.Ref),
			Note:     e.Ref.Note,
		})
	}
	return CheckOrderOf(items)
}

// CheckOrderOf is the shared comparison. See CheckOrder.
func CheckOrderOf(items []OrderItem) []OrderViolation {
	ordinal := make(map[AcquireID]int, len(items))
	note := make(map[AcquireID]string, len(items))
	for _, it := range items {
		if it.ID == "" {
			continue
		}
		// First occurrence wins, matching the activation de-duplication: if a
		// list somehow carries an id twice, the earlier position is the one
		// that decides whether a dependency loads in time.
		if _, seen := ordinal[it.ID]; !seen {
			ordinal[it.ID] = it.Ordinal
			note[it.ID] = it.Note
		}
	}

	// Sorted by the dependent's ordinal so a report reads down the file in the
	// order an operator would scroll it, rather than in map order.
	sorted := append([]OrderItem(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Ordinal < sorted[j].Ordinal })

	var out []OrderViolation
	for _, it := range sorted {
		if it.ID == "" {
			continue
		}
		for _, req := range it.Requires {
			if req == "" {
				continue
			}
			if req == it.ID {
				out = append(out, OrderViolation{
					Kind:              ViolationSelf,
					Dependent:         it.ID,
					DependentOrdinal:  it.Ordinal,
					DependentNote:     it.Note,
					Dependency:        req,
					DependencyOrdinal: it.Ordinal,
					DependencyNote:    it.Note,
				})
				continue
			}
			depOrd, ok := ordinal[req]
			if !ok {
				out = append(out, OrderViolation{
					Kind:              ViolationMissing,
					Dependent:         it.ID,
					DependentOrdinal:  it.Ordinal,
					DependentNote:     it.Note,
					Dependency:        req,
					DependencyOrdinal: -1,
				})
				continue
			}
			if depOrd > it.Ordinal {
				out = append(out, OrderViolation{
					Kind:              ViolationAfter,
					Dependent:         it.ID,
					DependentOrdinal:  it.Ordinal,
					DependentNote:     it.Note,
					Dependency:        req,
					DependencyOrdinal: depOrd,
					DependencyNote:    note[req],
				})
			}
		}
	}
	return out
}

// OrderReport renders violations for the CLI and the job log, one per line
// with its fix underneath.
func OrderReport(vs []OrderViolation, inst string) string {
	if len(vs) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d load-order violation(s); nothing was reordered:", len(vs))
	for _, v := range vs {
		b.WriteString("\n  ! ")
		b.WriteString(v.Error())
		b.WriteString("\n      ")
		b.WriteString(v.Fix(inst))
	}
	b.WriteString("\n  load order is hand-tuned and its reasoning lives in the list's own " +
		"comments, so gvm reports the conflict instead of resolving it")
	return b.String()
}
