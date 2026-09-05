package spec

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// sampleManifest exercises every shape the schema has to survive: the terse and
// the expanded arg forms, a control block, a port that names a config key, a
// two-namespace content block, and a freeze whose thaw rung is `none`.
const sampleManifest = `
apiVersion: gvm/v1
kind: Game
metadata:
  id: projectzomboid
  name: Project Zomboid
  revision: 7
  tags: [steam, survival, workshop, rcon]
  doc: |
    RCON does not bind until the server is fully booted.
spec:
  driverABI: 1
  provider:
    kind: steamcmd
    steamcmd:
      install_appid: 380870
      anonymous: true
      branch: public
      platform: linux
      validate_on: [install, repair]
      sentinel: {path: start-server.sh, executable: true}
      verify: {appmanifest: true, byte_parity: true, single_acf: true}
  runtime:
    exec: ./start-server.sh
    cwd: "{{ .Install }}"
    args:
      - "-cachedir={{ .Data }}"
      - "-servername"
      - {value: "-adminpassword", when: {phase: install}}
      - {values: ["-password", "{{ .Secret.join_password }}"], when: {secret_set: join_password}}
    env:
      HOME: "{{ .Home }}"
      TERM: dumb
    console:
      mode: pty
      strip_ansi: true
      charset: utf8
      line_ending: lf
      rows: 50
      cols: 200
      echo_input: true
      ring_bytes: 2097152
    process_group: true
    console_compat: screen
    arch: native
    jit: true
    limits: {nofile: 100000, tasks_max: 4096}
  readiness:
    hard_deadline: 3600s
    stall_timeout: 600s
    progress: ["Loading", "Steam initialized"]
    progress_pattern: "(?i)(workshop|downloading)"
    probes:
      - {kind: log_marker, pattern: "SERVER STARTED", timeout: 3600s}
      - {kind: udp_probe, port: game, after: log_marker, timeout: 60s, every: 3s}
    fail_markers:
      - {pattern: "Address already in use", fatal: true, hint: "check ss -lunp"}
  shutdown:
    budget: 300s
    extend_while_progressing: true
    progress_window: 45s
    steps:
      - via: console
        send: "save"
        wait_for_log: "(?i)saved"
        wait: 30s
        on_timeout: continue
      - {via: console, send: "quit", wait: 5s}
      - {via: signal, signal: SIGTERM, scope: process_group, wait: 30s}
  control:
    kind: source_rcon
    port: rcon
    password: "{{ .Secret.rcon_password }}"
    dialect: simple
    available_only_when_ready: true
    not_ready_reason: "RCON does not bind until Project Zomboid finishes booting"
    min_poll: 60s
  verbs:
    save: {console: "save", control: "save"}
    players: {control: "players"}
  port_stride: {pz: 10, admin: 1}
  ports:
    - {name: game, proto: udp, default: 16261, group: pz, expose: public,
       config_key: DefaultPort, config_file: server_ini}
    - {name: rcon, proto: tcp, default: 27015, group: admin, expose: none,
       bind: 127.0.0.1, config_key: RCONPort, config_file: server_ini,
       note: "plaintext remote console"}
  config_files:
    - name: server_ini
      path: "{{ .Data }}/Server/{{ .Var.server_name }}.ini"
      format: pz_ini
      owner: server
      create_if_missing: false
      mode: "0640"
      keys:
        Public: "{{ .Var.public }}"
        RCONPassword: "{{ .Secret.rcon_password }}"
      deny_keys: [SteamVAC]
  presets:
    source_glob: "{{ .Install }}/media/lua/shared/Sandbox/*.lua"
    exclude: [SandboxVars]
    target: sandbox
    transform: pz_return_to_sandboxvars
    overrides_var: sandbox_overrides
  content:
    - name: workshop
      source: steam_workshop
      workshop_appid: 108600
      order_significant: true
      roots: ["{{ .Data }}/steamapps/workshop/content/108600"]
      acquisition:
        mechanism: server_boot
        write_key: WorkshopItems
        write_file: server_ini
        separator: ";"
        keep_missing: true
        boot_timeout: 3600s
        verify: {dir_non_empty: true}
      activation:
        write_key: Mods
        write_file: server_ini
        separator: ";"
        resolver: pz.resolve_mod_ids
        resolver_vars:
          target_build: "{{ .Var.pz_build }}"
      metadata_api: steam_remote_storage
      collection_sync: true
      owned_keys: [WorkshopItems, Mods]
      impact: {add: download_cycle, remove: restart}
  hooks:
    post_install: [pz.first_boot]
    post_content: [pz.resolve_mod_ids]
    pre_start: [pz.apply_sandbox]
  validators: [pz.server_name_is_world_dir]
  backup:
    include: ["{{ .Data }}/Server"]
    include_optional: ["{{ .Data }}/Lua"]
    exclude: ["**/*.lock"]
    world_paths: ["{{ .Data }}/Saves"]
    destroy_protected: ["{{ .Data }}/db"]
    freeze:
      before: {via: console, send: "save", wait_for_log: "(?i)saved", wait: 20s}
      after: none
      lease_budget: 120s
  vars:
    server_name: {type: string, default: servertest, immutable: true, apply: rebuild}
    jvm_xmx: {type: memory, default: 5g, apply: restart, group: Memory, unit: GiB}
    max_players: {type: int, default: "8", min: 1, max: 64, apply: restart}
    pz_build: {type: enum, values: ["42", "41"], default: "42", apply: rebuild}
  sizing:
    memory_model: heap_plus_headroom
    heap_var: jvm_xmx
    native_headroom: 3G
    high_margin: 1G
    ram_floor_mb: 8192
    disk_floor_gb: 12
  recommended_jobs:
    nightly: {action: update, at: "05:00", jitter: 120s, persistent: false, why: "clears heap pressure"}
    backup: {at: "04:00", keep: 14, persistent: true}
  egress:
    - {host: "*.steampowered.com", why: "SteamCMD + workshop downloads"}
`

func decodeManifest(t *testing.T, src string) Manifest {
	t.Helper()
	var m Manifest
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	return m
}

func TestManifestDecodesTheShippedShape(t *testing.T) {
	m := decodeManifest(t, sampleManifest)

	if m.APIVersion != "gvm/v1" || m.Kind != "Game" {
		t.Errorf("apiVersion/kind = %q/%q", m.APIVersion, m.Kind)
	}
	if m.Metadata.ID != "projectzomboid" || m.Metadata.Revision != 7 {
		t.Errorf("metadata = %+v", m.Metadata)
	}
	if m.Spec.DriverABI != 1 {
		t.Errorf("driverABI = %d", m.Spec.DriverABI)
	}

	sc := m.Spec.Provider.SteamCMD
	if sc == nil {
		t.Fatal("provider.steamcmd is nil")
	}
	if sc.InstallAppID != 380870 || !sc.Anonymous {
		t.Errorf("steamcmd = %+v", sc)
	}
	if got, want := sc.ValidateOn, []Intent{IntentInstall, IntentRepair}; !reflect.DeepEqual(got, want) {
		t.Errorf("validate_on = %v, want %v", got, want)
	}
	if !sc.Verify.AppManifest || !sc.Verify.ByteParity || !sc.Verify.SingleACF {
		t.Errorf("verify = %+v, all three are mandatory for steamcmd", sc.Verify)
	}

	if got := m.Spec.Readiness.HardDeadline.Duration(); got != time.Hour {
		t.Errorf("hard_deadline = %v, want 1h", got)
	}
	if got := m.Spec.Readiness.Probes[1].Every.Duration(); got != 3*time.Second {
		t.Errorf("probes[1].every = %v", got)
	}
	if m.Spec.Runtime.Console.Rows != 50 || m.Spec.Runtime.Console.Cols != 200 {
		t.Errorf("console rows/cols = %d/%d, want 50/200",
			m.Spec.Runtime.Console.Rows, m.Spec.Runtime.Console.Cols)
	}
	if m.Spec.Runtime.Console.RingBytes != 2097152 {
		t.Errorf("ring_bytes = %d", m.Spec.Runtime.Console.RingBytes)
	}

	if m.Spec.Control == nil {
		t.Fatal("control is nil")
	}
	if m.Spec.Control.Dialect != DialectSimple {
		t.Errorf("dialect = %q, want %q (D-07)", m.Spec.Control.Dialect, DialectSimple)
	}
	if got := m.Spec.Control.MinPoll.Duration(); got != time.Minute {
		t.Errorf("min_poll = %v", got)
	}

	if len(m.Spec.Ports) != 2 {
		t.Fatalf("ports = %d, want 2", len(m.Spec.Ports))
	}
	if m.Spec.Ports[0].Expose != ExposePublic || m.Spec.Ports[1].Expose != ExposeNone {
		t.Errorf("expose = %q/%q", m.Spec.Ports[0].Expose, m.Spec.Ports[1].Expose)
	}
	if m.Spec.Ports[1].Bind != "127.0.0.1" || m.Spec.Ports[1].ConfigKey != "RCONPort" {
		t.Errorf("rcon port = %+v", m.Spec.Ports[1])
	}
	if m.Spec.PortStride["pz"] != 10 || m.Spec.PortStride["admin"] != 1 {
		t.Errorf("port_stride = %v", m.Spec.PortStride)
	}

	c := m.Spec.Content[0]
	if c.Acquisition.Separator != ";" || c.Activation.Separator != ";" {
		t.Errorf("separators = %q/%q; a comma breaks PZ's list silently",
			c.Acquisition.Separator, c.Activation.Separator)
	}
	if c.Acquisition.WriteKey != "WorkshopItems" || c.Activation.WriteKey != "Mods" {
		t.Errorf("the two namespaces collapsed: %q / %q",
			c.Acquisition.WriteKey, c.Activation.WriteKey)
	}
	if !c.Acquisition.Verify.DirNonEmpty {
		t.Error("acquisition.verify.dir_non_empty lost")
	}
	if c.Activation.ResolverVars["target_build"] != "{{ .Var.pz_build }}" {
		t.Errorf("resolver_vars = %v", c.Activation.ResolverVars)
	}
	if c.WorkshopAppID != 108600 {
		t.Errorf("workshop_appid = %d, want the GAME appid 108600", c.WorkshopAppID)
	}

	if got := m.Spec.Vars["max_players"]; got.Min == nil || *got.Min != 1 || got.Max == nil || *got.Max != 64 {
		t.Errorf("max_players bounds lost: %+v", got)
	}
	if got := m.Spec.Vars["pz_build"].Enum; !reflect.DeepEqual(got, []string{"42", "41"}) {
		t.Errorf("pz_build values = %v", got)
	}
	if !m.Spec.Vars["server_name"].Immutable {
		t.Error("server_name lost its immutable flag; it names the world directory")
	}

	if got, want := m.Spec.Hooks.PostInstall, []string{"pz.first_boot"}; !reflect.DeepEqual(got, want) {
		t.Errorf("hooks.post_install = %v", got)
	}
	if m.Spec.Presets == nil || m.Spec.Presets.Transform != "pz_return_to_sandboxvars" {
		t.Errorf("presets = %+v", m.Spec.Presets)
	}
	if m.Spec.Sizing.HeapVar != "jvm_xmx" || m.Spec.Sizing.NativeHeadroom != "3G" {
		t.Errorf("sizing = %+v", m.Spec.Sizing)
	}
	if j := m.Spec.Jobs["nightly"]; j.Action != "update" || j.Jitter.Duration() != 2*time.Minute {
		t.Errorf("recommended_jobs.nightly = %+v", j)
	}
	if j := m.Spec.Jobs["backup"]; j.Keep != 14 || !j.Persistent {
		t.Errorf("recommended_jobs.backup = %+v", j)
	}
	if len(m.Spec.Egress) != 1 || m.Spec.Egress[0].Host != "*.steampowered.com" {
		t.Errorf("egress = %+v", m.Spec.Egress)
	}
}

func TestManifestYAMLTagsRoundTrip(t *testing.T) {
	first := decodeManifest(t, sampleManifest)
	assertYAMLFixpoint(t, first, func(src string) any { return decodeManifest(t, src) })
}

func TestArgAcceptsBothFormsAndKeepsTheTerseOne(t *testing.T) {
	m := decodeManifest(t, sampleManifest)
	args := m.Spec.Runtime.Args

	if len(args) != 4 {
		t.Fatalf("args = %d, want 4", len(args))
	}
	if args[0].Value != "-cachedir={{ .Data }}" || args[0].When != nil {
		t.Errorf("terse arg decoded as %+v", args[0])
	}
	if args[2].Value != "-adminpassword" || args[2].When == nil || args[2].When.Phase != "install" {
		t.Errorf("value+when arg decoded as %+v", args[2])
	}
	if got := args[3].Values; !reflect.DeepEqual(got, []string{"-password", "{{ .Secret.join_password }}"}) {
		t.Errorf("values arg = %v", got)
	}
	if args[3].When == nil || args[3].When.SecretSet != "join_password" {
		t.Errorf("values arg when = %+v", args[3].When)
	}

	// A plain argument must re-emit as a plain string, or every save rewrites
	// every manifest's args block into mappings.
	out, err := yaml.Marshal(args[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "-cachedir={{ .Data }}" {
		t.Errorf("terse arg re-emitted as %q", got)
	}
}

func TestArgRejectsAnEmptyEntry(t *testing.T) {
	var rs RuntimeSpec
	err := yaml.Unmarshal([]byte("args:\n  - \n  - \"-x\"\n"), &rs)
	if err == nil {
		t.Fatalf("a null argv element decoded as %+v", rs.Args)
	}
	if !strings.Contains(err.Error(), "is empty") {
		t.Errorf("error %q does not explain the empty argument", err)
	}
}

// `after: none` is how the shipped manifests say a one-shot save has no matching
// thaw. Decoding it as a step with no via would put an unexecutable rung in the
// freeze ladder.
func TestFreezeNoneIsAbsentNotEmpty(t *testing.T) {
	m := decodeManifest(t, sampleManifest)
	f := m.Spec.Backup.Freeze

	if f.Before == nil || f.Before.Send != "save" {
		t.Errorf("freeze.before = %+v", f.Before)
	}
	if f.Before.Wait.Duration() != 20*time.Second {
		t.Errorf("freeze.before.wait = %v", f.Before.Wait.Duration())
	}
	if f.After != nil {
		t.Errorf("freeze.after = %+v, want nil for `none`", f.After)
	}
	if f.During != nil {
		t.Errorf("freeze.during = %+v, want nil when absent", f.During)
	}
	if f.LeaseBudget.Duration() != 2*time.Minute {
		t.Errorf("lease_budget = %v", f.LeaseBudget.Duration())
	}
}

func TestFreezeRejectsAnUnknownScalarRung(t *testing.T) {
	var b BackupSpec
	err := yaml.Unmarshal([]byte("freeze:\n  after: maybe\n"), &b)
	if err == nil {
		t.Fatalf("freeze.after: maybe decoded as %+v", b.Freeze.After)
	}
	if !strings.Contains(err.Error(), "none") {
		t.Errorf("error %q does not point at `none`", err)
	}
}

func TestControlNullMeansConsoleOnly(t *testing.T) {
	var m Manifest
	src := "spec:\n  control: null\n  verbs:\n    save: {console: \"save\"}\n"
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Spec.Control != nil {
		t.Errorf("control = %+v, want nil", m.Spec.Control)
	}
	if m.Spec.Verbs["save"].Console != "save" {
		t.Errorf("verbs = %v", m.Spec.Verbs)
	}
	if _, ok := m.Spec.Verbs["players"]; ok {
		t.Error("a verb appeared that the manifest never declared")
	}
}

// D-20: `public: true` is the pre-expose spelling and must keep loading, but a
// file that says both cannot be resolved by picking a winner.
func TestPortAcceptsTheLegacyPublicBool(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want Expose
	}{
		{"public true", "ports:\n  - {name: game, proto: udp, default: 2456, public: true}\n", ExposePublic},
		{"public false", "ports:\n  - {name: rcon, proto: tcp, default: 27015, public: false}\n", ExposeNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gs GameSpec
			if err := yaml.Unmarshal([]byte(tc.src), &gs); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := gs.Ports[0].Expose; got != tc.want {
				t.Errorf("expose = %q, want %q", got, tc.want)
			}
		})
	}

	var gs GameSpec
	both := "ports:\n  - {name: game, proto: udp, default: 2456, public: true, expose: none}\n"
	err := yaml.Unmarshal([]byte(both), &gs)
	if err == nil {
		t.Fatalf("a port setting both public and expose decoded as %+v", gs.Ports)
	}
	if !strings.Contains(err.Error(), "public") || !strings.Contains(err.Error(), "expose") {
		t.Errorf("error %q does not name both fields", err)
	}
}

func TestExposeValid(t *testing.T) {
	for _, e := range []Expose{ExposePublic, ExposeAdmin, ExposeNone} {
		if !e.Valid() {
			t.Errorf("Expose(%q).Valid() = false", e)
		}
	}
	for _, e := range []Expose{"", "true", "internal", "Public"} {
		if e.Valid() {
			t.Errorf("Expose(%q).Valid() = true", e)
		}
	}
}

// Exactly one predicate may be set. Predicates names every one that is, so the
// diagnostic can say which two collided instead of "invalid when".
func TestWhenPredicatesNamesEveryFieldSet(t *testing.T) {
	tests := []struct {
		name string
		when *When
		want []string
	}{
		{"nil", nil, nil},
		{"empty", &When{}, nil},
		{"one", &When{VarTrue: "public"}, []string{"var_true"}},
		{"var_equals counts", &When{VarEquals: &NameVal{Name: "mode", Value: "hard"}}, []string{"var_equals"}},
		{"not alone is not a predicate", &When{Not: &When{Phase: "install"}}, nil},
		{"two collide", &When{VarSet: "a", Phase: "install"}, []string{"var_set", "phase"}},
		{"all seven", &When{
			VarTrue:         "a",
			VarSet:          "b",
			VarEquals:       &NameVal{Name: "c", Value: "d"},
			SecretSet:       "e",
			PortSet:         "f",
			ContentNonEmpty: "g",
			Phase:           "install",
		}, []string{"var_true", "var_set", "var_equals", "secret_set", "port_set", "content_nonempty", "phase"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.when.Predicates(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Predicates() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExposeNarrows(t *testing.T) {
	tests := []struct {
		inst, manifest Expose
		want           bool
	}{
		{ExposeNone, ExposePublic, true},
		{ExposeAdmin, ExposePublic, true},
		{ExposePublic, ExposePublic, true},
		{ExposePublic, ExposeAdmin, false},
		{ExposeAdmin, ExposeNone, false}, // allowed by D-20, but as an explicit exception
		{ExposePublic, ExposeNone, false},
		{"", ExposePublic, false},
	}
	for _, tc := range tests {
		if got := tc.inst.Narrows(tc.manifest); got != tc.want {
			t.Errorf("Expose(%q).Narrows(%q) = %v, want %v", tc.inst, tc.manifest, got, tc.want)
		}
	}
}

// D-07: the manifest enum and the RCON client's constants are one list. If a
// dialect is added to internal/rcon and not here, a manifest cannot ask for it;
// if one is added here and not there, a manifest can ask for behaviour that does
// not exist.
// The dialect enum's D-07 checks live in dialect_sync_test.go, next to the
// assertions about the shipped manifests that depend on them.

func TestOriginRanking(t *testing.T) {
	if !(OriginOverride.Rank() > OriginPackage.Rank() && OriginPackage.Rank() > OriginEmbedded.Rank()) {
		t.Errorf("origin ranking is wrong: override=%d shipped=%d embedded=%d",
			OriginOverride.Rank(), OriginPackage.Rank(), OriginEmbedded.Rank())
	}
	if Origin("postgres").Valid() {
		t.Error("postgres is not a manifest origin (D-18)")
	}
}
