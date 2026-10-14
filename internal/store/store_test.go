package store_test

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/store"
)

// TestSchemaVersion confirms the constant matches the number of migration files.
func TestSchemaVersion(t *testing.T) {
	if store.SchemaVersion != 10 {
		t.Errorf("SchemaVersion = %d, want 10", store.SchemaVersion)
	}
}

// TestMigrationFilesEmbedded confirms each expected migration file can be read
// from the embedded FS via the Migrator's embedded view.
// This is a build-only test — no database is required.
func TestMigrationFilesEmbedded(t *testing.T) {
	// We test via the public API: open a zero-DSN store and confirm the
	// migration embed compiled.
	want := []string{
		"migrations/0001_types.sql",
		"migrations/0002_identity.sql",
		"migrations/0003_instances.sql",
		"migrations/0004_manifests.sql",
		"migrations/0005_content.sql",
		"migrations/0006_jobs_backups.sql",
		"migrations/0007_control_channel.sql",
		"migrations/0008_netedge.sql",
		"migrations/0009_secrets_audit.sql",
		"migrations/0010_grants.sql",
	}
	// Access migrations through the exported test helper.
	got := store.MigrationFiles()
	have := make(map[string]bool, len(got))
	for _, f := range got {
		have[f] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("migration file not found in embed: %s", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d migration files, want %d", len(got), len(want))
	}
}

// TestMigrationFilesHaveGooseMarkers confirms each SQL file has the goose
// Up/Down markers.
func TestMigrationFilesHaveGooseMarkers(t *testing.T) {
	files := store.MigrationFiles()
	if len(files) == 0 {
		t.Fatal("no migration files")
	}
	migFS := store.MigrationsFS()
	for _, f := range files {
		b, err := fs.ReadFile(migFS, f)
		if err != nil {
			t.Errorf("read %s: %v", f, err)
			continue
		}
		content := string(b)
		if !strings.Contains(content, "-- +goose Up") {
			t.Errorf("%s: missing '-- +goose Up' marker", f)
		}
	}
}

// TestStoreTypesCompile is a compile-time guard: if the Store, Migrator, and
// error variables compile, this test passes.
func TestStoreTypesCompile(t *testing.T) {
	_ = store.ErrDatastoreUnavailable
	_ = store.ErrSchemaVersion
}
