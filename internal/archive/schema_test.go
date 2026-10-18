package archive

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestManifestAcceptsCurrentAndRefusesNewer is gvm-self-upgrade-uninstall-10 for
// the manifest reader: an archive at this build's schema (or, once the schema
// bumps, below it) reads; one above it is refused with the one thing the
// operator can do — install a newer gvm — rather than a generic "unsupported".
func TestManifestAcceptsCurrentAndRefusesNewer(t *testing.T) {
	cur := &Manifest{Schema: SchemaVersion, Instance: Subject{Name: "pz-main"}, Class: ClassRoutine, CreatedAt: fixed}
	if err := cur.Validate(); err != nil {
		t.Errorf("a current-schema manifest was refused: %v", err)
	}

	newer := &Manifest{Schema: SchemaVersion + 1, Instance: Subject{Name: "pz-main"}, Class: ClassRoutine, CreatedAt: fixed}
	err := newer.Validate()
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("a newer-schema manifest was not refused with ErrSchema: %v", err)
	}
	if !strings.Contains(err.Error(), "install a newer gvm") {
		t.Errorf("the refusal does not tell the operator to install a newer gvm: %v", err)
	}

	bad := &Manifest{Schema: 0, Instance: Subject{Name: "pz-main"}, Class: ClassRoutine, CreatedAt: fixed}
	if err := bad.Validate(); !errors.Is(err, ErrSchema) {
		t.Errorf("a schema-0 manifest was not refused: %v", err)
	}
}

// TestLoadIndexAcceptsCurrentAndRefusesNewer: the index is a cache, so a newer
// schema is refused (Reindex then rebuilds from the archives), and a current
// one loads.
func TestLoadIndexAcceptsCurrentAndRefusesNewer(t *testing.T) {
	dir := t.TempDir()

	writeIndex := func(schema int) {
		body := fmt.Sprintf(`{"schema":%d,"entries":[]}`, schema)
		if err := os.WriteFile(IndexPath(dir), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeIndex(SchemaVersion + 1)
	_, err := LoadIndex(dir)
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("LoadIndex accepted a newer schema: %v", err)
	}
	if !strings.Contains(err.Error(), "install a newer gvm") {
		t.Errorf("the index refusal lacks the install hint: %v", err)
	}

	writeIndex(SchemaVersion)
	if _, err := LoadIndex(dir); err != nil {
		t.Errorf("a current-schema index was refused: %v", err)
	}
}
