package content

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// pzBlock is the shipped Project Zomboid content block, shaped as the manifest
// declares it. Written out rather than loaded so a test can mutate one field at
// a time; TestTheShippedManifestCompiles keeps it honest against the real file.
func pzBlock() spec.ContentSpec {
	return spec.ContentSpec{
		Name:             "workshop",
		Source:           spec.SourceSteamWorkshop,
		WorkshopAppID:    108600,
		OrderSignificant: true,
		Roots:            []string{"/home/pzserver/Zomboid/steamapps/workshop/content/108600"},
		Acquisition: spec.ContentAcquisition{
			Mechanism:   spec.AcquireServerBoot,
			WriteKey:    "WorkshopItems",
			WriteFile:   "server_ini",
			Separator:   ";",
			KeepMissing: true,
			Verify:      spec.AcquireVerify{DirNonEmpty: true},
		},
		Activation: spec.ContentActivation{
			WriteKey:     "Mods",
			WriteFile:    "server_ini",
			Separator:    ";",
			Resolver:     "pz.resolve_mod_ids",
			ResolverVars: map[string]string{"target_build": "42"},
		},
		MetadataAPI:    "steam_remote_storage",
		CollectionSync: true,
		OwnedKeys:      []string{"WorkshopItems", "Mods"},
		Impact: map[string]string{
			"add": "download_cycle", "remove": "restart", "reorder": "restart",
			"update": "download_cycle", "repin": "download_cycle",
		},
	}
}

func mustCompile(t *testing.T, block spec.ContentSpec, list spec.ContentList) *Schema {
	t.Helper()
	sc, err := Compile(block, list, block.Roots)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return sc
}

func TestCompileAcceptsTheShippedShape(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{Source: spec.SourceSteamWorkshop})
	if sc.Acquisition.Key != "WorkshopItems" || sc.Activation.Key != "Mods" {
		t.Fatalf("write keys = %q / %q", sc.Acquisition.Key, sc.Activation.Key)
	}
	if !sc.ServerBoot() {
		t.Error("PZ acquires through server_boot")
	}
	if sc.NeedsSteamCMDLock() {
		t.Error("server_boot needs no steamcmd lock, so N instances can sync concurrently (D-30)")
	}
	if got := sc.ImpactFor(ChangeAdd); got != ImpactDownloadCycle {
		t.Errorf("impact for add = %s, want download_cycle", got)
	}
	if got := sc.ImpactFor(ChangeNote); got != ImpactNone {
		t.Errorf("impact for a note = %s; a note is documentation", got)
	}
	// An undeclared kind must not fall to none: a content change that took
	// effect without a restart is a server running a list it never loaded.
	if got := sc.ImpactFor(ChangeActivation); got != ImpactRestart {
		t.Errorf("impact for an undeclared kind = %s, want restart", got)
	}
	if sc.VerbatimActivation {
		t.Error("the default activation policy derives")
	}
}

// TestCompileRefusesOneKeyForBothNamespaces is the central check of the
// two-list model. One key cannot carry both lists: whichever write landed
// second would overwrite the first, and the ini would look fine either way.
func TestCompileRefusesOneKeyForBothNamespaces(t *testing.T) {
	block := pzBlock()
	block.Activation.WriteKey = block.Acquisition.WriteKey
	block.OwnedKeys = []string{"WorkshopItems"}

	_, err := Compile(block, spec.ContentList{}, nil)
	if err == nil {
		t.Fatal("a block writing both namespaces to WorkshopItems compiled; the acquisition " +
			"and activation lists are different id namespaces and one key cannot hold both")
	}
	if !errors.Is(err, ErrInvalidSchema) {
		t.Errorf("error does not wrap ErrInvalidSchema: %v", err)
	}
	if !strings.Contains(err.Error(), "WorkshopItems") {
		t.Errorf("error does not name the key: %v", err)
	}
}

// Two namespaces in the same file under different keys is the SHIPPED shape and
// must keep compiling: PZ writes both WorkshopItems= and Mods= into one ini.
func TestCompileAllowsTwoKeysInOneFile(t *testing.T) {
	block := pzBlock()
	if block.Acquisition.WriteFile != block.Activation.WriteFile {
		t.Fatal("fixture no longer shares a file, so this test proves nothing")
	}
	mustCompile(t, block, spec.ContentList{})
}

func TestCompileRefusesAMissingSeparator(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*spec.ContentSpec)
		want string
	}{
		{"acquisition", func(b *spec.ContentSpec) { b.Acquisition.Separator = "" }, "WorkshopItems"},
		{"activation", func(b *spec.ContentSpec) { b.Activation.Separator = "" }, "Mods"},
		{"whitespace", func(b *spec.ContentSpec) { b.Activation.Separator = "  " }, "Mods"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := pzBlock()
			tc.mut(&block)
			_, err := Compile(block, spec.ContentList{}, nil)
			if err == nil {
				t.Fatal("compiled with no usable separator; the wrong separator disables " +
					"every mod with no error, so it is never guessed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name the key: %v", err)
			}
		})
	}
}

func TestCompileRefusesAWriteKeyMissingFromOwnedKeys(t *testing.T) {
	block := pzBlock()
	block.OwnedKeys = []string{"WorkshopItems"} // Mods left out

	_, err := Compile(block, spec.ContentList{}, nil)
	if err == nil {
		t.Fatal("compiled with Mods missing from owned_keys; the write would be refused " +
			"later, in the middle of a stopped server's sync window")
	}
	if !strings.Contains(err.Error(), "Mods") || !strings.Contains(err.Error(), "owned_keys") {
		t.Errorf("error does not explain which key and why: %v", err)
	}
}

func TestCompileRefusesAnUnknownImpactKeyOrValue(t *testing.T) {
	t.Run("value", func(t *testing.T) {
		block := pzBlock()
		block.Impact["remove"] = "rebuid"
		if _, err := Compile(block, spec.ContentList{}, nil); err == nil {
			t.Fatal("a typo'd impact value compiled; it would have become ImpactNone, which " +
				"is a world-affecting removal that takes no archive")
		}
	})
	t.Run("key", func(t *testing.T) {
		block := pzBlock()
		block.Impact["remvoe"] = "rebuild"
		_, err := Compile(block, spec.ContentList{}, nil)
		if err == nil {
			t.Fatal("a typo'd impact key compiled, so removals would keep their default")
		}
		if !strings.Contains(err.Error(), "remvoe") {
			t.Errorf("error does not name the key: %v", err)
		}
	})
}

func TestCompileRefusesASourceMismatchBetweenManifestAndInstance(t *testing.T) {
	_, err := Compile(pzBlock(), spec.ContentList{Source: spec.SourceModrinth}, nil)
	if err == nil {
		t.Fatal("a block that is steam_workshop in the manifest and modrinth in the instance " +
			"compiled; the ids are in whichever namespace the source uses, and resolving " +
			"workshop ids as Modrinth slugs produces an empty list with no error")
	}
}

func TestCompileRefusesAnUnknownResolver(t *testing.T) {
	block := pzBlock()
	block.Activation.Resolver = "pz.resolve_mods"

	_, err := Compile(block, spec.ContentList{}, nil)
	if err == nil {
		t.Fatal("an unimplemented resolver name compiled; it would fail mid-sync with the " +
			"server already stopped")
	}
	if !errors.Is(err, ErrNoResolver) {
		t.Errorf("error does not wrap ErrNoResolver: %v", err)
	}
	// The message has to name what IS available, or the operator's next step
	// is to guess again.
	if !strings.Contains(err.Error(), "pz.resolve_mod_ids") {
		t.Errorf("error does not list the implemented resolvers: %v", err)
	}
}

func TestCompileCarriesTheInstanceActivationPolicy(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{
		ActivationPolicy: spec.ActivationPreserve,
	})
	if !sc.VerbatimActivation {
		t.Fatal("activation_policy: preserve must compile to verbatim activation; that is the " +
			"adoption switch")
	}
	if _, err := Compile(pzBlock(), spec.ContentList{ActivationPolicy: "keep"}, nil); err == nil {
		t.Error("an unknown activation policy compiled")
	}
}

// TestTheShippedManifestCompiles is what keeps pzBlock() honest: the real
// games/projectzomboid.yaml content block has to compile through the same path,
// so a manifest edit that breaks this package fails here rather than on a box.
func TestTheShippedManifestCompiles(t *testing.T) {
	raw, err := fs.ReadFile(games.FS, "projectzomboid.yaml")
	if err != nil {
		t.Fatalf("read the shipped manifest: %v", err)
	}
	m, diags, err := spec.ValidateBytes("games/projectzomboid.yaml", raw)
	if err != nil {
		t.Fatalf("validate the shipped manifest: %v\n%s", err, diags)
	}
	if len(m.Spec.Content) == 0 {
		t.Fatal("the shipped Project Zomboid manifest declares no content block")
	}
	for _, block := range m.Spec.Content {
		// The roots are templated, so they are passed through as-is: this
		// package never renders, and the test is about the rest of the block.
		sc, err := Compile(block, spec.ContentList{Source: block.Source}, block.Roots)
		if err != nil {
			t.Fatalf("compile shipped block %q: %v", block.Name, err)
		}
		if sc.Acquisition.Key == sc.Activation.Key {
			t.Fatalf("shipped block %q writes both namespaces to %q", block.Name,
				sc.Acquisition.Key)
		}
		if sc.Activation.Resolver != "" {
			if _, ok := GetResolver(sc.Activation.Resolver); !ok {
				t.Fatalf("shipped block %q names resolver %q, which this build does not "+
					"implement", block.Name, sc.Activation.Resolver)
			}
		}
	}
}
