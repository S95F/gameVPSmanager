package adopt

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// fakeCutoverEnv is a CutoverEnv for testing. Callers can inject failures at
// any named step by setting the corresponding error.
type fakeCutoverEnv struct {
	failAt      map[string]error
	writes      map[string][]byte
	calls       []string
	worldFiles  []string
	worldCallN  int // counts WorldFiles invocations
	failWorldAt int // fail WorldFiles on this call (1-indexed; 0 = never)
}

func newFakeEnv(worldFiles []string) *fakeCutoverEnv {
	return &fakeCutoverEnv{
		failAt:     make(map[string]error),
		writes:     make(map[string][]byte),
		worldFiles: worldFiles,
	}
}

func (e *fakeCutoverEnv) mayFail(op string) error {
	e.calls = append(e.calls, op)
	if err, ok := e.failAt[op]; ok {
		return err
	}
	return nil
}

func (e *fakeCutoverEnv) WriteFile(path string, data []byte, _ fs.FileMode) error {
	if err := e.mayFail("WriteFile:" + path); err != nil {
		return err
	}
	e.writes[path] = append([]byte(nil), data...)
	return nil
}

func (e *fakeCutoverEnv) MkdirAll(path string, _ fs.FileMode) error {
	return e.mayFail("MkdirAll:" + path)
}

func (e *fakeCutoverEnv) SystemctlStop(unit string) error {
	return e.mayFail("SystemctlStop:" + unit)
}

func (e *fakeCutoverEnv) SystemctlDaemonReload() error {
	return e.mayFail("SystemctlDaemonReload")
}

func (e *fakeCutoverEnv) SystemctlEnable(unit string) error {
	return e.mayFail("SystemctlEnable:" + unit)
}

func (e *fakeCutoverEnv) SystemctlDisable(unit string) error {
	return e.mayFail("SystemctlDisable:" + unit)
}

func (e *fakeCutoverEnv) SystemctlStart(unit string) error {
	return e.mayFail("SystemctlStart:" + unit)
}

func (e *fakeCutoverEnv) TarCreate(_, _ string) error {
	return e.mayFail("TarCreate")
}

func (e *fakeCutoverEnv) WorldFiles(_ string) ([]string, error) {
	e.worldCallN++
	e.calls = append(e.calls, "WorldFiles")
	if e.failWorldAt > 0 && e.worldCallN == e.failWorldAt {
		return nil, errors.New("injected failure")
	}
	return e.worldFiles, nil
}

var testDisc = &DiscoverResult{
	Game:       "projectzomboid",
	UnitName:   "zomboid.service",
	InstallDir: "/opt/pzserver",
	DataDir:    "/home/pzserver/Zomboid",
	ServerName: "servertest",
	User:       "pzserver",
	Ports:      map[string]int{"game": 16261, "udp": 16262, "rcon": 27015},
}

var testWorldFiles = []string{"Multiplayer/servertest/map_t.bin", "Multiplayer/servertest/players.db"}
var testInstanceYAML = []byte("apiVersion: gvm/v1\nkind: Instance\nmetadata:\n  name: servertest\n")

func TestCutoverHappyPath(t *testing.T) {
	env := newFakeEnv(testWorldFiles)
	res, err := CutoverPZ(context.Background(), env, testDisc, testInstanceYAML)
	if err != nil {
		t.Fatalf("CutoverPZ: %v", err)
	}
	if res.RollbackScriptPath == "" {
		t.Error("RollbackScriptPath should be set")
	}
	if res.BackupPath == "" {
		t.Error("BackupPath should be set")
	}
}

// TestCutoverRollsBackOnEveryPostStep6Failure injects a failure at each of
// steps 6–11 and verifies that automatic rollback fires.
func TestCutoverRollsBackOnEveryPostStep6Failure(t *testing.T) {
	// Map of step→ inject function that triggers rollback.
	type stepCase struct {
		step   int
		key    string
		inject func(*fakeCutoverEnv)
	}
	postStepKeys := []stepCase{
		{6, "MkdirAll:/etc/gvm/instances", func(e *fakeCutoverEnv) {
			e.failAt["MkdirAll:/etc/gvm/instances"] = errors.New("injected failure")
		}},
		{7, "MkdirAll:/etc/systemd/system/gvm@servertest.service.d", func(e *fakeCutoverEnv) {
			e.failAt["MkdirAll:/etc/systemd/system/gvm@servertest.service.d"] = errors.New("injected failure")
		}},
		{8, "SystemctlDaemonReload", func(e *fakeCutoverEnv) {
			e.failAt["SystemctlDaemonReload"] = errors.New("injected failure")
		}},
		{9, "SystemctlStart:gvm@servertest.service", func(e *fakeCutoverEnv) {
			e.failAt["SystemctlStart:gvm@servertest.service"] = errors.New("injected failure")
		}},
		{10, "WorldFiles(post-cutover)", func(e *fakeCutoverEnv) {
			// The second WorldFiles call is the post-cutover check (step 10).
			e.failWorldAt = 2
		}},
		{11, "SystemctlDisable:zomboid.service", func(e *fakeCutoverEnv) {
			e.failAt["SystemctlDisable:zomboid.service"] = errors.New("injected failure")
		}},
	}

	for _, tc := range postStepKeys {
		tc := tc
		t.Run(tc.key, func(t *testing.T) {
			env := newFakeEnv(testWorldFiles)
			tc.inject(env)

			_, err := CutoverPZ(context.Background(), env, testDisc, testInstanceYAML)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var rb *ErrCutoverRolledBack
			if !errors.As(err, &rb) {
				t.Fatalf("expected ErrCutoverRolledBack, got %T: %v", err, err)
			}

			// Verify rollback fired: legacy unit re-enabled and started.
			legacyEnabled := false
			legacyStarted := false
			for _, call := range env.calls {
				if call == "SystemctlEnable:zomboid.service" {
					legacyEnabled = true
				}
				if call == "SystemctlStart:zomboid.service" {
					legacyStarted = true
				}
			}
			if !legacyEnabled {
				t.Errorf("step %d failure: legacy unit not re-enabled; calls: %v", tc.step, env.calls)
			}
			if !legacyStarted {
				t.Errorf("step %d failure: legacy unit not started; calls: %v", tc.step, env.calls)
			}
		})
	}
}

func TestWorldFingerprintSupersetSemantics(t *testing.T) {
	// Normal cutover: all pre-cutover files survive → pass.
	t.Run("AllFilesPresent", func(t *testing.T) {
		env := newFakeEnv(testWorldFiles)
		_, err := CutoverPZ(context.Background(), env, testDisc, testInstanceYAML)
		if err != nil {
			t.Errorf("expected success, got: %v", err)
		}
	})

	// Simulated fresh-world: a pre-cutover file vanishes → fail with names.
	t.Run("MissingFileAfterCutover", func(t *testing.T) {
		postFiles := []string{testWorldFiles[0]} // drop testWorldFiles[1]
		env := newFakeEnv(testWorldFiles)
		// Override WorldFiles so the second call returns fewer files.
		calls := 0
		env.failAt["WorldFiles"] = nil // reset; we'll intercept via a wrapper
		// Use a custom wrapper — override calls directly.
		overrideEnv := &overridingCutoverEnv{
			inner: env,
			worldFilesFn: func(_ string) ([]string, error) {
				calls++
				if calls == 1 {
					return testWorldFiles, nil // pre-cutover
				}
				return postFiles, nil // post-cutover: missing one file
			},
		}
		_, err := CutoverPZ(context.Background(), overrideEnv, testDisc, testInstanceYAML)
		if err == nil {
			t.Fatal("expected fingerprint error, got nil")
		}
		var rb *ErrCutoverRolledBack
		if !errors.As(err, &rb) {
			t.Fatalf("expected ErrCutoverRolledBack, got %T: %v", err, err)
		}
		if !strings.Contains(err.Error(), testWorldFiles[1]) {
			t.Errorf("error should name the missing file, got: %v", err)
		}
	})
}

// overridingCutoverEnv wraps a CutoverEnv and overrides WorldFiles.
type overridingCutoverEnv struct {
	inner        CutoverEnv
	worldFilesFn func(string) ([]string, error)
}

func (e *overridingCutoverEnv) WriteFile(p string, d []byte, m fs.FileMode) error {
	return e.inner.WriteFile(p, d, m)
}
func (e *overridingCutoverEnv) MkdirAll(p string, m fs.FileMode) error {
	return e.inner.MkdirAll(p, m)
}
func (e *overridingCutoverEnv) SystemctlStop(u string) error   { return e.inner.SystemctlStop(u) }
func (e *overridingCutoverEnv) SystemctlDaemonReload() error   { return e.inner.SystemctlDaemonReload() }
func (e *overridingCutoverEnv) SystemctlEnable(u string) error { return e.inner.SystemctlEnable(u) }
func (e *overridingCutoverEnv) SystemctlDisable(u string) error {
	return e.inner.SystemctlDisable(u)
}
func (e *overridingCutoverEnv) SystemctlStart(u string) error { return e.inner.SystemctlStart(u) }
func (e *overridingCutoverEnv) TarCreate(s, d string) error   { return e.inner.TarCreate(s, d) }
func (e *overridingCutoverEnv) WorldFiles(dir string) ([]string, error) {
	return e.worldFilesFn(dir)
}
