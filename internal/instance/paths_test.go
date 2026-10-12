package instance

import (
	"reflect"
	"strings"
	"testing"
)

// Section 5, transcribed. These are literal strings on purpose: the packaging
// carries the same paths in tmpfiles.d lines and systemd directives that no Go
// test can see, so the only way they stay in step is for a change here to have
// to be typed twice.
func TestPathsForIsSectionFive(t *testing.T) {
	p := PathsFor("pz-main")
	cases := []struct {
		field, got, want string
	}{
		{"Spec", p.Spec, "/etc/gvm/instances/pz-main.yaml"},
		{"ContentLock", p.ContentLock, "/etc/gvm/instances/pz-main.lock.json"},
		{"SpecDir", p.SpecDir, "/etc/gvm/instances/pz-main.d"},
		{"ManifestLock", p.ManifestLock, "/etc/gvm/instances/pz-main.d/manifest.lock.yaml"},
		{"SecretsDir", p.SecretsDir, "/etc/gvm/secrets/instances/pz-main"},
		{"Secret(rcon)", p.Secret("rcon"), "/etc/gvm/secrets/instances/pz-main/rcon"},

		{"State", p.State, "/var/lib/gvm/instances/pz-main"},
		{"Observed", p.Observed, "/var/lib/gvm/instances/pz-main/observed.json"},
		{"ContentObserved", p.ContentObserved, "/var/lib/gvm/instances/pz-main/content.observed.json"},
		{"PortsJSON", p.PortsJSON, "/var/lib/gvm/instances/pz-main/ports.json"},
		{"BlockedMods", p.BlockedMods, "/var/lib/gvm/instances/pz-main/blocked-mods.json"},
		{"ControlProbe", p.ControlProbe, "/var/lib/gvm/instances/pz-main/control-probe.json"},
		{"Lease", p.Lease, "/var/lib/gvm/instances/pz-main/lease.json"},
		{"SnapDir", p.SnapDir, "/var/lib/gvm/instances/pz-main/snap"},
		{"SteamScriptDir", p.SteamScriptDir, "/var/lib/gvm/instances/pz-main/steam"},
		{"SteamHome", p.SteamHome, "/var/lib/gvm/cache/steam/homes/pz-main"},

		{"Run", p.Run, "/run/gvm/pz-main"},
		{"ConsoleSock", p.ConsoleSock, "/run/gvm/pz-main/console.sock"},
		{"StatusJSON", p.StatusJSON, "/run/gvm/pz-main/status.json"},
		{"RunnerJSON", p.RunnerJSON, "/run/gvm/pz-main/runner.json"},
		{"ShutdownJSON", p.ShutdownJSON, "/run/gvm/pz-main/shutdown.json"},
		{"LastShutdown", p.LastShutdown, "/run/gvm/pz-main/last-shutdown.json"},
		{"FreezeLease", p.FreezeLease, "/run/gvm/pz-main/freeze.lease"},
		{"RunnerLock", p.RunnerLock, "/run/gvm/pz-main/job.lock"},
		{"Hold", p.Hold, "/run/gvm/pz-main/hold"},

		{"Logs", p.Logs, "/var/log/gvm/pz-main"},
		{"ConsoleLog", p.ConsoleLog, "/var/log/gvm/pz-main/console.log"},
		{"ShutdownLog", p.ShutdownLog, "/var/log/gvm/pz-main/shutdown.jsonl"},
		{"JobLogDir", p.JobLogDir, "/var/log/gvm/pz-main/jobs"},
		{"JobLog", p.JobLog("20260904-182211"), "/var/log/gvm/pz-main/jobs/20260904-182211.log"},
		{"Backups", p.Backups, "/var/backups/gvm/pz-main"},
		{"Srv", p.Srv, "/srv/gvm/pz-main"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
}

// The paths are the Linux host's, not the development machine's. Building them
// with path/filepath would produce backslashes here and a path set that is only
// correct on the machine it is never used on.
func TestEveryPathIsSlashSeparatedOnEveryPlatform(t *testing.T) {
	p := PathsFor("pz-main")
	v := reflect.ValueOf(p)
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.Name == "Name" {
			continue
		}
		s, ok := v.Field(i).Interface().(string)
		if !ok {
			continue
		}
		if strings.Contains(s, `\`) {
			t.Errorf("%s = %q contains a backslash; these are Linux paths", f.Name, s)
		}
		if !strings.HasPrefix(s, "/") {
			t.Errorf("%s = %q is not absolute", f.Name, s)
		}
	}
}

// Every derived path must live under a root the packaging actually creates. A
// path under, say, /var/lib/gvm/instance/ (singular) would be created silently
// at first write and never cleaned up by anything.
func TestEveryPathIsUnderADocumentedRoot(t *testing.T) {
	roots := []string{EtcRoot, StateRoot, RunRoot, LogRoot, BackupRoot, SrvRoot}
	p := PathsFor("pz-main")
	v := reflect.ValueOf(p)
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.Name == "Name" {
			continue
		}
		s, ok := v.Field(i).Interface().(string)
		if !ok {
			continue
		}
		found := false
		for _, r := range roots {
			if s == r || strings.HasPrefix(s, r+"/") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s = %q is under none of %v", f.Name, s, roots)
		}
	}
}

// D-02's whole point: /var/lib/gvm is disposable, which is only true because no
// secret is in it. If a secret path ever moves under StateRoot, "rm -rf
// /var/lib/gvm and re-apply" stops being safe advice.
func TestNoSecretPathLivesInTheDisposableTree(t *testing.T) {
	p := PathsFor("pz-main")
	for _, s := range []string{p.SecretsDir, p.Secret("rcon"), p.Secret("admin"), p.Secret("join"), HostSecretsDir} {
		if strings.HasPrefix(s, StateRoot) {
			t.Errorf("%q is under %s, which the documentation calls safe to lose (D-02)", s, StateRoot)
		}
		if !strings.HasPrefix(s, SecretsRoot+"/") {
			t.Errorf("%q is not under %s", s, SecretsRoot)
		}
	}
}

// Section 5.2: two HOMEs, deliberately. The game's HOME must be stable, because
// servers dlopen("$HOME/.steam/sdk64/steamclient.so"); SteamCMD's must be
// per-instance, because two concurrent steamcmd under one HOME corrupt ~/.steam.
func TestDefaultSpecPathsKeepsTheTwoHomesApart(t *testing.T) {
	sp := DefaultSpecPaths("pz-main")
	cases := []struct{ field, got, want string }{
		{"Install", sp.Install, "/srv/gvm/pz-main/install"},
		{"Data", sp.Data, "/srv/gvm/pz-main/data"},
		{"Home", sp.Home, "/srv/gvm/pz-main"},
		{"Work", sp.Work, "/var/lib/gvm/cache/steam/homes/pz-main"},
		{"Logs", sp.Logs, "/var/log/gvm/pz-main"},
		{"Backups", sp.Backups, "/var/backups/gvm/pz-main"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
	if sp.Home == sp.Work {
		t.Error("the game's HOME and SteamCMD's HOME are the same directory (section 5.2)")
	}
}

// A path set is a set of real filesystem paths on a live host. Deriving one
// from an unvalidated name is how an instance called "../../etc" gets a
// control-plane path outside the control plane.
func TestPathsForPanicsOnAnUnvalidatedName(t *testing.T) {
	for _, bad := range []string{"", "../etc", "PZ", "pz_main", "all"} {
		t.Run(bad, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("PathsFor(%q) produced a path set", bad)
				}
			}()
			PathsFor(bad)
		})
	}
}

// Two instances share no path at all. The runtime directory in particular is
// per-instance because instance A must not be able to stat instance B's
// console socket (D-10).
func TestTwoInstancesShareNoPath(t *testing.T) {
	a := reflect.ValueOf(PathsFor("pz-main"))
	b := reflect.ValueOf(PathsFor("pz-test"))
	for i := range a.NumField() {
		f := a.Type().Field(i)
		if f.Name == "Name" {
			continue
		}
		x, ok := a.Field(i).Interface().(string)
		if !ok {
			continue
		}
		y := b.Field(i).Interface().(string)
		if x == y {
			t.Errorf("%s is %q for both instances", f.Name, x)
		}
	}
}

// The host-wide roots are section 5 verbatim. They are asserted as literals
// rather than rebuilt from their own constants, because a test that says
// StateRoot+"/spool" == StateRoot+"/spool" passes no matter what StateRoot is.
func TestHostWideStateDirectories(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{SpoolDir, "/var/lib/gvm/spool"},
		{SteamHomesDir, "/var/lib/gvm/cache/steam/homes"},
		{SteamCMDLock, "/var/lib/gvm/locks/steamcmd.lock"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}
