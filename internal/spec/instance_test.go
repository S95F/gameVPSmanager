package spec

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// sampleInstance is the adopted Project Zomboid server from section 6.1: the one
// migration this project is not allowed to get wrong, and therefore the shape the
// schema has to hold exactly.
const sampleInstance = `
apiVersion: gvm/v1
kind: Instance
metadata:
  name: pz-main
  uid: 019f2c7a-8b31-7c4e-9d20-3f61a5b7c9e2
  created: 2026-09-04T18:22:11Z
  adopted_from: "bash-suite:/root/pz-server"
spec:
  game: projectzomboid
  manifest:
    revision: 7
    sha256: 1b40c9de4f7a2c8e5d3b1a09f6e4c2b8d7a5930e1c4f6b8a2d0e3c5f7a9b1d3e
    origin: shipped
  enabled: true
  managed_install: false
  user: pzserver
  group: pzserver
  paths:
    install: /opt/pzserver
    data: /home/pzserver/Zomboid
    home: /home/pzserver
    work: /var/lib/gvm/cache/steam/homes/pz-main
  ports:
    game: 16261
    direct: 16262
    rcon: 27015
  vars:
    server_name: servertest
    jvm_xmx: 5g
    max_players: "8"
  resources:
    cpu_weight: 100
    io_weight: 100
    memory_high: 7G
    memory_max: 8G
    memory_swap_max: "0"
    tasks_max: 4096
    nofile: 100000
  console:
    compat: screen
    sunset: 2026-12-03T00:00:00Z
  firewall:
    expose:
      rcon: none
    admin_cidrs: []
  egress:
    policy: open
    allow: []
  content:
    workshop:
      source: steam_workshop
      collection: "3795231359"
      collection_sync: manual
      activation_policy: preserve
      acquire:
        - {id: "3171167894", note: "that DAMN Library"}
        - {id: "3508537032", note: "NeatUI Framework [B42]"}
        - {id: "3536052310", note: "Neat Building [B42]", requires: ["3508537032"]}
  jobs:
    nightly:
      schedule: "*-*-* 05:00:00 America/Denver"
      action: update
      randomized_delay: 120s
      persistent: false
      on_failure: start_anyway
    backup:
      schedule: "*-*-* 04:00:00 America/Denver"
      action: backup
      randomized_delay: 300s
      persistent: true
      on_failure: fail
  backup:
    keep: 14
    keep_days: 45
    dir: /var/backups/gvm/pz-main
    remote: {kind: none}
`

func decodeInstance(t *testing.T, src string) Instance {
	t.Helper()
	var i Instance
	if err := yaml.Unmarshal([]byte(src), &i); err != nil {
		t.Fatalf("unmarshal instance: %v", err)
	}
	return i
}

func TestInstanceDecodesTheAdoptedShape(t *testing.T) {
	i := decodeInstance(t, sampleInstance)

	if i.APIVersion != "gvm/v1" || i.Kind != "Instance" {
		t.Errorf("apiVersion/kind = %q/%q", i.APIVersion, i.Kind)
	}
	if i.Metadata.Name != "pz-main" {
		t.Errorf("name = %q", i.Metadata.Name)
	}
	if i.Metadata.UID != "019f2c7a-8b31-7c4e-9d20-3f61a5b7c9e2" {
		t.Errorf("uid = %q", i.Metadata.UID)
	}
	if !i.Metadata.Created.Equal(time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC)) {
		t.Errorf("created = %v", i.Metadata.Created)
	}
	if i.Metadata.AdoptedFrom != "bash-suite:/root/pz-server" {
		t.Errorf("adopted_from = %q", i.Metadata.AdoptedFrom)
	}

	if i.Spec.ManagedInstall {
		t.Error("managed_install decoded true; an adopted install must refuse app_update")
	}
	if i.Spec.User != "pzserver" {
		t.Errorf("user = %q; an adopted instance keeps its pre-existing account", i.Spec.User)
	}
	if i.Spec.Manifest.Revision != 7 || i.Spec.Manifest.Origin != OriginPackage {
		t.Errorf("manifest pin = %+v", i.Spec.Manifest)
	}
	if len(i.Spec.Manifest.SHA256) != 64 {
		t.Errorf("pin sha256 = %q", i.Spec.Manifest.SHA256)
	}

	if i.Spec.Paths.Data != "/home/pzserver/Zomboid" || i.Spec.Paths.Home != "/home/pzserver" {
		t.Errorf("paths = %+v; adoption preserves them verbatim", i.Spec.Paths)
	}
	if i.Spec.Paths.Work != "/var/lib/gvm/cache/steam/homes/pz-main" {
		t.Errorf("work (SteamCMD's HOME) = %q", i.Spec.Paths.Work)
	}

	wantPorts := map[string]int{"game": 16261, "direct": 16262, "rcon": 27015}
	if !reflect.DeepEqual(i.Spec.Ports, wantPorts) {
		t.Errorf("ports = %v, want %v", i.Spec.Ports, wantPorts)
	}

	if i.Spec.Resources.MemoryMax != "8G" || i.Spec.Resources.MemoryHigh != "7G" {
		t.Errorf("resources = %+v; these render verbatim into the drop-in", i.Spec.Resources)
	}
	if i.Spec.Resources.MemorySwapMax != "0" {
		t.Errorf("memory_swap_max = %q, want the string \"0\"", i.Spec.Resources.MemorySwapMax)
	}

	if i.Spec.Console.Compat != "screen" {
		t.Errorf("console.compat = %q", i.Spec.Console.Compat)
	}
	if got, want := i.Spec.Console.Sunset, time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("console.sunset = %v, want %v", got, want)
	}
	if i.Spec.Firewall.Expose["rcon"] != ExposeNone {
		t.Errorf("firewall.expose = %v", i.Spec.Firewall.Expose)
	}
	if i.Spec.Egress.Policy != "open" {
		t.Errorf("egress.policy = %q", i.Spec.Egress.Policy)
	}
}

// The acquire list IS mods.txt, and its order is load order: a library that
// loads after its dependent breaks the world silently.
func TestInstanceContentKeepsOrderAndDeclaredRequires(t *testing.T) {
	i := decodeInstance(t, sampleInstance)

	c, ok := i.Spec.Content["workshop"]
	if !ok {
		t.Fatalf("content = %v, want a workshop block", i.Spec.Content)
	}
	if c.Source != "steam_workshop" || c.Collection != "3795231359" {
		t.Errorf("content block = %+v", c)
	}
	if c.CollectionSync != "manual" || c.ActivationPolicy != "preserve" {
		t.Errorf("sync/policy = %q/%q; adoption never rewrites Mods=",
			c.CollectionSync, c.ActivationPolicy)
	}

	wantIDs := []string{"3171167894", "3508537032", "3536052310"}
	var gotIDs []string
	for _, r := range c.Acquire {
		gotIDs = append(gotIDs, r.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("acquire order = %v, want %v", gotIDs, wantIDs)
	}
	if got := c.Acquire[2].Requires; !reflect.DeepEqual(got, []string{"3508537032"}) {
		t.Errorf("requires = %v", got)
	}
	if c.Acquire[0].Note == "" {
		t.Error("note dropped; mods.txt's comments are documentation and must persist as data")
	}
}

// A missed backup should catch up at boot; a missed restart must NOT fire the
// moment the box comes back. That distinction is one bool, so it has to survive.
func TestInstanceJobsKeepPersistencePerJob(t *testing.T) {
	i := decodeInstance(t, sampleInstance)

	nightly := i.Spec.Jobs["nightly"]
	if nightly.Action != "update" || nightly.Persistent {
		t.Errorf("nightly = %+v, want action update and persistent false", nightly)
	}
	if got := nightly.RandomizedDelay.Duration(); got != 2*time.Minute {
		t.Errorf("nightly.randomized_delay = %v", got)
	}
	if nightly.OnFailure != "start_anyway" {
		t.Errorf("nightly.on_failure = %q", nightly.OnFailure)
	}

	backup := i.Spec.Jobs["backup"]
	if !backup.Persistent || backup.OnFailure != "fail" {
		t.Errorf("backup = %+v", backup)
	}
	if i.Spec.Backup.Keep != 14 || i.Spec.Backup.KeepDays != 45 {
		t.Errorf("backup policy = %+v", i.Spec.Backup)
	}
	if i.Spec.Backup.Remote.Kind != "none" {
		t.Errorf("backup.remote = %+v", i.Spec.Backup.Remote)
	}
}

func TestInstanceYAMLTagsRoundTrip(t *testing.T) {
	first := decodeInstance(t, sampleInstance)
	assertYAMLFixpoint(t, first, func(src string) any { return decodeInstance(t, src) })
}

// assertYAMLFixpoint re-emits a decoded document, decodes it again, and requires
// that nothing moves after the first pass: the bytes are stable and so are the
// values. It does not compare against the ORIGINAL decode, because an explicit
// empty list normalises to an absent key on the way out — see
// TestExplicitEmptyListNormalisesToAbsent, which pins that behaviour rather than
// leaving it to be discovered.
func assertYAMLFixpoint(t *testing.T, v any, decode func(string) any) {
	t.Helper()

	out, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second := decode(string(out))

	again, err := yaml.Marshal(second)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(out) != string(again) {
		t.Fatalf("re-emission is not stable:\n--- first ---\n%s\n--- second ---\n%s", out, again)
	}
	if third := decode(string(again)); !reflect.DeepEqual(second, third) {
		t.Errorf("values did not survive a marshal/unmarshal cycle; emitted as:\n%s", again)
	}
}

// An operator who writes `admin_cidrs: []` and an operator who omits the key
// mean the same thing, and gvm must not treat them differently or a no-op edit
// shows up as a change in `gvm apply --diff`.
func TestExplicitEmptyListNormalisesToAbsent(t *testing.T) {
	var withEmpty, without Instance
	if err := yaml.Unmarshal([]byte("spec:\n  firewall:\n    admin_cidrs: []\n"), &withEmpty); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := yaml.Unmarshal([]byte("spec:\n  firewall: {}\n"), &without); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(withEmpty.Spec.Firewall.AdminCIDRs) != 0 {
		t.Errorf("admin_cidrs: [] decoded as %v", withEmpty.Spec.Firewall.AdminCIDRs)
	}

	a, err := yaml.Marshal(withEmpty)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, err := yaml.Marshal(without)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("an explicit empty list and an absent key re-emit differently:\n%s\n---\n%s", a, b)
	}
}

func TestParseUID(t *testing.T) {
	const canonical = "019f2c7a-8b31-7c4e-9d20-3f61a5b7c9e2"

	got, err := ParseUID(strings.ToUpper(canonical))
	if err != nil {
		t.Fatalf("ParseUID(uppercase): %v", err)
	}
	if string(got) != canonical {
		t.Errorf("ParseUID did not normalise case: %q", got)
	}

	bad := []struct{ name, in string }{
		{"empty", ""},
		{"truncated", "019f2c7a-8b31-7c4e-9d20-3f61a5b7c9e"},
		{"no dashes", "019f2c7a8b317c4e9d203f61a5b7c9e2"},
		{"dashes in the wrong places", "019f2c7a8-b31-7c4e-9d20-3f61a5b7c9e2"},
		{"not hex", "019f2c7a-8b31-7c4e-9d20-3f61a5b7c9zz"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if u, err := ParseUID(tc.in); err == nil {
				t.Fatalf("ParseUID(%q) = %q, want an error", tc.in, u)
			}
		})
	}
}

// A typo'd uid that still decoded would be a NEW instance identity, and the
// symptom is every instance_grants row silently disappearing on the next mirror
// sync. An absent uid, though, is just a spec that gvm create has not stamped.
func TestInstanceUIDIsCheckedAtDecode(t *testing.T) {
	var i Instance
	err := yaml.Unmarshal([]byte("metadata:\n  name: pz-main\n  uid: not-a-uuid\n"), &i)
	if err == nil {
		t.Fatalf("uid %q decoded", i.Metadata.UID)
	}
	if !strings.Contains(err.Error(), "uid") {
		t.Errorf("error %q does not name the field", err)
	}

	for _, src := range []string{
		"metadata:\n  name: pz-main\n",
		"metadata:\n  name: pz-main\n  uid: \"\"\n",
		"metadata:\n  name: pz-main\n  uid:\n",
	} {
		var j Instance
		if err := yaml.Unmarshal([]byte(src), &j); err != nil {
			t.Errorf("unmarshal(%q): %v", src, err)
		}
		if j.Metadata.UID != "" {
			t.Errorf("unmarshal(%q) invented a uid %q", src, j.Metadata.UID)
		}
	}
}

func TestDefaultHostConfigIsSafeBeforeAnyoneEditsIt(t *testing.T) {
	h := DefaultHostConfig()

	if !h.Spec.Edge.HTTPRedirect || !h.Spec.Edge.HTTP3 {
		t.Errorf("edge = %+v; the zero struct would silently disable both", h.Spec.Edge)
	}
	if h.Spec.Edge.HSTS {
		t.Error("hsts defaults on; it is a one-way door per hostname")
	}
	if len(h.Spec.Edge.TrustedProxies) != 0 {
		t.Errorf("trusted_proxies = %v, want empty: Caddy IS the edge (D-39)",
			h.Spec.Edge.TrustedProxies)
	}
	if !strings.HasPrefix(h.Spec.Edge.Caddy.Admin, "unix/") {
		t.Errorf("caddy admin = %q, want a unix socket (D-36)", h.Spec.Edge.Caddy.Admin)
	}
	if h.Spec.Edge.Caddy.ConfigPath != "/etc/gvm/caddy/Caddyfile" {
		t.Errorf("caddy config_path = %q; /etc/caddy/Caddyfile is a dpkg conffile (D-37)",
			h.Spec.Edge.Caddy.ConfigPath)
	}
	if h.Spec.Edge.Socket.Path != "/run/gvm/web/dashboard.sock" {
		t.Errorf("dashboard socket = %q (D-06)", h.Spec.Edge.Socket.Path)
	}
	if !h.Spec.Firewall.SSH.RateLimit {
		t.Error("ssh rate_limit defaults off; firewall.sh used `ufw limit`")
	}
	if len(h.Spec.Firewall.SSH.Ports) != 0 {
		t.Errorf("ssh.ports = %v, want empty (autodetect)", h.Spec.Firewall.SSH.Ports)
	}
	if h.Spec.Firewall.Egress.Policy != "open" {
		t.Errorf("egress policy = %q", h.Spec.Firewall.Egress.Policy)
	}
	if h.Spec.Steam.SharedCache {
		t.Error("steam.shared_cache defaults on; per-instance HOMEs are the protection (D-30)")
	}
}

func TestHostConfigYAMLRoundTrip(t *testing.T) {
	const src = `
apiVersion: gvm/v1
kind: Host
spec:
  edge:
    mode: caddy_managed
    site: gvm.example.com
    http_redirect: true
    http3: true
    hsts: false
    socket:
      path: /run/gvm/web/dashboard.sock
      group: caddy
    listen: ""
    trusted_proxies: []
    trusted_origins: []
    tls:
      mode: auto
      email: ops@example.com
      acme_ca: ""
      dns_provider: ""
      dns_env: {CF_API_TOKEN: CF_API_TOKEN}
    caddy:
      manage_unit: true
      config_path: /etc/gvm/caddy/Caddyfile
      admin: unix//run/caddy/admin.sock
      min_version: "2.11.1"
      log_path: /var/log/caddy/gvm.log
  firewall:
    backend: ufw
    manage: true
    logging: low
    ssh:
      ports: [22]
      rate_limit: true
      fail2ban: true
    admin_cidrs: ["203.0.113.7/32"]
    reserved_ports: [9100]
    egress:
      policy: open
      allow: []
  steam:
    shared_cache: false
`
	var first HostConfig
	if err := yaml.Unmarshal([]byte(src), &first); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if first.Spec.Edge.TLS.DNSEnv["CF_API_TOKEN"] != "CF_API_TOKEN" {
		t.Errorf("tls.dns_env = %v", first.Spec.Edge.TLS.DNSEnv)
	}
	if !reflect.DeepEqual(first.Spec.Firewall.SSH.Ports, []int{22}) {
		t.Errorf("ssh.ports = %v", first.Spec.Firewall.SSH.Ports)
	}
	if !reflect.DeepEqual(first.Spec.Firewall.ReservedPorts, []int{9100}) {
		t.Errorf("reserved_ports = %v", first.Spec.Firewall.ReservedPorts)
	}
	if first.Spec.Edge.Caddy.MinVersion != "2.11.1" {
		t.Errorf("min_version = %q", first.Spec.Edge.Caddy.MinVersion)
	}

	assertYAMLFixpoint(t, first, func(src string) any {
		var h HostConfig
		if err := yaml.Unmarshal([]byte(src), &h); err != nil {
			t.Fatalf("unmarshal host config: %v", err)
		}
		return h
	})
}
