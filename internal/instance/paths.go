package instance

import (
	"fmt"
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
	// LocksDir holds the flocks that serialise jobs (D-30). It is under
	// StateRoot rather than RunRoot because a HOST-level job's lock has no
	// instance runtime directory to live in, and because the directory has to
	// exist before the first job runs rather than be created by whichever job
	// got there first.
	LocksDir = StateRoot + "/locks"
	// SteamCMDLock is the host-wide flock, kept as belt-and-braces (D-30).
	SteamCMDLock = LocksDir + "/steamcmd.lock"

	// BlobsDir is the content-addressed store: every third-party byte gvm
	// downloads itself lands here under its own digest before anything links
	// it into an instance tree. 0755 gvm-fetch:gvm-content, so the uid that
	// parses hostile archives owns the cache and nothing else (D-16).
	BlobsDir = StateRoot + "/blobs"
	// BlobsTmpDir is where a download stages while it is being hashed. It is
	// inside BlobsDir rather than beside it so the rename that publishes a
	// verified blob is a same-filesystem rename, which is the only kind that
	// is atomic.
	BlobsTmpDir = BlobsDir + "/tmp"

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
	// RunnerLock is section 11.7's lock 1: the runner flocks it in preflight
	// and holds it for the whole life of the unit, which is what makes "one
	// supervisor per world" true and what makes a second `gvm run` exit 75.
	//
	// It is NOT the job engine's lock -- that is InstanceJobLockPath, under
	// LocksDir -- and the name says so because for a while the two were the
	// same file and nothing in the codebase said which role it was playing.
	// Its placement inside the RuntimeDirectory is correct for this role and
	// only this one: the holder's lifetime IS the unit's, so systemd's
	// removal of the directory coincides with the release. The on-disk name
	// stays job.lock, which is what section 11.7 and preflight's STATUS line
	// both quote.
	RunnerLock string
	Hold       string // present iff an operator stopped it
	// Degraded is the breadcrumb a job leaves when it could not put the
	// server back (section 13.4). It is the companion of Hold and the
	// opposite of it: a hold says a human wants this instance down and the
	// guard timer must leave it alone, while this says gvm wanted it up and
	// failed, which is exactly what the guard timer is for.
	Degraded string

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
		RunnerLock:   path.Join(run, "job.lock"),
		Hold:         path.Join(run, "hold"),
		Degraded:     path.Join(run, "degraded"),

		Logs:        logs,
		ConsoleLog:  path.Join(logs, "console.log"),
		ShutdownLog: path.Join(logs, "shutdown.jsonl"),
		JobLogDir:   path.Join(logs, "jobs"),
		Backups:     path.Join(BackupRoot, name),

		Srv: path.Join(SrvRoot, name),
	}
}

// HostJobLockPath is the flock a HOST-level job takes (D-13: `_guard`,
// `_verify`).
//
// It is derived from the job name rather than from an instance because there
// is no instance: the slug's instance half is empty. The file is named with
// the slug's own spelling so that `fuser /var/lib/gvm/locks/job__verify.lock`
// and `systemctl status gvm-job@_verify` name the same thing.
func HostJobLockPath(job string) string {
	if err := ValidateJobName(job); err != nil {
		panic(fmt.Sprintf("instance: %v; call ValidateJobName before deriving a lock path", err))
	}
	return path.Join(LocksDir, "job_"+JobSlug("", job)+".lock")
}

// InstanceJobLockPath is the flock an INSTANCE-scoped job takes, so that one
// instance runs one job at a time (D-30).
//
// It is keyed on the instance and not on the slug, because that is the
// invariant: job_pz-main.lock has to serialise pz-main_nightly against
// pz-main_update. A per-slug file would let a backup and an update into the
// same install tree together.
//
// It is NOT Paths.RunnerLock, and the two must never be the same file again.
// RunnerLock is §11.7 lock 1 -- the runner takes it in preflight and holds it
// for the whole life of the unit, which is what "one supervisor per world"
// means -- so a job that took it could not run on a live instance at all, and
// the runner could not start while a job held it. Its placement inside
// /run/gvm/<name> is right for THAT role, where the holder's lifetime is the
// unit's; it is wrong for this one, because gvm@.service declares
// RuntimeDirectory= and systemd deletes that directory on a stop the job
// itself performs -- unlinking a flocked file under its holder and letting a
// second job create a fresh inode at the same path.
//
// Under LocksDir with the host jobs' locks, where packaging's tmpfiles
// already creates the directory 0750 root:root before the first job runs
// rather than leaving it to whichever job got there first. Losing tmpfs costs
// nothing: no flock survives a process death or a reboot wherever the file
// lives, and a surviving holder LINE cannot be mistaken for contention,
// because a *Busy is only ever produced after a real TryLock failure and
// writeHolder truncates. D-45 forbids `_` in an instance name, so this can
// never collide with job__guard.lock.
func InstanceJobLockPath(name string) string {
	MustValidName(name)
	return path.Join(LocksDir, "job_"+name+".lock")
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
