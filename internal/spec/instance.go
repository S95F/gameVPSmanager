package spec

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrUID is the sentinel a malformed metadata.uid wraps.
var ErrUID = errors.New("invalid uid")

// UID is an instance's permanent identity: a UUIDv7 generated once by gvm and
// written into the instance YAML (D-25).
//
// It is generated in Go rather than by the database's DEFAULT, because rebuilding
// the Postgres mirror after a restore would otherwise reassign UUIDs and
// silently cascade away every instance_grants row. The YAML owns the identity;
// the mirror upserts by it and never deletes.
//
// It is a string rather than a parsed 16-byte value so this package stays
// dependency-free, but a non-empty UID is checked for canonical form at decode:
// a typo'd uid that still decodes would be a new instance identity, and that
// failure is invisible until grants disappear.
type UID string

// ParseUID accepts the canonical 8-4-4-4-12 hexadecimal form, case-insensitively,
// and normalises to lowercase. It deliberately does not require version 7: a
// file written by an older tool or by hand must still load and be reportable,
// and "this uid is not a v7" is a doctor finding, not a reason to refuse to read
// an operator's server definition.
func ParseUID(s string) (UID, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if len(t) != 36 {
		return "", fmt.Errorf("%w: %q: want 36 characters in 8-4-4-4-12 form", ErrUID, s)
	}
	for i, c := range t {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "", fmt.Errorf("%w: %q: expected '-' at position %d", ErrUID, s, i)
			}
		default:
			if !isHexDigit(c) {
				return "", fmt.Errorf("%w: %q: %q is not a hex digit", ErrUID, s, c)
			}
		}
	}
	return UID(t), nil
}

func isHexDigit(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// UnmarshalYAML accepts an empty or absent uid as the zero value, which is what
// a freshly hand-written spec looks like before `gvm create` stamps it, and
// refuses anything non-empty that is not a UUID.
func (u *UID) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag == "!!null" || n.Value == "" {
		*u = ""
		return nil
	}
	v, err := ParseUID(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*u = v
	return nil
}

// Instance is one server. This file, and only this file, is what a server IS.
// It holds no secrets, so it is safe to git or to paste into a support thread.
type Instance struct {
	APIVersion string       `yaml:"apiVersion"` // "gvm/v1"
	Kind       string       `yaml:"kind"`       // "Instance"
	Metadata   InstanceMeta `yaml:"metadata"`
	Spec       InstanceSpec `yaml:"spec"`
}

type InstanceMeta struct {
	Name        string            `yaml:"name"`
	UID         UID               `yaml:"uid"` // UUIDv7, generated once (D-25)
	Created     time.Time         `yaml:"created"`
	AdoptedFrom string            `yaml:"adopted_from,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
}

type InstanceSpec struct {
	Game     string      `yaml:"game"`
	Manifest ManifestPin `yaml:"manifest"`
	Enabled  bool        `yaml:"enabled"`

	// ManagedInstall=false is the adoption flag: the provider refuses
	// app_update and validate at the type level, gvm creates no user and chowns
	// nothing, and `gvm apply` reports the user as pre-existing.
	ManagedInstall bool `yaml:"managed_install"`

	User  string `yaml:"user"`
	Group string `yaml:"group,omitempty"` // defaults to User
	Paths Paths  `yaml:"paths"`

	// Ports are MATERIALISED, never recomputed at runtime: stable, greppable,
	// and immune to an allocator change moving a live server.
	Ports map[string]int `yaml:"ports"`

	Vars      map[string]string      `yaml:"vars"`
	Resources Resources              `yaml:"resources"`
	Console   ConsoleCompat          `yaml:"console,omitempty"`
	Firewall  InstanceFirewall       `yaml:"firewall,omitempty"`
	Egress    EgressPolicy           `yaml:"egress,omitempty"`
	Content   map[string]ContentList `yaml:"content,omitempty"`
	Jobs      map[string]Job         `yaml:"jobs,omitempty"`
	Backup    BackupPolicy           `yaml:"backup"`
}

// ManifestPin is what makes `apt upgrade gvm` safe (D-17). The runner compares
// it against the manifest it actually loaded and exits 64 on a mismatch rather
// than silently running new argv against a live world.
type ManifestPin struct {
	Revision int    `yaml:"revision"`
	SHA256   string `yaml:"sha256"`
	Origin   Origin `yaml:"origin"` // embedded | shipped | override
}

type Paths struct {
	Install string `yaml:"install"`
	Data    string `yaml:"data"`
	Home    string `yaml:"home"`           // the GAME's HOME
	Work    string `yaml:"work,omitempty"` // SteamCMD's HOME, per instance (D-30)
	Logs    string `yaml:"logs,omitempty"`
	Backups string `yaml:"backups,omitempty"`
}

// Resources render verbatim into the systemd drop-in. Strings, not int64, so the
// diff an operator reads is the string they wrote ("8G", not 8589934592).
type Resources struct {
	CPUWeight     int    `yaml:"cpu_weight"`
	IOWeight      int    `yaml:"io_weight"`
	MemoryHigh    string `yaml:"memory_high"`
	MemoryMax     string `yaml:"memory_max"`
	MemorySwapMax string `yaml:"memory_swap_max"`
	TasksMax      int    `yaml:"tasks_max"`
	NoFile        int    `yaml:"nofile"`
}

// ConsoleCompat keeps `screen -r <name>` working through an adoption week by
// running `gvm console` inside a screen session — a view, never the control
// path. Sunset is a declared date, not an intention (D-11).
type ConsoleCompat struct {
	Compat string    `yaml:"compat,omitempty"` // screen | ""
	Sunset time.Time `yaml:"sunset,omitempty"`
}

// InstanceFirewall may only NARROW the manifest's exposure, with one exception:
// none -> admin is allowed, because that is exactly what ADMIN_IP was for.
// none -> public is a load-time validation error (D-20).
type InstanceFirewall struct {
	Expose     map[string]Expose `yaml:"expose,omitempty"`
	AdminCIDRs []string          `yaml:"admin_cidrs,omitempty"`
}

// EgressPolicy is the one outbound control that genuinely works: allowlist
// renders systemd IPAddressDeny=any plus IPAddressAllow=, a per-cgroup BPF
// filter. It takes CIDRs, NOT hostnames, so it is realistic only for a fully
// provisioned instance that never phones home (section 18.4).
type EgressPolicy struct {
	Policy string   `yaml:"policy"` // open | allowlist
	Allow  []string `yaml:"allow,omitempty"`
}

// ContentList is the instance's half of the two-namespace content model: the
// acquisition ids an operator wrote, in order. For Project Zomboid this IS
// mods.txt, as data, and the order is load order.
type ContentList struct {
	Source         string `yaml:"source"`
	Collection     string `yaml:"collection,omitempty"`
	CollectionSync string `yaml:"collection_sync,omitempty"` // manual | on_update | off

	// ActivationPolicy `preserve` is the adoption switch: the live ini's Mods=
	// list is returned untouched and never re-derived, which is how "adoption
	// never rewrites Mods=" becomes a property of the data rather than a promise
	// in a doc. `derive` hands the list back to the resolver.
	ActivationPolicy string `yaml:"activation_policy,omitempty"` // preserve | derive

	Acquire []ContentRef `yaml:"acquire"`
}

// ContentRef is the ACQUISITION namespace: the only thing a human writes.
type ContentRef struct {
	ID   string `yaml:"id"`
	Pin  string `yaml:"pin,omitempty"`
	Note string `yaml:"note,omitempty"` // mods.txt's trailing comments, as data

	// Requires is operator-declared and ENFORCED. Steam Workshop publishes no
	// machine-readable dependency data — the "Required items" block is HTML —
	// so gvm does not guess; it refuses an ordering where a dependent precedes
	// its dependency, naming both.
	Requires []string `yaml:"requires,omitempty"`

	Vendored bool `yaml:"vendored,omitempty"` // operator bytes; never fetched, never pruned
}

// Job is one scheduled action, rendered into a systemd timer. The trigger is
// systemd and the ledger is Postgres, never the reverse: the nightly backup must
// fire when gvmd, gvm-web and the database are all dead (D-29).
type Job struct {
	Schedule string `yaml:"schedule"` // systemd OnCalendar, timezone included
	Action   string `yaml:"action"`   // update | restart | backup | content_sync | rebuild

	RandomizedDelay Duration `yaml:"randomized_delay,omitempty"`

	// Persistent=true catches a missed run up at boot. Correct for a backup;
	// wrong for a restart, which must not fire the moment a box comes back.
	Persistent bool `yaml:"persistent"`

	OnFailure string `yaml:"on_failure,omitempty"` // fail | start_anyway
}

type BackupPolicy struct {
	Keep     int          `yaml:"keep"`
	KeepDays int          `yaml:"keep_days"`
	Dir      string       `yaml:"dir"`
	Remote   RemoteTarget `yaml:"remote"`
}

// RemoteTarget is the off-box copy. EncryptTo is a PUBLIC age recipient, so the
// VPS can produce backups it cannot itself decrypt. HostKey is pinned: there is
// no trust-on-first-use and no known_hosts.
type RemoteTarget struct {
	Kind      string `yaml:"kind"` // none | sftp
	Host      string `yaml:"host,omitempty"`
	User      string `yaml:"user,omitempty"`
	Path      string `yaml:"path,omitempty"`
	KeyFile   string `yaml:"key_file,omitempty"`
	HostKey   string `yaml:"host_key,omitempty"`
	EncryptTo string `yaml:"encrypt_to,omitempty"`

	KeepRemote int `yaml:"keep_remote,omitempty"`

	// OnFailure defaults to warn: an off-box push failing must never fail the
	// local backup, which is the actual protection.
	OnFailure string `yaml:"on_failure,omitempty"` // warn | fail
}
