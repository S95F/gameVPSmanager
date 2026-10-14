package hook

import (
	"fmt"
	"sort"
	"strings"
)

// registry is the v1 hook set of section 8.8, and it is CLOSED.
//
// A map literal in Go source, not a plugin directory, not a table in Postgres,
// not a name the manifest can construct. A hook is argv-adjacent code that runs
// as root in a job process or as the instance user in the runner, and a
// registry an operator could extend by dropping a file somewhere would make
// `hooks: [something.clever]` arbitrary execution on every instance at the next
// restart -- the same reason D-18 keeps manifests off the database.
//
// The names are not derived from the types. registry_test.go asserts that every
// key equals its hook's Name(), which is what keeps `gvm game hooks` and a
// manifest's spelling from drifting apart, and that this set matches
// internal/spec's load-time list exactly -- two registries that could disagree
// would mean a manifest that validates and then fails when something runs it.
var registry = map[string]Hook{
	// Project Zomboid. These three are the reason D-21 exists: mod resolution
	// is a scoring problem over files on disk, and a YAML predicate like
	// `field('versionMin') >= 42` has unspecified numeric-versus-lexicographic
	// semantics against a real versionMin=42.0.1 -- where a wrong answer
	// SILENTLY OMITS A MOD while the server boots normally.
	"pz.first_boot":      pzFirstBoot{},
	"pz.apply_sandbox":   pzApplySandbox{},
	"pz.resolve_mod_ids": pzResolveModIDs{},

	// Steam. Shared by every steamcmd-installed game, which is the whole point:
	// corekeeper.yaml ships with zero lines of Go because these three already
	// exist.
	"steam.home_layout": steamHomeLayout{},
	"steam.link_sdk":    steamLinkSDK{},
	"steam.appid_shim":  steamAppIDShim{},

	// One game each, and each one a file the server generates from its own
	// shipped default rather than a template gvm invents.
	"pal.settings_seed": palSettingsSeed{},
	"sdtd.seed_config":  sdtdSeedConfig{},

	// Minecraft. These four complete the provider installation and the per-start
	// JVM selection. They are separate names for the same reason as the Steam
	// hooks: a manifest can take two of them and not the third.
	"mc.accept_eula":       mcAcceptEULA{},
	"mc.select_jdk":        mcSelectJDK{},
	"mc.write_properties":  mcWriteProperties{},
	"mc.gamerule_fallback": mcGameruleFallback{},
}

// Get returns a hook by name.
//
// The error names the nearest spelling, because the overwhelmingly likely cause
// is a typo in a manifest and "unknown hook: steam.link_sdk64" without a
// suggestion sends an author to read Go source.
func Get(name string) (Hook, error) {
	if h, ok := registry[name]; ok {
		return h, nil
	}
	suggestion := ""
	if near := nearest(name, Names()); near != "" {
		suggestion = fmt.Sprintf(" (did you mean %q?)", near)
	}
	return nil, fmt.Errorf("%w: %q%s; this build knows %s",
		ErrUnknownHook, name, suggestion, strings.Join(Names(), ", "))
}

// All returns every registered hook, sorted by name.
//
// `go doc ./internal/hook` plus this function is the complete list, which is
// what section 8.8 means by the registry being documentation rather than
// something to go looking for.
func All() []Hook {
	out := make([]Hook, 0, len(registry))
	for _, n := range Names() {
		out = append(out, registry[n])
	}
	return out
}

// Names returns every registered hook name, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// InPhase returns every hook that may be named in a phase, sorted. `gvm game
// hooks` prints this per phase.
func InPhase(p Phase) []Hook {
	var out []Hook
	for _, h := range All() {
		if declares(h, p) {
			out = append(out, h)
		}
	}
	return out
}

// nearest is a cheap edit-distance suggestion over the registry's names.
//
// Its own implementation rather than internal/spec's, because this package must
// not import spec's unexported helpers and the alternative -- exporting a
// string-distance function from the schema package -- would be a worse coupling
// than twenty lines.
func nearest(want string, have []string) string {
	best, bestD := "", 1<<30
	for _, h := range have {
		d := distance(want, h)
		// Only suggest something genuinely close. A suggestion for an
		// unrelated name is worse than none: it sends the author to check a
		// hook that was never the point.
		if d < bestD && d <= 1+len(want)/3 {
			best, bestD = h, d
		}
	}
	return best
}

func distance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
