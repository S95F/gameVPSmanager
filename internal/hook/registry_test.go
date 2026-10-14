package hook

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// shippedManifests loads every manifest gvm ships, validated.
func shippedManifests(t *testing.T) map[string]*spec.Manifest {
	t.Helper()
	files, err := fs.Glob(games.FS, "*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no shipped manifests: %v", err)
	}
	out := make(map[string]*spec.Manifest, len(files))
	for _, f := range files {
		raw, err := fs.ReadFile(games.FS, f)
		if err != nil {
			t.Fatal(err)
		}
		m, diags, err := spec.ValidateBytes(f, raw)
		if err != nil || diags.HasErrors() {
			t.Fatalf("%s does not validate: %v\n%s", f, err, diags)
		}
		out[f] = m
	}
	return out
}

// TestEveryHookEveryManifestNamesResolves is the assertion the plan asks for.
//
// A manifest naming a hook that does not exist must fail the BUILD, not fail at
// 3am on a first boot. Equally, a hook named in a phase it does not declare has
// to fail here: pz.first_boot moved into pre_start would ask for a boot-and-quit
// cycle before every single restart, which would look like a server that takes
// an hour to come back.
//
// It walks every phase of every shipped manifest, so a phase nobody thought to
// check is still covered, and it counts what it found -- a walk that quietly
// stopped matching would otherwise pass forever.
func TestEveryHookEveryManifestNamesResolves(t *testing.T) {
	manifests := shippedManifests(t)
	var named int
	used := map[string]bool{}

	for file, m := range manifests {
		t.Run(file, func(t *testing.T) {
			for _, p := range Phases() {
				names := HooksFor(m, p)
				for i, n := range names {
					named++
					used[n] = true
					h, err := Get(n)
					if err != nil {
						t.Errorf("hooks.%s[%d] = %q: %v", p, i, n, err)
						continue
					}
					if h.Name() != n {
						t.Errorf("hooks.%s[%d] = %q resolved to a hook calling itself %q",
							p, i, n, h.Name())
					}
					if !declares(h, p) {
						t.Errorf("hooks.%s[%d] = %q does not declare %s; it declares %s",
							p, i, n, p, phaseList(h.Phases()))
					}
				}
			}
			// The whole-manifest entry point has to agree with the per-phase
			// walk, because that is the one the loader will call.
			if err := ValidateManifest(m); err != nil {
				t.Errorf("ValidateManifest: %v", err)
			}
		})
	}

	if named < 15 {
		t.Fatalf("the shipped manifests name only %d hooks in total; the walk is not reading "+
			"them (projectzomboid alone names three)", named)
	}
	// Every implemented hook is used by something that ships. One that is not
	// is either dead code or a manifest that forgot it, and both are worth a
	// sentence in a review.
	for _, n := range Names() {
		if !used[n] {
			t.Errorf("%q is registered but no shipped manifest names it", n)
		}
	}
}

// TestTheRegistryKeysAreTheHookNames keeps `gvm game hooks`, the manifest's
// spelling and the Go type from drifting apart. A key that disagreed with its
// hook's Name() would make Get("steam.link_sdk") return a hook that reports
// itself as something else in every log line and audit row.
func TestTheRegistryKeysAreTheHookNames(t *testing.T) {
	for key, h := range registry {
		if h.Name() != key {
			t.Errorf("registry[%q] is a hook called %q", key, h.Name())
		}
		if strings.TrimSpace(h.Doc()) == "" {
			t.Errorf("%s has no Doc(); `gvm game hooks` and the manifest reference render it",
				key)
		}
		if len(h.Phases()) == 0 {
			t.Errorf("%s declares no phase, so no manifest can legally name it", key)
		}
		for _, p := range h.Phases() {
			if !p.Valid() {
				t.Errorf("%s declares phase %q, which is not a phase", key, p)
			}
		}
		// A name with no dot is a hook nobody can attribute to a game, and the
		// registry's whole readability rests on the prefix.
		if !strings.Contains(key, ".") {
			t.Errorf("%q has no <game>. prefix", key)
		}
	}
}

// TestTheTwoRegistriesNameTheSameSet is the check internal/spec's own comment
// promises exists here.
//
// spec.HookNames is the LOAD-time registry: it decides whether a manifest
// validates. This package is the RUNTIME one: it decides whether a hook can be
// run. Two lists that could disagree would mean a manifest that validates
// cleanly and then fails when a job tries to run it -- or, worse, one that is
// rejected for naming a hook that exists.
func TestTheTwoRegistriesNameTheSameSet(t *testing.T) {
	load := spec.HookNames()
	if len(load) == 0 {
		t.Fatal("spec.HookNames() is empty")
	}

	for name, support := range load {
		_, runnable := registry[name]
		switch support {
		case spec.SupportImplemented:
			if !runnable {
				t.Errorf("%q is SupportImplemented at load time but this build cannot run it; "+
					"a manifest naming it would validate and then fail in a job", name)
			}
		case spec.SupportPlanned:
			if runnable {
				t.Errorf("%q is SupportPlanned at load time but IS registered here; a manifest "+
					"naming it would be warned about and then run anyway", name)
			}
		default:
			t.Errorf("%q has support %v, which says nothing", name, support)
		}
	}
	for _, name := range Names() {
		if _, ok := load[name]; !ok {
			t.Errorf("%q is runnable but internal/spec will reject a manifest that names it",
				name)
		}
	}
}

// TestTheMinecraftHooksAreRegisteredAndRunnable verifies that the v1.1
// Minecraft hooks are in the registry and callable via Get.
func TestTheMinecraftHooksAreRegisteredAndRunnable(t *testing.T) {
	for _, n := range []string{
		"mc.accept_eula", "mc.select_jdk", "mc.write_properties", "mc.gamerule_fallback",
	} {
		if _, ok := registry[n]; !ok {
			t.Errorf("%q is not in the registry", n)
		}
		h, err := Get(n)
		if err != nil {
			t.Errorf("Get(%q) failed: %v", n, err)
			continue
		}
		if h.Name() != n {
			t.Errorf("Get(%q).Name() = %q", n, h.Name())
		}
		if len(h.Phases()) == 0 {
			t.Errorf("%q declares no phases", n)
		}
	}
}

func TestGetExplainsAnUnknownName(t *testing.T) {
	_, err := Get("steam.link_sdk64")
	if err == nil {
		t.Fatal("Get accepted a name the registry does not hold")
	}
	if !errors.Is(err, ErrUnknownHook) {
		t.Errorf("error does not wrap ErrUnknownHook: %v", err)
	}
	// The overwhelmingly likely cause is a typo in a manifest, and an error
	// without a suggestion sends the author to read Go source.
	if !strings.Contains(err.Error(), "steam.link_sdk") {
		t.Errorf("error suggests nothing: %v", err)
	}

	// An unrelated name gets no suggestion rather than a misleading one.
	_, err = Get("completely.different")
	if err == nil {
		t.Fatal("Get accepted an unrelated name")
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("a suggestion was offered for an unrelated name: %v", err)
	}
	// It still says what IS nameable, which is the actionable half.
	if !strings.Contains(err.Error(), "pz.first_boot") {
		t.Errorf("error does not list the registry: %v", err)
	}
}

func TestResolveRefusesTheWrongPhase(t *testing.T) {
	// pz.first_boot is post_install. In pre_start it would ask for a
	// boot-and-quit cycle before every restart.
	_, err := Resolve(PhasePreStart, []string{"pz.first_boot"})
	if err == nil {
		t.Fatal("Resolve accepted a hook in a phase it does not declare")
	}
	if !errors.Is(err, ErrWrongPhase) {
		t.Errorf("error does not wrap ErrWrongPhase: %v", err)
	}
	if !strings.Contains(err.Error(), "post_install") {
		t.Errorf("the error does not say which phase it DOES run in: %v", err)
	}
}

// TestResolveReportsEveryProblem: an operator fixing a manifest wants the whole
// list, and a validation that reveals its problems one run at a time turns a
// review into a guessing game.
func TestResolveReportsEveryProblem(t *testing.T) {
	_, err := Resolve(PhasePostInstall, []string{"steam.nope", "pz.apply_sandbox", "also.nope"})
	if err == nil {
		t.Fatal("Resolve accepted two unknown names")
	}
	for _, want := range []string{"steam.nope", "also.nope", "pz.apply_sandbox"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestInPhaseAndAll(t *testing.T) {
	if len(All()) != len(registry) {
		t.Fatalf("All() returned %d of %d hooks", len(All()), len(registry))
	}
	// Sorted, because `gvm game hooks` prints it and a map's order would make
	// the output differ between runs.
	names := Names()
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("Names() is not sorted: %v", names)
		}
	}
	post := InPhase(PhasePostInstall)
	if len(post) == 0 {
		t.Fatal("no hook runs in post_install, which is where six of them run")
	}
	for _, h := range post {
		if !declares(h, PhasePostInstall) {
			t.Errorf("InPhase(post_install) returned %s", h.Name())
		}
	}
	if len(InPhase(PhasePreBackup)) != 0 {
		t.Error("a hook declares pre_backup, which no shipped manifest uses and nothing tests")
	}
}

func TestHooksForCoversEveryPhase(t *testing.T) {
	// A HookSpec with one distinct name per phase, so a HooksFor that read the
	// wrong field would return the wrong string rather than an empty slice --
	// which an "is it non-empty" check would miss.
	m := &spec.Manifest{}
	m.Spec.Hooks = spec.HookSpec{
		Validate:    []string{"validate"},
		PostInstall: []string{"post_install"},
		PostContent: []string{"post_content"},
		PreStart:    []string{"pre_start"},
		PostStart:   []string{"post_start"},
		PreBackup:   []string{"pre_backup"},
		PostBackup:  []string{"post_backup"},
	}
	for _, p := range Phases() {
		got := HooksFor(m, p)
		if len(got) != 1 || got[0] != string(p) {
			t.Errorf("HooksFor(%s) = %v", p, got)
		}
	}
	if got := HooksFor(nil, PhasePostInstall); got != nil {
		t.Errorf("HooksFor(nil) = %v", got)
	}
}

// TestValidateManifestRejectsAnUnknownHook is the teeth check for the build
// gate: the thing being asserted is that ValidateManifest REFUSES, so a
// manifest that names nonsense has to be shown to fail.
func TestValidateManifestRejectsAnUnknownHook(t *testing.T) {
	m := &spec.Manifest{}
	m.Spec.Hooks.PostInstall = []string{"steam.home_layout", "steam.definitely_not_a_hook"}
	err := ValidateManifest(m)
	if err == nil {
		t.Fatal("ValidateManifest accepted an unknown hook name; a manifest naming a hook that " +
			"does not exist would then fail at 3am on first boot instead of here")
	}
	if !strings.Contains(err.Error(), "steam.definitely_not_a_hook") {
		t.Errorf("the error does not name the offending hook: %v", err)
	}
}
