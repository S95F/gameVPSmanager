// Package spec holds the on-disk truth: the game manifest schema, the instance
// schema, and the host configuration, as plain Go structs with YAML tags.
//
// Two rules shape everything here.
//
// The package is PURE. It parses, renders, compares, and reads and writes the
// files it owns; it never talks to systemd, a database or the network, and it
// carries no build tags, so the whole schema is testable on any host.
//
// Enum-valued fields decode PERMISSIVELY. A string type such as Dialect names
// the legal values and offers Valid, but unmarshalling an unrecognised value
// succeeds so that validation can report it as a diagnostic carrying the
// manifest's own file, line and column — per section 8.6, a bad dialect must
// read as
//
//	projectzomboid.yaml:118:7: spec.control.dialect: "valve_empty_response" is
//	not a dialect (want source|minecraft|rust|simple)
//
// and not as a decoder error that names no path. Shape ambiguities are the
// exception and are refused at decode, because a struct that can hold two
// contradictory readings of the same file has already lost the argument.
package spec

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Origin ranks where a manifest was loaded from. Highest wins: an
// operator-approved override shadows the packaged file, which shadows the
// embedded floor. The embedded copy is always present so `gvm doctor` and
// `gvm adopt discover` still work on a box whose /usr/share was mangled.
type Origin string

const (
	OriginEmbedded Origin = "embedded" // //go:embed games/<id>.yaml -- the floor
	OriginPackage  Origin = "shipped"  // /usr/share/gvm/games/<id>.yaml, dpkg-owned
	OriginOverride Origin = "override" // /etc/gvm/games/<id>.yaml, owner-approved (D-19)
)

// Rank orders origins for resolution. A higher rank shadows a lower one.
func (o Origin) Rank() int {
	switch o {
	case OriginOverride:
		return 3
	case OriginPackage:
		return 2
	case OriginEmbedded:
		return 1
	}
	return 0
}

// Valid reports whether o is one of the three known origins.
func (o Origin) Valid() bool { return o.Rank() != 0 }

// Intent is what a caller wants from a provider, not how to get it. It lives
// here rather than in internal/provider because the manifest's validate_on list
// is typed with it and providers already depend on this package.
type Intent string

const (
	IntentInstall Intent = "install" // first materialisation
	IntentUpdate  Intent = "update"  // buildid differs -> fetch delta
	IntentRepair  Intent = "repair"  // operator-requested; MAY validate (D-32)
	IntentVerify  Intent = "verify"  // read-only integrity check
)

// Dialect names the per-game RCON quirk set. The four values are exactly the
// constants in internal/rcon, so a manifest cannot ask for behaviour the client
// does not implement (D-07).
//
// "valve_empty_response" is NOT a value here and never will be. It is a
// terminator strategy, not a dialect, and it is precisely the trick
// internal/rcon/dialect.go documents as unsafe on Project Zomboid — the one
// server that has to work on day one. PZ is DialectSimple.
type Dialect string

const (
	DialectSource    Dialect = "source"
	DialectMinecraft Dialect = "minecraft"
	DialectRust      Dialect = "rust"
	DialectSimple    Dialect = "simple"
)

// Dialects returns the legal dialects in the order a diagnostic should list
// them. internal/spec's dialect sync test compares this against the constants in
// internal/rcon so the two cannot drift.
func Dialects() []Dialect {
	return []Dialect{DialectSource, DialectMinecraft, DialectRust, DialectSimple}
}

// Valid reports whether d is a registered dialect.
func (d Dialect) Valid() bool {
	for _, k := range Dialects() {
		if d == k {
			return true
		}
	}
	return false
}

// Expose is how far a port is reachable (D-20). Three states, because
// firewall.sh already needed three via ADMIN_IP and a bool collapsed
// "loopback only" into "not on the internet".
type Expose string

const (
	ExposePublic Expose = "public" // anywhere
	ExposeAdmin  Expose = "admin"  // only from firewall admin_cidrs
	ExposeNone   Expose = "none"   // no firewall rule at all
)

// Valid reports whether e is one of the three exposure levels.
func (e Expose) Valid() bool {
	return e == ExposePublic || e == ExposeAdmin || e == ExposeNone
}

// Narrows reports whether e is at least as restrictive as m. An instance may
// only narrow its manifest's exposure, with one exception handled by the caller:
// none -> admin is allowed, because that is exactly ADMIN_IP's use case (D-20).
func (e Expose) Narrows(m Expose) bool {
	rank := map[Expose]int{ExposePublic: 3, ExposeAdmin: 2, ExposeNone: 1}
	return rank[e] != 0 && rank[m] != 0 && rank[e] <= rank[m]
}

// Manifest is one game's complete description. Everything a game needs that is
// honestly data lives in the file; the ~10% that is not is named by the closed
// hook, resolver and validator registries.
type Manifest struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   GameMeta `yaml:"metadata"`
	Spec       GameSpec `yaml:"spec"`

	// Provenance, filled by the loader -- never by the file. A manifest that
	// could name its own origin could claim to be the approved override.
	Origin    Origin `yaml:"-"`
	SourceRef string `yaml:"-"`
	SHA256    string `yaml:"-"`
	Raw       []byte `yaml:"-"` // verbatim bytes written to manifest.lock.yaml (D-17)
}

type GameMeta struct {
	ID         string   `yaml:"id"` // ^[a-z0-9][a-z0-9-]{1,31}$
	Name       string   `yaml:"name"`
	Revision   int      `yaml:"revision"`
	Doc        string   `yaml:"doc"` // rendered in the dashboard's game header
	Tags       []string `yaml:"tags"`
	Deprecated string   `yaml:"deprecated,omitempty"` // non-empty => warn on apply
}

type GameSpec struct {
	DriverABI  int                `yaml:"driverABI"`
	Provider   ProviderSpec       `yaml:"provider"`
	Runtime    RuntimeSpec        `yaml:"runtime"`
	Readiness  ReadinessSpec      `yaml:"readiness"`
	Shutdown   ShutdownSpec       `yaml:"shutdown"`
	Control    *ControlSpec       `yaml:"control,omitempty"` // nil => console only
	Verbs      map[string]Verb    `yaml:"verbs"`
	Ports      []PortSpec         `yaml:"ports"`
	PortStride map[string]int     `yaml:"port_stride"`
	Configs    []ConfigFile       `yaml:"config_files"`
	Content    []ContentSpec      `yaml:"content"`
	Presets    *PresetSpec        `yaml:"presets,omitempty"`
	Legal      *LegalSpec         `yaml:"legal,omitempty"` // EULA gate
	Hooks      HookSpec           `yaml:"hooks"`
	Validators []string           `yaml:"validators"` // named, closed registry
	Backup     BackupSpec         `yaml:"backup"`
	Vars       map[string]VarSpec `yaml:"vars"`
	Sizing     Sizing             `yaml:"sizing"`
	Jobs       RecommendedJobs    `yaml:"recommended_jobs"`
	Egress     []EgressRule       `yaml:"egress"` // documentation + doctor, not enforcing
}

type ProviderSpec struct {
	Kind      string         `yaml:"kind"` // steamcmd | minecraft | adopted | none
	SteamCMD  *SteamCMDSpec  `yaml:"steamcmd,omitempty"`
	Minecraft *MinecraftSpec `yaml:"minecraft,omitempty"`
}

type SteamCMDSpec struct {
	InstallAppID      int      `yaml:"install_appid"`
	Anonymous         bool     `yaml:"anonymous"`
	Branch            string   `yaml:"branch"` // "public" is stable, not ""
	BranchPasswordVar string   `yaml:"branch_password_var,omitempty"`
	Platform          string   `yaml:"platform"`    // linux | windows | macos
	ValidateOn        []Intent `yaml:"validate_on"` // containing "update" is a LOAD ERROR (D-32)
	Sentinel          Sentinel `yaml:"sentinel"`    // REQUIRED
	Verify            Verify   `yaml:"verify"`      // REQUIRED
	ExtraApps         []int    `yaml:"extra_apps,omitempty"`
}

type MinecraftSpec struct {
	Loader     LoaderSpec `yaml:"loader"`
	MCVersion  string     `yaml:"mc_version"`
	Java       JavaSpec   `yaml:"java"`
	Entrypoint string     `yaml:"entrypoint"` // serverstarterjar | ...

	// MaterializeIsExplicit: Paperclip/Fabric/NeoForge first boot DOWNLOADS from
	// the network. That must be an observable install step, never a side effect
	// of a user's first Start click and never inside a firewalled rebuild.
	MaterializeIsExplicit bool `yaml:"materialize_is_explicit"`

	Sentinel Sentinel `yaml:"sentinel"`
	Verify   Verify   `yaml:"verify"`
}

type LoaderSpec struct {
	Kind    string `yaml:"kind"`    // neoforge | forge | fabric | vanilla
	Version string `yaml:"version"` // template
}

type JavaSpec struct {
	From   string `yaml:"from"`   // piston_meta | pinned
	Vendor string `yaml:"vendor"` // temurin | ...
}

type Sentinel struct {
	Path       string `yaml:"path"` // relative to Install
	Executable bool   `yaml:"executable"`
	MinBytes   int64  `yaml:"min_bytes,omitempty"`
}

// Verify is mandatory for steamcmd because gvm bans `validate` on the nightly
// cycle, and that nightly validate is what silently repaired partial downloads
// in update.sh. Removing it without a real compensating check is a regression
// (D-32).
type Verify struct {
	AppManifest bool `yaml:"appmanifest"` // StateFlags == 4
	ByteParity  bool `yaml:"byte_parity"` // Downloaded==ToDownload && Staged==ToStage
	SingleACF   bool `yaml:"single_acf"`  // guard 0 or >1 appmanifest files
}

type RuntimeSpec struct {
	Exec    string            `yaml:"exec"`
	Cwd     string            `yaml:"cwd"`
	Args    Args              `yaml:"args"`
	Env     map[string]string `yaml:"env"` // values are templates
	Wrapper *Wrapper          `yaml:"wrapper,omitempty"`

	Console ConsoleSpec `yaml:"console"`

	// ProcessGroup=true when the launch script does not exec its child. PZ's
	// start-server.sh runs ProjectZomboid64 in a subshell, so MainPID is the
	// shell and signalling it does nothing to the JVM.
	ProcessGroup bool `yaml:"process_group"`

	// ConsoleCompat=screen keeps `screen -r <name>` working through an adoption
	// week. Sunsets 90 days after first use; doctor warns from T-14 (D-11).
	ConsoleCompat string `yaml:"console_compat,omitempty"`

	Limits Limits `yaml:"limits"`

	// Arch drives SystemCallArchitectures=. A hardcoded "native" makes 32-bit
	// engines (GMod, L4D2, srcds) fail to start with no useful error.
	Arch string `yaml:"arch"` // native | i386 | any

	// JIT=true emits MemoryDenyWriteExecute=no with an inline comment. Every
	// Java title needs W^X off; a future hardening PR must not "fix" it.
	JIT bool `yaml:"jit"`
}

// Wrapper wraps the game's own argv, for engines that need a virtual framebuffer
// or a compatibility layer.
type Wrapper struct {
	Kind string   `yaml:"kind"` // xvfb | proton | wine
	Args []string `yaml:"args,omitempty"`
}

// Arg is a bare string, or {value|values, when}. The common case stays terse and
// the conditional case does not need a template with control flow in it (D-21).
type Arg struct {
	Value  string   `yaml:"value,omitempty"`
	Values []string `yaml:"values,omitempty"` // expands in place
	When   *When    `yaml:"when,omitempty"`
}

// Args is a named slice so the argv list can refuse a null element. yaml.v3
// SKIPS a null sequence entry rather than reporting it, which would quietly
// shorten a live server's command line — the exact class of change the manifest
// diff classifies as "CHANGES THE LAUNCH COMMAND OF A LIVE SERVER". A dropped
// `-servername` does not fail; it generates a fresh world.
type Args []Arg

func (as *Args) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag == "!!null" {
		*as = nil
		return nil
	}
	if n.Kind != yaml.SequenceNode {
		return fmt.Errorf("runtime.args: line %d: want a list", n.Line)
	}
	out := make(Args, 0, len(n.Content))
	for i, c := range n.Content {
		if c.Tag == "!!null" {
			return fmt.Errorf("runtime.args: line %d: element %d is empty; "+
				`write the literal "" if a blank argv element is intended`, c.Line, i)
		}
		var a Arg
		if err := c.Decode(&a); err != nil {
			return err
		}
		out = append(out, a)
	}
	*as = out
	return nil
}

func (a *Arg) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*a = Arg{Value: n.Value}
		return nil
	}
	type plain Arg
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*a = Arg(p)
	return nil
}

// MarshalYAML emits the terse form for a plain argument so a load/save cycle
// does not rewrite every manifest's args block into mappings.
func (a Arg) MarshalYAML() (any, error) {
	if a.When == nil && a.Values == nil {
		return a.Value, nil
	}
	type plain Arg
	return plain(a), nil
}

type ConsoleSpec struct {
	Mode       string `yaml:"mode"` // pty | pipe | none
	StripANSI  bool   `yaml:"strip_ansi"`
	Charset    string `yaml:"charset"`     // utf8 | latin1
	LineEnding string `yaml:"line_ending"` // lf (default) | cr | crlf
	RingBytes  int    `yaml:"ring_bytes"`  // default 2097152
	Rows       uint16 `yaml:"rows"`        // pty only; default 50
	Cols       uint16 `yaml:"cols"`        // pty only; default 200
	EchoInput  bool   `yaml:"echo_input"`  // echo operator input into the ring, attributed
}

type Limits struct {
	NoFile int `yaml:"nofile"`
	Tasks  int `yaml:"tasks_max"`
}

type ReadinessSpec struct {
	Probes []ReadyProbe `yaml:"probes"`
	Fail   []FailMarker `yaml:"fail_markers"`

	// Progress markers gate EXTEND_TIMEOUT_USEC. Seeing one (or any console
	// output at all) resets the stall clock; HardDeadline is never extended.
	Progress        []string `yaml:"progress"`
	ProgressPattern string   `yaml:"progress_pattern,omitempty"`

	// StallTimeout: no progress and no console output for this long => fail,
	// even if HardDeadline has not elapsed.
	StallTimeout Duration `yaml:"stall_timeout"`

	// HardDeadline is REQUIRED and finite. TimeoutStartSec is set from it, so
	// `infinity` would leave a wedged runner in `activating` forever with
	// Restart=on-failure never firing (D-12).
	HardDeadline Duration `yaml:"hard_deadline"`
}

type ReadyProbe struct {
	Kind    string   `yaml:"kind"`              // log_marker|tcp_probe|udp_probe|a2s|mc_ping|control_auth
	Pattern string   `yaml:"pattern,omitempty"` // RE2, compiled at LOAD time
	Port    string   `yaml:"port,omitempty"`    // manifest port NAME, not a number
	After   string   `yaml:"after,omitempty"`   // sequence probes
	Timeout Duration `yaml:"timeout,omitempty"`
	Every   Duration `yaml:"every,omitempty"`
}

type FailMarker struct {
	Pattern string `yaml:"pattern"`
	Fatal   bool   `yaml:"fatal"`
	Hint    string `yaml:"hint,omitempty"` // shown verbatim in the failure report
}

type ShutdownSpec struct {
	Budget                 Duration       `yaml:"budget"` // => TimeoutStopSec
	ExtendWhileProgressing bool           `yaml:"extend_while_progressing"`
	ProgressWindow         Duration       `yaml:"progress_window"`
	Steps                  []ShutdownStep `yaml:"steps"`
}

// ShutdownStep generalises PZ's screen-stuffed save/quit. The ladder always ends
// in SIGTERM-to-process-group then SIGKILL, both recorded as a degraded
// shutdown. There is no ExecStop= anywhere.
//
// wait_for_log + wait, stated once: wait up to Wait for the pattern; if it
// appears, advance immediately; if it does not, ADVANCE ANYWAY and record
// shutdown_step_timeout. It is a hurry-up, not a gate — worst case identical to
// the bash suite's blind `sleep 10`, best case faster, and "saved at 4s" becomes
// distinguishable from "waited the full 10s".
type ShutdownStep struct {
	Via        string   `yaml:"via"` // console | control | signal | wait_exit
	Send       string   `yaml:"send,omitempty"`
	Signal     string   `yaml:"signal,omitempty"` // Valheim: SIGINT, not SIGTERM
	Scope      string   `yaml:"scope,omitempty"`  // process | process_group (default)
	WaitForLog string   `yaml:"wait_for_log,omitempty"`
	Wait       Duration `yaml:"wait,omitempty"`
	OnTimeout  string   `yaml:"on_timeout,omitempty"` // continue (default) | escalate

	// TolerateDisconnect: a control-channel step that stops the server will have
	// its socket closed under it. Defaults TRUE for a step whose Send matches
	// verbs.stop.control. Without it, `quit` over an idle-terminated dialect
	// returns EOF and looks like a failure when it worked.
	TolerateDisconnect *bool `yaml:"tolerate_disconnect,omitempty"`
}

// ControlSpec carries a Dialect, not a terminator strategy, so a manifest cannot
// disagree with the shipped RCON client (D-07).
type ControlSpec struct {
	Kind                   string   `yaml:"kind"` // control.Kind
	Port                   string   `yaml:"port"`
	Password               string   `yaml:"password"` // template; tainted
	Dialect                Dialect  `yaml:"dialect"`
	AvailableOnlyWhenReady bool     `yaml:"available_only_when_ready"`
	NotReadyReason         string   `yaml:"not_ready_reason"`
	MinPoll                Duration `yaml:"min_poll"` // default 60s
	FallbackPlanned        string   `yaml:"fallback_planned,omitempty"`
	Deprecation            string   `yaml:"deprecation,omitempty"`
}

// Verb declares which channel serves an intent. A game with no control entry for
// "players" renders that button disabled. gvm never pretends.
type Verb struct {
	Console string `yaml:"console,omitempty"`
	Control string `yaml:"control,omitempty"`
}

type PortSpec struct {
	Name    string `yaml:"name"`
	Proto   string `yaml:"proto"` // tcp | udp | both
	Default int    `yaml:"default"`
	Group   string `yaml:"group"`  // a group shifts as a unit (contiguity)
	Expose  Expose `yaml:"expose"` // public | admin | none (D-20)
	Bind    string `yaml:"bind,omitempty"`

	// ConfigKey is the ONLY way a port reaches a config file. Naming the same
	// key in config_files[].keys is a validation error, so the allocator and the
	// file cannot drift.
	ConfigKey  string `yaml:"config_key,omitempty"`
	ConfigFile string `yaml:"config_file,omitempty"` // default: first config file
	Note       string `yaml:"note,omitempty"`        // UI tooltip + ufw comment
}

// UnmarshalYAML accepts the pre-D-20 `public: true` spelling and normalises it,
// so an existing manifest keeps loading. Setting both `public` and `expose` is
// refused rather than resolved: the two can contradict each other, and silently
// picking a winner is how a port ends up open to the internet because someone
// edited the field that lost.
func (p *PortSpec) UnmarshalYAML(n *yaml.Node) error {
	type plain PortSpec
	var q plain
	if err := n.Decode(&q); err != nil {
		return err
	}
	var legacy struct {
		Public *bool `yaml:"public"`
	}
	if err := n.Decode(&legacy); err != nil {
		return err
	}
	if legacy.Public != nil {
		if q.Expose != "" {
			return fmt.Errorf("ports: line %d: %q sets both `public` and `expose`; "+
				"`public` is the pre-D-20 spelling, delete it", n.Line, q.Name)
		}
		q.Expose = ExposeNone
		if *legacy.Public {
			q.Expose = ExposePublic
		}
	}
	*p = PortSpec(q)
	return nil
}

type ConfigFile struct {
	Name   string `yaml:"name"`   // referenced by ports and hooks
	Path   string `yaml:"path"`   // template
	Format string `yaml:"format"` // gamecfg codec registry key

	// Owner=server means the game rewrites this file on shutdown, so gvm only
	// mutates it while STOPPED and re-reads it after every clean stop.
	Owner string `yaml:"owner"` // server | gvm

	CreateIfMissing bool   `yaml:"create_if_missing"`
	Mode            string `yaml:"mode"` // "0640"

	// Keys is the declarative owned_keys list. Every other key in the file is
	// PROVABLY left alone -- that is a guarantee, not an implementation detail.
	Keys map[string]string `yaml:"keys"` // value is a template

	// DenyKeys are refused even to hooks.
	DenyKeys []string `yaml:"deny_keys,omitempty"`
}

// ContentSpec is the two-list model, lifted straight out of sync-mods.sh:
// acquisition ids (numeric workshop / CF projectID:fileID) are a DIFFERENT
// NAMESPACE from activation ids (mod.info `id=` / jar filename), and both must
// persist.
type ContentSpec struct {
	Name             string             `yaml:"name"`
	Source           string             `yaml:"source"` // steam_workshop | modrinth | curseforge
	OrderSignificant bool               `yaml:"order_significant"`
	Acquisition      ContentAcquisition `yaml:"acquisition"`
	Activation       ContentActivation  `yaml:"activation"`

	WorkshopAppID     int               `yaml:"workshop_appid,omitempty"` // 108600, NOT 380870
	Roots             []string          `yaml:"roots,omitempty"`
	MetadataAPI       string            `yaml:"metadata_api,omitempty"`
	CollectionSync    bool              `yaml:"collection_sync,omitempty"`
	EnvironmentFilter string            `yaml:"environment_filter,omitempty"`
	OnBlocked         string            `yaml:"on_blocked,omitempty"` // halt | continue
	OwnedKeys         []string          `yaml:"owned_keys,omitempty"`
	Impact            map[string]string `yaml:"impact,omitempty"` // add/remove/reorder/update/repin
}

type ContentAcquisition struct {
	Mechanism string `yaml:"mechanism"` // server_boot | steamcmd_item | http
	WriteKey  string `yaml:"write_key"` // "WorkshopItems"
	WriteFile string `yaml:"write_file"`

	// Separator is ";" for PZ. A comma breaks the list with NO error, which is
	// why it is data rather than a per-game constant in Go.
	Separator string `yaml:"separator"`

	KeepMissing bool          `yaml:"keep_missing"` // missing ids stay listed so the server retries
	BootTimeout Duration      `yaml:"boot_timeout,omitempty"`
	Verify      AcquireVerify `yaml:"verify"`
}

type AcquireVerify struct {
	DirNonEmpty bool `yaml:"dir_non_empty"`
}

type ContentActivation struct {
	// Mode is how an item becomes active. The default (empty) is key-based: a
	// write_key lists the enabled ids in a config file (Project Zomboid's
	// Mods=). "presence" means the file's existence in a content root IS its
	// activation — a Minecraft jar dropped in plugins/ loads, with no list to
	// write — so write_key and separator are neither needed nor allowed.
	Mode         string            `yaml:"mode,omitempty"`
	WriteKey     string            `yaml:"write_key"` // "Mods"
	WriteFile    string            `yaml:"write_file"`
	Separator    string            `yaml:"separator"`
	Resolver     string            `yaml:"resolver"` // closed registry: "pz.resolve_mod_ids"
	ResolverVars map[string]string `yaml:"resolver_vars,omitempty"`
}

// PresetSpec points at the server's OWN shipped preset files, so the list is
// whatever this build actually ships and never a hardcoded table that drifts.
type PresetSpec struct {
	SourceGlob   string   `yaml:"source_glob"`
	Exclude      []string `yaml:"exclude,omitempty"`
	Target       string   `yaml:"target"`              // a config_files[].name
	Transform    string   `yaml:"transform,omitempty"` // closed registry
	OverridesVar string   `yaml:"overrides_var,omitempty"`
}

// LegalSpec gates a game behind an agreement the operator must accept
// themselves. Only an explicit operator action may flip the var (D-22's `eula`
// capability); no hook accepts on their behalf without one.
type LegalSpec struct {
	EULA *EULASpec `yaml:"eula,omitempty"`
}

type EULASpec struct {
	Var    string `yaml:"var"` // the bool var that records acceptance
	URL    string `yaml:"url,omitempty"`
	Prompt string `yaml:"prompt,omitempty"`
}

// HookSpec names hooks from the closed registry, per phase, in run order. An
// unknown name is a load-time error naming file:line, which is what keeps
// "hooks: [something.clever]" from being a plugin system (D-21).
type HookSpec struct {
	Validate    []string `yaml:"validate,omitempty"`
	PostInstall []string `yaml:"post_install,omitempty"`
	PostContent []string `yaml:"post_content,omitempty"`
	PreStart    []string `yaml:"pre_start,omitempty"`
	PostStart   []string `yaml:"post_start,omitempty"`
	PreBackup   []string `yaml:"pre_backup,omitempty"`
	PostBackup  []string `yaml:"post_backup,omitempty"`
}

type BackupSpec struct {
	Include          []string   `yaml:"include"`
	IncludeOptional  []string   `yaml:"include_optional"` // absent => note, not error
	Exclude          []string   `yaml:"exclude"`
	WorldPaths       []string   `yaml:"world_paths"`       // never destroyed without --rebuild
	DestroyProtected []string   `yaml:"destroy_protected"` // rebuild.sh's survivors
	Freeze           FreezeSpec `yaml:"freeze"`
}

// FreezeSpec is a state machine with a MANDATORY unfreeze. Dying between
// save-off and save-on leaves a running server that silently stops persisting —
// the worst failure in the system — so After is guarded by a defer AND by an
// out-of-process lease the runner honours (D-31).
type FreezeSpec struct {
	Before      *ShutdownStep `yaml:"before,omitempty"`
	During      *ShutdownStep `yaml:"during,omitempty"`
	After       *ShutdownStep `yaml:"after,omitempty"`
	LeaseBudget Duration      `yaml:"lease_budget"`
}

// UnmarshalYAML treats the literal `none` as "no such rung", which is how the
// shipped manifests spell a one-shot save that has no matching thaw. Without
// this, `after: none` would decode as a step with no via and be attempted.
func (f *FreezeSpec) UnmarshalYAML(n *yaml.Node) error {
	var aux struct {
		Before      yaml.Node `yaml:"before"`
		During      yaml.Node `yaml:"during"`
		After       yaml.Node `yaml:"after"`
		LeaseBudget Duration  `yaml:"lease_budget"`
	}
	if err := n.Decode(&aux); err != nil {
		return err
	}
	var err error
	if f.Before, err = decodeStepOrNone(&aux.Before); err != nil {
		return fmt.Errorf("backup.freeze.before: %w", err)
	}
	if f.During, err = decodeStepOrNone(&aux.During); err != nil {
		return fmt.Errorf("backup.freeze.during: %w", err)
	}
	if f.After, err = decodeStepOrNone(&aux.After); err != nil {
		return fmt.Errorf("backup.freeze.after: %w", err)
	}
	f.LeaseBudget = aux.LeaseBudget
	return nil
}

func decodeStepOrNone(n *yaml.Node) (*ShutdownStep, error) {
	if n.IsZero() || n.Tag == "!!null" {
		return nil, nil
	}
	if n.Kind == yaml.ScalarNode {
		if n.Value == "none" {
			return nil, nil
		}
		return nil, fmt.Errorf("line %d: %q is not a step; write `none` or a {via: ...} mapping", n.Line, n.Value)
	}
	var s ShutdownStep
	if err := n.Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

// VarSpec is the single source for the dashboard form, the CLI's --var
// validation, and the template's declared variable set.
type VarSpec struct {
	Type     string   `yaml:"type"` // string|int|bool|memory|enum|duration|secret|password
	Default  string   `yaml:"default,omitempty"`
	Required bool     `yaml:"required,omitempty"`
	Enum     []string `yaml:"values,omitempty"`
	EnumFrom string   `yaml:"values_from,omitempty"` // closed: "presets"
	Min      *int     `yaml:"min,omitempty"`
	Max      *int     `yaml:"max,omitempty"`
	Pattern  string   `yaml:"pattern,omitempty"` // RE2, applied at validate

	// Immutable vars cannot change after creation. server_name is immutable
	// because it names the world directory; changing it orphans the save.
	Immutable bool `yaml:"immutable,omitempty"`

	// Apply drives the badge next to every dashboard field.
	Apply string `yaml:"apply"` // live | restart | rebuild

	// Help is rendered as the field tooltip. This is where config.env's "B42
	// native memory lives OUTSIDE the heap -- leave ~3GB free or the kernel
	// OOM-kills the JVM mid-generation" reaches the person editing the number,
	// at the moment they edit it, instead of dying in a comment.
	Help     string `yaml:"help,omitempty"`
	Advanced bool   `yaml:"advanced,omitempty"`
	Group    string `yaml:"group,omitempty"` // form section
	Unit     string `yaml:"unit,omitempty"`  // rendered suffix, e.g. "GiB"
}

type Sizing struct {
	Model          string `yaml:"memory_model"` // heap_plus_headroom | flat
	HeapVar        string `yaml:"heap_var,omitempty"`
	NativeHeadroom string `yaml:"native_headroom"`
	HighMargin     string `yaml:"high_margin"`
	RAMFloorMB     int    `yaml:"ram_floor_mb"`
	DiskFloorGB    int    `yaml:"disk_floor_gb"`
}

// RecommendedJobs are the schedules `gvm create` offers, keyed by job name. They
// are a suggestion: the instance's own jobs block is what systemd renders.
type RecommendedJobs map[string]RecommendedJob

type RecommendedJob struct {
	Action     string   `yaml:"action,omitempty"` // defaults to the job's name
	At         string   `yaml:"at"`               // "HH:MM"
	Jitter     Duration `yaml:"jitter,omitempty"`
	Persistent bool     `yaml:"persistent"`
	Keep       int      `yaml:"keep,omitempty"` // backup retention
	Why        string   `yaml:"why,omitempty"`
}

// EgressRule documents a host the game or its provider legitimately reaches. It
// is doctor input and dashboard copy, NOT enforcement: the enforcing knob is the
// instance's egress.policy, which takes CIDRs (section 18.4).
type EgressRule struct {
	Host string `yaml:"host"`
	Why  string `yaml:"why"`
}
