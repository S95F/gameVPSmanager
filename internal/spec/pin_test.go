package spec

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// pinGame is a manifest with one of each thing a pin protects: an argv, a port
// default, a shutdown ladder and a destroy glob.
func pinGame(revision int, serverArg, port, destroy string) string {
	return `apiVersion: gvm/v1
kind: Game
metadata:
  id: projectzomboid
  name: Project Zomboid
  revision: ` + strconv.Itoa(revision) + `
spec:
  driverABI: 1
  runtime:
    exec: ./start-server.sh
    args:
      - "-servername"
      - "` + serverArg + `"
  ports:
    - {name: game, proto: udp, default: ` + port + `, expose: public}
  shutdown:
    budget: 300s
    steps:
      - {via: console, send: save, wait: 20s}
  backup:
    destroy_protected: ["` + destroy + `"]
    world_paths: ["Zomboid/Saves"]
`
}

// TestPinIsolation is the D-17 regression: `apt upgrade gvm` shipping a new
// projectzomboid.yaml must not change a live instance's argv, port defaults,
// shutdown ladder or destroy globs. It is the whole reason the pin exists.
func TestPinIsolation(t *testing.T) {
	root := t.TempDir()
	share := filepath.Join(root, "share")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	catalogFile := filepath.Join(share, "projectzomboid.yaml")
	before := pinGame(7, "{{ .Var.server_name }}", "16261", "Zomboid/Saves")
	if err := os.WriteFile(catalogFile, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := NewCatalog(DirSource(OriginPackage, share))

	applied, _, err := cat.Load("projectzomboid", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	instDir := InstanceDropInDir(filepath.Join(root, "instances"), "pz-main")
	if err := WritePin(instDir, applied); err != nil {
		t.Fatalf("WritePin: %v", err)
	}
	pin := PinOf(applied)

	// The package upgrade lands.
	after := pinGame(9, "-XX:+UseZGC", "16271", "Zomboid/")
	if err := os.WriteFile(catalogFile, []byte(after), 0o644); err != nil {
		t.Fatal(err)
	}
	upgraded, _, err := cat.Load("projectzomboid", nil)
	if err != nil {
		t.Fatalf("Load after upgrade: %v", err)
	}
	if upgraded.Metadata.Revision != 9 {
		t.Fatalf("the catalog did not move: revision %d; this test would pass vacuously",
			upgraded.Metadata.Revision)
	}

	pinned, err := LoadPinned(instDir)
	if err != nil {
		t.Fatalf("LoadPinned: %v", err)
	}
	if pinned.Metadata.Revision != 7 {
		t.Errorf("pinned revision = %d, want 7", pinned.Metadata.Revision)
	}
	if !reflect.DeepEqual(pinned.Spec.Runtime.Args, applied.Spec.Runtime.Args) {
		t.Errorf("argv changed under a live instance:\n got %+v\nwant %+v",
			pinned.Spec.Runtime.Args, applied.Spec.Runtime.Args)
	}
	if !reflect.DeepEqual(pinned.Spec.Ports, applied.Spec.Ports) {
		t.Errorf("port defaults changed: %+v", pinned.Spec.Ports)
	}
	if !reflect.DeepEqual(pinned.Spec.Backup.DestroyProtected, applied.Spec.Backup.DestroyProtected) {
		t.Errorf("destroy globs changed: %+v", pinned.Spec.Backup.DestroyProtected)
	}
	if !reflect.DeepEqual(pinned.Spec.Shutdown, applied.Spec.Shutdown) {
		t.Errorf("shutdown ladder changed: %+v", pinned.Spec.Shutdown)
	}
	if string(pinned.Raw) != before {
		t.Error("the pinned bytes are not the approved file verbatim")
	}
	if pinned.SHA256 != applied.SHA256 {
		t.Errorf("pinned sha = %s, want %s", pinned.SHA256, applied.SHA256)
	}

	// And the instance still recognises its own pin while refusing the new one,
	// which is what the runner turns into exit 64.
	if err := pin.Verify(pinned); err != nil {
		t.Errorf("Verify(pinned) = %v, want nil", err)
	}
	if err := pin.Verify(upgraded); !errors.Is(err, ErrPinMismatch) {
		t.Errorf("Verify(upgraded) = %v, want ErrPinMismatch", err)
	}
}

func TestPinnedManifestHasNoOrigin(t *testing.T) {
	// The pinned copy is a copy of whichever layer won; letting it claim an
	// origin would let a file under /etc/gvm/instances present itself as an
	// approved override (D-19).
	dir := t.TempDir()
	share := filepath.Join(dir, "share")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, "valheim.yaml"), []byte(gameYAML("valheim", 3)), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := NewCatalog(DirSource(OriginOverride, share))
	m, _, err := cat.Load("valheim", RootCLIAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Origin != OriginOverride {
		t.Fatalf("setup: origin = %q", m.Origin)
	}

	instDir := filepath.Join(dir, "valheim-1.d")
	if err := WritePin(instDir, m); err != nil {
		t.Fatal(err)
	}
	pinned, err := LoadPinned(instDir)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Origin != "" {
		t.Errorf("pinned origin = %q, want empty", pinned.Origin)
	}
	if pinned.SourceRef != PinPath(instDir) {
		t.Errorf("SourceRef = %q, want the pin's own path", pinned.SourceRef)
	}
}

func TestWritePinRefusesToInventBytes(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		m    *Manifest
	}{
		{"nil", nil},
		{"no raw bytes", &Manifest{Metadata: GameMeta{ID: "valheim", Revision: 3}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Re-marshalling would pin a file with a different sha256 than the
			// one the operator approved.
			if err := WritePin(filepath.Join(dir, tc.name), tc.m); !errors.Is(err, ErrNoRawBytes) {
				t.Errorf("err = %v, want ErrNoRawBytes", err)
			}
		})
	}
}

func TestWritePinRefusesBytesThatDoNotMatchTheirDigest(t *testing.T) {
	// A caller that edited Raw and kept the old digest would pin bytes nothing
	// approved, and the mismatch would not surface until the runner refused to
	// start hours later.
	m := &Manifest{
		Metadata: GameMeta{ID: "valheim", Revision: 3},
		Raw:      []byte(gameYAML("valheim", 3)),
		SHA256:   "00000000000000000000000000000000000000000000000000000000deadbeef",
	}
	err := WritePin(filepath.Join(t.TempDir(), "valheim-1.d"), m)
	if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("err = %v, want ErrPinMismatch", err)
	}
}

func TestLoadPinnedSaysWhenThereIsNoPin(t *testing.T) {
	// "Never applied" and "cannot read /etc" are different problems with
	// different fixes, so they are different errors.
	_, err := LoadPinned(filepath.Join(t.TempDir(), "pz-main.d"))
	if !errors.Is(err, ErrNoPin) {
		t.Fatalf("err = %v, want ErrNoPin", err)
	}
	if !strings.Contains(err.Error(), PinFileName) {
		t.Errorf("err = %v, want it to name the missing file", err)
	}
}

func TestVerifyDistinguishesAnInPlaceEditFromAnUpgrade(t *testing.T) {
	edited := &Manifest{
		Metadata: GameMeta{ID: "valheim", Revision: 7},
		Raw:      []byte(gameYAML("valheim", 7, "  # a comment that changes the bytes\n")),
	}
	edited.SHA256 = sha256Hex(edited.Raw)

	tests := []struct {
		name     string
		pin      ManifestPin
		wantErr  bool
		wantWord string
	}{
		{"matching", ManifestPin{Revision: 7, SHA256: edited.SHA256}, false, ""},
		{"case-insensitive digest", ManifestPin{Revision: 7, SHA256: strings.ToUpper(edited.SHA256)}, false, ""},
		{"edited in place", ManifestPin{Revision: 7, SHA256: strings.Repeat("a", 64)}, true, "without bumping metadata.revision"},
		{"honest upgrade", ManifestPin{Revision: 5, SHA256: strings.Repeat("a", 64)}, true, "pinned revision 5"},
		{"no digest recorded", ManifestPin{Revision: 7}, true, "records no sha256"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.pin.Verify(edited)
			if tc.wantErr != (err != nil) {
				t.Fatalf("Verify = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				if !errors.Is(err, ErrPinMismatch) {
					t.Errorf("err = %v, want ErrPinMismatch", err)
				}
				if !strings.Contains(err.Error(), tc.wantWord) {
					t.Errorf("err = %q, want it to contain %q", err, tc.wantWord)
				}
			}
		})
	}
}

func TestLoadPinnedForChecksBeforeItReturns(t *testing.T) {
	// Reading the pinned file and forgetting to verify it is the same bug as not
	// pinning at all, so the one call does both.
	dir := t.TempDir()
	share := filepath.Join(dir, "share")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, "valheim.yaml"), []byte(gameYAML("valheim", 3)), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := NewCatalog(DirSource(OriginPackage, share))
	m, _, err := cat.Load("valheim", nil)
	if err != nil {
		t.Fatal(err)
	}
	instDir := filepath.Join(dir, "valheim-1.d")
	if err := WritePin(instDir, m); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadPinnedFor(instDir, PinOf(m)); err != nil {
		t.Errorf("LoadPinnedFor with the right pin = %v", err)
	}
	wrong := ManifestPin{Revision: 3, SHA256: strings.Repeat("b", 64)}
	if _, err := LoadPinnedFor(instDir, wrong); !errors.Is(err, ErrPinMismatch) {
		t.Errorf("LoadPinnedFor with a stale pin = %v, want ErrPinMismatch", err)
	}
}

func TestWritePinCreatesTheDropInDirectory(t *testing.T) {
	dir := t.TempDir()
	instDir := InstanceDropInDir(filepath.Join(dir, "etc", "instances"), "pz-main")
	if !strings.HasSuffix(instDir, "pz-main.d") {
		t.Fatalf("InstanceDropInDir = %q, want <name>.d (D-04)", instDir)
	}
	m := &Manifest{Metadata: GameMeta{ID: "valheim", Revision: 1}, Raw: []byte(gameYAML("valheim", 1))}
	if err := WritePin(instDir, m); err != nil {
		t.Fatalf("WritePin: %v", err)
	}
	fi, err := os.Stat(PinPath(instDir))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if got := fi.Mode().Perm(); got != ModeSpec {
			t.Errorf("pin mode = %04o, want %04o root:gvm", got, ModeSpec)
		}
	}
}
