// Capabilities are COMPUTED from the manifest and the live status, never
// declared (D-22).
//
// A game manifest never says "show a Players button". It says which verbs exist
// and on which channel, and the derivation below turns that into a tri-state the
// dashboard renders with no per-game UI code:
//
//	Supported  a fact about the GAME       -- Valheim has no query channel, ever
//	Available  a fact about THIS INSTANCE  -- RCON is not up yet
//	Reason     the disabled button's tooltip
//
// The distinction is the whole point. Project Zomboid's 40-60 minute first boot
// renders as "RCON does not bind until the server finishes booting" instead of
// looking like a broken dashboard, and Valheim's Players button is OMITTED
// rather than greyed, because a greyed control implies it will work later.

package spec

import "fmt"

// Capability names one thing gvm might be able to do to an instance.
type Capability string

const (
	CapConsoleRead    Capability = "console.read"
	CapConsoleWrite   Capability = "console.write"
	CapControl        Capability = "control"
	CapPlayers        Capability = "players"
	CapBroadcast      Capability = "broadcast"
	CapKick           Capability = "kick"
	CapSaveOnDemand   Capability = "save"
	CapWorkshopMods   Capability = "content.workshop"
	CapCurseForge     Capability = "content.curseforge"
	CapModrinth       Capability = "content.modrinth"
	CapSandboxPresets Capability = "presets"
	CapEULA           Capability = "eula"
	CapConfigEdit     Capability = "config.edit"
	CapUpdateCheck    Capability = "update.check"
	CapRebuild        Capability = "rebuild"
	CapBackupFreeze   Capability = "backup.freeze"
	CapManagedInstall Capability = "install.managed"
	CapPlatformShim   Capability = "platform.shim"
)

// CapState is the triple the dashboard renders directly.
type CapState struct {
	Cap       Capability `json:"cap"`
	Supported bool       `json:"supported"`
	Available bool       `json:"available"`
	Reason    string     `json:"reason,omitempty"`
	Channel   string     `json:"channel,omitempty"` // console | control
	Verb      string     `json:"verb,omitempty"`    // POST target when available
}

// Channel names, as CapState.Channel carries them.
const (
	ChannelConsole = "console"
	ChannelControl = "control"
)

// Instance states, as the runner writes them into status.json.
const (
	StateInactive   = "inactive"
	StateActivating = "activating"
	StateReady      = "ready"
	StateStopping   = "stopping"
	StateFailed     = "failed"
)

// Live is the instance's current state, read from /run/gvm/<name>/status.json.
//
// It is a plain struct written by the runner rather than a database read, so
// capabilities are computable with Postgres down — which is the state in which
// an operator most needs the dashboard to tell them what they can still do.
type Live struct {
	State        string
	ControlReady bool
	Blocked      int // AWAITING_MANUAL_FILES count
	EULAAccepted bool

	// PresetFiles is how many files presets.source_glob matched at the last
	// scan. This package is pure and never touches a filesystem, and the glob
	// points inside the install tree, so the count has to arrive with the rest
	// of the live state; zero correctly reads as "the server has not booted yet".
	PresetFiles int
}

// ControlKind is the control channel a manifest asks for. The constants are the
// ones internal/control implements or plans; that package declares
// `type Kind = spec.ControlKind` rather than a second enum, for the same reason
// Dialect lives here (D-07): a manifest must not be able to name behaviour the
// shipped client does not have.
type ControlKind string

const (
	ControlNone       ControlKind = "none"
	ControlSourceRCON ControlKind = "source_rcon"
	ControlTelnet     ControlKind = "telnet"         // 7DTD 8081
	ControlWSRCON     ControlKind = "websocket_rcon" // Rust 28016
	ControlHTTPAPI    ControlKind = "http_api"       // Satisfactory
	ControlMCMgmt     ControlKind = "mc_mgmt"        // JSON-RPC over WebSocket
)

// Support is the tri-state that lets a manifest name a kind this build lacks
// without failing to load. An unknown name is an error; a known-but-unimplemented
// one degrades the capability and says so (section 10.2).
type Support int

const (
	SupportUnknown Support = iota
	SupportPlanned
	SupportImplemented
)

// plannedRelease is what a SupportPlanned capability tells the operator to wait
// for. One constant, so the dashboard, doctor and `gvm game validate` cannot
// promise three different versions.
const plannedRelease = "v1.1"

// controlSupport is the registry section 10.2 describes. 7daystodie.yaml ships
// in v1 declaring `kind: telnet` and renders honestly as a planned channel
// instead of failing to load.
var controlSupport = map[ControlKind]Support{
	ControlNone:       SupportImplemented,
	ControlSourceRCON: SupportImplemented,
	ControlTelnet:     SupportImplemented,
	ControlWSRCON:     SupportPlanned,
	ControlHTTPAPI:    SupportPlanned,
	ControlMCMgmt:     SupportImplemented,
}

// ControlSupport reports how well this build implements a control kind.
func ControlSupport(k ControlKind) Support { return controlSupport[k] }

// ControlKinds returns the known kinds in the order a diagnostic lists them.
func ControlKinds() []ControlKind {
	return []ControlKind{ControlNone, ControlSourceRCON, ControlTelnet, ControlWSRCON, ControlHTTPAPI, ControlMCMgmt}
}

// capabilityOrder fixes the order Capabilities returns, so the dashboard's
// button row does not reshuffle between polls.
var capabilityOrder = []Capability{
	CapConsoleRead, CapConsoleWrite, CapControl,
	CapPlayers, CapBroadcast, CapKick, CapSaveOnDemand,
	CapWorkshopMods, CapCurseForge, CapModrinth,
	CapSandboxPresets, CapEULA, CapConfigEdit,
	CapUpdateCheck, CapRebuild, CapBackupFreeze,
	CapManagedInstall, CapPlatformShim,
}

// Capabilities derives every capability for one instance.
//
// inst may be nil: the game page renders a manifest with no instance behind it,
// and the honest answer there is "supported, not available", not a panic.
func Capabilities(m *Manifest, inst *Instance, l Live) []CapState {
	if m == nil {
		return nil
	}
	d := deriver{m: m, inst: inst, l: l}
	d.consoleStates()
	d.controlState()
	d.verbStates()
	d.contentStates()
	d.presetState()
	d.eulaState()
	d.configEditState()
	d.lifecycleStates()
	d.platformShimState()

	out := make([]CapState, 0, len(capabilityOrder))
	for _, c := range capabilityOrder {
		if s, ok := d.states[c]; ok {
			out = append(out, s)
		}
	}
	return out
}

// FindCap returns the state for c. The dashboard renders the whole slice; a
// precondition check ("is broadcast available before I offer the button") wants
// exactly one, and going through this rather than re-deriving keeps the answer
// the API returned and the answer the handler enforced identical.
func FindCap(states []CapState, c Capability) (CapState, bool) {
	for _, s := range states {
		if s.Cap == c {
			return s, true
		}
	}
	return CapState{}, false
}

type deriver struct {
	m      *Manifest
	inst   *Instance
	l      Live
	states map[Capability]CapState
}

func (d *deriver) set(s CapState) {
	if d.states == nil {
		d.states = map[Capability]CapState{}
	}
	d.states[s.Cap] = s
}

// game names the game the way an operator would, for a Reason sentence.
func (d *deriver) game() string {
	if n := d.m.Metadata.Name; n != "" {
		return n
	}
	return d.m.Metadata.ID
}

func (d *deriver) managedInstall() bool {
	return d.inst != nil && d.inst.Spec.ManagedInstall
}

func (d *deriver) consoleMode() string { return d.m.Spec.Runtime.Console.Mode }

// consoleSupported is the one place "does this game have a console" is decided.
// An unset mode counts as none: a manifest that never says how to capture output
// has no console, and claiming one would render a console page that is
// permanently blank.
func (d *deriver) consoleSupported() bool {
	m := d.consoleMode()
	return m != "" && m != ConsoleModeNone
}

func (d *deriver) consoleStates() {
	sup := d.consoleSupported()
	read := CapState{Cap: CapConsoleRead, Supported: sup, Channel: ChannelConsole}
	write := CapState{Cap: CapConsoleWrite, Supported: sup, Channel: ChannelConsole, Verb: "console"}
	if !sup {
		read.Reason = d.game() + " captures no console output"
		write.Reason = read.Reason
		d.set(read)
		d.set(write)
		return
	}

	// Read stays available while the server is stopping or has failed: the last
	// thing in the ring is exactly what an operator needs after a crash, and
	// hiding it at the moment it matters is the opposite of useful.
	read.Available = d.l.State != StateInactive && d.l.State != ""
	if !read.Available {
		read.Reason = "the server is stopped"
	}
	write.Available = d.l.State == StateActivating || d.l.State == StateReady
	if !write.Available {
		write.Reason = "the server is stopped"
	}
	d.set(read)
	d.set(write)
}

func (d *deriver) controlState() {
	s := CapState{Cap: CapControl, Channel: ChannelControl}
	c := d.m.Spec.Control
	if c == nil {
		s.Reason = d.game() + " has no remote control channel; the console is the only command path"
		d.set(s)
		return
	}
	kind := ControlKind(c.Kind)
	switch ControlSupport(kind) {
	case SupportImplemented:
		s.Supported = true
	case SupportPlanned:
		s.Reason = fmt.Sprintf("%s control ships in gvm %s; use the console meanwhile", kind, plannedRelease)
		d.set(s)
		return
	default:
		s.Reason = fmt.Sprintf("%q is not a control kind this build knows", c.Kind)
		d.set(s)
		return
	}

	s.Available = d.l.ControlReady
	if !s.Available {
		s.Reason = c.NotReadyReason
		if s.Reason == "" {
			s.Reason = "the control channel is not up yet"
		}
	}
	d.set(s)
}

// verbStates derives the per-verb buttons. A verb is supported when it exists
// AND names a channel this game actually has: a manifest declaring
// `players: {control: "players"}` on a game whose control kind is only planned
// must not render a button that cannot work.
func (d *deriver) verbStates() {
	for _, vb := range []struct {
		cap  Capability
		verb string
	}{
		{CapPlayers, "players"},
		{CapBroadcast, "broadcast"},
		{CapKick, "kick"},
		{CapSaveOnDemand, "save"},
	} {
		d.set(d.verbState(vb.cap, vb.verb))
	}
}

func (d *deriver) verbState(c Capability, verb string) CapState {
	s := CapState{Cap: c, Verb: verb}
	v, declared := d.m.Spec.Verbs[verb]

	// A freeze rung is a save the game can do even with no `save` verb written
	// out, which is how a backup can quiesce a world that has no save command in
	// its own verb list.
	if !declared && c == CapSaveOnDemand && d.m.Spec.Backup.Freeze.Before != nil {
		v = Verb{
			Console: stepSend(d.m.Spec.Backup.Freeze.Before, "console"),
			Control: stepSend(d.m.Spec.Backup.Freeze.Before, "control"),
		}
		declared = v.Console != "" || v.Control != ""
	}
	if !declared {
		s.Reason = fmt.Sprintf("%s has no %s channel", d.game(), verb)
		return s
	}

	control, _ := d.states[CapControl]
	write := d.states[CapConsoleWrite]

	// Control first: it is the channel that answers, and a verb that names both
	// (PZ's save) should report the one the commander will actually pick.
	if v.Control != "" && control.Supported {
		s.Supported, s.Channel = true, ChannelControl
		s.Available = control.Available
		s.Reason = ""
		if !s.Available {
			s.Reason = control.Reason
		}
		return s
	}
	if v.Console != "" && write.Supported {
		s.Supported, s.Channel = true, ChannelConsole
		s.Available = write.Available
		if !s.Available {
			s.Reason = write.Reason
		}
		return s
	}

	// Declared, but on a channel this build or this game does not have. The
	// reason is the channel's own, which is the sentence that explains it.
	switch {
	case v.Control != "":
		s.Reason = firstNonEmpty(control.Reason, d.game()+" has no control channel")
	default:
		s.Reason = firstNonEmpty(write.Reason, d.game()+" has no console channel")
	}
	return s
}

// stepSend returns a freeze rung's command when it runs over the named channel.
func stepSend(s *ShutdownStep, via string) string {
	if s == nil || s.Via != via {
		return ""
	}
	return s.Send
}

func (d *deriver) contentStates() {
	for _, cs := range []struct {
		cap    Capability
		source string
		verb   string
	}{
		{CapWorkshopMods, SourceSteamWorkshop, "content/sync"},
		{CapCurseForge, SourceCurseForge, "content/resolve-blocked"},
		{CapModrinth, SourceModrinth, "content/sync"},
	} {
		d.set(d.contentState(cs.cap, cs.source, cs.verb))
	}
}

func (d *deriver) contentState(c Capability, source, verb string) CapState {
	s := CapState{Cap: c, Verb: verb}
	var block *ContentSpec
	for i := range d.m.Spec.Content {
		if d.m.Spec.Content[i].Source == source {
			block = &d.m.Spec.Content[i]
			break
		}
	}
	if block == nil {
		s.Reason = fmt.Sprintf("%s has no %s content", d.game(), source)
		return s
	}
	s.Supported = true
	s.Available = true

	// A game that downloads its own items during boot cannot have the list
	// changed under it: the edit would take effect at an unpredictable moment,
	// half-applied. PZ is the case.
	if block.Acquisition.Mechanism == AcquireServerBoot && d.l.State != StateInactive && d.l.State != "" {
		s.Available = false
		s.Reason = fmt.Sprintf("%s downloads %s items on boot; stop the server to change the list",
			d.game(), source)
		return s
	}
	if c == CapCurseForge && d.l.Blocked > 0 {
		s.Available = false
		s.Reason = plural(d.l.Blocked, "file", "files") + " await manual download"
	}
	return s
}

func (d *deriver) presetState() {
	s := CapState{Cap: CapSandboxPresets, Verb: "config"}
	if d.m.Spec.Presets == nil {
		s.Reason = d.game() + " ships no sandbox presets"
		d.set(s)
		return
	}
	s.Supported = true
	s.Available = d.l.PresetFiles > 0
	if !s.Available {
		s.Reason = "the server must boot once before its preset list exists"
	}
	d.set(s)
}

func (d *deriver) eulaState() {
	s := CapState{Cap: CapEULA, Verb: "config"}
	if d.m.Spec.Legal == nil || d.m.Spec.Legal.EULA == nil {
		d.set(s)
		return
	}
	s.Supported = true
	s.Available = !d.l.EULAAccepted
	if !s.Available {
		s.Reason = "already accepted"
	}
	d.set(s)
}

// configEditState is not in section 8.5's table because it falls straight out of
// 8.4: an `owner: server` file is rewritten by the game on shutdown, so gvm only
// edits it while the unit is stopped. Deriving it here means the dashboard's
// Config button carries that rule instead of the operator discovering it by
// losing an edit.
func (d *deriver) configEditState() {
	s := CapState{Cap: CapConfigEdit, Verb: "config"}
	if len(d.m.Spec.Configs) == 0 {
		s.Reason = d.game() + " has no editable config file; everything is argv"
		d.set(s)
		return
	}
	s.Supported = true
	s.Available = true
	for _, cf := range d.m.Spec.Configs {
		if cf.Owner != OwnerServer {
			continue
		}
		if d.l.State != StateInactive && d.l.State != "" {
			s.Available = false
			s.Reason = fmt.Sprintf("%s rewrites %s when it stops; stop the server to edit it",
				d.game(), cf.Name)
		}
		break
	}
	d.set(s)
}

func (d *deriver) lifecycleStates() {
	kind := d.m.Spec.Provider.Kind

	// update.check is supported when the provider can answer "is there a newer
	// build" without touching the install. `adopted` and `none` cannot.
	upd := CapState{Cap: CapUpdateCheck, Verb: "update"}
	upd.Supported = kind == ProviderSteamCMD || kind == ProviderMinecraft
	upd.Available = upd.Supported
	if !upd.Supported {
		upd.Reason = fmt.Sprintf("the %q provider cannot report an available version", kind)
	}
	d.set(upd)

	reb := CapState{Cap: CapRebuild, Verb: "rebuild"}
	reb.Supported = len(d.m.Spec.Backup.WorldPaths) > 0
	if !reb.Supported {
		reb.Reason = d.game() + " declares no world_paths, so there is nothing a rebuild could preserve"
	} else {
		reb.Available = d.managedInstall()
		if !reb.Available {
			reb.Reason = "adopted instance; run `gvm instance promote` first"
		}
	}
	d.set(reb)

	inst := CapState{Cap: CapManagedInstall}
	inst.Supported = kind != ProviderAdopted && kind != ""
	if !inst.Supported {
		inst.Reason = "this game is adoption-only; gvm never installs it"
	} else {
		inst.Available = d.managedInstall()
		if !inst.Available {
			inst.Reason = "adopted instance; gvm does not own this install tree"
		}
	}
	d.set(inst)

	frz := CapState{Cap: CapBackupFreeze, Verb: "backups"}
	before := d.m.Spec.Backup.Freeze.Before
	if before == nil {
		frz.Reason = d.game() + " has no way to quiesce a world before a backup"
		d.set(frz)
		return
	}
	frz.Supported = true
	frz.Channel = before.Via
	switch before.Via {
	case ChannelControl:
		ctl := d.states[CapControl]
		frz.Supported = ctl.Supported
		frz.Available = ctl.Available
		frz.Reason = ctl.Reason
	case ChannelConsole:
		w := d.states[CapConsoleWrite]
		frz.Supported = w.Supported
		frz.Available = w.Available
		frz.Reason = w.Reason
	default:
		frz.Available = true
	}
	d.set(frz)
}

// platformShimState is informational: it is the badge that explains why an
// instance runs under xvfb or fetches Windows binaries, so nobody spends an
// afternoon working out where the extra process came from.
func (d *deriver) platformShimState() {
	s := CapState{Cap: CapPlatformShim}
	rt := d.m.Spec.Runtime
	sc := d.m.Spec.Provider.SteamCMD
	switch {
	case rt.Wrapper != nil:
		s.Supported, s.Available = true, true
		s.Reason = fmt.Sprintf("runs under %s", rt.Wrapper.Kind)
	case sc != nil && sc.Platform != "" && sc.Platform != PlatformLinux:
		s.Supported, s.Available = true, true
		s.Reason = fmt.Sprintf("installs the %s build", sc.Platform)
	}
	d.set(s)
}
