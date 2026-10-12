package content

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// TestTheTwoRegistriesNameTheSameSet is D-21's closed registry, asserted in
// BOTH directions.
//
// spec.ResolverNames is the LOAD-time registry: it decides whether a manifest
// validates. This package is the RUNTIME one: it decides whether a resolve can
// run. Two lists that could disagree would mean a manifest that validates
// cleanly and then fails when a content sync tries to use it -- with the
// server already stopped -- or, worse, a resolver that exists and that no
// manifest is allowed to name, which is a feature nobody can reach.
func TestTheTwoRegistriesNameTheSameSet(t *testing.T) {
	load := spec.ResolverNames()
	if len(load) == 0 {
		t.Fatal("spec.ResolverNames() is empty")
	}

	for name, support := range load {
		_, runnable := GetResolver(name)
		switch support {
		case spec.SupportImplemented:
			if !runnable {
				t.Errorf("a manifest may declare resolver %q and this package does not "+
					"implement it; the manifest would validate and then fail mid-sync", name)
			}
		default:
			if runnable {
				t.Errorf("resolver %q is registered here but spec calls it %v; it would run "+
					"while the schema advertises it as unavailable", name, support)
			}
		}
	}

	for _, name := range ResolverNames() {
		support, declarable := load[name]
		if !declarable {
			t.Errorf("resolver %q is implemented and no manifest is allowed to name it", name)
			continue
		}
		if support != spec.SupportImplemented {
			t.Errorf("resolver %q is implemented and spec calls it %v", name, support)
		}
	}
}

func TestTheRegistryKeysAreTheResolverNames(t *testing.T) {
	for key, r := range resolvers {
		if r.Name() != key {
			t.Errorf("resolvers[%q] is a resolver called %q; every log line would attribute it "+
				"to the wrong name", key, r.Name())
		}
		if strings.TrimSpace(r.Doc()) == "" {
			t.Errorf("%s has no Doc(); the manifest reference renders it", key)
		}
		// A name with no dot is a resolver nobody can attribute to a game, and
		// the registry's readability rests on the prefix.
		if !strings.Contains(key, ".") {
			t.Errorf("%q has no <game>. prefix", key)
		}
	}
}

// TestEveryImplementedResolverIsNamedByAShippedManifest catches dead code in
// one direction and a manifest that forgot its resolver in the other.
func TestEveryImplementedResolverIsNamedByAShippedManifest(t *testing.T) {
	used := map[string]bool{}
	ents, err := fs.ReadDir(games.FS, ".")
	if err != nil {
		t.Fatalf("read the embedded manifests: %v", err)
	}
	for _, e := range ents {
		raw, err := fs.ReadFile(games.FS, e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		m, _, err := spec.ValidateBytes("games/"+e.Name(), raw)
		if err != nil {
			continue // another test owns manifest validity
		}
		for _, block := range m.Spec.Content {
			if r := strings.TrimSpace(block.Activation.Resolver); r != "" {
				used[r] = true
			}
		}
	}
	for _, name := range ResolverNames() {
		if !used[name] {
			t.Errorf("%q is registered but no shipped manifest names it", name)
		}
	}
}

func TestMustResolverNamesWhatIsAvailable(t *testing.T) {
	_, err := MustResolver("pz.resolve_mods")
	if !errors.Is(err, ErrNoResolver) {
		t.Fatalf("MustResolver returned %v for an unknown name", err)
	}
	if !strings.Contains(err.Error(), "pz.resolve_mod_ids") {
		t.Errorf("the error does not list what IS implemented, so the operator guesses again: %v",
			err)
	}
	if _, err := MustResolver(" pz.resolve_mod_ids "); err != nil {
		t.Errorf("a name with stray whitespace was rejected: %v", err)
	}
	if docs := ResolverDocs(); len(docs) != len(resolvers) {
		t.Errorf("ResolverDocs has %d entries for %d resolvers", len(docs), len(resolvers))
	}
}

// shortResolver returns fewer items than it was given, and records no reason.
// A real resolver should never do this; the wrapper's job is to make sure the
// gate closes anyway.
type shortResolver struct{}

func (shortResolver) Name() string { return "test.short" }
func (shortResolver) Doc() string  { return "drops items on the floor" }

func (shortResolver) Resolve(context.Context, *ResolverInput) (*ResolverResult, error) {
	return &ResolverResult{Items: []ResolvedItem{{ID: "3171167894",
		Activation: []ActivationID{"ThatDamnLibrary"}}}}, nil
}

// TestResolveAccountsForEveryItemEvenWhenTheResolverForgets is why the
// cross-check lives in the wrapper rather than in each resolver: the gate has
// to close on a resolver whose author forgot it, because the failure it guards
// against is invisible downstream.
func TestResolveAccountsForEveryItemEvenWhenTheResolverForgets(t *testing.T) {
	resolvers["test.short"] = shortResolver{}
	t.Cleanup(func() { delete(resolvers, "test.short") })

	block := pzBlock()
	block.Activation.Resolver = "test.short"
	sc := mustCompile(t, block, spec.ContentList{})

	res, err := Resolve(context.Background(), sc, []ResolverItem{
		{Ordinal: 0, ID: "3171167894"},
		{Ordinal: 1, ID: "3508537032"},
		{Ordinal: 2, ID: "2866258937"},
	}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Unaccounted) != 2 {
		t.Fatalf("got %d unaccounted record(s), want 2: %v", len(res.Unaccounted), res.Unaccounted)
	}
	for _, u := range res.Unaccounted {
		if u.ID == "" || !strings.Contains(u.Reason, "test.short") {
			t.Errorf("the record does not name the item and the resolver: %+v", u)
		}
	}
}

func TestResolveRefusesABlockWithNoResolver(t *testing.T) {
	block := pzBlock()
	block.Activation.Resolver = ""
	sc := mustCompile(t, block, spec.ContentList{})

	if _, err := Resolve(context.Background(), sc, nil, nil); !errors.Is(err, ErrNoResolver) {
		t.Fatalf("Resolve on a block with no resolver = %v; acquisition ids cannot be mapped "+
			"onto activation ids by guessing", err)
	}
}

// nilResolver returns (nil, nil), which a careless implementation can do by
// falling off the end of a switch.
type nilResolver struct{}

func (nilResolver) Name() string { return "test.nil" }
func (nilResolver) Doc() string  { return "returns nothing at all" }
func (nilResolver) Resolve(context.Context, *ResolverInput) (*ResolverResult, error) {
	return nil, nil
}

func TestResolveTreatsANilResultAsUnaccounted(t *testing.T) {
	resolvers["test.nil"] = nilResolver{}
	t.Cleanup(func() { delete(resolvers, "test.nil") })

	block := pzBlock()
	block.Activation.Resolver = "test.nil"
	sc := mustCompile(t, block, spec.ContentList{})

	_, err := Resolve(context.Background(), sc, []ResolverItem{{ID: "1"}}, nil)
	if !errors.Is(err, ErrUnaccounted) {
		t.Fatalf("a nil result was accepted: %v; it would read as \"no mods\"", err)
	}
}

func TestResolverInputVarIsTrimmed(t *testing.T) {
	in := &ResolverInput{Vars: map[string]string{"target_build": " 42 "}}
	if got := in.Var("target_build"); got != "42" {
		t.Errorf("Var = %q", got)
	}
	if got := (*ResolverInput)(nil).Var("x"); got != "" {
		t.Errorf("Var on a nil input = %q", got)
	}
}
