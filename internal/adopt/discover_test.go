package adopt

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// fixtureEnv is a DiscoverEnv backed by the testdata/live-pz/rootfs tree.
// Every call is recorded so TestDiscoverIsReadOnly can verify no mutations.
type fixtureEnv struct {
	root  string
	calls []string
}

func newFixtureEnv(t *testing.T) *fixtureEnv {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "testdata", "live-pz", "rootfs")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("fixture root not found: %s", root)
	}
	return &fixtureEnv{root: root}
}

func (e *fixtureEnv) ReadFile(path string) ([]byte, error) {
	e.calls = append(e.calls, "ReadFile:"+path)
	return os.ReadFile(filepath.Join(e.root, path))
}

func (e *fixtureEnv) ReadDir(path string) ([]fs.DirEntry, error) {
	e.calls = append(e.calls, "ReadDir:"+path)
	entries, err := os.ReadDir(filepath.Join(e.root, path))
	if err != nil {
		return nil, err
	}
	// Sort deterministically so discovery output is stable.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func (e *fixtureEnv) Stat(path string) (fs.FileInfo, error) {
	e.calls = append(e.calls, "Stat:"+path)
	return os.Stat(filepath.Join(e.root, path))
}

func (e *fixtureEnv) UserProcesses(_ string) ([]ProcInfo, error) {
	e.calls = append(e.calls, "UserProcesses")
	return nil, nil // fixture = not running
}

// writeOnlyMethods lists method names that would mutate state.
var writeOnlyMethods = []string{"WriteFile", "MkdirAll", "SystemctlStop",
	"SystemctlEnable", "SystemctlDisable", "SystemctlStart", "SystemctlDaemonReload"}

func TestDiscoverIsReadOnly(t *testing.T) {
	env := newFixtureEnv(t)
	_, err := DiscoverPZ(env, "zomboid")
	if err != nil {
		t.Fatalf("DiscoverPZ: %v", err)
	}
	for _, call := range env.calls {
		for _, bad := range writeOnlyMethods {
			if strings.Contains(call, bad) {
				t.Errorf("discovery made a mutating call: %s", call)
			}
		}
	}
}

func TestDiscoverGolden(t *testing.T) {
	env := newFixtureEnv(t)
	r, err := DiscoverPZ(env, "zomboid")
	if err != nil {
		t.Fatalf("DiscoverPZ: %v", err)
	}
	got := FormatDiscoverResult(r)

	goldenPath := filepath.Join("testdata", "golden", "discover-pz-main.txt")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, []byte(got), 0644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden updated: %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with UPDATE_GOLDEN=1 to create)", goldenPath, err)
	}
	if got != string(want) {
		t.Errorf("output does not match golden\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestDiscoverPZFindings verifies the four expected findings are present.
func TestDiscoverPZFindings(t *testing.T) {
	env := newFixtureEnv(t)
	r, err := DiscoverPZ(env, "zomboid")
	if err != nil {
		t.Fatalf("DiscoverPZ: %v", err)
	}

	want := map[string]bool{
		"ADOPT-001": false,
		"ADOPT-002": false,
		"ADOPT-003": false,
		"ADOPT-004": false,
	}
	for _, f := range r.Findings {
		want[f.Code] = true
	}
	for code, found := range want {
		if !found {
			t.Errorf("expected finding %s not present", code)
		}
	}
}

func TestDiscoverModCountMismatch(t *testing.T) {
	env := newFixtureEnv(t)
	r, err := DiscoverPZ(env, "zomboid")
	if err != nil {
		t.Fatalf("DiscoverPZ: %v", err)
	}
	if r.WorkshopItemCount != 33 {
		t.Errorf("WorkshopItems = %d, want 33", r.WorkshopItemCount)
	}
	if r.ModCount != 32 {
		t.Errorf("Mods = %d, want 32", r.ModCount)
	}
}

func TestDiscoverAmbiguousMod(t *testing.T) {
	env := newFixtureEnv(t)
	r, err := DiscoverPZ(env, "zomboid")
	if err != nil {
		t.Fatalf("DiscoverPZ: %v", err)
	}
	if len(r.AmbiguousMods) != 1 {
		t.Fatalf("AmbiguousMods = %d, want 1", len(r.AmbiguousMods))
	}
	if r.AmbiguousMods[0].WorkshopID != "2866258937" {
		t.Errorf("AmbiguousMod.WorkshopID = %q, want 2866258937", r.AmbiguousMods[0].WorkshopID)
	}
	if len(r.AmbiguousMods[0].Candidates) != 2 {
		t.Errorf("Candidates count = %d, want 2", len(r.AmbiguousMods[0].Candidates))
	}
}

func TestDiscoverPortsFromINI(t *testing.T) {
	env := newFixtureEnv(t)
	r, err := DiscoverPZ(env, "zomboid")
	if err != nil {
		t.Fatalf("DiscoverPZ: %v", err)
	}
	// Ports come from live .ini, never from manifest defaults.
	if r.Ports["game"] != 16261 {
		t.Errorf("game port = %d, want 16261", r.Ports["game"])
	}
	if r.Ports["udp"] != 16262 {
		t.Errorf("udp port = %d, want 16262", r.Ports["udp"])
	}
	if r.Ports["rcon"] != 27015 {
		t.Errorf("rcon port = %d, want 27015", r.Ports["rcon"])
	}
}

func TestDiscoverScreenDetected(t *testing.T) {
	env := newFixtureEnv(t)
	r, err := DiscoverPZ(env, "zomboid")
	if err != nil {
		t.Fatalf("DiscoverPZ: %v", err)
	}
	if !r.UsesScreen {
		t.Error("UsesScreen should be true (ExecStart contains screen)")
	}
	// ADOPT-004 note must be present.
	found := false
	for _, f := range r.Findings {
		if f.Code == "ADOPT-004" && f.Severity == severityNote {
			found = true
		}
	}
	if !found {
		t.Error("ADOPT-004 note not found for screen-based ExecStart")
	}
}
