package content

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Unaccounted is one thing in the declared set that the resolver could not
// classify.
//
// Not an error and not a warning: a record. It carries the id, why it could
// not be accounted for, and what the operator can do, because the whole point
// of a fail-closed gate is that the refusal is actionable. A gate that said
// only "refused" would be a gate operators disable.
type Unaccounted struct {
	// ID is the acquisition id, when there is one. An unaccounted record with
	// no id is the arithmetic failure: the counts did not add up, which is the
	// case that catches a bad parse rather than a bad item.
	ID     AcquireID `json:"id,omitempty"`
	Reason string    `json:"reason"`
	Hint   string    `json:"hint,omitempty"`
}

func (u Unaccounted) String() string {
	var b strings.Builder
	if u.ID != "" {
		b.WriteString(string(u.ID))
		b.WriteString(": ")
	}
	b.WriteString(u.Reason)
	if u.Hint != "" {
		b.WriteString(" (")
		b.WriteString(u.Hint)
		b.WriteString(")")
	}
	return b.String()
}

// Change is one edit between the declared set and what is locked or observed.
type Change struct {
	Kind ChangeKind `json:"kind"`
	ID   AcquireID  `json:"id"`
	// From and To describe the move: ordinals for a reorder, pins for a repin,
	// activation ids for an activation change.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Unmaterialized marks a change that introduces an item whose bytes are
	// not on disk. On a server_boot source that escalates the whole plan to
	// ImpactDownloadCycle, because nothing but the server itself will fetch
	// them.
	Unmaterialized bool `json:"unmaterialized,omitempty"`
	// WorldData marks an item the manifest declares as world-affecting:
	// Minecraft mods that add dimensions and blocks. Removing one escalates to
	// ImpactRebuild and requires typed confirmation. PZ workshop items
	// generally are not, which is why this is per item and not a blanket
	// scare.
	WorldData bool   `json:"world_data,omitempty"`
	Why       string `json:"why,omitempty"`
}

func (c Change) String() string {
	s := string(c.Kind) + " " + string(c.ID)
	if c.From != "" || c.To != "" {
		s += fmt.Sprintf(" (%s -> %s)", c.From, c.To)
	}
	if c.Why != "" {
		s += ": " + c.Why
	}
	return s
}

// PlanEntry is one declared item, resolved. One per Ref, in DECLARED ORDER,
// because that order is load order and this package never reorders it.
type PlanEntry struct {
	// Ordinal is the position in the declared list. It is recorded rather than
	// implied by the slice index so that a filtered or sorted view of a plan
	// still reports the real load position -- which is the number an operator
	// needs when a requires: edge is violated.
	Ordinal int
	Ref     Ref

	// Activation is this item's activation ids. A SLICE, not a string: one
	// workshop item legitimately contains several mods (sync-mods.sh's `multi`
	// status), and anything that models it one-to-one is wrong on day one.
	Activation []ActivationID
	// Dropped are ids rejected as lower-scoring build variants, kept so a
	// surprising exclusion is explainable months later.
	Dropped []ActivationID

	Resolution ResolutionPath
	Artifact   *Artifact
	Upstream   *Upstream
	// Evidence is why this item resolved the way it did:
	// "modinfo:versionMin>=42", "tags:Build 42", "tags:conflict". It is what
	// sync-mods.sh's "skipped non-B42: …" line was reaching for.
	Evidence []string
	// Ambiguous is NOT Unaccounted. An ambiguous item has no build marker, so
	// every id inside it is enabled WITH A WARNING -- today's behaviour,
	// preserved exactly -- and flagged. An unaccounted item aborts. Two
	// different failures, two different responses.
	Ambiguous bool
	// Materialized is "the bytes are on disk". False on a server_boot source
	// means an acquisition boot is needed.
	Materialized bool
}

// AcquireID is this entry's id in the acquisition namespace.
func (e PlanEntry) AcquireID() AcquireID { return RefID(e.Ref) }

// Plan is what Resolve produced: the declared set, classified.
//
// It is read-only intent. Nothing in a plan has been written anywhere, which
// is what makes `gvm content sync --dry-run` and `gvm content diff` safe
// against a live server with players online.
type Plan struct {
	Block  string
	Source SourceKind

	// Declared is how many Refs Resolve was handed. It is stored rather than
	// recomputed because it is one half of the accounting cross-check, and a
	// count derived from the same slice as the entries could never disagree
	// with them -- which would make the check decorative.
	Declared int

	Entries []PlanEntry
	Changes []Change

	// Unaccounted is the FAIL-CLOSED GATE. See CheckAccounted.
	Unaccounted []Unaccounted

	// Boot is "an acquisition boot is needed": at least one item on a
	// server_boot source is not materialized. It is SKIPPED entirely when
	// Observe already finds every item, which is most nights (D-43).
	Boot bool
	// Bytes is the download figure, when the source knows it.
	Bytes int64
	// Steps are the lines `gvm content sync --dry-run` prints.
	Steps []string
	// Warnings are the findings that need a human but must not stop a sync:
	// section 12.5's "file says 42, Workshop says 41", a Build 41-only item on
	// a Build 42 server. They are MESSAGES -- nothing in this package branches
	// on them, and the refusals all live in Unaccounted instead, so a warning
	// can never become a silent gate.
	Warnings []string
	// Observed is the local reading this resolve was computed against, kept so
	// a three-way diff (declared / locked / observed) needs one call.
	Observed *Observed
	// Locked is the entry count of the lock this plan was compared against,
	// for the diff's header line.
	Locked int

	// impact is the computed plan impact. Unexported with an accessor, so it
	// cannot be assigned past ComputeImpact's two structural escalations: a
	// caller that set it directly could downgrade a rebuild to a restart,
	// which is an archive that never gets taken.
	impact Impact
}

// Impact is how far this plan has to go. It is the maximum over the plan's
// changes plus two structural escalations; see ComputeImpact.
func (p *Plan) Impact() Impact {
	if p == nil {
		return ImpactNone
	}
	return p.impact
}

// Empty reports the normal nightly answer: nothing to do. A first-class
// success that must not page anyone (D-43).
func (p *Plan) Empty() bool { return p == nil || len(p.Changes) == 0 }

// Entry returns the plan entry for one acquisition id.
func (p *Plan) Entry(id AcquireID) (PlanEntry, bool) {
	if p == nil {
		return PlanEntry{}, false
	}
	for _, e := range p.Entries {
		if e.AcquireID() == id {
			return e, true
		}
	}
	return PlanEntry{}, false
}

// AcquireIDs is the acquisition line this plan implies, in declared order.
//
// keepMissing true keeps an item that has not downloaded listed, so the server
// retries it on the next boot. That is not an oversight: dropping it means the
// item is never requested again, so it is never downloaded, so the next
// resolve fails identically -- a recoverable state turned into a stall.
func (p *Plan) AcquireIDs(keepMissing bool) []AcquireID {
	if p == nil {
		return nil
	}
	out := make([]AcquireID, 0, len(p.Entries))
	seen := make(map[AcquireID]bool, len(p.Entries))
	for _, e := range p.Entries {
		id := e.AcquireID()
		if id == "" || seen[id] {
			continue
		}
		if !e.Materialized && !keepMissing {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// ActivationIDs is the activation line this plan implies, in declared order,
// de-duplicated by first occurrence.
func (p *Plan) ActivationIDs() []ActivationID {
	if p == nil {
		return nil
	}
	var out []ActivationID
	for _, e := range p.Entries {
		out = append(out, e.Activation...)
	}
	return dedupeActivate(out)
}

// Ambiguous lists the items enabled with a warning. Reported, never refused.
func (p *Plan) Ambiguous() []AcquireID {
	if p == nil {
		return nil
	}
	var out []AcquireID
	for _, e := range p.Entries {
		if e.Ambiguous {
			out = append(out, e.AcquireID())
		}
	}
	return out
}

// Unmaterialized lists the items whose bytes are not on disk.
func (p *Plan) Unmaterialized() []AcquireID {
	if p == nil {
		return nil
	}
	var out []AcquireID
	for _, e := range p.Entries {
		if !e.Materialized {
			out = append(out, e.AcquireID())
		}
	}
	return out
}

// CheckAccounted is THE GATE. Materialize's first statement is a call to it,
// and so is Activate's (through the lock's copy of the same check).
//
// Two conditions, both fail-closed:
//
//  1. Nothing is recorded as unaccounted.
//  2. The arithmetic adds up: every declared Ref produced either an entry or
//     an unaccounted record, and no more entries than there were Refs.
//
// Condition 2 is rebuild.sh's RAW_COUNT != COUNT check, generalised. That
// script counted the ids it had parsed against the ids in the raw file and
// refused to rewrite mods.txt when the two disagreed, leaving the file
// untouched -- because a partial list and a deliberate list are the same
// bytes, and the server would boot happily on either one. The same logic
// applies here: a resolver that silently returned 31 entries for 33 declared
// items has produced a plausible-looking plan that disables two mods, and
// nothing downstream can tell.
func (p *Plan) CheckAccounted() error {
	if p == nil {
		return fmt.Errorf("%w: there is no plan", ErrUnaccounted)
	}
	if n := len(p.Unaccounted); n > 0 {
		return fmt.Errorf("%w: %s", ErrUnaccounted,
			unaccountedDetail(p.Block, n, p.Declared, p.Unaccounted))
	}
	if got := len(p.Entries); got != p.Declared {
		// The counts disagreeing with no unaccounted record to explain it is
		// worse than an unaccounted item: it means the resolver lost track of
		// its own input, so nothing it reported can be trusted either.
		return fmt.Errorf("%w: block %q resolved %d entries from %d declared item(s) and "+
			"recorded no reason for the difference; this is a resolver bug and the lists were "+
			"left as they are", ErrUnaccounted, p.Block, got, p.Declared)
	}
	return nil
}

// Accounted reports the gate's answer as a boolean, for a report that wants to
// print a status rather than an error.
func (p *Plan) Accounted() bool { return p.CheckAccounted() == nil }

// Validate is the structural check a plan must pass before it can be acted on
// at all. CheckAccounted is the policy gate; this is the well-formedness one.
func (p *Plan) Validate() error {
	if p == nil {
		return fmt.Errorf("%w: there is no plan", ErrUnaccounted)
	}
	var errs []error
	if strings.TrimSpace(p.Block) == "" {
		errs = append(errs, fmt.Errorf("%w: a plan belongs to a named content block",
			ErrInvalidSchema))
	}
	if !p.Source.Valid() {
		errs = append(errs, fmt.Errorf("%w: %q is not a content source", ErrInvalidSchema,
			p.Source))
	}
	if p.Declared < 0 {
		errs = append(errs, fmt.Errorf("%w: Declared is %d", ErrInvalidSchema, p.Declared))
	}
	seen := make(map[AcquireID]int, len(p.Entries))
	for i, e := range p.Entries {
		id := e.AcquireID()
		if id == "" {
			errs = append(errs, fmt.Errorf("%w: entry[%d] has no acquisition id",
				ErrInvalidSchema, i))
			continue
		}
		if first, dup := seen[id]; dup {
			// Two entries for one id makes the derived activation list depend
			// on which one is read, and the two can carry different activation
			// ids.
			errs = append(errs, fmt.Errorf("%w: entry[%d] and entry[%d] are both %s",
				ErrInvalidSchema, first, i, id))
			continue
		}
		seen[id] = i
		if e.Resolution != "" && !e.Resolution.Valid() {
			errs = append(errs, fmt.Errorf("%w: entry %s resolved by %q, which is not a "+
				"resolution path", ErrInvalidSchema, id, e.Resolution))
		}
	}
	for i, u := range p.Unaccounted {
		if strings.TrimSpace(u.Reason) == "" {
			errs = append(errs, fmt.Errorf("%w: unaccounted[%d] carries no reason; a refusal "+
				"nobody can act on is a refusal operators route around", ErrInvalidSchema, i))
		}
	}
	return errors.Join(errs...)
}

// ComputeImpact folds the plan's changes into one impact and records it.
//
// The manifest's `impact:` map supplies the per-kind baseline, and two
// structural rules escalate on top of it (section 12.6):
//
//   - Any change introducing an unmaterialized item on a server_boot source
//     becomes ImpactDownloadCycle. The bytes are not there yet, and only the
//     server can fetch them.
//   - Removing an item the manifest declares world-affecting becomes
//     ImpactRebuild, which means archive-before-destroy and typed
//     confirmation.
//
// Structural, not declarable: a manifest cannot opt out of either one, because
// both describe what is physically true rather than what the author intended.
func (p *Plan) ComputeImpact(sc *Schema) Impact {
	if p == nil {
		return ImpactNone
	}
	imp := ImpactNone
	for i, c := range p.Changes {
		step := sc.ImpactFor(c.Kind)
		if c.Unmaterialized && sc.ServerBoot() {
			step = step.Max(ImpactDownloadCycle)
			p.Changes[i].Why = appendWhy(p.Changes[i].Why,
				"the item's bytes are not on disk, so an acquisition boot is needed")
		}
		if c.Kind == ChangeRemove && c.WorldData {
			step = step.Max(ImpactRebuild)
			p.Changes[i].Why = appendWhy(p.Changes[i].Why,
				"the manifest declares this item world-affecting, so it is "+
					"archive-before-destroy")
		}
		imp = imp.Max(step)
	}
	p.impact = imp
	p.Boot = imp >= ImpactDownloadCycle && sc.ServerBoot() && len(p.Unmaterialized()) > 0
	return imp
}

// SetImpact records an impact a caller computed elsewhere, and refuses to
// lower one already computed.
//
// It exists for the pipeline that has to escalate a plan for a reason this
// package cannot see (a var change in the same job, say), and it is one-way on
// purpose: nothing should ever be able to talk a plan down from a rebuild.
func (p *Plan) SetImpact(i Impact) error {
	if p == nil {
		return fmt.Errorf("%w: there is no plan", ErrUnaccounted)
	}
	if !i.Valid() {
		return fmt.Errorf("%w: %d is not an impact", ErrInvalidSchema, int(i))
	}
	if i < p.impact {
		return fmt.Errorf("%w: refusing to lower block %q from %s to %s", ErrInvalidSchema,
			p.Block, p.impact, i)
	}
	p.impact = i
	return nil
}

// Materializable is Materialize's gate, as a free function so a source's first
// statement reads as the rule it is enforcing.
//
// Materialize may only write bytes once the declared set is fully accounted
// for. An unverified download is a hard failure and a partial plan is a
// refusal; neither is a warning.
func Materializable(p *Plan) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return p.CheckAccounted()
}

// Summary is the one-line report a job log and the CLI both print.
func (p *Plan) Summary() string {
	if p == nil {
		return "no plan"
	}
	parts := []string{fmt.Sprintf("%d declared", p.Declared)}
	if n := len(p.Changes); n > 0 {
		parts = append(parts, fmt.Sprintf("%d change(s)", n))
	} else {
		parts = append(parts, "no changes")
	}
	if n := len(p.Ambiguous()); n > 0 {
		parts = append(parts, fmt.Sprintf("%d ambiguous", n))
	}
	if n := len(p.Unmaterialized()); n > 0 {
		parts = append(parts, fmt.Sprintf("%d not yet downloaded", n))
	}
	if n := len(p.Unaccounted); n > 0 {
		parts = append(parts, fmt.Sprintf("%d UNACCOUNTED (refusing)", n))
	}
	parts = append(parts, "impact "+p.Impact().String())
	return strings.Join(parts, ", ")
}

// Lock renders this plan into a lockfile source block.
//
// It is the only path from a plan to a lock, and it calls the gate first: a
// lock written off an unaccounted plan would carry the partial list forward as
// recorded intent, which is the failure one step worse than writing it to the
// ini.
func (p *Plan) Lock(sc *Schema) (*SourceLock, error) {
	if err := Materializable(p); err != nil {
		return nil, err
	}
	if sc == nil {
		return nil, fmt.Errorf("%w: no compiled schema", ErrInvalidSchema)
	}
	sl := &SourceLock{
		Kind:               p.Source,
		OrderSignificant:   sc.OrderSignificant,
		VerbatimActivation: sc.VerbatimActivation,
	}
	for _, e := range p.Entries {
		sl.Entries = append(sl.Entries, LockEntry{
			Ordinal:       e.Ordinal,
			Source:        p.Source,
			Acquire:       e.AcquireID(),
			Pin:           e.Ref.Pin,
			Note:          e.Ref.Note,
			Requires:      RefRequires(e.Ref),
			Vendored:      e.Ref.Vendored,
			Materialized:  e.Materialized,
			ActivationIDs: append([]ActivationID(nil), e.Activation...),
			Dropped:       append([]ActivationID(nil), e.Dropped...),
			Resolution:    e.Resolution,
			Artifact:      e.Artifact,
			Upstream:      e.Upstream,
			Ambiguous:     e.Ambiguous,
			Evidence:      append([]string(nil), e.Evidence...),
		})
	}
	sort.SliceStable(sl.Entries, func(i, j int) bool {
		return sl.Entries[i].Ordinal < sl.Entries[j].Ordinal
	})
	sl.AcquisitionLine = joinAcquire(p.AcquireIDs(sc.Acquisition.KeepMissing),
		sc.Acquisition.Separator)
	if !sc.VerbatimActivation {
		sl.ActivationLine = joinActivate(p.ActivationIDs(), sc.Activation.Separator)
	}
	return sl, nil
}

// unaccountedDetail is the message body shared by Plan.CheckAccounted and
// SourceLock.CheckAccounted, so the refusal an operator reads is the same
// sentence whether it came off a fresh resolve or off a lockfile.
func unaccountedDetail(block string, n, declared int, us []Unaccounted) string {
	var b strings.Builder
	fmt.Fprintf(&b, "block %q left %d of %d declared item(s) unaccounted for, so nothing was "+
		"written", block, n, declared)
	for _, u := range us {
		b.WriteString("\n  - ")
		b.WriteString(u.String())
	}
	b.WriteString("\n  a partial list looks exactly like a deliberate one, so the lists were " +
		"left as they are")
	return b.String()
}

func appendWhy(have, add string) string {
	if have == "" {
		return add
	}
	if strings.Contains(have, add) {
		return have
	}
	return have + "; " + add
}
