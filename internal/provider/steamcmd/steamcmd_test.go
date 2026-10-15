package steamcmd

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The driver end to end, on Windows, against an in-memory SteamCMD.
//
// What makes that worth anything is that the fake reads the runscript gvm
// generated and writes a real appmanifest, so Plan -> Authorize -> Apply ->
// post-verify is the same chain the box runs, with only the process boundary
// replaced.

const pzApp = 380870

type harness struct {
	t          *testing.T
	fake       *Fake
	shims      *memShimFS
	driver     *Provider
	ctx        *provider.Ctx
	installDir string
	workDir    string
	slept      []time.Duration
}

func newHarness(t *testing.T, managed bool, opts ...Option) *harness {
	t.Helper()
	base := t.TempDir()
	install := filepath.Join(base, "opt", "pzserver")
	data := filepath.Join(base, "home", "pzserver", "Zomboid")
	home := filepath.Join(base, "home", "pzserver")
	work := filepath.Join(base, "var", "lib", "gvm", "cache", "steam", "homes", "pz-main")
	steamDir := filepath.Join(base, "opt", "gvm", "steamcmd")
	for _, d := range []string{install, data, home, work, steamDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(steamDir, "steamcmd.sh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	paths, err := provider.NewPaths(spec.Paths{Install: install, Data: data, Home: home, Work: work})
	if err != nil {
		t.Fatal(err)
	}
	runAs, err := provider.NewUnixIdent("gvm-pz-main", "gvm-pz-main", 2001, 2001)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := provider.NewStore(filepath.Join(base, "blobs"))
	if err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, fake: NewFake(), shims: newMemShimFS(), installDir: install, workDir: work}
	h.fake.Add(pzApp, FakeApp{
		Name:    "Project Zomboid Dedicated Server",
		BuildID: "18452901",
		Bytes:   1417183232,
		Files: map[string]FakeFile{
			"start-server.sh":  {Data: []byte("#!/bin/sh\nexec ./ProjectZomboid64 \"$@\"\n"), Mode: 0o755},
			"ProjectZomboid64": {Data: []byte("ELF"), Mode: 0o755},
		},
	})
	// The sdk target, so the shims have something real to point at.
	for _, bits := range []string{"32", "64"} {
		if err := h.shims.WriteFile(steamDir+"/linux"+bits+"/steamclient.so",
			[]byte("ELF"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	baseOpts := []Option{
		WithRunner(h.fake),
		WithSteamCMD(bin, steamDir),
		WithTreeFS(h.fake.TreeFS),
		WithShimFS(h.shims),
		WithSleep(func(ctx context.Context, d time.Duration) error {
			h.slept = append(h.slept, d)
			return ctx.Err()
		}),
	}
	h.driver = New(append(baseOpts, opts...)...)

	h.ctx = &provider.Ctx{
		Name:           "pz-main",
		Manifest:       pzManifest(),
		Vars:           spec.VarValues{},
		Ports:          spec.PortValues{"game": 16261},
		Paths:          paths,
		RunAs:          runAs,
		Blobs:          blobs,
		ManagedInstall: managed,
		Clock:          func() time.Time { return time.Unix(1762000000, 0).UTC() },
	}
	return h
}

func pzManifest() *spec.Manifest {
	return &spec.Manifest{
		APIVersion: spec.APIVersionV1,
		Kind:       spec.KindGame,
		Metadata:   spec.GameMeta{ID: "projectzomboid", Revision: 7},
		Spec: spec.GameSpec{
			DriverABI: spec.BuiltDriverABI,
			Provider: spec.ProviderSpec{
				Kind: spec.ProviderSteamCMD,
				SteamCMD: &spec.SteamCMDSpec{
					InstallAppID: pzApp,
					Anonymous:    true,
					Branch:       DefaultBranch,
					Platform:     spec.PlatformLinux,
					ValidateOn:   []spec.Intent{spec.IntentInstall, spec.IntentRepair},
					Sentinel:     spec.Sentinel{Path: "start-server.sh", Executable: true},
					Verify:       spec.Verify{AppManifest: true, ByteParity: true, SingleACF: true},
				},
			},
			Runtime: spec.RuntimeSpec{Arch: "native", Env: map[string]string{"HOME": "{{ .Home }}"}},
		},
	}
}

// plan builds and authorizes a plan the way the apply pipeline does.
func (h *harness) plan(in provider.Intent) *provider.Plan {
	h.t.Helper()
	pl, err := h.driver.Plan(context.Background(), h.ctx, in)
	if err != nil {
		h.t.Fatalf("Plan(%s): %v", in, err)
	}
	if err := pl.Authorize(h.ctx.ManagedInstall); err != nil {
		h.t.Fatalf("Authorize: %v", err)
	}
	return pl
}

func (h *harness) apply(pl *provider.Plan) (*provider.Applied, error) {
	h.t.Helper()
	return h.driver.Apply(context.Background(), h.ctx, pl, provider.Progress{})
}

func (h *harness) script(t *testing.T, purpose string) string {
	t.Helper()
	for _, inv := range h.fake.Runs() {
		if inv.Purpose == purpose {
			return string(inv.Script)
		}
	}
	t.Fatalf("no %s invocation; runs = %d", purpose, len(h.fake.Runs()))
	return ""
}

func TestInstallPlanThenApply(t *testing.T) {
	h := newHarness(t, true)

	pl := h.plan(provider.IntentInstall)
	if len(pl.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v", pl.Artifacts)
	}
	art := pl.Artifacts[0]
	if got, want := string(art.Digest), "buildid:380870:18452901"; got != want {
		t.Errorf("digest = %q, want %q", got, want)
	}
	if art.Digest.Verifiable() {
		t.Error("a steamcmd artifact claims to be hashable; SteamCMD writes its own bytes and " +
			"the blob store must refuse to serve this")
	}
	if art.DestRel != "." {
		t.Errorf("DestRel = %q; the install tree itself is what steamcmd writes", art.DestRel)
	}
	if pl.Bytes != 1417183232 {
		t.Errorf("Bytes = %d; the disk preflight has nothing to check against", pl.Bytes)
	}
	if pl.Destructive {
		t.Error("an install into an empty tree is not destructive")
	}

	applied, err := h.apply(pl)
	if err != nil {
		t.Fatalf("Apply: %v\nsteps: %v", err, applied.Steps)
	}
	if !applied.Changed {
		t.Error("Changed is false after a download")
	}
	if applied.Version.ID != "18452901" {
		t.Errorf("version = %q", applied.Version.ID)
	}
	if !strings.Contains(applied.Note, "verified") {
		t.Errorf("note = %q", applied.Note)
	}

	// Install is on validate_on, and the keyword is positional.
	update := h.script(t, "app_update")
	if !strings.Contains(update, "app_update 380870 validate") {
		t.Errorf("update script =\n%s", update)
	}
	// Valve's ordering, in the bytes the sandbox was handed.
	if !strings.Contains(update, "force_install_dir "+h.installDir+"\nlogin anonymous\n") {
		t.Errorf("force_install_dir does not precede the login:\n%s", update)
	}

	// The post-install shims are applied, and they land in the game's HOME.
	sdk := h.ctx.Paths.Home.Path() + "/.steam/sdk64/steamclient.so"
	if _, err := h.shims.Lstat(sdk); err != nil {
		t.Errorf("%s was not created: %v", sdk, err)
	}
}

// The invocation the sandbox receives is the D-16 contract: the instance user,
// the per-instance HOME, a 0600 script at the path section 13.5 names, and a
// finite budget.
func TestTheInvocationCarriesTheSandboxContract(t *testing.T) {
	h := newHarness(t, true)
	if _, err := h.apply(h.plan(provider.IntentInstall)); err != nil {
		t.Fatal(err)
	}

	runs := h.fake.Runs()
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want an app_info then an app_update", len(runs))
	}
	check, update := runs[0], runs[1]
	if check.Purpose != "app_info" || update.Purpose != "app_update" {
		t.Fatalf("purposes = %q, %q", check.Purpose, update.Purpose)
	}
	if got, want := update.ScriptPath, "/var/lib/gvm/instances/pz-main/steam/update.txt"; got != want {
		t.Errorf("script path = %q, want %q", got, want)
	}
	if got, want := check.ScriptPath, "/var/lib/gvm/instances/pz-main/steam/check.txt"; got != want {
		t.Errorf("check script path = %q, want %q", got, want)
	}
	for _, inv := range runs {
		if inv.ScriptMode != 0o600 {
			t.Errorf("%s script mode = %04o, want 0600", inv.Purpose, inv.ScriptMode)
		}
		if inv.RunAs.UID() != 2001 {
			t.Errorf("%s runs as uid %d", inv.Purpose, inv.RunAs.UID())
		}
		if got := inv.Env["HOME"]; got != h.workDir {
			t.Errorf("%s HOME = %q, want the per-instance steam home %q (D-30)",
				inv.Purpose, got, h.workDir)
		}
		if inv.Budget <= 0 {
			t.Errorf("%s has no time budget", inv.Purpose)
		}
		if inv.Binary == "" || len(inv.Args) != 2 || inv.Args[0] != "+runscript" {
			t.Errorf("%s argv = %q %v", inv.Purpose, inv.Binary, inv.Args)
		}
	}
	if update.Budget != DefaultUpdateBudget || check.Budget != DefaultCheckBudget {
		t.Errorf("budgets = %v, %v", update.Budget, check.Budget)
	}
}

// D-43: on most nights nothing stops, nothing downloads and nothing pages
// anyone.
func TestAnUpdateWithNoBuildDifferenceIsSkipped(t *testing.T) {
	h := newHarness(t, true)
	if err := h.fake.Seed(h.installDir, pzApp); err != nil {
		t.Fatal(err)
	}

	pl := h.plan(provider.IntentUpdate)
	if pl.Writes() || pl.Destructive {
		t.Fatalf("a no-op nightly planned %d artifacts (destructive %t)",
			len(pl.Artifacts), pl.Destructive)
	}
	if !strings.Contains(pl.Reason, "is current") || !strings.Contains(pl.Reason, "nothing stops") {
		t.Errorf("reason = %q", pl.Reason)
	}

	applied, err := h.apply(pl)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Changed {
		t.Error("Changed is true although nothing ran")
	}
	if strings.TrimSpace(applied.Note) == "" {
		t.Error("a skipped nightly's whole output is that sentence (D-43)")
	}
	// The check ran; the update did not.
	for _, inv := range h.fake.Runs() {
		if inv.Purpose == "app_update" {
			t.Fatal("a no-op nightly ran app_update")
		}
	}
}

// And when there IS a build difference, the update still never validates: that
// is the whole of D-32's first half.
func TestAnUpdateNeverValidates(t *testing.T) {
	h := newHarness(t, true)
	if err := h.fake.Seed(h.installDir, pzApp); err != nil {
		t.Fatal(err)
	}
	h.fake.SetBuild(pzApp, "18461233")

	pl := h.plan(provider.IntentUpdate)
	if !pl.Writes() {
		t.Fatalf("a build difference planned nothing: %q", pl.Reason)
	}
	if !strings.Contains(pl.Reason, "18452901 -> 18461233") {
		t.Errorf("reason = %q", pl.Reason)
	}
	if pl.Destructive {
		t.Error("a plain app_update is not destructive")
	}

	if _, err := h.apply(pl); err != nil {
		t.Fatal(err)
	}
	update := h.script(t, "app_update")
	if strings.Contains(update, "validate") {
		t.Fatalf("the nightly update script validates, which overwrites every customised file "+
			"(D-32):\n%s", update)
	}
}

// Section 13.5's adopted paragraph, verbatim in behaviour: the check runs, the
// answer is recorded, the server is never stopped, and the message names the
// one command that changes it.
func TestAnAdoptedInstanceIsToldToPromote(t *testing.T) {
	h := newHarness(t, false)
	if err := h.fake.Seed(h.installDir, pzApp); err != nil {
		t.Fatal(err)
	}
	h.fake.SetBuild(pzApp, "18461233")

	pl := h.plan(provider.IntentUpdate)
	if pl.Writes() || pl.Destructive {
		t.Fatal("a plan for an adopted instance would write into an install gvm does not own")
	}
	for _, want := range []string{"18452901 -> 18461233", "adopted", "not available in this build"} {
		if !strings.Contains(pl.Reason, want) {
			t.Errorf("reason = %q, missing %q", pl.Reason, want)
		}
	}

	applied, err := h.apply(pl)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Changed {
		t.Error("Changed is true on an adopted instance")
	}
	for _, inv := range h.fake.Runs() {
		if inv.Purpose == "app_update" {
			t.Fatal("an adopted instance was handed to app_update")
		}
	}
}

func TestAdoptedInstallAndRepairAreRefusedAtTheTypeLevel(t *testing.T) {
	h := newHarness(t, false)
	for _, in := range []provider.Intent{provider.IntentInstall, provider.IntentRepair} {
		t.Run(string(in), func(t *testing.T) {
			_, err := h.driver.Plan(context.Background(), h.ctx, in)
			if !errors.Is(err, provider.ErrAdopted) {
				t.Fatalf("err = %v, want ErrAdopted", err)
			}
			if !provider.Terminal(err) {
				t.Error("the refusal is not terminal, so a job would retry a flag it cannot change")
			}
			if !strings.Contains(err.Error(), "not available in this build") {
				t.Errorf("the refusal does not say what to do: %v", err)
			}
			if len(h.fake.Runs()) != 0 {
				t.Error("the refusal still ran steamcmd")
			}
		})
	}
}

// D-32's response to a failed verification: do not auto-remediate, report it,
// and tell the operator about --repair. The record of what was downloaded
// survives alongside the error.
func TestPostVerifyFailureIsReportedAndNotRemediated(t *testing.T) {
	h := newHarness(t, true)
	h.fake.Add(pzApp, FakeApp{
		Name:       "Project Zomboid Dedicated Server",
		BuildID:    "18452901",
		Bytes:      1417183232,
		StateFlags: 1062,
		Files: map[string]FakeFile{
			"start-server.sh": {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
		},
	})

	applied, err := h.apply(h.plan(provider.IntentInstall))
	if !errors.Is(err, ErrStateFlags) {
		t.Fatalf("err = %v, want ErrStateFlags", err)
	}
	if applied == nil {
		t.Fatal("the record of what was downloaded was lost with the error")
	}
	if !strings.Contains(applied.Note, "--repair") {
		t.Errorf("note = %q", applied.Note)
	}
	if !strings.Contains(applied.Note, "overwrites files the operator customised") {
		t.Errorf("the note does not explain why gvm will not fix it itself: %q", applied.Note)
	}
	// Exactly one app_update: no silent `validate` retry.
	updates := 0
	for _, inv := range h.fake.Runs() {
		if inv.Purpose == "app_update" {
			updates++
		}
	}
	if updates != 1 {
		t.Errorf("app_update ran %d times; a verification failure must not be auto-remediated",
			updates)
	}
}

// The retry ladder: three attempts, 30s then 2m, and a success on the last one
// is a success.
func TestTheRetryLadder(t *testing.T) {
	h := newHarness(t, true)
	fail := string(fixture(t, "steamcmd_0x602.stdout"))
	h.fake.Outcomes = []FakeOutcome{
		{Stdout: fail, Exit: Exit{Code: 0}},
		{Stdout: fail, Exit: Exit{Code: 8}},
	}

	applied, err := h.apply(h.plan(provider.IntentInstall))
	if err != nil {
		t.Fatalf("the third attempt should have succeeded: %v\nsteps: %v", err, applied.Steps)
	}
	updates := 0
	for _, inv := range h.fake.Runs() {
		if inv.Purpose == "app_update" {
			updates++
		}
	}
	if updates != 3 {
		t.Errorf("app_update ran %d times, want 3", updates)
	}
	want := []time.Duration{30 * time.Second, 2 * time.Minute}
	if len(h.slept) != len(want) {
		t.Fatalf("waits = %v, want %v", h.slept, want)
	}
	for i := range want {
		if h.slept[i] != want[i] {
			t.Errorf("wait %d = %v, want %v", i, h.slept[i], want[i])
		}
	}
}

// A credential failure is terminal, and retrying one is worse than pointless:
// Steam rate-limits and then locks the account.
func TestAHardFailureStopsAfterOneAttempt(t *testing.T) {
	h := newHarness(t, true)
	h.fake.Outcomes = []FakeOutcome{{
		Stdout: string(fixture(t, "steamcmd_no_subscription.stdout")),
		Exit:   Exit{Code: 1},
	}}

	_, err := h.apply(h.plan(provider.IntentInstall))
	if !errors.Is(err, ErrHardFail) {
		t.Fatalf("err = %v, want ErrHardFail", err)
	}
	updates := 0
	for _, inv := range h.fake.Runs() {
		if inv.Purpose == "app_update" {
			updates++
		}
	}
	if updates != 1 {
		t.Errorf("app_update ran %d times after a hard failure", updates)
	}
	if len(h.slept) != 0 {
		t.Errorf("the ladder waited %v before giving up on a terminal failure", h.slept)
	}
}

// The central trap, at the driver level: steamcmd prints Error! 0x602, exits 0,
// and leaves a tree that looks plausible. Apply must still fail.
func TestExitZeroAfterAnErrorIsAFailedApply(t *testing.T) {
	h := newHarness(t, true, WithAttempts(1))
	h.fake.Outcomes = []FakeOutcome{{
		Stdout:      string(fixture(t, "steamcmd_0x602.stdout")),
		Exit:        Exit{Code: 0},
		Materialize: true, // the tree is left looking complete
	}}

	applied, err := h.apply(h.plan(provider.IntentInstall))
	if err == nil {
		t.Fatal("Apply reported success for a run that printed `Error! … 0x602` and exited 0; " +
			"this is the install that boots broken and takes an evening to diagnose")
	}
	if !errors.Is(err, ErrRetry) {
		t.Errorf("err = %v, want ErrRetry", err)
	}
	if applied.Changed {
		t.Error("Changed is true after a failed run")
	}
	// And the appmanifest it left behind verifies, which is exactly why the
	// output has to be read as well as the disk.
	if _, verr := h.driver.verify(h.ctx, pzManifest().Spec.Provider.SteamCMD, ""); verr != nil {
		t.Fatalf("the fixture is meant to leave a tree that passes verification, or this test "+
			"does not prove the output was consulted: %v", verr)
	}
}

// The other half of the central trap: steamcmd prints Success, exits 0, and
// moves nothing.
//
// classify.go reads the output and verify.go reads the tree, and until the
// installed build is compared against the build the plan was authorized to
// fetch, "we downloaded everything" and "we downloaded nothing" are the same
// answer. If the cause is deterministic -- a bad ReadWritePaths derivation, a
// runscript the unit cannot open, a re-exec that drops +runscript -- every
// nightly stops the server, reports success, restarts it on the old build and
// repeats.
func TestARunThatDidNotMoveTheBuildIsAFailedApply(t *testing.T) {
	h := newHarness(t, true, WithAttempts(1))
	if err := h.fake.Seed(h.installDir, pzApp); err != nil {
		t.Fatal(err)
	}
	h.fake.SetBuild(pzApp, "18461233")

	pl := h.plan(provider.IntentUpdate)
	h.fake.Outcomes = []FakeOutcome{{
		Stdout:      "Success! App '380870' fully installed.\n",
		Exit:        Exit{Code: 0},
		Materialize: false, // the tree keeps the build it already had
	}}

	applied, err := h.apply(pl)
	if !errors.Is(err, ErrWrongBuild) {
		t.Fatalf("err = %v, want ErrWrongBuild", err)
	}
	if applied == nil {
		t.Fatal("the record of the attempt was lost with the error")
	}
	if applied.Changed {
		t.Error("Changed is true although the install is still on the old build")
	}
	if len(applied.Wrote) != 0 {
		t.Errorf("Wrote = %v; nothing moved, so nothing was written", applied.Wrote)
	}
	if strings.Contains(applied.Note, "installed and verified") {
		t.Errorf("the note reports a successful update of a build that never arrived: %q",
			applied.Note)
	}
	for _, want := range []string{"18452901", "18461233"} {
		if !strings.Contains(applied.Note, want) {
			t.Errorf("the note does not name build %s: %q", want, applied.Note)
		}
	}
}

// And the unrecognised shape of the same failure: steamcmd started, read no
// commands and exited 0, so there is no outcome line and no progress state.
//
// This is what an unreadable runscript produces -- the banner and the API load
// are printed whatever happens -- and it is caught one rung earlier than the
// build comparison, by the classifier, because all three of the signals a real
// app_update leaves behind are absent at once.
func TestAnExitZeroRunThatNeverRanAppUpdateIsAFailure(t *testing.T) {
	h := newHarness(t, true, WithAttempts(1))
	if err := h.fake.Seed(h.installDir, pzApp); err != nil {
		t.Fatal(err)
	}
	h.fake.SetBuild(pzApp, "18461233")

	pl := h.plan(provider.IntentUpdate)
	h.fake.Outcomes = []FakeOutcome{{
		Stdout:      "",
		Exit:        Exit{Code: 0},
		Materialize: false,
	}}

	applied, err := h.apply(pl)
	if err == nil {
		t.Fatal("Apply reported success for a run that printed no outcome line, saw no download " +
			"state and left the tree untouched")
	}
	if !errors.Is(err, ErrRetry) {
		t.Errorf("err = %v, want ErrRetry", err)
	}
	if applied.Changed {
		t.Error("Changed is true after a run that did nothing")
	}
	if strings.Contains(applied.Note, "verified") {
		t.Errorf("note = %q", applied.Note)
	}
}

// A build NEWER than the one the plan asked for still passes: Steam can publish
// again during a multi-hour 65 GB download, and failing that run would set
// NEEDS_REPAIR and recommend `--repair`, which validates and overwrites files
// the operator customised. It is recorded instead.
func TestABuildAheadOfThePlanPassesAndIsRecorded(t *testing.T) {
	h := newHarness(t, true, WithAttempts(1))
	if err := h.fake.Seed(h.installDir, pzApp); err != nil {
		t.Fatal(err)
	}
	h.fake.SetBuild(pzApp, "18461233")

	pl := h.plan(provider.IntentUpdate)
	// Steam publishes again while the download is running.
	h.fake.SetBuild(pzApp, "18470001")

	applied, err := h.apply(pl)
	if err != nil {
		t.Fatalf("a build newer than the plan's failed the apply: %v\nsteps: %v", err, applied.Steps)
	}
	if applied.Version.ID != "18470001" {
		t.Errorf("version = %q", applied.Version.ID)
	}
	joined := strings.Join(applied.Steps, "\n")
	if !strings.Contains(joined, "18461233") || !strings.Contains(joined, "newer") {
		t.Errorf("the record does not say the build overshot the plan: %v", applied.Steps)
	}
}

func TestRepairValidatesAndSaysSo(t *testing.T) {
	h := newHarness(t, true)
	if err := h.fake.Seed(h.installDir, pzApp); err != nil {
		t.Fatal(err)
	}

	pl := h.plan(provider.IntentRepair)
	if !pl.Destructive {
		t.Error("a repair validates, which overwrites customised files: that is destructive")
	}
	if strings.TrimSpace(pl.Reason) == "" {
		t.Error("a destructive plan with no reason is an audit row nobody can act on")
	}
	if !strings.Contains(strings.Join(pl.Steps, "\n"), "OVERWRITES") {
		t.Errorf("the steps do not warn about the overwrite: %v", pl.Steps)
	}
	if _, err := h.apply(pl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.script(t, "app_update"), "validate") {
		t.Error("a repair did not validate")
	}
}

func TestVerifyIntentIsReadOnlyAndNeverFailsTheApply(t *testing.T) {
	h := newHarness(t, true)

	// Nothing installed: verification fails, and the intent still works.
	pl := h.plan(provider.IntentVerify)
	if pl.Writes() || pl.Destructive {
		t.Fatal("a verify plan would write")
	}
	applied, err := h.apply(pl)
	if err != nil {
		t.Fatalf("a verify whose CHECK failed must not fail the Apply: %v", err)
	}
	if !strings.Contains(applied.Note, "FAILED verification") {
		t.Errorf("note = %q", applied.Note)
	}
	if len(h.fake.Runs()) != 0 {
		t.Error("the read-only intent ran steamcmd")
	}

	if err := h.fake.Seed(h.installDir, pzApp); err != nil {
		t.Fatal(err)
	}
	applied, err = h.apply(h.plan(provider.IntentVerify))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(applied.Note, "verified") || applied.Changed {
		t.Errorf("note = %q, changed = %t", applied.Note, applied.Changed)
	}
}

func TestPlanRefusals(t *testing.T) {
	t.Run("a manifest with no steamcmd block", func(t *testing.T) {
		h := newHarness(t, true)
		h.ctx.Manifest.Spec.Provider.SteamCMD = nil
		if _, err := h.driver.Plan(context.Background(), h.ctx, provider.IntentUpdate); !errors.Is(
			err, ErrManifest) {
			t.Fatalf("err = %v, want ErrManifest", err)
		}
	})
	t.Run("a manifest with no sentinel", func(t *testing.T) {
		h := newHarness(t, true)
		h.ctx.Manifest.Spec.Provider.SteamCMD.Sentinel = spec.Sentinel{}
		_, err := h.driver.Plan(context.Background(), h.ctx, provider.IntentUpdate)
		if !errors.Is(err, ErrManifest) {
			t.Fatalf("err = %v, want ErrManifest", err)
		}
		if !strings.Contains(err.Error(), "D-32") {
			t.Errorf("the refusal does not cite the decision: %v", err)
		}
	})
	t.Run("a game that needs a Steam account", func(t *testing.T) {
		h := newHarness(t, true)
		h.ctx.Manifest.Spec.Provider.SteamCMD.Anonymous = false
		_, err := h.driver.Plan(context.Background(), h.ctx, provider.IntentInstall)
		if !errors.Is(err, ErrCredentialed) {
			t.Fatalf("err = %v, want ErrCredentialed", err)
		}
	})
	t.Run("an intent that is not one", func(t *testing.T) {
		h := newHarness(t, true)
		_, err := h.driver.Plan(context.Background(), h.ctx, provider.Intent("reinstall"))
		if !errors.Is(err, provider.ErrInvalidPlan) {
			t.Fatalf("err = %v, want ErrInvalidPlan", err)
		}
	})
	t.Run("a branch Steam does not publish", func(t *testing.T) {
		h := newHarness(t, true)
		h.ctx.Manifest.Spec.Provider.SteamCMD.Branch = "b42-unstable"
		_, err := h.driver.Plan(context.Background(), h.ctx, provider.IntentUpdate)
		if !errors.Is(err, ErrNoSuchBranch) {
			t.Fatalf("err = %v, want ErrNoSuchBranch", err)
		}
	})
}

// The registered driver can read, and refuses to write, until something gives
// it a sandbox. Defaulting to os/exec instead would mean the first unwired call
// site ran a self-updating Valve binary as root (D-16).
func TestTheDefaultDriverRefusesToRunAnything(t *testing.T) {
	h := newHarness(t, true)
	bin, dir, err := h.driver.locate()
	if err != nil {
		t.Fatal(err)
	}
	bare := New(WithSteamCMD(bin, dir), WithTreeFS(h.fake.TreeFS))

	_, err = bare.Plan(context.Background(), h.ctx, provider.IntentUpdate)
	if !errors.Is(err, ErrNoRunner) {
		t.Fatalf("err = %v, want ErrNoRunner", err)
	}
	if !strings.Contains(err.Error(), "D-16") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// Read-only still works, because none of it executes anything.
	if err := h.fake.Seed(h.installDir, pzApp); err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Inspect(context.Background(), h.ctx); err != nil {
		t.Errorf("Inspect needs no sandbox: %v", err)
	}
	if _, err := bare.Observe(context.Background(), h.ctx); err != nil {
		t.Errorf("Observe needs no sandbox: %v", err)
	}
}

func TestApplyRefusesAPlanFromSomewhereElse(t *testing.T) {
	h := newHarness(t, true)
	// A plan with artifacts and no decision: it did not come from this Plan, so
	// Apply has no runscript and will not invent one.
	foreign := &provider.Plan{
		Intent: provider.IntentInstall,
		Reason: "hand-written",
		Artifacts: []provider.Artifact{{
			Digest:  provider.Digest("buildid:380870:18452901"),
			Source:  "steamcmd:app_update 380870",
			DestRel: ".",
		}},
	}
	if err := foreign.Authorize(true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.apply(foreign); !errors.Is(err, provider.ErrInvalidPlan) {
		t.Fatalf("err = %v, want ErrInvalidPlan", err)
	}
	if len(h.fake.Runs()) != 0 {
		t.Error("the refusal still ran steamcmd")
	}
}

func TestObserveIsCheapAndHonest(t *testing.T) {
	h := newHarness(t, true)

	// Nothing installed: not an error. Every instance starts here, and a poll
	// that errored for it would fail the dashboard every thirty seconds.
	obs, err := h.driver.Observe(context.Background(), h.ctx)
	if err != nil {
		t.Fatalf("Observe on an empty tree: %v", err)
	}
	if obs.Healthy || obs.Version.Known() {
		t.Errorf("observation = %+v", obs)
	}
	if !strings.Contains(obs.Detail, "nothing has been installed") {
		t.Errorf("detail = %q", obs.Detail)
	}
	if obs.ReadAt != time.Unix(1762000000, 0).UTC() {
		t.Errorf("ReadAt = %v; the clock is injectable so this is assertable", obs.ReadAt)
	}

	if err := h.fake.Seed(h.installDir, pzApp); err != nil {
		t.Fatal(err)
	}
	obs, err = h.driver.Observe(context.Background(), h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Healthy || obs.Version.ID != "18452901" {
		t.Errorf("observation = %+v", obs)
	}
}

// Observe reads the whole appmanifest and must use the whole appmanifest.
//
// The poll's cheapness is about reading ONE path instead of walking a 65 GB
// tree, not about dropping three fields out of the file it already parsed. A
// short download leaves StateFlags 4 with the byte pairs disagreeing -- it is
// exactly the state a failed update is designed to leave on disk, because D-32
// forbids auto-remediation -- and a poll that called that healthy would report
// green indefinitely while the job record said repair-needed.
func TestObserveJudgesTheBytePairsAndNotJustStateFlags(t *testing.T) {
	cases := []struct {
		name string
		app  FakeApp
		says string
	}{
		{
			name: "the download is short",
			app:  FakeApp{ShortBy: 500 << 20},
			says: "download is short",
		},
		{
			name: "the staging step is short",
			app:  FakeApp{StagedShortBy: 500 << 20},
			says: "staging step is short",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true)
			app := FakeApp{
				Name:          "Project Zomboid Dedicated Server",
				BuildID:       "18452901",
				Bytes:         1417183232,
				ShortBy:       tc.app.ShortBy,
				StagedShortBy: tc.app.StagedShortBy,
				Files: map[string]FakeFile{
					"start-server.sh": {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
				},
			}
			h.fake.Add(pzApp, app)
			if err := h.fake.Seed(h.installDir, pzApp); err != nil {
				t.Fatal(err)
			}

			obs, err := h.driver.Observe(context.Background(), h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Healthy {
				t.Errorf("Healthy is true for a short install: %+v", obs)
			}
			if !strings.Contains(obs.Detail, tc.says) {
				t.Errorf("detail = %q, want something about %q", obs.Detail, tc.says)
			}

			// The invariant that was actually broken: two components reading
			// the same single file must not reach opposite verdicts.
			ins, err := h.driver.Inspect(context.Background(), h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if ins.Integrity.Failed() == obs.Healthy {
				t.Errorf("Inspect says integrity failed = %t and Observe says healthy = %t for "+
					"one tree:\n  inspect: %v\n  observe: %s",
					ins.Integrity.Failed(), obs.Healthy, ins.Integrity.Problems, obs.Detail)
			}
		})
	}
}

func TestInspectReportsFindingsRatherThanFailing(t *testing.T) {
	h := newHarness(t, true)
	h.fake.Add(pzApp, FakeApp{
		Name: "Project Zomboid Dedicated Server", BuildID: "18452901", Bytes: 1 << 20,
		StateFlags: 1062,
		Files: map[string]FakeFile{
			"start-server.sh": {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
		},
	})
	if err := h.fake.Seed(h.installDir, pzApp); err != nil {
		t.Fatal(err)
	}

	ins, err := h.driver.Inspect(context.Background(), h.ctx)
	if err != nil {
		t.Fatalf("Inspect turned a finding into an error: %v", err)
	}
	if !ins.Present {
		t.Error("Present is false for a populated tree")
	}
	if !ins.Integrity.Checked || !ins.Integrity.Failed() {
		t.Errorf("integrity = %+v", ins.Integrity)
	}
	if err := ins.Sentinel.OK(); err != nil {
		t.Errorf("the sentinel is fine and reported %v", err)
	}
	if len(ins.HandEdits) != 0 {
		t.Error("hand edits were invented; SteamCMD publishes no per-file digests")
	}
	if !strings.Contains(ins.Integrity.Facts["hand_edits"], "validate") {
		t.Errorf("the reason hand edits are undetectable is not recorded: %v", ins.Integrity.Facts)
	}
}

func TestLocator(t *testing.T) {
	exists := func(paths ...string) func(string) (fs.FileInfo, error) {
		set := map[string]fs.FileMode{}
		for _, p := range paths {
			set[p] = 0o755
		}
		return func(p string) (fs.FileInfo, error) {
			mode, ok := set[p]
			if !ok {
				return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
			}
			return memShimInfo{name: p, e: &memEntry{mode: mode}}, nil
		}
	}
	pathEnv := strings.Join([]string{
		filepath.Join("/usr", "local", "bin"),
		filepath.Join("/usr", "bin"),
	}, string(filepath.ListSeparator))

	t.Run("the configured directory wins", func(t *testing.T) {
		want := filepath.Join("/opt", "custom", "steamcmd.sh")
		got, err := Locator{
			Dir:  filepath.Join("/opt", "custom"),
			PATH: pathEnv,
			Stat: exists(want, filepath.Join(DefaultSteamCMDDir, "steamcmd.sh")),
		}.Find()
		if err != nil || got != want {
			t.Fatalf("Find = %q, %v", got, err)
		}
	})
	t.Run("PATH is searched last", func(t *testing.T) {
		want := filepath.Join("/usr", "local", "bin", "steamcmd")
		got, err := Locator{PATH: pathEnv, Stat: exists(want)}.Find()
		if err != nil || got != want {
			t.Fatalf("Find = %q, %v", got, err)
		}
	})
	t.Run("a file with no execute bit is not it", func(t *testing.T) {
		p := filepath.Join(DefaultSteamCMDDir, "steamcmd.sh")
		stat := func(name string) (fs.FileInfo, error) {
			if name != p {
				return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
			}
			return memShimInfo{name: name, e: &memEntry{mode: 0o644}}, nil
		}
		if _, err := (Locator{Stat: stat}).Find(); !errors.Is(err, ErrNoSteamCMD) {
			t.Fatalf("err = %v, want ErrNoSteamCMD", err)
		}
	})
	t.Run("nothing anywhere", func(t *testing.T) {
		_, err := Locator{PATH: pathEnv, Stat: exists()}.Find()
		if !errors.Is(err, ErrNoSteamCMD) {
			t.Fatalf("err = %v", err)
		}
		// The message has to name where it looked and what to install, because
		// the i386 requirement is the non-obvious half: steamcmd itself is a
		// 32-bit binary even when every server on the box is 64-bit.
		for _, want := range []string{DefaultSteamCMDDir, "lib32gcc-s1", "i386"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not mention %q: %v", want, err)
			}
		}
	})
}

// A Plan is network-reads-only, and the one thing it does write is steamcmd's
// own cache -- which it deletes, because that cache is what makes the read
// wrong.
func TestThePlanPurgesTheAppInfoCache(t *testing.T) {
	h := newHarness(t, true)
	cache := filepath.Join(h.workDir, ".steam", "appcache", "appinfo.vdf")
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := h.driver.Plan(context.Background(), h.ctx, provider.IntentUpdate); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the stale appinfo cache survived the check, so app_info_print would keep " +
			"answering with the old buildid forever")
	}
}

func TestAnInstanceWithNoSteamHomeIsRefused(t *testing.T) {
	h := newHarness(t, true)
	paths, err := provider.NewPaths(spec.Paths{
		Install: h.installDir,
		Data:    filepath.Dir(h.installDir),
		Home:    filepath.Dir(h.installDir),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.ctx.Paths = paths
	_, err = h.driver.Plan(context.Background(), h.ctx, provider.IntentUpdate)
	if !errors.Is(err, ErrManifest) {
		t.Fatalf("err = %v; without a per-instance HOME two steamcmd would share ~/.steam (D-30)",
			err)
	}
}
