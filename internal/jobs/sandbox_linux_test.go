//go:build linux

package jobs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/provider/steamcmd"
)

// TestTheSandboxIsReal is the Linux-CI half of D-16.
//
// Every other test in this package proves gvm BUILDS the right transient unit.
// This one proves the unit IS one: that `systemctl show` reports a .service and
// not a .scope, that --uid landed as User=, that RuntimeMaxSec is a number
// rather than "infinity", and that every hardening directive gvm passed is
// carried by the running unit.
//
// A host whose systemd silently ignored a property accepts the command and
// confines nothing, and the symptom -- SteamCMD running with more reach than
// the game it installs -- looks exactly like success from gvm's side. So the
// assertion has to read the properties back off the live unit; constructing the
// command correctly is worth nothing on its own.
func TestTheSandboxIsReal(t *testing.T) {
	requireTransientUnits(t)
	acct := sandboxAccount(t)
	sleep := requireTool(t, "/bin/sleep", "/usr/bin/sleep")

	// Not t.TempDir(): the unit runs with PrivateTmp=yes and would see its own
	// empty /tmp, so a directory under /tmp is invisible to the thing being
	// tested. /home also exercises ProtectHome=no, which is the directive D-15
	// exists for.
	home := "/home/gvm-jobs-test"
	mkdirOwned(t, home, acct)

	sb := Sandbox{
		Unit:             "gvm-jobs-test",
		User:             acct.name,
		Group:            acct.group,
		Home:             home,
		WorkDir:          home,
		Env:              map[string]string{"LANG": "en_US.UTF-8"},
		ProtectSystem:    "full",
		ProtectHome:      "no",
		PrivateTmp:       Yes(),
		NoNewPrivileges:  Yes(),
		RestrictSUIDSGID: Yes(),
		MemoryMax:        "256M",
		LimitNOFILE:      4096,
		TasksMax:         32,
		ReadWritePaths:   []string{home},
		Timeout:          60 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// The unit has to be inspected WHILE it runs: Args passes --collect, so
	// there is nothing left to ask once it goes inactive. That is the right
	// trade -- the unit name is fixed per instance, and a failed one left
	// loaded would make every subsequent nightly fail with "unit already
	// exists" -- but it means the verification is concurrent with the run.
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- RunSandboxed(ctx, sb, []string{sleep, "6"}, &out) }()

	got := waitForUnit(t, ctx, sb)
	if ms := sb.CheckShow(got); len(ms) != 0 {
		for _, m := range ms {
			t.Errorf("the unit does not carry what it was given: %s", m)
		}
	}

	// Said separately and in the plainest possible terms, because this one
	// line is the whole of D-16 and a future refactor that reached for
	// --scope would still pass every argv test in this package.
	if id := got["Id"]; !strings.HasSuffix(id, ".service") {
		t.Errorf("Id = %q; third-party code runs in a transient SERVICE. A scope inherits the "+
			"caller's context and carries no confinement at all", id)
	}
	if got["User"] != acct.name {
		t.Errorf("User = %q, want %q: --uid is the enforced version of the bash suite's as_pz",
			got["User"], acct.name)
	}
	switch rt := got["RuntimeMaxUSec"]; rt {
	case "", "infinity":
		t.Errorf("RuntimeMaxUSec = %q; a wedged download would hold the nightly job open "+
			"forever, and RuntimeMaxSec is the kernel-enforced version of update.sh's "+
			"`timeout 1800`", rt)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunSandboxed: %v\noutput: %s", err, out.String())
		}
	case <-ctx.Done():
		t.Fatal("RunSandboxed did not return")
	}
}

// TestRuntimeMaxSecActuallyKills is the other half: the budget is not a label.
//
// It is the one property whose failure is silent in the worst way -- a download
// that wedged on a content server holds the instance lock, the nightly job and
// the maintenance window, and nothing reports anything until somebody notices
// in the morning.
func TestRuntimeMaxSecActuallyKills(t *testing.T) {
	requireTransientUnits(t)
	acct := sandboxAccount(t)
	sleep := requireTool(t, "/bin/sleep", "/usr/bin/sleep")

	home := "/home/gvm-jobs-budget"
	mkdirOwned(t, home, acct)

	sb := Sandbox{
		Unit: "gvm-jobs-budget", User: acct.name, Group: acct.group,
		Home: home, WorkDir: home,
		ProtectSystem: "full", ProtectHome: "no",
		PrivateTmp: Yes(), NoNewPrivileges: Yes(), RestrictSUIDSGID: Yes(),
		ReadWritePaths: []string{home},
		Timeout:        2 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	started := time.Now()
	err := RunSandboxed(ctx, sb, []string{sleep, "300"}, nil)
	elapsed := time.Since(started)

	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v (%T), want an *ExitError: the child ran, so the failure is the "+
			"child's and not the sandbox's", err, err)
	}
	if !ee.Timeout {
		t.Errorf("ExitError.Timeout = false after %v against a 2s budget", elapsed)
	}
	if elapsed > 30*time.Second {
		t.Errorf("the unit took %v to be killed on a 2s RuntimeMaxSec", elapsed)
	}
}

// TestANonZeroExitIsAnExitAndNotASandboxFailure: section 13.5's "the exit code
// is not evidence" depends on telling these two apart. A steamcmd that ran and
// exited 1 gets its output classified; a sandbox that could not start gets
// retried or reported, and there is nothing to classify.
func TestANonZeroExitIsAnExitAndNotASandboxFailure(t *testing.T) {
	requireTransientUnits(t)
	acct := sandboxAccount(t)
	falseBin := requireTool(t, "/bin/false", "/usr/bin/false")

	home := "/home/gvm-jobs-exit"
	mkdirOwned(t, home, acct)

	sb := Sandbox{
		Unit: "gvm-jobs-exit", User: acct.name, Group: acct.group,
		Home: home, WorkDir: home,
		ProtectSystem: "full", ProtectHome: "no",
		PrivateTmp: Yes(), NoNewPrivileges: Yes(), RestrictSUIDSGID: Yes(),
		ReadWritePaths: []string{home},
		Timeout:        30 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := RunSandboxed(ctx, sb, []string{falseBin}, nil)
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v (%T), want an *ExitError", err, err)
	}
	if ee.Timeout {
		t.Error("a prompt non-zero exit was reported as a budget overrun")
	}
	if ee.Unit != sb.UnitName() {
		t.Errorf("ExitError.Unit = %q, want %q", ee.Unit, sb.UnitName())
	}
}

// TestTheRunscriptLandsOwnedByTheInstanceUser exercises the real chown.
//
// The script carries steamcmd's whole instruction set and may carry a
// private-branch password, and the account that has to read it is the one
// steamcmd drops to. Root-owned 0600 is the failure that looks like success:
// steamcmd starts, finds no commands, and exits 0.
func TestTheRunscriptLandsOwnedByTheInstanceUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root")
	}
	acct := sandboxAccount(t)
	u, err := user.Lookup(acct.name)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	g, err := user.LookupGroup(acct.group)
	if err != nil {
		t.Fatal(err)
	}
	gid, _ := strconv.Atoi(g.Gid)

	ident, err := provider.NewUnixIdent(acct.name, acct.group, uid, gid)
	if err != nil {
		t.Skipf("the probe account is not usable as an instance identity: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "steam")
	script := filepath.Join(dir, "update.txt")
	r, err := NewSteamRunner(SteamConfig{Instance: "pz-main", Install: "/srv/gvm/pz-main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.writeScript(steamcmd.Invocation{
		Purpose:    "app_update",
		Binary:     "/opt/gvm/steamcmd/steamcmd.sh",
		Script:     []byte("@ShutdownOnFailedCommand 1\nquit\n"),
		ScriptPath: script,
		ScriptMode: steamcmd.RunscriptMode,
		RunAs:      ident,
	}); err != nil {
		t.Fatalf("writeScript: %v", err)
	}

	fi, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %04o, want 0600", fi.Mode().Perm())
	}
	if ownerOf(t, script) != uid {
		t.Errorf("the runscript is owned by uid %d, not the instance user %d; steamcmd would "+
			"read no commands and exit 0", ownerOf(t, script), uid)
	}
	if ownerOf(t, dir) != uid {
		t.Errorf("the script directory is owned by uid %d, not %d", ownerOf(t, dir), uid)
	}
}

// waitForUnit polls `systemctl show` until the unit exists.
//
// Polling rather than sleeping a fixed interval: systemd-run's start job
// completes at its own pace under load, and a fixed wait is a test that fails
// on a busy CI runner for a reason that has nothing to do with what it checks.
func waitForUnit(t *testing.T, ctx context.Context, sb Sandbox) map[string]string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last map[string]string
	for time.Now().Before(deadline) {
		got, err := ShowUnit(ctx, sb.UnitName(), sb.ShowProperties())
		if err == nil && got["Id"] != "" {
			return got
		}
		last = got
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the unit %s never appeared; last answer: %v", sb.UnitName(), last)
	return nil
}

// requireTransientUnits skips unless this box can actually start one.
//
// The skips are specific rather than one blanket check, because a CI job that
// silently stopped running this test would be worse than one that fails: the
// message says which precondition disappeared.
func requireTransientUnits(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("systemd-run --uid needs root")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("no systemd on this host (/run/systemd/system is absent)")
	}
	if _, err := findTool(systemdRunPaths); err != nil {
		t.Skip("systemd-run is not installed")
	}
	if _, err := findTool(systemctlPaths); err != nil {
		t.Skip("systemctl is not installed, so nothing could verify the unit")
	}
}

func requireTool(t *testing.T, candidates ...string) string {
	t.Helper()
	p, err := findTool(candidates)
	if err != nil {
		t.Skipf("none of %v is present", candidates)
	}
	return p
}

type account struct{ name, group string }

// sandboxAccount picks an unprivileged account that exists on a Debian box.
func sandboxAccount(t *testing.T) account {
	t.Helper()
	for _, c := range []account{{"nobody", "nogroup"}, {"nobody", "nobody"}, {"daemon", "daemon"}} {
		if _, err := user.Lookup(c.name); err != nil {
			continue
		}
		if _, err := user.LookupGroup(c.group); err != nil {
			continue
		}
		return c
	}
	t.Skip("no unprivileged account to run a sandbox as")
	return account{}
}

// ownerOf reads a path's owning uid. There is no cross-platform way to ask, so
// it lives in the Linux-only test file beside the only assertion that needs it.
func ownerOf(t *testing.T, path string) int {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no stat information for %s", path)
	}
	return int(st.Uid)
}

func mkdirOwned(t *testing.T, dir string, a account) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	u, err := user.Lookup(a.name)
	if err != nil {
		t.Fatal(err)
	}
	g, err := user.LookupGroup(a.group)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(g.Gid)
	if err := os.Chown(dir, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
}
