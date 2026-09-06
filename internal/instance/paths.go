package instance

import (
	"path"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// EtcRoot is INTENT: operator-editable, git-able, secrets excepted.
//
// It is an alias for spec.EtcDir, not a second spelling of the same string.
// spec's loader got there first, and a directory spelled two ways is how a
// reconciler ends up cleaning three of the four files it wrote. The rest of
// /etc/gvm is spec's too: spec.EtcGamesDir and spec.EtcInstancesDir.
const EtcRoot = spec.EtcDir

// The remaining control-plane roots, from section 5. They are constants rather
// than configuration on purpose: every one of them is referenced by a packaged
// tmpfiles.d line, a systemd directive or a dpkg-owned directory, so a
// relocatable root would be a lie the packaging cannot honour.
//
// They are slash-separated Linux paths, built here and below with path.Join
// and never path/filepath.Join, so they render identically on the Windows
// development machine and can be asserted against verbatim in a test there.
const (
	// StateRoot is DERIVED: rebuildable, genuinely safe to lose. That claim
	// is only true because D-02 moved every secret out of it.
	StateRoot = "/var/lib/gvm"
	// RunRoot is owned by tmpfiles.d, not by any unit's RuntimeDirectory=,
	// so `systemctl stop gvmd` does not delete every instance's console
	// socket (D-05).
	RunRoot    = "/run/gvm"
	LogRoot    = "/var/log/gvm"
	BackupRoot = "/var/backups/gvm"
	SrvRoot    = "/srv/gvm"

	// SecretsRoot is 0700 root:root inside a 0750 root:gvm parent, so the
	// unprivileged web tier can read specs and cannot traverse to a
	// credential (D-02).
	SecretsRoot = EtcRoot + "/secrets"
	// HostSecretsDir holds the box-wide credentials: curseforge, modrinth,
	// sftp_ed25519, age_recipient, steam.
	HostSecretsDir = SecretsRoot + "/host"

	// SystemdUnitDir is where gvm writes generated units and drop-ins. The
	// shipped templates live in /usr/lib/systemd/system and gvm never writes
	// there: that is the package's directory, and an upgrade owns it.
	SystemdUnitDir = "/etc/systemd/system"

	// EtcJobsDir holds one descriptor per job slug (D-13, section 5). It is
	// what turns the "<instance>_<job>" systemd passes to `gvm job run` back
	// into an instance, a job and an action without a database.
	EtcJobsDir = EtcRoot + "/jobs"

	// SteamHomesDir gives each instance its own SteamCMD HOME, which removes
	// the shared ~/.steam corruption class outright rather than only
	// serialising around it (D-30, section 5.2).
	SteamHomesDir = StateRoot + "/cache/steam/homes"
	// SteamCMDLock is the host-wide flock, kept as belt-and-braces (D-30).
	SteamCMDLock = StateRoot + "/locks/steamcmd.lock"

	// SpoolDir holds the audit and job records Postgres has not accepted yet
	// (section 14.6). It is host-wide rather than per-instance because the
	// records in it are not all about instances -- a user creation and a
	// firewall apply land here too.
	//
	// It is under StateRoot, and that is the correct place for it precisely
	// because StateRoot is disposable: journald is the durable off-database
	// copy, and the spool exists only so Postgres can catch up.
	SpoolDir = StateRoot + "/spool"
)

// Paths is every control-plane path derived from one instance's name.
//
// It exists so that section 5 is transcribed exactly once. The alternative --
// path.Join(StateRoot, "instances", name, "ports.json") at each call site --
// is how a directory ends up spelled two ways and how a reconciler cleans up
// three of the four files it wrote.
//
// None of these are the game's own paths. Where the world and the binaries
// live is spec.Paths, which an adopted instance keeps verbatim
// (/opt/pzserver, /home/pzserver/Zomboid) forever.
type Paths struct {
	Name string

	// Intent, under /etc/gvm. 0640 root:gvm, so the web tier reads and never
	// writes.
	Spec         string // <name>.yaml            -- the instance definition
	ContentLock  string // <name>.lock.json       -- pinned content intent (D-03)
	SpecDir      string // <name>.d/
	ManifestLock string // <name>.d/manifest.lock.yaml -- pinned manifest bytes (D-04)
	SecretsDir   string // secrets/instances/<name>/   -- 0700 root:root (D-02)

	// Derived, under /var/lib/gvm. 0750 root:root: a compromised mod must not
	// be able to rewrite the content lock or the observed state (section 5.3).
	State           string // instances/<name>/
	Observed        string // last-read game config, buildid, StateFlags
	ContentObserved string // last Observe() of the content namespaces
	PortsJSON       string // echo of the materialized allocation
	BlockedMods     string // present iff AWAITING_MANUAL_FILES
	ControlProbe    string // `gvm control probe` finding
	// Lease is the job that currently owns the instance (section 5.3). It is
	// here rather than under /run deliberately: /run is tmpfs, and a lease
	// that vanished on reboot could not distinguish "the job finished" from
	// "the box died mid-update". The recorded boot id is what makes the
	// reboot case decidable instead.
	Lease          string
	SnapDir        string // reflink/btrfs snapshot staging
	SteamScriptDir string // generated steamcmd runscripts
	SteamHome      string // SteamCMD's HOME for this instance (D-30)

	// Runtime, under /run/gvm/<name>. The directory is 2750
	// gvm-<name>:gvm-console (setgid) so gvm-web can traverse it and instance
	// A cannot stat instance B's (D-09, D-10).
	Run          string
	ConsoleSock  string
	StatusJSON   string
	RunnerJSON   string // pid/pgid/starttime pin
	ShutdownJSON string // the rendered ladder, for run-shutdown
	LastShutdown string
	FreezeLease  string // present iff a freeze is armed (D-31)
	JobLock      string // per-instance flock (D-30)
	Hold         string // present iff an operator stopped it

	// Logs and archives. Backups is 0700 root:root, which is the one thing in
	// install.sh deliberately not ported: a compromised mod must not be able
	// to read every world the server has ever had (D-44).
	Logs        string
	ConsoleLog  string
	ShutdownLog string // append-only jsonl, gvmd ingests
	JobLogDir   string
	Backups     string

	// Srv is the default home for a NEW instance. An adopted one has no
	// business here, and nothing in gvm creates it for one.
	Srv string
}

// PathsFor derives the control-plane path set for an instance.
func PathsFor(name string) Paths {
	MustValidName(name)
	etc := path.Join(spec.EtcInstancesDir, name)
	state := path.Join(StateRoot, "instances", name)
	run := path.Join(RunRoot, name)
	logs := path.Join(LogRoot, name)

	return Paths{
		Name: name,

		Spec:         etc + ".yaml",
		ContentLock:  etc + ".lock.json",
		SpecDir:      etc + ".d",
		ManifestLock: path.Join(etc+".d", "manifest.lock.yaml"),
		SecretsDir:   path.Join(SecretsRoot, "instances", name),

		State:           state,
		Observed:        path.Join(state, "observed.json"),
		ContentObserved: path.Join(state, "content.observed.json"),
		PortsJSON:       path.Join(state, "ports.json"),
		BlockedMods:     path.Join(state, "blocked-mods.json"),
		ControlProbe:    path.Join(state, "control-probe.json"),
		Lease:           path.Join(state, "lease.json"),
		SnapDir:         path.Join(state, "snap"),
		SteamScriptDir:  path.Join(state, "steam"),
		SteamHome:       path.Join(SteamHomesDir, name),

		Run:          run,
		ConsoleSock:  path.Join(run, "console.sock"),
		StatusJSON:   path.Join(run, "status.json"),
		RunnerJSON:   path.Join(run, "runner.json"),
		ShutdownJSON: path.Join(run, "shutdown.json"),
		LastShutdown: path.Join(run, "last-shutdown.json"),
		FreezeLease:  path.Join(run, "freeze.lease"),
		JobLock:      path.Join(run, "job.lock"),
		Hold:         path.Join(run, "hold"),

		Logs:        logs,
		ConsoleLog:  path.Join(logs, "console.log"),
		ShutdownLog: path.Join(logs, "shutdown.jsonl"),
		JobLogDir:   path.Join(logs, "jobs"),
		Backups:     path.Join(BackupRoot, name),

		Srv: path.Join(SrvRoot, name),
	}
}

// Secret is the file backing one of this instance's credentials -- the path a
// LoadCredential= line names (D-02). Key is "rcon", "admin" or "join".
func (p Paths) Secret(key string) string { return path.Join(p.SecretsDir, key) }

// JobLog is where one job run's output lands.
func (p Paths) JobLog(jobID string) string { return path.Join(p.JobLogDir, jobID+".log") }

// DefaultSpecPaths is where a NEW instance's game data goes: the defaults
// `gvm create` writes into the instance YAML.
//
// Home and Work are deliberately different directories (section 5.2). Home is
// the GAME's, because servers dlopen("$HOME/.steam/sdk64/steamclient.so") and
// that shim has to be somewhere stable; Work is SteamCMD's, per instance,
// because two concurrent steamcmd under one HOME corrupt ~/.steam and race on
// appmanifests.
//
// An adopted instance never calls this. Its paths are discovered from the
// running server and written down verbatim; guessing them is exactly how an
// adoption generates a fresh empty world beside the real one.
func DefaultSpecPaths(name string) spec.Paths {
	p := PathsFor(name)
	return spec.Paths{
		Install: path.Join(p.Srv, "install"),
		Data:    path.Join(p.Srv, "data"),
		Home:    p.Srv,
		Work:    p.SteamHome,
		Logs:    p.Logs,
		Backups: p.Backups,
	}
}
