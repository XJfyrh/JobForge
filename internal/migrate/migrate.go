// Package migrate provides a lightweight, embedded SQL migration runner.
// It uses Go's embed to bundle migration files into the binary and applies
// them in version order on startup. A PostgreSQL advisory lock prevents
// concurrent migration execution across multiple instances.
//
// Migration files follow the naming convention: NNNN_description.up.sql.
// Applied versions are tracked in the schema_migrations table.
package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/migrations"
)

// advisoryLockID is a fixed arbitrary int64 used as the PostgreSQL advisory
// lock key for migration execution. Only one process can hold this lock,
// preventing concurrent schema changes.
const advisoryLockID int64 = 7483921654

// Migrator applies embedded SQL migrations to a PostgreSQL database.
type Migrator struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// New creates a Migrator for the given connection pool.
func New(pool *pgxpool.Pool, logger *slog.Logger) *Migrator {
	return &Migrator{pool: pool, logger: logger}
}

// Up applies all pending migrations in version order. It acquires a
// PostgreSQL advisory lock to ensure only one instance migrates at a time.
//
// Invariant: migrations are applied sequentially within a single goroutine.
// Each migration runs in its own transaction. A failed migration aborts the
// process without marking the version as applied.
func (m *Migrator) Up(ctx context.Context) error {
	// Acquire a dedicated connection so the session-level advisory lock and
	// its release are guaranteed to run on the same physical connection.
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn for migration lock: %w", err)
	}
	defer conn.Release()

	// Acquire advisory lock (blocks until available).
	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", advisoryLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Release advisory lock on the same connection. Use background context
		// in case the original is cancelled during shutdown.
		_, _ = conn.Exec(context.Background(), "select pg_advisory_unlock($1)", advisoryLockID)
	}()

	// Ensure tracking table exists.
	if err := m.ensureTable(ctx); err != nil {
		return err
	}

	// Get already-applied versions.
	applied, err := m.appliedVersions(ctx)
	if err != nil {
		return err
	}

	// Discover embedded migration files.
	migrations, err := discoverMigrations()
	if err != nil {
		return err
	}

	// Apply pending migrations in order.
	pendingCount := 0
	for _, mig := range migrations {
		if applied[mig.version] {
			continue
		}

		m.logger.Info("applying migration",
			"version", mig.version,
			"name", mig.name,
		)

		if err := m.applyOne(ctx, mig); err != nil {
			return fmt.Errorf("apply migration %s: %w", mig.name, err)
		}
		pendingCount++
	}

	if pendingCount == 0 {
		m.logger.Info("schema up to date", "applied_versions", len(applied))
	} else {
		m.logger.Info("migrations applied", "count", pendingCount)
	}

	return nil
}

// ensureTable creates the schema_migrations tracking table if it does not exist.
func (m *Migrator) ensureTable(ctx context.Context) error {
	_, err := m.pool.Exec(ctx, `
		create table if not exists schema_migrations (
			version integer primary key,
			name text not null,
			applied_at timestamptz not null default now()
		)
	`)
	if err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}
	return nil
}

// appliedVersions returns the set of already-applied migration versions.
func (m *Migrator) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := m.pool.Query(ctx, "select version from schema_migrations order by version")
	if err != nil {
		return nil, fmt.Errorf("query applied versions: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// applyOne executes a single migration within a transaction and records it.
func (m *Migrator) applyOne(ctx context.Context, mig migration) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Execute the migration SQL.
	if _, err := tx.Exec(ctx, mig.sql); err != nil {
		return fmt.Errorf("exec sql: %w", err)
	}

	// Record the migration as applied.
	if _, err := tx.Exec(ctx,
		"insert into schema_migrations (version, name) values ($1, $2)",
		mig.version, mig.name,
	); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	return tx.Commit(ctx)
}

// migration represents a single parsed migration file.
type migration struct {
	version int
	name    string
	sql     string
}

// discoverMigrations reads all .up.sql files from the embedded filesystem
// and returns them sorted by version.
func discoverMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	var migs []migration
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}

		version, err := parseVersion(name)
		if err != nil {
			return nil, fmt.Errorf("parse migration filename %q: %w", name, err)
		}

		content, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}

		migs = append(migs, migration{
			version: version,
			name:    name,
			sql:     string(content),
		})
	}

	sort.Slice(migs, func(i, j int) bool {
		return migs[i].version < migs[j].version
	})

	return migs, nil
}

// parseVersion extracts the numeric version prefix from a migration filename.
// Expected format: "NNNN_description.up.sql" -> NNNN as integer.
func parseVersion(filename string) (int, error) {
	idx := strings.Index(filename, "_")
	if idx < 1 {
		return 0, fmt.Errorf("missing version prefix")
	}
	var version int
	if _, err := fmt.Sscanf(filename[:idx], "%d", &version); err != nil {
		return 0, fmt.Errorf("invalid version number: %w", err)
	}
	return version, nil
}
