package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodRebuildSpec = `apiVersion: gvm/v1
kind: Instance
metadata:
  name: v1
spec:
  game: valheim
  enabled: true
`

// rebuildArchive writes a real gvm archive carrying specYAML, the way a backup
// of instance v1 would, and returns its path.
func rebuildArchive(t *testing.T, specYAML string) string {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "world.db"), []byte("world bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ref, err := Create(context.Background(), Options{
		Dir: dir, Instance: "v1", Class: ClassRoutine, Reason: "pre-move",
		Now:      func() time.Time { return fixed },
		SpecYAML: specYAML,
		Subject:  Subject{Name: "v1", Game: "valheim"},
		Sources:  []Source{{Prefix: "data/", Base: data}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ref.Path
}

// TestInspectRebuildSourceAcceptsADecryptedArchiveAndParsesItsSpec: a plain,
// well-formed gvm archive is accepted, its head manifest comes back, and the
// redacted instance spec it carries is parsed -- which is what a rebuild reads.
func TestInspectRebuildSourceAcceptsADecryptedArchiveAndParsesItsSpec(t *testing.T) {
	path := rebuildArchive(t, goodRebuildSpec)

	src, err := InspectRebuildSource(path)
	if err != nil {
		t.Fatalf("a decrypted, well-formed archive was refused: %v", err)
	}
	if src.Manifest == nil || src.Manifest.Instance.Name != "v1" {
		t.Fatalf("the head manifest did not come back: %+v", src.Manifest)
	}
	if src.Instance == nil {
		t.Fatal("the embedded instance spec was not parsed")
	}
	if src.Instance.Metadata.Name != "v1" || src.Instance.Spec.Game != "valheim" {
		t.Errorf("the parsed spec is not the archive's: %+v", src.Instance)
	}
}

// TestInspectRebuildSourceRefusesAnAgeFileWithTheDecryptCommand is section
// 13.2's rule made concrete: gvm never decrypts, so a .age file is refused by
// its name alone -- without being opened -- and the refusal carries the one
// command that moves it forward, age -d on a workstation.
func TestInspectRebuildSourceRefusesAnAgeFileWithTheDecryptCommand(t *testing.T) {
	dir := t.TempDir()
	age := filepath.Join(dir, "v1-20260904-040003-routine.tar.gz.age")
	if err := os.WriteFile(age, []byte("age-encryption.org/v1\n...ciphertext..."), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := InspectRebuildSource(age)
	if !errors.Is(err, ErrEncrypted) {
		t.Fatalf("a .age file was not refused with ErrEncrypted: %v", err)
	}
	for _, want := range []string{"decrypt it first", "age -d -i <identity>", "docs/backups-and-restore.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is missing %q: %v", want, err)
		}
	}
	// The command names the decrypted target, with the .age stripped, so the
	// operator can paste it rather than work out the destination name.
	if !strings.Contains(err.Error(), "routine.tar.gz (see") {
		t.Errorf("the refusal does not name the decrypted file: %v", err)
	}
}

// TestInspectRebuildSourceRefusesAnEncryptedFileRenamedToLookPlain: the age
// header is checked even when the name is not .age. Without it a rebuild would
// gunzip ciphertext and report a corrupt archive, hiding that the real problem
// is an un-decrypted backup.
func TestInspectRebuildSourceRefusesAnEncryptedFileRenamedToLookPlain(t *testing.T) {
	for name, header := range map[string]string{
		"binary": "age-encryption.org/v1\nsome binary age body that is not gzip",
		"armor":  "-----BEGIN AGE ENCRYPTED FILE-----\nYWdlLWVuY3J5cHRpb24=",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "world.tar.gz") // not .age
			if err := os.WriteFile(p, []byte(header), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := InspectRebuildSource(p); !errors.Is(err, ErrEncrypted) {
				t.Fatalf("an age-headed file was not refused with ErrEncrypted: %v", err)
			}
		})
	}
}

// TestInspectRebuildSourceRefusesSomethingThatIsNotAGvmArchive: a file that is
// neither encrypted nor a gvm archive is refused rather than rebuilt from.
func TestInspectRebuildSourceRefusesSomethingThatIsNotAGvmArchive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notes.tar.gz")
	if err := os.WriteFile(p, []byte("just some bytes, not a gzip stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectRebuildSource(p); err == nil {
		t.Fatal("a non-archive file was accepted as a rebuild source")
	}
}

// TestInspectRebuildSourceRefusesACorruptEmbeddedSpec: the archive is a real
// gvm archive, but the instance spec it carries does not parse. A rebuild reads
// that spec, so catching it here is the difference between a clean refusal and a
// half-created instance.
func TestInspectRebuildSourceRefusesACorruptEmbeddedSpec(t *testing.T) {
	path := rebuildArchive(t, "apiVersion: gvm/v1\nkind: Instance\nspec:\n  enabled: definitely-not-a-bool\n")

	_, err := InspectRebuildSource(path)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a corrupt embedded spec was not refused with ErrCorrupt: %v", err)
	}
}
