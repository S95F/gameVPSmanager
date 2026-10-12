package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func healthyFacts() Facts {
	return Facts{
		Instance: "pz-main",
		Exec:     PathFact{Path: "/opt/pzserver/start-server.sh"},
		Install:  PathFact{Path: "/opt/pzserver"},
		Data:     PathFact{Path: "/home/pzserver/Zomboid"},
		Logs:     PathFact{Path: "/var/log/gvm/pz-main"},
		Run:      PathFact{Path: "/run/gvm/pz-main"},
		Lock:     LockFact{Path: "/run/gvm/pz-main/job.lock"},
	}
}

func wantFailure(t *testing.T, err error, code int, check string) *Failure {
	t.Helper()
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("error %v is not a *Failure", err)
	}
	if f.Code != code {
		t.Errorf("exit code = %d, want %d (check %s)", f.Code, code, f.Check)
	}
	if f.Check != check {
		t.Errorf("check = %q, want %q", f.Check, check)
	}
	if strings.TrimSpace(f.Status) == "" {
		// STATUS= is what `systemctl status` shows, and it is often the only
		// thing an operator reads.
		t.Error("the failure carries no STATUS= sentence")
	}
	if got := ExitCode(err); got != code {
		t.Errorf("ExitCode = %d, want %d", got, code)
	}
	return f
}

func TestEvaluateAcceptsAHealthyHost(t *testing.T) {
	if err := Evaluate(healthyFacts()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
}

func TestAManifestPinMismatchExits64AndComesFirst(t *testing.T) {
	// D-17: `apt upgrade gvm` shipping a new manifest must not silently change
	// a live adopted server's argv. And it is reported BEFORE the directory
	// checks, because those directories are the ones the wrong manifest names.
	f := healthyFacts()
	f.PinErr = errors.New("sha256 mismatch")
	f.Install.Missing = true

	fail := wantFailure(t, Evaluate(f), ExitUsage, "manifest_pin")
	if !strings.Contains(fail.Status, "manifest.lock.yaml") {
		t.Errorf("STATUS does not name the file: %q", fail.Status)
	}
}

func TestAMissingDirectoryExits65AndAnUnwritableOneExits78(t *testing.T) {
	// The distinction is the restart policy. A missing directory is a data
	// error that retrying cannot fix; an existing directory that cannot be
	// written to is the ProtectHome mistake -- a unit-file bug that must fail
	// loudly in under a second rather than silently at the first world write.
	missing := healthyFacts()
	missing.Data.Missing = true
	wantFailure(t, Evaluate(missing), ExitDataErr, "data_dir")

	sandboxed := healthyFacts()
	sandboxed.Data.WriteErr = os.ErrPermission
	fail := wantFailure(t, Evaluate(sandboxed), ExitConfig, "data_dir")
	if !strings.Contains(fail.Status, "ReadWritePaths=") {
		t.Errorf("STATUS does not name the fix: %q", fail.Status)
	}
}

func TestAMissingOrNonExecutableEntrypointExits65(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*PathFact)
	}{
		{"missing", func(p *PathFact) { p.Missing = true }},
		{"not a file", func(p *PathFact) { p.NotFile = true }},
		{"not executable", func(p *PathFact) { p.NoExec = true }},
	} {
		f := healthyFacts()
		c.set(&f.Exec)
		t.Run(c.name, func(t *testing.T) {
			wantFailure(t, Evaluate(f), ExitDataErr, "exec")
		})
	}
}

func TestAHeldJobLockExits75SoSystemdRetries(t *testing.T) {
	// Lock 1 of section 11.7's three: an operator running `gvm run` by hand
	// while the unit is up. 75 rather than 65 because the condition resolves
	// itself when the other runner exits.
	f := healthyFacts()
	f.Lock.Err = platform.ErrLocked
	wantFailure(t, Evaluate(f), ExitTempFail, "job_lock")
}

func TestALockThatCannotBeTakenAtAllIsAConfigError(t *testing.T) {
	f := healthyFacts()
	f.Lock.Err = os.ErrPermission
	wantFailure(t, Evaluate(f), ExitConfig, "job_lock")
}

func TestALiveOrphanExits75WithTheDocumentedSentence(t *testing.T) {
	// Lock 3: runner.json survived a restart and its pinned process is still
	// alive. The sentence is section 11.7's verbatim, because it is what an
	// operator greps the journal for.
	f := healthyFacts()
	f.Orphan = OrphanFact{Present: true, Alive: true, Identity: Identity{PID: 41799, PGID: 41799, StartTicks: 8391244}}

	fail := wantFailure(t, Evaluate(f), ExitTempFail, "orphan")
	if fail.Status != "orphaned game process 41799 still running" {
		t.Errorf("STATUS = %q", fail.Status)
	}
}

func TestAnUndecidableOrphanIsRefusedRatherThanAssumedDead(t *testing.T) {
	// The single most dangerous default in the subsystem. "I could not read
	// /proc" must never become "the world is free": the consequence is two
	// servers on one world.
	f := healthyFacts()
	f.Orphan = OrphanFact{Present: true, Err: errors.New("permission denied")}
	fail := wantFailure(t, Evaluate(f), ExitTempFail, "orphan")
	if !strings.Contains(fail.Status, "second server") {
		t.Errorf("STATUS does not say what is being prevented: %q", fail.Status)
	}
}

func TestAnAbsentOrphanPinIsFine(t *testing.T) {
	f := healthyFacts()
	f.Orphan = OrphanFact{Path: "/run/gvm/pz-main/runner.json"}
	if err := Evaluate(f); err != nil {
		t.Fatalf("a first start was refused: %v", err)
	}
}

func TestMissingCredentialsExit65(t *testing.T) {
	noDir := healthyFacts()
	noDir.Creds = CredFact{Want: []string{"rcon", "admin"}}
	fail := wantFailure(t, Evaluate(noDir), ExitDataErr, "credentials")
	if !strings.Contains(fail.Status, "LoadCredential=") {
		t.Errorf("STATUS does not name the fix: %q", fail.Status)
	}

	oneMissing := healthyFacts()
	oneMissing.Creds = CredFact{Dir: "/run/credentials/x", Want: []string{"rcon", "admin"}, Missing: []string{"admin"}}
	fail = wantFailure(t, Evaluate(oneMissing), ExitDataErr, "credentials")
	if !strings.Contains(fail.Status, "admin") {
		t.Errorf("STATUS does not name the credential: %q", fail.Status)
	}
}

func TestAnInstanceThatNeedsNoCredentialsIsNotAskedForAny(t *testing.T) {
	f := healthyFacts()
	f.Creds = CredFact{}
	if err := Evaluate(f); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
}

func TestExitCodeOfAnUnexpectedErrorIs70(t *testing.T) {
	// Anything that is not a *Failure is a runner bug rather than an
	// environment problem, and 70 is the code that retries.
	if got := ExitCode(errors.New("boom")); got != ExitSoftware {
		t.Errorf("ExitCode = %d, want %d", got, ExitSoftware)
	}
	if got := ExitCode(nil); got != 0 {
		t.Errorf("ExitCode(nil) = %d", got)
	}
	if got := StatusOf(errors.New("boom")); got != "boom" {
		t.Errorf("StatusOf = %q", got)
	}
}

// fakeLock returns a lock value the test can hand back without touching a real
// filesystem lock, which does not exist off Linux.
func fakeLock(err error) func(string) (*platform.FileLock, error) {
	return func(string) (*platform.FileLock, error) {
		if err != nil {
			return nil, err
		}
		return &platform.FileLock{}, nil
	}
}

func testRequest(t *testing.T) Request {
	t.Helper()
	root := t.TempDir()
	install := filepath.Join(root, "install")
	data := filepath.Join(root, "data")
	logs := filepath.Join(root, "logs")
	run := filepath.Join(root, "run")
	for _, d := range []string{install, data, logs, run} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	execPath := filepath.Join(install, "start-server.sh")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Request{
		Instance:   "pz-main",
		Manifest:   testManifest(),
		Pin:        spec.ManifestPin{Revision: 3, SHA256: "1b40c9de"},
		Exec:       execPath,
		InstallDir: install,
		DataDir:    data,
		LogDir:     logs,
		RunDir:     run,
		RunnerLock: filepath.Join(run, "job.lock"),
		RunnerJSON: filepath.Join(run, "runner.json"),
		Flock:      fakeLock(nil),
	}
}

func TestGatherWriteProbesRatherThanTrustingAMode(t *testing.T) {
	// A mode says nothing about a read-only bind mount, a full filesystem or
	// ProtectHome=, which are the three ways this fails in production.
	req := testRequest(t)
	facts, lock := Gather(req)
	if lock == nil {
		t.Fatal("Gather took no lock")
	}
	if err := Evaluate(facts); err != nil {
		t.Fatalf("a healthy tree was refused: %v", err)
	}
	// And the probe leaves nothing behind.
	entries, err := os.ReadDir(req.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the write probe left %d files behind", len(entries))
	}
}

func TestGatherNoticesAFileWhereADirectoryShouldBe(t *testing.T) {
	req := testRequest(t)
	req.DataDir = filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(req.DataDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	facts, _ := Gather(req)
	if !facts.Data.NotDir {
		t.Fatalf("Data = %+v", facts.Data)
	}
	wantFailure(t, Evaluate(facts), ExitDataErr, "data_dir")
}

func TestGatherReadsThePinnedOrphanAndAsksTheHost(t *testing.T) {
	req := testRequest(t)
	id := Identity{PID: 41799, PGID: 41799, StartTicks: 8391244}
	if err := WriteRunnerFile(req.RunnerJSON, RunnerFile{Instance: "pz-main", Process: id}); err != nil {
		t.Fatal(err)
	}
	var askedPID int
	var askedTicks uint64
	req.Alive = func(pid int, ticks uint64) (bool, error) {
		askedPID, askedTicks = pid, ticks
		return true, nil
	}

	facts, _ := Gather(req)
	if askedPID != id.PID || askedTicks != id.StartTicks {
		// Asking with the pid alone is the pid-reuse bug: a recycled pid would
		// answer "alive" for a completely unrelated process.
		t.Fatalf("asked Alive(%d, %d), want (%d, %d)", askedPID, askedTicks, id.PID, id.StartTicks)
	}
	if !facts.Orphan.Present || !facts.Orphan.Alive {
		t.Fatalf("Orphan = %+v", facts.Orphan)
	}
	wantFailure(t, Evaluate(facts), ExitTempFail, "orphan")
}

func TestGatherRefusesACorruptOrphanPin(t *testing.T) {
	// A runner.json that cannot be parsed may still name a live process, so it
	// is not permission to start.
	req := testRequest(t)
	if err := os.WriteFile(req.RunnerJSON, []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}
	req.Alive = func(int, uint64) (bool, error) {
		t.Fatal("liveness was queried for an unparsable pin")
		return false, nil
	}
	facts, _ := Gather(req)
	if facts.Orphan.Err == nil {
		t.Fatal("a corrupt runner.json was ignored")
	}
	wantFailure(t, Evaluate(facts), ExitTempFail, "orphan")
}

func TestGatherVerifiesTheManifestItIsAboutToRun(t *testing.T) {
	// The check is against the loaded manifest rather than a second read of
	// the file, so it covers the bytes that will actually build the argv.
	req := testRequest(t)
	facts, _ := Gather(req)
	if facts.PinErr != nil {
		t.Fatalf("a matching pin was rejected: %v", facts.PinErr)
	}

	req.Pin.SHA256 = "deadbeef"
	facts, _ = Gather(req)
	if facts.PinErr == nil {
		t.Fatal("a mismatched pin was accepted")
	}
	wantFailure(t, Evaluate(facts), ExitUsage, "manifest_pin")
}

func TestPreflightRefusesWithNoLockLeftHeld(t *testing.T) {
	// A refused start must not leave the instance locked against the next
	// attempt: the caller of a failed Preflight is about to exit, and a lock
	// it was handed but not told about would be leaked until the process died.
	req := testRequest(t)
	req.InstallDir = filepath.Join(req.InstallDir, "missing")

	lock, err := Preflight(req)
	if err == nil {
		t.Fatal("Preflight accepted a missing install directory")
	}
	if lock != nil {
		t.Error("Preflight returned a lock alongside a refusal")
	}
	wantFailure(t, err, ExitDataErr, "install_dir")
}

func TestPreflightWithNoLockPathRefuses(t *testing.T) {
	// World safety must not be switchable off by leaving a field blank.
	req := testRequest(t)
	req.RunnerLock = ""
	if _, err := Preflight(req); err == nil {
		t.Fatal("preflight ran with no job lock")
	}
}

func TestPreflightHoldsTheLockOnSuccess(t *testing.T) {
	req := testRequest(t)
	lock, err := Preflight(req)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if lock == nil {
		t.Fatal("Preflight succeeded without returning the lock it took")
	}
}

func TestRunnerFileRoundTripsThePin(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.json")
	want := RunnerFile{
		Instance: "pz-main",
		Game:     "projectzomboid",
		Unit:     "gvm@pz-main.service",
		Process:  Identity{PID: 41799, PGID: 41799, StartTicks: 8391244},
	}
	if err := WriteRunnerFile(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRunnerFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Process != want.Process || got.Instance != want.Instance {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	if got.Proto != RunnerFileProto {
		t.Errorf("proto = %d, want %d", got.Proto, RunnerFileProto)
	}
	// Atomic writes leave no temporary file behind for the next start to trip
	// over.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d files after one write", len(entries))
	}
}

func TestAnUnpinnedInstanceIsRefusedRatherThanUnchecked(t *testing.T) {
	// A caller that forgot to fill in the pin must not silently get a start
	// with no manifest check at all, which is the one outcome D-17 exists to
	// prevent.
	req := testRequest(t)
	req.Pin = spec.ManifestPin{}
	facts, _ := Gather(req)
	wantFailure(t, Evaluate(facts), ExitUsage, "manifest_pin")
}
