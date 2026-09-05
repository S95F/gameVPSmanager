// Manifest validation: the ordered checks of ARCHITECTURE.md 8.6.
//
// Everything here runs in `gvm game validate <file>`, in `gvm apply`, and in a
// Go test over games/*.yaml — so a broken shipped manifest fails CI rather than
// a customer's 3am restart.
//
// Two properties are deliberate and load-bearing.
//
// POSITIONS. Every diagnostic carries the manifest's own file, line and column,
// which is why loading decodes twice: once into a yaml.Node tree for geometry
// (nodeindex.go) and once into the structs for values. A message that cannot say
// where it applies makes the operator grep a 300-line YAML file.
//
// ORDER AND SUPPRESSION. The checks run in the order below, and a check does not
// speak about a node an earlier check already condemned. A `proto: tcp/udp`
// typo should produce ONE diagnostic, not one from the protocol check plus one
// from the firewall check plus one from the flattening — a wall of ten
// diagnostics for one mistake trains people to read the first line and guess.

package spec

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The schema's own identity. BuiltDriverABI is what THIS binary implements: a
// manifest asking for a higher one is refused, which is the only one of the
// three version axes that can make an old binary reject a new manifest (8.1).
const (
	APIVersionV1   = "gvm/v1"
	KindGame       = "Game"
	KindInstance   = "Instance"
	BuiltDriverABI = 1
)

// Provider kinds (section 10).
const (
	ProviderSteamCMD  = "steamcmd"
	ProviderMinecraft = "minecraft"
	ProviderAdopted   = "adopted"
	ProviderNone      = "none"
)

// Content sources (section 12).
const (
	SourceSteamWorkshop = "steam_workshop"
	SourceModrinth      = "modrinth"
	SourceCurseForge    = "curseforge"
)

// Content acquisition mechanisms.
const (
	AcquireServerBoot   = "server_boot"
	AcquireSteamCMDItem = "steamcmd_item"
	AcquireHTTP         = "http"
)

// Console capture modes (section 11.3).
const (
	ConsoleModePTY  = "pty"
	ConsoleModePipe = "pipe"
	ConsoleModeNone = "none"
)

// Config file ownership (section 8.4).
const (
	OwnerServer = "server"
	OwnerGVM    = "gvm"
)

// SteamCMD target platforms.
const (
	PlatformLinux   = "linux"
	PlatformWindows = "windows"
	PlatformMacOS   = "macos"
)

// Shutdown step channels (section 11.6).
const (
	ViaConsole  = "console"
	ViaControl  = "control"
	ViaSignal   = "signal"
	ViaWaitExit = "wait_exit"
)

// When a var change takes effect. The badge next to every dashboard field.
const (
	ApplyLive    = "live"
	ApplyRestart = "restart"
	ApplyRebuild = "rebuild"
)

// Var types.
const (
	VarString   = "string"
	VarInt      = "int"
	VarBool     = "bool"
	VarMemory   = "memory"
	VarEnum     = "enum"
	VarDuration = "duration"
	VarSecret   = "secret"
	VarPassword = "password"
)

// registry is one closed name set plus how well this build implements each
// entry. An unknown name is an error naming the nearest match; a known but
// unimplemented one is a warning, so 7daystodie.yaml can ship in v1 declaring a
// telnet control channel and render honestly instead of failing to load (10.2).
type registry struct {
	what    string // "hook", "content source" -- appears in the message
	entries map[string]Support
}

func (r registry) lookup(name string) Support { return r.entries[name] }

func (r registry) names() []string {
	out := make([]string, 0, len(r.entries))
	for n := range r.entries {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (r registry) nearest(name string) string { return nearestName(name, r.names()) }

var (
	providerKinds = registry{what: "provider kind", entries: map[string]Support{
		ProviderSteamCMD:  SupportImplemented,
		ProviderAdopted:   SupportImplemented,
		ProviderNone:      SupportImplemented,
		ProviderMinecraft: SupportPlanned,
	}}

	contentSources = registry{what: "content source", entries: map[string]Support{
		SourceSteamWorkshop: SupportImplemented,
		SourceModrinth:      SupportPlanned,
		SourceCurseForge:    SupportPlanned,
	}}

	// configFormats mirrors the codec table of section 8.4. It is a schema fact
	// — which format names a manifest may write — and lives here rather than
	// being read out of internal/gamecfg's registry, because validation must
	// work whether or not anything imported gamecfg for its side effects. A
	// gamecfg test cross-checks the two name sets; it can import this package,
	// and this package must never import it.
	configFormats = registry{what: "config format", entries: map[string]Support{
		"pz_ini":             SupportImplemented,
		"lua_sandboxvars":    SupportImplemented,
		"java_properties":    SupportImplemented,
		"xml_property_list":  SupportImplemented,
		"ue_option_settings": SupportImplemented,
		"json_path":          SupportImplemented,
		"owned_lines":        SupportImplemented,
		"toml_kv":            SupportImplemented,
	}}

	probeKinds = registry{what: "readiness probe kind", entries: map[string]Support{
		"log_marker":   SupportImplemented,
		"tcp_probe":    SupportImplemented,
		"udp_probe":    SupportImplemented,
		"a2s":          SupportImplemented,
		"mc_ping":      SupportImplemented,
		"control_auth": SupportImplemented,
	}}

	// hookNames is the v1 registry of section 8.8. internal/hook.Get is the
	// runtime lookup; this is the load-time one, and hook's own test asserts
	// they name the same set.
	hookNames = registry{what: "hook", entries: map[string]Support{
		"pz.first_boot":      SupportImplemented,
		"pz.apply_sandbox":   SupportImplemented,
		"pz.resolve_mod_ids": SupportImplemented,
		"steam.link_sdk":     SupportImplemented,
		"steam.appid_shim":   SupportImplemented,
		"steam.home_layout":  SupportImplemented,
		"pal.settings_seed":  SupportImplemented,
		"sdtd.seed_config":   SupportImplemented,

		"mc.accept_eula":       SupportPlanned,
		"mc.select_jdk":        SupportPlanned,
		"mc.write_properties":  SupportPlanned,
		"mc.gamerule_fallback": SupportPlanned,
	}}

	resolverNames = registry{what: "activation resolver", entries: map[string]Support{
		"pz.resolve_mod_ids": SupportImplemented,
		"mc.jar_manifest":    SupportPlanned,
	}}

	transformNames = registry{what: "preset transform", entries: map[string]Support{
		"pz_return_to_sandboxvars": SupportImplemented,
	}}

	controlKindNames = registry{what: "control kind", entries: func() map[string]Support {
		m := make(map[string]Support, len(controlSupport))
		for k, v := range controlSupport {
			m[string(k)] = v
		}
		return m
	}()}
)

// knownSignals is the set a shutdown step may name. It is a closed list rather
// than a syscall lookup because this package compiles and tests on Windows, and
// because a manifest has no business sending SIGSYS to a game server.
var knownSignals = map[string]bool{
	"SIGTERM": true, "SIGINT": true, "SIGKILL": true, "SIGHUP": true,
	"SIGQUIT": true, "SIGUSR1": true, "SIGUSR2": true,
}

// ipLiteralRe matches a bare IPv4 or IPv6 literal. `bind:` takes an address, not
// a hostname: resolving one at unit-generation time would bake today's DNS
// answer into a systemd file that outlives it.
var ipLiteralRe = regexp.MustCompile(`^([0-9]{1,3}(\.[0-9]{1,3}){3}|\[?[0-9A-Fa-f:]+\]?)$`)

// Validate runs the ordered checks of 8.6 against an already-decoded manifest.
//
// ix positions the diagnostics and may be nil, in which case they carry a path
// and no line — which is what `gvm apply` gets when it validates a manifest that
// came out of the pin rather than off disk.
//
// The template GUARD is not re-run here. parseManifest refuses a manifest
// outside the closed grammar before it ever becomes a *Manifest, so a manifest
// that reaches validation has already passed; running it again would print every
// template error twice.
func Validate(m *Manifest, ix *NodeIndex) Diags {
	if m == nil {
		return nil
	}
	c := &checker{m: m, ix: ix, bad: map[string]bool{}}

	c.checkSchema()
	c.checkRegistries()
	c.checkDialect()
	c.checkTemplates()
	c.checkPorts()
	c.checkConfigKeys()
	c.checkProvider()
	c.checkShutdown()
	c.checkReadiness()
	c.checkSizing()
	c.checkVars()
	c.checkWarnings()

	return c.out
}

// ValidateBytes is the whole of `gvm game validate <file>`: decode, guard, then
// the ordered checks.
//
// A decode or guard failure returns its own diagnostics and a nil manifest —
// there is nothing to validate — so a caller prints diags either way and only
// looks at the manifest when err is nil.
func ValidateBytes(ref string, raw []byte) (*Manifest, Diags, error) {
	m, ix, diags, err := parseManifestIndexed(ref, raw)
	if err != nil {
		return nil, diags, err
	}
	return m, append(diags, Validate(m, ix)...), nil
}

// checker accumulates diagnostics and remembers which document paths have
// already been condemned.
type checker struct {
	m   *Manifest
	ix  *NodeIndex
	out Diags
	bad map[string]bool
}

// add records a diagnostic. An error marks its path, so a later check that would
// speak about the same node stays quiet.
func (c *checker) add(d Diag) {
	if d.Sev == SevError && d.Path != "" {
		c.bad[d.Path] = true
	}
	c.out = append(c.out, d)
}

func (c *checker) errf(path, format string, args ...any) Diag {
	return c.ix.Errorf(path, format, args...)
}

func (c *checker) warnf(path, format string, args ...any) Diag {
	return c.ix.Warnf(path, format, args...)
}

// ok reports whether every named path is still clean — the path itself and
// every ancestor of it. An ancestor matters: once `spec.ports[1]` is condemned
// there is nothing useful to say about `spec.ports[1].bind`.
func (c *checker) ok(paths ...string) bool {
	for _, p := range paths {
		for q := p; ; q = parentPath(q) {
			if c.bad[q] {
				return false
			}
			if q == "" {
				break
			}
		}
	}
	return true
}

// closed checks one value against a registry and reports the outcome. It returns
// false when the value is unusable, so the caller can skip whatever depended on
// it.
func (c *checker) closed(r registry, path, value string) bool {
	switch r.lookup(value) {
	case SupportImplemented:
		return true
	case SupportPlanned:
		c.add(c.warnf(path, "%s %q is known but not implemented in this build", r.what, value).
			WithFix("it loads and validates; the capability it drives renders as unavailable until gvm " + plannedRelease))
		return true
	}
	d := c.errf(path, "%q is not a %s (want %s)", value, r.what, strings.Join(r.names(), "|"))
	if n := r.nearest(value); n != "" {
		d = d.WithFix("did you mean %q?", n)
	}
	c.add(d)
	return false
}

// checkSchema is check 1 of ARCHITECTURE 8.6.
func (c *checker) checkSchema() {
	if c.m.APIVersion != APIVersionV1 {
		c.add(c.errf("apiVersion", "%q is not a known schema version (want %s)",
			c.m.APIVersion, APIVersionV1))
	}
	if c.m.Kind != KindGame {
		c.add(c.errf("kind", "%q is not a game manifest (want %s)", c.m.Kind, KindGame))
	}
	if abi := c.m.Spec.DriverABI; abi > BuiltDriverABI {
		c.add(c.errf("spec.driverABI", "the manifest needs driverABI %d; this build implements %d", abi, BuiltDriverABI).
			WithFix("upgrade gvm, or pin this instance to an earlier manifest revision"))
	} else if abi < 1 {
		c.add(c.errf("spec.driverABI", "driverABI %d is not a version (the first is 1)", abi))
	}
	if !ValidGameID(c.m.Metadata.ID) {
		c.add(c.errf("metadata.id", "%q is not a game id (want %s)", c.m.Metadata.ID, gameIDRe))
	}
	if c.m.Metadata.Revision < 1 {
		c.add(c.errf("metadata.revision", "revision %d is not a revision; the first is 1", c.m.Metadata.Revision).
			WithFix("revision is monotonic per game id and is what `gvm manifest upgrade` diffs by"))
	}
}

// checkRegistries is check 2 of ARCHITECTURE 8.6.
func (c *checker) checkRegistries() {
	c.closed(providerKinds, "spec.provider.kind", c.m.Spec.Provider.Kind)

	for i, cs := range c.m.Spec.Content {
		p := fmt.Sprintf("spec.content[%d]", i)
		c.closed(contentSources, p+".source", cs.Source)
		if mech := cs.Acquisition.Mechanism; mech != "" &&
			mech != AcquireServerBoot && mech != AcquireSteamCMDItem && mech != AcquireHTTP {
			c.add(c.errf(p+".acquisition.mechanism", "%q is not an acquisition mechanism (want %s|%s|%s)",
				mech, AcquireServerBoot, AcquireSteamCMDItem, AcquireHTTP))
		}
		if r := cs.Activation.Resolver; r != "" {
			c.closed(resolverNames, p+".activation.resolver", r)
		}
	}

	for i, cf := range c.m.Spec.Configs {
		p := fmt.Sprintf("spec.config_files[%d]", i)
		c.closed(configFormats, p+".format", cf.Format)
		if cf.Owner != OwnerServer && cf.Owner != OwnerGVM {
			c.add(c.errf(p+".owner", "%q is not an owner (want %s|%s)", cf.Owner, OwnerServer, OwnerGVM).
				WithFix("`server` means the game rewrites the file on shutdown, so gvm edits it only while stopped"))
		}
	}

	for i, pr := range c.m.Spec.Readiness.Probes {
		c.closed(probeKinds, fmt.Sprintf("spec.readiness.probes[%d].kind", i), pr.Kind)
	}

	for _, ph := range hookPhases {
		for i, name := range ph.list(&c.m.Spec.Hooks) {
			c.closed(hookNames, fmt.Sprintf("spec.hooks.%s[%d]", ph.key, i), name)
		}
	}

	for i, v := range c.m.Spec.Validators {
		c.closed(validatorNames, fmt.Sprintf("spec.validators[%d]", i), v)
	}

	if p := c.m.Spec.Presets; p != nil && p.Transform != "" {
		c.closed(transformNames, "spec.presets.transform", p.Transform)
	}

	if ctl := c.m.Spec.Control; ctl != nil {
		c.closed(controlKindNames, "spec.control.kind", ctl.Kind)
	}

	mode := c.m.Spec.Runtime.Console.Mode
	if mode != ConsoleModePTY && mode != ConsoleModePipe && mode != ConsoleModeNone {
		c.add(c.errf("spec.runtime.console.mode", "%q is not a console mode (want %s|%s|%s)",
			mode, ConsoleModePTY, ConsoleModePipe, ConsoleModeNone).
			WithFix("`none` is a real answer; it makes the console capability report unsupported instead of blank"))
	}
}

// hookPhases pairs the manifest key with the field, so the loop that checks
// every phase is one loop and cannot silently skip a phase somebody added.
var hookPhases = []struct {
	key  string
	list func(*HookSpec) []string
}{
	{"validate", func(h *HookSpec) []string { return h.Validate }},
	{"post_install", func(h *HookSpec) []string { return h.PostInstall }},
	{"post_content", func(h *HookSpec) []string { return h.PostContent }},
	{"pre_start", func(h *HookSpec) []string { return h.PreStart }},
	{"post_start", func(h *HookSpec) []string { return h.PostStart }},
	{"pre_backup", func(h *HookSpec) []string { return h.PreBackup }},
	{"post_backup", func(h *HookSpec) []string { return h.PostBackup }},
}

// checkDialect is check 3 of ARCHITECTURE 8.6 (D-07).
func (c *checker) checkDialect() {
	ctl := c.m.Spec.Control
	if ctl == nil || !c.ok("spec.control.kind") {
		return
	}
	if ctl.Dialect == "" && ControlKind(ctl.Kind) == ControlNone {
		return
	}
	if ctl.Dialect.Valid() {
		return
	}
	want := make([]string, 0, 4)
	for _, d := range Dialects() {
		want = append(want, string(d))
	}
	c.add(c.errf("spec.control.dialect", "%q is not a dialect (want %s)", string(ctl.Dialect), strings.Join(want, "|")).
		WithFix("the dialects are exactly the constants in internal/rcon; a manifest cannot ask for behaviour the client does not implement (D-07)"))
}

// secretSurfaces are the only document paths a `.Secret.*` reference may appear
// at. Everywhere else a secret would be rendered into something that gets
// logged, diffed, echoed to a dashboard or written to a world-readable file,
// and taint tracking cannot protect a value from a place it should never have
// reached.
var secretSurfaces = []string{
	"spec.runtime.args",
	"spec.runtime.env",
	"spec.control.password",
	"spec.verbs",
	"spec.config_files.keys",
}

// checkTemplates is check 4 of ARCHITECTURE 8.6.
func (c *checker) checkTemplates() {
	if c.ix == nil {
		return
	}
	for _, path := range TemplateFields(c.ix) {
		refs, te := TemplateRefs(path, c.ix.Node(path).Value, SurfaceFor(path))
		if te != nil {
			// The guard already refused this file at load; reaching here means a
			// caller built a Manifest by hand. Report it rather than skipping.
			c.add(c.errf(path, "%s", te.Msg))
			continue
		}
		for _, r := range refs {
			c.checkRef(path, r)
		}
	}
}

func (c *checker) checkRef(path string, r TemplateRef) {
	switch r.Root {
	case "Var":
		if _, ok := c.m.Spec.Vars[r.Name]; !ok {
			d := c.errf(path, "template references .Var.%s, which is not declared in vars", r.Name)
			if n := nearestName(r.Name, varNames(c.m)); n != "" {
				d = d.WithFix("did you mean .Var.%s?", n)
			} else {
				d = d.WithFix("every variable a template names must appear under spec.vars")
			}
			c.add(d)
		}
	case "Port":
		if _, ok := c.m.Spec.PortByName(r.Name); !ok {
			d := c.errf(path, "template references .Port.%s, which is not declared in ports", r.Name)
			if n := nearestName(r.Name, portNames(c.m)); n != "" {
				d = d.WithFix("did you mean .Port.%s?", n)
			}
			c.add(d)
		}
	case "Content":
		if !contentBlockExists(c.m, r.Name) {
			c.add(c.errf(path, "template references .Content.%s, which is not declared in content", r.Name))
		}
	case "Secret":
		// The name is NOT checked: a secret's declaration site is
		// /etc/gvm/secrets (D-02), not the manifest, so there is nothing here to
		// check it against. Where it may appear is checkable, and is the half
		// that keeps a password out of a log line.
		if !secretAllowedAt(path) {
			c.add(c.errf(path, "a secret may not be rendered here").
				WithFix("`.Secret.*` is allowed only in runtime.args, runtime.env, control.password, verbs and config_files[].keys"))
		}
	}
}

// secretAllowedAt matches a document path against secretSurfaces with list
// indices erased, so spec.config_files[1].keys.Password is the same surface as
// spec.config_files[0].keys.Password.
func secretAllowedAt(path string) bool {
	stripped := indexRe.ReplaceAllString(path, "")
	for _, s := range secretSurfaces {
		if stripped == s || pathHasPrefix(stripped, s) {
			return true
		}
	}
	return false
}

var indexRe = regexp.MustCompile(`\[[0-9]+\]`)

// checkPorts is check 5 of ARCHITECTURE 8.6.
func (c *checker) checkPorts() {
	seen := map[string]int{}
	for i, p := range c.m.Spec.Ports {
		base := fmt.Sprintf("spec.ports[%d]", i)

		if p.Name == "" {
			c.add(c.errf(base+".name", "a port needs a name; templates and probes reference ports by name, never by number"))
		} else if prev, dup := seen[p.Name]; dup {
			c.add(c.errf(base+".name", "port %q is already declared at spec.ports[%d]", p.Name, prev).
				WithFix("names are the allocator's keys; two ports with one name means one of them is never allocated"))
		} else {
			seen[p.Name] = i
		}

		if _, err := ParseProtos(p.Proto); err != nil {
			c.add(c.errf(base+".proto", "%s", err.Error()))
		}

		if p.Default <= 0 || p.Default > 65535 {
			c.add(c.errf(base+".default", "%d is not a port number (want 1-65535)", p.Default))
		}

		if p.Group != "" {
			if n, ok := c.m.Spec.PortStride[p.Group]; !ok {
				c.add(c.errf(base+".group", "group %q has no entry in port_stride", p.Group).
					WithFix("a group shifts as a unit; without a stride the allocator would move it by 1 and break the game's contiguity requirement"))
			} else if n <= 0 {
				c.add(c.errf("spec.port_stride."+p.Group, "stride %d is not a shift; want a positive number", n))
			}
		}

		if p.Bind != "" && !ipLiteralRe.MatchString(p.Bind) {
			c.add(c.errf(base+".bind", "%q is not an IP literal", p.Bind).
				WithFix("bind takes an address, not a hostname: a name resolved at unit-generation time would be baked into a systemd file that outlives the answer"))
		}

		if !p.Expose.Valid() {
			c.add(c.errf(base+".expose", "%q is not an exposure (want %s|%s|%s)",
				string(p.Expose), ExposePublic, ExposeAdmin, ExposeNone))
		}

		// The contradiction. A port advertised to the internet that can only be
		// reached from the box itself is a firewall hole opened for a service
		// nobody can use, and whichever half is wrong, the file does not say.
		if c.ok(base+".expose", base+".bind") && p.Expose == ExposePublic && isLoopback(p.Bind) {
			c.add(c.errf(base+".expose", "expose: public contradicts bind: %s", p.Bind).
				WithFix("a loopback-bound port is reachable only from this host; set expose: none, or drop the bind"))
		}
	}
}

func isLoopback(bind string) bool {
	return strings.HasPrefix(bind, "127.") || bind == "::1" || bind == "[::1]"
}

// checkConfigKeys is check 6 of ARCHITECTURE 8.6.
func (c *checker) checkConfigKeys() {
	seen := map[string]int{}
	byPort := c.m.Spec.ConfigKeyPorts()

	for i, cf := range c.m.Spec.Configs {
		base := fmt.Sprintf("spec.config_files[%d]", i)

		if cf.Name == "" {
			c.add(c.errf(base+".name", "a config file needs a name; ports and hooks reference it by name"))
		} else if prev, dup := seen[cf.Name]; dup {
			c.add(c.errf(base+".name", "config file %q is already declared at spec.config_files[%d]", cf.Name, prev))
		} else {
			seen[cf.Name] = i
		}
		if cf.Path == "" {
			c.add(c.errf(base+".path", "a config file needs a path"))
		}

		deny := map[string]bool{}
		for _, k := range cf.DenyKeys {
			deny[k] = true
		}
		ports := byPort[cf.Name]

		for _, k := range sortedKeys(cf.Keys) {
			kp := base + ".keys." + k
			if err := keySyntax(k); err != nil {
				c.add(c.errf(kp, "%s", err.Error()))
				continue
			}
			if deny[k] {
				c.add(c.errf(kp, "%q is in both keys and deny_keys", k).
					WithFix("deny_keys is refused even to hooks; a key cannot be both owned and forbidden"))
			}
			if port, ok := ports[k]; ok {
				c.add(c.errf(kp, "%q is already written by port %q via config_key", k, port).
					WithFix("config_key is the ONLY way a port reaches a config file, so the allocator and the file cannot drift; delete this key"))
			}
		}
	}

	// config_key is the only thing that carries an allocation into the game's
	// own file, so a key that lands in no file — or that a later port
	// overwrites — is an allocation the game never sees: the firewall opens the
	// number and the server keeps listening on its compiled-in default.
	claimed := map[[2]string]int{}
	for i, p := range c.m.Spec.Ports {
		if p.ConfigKey == "" {
			continue
		}
		base := fmt.Sprintf("spec.ports[%d]", i)

		file := p.ConfigFile
		switch {
		case file != "":
			if _, ok := seen[file]; !ok {
				c.add(c.errf(base+".config_file", "there is no config file named %q", file))
				continue
			}
		case len(c.m.Spec.Configs) > 0:
			file = c.m.Spec.Configs[0].Name
		default:
			c.add(c.errf(base+".config_key",
				"port %q writes %q but the manifest declares no config file", p.Name, p.ConfigKey).
				WithFix("config_key is the ONLY way a port reaches a config file; declare the file under config_files, or drop the config_key"))
			continue
		}

		if prev, dup := claimed[[2]string{file, p.ConfigKey}]; dup {
			c.add(c.errf(base+".config_key",
				"%q in %q is already written by port %q at spec.ports[%d]",
				p.ConfigKey, file, c.m.Spec.Ports[prev].Name, prev).
				WithFix("one key holds one value; the second allocation is opened in the firewall and written nowhere"))
			continue
		}
		claimed[[2]string{file, p.ConfigKey}] = i
	}
}

// keySyntax is the format-independent half of check 6: what no codec could
// accept. The per-codec KeySyntax (a `=` in a pz_ini key, a dotted path that
// resolves into a TOML inline table) runs in internal/gamecfg, which is where
// the codecs live.
func keySyntax(k string) error {
	if strings.TrimSpace(k) != k || k == "" {
		return fmt.Errorf("%q is not a config key: it is empty or has leading/trailing space", k)
	}
	if strings.ContainsAny(k, "\r\n") {
		return fmt.Errorf("%q is not a config key: it spans lines", k)
	}
	if strings.ContainsAny(k, "=") {
		return fmt.Errorf("%q is not a config key: it contains '='", k)
	}
	return nil
}

// checkProvider is check 7 of ARCHITECTURE 8.6.
func (c *checker) checkProvider() {
	if !c.ok("spec.provider.kind") {
		return
	}
	p := c.m.Spec.Provider
	switch p.Kind {
	case ProviderSteamCMD:
		if p.SteamCMD == nil {
			c.add(c.errf("spec.provider.steamcmd", "provider kind steamcmd needs a steamcmd block"))
			return
		}
		c.checkSteamCMD(p.SteamCMD)
	case ProviderMinecraft:
		if p.Minecraft == nil {
			c.add(c.errf("spec.provider.minecraft", "provider kind minecraft needs a minecraft block"))
			return
		}
		c.checkSentinel("spec.provider.minecraft.sentinel", p.Minecraft.Sentinel)
	}
}

func (c *checker) checkSteamCMD(s *SteamCMDSpec) {
	const base = "spec.provider.steamcmd"

	c.checkSentinel(base+".sentinel", s.Sentinel)

	// D-32. gvm bans `validate` on the nightly cycle because it overwrites
	// customised files; the nightly validate is also what silently repaired
	// partial downloads in update.sh. Removing it without a compensating check
	// is a regression, so the compensating check is mandatory rather than
	// recommended.
	if missing := missingVerify(s.Verify); missing != "" {
		c.add(c.errf(base+".verify",
			"steamcmd needs verify.appmanifest and verify.byte_parity; %s not set", missing).
			WithFix("gvm never runs `validate` on the nightly update, so the post-download integrity check is the only thing left that catches a partial install (D-32)"))
	}

	for i, in := range s.ValidateOn {
		ip := fmt.Sprintf("%s.validate_on[%d]", base, i)
		switch in {
		case IntentInstall, IntentRepair, IntentVerify:
		case IntentUpdate:
			c.add(c.errf(ip, "`update` may not be on validate_on").
				WithFix("SteamCMD's validate overwrites customised files; a nightly update that validates silently reverts every hand edit (D-32). Use `repair` for the operator-requested case"))
		default:
			c.add(c.errf(ip, "%q is not an intent (want %s|%s|%s)", string(in),
				IntentInstall, IntentRepair, IntentVerify))
		}
	}

	if s.InstallAppID <= 0 {
		c.add(c.errf(base+".install_appid", "install_appid %d is not an app id", s.InstallAppID))
	}
	if pl := s.Platform; pl != "" && pl != PlatformLinux && pl != PlatformWindows && pl != PlatformMacOS {
		c.add(c.errf(base+".platform", "%q is not a platform (want %s|%s|%s)",
			pl, PlatformLinux, PlatformWindows, PlatformMacOS))
	}
}

// missingVerify names the unset halves of the mandatory check, so the message
// says which line to edit rather than restating the rule.
func missingVerify(v Verify) string {
	switch {
	case !v.AppManifest && !v.ByteParity:
		return "neither appmanifest nor byte_parity is"
	case !v.AppManifest:
		return "appmanifest is"
	case !v.ByteParity:
		return "byte_parity is"
	}
	return ""
}

// checkSentinel enforces the one fact that makes "is it installed?" answerable
// without trusting SteamCMD's exit code: a relative path inside the install tree
// that must exist afterwards.
func (c *checker) checkSentinel(path string, s Sentinel) {
	if s.Path == "" {
		c.add(c.errf(path+".path", "a provider needs a sentinel path").
			WithFix("the sentinel is how gvm knows an install finished; without one a half-downloaded tree looks installed"))
		return
	}
	if strings.HasPrefix(s.Path, "/") || strings.HasPrefix(s.Path, "\\") || strings.Contains(s.Path, "..") {
		c.add(c.errf(path+".path", "%q must be relative to the install directory", s.Path).
			WithFix("an absolute or climbing path would let a sentinel be satisfied by a file outside the instance"))
	}
}

// checkShutdown is check 8 of ARCHITECTURE 8.6.
func (c *checker) checkShutdown() {
	sd := c.m.Spec.Shutdown
	var sum Duration
	for i, st := range sd.Steps {
		base := fmt.Sprintf("spec.shutdown.steps[%d]", i)
		sum += st.Wait

		switch st.Via {
		case ViaConsole:
			if c.ok("spec.runtime.console.mode") && !c.consoleUsable() {
				c.add(c.errf(base+".via", "a console step needs runtime.console.mode other than none").
					WithFix("this is the ladder that stops a live server; a step on a channel the game does not have is a rung that always times out"))
			}
		case ViaControl:
			if c.m.Spec.Control == nil {
				c.add(c.errf(base+".via", "a control step needs spec.control").
					WithFix("with control: null the only command channel is the console (D-08)"))
			}
		case ViaSignal:
			if st.Signal == "" {
				c.add(c.errf(base+".signal", "a signal step needs a signal"))
			} else if !knownSignals[st.Signal] {
				c.add(c.errf(base+".signal", "%q is not a signal (want %s)", st.Signal, orList(sortedKeys(knownSignals))).
					WithFix("Valheim saves on SIGINT and not on SIGTERM, which is why the signal is manifest data"))
			}
		case ViaWaitExit:
		case "":
			c.add(c.errf(base+".via", "a shutdown step needs a via (want %s|%s|%s|%s)",
				ViaConsole, ViaControl, ViaSignal, ViaWaitExit))
		default:
			c.add(c.errf(base+".via", "%q is not a shutdown channel (want %s|%s|%s|%s)",
				st.Via, ViaConsole, ViaControl, ViaSignal, ViaWaitExit))
		}

		if st.Scope != "" && st.Scope != "process" && st.Scope != "process_group" {
			c.add(c.errf(base+".scope", "%q is not a scope (want process|process_group)", st.Scope))
		}
		if st.OnTimeout != "" && st.OnTimeout != "continue" && st.OnTimeout != "escalate" {
			c.add(c.errf(base+".on_timeout", "%q is not an on_timeout (want continue|escalate)", st.OnTimeout))
		}
		c.checkRegexp(base+".wait_for_log", st.WaitForLog)
	}

	if sd.Budget > 0 && sum > sd.Budget {
		c.add(c.errf("spec.shutdown.budget",
			"the steps wait %s in total, which is more than the %s budget", sum, sd.Budget).
			WithFix("budget becomes TimeoutStopSec: systemd would SIGKILL mid-ladder, which is a degraded shutdown every single time"))
	}

	c.checkFreezeStep("spec.backup.freeze.before", c.m.Spec.Backup.Freeze.Before)
	c.checkFreezeStep("spec.backup.freeze.during", c.m.Spec.Backup.Freeze.During)
	c.checkFreezeStep("spec.backup.freeze.after", c.m.Spec.Backup.Freeze.After)
}

func (c *checker) checkFreezeStep(path string, st *ShutdownStep) {
	if st == nil {
		return
	}
	switch st.Via {
	case ViaConsole:
		if c.ok("spec.runtime.console.mode") && !c.consoleUsable() {
			c.add(c.errf(path+".via", "a console step needs runtime.console.mode other than none"))
		}
	case ViaControl:
		if c.m.Spec.Control == nil {
			c.add(c.errf(path+".via", "a control step needs spec.control"))
		}
	case ViaSignal, ViaWaitExit:
		c.add(c.errf(path+".via", "a freeze rung must be %s or %s", ViaConsole, ViaControl).
			WithFix("freezing a world is a command, not a signal; write `none` if this game has no such command"))
	default:
		c.add(c.errf(path+".via", "%q is not a freeze channel (want %s|%s)", st.Via, ViaConsole, ViaControl))
	}
	c.checkRegexp(path+".wait_for_log", st.WaitForLog)

	// The lease exists because dying between save-off and save-on leaves a
	// running server that has silently stopped persisting — the worst failure in
	// the system (D-31). A During with no After is exactly that shape.
	if path == "spec.backup.freeze.during" && c.m.Spec.Backup.Freeze.After == nil {
		c.add(c.errf(path, "a `during` rung with no `after` leaves the world frozen").
			WithFix("every freeze needs its thaw; write the matching `after` step"))
	}
}

func (c *checker) consoleUsable() bool {
	m := c.m.Spec.Runtime.Console.Mode
	return m == ConsoleModePTY || m == ConsoleModePipe
}

// checkReadiness is check 9 of ARCHITECTURE 8.6.
func (c *checker) checkReadiness() {
	r := c.m.Spec.Readiness

	// D-12. hard_deadline becomes TimeoutStartSec; `infinity` (which is what an
	// absent value would have to mean) leaves a wedged runner in `activating`
	// forever with Restart=on-failure never firing.
	if r.HardDeadline <= 0 {
		c.add(c.errf("spec.readiness.hard_deadline", "hard_deadline is required and must be finite").
			WithFix("it becomes TimeoutStartSec; without it a wedged start sits in `activating` forever and Restart=on-failure never fires (D-12)"))
	} else if r.StallTimeout > 0 && r.StallTimeout >= r.HardDeadline {
		c.add(c.errf("spec.readiness.stall_timeout",
			"stall_timeout %s is not shorter than hard_deadline %s", r.StallTimeout, r.HardDeadline).
			WithFix("the stall clock exists to fail a silent server EARLY; at or above the deadline it can never fire"))
	}

	if len(r.Probes) == 0 {
		c.add(c.errf("spec.readiness.probes", "a game needs at least one readiness probe").
			WithFix("without one there is nothing to send READY=1 on, and the unit sits in `activating` until hard_deadline"))
	}

	for i, p := range r.Probes {
		base := fmt.Sprintf("spec.readiness.probes[%d]", i)
		c.checkRegexp(base+".pattern", p.Pattern)
		if p.Port != "" {
			if _, ok := c.m.Spec.PortByName(p.Port); !ok {
				d := c.errf(base+".port", "there is no port named %q", p.Port)
				if n := nearestName(p.Port, portNames(c.m)); n != "" {
					d = d.WithFix("did you mean %q?", n)
				}
				c.add(d)
			}
		}
		if p.Kind == "log_marker" && p.Pattern == "" {
			c.add(c.errf(base+".pattern", "a log_marker probe needs a pattern"))
		}
	}

	c.checkRegexp("spec.readiness.progress_pattern", r.ProgressPattern)
	for i, f := range r.Fail {
		c.checkRegexp(fmt.Sprintf("spec.readiness.fail_markers[%d].pattern", i), f.Pattern)
	}
}

// checkRegexp compiles a pattern AT LOAD TIME. A pattern that only fails when
// the line it was meant to match arrives is a readiness probe that hangs until
// hard_deadline, at the end of a 40-minute boot.
func (c *checker) checkRegexp(path, pattern string) {
	if pattern == "" {
		return
	}
	if _, err := regexp.Compile(pattern); err != nil {
		// regexp's error repeats the pattern at the end in backquotes; the
		// diagnostic already quotes it once, and twice reads like two problems.
		msg := strings.TrimPrefix(err.Error(), "error parsing regexp: ")
		if i := strings.LastIndex(msg, ": `"); i > 0 {
			msg = msg[:i]
		}
		c.add(c.errf(path, "%q is not a valid RE2 pattern: %s", pattern, msg))
	}
}

// checkSizing is check 10 of ARCHITECTURE 8.6.
func (c *checker) checkSizing() {
	s := c.m.Spec.Sizing
	switch s.Model {
	case MemoryModelHeapPlusHeadroom:
		if s.HeapVar == "" {
			c.add(c.errf("spec.sizing.heap_var", "memory_model %s needs a heap_var", s.Model))
		} else if v, ok := c.m.Spec.Vars[s.HeapVar]; !ok {
			c.add(c.errf("spec.sizing.heap_var", "heap_var names %q, which is not declared in vars", s.HeapVar))
		} else if v.Type != VarMemory {
			c.add(c.errf("spec.sizing.heap_var", "heap_var names %q, whose type is %q, not %s",
				s.HeapVar, v.Type, VarMemory))
		}
	case MemoryModelFlat:
		if s.RAMFloorMB <= 0 {
			c.add(c.errf("spec.sizing.ram_floor_mb", "memory_model flat needs a positive ram_floor_mb"))
		}
	case "":
		c.add(c.errf("spec.sizing.memory_model", "memory_model is required (want %s)", orList(MemoryModels())))
	default:
		c.add(c.errf("spec.sizing.memory_model", "%q is not a memory model (want %s)",
			s.Model, orList(MemoryModels())))
	}

	c.checkSize("spec.sizing.native_headroom", s.NativeHeadroom, true)
	c.checkSize("spec.sizing.high_margin", s.HighMargin, false)
}

func (c *checker) checkSize(path, value string, required bool) {
	if value == "" {
		if required {
			c.add(c.errf(path, "a size is required here (systemd's K/M/G/T, base 1024)"))
		}
		return
	}
	if _, err := ParseBytes(value); err != nil {
		c.add(c.errf(path, "%s", strings.TrimPrefix(err.Error(), "invalid byte size: ")))
	}
}

// checkVars is check 11 of ARCHITECTURE 8.6.
func (c *checker) checkVars() {
	for _, name := range sortedKeys(c.m.Spec.Vars) {
		v := c.m.Spec.Vars[name]
		base := "spec.vars." + name

		switch v.Type {
		case VarString, VarSecret, VarPassword:
		case VarInt, VarBool, VarMemory, VarDuration:
		case VarEnum:
			if len(v.Enum) == 0 && v.EnumFrom == "" {
				c.add(c.errf(base+".values", "an enum var needs `values` or `values_from`").
					WithFix("an enum with no values renders as a dropdown with nothing in it"))
			}
		case "":
			c.add(c.errf(base+".type", "a var needs a type (want %s)", orList(varTypes())))
		default:
			d := c.errf(base+".type", "%q is not a var type (want %s)", v.Type, orList(varTypes()))
			if n := nearestName(v.Type, varTypes()); n != "" {
				d = d.WithFix("did you mean %q?", n)
			}
			c.add(d)
		}

		if v.EnumFrom != "" && v.EnumFrom != "presets" {
			c.add(c.errf(base+".values_from", "%q is not a value source (want presets)", v.EnumFrom))
		}
		if v.EnumFrom == "presets" && c.m.Spec.Presets == nil {
			c.add(c.errf(base+".values_from", "values_from: presets, but the game declares no presets block"))
		}

		switch v.Apply {
		case ApplyLive, ApplyRestart, ApplyRebuild:
		case "":
			c.add(c.errf(base+".apply", "a var needs an apply (want %s)", orList([]string{ApplyLive, ApplyRestart, ApplyRebuild})).
				WithFix("apply is the badge next to the field; without it nobody knows whether editing this costs a restart"))
		default:
			c.add(c.errf(base+".apply", "%q is not an apply mode (want %s)", v.Apply,
				orList([]string{ApplyLive, ApplyRestart, ApplyRebuild})))
		}

		c.checkRegexp(base+".pattern", v.Pattern)

		if c.ok(base+".type") && v.Default != "" {
			if err := checkVarValue(v, v.Default); err != nil {
				c.add(c.errf(base+".default", "%s", err.Error()))
			}
		}

		// An instance that can never be created. required means the operator must
		// supply a value, immutable means it can only be supplied at creation,
		// and no default means the form starts empty — which is fine — but the
		// three together with an enum that has no values is the uncreatable case
		// 8.6 names.
		if v.Required && v.Immutable && v.Default == "" && v.Type == VarEnum && len(v.Enum) == 0 && v.EnumFrom == "" {
			c.add(c.errf(base, "required, immutable, no default and no values: this instance can never be created"))
		}

		if v.Min != nil && v.Max != nil && *v.Min > *v.Max {
			c.add(c.errf(base+".min", "min %d is greater than max %d", *v.Min, *v.Max))
		}
	}
}

// checkVarValue parses a value the way its declared type says to. It is shared
// with the instance-side validation, where the values are the operator's rather
// than the manifest's defaults.
func checkVarValue(v VarSpec, value string) error {
	switch v.Type {
	case VarInt:
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%q is not an integer", value)
		}
		if v.Min != nil && n < *v.Min {
			return fmt.Errorf("%d is below the declared minimum %d", n, *v.Min)
		}
		if v.Max != nil && n > *v.Max {
			return fmt.Errorf("%d is above the declared maximum %d", n, *v.Max)
		}
	case VarBool:
		switch strings.ToLower(value) {
		case "true", "false", "1", "0", "yes", "no", "on", "off":
		default:
			return fmt.Errorf("%q is not a boolean (want true|false)", value)
		}
	case VarMemory:
		if _, err := ParseBytes(value); err != nil {
			return fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "invalid byte size: "))
		}
	case VarDuration:
		if _, err := ParseDuration(value); err != nil {
			return fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "invalid duration: "))
		}
	case VarEnum:
		if len(v.Enum) == 0 {
			return nil // values_from resolves at apply time against the install tree
		}
		for _, e := range v.Enum {
			if e == value {
				return nil
			}
		}
		return fmt.Errorf("%q is not one of %s", value, orList(v.Enum))
	}
	if v.Pattern != "" {
		re, err := regexp.Compile(v.Pattern)
		if err == nil && !re.MatchString(value) {
			return fmt.Errorf("%q does not match the declared pattern %q", value, v.Pattern)
		}
	}
	return nil
}

func varTypes() []string {
	return []string{VarString, VarInt, VarBool, VarMemory, VarEnum, VarDuration, VarSecret, VarPassword}
}

// checkWarnings is check 12 of ARCHITECTURE 8.6: the cross-cutting
// warnings and the `when:` guards.
func (c *checker) checkWarnings() {
	if c.m.Spec.Control != nil {
		if _, ok := c.m.Spec.Verbs["players"]; !ok {
			c.add(c.warnf("spec.verbs", "the game has a control channel but declares no `players` verb").
				WithFix("the dashboard omits the player list rather than showing an empty one; add `players: {control: ...}` if the game answers it"))
		}
	}

	include := map[string]bool{}
	for _, p := range c.m.Spec.Backup.Include {
		include[p] = true
	}
	for i, w := range c.m.Spec.Backup.WorldPaths {
		if !include[w] {
			c.add(c.warnf(fmt.Sprintf("spec.backup.world_paths[%d]", i),
				"%q is a world path but is not in backup.include", w).
				WithFix("a world that is never backed up cannot be restored after the rebuild that protected it"))
		}
	}

	if d := c.m.Metadata.Deprecated; d != "" {
		c.add(c.warnf("metadata.deprecated", "this manifest is deprecated: %s", d))
	}

	c.checkWhens()
}

// checkWhens validates every `when:` guard in the document. Args are the only
// place the schema puts one — a config key is a plain string, so its
// conditionality is expressed by the arg that sets it — and When.Check needs the
// index rather than the struct, because it reports a collision at the key the
// FILE wrote second and a decoded struct has no memory of document order.
func (c *checker) checkWhens() {
	for i, a := range c.m.Spec.Runtime.Args {
		if a.When != nil {
			c.out = append(c.out, a.When.Check(c.ix, fmt.Sprintf("spec.runtime.args[%d].when", i))...)
		}
	}
}

// ConfigFormats reports every `format:` a manifest may name, and how far this
// build implements each.
//
// It exists so internal/gamecfg's test can prove its codec registry names the
// same set. The two lists are kept separately on purpose — validation must not
// depend on a codec package having been imported for its side effects — and a
// silent divergence would mean either a format that validates and then has no
// codec, or a codec no manifest is allowed to ask for.
func ConfigFormats() map[string]Support {
	out := make(map[string]Support, len(configFormats.entries))
	for k, v := range configFormats.entries {
		out[k] = v
	}
	return out
}

// HookNames reports every hook a manifest may name, and how far this build
// implements each.
//
// This is the LOAD-time registry; internal/hook.Get is the runtime one, and
// hook's own test asserts the two name the same set. `gvm game hooks` prints
// this list so an author can see what is nameable without reading Go.
func HookNames() map[string]Support {
	out := make(map[string]Support, len(hookNames.entries))
	for k, v := range hookNames.entries {
		out[k] = v
	}
	return out
}

func varNames(m *Manifest) []string { return sortedKeys(m.Spec.Vars) }
func portNames(m *Manifest) []string {
	out := make([]string, 0, len(m.Spec.Ports))
	for _, p := range m.Spec.Ports {
		out = append(out, p.Name)
	}
	return out
}

func contentBlockExists(m *Manifest, name string) bool {
	for _, cs := range m.Spec.Content {
		if cs.Name == name {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// orList renders a closed set the way a message should read them.
func orList(vals []string) string { return strings.Join(vals, "|") }

// nearestName returns the closest candidate within an edit distance that makes
// the suggestion trustworthy. A suggestion that is merely the alphabetically
// first entry is worse than none: it sends the reader to rewrite a line that was
// not the problem.
func nearestName(name string, candidates []string) string {
	best, bestDist := "", 1<<30
	for _, cand := range candidates {
		if d := editDistance(strings.ToLower(name), strings.ToLower(cand)); d < bestDist {
			best, bestDist = cand, d
		}
	}
	if bestDist > len(name)/3+1 {
		return ""
	}
	return best
}

// editDistance is Levenshtein, used only to decide whether a "did you mean"
// suggestion is trustworthy. Manifest name sets are tiny, so the row-at-a-time
// implementation is more than fast enough and has no allocation surprises.
func editDistance(a, b string) int {
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
