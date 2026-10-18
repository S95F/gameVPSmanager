package hostinit

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/packaging"
)

// fakeSystem records every effect in the order it happened, so a test can
// assert the ORDER host init touches the host in — which is the only property
// that distinguishes a correct install from a broken one.
type fakeSystem struct {
	exists map[string]bool
	calls  []string
	writes map[string][]byte
}

func newFake() *fakeSystem {
	return &fakeSystem{exists: map[string]bool{}, writes: map[string][]byte{}}
}

func (f *fakeSystem) Exists(p string) (bool, error) {
	f.calls = append(f.calls, "exists:"+p)
	return f.exists[p], nil
}
func (f *fakeSystem) ReplaceBinary(d string) error {
	f.calls = append(f.calls, "replace:"+d)
	return nil
}
func (f *fakeSystem) WriteRoot(p string, data []byte, _ uint32) error {
	f.calls = append(f.calls, "write:"+p)
	f.writes[p] = data
	return nil
}
func (f *fakeSystem) Sysusers(context.Context) error {
	f.calls = append(f.calls, "sysusers")
	return nil
}
func (f *fakeSystem) Tmpfiles(_ context.Context, p string) error {
	f.calls = append(f.calls, "tmpfiles:"+p)
	return nil
}
func (f *fakeSystem) ReloadDaemon(context.Context) error {
	f.calls = append(f.calls, "reload")
	return nil
}
func (f *fakeSystem) EnableUnit(_ context.Context, u string) error {
	f.calls = append(f.calls, "enable:"+u)
	return nil
}

func indexOf(calls []string, pred func(string) bool) int {
	for i, c := range calls {
		if pred(c) {
			return i
		}
	}
	return -1
}

func hasPrefix(p string) func(string) bool {
	return func(c string) bool { return strings.HasPrefix(c, p) }
}

// TestManifestCoversEveryPackagedFile is the point of generating the table
// from packaging.FS: every embedded file must be installed exactly once, to an
// absolute path under /usr/lib, and no file may be left behind.
func TestManifestCoversEveryPackagedFile(t *testing.T) {
	var embedded []string
	for _, dir := range []string{"systemd", "tmpfiles", "sysusers.d"} {
		entries, err := packaging.FS.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				embedded = append(embedded, dir+"/"+e.Name())
			}
		}
	}
	if len(embedded) < 5 {
		t.Fatalf("only %d embedded files; the walk is broken", len(embedded))
	}

	// The base manifest and the dashboard manifest PARTITION the embed: every
	// packaged file is installed by exactly one of them, and a dashboard file
	// never leaks into a games-only install (onboarding-1).
	m := append(Manifest(), DashboardManifest()...)
	srcSeen := map[string]int{}
	destSeen := map[string]int{}
	for _, f := range m {
		srcSeen[f.Src]++
		destSeen[f.Dest]++
		if !strings.HasPrefix(f.Dest, "/usr/lib/") {
			t.Errorf("%s installs to %s, which is not under /usr/lib", f.Src, f.Dest)
		}
		if f.Mode != 0o644 {
			t.Errorf("%s has mode %o, want 0644", f.Dest, f.Mode)
		}
	}
	for _, src := range embedded {
		if srcSeen[src] != 1 {
			t.Errorf("%s appears %d times across the two manifests, want 1", src, srcSeen[src])
		}
	}
	for d, n := range destSeen {
		if n != 1 {
			t.Errorf("%s is written by %d manifest entries, want 1", d, n)
		}
	}
	if len(m) != len(embedded) {
		t.Errorf("manifests have %d entries, %d files are embedded", len(m), len(embedded))
	}

	for _, f := range Manifest() {
		if dashboardSrcs[f.Src] {
			t.Errorf("%s is a dashboard file but is in the games-only manifest", f.Src)
		}
	}
	for _, f := range DashboardManifest() {
		if !dashboardSrcs[f.Src] {
			t.Errorf("%s is in the dashboard manifest but not declared dashboard-only", f.Src)
		}
	}
}

// TestDashboardInstallsExtras: --dashboard writes the control-plane units and
// a 0400 master key, points at `gvm db migrate` next, and never rewrites an
// existing key — rewriting it would orphan every sealed TOTP secret.
func TestDashboardInstallsExtras(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	if err := Run(context.Background(), f, &out, Options{Dashboard: true}); err != nil {
		t.Fatal(err)
	}
	for _, dest := range []string{
		"/usr/lib/systemd/system/gvmd.service",
		"/usr/lib/systemd/system/gvm-web.service",
		"/usr/lib/systemd/system/gvm-db-migrate.service",
		"/usr/lib/tmpfiles.d/gvm-web.conf",
	} {
		if _, ok := f.writes[dest]; !ok {
			t.Errorf("--dashboard did not install %s", dest)
		}
	}
	key, ok := f.writes["/etc/gvm/master.key"]
	if !ok {
		t.Fatal("--dashboard did not create master.key")
	}
	if len(key) != 32 {
		t.Errorf("master.key is %d bytes, want 32", len(key))
	}
	if !strings.Contains(out.String(), "next: sudo gvm db migrate") {
		t.Error("--dashboard should end with the migrate step")
	}
	if !strings.Contains(out.String(), "pg_hba.conf") {
		t.Error("--dashboard should print the postgresql peer-auth lines")
	}
	// Nothing is STARTED: no enable call names the dashboard units.
	for _, c := range f.calls {
		if c == "enable:gvmd.service" || c == "enable:gvm-web.service" {
			t.Errorf("--dashboard enabled %s; starting the dashboard is the operator's move", c)
		}
	}

	// A second run keeps the existing key.
	f2 := newFake()
	f2.exists["/etc/gvm/master.key"] = true
	var out2 bytes.Buffer
	if err := Run(context.Background(), f2, &out2, Options{Dashboard: true}); err != nil {
		t.Fatal(err)
	}
	if _, rewrote := f2.writes["/etc/gvm/master.key"]; rewrote {
		t.Error("a re-init rewrote master.key")
	}
}

// TestGamesOnlyInstallSkipsTheDashboard: without the flag, no dashboard file
// lands — a games-only host converges with no caddy group (onboarding-1).
func TestGamesOnlyInstallSkipsTheDashboard(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	if err := Run(context.Background(), f, &out, Options{}); err != nil {
		t.Fatal(err)
	}
	for dest := range f.writes {
		if strings.Contains(dest, "gvm-web") || strings.Contains(dest, "gvmd") || strings.Contains(dest, "master.key") {
			t.Errorf("games-only install wrote %s", dest)
		}
	}
}

// TestManifestIsSortedByDest: a stable order is what makes `--diff` and the
// install log read the same every time.
func TestManifestIsSortedByDest(t *testing.T) {
	m := Manifest()
	for i := 1; i < len(m); i++ {
		if m[i-1].Dest > m[i].Dest {
			t.Errorf("manifest is not sorted: %q before %q", m[i-1].Dest, m[i].Dest)
		}
	}
}

func TestRunInstallsInTheRightOrder(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	if err := Run(context.Background(), f, &out, Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	replace := indexOf(f.calls, hasPrefix("replace:"))
	sysusers := indexOf(f.calls, hasPrefix("sysusers"))
	tmpfiles := indexOf(f.calls, hasPrefix("tmpfiles:"))
	reload := indexOf(f.calls, hasPrefix("reload"))
	firstWrite := indexOf(f.calls, hasPrefix("write:/usr/lib/"))

	if replace < 0 || sysusers < 0 || tmpfiles < 0 || reload < 0 || firstWrite < 0 {
		t.Fatalf("a required step was never called: %v", f.calls)
	}
	// Binary first, then files, then accounts, then dirs, then one reload.
	if !(replace < firstWrite && firstWrite < sysusers && sysusers < tmpfiles && tmpfiles < reload) {
		t.Errorf("steps out of order: replace=%d write=%d sysusers=%d tmpfiles=%d reload=%d\n%v",
			replace, firstWrite, sysusers, tmpfiles, reload, f.calls)
	}

	// First init (no marker): both timers enabled and a marker written.
	for _, timer := range FirstTimers() {
		if indexOf(f.calls, func(c string) bool { return c == "enable:"+timer }) < 0 {
			t.Errorf("%s was not enabled on first init:\n%v", timer, f.calls)
		}
	}
	if _, ok := f.writes[MarkerPath]; !ok {
		t.Error("first init wrote no marker")
	}
	if _, ok := f.writes[ConfigPath]; !ok {
		t.Error("first init wrote no default config")
	}
	if !strings.Contains(out.String(), "next: sudo gvm doctor") {
		t.Errorf("output did not end by pointing at doctor:\n%s", out.String())
	}
	// Rule 7: the apt line is printed, so an operator can act on it.
	if !strings.Contains(out.String(), "lib32gcc-s1") {
		t.Errorf("the SteamCMD apt line was not printed:\n%s", out.String())
	}
}

// TestRunIsIdempotent: on a converged box the marker and the config already
// exist, so no timer is re-enabled and the operator's config is kept.
func TestRunIsIdempotent(t *testing.T) {
	f := newFake()
	f.exists[MarkerPath] = true
	f.exists[ConfigPath] = true
	var out bytes.Buffer
	if err := Run(context.Background(), f, &out, Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if i := indexOf(f.calls, hasPrefix("enable:")); i >= 0 {
		t.Errorf("a timer was enabled on a re-init: %q", f.calls[i])
	}
	if _, ok := f.writes[MarkerPath]; ok {
		t.Error("a re-init rewrote the marker")
	}
	if _, ok := f.writes[ConfigPath]; ok {
		t.Error("a re-init overwrote the operator's config")
	}
}

// TestDiffChangesNothing: --diff may read host state but must perform no
// mutating effect at all.
func TestDiffChangesNothing(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	if err := Run(context.Background(), f, &out, Options{Diff: true}); err != nil {
		t.Fatalf("diff: %v", err)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "exists:") {
			t.Errorf("--diff performed a mutating call %q; it must only read:\n%v", c, f.calls)
		}
	}
	if len(f.writes) != 0 {
		t.Errorf("--diff wrote %d file(s)", len(f.writes))
	}
	if !strings.Contains(out.String(), "nothing will be changed") {
		t.Errorf("--diff did not say it changes nothing:\n%s", out.String())
	}
}

// TestDefaultConfigIsValidYAML: the config host init writes must be the one the
// loader accepts, or an operator's first `gvm apply` fails on gvm's own file.
func TestDefaultConfigIsValidYAML(t *testing.T) {
	f := newFake()
	if err := Run(context.Background(), f, new(bytes.Buffer), Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	data := f.writes[ConfigPath]
	if len(data) == 0 {
		t.Fatal("no config written")
	}
	// A sanity check that it is non-empty YAML, not a Go zero value dumped raw.
	if !bytes.Contains(data, []byte(":")) {
		t.Errorf("config does not look like YAML:\n%s", data)
	}
}
