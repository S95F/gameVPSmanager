package content

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Resolver maps the ACQUISITION namespace onto the ACTIVATION namespace by
// reading the files that were acquired.
//
// It is a closed Go registry, not a plugin system (D-21). `hooks: {post_content:
// [pz.resolve_mod_ids]}` with no stated contract is exactly the extension point
// this design exists to avoid, and a YAML predicate like `field('versionMin')
// >= 42` has unspecified numeric-versus-lexicographic semantics against a real
// `versionMin=42.0.1` -- where a wrong answer SILENTLY OMITS A MOD while the
// server boots normally, regressing sync-mods.sh's visible `check` warning into
// silent data loss. So the mapping is Go, with a registry that a test proves is
// closed in both directions: a name a manifest may declare must exist, and a
// resolver that exists must be declarable.
//
// What a resolver may see is in ResolverInput and nothing else: read-only fs.FS
// handles for the declared roots, the acquired items with their pre-fetched
// upstream metadata, and the manifest's resolver_vars. No network, no writer,
// no database, no instance handle.
type Resolver interface {
	// Name is the registry key and the manifest spelling. They are the same
	// string, asserted by a test, so a log line naming a resolver names
	// something an operator can grep the manifest for.
	Name() string

	// Doc is what `gvm game hooks` and the manifest reference print, so an
	// author can see what is nameable without reading Go.
	Doc() string

	// Resolve reads files and returns ids. It must account for every item it
	// was given: anything it cannot classify goes in
	// ResolverResult.Unaccounted, never dropped.
	//
	// ctx is for cancellation only. Walking a 6 GB workshop tree is minutes of
	// IO, and an operator who pressed ctrl-C should not have to wait for it.
	Resolve(ctx context.Context, in *ResolverInput) (*ResolverResult, error)
}

// ResolverInput is the EXHAUSTIVELY ENUMERATED input struct of D-21.
//
// Exhaustive on purpose: every field is something a resolver is allowed to
// read, and there is no `any`, no context value bag and no handle it could
// reach something else through. Adding a field here is a design decision
// somebody has to make deliberately, which is the opposite of a plugin API
// that hands over an instance and hopes.
type ResolverInput struct {
	// Block is the content block's name, for messages.
	Block string
	// Roots are the declared content roots that EXIST, in declared order, each
	// a read-only fs.FS rooted at that directory. Searched in order; the first
	// holding an item wins, because the server writes workshop content to
	// different locations depending on version and both may be on one box.
	Roots []Root
	// Items are the acquired items, in declared (load) order.
	Items []ResolverItem
	// Vars are the manifest's resolver_vars, already rendered by the caller.
	// A resolver cannot render: rendering needs vars, ports and secrets, and a
	// resolver that could render could read a secret into a path.
	Vars map[string]string
}

// Var reads one resolver_var, trimmed.
func (in *ResolverInput) Var(key string) string {
	if in == nil {
		return ""
	}
	return strings.TrimSpace(in.Vars[key])
}

// AcquireIDs is the items' ids, in declared order.
func (in *ResolverInput) AcquireIDs() []AcquireID {
	if in == nil {
		return nil
	}
	out := make([]AcquireID, 0, len(in.Items))
	for _, it := range in.Items {
		out = append(out, it.ID)
	}
	return out
}

// ResolverItem is one acquired item as the resolver sees it.
type ResolverItem struct {
	Ordinal int
	ID      AcquireID
	Pin     string
	// Vendored means operator bytes: never fetched, never pruned. A resolver
	// still reads them, because an operator-supplied mod has a mod.info too.
	Vendored bool
	// Upstream is pre-fetched metadata, or nil when the API was unreachable.
	// Nil is a NORMAL value: section 12.5's rule is that an unreachable API
	// degrades the resolve to the local file heuristic, so a resolver that
	// required this would turn a Steam outage into an empty mod list.
	Upstream *Upstream
}

// ResolverResult is what a resolver found.
type ResolverResult struct {
	Items []ResolvedItem
	// Unaccounted is the resolver's half of the fail-closed gate. An item the
	// resolver was handed and cannot say anything about goes here; it does not
	// get left out of Items silently, because a short list and a deliberate
	// list are the same bytes.
	Unaccounted []Unaccounted
	// Warnings are sentences for a report: section 12.5's "doctor warns 'file
	// says 42, Workshop says 41'".
	//
	// MESSAGES, never decisions. Nothing branches on a warning, and every
	// finding that has one also has a machine-readable record of itself in the
	// relevant entry's Evidence. A warning list that something acted on would
	// be a second, undeclared gate.
	Warnings []string
}

// ResolvedItem is one item's mapping into the activation namespace.
type ResolvedItem struct {
	ID AcquireID
	// Activation is a SLICE: one item legitimately holds several mods.
	Activation []ActivationID
	// Dropped are the ids rejected as lower-scoring build variants, kept so a
	// surprising exclusion is explainable later.
	Dropped []ActivationID
	// Evidence is why: "modinfo:versionMin>=42", "modinfo:no-build-marker".
	Evidence []string
	// Ambiguous means "enabled with a warning", not "refused". No build marker
	// was found, so every id inside the item was enabled -- exactly today's
	// behaviour -- and flagged for the report.
	Ambiguous bool
	// Missing means the item's bytes are not on disk yet. Not an error: on a
	// server_boot source it is what an acquisition boot is for, and the id
	// stays listed so the server retries it.
	Missing bool
	// Path is where the item was found, for the lock's artifact and for
	// diagnostics.
	Path string
}

// resolvers is the closed registry.
//
// A package-level map with no registration function reachable from outside:
// another package cannot add an entry, so the set of resolvers in a build is
// the set in this file, and resolver_test.go proves it matches the set a
// manifest is allowed to name.
var resolvers = func() map[string]Resolver {
	out := map[string]Resolver{}
	for _, r := range []Resolver{
		pzResolveModIDs{},
	} {
		out[r.Name()] = r
	}
	return out
}()

// GetResolver is the runtime lookup. spec.ResolverNames is the load-time one,
// and resolver_test.go asserts the two name the same set: two lists that could
// disagree would mean a manifest that validates cleanly and then fails in the
// middle of a sync, with the server already stopped.
func GetResolver(name string) (Resolver, bool) {
	r, ok := resolvers[strings.TrimSpace(name)]
	return r, ok
}

// MustResolver is GetResolver for a caller that has already validated the
// name, and it returns an error rather than panicking because "already
// validated" is a claim and this is the place it can be checked cheaply.
func MustResolver(name string) (Resolver, error) {
	r, ok := GetResolver(name)
	if !ok {
		return nil, fmt.Errorf("%w: %q; this build implements %s", ErrNoResolver, name,
			strings.Join(ResolverNames(), "|"))
	}
	return r, nil
}

// ResolverNames lists the registered resolvers, sorted.
func ResolverNames() []string {
	out := make([]string, 0, len(resolvers))
	for n := range resolvers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ResolverDocs is the name/doc table `gvm game resolvers` prints.
func ResolverDocs() map[string]string {
	out := make(map[string]string, len(resolvers))
	for n, r := range resolvers {
		out[n] = r.Doc()
	}
	return out
}

// Resolve runs the block's declared resolver over the acquired items.
//
// This is the wrapper every source calls, and it is where the accounting
// cross-check happens -- once, for every resolver, rather than in each one. A
// resolver that returned a short list without filling in Unaccounted has the
// difference recorded for it here, which means the gate closes even on a
// resolver whose author forgot it.
func Resolve(ctx context.Context, sc *Schema, items []ResolverItem, roots []Root) (*ResolverResult, error) {
	if sc == nil {
		return nil, fmt.Errorf("%w: no compiled schema", ErrInvalidSchema)
	}
	if sc.Activation.Resolver == "" {
		return nil, fmt.Errorf("%w: block %q declares no activation resolver, so acquisition "+
			"ids cannot be mapped onto activation ids", ErrNoResolver, sc.Block)
	}
	r, err := MustResolver(sc.Activation.Resolver)
	if err != nil {
		return nil, err
	}

	in := &ResolverInput{
		Block: sc.Block,
		Roots: roots,
		Items: items,
		Vars:  sc.Activation.Vars,
	}
	res, err := r.Resolve(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("resolver %s on block %q: %w", r.Name(), sc.Block, err)
	}
	if res == nil {
		return nil, fmt.Errorf("%w: resolver %s returned nothing for block %q", ErrUnaccounted,
			r.Name(), sc.Block)
	}

	// The cross-check: every item in, accounted for once on the way out.
	seen := make(map[AcquireID]bool, len(res.Items))
	for _, it := range res.Items {
		seen[it.ID] = true
	}
	for _, u := range res.Unaccounted {
		if u.ID != "" {
			seen[u.ID] = true
		}
	}
	for _, it := range items {
		if seen[it.ID] {
			continue
		}
		res.Unaccounted = append(res.Unaccounted, Unaccounted{
			ID: it.ID,
			Reason: fmt.Sprintf("resolver %s returned neither a result nor a reason for it",
				r.Name()),
			Hint: "this is a gvm bug; the lists were left as they are rather than written short",
		})
	}
	return res, nil
}
