package steamworkshop

import (
	"context"
	"fmt"
	"io/fs"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/content"
)

// Observe reads LOCAL state only, and it is mandatory on every source for a
// reason: adoption, drift detection and "has someone hand-edited this" become
// one call graph instead of three per-source special cases, answerable with
// the network down and Postgres gone.
//
// It reads three things and joins them:
//
//  1. The live config's two keys, as the caller read them (Input.Live). Raw,
//     because that is what verbatim activation preserves and what a hand edit
//     looks like.
//  2. Every item id the declared roots actually hold. Discovered, not
//     declared: an item nobody listed is exactly what has to show up as
//     Unlisted during adoption.
//  3. The local resolve over the union, which is what gives each item its
//     activation ids -- and therefore what makes "WorkshopItems= has 32
//     entries and Mods= has 33" into a named id rather than a puzzle.
//
// A nil Input.Live is tolerated here and refused in Resolve. Reading state is
// something an operator may ask for on an instance whose config does not exist
// yet; deciding what to WRITE without knowing what is live is not.
func (s *Source) Observe(ctx context.Context, in *content.Input) (*content.Observed, error) {
	sc, err := s.schemaOf(in)
	if err != nil {
		return nil, err
	}

	obs := &content.Observed{Block: sc.Block, ReadAt: in.Now()}
	for _, r := range in.Roots {
		obs.Roots = append(obs.Roots, r.Name)
	}

	if in.Live != nil {
		obs.ConfigRead = true
		// Carried, not folded into ConfigRead: a reading that found no file is
		// still a reading, and planChanges depends on that so a fresh instance
		// can be given its first acquisition line.
		obs.ConfigAbsent = in.Live.Absent
		obs.AcquisitionRaw = in.Live.Acquisition
		obs.ActivationRaw = in.Live.Activation
		obs.Acquire = splitAcquire(in.Live.Acquisition, sc.Acquisition.Separator)
		obs.Activate = splitActivate(in.Live.Activation, sc.Activation.Separator)
	}

	// The union, in a stable order: what the config asks for first, then
	// anything else the roots hold. Config order first because that is load
	// order, and a report that listed a stray directory before the operator's
	// first mod would be a report nobody reads twice.
	onDisk, err := rootItems(in.Roots)
	if err != nil {
		return nil, err
	}
	ids := make([]content.AcquireID, 0, len(obs.Acquire)+len(onDisk))
	seen := make(map[content.AcquireID]bool, len(ids))
	for _, id := range append(append([]content.AcquireID(nil), obs.Acquire...), onDisk...) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}

	items := make([]content.ResolverItem, 0, len(ids))
	for i, id := range ids {
		// The metadata ALREADY IN HAND, never a fetch. Observe makes no
		// request: passing what the caller holds keeps an observed item's
		// ambiguity flag agreeing with the plan's for the same item, and
		// passing nothing would make the two disagree for no reason an
		// operator could see.
		items = append(items, content.ResolverItem{Ordinal: i, ID: id,
			Upstream: in.UpstreamFor(id)})
	}

	if len(items) > 0 {
		res, err := content.Resolve(ctx, sc, items, in.Roots)
		if err != nil {
			return nil, fmt.Errorf("observe %s: %w", sc.Block, err)
		}
		accounted := make(map[content.ActivationID]bool)
		for _, it := range res.Items {
			oi := content.ObservedItem{
				ID:         it.ID,
				Present:    it.Path != "",
				Empty:      it.Path != "" && it.Missing,
				Path:       it.Path,
				Activation: it.Activation,
				Dropped:    it.Dropped,
				Evidence:   it.Evidence,
				Ambiguous:  it.Ambiguous,
			}
			obs.Items = append(obs.Items, oi)
			// Both the enabled and the DROPPED ids count as accounted for.
			// A dropped id live in Mods= is a stale entry from before a
			// variant switch, which is a different finding from an id nothing
			// on this box has ever heard of, and lumping the two together is
			// how the second one gets ignored.
			for _, a := range it.Activation {
				accounted[a] = true
			}
			for _, a := range it.Dropped {
				accounted[a] = true
			}
		}
		for _, a := range obs.Activate {
			if !accounted[a] {
				obs.Unlisted = append(obs.Unlisted, a)
			}
		}
	}

	return obs, nil
}

// rootItems lists every item directory under the declared roots.
//
// A root that cannot be LISTED is an error, not an empty root. The two readings
// differ by everything that matters: "no items are downloaded" sends a
// server_boot instance into an hour-long acquisition boot it does not need and
// reports every one of the operator's mods as missing, while "this directory
// could not be read" is a permissions problem with a one-line fix.
func rootItems(roots []content.Root) ([]content.AcquireID, error) {
	var out []content.AcquireID
	seen := map[content.AcquireID]bool{}
	for _, r := range roots {
		if r.FS == nil {
			// Same refusal as the resolver's: scanning the roots that happen
			// to have handles quietly produces a shorter answer.
			return nil, fmt.Errorf("%w: root %q was declared with no filesystem handle",
				ErrInput, r.Name)
		}
		ents, err := fs.ReadDir(r.FS, ".")
		if err != nil {
			return nil, fmt.Errorf("%w: listing content root %q: %w", ErrInput, r.Name, err)
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			id := content.AcquireID(e.Name())
			if !numericID(e.Name()) || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// splitAcquire parses a raw acquisition value into acquisition ids.
//
// Separate from splitActivate, with no shared generic helper, for the same
// reason joinAcquire and joinActivate are separate in internal/content: there
// is no call site at which the wrong namespace's ids can arrive, because the
// return types do not match.
func splitAcquire(raw, sep string) []content.AcquireID {
	parts := splitRaw(raw, sep)
	if len(parts) == 0 {
		// nil, not an empty slice: "there is no list" is the answer, and an
		// empty-but-present slice is the shape a caller tests for length and
		// then treats as a deliberate empty list.
		return nil
	}
	out := make([]content.AcquireID, 0, len(parts))
	for _, p := range parts {
		out = append(out, content.AcquireID(p))
	}
	return out
}

// splitActivate parses a raw activation value into activation ids.
func splitActivate(raw, sep string) []content.ActivationID {
	parts := splitRaw(raw, sep)
	if len(parts) == 0 {
		return nil
	}
	out := make([]content.ActivationID, 0, len(parts))
	for _, p := range parts {
		out = append(out, content.ActivationID(p))
	}
	return out
}

// splitRaw splits a config list value.
//
// Empty fields are dropped, which is what makes a trailing separator harmless:
// PZ writes "a;b;c;" about as often as "a;b;c", and an empty id in the middle
// of Mods= is a typo rather than an instruction. The surviving fields are
// trimmed but otherwise untouched -- no sorting, no de-duplication -- because
// the ORDER is load order and a duplicate is evidence of an edit.
func splitRaw(raw, sep string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if sep == "" {
		// A caller with no separator cannot have a list, and guessing one is
		// how a comma ends up disabling every mod at once.
		return []string{strings.TrimSpace(raw)}
	}
	var out []string
	for _, f := range strings.Split(raw, sep) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
