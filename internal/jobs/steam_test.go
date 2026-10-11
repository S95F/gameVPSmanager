package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/provider/steamcmd"
)

// The runner is the steamcmd driver's Runner seam. If this stops compiling, the
// write path has no sandbox and the driver refuses every install with
// ErrNoRunner -- at runtime, in a nightly job, rather than here.
var _ steamcmd.Runner = (*SteamRunner)(nil)

// fakeScriptFS records every effect in order, so the ORDER is assertable: a
// script chowned before it is written, or a directory handed over after the
// write, are both "it worked on my box" bugs.
type fakeScriptFS struct {
	calls []string
	files map[string]fakeFile
	// failWrite names a path whose write fails, which is how "the runscript on
	// disk is not the runscript gvm rendered" is reached without root.
	failWrite string
	failMkdir bool
}

type fakeFile struct {
	data     []byte
	mode     fs.FileMode
	uid, gid int
	owned    bool
}

func newFakeScriptFS() *fakeScriptFS {
	return &fakeScriptFS{files: map[string]fakeFile{}}
}

func (f *fakeScriptFS) MkdirAll(dir string, perm fs.FileMode) error {
	f.calls = append(f.calls, fmt.Sprintf("mkdir %s %04o", dir, perm))
	if f.failMkdir {
		return errors.New("read-only file system")
	}
	return nil
}

func (f *fakeScriptFS) WriteFile(p string, data []byte, perm fs.FileMode) error {
	f.calls = append(f.calls, fmt.Sprintf("write %s %04o", p, perm))
	if p == f.failWrite {
		// A partial write: O_TRUNC has already emptied the file, so what is
		// left on disk is not what the driver rendered.
		f.files[p] = fakeFile{data: data[:0], mode: perm}
		return errors.New("no space left on device")
	}
	f.files[p] = fakeFile{data: data, mode: perm}
	return nil
}

func (f *fakeScriptFS) Chown(p string, uid, gid int) error {
	f.calls = append(f.calls, fmt.Sprintf("chown %s %d:%d", p, uid, gid))
	if fl, ok := f.files[p]; ok {
		fl.uid, fl.gid, fl.owned = uid, gid, true
		f.files[p] = fl
	}
	return nil
}

func (f *fakeScriptFS) Remove(p string) error {
	f.calls = append(f.calls, "remove "+p)
	delete(f.files, p)
	return nil
}

func testIdent(t *testing.T) provider.UnixIdent {
	t.Helper()
	id, err := provider.NewUnixIdent("gvm-pz-main", "gvm-pz-main", 998, 998)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func updateInvocation(t *testing.T) steamcmd.Invocation {
	t.Helper()
	return steamcmd.Invocation{
		Purpose:    "app_update",
		Binary:     "/opt/gvm/steamcmd/steamcmd.sh",
		Args:       []string{"+runscript", "/var/lib/gvm/instances/pz-main/steam/update.txt"},
		Script:     []byte("@ShutdownOnFailedCommand 1\nquit\n"),
		ScriptPath: "/var/lib/gvm/instances/pz-main/steam/update.txt",
		ScriptMode: steamcmd.RunscriptMode,
		Env: map[string]string{
			"HOME": "/var/lib/gvm/cache/steam/homes/pz-main",
			"LANG": "en_US.UTF-8",
		},
		WorkDir: "/srv/gvm/pz-main/install",
		RunAs:   testIdent(t),
		Budget:  45 * time.Minute,
	}
}

func newTestRunner(t *testing.T, cfg SteamConfig, opts ...SteamOption) *SteamRunner {
	t.Helper()
	if cfg.Instance == "" {
		cfg.Instance = "pz-main"
	}
	if cfg.Install == "" {
		cfg.Install = "/srv/gvm/pz-main/install"
	}
	r, err := NewSteamRunner(cfg, opts...)
	if err != nil {
		t.Fatalf("NewSteamRunner: %v", err)
	}
	return r
}

func TestSteamSandboxDerivation(t *testing.T) {
	r := newTestRunner(t, SteamConfig{})
	sb, err := r.SteamSandbox(updateInvocation(t))
	if err != nil {
		t.Fatalf("SteamSandbox: %v", err)
	}

	if sb.Unit != "gvm-steam-pz-main" {
		t.Errorf("Unit = %q, want the name section 7.7 documents, because it is what an "+
			"operator types during a 65 GB download", sb.Unit)
	}
	if sb.User != "gvm-pz-main" || sb.Group != "gvm-pz-main" {
		t.Errorf("the unit runs as %s:%s, not the instance account", sb.User, sb.Group)
	}
	if sb.Timeout != 45*time.Minute {
		t.Errorf("Timeout = %v, want the invocation's budget", sb.Timeout)
	}

	// HOME is lifted out of Env into the Home field, because Validate refuses
	// it in both and the whole of D-30 is that this one variable is not
	// allowed to be ambiguous.
	if sb.Home != "/var/lib/gvm/cache/steam/homes/pz-main" {
		t.Errorf("Home = %q", sb.Home)
	}
	if _, dup := sb.Env["HOME"]; dup {
		t.Error("HOME is in Env as well as Home; one of the two would silently win")
	}
	if sb.Env["LANG"] != "en_US.UTF-8" {
		t.Error("LANG did not survive; steamcmd misbehaves without a UTF-8 locale")
	}

	// The steamcmd installation is READ-ONLY. It is shared by every instance
	// on the box, so an instance that could write it could plant a binary
	// every other instance then executes as its own user.
	if !contains(sb.BindReadOnly, "/opt/gvm/steamcmd") {
		t.Errorf("BindReadOnly = %v, want the steamcmd installation", sb.BindReadOnly)
	}
	for _, p := range sb.ReadWritePaths {
		if strings.HasPrefix(p, "/opt/gvm/steamcmd") {
			t.Errorf("ReadWritePaths includes the shared steamcmd installation (%q): one "+
				"instance's self-update would be code every other instance executes", p)
		}
	}

	for _, want := range []string{
		"/srv/gvm/pz-main/install",               // the install tree
		"/var/lib/gvm/cache/steam/homes/pz-main", // the per-instance steam HOME
	} {
		if !contains(sb.ReadWritePaths, want) {
			t.Errorf("ReadWritePaths = %v, missing %s", sb.ReadWritePaths, want)
		}
	}
	// And the runscript's directory is NOT writable, which is the stronger
	// statement: the unit never opens it, because the script arrives as a
	// credential (see SteamScriptCredential), and ARCHITECTURE 5.3 keeps
	// /var/lib/gvm/instances/<name> root-owned 0750 so a compromised mod
	// cannot rewrite the content lock.
	if contains(sb.ReadWritePaths, "/var/lib/gvm/instances/pz-main/steam") {
		t.Errorf("ReadWritePaths = %v; the state tree is writable by the unit", sb.ReadWritePaths)
	}

	// Nothing under /home is involved, so the strict answer is available and
	// has to be taken. D-15: the directive is always explicit.
	if sb.ProtectHome != "read-only" {
		t.Errorf("ProtectHome = %q, want read-only when no path lives under /home", sb.ProtectHome)
	}
	if sb.ProtectSystem != "strict" {
		t.Errorf("ProtectSystem = %q", sb.ProtectSystem)
	}
}

// TestProtectHomeIsOffForTheAdoptedInstance is D-15 inside a job unit.
//
// The adopted Project Zomboid server's install is /opt/pzserver and its world
// is /home/pzserver/Zomboid. With ProtectHome=yes the unit starts, the download
// reports success, and the files land in a tmpfs that disappears -- and
// "ReadWritePaths takes precedence" is documented but version-dependent across
// systemd 249/252/255, which is exactly why this is computed rather than
// assumed.
func TestProtectHomeIsOffForTheAdoptedInstance(t *testing.T) {
	for name, cfg := range map[string]SteamConfig{
		"install under /home": {Instance: "pz-main", Install: "/home/pzserver"},
		"data under /home": {
			Instance: "pz-main", Install: "/opt/pzserver",
			ExtraWritable: []string{"/home/pzserver/Zomboid"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newTestRunner(t, cfg)
			inv := updateInvocation(t)
			inv.WorkDir = cfg.Install
			sb, err := r.SteamSandbox(inv)
			if err != nil {
				t.Fatal(err)
			}
			if sb.ProtectHome != "no" {
				t.Fatalf("ProtectHome = %q with a path under /home; the install would write "+
					"into a tmpfs and report success (D-15)", sb.ProtectHome)
			}
		})
	}
}

func TestSteamSandboxRefusals(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*steamcmd.Invocation)
		want string
	}{
		{"unknown purpose", func(i *steamcmd.Invocation) { i.Purpose = "app_delete" },
			"unknown steamcmd purpose"},
		{"no account", func(i *steamcmd.Invocation) { i.RunAs = provider.UnixIdent{} },
			"an unset uid is root"},
		{"no HOME", func(i *steamcmd.Invocation) { delete(i.Env, "HOME") }, "sets no HOME"},
		{"empty HOME", func(i *steamcmd.Invocation) { i.Env["HOME"] = " " }, "sets no HOME"},
		{"relative binary", func(i *steamcmd.Invocation) { i.Binary = "steamcmd.sh" },
			"not an absolute path"},
		{"relative script", func(i *steamcmd.Invocation) { i.ScriptPath = "steam/update.txt" },
			"not an absolute path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRunner(t, SteamConfig{})
			inv := updateInvocation(t)
			tc.mut(&inv)
			_, err := r.SteamSandbox(inv)
			if err == nil {
				t.Fatal("the sandbox was built anyway")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not explain the refusal\n got: %v\nwant substring: %q",
					err, tc.want)
			}
		})
	}
}

// TestTheCheckRunGetsItsOwnUnitName: the check and the update it decides on run
// seconds apart inside one job, and a transient unit that has not finished
// being garbage-collected makes the next start fail with "unit already exists"
// -- a self-inflicted outage three layers from its symptom.
func TestTheCheckRunGetsItsOwnUnitName(t *testing.T) {
	r := newTestRunner(t, SteamConfig{})
	inv := updateInvocation(t)
	inv.Purpose = "app_info"
	inv.ScriptPath = "/var/lib/gvm/instances/pz-main/steam/check.txt"
	check, err := r.SteamSandbox(inv)
	if err != nil {
		t.Fatal(err)
	}
	update, err := r.SteamSandbox(updateInvocation(t))
	if err != nil {
		t.Fatal(err)
	}
	if check.Unit == update.Unit {
		t.Fatalf("both runs are called %q", check.Unit)
	}
	if !strings.Contains(check.Unit, "pz-main") {
		t.Errorf("the check unit %q does not name the instance", check.Unit)
	}
}

func TestWriteScriptOrderAndOwnership(t *testing.T) {
	fsys := newFakeScriptFS()
	r := newTestRunner(t, SteamConfig{}, WithScriptFS(fsys),
		WithSandboxRunner(func(context.Context, Sandbox, []string, io.Writer) error { return nil }))

	inv := updateInvocation(t)
	if _, err := r.Run(context.Background(), inv); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{
		// The directory first: nothing else creates
		// /var/lib/gvm/instances/<name>/steam, and a job that failed because it
		// was missing would be reported as a steamcmd failure.
		"mkdir /var/lib/gvm/instances/pz-main/steam 0750",
		// Then the script, at its final mode. The mode is applied at creation
		// rather than fixed afterwards, so there is no window in which the file
		// exists 0644 carrying a private-branch password.
		"write /var/lib/gvm/instances/pz-main/steam/update.txt 0600",
	}
	if strings.Join(fsys.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("filesystem effects\n got:\n  %s\nwant:\n  %s",
			strings.Join(fsys.calls, "\n  "), strings.Join(want, "\n  "))
	}

	f := fsys.files[inv.ScriptPath]
	if !bytes.Equal(f.data, inv.Script) {
		t.Errorf("the script on disk is not the script the driver rendered")
	}
	if f.mode != 0o600 {
		t.Errorf("script is mode %04o; it may carry a private-branch password", f.mode)
	}
	// NOT handed to the instance account, and that is the point rather than an
	// omission: systemd reads this file as root and exposes it inside the unit
	// as a credential, so a chown here would widen it for no gain -- and could
	// never have helped, since the account cannot traverse the root-owned 0750
	// ancestors ARCHITECTURE 5.3 requires.
	if f.owned {
		t.Errorf("the script was chowned to %d:%d", f.uid, f.gid)
	}
}

func TestWriteScriptRefusals(t *testing.T) {
	t.Run("an empty script", func(t *testing.T) {
		fsys := newFakeScriptFS()
		r := newTestRunner(t, SteamConfig{}, WithScriptFS(fsys),
			WithSandboxRunner(func(context.Context, Sandbox, []string, io.Writer) error {
				t.Fatal("steamcmd was launched with nothing to run")
				return nil
			}))
		inv := updateInvocation(t)
		inv.Script = nil
		if _, err := r.Run(context.Background(), inv); err == nil {
			t.Fatal("an empty runscript was accepted; steamcmd would log in, do nothing and " +
				"exit 0, which the pipeline would read as a successful update")
		}
	})

	t.Run("a world-readable mode", func(t *testing.T) {
		fsys := newFakeScriptFS()
		r := newTestRunner(t, SteamConfig{}, WithScriptFS(fsys))
		inv := updateInvocation(t)
		inv.ScriptMode = 0o644
		_, err := r.Run(context.Background(), inv)
		if err == nil || !strings.Contains(err.Error(), "another account") {
			t.Fatalf("err = %v, want a refusal naming the exposure", err)
		}
	})

	t.Run("a write the host refuses", func(t *testing.T) {
		fsys := newFakeScriptFS()
		fsys.failWrite = "/var/lib/gvm/instances/pz-main/steam/update.txt"
		launched := false
		r := newTestRunner(t, SteamConfig{}, WithScriptFS(fsys),
			WithSandboxRunner(func(context.Context, Sandbox, []string, io.Writer) error {
				launched = true
				return nil
			}))
		if _, err := r.Run(context.Background(), updateInvocation(t)); err == nil {
			t.Fatal("a half-written script was accepted")
		}
		if launched {
			t.Error("steamcmd was launched against a script that is not the one gvm rendered; a " +
				"truncated runscript is a run that reads some commands, or none, and exits 0 -- " +
				"a silent no-op reported as a successful update")
		}
		// And the partial file is removed, so the next run fails loudly on the
		// write rather than inheriting it.
		if !contains(fsys.calls, "remove /var/lib/gvm/instances/pz-main/steam/update.txt") {
			t.Errorf("the unusable script was left behind: %v", fsys.calls)
		}
	})

	t.Run("a directory that cannot be created", func(t *testing.T) {
		fsys := newFakeScriptFS()
		fsys.failMkdir = true
		r := newTestRunner(t, SteamConfig{}, WithScriptFS(fsys))
		if _, err := r.Run(context.Background(), updateInvocation(t)); err == nil {
			t.Fatal("Run proceeded with no directory to write the script into")
		}
	})
}

// TestRunTranslatesAnExitRatherThanAnError is section 13.5's "the exit code is
// not evidence", at the seam.
//
// A steamcmd that ran and exited non-zero is an Exit, not an error: the driver's
// classifier decides what happened by reading the output. A sandbox that FAILED
// is an error, because there is no output to classify. Collapsing the two would
// make a content-server blip indistinguishable from a missing systemd.
func TestRunTranslatesAnExitRatherThanAnError(t *testing.T) {
	cases := []struct {
		name    string
		runErr  error
		want    steamcmd.Exit
		wantErr bool
	}{
		{"clean", nil, steamcmd.Exit{}, false},
		{"non-zero", &ExitError{Unit: "u.service", Code: 7}, steamcmd.Exit{Code: 7}, false},
		{"signalled", &ExitError{Unit: "u.service", Code: -1, Signaled: true},
			steamcmd.Exit{Code: -1, Signaled: true}, false},
		{"over budget", &ExitError{Unit: "u.service", Code: 1, Timeout: true},
			steamcmd.Exit{Code: 1, Timeout: true}, false},
		{"wrapped exit", fmt.Errorf("retrying: %w", &ExitError{Code: 42}),
			steamcmd.Exit{Code: 42}, false},
		{"the sandbox itself failed", errors.New("systemd-run: unit already exists"),
			steamcmd.Exit{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRunner(t, SteamConfig{}, WithScriptFS(newFakeScriptFS()),
				WithSandboxRunner(func(context.Context, Sandbox, []string, io.Writer) error {
					return tc.runErr
				}))
			got, err := r.Run(context.Background(), updateInvocation(t))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("Exit = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRunPassesTheCommandAndStreamsOutput(t *testing.T) {
	var gotCmd []string
	var gotSandbox Sandbox
	out := &bytes.Buffer{}
	r := newTestRunner(t, SteamConfig{}, WithScriptFS(newFakeScriptFS()),
		WithSandboxRunner(func(_ context.Context, s Sandbox, cmd []string, w io.Writer) error {
			gotCmd, gotSandbox = cmd, s
			// The driver's classifier is a streaming writer; the sandbox has
			// to hand it the bytes rather than a buffer at the end.
			_, _ = io.WriteString(w, "Success! App '380870' fully installed.\n")
			return nil
		}))

	inv := updateInvocation(t)
	inv.Stdout = out
	if _, err := r.Run(context.Background(), inv); err != nil {
		t.Fatal(err)
	}

	// The binary and the verb are the driver's, verbatim. The script path is
	// the one place the sandbox substitutes: the driver names the file it wants
	// run, and only this layer knows the unit name the credential is exposed
	// under -- and that the host path is one the unit's uid cannot traverse to.
	want := []string{
		"/opt/gvm/steamcmd/steamcmd.sh", "+runscript",
		"/run/credentials/gvm-steam-pz-main.service/" + SteamScriptCredential,
	}
	if strings.Join(gotCmd, " ") != strings.Join(want, " ") {
		t.Errorf("command = %v, want %v", gotCmd, want)
	}
	if !strings.Contains(out.String(), "fully installed") {
		t.Errorf("the classifier received %q", out.String())
	}
	// And the sandbox that was handed over is one Args will accept.
	if _, err := gotSandbox.Args(gotCmd); err != nil {
		t.Errorf("the derived sandbox does not render: %v", err)
	}
}

func TestNewSteamRunnerRefusesABadInstance(t *testing.T) {
	for name, cfg := range map[string]SteamConfig{
		"no instance":      {Install: "/srv/gvm/x/install"},
		"underscore":       {Instance: "pz_main", Install: "/srv/gvm/x/install"},
		"relative install": {Instance: "pz-main", Install: "srv/gvm/x"},
		"no install":       {Instance: "pz-main"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSteamRunner(cfg); err == nil {
				t.Fatal("NewSteamRunner accepted it")
			}
		})
	}
}

func TestSteamConfigDefaults(t *testing.T) {
	r := newTestRunner(t, SteamConfig{})
	sb, err := r.SteamSandbox(updateInvocation(t))
	if err != nil {
		t.Fatal(err)
	}
	if sb.MemoryMax != DefaultSteamMemoryMax || sb.LimitNOFILE != DefaultSteamLimitNOFILE {
		t.Errorf("MemoryMax=%q LimitNOFILE=%d; steamcmd opens a descriptor per depot chunk in "+
			"flight, so an unset limit is the default 1024 and a large download runs out",
			sb.MemoryMax, sb.LimitNOFILE)
	}
	// A zero budget would be no RuntimeMaxSec, which Validate refuses, so the
	// driver's own default has to be picked up here.
	inv := updateInvocation(t)
	inv.Budget = 0
	sb, err = r.SteamSandbox(inv)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Timeout != steamcmd.DefaultUpdateBudget {
		t.Errorf("Timeout = %v, want the driver's default budget", sb.Timeout)
	}
}

// The runscript reaches steamcmd through LoadCredential=, not through the
// instance state tree.
//
// The DAC arithmetic that forces this: the script lives at
// /var/lib/gvm/instances/<name>/steam/update.txt, and every ancestor of it is
// root-owned 0750 -- /var/lib/gvm and /var/lib/gvm/instances from
// packaging/tmpfiles/gvm.conf, and the two per-instance levels from hostops'
// DirState and DirSteamScripts, neither of which carries a group or
// OwnedByInstance. ARCHITECTURE 5.3 requires that on purpose, so a compromised
// mod cannot rewrite the content lock, and ARCHITECTURE:110 has doctor assert
// that gvm-<name> is in no group but its own. So the instance account cannot
// TRAVERSE to the file, whatever the file's own owner and mode are, and
// ReadWritePaths= bind-mounts without altering DAC.
//
// systemd reads the credential as root and exposes it inside the unit at 0400
// owned by the unit's uid, so no ancestor mode has to change. A steamcmd that
// cannot open its runscript finds no commands and exits 0, which is the silent
// no-op reported as a successful update that this whole package exists to
// prevent.
func TestTheRunscriptIsDeliveredAsACredential(t *testing.T) {
	fsys := newFakeScriptFS()
	var argv []string
	r := newTestRunner(t, SteamConfig{}, WithScriptFS(fsys),
		WithSandboxRunner(func(_ context.Context, _ Sandbox, cmd []string, _ io.Writer) error {
			argv = cmd
			return nil
		}))

	inv := updateInvocation(t)
	if _, err := r.Run(context.Background(), inv); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sb, err := r.SteamSandbox(inv)
	if err != nil {
		t.Fatal(err)
	}

	if got := sb.Credentials[SteamScriptCredential]; got != inv.ScriptPath {
		t.Errorf("Credentials[%q] = %q, want the script's host path %q",
			SteamScriptCredential, got, inv.ScriptPath)
	}
	want := "/run/credentials/" + sb.UnitName() + "/" + SteamScriptCredential
	if !contains(argv, want) {
		t.Errorf("argv = %v, want +runscript %s", argv, want)
	}
	if contains(argv, inv.ScriptPath) {
		t.Errorf("argv still names the host path %q, which the unit's uid cannot traverse to",
			inv.ScriptPath)
	}
	// Nothing in the argv may address the state tree at all: it is 0750
	// root:root by ARCHITECTURE 5.3 and the unit runs as the instance account.
	for _, a := range argv {
		if strings.HasPrefix(a, "/var/lib/gvm/instances/") {
			t.Errorf("argv[%q] is under the root-owned 0750 state tree", a)
		}
	}
	// And the unit no longer opens that directory, so it must not be writable
	// either: ReadWritePaths is the whole of what this unit may write.
	for _, p := range sb.ReadWritePaths {
		if strings.HasPrefix(p, "/var/lib/gvm/instances/") {
			t.Errorf("ReadWritePaths still includes %q, which nothing in the unit reads or "+
				"writes now that the script arrives as a credential", p)
		}
	}
	// The directive actually reaches systemd-run.
	args, err := sb.Args([]string{inv.Binary, "+runscript", want})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(args, "--property=LoadCredential="+SteamScriptCredential+":"+inv.ScriptPath) {
		t.Errorf("the rendered unit carries no LoadCredential for the runscript: %v", args)
	}
}
