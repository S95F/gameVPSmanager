package content

import (
	"context"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/pz"
)

// targetBuildVar is the resolver_var Project Zomboid's manifest declares, as
// `target_build: "{{ .Var.pz_build }}"`.
const targetBuildVar = "target_build"

// pzResolveModIDs is the `pz.resolve_mod_ids` resolver.
//
// It DELEGATES. internal/pz owns the scoring -- which variant inside a
// dual-build workshop item is the one this server should enable -- and is
// tested against the live 33-item mods.txt, byte-identical round trip
// included. This file renders that package's answer into the registry's shape
// and does not re-implement a line of it: a second copy of the B41-versus-B42
// rules would be a second thing to keep in step with a real server, and the
// failure mode of the two disagreeing is a mod that silently never loads.
type pzResolveModIDs struct{}

func (pzResolveModIDs) Name() string { return "pz.resolve_mod_ids" }

func (pzResolveModIDs) Doc() string {
	return "Maps Steam Workshop ids onto the internal mod ids that belong in Mods=, by " +
		"scanning each downloaded item's mod.info files and scoring the Build 41 and Build 42 " +
		"variants many items ship side by side. The target build comes from " +
		"resolver_vars.target_build; selection within an item stays purely file-based, so an " +
		"item whose files carry no build marker has every id inside it enabled and flagged " +
		"rather than guessed at. Items that have not downloaded yet are reported as missing " +
		"and stay listed, so the server retries them."
}

// Resolve reads the acquired item trees and returns activation ids.
//
// Three outcomes, three records, and the difference between them is the whole
// point:
//
//   - resolved, with the markers that justified it as evidence;
//   - AMBIGUOUS: no build marker anywhere in the item, so every id inside it
//     is enabled and flagged. This is exactly today's behaviour and it is
//     preserved deliberately -- item 2866258937 is the live example;
//   - MISSING: the bytes are not on disk. Not unaccounted, because a
//     server_boot source is supposed to fetch them on its next boot and the
//     id has to stay listed for that to happen.
//
// Nothing here can produce an Unaccounted record from a file it read: an item
// the walk could not classify at all is one internal/pz reported no result
// for, and that is the case the wrapper's cross-check catches.
func (r pzResolveModIDs) Resolve(ctx context.Context, in *ResolverInput) (*ResolverResult, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: %s was given no input", ErrInvalidSchema, r.Name())
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	target, err := pzTarget(in.Var(targetBuildVar))
	if err != nil {
		return nil, err
	}

	roots := make([]pz.WorkshopRoot, 0, len(in.Roots))
	for _, root := range in.Roots {
		if root.FS == nil {
			// A declared root with no handle is a caller bug, not a missing
			// download: resolving against the roots that happen to have
			// handles would quietly produce a shorter mod list.
			return nil, fmt.Errorf("%w: root %q was declared with no filesystem handle",
				ErrInvalidSchema, root.Name)
		}
		roots = append(roots, pz.WorkshopRoot{Name: root.Name, FS: root.FS})
	}

	ids := make([]string, 0, len(in.Items))
	for _, it := range in.Items {
		ids = append(ids, string(it.ID))
	}

	res := pz.ResolveFor(roots, ids, target)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The upstream metadata, keyed so the tag layering below can find an
	// item's tags without depending on the resolver returning its results in
	// the order they went in. Mis-attributing one item's tags to another would
	// be a conflict recorded against the wrong mod.
	upstream := make(map[AcquireID]*Upstream, len(in.Items))
	for _, it := range in.Items {
		upstream[it.ID] = it.Upstream
	}

	out := &ResolverResult{Items: make([]ResolvedItem, 0, len(res.Items))}
	for _, item := range res.Items {
		id := AcquireID(item.WorkshopID)

		// Section 12.5's evidence table, layered on top of the file scoring.
		// internal/pz owns the table (tags.go) for the same reason it owns the
		// scoring: a second copy of "tags never drop an item" would be a
		// second thing to keep in step, and the failure mode of the two
		// disagreeing is a mod the operator declared that silently never
		// loads. This file only renders the verdict into the registry's shape.
		verdict := pz.LayerTags(item, pzTagsOf(upstream[id]), target)

		ri := ResolvedItem{
			ID:         id,
			Activation: toActivationIDs(item.ModIDs),
			Dropped:    toActivationIDs(item.Dropped),
			Path:       item.Dir,
			Evidence:   append(fileEvidence(item), verdict.Evidence...),
		}
		if verdict.Warning != "" {
			// Section 12.5's doctor line. The machine-readable record of the
			// same finding is already in Evidence; this is the sentence a
			// human reads, and nothing branches on it -- "gvm reports it and
			// lets a human decide" is only true if the report says what each
			// side claimed.
			out.Warnings = append(out.Warnings, verdict.Warning)
		}
		switch verdict.Status {
		case pz.StatusCheck:
			// Enabled WITH A WARNING, not refused. Section 12.5's rule is that
			// evidence never drops an item; dropping happens only between
			// variants inside one item, by file score. An ambiguous item whose
			// ids were withheld would be a mod the operator declared and that
			// silently never loads -- the exact failure being avoided.
			ri.Ambiguous = true
		case pz.StatusMissing, pz.StatusEmpty:
			ri.Missing = true
		}
		out.Items = append(out.Items, ri)
	}
	return out, nil
}

// pzTagsOf turns the pre-fetched upstream metadata into a tag set.
//
// A nil Upstream is a NORMAL value and the one this function exists to name:
// it means the metadata API was not reached for this item, and section 12.5
// requires that to degrade to exactly the mod.info heuristic rather than to
// change any answer. pz.TagsUnavailable() is the same value as the zero
// TagSet, so the degradation is what a forgotten field produces too.
func pzTagsOf(u *Upstream) pz.TagSet {
	if u == nil {
		return pz.TagsUnavailable()
	}
	return pz.TagsFrom(u.Tags)
}

// pzTarget decodes resolver_vars.target_build.
//
// An absent value means Build 42, which is what every shipped manifest
// resolves to today and what internal/pz's own default is. A PRESENT but
// unparseable value is an error: "4.2" or "B42" silently becoming 42 would be
// a declaration nobody checked, and on a B41 server the wrong answer enables
// the B42 variant of every dual-build item.
func pzTarget(raw string) (pz.Build, error) {
	if strings.TrimSpace(raw) == "" {
		return pz.BuildB42, nil
	}
	b, err := pz.ParseBuild(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: resolver_vars.%s: %w", ErrInvalidSchema, targetBuildVar, err)
	}
	return b, nil
}

// fileEvidence renders internal/pz's markers as the lock's evidence strings.
//
// Prefixed with "modinfo:" because these are conclusions drawn from the files
// the server downloaded. Upstream metadata contributes "tags:…" evidence, and
// keeping the two prefixes distinct is what lets a reader tell "the files say
// 42" from "the Workshop page says 42" when the two disagree.
func fileEvidence(item pz.ItemResult) []string {
	switch item.Status {
	case pz.StatusMissing:
		return []string{"modinfo:not-downloaded"}
	case pz.StatusEmpty:
		return []string{"modinfo:no-usable-mod-info"}
	}
	out := make([]string, 0, len(item.Markers))
	for _, m := range item.Markers {
		out = append(out, "modinfo:"+m)
	}
	return out
}

// toActivationIDs crosses the namespace boundary exactly once, here, in a
// function whose name says what it is doing.
//
// internal/pz is a stdlib-only leaf package and speaks in strings; this
// package speaks in two incompatible types. One narrow conversion with a
// comment is reviewable. A dozen inline string casts scattered through a
// resolver are how an acquisition id eventually ends up in Mods=.
func toActivationIDs(ids []string) []ActivationID {
	if len(ids) == 0 {
		return nil
	}
	out := make([]ActivationID, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, ActivationID(id))
		}
	}
	return out
}
