// This file is package provider_test rather than package provider, which is
// what lets it import the driver packages: they import provider, and an
// internal test file importing them back would close an import cycle. The
// external test package is the only place the registry can be observed fully
// populated.
package provider_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"

	// Every driver, imported for its registration. TestEveryProviderPackageIsRegistered
	// fails the build if a package appears in this directory and not in this
	// list, so the enumeration below cannot quietly stop covering a driver.
	_ "github.com/S95F/gameVPSmanager/internal/provider/adopted"
	_ "github.com/S95F/gameVPSmanager/internal/provider/minecraft"
	_ "github.com/S95F/gameVPSmanager/internal/provider/none"
	_ "github.com/S95F/gameVPSmanager/internal/provider/steamcmd"
)

// planBudget bounds one Plan call. Plan is network-reads-only and must answer
// or refuse; a driver that blocks past this is itself the finding, because the
// nightly cycle calls Plan before it decides whether anything stops.
const planBudget = 30 * time.Second

// adoptedCtx is an instance whose files predate gvm: a real world, with a real
// install tree, that gvm must not write into.
//
// Both optional provider blocks are filled in regardless of the kind under
// test, so this helper knows nothing about any particular driver: a driver
// that needs its own configuration finds it, and the enumeration stays
// kind-agnostic as drivers are added.
func adoptedCtx(t *testing.T, kind string) *provider.Ctx { return newCtx(t, kind, false) }

func managedCtx(t *testing.T, kind string) *provider.Ctx { return newCtx(t, kind, true) }

func newCtx(t *testing.T, kind string, managed bool) *provider.Ctx {
	t.Helper()
	base := t.TempDir()
	install := filepath.Join(base, "opt", "pzserver")
	data := filepath.Join(base, "home", "pzserver", "Zomboid")
	home := filepath.Join(base, "home", "pzserver")
	work := filepath.Join(base, "var", "lib", "gvm", "cache", "steam", "homes", "pz-main")
	for _, d := range []string{filepath.Join(install, "steamapps"), data, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Something in the install tree, so Inspect has a real "present" to find.
	if err := os.WriteFile(filepath.Join(install, "start-server.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	paths, err := provider.NewPaths(spec.Paths{Install: install, Data: data, Home: home, Work: work})
	if err != nil {
		t.Fatal(err)
	}
	runAs, err := provider.NewUnixIdent("gvm-pz-main", "gvm-pz-main", 2001, 2001)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := provider.NewStore(filepath.Join(base, "blobs"))
	if err != nil {
		t.Fatal(err)
	}

	return &provider.Ctx{
		Name: "pz-main",
		Manifest: &spec.Manifest{
			APIVersion: spec.APIVersionV1,
			Kind:       spec.KindGame,
			Metadata:   spec.GameMeta{ID: "projectzomboid", Revision: 3},
			Spec: spec.GameSpec{
				DriverABI: spec.BuiltDriverABI,
				Provider: spec.ProviderSpec{
					Kind: kind,
					SteamCMD: &spec.SteamCMDSpec{
						InstallAppID: 380870,
						Anonymous:    true,
						Branch:       "public",
						Platform:     spec.PlatformLinux,
						Sentinel:     spec.Sentinel{Path: "start-server.sh", Executable: true},
						Verify:       spec.Verify{AppManifest: true, ByteParity: true, SingleACF: true},
					},
					Minecraft: &spec.MinecraftSpec{
						MCVersion:  "1.20.1",
						Loader:     spec.LoaderSpec{Kind: "vanilla", Version: "1.20.1"},
						Entrypoint: "serverstarterjar",
						Sentinel:   spec.Sentinel{Path: "server.jar"},
					},
				},
			},
		},
		Vars:           spec.VarValues{},
		Ports:          spec.PortValues{"game": 16261},
		Paths:          paths,
		RunAs:          runAs,
		Blobs:          blobs,
		ManagedInstall: managed,
		Clock:          func() time.Time { return time.Unix(1762000000, 0).UTC() },
	}
}

// TestEveryRegisteredProviderRefusesAPlanThatSkippedAuthorize is the structural
// half of the adopted-install guarantee.
//
// Authorize is the only thing that stamps a plan, the stamp is an unexported
// field, and Authorize refuses a destructive or writing plan on an adopted
// instance. So if every Apply refuses an unstamped plan, then every plan that
// reaches Apply has been through the managed_install gate -- and that is a
// property of the whole registry rather than of the one driver whose tests
// somebody remembered to write.
//
// A driver added later whose Apply does not start with CheckAuthorized fails
// here.
func TestEveryRegisteredProviderRefusesAPlanThatSkippedAuthorize(t *testing.T) {
	kinds := provider.Kinds()
	if len(kinds) == 0 {
		t.Fatal("no providers are registered, so this test proves nothing")
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			p, ok := provider.Lookup(kind)
			if !ok {
				t.Fatalf("Kinds() named %q and Lookup does not know it", kind)
			}
			c := adoptedCtx(t, kind)

			// A plan that wipes the install tree, exactly as a careless or
			// compromised caller would hand it over: never authorized.
			unstamped := &provider.Plan{
				Intent:      provider.IntentInstall,
				Reason:      "reinstall from scratch",
				Destructive: true,
				Artifacts: []provider.Artifact{{
					Digest:  provider.Digest("sha256:" + strings.Repeat("ab", 32)),
					Size:    4,
					Source:  "https://example.invalid/evil.tar",
					DestRel: "start-server.sh",
				}},
			}
			if unstamped.Authorized() {
				t.Fatal("a composite literal produced an authorized plan; the stamp is forgeable")
			}
			_, err := p.Apply(context.Background(), c, unstamped, provider.Progress{})
			if !errors.Is(err, provider.ErrUnauthorized) {
				t.Fatalf("Apply accepted a plan that never passed Authorize: err=%v", err)
			}
		})
	}
}

// TestEveryRegisteredProviderRefusesAPlanAuthorizedForAnotherInstance covers
// the second way a destructive plan could reach an adopted world: a plan
// legitimately cleared against a MANAGED instance, applied to an adopted one.
//
// That is a plan carrying a real stamp, so the stamp alone is not enough --
// Apply has to compare it to the instance it is standing in.
func TestEveryRegisteredProviderRefusesAPlanAuthorizedForAnotherInstance(t *testing.T) {
	for _, kind := range provider.Kinds() {
		t.Run(kind, func(t *testing.T) {
			p, _ := provider.Lookup(kind)

			stamped := provider.EmptyPlan(provider.IntentUpdate, "nothing to do")
			if err := stamped.Authorize(true); err != nil {
				t.Fatalf("Authorize for a managed instance: %v", err)
			}
			if !stamped.Authorized() {
				t.Fatal("Authorize did not stamp the plan")
			}

			_, err := p.Apply(context.Background(), adoptedCtx(t, kind), stamped, provider.Progress{})
			if !errors.Is(err, provider.ErrUnauthorized) {
				t.Fatalf("Apply accepted a plan authorized for managed_install:true against an "+
					"adopted instance: err=%v", err)
			}
		})
	}
}

// TestNoRegisteredProviderPlansAWriteForAnAdoptedInstance is the behavioural
// half: for every driver and every intent, a plan built against an adopted
// instance either refuses outright or writes nothing and destroys nothing.
//
// Plan is network-reads-only, so a driver that needs a tool or an endpoint
// this machine does not have returns an error, and an error IS a refusal. What
// no driver may do is come back with a plan that would put bytes into an
// install gvm does not own.
func TestNoRegisteredProviderPlansAWriteForAnAdoptedInstance(t *testing.T) {
	for _, kind := range provider.Kinds() {
		for _, in := range provider.Intents() {
			t.Run(kind+"/"+string(in), func(t *testing.T) {
				p, _ := provider.Lookup(kind)
				c := adoptedCtx(t, kind)

				ctx, cancel := context.WithTimeout(context.Background(), planBudget)
				defer cancel()

				pl, err := p.Plan(ctx, c, in)
				if err != nil {
					return // a refusal is a correct answer
				}
				if pl == nil {
					t.Fatal("Plan returned no plan and no error")
				}
				if err := pl.Validate(); err != nil {
					t.Fatalf("Plan produced a plan it cannot pass Validate with: %v", err)
				}
				if pl.Destructive {
					t.Error("Plan is destructive against an adopted install")
				}
				if pl.Writes() {
					t.Errorf("Plan would write %d artifact(s) into an install gvm does not own",
						len(pl.Artifacts))
				}
				// The plan must also be usable: Inspect, verify and the
				// nightly build check all run on adopted instances, so the
				// gate has to let a non-writing plan through.
				if err := pl.Authorize(false); err != nil {
					t.Errorf("a non-writing plan was refused for an adopted instance: %v", err)
				}
				applied, err := p.Apply(context.Background(), c, pl, provider.Progress{})
				if err != nil {
					t.Fatalf("Apply of an authorized empty plan: %v", err)
				}
				if applied.Changed {
					t.Error("Apply reported a change having written nothing")
				}
				if strings.TrimSpace(applied.Note) == "" {
					t.Error("a no-op Apply must say why it did nothing; that sentence is the " +
						"whole output of a skipped nightly (D-43)")
				}
			})
		}
	}
}

// TestEveryProviderPackageIsRegistered is what stops the enumeration above
// from silently narrowing.
//
// Every test here walks provider.Kinds(), which only lists drivers something
// imported. A new driver package that nobody blank-imports in this file would
// therefore be covered by nothing at all, and would pass the build while being
// free to wipe an adopted install. So the directory itself is the source of
// truth: a package in internal/provider is a driver, its directory name is its
// kind, and it has to be in the registry.
func TestEveryProviderPackageIsRegistered(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	registered := make(map[string]bool)
	for _, k := range provider.Kinds() {
		registered[k] = true
	}

	found := 0
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "testdata" {
			continue
		}
		if !hasGoFiles(t, e.Name()) {
			continue
		}
		found++
		if !registered[e.Name()] {
			t.Errorf("internal/provider/%s is a Go package and %q is not a registered provider "+
				"kind.\nA driver nothing imports is covered by no test in this file, and is "+
				"therefore free to write into an adopted install.\nFix: call provider.Register "+
				"from its init, name the package directory after its Kind(), and add a blank "+
				"import of it to plan_authorize_test.go.", e.Name(), e.Name())
		}
	}
	if found == 0 {
		t.Fatal("found no driver packages beside this test, so the scan proves nothing")
	}
}

// TestTheRegistryOnlyHoldsKindsTheSchemaImplements keeps the driver set and the
// manifest schema from drifting, in the direction that can be checked today: a
// driver registered under a name no manifest may name is unreachable code that
// looks like a feature.
//
// internal/spec keeps the list separately on purpose -- validation must not
// depend on a driver package having been imported for its side effects -- and
// provider.Register enforces this at registration time. This is the test that
// says so out loud.
func TestTheRegistryOnlyHoldsKindsTheSchemaImplements(t *testing.T) {
	schema := spec.ProviderKinds()
	for _, kind := range provider.Kinds() {
		switch schema[kind] {
		case spec.SupportImplemented:
		case spec.SupportPlanned:
			t.Errorf("%q is registered as a working driver and internal/spec calls it planned",
				kind)
		default:
			t.Errorf("%q is registered and is not a provider kind internal/spec names, so no "+
				"manifest could ever reach it", kind)
		}
	}
}

func TestAuthorizeRefusesADestructivePlanOnAnAdoptedInstance(t *testing.T) {
	p := &provider.Plan{
		Intent:      provider.IntentRepair,
		Reason:      "operator ran `gvm update --repair`",
		Destructive: true,
	}
	err := p.Authorize(false)
	if !errors.Is(err, provider.ErrAdopted) {
		t.Fatalf("want ErrAdopted, got %v", err)
	}
	if p.Authorized() {
		t.Fatal("a refused plan was stamped anyway")
	}
	if !provider.Terminal(err) {
		t.Error("an adopted refusal is terminal: retrying the job cannot change the flag")
	}
	// The operator's next move has to be in the message, or this refusal is a
	// dead end at 2am.
	if !strings.Contains(err.Error(), "not available in this build") {
		t.Errorf("the refusal does not say what to do about it: %v", err)
	}

	// The same plan against an instance gvm owns is allowed.
	if err := p.Authorize(true); err != nil {
		t.Fatalf("a destructive plan on a managed instance was refused: %v", err)
	}
	if !p.Authorized() {
		t.Fatal("Authorize did not stamp an accepted plan")
	}
}

// A provider that mislabels an overwrite as non-destructive must not get past
// the gate either: writing bytes into a tree that predates gvm is the
// destructive act whether or not the driver said so.
func TestAuthorizeRefusesWritesOnAnAdoptedInstanceEvenWhenTheyAreNotLabelledDestructive(t *testing.T) {
	p := &provider.Plan{
		Intent: provider.IntentUpdate,
		Reason: "buildid 18452901 -> 18461233",
		Artifacts: []provider.Artifact{{
			Digest:  provider.Digest("sha256:" + strings.Repeat("cd", 32)),
			Size:    1 << 30,
			Source:  "steamcmd:app_update 380870",
			DestRel: ".",
		}},
	}
	err := p.Authorize(false)
	if !errors.Is(err, provider.ErrAdopted) {
		t.Fatalf("want ErrAdopted, got %v", err)
	}
	if p.Authorized() {
		t.Fatal("a refused plan was stamped")
	}
}

// A verify plan reads and writes nothing, and must work on an adopted
// instance: `gvm doctor` and the nightly build check run there (section 13.5).
func TestAuthorizeAllowsAReadOnlyPlanOnAnAdoptedInstance(t *testing.T) {
	p := provider.EmptyPlan(provider.IntentVerify, "post-update integrity check")
	if err := p.Authorize(false); err != nil {
		t.Fatalf("a read-only plan was refused on an adopted instance: %v", err)
	}
	if err := p.CheckAuthorized(false); err != nil {
		t.Fatalf("CheckAuthorized refused the plan Authorize just stamped: %v", err)
	}
}

func TestAuthorizeRefusesAPlanItCannotValidate(t *testing.T) {
	cases := []struct {
		name string
		plan *provider.Plan
	}{
		{"unknown intent", &provider.Plan{Intent: provider.Intent("reinstall")}},
		{"negative byte budget", &provider.Plan{Intent: provider.IntentInstall, Bytes: -1}},
		{
			// An audit row reading "destroyed 14.2 GB" with no sentence beside
			// it is a row nobody can act on.
			"destructive with no reason",
			&provider.Plan{Intent: provider.IntentRepair, Destructive: true},
		},
		{
			"two artifacts writing one path",
			&provider.Plan{Intent: provider.IntentInstall, Artifacts: []provider.Artifact{
				{Digest: provider.Digest("sha256:" + strings.Repeat("ab", 32)), Source: "a", DestRel: "mods/x.jar"},
				{Digest: provider.Digest("sha256:" + strings.Repeat("cd", 32)), Source: "b", DestRel: "./mods/x.jar"},
			}},
		},
		{
			"an artifact escaping the install tree",
			&provider.Plan{Intent: provider.IntentInstall, Artifacts: []provider.Artifact{
				{Digest: provider.Digest("sha256:" + strings.Repeat("ab", 32)), Source: "a",
					DestRel: "../../../../etc/cron.d/root"},
			}},
		},
		{
			"an artifact with no stated source",
			&provider.Plan{Intent: provider.IntentInstall, Artifacts: []provider.Artifact{
				{Digest: provider.Digest("sha256:" + strings.Repeat("ab", 32)), DestRel: "mods/x.jar"},
			}},
		},
		{
			"an artifact with an unparseable digest",
			&provider.Plan{Intent: provider.IntentInstall, Artifacts: []provider.Artifact{
				{Digest: "deadbeef", Source: "a", DestRel: "mods/x.jar"},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Checked against a MANAGED instance, so the refusal is Validate's
			// and not the adopted gate's.
			if err := tc.plan.Authorize(true); !errors.Is(err, provider.ErrInvalidPlan) {
				t.Fatalf("want ErrInvalidPlan, got %v", err)
			}
			if tc.plan.Authorized() {
				t.Error("an invalid plan was stamped")
			}
			if err := tc.plan.CheckAuthorized(true); !errors.Is(err, provider.ErrUnauthorized) {
				t.Errorf("Apply would have accepted it: %v", err)
			}
		})
	}
}

// A steamcmd artifact's digest is "buildid:…", which the blob store cannot
// verify. That has to be a legal artifact -- SteamCMD writes its own bytes --
// and the store still has to refuse to serve it (blobs_test.go). Both halves
// are needed: if Validate rejected it, no steamcmd plan could exist; if the
// store accepted it, an unverifiable identity would become a verified one.
func TestAnUnverifiableDigestIsAValidArtifactAndStillNotABlob(t *testing.T) {
	a := provider.Artifact{
		Digest:  "buildid:380870:18452901",
		Size:    14_200_000_000,
		Source:  "steamcmd:app_update 380870",
		DestRel: ".",
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("a steamcmd artifact was refused: %v", err)
	}
	if a.Digest.Verifiable() {
		t.Error("a buildid claims to be hashable")
	}
}

func TestCheckAuthorizedRefusesNilAndUnstamped(t *testing.T) {
	var nilPlan *provider.Plan
	if err := nilPlan.CheckAuthorized(true); !errors.Is(err, provider.ErrUnauthorized) {
		t.Errorf("want ErrUnauthorized for a nil plan, got %v", err)
	}
	if err := nilPlan.Authorize(true); !errors.Is(err, provider.ErrInvalidPlan) {
		t.Errorf("want ErrInvalidPlan authorizing a nil plan, got %v", err)
	}
	if nilPlan.Authorized() || nilPlan.Writes() {
		t.Error("a nil plan answered yes to something")
	}
}

func hasGoFiles(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}
