package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/reconcile"
)

// TestTheGeneratedMarkerMatchesTheOneApplyWrites is where the deliberate
// duplication is paid for.
//
// observe.go carries its own copy of the header line so that reading a phase
// does not pull the whole rendering layer -- templates, golden files, the
// archive dependency -- into a process that only wants to know whether a
// server is up. A copy is only safe while something checks it, and this is the
// something. If it ever fails, the observer would stop recognising gvm's own
// drop-ins and would report every applied instance as carrying a foreign one.
func TestTheGeneratedMarkerMatchesTheOneApplyWrites(t *testing.T) {
	if generatedMarker != reconcile.GeneratedHeader {
		t.Fatalf("observe.go's marker %q != reconcile.GeneratedHeader %q",
			generatedMarker, reconcile.GeneratedHeader)
	}
	if localDropIn != reconcile.LocalDropIn {
		t.Fatalf("observe.go names %q, reconcile names %q", localDropIn, reconcile.LocalDropIn)
	}
}

// TestParseDropInHeaderReadsWhatApplyWrote runs the parser over the real
// golden file, so the header format is proven against the bytes apply
// actually produces rather than against a copy of them.
func TestParseDropInHeaderReadsWhatApplyWrote(t *testing.T) {
	raw, err := os.ReadFile("../reconcile/testdata/golden/pz-main-adopted.10-gvm.conf")
	if err != nil {
		t.Fatal(err)
	}
	h := ParseDropInHeader(strings.NewReader(string(raw)))
	if !h.Generated {
		t.Fatal("the golden drop-in was not recognised as generated")
	}
	if h.SpecPath != "/etc/gvm/instances/pz-main.yaml" {
		t.Errorf("SpecPath = %q", h.SpecPath)
	}
	if len(h.SpecSHA256) != 64 {
		t.Errorf("SpecSHA256 = %q, want a full sha256", h.SpecSHA256)
	}
	if h.Game != "projectzomboid" || h.Revision != 7 {
		t.Errorf("manifest line read as game=%q rev=%d", h.Game, h.Revision)
	}
	if h.ManifestSHA != "1b40c9de" || h.Origin != "shipped" {
		t.Errorf("manifest line read as sha=%q origin=%q", h.ManifestSHA, h.Origin)
	}
	if h.Rendered.IsZero() || h.Version != "0.1.0" {
		t.Errorf("rendered line read as %v by %q", h.Rendered, h.Version)
	}
}

// TestParseDropInHeaderOnAFileGVMDidNotWrite. The answer is a fact, not an
// error: the caller needs to know it is foreign so it can refuse to overwrite
// it, and an error would tempt a call site into treating it as absent.
func TestParseDropInHeaderOnAFileGVMDidNotWrite(t *testing.T) {
	h := ParseDropInHeader(strings.NewReader("[Service]\nMemoryMax=8G\n"))
	if h.Generated || h.SpecSHA256 != "" {
		t.Fatalf("an operator's own drop-in parsed as %+v", h)
	}
	// A comment block further down the file explains a directive; it is not
	// provenance and must not be mined for one.
	h = ParseDropInHeader(strings.NewReader("[Service]\n# spec sha256 deadbeef\n"))
	if h.SpecSHA256 != "" {
		t.Fatalf("a comment below the header was read as provenance: %+v", h)
	}
}

// fullHost builds a fake filesystem holding a complete, healthy valheim-1.
//
// The manifest is the SHIPPED one, read off disk, and the instance's pin is
// computed from its bytes -- so the pin check is exercised against a real
// manifest and the fixture cannot go stale when games/valheim.yaml is edited.
func fullHost(t *testing.T) (*fakeFS, string) {
	t.Helper()
	manifest, err := os.ReadFile("../../games/valheim.yaml")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(manifest)
	sha := hex.EncodeToString(sum[:])

	inst := fmt.Sprintf(`apiVersion: gvm/v1
kind: Instance
metadata:
  name: valheim-1
  uid: 019f2c7a-8b31-7c4e-9d20-3f61a5b7c9e3
spec:
  game: valheim
  manifest: {revision: 4, sha256: %s, origin: shipped}
  enabled: true
  managed_install: true
  user: gvm-valheim-1
  paths:
    install: /srv/gvm/valheim-1/install
    data: /srv/gvm/valheim-1/data
    home: /srv/gvm/valheim-1
    work: /var/lib/gvm/cache/steam/homes/valheim-1
  ports: {game: 2456, query: 2457}
  vars: {public_name: Ashlands}
`, sha)

	p := instance.PathsFor("valheim-1")
	f := newFS()
	f.put(p.Spec, inst)
	f.put(p.ManifestLock, string(manifest))
	f.put(instance.DropInPath("valheim-1"), generatedMarker+"\n"+
		"# source      "+p.Spec+"\n"+
		"# spec sha256 "+sha256Hex([]byte(inst))+"\n"+
		"# manifest    valheim rev 4 sha256:"+sha[:8]+" (origin: shipped)\n"+
		"# rendered    2026-09-04T18:22:11Z by gvm 0.1.0\n"+
		"[Service]\nUser=gvm-valheim-1\n")
	f.put("/srv/gvm/valheim-1/install/valheim_server.x86_64", "ELF")
	f.putAt(p.StatusJSON, `{"proto":1,"instance":"valheim-1","state":"ready",
		"ready_at":"2026-09-04T18:23:58Z","process":{"pid":41799,"pgid":41799,"start_ticks":1},
		"control":{"kind":"source_rcon","ready":true}}`, at("2026-09-04T18:59:58Z"))
	return f, sha
}

func TestObserveReadsAHealthyInstance(t *testing.T) {
	f, sha := fullHost(t)
	sd := newSystemd().set("gvm@valheim-1.service", platform.UnitState{
		LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 41799,
	})
	o := &Observer{FS: f, Units: sd, Now: func() time.Time { return at("2026-09-04T19:00:00Z") }}

	obs, err := o.Observe(context.Background(), "valheim-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Problems) != 0 {
		t.Fatalf("a healthy host produced problems: %+v", obs.Problems)
	}
	if !obs.SpecPresent || obs.Game != "valheim" || !obs.Enabled {
		t.Fatalf("spec read as %+v", obs)
	}
	if !obs.Pin.Present || !obs.Pin.OK {
		t.Fatalf("pin read as %+v (manifest sha %s)", obs.Pin, sha[:8])
	}
	if !obs.DropIn.Generated || obs.DropIn.SpecSHA256 != obs.SpecSHA256 {
		t.Fatalf("drop-in header %+v does not match spec sha %s", obs.DropIn, obs.SpecSHA256)
	}
	if !obs.UnitChecked || !obs.Unit.Running() {
		t.Fatalf("unit read as %+v", obs.Unit)
	}
	if obs.Status == nil || obs.Status.State != "ready" {
		t.Fatalf("status read as %+v", obs.Status)
	}
	if obs.Installed != Yes {
		t.Fatalf("the sentinel is on disk and Installed = %s", obs.Installed)
	}
	if v := Derive(obs); v.Phase != PhaseRunning {
		t.Fatalf("a healthy host derived %s: %s", v.Phase, v.Reason)
	}
	// The reconciler and the manager both go through Observe; a read that
	// mutated anything would be a sweep with side effects.
	if sd.Actuations() != 0 {
		t.Fatalf("observing called %d mutating systemd methods", sd.Actuations())
	}
}

func TestObserveReportsWhatItCouldNotRead(t *testing.T) {
	f, _ := fullHost(t)
	f.fail(instance.PathsFor("valheim-1").StatusJSON, fs.ErrPermission)
	o := &Observer{FS: f, Now: func() time.Time { return at("2026-09-04T19:00:00Z") }}

	obs, err := o.Observe(context.Background(), "valheim-1")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Status != nil {
		t.Fatal("a status file that could not be read was decoded anyway")
	}
	var found bool
	for _, p := range obs.Problems {
		if p.Source == "status" {
			found = true
		}
	}
	if !found {
		t.Fatalf("an unreadable status.json left no problem behind: %+v", obs.Problems)
	}
}

// TestASentinelThatIsTooSmallIsNotInstalled. A zero-length or truncated
// sentinel is what a half-finished extract leaves, and "the file exists" would
// call that installed.
func TestASentinelThatIsTooSmallIsNotInstalled(t *testing.T) {
	f, _ := fullHost(t)
	manifest, err := os.ReadFile("../../games/valheim.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Give the manifest a min_bytes the empty sentinel cannot meet.
	patched := strings.Replace(string(manifest),
		"sentinel: {path: valheim_server.x86_64, executable: true}",
		"sentinel: {path: valheim_server.x86_64, executable: true, min_bytes: 1024}", 1)
	if patched == string(manifest) {
		t.Fatal("the sentinel line moved; this fixture patch no longer applies")
	}
	sum := sha256.Sum256([]byte(patched))
	p := instance.PathsFor("valheim-1")
	f.put(p.ManifestLock, patched)
	// Re-pin the instance so the pin still verifies and the failure under
	// test is the sentinel and not the pin.
	instYAML, err := f.ReadFile(p.Spec)
	if err != nil {
		t.Fatal(err)
	}
	f.put(p.Spec, replaceSHA(string(instYAML), hex.EncodeToString(sum[:])))

	o := &Observer{FS: f, Now: func() time.Time { return at("2026-09-04T19:00:00Z") }}
	obs, err := o.Observe(context.Background(), "valheim-1")
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Pin.OK {
		t.Fatalf("the re-pinned fixture no longer verifies: %s", obs.Pin.Reason)
	}
	if obs.Installed != No {
		t.Fatalf("a 3-byte sentinel against min_bytes 1024 read as Installed=%s", obs.Installed)
	}
}

func replaceSHA(instYAML, sha string) string {
	i := strings.Index(instYAML, "sha256: ")
	j := strings.Index(instYAML[i:], ",")
	return instYAML[:i+len("sha256: ")] + sha + instYAML[i+j:]
}

// TestObserveClassifiesTheLeaseWhereTheEvidenceIs.
//
// Derive consumes LeaseState rather than re-deriving it, so this is where the
// boot id and the process table are allowed to be consulted and the only place
// a mistake about them can be made.
func TestObserveClassifiesTheLeaseWhereTheEvidenceIs(t *testing.T) {
	const boot = "5f1c0b3e-7a24-4b8e-9c31-2d8e6a4f0b17"
	l := Lease{
		Instance: "valheim-1", Kind: JobUpdate, PID: 900, BootID: boot,
		StartedAt: at("2026-09-04T18:55:00Z"),
	}
	body, err := l.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		alive func(int) Tri
		want  LeaseState
	}{
		{"the holder is running", aliveFunc(900), LeaseLive},
		{"the holder is gone", aliveFunc(), LeaseStale},
		{"nobody could look", nil, LeaseLive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := fullHost(t)
			f.put(LeasePath("valheim-1"), string(body))
			o := &Observer{
				FS: f, BootID: boot, Alive: tc.alive,
				Now: func() time.Time { return at("2026-09-04T19:00:00Z") },
			}
			obs, err := o.Observe(context.Background(), "valheim-1")
			if err != nil {
				t.Fatal(err)
			}
			if obs.LeaseState != tc.want {
				t.Fatalf("LeaseState = %s, want %s", obs.LeaseState, tc.want)
			}
		})
	}
}

// TestObserveDoesNotCallAFutureLeaseUnreadable is the observer side of the N-1
// skew rule (U17). Before it, a lease a newer gvm wrote -- a proto this build
// had never seen -- failed ParseLease, so the instance was reported "failed"
// and the printed fix was `gvm job clear`, which deletes the lease of a job
// that is still running. Now the observer classifies it from the frozen holder
// fields: the instance reads LIVE, and job clear's own LeaseLive guard protects
// it.
func TestObserveDoesNotCallAFutureLeaseUnreadable(t *testing.T) {
	const boot = "5f1c0b3e-7a24-4b8e-9c31-2d8e6a4f0b17"
	future := `{"proto":2,"instance":"valheim-1","kind":"update",` +
		`"unit":"gvm-job@valheim-1_nightly.service","pid":900,"boot_id":"` + boot + `",` +
		`"started_at":"2026-09-04T18:55:00Z"}`

	f, _ := fullHost(t)
	f.put(LeasePath("valheim-1"), future)
	o := &Observer{
		FS: f, BootID: boot, Alive: aliveFunc(900),
		Now: func() time.Time { return at("2026-09-04T19:00:00Z") },
	}
	obs, err := o.Observe(context.Background(), "valheim-1")
	if err != nil {
		t.Fatal(err)
	}
	if obs.LeaseUnreadable {
		t.Error("a future-proto lease was called unreadable; its holder fields are perfectly readable")
	}
	if obs.LeaseState != LeaseLive {
		t.Fatalf("LeaseState = %s, want live: the holder (pid 900, this boot) is alive", obs.LeaseState)
	}
	if obs.Lease == nil || obs.Lease.PID != 900 {
		t.Fatalf("the holder was not carried through for display: %+v", obs.Lease)
	}
}

// TestListReadsEtcAndNothingElse.
//
// The sibling files in /etc/gvm/instances are the trap: a `.lock.json`, a
// `.d/` directory and an editor's `.yaml.bak` all live beside the specs, and
// a glob that took them would produce instance names that derive paths nothing
// on the host has.
func TestListReadsEtcAndNothingElse(t *testing.T) {
	f := newFS()
	f.put("/etc/gvm/instances/pz-main.yaml", "x")
	f.put("/etc/gvm/instances/valheim-1.yaml", "x")
	f.put("/etc/gvm/instances/pz-main.lock.json", "{}")
	f.put("/etc/gvm/instances/pz-main.yaml.bak", "x")
	f.put("/etc/gvm/instances/pz-main.d/manifest.lock.yaml", "x")
	f.put("/etc/gvm/instances/Not_A_Name.yaml", "x")

	names, probs, err := List(f)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pz-main", "valheim-1"}
	if len(names) != len(want) {
		t.Fatalf("List = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("List = %v, want %v", names, want)
		}
	}
	// The illegal name is REPORTED rather than dropped: a file somebody meant
	// to be an instance and that gvm silently ignores is a server nobody can
	// find out is unmanaged.
	if len(probs) != 1 || !strings.Contains(probs[0].Path, "Not_A_Name.yaml") {
		t.Fatalf("problems = %+v, want one naming Not_A_Name.yaml", probs)
	}
}

// TestListNeverReachesForADatabase is section 1's rule, asserted at the one
// place enumeration happens: the only argument List takes is a filesystem.
func TestListNeverReachesForADatabase(t *testing.T) {
	f := newFS()
	f.put("/etc/gvm/instances/pz-main.yaml", "x")
	names, _, err := List(f)
	if err != nil || len(names) != 1 {
		t.Fatalf("List against a filesystem alone returned %v, %v", names, err)
	}
}
