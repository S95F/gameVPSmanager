package spec

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestWriteFileAtomicReplacesAndKeepsTheOldVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pz-main.yaml")

	if err := WriteFileAtomic(path, []byte("first\n"), ModeSpec); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := os.Stat(path + BackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("a first write invented a .bak of a file that never existed")
	}

	if err := WriteFileAtomic(path, []byte("second\n"), ModeSpec); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got := readFile(t, path); got != "second\n" {
		t.Errorf("content = %q, want the new bytes", got)
	}
	if got := readFile(t, path+BackupSuffix); got != "first\n" {
		t.Errorf(".bak = %q, want the previous version", got)
	}

	// The rename must replace an existing target rather than fail on it, which
	// is the one place Windows and Linux could have differed.
	if err := WriteFileAtomic(path, []byte("third\n"), ModeSpec); err != nil {
		t.Fatalf("third write over an existing file: %v", err)
	}
	if got := readFile(t, path); got != "third\n" {
		t.Errorf("content = %q", got)
	}
	if got := readFile(t, path+BackupSuffix); got != "second\n" {
		t.Errorf(".bak = %q, want the version replaced by this write", got)
	}
}

func TestWriteFileAtomicSetsTheModeBeforeThePathExists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX mode bits to assert; the chmod is a no-op there")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.yaml")
	if err := WriteFileAtomic(path, []byte("x\n"), ModeSpec); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// os.CreateTemp makes 0600; anything but 0640 here means the web tier either
	// cannot read the spec it has to render, or could write one.
	if got := fi.Mode().Perm(); got != ModeSpec {
		t.Errorf("mode = %04o, want %04o", got, ModeSpec)
	}
}

func TestFsyncOrderIsWriteSyncBackupRenameSyncDir(t *testing.T) {
	// Ordering is the whole of the durability guarantee and no functional test
	// can see it: every wrong order still produces the right bytes on a machine
	// that does not lose power.
	dir := t.TempDir()
	path := filepath.Join(dir, "pz-main.yaml")
	if err := WriteFileAtomic(path, []byte("first\n"), ModeSpec); err != nil {
		t.Fatal(err)
	}

	p := &writeProbe{}
	if err := writeAtomic(path, []byte("second\n"), ModeSpec, true, -1, -1, p); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := []string{stepWrite, stepSyncFile, stepCloseFile, stepChmod, stepBackup, stepRename, stepSyncDir}
	if !reflect.DeepEqual(p.trace, want) {
		t.Fatalf("trace = %v, want %v", p.trace, want)
	}
}

func TestCrashBeforeRenameLeavesTheOriginalIntact(t *testing.T) {
	// Each of these is a process death at a point where a naive implementation
	// (truncate-and-write) would have already destroyed the file.
	for _, failAt := range []string{stepWrite, stepSyncFile, stepCloseFile, stepChmod, stepBackup} {
		t.Run(failAt, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "pz-main.yaml")
			const original = "apiVersion: gvm/v1\nkind: Instance\n"
			if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}

			err := writeAtomic(path, []byte("REPLACEMENT\n"), ModeSpec, true, -1, -1, &writeProbe{failAt: failAt})
			if !errors.Is(err, errProbeAbort) {
				t.Fatalf("err = %v, want the injected abort", err)
			}
			if got := readFile(t, path); got != original {
				t.Errorf("original = %q, want it untouched", got)
			}
			if _, err := os.Stat(path + BackupSuffix); err == nil {
				// A .bak may exist by the backup step, but it must never be the
				// half-written new content.
				if got := readFile(t, path+BackupSuffix); got != original {
					t.Errorf(".bak = %q, want the original or nothing", got)
				}
			}
		})
	}
}

func TestCrashAfterRenameHasAlreadyCommitted(t *testing.T) {
	// The rename is the commit point. Dying after it must leave the NEW file,
	// not a rollback, or a crash during the directory fsync would look like a
	// write that never happened.
	dir := t.TempDir()
	path := filepath.Join(dir, "pz-main.yaml")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeAtomic(path, []byte("new\n"), ModeSpec, true, -1, -1, &writeProbe{failAt: stepRename})
	if !errors.Is(err, errProbeAbort) {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, path); got != "new\n" {
		t.Errorf("content = %q, want the committed new bytes", got)
	}
	if got := readFile(t, path+BackupSuffix); got != "old\n" {
		t.Errorf(".bak = %q", got)
	}
}

func TestLeftoverTempFromACrashIsNotAManifest(t *testing.T) {
	// A crash leaves the temp file behind. It must not be picked up as a
	// manifest by the catalog, or the next `gvm apply` runs half a file.
	dir := t.TempDir()
	path := filepath.Join(dir, "valheim.yaml")
	if err := os.WriteFile(path, []byte(gameYAML("valheim", 3)), 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeAtomic(path, []byte(gameYAML("valheim", 4)), ModeSpec, true, -1, -1, &writeProbe{failAt: stepWrite})
	if !errors.Is(err, errProbeAbort) {
		t.Fatalf("err = %v", err)
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var leftover string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			leftover = e.Name()
		}
	}
	if leftover == "" {
		t.Fatal("no temp file survived; this test is asserting nothing")
	}

	cat := NewCatalog(DirSource(OriginPackage, dir))
	ids, err := cat.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"valheim"}) {
		t.Errorf("List() = %v, want only valheim despite the leftover %q", ids, leftover)
	}
	m, _, err := cat.Load("valheim", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Metadata.Revision != 3 {
		t.Errorf("revision = %d, want the intact original 3", m.Metadata.Revision)
	}
}

func TestWriteFileAtomicRefusesAnUnwritableDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	err := WriteFileAtomic(filepath.Join(dir, "x.yaml"), []byte("x"), ModeSpec)
	if err == nil {
		t.Fatal("write into a missing directory succeeded")
	}
	if !strings.Contains(err.Error(), "create temp") {
		t.Errorf("err = %v, want it to name the failing step", err)
	}
}

func TestSaveInstanceWritesSomethingThatLoadsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pz-main.yaml")
	in := &Instance{
		APIVersion: "gvm/v1",
		Kind:       "Instance",
		Metadata: InstanceMeta{
			Name: "pz-main",
			UID:  "019f2c7a-8b31-7c4e-9d20-3f61a5b7c9e2",
		},
		Spec: InstanceSpec{
			Game:   "projectzomboid",
			Ports:  map[string]int{"game": 16261, "rcon": 27015},
			Vars:   map[string]string{"jvm_xmx": "8G"},
			Backup: BackupPolicy{Keep: 14, Dir: "/var/backups/gvm/pz-main"},
		},
	}
	if err := SaveInstance(path, in); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	got, diags, err := LoadInstance(path)
	if err != nil || diags.HasErrors() {
		t.Fatalf("LoadInstance: %v %s", err, diags)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round trip changed the instance:\n got %+v\nwant %+v", got, in)
	}

	// Two-space indent, so a gvm-written file and a hand-written one do not
	// differ cosmetically in every diff.
	if !strings.Contains(readFile(t, path), "\nspec:\n  game: projectzomboid") {
		t.Errorf("not written at a two-space indent:\n%s", readFile(t, path))
	}
}

func TestSaveHostConfigRoundTripsTheDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gvm.yaml")
	want := DefaultHostConfig()
	if err := SaveHostConfig(path, &want); err != nil {
		t.Fatalf("SaveHostConfig: %v", err)
	}
	var got HostConfig
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &got); err != nil {
		t.Fatal(err)
	}
	// Compared by re-encoding, not field by field: an absent list decodes to an
	// empty one, so DeepEqual would report a difference that no operator can see
	// and no consumer can act on. What must hold is that saving what was loaded
	// produces the same file.
	again, err := MarshalYAML(&got)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != readFile(t, path) {
		t.Errorf("round trip changed the host config:\n got %s\nwant %s", again, readFile(t, path))
	}
	if got.Spec.Firewall.SSH.RateLimit != want.Spec.Firewall.SSH.RateLimit ||
		got.Spec.Edge.HTTPRedirect != want.Spec.Edge.HTTPRedirect {
		t.Error("a default that is true survived as false: the zero struct was written, not the defaults")
	}
}
