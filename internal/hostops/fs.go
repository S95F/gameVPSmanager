package hostops

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// WebGroup is the group gvm-web runs in (section 6.4).
//
// Files under /etc/gvm that the dashboard has to render are 0640 root:gvm so
// the web tier can read a spec and can never write one. Nothing else in this
// package grants that group anything.
const WebGroup = "gvm"

// FS is every filesystem effect this package has.
//
// It is a seam and not a convenience: the interesting half of a write is which
// path, which mode and which owner, and none of that can be exercised on the
// Windows development machine against a real /etc. A fake implementing this
// interface lets ops_test.go assert the whole derivation table -- and the
// order of chown before chmod -- without root and without Linux.
type FS interface {
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	// WriteAtomic replaces path or leaves it exactly as it was. backup keeps
	// the previous contents beside it.
	WriteAtomic(path string, data []byte, perm fs.FileMode, backup bool) error
	Remove(path string) error
	// Mkdir creates ONE directory. The parent must exist -- see mkdirLeaf.
	Mkdir(path string, perm fs.FileMode) error
	Chmod(path string, perm fs.FileMode) error
	// Chown sets the owner by NAME. An empty group means the account's own
	// primary group, which is the only correct answer for an adopted
	// instance: pzserver's primary group is pzserver, and deriving
	// "gvm-pz-main" from the instance name would chgrp a world to a group its
	// account is not in.
	Chown(path, owner, group string) error
	DiskUsage(path string) (platform.DiskUsage, error)
}

// Errors the file and directory verbs return. Each is a sentinel because the
// caller reacts differently: an unknown kind is a call-site bug, a missing
// parent is a broken installation, and a bad legacy unit name is operator
// input that must be refused rather than sanitised.
var (
	ErrUnknownKind   = errors.New("hostops: file or directory kind is not declared")
	ErrMissingParent = errors.New("hostops: the parent directory does not exist")
	ErrLegacyUnit    = errors.New("hostops: not a plausible legacy unit name")
	ErrNotADirectory = errors.New("hostops: path exists and is not a directory")
)

// FileKind is a control-plane file gvm owns (section 5).
//
// The verb names the kind and hostops derives the path, the mode and the
// owner. A verb that took a path instead would be a root-equivalent primitive
// wearing an enum's name.
//
// Secrets are deliberately ABSENT. /etc/gvm/secrets is 0700 root:root and its
// contents are the one thing that must never travel inside a generic request
// struct: secrets.Value exists precisely so a credential cannot be logged by
// accident, and a `file.write(kind=secret, body=…)` would hand every socket
// frame, every error string and every audit param map a plaintext password.
// Secret writes get their own verb, with their own type, in phase 15.
type FileKind string

const (
	// FileDropIn is the generated 10-gvm.conf: user, paths, env, credentials,
	// resources, sandbox, arch, JIT. Rewritten on every apply (section 7.1).
	FileDropIn FileKind = "dropin"
	// FileAdoptGuard is 20-adopt.conf on the GVM unit: Conflicts= against the
	// legacy unit, written once by `gvm adopt cutover`.
	FileAdoptGuard FileKind = "adopt_guard"
	// FileLegacyGuard is the reciprocal on the LEGACY unit, so systemd itself
	// refuses to run both against the same world (section 7.4).
	FileLegacyGuard FileKind = "legacy_guard"
	// FileJobTimer is the generated timer for one job (section 7.5).
	FileJobTimer FileKind = "job_timer"
	// FileJobDesc maps a slug back to its instance and job (D-13).
	FileJobDesc FileKind = "job_desc"
	// FileInstanceSpec is THE definition of one server.
	FileInstanceSpec FileKind = "instance_spec"
	// FileContentLock is the pinned content intent (D-03).
	FileContentLock FileKind = "content_lock"
	// FileManifestLock is the pinned manifest bytes (D-04).
	FileManifestLock FileKind = "manifest_lock"
	// FilePortsEcho is the echo of the materialised port allocation. Derived,
	// under /var/lib/gvm, and safe to lose.
	FilePortsEcho FileKind = "ports_echo"
	// FileHold is /run/gvm/<name>/hold: present iff an operator stopped the
	// instance (section 13.4). It is what stops `gvm-guard.timer` from
	// restarting a server somebody stopped on purpose, so writing it is a
	// privileged act with a consequence, and it is declared here rather than
	// written by whichever package happened to be doing the stopping.
	//
	// 0644 and world-readable on purpose: the unprivileged web tier has to be
	// able to say "held by an operator" instead of showing a red "down".
	FileHold FileKind = "hold"
	// FileDegraded is /run/gvm/<name>/degraded: the breadcrumb a job leaves
	// when it stopped a server and could not start it again (section 13.4).
	//
	// It is the exact opposite of FileHold and is declared beside it for that
	// reason. A hold suppresses `gvm-guard.timer`; this one is what the guard
	// timer acts on. Both are privileged writes with a consequence, so neither
	// is left to whichever package happened to be holding the instance when
	// things went wrong -- and routing it through here means the audit row
	// that says a world was left down is written by the same call that left
	// the marker.
	//
	// 0644 for the same reason as the hold: the web tier has to be able to
	// show "a nightly update left this stopped" rather than a bare red dot.
	FileDegraded FileKind = "degraded"
	// FileLease is /var/lib/gvm/instances/<name>/lease.json: which job owns
	// this instance right now (section 5.3, lifecycle.Lease).
	//
	// A privileged kind rather than a plain write because taking it is what
	// makes `gvm start` refuse, so an unaudited lease is an unaudited refusal.
	// It is under /var/lib/gvm rather than /run deliberately -- lifecycle's
	// Lease doc has the reason: /run is tmpfs, and a lease that vanished on
	// reboot could not tell "the job finished" from "the box died
	// mid-update".
	FileLease FileKind = "lease"
)

// fileDecl is one kind's declaration.
type fileDecl struct {
	Kind  FileKind
	Mode  fs.FileMode
	Owner string // "" is root
	Group string // "" is the owner's primary group

	// Backup keeps the previous contents as <path>.bak.
	//
	// It is false for everything under /etc/systemd/system. That directory is
	// scanned by the service manager, and gvm does not leave litter in a
	// directory another program parses. The loss is nil: every generated unit
	// file carries the spec sha256 in its header and is reproducible by
	// re-rendering. Operator-editable files under /etc/gvm keep their .bak,
	// because there the previous version is a human's work.
	Backup bool

	// Job says the kind is addressed by (instance, job) rather than by
	// instance alone.
	Job bool
	// Legacy says the kind needs Request.LegacyUnit.
	Legacy bool

	// ParentMode, when non-zero, is the mode of a parent directory this kind
	// may create. It is set only where the directory is part of the file's
	// identity rather than an independent object: a drop-in without its `.d`
	// directory is not a thing.
	ParentMode fs.FileMode

	Path func(t fileTarget) (string, error)
}

// fileTarget is the addressing information a file kind may use. It is a
// struct rather than three arguments so a kind that ignores a field ignores it
// visibly.
type fileTarget struct {
	Instance   string
	Job        string
	LegacyUnit string
}

var fileKinds = map[FileKind]fileDecl{
	FileDropIn: {
		Kind: FileDropIn, Mode: 0o644, ParentMode: 0o755,
		Path: func(t fileTarget) (string, error) { return instance.DropInPath(t.Instance), nil },
	},
	FileAdoptGuard: {
		Kind: FileAdoptGuard, Mode: 0o644, ParentMode: 0o755,
		Path: func(t fileTarget) (string, error) {
			// The drop-in directory is the one instance.DropInPath already
			// names; taking its parent keeps section 7.1's numbering
			// contract in one place instead of two.
			return path.Join(path.Dir(instance.DropInPath(t.Instance)), "20-adopt.conf"), nil
		},
	},
	FileLegacyGuard: {
		Kind: FileLegacyGuard, Mode: 0o644, ParentMode: 0o755, Legacy: true,
		Path: func(t fileTarget) (string, error) {
			unit, err := validateLegacyUnit(t.LegacyUnit)
			if err != nil {
				return "", err
			}
			return path.Join(instance.SystemdUnitDir, unit+".d", "20-gvm-adopt.conf"), nil
		},
	},
	FileJobTimer: {
		Kind: FileJobTimer, Mode: 0o644, Job: true,
		Path: func(t fileTarget) (string, error) { return instance.JobTimerPath(t.Instance, t.Job), nil },
	},
	FileJobDesc: {
		Kind: FileJobDesc, Mode: 0o644, Group: WebGroup, Backup: true, Job: true,
		ParentMode: 0o750,
		Path:       func(t fileTarget) (string, error) { return instance.JobDescPath(t.Instance, t.Job), nil },
	},
	FileInstanceSpec: {
		Kind: FileInstanceSpec, Mode: spec.ModeSpec, Group: WebGroup, Backup: true,
		Path: func(t fileTarget) (string, error) { return instance.PathsFor(t.Instance).Spec, nil },
	},
	FileContentLock: {
		Kind: FileContentLock, Mode: 0o644, Backup: true,
		Path: func(t fileTarget) (string, error) { return instance.PathsFor(t.Instance).ContentLock, nil },
	},
	FileManifestLock: {
		Kind: FileManifestLock, Mode: spec.ModeSpec, Group: WebGroup, Backup: true,
		Path: func(t fileTarget) (string, error) { return instance.PathsFor(t.Instance).ManifestLock, nil },
	},
	FilePortsEcho: {
		Kind: FilePortsEcho, Mode: 0o640,
		Path: func(t fileTarget) (string, error) { return instance.PathsFor(t.Instance).PortsJSON, nil },
	},
	FileHold: {
		// ParentMode is set because /run/gvm/<name> is systemd's
		// RuntimeDirectory= and the template declares
		// RuntimeDirectoryPreserve=restart -- so the directory is REMOVED by a
		// clean stop, which is exactly the moment the hold has to be written.
		// A hold whose directory is gone is not a hold. Ownership stays
		// root:root here; `gvm run-prepare` restores the 2750
		// gvm-<n>:gvm-console setgid ownership as ExecStartPre before the
		// runner creates anything else in it (section 6.5).
		Kind: FileHold, Mode: 0o644, ParentMode: 0o750,
		Path: func(t fileTarget) (string, error) { return instance.PathsFor(t.Instance).Hold, nil },
	},
	FileDegraded: {
		// ParentMode for the same reason the hold has one: the marker is
		// written in the moments after a stop, and a clean stop takes
		// /run/gvm/<name> with it (RuntimeDirectoryPreserve=restart).
		Kind: FileDegraded, Mode: 0o644, ParentMode: 0o750,
		Path: func(t fileTarget) (string, error) { return instance.PathsFor(t.Instance).Degraded, nil },
	},
	FileLease: {
		// 0640 root:root inside a 0750 root:root directory. No .bak: a
		// previous lease is not a human's work, and a stale copy of a
		// document whose whole purpose is to say "a job is running NOW" is
		// the one kind of litter that could be misread as live.
		Kind: FileLease, Mode: 0o640,
		Path: func(t fileTarget) (string, error) { return instance.PathsFor(t.Instance).Lease, nil },
	},
}

// DirKind is a directory gvm creates for an instance (section 5).
//
// /run/gvm/<name> is deliberately absent: systemd creates it through
// RuntimeDirectory= and `gvm run-prepare` sets its setgid ownership as
// ExecStartPre, because the setgid bit has to be in place before the runner
// creates the socket inside it (section 6.5). Creating it here would work on
// the first boot and then hide a unit that had lost the directive.
type DirKind string

const (
	DirSpecDir      DirKind = "spec_dir"
	DirSecrets      DirKind = "secrets"
	DirState        DirKind = "state"
	DirSteamScripts DirKind = "steam_scripts"
	DirSnap         DirKind = "snap"
	DirSteamHome    DirKind = "steam_home"
	DirLogs         DirKind = "logs"
	DirJobLogs      DirKind = "job_logs"
	DirBackups      DirKind = "backups"
	DirSrv          DirKind = "srv"
	DirJobs         DirKind = "jobs"
	// DirInstall and DirData are the GAME's own two directories, and they are
	// the only kinds whose path comes from the instance YAML rather than from
	// the instance name -- an adopted pz-main installs in /opt/pzserver with
	// its world in /home/pzserver/Zomboid, and deriving /srv/gvm/pz-main/...
	// would create the wrong directory beside the right one.
	//
	// They exist because D-15's write-probe lists both in the transient unit's
	// ReadWritePaths=, and systemd refuses namespace setup for a path that is
	// not there: without these the FIRST apply of every new managed instance
	// fails at the probe, before the drop-in, the reload and the enable.
	DirInstall DirKind = "install_dir"
	DirData    DirKind = "data_dir"
)

// dirDecl is one directory kind's declaration.
type dirDecl struct {
	Kind  DirKind
	Mode  fs.FileMode
	Group string

	// OwnedByInstance means the directory belongs to the account the game
	// runs as, which is read from the instance's own YAML rather than derived
	// from its name: an adopted instance keeps pzserver, and chowning its log
	// directory to a gvm-pz-main that does not exist fails, while chowning it
	// to one that does leaves the running server unable to write.
	OwnedByInstance bool

	// CreateOnly means the mode and owner are asserted when this call creates
	// the directory and never afterwards. It is how an ADOPTED instance's
	// existing tree is left alone: section 5 says an adopted instance keeps its
	// paths verbatim, and re-asserting 0750 gvm-<n> over /opt/pzserver would
	// take a live world's directories away from the process using them. For a
	// managed instance the distinction is invisible, because gvm created them.
	CreateOnly bool

	// Path derives the directory from the instance NAME. Exactly one of Path
	// and SpecPath is set.
	Path func(name string) string
	// SpecPath reads it out of the instance's own YAML instead, for the two
	// kinds an adopted instance does not keep under /srv/gvm.
	SpecPath func(in *spec.Instance) string
}

var dirKinds = map[DirKind]dirDecl{
	DirSpecDir: {
		Kind: DirSpecDir, Mode: spec.ModeSpecDir, Group: WebGroup,
		Path: func(n string) string { return instance.PathsFor(n).SpecDir },
	},
	DirSecrets: {
		Kind: DirSecrets, Mode: 0o700,
		Path: func(n string) string { return instance.PathsFor(n).SecretsDir },
	},
	DirState: {
		Kind: DirState, Mode: 0o750,
		Path: func(n string) string { return instance.PathsFor(n).State },
	},
	DirSteamScripts: {
		Kind: DirSteamScripts, Mode: 0o750,
		Path: func(n string) string { return instance.PathsFor(n).SteamScriptDir },
	},
	DirSnap: {
		Kind: DirSnap, Mode: 0o750,
		Path: func(n string) string { return instance.PathsFor(n).SnapDir },
	},
	DirSteamHome: {
		Kind: DirSteamHome, Mode: 0o700, OwnedByInstance: true,
		Path: func(n string) string { return instance.PathsFor(n).SteamHome },
	},
	DirLogs: {
		Kind: DirLogs, Mode: 0o750, OwnedByInstance: true,
		Path: func(n string) string { return instance.PathsFor(n).Logs },
	},
	DirJobLogs: {
		Kind: DirJobLogs, Mode: 0o750,
		Path: func(n string) string { return instance.PathsFor(n).JobLogDir },
	},
	// 0700 root:root, and this is the one line of install.sh deliberately not
	// ported: a compromised mod must not be able to read every world the
	// server has ever had (D-44).
	DirBackups: {
		Kind: DirBackups, Mode: 0o700,
		Path: func(n string) string { return instance.PathsFor(n).Backups },
	},
	DirSrv: {
		Kind: DirSrv, Mode: 0o750, OwnedByInstance: true,
		Path: func(n string) string { return instance.PathsFor(n).Srv },
	},
	// The job descriptor directory is host-wide but is created per apply,
	// because nothing else creates it and a missing one turns every timer
	// write into a failure the operator cannot act on.
	DirJobs: {
		Kind: DirJobs, Mode: spec.ModeSpecDir, Group: WebGroup,
		Path: func(string) string { return instance.EtcJobsDir },
	},
	DirInstall: {
		Kind: DirInstall, Mode: 0o750, OwnedByInstance: true, CreateOnly: true,
		SpecPath: func(in *spec.Instance) string { return in.Spec.Paths.Install },
	},
	DirData: {
		Kind: DirData, Mode: 0o750, OwnedByInstance: true, CreateOnly: true,
		SpecPath: func(in *spec.Instance) string { return in.Spec.Paths.Data },
	},
}

// DirPath returns the path a directory kind resolves to for one instance.
//
// It is exported so a planner can NAME the directory it would ensure without
// issuing the verb: `gvm apply --diff` has to print the object, and a second
// derivation in the planner is how a plan ends up describing a directory the
// verb does not touch. ok is false for an undeclared kind, which is a
// call-site bug rather than an operating condition.
func DirPath(k DirKind, instanceName string) (string, bool) {
	d, ok := dirKinds[k]
	if !ok || d.Path == nil {
		// A spec-resolved kind has no answer from the name alone -- that is the
		// whole reason it is spec-resolved -- so the planner reads it from the
		// instance it already holds. Answering with a derived /srv/gvm path
		// here would be a plan that names a directory the verb does not touch.
		return "", false
	}
	return d.Path(instanceName), true
}

// DirPathFor resolves any kind, including the two that come from the spec.
func DirPathFor(k DirKind, in *spec.Instance) (string, bool) {
	d, ok := dirKinds[k]
	if !ok {
		return "", false
	}
	if d.SpecPath != nil {
		if in == nil {
			return "", false
		}
		return d.SpecPath(in), true
	}
	return d.Path(in.Metadata.Name), true
}

// FilePath returns the path a file kind resolves to, for the same reason.
//
// It takes the whole addressing triple rather than just the instance because
// several kinds are per-job and one is addressed by the legacy unit it guards
// against; passing them separately is what lets the same validator refuse an
// operator-typed unit name here and inside the verb.
func FilePath(k FileKind, instanceName, job, legacyUnit string) (string, error) {
	d, ok := fileKinds[k]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownKind, k)
	}
	t, err := Request{Instance: instanceName, Job: job, LegacyUnit: legacyUnit}.fileTarget(d)
	if err != nil {
		return "", err
	}
	return d.Path(t)
}

// FileKinds and DirKinds return the declared kinds, sorted. They are what the
// capability listing prints and what the closure tests enumerate.
func FileKinds() []FileKind {
	out := make([]FileKind, 0, len(fileKinds))
	for k := range fileKinds {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func DirKinds() []DirKind {
	out := make([]DirKind, 0, len(dirKinds))
	for k := range dirKinds {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// legacyUnitRE is systemd's unit-name charset, restricted to services.
//
// The legacy guard is the only path in this package derived from a string an
// operator typed, so it is the only one that needs a validator at all. It has
// to reject '/' and '..' (a drop-in written into an arbitrary directory is a
// root shell), and it is limited to .service because the guard's whole content
// is a Conflicts= against a service.
//
// '@' is in, because a legacy setup may well be a template instance
// (srcds@1.service) and refusing to guard one would leave both units able to
// run against the same world. The backslash systemd allows in escaped unit
// names is deliberately OUT: it buys nothing here and every character that can
// appear in a path is a character this validator has to reason about.
var legacyUnitRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]{0,127}\.service$`)

func validateLegacyUnit(unit string) (string, error) {
	if !legacyUnitRE.MatchString(unit) {
		return "", fmt.Errorf("%w: %q", ErrLegacyUnit, unit)
	}
	if strings.Contains(unit, "..") {
		return "", fmt.Errorf("%w: %q contains '..'", ErrLegacyUnit, unit)
	}
	// gvm's own units are not "legacy". Accepting one would let the adopt
	// guard write a drop-in into gvm@<name>.service.d under a slot number the
	// numbering contract in section 7.1 does not describe, which is exactly
	// the drift the numbering exists to prevent.
	if strings.HasPrefix(unit, "gvm@") || strings.HasPrefix(unit, "gvm-job@") {
		return "", fmt.Errorf("%w: %q is a gvm unit, not the legacy one it is meant to conflict with", ErrLegacyUnit, unit)
	}
	return unit, nil
}

// osFS is the production FS. Every method is a thin wrapper, and the atomic
// write is spec's rather than a second implementation of it: the crash
// ordering that file pins down -- write, fsync, chmod, back up, rename, fsync
// the directory -- has to hold for a systemd drop-in for the same reason it
// holds for an instance YAML.
type osFS struct{}

func (osFS) Stat(p string) (fs.FileInfo, error)  { return os.Stat(p) }
func (osFS) ReadFile(p string) ([]byte, error)   { return os.ReadFile(p) }
func (osFS) Remove(p string) error               { return os.Remove(p) }
func (osFS) Mkdir(p string, m fs.FileMode) error { return os.Mkdir(p, m) }
func (osFS) Chmod(p string, m fs.FileMode) error { return os.Chmod(p, m) }

func (osFS) WriteAtomic(p string, data []byte, perm fs.FileMode, backup bool) error {
	if backup {
		return spec.WriteFileAtomic(p, data, perm)
	}
	return spec.WriteFileAtomicNoBackup(p, data, perm)
}

// Chown resolves names through os/user, which with CGO_ENABLED=0 reads
// /etc/passwd and /etc/group directly.
//
// That is the behaviour this package wants: an apply must not block on an NSS
// lookup against a network directory service, and every account gvm cares
// about is local by construction (section 6.4).
func (osFS) Chown(p, owner, group string) error {
	uid, gid := -1, -1
	if owner != "" {
		u, err := user.Lookup(owner)
		if err != nil {
			return fmt.Errorf("hostops: chown %s: %w", p, err)
		}
		if uid, err = strconv.Atoi(u.Uid); err != nil {
			return fmt.Errorf("hostops: account %s has a non-numeric uid %q", owner, u.Uid)
		}
		if group == "" {
			if gid, err = strconv.Atoi(u.Gid); err != nil {
				return fmt.Errorf("hostops: account %s has a non-numeric gid %q", owner, u.Gid)
			}
		}
	}
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return fmt.Errorf("hostops: chown %s: %w; the gvm package's postinstall creates it", p, err)
		}
		if gid, err = strconv.Atoi(g.Gid); err != nil {
			return fmt.Errorf("hostops: group %s has a non-numeric gid %q", group, g.Gid)
		}
	}
	if uid == -1 && gid == -1 {
		return nil
	}
	return os.Chown(p, uid, gid)
}

// DiskUsage is platform.Statfs, which is Linux-only and returns
// platform.ErrNotSupported elsewhere. The arithmetic a preflight does with the
// answer lives in platform.DiskUsage and has no build tag, so the decision is
// testable here even though the measurement is not.
func (osFS) DiskUsage(p string) (platform.DiskUsage, error) { return platform.Statfs(p) }

// writeFile is the whole of file.write, and it is separate from the handler so
// that the "is it already right?" question and the write are one function.
//
// It reports whether the host changed. Skipping an identical write is not an
// optimisation: rewriting a drop-in that did not change bumps its mtime, and
// the reconciler's next daemon-reload then looks like it had a reason.
func writeFile(f FS, d fileDecl, p string, body []byte) (bool, error) {
	if d.ParentMode != 0 {
		if _, err := mkdirLeaf(f, path.Dir(p), d.ParentMode, d.Owner, d.Group); err != nil {
			return false, err
		}
	} else if err := requireParent(f, p); err != nil {
		return false, err
	}

	changed := true
	if fi, err := f.Stat(p); err == nil {
		cur, rerr := f.ReadFile(p)
		// A file that cannot be read is replaced rather than reported: the
		// question here is what the host should look like afterwards, and an
		// unreadable drop-in is not a state worth preserving.
		changed = rerr != nil || !bytes.Equal(cur, body) || fi.Mode().Perm() != d.Mode.Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	// Ownership is a separate step because spec's writer has no business
	// knowing about unix accounts -- and it is re-asserted on EVERY call, not
	// only after a write.
	//
	// The three kinds that declare a group (the instance spec, the manifest
	// lock, a job descriptor) are precisely the ones whose bytes converge on
	// the first apply -- apply reads the spec off disk and writes those same
	// bytes back -- so gating the chown on a content change means their
	// ownership is asserted once, ever. 0640 root:gvm is what lets gvm-web read
	// a spec, and a chown lost to a restore, an rsync, a hand edit or a failed
	// first write would then never be repaired by any later apply. The
	// directory kinds already self-heal this way in mkdirLeaf, and dirs.go
	// advertises exactly that: "already present; the mode and owner are
	// re-asserted".
	//
	// A chown to the value already in place is a no-op and does not touch the
	// mtime, and no unit-file kind declares an owner or a group, so this cannot
	// provoke a drop-in rewrite or a daemon-reload.
	own := func() error {
		if d.Owner == "" && d.Group == "" {
			return nil
		}
		return f.Chown(p, d.Owner, d.Group)
	}
	if !changed {
		// The returned Changed stays false even when this repairs something.
		// That matches mkdirLeaf, which has always re-asserted a directory's
		// owner outside its own changed calculation; reporting an ownership
		// repair would need the FS seam to be able to READ an owner, which it
		// deliberately cannot.
		return false, own()
	}
	if err := f.WriteAtomic(p, body, d.Mode, d.Backup); err != nil {
		return false, err
	}
	// The atomic write already applied the mode.
	if err := own(); err != nil {
		return true, err
	}
	return true, nil
}

// mkdirLeaf creates ONE directory, and requires its parent to already exist.
//
// os.MkdirAll would be shorter and wrong: every intermediate directory would
// get the leaf's mode, so ensuring /var/log/gvm/<name> at 0750 on a host where
// systemd-tmpfiles had not run would silently create /var/log/gvm at 0750
// instead of the 0755 section 5.1 specifies, and nothing would ever say so. A
// missing parent is a broken installation and is reported as one.
//
// It reports whether anything changed, so an apply that is already correct
// says "= unchanged" rather than claiming work it did not do.
func mkdirLeaf(f FS, p string, mode fs.FileMode, owner, group string) (bool, error) {
	if err := requireParent(f, p); err != nil {
		return false, err
	}
	changed := false
	fi, err := f.Stat(p)
	switch {
	case err == nil:
		if !fi.IsDir() {
			return false, fmt.Errorf("%w: %s", ErrNotADirectory, p)
		}
		if fi.Mode().Perm() != mode.Perm() {
			changed = true
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := f.Mkdir(p, mode); err != nil {
			return false, err
		}
		changed = true
	default:
		return false, err
	}

	// chown BEFORE chmod. On Linux a chown clears the setgid and setuid bits,
	// so a chmod that ran first would be silently undone -- the lesson `gvm
	// run-prepare` already carries for /run/gvm/<name>.
	if owner != "" || group != "" {
		if err := f.Chown(p, owner, group); err != nil {
			return changed, err
		}
	}
	// The chmod runs only when this call created the directory or found its
	// mode wrong. It cannot be unconditional: `changed` is computed from
	// Mode().Perm(), which masks setgid off, so a live 2750 compares equal to a
	// declared 0750 and an unconditional chmod would then strip the bit. The
	// directory that reaches this is /run/gvm/<name> -- FileHold declares a
	// ParentMode for it, and `gvm run-prepare` owns it at 2750
	// gvm-<n>:gvm-console per section 6.5, which is the whole mechanism of
	// D-09/D-10. mkdirLeaf's job there is to make the directory exist so the
	// hold can land, not to redefine its mode. The post-Mkdir chmod is still
	// required: os.Mkdir is subject to umask.
	if changed {
		if err := f.Chmod(p, mode); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// requireParent refuses to act when the containing directory is absent.
func requireParent(f FS, p string) error {
	parent := path.Dir(p)
	fi, err := f.Stat(parent)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s, needed for %s; is the gvm package installed and has systemd-tmpfiles run?",
			ErrMissingParent, parent, path.Base(p))
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s", ErrNotADirectory, parent)
	}
	return nil
}
