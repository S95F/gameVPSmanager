package spec

import (
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/games"
)

// The shipped manifests are held to a stricter standard than an operator's own:
// they are the examples everyone copies, they are what `gvm game try` is
// measured against, and a warning that nobody can clear teaches the whole
// audience to scroll past warnings. So every file in games/ must validate with
// zero errors and with no warning that is not deliberate and named below.

// shippedGames is the v1 set of section 9. It is written out rather than read
// off the directory so that DELETING a manifest fails here too: a game that
// silently stops shipping is exactly as bad as one that stops validating, and a
// test that derives its expectations from the thing under test proves nothing.
var shippedGames = []string{
	"7daystodie",
	"7daystodie-telnet",
	"ark-se",
	"corekeeper",
	"cs2",
	"enshrouded",
	"generic-steam",
	"minecraft-forge",
	"minecraft-neoforge",
	"minecraft-paper",
	"palworld",
	"projectzomboid",
	"rust",
	"satisfactory",
	"valheim",
	"vrising",
}

// allowedWarnings is the complete set of warnings a shipped manifest may carry,
// per game id. Every entry needs a reason that stays true; anything else is a
// manifest bug.
var allowedWarnings = map[string][]string{
	// websocket_rcon is a known control kind that is not yet implemented in this
	// build (gvm v1.1). The dialect is valid; the control channel renders as
	// unavailable until the kind is wired up.
	"rust": {"control kind \"websocket_rcon\" is known but not implemented in this build"},
}

func shippedManifest(t *testing.T, id string) (*Manifest, Diags) {
	t.Helper()
	raw, err := fs.ReadFile(games.FS, id+".yaml")
	if err != nil {
		t.Fatalf("read %s.yaml: %v", id, err)
	}
	m, diags, err := ValidateBytes("games/"+id+".yaml", raw)
	if err != nil {
		t.Fatalf("%s.yaml: %v\n%s", id, err, diags)
	}
	return m, diags
}

func TestShippedSetIsExactlyTheV1List(t *testing.T) {
	ents, err := fs.ReadDir(games.FS, ".")
	if err != nil {
		t.Fatalf("read embedded games: %v", err)
	}
	var got []string
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok {
			t.Errorf("games/%s is embedded but is not a manifest", e.Name())
			continue
		}
		got = append(got, id)
	}
	sort.Strings(got)
	if strings.Join(got, " ") != strings.Join(shippedGames, " ") {
		t.Errorf("embedded games are [%s], want [%s]",
			strings.Join(got, " "), strings.Join(shippedGames, " "))
	}
}

func TestEveryShippedManifestValidatesWithNoErrors(t *testing.T) {
	for _, id := range shippedGames {
		t.Run(id, func(t *testing.T) {
			_, diags := shippedManifest(t, id)
			if errs := diags.Errors(); len(errs) > 0 {
				t.Fatalf("%d error(s):\n%s", len(errs), errs)
			}
		})
	}
}

func TestShippedManifestWarningsAreDeliberate(t *testing.T) {
	for _, id := range shippedGames {
		t.Run(id, func(t *testing.T) {
			_, diags := shippedManifest(t, id)

			var got []string
			for _, d := range diags {
				if d.Sev == SevWarning {
					got = append(got, d.Msg)
				}
			}
			want := allowedWarnings[id]
			if len(got) != len(want) {
				t.Fatalf("%d warning(s), want %d:\ngot:  %s\nwant: %s",
					len(got), len(want), strings.Join(got, "\n      "), strings.Join(want, "\n      "))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("warning %d:\n got %q\nwant %q", i, got[i], want[i])
				}
			}
		})
	}
}

// The catalog keys on the FILENAME and never compares it to metadata.id, so a
// mismatch would make `gvm game which pz` load a file whose manifest calls
// itself something else — and the pin would then record that other name.
func TestShippedManifestIDMatchesItsFilename(t *testing.T) {
	for _, id := range shippedGames {
		t.Run(id, func(t *testing.T) {
			m, _ := shippedManifest(t, id)
			if m.Metadata.ID != id {
				t.Errorf("games/%s.yaml declares metadata.id %q", id, m.Metadata.ID)
			}
			if m.Metadata.Name == "" {
				t.Error("no metadata.name; the dashboard has nothing to title the page with")
			}
		})
	}
}

// The embedded floor has to behave exactly like a real directory, because
// SystemCatalog stacks the two and the resolution order is only testable if
// they are interchangeable.
func TestEmbeddedCatalogResolvesEveryShippedGame(t *testing.T) {
	cat := NewCatalog(EmbeddedSource(games.FS, "embedded games"))

	ids, err := cat.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if strings.Join(ids, " ") != strings.Join(shippedGames, " ") {
		t.Fatalf("List = [%s], want [%s]", strings.Join(ids, " "), strings.Join(shippedGames, " "))
	}

	for _, id := range ids {
		m, diags, err := cat.Load(id, nil)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if diags.HasErrors() {
			t.Fatalf("%s:\n%s", id, diags)
		}
		if m.Origin != OriginEmbedded {
			t.Errorf("%s: origin %q, want %q", id, m.Origin, OriginEmbedded)
		}
		// The pin copies Raw verbatim and refuses a manifest whose digest does
		// not describe its own bytes, so a catalog that fills these in wrongly
		// makes every instance unpinnable.
		if m.SHA256 != sha256Hex(m.Raw) {
			t.Errorf("%s: sha256 does not describe Raw", id)
		}
	}
}

// Every manifest has to produce cgroup limits from its own declared defaults.
// A manifest whose sizing only works once an operator overrides a var would
// fail at `gvm create`, on a box where nobody has yet typed anything.
func TestShippedSizingComputesFromDeclaredDefaults(t *testing.T) {
	for _, id := range shippedGames {
		t.Run(id, func(t *testing.T) {
			m, _ := shippedManifest(t, id)
			vars := VarValues{}
			for name, v := range m.Spec.Vars {
				vars[name] = v.Default
			}
			lim, err := m.Spec.Sizing.Memory(vars)
			if err != nil {
				t.Fatalf("sizing: %v", err)
			}
			if lim.HighBytes >= lim.MaxBytes {
				t.Errorf("MemoryHigh %s is not below MemoryMax %s", lim.High, lim.Max)
			}
			floor := int64(m.Spec.Sizing.RAMFloorMB) << 20
			if floor > 0 && lim.MaxBytes < floor {
				t.Errorf("MemoryMax %s is below the manifest's own ram_floor_mb %d",
					lim.Max, m.Spec.Sizing.RAMFloorMB)
			}
		})
	}
}

// capability_test.go derives capabilities from manifests built in Go, which
// proves the derivation. This proves the SHIPPED files feed it the right facts —
// a manifest can be perfectly valid and still describe a dashboard that lies.
func TestShippedCapabilitiesDescribeWhatEachGameCanActuallyDo(t *testing.T) {
	// Valheim and Core Keeper have no query channel at all, ever. D-22 wants
	// those buttons OMITTED rather than greyed: greying one implies it will
	// work later, and it never will.
	for _, id := range []string{"valheim", "corekeeper"} {
		m, _ := shippedManifest(t, id)
		caps := Capabilities(m, nil, Live{State: StateReady})
		if s, ok := FindCap(caps, CapPlayers); ok && s.Supported {
			t.Errorf("%s: players reports supported (%+v); this game has no query channel", id, s)
		}
		if s, ok := FindCap(caps, CapConsoleWrite); !ok || !s.Supported {
			t.Errorf("%s: the console is the ONLY command channel and it reports unsupported", id)
		}
	}

	// Project Zomboid's RCON does not bind for the whole of a first boot, which
	// with a large workshop list is 40-60 minutes. That has to read as the
	// manifest's own sentence, not as a broken dashboard.
	m, _ := shippedManifest(t, "projectzomboid")
	booting := Live{State: StateActivating, ControlReady: false}
	ctl, ok := FindCap(Capabilities(m, nil, booting), CapControl)
	if !ok || !ctl.Supported || ctl.Available {
		t.Fatalf("control while activating = %+v; want supported and unavailable", ctl)
	}
	if ctl.Reason != m.Spec.Control.NotReadyReason {
		t.Errorf("reason %q, want the manifest's own not_ready_reason %q",
			ctl.Reason, m.Spec.Control.NotReadyReason)
	}
}

// Project Zomboid is the manifest that has to encode everything the bash suite
// knew. Each assertion below is a fact from legacy/projectzomboid-bash that was
// learned the hard way and is invisible in the YAML if you are not looking for
// it; losing any of them reads as a working server that is subtly wrong.
func TestProjectZomboidKeepsTheLegacyBashContract(t *testing.T) {
	m, _ := shippedManifest(t, "projectzomboid")
	s := m.Spec

	// install.sh installs 380870 and the workshop lives under the GAME appid.
	// Swapping them downloads nothing and reports success.
	if got := s.Provider.SteamCMD.InstallAppID; got != 380870 {
		t.Errorf("install_appid %d, want 380870 (the server app, not the game)", got)
	}
	if got := s.Content[0].WorkshopAppID; got != 108600 {
		t.Errorf("workshop_appid %d, want 108600 (the game app, not the server)", got)
	}

	// sync-mods.sh: the two lists are different namespaces and both persist.
	acq, act := s.Content[0].Acquisition, s.Content[0].Activation
	if acq.WriteKey != "WorkshopItems" || act.WriteKey != "Mods" {
		t.Errorf("content writes %q/%q, want WorkshopItems/Mods", acq.WriteKey, act.WriteKey)
	}
	if acq.Separator != ";" || act.Separator != ";" {
		t.Errorf("separators %q/%q, want ; — a comma breaks the list with no error",
			acq.Separator, act.Separator)
	}
	if act.Resolver != "pz.resolve_mod_ids" {
		t.Errorf("activation resolver %q; the mod.info B41/B42 scoring is Go, not a YAML predicate (D-21)", act.Resolver)
	}

	// firewall.sh opens both UDP ports. 16261 completes the handshake and 16262
	// carries the data: one without the other is a server people can see and
	// never join. RCON is never public.
	want := map[string]struct {
		proto  string
		def    int
		expose Expose
	}{
		"game":   {"udp", 16261, ExposePublic},
		"direct": {"udp", 16262, ExposePublic},
		"rcon":   {"tcp", 27015, ExposeNone},
	}
	for name, w := range want {
		p, ok := s.PortByName(name)
		if !ok {
			t.Fatalf("no port named %q", name)
		}
		if p.Proto != w.proto || p.Default != w.def || p.Expose != w.expose {
			t.Errorf("port %s = %s/%d %s, want %s/%d %s",
				name, p.Proto, p.Default, p.Expose, w.proto, w.def, w.expose)
		}
	}

	// The unit's ExecStop stuffed `save`, slept, then `quit`. The ladder that
	// replaces it must stop on the CONSOLE, because RCON does not bind for the
	// whole of a 40-minute first boot (D-08).
	if via := s.Shutdown.Steps[0].Via; via != ViaConsole {
		t.Errorf("first shutdown rung is %q; PZ's RCON is unavailable exactly when a stop is most likely", via)
	}
	var ladder Duration
	for _, st := range s.Shutdown.Steps {
		ladder += st.Wait
	}
	if s.Shutdown.Budget <= ladder {
		t.Errorf("budget %s does not cover the %s ladder; TimeoutStopSec would SIGKILL mid-save",
			s.Shutdown.Budget, ladder)
	}

	// install.sh passes -adminpassword ONLY on the first boot, because that is
	// when the account is created. Unguarded, it would be on every restart's
	// argv and in every journal line that logs one.
	var guardedSecret bool
	for _, a := range s.Runtime.Args {
		if !strings.Contains(a.Value, ".Secret.admin_password") {
			continue
		}
		guardedSecret = true
		if a.When == nil || a.When.Phase != string(PhaseInstall) {
			t.Errorf("the admin password argument is not guarded to the install phase")
		}
	}
	if !guardedSecret {
		t.Error("no -adminpassword argument; the first boot is what creates the admin account")
	}

	// rebuild.sh's promise: "accounts, passwords and server settings are kept".
	protected := strings.Join(s.Backup.DestroyProtected, " ")
	for _, keep := range []string{"/Server", "/db"} {
		if !strings.Contains(protected, keep) {
			t.Errorf("destroy_protected %v does not keep %s", s.Backup.DestroyProtected, keep)
		}
	}
	// ...and both halves of the save. The _player directory holds the
	// characters; archiving only the world loses everyone's progress.
	if len(s.Backup.WorldPaths) != 2 || !strings.HasSuffix(s.Backup.WorldPaths[1], "_player") {
		t.Errorf("world_paths %v; the characters live in the _player directory", s.Backup.WorldPaths)
	}

	// common.sh reads the preset list out of the server's own install tree, so
	// it is whatever this build ships rather than a table that drifts.
	if s.Presets == nil || !strings.Contains(s.Presets.SourceGlob, "media/lua/shared/Sandbox") {
		t.Error("presets do not come from the server's own Sandbox directory")
	}
}
