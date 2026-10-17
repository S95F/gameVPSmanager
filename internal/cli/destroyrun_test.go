package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// destroyable seeds an instance with real install and data trees, an archive of
// them that verifies, and the two path seams pointed at the temp tree.
//
// Both seams exist because two of an instance's control-plane paths are used as
// REAL filesystem paths rather than as keys into the fake FS: the destroy jail
// runs filepath.IsAbs over SecretsDir, and the spot verify os.MkdirAll's
// SnapDir. Left at their Linux constants the first refuses on Windows and the
// second would write to the developer's C: drive.
func destroyable(t *testing.T, h *testHost, name string) (install, data, archivePath string) {
	t.Helper()
	root := t.TempDir()
	install = filepath.Join(root, "install")
	data = filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(data, "worlds_local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{
		filepath.Join(install, "server.x86_64"):        "ELF",
		filepath.Join(data, "adminlist.txt"):           "steve\n",
		filepath.Join(data, "worlds_local", "Ded.db"):  "world bytes",
		filepath.Join(data, "worlds_local", "Ded.fwl"): "meta",
	} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	h.HostEnv.Paths = func(n string) instance.Paths {
		p := instance.PathsFor(n)
		p.SecretsDir = filepath.Join(root, "secrets", n)
		p.SnapDir = filepath.Join(root, "snap", n)
		return p
	}

	backups := filepath.Join(root, "backups")
	if err := os.MkdirAll(backups, 0o755); err != nil {
		t.Fatal(err)
	}
	ref, err := archive.Create(context.Background(), archive.Options{
		Dir: backups, Instance: name, Class: archive.ClassRoutine, Reason: "pre-destroy",
		Now: func() time.Time { return testTime },
		Sources: []archive.Source{
			{Prefix: "install/", Base: install},
			{Prefix: "data/", Base: data},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return install, data, ref.Path
}

// TestDestroyRefusesWhenTheRestoreWouldNotFit is section 13.6 step 7's space
// refusal, wired.
//
// runDestroy builds the preflight, prints its "needed to restore" line, and
// then has to CONSULT it. Printing a refusal and proceeding anyway destroys the
// operator's ability to put the world back on that box -- which is the exact
// property the refusal's own sentinel says it protects, and the whole reason
// --archive has no waiver.
//
// It is invisible without the Statfs seam: platform.Statfs is Linux-only, so on
// the development box every volume is reported unchecked and Fits() is true by
// definition.
func TestDestroyRefusesWhenTheRestoreWouldNotFit(t *testing.T) {
	h := newTestHost(t)
	install, data, arc := destroyable(t, h, "valheim-1")
	seedInstance(t, h, "valheim-1", "valheim", "--install", install, "--data", data,
		"--var", "public_name=Test")

	// A volume with a few bytes free: the destroy releases the world, and the
	// restore still would not fit.
	h.HostEnv.Statfs = func(string) (platform.DiskUsage, error) {
		return platform.DiskUsage{Total: 1 << 20, Free: 8, Avail: 8}, nil
	}

	code, out, errb := runWith(t, h, "valheim-1\ny\n", "destroy", "valheim-1", "--archive", arc)
	if code == 0 {
		t.Fatalf("destroy succeeded with no room to restore afterwards\nstdout:\n%s", out)
	}
	if !strings.Contains(errb, "short") && !strings.Contains(errb, "restor") {
		t.Errorf("the refusal does not read as a space refusal:\n%s", errb)
	}
	// Nothing went.
	for _, p := range []string{
		filepath.Join(install, "server.x86_64"),
		filepath.Join(data, "worlds_local", "Ded.db"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed by a destroy that was refused: %v", p, err)
		}
	}
	mustReadInstance(t, h, "valheim-1")
}

// TestDestroyDisablesTheUnitAndEveryJobTimer is what a destroyed instance must
// leave behind on the host: nothing that systemd will still try to run.
//
// `gvm apply` enables gvm@<name>.service and enables-and-starts a timer per
// job. Nothing else in gvm removes either -- apply plans only for the jobs a
// spec declares, and there is no sweeper -- so a destroy that removed only the
// drop-in, the lock and the YAML would leave a unit that fails at every boot
// and a timer that fires a nightly job against an instance whose definition is
// gone.
func TestDestroyDisablesTheUnitAndEveryJobTimer(t *testing.T) {
	h := newTestHost(t)
	install, data, arc := destroyable(t, h, "valheim-1")
	seedInstance(t, h, "valheim-1", "valheim", "--install", install, "--data", data,
		"--var", "public_name=Test")

	in := mustReadInstance(t, h, "valheim-1")
	if len(in.Spec.Jobs) == 0 {
		t.Fatal("the fixture declares no jobs, so this test would assert nothing")
	}

	code, out, errb := runWith(t, h, "valheim-1\ny\n", "destroy", "valheim-1", "--archive", arc)
	if code != 0 {
		t.Fatalf("destroy: exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}

	got := strings.Join(h.Systemd.calls, "\n")
	if !strings.Contains(got, "disable "+instance.UnitName("valheim-1")) {
		t.Errorf("the instance unit was never disabled, so systemd still tries it at every boot:\n%s", got)
	}
	for job := range in.Spec.Jobs {
		unit := strings.TrimSuffix(instance.JobUnit("valheim-1", job), ".service") + ".timer"
		for _, want := range []string{"stop " + unit, "disable " + unit} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q; the timer survives the instance it belongs to:\n%s", want, got)
			}
		}
	}
	if !strings.Contains(got, "daemon-reload") {
		t.Errorf("systemd was never told to re-read after the unit files were deleted:\n%s", got)
	}

	// And the files behind them are gone, spec last.
	for _, p := range []string{
		instance.PathsFor("valheim-1").Spec,
		instance.PathsFor("valheim-1").ManifestLock,
	} {
		if _, err := h.FS.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the destroy: %v", p, err)
		}
	}
}

// TestDestroyNamesWhatItLeftInPlace is gvm-self-upgrade-uninstall-7: destroy
// keeps secrets, the account, state, logs and the Steam home, and it must SAY
// so — an operator who read "destroy" as "gone" should not later find
// passwords and a service account still on the box.
func TestDestroyNamesWhatItLeftInPlace(t *testing.T) {
	h := newTestHost(t)
	install, data, arc := destroyable(t, h, "valheim-1")
	seedInstance(t, h, "valheim-1", "valheim", "--install", install, "--data", data,
		"--var", "public_name=Test")

	code, out, errb := runWith(t, h, "valheim-1\ny\n", "destroy", "valheim-1", "--archive", arc)
	if code != 0 {
		t.Fatalf("destroy: exit %d\n%s\n%s", code, out, errb)
	}
	if !strings.Contains(out, "LEFT IN PLACE") {
		t.Fatalf("destroy printed no LEFT IN PLACE block:\n%s", out)
	}
	for _, want := range []string{
		"gvm-valheim-1",       // the service account, by name
		"secrets",             // the secrets directory
		"not reversible",      // why the secrets are kept
		"instances/valheim-1", // the recorded state
		"logs",                // the logs
		"steam",               // the SteamCMD home
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the LEFT IN PLACE block is missing %q:\n%s", want, out)
		}
	}
}

// TestDestroyDryRunReportsAndRemovesNothing keeps --dry's contract intact after
// the space refusal was wired in: a dry run measures and reports, so a full
// volume must produce a report rather than a non-zero exit.
func TestDestroyDryRunReportsAndRemovesNothing(t *testing.T) {
	h := newTestHost(t)
	install, data, arc := destroyable(t, h, "valheim-1")
	seedInstance(t, h, "valheim-1", "valheim", "--install", install, "--data", data,
		"--var", "public_name=Test")
	h.HostEnv.Statfs = func(string) (platform.DiskUsage, error) {
		return platform.DiskUsage{Total: 1 << 20, Free: 8, Avail: 8}, nil
	}
	before := len(h.FS.writes)

	code, out, errb := runWith(t, h, "", "destroy", "valheim-1", "--archive", arc, "--dry")
	if code != 0 {
		t.Fatalf("--dry over a full volume exited %d; a dry run measures and reports\n%s", code, errb)
	}
	if !strings.Contains(out, "nothing was removed") {
		t.Errorf("the dry run does not say it removed nothing:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(data, "worlds_local", "Ded.db")); err != nil {
		t.Errorf("--dry removed the world: %v", err)
	}
	if len(h.FS.writes) != before {
		t.Errorf("--dry wrote %v", h.FS.writes[before:])
	}
	if len(h.Systemd.calls) != 0 {
		t.Errorf("--dry touched systemd: %v", h.Systemd.calls)
	}
}
