// The seams every instance verb shares, and the production wiring of them.
//
// Development happens on Windows and every path in section 5 is an absolute
// Linux path, so a command that reached for os.ReadFile("/etc/gvm/...") would
// be testable only on the machine it is never written on. Everything a verb
// touches therefore arrives through HostEnv, which has exactly one production
// constructor -- and that constructor refuses off Linux rather than half
// working, because the single most misleading thing this CLI could do is
// report a clean apply against a host it never touched.
//
// Nothing here writes. The write path is internal/reconcile -> internal/hostops
// and there is no second one: every mutation in this package is a
// hostops.Request handed to HostEnv.Ops.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/content/steamworkshop"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/ports"
	"github.com/S95F/gameVPSmanager/internal/reconcile"
	"github.com/S95F/gameVPSmanager/internal/secrets"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// HostOps is the privileged surface, and it is exactly hostops.Do.
//
// One method, for the same reason internal/reconcile and internal/lifecycle
// each declare the same interface: there is no second method through which a
// privileged operation could be reached, so a CLI command cannot grow a
// side door into root by adding a call.
type HostOps interface {
	Do(ctx context.Context, req hostops.Request) (hostops.Result, error)
}

// HostEnv is every host-touching thing the instance verbs use.
//
// A zero field means "the production one", which is why a test supplies the
// whole struct rather than patching parts of it: half-production seams would
// let a Windows test reach C:\etc\gvm by accident, and that is a test that
// modifies the developer's machine.
type HostEnv struct {
	// Ops is the privileged surface. Required.
	Ops HostOps
	// Files is the read-only filesystem the observing verbs see.
	Files lifecycle.Files
	// Units is the service manager, read-only. Nil means the unit state is
	// reported as NOT CHECKED rather than guessed at.
	Units lifecycle.UnitReader
	// Accounts answers whether a unix account exists. It is separate from
	// Ops because a planner asking "does pzserver exist" must not write an
	// audit row (hostops audits its mutating verbs).
	Accounts func(name string) bool
	// Ledger is the port reservation store (section 6.7). Nil disables the
	// reservation window, which is only correct in a test.
	Ledger *ports.Store
	// Listeners reads the host's bound sockets, the third occupancy source.
	// Nil is ports.HostListeners, which is Linux-only; an error from it is
	// reported as "occupancy is unknown here", never as "nothing is bound".
	Listeners func() ([]ports.Listener, error)
	// Ephemeral reads ip_local_port_range. Nil is ports.HostEphemeral, and a
	// failure falls back to platform.DefaultEphemeral with the substitution
	// recorded on the Occupancy.
	Ephemeral func() (platform.PortRange, error)
	// Catalog resolves game ids across the three shadowing layers.
	Catalog *spec.Catalog
	// LoadPinned reads an instance's pinned manifest bytes (D-04).
	LoadPinned func(instanceDir string) (*spec.Manifest, error)
	// HasSecret reports whether a credential file exists, for the drop-in's
	// LoadCredential= lines. It answers presence and never reads a value.
	HasSecret func(inst, id string) bool
	// Probe is the D-15 write-probe. Nil is the real one, which launches a
	// transient unit through systemd-run; it is a seam ONLY so that a test can
	// exercise the plan on a box with no systemd, never so that an apply can
	// skip it.
	Probe reconcile.ProbeFunc
	// Statfs answers how much room a volume has, for the destroy preflight's
	// "could this be undone" arithmetic. Nil is platform.Statfs, which is
	// Linux-only; it is a seam so that the REFUSAL is exercised on the
	// development box, where the real one returns ErrNotSupported and every
	// volume is therefore reported unchecked.
	Statfs func(string) (platform.DiskUsage, error)
	// Paths locates an instance's control-plane path set. Nil is
	// instance.PathsFor. It is a seam because two of those paths are used as
	// REAL filesystem paths rather than as keys into Files: the destroy jail
	// resolves SecretsDir with filepath.IsAbs, which refuses a Linux constant
	// on Windows, and a spot verify creates SnapDir with os.MkdirAll, which on
	// Windows would land on the developer's own C: drive. Without the seam the
	// destroy path could neither be driven nor safely driven on the machine it
	// is written on.
	Paths func(name string) instance.Paths
	// ContentRoot opens one content root read-only, for the content verbs.
	//
	// Nil is os.DirFS behind an os.Stat, and the Stat is deliberately not an
	// Lstat: a workshop root reached through a symlink is ordinary on an
	// adopted box, and reporting it as absent would send a server_boot
	// instance into an hour-long acquisition boot it does not need. A root
	// that is not there returns an error wrapping fs.ErrNotExist, which the
	// caller SKIPS with a note -- the server writes workshop content to
	// different locations depending on version and a manifest lists both.
	//
	// It is a seam for the reason Paths is: these are absolute Linux paths,
	// and os.DirFS("/srv/gvm/...") on the development machine resolves against
	// C:. Without it the three-way diff could not be driven on the box it is
	// written on.
	ContentRoot func(abs string) (fs.FS, error)
	// Workshop builds the Steam Workshop metadata client `gvm content
	// collection sync` fetches through. Nil is steamworkshop.NewAPI.
	//
	// A seam so that a test points it at an httptest server through
	// fetch.Loopback rather than at Steam. That is what makes "no test in this
	// tree touches the network" a property of the construction: the only
	// command here that fetches at all cannot reach a real host unless
	// somebody replaces this field.
	Workshop func(cfg steamworkshop.Config) (*steamworkshop.API, error)
	// Shutdown drives the ladder for `gvm stop`. Nil makes a stop REFUSE:
	// audit.InstanceStop requires `rung` on its success row, and a stop that
	// cannot say whether the world saved is the record the bash suite already
	// produced.
	Shutdown lifecycle.ShutdownFunc
	// BootID is this boot's id, for classifying a job lease. Empty only
	// disables the boot check, which is the safe direction.
	BootID string
	// Audit is where the job layer's `job.run` spans go. Nil is a
	// journal-only logger: section 3.3 is explicit that a verb works with the
	// control plane dead and logs a warning if the record fails, so a missing
	// sink must not be a missing verb.
	Audit *audit.Logger
	// Now is the clock.
	Now func() time.Time

	// Jobs are the work engines internal/jobs drives, and the seams that let
	// a whole job run be driven from a Windows test (job.go).
	Jobs JobSeams

	// closers are released by Close. Production holds a systemd connection
	// and an open spool file.
	closers []func() error
}

// Close releases whatever the production constructor opened.
func (h *HostEnv) Close() error {
	var first error
	for i := len(h.closers) - 1; i >= 0; i-- {
		if err := h.closers[i](); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (h *HostEnv) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *HostEnv) files() lifecycle.Files {
	if h.Files != nil {
		return h.Files
	}
	return osFiles{}
}

func (h *HostEnv) statfs() func(string) (platform.DiskUsage, error) {
	if h.Statfs != nil {
		return h.Statfs
	}
	return platform.Statfs
}

func (h *HostEnv) paths(name string) instance.Paths {
	if h.Paths != nil {
		return h.Paths(name)
	}
	return instance.PathsFor(name)
}

// contentRoot opens one content root, or reports that it is not there.
func (h *HostEnv) contentRoot(abs string) (fs.FS, error) {
	if h.ContentRoot != nil {
		return h.ContentRoot(abs)
	}
	return osContentRoot(abs)
}

// osContentRoot is the production content root: os.DirFS, gated on a Stat that
// FOLLOWS symlinks. See HostEnv.ContentRoot.
func osContentRoot(abs string) (fs.FS, error) {
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		// Not a missing root and not a usable one. Reported as its own
		// condition rather than as fs.ErrNotExist, because a FILE where a
		// workshop root belongs is a mount that did not happen, and skipping
		// it silently reports every mod as not downloaded.
		return nil, &fs.PathError{Op: "open", Path: abs, Err: errors.New("not a directory")}
	}
	return os.DirFS(abs), nil
}

// workshopAPI builds the Steam Workshop client. See HostEnv.Workshop.
func (h *HostEnv) workshopAPI(cfg steamworkshop.Config) (*steamworkshop.API, error) {
	if h.Workshop != nil {
		return h.Workshop(cfg)
	}
	return steamworkshop.NewAPI(cfg)
}

func (h *HostEnv) catalog() *spec.Catalog {
	if h.Catalog != nil {
		return h.Catalog
	}
	return spec.SystemCatalog(games.FS)
}

func (h *HostEnv) loadPinned(dir string) (*spec.Manifest, error) {
	if h.LoadPinned != nil {
		return h.LoadPinned(dir)
	}
	return spec.LoadPinned(dir)
}

func (h *HostEnv) accountExists(name string) bool {
	if h.Accounts != nil {
		return h.Accounts(name)
	}
	_, err := user.Lookup(name)
	return err == nil
}

func (h *HostEnv) hasSecret(inst, id string) bool {
	if h.HasSecret != nil {
		return h.HasSecret(inst, id)
	}
	ok, err := secrets.DefaultStore().Has(secrets.InstanceRef(inst, id))
	return err == nil && ok
}

// host returns the seams, building the production ones on first use.
//
// The Linux gate is HERE rather than at the top of each command on purpose: a
// test that supplies its own HostEnv is not touching this machine's /etc, so
// it should run, and every command that reaches for the real host should get
// the same refusal in the same words.
func (env *Env) host(ctx context.Context) (*HostEnv, error) {
	if env.Host != nil {
		return env.Host, nil
	}
	if runtime.GOOS != "linux" {
		return nil, platform.Unsupported("gvm's privileged verbs need a Linux host")
	}
	h := &HostEnv{}

	lg, closeAudit := productionAudit()
	h.closers = append(h.closers, closeAudit)
	// The same logger hostops audits through, so a job's `job.run` span and
	// the `instance_stop` row its ladder writes land in one spool in one
	// order. Two loggers would be two spool files whose interleaving nothing
	// preserved.
	h.Audit = lg

	ops, err := hostops.NewLocal(ctx, lg)
	if err != nil {
		_ = h.Close()
		return nil, err
	}
	h.Ops = ops
	h.closers = append(h.closers, ops.Close)
	h.Units = unitsOver{ops: ops}
	h.Ledger = &ports.Store{Dir: instance.StateRoot}
	if id, err := audit.BootID(); err == nil {
		h.BootID = id
	}
	env.Host = h
	return h, nil
}

// productionAudit builds the two off-database sinks of section 14.8.
//
// A spool that cannot be opened is a WARNING and not a failure. Section 3.3 is
// explicit that `sudo gvm stop pz-main` works with the whole control plane
// dead and logs a warning if the audit insert fails rather than refusing to
// act; a spool file on a full disk is the same condition, and refusing to stop
// a server because the paperwork failed is the wrong trade every time.
func productionAudit() (*audit.Logger, func() error) {
	sinks := audit.MultiSink{audit.NewSlogSink(slog.Default())}
	noop := func() error { return nil }

	bootID, err := audit.BootID()
	if err != nil {
		slog.Warn("audit spool disabled: boot id unreadable, records go to the journal only",
			"err", err)
		return audit.New(sinks), noop
	}
	sp, err := audit.OpenSpool(path.Join(instance.StateRoot, "spool"), bootID)
	if err != nil {
		slog.Warn("audit spool disabled: records go to the journal only", "err", err)
		return audit.New(sinks), noop
	}
	return audit.New(append(sinks, sp)), sp.Close
}

// unitsOver turns the privileged surface into a read-only unit reader.
//
// hostops' unit verbs take an INSTANCE and derive the unit name, which is the
// property that makes the verb table closed; this adapter therefore turns a
// unit name back into the instance it belongs to and refuses anything that is
// not one of gvm's own. That is not a workaround for the enum, it is the enum
// working: a caller that wanted `sshd.service` has nowhere to put it.
type unitsOver struct{ ops HostOps }

func (u unitsOver) State(ctx context.Context, unit string) (platform.UnitState, error) {
	name, ok := instanceOfUnit(unit)
	if !ok {
		return platform.UnitState{}, fmt.Errorf("gvm: %q is not a gvm instance unit", unit)
	}
	res, err := u.ops.Do(ctx, hostops.Request{
		Verb: hostops.VerbUnitState, Actor: audit.RootCLI(), Instance: name,
	})
	if err != nil {
		return platform.UnitState{}, err
	}
	return res.State, nil
}

func (u unitsOver) ListUnits(ctx context.Context, pattern string) ([]platform.UnitState, error) {
	scope := hostops.ScopeInstances
	switch {
	case strings.HasPrefix(pattern, "gvm-job@") && strings.HasSuffix(pattern, ".timer"):
		scope = hostops.ScopeJobTimers
	case strings.HasPrefix(pattern, "gvm-job@"):
		scope = hostops.ScopeJobs
	}
	res, err := u.ops.Do(ctx, hostops.Request{
		Verb: hostops.VerbUnitList, Actor: audit.RootCLI(), Scope: scope,
	})
	if err != nil {
		return nil, err
	}
	return res.Units, nil
}

// instanceOfUnit reverses instance.UnitName.
func instanceOfUnit(unit string) (string, bool) {
	const pre, suf = "gvm@", ".service"
	if !strings.HasPrefix(unit, pre) || !strings.HasSuffix(unit, suf) {
		return "", false
	}
	name := unit[len(pre) : len(unit)-len(suf)]
	if instance.ValidateName(name) != nil {
		return "", false
	}
	return name, true
}

// inspector is the read-only host view a PLANNER may use (internal/reconcile's
// Inspector). Reads and no writes, so that `gvm apply --diff` can honestly say
// nothing happened.
type inspector struct {
	files    lifecycle.Files
	accounts func(string) bool
}

// Stat uses the Files seam and is deliberately answered by a LSTAT in
// production (osFiles below): a systemd enablement is a symlink into
// /usr/lib/systemd/system, and a dangling one is still an enablement. Reporting
// it as absent would make every apply re-enable a unit that is already enabled,
// which is exactly the idempotence this command is judged on.
func (i inspector) Stat(p string) (fs.FileInfo, error) { return i.files.Stat(p) }

func (i inspector) ReadFile(p string) ([]byte, error) { return i.files.ReadFile(p) }

func (i inspector) ReadDir(p string) ([]fs.DirEntry, error) { return i.files.ReadDir(p) }

func (i inspector) AccountExists(name string) bool {
	if i.accounts == nil {
		return false
	}
	return i.accounts(name)
}

func (h *HostEnv) inspector() inspector {
	return inspector{files: h.files(), accounts: h.accountExists}
}

// observer builds the lifecycle observer every read-only verb shares, so
// `gvm status`, `gvm list` and a refusal from `gvm stop` cannot disagree about
// what is true.
func (h *HostEnv) observer() *lifecycle.Observer {
	return &lifecycle.Observer{
		FS:     h.files(),
		Units:  h.Units,
		Alive:  aliveTri,
		BootID: h.BootID,
		Now:    h.Now,
	}
}

// aliveTri answers whether a pid is running, and answers UNKNOWN when it could
// not look.
//
// The asymmetry is load-bearing (see lifecycle.Lease.Classify): a lease whose
// holder cannot be checked stays LIVE, because calling it stale lets a second
// updater into an install tree the first one may still have open. Off Linux
// there is no /proc to read, so the honest answer is Unknown and never No.
// It asks whether the pid EXISTS, through ReadStat, and deliberately not
// through platform.Alive. Alive answers a different question -- "is this pid
// still the process that was pinned with these start ticks" -- and
// lifecycle.Lease carries no such pin, so the only value to pass would be
// zero. Alive ends in `st.StartTicks == wantStartTicks`, so a zero pin reads
// every LIVE process as (false, nil): provably gone. That would classify every
// running job's lease as STALE, which makes FindingStaleLease fire during
// every nightly, makes Allowed refuse `gvm start` with "a job died holding
// this instance" while one is working, and -- once gvm-guard.timer recovers a
// stale-leased instance -- would have the guard start a server a LIVE job had
// just stopped on purpose. There is no pin to lose: a reused pid reads as
// alive, which is the safe direction Classify documents.
func aliveTri(pid int) lifecycle.Tri {
	if pid <= 0 {
		return lifecycle.Unknown
	}
	_, err := platform.ReadStat(pid)
	return aliveFrom(err)
}

// aliveFrom is the decision aliveTri makes, split out so the asymmetry is
// assertable on a machine with no /proc.
func aliveFrom(err error) lifecycle.Tri {
	switch {
	case err == nil:
		return lifecycle.Yes
	case errors.Is(err, fs.ErrNotExist):
		// Looked, and it is gone. The only answer that may be No.
		return lifecycle.No
	default:
		// Could not look: no /proc, a permission error, a malformed stat
		// line. Spelling that the same way as "it is gone" lets a second
		// updater into an install tree the first one may still have open.
		return lifecycle.Unknown
	}
}

// osFiles is the production read-only filesystem.
//
// It is not lifecycle.OSFiles because Stat must be an LSTAT here: the only
// thing this package stats that can be a symlink is a unit's
// multi-user.target.wants entry, and following it turns "enabled, but the
// template is missing" into "not enabled", which is a re-enable on every
// apply and a wrong sentence in `gvm status`.
type osFiles struct{}

func (osFiles) Stat(name string) (fs.FileInfo, error)      { return os.Lstat(name) }
func (osFiles) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (osFiles) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }

// loadInstance reads and parses one instance YAML through the Files seam.
//
// It returns the raw bytes too, because the drop-in's header sha256 is taken
// over exactly these bytes: re-serialising the parsed struct would make
// `sha256sum /etc/gvm/instances/<name>.yaml` disagree with what apply wrote.
func loadInstance(h *HostEnv, name string) (*spec.Instance, []byte, error) {
	if err := instance.ValidateName(name); err != nil {
		return nil, nil, err
	}
	p := instance.PathsFor(name).Spec
	raw, err := h.files().ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fmt.Errorf("no instance %q: %s does not exist. "+
				"`gvm list` shows what this host has", name, p)
		}
		return nil, nil, err
	}
	in, diags, err := spec.ParseInstance(p, raw)
	if err != nil {
		return nil, nil, err
	}
	if bad := diags.Errors(); len(bad) > 0 {
		return nil, nil, fmt.Errorf("%s: %s", p, bad[0])
	}
	return in, raw, nil
}

// pinnedManifest loads the manifest an instance is PINNED to, never the
// catalog's current one (D-04, D-17).
//
// Reading the catalog here would make `apt upgrade gvm` able to change a live
// server's argv on the next apply, which is the whole reason the pin exists.
func pinnedManifest(h *HostEnv, in *spec.Instance) (*spec.Manifest, error) {
	dir := instance.PathsFor(in.Metadata.Name).SpecDir
	m, err := h.loadPinned(dir)
	if err == nil {
		if err := in.Spec.Manifest.Verify(m); err != nil {
			return nil, fmt.Errorf("%s: %w", spec.PinPath(dir), err)
		}
		return m, nil
	}
	if !errors.Is(err, spec.ErrNoPin) {
		return nil, err
	}

	// No manifest.lock.yaml yet. That is the state between `gvm create` and
	// the first `gvm apply`, which is the apply that WRITES the lock file --
	// so falling back to the catalog here is not a hole in D-17, it is the
	// only way the first apply can happen at all.
	//
	// What keeps it honest is that the fallback is VERIFIED against the pin
	// recorded in the instance YAML: revision and sha256 both. A catalog whose
	// manifest has moved since create is refused by name rather than silently
	// used, which is the same refusal the runner makes at start.
	cat, diags, cerr := h.catalog().Load(in.Spec.Game, overrideAuth(false))
	if cerr != nil {
		return nil, fmt.Errorf("%s has no pinned manifest and %s could not be loaded from the "+
			"catalog either: %w", spec.PinPath(dir), in.Spec.Game, cerr)
	}
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s does not validate:\n%s", in.Spec.Game, diags.Errors())
	}
	if err := in.Spec.Manifest.Verify(cat); err != nil {
		return nil, fmt.Errorf("%s records no pinned manifest bytes and the catalog's %s does "+
			"not match the pin in %s: %w", in.Metadata.Name, in.Spec.Game,
			instance.PathsFor(in.Metadata.Name).Spec, err)
	}
	return cat, nil
}

// oneInstance is the shared "exactly one instance name" check.
//
// It is a function rather than four copies because the error an operator sees
// for `gvm stop` and for `gvm status` must be the same sentence: two spellings
// of the same refusal read as two different problems.
func oneInstance(pos []string) (string, error) {
	switch len(pos) {
	case 0:
		return "", errors.New("name an instance")
	case 1:
		if err := instance.ValidateName(pos[0]); err != nil {
			return "", err
		}
		return pos[0], nil
	default:
		return "", fmt.Errorf("name one instance, not %d", len(pos))
	}
}

// actorFor is who the audit row says did this.
//
// A root CLI invocation is `root_cli` and not a user id, and that is honest
// rather than lazy: the person at the keyboard authenticated to sshd, not to
// gvm, and inventing a gvm identity for them would put a name in audit_log
// that nothing in gvm ever verified.
func actorFor(t Transport) audit.Actor {
	_ = t
	return audit.RootCLI()
}
