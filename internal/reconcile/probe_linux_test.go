//go:build linux

package reconcile

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestTheWriteProbeIsReal is the Linux-CI half of D-15.
//
// Every other test in this package proves that gvm BUILDS the right transient
// unit. This one proves the unit answers the question -- that a directory
// under /home is writable with ProtectHome=no and is NOT writable with
// ProtectHome=read-only, which is the exact failure the probe exists to catch
// and the one that disqualified a competing design.
//
// A probe that always said "fine" would pass every unit test in this file and
// be worth nothing, so the assertion that matters is the NEGATIVE one: the
// same paths, the same account, one directive changed, and the probe has to
// refuse.
func TestTheWriteProbeIsReal(t *testing.T) {
	requireTransientUnits(t)

	acct := probeAccount(t)
	home := filepath.Join("/home", "gvm-probe-test")
	mkdirOwned(t, home, acct)

	bin := installProbeBinary(t)

	base := Sandbox{ProtectSystem: "full", Paths: []string{home}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	t.Run("ProtectHome=no can write under /home", func(t *testing.T) {
		sb := base
		sb.ProtectHome = "no"
		results, err := ProbeWritable(ctx, ProbeSpec{
			Instance: "probe-ok", User: acct.name, Group: acct.group,
			Sandbox: sb, Binary: bin, Slice: "",
		})
		if err != nil {
			t.Fatalf("ProbeWritable: %v", err)
		}
		if err := ProbeError(sb, results); err != nil {
			t.Fatalf("a directory the account owns was reported unwritable with ProtectHome=no: %v", err)
		}
	})

	t.Run("ProtectHome=read-only cannot", func(t *testing.T) {
		sb := base
		// This is the template's default, and it is what an adopted Project
		// Zomboid instance would run under if the generator did not override
		// it. The server would start, boot, accept players and fail to save.
		sb.ProtectHome = "read-only"
		results, err := ProbeWritable(ctx, ProbeSpec{
			Instance: "probe-ro", User: acct.name, Group: acct.group,
			Sandbox: sb, Binary: bin,
		})
		if err != nil {
			t.Fatalf("ProbeWritable: %v", err)
		}
		perr := ProbeError(sb, results)
		if perr == nil {
			t.Fatal("ProtectHome=read-only was reported as permitting a write under /home. " +
				"The probe is not measuring anything, and D-15 is a comment.")
		}
		// And it has to name the directive, or an operator is left reading
		// the whole unit file to find out what refused them.
		if got := results[0].Blocked; got != "ProtectHome=read-only" {
			t.Errorf("Blocked = %q, want the directive responsible", got)
		}
	})

	// The probe removes what it wrote. A dot-file left in a world directory
	// gets blamed on the game.
	ents, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("the probe left %d file(s) in %s: %v", len(ents), home, ents)
	}
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
}

type account struct{ name, group string }

// probeAccount picks an unprivileged account that exists on a Debian box.
func probeAccount(t *testing.T) account {
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
	t.Skip("no unprivileged account to probe as")
	return account{}
}

// mkdirOwned creates a directory the probe account can write, and removes it
// afterwards.
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

// installProbeBinary builds gvm and puts it where a confined unit can exec it.
//
// Not t.TempDir(): the transient unit runs with PrivateTmp=yes and would see
// its own empty /tmp. /usr/local/bin is where the package installs gvm, so it
// is also where a ProtectSystem=full unit can read it from.
func installProbeBinary(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool to build the probe binary with")
	}
	dst := "/usr/local/bin/gvm-probe-test"
	cmd := exec.Command(goBin, "build", "-o", dst, "github.com/S95F/gameVPSmanager/cmd/gvm")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building gvm: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = os.Remove(dst) })
	return dst
}
