package store

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// SchemaVersion is the expected migration version that this binary requires.
// It equals the highest migration file number.
const SchemaVersion = 10

//go:embed migrations/*.sql
var migrations embed.FS

// Migrator wraps a pool and runs goose migrations against it.
type Migrator struct{ pool *pgxpool.Pool }

// NewMigrator returns a Migrator backed by pool.
func NewMigrator(pool *pgxpool.Pool) *Migrator { return &Migrator{pool: pool} }

// Migrate runs all pending Up migrations.
func (m *Migrator) Migrate(ctx context.Context) error {
	db := stdlib.OpenDBFromPool(m.pool)
	defer db.Close()
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("store: set dialect: %w", err)
	}
	return goose.UpContext(ctx, db, "migrations")
}

// Version returns the current schema version recorded in goose's version table.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	db := stdlib.OpenDBFromPool(m.pool)
	defer db.Close()
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("store: get version: %w", err)
	}
	return v, nil
}

// MigrationFiles returns the paths of all embedded migration files.
// It exists primarily for testing.
func MigrationFiles() []string {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, "migrations/"+e.Name())
		}
	}
	return out
}

// MigrationsFS returns the embedded filesystem containing the migration SQL
// files. It exists primarily for testing.
func MigrationsFS() embed.FS { return migrations }

// AssertCompatible returns an error if the live schema version does not match
// SchemaVersion. gvmd calls this at start and refuses to serve if it fails.
func (m *Migrator) AssertCompatible(ctx context.Context) error {
	v, err := m.Version(ctx)
	if err != nil {
		return err
	}
	if v != SchemaVersion {
		return fmt.Errorf("%w: schema version %d, binary expects %d — run `gvm db migrate`",
			ErrSchemaVersion, v, SchemaVersion)
	}
	return nil
}
