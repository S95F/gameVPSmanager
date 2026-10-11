package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/provider/steamcmd"
)

// SteamRunner is the sandbox the SteamCMD driver executes in.
//
// It is the implementation of steamcmd.Runner, which is the seam the driver
// exposes precisely so that nothing in internal/provider/steamcmd imports
// os/exec (its noexec_test.go asserts that). The driver renders a runscript and
// says what it wants run; this decides what that is allowed to reach.
//
// The registered driver -- provider.Lookup("steamcmd") -- has no runner and
// refuses the write path with ErrNoRunner, so Inspect, Observe and Verify work
// through the registry while an install has to go through a caller that built
// one of these on purpose:
//
//	r, err := jobs.NewSteamRunner(jobs.SteamConfig{Instance: name, Install: install})
//	p := steamcmd.New(steamcmd.WithRunner(r))
type SteamRunner struct {
	cfg  SteamConfig
	fsys ScriptFS
	// run is RunSandboxed, replaced in tests. The whole derivation -- which
	// paths are writable, which account, which budget -- is then asserted on
	// the development machine, where there is no systemd to start a unit with.
	run func(ctx context.Context, s Sandbox, cmd []string, out io.Writer) error
}

// SteamConfig is what the runner needs that an Invocation does not carry.
type SteamConfig struct {
	// Instance names the transient unit, so `systemctl status
	// gvm-steam-pz-main` works while a 65 GB download is running.
	Instance string

	// Install is the instance's install tree: the one place a SteamCMD run is
	// supposed to write game files. It is taken from configuration rather than
	// from the Invocation's WorkDir, so that "what may this unit write" is a
	// decision the job layer made and not one inherited from whatever
	// directory the driver chose to start in.
	Install string

	// ExtraWritable are further paths the run must write. An adopted instance
	// whose data tree is outside its install tree needs one; nothing else
	// should.
	ExtraWritable []string

	// SteamCMDDir overrides the read-only bind of the steamcmd installation.
	// Empty means it is derived from the Invocation's binary path, which is
	// where the driver's own locator found it.
	SteamCMDDir string

	// MemoryMax bounds the run. SteamCMD's depot decryption is memory-hungry
	// in proportion to chunk size and not to the download, so this is a
	// ceiling against a leak rather than a sizing decision.
	MemoryMax string
	// LimitNOFILE: steamcmd opens a descriptor per depot chunk in flight.
	LimitNOFILE int
}

// Defaults for a SteamCMD run. They are the figures section 7.7's worked
// example carries.
const (
	DefaultSteamMemoryMax   = "2G"
	DefaultSteamLimitNOFILE = 8192
	// steamScriptDirMode is 0750 root:root, the mode ARCHITECTURE 5.3 requires
	// for everything under /var/lib/gvm/instances/<name>: it holds a runscript
	// that may contain a private-branch password, and nothing but gvm's own
	// processes reads it.
	steamScriptDirMode fs.FileMode = 0o750
)

// SteamScriptCredential is the name the runscript is exposed under inside the
// transient unit, at $CREDENTIALS_DIRECTORY/<name>.
//
// A credential and not a path the unit opens itself, because it CANNOT open it:
// the script lives under /var/lib/gvm/instances/<name>/steam, and every
// ancestor of that is root-owned 0750 -- /var/lib/gvm and
// /var/lib/gvm/instances from packaging/tmpfiles/gvm.conf, the two per-instance
// levels from hostops' DirState and DirSteamScripts. ARCHITECTURE 5.3 requires
// that on purpose (a compromised mod must not be able to rewrite the content
// lock) and ARCHITECTURE:110 has doctor assert that gvm-<name> is in no group
// but its own, so there is no group that grants traversal either. The instance
// account therefore cannot reach the file whatever the file's own owner and
// mode are, and ReadWritePaths= bind-mounts without altering DAC.
//
// systemd reads the credential as root and exposes it inside the unit at 0400
// owned by the unit's uid, so the script reaches steamcmd with no ancestor mode
// changed and no widening of the state tree. The alternative a steamcmd that
// cannot open its runscript produces is the worst one available: it finds no
// commands, exits 0, and the run is reported as a successful update.
const SteamScriptCredential = "runscript"

// ScriptFS is every filesystem effect writing a runscript has.
//
// A seam for the same reason steamcmd.ShimFS is one: the interesting half of
// this write is the OWNER, and chown is meaningless on the Windows development
// machine. A fake implementing this exercises the whole ordering -- create the
// directory, hand it to the instance user, write the script 0600, hand that
// over too -- without root and without Linux.
type ScriptFS interface {
	MkdirAll(dir string, perm fs.FileMode) error
	WriteFile(path string, data []byte, perm fs.FileMode) error
	// Chown takes NUMERIC ids. Names would need a passwd lookup inside the
	// one call that must not fail halfway, and provider.UnixIdent has already
	// resolved and refused root.
	//
	// Nothing calls it any more, and it stays on the seam deliberately: the
	// runscript must remain ROOT-owned (see SteamScriptCredential), and
	// "nothing chowned it" is only a falsifiable assertion while a chown is
	// something this filesystem could have done. Remove it and the test that
	// pins the ownership becomes vacuous.
	Chown(path string, uid, gid int) error
	Remove(path string) error
}

// OSScriptFS is the real filesystem.
var OSScriptFS ScriptFS = osScriptFS{}

type osScriptFS struct{}

func (osScriptFS) MkdirAll(d string, perm fs.FileMode) error { return os.MkdirAll(d, perm) }
func (osScriptFS) Chown(p string, uid, gid int) error        { return os.Chown(p, uid, gid) }
func (osScriptFS) Remove(p string) error                     { return os.Remove(p) }

// WriteFile creates the file with its final mode rather than creating it 0666
// and fixing it afterwards.
//
// O_EXCL is deliberately absent and O_TRUNC present: the script is rewritten
// every run and a leftover from a crashed job must not make the next one fail.
// The mode is applied at creation because the window between a 0644 create and
// a chmod is a window in which another account on the box can read a
// private-branch password.
func (osScriptFS) WriteFile(p string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	// Mode is re-asserted because an existing file keeps its own mode through
	// O_CREATE, so a file left behind 0644 by an earlier build would stay
	// readable.
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// SteamOption configures a SteamRunner.
type SteamOption func(*SteamRunner)

// WithScriptFS replaces the filesystem the runscript is written through.
func WithScriptFS(f ScriptFS) SteamOption {
	return func(r *SteamRunner) {
		if f != nil {
			r.fsys = f
		}
	}
}

// WithSandboxRunner replaces RunSandboxed. Tests use it; nothing in production
// should.
func WithSandboxRunner(run func(ctx context.Context, s Sandbox, cmd []string, out io.Writer) error) SteamOption {
	return func(r *SteamRunner) {
		if run != nil {
			r.run = run
		}
	}
}

// NewSteamRunner builds the runner for one instance.
func NewSteamRunner(cfg SteamConfig, opts ...SteamOption) (*SteamRunner, error) {
	if err := instance.ValidateName(cfg.Instance); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSandbox, err)
	}
	if !strings.HasPrefix(cfg.Install, "/") {
		return nil, fmt.Errorf("%w: install tree %q is not an absolute path", ErrSandbox,
			cfg.Install)
	}
	if cfg.MemoryMax == "" {
		cfg.MemoryMax = DefaultSteamMemoryMax
	}
	if cfg.LimitNOFILE == 0 {
		cfg.LimitNOFILE = DefaultSteamLimitNOFILE
	}
	r := &SteamRunner{
		cfg:  cfg,
		fsys: OSScriptFS,
		run: func(ctx context.Context, s Sandbox, cmd []string, out io.Writer) error {
			return RunSandboxed(ctx, s, cmd, out)
		},
	}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

// steamPurposes is the closed set of runs this sandbox will launch.
//
// Closed because the purpose names the unit, and a purpose nobody declared
// would produce a unit name nobody can predict -- which is the one thing an
// operator needs to be able to type while a download is running.
var steamPurposes = map[string]string{
	// The update run writes the game. It gets the documented name of section
	// 7.7, because it is the one an operator looks for during a long download.
	"app_update": "gvm-steam-",
	// The check run reads metadata and writes only steamcmd's own cache. A
	// separate unit name rather than the same one, because the check and the
	// update it decides on run seconds apart inside one job, and a transient
	// unit that has not finished being garbage-collected makes the next start
	// fail with "unit already exists".
	"app_info": "gvm-steam-check-",
}

// Run writes the runscript and executes steamcmd in a transient service.
//
// The contract, in order:
//
//  1. inv.Script is written to inv.ScriptPath, mode inv.ScriptMode, root-owned,
//     and handed to the unit as the SteamScriptCredential. The driver
//     deliberately never writes it: the path is under a root-owned 0750 tree
//     the driver has no privilege to create.
//  2. inv.Binary is launched with inv.Args via systemd-run as a SERVICE, in
//     gvm-jobs.slice, with --uid set to the instance user (D-16). Any argument
//     naming inv.ScriptPath is rewritten to the credential's in-unit path,
//     because the host path is one the unit's uid cannot traverse to.
//  3. Combined stdout and stderr stream to inv.Stdout as they arrive.
//  4. inv.Budget becomes RuntimeMaxSec, and exceeding it is reported as
//     Exit{Timeout:true}.
//  5. The returned error is non-nil ONLY when the sandbox itself failed. A
//     steamcmd that ran and exited non-zero is an Exit, not an error, because
//     its exit code is not evidence either way (section 13.5) and the driver's
//     classifier is what reaches a verdict.
func (r *SteamRunner) Run(ctx context.Context, inv steamcmd.Invocation) (steamcmd.Exit, error) {
	sb, err := r.sandbox(inv)
	if err != nil {
		return steamcmd.Exit{}, err
	}
	if err := r.writeScript(inv); err != nil {
		return steamcmd.Exit{}, err
	}

	cmd := append([]string{inv.Binary}, rewriteScriptArgs(sb, inv)...)
	runErr := r.run(ctx, sb, cmd, inv.Stdout)
	if runErr == nil {
		return steamcmd.Exit{}, nil
	}
	var ee *ExitError
	if errors.As(runErr, &ee) {
		// The child ran. Hand the status back as data and no error: a
		// steamcmd that printed "Error! App '380870' state is 0x602" and
		// exited 0 is a failure, and one that exited 7 after a successful
		// install is not, so the only honest thing to do with this number is
		// give it to the classifier.
		return steamcmd.Exit{Code: ee.Code, Signaled: ee.Signaled, Timeout: ee.Timeout}, nil
	}
	return steamcmd.Exit{}, runErr
}

// rewriteScriptArgs points the argv at the credential rather than at the host
// path.
//
// The driver names the file it wants run and the sandbox decides how that file
// is delivered, which is the division Invocation's own doc comment draws: the
// driver has no uid, no privileges and no knowledge of the unit name the
// credential path is derived from.
func rewriteScriptArgs(sb Sandbox, inv steamcmd.Invocation) []string {
	in := sb.CredentialPath(SteamScriptCredential)
	out := make([]string, 0, len(inv.Args))
	for _, a := range inv.Args {
		if a == inv.ScriptPath {
			a = in
		}
		out = append(out, a)
	}
	return out
}

// sandbox derives the transient unit from one invocation.
//
// Pure, so every rule below is a test on the development machine: which paths
// are writable, that the steamcmd installation is NOT one of them, that HOME is
// the per-instance one, and that ProtectHome gets turned off exactly when a
// path lives under /home.
func (r *SteamRunner) sandbox(inv steamcmd.Invocation) (Sandbox, error) {
	prefix, ok := steamPurposes[inv.Purpose]
	if !ok {
		return Sandbox{}, fmt.Errorf("%w: unknown steamcmd purpose %q; the purpose names the "+
			"unit, so an undeclared one would produce a unit name nobody can predict",
			ErrSandbox, inv.Purpose)
	}
	if !inv.RunAs.Valid() {
		return Sandbox{}, fmt.Errorf("%w: %w: the invocation carries no account, and an unset "+
			"uid is root", ErrSandbox, provider.ErrIdent)
	}
	if !strings.HasPrefix(inv.Binary, "/") {
		return Sandbox{}, fmt.Errorf("%w: steamcmd binary %q is not an absolute path",
			ErrSandbox, inv.Binary)
	}
	if !strings.HasPrefix(inv.ScriptPath, "/") {
		return Sandbox{}, fmt.Errorf("%w: runscript path %q is not an absolute path",
			ErrSandbox, inv.ScriptPath)
	}

	home := inv.Env["HOME"]
	if strings.TrimSpace(home) == "" {
		return Sandbox{}, fmt.Errorf("%w: the invocation sets no HOME; steamcmd derives "+
			"XDG_DATA_HOME from it, and two instances sharing ~/.steam is the single best way "+
			"to corrupt an install (D-30)", ErrSandbox)
	}
	env := make(map[string]string, len(inv.Env))
	for k, v := range inv.Env {
		if k == "HOME" {
			continue
		}
		env[k] = v
	}

	steamDir := r.cfg.SteamCMDDir
	if steamDir == "" {
		steamDir = filepath.ToSlash(filepath.Dir(inv.Binary))
	}

	// The steamcmd installation is bound READ-ONLY and is deliberately not
	// writable, which costs a self-update and buys something worth more: the
	// directory is shared by every instance on the box, so an instance that
	// could write it could plant a binary every other instance then executes
	// as its own user. Keeping the tarball current is host preparation's job.
	// The runscript's directory is deliberately NOT here. The unit never opens
	// it -- the script arrives as a credential, see SteamScriptCredential --
	// and ARCHITECTURE 5.3 keeps /var/lib/gvm/instances/<name> root-owned 0750
	// precisely so a compromised mod cannot write into it.
	writable := dedupe([]string{
		r.cfg.Install,
		home,
	})
	writable = append(writable, dedupe(r.cfg.ExtraWritable)...)
	writable = dedupe(writable)

	sb := Sandbox{
		Unit:    prefix + r.cfg.Instance,
		User:    inv.RunAs.User(),
		Group:   inv.RunAs.Group(),
		Slice:   instance.JobSliceName,
		Home:    home,
		WorkDir: inv.WorkDir,
		Env:     env,

		ProtectSystem: "strict",
		ProtectHome:   protectHomeFor(append(append([]string{}, writable...), steamDir, inv.WorkDir)),

		PrivateTmp:       Yes(),
		NoNewPrivileges:  Yes(),
		RestrictSUIDSGID: Yes(),

		// No RestrictAddressFamilies and no SystemCallFilter. SteamCMD needs
		// AF_NETLINK and AF_UNIX as well as inet, forks a child per depot
		// worker and memory-maps its decryption buffers; a filter tight enough
		// to be worth writing has broken real downloads, and a filter that was
		// reverted in production the first week is worse than an honest
		// absence. The content fetcher, which runs OUR code, does carry both
		// (FetchSandbox).
		MemoryMax:   r.cfg.MemoryMax,
		LimitNOFILE: r.cfg.LimitNOFILE,

		ReadWritePaths: writable,
		BindReadOnly:   []string{steamDir},
		Credentials:    map[string]string{SteamScriptCredential: inv.ScriptPath},
		Timeout:        inv.Budget,
	}
	if sb.Timeout <= 0 {
		sb.Timeout = steamcmd.DefaultUpdateBudget
	}
	if err := sb.Validate(); err != nil {
		return Sandbox{}, err
	}
	return sb, nil
}

// SteamSandbox exposes the derivation for the apply pipeline's dry run.
//
// `gvm update <name> --diff` has to be able to print the unit it would start
// without starting it, and a second copy of these rules written for the printer
// is a second set of rules.
func (r *SteamRunner) SteamSandbox(inv steamcmd.Invocation) (Sandbox, error) {
	return r.sandbox(inv)
}

// protectHomeFor decides ProtectHome, always explicitly (D-15).
//
// ProtectHome=yes makes /home an empty tmpfs inside the unit. The adopted
// Project Zomboid server's world is /home/pzserver/Zomboid, and ReadWritePaths=
// "takes precedence" is documented but version-dependent across systemd
// 249/252/255 -- which is the exact silent failure D-15 exists to prevent: the
// unit starts, the download reports success and writes into a tmpfs that
// disappears.
//
// So: off when any path the unit must touch is under /home or /root, read-only
// otherwise. Never left to the default.
func protectHomeFor(paths []string) string {
	for _, p := range paths {
		if underAny(p, "/home", "/root") {
			return "no"
		}
	}
	return "read-only"
}

func underAny(p string, roots ...string) bool {
	p = filepath.ToSlash(p)
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// writeScript puts the runscript where systemd will read it from.
//
// The directory is created here because nothing else does: it is
// /var/lib/gvm/instances/<name>/steam, and a job that failed because a
// directory was missing would be reported as a steamcmd failure.
//
// Nothing is chowned. The file is read by systemd as root and exposed inside
// the unit as a credential (SteamScriptCredential), so handing it to the
// instance account would only widen a file that may carry a private-branch
// password -- and could not have helped in any case, since the account cannot
// traverse the root-owned 0750 ancestors ARCHITECTURE 5.3 requires. The mode is
// applied at creation rather than fixed afterwards, so there is no window in
// which the file exists 0644.
func (r *SteamRunner) writeScript(inv steamcmd.Invocation) error {
	if len(inv.Script) == 0 {
		return fmt.Errorf("%w: nothing to write to %s; a runscript steamcmd would read as "+
			"empty is a run that logs in and does nothing, and reports success",
			ErrSandbox, inv.ScriptPath)
	}
	mode := inv.ScriptMode
	if mode == 0 {
		mode = steamcmd.RunscriptMode
	}
	if mode.Perm()&0o077 != 0 {
		return fmt.Errorf("%w: runscript mode %04o lets another account read %s, which may "+
			"carry a private-branch password", ErrSandbox, mode.Perm(), inv.ScriptPath)
	}

	dir := filepath.ToSlash(filepath.Dir(inv.ScriptPath))
	if err := r.fsys.MkdirAll(dir, steamScriptDirMode); err != nil {
		return fmt.Errorf("jobs: create %s: %w", dir, err)
	}
	if err := r.fsys.WriteFile(inv.ScriptPath, inv.Script, mode.Perm()); err != nil {
		// What is on disk now is not what the driver rendered: the write
		// truncates, so a failure halfway leaves a runscript steamcmd would
		// read some commands out of, or none, and then exit 0 -- a silent
		// no-op reported as a successful update. Removing it makes the next
		// run fail loudly on the write instead of inheriting it.
		_ = r.fsys.Remove(inv.ScriptPath)
		return fmt.Errorf("jobs: write %s: %w", inv.ScriptPath, err)
	}
	return nil
}

// FetchSandbox is the content fetcher's unit (section 7.7, D-16).
//
// Everything a SteamCMD run cannot have, this one does: a syscall filter, a
// restricted address family set and a TasksMax, because the program inside it is
// gvm's own content-fetch subcommand rather than a Valve binary. What it does
// NOT have is any path in an instance tree -- a zip-slip in a modpack lands in a
// uid that owns nothing but a cache, and gvmd then links the blobs into place
// itself through a step that parses nothing.
//
// It lives here, unused by this step, because it is the other half of the
// decision this file records: the two sandboxes differ, and they differ for
// reasons that have to be written down next to each other or the next author
// will "harmonise" them.
func FetchSandbox(in FetchInput) (Sandbox, error) {
	if strings.TrimSpace(in.JobID) == "" {
		return Sandbox{}, fmt.Errorf("%w: a fetch unit needs a job id to name it", ErrSandbox)
	}
	if err := validUnitStem("gvm-fetch-" + in.JobID); err != nil {
		return Sandbox{}, err
	}
	sb := Sandbox{
		Unit:    "gvm-fetch-" + in.JobID,
		User:    in.User,
		Group:   in.Group,
		Slice:   instance.JobSliceName,
		Home:    in.Staging,
		WorkDir: in.Staging,

		ProtectSystem: "strict",
		// The fetch account has no home anybody cares about and never reads a
		// world, so this is the one sandbox that gets the strict answer.
		ProtectHome: "yes",

		PrivateTmp:       Yes(),
		NoNewPrivileges:  Yes(),
		RestrictSUIDSGID: Yes(),

		RestrictAddressFamilies: []string{"AF_INET", "AF_INET6"},
		// Two directives, in this order: an allow list, then a deny list.
		// Joined into one they would be a filter that means neither.
		SystemCallFilter: []string{"@system-service", "~@privileged @resources"},

		MemoryMax:     "1G",
		MemorySwapMax: "0",
		TasksMax:      64,

		ReadWritePaths: dedupe(append([]string{in.BlobDir, in.Staging}, in.ExtraWritable...)),
		BindReadOnly:   dedupe(in.BindReadOnly),
		Credentials:    in.Credentials,
		Timeout:        in.Timeout,
	}
	if sb.Timeout <= 0 {
		sb.Timeout = DefaultFetchTimeout
	}
	if err := sb.Validate(); err != nil {
		return Sandbox{}, err
	}
	return sb, nil
}

// DefaultFetchTimeout bounds a content fetch. Shorter than an install budget
// because a modpack is megabytes and a game is tens of gigabytes.
const DefaultFetchTimeout = 30 * time.Minute

// FetchInput is one content-fetch unit's description.
type FetchInput struct {
	JobID string
	// User and Group are gvm-fetch: a uid in no group that can reach
	// admin.sock and with no database grant (D-16).
	User, Group string
	// BlobDir is the shared content-addressed store, the only tree this unit
	// writes into.
	BlobDir string
	// Staging is blobs/tmp: HOME and WorkingDirectory both, so a downloader
	// that writes beside itself writes there.
	Staging       string
	ExtraWritable []string
	BindReadOnly  []string
	Credentials   map[string]string
	Timeout       time.Duration
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
