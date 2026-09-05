package spec

import (
	"strings"
	"testing"
)

// valheimLike is the console-only shape of section 9.2: no RCON of any kind, so
// the dashboard must OMIT players/broadcast/kick with a real reason rather than
// grey them out, because a greyed control implies it will work later.
func valheimLike() *Manifest {
	return &Manifest{
		Metadata: GameMeta{ID: "valheim", Name: "Valheim"},
		Spec: GameSpec{
			Provider: ProviderSpec{Kind: ProviderSteamCMD, SteamCMD: &SteamCMDSpec{Platform: PlatformLinux}},
			Runtime:  RuntimeSpec{Console: ConsoleSpec{Mode: ConsoleModePipe}},
			Control:  nil,
			Verbs:    map[string]Verb{"save": {Console: "save"}},
			Backup:   BackupSpec{WorldPaths: []string{"{{ .Data }}/worlds_local"}},
		},
	}
}

// pzLike is the rich shape of section 9.1: RCON that does NOT bind until the
// server has finished booting, which for a large workshop list is 40-60 minutes.
func pzLike() *Manifest {
	return &Manifest{
		Metadata: GameMeta{ID: "projectzomboid", Name: "Project Zomboid"},
		Spec: GameSpec{
			Provider: ProviderSpec{Kind: ProviderSteamCMD, SteamCMD: &SteamCMDSpec{Platform: PlatformLinux}},
			Runtime:  RuntimeSpec{Console: ConsoleSpec{Mode: ConsoleModePTY}},
			Control: &ControlSpec{
				Kind:                   string(ControlSourceRCON),
				Dialect:                DialectSimple,
				AvailableOnlyWhenReady: true,
				NotReadyReason:         "RCON does not bind until Project Zomboid finishes booting",
			},
			Verbs: map[string]Verb{
				"save":      {Console: "save", Control: "save"},
				"players":   {Control: "players"},
				"broadcast": {Control: "servermsg"},
			},
			Content: []ContentSpec{{
				Name:        "workshop",
				Source:      SourceSteamWorkshop,
				Acquisition: ContentAcquisition{Mechanism: AcquireServerBoot},
			}},
			Configs: []ConfigFile{{Name: "server_ini", Format: "pz_ini", Owner: OwnerServer}},
			Presets: &PresetSpec{SourceGlob: "{{ .Install }}/media/lua/shared/Sandbox/*.lua"},
			Backup:  BackupSpec{WorldPaths: []string{"{{ .Data }}/Saves"}},
		},
	}
}

func managed() *Instance { return &Instance{Spec: InstanceSpec{ManagedInstall: true}} }
func adopted() *Instance { return &Instance{Spec: InstanceSpec{ManagedInstall: false}} }
func capOf(t *testing.T, m *Manifest, in *Instance, l Live, c Capability) CapState {
	t.Helper()
	s, ok := FindCap(Capabilities(m, in, l), c)
	if !ok {
		t.Fatalf("%s was not derived at all", c)
	}
	return s
}

// TestAGameWithNoPlayerListReportsUnsupported is the Valheim case: Supported is
// a fact about the GAME, and no amount of waiting will make a query channel
// appear.
func TestAGameWithNoPlayerListReportsUnsupported(t *testing.T) {
	got := capOf(t, valheimLike(), managed(), Live{State: StateReady, ControlReady: true}, CapPlayers)

	if got.Supported {
		t.Fatal("Valheim has no query channel; claiming otherwise renders a button that cannot work")
	}
	if got.Available {
		t.Error("an unsupported capability can never be available")
	}
	if got.Reason == "" {
		t.Error("the dashboard omits this button and doctor explains why; both need the reason")
	}
	if !strings.Contains(got.Reason, "Valheim") {
		t.Errorf("the reason should name the game: %q", got.Reason)
	}
}

// TestControlIsSupportedButUnavailableWhileBooting is the whole point of the
// tri-state. During PZ's first boot the answer is not "broken" and not "ready";
// it is "supported, not yet, and here is why".
func TestControlIsSupportedButUnavailableWhileBooting(t *testing.T) {
	got := capOf(t, pzLike(), managed(), Live{State: StateActivating, ControlReady: false}, CapControl)

	if !got.Supported {
		t.Fatal("source_rcon is implemented in this build")
	}
	if got.Available {
		t.Fatal("RCON has not bound yet")
	}
	if got.Reason != "RCON does not bind until Project Zomboid finishes booting" {
		t.Errorf("the manifest's own not_ready_reason must reach the tooltip verbatim, got %q", got.Reason)
	}
}

func TestVerbsInheritTheirChannelsAvailability(t *testing.T) {
	booting := Live{State: StateActivating, ControlReady: false}
	ready := Live{State: StateReady, ControlReady: true}

	players := capOf(t, pzLike(), managed(), booting, CapPlayers)
	if !players.Supported || players.Available {
		t.Fatalf("players should be supported and unavailable while RCON is still binding: %+v", players)
	}
	if players.Channel != ChannelControl {
		t.Errorf("channel = %q, want control", players.Channel)
	}
	if players.Reason == "" {
		t.Error("a disabled button needs its tooltip")
	}

	if got := capOf(t, pzLike(), managed(), ready, CapPlayers); !got.Available {
		t.Errorf("players should be available once the control channel is up: %+v", got)
	}
}

// TestAPlannedControlKindIsNotSupportedAndSaysSo is what lets 7daystodie.yaml
// ship in v1 declaring `kind: telnet` (10.2).
func TestAPlannedControlKindIsNotSupportedAndSaysSo(t *testing.T) {
	m := pzLike()
	m.Spec.Control.Kind = string(ControlTelnet)

	got := capOf(t, m, managed(), Live{State: StateReady, ControlReady: true}, CapControl)
	if got.Supported || got.Available {
		t.Fatalf("a control kind this build does not implement is not supported: %+v", got)
	}
	if !strings.Contains(got.Reason, plannedRelease) {
		t.Errorf("the reason must name the release that fixes it, got %q", got.Reason)
	}

	// And the verbs that ride on it must not claim to work either.
	if p := capOf(t, m, managed(), Live{State: StateReady}, CapPlayers); p.Supported {
		t.Errorf("players rides on a channel that does not exist yet: %+v", p)
	}
}

func TestConsoleCapabilitiesFollowTheProcessState(t *testing.T) {
	for _, tc := range []struct {
		state     string
		wantRead  bool
		wantWrite bool
	}{
		{state: StateInactive, wantRead: false, wantWrite: false},
		{state: StateActivating, wantRead: true, wantWrite: true},
		{state: StateReady, wantRead: true, wantWrite: true},
		// Read survives a crash on purpose: the last lines in the ring are
		// exactly what an operator needs at the moment the unit fails, and
		// hiding them then is the opposite of useful.
		{state: StateFailed, wantRead: true, wantWrite: false},
		{state: StateStopping, wantRead: true, wantWrite: false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			l := Live{State: tc.state}
			if got := capOf(t, pzLike(), managed(), l, CapConsoleRead); got.Available != tc.wantRead {
				t.Errorf("console.read available = %v, want %v", got.Available, tc.wantRead)
			}
			got := capOf(t, pzLike(), managed(), l, CapConsoleWrite)
			if got.Available != tc.wantWrite {
				t.Errorf("console.write available = %v, want %v", got.Available, tc.wantWrite)
			}
			if !got.Available && got.Reason == "" {
				t.Error("an unavailable capability needs a reason")
			}
		})
	}
}

func TestAGameWithNoConsoleReportsConsoleUnsupported(t *testing.T) {
	m := valheimLike()
	m.Spec.Runtime.Console.Mode = ConsoleModeNone

	got := capOf(t, m, managed(), Live{State: StateReady}, CapConsoleRead)
	if got.Supported || got.Reason == "" {
		t.Errorf("a game that captures nothing must not offer a console page: %+v", got)
	}
}

// TestWorkshopEditsAreBlockedWhileTheServerIsRunning encodes PZ's acquisition
// mechanism: the game downloads its own items during boot, so changing the list
// under it would take effect at an unpredictable moment, half-applied.
func TestWorkshopEditsAreBlockedWhileTheServerIsRunning(t *testing.T) {
	running := capOf(t, pzLike(), managed(), Live{State: StateReady}, CapWorkshopMods)
	if !running.Supported {
		t.Fatal("the manifest declares a steam_workshop content block")
	}
	if running.Available {
		t.Fatal("the list may not be edited while the server that downloads it is up")
	}
	if !strings.Contains(running.Reason, "stop the server") {
		t.Errorf("the reason must say what to do, got %q", running.Reason)
	}

	if stopped := capOf(t, pzLike(), managed(), Live{State: StateInactive}, CapWorkshopMods); !stopped.Available {
		t.Errorf("a stopped server's mod list is editable: %+v", stopped)
	}
}

func TestCurseForgeWaitsOnBlockedFiles(t *testing.T) {
	m := pzLike()
	m.Spec.Content = append(m.Spec.Content, ContentSpec{Name: "cf", Source: SourceCurseForge})

	got := capOf(t, m, managed(), Live{State: StateInactive, Blocked: 3}, CapCurseForge)
	if !got.Supported || got.Available {
		t.Fatalf("blocked files make the sync unavailable, not unsupported: %+v", got)
	}
	if !strings.Contains(got.Reason, "3 files") {
		t.Errorf("the reason must carry the count, got %q", got.Reason)
	}
}

// TestPresetsNeedTheServerToHaveBootedOnce: the preset list is the server's OWN
// shipped files, so before the first install there is nothing to list.
func TestPresetsNeedTheServerToHaveBootedOnce(t *testing.T) {
	before := capOf(t, pzLike(), managed(), Live{State: StateInactive}, CapSandboxPresets)
	if !before.Supported || before.Available {
		t.Fatalf("supported by the manifest, unavailable until the files exist: %+v", before)
	}
	if before.Reason == "" {
		t.Error("a reason is what stops this reading as a bug")
	}
	if after := capOf(t, pzLike(), managed(), Live{State: StateInactive, PresetFiles: 12}, CapSandboxPresets); !after.Available {
		t.Errorf("with files on disk the preset list is available: %+v", after)
	}
}

func TestRebuildAndManagedInstallDependOnWhoOwnsTheTree(t *testing.T) {
	l := Live{State: StateInactive}

	for _, c := range []Capability{CapRebuild, CapManagedInstall} {
		got := capOf(t, pzLike(), adopted(), l, c)
		if !got.Supported {
			t.Errorf("%s: the game supports it; the INSTANCE is the constraint", c)
		}
		if got.Available {
			t.Errorf("%s: gvm does not own an adopted install tree", c)
		}
		if got.Reason == "" {
			t.Errorf("%s: the operator needs to be told to promote first", c)
		}
	}

	for _, c := range []Capability{CapRebuild, CapManagedInstall} {
		if got := capOf(t, pzLike(), managed(), l, c); !got.Available {
			t.Errorf("%s should be available on a managed install: %+v", c, got)
		}
	}
}

func TestRebuildIsUnsupportedWithoutWorldPaths(t *testing.T) {
	m := pzLike()
	m.Spec.Backup.WorldPaths = nil

	got := capOf(t, m, managed(), Live{State: StateInactive}, CapRebuild)
	if got.Supported {
		t.Fatal("with no world_paths there is nothing a rebuild could preserve")
	}
}

func TestAnAdoptedProviderCannotCheckForUpdates(t *testing.T) {
	m := pzLike()
	m.Spec.Provider = ProviderSpec{Kind: ProviderAdopted}

	got := capOf(t, m, managed(), Live{State: StateInactive}, CapUpdateCheck)
	if got.Supported || got.Reason == "" {
		t.Errorf("the adopted provider reports no available version: %+v", got)
	}
}

// TestAServerOwnedConfigIsOnlyEditableWhileStopped falls out of section 8.4:
// the game rewrites the file when it stops, so an edit made while it runs is
// silently discarded.
func TestAServerOwnedConfigIsOnlyEditableWhileStopped(t *testing.T) {
	if got := capOf(t, pzLike(), managed(), Live{State: StateReady}, CapConfigEdit); got.Available {
		t.Errorf("editing a server-owned file while it runs loses the edit: %+v", got)
	}
	if got := capOf(t, pzLike(), managed(), Live{State: StateInactive}, CapConfigEdit); !got.Available {
		t.Errorf("a stopped server's config is editable: %+v", got)
	}
}

func TestPlatformShimIsAnInformationalBadge(t *testing.T) {
	m := valheimLike()
	m.Spec.Runtime.Wrapper = &Wrapper{Kind: "xvfb"}

	got := capOf(t, m, managed(), Live{}, CapPlatformShim)
	if !got.Supported || got.Reason == "" {
		t.Errorf("the badge exists so nobody spends an afternoon finding the extra process: %+v", got)
	}
	if plain := capOf(t, valheimLike(), managed(), Live{}, CapPlatformShim); plain.Supported {
		t.Errorf("a game with no wrapper and a linux build needs no badge: %+v", plain)
	}
}

func TestCapabilityOrderIsStable(t *testing.T) {
	// The dashboard renders this slice directly; a set-iteration order would
	// reshuffle the button row on every five-second poll.
	first := Capabilities(pzLike(), managed(), Live{State: StateReady})
	for i := 0; i < 20; i++ {
		got := Capabilities(pzLike(), managed(), Live{State: StateReady})
		if len(got) != len(first) {
			t.Fatalf("derived %d capabilities, then %d", len(first), len(got))
		}
		for j := range got {
			if got[j].Cap != first[j].Cap {
				t.Fatalf("position %d was %s, now %s", j, first[j].Cap, got[j].Cap)
			}
		}
	}
	if len(first) != len(capabilityOrder) {
		t.Errorf("derived %d capabilities but the order list has %d; one is never rendered",
			len(first), len(capabilityOrder))
	}
}

func TestCapabilitiesWithoutAnInstance(t *testing.T) {
	// The game page renders a manifest nobody has created an instance from yet.
	got := Capabilities(pzLike(), nil, Live{})
	if len(got) == 0 {
		t.Fatal("a manifest alone still has capabilities")
	}
	if s, _ := FindCap(got, CapManagedInstall); s.Available {
		t.Error("with no instance there is nothing managed yet")
	}
}

func TestCapabilitiesOfNothing(t *testing.T) {
	if got := Capabilities(nil, nil, Live{}); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestControlSupportCoversEveryKindTheSchemaNames(t *testing.T) {
	for _, k := range ControlKinds() {
		if ControlSupport(k) == SupportUnknown {
			t.Errorf("%s is a named kind with no support level; validation would reject a manifest the schema allows", k)
		}
	}
	if ControlSupport("valve_empty_response") != SupportUnknown {
		t.Error("an unnamed kind must stay unknown")
	}
}
