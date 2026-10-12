package backup

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// TestTheEngineSatisfiesTheJobLayersBackupsSeam.
//
// internal/jobs declares the Backups interface next to the step table that
// calls it, and this package deliberately does NOT import internal/jobs: the
// import would pull the job layer -- and therefore os/exec -- into the
// dependency graph of a package whose whole purpose is to not spawn anything.
//
// Asserting the contract from a TEST proves the same thing and costs nothing
// at runtime. If a step is added to the seam, this line stops compiling, which
// is the notification that matters.
func TestTheEngineSatisfiesTheJobLayersBackupsSeam(t *testing.T) {
	var _ jobs.Backups = (*Engine)(nil)
}

// rig is one instance's worth of real files plus the fakes around them.
//
// The archive itself is written for real, because internal/archive's two-pass
// digest check and its manifest are the integrity story and a fake would test
// neither. Everything else -- the lease, the channels, the disk measurement --
// is a seam.
type rig struct {
	t     *testing.T
	root  string
	paths instance.Paths
	fs    *fakeFS
	clock *fakeClock

	console *fakeConsole
	log     *fakeLog
	audits  *audit.MemorySink

	engine *Engine
	notes  []string
}

// newRig builds a Project Zomboid instance with a small world on disk.
func newRig(t *testing.T, opts ...func(*Config)) *rig {
	t.Helper()
	// Forward slashes throughout: the production paths are slash-separated
	// Linux paths from internal/instance, and using the same shape here means
	// the placement rules are exercised in their production form.
	root := filepath.ToSlash(t.TempDir())
	data := root + "/data"
	world := data + "/Saves/Multiplayer/gvm"

	for _, d := range []string{
		data + "/Server", world, world + "_player", data + "/db", data + "/Lua",
		data + "/steamapps/workshop/content/108600/2169435993",
	} {
		if err := os.MkdirAll(filepath.FromSlash(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		if err := os.WriteFile(filepath.FromSlash(p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(data+"/Server/gvm.ini", "Mods=\nWorkshopItems=\n")
	write(world+"/map_t.bin", strings.Repeat("w", 4096))
	write(world+"/map_t.bin.lock", "a lock file the manifest excludes")
	write(world+"_player/players.db", "players")
	write(data+"/db/gvm.db", "accounts")
	write(data+"/Lua/shared.lua", "optional")
	write(data+"/steamapps/workshop/content/108600/2169435993/mod.info", strings.Repeat("m", 8192))

	// The control-plane paths. Overridden to the temp tree so a test never
	// touches /run or /var, while keeping the same field names production
	// uses.
	paths := instance.PathsFor("pz-main")
	paths.Backups = root + "/backups"
	paths.SnapDir = root + "/snap"

	in := &spec.Instance{}
	in.Metadata.Name = "pz-main"
	in.Spec.Game = "projectzomboid"
	in.Spec.User = "gvm-pz-main"
	in.Spec.Enabled = true
	in.Spec.ManagedInstall = true
	in.Spec.Manifest = spec.ManifestPin{Revision: 7, SHA256: "b1946ac9"}
	in.Spec.Paths = spec.Paths{
		Install: root + "/install",
		Data:    data,
		Home:    root + "/home",
		Work:    root + "/work",
		Logs:    root + "/logs",
		Backups: paths.Backups,
	}
	in.Spec.Backup = spec.BackupPolicy{Keep: 2}

	r := &rig{
		t: t, root: root, paths: paths,
		fs:      newFS(),
		clock:   newClock(testNow),
		console: &fakeConsole{StartSeq: 10},
		log:     &fakeLog{cursor: 5},
		audits:  audit.NewMemorySink(),
	}

	cfg := Config{
		Instance: "pz-main",
		Game:     "projectzomboid",
		Spec:     in,
		Manifest: shippedManifest(t, "projectzomboid"),
		Renderer: spec.NewRenderer(spec.RenderData{
			Name: "pz-main", Game: "projectzomboid",
			Install: in.Spec.Paths.Install, Data: data,
			Home: in.Spec.Paths.Home, Work: in.Spec.Paths.Work,
			User: "gvm-pz-main", Group: "gvm-pz-main",
			Var:  spec.VarValues{"server_name": "gvm"},
			Port: spec.PortValues{"game": 16261},
		}),
		SpecYAML: "apiVersion: gvm/v1\nkind: Instance\n",
		Paths:    paths,
		FS:       r.fs,
		Channels: Channels{
			Console: r.console, Log: r.log,
			Actor: "gvm-backup", Sleep: r.clock.Sleep, Now: r.clock.Now,
		},
		Audit: audit.New(r.audits).WithClock(r.clock.Now),
		Actor: audit.Timer(),
		Log:   quietLogger(),
		Now:   r.clock.Now,
		PID:   48812,
		Unit:  "gvm-job@pz-main_nightly.service",
		Notes: func(s string) { r.notes = append(r.notes, s) },
	}
	for _, o := range opts {
		o(&cfg)
	}
	r.engine = New(cfg)
	return r
}

func (r *rig) leasePath() string { return r.paths.FreezeLease }

// run performs the whole step sequence the job's table performs.
func (r *rig) run(ctx context.Context) (*archive.Ref, error) {
	r.t.Helper()
	if err := r.engine.Preflight(ctx); err != nil {
		return nil, err
	}
	if err := r.engine.Freeze(ctx); err != nil {
		return nil, err
	}
	// The step's Undo, registered the moment the freeze starts. It is the net
	// under a panic; the engine's own Snapshot thaws first on every ordinary
	// path, which is what this call being a no-op demonstrates.
	defer func() { _ = r.engine.Thaw(ctx) }()

	ref, err := r.engine.Snapshot(ctx, archive.ClassRoutine, "routine")
	if err != nil {
		return nil, err
	}
	if err := r.engine.Verify(ctx, ref); err != nil {
		return ref, err
	}
	if err := r.engine.Prune(ctx, ref); err != nil {
		return ref, err
	}
	if err := r.engine.Push(ctx, ref); err != nil {
		return ref, err
	}
	return ref, nil
}

// TestTheWholeBackupRunsAndTheFreezeEndsBeforeTheVerify.
//
// The ordering claim is the important one. The job's step table registers the
// thaw as the freeze step's Undo, and an Undo runs during the UNWIND -- after
// verify, after prune, after the off-box push. A `direct` backup whose thaw
// waited for that would hold a world unsaved through a deep verification and a
// 2 GB upload. So the engine thaws as the last act of Snapshot, and the Undo is
// only the net under a panic.
func TestTheWholeBackupRunsAndTheFreezeEndsBeforeTheVerify(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if !r.fs.has(r.leasePath()) {
		t.Fatal("the freeze lease is not on disk after Freeze")
	}

	ref, err := r.engine.Snapshot(ctx, archive.ClassRoutine, "routine")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if r.fs.has(r.leasePath()) {
		t.Fatal("Snapshot returned with the freeze lease still armed; the thaw must happen " +
			"before the verify, not during the job's unwind")
	}
	res := r.engine.Result()
	if !res.Thaw.Thawed || !res.Thaw.LeaseCleared {
		t.Fatalf("thaw result = %+v", res.Thaw)
	}

	if err := r.engine.Verify(ctx, ref); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ref.Verified() {
		t.Fatal("Verify left VerifiedAt zero, so nothing downstream may prune or destroy")
	}
	if err := r.engine.Prune(ctx, ref); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// The archive is real and contains the world.
	rep, err := archive.Verify(ctx, ref.Path, archive.VerifyOptions{Mode: archive.ModeDeep})
	if err != nil {
		t.Fatalf("deep verify: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("the archive has problems: %v", rep.Problems)
	}
	members := map[string]bool{}
	for _, m := range rep.Manifest.Members {
		members[m.Path] = true
	}
	for _, want := range []string{
		"data/Server/gvm.ini",
		"data/Saves/Multiplayer/gvm/map_t.bin",
		"data/Saves/Multiplayer/gvm_player/players.db",
		"data/db/gvm.db",
		"data/Lua/shared.lua",
	} {
		if !members[want] {
			t.Errorf("the archive is missing %s", want)
		}
	}
	// Section 13.2's two named exclusions: the workshop subtree is the
	// difference between a 2.4 GB nightly and an 8 GB one, and "**/*.lock" is
	// a bare pattern that was plainly meant to apply everywhere.
	for m := range members {
		if strings.Contains(m, "steamapps/workshop") {
			t.Errorf("%s is in the archive and the manifest excludes the workshop subtree", m)
		}
		if strings.HasSuffix(m, ".lock") {
			t.Errorf("%s is in the archive and the manifest excludes **/*.lock", m)
		}
	}
}

// TestTheLeaseIsArmedBeforeTheFirstRungIsSent.
//
// D-31's ordering, and the only order that works. A lease written after the
// flush leaves a window exactly as wide as the send -- and for a game whose
// `before` is `save-off`, that send is the command that turns persistence off.
func TestTheLeaseIsArmedBeforeTheFirstRungIsSent(t *testing.T) {
	var order []string
	r := newRig(t, func(c *Config) {
		// Wrap the console so the ordering of the two effects is recorded
		// rather than inferred.
		inner := c.Channels.Console
		c.Channels.Console = consoleFunc(func(ctx context.Context, actor, line string) (uint64, error) {
			order = append(order, "send:"+line)
			return inner.Send(ctx, actor, line)
		})
	})
	r.fs.onWrite = func(p string) {
		if strings.HasSuffix(p, "freeze.lease") {
			order = append(order, "arm")
		}
	}
	ctx := context.Background()
	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "arm" || order[1] != "send:save" {
		t.Fatalf("order = %v, want the lease armed and then the flush sent", order)
	}
}

// TestAGameThatDeclaresNoFreezeArmsNoLease.
//
// Core Keeper and the generic Steam manifest declare
// `{before: none, after: none}`. Arming a lease there would put a watchdog on
// a world nobody touched, and `gvm status` would show a freeze during every
// nightly for a game that has none.
func TestAGameThatDeclaresNoFreezeArmsNoLease(t *testing.T) {
	r := newRig(t, func(c *Config) {
		m := shippedManifest(t, "corekeeper")
		// Core Keeper's own include list points somewhere else entirely, so
		// the freeze is swapped onto the Project Zomboid manifest rather than
		// rebuilding the world tree: the claim under test is about the freeze.
		pz := shippedManifest(t, "projectzomboid")
		pz.Spec.Backup.Freeze = m.Spec.Backup.Freeze
		c.Manifest = pz
	})
	ctx := context.Background()
	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	if r.fs.has(r.leasePath()) {
		t.Fatal("a game with no freeze armed a lease")
	}
	if !containsSubstring(r.notes, "declares no freeze") {
		t.Errorf("notes = %v, want one saying the archive has no flush behind it", r.notes)
	}
	if _, err := r.engine.Snapshot(ctx, archive.ClassRoutine, "routine"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(r.console.lines()) != 0 {
		t.Errorf("a game with no freeze sent %q", r.console.lines())
	}
}

// TestAFailedThawFailsTheSnapshotAndLeavesTheLeaseArmed.
//
// The archive bytes are on disk, and the engine still fails: a server left
// with saving disabled is worse than a missing tarball. The lease stays, which
// is what the runner's own timer acts on -- so the world is fixed within a poll
// interval of the deadline even though this job is about to exit with an error.
func TestAFailedThawFailsTheSnapshotAndLeavesTheLeaseArmed(t *testing.T) {
	r := newRig(t, func(c *Config) {
		// A three-rung freeze, so there is a thaw to fail. The console
		// accepts the flush and then refuses the thaw.
		pz := shippedManifest(t, "projectzomboid")
		pz.Spec.Backup.Freeze.After = &spec.ShutdownStep{
			Via: spec.ViaControl, Send: "save-on", Wait: spec.Duration(5 * time.Second)}
		c.Manifest = pz
		c.Channels.Control = &fakeControl{Fail: errInjected}
	})
	ctx := context.Background()
	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := r.engine.Snapshot(ctx, archive.ClassRoutine, "routine")
	if !errors.Is(err, ErrFreeze) {
		t.Fatalf("Snapshot error = %v, want ErrFreeze", err)
	}
	if !r.fs.has(r.leasePath()) {
		t.Fatal("the lease was cleared after a thaw that did not land; it is the only remaining " +
			"evidence that this world may not be saving")
	}
	l, rerr := ReadLease(r.fs, r.leasePath())
	if rerr != nil || l == nil {
		t.Fatalf("the lease is unreadable: %v", rerr)
	}
	if len(l.ThawSteps) != 1 || l.ThawSteps[0].Send != "save-on" {
		t.Errorf("the lease's thaw steps = %+v, want the save-on the runner must re-send", l.ThawSteps)
	}
}

// TestTheThawIsIdempotentAcrossItsThreeCallers.
//
// Snapshot thaws, the job's step Undo thaws, and the runner's lease timer
// thaws. All three can fire, and `save-on` sent twice to a server that is
// already saving is a no-op -- but reporting the thaw twice, or sending it
// twice, would make the record say something that did not happen.
func TestTheThawIsIdempotentAcrossItsThreeCallers(t *testing.T) {
	r := newRig(t, func(c *Config) {
		pz := shippedManifest(t, "projectzomboid")
		pz.Spec.Backup.Freeze.After = &spec.ShutdownStep{Via: spec.ViaControl, Send: "save-on"}
		c.Manifest = pz
		c.Channels.Control = &fakeControl{}
	})
	ctx := context.Background()
	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := r.engine.Thaw(ctx); err != nil {
			t.Fatalf("Thaw %d: %v", i, err)
		}
	}
	ctl := r.engine.cfg.Channels.Control.(*fakeControl)
	if got := ctl.lines(); len(got) != 1 || got[0] != "save-on" {
		t.Fatalf("control got %q, want exactly one save-on", got)
	}
}

// TestTheStallIsMeasuredAndRedAboveThirtySeconds.
//
// On an 8 GB ext4 VPS a direct nightly pauses the world for seventy-four
// seconds, and section 13.2 says the operator deserves to know. The figure is
// the clock's, so a test can produce it without a 2.4 GB world.
func TestTheStallIsMeasuredAndRedAboveThirtySeconds(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	// Stand in for the seventy-four seconds the tar takes on a small VPS.
	r.clock.Advance(74210 * time.Millisecond)
	if _, err := r.engine.Snapshot(ctx, archive.ClassRoutine, "routine"); err != nil {
		t.Fatal(err)
	}
	res := r.engine.Result()
	if res.Snapshot.StallMs < 74210 {
		t.Fatalf("stall_ms = %d, want at least the 74210 the clock advanced", res.Snapshot.StallMs)
	}
	if !res.Snapshot.Red() {
		t.Error("a 74-second stall is not red")
	}
	if !containsSubstring(r.notes, "the dashboard renders") {
		t.Errorf("notes = %v, want one telling the operator what would cut the stall", r.notes)
	}
	// Off Linux the probe refuses, so the method is direct and the archive is
	// taken inside the freeze -- which is the hard path and the one most VPS
	// images take.
	if res.Snapshot.Method != MethodDirect && !isLinux() {
		t.Errorf("method = %q off Linux, want %q", res.Snapshot.Method, MethodDirect)
	}
}

// TestThePreflightRefusesBeforeTheFreezeWhenTheDiskIsFull.
//
// The one refusal that is deliberately on the near side of the freeze.
// Discovering a full disk halfway through a tar leaves a truncated archive
// where a verified one was promised -- and under a direct snapshot it leaves it
// there with autosave off.
func TestThePreflightRefusesBeforeTheFreezeWhenTheDiskIsFull(t *testing.T) {
	r := newRig(t)
	r.fs.Avail = 16
	err := r.engine.Preflight(context.Background())
	if !errors.Is(err, ErrNoRoom) {
		t.Fatalf("Preflight error = %v, want ErrNoRoom", err)
	}
	if !strings.Contains(err.Error(), "before the freeze") {
		t.Errorf("the refusal does not say why it is here: %v", err)
	}
	if r.fs.has(r.leasePath()) {
		t.Error("a refused preflight armed a lease")
	}
	if len(r.console.lines()) != 0 {
		t.Errorf("a refused preflight sent %q", r.console.lines())
	}
}

// TestFreeSpaceThatCannotBeMeasuredIsNotARefusal.
//
// On a host where statfs cannot be asked -- which includes the development
// machine -- the create itself will fail with ENOSPC, and internal/archive
// turns that into a removed partial file and a named error. A worse outcome
// than a preflight, but not a worse one than not backing up at all.
func TestFreeSpaceThatCannotBeMeasuredIsNotARefusal(t *testing.T) {
	r := newRig(t)
	r.fs.FailStatfs = true
	if err := r.engine.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !containsSubstring(r.notes, "could not be measured") {
		t.Errorf("notes = %v, want one recording the unmeasured disk", r.notes)
	}
}

// TestTheEngineRefusesToRunOutOfOrder.
//
// Freeze before Preflight would arm a lease whose deadline was computed from a
// snapshot method nobody had chosen, and Snapshot before Preflight would
// archive a source set nobody had resolved. Both are call-site bugs and both
// are named rather than tolerated.
func TestTheEngineRefusesToRunOutOfOrder(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.engine.Freeze(ctx); !errors.Is(err, ErrConfig) {
		t.Errorf("Freeze before Preflight = %v, want ErrConfig", err)
	}
	if _, err := r.engine.Snapshot(ctx, archive.ClassRoutine, "x"); !errors.Is(err, ErrConfig) {
		t.Errorf("Snapshot before Preflight = %v, want ErrConfig", err)
	}
}

// TestAnInstanceWithNoUserCannotArmALease.
//
// The lease has to be readable by the account the game runs as, and that
// account is READ from the instance YAML rather than derived from the name: an
// adopted pz-main runs as pzserver (section 6.4). With no user there is
// nobody to hand it to, and a lease nobody can read is a safety net that is
// not there.
func TestAnInstanceWithNoUserCannotArmALease(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Spec.Spec.User = "" })
	err := r.engine.Preflight(context.Background())
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("Preflight error = %v, want ErrConfig", err)
	}
	if !strings.Contains(err.Error(), "freeze lease") {
		t.Errorf("the refusal does not say what is lost: %v", err)
	}
}

// TestTheLeaseIsHandedToTheAccountTheInstanceYAMLNames.
func TestTheLeaseIsHandedToTheAccountTheInstanceYAMLNames(t *testing.T) {
	// An adopted instance: a pre-existing account that gvm did not create and
	// whose name is nothing like the instance's.
	r := newRig(t, func(c *Config) {
		c.Spec.Spec.User = "pzserver"
		c.Spec.Spec.ManagedInstall = false
	})
	ctx := context.Background()
	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.fs.owner(r.leasePath()); got != "pzserver" {
		t.Errorf("lease owner = %q, want pzserver and never a derived gvm-pz-main", got)
	}
	if got := r.fs.mode(r.leasePath()); got != LeaseFileMode {
		t.Errorf("lease mode = %v, want %v", got, LeaseFileMode)
	}
}

// TestTheBackupCreateAuditRowIsWrittenWhetherOrNotTheArchiveSucceeds.
//
// Section 14.8's backup.create row exists so that "there is no backup from
// that night" is answerable. A failure that wrote no row is precisely the case
// where it is not.
func TestTheBackupCreateAuditRowIsWrittenWhetherOrNotTheArchiveSucceeds(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, rec := range r.audits.Records() {
		if rec.Action != audit.BackupCreate {
			continue
		}
		found = true
		for _, key := range []string{"path", "sha256", "bytes", "snapshot_method", "freeze_ms"} {
			if _, ok := rec.Params[key]; !ok {
				t.Errorf("the backup.create row has no %q: %v", key, rec.Params)
			}
		}
	}
	if !found {
		t.Fatal("no backup.create row was written")
	}
}

// TestRetentionIsAppliedFromTheInstancesOwnPolicyAndNeverReimplemented.
//
// `keep: 2` plus three nights. The mechanics are archive.Prune's -- which
// already refuses to touch a preserve archive, an unmanaged tarball, or
// anything at all without a fresh verified Ref -- and the engine's job is to
// hand it the policy and the receipt. A second retention policy here would be
// a second set of rules about deleting worlds.
func TestRetentionIsAppliedFromTheInstancesOwnPolicyAndNeverReimplemented(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	// Two older nightlies and one preserve archive from a rebuild.
	dir := filepath.FromSlash(r.paths.Backups)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var older []string
	for i := 1; i <= 2; i++ {
		at := testNow.Add(-time.Duration(i) * 24 * time.Hour)
		older = append(older, copyArchive(t, r, dir, archive.ClassRoutine, at))
	}
	preserve := copyArchive(t, r, dir, archive.ClassPreserve, testNow.Add(-90*24*time.Hour))

	if _, err := r.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	res := r.engine.Result()
	if res.Prune == nil {
		t.Fatal("no prune result")
	}
	// keep: 2 means tonight's plus one older.
	if len(res.Prune.Removed) != 1 {
		t.Fatalf("removed %v, want one of the two older nightlies", res.Prune.Removed)
	}
	if slices.Contains(res.Prune.Removed, filepath.Base(preserve)) {
		t.Fatal("retention removed a preserve archive")
	}
	if _, err := os.Stat(preserve); err != nil {
		t.Fatalf("the preserve archive is gone: %v", err)
	}
	if !containsSubstring(r.notes, "never pruned by any code path") {
		t.Errorf("notes = %v, want one accounting for the preserve bytes", r.notes)
	}
	// The one that went is the OLDEST of the two, not whichever the directory
	// listing happened to yield first.
	if res.Prune.Removed[0] != filepath.Base(older[len(older)-1]) {
		t.Errorf("removed %q, want the oldest nightly %q",
			res.Prune.Removed[0], filepath.Base(older[len(older)-1]))
	}
}

// copyArchive writes a real archive into dir with a chosen class and
// timestamp, so retention has history to act on.
func copyArchive(t *testing.T, r *rig, dir string, class archive.Class, at time.Time) string {
	t.Helper()
	srcs, _, err := BuildSources(r.engine.cfg.Manifest, r.engine.cfg.Renderer, r.engine.cfg.Spec.Spec.Paths)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := archive.Create(context.Background(), archive.Options{
		Dir: r.paths.Backups, Instance: "pz-main", Class: class, Reason: "fixture",
		Sources: srcs, Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	return ref.Path
}

// TestBuildSourcesPlacesEveryDeclaredPathUnderItsOwnRoot.
func TestBuildSourcesPlacesEveryDeclaredPathUnderItsOwnRoot(t *testing.T) {
	r := newRig(t)
	srcs, notes, err := BuildSources(r.engine.cfg.Manifest, r.engine.cfg.Renderer,
		r.engine.cfg.Spec.Spec.Paths)
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("%d sources, want one: every Project Zomboid backup path is under .Data", len(srcs))
	}
	s := srcs[0]
	if s.Prefix != "data/" {
		t.Errorf("prefix = %q, want data/", s.Prefix)
	}
	want := []string{"Server", "Saves/Multiplayer/gvm", "Saves/Multiplayer/gvm_player", "db"}
	if !slices.Equal(s.Include, want) {
		t.Errorf("include = %v, want %v", s.Include, want)
	}
	if !slices.Equal(s.IncludeOptional, []string{"Lua"}) {
		t.Errorf("include_optional = %v", s.IncludeOptional)
	}
	// The absolute excludes are relativised; the bare pattern is carried
	// through as-is and applied to every root.
	for _, pat := range []string{"steamapps/workshop/**", "server-console.txt", "**/*.lock"} {
		if !slices.Contains(s.Exclude, pat) {
			t.Errorf("exclude %v is missing %q", s.Exclude, pat)
		}
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none for a manifest whose paths all place", notes)
	}
}

// TestAnIncludeUnderNoRootIsRefusedAndNotWarnedAbout.
//
// Section 13.2's reason, in its own words: the Paper-family `_nether` and
// `_the_end` siblings exist because "a backup routine that only archives
// <level>/ loses two dimensions there". A path that silently did not make it
// into the archive is discovered as a missing dimension months later, so it is
// an error.
func TestAnIncludeUnderNoRootIsRefusedAndNotWarnedAbout(t *testing.T) {
	r := newRig(t)
	m := shippedManifest(t, "projectzomboid")
	m.Spec.Backup.Include = append(m.Spec.Backup.Include, "/etc/somewhere/else")
	_, _, err := BuildSources(m, r.engine.cfg.Renderer, r.engine.cfg.Spec.Spec.Paths)
	if !errors.Is(err, ErrUnplaced) {
		t.Fatalf("BuildSources error = %v, want ErrUnplaced", err)
	}
	if !strings.Contains(err.Error(), "quietly skips part of a world") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if !strings.Contains(err.Error(), "data=") {
		t.Errorf("the refusal does not list the roots it tried: %v", err)
	}
}

// TestAnOptionalIncludeUnderNoRootIsOnlyANote.
//
// Its whole contract is that its absence is a note rather than an error
// (section 8.2), and "under no root" is a stronger form of absent.
func TestAnOptionalIncludeUnderNoRootIsOnlyANote(t *testing.T) {
	r := newRig(t)
	m := shippedManifest(t, "projectzomboid")
	m.Spec.Backup.IncludeOptional = append(m.Spec.Backup.IncludeOptional, "/etc/somewhere/else")
	_, notes, err := BuildSources(m, r.engine.cfg.Renderer, r.engine.cfg.Spec.Spec.Paths)
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	if !containsSubstring(notes, "/etc/somewhere/else") {
		t.Errorf("notes = %v, want one naming the path that was left out", notes)
	}
}

// TestPlacementPicksTheMostSpecificRoot.
//
// A new instance's Data is INSIDE its Home (/srv/gvm/<n>/data under
// /srv/gvm/<n>), so a longest-match rule is the difference between an archive
// whose member paths say `data/Server` and one whose say `home/data/Server` --
// and a restore puts them back by those paths.
//
// The comparison is on path BOUNDARIES: /srv/gvm/pz must not be treated as
// containing /srv/gvm/pz-main, which is the near-miss that would produce an
// archive full of another instance's world.
func TestPlacementPicksTheMostSpecificRoot(t *testing.T) {
	roots := rootsOf(spec.Paths{
		Home:    "/srv/gvm/pz",
		Data:    "/srv/gvm/pz/data",
		Install: "/srv/gvm/pz/install",
		Logs:    "/var/log/gvm/pz",
	})
	cases := map[string]struct{ label, rel string }{
		"/srv/gvm/pz/data/Server": {"data", "Server"},
		"/srv/gvm/pz/install/bin": {"install", "bin"},
		"/srv/gvm/pz/other":       {"home", "other"},
		"/srv/gvm/pz/data":        {"data", "."},
		"/var/log/gvm/pz/console": {"logs", "console"},
	}
	for in, want := range cases {
		rt, rel, ok := place(roots, in)
		if !ok {
			t.Errorf("place(%q) found no root", in)
			continue
		}
		if rt.Label != want.label || rel != want.rel {
			t.Errorf("place(%q) = (%s, %q), want (%s, %q)", in, rt.Label, rel, want.label, want.rel)
		}
	}
	// The near miss.
	if _, _, ok := place(rootsOf(spec.Paths{Data: "/srv/gvm/pz"}), "/srv/gvm/pz-main/data"); ok {
		t.Error("/srv/gvm/pz was treated as containing /srv/gvm/pz-main")
	}
}

// TestSteamCMDsHomeIsNeverARoot.
//
// .Work is SteamCMD's per-instance HOME (D-30): a download cache and an
// appmanifest. Archiving it would put a rebuildable 8 GB of Steam state in
// every nightly, which is the same mistake the workshop exclusion exists to
// avoid.
func TestSteamCMDsHomeIsNeverARoot(t *testing.T) {
	roots := rootsOf(spec.Paths{Data: "/srv/gvm/pz/data", Work: "/var/lib/gvm/cache/steam/homes/pz"})
	for _, r := range roots {
		if strings.Contains(r.Path, "cache/steam") {
			t.Fatalf("%s is a backup root", r.Path)
		}
	}
	if _, _, ok := place(roots, "/var/lib/gvm/cache/steam/homes/pz/.steam"); ok {
		t.Error("a path under SteamCMD's HOME placed onto a backup root")
	}
}

// TestNestedRootsAreReportedRatherThanSilentlyDoubling.
//
// Two roots in use where one contains the other means a recursive include
// under the outer one stores the inner one's files twice, once under each
// prefix. No shipped manifest does it; saying so when it happens costs one
// note and saves a confusing 2x archive.
func TestNestedRootsAreReportedRatherThanSilentlyDoubling(t *testing.T) {
	r := newRig(t)
	paths := r.engine.cfg.Spec.Spec.Paths
	// Make Home the parent of Data and include something from each.
	paths.Home = path.Dir(paths.Data)
	m := shippedManifest(t, "projectzomboid")
	m.Spec.Backup.Include = append(m.Spec.Backup.Include, paths.Home+"/home-thing")
	_, notes, err := BuildSources(m, r.engine.cfg.Renderer, paths)
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	if !containsSubstring(notes, "would store those files under both prefixes") {
		t.Errorf("notes = %v, want one about the nesting", notes)
	}
}

// TestAManifestWithNoBackupIncludeIsRefused.
//
// An archive of nothing would satisfy archive-before-destroy while protecting
// nothing, which is exactly the shape internal/archive refuses one level down.
func TestAManifestWithNoBackupIncludeIsRefused(t *testing.T) {
	r := newRig(t)
	m := shippedManifest(t, "projectzomboid")
	m.Spec.Backup.Include = nil
	m.Spec.Backup.IncludeOptional = nil
	if _, _, err := BuildSources(m, r.engine.cfg.Renderer, r.engine.cfg.Spec.Spec.Paths); !errors.Is(err, ErrConfig) {
		t.Fatalf("BuildSources error = %v, want ErrConfig", err)
	}
}

// TestACancelledJobStillThaws.
//
// The case a naive implementation gets wrong. systemd's TimeoutStartSec
// cancels a job's context exactly when something still has to be sent to the
// game, and a thaw that gave up on a cancelled context would leave the freeze
// in place -- with only the out-of-process lease to save it.
func TestACancelledJobStillThaws(t *testing.T) {
	r := newRig(t, func(c *Config) {
		pz := shippedManifest(t, "projectzomboid")
		pz.Spec.Backup.Freeze.After = &spec.ShutdownStep{Via: spec.ViaControl, Send: "save-on"}
		c.Manifest = pz
		// A control channel that does not consult the context: a real one
		// would be mid-write on a socket. The claim under test is that the
		// engine still ASKS. The test below covers the other half -- what
		// happens when a real channel refuses the cancelled context.
		c.Channels.Control = &fakeControl{IgnoreCtx: true}
	})
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()

	if err := r.engine.Thaw(ctx); err != nil {
		t.Fatalf("Thaw on a cancelled context: %v", err)
	}
	ctl := r.engine.cfg.Channels.Control.(*fakeControl)
	if got := ctl.lines(); len(got) != 1 || got[0] != "save-on" {
		t.Fatalf("control got %q on a cancelled context, want save-on", got)
	}
	if r.fs.has(r.leasePath()) {
		t.Error("the lease was not cleared by a thaw that landed")
	}
}

// TestAThawThatFailedOnACancelledContextIsRetriedOnTheUndosBudget.
//
// The test above asserts the engine still asks. This one asserts what happens
// when the asking FAILS, which is what a real channel does on a cancelled
// context: rconctl.Exec does connect(ctx) and then conn.Execute(ctx, cmd).
//
// The job's step Undo runs on context.WithoutCancel with its own budget for
// exactly this case -- the unwind's comment says a cancelled job whose freeze
// was never thawed leaves a running server that has silently stopped saving --
// and that is the only retry there is in-process. So a thaw that could not be
// delivered must stay re-sendable: `thawed` has to mean "the rung landed", not
// "the rung was attempted". `save-on` to a server that is already saving is a
// documented no-op, so re-sending costs nothing and not sending it is the
// overnight outage D-31 exists for.
func TestAThawThatFailedOnACancelledContextIsRetriedOnTheUndosBudget(t *testing.T) {
	r := newRig(t, func(c *Config) {
		pz := shippedManifest(t, "projectzomboid")
		pz.Spec.Backup.Freeze.After = &spec.ShutdownStep{Via: spec.ViaControl, Send: "save-on"}
		c.Manifest = pz
		c.Channels.Control = &fakeControl{}
	})
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()

	if err := r.engine.Thaw(ctx); !errors.Is(err, ErrFreeze) {
		t.Fatalf("Thaw on a cancelled context = %v, want ErrFreeze: the rung was not delivered", err)
	}
	ctl := r.engine.cfg.Channels.Control.(*fakeControl)
	if got := ctl.lines(); len(got) != 0 {
		t.Fatalf("control recorded %q from a thaw that could not be delivered", got)
	}
	if !r.fs.has(r.leasePath()) {
		t.Fatal("the lease was cleared by a thaw that never landed; it is the only remaining " +
			"evidence that this world may not be saving")
	}

	// Now the unwind's detached context, which is the one call that can still
	// deliver save-on.
	if err := r.engine.Thaw(context.Background()); err != nil {
		t.Fatalf("the retry on a detached context: %v", err)
	}
	if got := ctl.lines(); len(got) != 1 || got[0] != "save-on" {
		t.Fatalf("control got %q after the retry, want exactly one save-on", got)
	}
	if r.fs.has(r.leasePath()) {
		t.Error("the lease survived a thaw that landed")
	}
	// And the latch now holds, so a third caller sends nothing.
	if err := r.engine.Thaw(context.Background()); err != nil {
		t.Fatalf("the third caller: %v", err)
	}
	if got := ctl.lines(); len(got) != 1 {
		t.Errorf("control got %q from a third caller, want the one send to stand", got)
	}
}

// TestATransientlyFailedThawIsRetriedWithoutACancellation.
//
// The likelier production shape, and the one that shows the defect is not
// about contexts at all: ThawResult.Thawed goes false on any undelivered rung.
// socket.Client.Do returns a *ProtocolError for an ev:error frame with the
// connection still alive, and a `via: control` rung can hit a momentary
// !Ready(). Either way the next attempt would have worked.
func TestATransientlyFailedThawIsRetriedWithoutACancellation(t *testing.T) {
	r := newRig(t, func(c *Config) {
		pz := shippedManifest(t, "projectzomboid")
		pz.Spec.Backup.Freeze.After = &spec.ShutdownStep{Via: spec.ViaControl, Send: "save-on"}
		c.Manifest = pz
		c.Channels.Control = &fakeControl{Fail: errInjected, FailOnce: true}
	})
	ctx := context.Background()
	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Thaw(ctx); !errors.Is(err, ErrFreeze) {
		t.Fatalf("the first thaw = %v, want ErrFreeze", err)
	}
	if !r.fs.has(r.leasePath()) {
		t.Fatal("the lease was cleared by a thaw that never landed")
	}
	if err := r.engine.Thaw(ctx); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	ctl := r.engine.cfg.Channels.Control.(*fakeControl)
	if got := ctl.lines(); len(got) != 1 || got[0] != "save-on" {
		t.Fatalf("control got %q, want the retry to have re-sent save-on", got)
	}
	if r.fs.has(r.leasePath()) {
		t.Error("the lease survived a thaw that landed")
	}
}

// TestAFastSnapshotArchivesFromTheCloneSoTheFreezeEndsFirst.
//
// The reflink and btrfs families exist to make the freeze a metadata copy
// instead of a whole archive, and the ordering that delivers that is
// clone -> THAW -> tar from the clone. The assertion is on the bytes: a file
// that exists only inside the clone turns up in the archive, which is only
// possible if the archive was taken from the clone -- and the clone is taken
// before the thaw while the archive is taken after it.
//
// The two Linux-only operations are injected, which is why this runs on the
// development machine at all. The ordering is the part that can be wrong.
func TestAFastSnapshotArchivesFromTheCloneSoTheFreezeEndsFirst(t *testing.T) {
	var leaseArmedDuringClone bool
	r := newRig(t, func(c *Config) {
		c.Probe = func([]string, string) (Capability, error) {
			return Capability{FSType: "xfs", Reflink: true}, nil
		}
		c.Clone = nil // set below, once the rig's paths exist
	})
	r.engine.cfg.Probe = func([]string, string) (Capability, error) {
		return Capability{FSType: "xfs", Reflink: true}, nil
	}
	r.engine.cfg.Clone = func(ctx context.Context, m Method, src, dst string) ([]string, error) {
		if m != MethodReflink {
			t.Errorf("clone method = %q, want %q", m, MethodReflink)
		}
		leaseArmedDuringClone = r.fs.has(r.leasePath())
		// A clone is milliseconds; stand in for that so the stall figure has
		// something in it that is clearly not the archive.
		r.clock.Advance(4 * time.Millisecond)
		if err := copyTree(src, dst); err != nil {
			return nil, err
		}
		// A file that exists ONLY in the clone, inside one of the manifest's
		// own include paths. If it reaches the archive, the archive was
		// taken from the clone.
		return nil, os.WriteFile(filepath.Join(filepath.FromSlash(dst), "Server", "from-the-clone"),
			[]byte("only in the clone"), 0o644)
	}

	ctx := context.Background()
	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	res := r.engine.Result()
	if res.Snapshot.Method != MethodReflink {
		t.Fatalf("method = %q, want %q", res.Snapshot.Method, MethodReflink)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	ref, err := r.engine.Snapshot(ctx, archive.ClassRoutine, "routine")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !leaseArmedDuringClone {
		t.Error("the clone was taken with no freeze lease armed")
	}
	if r.fs.has(r.leasePath()) {
		t.Error("the lease is still armed after Snapshot")
	}

	rep, err := archive.Verify(ctx, ref.Path, archive.VerifyOptions{Mode: archive.ModeDeep})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	var fromClone bool
	for _, m := range rep.Manifest.Members {
		if strings.HasSuffix(m.Path, "from-the-clone") {
			fromClone = true
		}
	}
	if !fromClone {
		t.Fatal("the archive does not contain the clone-only file, so it was taken from the live " +
			"tree and the freeze covered the whole archive")
	}
	// The clone is removed on the way out; the startup sweep is only the net
	// under a SIGKILL. Asserted through the filesystem seam, because the
	// injected clone above wrote real directories while the engine's
	// housekeeping goes through the fake -- and what is under test is that the
	// engine asked.
	if !r.fs.askedToRemoveAll(r.engine.plan.Clone) {
		t.Errorf("the snapshot clone %s was never removed; a 2.4 GB copy of the world left "+
			"behind once a week fills a VPS in a month", r.engine.plan.Clone)
	}
}

// TestACloneRefusedAtTheLastMomentExtendsTheLeaseBeforeArchivingDirectly.
//
// The capability answer can change between the probe and the attempt: a btrfs
// path that is not really a subvolume, a kernel that refuses FICLONE on this
// particular file. Falling back to a direct archive is right -- but a direct
// archive happens INSIDE the freeze, and the lease was armed with a budget
// that assumed a four-millisecond clone. Extending it first is the difference
// between a slow backup and a thaw in the middle of the tar, which
// archive.Create would then report as ErrDrifted.
func TestACloneRefusedAtTheLastMomentExtendsTheLeaseBeforeArchivingDirectly(t *testing.T) {
	// A world large enough that a direct archive of it is measurable against
	// the throughput floor, because the extension this test is about is
	// derived from exactly that: a four-kilobyte world does not need one and
	// correctly does not get one.
	r := newRig(t, func(c *Config) { c.FloorMBps = 1 })
	if err := os.WriteFile(
		filepath.FromSlash(r.root+"/data/Saves/Multiplayer/gvm/big.bin"),
		[]byte(strings.Repeat("w", 3<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	r.engine.cfg.Probe = func([]string, string) (Capability, error) {
		return Capability{FSType: "btrfs", Btrfs: true, Subvolumes: true}, nil
	}
	r.engine.cfg.Clone = func(context.Context, Method, string, string) ([]string, error) {
		return nil, ErrNoClone
	}
	ctx := context.Background()
	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.engine.Result().Snapshot.Method; got != MethodBtrfs {
		t.Fatalf("method = %q, want %q", got, MethodBtrfs)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.engine.Snapshot(ctx, archive.ClassRoutine, "routine"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	res := r.engine.Result()
	if !res.Snapshot.Fallback || res.Snapshot.Method != MethodDirect {
		t.Fatalf("snapshot = %+v, want a recorded fallback to direct", res.Snapshot)
	}

	// Two lease documents were written: the armed one and the extended one.
	bodies := r.fs.wroteBodies(r.leasePath())
	if len(bodies) != 2 {
		t.Fatalf("%d lease documents were written, want the arm and the extension", len(bodies))
	}
	first, err := ParseLease(strings.NewReader(string(bodies[0])))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseLease(strings.NewReader(string(bodies[1])))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Deadline.After(first.Deadline) {
		t.Fatalf("the lease deadline went from %v to %v; a direct archive inside a clone-sized "+
			"budget would be thawed mid-tar", first.Deadline, second.Deadline)
	}
	if !containsSubstring(r.notes, "freeze lease was extended") {
		t.Errorf("notes = %v, want one recording the extension", r.notes)
	}
}

// TestAFailedLeaseExtensionFailsTheBackupRatherThanRacingTheRunner.
//
// If the lease cannot be extended to cover a direct archive, the honest thing
// is to stop: continuing would mean the runner re-enables autosave partway
// through a read that archive.Create is checksumming in two passes, and the
// result is a named ErrDrifted at best.
func TestAFailedLeaseExtensionFailsTheBackupRatherThanRacingTheRunner(t *testing.T) {
	r := newRig(t)
	r.engine.cfg.Probe = func([]string, string) (Capability, error) {
		return Capability{FSType: "xfs", Reflink: true}, nil
	}
	r.engine.cfg.Clone = func(context.Context, Method, string, string) ([]string, error) {
		return nil, ErrNoClone
	}
	ctx := context.Background()
	if err := r.engine.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	// The extension is the next write of the lease.
	r.fs.FailWrite = "freeze.lease"
	if _, err := r.engine.Snapshot(ctx, archive.ClassRoutine, "routine"); !errors.Is(err, ErrFreeze) {
		t.Fatalf("Snapshot error = %v, want ErrFreeze", err)
	}
}

// copyTree copies src to dst, which is what a fake reflink does: the real one
// shares the extents instead, and the difference is invisible from here.
func copyTree(src, dst string) error {
	src, dst = filepath.FromSlash(src), filepath.FromSlash(dst)
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// consoleFunc adapts a function to ConsoleSender.
type consoleFunc func(ctx context.Context, actor, line string) (uint64, error)

func (f consoleFunc) Send(ctx context.Context, actor, line string) (uint64, error) {
	return f(ctx, actor, line)
}
